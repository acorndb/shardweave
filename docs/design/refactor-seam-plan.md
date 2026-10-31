# 重构落地计划：P1/P2 seam refactor

> 状态：讨论稿 v0.1
> 日期：2026-10-31
> 上位：[module-map.md](module-map.md)、[architecture.md](../architecture.md)
> 范围：**行为保持**的模块归属与接口重构（P1/P2），不含分片功能。
>
> 提交提示：`docs/design/` 当前被 `.gitignore` 忽略；本文件需
> `git add -f docs/design/refactor-seam-plan.md`。

## 0. 一句话

用 6 个可独立回滚的 PR，把当前"`router` 持有拓扑 + `executor` 直写前端"的形状，
迁移到"`backend.Slice` + `route/source` + `plan` + `result` + `execute`"的目标形状。
每个 PR 行为不变，现有差分校准兜底；分片功能（P3）在 R6 之后开始。

## 1. 目标与非目标

### 1.1 目标

- 资源层与路由层分开：`backend.Slice` 持有主从与池；`route/source` 只做选源。
- 执行层有 Plan 插槽：`single`/`rwsplit` 走同一条 N=1 Plan → execute 路径。
- 结果层有 Stream 抽象：N=1 仍走 raw packet，不因为引入接口而解码。
- 配置切到 `backend` + `slices`，`mode` 只影响装配。
- 删除 `shardingExecutor` 假缝；分片将作为 Router/Plan 演进，而不是第二个执行器。
- 每一步都能独立验证、独立回滚，不出现"重构到一半不能跑"的状态。

### 1.2 非目标

- 不实现分片路由、分片规则、跨片执行、归并（P3/P4）。
- 不实现表名改写（P4）。
- 不动 `pkg/mysql` 的协议行为（只允许包内必要重命名）。
- 不做 admin / observability / 多租户 / 2PC。
- 不在 R1–R4 改 YAML；R5 才做 breaking 配置变更。
- 不引入新的运行时依赖。

### 1.3 整体成功标准

- `single` / `rwsplit` 的外部行为与重构前一致；差分校准全绿。
- `pkg/mysql` 除必要重命名外无 diff。
- `pkg/execute` 中不再出现 `*backend.MySQLBackend` 类型断言。
- 配置只接受 `backend.slices` 形状；`mode=sharding` 仍明确拒绝。
- 每个 PR 的 code review 报告无 `status: open` 的 Must/Should。

## 2. 每个 PR 都必须满足的不变量

1. **行为不变**：R1–R3、R6 是纯重构；R4 只修正"执行成功后才更新会话状态"；R5 只改配置形状与校验。
2. **协议不变**：不修改 `pkg/mysql` 的帧语义、sequence 处理、`ForwardResponse` 逻辑。
3. **只跑聚焦测试**：按 AGENTS.md，不自动跑全量 `make test-unit` / `make test`；差分需要 Docker，由用户明确批准后运行。
4. **测试先行**：新增接口/行为先写失败测试；纯移动类重构先确认既有测试覆盖，再 `git mv` 测试并保持全绿。
5. **依赖规则**：每个 PR 结束跑依赖 grep（见 §6），不允许新增反向依赖。
6. **Code review**：每个 PR 按 AGENTS.md 产出 `docs/code-review/review-<topic>.md`；Must/Should 必须修复。
7. **文档同步**：涉及配置/包名/接口的 PR 同步更新 `architecture.md`、`module-map.md`、`README.md`、`etc/prism.yaml`。

## 3. 基线盘点

### 3.1 当前包与测试

