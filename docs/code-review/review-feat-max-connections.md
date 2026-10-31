---
title: 前端最大连接数（max_connections）review
files:
  - pkg/server/server.go
  - pkg/config/config.go
---

## Must Fix

无。

## Should Fix

无。

## Nit

### 超限时直接关闭 TCP，不返回 MySQL 1040
- status: wontfix
- file: pkg/server/server.go

在 accept 后立刻关闭连接，客户端通常看到 EOF/connection reset。MySQL server 的
1040 需要先完成握手再回 ERR；为了不让超限连接消耗握手资源，这里选择立即关闭。
后续如果需要更友好的客户端错误，可做“轻量 handshake + 1040”专用路径。

### 每次拒绝都打 Warn 日志
- status: wontfix
- file: pkg/server/server.go

连接洪泛时日志可能被刷屏；等 metrics/限流落地后改成计数 + 采样/限速日志。

### 只限制单实例总量，没有每用户/IP 限制
- status: wontfix
- file: pkg/server/server.go

本轮只做 `server.max_connections`；每用户/IP 配额、连接速率限制是后续准入控制
工作流。`0` 表示不限制，生产示例建议显式设置。

## 验证

- `go build ./...`：通过
- `go vet ./...`：通过
- `gofumpt -extra -l` 变更包：无输出
- `go test -race -short -count=1 ./pkg/backend/ ./pkg/config/ ./pkg/server/ ./pkg/execute/ ./pkg/proxy/ ./pkg/mysql/...`：全绿
- `go test -short -run '^$' -count=1 ./test/...`：编译通过（差分未跑，Docker 不可用）
- 新增用例：`TestServer_MaxConnections`（第 2 个连接被拒、关闭后释放槽位）、
  `TestValidate` 的 `max_connections` 负数校验
- 覆盖率：`pkg/config` 94.1%、`pkg/server` 77.3%
