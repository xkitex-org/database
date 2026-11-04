// Package database 提供基于 XORM 的具名数据源、上下文事务和就绪检查。
// 引入包或调用 New 都不会读取环境变量、连接数据库或启动后台任务。
package database

import (
	"context"

	"go.chaoxizhongzhi.com/xkitex/database/internal/runtime"
)

// Controller 是供 core 管理的独立接入协议；配置内容只有 database 节。
type Controller interface {
	ConfigureDatabase([]byte) error
	Start(context.Context) error
	ReadinessChecks() map[string]func(context.Context) error
	Database() *Handle
	Close() error
}

// Handle 表示默认或指定数据源；创建上下文会话后执行 XORM 查询。
type Handle = runtime.Handle

// Session 是每次业务操作独立的查询声明，执行时自动选择普通连接或上下文事务。
type Session = runtime.Session

// Transaction 管理一个数据源的事务边界，业务通过 Begin 返回的 ctx 传递事务。
type Transaction = runtime.Transaction

// New 创建独立数据源管理器，配置和连接池创建由 core 在启动时显式执行。
func New() Controller { return runtime.New() }
