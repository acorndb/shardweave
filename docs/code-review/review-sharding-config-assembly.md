---
title: 分片配置模型与多 Slice 装配 review
files:
  - pkg/config/config.go
  - pkg/config/runtime.go
  - pkg/constant/constant.go
  - pkg/server/server.go
---

## Must Fix

（无）

## Should Fix

### reload 时 mode 变化被 sharding "not implemented" 掩盖
- status: fixed
- file: pkg/config/runtime.go

`Apply` 原来先 `Validate(next)`；`mode=sharding` 的合法配置会先返回
"not implemented"，导致 rwsplit→sharding 看不到明确的 mode 变更错误。
现在对**已知模式**先比较 `backend.mode`，再校验其余配置；同时保留
`SameTopology` 作为第二道防线。补充 single→rwsplit、rwsplit→sharding、
sharding→single 三组测试。

### 合法的 sharding 配置仍不可用
- status: wontfix
- file: pkg/config/config.go

A1 只落模型与校验，`mode=sharding` 在校验通过后仍返回 "not implemented"。
这是有意的阶段门禁：执行层还没有 N 目标 dispatch，提前打开会得到半成品。
B1 完成后再移除该返回。

### 每个 Slice 不能覆盖 pool / load_balance
- status: wontfix
- file: pkg/server/server.go

P0 沿用全局 `pool` 配置与顶层 `load_balance`，`buildCluster` 对每个 Slice
应用同一套参数。per-slice 覆盖会扩大配置面；等真实容量差异出现后再加。

## Nit

### single 模式也设置了 LoadBalance
- status: wontfix
- file: pkg/server/server.go

`buildCluster` 对单 Slice 也写 `round_robin`，但 single 没有 slaves，字段不生效。
统一赋值让装配少一个分支，代价可忽略。
