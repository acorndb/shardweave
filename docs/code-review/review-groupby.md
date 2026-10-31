---
title: 多分片 GROUP BY 归并 review
files:
  - pkg/plan/plan.go
  - pkg/merge/group.go
  - pkg/merge/merge.go
  - pkg/execute/dispatch_test.go
  - test/metamorphic/aggregate_test.go
---

本次实现多分片 `GROUP BY`：plan 校验分组列/聚合投影并复用 AVG 隐藏列改写，
merge 用 map 累积每个 group 的部分聚合。验证边界：plan/merge/execute 单测、
完整 `make test-unit`、2 Slice e2e、single-DB oracle 全绿；sharding e2e 约
69s，oracle 约 28s；golangci-lint 0 issues。

## Must Fix

### 分组列必须在结果集中
- status: fixed
- file: pkg/plan/plan.go

`SELECT COUNT(*) FROM orders GROUP BY user_id` 没有把分组键带回，归并层无法
区分 group。修复：显式 1235，要求 GROUP BY 列出现在 select list；位置/别名
按 `selectFieldIndex` 解析，未选列/表达式拒绝。

### 跨片部分分组不能直接拼接
- status: fixed
- file: pkg/merge/group.go

同一个 group 可能出现在多个分片（例如 `GROUP BY amount`）。修复：`Group`
用规范化 group key 累积 `COUNT/SUM/MIN/MAX/AVG`，同 key 合并后输出一次；
AVG 复用隐藏 SUM/COUNT 列。

### AVG 隐藏列与输出投影
- status: fixed
- file: pkg/plan/plan.go, pkg/merge/group.go

GROUP BY + AVG 时隐藏列追加在原 select 之后，`Group` 只读取隐藏 SUM/COUNT，
输出仍按 `Output` 投影到原始列；列名/scale 由原 AVG 字段元数据决定。

### 输出顺序不确定
- status: fixed
- file: pkg/merge/group.go

MySQL 未保证 GROUP BY 顺序；`Group` 按 group 首次出现顺序输出，测试/oracle
对分组结果做排序后比较，不依赖代理顺序。

## Should Fix

### 字符串/二进制分组键
- status: wontfix
- file: pkg/merge/group.go

需要 collation 权重表/规范化 key；当前字符串分组 1235。按已定方向先不做
权重表，客户端使用数值/日期分组或显式拼接键。

### 无分组内存上限
- status: wontfix
- file: pkg/merge/group.go

`Group` 的 map 随分组数增长，尚未设置内存上限/超限错误；当前 P0 数据量与
测试场景可接受。GROUP BY/DISTINCT 正式开放前应加配置上限（如 64 MiB 或
最大组数）与稳定错误码。

### GROUP BY + ORDER BY/LIMIT
- status: wontfix
- file: pkg/plan/plan.go

需要在分组后再排序/截断；当前显式 1235，属于 D5.2d 后续。

### HAVING / 聚合 DISTINCT / DISTINCT
- status: wontfix
- file: pkg/plan/plan.go

HAVING 需要聚合后过滤；DISTINCT 需要去重算子；当前均 1235。

## Nit

### group key 编码
- status: wontfix
- file: pkg/merge/group.go

数值/无符号/浮点/DECIMAL/日期文本分别编码并长度前缀；float 归一化 -0。
字符串若未来接入 collation，应改用 WeightString 而不是原始字节。

### 分组输出值代表
- status: wontfix
- file: pkg/merge/group.go

同一 group 的分组列原始值取首次出现的行；MySQL 对等价值（如未来 PAD SPACE
字符串）返回哪个代表值本身不保证，当前 bin/数值场景无歧义。
