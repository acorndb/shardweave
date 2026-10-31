---
title: cmd/shardweave + pkg/config + simple SIGUSR1 reload review
files:
  - pkg/config/config.go
  - pkg/config/runtime.go
  - pkg/server/server.go
  - cmd/shardweave/main.go
  - cmd/shardweave/log.go
  - etc/shardweave.yaml
---

## Must Fix

（无）

## Should Fix

### sharding 仅打标未在 Load 时强制可观测
- status: fixed
- file: pkg/config/config.go

已删除 `Sharding` / `ShardingIgnored`；分片未实现前不占配置位。

### 热更未 Apply pool 上限
- status: wontfix
- file: pkg/config/runtime.go

本波为「简单版」热更：users / log.level。pool 上限热更属设计中的「尽力」项，留待后续；避免半吊子改池。

## Nit

### accept 错误在非关闭场景直接退出进程
- status: wontfix
- file: pkg/server/server.go

与 kingshard 类似，Accept 瞬时错误少见；优雅退出靠关 listener。若需重试可后置。

### main 职责过重（监听/池/信号揉在一起）
- status: fixed
- file: cmd/shardweave/main.go

对齐 kingshard/Gaea：`pkg/server` 提供 `New` / `Run` / `Close` / `UpdateConfig`；`main` = 加载配置 + 信号 goroutine + 阻塞 `Run()`。信号与热更留在 main（与开源一致），不塞进 `Run`。
