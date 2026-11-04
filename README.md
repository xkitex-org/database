# database：具名数据源与业务事务

独立模块 `go.chaoxizhongzhi.com/xkitex/database`，基于 XORM v1.4.3，支持 PostgreSQL（pgx）和 MySQL。当前正式版本为 `v0.0.1`。

## 启动接入

业务导入以下包，无需别名：

```go
import (
    "go.chaoxizhongzhi.com/xkitex/core"
    "go.chaoxizhongzhi.com/xkitex/database"
    "go.chaoxizhongzhi.com/xkitex/monitor"
)

err := x.Server(
    x.WithDatabase(database.New()),
    x.WithMonitor(monitor.New()),
    x.WithService(productservice.RegisterService, productHandler),
    x.WithService(categoryservice.RegisterService, categoryHandler),
)
```

`New` 和选项构造均不创建连接池。core 从 Consul 单次读取、合并配置后，只将 `database` 节交给模块。没有环境变量开关或环境变量覆盖；显式选项决定是否启用。未传 `WithDatabase` 时忽略该配置节。

普通业务方法在启动完成后获取数据源；不要在包级变量或启动前的 Handler 构造阶段缓存 `x.Database()`。进程内只维护一套应用数据源，重复启用或覆盖运行中的管理器会报错。

## 配置

应用入口仍为 `config/GROUP/NAME/application.yaml`。共享连接配置可以通过 imports 引入，入口字段递归覆盖共享值；database 不额外访问 Consul，也不动态更新配置。

```yaml
database:
  default: primary
  sources:
    primary:
      type: postgresql
      host: postgres
      port: 5432
      database: zmall
      username: zmall
      password: "管理员维护的密码"
      ssl_mode: disable
      max_open_conns: 20
      max_idle_conns: 5
      connect_timeout: 5s
      ping_timeout: 3s
      conn_max_lifetime: 30m
      conn_max_idle_time: 5m
    second:
      type: mysql
      host: mysql
      port: 3306
      database: reports
      username: report
      password: "管理员维护的密码"
      tls: "false"
```

`default` 必须明确指定已配置的数据源。类型只接受 `postgresql`、`mysql`，没有按顺序猜默认值、未知名称回退或自动读写分离。主机、库名和用户名必填；密码可以为空。未知字段、重复字段、多个 YAML 文档和无效参数均报错。

省略端口时 PostgreSQL 使用 5432，MySQL 使用 3306。其余默认值与 primary 示例一致；小连接池的默认空闲数为 `min(5, max_open_conns)`。PostgreSQL 的默认 `ssl_mode` 为 `prefer`；示例显式使用 `disable`。MySQL 的 `tls` 使用字符串字段。连接数及超时必须满足校验要求。

## 普通查询与多数据源

```go
db := x.Database()

var products []Product
err := db.Context(ctx).
    Table("product").
    Cols("id", "name").
    Where("name LIKE ?", "%手机%").
    In("id", []int64{1, 2, 3}).
    Desc("id").
    Limit(20, 0).
    Find(&products)

second, err := db.WithDataSource("second")
if err != nil {
    return err
}
err = second.Context(ctx).
    SQL("SELECT id, name FROM product WHERE id = ?", id).
    Find(&products)
```

`Context(ctx)` 每次创建独立查询声明；执行时创建普通 XORM Session，执行后释放。它不是共享的全局请求 Session。可选条件按 XORM 风格用 `if` 添加，本次不额外实现 `WhereIf`。

当前公开查询能力为 `Table`、`Cols`、`Where`、`And`、`Or`、`In`、`NotIn`、`ID`、`Asc`、`Desc`、`OrderBy`、`Limit`、`Select`、`SQL`、`Alias`、`Join`、`GroupBy`、`Having`、`Distinct`、`AllCols`、`ForUpdate`；执行方法为 `Find`、`Get`、`Insert`、`Update`、`Delete`、`Count`、`Exec`。它们使用 XORM 语义，`Get` 返回 `(bool, error)`；更新零值请使用 map 或显式 `Cols`。数据值走参数，动态表名和排序字段由业务白名单选择。

