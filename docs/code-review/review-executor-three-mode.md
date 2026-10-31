---
title: executor three-mode review
files:
  - pkg/executor/executor.go
  - pkg/executor/forward.go
  - pkg/proxy/session.go
  - pkg/server/server.go
  - docs/design/executor-three-mode-design.md
---

## Must Fix

（无）

## Should Fix

### Single/RWSplit 同为未导出 passthrough，仅 pick 闭包不同
- status: wontfix
- file: pkg/executor/executor.go

对外类型已满足「同接口异策略」；再拆两个空壳类型无行为差，留待 sharding 真实现时再分文件。

### 集成测每进程共享一个 Executor 关闭即关池
- status: fixed
- file: test/passthrough/passthrough_test.go, test/differential/*

Accept 循环外构造一次 `executor.New`，Cleanup 调 `Close`。

## Nit

### proxy COM_RESET 忽略 ReleaseSticky 错误
- status: wontfix
- file: pkg/proxy/session.go

与改前「Release 失败仍 Reset 状态」一致；可观测性后置。
