---
title: 多分片 ORDER BY 有序归并 review
files:
  - pkg/merge/ordered.go
  - pkg/plan/plan.go
  - pkg/route/shard/router.go
  - pkg/execute/dispatch.go
  - test/sharding/sharding_test.go
---

本次增量实现多分片 `ORDER BY`：各分片执行原排序，`pkg/merge.Ordered` 用
k 路堆归并已排序的 `result.Stream`；同时支持 `ORDER BY ... LIMIT n`
（OFFSET=0）。验证边界：`pkg/merge`/`plan`/`route`/`execute` 聚焦单测、
完整 `make test-unit`、2 Slice 真拓扑 e2e 全绿；golangci-lint 0 issues。
未跑全量差分/randgen，按 AGENTS 需用户显式批准。

## Must Fix

### ORDER BY 类型/列校验必须在写列头之前
- status: fixed
- file: pkg/execute/dispatch.go, pkg/merge/ordered.go

如果先写列数/列定义再发现“排序列缺失”或“字符串列不支持”，客户端会收到
半截结果集后断连。修复：`merge.Ordered` 在任何 packet 写出前解析列名/位置、
校验字段类型并构建比较器；失败返回 `*errs.MySQL`（1235），dispatch 丢弃
后端连接后由 proxy 正常写 ERR。单测断言此时前端 packet 为空。

### 字符串/JSON 等 collation 相关排序不能按字节比较
- status: fixed
- file: pkg/merge/ordered.go

MySQL 字符串排序依赖 collation（大小写、重音、语言规则），按 UTF-8 字节序
会静默返回不同顺序。修复：只支持整数/无符号整数、FLOAT/DOUBLE、DECIMAL、
DATE/DATETIME/TIMESTAMP（规范文本）比较；其他类型显式 1235。后续需要
charset/collation 元数据再开放字符串排序。

### NULL 排序语义
- status: fixed
- file: pkg/merge/ordered.go

MySQL 规定 NULL 在 ASC 时最前、DESC 时最后。比较器先按 ASC 语义比较
（NULL 小于非 NULL），DESC 时统一取反，保证两种方向都正确；表驱动单测覆盖
两片各含 NULL 的合并。

### DESC 归并要求每个分片按 DESC 返回
- status: fixed
- file: pkg/plan/plan.go, pkg/merge/ordered.go

`Ordered` 假设每条上游流已按目标键有序；`ORDER BY x DESC` 时若分片仍按 ASC
返回，k 路堆会产生错误交错（单测最初用 ASC 流模拟 DESC 已复现）。当前实现
把原 SQL（含 DESC）原样发给每个分片，因此上游顺序与归并方向一致；测试改为
分别提供 ASC/DESC 有序流。

## Should Fix

### ORDER BY 隐藏列（未投影列）暂不支持
- status: wontfix
- file: pkg/merge/ordered.go

`SELECT amount FROM orders ORDER BY user_id` 需要把 user_id 作为隐藏列带回，
否则归并层拿不到排序键。当前按输出名/OrgName 解析，找不到即 1235；e2e 覆盖。
隐藏列注入属于 D5.1 的 rewrite 工作（同时可减少对 `SELECT *` 的字段推断）。

### LIMIT OFFSET 仍需 per-shard 改写
- status: fixed
- file: pkg/plan/plan.go

`LIMIT m, n` 需要每片改写成 `LIMIT m+n` 再全局跳过 m 行。D5.1b 已实现：
plan 用 AST restore 生成 per-target SQL，`merge.Limit` 负责全局跳过；单测与
e2e 覆盖。

### 表达式排序（`ORDER BY amount+1`）
- status: wontfix
- file: pkg/plan/plan.go

表达式同样需要隐藏列/表达式下推；当前只接受列名或位置（`ORDER BY 1`），
其他 1235。计划在 D5.1 的 rewrite 中扩展。

## Nit

### DECIMAL 每行分配 big.Rat
- status: wontfix
- file: pkg/merge/ordered.go

DECIMAL 比较用 `big.Rat` 保证精度，行读取时每个排序键分配一次；P0 可接受。
后续可缓存解析结果或按 scale 用 int64/128 比较。

### 相同排序键的 tie-break
- status: wontfix
- file: pkg/merge/ordered.go

键相等时按上游流下标决定顺序，保证确定性；MySQL 对相同排序键不保证顺序，
因此不构成语义差异。
