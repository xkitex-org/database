package runtime

import (
	"sync/atomic"

	"github.com/cloudwego/kitex/pkg/klog"
	"xorm.io/xorm/log"
)

// logger 将 XORM 内部诊断统一交给 klog，不记录原始 SQL、绑定参数或驱动错误正文。
type logger struct{ level atomic.Int32 }

// newLogger 安装固定安全日志策略，避免 XORM 默认向 stdout 直接输出。
func newLogger() *logger { l := &logger{}; l.SetLevel(log.LOG_WARNING); return l }

// BeforeSQL 保留接口但不输出业务 SQL。
func (*logger) BeforeSQL(log.LogContext) {}

// AfterSQL 保留接口但不输出绑定参数。
func (*logger) AfterSQL(log.LogContext) {}

// Debugf 将内部诊断归入统一调试日志，不回显可能含数据的可变参数。
func (l *logger) Debugf(string, ...any) {
	if l.Level() <= log.LOG_DEBUG {
		klog.Debug("database: XORM 调试诊断")
	}
}

// Infof 将内部信息归入统一日志。
func (l *logger) Infof(string, ...any) {
	if l.Level() <= log.LOG_INFO {
		klog.Info("database: XORM 内部信息")
	}
}

// Warnf 输出安全的 XORM 警告，业务调用者仍能获得原始操作错误。
func (l *logger) Warnf(string, ...any) {
	if l.Level() <= log.LOG_WARNING {
		klog.Warn("database: XORM 内部警告")
	}
}

// Errorf 输出安全错误，不回显 SQL、参数或连接字符串。
func (l *logger) Errorf(string, ...any) {
	if l.Level() <= log.LOG_ERR {
		klog.Error("database: XORM 操作失败，具体错误已返回业务调用方")
	}
}

// Level 返回当前内部诊断级别。
func (l *logger) Level() log.LogLevel { return log.LogLevel(l.level.Load()) }

// SetLevel 原子更新内部诊断级别。
func (l *logger) SetLevel(level log.LogLevel) { l.level.Store(int32(level)) }

// ShowSQL 保留 XORM 接口，当前模块不开放包含业务参数的 SQL 输出。
func (*logger) ShowSQL(...bool) {}

// IsShowSQL 禁止 XORM 的原始 SQL 与参数日志。
func (*logger) IsShowSQL() bool { return false }
