package runtime

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/cloudwego/kitex/pkg/klog"
	"xorm.io/xorm"
)

// Manager 管理一次应用启动的数据源快照及全部连接池，不引用 core 或 monitor 模块。
type Manager struct {
	mu                          sync.RWMutex
	cfg                         settings
	engines                     map[string]*xorm.Engine
	configured, started, closed bool
	closeErr                    error
	txMu                        sync.Mutex
	transactions                map[*Transaction]struct{}
}

// New 创建没有任何资源副作用的管理器。
func New() *Manager { return &Manager{transactions: make(map[*Transaction]struct{})} }

// ConfigureDatabase 单次接收 database 节，缺失或非法配置阻止启动。
func (m *Manager) ConfigureDatabase(body []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.configured || m.closed {
		return errors.New("database: 配置只允许在启动前应用一次")
	}
	cfg, err := parseSettings(body)
	if err != nil {
		return err
	}
	m.cfg, m.configured = cfg, true
	return nil
}

// Start 创建全部具名连接池，失败时释放已创建资源；连通性由可选 monitor 的 readiness 检查确认。
func (m *Manager) Start(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.configured || m.closed || m.started {
		return errors.New("database: 启动生命周期无效")
	}
	if ctx == nil {
		return errors.New("database: context 不能为空")
	}
	engines := make(map[string]*xorm.Engine)
	for _, name := range names(m.cfg.Sources) {
		if err := ctx.Err(); err != nil {
			closeEngines(engines)
			return err
		}
		engine, err := openEngine(m.cfg.Sources[name])
		if err != nil {
			closeEngines(engines)
			return fmt.Errorf("database: 数据源[%s]：%w", name, err)
		}
		engines[name] = engine
		klog.Infof("database: 连接池已创建：source=[%s] type=[%s]", name, m.cfg.Sources[name].Type)
	}
	m.engines, m.started = engines, true
	return nil
}

// Database 返回默认数据源句柄，真正的可用性和事务归属在执行时校验。
func (m *Manager) Database() *Handle { return &Handle{manager: m} }

// ReadinessChecks 返回每个数据源独立的 Ping 检查，不启动后台任务。
func (m *Manager) ReadinessChecks() map[string]func(context.Context) error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	checks := make(map[string]func(context.Context) error, len(m.engines))
	for name := range m.engines {
		checks["database."+name] = func(ctx context.Context) error { return m.ping(ctx, name) }
	}
	return checks
}

// ping 遵循 monitor 传入的取消和截止时间，同时限制单数据源 Ping 上限。
func (m *Manager) ping(ctx context.Context, name string) error {
	if ctx == nil {
		return errors.New("database: context 不能为空")
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if err := m.usable(); err != nil {
		return err
	}
	engine, ok := m.engines[name]
	if !ok {
		return errors.New("database: 数据源不存在")
	}
	ctx, cancel := context.WithTimeout(ctx, m.cfg.Sources[name].pingTimeout)
	defer cancel()
	return engine.PingContext(ctx)
}

// Close 幂等回滚未结束事务并关闭全部连接池；core 必须在 RPC 排空后调用。
func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return m.closeErr
	}
	m.closed = true
	m.txMu.Lock()
	active := make([]*Transaction, 0, len(m.transactions))
	for tx := range m.transactions {
		active = append(active, tx)
	}
	m.txMu.Unlock()
	for _, tx := range active {
		m.closeErr = errors.Join(m.closeErr, tx.Close())
	}
	m.closeErr = errors.Join(m.closeErr, closeEngines(m.engines))
	if m.started {
		klog.Infof("database: 全部连接池已关闭：sources=[%d]", len(m.engines))
	}
	return m.closeErr
}

// usable 在持有管理器读锁或写锁时校验应用生命周期。
func (m *Manager) usable() error {
	if m.closed {
		return errors.New("database: 数据源管理器已关闭")
	}
	if !m.started {
		return errors.New("database: 尚未通过 WithDatabase 启动数据源")
	}
	return nil
}

// names 返回稳定排序的配置名称，保证启动、日志和释放顺序可预测。
func names[V any](values map[string]V) []string {
	result := make([]string, 0, len(values))
	for name := range values {
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}

// closeEngines 关闭全部已创建连接池，保留所有清理错误。
func closeEngines(engines map[string]*xorm.Engine) error {
	var err error
	for _, name := range names(engines) {
		err = errors.Join(err, engines[name].Close())
	}
	return err
}
