---
title: Plan 契约、StmtTable 与 AST 解析 review
files:
  - pkg/plan/plan.go
  - pkg/session/state.go
  - pkg/session/stmt.go
  - pkg/proxy/session.go
  - pkg/server/server.go
---

## Must Fix

（无）

## Should Fix

### AST 解析只在 sharding 模式启用
- status: wontfix
- file: pkg/proxy/session.go

`ParseAST` 由 `server` 按 `backend.mode` 设置；当前 `mode=sharding` 仍被配置
校验拒绝，所以运行时路径实际不会开启。A5 只落 seam 与单测，B1 打开 sharding 后
才会走解析路径。single/rwsplit 保持零解析开销。

### 解析失败只记录 `stmt.Err`，不拒绝
- status: wontfix
- file: pkg/proxy/session.go

A5 保持行为不变：语法错误仍交给后端返回原生 1064。B1 的 shard Router 必须在
需要 AST 时检查 `stmt.Err`/`stmt.AST`，无法路由时 Reject，不能退回字符串猜测。

### StmtTable 尚未接入 COM_STMT_PREPARE
- status: wontfix
- file: pkg/session/stmt.go

表结构与 Reset 已落地，proxy 仍按原逻辑转发 prepare。B1 单分片 prepared 时
再解析 prepare response、填充每 slice 的 backend stmt id。

### MergeSpec/TxPolicy 尚未被 Builder 使用
- status: wontfix
- file: pkg/plan/plan.go

A5 只定契约；B2/B1 分别由 rewrite/merge 和事务策略填充。当前 zero value 不影响
single/rwsplit。

## Nit

### `StmtTable` 无并发保护
- status: wontfix
- file: pkg/session/stmt.go

会话命令循环单 goroutine；与 `session.State` 一致。分片 dispatch 汇总在命令
goroutine 内完成，不需要额外锁。
