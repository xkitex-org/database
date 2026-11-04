package runtime

import (
	"context"
	"database/sql"
	"errors"

	"xorm.io/xorm"
)

// Handle 绑定具名数据源，切换数据源只创建新句柄，不改变管理器默认值。
type Handle struct {
	manager *Manager
	name    string
}

// WithDataSource 校验数据源名称并返回独立句柄，未知名称绝不回退默认数据源。
func (h *Handle) WithDataSource(name string) (*Handle, error) {
	if h == nil || h.manager == nil {
		return nil, errors.New("database: 数据源句柄无效")
	}
	h.manager.mu.RLock()
	defer h.manager.mu.RUnlock()
	if err := h.manager.usable(); err != nil {
		return nil, err
	}
	if _, ok := h.manager.engines[name]; !ok {
		return nil, errors.New("database: 指定数据源不存在")
	}
	return &Handle{manager: h.manager, name: name}, nil
}

// Context 创建只属于本次操作的查询声明，不在全局 Engine 上保存请求上下文。
func (h *Handle) Context(ctx context.Context) *Session { return &Session{handle: h, ctx: ctx} }

// resolve 在持有管理器锁时解析默认名称及连接池。
func (h *Handle) resolve() (string, *xorm.Engine, error) {
	if err := h.manager.usable(); err != nil {
		return "", nil, err
	}
	name := h.name
	if name == "" {
		name = h.manager.cfg.Default
	}
	engine, ok := h.manager.engines[name]
	if !ok {
		return "", nil, errors.New("database: 指定数据源不存在")
	}
	return name, engine, nil
}

// Session 保存单次操作的条件，所有终结方法经过同一事务归属校验。
// 不暴露原生 Engine 或 Session，避免绕过 ctx 校验或意外提交共享事务。
type Session struct {
	handle *Handle
	ctx    context.Context
	steps  []func(*xorm.Session) *xorm.Session
}

// apply 保存查询步骤，在执行锁内才应用到原生 Session。
func (s *Session) apply(step func(*xorm.Session) *xorm.Session) *Session {
	s.steps = append(s.steps, step)
	return s
}

// execute 复用 ctx 的事务或创建普通 Session，结束事务及跨源调用均返回错误。
func (s *Session) execute(run func(*xorm.Session) error) error {
	if s == nil || s.handle == nil || s.handle.manager == nil {
		return errors.New("database: 查询句柄无效")
	}
	if s.ctx == nil {
		return errors.New("database: context 不能为空")
	}
	m := s.handle.manager
	m.mu.RLock()
	defer m.mu.RUnlock()
	selected := *s.handle
	tx := boundTransaction(s.ctx)
	// 默认入口跟随事务已选择的数据源，显式 WithDataSource 才表示切换意图。
	if tx != nil && selected.name == "" {
		selected.name = tx.source
	}
	name, engine, err := selected.resolve()
	if err != nil {
		return err
	}
	if err := s.ctx.Err(); err != nil {
		return err
	}
	var native *xorm.Session
	if tx != nil {
		if tx.manager != m || tx.source != name {
			return errors.New("database: 事务内不允许切换数据源或应用实例")
		}
		tx.mu.Lock()
		defer tx.mu.Unlock()
		if tx.ended {
			return errors.New("database: 事务已经结束")
		}
		if err := s.ctx.Err(); err != nil {
			return err
		}
		if err := tx.ctx.Err(); err != nil {
			return err
		}
		native = tx.session
	} else {
		native = engine.NewSession()
		defer native.Close()
	}
	native.Context(s.ctx)
	for _, step := range s.steps {
		native = step(native)
	}
	return run(native)
}

// Find 执行查询并将多条结果映射到切片，条件结构体遵循 XORM 原生规则。
func (s *Session) Find(dest any, conditions ...any) error {
	return s.execute(func(native *xorm.Session) error { return native.Find(dest, conditions...) })
}

// Get 查询单条记录，未找到时返回 false 而不是伪造业务错误。
func (s *Session) Get(dest any) (found bool, err error) {
	err = s.execute(func(native *xorm.Session) error { found, err = native.Get(dest); return err })
	return
}

// Insert 在当前连接或事务中插入记录。
func (s *Session) Insert(beans ...any) (affected int64, err error) {
	err = s.execute(func(native *xorm.Session) error { affected, err = native.Insert(beans...); return err })
	return
}

// Update 执行更新，零值字段选择遵循 XORM 的 Cols、AllCols 和 map 规则。
func (s *Session) Update(bean any, conditions ...any) (affected int64, err error) {
	err = s.execute(func(native *xorm.Session) error { affected, err = native.Update(bean, conditions...); return err })
	return
}

// Delete 执行当前条件下的删除操作。
func (s *Session) Delete(bean any) (affected int64, err error) {
	err = s.execute(func(native *xorm.Session) error { affected, err = native.Delete(bean); return err })
	return
}