## 通过 ctx 传递事务

Service 显式确定一个业务的事务边界，所有 Repository 和子业务方法传递 `txCtx`：

```go
txCtx, tx, err := x.Database().Begin(ctx)
if err != nil {
    return nil, err
}
defer tx.Close()

if err := s.productRepo.Insert(txCtx, product); err != nil {
    return nil, err
}
// 这里正常执行业务逻辑；两次写入之间事务仍然存续。
relation, err := s.buildCategoryRelation(txCtx, product, req)
if err != nil {
    return nil, err
}
if err := s.categoryRepo.Insert(txCtx, relation); err != nil {
    return nil, err
}
if err := tx.Commit(); err != nil {
    return nil, err
}
return product, nil
```

Repository 使用普通调用形式，无须接收、提交或关闭事务对象：

```go
// Insert 保存商品，自动使用上下文中的事务。
func (r *ProductRepository) Insert(ctx context.Context, product *Product) error {
    _, err := x.Database().Context(ctx).Table("product").Insert(product)
    return err
}
```

- 普通 ctx 使用普通连接；事务 ctx 复用同一个数据源的原生 XORM Session。即使在 second 开启事务，Repository 的默认入口 `x.Database().Context(txCtx)` 也会跟随 second；显式选择其他数据源才报错。
- 通过 `context.WithCancel` 等派生的 ctx 仍保留事务；丢弃 txCtx 会丢失该绑定。
- `Commit` 失败原样返回；提交、回滚后使用旧 ctx 会报错，绝不转成普通连接。
- `Close` 幂等，未提交时回滚；配合 `defer` 覆盖业务提前返回及 panic 路径，panic 仍正常传播。
- 事务内拒绝切换数据源、跨应用管理器和嵌套 Begin；子业务沿用传入 ctx。
- 单条事务内的 SQL 操作串行应用条件，防止原生 Session 的可变状态相互污染；业务仍应在全部子操作完成后提交。
- 不暴露可绕过这些校验的原生 Engine、Session 或事务提交接口。事务只能回滚当前数据库的操作。

## monitor 与停机

同时接入 monitor 时，core 自动挂载 `database.primary`、`database.second` 等逐源 readiness 检查。检查只执行 Ping，遵循 monitor 更短的 timeout 和数据源 ping_timeout。全部数据源通过才就绪，失败及恢复由现有 monitor、core 生命周期及 Consul 独立就绪检查处理，不影响 liveness。

配置错误阻止启动。连接池句柄创建成功不代表数据库连通；连通性检查交给 readiness，数据库暂不可达时应用可以保持运行、等待恢复。没有接入 monitor 时不执行 Ping、不启动周期检查，沿用直接注册可用的约定。自定义 Monitor 如需与 database 同时接入，必须实现 `AddReadiness(string, func(context.Context) error) error`，否则启动报错。

正常停机先撤销可用性及排空 RPC，再停止探针并关闭连接池；未结束事务统一回滚。配置或启动失败也释放已创建资源。所有诊断走 Kitex klog，不记录密码、连接字符串、原始 SQL 或绑定参数，不自动执行建表或迁移。

## 本地验证

跨模块未发布代码使用 `scripts/test-local.py` 在临时目录生成本地模块代理快照，再执行 database、cache/sentinel（存在时）、core、monitor 的测试。源目录不会添加 go.work、replace 或根 go.mod；构建不代表已发布。

```sh
python3 scripts/test-local.py
```

测试使用临时 SQLite 文件验证真实事务语义，SQLite 不开放为配置类型；SQL mock 使用真实 XORM PostgreSQL/MySQL 方言检验参数和失败路径，monitor 测试使用本机临时端口。真实 PostgreSQL/MySQL 服务的验证范围单独报告，不把模拟驱动结果当成真实服务验证。

## 目录

根包只有接入入口和必要类型。配置、连接池、查询、事务及日志实现位于 `internal/runtime`，不依赖 core 或 monitor 模块。
