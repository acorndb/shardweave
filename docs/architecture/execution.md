# 执行与资源层（pkg/execute、pkg/backend、pkg/session）

唯一 import `backend` 的数据面模块。计划在这里变成连接上的命令，会话状态在这里
落到连接上（Align），连接生命周期在这里裁决。

## 包清单

| 包 | 职责 |
|---|---|
| `execute` | 单 `Pipeline`：Build → lease → Align → Exec → 转发/归并；`lease` 按 slice 持有 |
| `backend` | `Conn / Pool / Slice / Cluster`：拨号（含 results=NULL 策略）、池、拓扑 |
| `session` | 纯状态：db / charset / autocommit / reserved / stmtPinned / 白名单变量 / StmtTable |

## 关键类型

- `Pipeline.Handle`：单条命令的完整路径；`Release(rt, discard)` 归还或淘汰全部 lease。
- `lease{slice, conn, pool, reusable}`：会话到连接的租约；reusable=false 的连接归还时淘汰。
- `backend.Pool`：预热、健康检查（idle 超 `HealthCheckInterval` 先 ping）、
  idle 回收、MaxLifetime、AcquireTimeout。
- `MySQLConn.Align(AlignSettings)`：库（COM_INIT_DB）、字符集
  （`SET character_set_client/connection`，不动 results）、白名单变量
  （diff → 一条 SET；连接上有而会话没有的 → `=DEFAULT`）。
- `rawStream / decodedStream.Drained()`：响应是否读尽——未读尽的连接不可复用。

## 行为不变量

1. **mode 不出现在此层**（依赖规则 1）；三模式共用同一条 Pipeline。
2. **lease 归 execute**（A4）：session 只表达粘性意图（`NeedsSticky`）。
3. **不该放回的连接不放回**（A12）：

   | 归还判定 | 动作 |
   |---|---|
   | 响应未读完（`Drained()==false`） | 淘汰 |
   | Exec / Align / 转发出错 | 淘汰 |
   | 脏语句（用户变量写、临时表、锁、多语句、Exec 注释、扫描失败） | 淘汰 |
   | 归还时仍 sticky（事务 / autocommit=0 / reserved / stmt pin） | 淘汰 |
   | 其余 | 直接归还，**不发 `COM_RESET_CONNECTION`** |

   对照：database/sql 不清理、kingshard 不清理、Gaea 定向清理 + 变量重放、
   pgxpool `AfterRelease` 钩子、HikariCP dirty bits——没有实现每次全量 Reset 的。
4. **白名单变量重放**（A13）：`time_zone` / `transaction_isolation`；非白名单在
   语句层已被 1235，执行层只会见到白名单。
5. **prepared（sharding）**：代理侧 stmt 表（`session.StmtTable`）按 slice 记录后端
   stmt id；`COM_STMT_EXECUTE` 改写 id 后透传；非 sharding 的 prepare 直接粘连接。
6. **两处本地应答**：`EXPLAIN ROUTE` 与 `SET NAMES utf8mb4` 不触后端（A10）。

## 已知缺口

`Conn.Exec/Ping/Align` 的 `ctx` 未接线（无查询总超时 / KILL QUERY）；
`termination_drain` 配置已解析未生效——见
[design/industrial-readiness-gap.md](../design/industrial-readiness-gap.md)。

## 测试入口

`pool_test.go / stream_test.go / conn_test.go`（net.Pipe 假服务端）、
`pipeline_test.go / dispatch_test.go`（fakeConn/fakeStream，
含脏语句淘汰、Drained、重放断言）、`test/rwsplit/`（真拓扑 sql_type_matrix）。
