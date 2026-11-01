---
title: 连接池归还生命周期（不再无条件 Reset）review
files:
  - pkg/backend/pool.go
  - pkg/backend/stream.go
  - pkg/backend/decode.go
  - pkg/execute/pipeline.go
  - pkg/execute/dispatch.go
  - pkg/sql/scanner.go
---

## Must Fix

### 每次归还都发 COM_RESET_CONNECTION
- status: fixed
- file: pkg/backend/pool.go

原实现 `Release` 无条件 `b.Reset(ctx)`：每查询固定多 1 RTT，且 `COM_RESET_CONNECTION`
会把会话字符集重置回服务端默认（D5 oracle 已实测：连接从 45 变为 255），使握手阶段的
字符集固定失效。

修复：`Release` 只负责把「可复用」连接放回 idle；不可复用的连接由调用方走 `Discard`。
判定规则：

| 条件 | 动作 |
|---|---|
| `Drained()==false`（结果集未读完） | Discard |
| Exec/Align/Forward 出错 | Discard |
| 脏语句（用户变量写、非 autocommit 的 SET SESSION、临时表、锁、多语句、Exec 注释、扫描失败） | Discard |
| 归还时会话仍 sticky（事务 / autocommit=0 / reserved / stmt pin） | Discard |
| 以上都不成立 | 归还，不发 Reset |

对照：`database/sql` + `go-sql-driver` 的 `ResetSession` 只做存活检查；kingshard 不做
任何清理；Gaea 做定向清理（rollback + autocommit）并在取用时重放会话变量；
pgxpool 用 `AfterRelease` 钩子返回 bool 决定复用或销毁；HikariCP 用 dirty bits 只恢复
自己跟踪的状态。没有实现每次全量 reset 的。

### 预编译 SQL 的 `?` 占位符无法扫描
- status: fixed
- file: pkg/sql/scanner.go

`Scan` 把 `?` 视为非法字节，导致预处理语句的 SQL 文本触发 `ScanFailed`。后果：
`dirtyForReuse` 将其判为脏 → 连接被丢弃（本次测试暴露），并且若其他路径读取
`Class.WantSlave/Reserved` 也会得到保守但错误的结果。

修复：`?` 归入 `TokPunct`，新增扫描用例。

### dispatch 并发调用 EnsureClass 的数据竞争
- status: fixed
- file: pkg/execute/dispatch.go

`execOnLease` 在每个分片 goroutine 中调用 `stmt.EnsureClass()`，首次调用会写
`Statement.Class/classReady`，`-race` 报 DATA RACE。

修复：`dispatch` 在启动分片 goroutine 之前先调用一次 `stmt.EnsureClass()`（生产路径
`plan.Build` 已计算过，这里是防御性保证）；此后各 goroutine 只读。

## Should Fix

### 会话变量白名单（B2）未落地
- status: wontfix
- file: pkg/execute/pipeline.go

白名单外的 `SET SESSION` 目前是「执行后丢弃连接」，而不是返回 1235；`time_zone` 也尚未
进入 `Align` 重放。本改动已保证**不会泄漏**（脏连接一律丢弃），拒绝与重放属于独立的
后续小步，需要扩展 `AlignSettings` 与差分语料。

### `Conn.Reset` 现在未被连接池使用
- status: wontfix
- file: pkg/backend/conn.go

保留该方法：`conn_test.go` 覆盖其协议行为，后续若需要显式重置（如运维命令）仍可复用；
删除会波及 `Conn` 接口与全部测试替身，收益仅为减少一个未调用的导出方法。

### `SET GLOBAL` 被判定为可复用
- status: wontfix
- file: pkg/execute/pipeline.go

全局变量修改不影响连接级隔离（不会跨会话泄漏），因此不触发 Discard。若后续要对全局
写入做策略限制，应在 `Class.UnsupportedReason` 层处理，而不是池生命周期。

## Nit

### `Drained()` 仅在转发后有意义
- status: wontfix
- file: pkg/backend/stream.go、pkg/backend/decode.go

`rawStream.Drained()` 在 `ForwardTo` 之前恒为 false，语义正确（未消费）；已在方法注释
说明。`noReplyStream` 恒为 true。

### 非生产流默认视为已排空
- status: wontfix
- file: pkg/execute/pipeline.go

`streamDrained` 对未实现 `Drained()` 的流返回 true，避免测试替身与合成流被迫实现该
接口；生产流的 `rawStream`/`decodedStream` 均实现了追踪。

### `Pool.Release` 仍返回 error 但恒为 nil
- status: wontfix
- file: pkg/backend/pool.go

保留签名以兼容现有调用点与测试；后续如引入「释放失败」语义（例如强制 reset 模式）可
直接使用该返回值。
