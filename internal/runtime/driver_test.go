package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"xorm.io/xorm"
	"xorm.io/xorm/core"
)

// mockedManager 使用真实 XORM 方言和 sqlmock 驱动检验驱动边界，不连接外部数据库。
func mockedManager(t *testing.T, driver string) (*Manager, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
	if err != nil {
		t.Fatal(err)
	}
	dsn := "user:password@tcp(localhost:3306)/test"
	if driver == "pgx" {
		dsn = "postgresql://user:password@localhost:5432/test?sslmode=disable"
	}
	engine, err := xorm.NewEngineWithDB(driver, dsn, core.FromDB(db))
	if err != nil {
		t.Fatal(err)
	}
	engine.SetLogger(newLogger())
	m := New()
	m.configured, m.started = true, true
	m.cfg = settings{Default: "primary", Sources: map[string]sourceSettings{"primary": {pingTimeout: time.Second}}}
	m.engines = map[string]*xorm.Engine{"primary": engine}
	t.Cleanup(func() {
		mock.ExpectClose()
		if err := m.Close(); err != nil {
			t.Error(err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
	})
	return m, mock
}

// TestPostgresAndMySQLBindings 验证参数经过 XORM 方言转换且原样交给驱动，不拼接用户值。
func TestPostgresAndMySQLBindings(t *testing.T) {
	for _, driver := range []string{"pgx", "mysql"} {
		t.Run(driver, func(t *testing.T) {
			m, mock := mockedManager(t, driver)
			query := `UPDATE product SET name = \? WHERE id = \?`
			if driver == "pgx" {
				query = `UPDATE product SET name = \$1 WHERE id = \$2`
			}
			mock.ExpectExec(query).WithArgs("用户输入' ?", int64(1)).WillReturnResult(sqlmock.NewResult(0, 1))
			_, err := m.Database().Context(context.Background()).Exec("UPDATE product SET name = ? WHERE id = ?", "用户输入' ?", int64(1))
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestCommitFailureIsReturnedAndContextCannotEscape 验证提交错误可判断，旧 ctx 不会重新从连接池执行。
func TestCommitFailureIsReturnedAndContextCannotEscape(t *testing.T) {
	m, mock := mockedManager(t, "pgx")
	mock.ExpectBegin()
	ctx, tx, err := m.Database().Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	failure := errors.New("commit failed")
	mock.ExpectCommit().WillReturnError(failure)
	if err := tx.Commit(); !errors.Is(err, failure) {
		t.Fatalf("commit error lost: %v", err)
	}
	if _, err := m.Database().Context(ctx).Exec("UPDATE product SET name = ?", "不可写入"); err == nil {
		t.Fatal("failed commit fell back to pool")
	}
	if err := tx.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestReadinessFailureRecoveryAndDeadline 验证每次 Ping 可独立恢复且遵循调用方更短的超时。
func TestReadinessFailureRecoveryAndDeadline(t *testing.T) {
	m, mock := mockedManager(t, "pgx")
	check := m.ReadinessChecks()["database.primary"]
	mock.ExpectPing().WillReturnError(errors.New("temporarily unavailable"))
	if err := check(context.Background()); err == nil {
		t.Fatal("failed ping was healthy")
	}
	mock.ExpectPing()
	if err := check(context.Background()); err != nil {
		t.Fatal(err)
	}
	mock.ExpectPing().WillDelayFor(200 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := check(ctx); err == nil || time.Since(start) > 150*time.Millisecond {
		t.Fatalf("deadline ignored: %v", err)
	}
}
