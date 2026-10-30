---
title: backend_server single+rwsplit review
files:
  - pkg/backend_server/backend.go
  - pkg/backend_server/pool.go
  - pkg/router/classify.go
  - pkg/router/pick.go
  - pkg/router/group.go
  - pkg/proxy/session.go
  - pkg/server/server.go
  - pkg/config/config.go
  - pkg/session/state.go
---

## Must Fix

（无）

## Should Fix

### LoadBalance=random 实为「随机起点的轮询」而非真随机选片
- status: wontfix
- file: pkg/router/group.go

`slaveOrder` 在 `LBRandom` 下用 `rr` 取起点再顺序尝试，语义足够做 failover 与分散；真 `rand` 可后置，不阻塞 single/rwsplit 主路径。

### 热更新仍不重建 backend 地址（仅 rt 占位）
- status: wontfix
- file: pkg/server/server.go

`newBackendGroup` 里 `_ = rt`；地址热更仍拒绝/后置，与既有 pool 行为一致。

## Nit

### Classify 多语句分号启发式可能误判字符串内分号边界
- status: wontfix
- file: pkg/router/classify.go

误判时偏向 master（安全侧）；完整 SQL lexer 后置。
