# 装配与进程层（pkg/proxy、pkg/server、pkg/config、cmd/shardweave）

进程的骨架：命令循环、唯一 mode 分支点、配置与生命周期。API 形态对齐
kingshard / Gaea：`New → Run（阻塞 Accept）→ Close`，热更走 `UpdateConfig`。

## 包清单

| 包 | 职责 |
|---|---|
| `proxy` | `Serve`：一条前端连接的握手 + COM 循环；构造 `Statement` 后交给 Executor |
| `server` | 监听 / Accept / 连接上限 / **唯一按 mode 分支的装配点**（newExecutor 三 Router） |
| `config` | YAML spec + 严格校验 + 热更门禁（`Runtime` 快照） |
| `cmd/shardweave` | 入口：装配、信号循环（INT/TERM/QUIT 退出，USR1 热更 users/log） |

## 关键类型

- `proxy.SessionConfig`：`Users / Exec / 超时 / MaxAllowedPacket / Sharding`
  （sharding 会话 = 解析 AST + prepared 走 stmt 表，A14）。
- `server.New(cfg, path)`：校验 → Runtime → newExecutor → Listen。
- `config.Runtime.Apply`：快照换入；listen / mode / 拓扑 / server / pool 变更一律
  拒绝（要求重启），仅 users / log 可热更。

## 行为不变量

1. **只有 `pkg/server` 按 mode 分支**（依赖规则 1）；`Sharding` 会话开关是 mode
   翻译后的唯一残留，proxy/execute 拿到的是行为开关而非 mode。
2. **proxy 不 import `backend`**（依赖规则 3）：命令循环只做协议与会话字段。
3. **效果后置**：`stmt.Effect` 在后端响应成功后才应用到 session（USE/SET NAMES/
   autocommit/白名单变量）；`SET NAMES utf8mb4` 与 `EXPLAIN ROUTE` 本地应答。
4. **错误分级**：`*errs.MySQL` 写 ERR 且不断会话；池耗尽映射 TooManyConnections；
   其余错误终止会话。
5. **配置只产 spec**（依赖规则 10）：不建池、不 import 运行时模块。

## 已知缺口

优雅 drain（`termination_drain` 未接线，Close 后活跃会话被硬切）、accept 退避、
metrics / healthz / admin 面（M4 范围）。

## 测试入口

`proxy/serve_test.go`（fake Executor 的命令级用例）、`server/server_test.go`、
`config/config_test.go / runtime_test.go`（热更门禁）、`test/passthrough/`（e2e）。
