---
title: 分片聚合 single-DB oracle review
files:
  - test/metamorphic/aggregate_test.go
  - test/harness/sharded.go
  - test/harness/topology.go
  - test/sharding/replica_test.go
---

本次新增“分片聚合 vs 单库”独立 oracle，并保留/整理真实复制拓扑验证。
验证边界：`test/metamorphic`（Docker，约 26s）、`test/sharding` 两套 e2e
（约 69s）通过；完整 `make test-unit` 全绿；golangci-lint 0 issues。

## Must Fix

### 聚合正确性只靠手写期望值
- status: fixed
- file: test/metamorphic/aggregate_test.go

原有 e2e 用少量已知数据断言 `COUNT/SUM/MIN/MAX` 的固定值，没有独立 oracle；
如果手算期望或 schema 理解错了，测试会和实现一起错。修复：2 个 master-only
slice + 1 个独立 MySQL oracle，同一份 24 行数据（含 NULL、负数）按
`user_id % 2` 分布到分片，oracle 保存全量；逐条比较聚合查询的列名与值。
比较时 NULL 单独判断，数值用 big.Rat 归一化（COUNT/SUM 的文本 scale 差异）。

### 真实复制拓扑没有读从/写主验证
- status: fixed
- file: test/harness/sharded.go, test/sharding/replica_test.go

`StartShardedTopology` 只有 master；新增
`StartShardedTopologyWithReplicas(ctx, slices, replicas)`，每片独立 GTID
复制、从库 read_only。`TestShardedReplicas` 验证 `SELECT @@server_id` 命中
s0 replica、DDL/数据复制、写主读从、写后 `Sync` 再读、聚合/ORDER/LIMIT/
prepared 和错误拒绝。

## Should Fix

### metamorphic 只覆盖聚合
- status: wontfix
- file: test/metamorphic/aggregate_test.go

ORDER BY/LIMIT OFFSET 仍由固定 fixture e2e 验证；下一步应加同一 oracle 下的
排序/分页查询，以及随机数据生成（含重复键、边界值、不同基数）。

### 数据量为固定 24 行
- status: wontfix
- file: test/metamorphic/aggregate_test.go

固定数据便于复现，但覆盖不了大基数/倾斜分布。后续接 `test/randgen` 或仓内
生成器，种子化扩大数据量；聚合结果仍与单库 oracle 对比。

### 复制拓扑测试成本
- status: wontfix
- file: test/sharding/replica_test.go

4 个容器约 54s；master-only 快速 e2e 保留。CI 分层：PR 跑 master-only，
merge/nightly 跑复制拓扑。

## Nit

### oracle 用单实例分片 harness
- status: wontfix
- file: test/metamorphic/aggregate_test.go

`StartShardedTopology(ctx, 1)` 起一个单实例作为 oracle，复用了容器启动/清理
逻辑；它同时携带未使用的 sharding 规则，但不影响查询。若后续 oracle 需求
增多，再抽 `StartOracle`。

### 数值比较归一化
- status: wontfix
- file: test/metamorphic/aggregate_test.go

`sameCell` 对可解析数值用 big.Rat 比较，允许 `250` 与 `250.0000` 等价；
字符串/列名仍要求完全一致。这是刻意避免 DECIMAL 文本 scale 差异造成假失败。
