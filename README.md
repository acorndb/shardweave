# sharding-proxy

MySQL 分库分表代理。设计目标与模块层级见 [docs/architecture.md](docs/architecture.md)。

## 当前状态

M1(透传)起步。差分测试基建已落地:零号代理(纯字节转发的 TCP 管道)作为校准基准,
同一会话脚本在「直连 MySQL」与「经代理」两条路径上的观测必须完全一致——
错误码、affected_rows、last_insert_id、结果集、warnings 逐字段对比。
协议层的第一行代码落地前,这条流水线先证明自己可信。

## 开发

要求:Go 1.25+（引入 tidb/parser 后 go.mod 可能要求更高工具链，随 `GOTOOLCHAIN=auto` 自动下载）;Docker(差分测试用,不可用时自动跳过)。

## P0 已落地

- `mysql/packet`：长度编码 + 帧读写（对齐 go-mysql `WritePacket` 前 4 字节 header）
- `mysql/auth` + `server`/`client`：HandshakeV10 / native-password 内存闭环
- `sql/parser`：tidb/parser 包装（panic→error）；数据面尚未改写

```sh
make build            # 编译
make test             # 单元(short) + 差分校准
make test-integration # 仅差分校准(需要 Docker)
make lint             # vet + golangci-lint
make fmt              # gofumpt 优先, 回退 gofmt
```

差分 harness 在 [test/differential/](test/differential/);SQL 语料
(CalibrationScript)随测试演进持续增长,每个真实 bug 都应转为新条目。
