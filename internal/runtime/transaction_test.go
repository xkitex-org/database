package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"xorm.io/xorm"
)

// testManager 用真实 SQLite 连接验证事务语义，SQLite 仅限测试，不开放为生产配置类型。
func testManager(t *testing.T) *Manager {
	t.Helper()
	m := New()
	m.cfg = settings{Default: "primary", Sources: map[string]sourceSettings{}}
	m.engines = make(map[string]*xorm.Engine)
	for _, name := range []string{"primary", "second"} {
		engine, err := xorm.NewEngine("sqlite3", filepath.Join(t.TempDir(), name+".db")+"?_busy_timeout=2000")
		if err != nil {
			t.Fatal(err)
		}
		engine.SetLogger(newLogger())
		if _, err := engine.Exec("CREATE TABLE product (id INTEGER PRIMARY KEY, name TEXT NOT NULL)"); err != nil {
			t.Fatal(err)
		}
		m.engines[name] = engine
		m.cfg.Sources[name] = sourceSettings{pingTimeout: time.Second}
	}
	m.configured, m.started = true, true
	t.Cleanup(func() {
		if err := m.Close(); err != nil {
			t.Error(err)
		}
	})
	return m
}

// insertProduct 模拟独立 Repository，只接收 ctx，不接收或操作事务对象。
func insertProduct(ctx context.Context, h *Handle, id int64) error {
	_, err := h.Context(ctx).Table("product").Insert(map[string]any{"id": id, "name": "商品"})
	return err
}