| 当前包 | 文件 | 测试 | 说明 |
|---|---|---|---|
| `pkg/backend_server` | `backend.go`、`pool.go` | `align_test.go`、`pool_test.go` | 池 + MySQL 连接；`Backend` 接口只有 Reset/Close |
| `pkg/router` | `group.go`、`pick.go`、`classify.go` | `group_test.go`、`route_test.go` | 拓扑 + 主从选源 + 池生命周期混在一起 |
| `pkg/executor` | `executor.go`、`forward.go` | `executor_test.go` | passthrough + sharding 桩；直写前端 |
| `pkg/session` | `state.go` | `state_test.go` | 最小会话状态 |
| `pkg/proxy` | `session.go` | `session_test.go` | COM 循环 + USE/SET 字符串解析 |
| `pkg/config` | `config.go`、`runtime.go` | `config_test.go` | `backend_server` 字段拍平 |
| `pkg/server` | `server.go` | `server_test.go` | 按 mode 手工建 Group + Executor |
| `pkg/sql/parser` | `parser.go`、`driver.go` | `parser_test.go` | 仅包装，数据面未用 |
| `test/differential` | 4 个文件 | 4 个测试文件 | 透传差分基线 |

### 3.2 既有测试能覆盖什么

- `pkg/router/group_test.go`：主从获取、从库回落、LB 顺序 —— 迁到 `backend.Slice` 后必须原样通过。
- `pkg/router/route_test.go`：`Pick` / `ClassifyWantSlave` 规则表 —— 迁到 `route/source` 后必须原样通过。
- `pkg/executor/executor_test.go`：选源/mode/sticky 骨架；**没有覆盖真正的执行路径**（因为 `execCommand` 需要 `*MySQLBackend`）。R6 需要用 fake `Conn` 补上。
- `pkg/proxy/session_test.go`：USE/SET/autocommit 解析 —— R4 迁到 `pkg/sql` 时测试跟着走。
- `pkg/backend_server/align_test.go`：Align 行为 —— 迁到 `pkg/backend` 后保留。
- `test/differential`：协议与结果保真的总兜底；R1–R6 每个 PR 建议由用户批准跑一次。

## 4. PR 计划

| PR | 阶段 | 内容 | 行为变化 | 主要门禁 |
|---|---|---|---|---|
| R1 | P1a | 包归属：`backend` / `route/source` / `constant.Role` | 无 | 迁移后的既有测试 + 差分 |
| R2 | P1b | `sql.Statement` + `plan.Plan`；single/rwsplit 生成 N=1 Plan | 无 | plan/sql 单测 + 差分 |
| R3 | P1c | `result.Stream` + `execute.Pipeline`；N=1 raw；删 sharding 桩 | 无 | execute 单测 + 差分 |
| R4 | P2a | USE/SET 语义移到 `sql`；会话状态"成功后更新" | 小（错误路径修正） | sql/session/proxy 单测 + 差分 |
| R5 | P2b | 配置切到 `backend` + `slices`；`SameTopology` | 配置 breaking | config 单测 + 差分 + rwsplit 冒烟 |
| R6 | P2c | `backend.Conn.Exec/Align`；`Cluster`；去向下转型 | 无 | fake Conn 单测 + 差分 |
| R7 | P3 | 分片规则、单分片、Reject、EXPLAIN ROUTE | 新功能 | 见 `sharding-p0-design.md` |

R1–R6 属于本次重构；R7 是 P3 的分片实现，列在这里只说明衔接点，不在重构范围。

---

### R1：资源层与路由层的包归属

**目标**：把拓扑从 `router` 挪到 `backend`，把选源从 `router` 挪到 `route/source`；不改逻辑。

**文件动作**

| 当前 | 目标 | 说明 |
|---|---|---|
| `pkg/backend_server/backend.go` | `pkg/backend/conn.go` | `Backend` 接口 → `Conn`；`MySQLBackend` → `MySQLConn` |
| `pkg/backend_server/pool.go` | `pkg/backend/pool.go` | `Config` → `PoolConfig`；`New` → `NewPool` |
| `pkg/backend_server/align_test.go` | `pkg/backend/conn_test.go` | 按被测文件改名；不要留无对应生产文件的测试名 |
| `pkg/backend_server/pool_test.go` | `pkg/backend/pool_test.go` | 纯移动 |
| `pkg/router/group.go` | `pkg/backend/slice.go` | `Group` → `Slice`；`Acquire/Release/Discard/Close` 原样 |
| `pkg/router/group_test.go` | `pkg/backend/slice_test.go` | 断言不变，只改包名/类型名 |
| `pkg/router/pick.go` | `pkg/route/source/pick.go` | `Pick` → `source.Pick` |
| `pkg/router/classify.go` | `pkg/route/source/classify.go` | `ClassifyWantSlave` → `source.ClassifyWantSlave` |
| `pkg/router/route_test.go` | `pkg/route/source/pick_test.go` + `classify_test.go` | 按生产文件拆分；跨文件用例可在文件头注明 |

