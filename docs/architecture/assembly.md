# 装配与进程层（pkg/server、pkg/config、cmd/shardweave）

进程的骨架：监听与 Accept、每条前端连接的命令循环、唯一 mode 分支点、配置与
生命周期。API 形态对齐 kingshard / Gaea：`New → Run（阻塞 Accept）→ Close`，
热更走 `UpdateConfig`。`pkg/proxy` 已并入本包（A15）：连接循环与装配同居
`pkg/server`，形态与 kingshard 的 server 包一致。

## 包清单

| 包 | 职责 |
|---|---|
| `server` | 监听 / Accept / 连接上限 / **唯一按 mode 分支的装配点**（newExecutor 三 Router）/ 前端连接的握手 + COM 循环（`serve.go`，原 `pkg/proxy`） |
| `config` | YAML spec + 严格校验 + 热更门禁（`Runtime` 快照） |
| `cmd/shardweave` | 入口：装配、信号循环（INT/TERM/QUIT 退出，USR1 热更 users/log） |

## 关键类型

- `server.SessionConfig`：`Users / Exec / 超时 / MaxAllowedPacket / Sharding`
  （sharding 会话 = 解析 AST + prepared 走 stmt 表，A14）。
- `server.Serve(ctx, conn, cfg)`：一条前端连接的握手 + COM 循环；构造 `Statement`
  后交给 Executor。
- `server.New(cfg, path)`：校验 → Runtime → newExecutor → Listen。
- `config.Runtime.Apply`：快照换入；listen / mode / 拓扑 / server / pool 变更一律
  拒绝（要求重启），仅 users / log 可热更。

## 行为不变量

1. **只有 `pkg/server` 按 mode 分支**（依赖规则 1）；`Sharding` 会话开关是 mode
   翻译后的唯一残留，连接循环拿到的是行为开关而非 mode。
2. **连接循环不触碰 backend（文件级约定，A15）**：`serve.go` 不 import / 不使用
   `backend`，只经 `execute.Executor` 交互。合并前这是包级 import 规则（proxy
   不 import backend），合并后 server 因装配需要 import backend（`buildCluster`），
   边界退到文件级，由 review 与 `serve.go` 的 import 清单守护。
3. **效果后置**：`stmt.Effect` 在后端响应成功后才应用到 session（USE/SET NAMES/
   autocommit/白名单变量）；`SET NAMES utf8mb4` 与 `EXPLAIN ROUTE` 本地应答。
4. **错误分级**：`*errs.MySQL` 写 ERR 且不断会话；池耗尽映射 TooManyConnections；
   其余错误终止会话。
5. **配置只产 spec**（依赖规则 10）：不建池、不 import 运行时模块。

## 已知缺口

优雅 drain（`termination_drain` 未接线，Close 后活跃会话被硬切）、accept 退避、
metrics / healthz / admin 面（M4 范围）。

## 测试入口

`server/serve_test.go`（fake Executor 的命令级用例）、`server/server_test.go`
（Accept/上限/热更门禁）、`config/config_test.go / runtime_test.go`（热更门禁）、
`test/passthrough/`（e2e）。
