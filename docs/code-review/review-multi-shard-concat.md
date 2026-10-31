---
title: 多分片 SELECT scatter + concat review
files:
  - pkg/route/shard/router.go
  - pkg/plan/plan.go
  - pkg/execute/pipeline.go
  - pkg/execute/dispatch.go
  - pkg/session/state.go
  - test/sharding/sharding_test.go
---

本次变更（D4.0）在分片查询链路上新增：无键/跨片 `IN` 的 scatter 路由、
`MultiShard + TxNone` 计划、`execute.dispatch` 的逐片列头校验与 concat 写回，
以及 prepared statement pin 的独立释放。验证边界：变更包聚焦单测
（`route/shard`、`plan`、`execute`、`session`）全绿；2 Slice 真拓扑 e2e
（Docker）通过；`go vet` 与 golangci-lint v2.15.0 干净。未跑全量差分/randgen，
按 AGENTS 需用户显式批准。

## Must Fix

### 跨片 IN 的组合语句绕过 scatter 形状检查
- status: fixed
- file: pkg/route/shard/router.go

初版只在“键约束不完整”时调用 `checkScatterSelect`。`WHERE user_id IN (1,2)
ORDER BY id` 的键值完整但映射到两个 slice，仍会生成 `MultiShard`，concat 后
顺序与 `ORDER BY` 语义不符（静默错误结果）。修复：只要最终 `len(slices) > 1`
就执行形状检查；单分片键（含同片 IN）仍允许排序/聚合，由该分片 MySQL 处理。
单测覆盖 `cross_shard_in_order_by`、`cross_shard_in_aggregate` 与
`keyed_single_order_by`。

### prepared pin 永不解释放，阻塞后续多分片查询
- status: fixed
- file: pkg/execute/pipeline.go, pkg/session/state.go

D2 用 `MarkReserved()` 让 prepared 会话持有后端连接，但 `COM_STMT_CLOSE`
只删除 StmtTable 条目，`reserved` 一直为 true。结果是同一连接只要 prepare 过
一次，后续所有 scatter 查询都被 1235 拒绝。修复：新增独立的 `stmtPinned`
状态（不污染 reserved 的 SQL 分类语义），最后一个 `COM_STMT_CLOSE` 清 pin 并
在 `runOne` 的 deferred sticky 检查中释放 lease；`Reset` 同步清理。
单测覆盖 pin/释放/Reset；e2e 覆盖“prepared 未关闭时 scatter 1235、关闭后恢复”。

### 失败路径把未读完的后端连接放回池
- status: fixed
- file: pkg/execute/dispatch.go

某片列元数据不一致或读行中途失败时，stream 里仍有未消费的 packet。若按普通
`Release`（COM_RESET_CONNECTION）还池，残留 packet 会被下一次请求读到，造成
协议错序。修复：失败 cleanup 统一 `Discard`（关闭连接）；成功路径才
`Release`。所有列头在写前端之前完成解码与一致性校验，因此元数据错误不会
产生部分结果集。单测 `MetadataMismatch`/`RowError`/`NoColumns` 断言连接被关闭。

### shard.New 接受重复/空 slice 名，scatter 会重复执行
- status: fixed
- file: pkg/route/shard/router.go

`Rule.Slices` 重复时（例如 `["s0","s0"]`），scatter 会生成两个相同 target，
对同一连接连续执行两次并破坏结果流；空 slice 名则会在运行时 `cluster.Get`
失败。配置层已校验，但 `shard.New` 是导出入口且被单测直接使用。修复：在
`New` 中拒绝空名与重复名，并补表驱动用例。

## Should Fix

### 多分片结束符 warnings 固定为 0
- status: wontfix
- file: pkg/execute/dispatch.go

concat 结果的 OK/EOF 结束符没有汇总各分片 `warning_count`。要正确汇总需要
`decodedStream` 暴露每个分片的终止状态，且 `SHOW WARNINGS` 在产品上已明确
不支持（1235），客户端无法消费对应 warning 详情。当前在设计中记录为已知
限制，D6 引入 merge 层时再统一处理终止符元数据。不修的理由：单独汇总一个
无法查询详情的计数没有可操作价值，反而增加协议状态面。

### dispatch 防御分支缺少用例
- status: fixed
- file: pkg/execute/dispatch.go

补了非 COM_QUERY、非空 MergeSpec、零 target、无列、行宽不符、不支持值类型
的表驱动用例，避免后续接线时静默走错路径。

## Nit

### scatter 对子查询中的聚合保守拒绝
- status: wontfix
- file: pkg/route/shard/router.go

`selectNeedsMerge` 会遍历整个 SELECT（含标量子查询），因此
`SELECT (SELECT COUNT(*) FROM t2) FROM orders` 这类本可在单片内完成的查询在
scatter 时被 1235。P0 选择保守拒绝而不是做子查询归属分析；D5 做 AST 改写
时可收窄到“当前查询块”的投影/HAVING。

### 逐片串行执行
- status: wontfix
- file: pkg/execute/dispatch.go

P0.5 顺序 acquire/exec，延迟随分片数线性增长。并发 dispatch、取消与失败
传播属于 D7；当前实现优先保证 lease/stream 生命周期简单可证明。

### 结果行顺序等于 target 顺序
- status: wontfix
- file: pkg/execute/dispatch.go

无 `ORDER BY` 的 SQL 本身不保证行序，concat 按规则配置的 slice 顺序输出。
需要稳定顺序的客户端应使用单分片键或等待 D5 的 ORDER BY 下推；文档已注明。