**类型决策**

- `TargetRole` / `RoleMaster` / `RoleSlave` → `constant.Role`（`type Role string`）。
  理由：`backend` 与 `route` 都要用角色，但 `route` 不能 import `backend`；
  `constant` 是依赖自由的共享词汇。
- `pkg/route/source` 允许 import：`session`、`constant`、`mysql`（命令常量）。
  **禁止** import `backend`。
- `pkg/backend` 允许 import：`mysql`、`constant`；禁止 import `session` / `route`。

**Red / Green**

- 这是纯重构：先确认 `group_test.go` / `route_test.go` 覆盖了现有行为；
- `git mv` 代码与测试，只改包名/类型名/import；
- 不重写断言、不改测试数据；
- 新增编译期断言：`var _ Conn = (*MySQLConn)(nil)`。

**退出条件**

- `go test -race -short -count=1 ./pkg/backend/ ./pkg/route/... ./pkg/executor/ ./pkg/server/` 全绿；
- 差分（用户批准后）全绿；
- `pkg/router` 目录删除（P3 再建 `pkg/route/shard`）。

**风险 / 回滚**

- 风险：import cycle（`route/source → backend`）、角色类型替换遗漏、测试文件命名不符合 AGENTS。
- 回滚：revert 单个 PR 即可，无数据/配置迁移。

---

### R2：`sql.Statement` + `plan.Plan` 骨架

**目标**：让 SQL 在 `proxy` 只解析/构造一次；single/rwsplit 通过 Plan 进入执行层。
仍然只有一个目标，仍然走现有转发路径。

**文件动作**

- 新增 `pkg/sql/statement.go`：

```go
type Statement struct {
    Cmd     byte
    Payload []byte      // 完整命令包（含 cmd 字节），转发时逐字节使用
    Raw     string      // COM_QUERY 文本
    AST     ast.StmtNode // 可空；R2 不强制解析
    Kind    Kind
    Err     error
}
```

- 新增 `pkg/route/route.go`：`Target`、`Router` 接口。
- 新增 `pkg/plan/plan.go`：`Kind`、`Plan`、`Builder`、`MergeSpec`（空值）。
- `pkg/route/source`：把 R1 的函数包装成 `source.AllMaster` / `source.Pick`，
  实现 `route.Router`。
- `pkg/proxy`：把 `executor.Request` 换成 `sql.Statement`；`Payload` 必须原样保留。
- `pkg/executor`：`Handle` 先 `plan.Build`，再按 `Plan.Targets[0].Role` 从 `Slice` 取连接；
  仍用现有 `execCommand` / `ForwardResponse`。

**Red / Green**

- 先写 `pkg/plan/plan_test.go` 表驱动：
  - single → 恒 master；
  - rwsplit SELECT → slave；
  - rwsplit INSERT → master；
  - sticky/事务 → master；
  - R2 的 `Kind` 恒为 `Passthrough`。
- 先写 `pkg/sql/statement_test.go`：COM_QUERY 构造、`Payload` 与原始字节一致。
- 更新 `pkg/executor/executor_test.go`：断言消费 Plan，而不是直接 pick。

**退出条件**

- 聚焦测试全绿；差分全绿；
- 转发字节与重构前一致（差分覆盖）。

**风险 / 回滚**

- 风险：`Statement.Payload` 重建导致字节变化；禁止重新拼接 SQL，必须复用原包。
- 回滚：revert；Plan 只是新增一层，不持久化状态。

