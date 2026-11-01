---
title: 前端连接 ID 与 server status review
files:
  - pkg/mysql/const.go
  - pkg/mysql/server/conn.go
  - pkg/session/state.go
  - pkg/sql/session_func.go
  - pkg/sql/classify.go
  - pkg/execute/pipeline.go
  - pkg/execute/dispatch.go
  - pkg/server/connid.go
  - pkg/server/server.go
  - pkg/server/serve.go
---

## Must Fix

### PING 在事务中返回固定的 autocommit 状态
- status: fixed
- file: pkg/server/serve.go

`COM_PING` 原先恒写 `0x0002`，客户端在事务中看到的 `SERVER_STATUS` 与 MySQL 不一致。改为
`mysql.ServerStatusWord(st.Autocommit(), st.InTx())`；红测覆盖 autocommit、事务中、
autocommit=0 三种组合，并在 `COM_RESET_CONNECTION` 后断言回到 `0x0002`。

### 连接 ID 固定为 1，CONNECTION_ID() 未被代理接管
- status: fixed
- file: pkg/server/connid.go, pkg/mysql/server/conn.go, pkg/session/state.go, pkg/sql/session_func.go, pkg/sql/classify.go, pkg/execute/pipeline.go

握手字段、`CONNECTION_ID()` 与日志必须共用一个前端会话 ID，否则客户端诊断、监控和未来的
KILL 都指向错误对象。新增 `connIDAllocator`（从 1 开始，32 位回绕跳过 0 与在用 ID），
`Options.ConnectionID` 传入握手，`session.State` 保存并由 `rewriteSessionFunc` 把
`SELECT CONNECTION_ID()` 改写为 `CAST` 字面量。无法改写的形态（表达式、prepared）在
`UnsupportedReason` 中显式拒绝，与 `LAST_INSERT_ID`/`ROW_COUNT` 的既有策略一致。

### KILL 未拦截，可能误杀池化后端
- status: fixed
- file: pkg/sql/classify.go

代理接管连接 ID 后，`KILL <id>` 不再对应后端线程；继续透传会杀掉可能正服务其他前端
的池化连接。分类层现在拒绝 `KILL`（首关键字判定），并在 pipeline 测试中断言不打开任何
后端连接。

### EXPLAIN ROUTE 的合成终止包缺少 IN_TRANS
- status: fixed
- file: pkg/execute/pipeline.go

`writeTextResult` 原先固定 `0x0002`。`EXPLAIN ROUTE` 是代理本地合成结果，事务中应反映
`SERVER_STATUS_IN_TRANS`；改为传入 `ServerStatusWord` 计算的 status，并加红测。

### 分片归并中段错误直接断开连接
- status: fixed
- file: pkg/execute/dispatch.go

结果头已写给前端后，`merged.Next()` 的传输/解码错误只会向上返回，`Serve` 认不出
MySQL 错误而关闭连接，客户端拿到半截结果集后看到 EOF。现在保留分片返回的
`*errs.MySQL` 原错误码，其余错误映射为 1105/HY000；`handleExecError` 写 ERR 后保留
会话。新增 `TestDispatch_RowMySQLServerError` 锁定真实错误码不被 1105 覆盖。

### max_connections 超限只关闭 TCP
- status: fixed
- file: pkg/server/server.go, pkg/server/serve.go

accept 阶段超限时仅 `slog.Warn` 后 `conn.Close()`，客户端无法区分连接数满与网络故障。
现在由 `rejectTooManyConnections` 以 ERR 首包返回 1040/08004，并带写超时；拒绝放在
goroutine 中，避免慢客户端阻塞 accept 循环。`TestServer_MaxConnections` 改为断言首包
是 ERR 且错误码为 1040。

## Should Fix

### 直接调用 Serve 时连接 ID 为 0
- status: fixed
- file: pkg/mysql/server/conn.go, pkg/server/serve.go

`Serve` 被测试与差分用例直接调用，没有 `Server` 分配器。`NewConnWithOptions` 将 0 回退为
1（MySQL 不分配 0），`Serve` 再从 `Conn.ConnectionID()` 回填 `session.State`，保证
握手、`CONNECTION_ID()` 与状态三者一致。

### 散射合并 EOF 的动态状态
- status: wontfix
- file: pkg/execute/dispatch.go

原判断认为多分片合并的终止包需要 `IN_TRANS`。红测证明散射在事务/粘性会话中会被
`plan.Builder` 直接拒绝（1235），因此该终止包不可能处于事务中，保持 `0x0002` 正确。
已补注释与 `TestDispatch_ConcatRejectedInTransaction` 固化该不变量，避免后续误改。

### 连接 ID 回绕与在用 ID 复用
- status: fixed
- file: pkg/server/connid.go

对齐 WL#7293：回绕时跳过 0 和仍在服务的前端 ID；连接关闭时从在用集合移除。重启后
计数器归零可接受（MySQL 同样不保证跨重启唯一）。单测覆盖顺序分配、释放后不立即复用、
回绕跳过 0/在用 ID、并发唯一性。

## Nit

### 握手状态字仍为固定 0x0002
- status: wontfix
- file: pkg/mysql/server/conn.go

初始会话恒为 autocommit=1、无活动事务，`0x0002` 正是 MySQL 初始握手的值；语义正确，
不引入动态计算。字面量已替换为 `mysql.ServerStatusAutocommit`。

### 可执行注释中的 KILL 仍会透传
- status: wontfix
- file: pkg/sql/classify.go

`/*! ... KILL ... */` 会被 token 层归入 executable comment，`First` 为空，绕过首关键字
检查。这是既有的 executable-comment 策略缺口（所有被拒语句都有同类形态），修复需要
单独决定是否拒绝全部可执行注释，会误伤 mysqldump 风格语句；本次不扩大范围。

### prepared 形式的 CONNECTION_ID() 被拒绝
- status: wontfix
- file: pkg/sql/classify.go

与 `LAST_INSERT_ID()`/`ROW_COUNT()` 一致：prepared 语句不做改写，保留原函数引用意味着
会落到后端并返回后端 ID，因此显式拒绝优于静默错误。
