---
title: rwsplit stub route smoke review
files:
  - pkg/executor/executor_test.go
  - docs/design/rwsplit-route-smoke-design.md
  - etc/prism.yaml
---

## Must Fix

（无）

## Should Fix

（无）

## Nit

### RouteSmoke 经 withBackend 而非完整 Handle
- status: wontfix
- file: pkg/executor/executor_test.go

Handle 需要 MySQLBackend 做 Forward；本切片目标是选源，withBackend 足够。真协议冒烟后置。