---

### R3：`result.Stream` + `execute.Pipeline`

**目标**：执行层统一成一条 Pipeline；N=1 走 raw stream；删除 `shardingExecutor`。

**文件动作**

| 当前 | 目标 |
|---|---|
| `pkg/executor/` | `pkg/execute/` |
| `pkg/executor/executor.go` | `pkg/execute/execute.go` |
| `pkg/executor/forward.go` | `pkg/execute/forward.go` |
| `pkg/executor/executor_test.go` | `pkg/execute/execute_test.go` |
| 新增 | `pkg/result/result.go`（`Field`/`Row`/`Stream`） |

- `execute.Runtime` 持有 `*session.State` + 单条 lease（P3 再扩成 map）。
- `execute` 内部定义最小 `rawForwarder` 接口：

```go
type rawForwarder interface {
    ForwardTo(ctx context.Context, front *packet.Conn) error
}
```

- R3 暂时保留 `MySQLConn.Packet()` 与 `execute` 内部的 raw 适配；R6 用
  `Conn.Exec` 替换，去掉这个临时缝。
- 删除 `shardingExecutor`、`ErrShardingNotImplemented`；`mode=sharding` 仍由
  `config.Validate` 拒绝。
- `executor.New(mode, group)` → `execute.NewPipeline(builder, slice)`；mode 只在
  `pkg/server` 用于选择 Router。

**Red / Green**

- `pkg/result`：Stream 接口的最小契约测试（fake stream）。
- `pkg/execute`：fake `rawForwarder` + fake backend，覆盖：
  - 单目标 raw 转发；
  - sticky 会话不释放；
  - 非 sticky 释放；
  - 连接断开时 discard。
- `pkg/proxy`：只改 import/类型，行为不变。

**退出条件**

- 聚焦测试全绿；差分全绿；
- `grep -r "shardingExecutor\|ErrShardingNotImplemented" pkg/` 为空。

**风险 / 回滚**

- 风险：`ForwardResponse` 的 sequence/EOF 行为被包装层改变。缓解：包装层只负责
  acquire/release 与调用原函数，不改 `pkg/mysql/forward.go`。
- 回滚：revert；Pipeline 与旧 executor 行为等价。

---

### R4：SQL 语义与会话状态时机

**目标**：`proxy` 不再做 SQL 字符串解析；USE/SET 语义进 `pkg/sql`；
会话状态在**执行成功后**更新。

**文件动作**

- 新增 `pkg/sql/session_cmd.go`（或并入 `statement.go`）：
  `ParseUse`、`ParseSetNames`、`ParseSetAutocommit`，返回 `sql.Effect`。
- `Statement` 增加 `Effect *Effect`（或 `Effect` 值）。
- `pkg/proxy/session.go`：
  - 删除 `parseUse/parseSetNames/parseSetAutocommit`；
  - 先构造 `Statement`，执行成功后 `session.Apply(stmt.Effect)`；
  - 失败时不动状态。
- `pkg/session`：新增 `Apply(effect)` 或对应 setter；补 `Reset` 测试。
- `session.StmtTable` **不在 R4 落地**：prepare 的 id 映射需要解析
  `COM_STMT_PREPARE_OK`，属于 P3 的 sharding prepare 设计，避免引入未使用类型。

**Red / Green**

- 先写 `pkg/sql/session_cmd_test.go`：覆盖现有的 USE/SET NAMES/autocommit 解析表，
  与原 `proxy` 测试一一对应。
- 先写 `pkg/proxy/session_test.go` 失败路径：执行返回错误时，DB/charset/autocommit 不变。
- 成功路径保持现有差分结果。

**退出条件**

- 聚焦测试全绿；差分全绿；
- `grep -n "parseUse\|parseSetNames\|parseSetAutocommit" pkg/proxy` 为空。

**风险 / 回滚**

