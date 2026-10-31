---
title: 分片复制拓扑 harness 与真实能力验证 review
files:
  - test/harness/sharded.go
  - test/harness/topology.go
  - test/harness/sharded_test.go
  - test/sharding/replica_test.go
---

本次把分片 e2e 从 master-only 扩展到“每片 master + GTID 从库”的真实拓扑，
并新增 `TestShardedReplicas` 验证已实现能力。验证边界：完整 `make test-unit`
全绿；`./test/sharding/`（master-only + 复制拓扑两套，约 69s）通过；
golangci-lint 0 issues。

## Must Fix

### 分片拓扑只有 master，读从/复制行为没有被真实覆盖
- status: fixed
- file: test/harness/sharded.go

`StartShardedTopology` 只起 N 个独立 master，sharding 下的 `RoleSlave` 实际
总是回落到 master，无法验证读从、复制延迟和误写从库。修复：
`StartShardedTopologyWithReplicas(ctx, slices, replicas)` 在同一个 Docker
network 内为每个 slice 起 master + GTID 从库，从库追平后 `read_only=ON`；
`ProxyConfig` 为每个 slice 填 `slaves`；`ShardedTopology.Sync` 等待所有分片
复制追平。

### 新增测试里沿用了已过期的 LIMIT OFFSET 拒绝断言
- status: fixed
- file: test/sharding/replica_test.go

D5.1b 已支持 `LIMIT 1,1`，初版复制拓扑测试仍断言 1235。已改为断言返回 1 行，
避免测试把已实现能力误判为不支持。

## Should Fix

### 复制拓扑测试耗时
- status: wontfix
- file: test/sharding/replica_test.go

2 slice × (1 master + 1 replica) 共 4 个容器，单测约 54s；master-only e2e
仍保留给快速回归。后续可把两套测试共享拓扑或按 CI 分层（PR 跑 master-only，
merge/nightly 跑复制拓扑）。

### 复制同步需要测试显式调用
- status: wontfix
- file: test/harness/sharded.go

生产是异步复制，代理不做 read-after-write；测试在写后调用 `tp.Sync` 才读从库，
这是刻意的确定性处理。后续做 D4.1 从库健康/延迟时，再补“延迟从库/停止复制”
的故障用例。

### 没有从库故障/延迟场景
- status: wontfix
- file: test/sharding/replica_test.go

当前只验证正常复制链路；`STOP REPLICA`、杀从库、延迟从库属于 D4.1/R3 的
健康探测测试，等池内探测实现后再加。

## Nit

### server-id 分配规则
- status: wontfix
- file: test/harness/sharded.go

每片 server-id 按 `slice*(replicas+1)+1` 递增；够用且可预测，但测试容器
拓扑变化时需要保持唯一。已由 harness 内部集中计算，调用方无需关心。

### 容器别名与实例名
- status: wontfix
- file: test/harness/sharded.go

网络别名 `mysql-s0-master`/`mysql-s0-replica-1` 与实例名 `s0`/
`s0-replica-1` 并存，分别服务于复制源解析和测试可读性；不合并。
