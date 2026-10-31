# 单机代理工业级差距评估

> 状态：评估报告（2026-10-31）
> 范围：**单机 MySQL 代理**（single / rwsplit），不含分片功能。
> 依据：当前 `main`（R1–R6 重构完成）源码逐模块检查 + 现有测试。
> 提交提示：`docs/design/` 被 `.gitignore` 忽略；入仓需 `git add -f`。

## 0. 结论

**当前是“可用的透传原型”，不是“生产候选”。**

- 核心路径已经成立：协议帧、native 握手、连接池、single/rwsplit 选源、Plan→Execute、raw 结果转发、
  USE/SET effect、差分 harness 骨架。
- 但认证/TLS、协议能力协商、超时与连接上限、池健康、优雅退出、可观测性/运维面仍是空白或半成品。
- 粗略成熟度（按能力面，不是代码行数）：

| 模块 | 成熟度判断 |
|---|---|
| 协议帧 / packet | 可用（缺上限/超时/压缩） |
| 认证 / TLS | **原型**（仅 native；无 TLS） |
| 会话状态 | **原型**（字段很少；变量未跟踪） |
| 后端连接池 | 可用透传 + 健康检查 / idle / lifetime / 预热（本轮）；缺查询失败重试/换库/指标 |
| 读写分离路由 | 原型（启发式；无健康/延迟/重试） |
| 执行 / 结果转发 | 可用透传（缺超时/取消/能力协商） |
| 配置 | 可用（缺 TLS/超时/限流/运维字段） |
| 可观测 / 管理面 | **空白** |
| 安全 | **空白/原型**（无 TLS、明文密码、无连接/包上限） |
| 测试 | 差分骨架 + 单测；缺认证/TLS/prepare/事务/故障/负载 |

**距离生产试点**：至少 4 个 P0 工作流（认证与 TLS、协议保真、超时/池/生命周期、
可观测与运维），再加 2–3 个 P1 工作流（会话变量、读写分离健康与重试、配置/安全硬化）。
不是“补几个参数”，而是四个能力面的补齐。

## 1. 评估口径

“工业级可用”按以下维度判断：

1. **兼容性**：MySQL 8 默认客户端能连、常见驱动/ORM 能用、协议能力协商正确。
2. **正确性**：会话状态、事务、prepared statement、错误/结果集语义与裸 MySQL 一致。
3. **可靠性**：超时、连接上限、后端健康、故障回落、优雅退出。
4. **安全**：TLS、密码存储、连接/包/查询限制、审计。
5. **可观测/运维**：日志、指标、健康检查、管理面、慢查询、连接/池统计。
6. **性能**：连接复用、流式转发、无无界缓冲；当前 raw 转发方向是对的。
7. **测试**：差分、随机、故障注入、负载、fuzz；每个 P0 能力都要有门禁。

## 2. 已经具备的能力

- `pkg/mysql/packet`：长度编码、帧读写、sequence、continuation（对齐 go-mysql 约定）。
- `pkg/mysql/auth`：native password + caching_sha2 **token 计算**；`HandshakeV10/Response41` 编解码。
- `pkg/backend`：`Conn`/`Pool`/`Slice`/`Cluster`；池释放时 `COM_RESET_CONNECTION`、失败丢弃。
- `pkg/route/source`：single/rwsplit 选源规则；`source.Pick` 支持显式 slice。
- `pkg/plan` / `pkg/sql`：`Statement`、`Effect`、N=1 `Passthrough` Plan；USE/SET effect 成功后应用。
- `pkg/execute`：单 Pipeline、按 slice 的 lease、raw 流直通、ERR 不污染 InTx、sticky 判断在 effect 之后。
- `pkg/proxy`：COM 循环；PING/QUIT/INIT_DB/QUERY/STMT/RESET；命令级 fake Executor 测试。
- `pkg/server`：listener/Accept/Close、SIGUSR1 热更 users/log。
- `test/differential`：经真代理 vs 直连 MySQL 的透传差分；`test/handshake`、`test/passthrough`。

这些是继续补齐工业能力的底座，不需要推倒。

## 3. P0：生产试点前必须补

### 3.1 认证与 TLS

