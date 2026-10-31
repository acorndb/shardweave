---
title: 多分片 LIMIT OFFSET 改写 review
files:
  - pkg/plan/plan.go
  - test/sharding/sharding_test.go
---

本次增量支持 `LIMIT m, n` / `LIMIT n OFFSET m`：plan 用 AST restore 把每个
target 的 SQL 改写为 `LIMIT m+n`，`merge.Limit` 在归并后全局跳过 m 行。
验证边界：plan/execute/merge 聚焦单测、完整 `make test-unit`、2 Slice 真拓扑
e2e 全绿；golangci-lint 0 issues。未跑全量差分/randgen，按 AGENTS 需用户
显式批准。

## Must Fix

（无）

## Should Fix

### 改写不能永久修改调用方 AST
- status: fixed
- file: pkg/plan/plan.go

`SelectStmt` 没有 Clone；若直接覆盖 `sel.Limit` 后 restore，语句 AST 会残留
per-shard 的 `LIMIT`，影响同请求后续使用（例如 EXPLAIN/日志/重试）。
`restoreSelectLimit` 保存旧 `Limit` 指针并在 defer 中恢复；单测在 Build 后
再次 `sql.ParseLimit(st.AST.(*ast.SelectStmt).Limit)`，断言仍是原始
`OFFSET 2 / COUNT 3`。

### OFFSET+COUNT 溢出
- status: fixed
- file: pkg/plan/plan.go

`offset+count` 可能超过 int64；若不检查，改写的 LIMIT 会变成负数/错误值。
修复：`count > math.MaxInt64-offset` 时 1235；单测覆盖
`OFFSET 9223372036854775807`。

### 每片先丢 OFFSET 会少读行
- status: fixed
- file: pkg/plan/plan.go

若每个分片仍执行原 `LIMIT m, n`，concat/merge 再全局跳过 m 行会少读；改写为
`LIMIT m+n` 后，归并层能凑够 m+n 行再截断。e2e 用
`ORDER BY user_id LIMIT 1 OFFSET 1` 验证返回第二行，以及
`LIMIT 5 OFFSET 10` 返回 0 行。

## Nit

### AST restore 会丢注释/优化器 hint
- status: wontfix
- file: pkg/plan/plan.go

`SelectStmt.Restore` 只保留语法结构：`/*master*/`、`/*+ MAX_EXECUTION_TIME */`
等注释/提示不会进入改写后的 SQL。路由角色在 rewrite 前已由 raw SQL 决定，
`MAX_EXECUTION_TIME` 属于尚未实现的查询超时能力；P0 接受该限制。需要保留
hint 时应改为基于 AST 的 hint 保留/文本区间替换。

### 改写后 SQL 的格式与字面量形式
- status: wontfix
- file: pkg/plan/plan.go

Restore 使用 `DefaultRestoreFlags`（反引号标识符、单引号字符串、大写关键字），
语义等价但文本与用户输入不同；`EXPLAIN ROUTE` 会展示改写后的 SQL。若需要
“原样显示”，应在 `Target` 上区分 `SQL` 与 `DisplaySQL`，当前不做。
