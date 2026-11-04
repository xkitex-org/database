package runtime

import (
	"context"
	"strings"
	"testing"
)

const validConfig = `default: primary
sources:
  primary:
    type: postgresql
    host: localhost
    database: zmall
    username: zmall
    password: "secret-marker"
  second:
    type: mysql
    host: localhost
    database: reports
    username: report
    password: "secret-marker"
`

// TestStrictSingleShotConfiguration 验证默认数据源、驱动选择及严格字段校验，不输出密码。
func TestStrictSingleShotConfiguration(t *testing.T) {
	for _, body := range []string{
		"", "{}", "[]", "default: missing\nsources: {}", validConfig + "unknown: secret-marker\n",
		strings.Replace(validConfig, "postgresql", "sqlite3", 1),
		strings.Replace(validConfig, "host: localhost", "host: 'bad host'", 1),
		strings.Replace(validConfig, "database: zmall", "port: -1\n    database: zmall", 1),
		strings.Replace(validConfig, "database: zmall", "ping_timeout: invalid\n    database: zmall", 1),
		validConfig + "---\n{}", validConfig + "default: second",
	} {
		if _, err := parseSettings([]byte(body)); err == nil || strings.Contains(err.Error(), "secret-marker") {
			t.Fatalf("invalid config accepted or leaked: %v", err)
		}
	}
	m := New()
	if err := m.ConfigureDatabase([]byte(validConfig)); err != nil {
		t.Fatal(err)
	}
	if err := m.ConfigureDatabase([]byte(validConfig)); err == nil {
		t.Fatal("configuration was reloaded")
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if m.engines["primary"].DriverName() != "pgx" || m.engines["second"].DriverName() != "mysql" {
		t.Fatal("wrong driver selection")
	}
	if len(m.ReadinessChecks()) != 2 {
		t.Fatal("named dependencies were not exposed")
	}
	if err := m.Start(context.Background()); err == nil {
		t.Fatal("duplicate start accepted")
	}
}

// TestReadinessChecksAndNoConstructorSideEffects 验证健康检查独立于事务且未启动句柄安全报错。
func TestReadinessChecksAndNoConstructorSideEffects(t *testing.T) {
	unused := New()
	if len(unused.ReadinessChecks()) != 0 {
		t.Fatal("constructor created resources")
	}
	if _, err := unused.Database().Context(context.Background()).Exec("SELECT 1"); err == nil {
		t.Fatal("uninitialized manager accepted query")
	}
	m := testManager(t)
	checks := m.ReadinessChecks()
	for name, check := range checks {
		if err := check(context.Background()); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := checks["database.primary"](ctx); err == nil {
		t.Fatal("healthcheck ignored cancellation")
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if err := checks["database.primary"](context.Background()); err == nil {
		t.Fatal("healthcheck accepted closed pool")
	}
}