| 缺口 | 证据 | 影响 |
|---|---|---|
| 前端只支持 `mysql_native_password` | `pkg/mysql/server/conn.go:64` | MySQL 8 默认 `caching_sha2_password` 客户端无法登录 |
| `caching_sha2` 只有 token 计算，无 fast/full auth、无 RSA/TLS 分支 | `pkg/mysql/auth/caching_sha2.go:5-26` | 只能做 native 场景；8.0 客户端不可用 |
| 无 `CLIENT_SSL` / SSLRequest / 前端 TLS | `server/conn.go` 能力位未含 `ClientSSL` | 要求 TLS 的客户端/合规环境无法接入 |
| 后端连接只走 native，无 TLS | `pkg/mysql/client/conn.go:48-57` | 连 MySQL 8 默认认证或 TLS 后端失败 |
| 无 AuthSwitch / 插件协商 | `server/conn.go` 固定 `NativePasswordPlugin` | 客户端用其他插件时直接失败 |
| 用户密码明文存 YAML | `pkg/config/config.go:25-29` | 生产 secret 管理不合格 |

### 3.2 协议保真与能力协商

| 缺口 | 证据 | 影响 |
|---|---|---|
| `deprecateEOF` 硬编码 true | `pkg/backend/stream.go:20-25` | 客户端未协商 `CLIENT_DEPRECATE_EOF` 时结果集分帧错误 |
| 服务端声明 `CLIENT_SESSION_TRACK` 但不解析 | `server/conn.go:59-60` | 会话状态仍靠猜；跟踪能力形同虚设 |
| 无按连接保存的能力位 | `server.Conn` 不暴露 capability | 无法按客户端协商结果转发/编码 |
| 命令覆盖不全 | `pkg/proxy/session.go:75-111` | `COM_FIELD_LIST`、`COM_STMT_SEND_LONG_DATA`、`COM_CHANGE_USER`、`COM_SET_OPTION`、`COM_STATISTICS`、`COM_PROCESS_KILL` 等直接 1047 |
| `COM_FIELD_LIST` / `SEND_LONG_DATA` 路由分支不可达 | `pkg/route/source/pick.go:16-20` vs `proxy` 默认 ERR | 代码与行为不一致；驱动兼容性差 |
| 连接 ID 固定 1、server status 固定 0x0002 | `server/conn.go:54,63` | 客户端诊断/事务状态不可信 |
| PING 返回固定 autocommit status | `pkg/proxy/session.go:78-80` | 事务中 PING 状态错误 |
| 包大小无上限 | `pkg/mysql/packet/packet.go:40-73` | 单连接可发超大/连续包造成内存压力 |
| 无 LOCAL INFILE 处理 | `pkg/mysql/forward.go` 返回错误 | 客户端 LOAD DATA LOCAL 场景断连 |

### 3.3 超时、连接上限与生命周期

| 缺口 | 证据 | 影响 |
|---|---|---|
| 前端读无 deadline，ctx 只在循环顶检查 | `pkg/proxy/session.go:42-52` | 空闲/半开连接永久占 goroutine；查询中无法取消 |
| 后端读无 deadline，`Align/Exec/Reset` 忽略 ctx | `pkg/backend/conn.go` | 后端 hang 会永久阻塞客户端 |
| 无查询超时 / `MAX_EXECUTION_TIME` | `pkg/execute/pipeline.go:155-172` | 慢查询不可控 |
| 无最大连接数 / 每用户/IP 限制 | `pkg/server/server.go:143-157` | 连接洪泛 DoS |
| 无优雅退出/drain | `pkg/server/server.go:181-195` | `Close` 取消 ctx 但阻塞中的 session 不会退出；`termination_drain` 未使用 |
| 无 accept 退避 | `server.go:145-153` | accept 错误时忙循环 |
| `termination_drain` 配置未生效 | `pkg/config/config.go:22` 仅解析 | 假配置项 |
| `pool.capacity` / `pool.idle_timeout` | 已在 `feat/pool-hardening` 实现预热与 idle reaper | 仍需池指标与失败日志 |

### 3.4 连接池健康与故障处理

> 2026-10-31 `feat/pool-hardening` 已补：`Conn.Ping` 健康检查、idle reaper
> （`IdleTimeout`）、`MaxIdle`、`MaxLifetime`、`Capacity` 预热、`Pool.Stats()`。
> 仍缺：查询失败重试/换库、全局/每用户上限、池指标、warmup 失败日志。

| 缺口 | 证据 | 影响 |
|---|---|---|
| 无查询失败重试/重建 | `pkg/execute/pipeline.go` | 网络抖动直接暴露给客户端 |
| 无健康检查/只读检测/延迟/权重 | `pkg/backend/slice.go` | 从库故障只能靠 acquire 失败回落，查询失败不回落 |
| `Discard` 可把 active 减成负数 | `pool.go` | 已加 `active > 0` 保护；重复释放仍可能掩盖错误 |
| 关闭不等待活跃连接 | `pool.go` | 进程退出时请求被截断 |

### 3.5 Prepared statement

