---
title: rwsplit 真实拓扑测试基建 review
files:
  - pkg/session/state.go
  - test/harness/topology.go
  - test/harness/proxy.go
  - test/rwsplit/rwsplit_test.go
---

## Must Fix

### DetectReservedSQL 把 @@system_var 误判为用户变量
- status: fixed
- file: pkg/session/state.go

`hasUserVariableAssign` 逐字符扫描 `@`，`@@server_id` 的第二个 `@` 后跟字母，
被当作 `@user_var` 命中，导致 `SELECT @@server_id` 在进入路由前就被标记
reserved、pin 到主库。真实拓扑用例 `plain_select_routes_to_replica` 首次复现：
期望从库 server_id ∈ {2,3}，实际返回主库 1。

修复：遇到 `@@` 成对跳过，只把单个 `@` + 标识符视为用户变量。补表驱动用例：
`SELECT @@server_id`、`SELECT @@GLOBAL.max_connections`、
`SET @@SESSION.sql_mode` 均为 false；`SELECT @probe` 仍为 true。

## Should Fix

### adminExec 错误信息包含复制密码
- status: fixed
- file: test/harness/topology.go

`CREATE USER ... IDENTIFIED BY 'repl-pass-123'` 失败时原样拼进 error，CI 失败日志
会泄漏测试密码。改为只报告语句序号，不输出 SQL 文本。

### 复制状态查询瞬时失败被当作终态
- status: fixed
- file: test/harness/topology.go

`START REPLICA` 后立即查 `SHOW REPLICA STATUS` 可能短暂无行；原来直接返回
`replication not configured`，属于启动竞态。改为在 60s 期限内重试，超时错误里
带最后一次 status 错误。

### 启动失败时清理没有超时
- status: fixed
- file: test/harness/topology.go

`StartTopology` 失败路径用 `context.Background()` 清理，Docker 异常时可能挂死。
改为 2 分钟超时上下文。

## Nit

### StartProxy 忽略 Run 返回值
- status: wontfix
- file: test/harness/proxy.go

`Run` 只在 listener 关闭后返回 nil；测试进程内没有需要上报的后台错误。
若后续需要捕获 accept 异常，再引入 error channel。

### 拓扑固定 mysql_native_password
- status: wontfix
- file: test/harness/topology.go

代理当前只实现 native password 认证，拓扑 cnf 固定
`default-authentication-plugin=mysql_native_password` 与现有 passthrough 测试一致。
caching_sha2 全流程支持后再扩展认证矩阵。

### 固定 10s 连接/40s 读写超时
- status: wontfix
- file: test/harness/topology.go

测试拓扑在本机/CI 容器内，固定超时足够；真故障注入（延迟/半包）应由独立的
TCP fault proxy 控制，而不是靠放大超时。
