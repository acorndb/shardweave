---
title: R4 会话 effect 与状态时机 review
files:
  - pkg/mysql/forward.go
  - pkg/sql/statement.go
  - pkg/sql/session_cmd.go
  - pkg/execute/pipeline.go
  - pkg/execute/forward.go
  - pkg/proxy/session.go
  - pkg/backend/stream.go
---

## Must Fix

### ERR 包会把 `InTx` 清成 false
- status: fixed
- file: pkg/execute/pipeline.go

旧 `execCommand` 在 `ForwardResponse` 返回 ERR 时拿到 status=0，随后
`SetInTx(false)`，会错误地清掉事务状态。R4 增加
`mysql.ForwardResponseResult` 的 `serverErr`，`execute` 仅在非 ERR 时更新
`InTx`；`TestPipeline_ServerErrKeepsInTx` 覆盖。

### `SET autocommit=0` 的 effect 必须早于 sticky 判断
- status: fixed
- file: pkg/execute/pipeline.go

如果在 `Handle` 返回后才应用 autocommit effect，deferred sticky 检查看到的仍是
`autocommit=1`，会提前释放 lease。现在 Effect 在 `ForwardTo` 成功、
deferred sticky 检查之前应用；`TestPipeline_EffectAppliedAfterSuccess` 断言
lease 被保留。

## Should Fix

无。

## Nit

### `ForwardPrepareResponse` 不报告 serverErr
- status: wontfix
- file: pkg/mysql/forward.go

prepare 响应不更新 `InTx`、也没有 session Effect，因此不需要 `serverErr`；
若后续 prepare 需要状态跟踪，再加 `ForwardPrepareResponseResult`。

### `ParseSessionEffect` 只识别 USE / SET NAMES / SET autocommit
- status: wontfix
- file: pkg/sql/session_cmd.go

其他 SET（用户变量、sql_mode 等）仍走 reserved/sticky 保守路径，P3/P4 再扩。
R4 不引入未使用的大而全解析器。

### `proxy` 仍在执行前 `DetectReservedSQL`
- status: wontfix
- file: pkg/proxy/session.go

reserved 是保守粘性标记；即使语句失败，保留粘性不会返回错误结果。
把它移到执行成功后会削弱“临时表/锁”安全边界，暂不改。

## 验证

- `go build ./...`：通过
- `go vet ./...`：通过
- `go test -race -short -count=1 ./pkg/mysql/... ./pkg/sql/... ./pkg/execute/ ./pkg/backend/ ./pkg/proxy/ ./pkg/server/`：全绿
- `go test -short -run '^$' -count=1 ./test/...`：编译通过（差分未跑，Docker 不可用）
- 覆盖率：`pkg/sql` 93.4%、`pkg/execute` 87.9%、`pkg/proxy` 73.3%
- `grep parseUse|parseSetNames|parseSetAutocommit pkg/proxy`：无结果
