---
title: 存量架构清理 review
files:
  - pkg/execute/pipeline.go
  - pkg/execute/dispatch.go
  - pkg/mysql/result.go
  - pkg/mysql/server/conn.go
  - pkg/proxy/serve.go
  - pkg/result/type.go
  - pkg/merge/ordered.go
  - pkg/merge/aggregate.go
  - pkg/plan/plan.go
  - pkg/sql/reserved.go
  - pkg/session/state.go
---

本次是行为不变的结构清理：收掉 execute 的重复打开逻辑、统一 MySQL 写包
helper、集中列类型常量、把 reserved 检测移到 sql、显式化 ORDER BY key 类型，
并把 `proxy/session.go` 改名为 `serve.go`。验证边界：完整 `make test-unit`
全绿；2 Slice 真拓扑 e2e 通过（17.4s）；golangci-lint 0 issues。

## Must Fix

### runOne 与 openDecoded 各自实现 acquire/align/exec
- status: fixed
- file: pkg/execute/pipeline.go, pkg/execute/dispatch.go

两条执行路径重复 `cluster.Get → Slice.Acquire → Align → payloadFor → Exec`，
以后加查询超时、健康检查、重试时很容易只改一处。修复：抽出
`acquireLease` / `execTarget`，runOne 与 dispatch 共用；Align 失败时仅释放
本次新 acquire 的 lease，保持原语义。现有 runOne/dispatch 测试覆盖。

### 写 OK/列头/EOF 的 framing 重复
- status: fixed
- file: pkg/mysql/result.go, pkg/proxy/serve.go, pkg/mysql/server/conn.go, pkg/execute

proxy 与 server 各有一份 `writeOK`；execute 的 explain 和 dispatch 各拼一次
column count/definitions/EOF。修复：`mysql.WriteOK`、`WriteResultHeader`、
`WriteEOF` 统一 framing，proxy/server/execute 复用；`WriteERR` 也改用同一
`writePayload`。新增三个 helper 的单测（正常 + nil front）。

### MySQL 列类型码在 merge 内重复硬编码
- status: fixed
- file: pkg/result/type.go, pkg/merge/ordered.go, pkg/merge/aggregate.go

`0x01/0x03/0xf6/0x20...` 同时存在于 merge 与测试，且 `pkg/mysql` 没有对应
常量。修复：`pkg/result` 增加 `Type*` 与 `FlagUnsigned`（Field.Type 的 wire
值），merge 与测试统一引用，避免协议知识继续分叉。

## Should Fix

### reserved 检测放在 session 包
- status: fixed
- file: pkg/sql/reserved.go, pkg/session/state.go, pkg/proxy/serve.go

`DetectReservedSQL` 只被 proxy 使用，本质是 SQL 文本分类而非会话状态。
已迁到 `pkg/sql`，测试同步移动；session 包只保留 State/StmtTable，
后续 R0 `StatementClass` 可直接替换该启发式。

### ORDER BY key 依赖 `Column=-1` 约定
- status: fixed
- file: pkg/plan/plan.go, pkg/merge/ordered.go

原先用负数列下标表示“按名字解析”，隐式且易错。新增 `OrderKey.Kind`
（`OrderByPosition`/`OrderByName`），merge 按 kind 解析；位置键的零值语义
保持不变。

### proxy 文件名与 session 包混淆
- status: fixed
- file: pkg/proxy/serve.go

`pkg/proxy/session.go` 实际是 COM `Serve` 循环，和 `pkg/session` 容易混淆；
已重命名为 `serve.go`，设计文档中的路径同步更新。

### e2e 单体测试过大
- status: wontfix
- file: test/sharding/sharding_test.go

`TestSingleShardRouting` 已有 ~400 行，覆盖路由/order/limit/aggregate/错误。
拆分需要共享同一拓扑的 subtest 结构，属于测试可读性优化，不影响行为；
当前先保留，避免与本次结构清理混在一起。

### plan.go 仍混合 Plan/MergeSpec/聚合解析
- status: wontfix
- file: pkg/plan/plan.go

329 行仍在可读范围；拆分文件会触发测试文件命名迁移（`merge_spec_test.go`），
收益低于本次 execute/mysql 的重复消除。后续加 AVG/GROUP BY 时再拆。

## Nit

### explain 与 dispatch 的列转换各有一小段
- status: wontfix
- file: pkg/execute/pipeline.go, pkg/execute/dispatch.go

explain 的列是字符串（VAR_STRING），dispatch 是 `result.Field`，列元数据来源
不同，强行共用会引入不必要的中间类型；保留两处小转换。

### `DetectReservedSQL` 仍是子串启发式
- status: wontfix
- file: pkg/sql/reserved.go

用户变量/临时表/命名锁的漏判仍是已知问题；R0/R1 的 StatementClass 落地时
替换，当前只做了位置收敛。

### 依赖图未画全
- status: wontfix
- file: docs/design/module-map.md

4.1 的图只保留主路径，`route/source`、`route/shard`、`session` 的边未全部
展开；模块表和禁止依赖表才是准绳。