- 风险：`USE db` 失败后状态已改的旧行为被修正，可能影响依赖旧行为的测试；这是有意修正。
- 回滚：revert；状态时机变更集中在 R4。

---

### R5：配置切到 `backend` + `slices`

**目标**：配置形状统一；`mode` 只做校验预设；不实现 sharding。

**文件动作**

- `pkg/config/config.go`：
  - `Config.Backend BackendConfig`，YAML `backend`；
  - `BackendConfig{Mode, Slices, LoadBalance, Sharding}`；
  - `SliceConfig{Name, Master, Slaves}`；
  - `ShardingConfig` / `ShardRule` 结构先定义，但 `mode=sharding` 仍拒绝；
  - `Primary()` / `SameIdentity` → `SameTopology`；
  - `Clone` 深拷贝 slices。
- `pkg/server`：从 `BackendConfig.Slices` 构建单个 `backend.Slice`
  （single/rwsplit 只有 1 个）；`mode` 校验通过后才装配。
- `etc/prism.yaml`、`README.md`、`docs/architecture.md`、`docs/design/*` 同步。
- 配置迁移：pre-1.0 直接 breaking；旧字段不进新结构体，`KnownFields(true)` 报错。

**Red / Green**

- `pkg/config/config_test.go` 表驱动：
  - single：1 slice、0 slave、无 sharding、无 LB；
  - rwsplit：1 slice、≥1 slave；
  - sharding：Validate 返回 not implemented；
  - 未知字段/新旧混用/规则引用不存在的 slice → 错误；
  - `SameTopology` 对 slices/rules/LB 的变化返回 false。
- `pkg/server/server_test.go`：用新 YAML 构造 server；`Addr()` 等行为不变。

**退出条件**

- config/server 单测全绿；差分全绿；rwsplit 冒烟（Docker，用户批准后）。

**风险 / 回滚**

- 风险：用户旧配置全部失效；需要文档与示例同步。
- 回滚：revert R5；由于未部署，无数据迁移。

---

### R6：`backend.Conn` 接口与 Cluster

**目标**：执行层不再向下转型；`Conn` 提供 `Exec/Align`；`Cluster` 统一承载 Slices。

**文件动作**

- `pkg/backend/conn.go`：

```go
type Conn interface {
    Exec(ctx context.Context, db, query string) (result.Stream, error)
    Align(ctx context.Context, s AlignSettings) error
    Reset(ctx context.Context) error
    Close() error
}
```

- `MySQLConn` 实现 `Exec`：发送命令并返回 packet-backed `RawStream`。
- `pkg/backend/cluster.go`：`Cluster{Get, Names}`；`server` 从 `slices` 构建。
- `pkg/execute`：
  - 删除 `writeBackend` / `execCommand` 中对 `*MySQLBackend` 的断言；
  - 使用 `Conn.Exec`，N=1 从 `RawStream` 直通；
  - `Runtime.leases` 改为 `map[string]*lease`（single/rwsplit 仍只有 1 个 key）。
- `pkg/backend` 依赖 `pkg/result`；`result` 仍是叶子。

**Red / Green**

- fake `Conn`：脚本化返回 `RawStream`，覆盖 Exec/Align/Reset/Close。
- `execute` 测试：fake Plan + fake Conn + fake Stream，不依赖 MySQL。
- `backend` 池测试：fake Conn，覆盖 Release/Discard/Reset 失败。

**退出条件**

- 聚焦测试全绿；差分全绿；rwsplit 冒烟；
- `grep -rn "MySQLBackend\|\\.Packet()" pkg/execute` 为空。

**风险 / 回滚**

- 风险：接口变宽导致大量测试编译失败；按包逐步改，先 fake 后实现。
- 回滚：revert R6；R1–R5 的结构保留。

---

## 5. 测试迁移与新增矩阵

