---
title: 多分片 DISTINCT 归并 review
files:
  - pkg/plan/plan.go
  - pkg/merge/distinct.go
  - pkg/merge/merge.go
  - pkg/execute/dispatch_test.go
  - test/metamorphic/aggregate_test.go
---

本次实现跨片 `SELECT DISTINCT`：各分片先执行 DISTINCT，归并层按规范化行 key
去重。验证边界：plan/merge/execute 单测、完整 `make test-unit`、2 Slice e2e、
single-DB oracle 全绿；sharding e2e 约 69s，oracle 约 27s；golangci-lint
0 issues。

## Must Fix

### 各片 DISTINCT 后仍有跨片重复
- status: fixed
- file: pkg/merge/distinct.go

`SELECT DISTINCT amount` 中同一个 amount 可能在不同 slice 各出现一次，单纯
concat 会重复。修复：`Distinct` 用规范化行 key 去重，跨流只输出首次出现的行；
单测覆盖跨流重复、NULL、空输入。

### 行 key 的数值/日期规范化
- status: fixed
- file: pkg/merge/distinct.go

文本结果不能直接按字节去重：数值/DECIMAL/日期需要按类型规范化，NULL 单独
表示。修复：复用 `encodeRowKey`（int/uint/float/DECIMAL/日期文本），float
归一化 -0；字符串类型在构造阶段 1235。

### DISTINCT 与其他归并语义的组合
- status: fixed
- file: pkg/plan/plan.go, pkg/merge/merge.go

`DISTINCT + ORDER BY/LIMIT`、`DISTINCT + 聚合`、`DISTINCT + GROUP BY` 需要
先定义执行顺序；当前显式 1235，避免结果不确定。

### 输出顺序
- status: fixed
- file: pkg/merge/distinct.go

DISTINCT 未保证顺序；实现按行首次出现顺序输出，oracle 对行排序后比较，不
依赖代理顺序。

## Should Fix

### 字符串/二进制 DISTINCT
- status: wontfix
- file: pkg/merge/distinct.go

需要 collation 权重/规范化 key；当前字符串列 1235。按已定方向暂不引入权重表。

### 去重集合无内存上限
- status: wontfix
- file: pkg/merge/distinct.go

`seen` map 随 distinct 行数增长；尚未设置内存预算/超限错误。正式开放前应与
GROUP BY 一起加配置上限。

### DISTINCT + ORDER BY/LIMIT
- status: wontfix
- file: pkg/plan/plan.go

需要在去重后排序/截断；当前 1235，属于后续 D5.2d/merge operator 组合。

### 聚合 DISTINCT
- status: wontfix
- file: pkg/plan/plan.go

`COUNT(DISTINCT ...)` 需要跨片去重集合而非简单求和；当前 1235。

## Nit

### 行 key 编码
- status: wontfix
- file: pkg/merge/distinct.go

key 使用类型前缀 + 长度前缀，避免数值/字符串拼接冲突；字符串未来接入
collation 时应改为 WeightString。

### 首次出现顺序
- status: wontfix
- file: pkg/merge/distinct.go

输出顺序取决于 target 顺序与分片返回顺序；MySQL 未保证 DISTINCT 顺序，
客户端需要稳定顺序时应显式 ORDER BY（当前该组合仍 1235）。
