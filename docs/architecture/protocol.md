# 协议层（pkg/mysql）

MySQL 线协议的第一方实现：帧编解码、握手、列/行编解码、collation。保真度是项目
差异化的所在——可以参考 go-mysql / vitess，但不做黑盒依赖（设计原则 2）。

## 包清单

| 包 | 职责 |
|---|---|
| `mysql/packet` | 帧读写（4 字节头 + sequence）、长度编码、`max_allowed_packet` 逻辑包上限、读写 I/O 超时 |
| `mysql/auth` | HandshakeV10 / HandshakeResponse41 编解码、native-password scramble |
| `mysql/server` | 前端握手（`UserProvider` 查口令）与连接包装 |
| `mysql/client` | 后端握手（`Dial`）；握手 collation 固定 `utf8mb4_general_ci`(45) |
| `mysql/collation` | vendored vitess collations（stdlib-only、commit 锁定，A9） |
| `mysql`（根） | OK/ERR/EOF/ColumnDefinition/TextRow 编解码、响应转发（`ForwardResponseInfo`） |

## 关键类型与入口

- `packet.Conn`：唯一帧读写入口；`ResetSequence` 在每条命令前调用。
- `server.NewConnWithOptions(raw, users, opts)`：完成前端 native-password 握手。
- `server.Conn.ConnectionID()`：握手字段、`CONNECTION_ID()` 与日志共用的前端连接 ID。
- `client.Dial(raw, user, password, db)`：完成后端握手。
- `ForwardResponseInfo(dst, src, deprecateEOF)`：把一条命令响应从后端搬到前端，
  并解析出 OK 包元数据（affected/lastInsertID/status）。

## 行为不变量

1. **后端拨号后固定 `SET character_set_results = NULL`**（A10）：结果不做转码，
   ColumnDefinition41 携带列真实 collation 而非连接字符集；utf8mb4 数据字节不变。
2. **后端握手 collation 固定 45**，不回显服务端默认——配合上一条构成列元数据保真。
3. 仅 `mysql_native_password`；无 TLS（路线图）。
4. `deprecateEOF` 当前按前端声明能力恒为 true——每连接能力协商是已知缺口。
5. **前端连接 ID 每连接唯一**：`pkg/server` 按 WL#7293 分配（从 1 开始，32 位回绕
   跳过 0 与在用 ID），握手字段、`CONNECTION_ID()`、日志共用同一个值；
   `COM_RESET_CONNECTION` 不改变它。
6. **代理合成响应的 `SERVER_STATUS` 由会话状态计算**：`mysql.ServerStatusWord(autocommit, inTx)`
   用于 PING、`COM_RESET_CONNECTION`、`SET NAMES` 本地应答与 `EXPLAIN ROUTE` 终止包；
   握手状态字固定为初始值 `0x0002`（autocommit=1、无事务）。透传响应的状态位保持
   后端原值，more-results/cursor 等位不由代理重建。
7. **分片归并中段错误以 ERR 终止结果集**：结果头已写出后，行流错误不再断开前端连接；
   分片返回的 `*errs.MySQL` 保留原错误码，传输/解码错误映射为 1105/HY000。
8. **超过 `server.max_connections` 以 1040/08004 拒绝**：ERR 作为首包（不先发
   HandshakeV10），而不是直接关闭 TCP。
9. `pkg/mysql` 不做 SQL 语义（依赖规则 5）：它只认帧与编码，不解析语句。

## 已知缺口

`COM_STMT_SEND_LONG_DATA` / `COM_CHANGE_USER` / `COM_FIELD_LIST` 未覆盖、能力位未按
连接保存——清单与影响见
[design/industrial-readiness-gap.md](../design/industrial-readiness-gap.md)。

## 测试入口

codec 往返单测（`packet_test.go` / `decode_test.go`）、握手集成
（`test/handshake/`）、collation 上游 golden（`collation/*_test.go`）、
透传差分（`test/differential/`，协议层不依赖路由即可差分）。