| 缺口 | 证据 | 影响 |
|---|---|---|
| 无 stmt 表/ID 映射 | `pkg/session` 无 StmtTable | 连接重建/切换后端后 stmt id 失效；无法诊断 |
| `COM_STMT_SEND_LONG_DATA` 未支持 | `proxy` 默认 1047 | 大数据参数驱动失败 |
| `COM_STMT_FETCH` / 游标未支持 | 未实现 | cursor 模式不可用 |
| prepare 后永久 reserved | `proxy:93-97` | 长连接持有后端，池利用率下降 |
| 无 prepare 错误/关闭生命周期 | 转发即透传 | 资源泄漏与状态不一致 |

### 3.6 可观测与管理面

| 缺口 | 证据 | 影响 |
|---|---|---|
| 无 metrics（Prometheus） | 无 `pkg/obs`/admin | 无法看 QPS、延迟、错误、池/后端健康 |
| 无 `/healthz` `/readyz` | 无 HTTP server | K8s/负载均衡无法探活 |
| 无管理面/状态查询 | 无 `pkg/admin` | 无法看连接、池、后端、慢查询、kill |
| 无访问/慢查询/错误日志 | `pkg/log` 只有 slog text | 线上排障困难 |
| 无连接 ID/remote addr 日志 | `proxy` 无日志 | 无法关联会话 |
| 无结构化 JSON / 日志级别热更之外的字段 | `pkg/log/log.go` | 采集/检索不便 |
| 无 tracing | 无 OpenTelemetry | 跨层定位难 |

## 4. P1：生产可用前应补

### 4.1 会话状态与事务

- `session.State` 只有 database/inTx/autocommit/reserved/charset（`pkg/session/state.go:8-15`）。
- 未跟踪 `sql_mode`、`time_zone`、`character_set_*`、`transaction_isolation`、用户变量值。
- `Align` 只对齐 database/charset（`pkg/backend/conn.go`），系统变量/用户变量跨连接会丢。
- `SET autocommit` 只识别 `1/0/ON/OFF`，不识别 `TRUE/FALSE`、`:=` 等形式（`pkg/sql/session_cmd.go`）。
- `DetectReservedSQL` 是子串启发式（`pkg/session/state.go:71-91`）：临时表/锁/用户变量有漏判/误判。
- 事务隔离、savepoint、XA、只读事务、事务超时未建模。
- `COM_RESET_CONNECTION` 保留 database 的语义需要与 MySQL 版本对齐验证。

### 4.2 读写分离正确性

- 选源是字符串启发式（`pkg/route/source/classify.go`），没有 AST/真正的 tokenizer。
- 没有从库延迟、健康分、权重、最小连接数/延迟 LB（`pkg/backend/slice.go` 只有 round_robin/random）。
- 没有 read-after-write / 事务粘性之外的读一致性策略。
- 从库查询失败不会自动换库重试；只在 acquire 失败时回落 master。
- 没有用户级/查询规则级路由、`/*slave*/` 等扩展。
- `SELECT ... INTO OUTFILE` 等有副作用的 SELECT 可能被误判为只读。

### 4.3 配置与安全

- 配置缺 TLS、连接/查询/packet 上限、超时、admin/metrics、健康检查、权重、每用户限制（`pkg/config/config.go`）。
- 用户密码明文；无密码哈希/外部 secret。
- 无 IP allowlist、限流、最大连接数、查询大小限制。
- 无 SQL 审计/防火墙（可作为 P2）。
- 配置只热更 users/log；backend 拓扑变更要求重启（当前可接受，但要文档化）。

### 4.4 运维与部署

- 无 readiness/liveness、version/build info、pprof（可选）、metrics。
- 无优雅 drain；`termination_drain` 未使用。
- 无 admin kill connection / 动态日志级别之外的控制面。
- 无连接/池/后端健康状态查询。

## 5. P2：后续增强

- 压缩协议（`CLIENT_COMPRESS`）。
- 查询指纹/慢查询聚合/query digest。
- 查询缓存/plan cache（语义安全前提下）。
- 审计日志、SQL 防火墙、脱敏。
- 多租户 namespace、按用户/租户隔离池。
- 连接多路复用（multiplexing）与更激进的池策略（需协议/事务安全评估）。
- 故障注入/混沌、MTR/RQG 深度套件、性能基线/门禁。
- Proxy Protocol、SO_REUSEPORT、TLS 证书热更。
- 管理 REST/OpenAPI、Prometheus 指标、OpenTelemetry tracing。

## 6. 建议的 P0 工作流（按依赖顺序）

### A. Auth & TLS

