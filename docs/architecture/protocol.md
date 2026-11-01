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
- `client.Dial(raw, user, password, db)`：完成后端握手。
- `ForwardResponseInfo(dst, src, deprecateEOF)`：把一条命令响应从后端搬到前端，
  并解析出 OK 包元数据（affected/lastInsertID/status）。

## 行为不变量

1. **后端拨号后固定 `SET character_set_results = NULL`**（A10）：结果不做转码，
   ColumnDefinition41 携带列真实 collation 而非连接字符集；utf8mb4 数据字节不变。
2. **后端握手 collation 固定 45**，不回显服务端默认——配合上一条构成列元数据保真。
3. 仅 `mysql_native_password`；无 TLS（路线图）。
4. `deprecateEOF` 当前按前端声明能力恒为 true——每连接能力协商是已知缺口。
5. `pkg/mysql` 不做 SQL 语义（依赖规则 5）：它只认帧与编码，不解析语句。

## 已知缺口

连接 ID 固定为 1、`SERVER_STATUS` 部分场景硬编码、`COM_STMT_SEND_LONG_DATA` /
`COM_CHANGE_USER` / `COM_FIELD_LIST` 未覆盖——清单与影响见
[design/industrial-readiness-gap.md](../design/industrial-readiness-gap.md)。

## 测试入口

codec 往返单测（`packet_test.go` / `decode_test.go`）、握手集成
（`test/handshake/`）、collation 上游 golden（`collation/*_test.go`）、
透传差分（`test/differential/`，协议层不依赖路由即可差分）。