// productCount 读取真实数据库中的记录数量。
func productCount(t *testing.T, h *Handle) int64 {
	t.Helper()
	n, err := h.Context(context.Background()).Table("product").Count()
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// TestBusinessTransactionPropagation 验证跨 Repository 共享事务、提交前隔离及提交后返回业务对象。
func TestBusinessTransactionPropagation(t *testing.T) {
	m := testManager(t)
	h := m.Database()
	txCtx, tx, err := h.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Close()
	if err := insertProduct(txCtx, h, 1); err != nil {
		t.Fatal(err)
	}
	if n := productCount(t, h); n != 0 {
		t.Fatalf("uncommitted write escaped transaction: %d", n)
	}
	// 模拟两次插入之间的业务处理，并通过派生 ctx 继续传递事务。
	businessResult := struct{ ID int64 }{ID: 2}
	child, cancel := context.WithCancel(txCtx)
	defer cancel()
	if err := insertProduct(child, h, businessResult.ID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if businessResult.ID != 2 || productCount(t, h) != 2 {
		t.Fatal("business result or commit was lost")
	}
	if err := insertProduct(txCtx, h, 3); err == nil || !strings.Contains(err.Error(), "事务已经结束") {
		t.Fatalf("ended ctx escaped to pool: %v", err)
	}
	if err := tx.Commit(); err == nil {
		t.Fatal("second commit was accepted")
	}
}

// TestRollbackOnErrorAndPanic 验证业务提前返回和 panic 均由 defer Close 撤销已写入数据。
func TestRollbackOnErrorAndPanic(t *testing.T) {
	for _, panicBusiness := range []bool{false, true} {
		t.Run(map[bool]string{false: "error", true: "panic"}[panicBusiness], func(t *testing.T) {
			m := testManager(t)
			h := m.Database()
			func() {
				defer func() { _ = recover() }()
				ctx, tx, err := h.Begin(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Close()
				if err := insertProduct(ctx, h, 1); err != nil {
					t.Fatal(err)
				}
				if panicBusiness {
					panic("业务处理失败")
				}
			}()
			if productCount(t, h) != 0 {
				t.Fatal("unfinished transaction was committed")
			}
		})
	}
}

// TestNamedSourcesAndTransactionGuards 验证切换不修改默认值，跨源、跨应用及嵌套事务均被拒绝。
func TestNamedSourcesAndTransactionGuards(t *testing.T) {
	m := testManager(t)
	h := m.Database()
	second, err := h.WithDataSource("second")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.WithDataSource("missing"); err == nil {
		t.Fatal("unknown source silently fell back")
	}
	if err := insertProduct(context.Background(), second, 1); err != nil {
		t.Fatal(err)
	}
	if productCount(t, h) != 0 || productCount(t, second) != 1 {
		t.Fatal("source selection changed default")
	}
	ctx, tx, err := second.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Close()
	primary, err := h.WithDataSource("primary")
	if err != nil {
		t.Fatal(err)
	}
	if err := insertProduct(ctx, primary, 2); err == nil {
		t.Fatal("cross-source transaction was accepted")
	}
	other := testManager(t)
	if err := insertProduct(ctx, other.Database(), 2); err == nil {
		t.Fatal("cross-application transaction was accepted")
	}
	if _, _, err := second.Begin(ctx); err == nil {
		t.Fatal("nested transaction was accepted")
	}
	if err := insertProduct(ctx, second, 2); err != nil {
		t.Fatal(err)
	}
	if err := insertProduct(ctx, h, 3); err != nil {
		t.Fatalf("default entry did not follow named transaction: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if productCount(t, second) != 1 {
		t.Fatal("rollback affected previously committed records")
	}
}

// TestCancellationAndClosedContextDoNotFallBack 验证取消和结束 ctx 不会触发普通连接写入。
func TestCancellationAndClosedContextDoNotFallBack(t *testing.T) {
	m := testManager(t)
	h := m.Database()
	parent, cancel := context.WithCancel(context.Background())
	ctx, tx, err := h.Begin(parent)
	if err != nil {
		t.Fatal(err)
	}
	if err := insertProduct(ctx, h, 1); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := insertProduct(ctx, h, 2); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation ignored: %v", err)
	}
	if err := tx.Commit(); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled transaction committed: %v", err)
	}
	if productCount(t, h) != 0 {
		t.Fatal("canceled transaction persisted data")
	}
	if _, _, err := h.Begin(nil); err == nil {
		t.Fatal("nil transaction context accepted")
	}
	if _, err := h.Context(nil).Exec("SELECT 1"); err == nil {
		t.Fatal("nil query context accepted")
	}
}

// TestConcurrentRepositoryOperationsSerializeSession 验证同一事务中的多次调用不会混用原生条件。
func TestConcurrentRepositoryOperationsSerializeSession(t *testing.T) {
	m := testManager(t)
	h := m.Database()
	ctx, tx, err := h.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Close()
	var group sync.WaitGroup
	for id := int64(1); id <= 12; id++ {
		group.Add(1)
		go func() {
			defer group.Done()
			if err := insertProduct(ctx, h, id); err != nil {
				t.Error(err)
			}
		}()
	}
	group.Wait()
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if productCount(t, h) != 12 {
		t.Fatal("transaction statements interfered")
	}
}

// TestQueriesAndResourceClose 验证链式查询、IN、零值更新和资源关闭后的拒绝访问。
func TestQueriesAndResourceClose(t *testing.T) {
	m := testManager(t)
	h := m.Database()
	for _, id := range []int64{1, 2, 3} {
		if err := insertProduct(context.Background(), h, id); err != nil {
			t.Fatal(err)
		}
	}
	var rows []struct {
		ID   int64  `xorm:"id"`
		Name string `xorm:"name"`
	}
	err := h.Context(context.Background()).Table("product").Cols("id", "name").Where("name LIKE ?", "%商%").In("id", []int64{1, 3}).Desc("id").Limit(1).Find(&rows)
	if err != nil || len(rows) != 1 || rows[0].ID != 3 {
		t.Fatalf("query: %+v %v", rows, err)
	}
	n, err := h.Context(context.Background()).Table("product").Where("id = ?", 3).Update(map[string]any{"name": ""})
	if err != nil || n != 1 {
		t.Fatalf("zero-value update: %d %v", n, err)
	}
	ctx, tx, err := h.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := insertProduct(ctx, h, 4); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Context(ctx).Exec("SELECT 1"); err == nil {
		t.Fatal("closed manager still accepted queries")
	}
	if err := tx.Commit(); err == nil {
		t.Fatal("shutdown did not roll back pending transaction")
	}
}