- `caching_sha2_password` fast auth + full auth（TLS 或 RSA public key）。
- `AuthSwitchRequest` / 插件协商。
- 前端 TLS：`CLIENT_SSL`、SSLRequest、证书配置。
- 后端 TLS + caching_sha2 客户端。
- 密码哈希/secret 配置。
- 门禁：MySQL 8 默认客户端、`caching_sha2`、TLS、错误密码、`AuthSwitch` 集成测试。

### B. 协议保真

- 每个前端连接保存协商后的 capability。
- `deprecateEOF`、`multiStatements`、`multiResults`、`sessionTrack` 按协商结果使用。
- 补 `COM_FIELD_LIST`、`COM_STMT_SEND_LONG_DATA`、`COM_CHANGE_USER`、`COM_SET_OPTION`、
  `COM_PROCESS_KILL`、`COM_STATISTICS` 等（至少明确拒绝而不是 1047 断连）。
- packet 上限、连接 ID、动态 server status。
- 门禁：能力矩阵差分（带/不带 deprecateEOF）、常见驱动/ORM 冒烟。

### C. 超时、上限、优雅退出

- 前端 read/write/idle timeout、查询 timeout、后端 dial/read/write timeout。
- max connections / per-user / per-IP 限制。
- max packet / max query size。
- 优雅 drain：停止 accept → 等待活跃 session 到 `termination_drain` → 关闭池。
- 门禁：超时/半开连接/慢后端/大量连接/failover 测试。

### D. 池健康

- acquire 前 ping/validate、失败重试/重建、idle reaper、max lifetime、预热。
- 后端健康检查、只读检测、延迟/权重、从库失败换库。
- 池指标。
- 门禁：杀后端/重启/从库挂掉/空闲超时。

### E. Prepared statement

- 单后端 sticky 下的 stmt 生命周期（至少 SEND_LONG_DATA/CLOSE/RESET）。
- 连接失效时 stmt 失效与错误映射。
- 后续 sharding 再做 stmt 表/广播（不在当前范围）。
- 门禁：常见驱动（Go/Java/PHP）prepare 场景。

### F. 可观测与管理面

- Prometheus metrics：连接、QPS、延迟、错误、池、后端健康、慢查询。
- `/healthz` `/readyz`、version/build info。
- 结构化日志（JSON 可选）、access/slow/error 日志、连接 ID/remote addr。
- 管理面：状态、kill connection、池/后端详情（先只读）。
- 门禁：指标存在性、健康检查、日志可检索。

## 7. 建议路线图（单机）

| 阶段 | 内容 | 退出条件 |
|---|---|---|
| **S1 生产试点阻塞项** | Auth/TLS、capability 协商、超时/连接上限、优雅 drain、池健康、prepared 基础、metrics/health | MySQL 8 默认客户端可连；TLS 可连；无超时挂死；重启不丢请求；核心指标可见 |
| **S2 运维与正确性** | 会话变量对齐、读写分离健康/重试、访问/慢日志、每用户限制、配置扩展 | 会话变量不串；从库故障可回落；排障有日志 |
| **S3 硬化** | fuzz、故障注入、负载、MTR/RQG、压缩、查询指纹 | 故障/负载/长期运行稳定；性能基线 |
| **S4 可选** | 多租户、审计/防火墙、连接多路复用、管理面扩展 | 按业务需求 |

## 8. 当前不建议直接上生产的 Top 10 理由

1. MySQL 8 默认 `caching_sha2_password` 客户端无法登录。
2. 无前端/后端 TLS。
3. 客户端未协商 `CLIENT_DEPRECATE_EOF` 时结果集可能错帧。
4. 前端/后端都没有读超时，慢/死连接会永久占资源。
5. 无最大连接数/包大小/查询大小限制，存在 DoS 风险。
6. 池已补健康检查/idle/lifetime/预热，但查询失败不重试、不换库，池也还没有指标。
7. 优雅退出/drain 未实现，`termination_drain` 未生效。
8. `COM_STMT_SEND_LONG_DATA`、`COM_CHANGE_USER` 等常用命令不支持。
9. 无 metrics/health/admin/access log，线上不可观测。
10. 读写分离无延迟/健康/权重/查询失败重试，读一致性策略不足。

## 9. 参考

- 现有设计：`docs/architecture.md`、`docs/design/module-map.md`、
  `docs/design/refactor-seam-plan.md`、`docs/design/test-infrastructure.md`
- 开源对照：ProxySQL、Gaea、kingshard、Vitess、ShardingSphere 的能力面
- MySQL 协议：HandshakeV10 / HandshakeResponse41、COM_*、CLIENT_* 能力位、
  `caching_sha2_password`、TLS、session tracking