| 现有测试 | 目标位置 | 处理 |
|---|---|---|
| `pkg/backend_server/pool_test.go` | `pkg/backend/pool_test.go` | 纯移动 |
| `pkg/backend_server/align_test.go` | `pkg/backend/conn_test.go` | 按被测文件改名；保留断言 |
| `pkg/router/group_test.go` | `pkg/backend/slice_test.go` | 纯移动；`Group`→`Slice` |
| `pkg/router/route_test.go` | `pkg/route/source/pick_test.go` + `classify_test.go` | 按生产文件拆分 |
| `pkg/executor/executor_test.go` | `pkg/execute/execute_test.go` | 保留骨架；新增 Pipeline 用例 |
| `pkg/proxy/session_test.go` | 保留 proxy 用例 | 解析用例迁到 `pkg/sql/session_cmd_test.go` |
| `pkg/config/config_test.go` | 同路径重写 | 覆盖新 `backend`/`slices` |
| `pkg/session/state_test.go` | 同路径扩展 | `Apply`/`Reset` 用例 |
| `pkg/server/server_test.go` | 同路径更新 | 新配置结构 |
| `test/differential` | 不动 | 每 PR 的兜底 |

## 6. 验证命令

每个 PR 只跑变更包（示例，按实际改动裁剪）：

```sh
go test -race -short -count=1 \
  ./pkg/backend/ ./pkg/route/... ./pkg/sql/ ./pkg/plan/ \
  ./pkg/execute/ ./pkg/proxy/ ./pkg/session/ ./pkg/config/ ./pkg/server/
go vet ./pkg/...
make fmt
```

差分（需要 Docker，**等用户明确批准**）：

```sh
go test -count=1 ./test/differential/
```

依赖规则检查（每个 PR）：

```sh
grep -rn "pkg/backend" pkg/route pkg/plan pkg/session && exit 1
grep -rn "ModeSingle\|ModeRWSplit\|ModeSharding" \
  pkg/execute pkg/proxy pkg/session pkg/backend pkg/route pkg/plan && exit 1
```

## 7. 风险与回滚

| 风险 | 缓解 |
|---|---|
| import cycle（route/source → backend） | R1 只允许 `constant.Role` 共享；依赖 grep 进 CI |
| 转发字节漂移 | 不重新拼接 SQL；`Statement.Payload` 复用原包；差分兜底 |
| 测试随包移动后命名不符合 AGENTS | 按"生产文件名 + `_test`"拆分；跨文件用例在文件头注明 |
| 配置 breaking 导致旧 YAML 全挂 | 文档/示例同步；`KnownFields` 报错清晰；pre-1.0 接受 |
| R3 临时保留 `Packet()` 下转型 | R6 必须删除；DoD 有 grep 检查 |
| 范围蔓延到分片实现 | 非目标清单；R7 之前不写 `route/shard`、不写 merge |
| sticky/lease 行为变化 | R3/R6 用 fake Conn 覆盖 release/discard/reset；差分兜底 |

## 8. 完成定义（DoD）

- 代码中不再有 `pkg/backend_server`、`router.Group`、`shardingExecutor`、
  `Executor.Mode()`。
- `pkg/route` / `pkg/plan` 不 import `pkg/backend`；`pkg/session` 不 import `pkg/backend`。
- `pkg/execute` 中无 `*MySQLBackend` 类型断言、无 `.Packet()` 调用。
- `single` / `rwsplit` 差分全绿，外部行为与重构前一致。
- 配置只接受 `backend.slices`；`mode=sharding` 仍明确拒绝。
- 每个 PR 的 review 报告无 `status: open` 的 Must/Should。
- `architecture.md`、`module-map.md`、`README.md`、`etc/prism.yaml` 与实现一致。
- 没有新增运行时依赖；`pkg/mysql` 无行为 diff。

## 9. 后续

R6 完成后，目标形状已就位，进入 P3：

1. 先写 [sharding-p0-design.md](sharding-p0-design.md)；
2. R7：`config.sharding.rules` + `route/shard` + `Plan.SingleShard/Reject` +
   `EXPLAIN ROUTE` + `StmtTable`/prepare 广播 + 事务钉片；
3. P4：`MultiShard` + dispatch + merge + 变形 harness。
