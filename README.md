# prism

MySQL 分库分表代理。设计目标与模块层级见 [docs/architecture.md](docs/architecture.md)。

## 当前状态

M1 透传进行中。P0–P2 已可用：协议握手、后端池、`COM_QUERY` / `COM_STMT_*` 透传，
以及 `TestPassthroughDifferential`（CalibrationScript 经真代理 vs 直连）。

## 开发

要求:Go 1.25+（引入 tidb/parser 后 go.mod 可能要求更高工具链，随 `GOTOOLCHAIN=auto` 自动下载）;Docker(差分测试用,不可用时自动跳过)。

## 已落地

- `mysql/packet`：长度编码 + 帧读写（对齐 go-mysql `WritePacket` 前 4 字节 header）
- `mysql/auth` + `server`/`client`：HandshakeV10 / native-password 内存闭环
- `sql/parser`：tidb/parser 包装（panic→error）
- `pool` + `session`：单后端池、reserved 粘性骨架
- `proxy`：命令循环透传；`mysql.ForwardResponse` / `ForwardPrepareResponse`

```sh
make build            # 编译
make test             # 单元(short) + 差分校准
make test-integration # 仅差分校准(需要 Docker)
make lint             # vet + golangci-lint
make fmt              # gofumpt 优先, 回退 gofmt
```

差分 harness 在 [test/differential/](test/differential/);SQL 语料
(CalibrationScript)随测试演进持续增长,每个真实 bug 都应转为新条目。
