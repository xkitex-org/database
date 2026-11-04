package runtime

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"
	"gopkg.in/yaml.v3"
	"xorm.io/xorm"
)

// settings 只保存本次启动的配置快照，不接受环境变量覆盖。
type settings struct {
	Default string                    `yaml:"default"`
	Sources map[string]sourceSettings `yaml:"sources"`
}

// sourceSettings 保存单个具名数据源，认证信息仅用于建立连接。
type sourceSettings struct {
	Type                                                  string `yaml:"type"`
	Host                                                  string `yaml:"host"`
	Port                                                  int    `yaml:"port"`
	Database                                              string `yaml:"database"`
	Username                                              string `yaml:"username"`
	Password                                              string `yaml:"password"`
	SSLMode                                               string `yaml:"ssl_mode"`
	TLS                                                   string `yaml:"tls"`
	MaxOpen                                               int    `yaml:"max_open_conns"`
	MaxIdle                                               *int   `yaml:"max_idle_conns"`
	ConnectTimeout                                        string `yaml:"connect_timeout"`
	PingTimeout                                           string `yaml:"ping_timeout"`
	MaxLifetime                                           string `yaml:"conn_max_lifetime"`
	MaxIdleTime                                           string `yaml:"conn_max_idle_time"`
	connectTimeout, pingTimeout, maxLifetime, maxIdleTime time.Duration
}

var sourceName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// parseSettings 严格校验最终合并配置，错误仅说明字段位置，不回显配置值。
func parseSettings(body []byte) (settings, error) {
	var cfg settings
	decoder := yaml.NewDecoder(bytes.NewReader(body))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return cfg, errors.New("database: 配置必须是合法的 database 对象且不能含未知字段")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return cfg, errors.New("database: 不允许多个 YAML 文档")
	}
	if !sourceName.MatchString(cfg.Default) || len(cfg.Sources) == 0 {
		return cfg, errors.New("database: 必须配置 default 和至少一个具名 sources")
	}
	if _, ok := cfg.Sources[cfg.Default]; !ok {
		return cfg, errors.New("database: default 必须指向已配置的数据源")
	}
	for name, source := range cfg.Sources {
		if !sourceName.MatchString(name) {
			return cfg, errors.New("database: 数据源名称仅允许字母、数字、点、下划线和连字符，并以字母或数字开头")
		}
		if err := source.validate(); err != nil {
			return cfg, fmt.Errorf("database: 数据源[%s]：%w", name, err)
		}
		cfg.Sources[name] = source
	}
	return cfg, nil
}

// validate 校验类型及连接池参数，并为缺省字段填入固定默认值。
func (s *sourceSettings) validate() error {
	if s.Type != "postgresql" && s.Type != "mysql" {
		return errors.New("type 仅支持 postgresql 或 mysql")
	}
	if strings.TrimSpace(s.Host) == "" || strings.ContainsAny(s.Host, "/@?#\r\n\t ") || s.Database == "" || s.Username == "" {
		return errors.New("host、database、username 必填，host 必须是主机名或 IP")
	}
	if s.Port == 0 {
		if s.Type == "postgresql" {
			s.Port = 5432
		} else {
			s.Port = 3306
		}
	}
	if s.Port < 1 || s.Port > 65535 {
		return errors.New("port 必须介于 1 和 65535")
	}
	if s.MaxOpen == 0 {
		s.MaxOpen = 20
	}
	if s.MaxIdle == nil {
		n := min(5, s.MaxOpen)
		s.MaxIdle = &n
	}
	if s.MaxOpen < 1 || *s.MaxIdle < 0 || *s.MaxIdle > s.MaxOpen {
		return errors.New("连接池要求 max_open_conns > 0 且 0 <= max_idle_conns <= max_open_conns")
	}
	var err error
	for _, item := range []struct {
		raw      string
		fallback time.Duration
		dest     *time.Duration
	}{
		{s.ConnectTimeout, 5 * time.Second, &s.connectTimeout},
		{s.PingTimeout, 3 * time.Second, &s.pingTimeout},
		{s.MaxLifetime, 30 * time.Minute, &s.maxLifetime},
		{s.MaxIdleTime, 5 * time.Minute, &s.maxIdleTime},
	} {
		*item.dest = item.fallback
		if item.raw != "" {
			*item.dest, err = time.ParseDuration(item.raw)
		}
		if err != nil || *item.dest <= 0 {
			return errors.New("连接与连接池超时必须是正的 Go duration")
		}
	}
	if s.Type == "postgresql" {
		if s.TLS != "" {
			return errors.New("PostgreSQL 请使用 ssl_mode 字段")
		}
		if s.SSLMode == "" {
			s.SSLMode = "prefer"
		}
		switch s.SSLMode {
		case "disable", "allow", "prefer", "require", "verify-ca", "verify-full":
		default:
			return errors.New("ssl_mode 无效")
		}
	} else {
		if s.SSLMode != "" {
			return errors.New("MySQL 请使用 tls 字段")
		}
		switch s.TLS {
		case "", "false", "true", "skip-verify", "preferred":
		default:
			return errors.New("tls 无效")
		}
	}
	return nil
}

// openEngine 使用对应驱动创建连接池句柄，不在此执行 Ping 或自动同步表结构。
func openEngine(s sourceSettings) (*xorm.Engine, error) {
	var driver, dsn string
	if s.Type == "postgresql" {
		u := url.URL{Scheme: "postgresql", Host: net.JoinHostPort(s.Host, strconv.Itoa(s.Port)), User: url.UserPassword(s.Username, s.Password), Path: "/" + s.Database}
		q := url.Values{"sslmode": {s.SSLMode}, "connect_timeout": {strconv.FormatInt(int64((s.connectTimeout+time.Second-1)/time.Second), 10)}}
		u.RawQuery = q.Encode()
		driver, dsn = "pgx", u.String()
	} else {
		cfg := mysql.NewConfig()
		cfg.User, cfg.Passwd, cfg.Net = s.Username, s.Password, "tcp"
		cfg.Addr, cfg.DBName = net.JoinHostPort(s.Host, strconv.Itoa(s.Port)), s.Database
		cfg.ParseTime, cfg.Timeout, cfg.TLSConfig = true, s.connectTimeout, s.TLS
		driver, dsn = "mysql", cfg.FormatDSN()
	}
	engine, err := xorm.NewEngine(driver, dsn)
	if err != nil {
		return nil, errors.New("连接池创建失败，请检查数据源配置")
	}
	engine.SetLogger(newLogger())
	engine.SetMaxOpenConns(s.MaxOpen)
	engine.SetMaxIdleConns(*s.MaxIdle)
	engine.SetConnMaxLifetime(s.maxLifetime)
	engine.SetConnMaxIdleTime(s.maxIdleTime)
	return engine, nil
}
