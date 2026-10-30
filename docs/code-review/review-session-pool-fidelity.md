---
title: session-pool-fidelity review
files:
  - pkg/mysql/forward.go
  - pkg/mysql/forward_test.go
  - pkg/session/state.go
  - pkg/session/state_test.go
  - pkg/pool/backend.go
  - pkg/pool/align_test.go
  - pkg/pool/pool.go
  - pkg/pool/pool_test.go
  - pkg/proxy/session.go
  - pkg/proxy/session_test.go
  - docs/design/m1-passthrough-pipeline-design.md
---

## Must Fix

### sticky 断开时池 active 泄漏
- status: fixed
- file: pkg/pool/pool.go, pkg/proxy/session.go

原 `defer sticky.Close()` 不递减 `pool.active`，reserved 会话断开后池容量永久被占。改为 `Pool.Discard`（Close + active--），并补单测。

## Should Fix

### Align 失败时前端未收到 ERR 包
- status: wontfix
- file: pkg/proxy/session.go

`withBackend` 在 Align 失败时直接返回 error，Serve 退出并关连接，客户端看到连接断开而非 MySQL ERR。与其它命令路径上的传输失败一致；专门编 ERR 可后置，不阻塞本阶段。

## Nit

### parseUse 用前缀 `USE` 可能误匹配 `USER...`
- status: wontfix
- file: pkg/proxy/session.go

既有启发式；需跟空白/标识符边界才更严，本阶段不扩 scope。
