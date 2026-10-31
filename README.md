# prism

MySQL 分库分表代理。设计目标与模块层级见 [docs/architecture.md](docs/architecture.md)。

## 当前状态

M1 透传进行中。P0–P2 已可用：协议握手、后端池、`COM_QUERY` / `COM_STMT_*` 透传，
以及 `TestPassthroughDifferential`（CalibrationScript 经真代理 vs 直连）。

## 开发

要求:Go 1.25+（引入 tidb/parser 后 go.mod 可能要求更高工具链，随 `GOTOOLCHAIN=auto` 自动下载）;Docker(差分测试用,不可用时自动跳过)。

## 已落地

- `pkg/mysql/packet`：长度编码 + 帧读写（对齐 go-mysql `WritePacket` 前 4 字节 header）
- `pkg/mysql/auth` + `server`/`client`：HandshakeV10 / native-password 内存闭环
- `pkg/sql`：`Statement`（Cmd/Payload/Raw/Kind/Effect）统一命令形态；USE/SET effect 成功后由 `execute` 应用
- `pkg/constant`：mode / load_balance / role / log level 等全局常量
- `pkg/backend` + `pkg/session`：后端连接、连接池、主从 Slice、`Conn.Exec/Align`
- `pkg/route/source`：主从选源规则（Pick / ClassifyWantSlave）
- `pkg/plan`：N=1 `Passthrough` Plan 骨架（Plan/Kind/Builder）
- `pkg/result`：Field/Row/Stream（raw packet 与逻辑行两种形态）
- `pkg/execute`：单 Pipeline；`backend.Conn.Exec` + raw stream；single/rwsplit 共用
- `pkg/proxy`：命令循环；预处理会话字段后交给 Executor
- `pkg/config` + `cmd/prism`：YAML `backend` + `slices`、Accept 循环、简单 `SIGUSR1` 热更

```sh
make build            # 编译
make test             # 单元(short) + 差分校准
make test-integration # 仅差分校准(需要 Docker)
make lint             # vet + golangci-lint
make fmt              # gofumpt 优先, 回退 gofmt
```

差分 harness 在 [test/differential/](test/differential/);SQL 语料
(CalibrationScript)随测试演进持续增长,每个真实 bug 都应转为新条目。
