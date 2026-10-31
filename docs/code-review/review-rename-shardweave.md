---
title: 项目更名为 Shardweave review
files:
  - go.mod
  - cmd/shardweave/main.go
  - etc/shardweave.yaml
  - AGENTS.md
  - README.md
  - docs/architecture.md
---

## Must Fix

无。

## Should Fix

无。

## Nit

### GitHub 仓库与 remote 仍是旧名
- status: wontfix
- file: go.mod

代码 module 已改为 `github.com/acorndb/shardweave`，但 GitHub 仓库当前仍是
`acorndb/prism`，本地 remote 仍指向旧的 `acorndb/sharding-proxy`（会重定向）。
仓库改名与 `git remote set-url` 是外部操作，另行确认执行；本提交不包含。

### 历史调研路径保留 `prism` 字样
- status: wontfix
- file: docs/design/module-map.md

`/tmp/prism-oss-survey` 是本地开源调研 checkout 的实际路径，不是项目名；
保留原样，避免文档里的参考路径失效。

### 重命名为 breaking change
- status: wontfix
- file: go.mod

module path/import path 变化会影响任何下游 import；项目 pre-1.0，按计划接受。
配置默认路径也从 `etc/prism.yaml` 改为 `etc/shardweave.yaml`。

## 验证

- 全仓 `git grep -i prism`：只剩 `/tmp/prism-oss-survey` 调研路径
- `go build ./...`：通过
- `go vet ./...`：通过
- `make test-unit`（race + short，全量单测）：全绿
- `gofmt -l cmd pkg test`：无输出
- `git diff --check`：通过
- `go.mod`：`module github.com/acorndb/shardweave`
- server version：`8.0.36-shardweave`