// Count 统计当前条件匹配的记录数量。
func (s *Session) Count(bean ...any) (count int64, err error) {
	err = s.execute(func(native *xorm.Session) error { count, err = native.Count(bean...); return err })
	return
}

// Exec 执行原生 SQL，始终经过上下文事务校验。
func (s *Session) Exec(query any, args ...any) (result sql.Result, err error) {
	err = s.execute(func(native *xorm.Session) error {
		result, err = native.Exec(append([]any{query}, args...)...)
		return err
	})
	return
}

// Table 指定查询表名或业务实体。
func (s *Session) Table(table any) *Session {
	return s.apply(func(native *xorm.Session) *xorm.Session { return native.Table(table) })
}

// Cols 显式选择查询或更新字段。
func (s *Session) Cols(columns ...string) *Session {
	return s.apply(func(native *xorm.Session) *xorm.Session { return native.Cols(columns...) })
}

// Where 添加参数化条件或 XORM builder 条件。
func (s *Session) Where(query any, args ...any) *Session {
	return s.apply(func(native *xorm.Session) *xorm.Session { return native.Where(query, args...) })
}

// And 追加 AND 条件。
func (s *Session) And(query any, args ...any) *Session {
	return s.apply(func(native *xorm.Session) *xorm.Session { return native.And(query, args...) })
}

// Or 追加 OR 条件。
func (s *Session) Or(query any, args ...any) *Session {
	return s.apply(func(native *xorm.Session) *xorm.Session { return native.Or(query, args...) })
}

// In 添加 IN 条件，支持切片参数。
func (s *Session) In(column string, values ...any) *Session {
	return s.apply(func(native *xorm.Session) *xorm.Session { return native.In(column, values...) })
}

// NotIn 添加 NOT IN 条件。
func (s *Session) NotIn(column string, values ...any) *Session {
	return s.apply(func(native *xorm.Session) *xorm.Session { return native.NotIn(column, values...) })
}

// ID 按主键添加查询条件。
func (s *Session) ID(id any) *Session {
	return s.apply(func(native *xorm.Session) *xorm.Session { return native.ID(id) })
}

// Asc 按指定字段升序排序。
func (s *Session) Asc(columns ...string) *Session {
	return s.apply(func(native *xorm.Session) *xorm.Session { return native.Asc(columns...) })
}

// Desc 按指定字段降序排序。
func (s *Session) Desc(columns ...string) *Session {
	return s.apply(func(native *xorm.Session) *xorm.Session { return native.Desc(columns...) })
}

// OrderBy 设置由业务白名单确定的排序表达式。
func (s *Session) OrderBy(order any, args ...any) *Session {
	return s.apply(func(native *xorm.Session) *xorm.Session { return native.OrderBy(order, args...) })
}

// Limit 设置分页条数及可选偏移量。
func (s *Session) Limit(limit int, offset ...int) *Session {
	return s.apply(func(native *xorm.Session) *xorm.Session { return native.Limit(limit, offset...) })
}

// Select 指定 SELECT 表达式。
func (s *Session) Select(columns string) *Session {
	return s.apply(func(native *xorm.Session) *xorm.Session { return native.Select(columns) })
}

// SQL 使用原生 SQL 并保留结果映射能力。
func (s *Session) SQL(query any, args ...any) *Session {
	return s.apply(func(native *xorm.Session) *xorm.Session { return native.SQL(query, args...) })
}

// Alias 指定当前表别名。
func (s *Session) Alias(alias string) *Session {
	return s.apply(func(native *xorm.Session) *xorm.Session { return native.Alias(alias) })
}

// Join 添加关联查询。
func (s *Session) Join(operator string, table, condition any, args ...any) *Session {
	return s.apply(func(native *xorm.Session) *xorm.Session { return native.Join(operator, table, condition, args...) })
}

// GroupBy 设置分组字段。
func (s *Session) GroupBy(columns string) *Session {
	return s.apply(func(native *xorm.Session) *xorm.Session { return native.GroupBy(columns) })
}

// Having 设置分组筛选条件。
func (s *Session) Having(condition string) *Session {
	return s.apply(func(native *xorm.Session) *xorm.Session { return native.Having(condition) })
}

// Distinct 设置去重查询字段。
func (s *Session) Distinct(columns ...string) *Session {
	return s.apply(func(native *xorm.Session) *xorm.Session { return native.Distinct(columns...) })
}

// AllCols 显式包含更新中的零值字段。
func (s *Session) AllCols() *Session {
	return s.apply(func(native *xorm.Session) *xorm.Session { return native.AllCols() })
}

// ForUpdate 在事务中添加行锁查询。
func (s *Session) ForUpdate() *Session {
	return s.apply(func(native *xorm.Session) *xorm.Session { return native.ForUpdate() })
}
