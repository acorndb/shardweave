# AGENTS.md

sharding-proxy 是 Go 1.25+ 项目（module: `github.com/sharding-proxy`）：MySQL 分库分表代理。
- 实现偏离已定设计时：**先改文档，再改代码**。

## 测试与 TDD 规范（强制）

所有代码变更必须遵循 TDD，并保证单元测试覆盖。此规范适用于整个仓库。

### TDD 流程：Red → Green → Refactor

1. **Red**：先写描述目标行为、且当前会失败的测试。测试必须能编译，运行应失败。
2. **Green**：编写刚好让测试通过的实现，不超前、不加此刻不需要的功能。
3. **Refactor**：测试全绿后再重构实现与测试（去重、命名、结构）。

每次行为变更都重复此循环；**不允许「先写实现再补测试」**。

### 单元测试覆盖要求

- 所有新增或修改的导出函数、方法、类型必须有对应单元测试。
- 非导出函数若含分支逻辑，也应覆盖。
- **Bug 修复**：必须先写能复现该 Bug 的失败测试，再修实现。协议/语义类 Bug 同时考虑落入 `test/differential` 语料（CalibrationScript / corpus）。
- **重构**：重构前确认已有测试覆盖该行为；重构后全绿、行为不变。
- 须覆盖：正常路径、边界条件、错误分支。

### 测试约定

- 测试文件与被测代码同包，命名 `xxx_test.go`。
- 优先**表驱动** + `t.Run(tc.name, ...)`（参考 `test/differential/` 中的对比与脚本风格）。
- 断言使用 `github.com/stretchr/testify`。
- **单元测试**（`-short`）：不得依赖 Docker、真实 MySQL、网络或外部服务；用 fake/stub/内存实现。
- **差分 / 集成**：放在 `test/differential/`（或后续 `test/e2e/`）；Docker 不可用时跳过。需要 Docker 的用例须在 `-short` 下 Skip。
- 可选：纯集成文件可用 `xxx_integration_test.go` + 首行 `//go:build integration`；与默认 `go test` 分离时须在文档/Makefile 中说明如何跑。
- 测试命名：`TestXxx`；差分校准类保持现有 `TestNullProxyCalibration` / 后续 `TestPassthroughDifferential` 风格。

### 运行与验证

| 命令 | 含义 |
|---|---|
| `make test-unit` | `go test -race -short -count=1 ./...`（无 Docker） |
| `make test-integration` | 差分校准（需要 Docker） |
| `make test` | unit + integration |
| `make lint` | `go vet` + golangci-lint |
| `make fmt` | gofumpt（优先）/ gofmt |
| `make build` | `go build ./...` |

**禁止 Agent 自动运行**完整的 `make test-unit`、`make test`、`make test-integration`：只有用户在当前任务中**明确要求**执行时才可运行，不能因「提交前验证」「门禁检查」或历史习惯自行触发。

默认验证方式：

1. 对**本次变更涉及的包**跑聚焦测试，例如：  
   `go test -race -short -count=1 ./mysql/packet/ ./pool/ ...`
2. 需要覆盖率时对上述包加 `-cover`（或 `-coverprofile`），新增可执行逻辑应有对应用例盖住；目标是变更相关包保持全绿且新增分支可测。
3. 改动后跑 `make lint`（若环境无 golangci-lint，至少 `go vet` 变更包）。
4. 涉及协议行为或代理语义时，在用户明确要求下再跑差分；差分语料随 Bug 增长。

### 提交前自检清单

- 是否先写测试，再写实现？
- 变更代码是否都有对应单测？
- 是否覆盖正常 / 边界 / 错误？
- 单测是否不依赖 Docker / 真实 MySQL / 外网？
- 协议 Bug 是否已考虑写入差分语料？
- 变更包聚焦测试是否全绿？
- `make lint`（或等价 vet）是否干净？
- 是否违反 `mysql/` 依赖边界或三态契约？
- Code Review 报告中是否还有 `status: open` 的问题？（见下节）

## Code Review 流程（强制）

所有功能提交（feat/fix/refactor）在提交前须经过一轮完整 code review；发现问题的修复情况以报告落盘，供人工/后续门禁核对。

### Review 时机与方式

1. **时机**：实现完成、变更包聚焦测试通过之后，提交之前。
2. **方式**：先跑格式与静态检查（`make fmt` 意图下的 gofumpt/gofmt、`go vet` / `make lint`），再逐文件精读 diff：错误处理、命名、并发、接口、资源释放、协议正确性、日志、测试；并对被改函数的**全部调用点**做交叉验证。
3. **问题分级**：
   - **Must Fix**：正确性缺陷（逻辑错误、协议错序、结果静默错误、并发、泄漏）、安全风险、明显性能反模式；
   - **Should Fix**：可观测性缺失、死代码/死参数、误导性命名或注释、依赖方向侵蚀；
   - **Nit**：风格与可选优化——可不修，但须记录理由。
4. **修复后再提交**：Must Fix 与 Should Fix 必须修复；Nit 可选。

### Review 报告规范（docs/code-review/）

结论落盘为 `docs/code-review/review-<变更主题>.md`：

```markdown
---
title: <变更主题> review
files:
  - <本报告覆盖的生产文件路径，与 git diff 路径一致>
---

## Must Fix

### <问题标题：一句话描述>
- status: open | fixed | wontfix
- file: <相关文件路径>

<问题描述：位置、影响、修复方式。wontfix 必须写明不修的理由。>

## Should Fix
（同上结构）

## Nit
（同上结构；wontfix 居多）
```

要点：

- `files:` 列出报告覆盖的生产文件；本次变更若与某报告 `files:` 有交集，则该报告内不允许残留 `status: open`。
- 每个问题必须有 `### 标题` + `- status:`；无 status 的说明段落不计为问题。
- 修复后将 `open` 改为 `fixed`（保留条目）；不修的改为 `wontfix` 并写理由。

### 与 CI / 本地门禁的关系

- CI（`.github/workflows/ci.yml`）跑 `make test-unit` 与 `make test-integration`、以及 `make lint`。
- 仓库若后续增加 `make coverage-gate`（增量覆盖率 + review `open` 扫描），以其为准；在此之前 Agent 仍须遵守本节报告格式与「无 open 再提交」约定。
- **不要**为了「模拟门禁」自动跑全量 `make test`；等用户显式要求。

## 实现时额外约束（本仓库）

- M1 主路径：`NoopRouter → PassthroughPlan → Dispatch(1) → IdentityMerger`；分片/改写/聚合只经预留接口演进。
- 不采用 Gaea 式「全局物化再计算」作为默认跨片路径；分片规则后置调研。
- 配置中的 `sharding` 段：M1 可忽略并 warn，不得假装已生效。
- 用户变量 / 临时表 / 锁：会话 **reserved** 钉后端连接；系统变量白名单借连对齐；归还池前优先 `COM_RESET_CONNECTION`。
- Prepare：前后端均走二进制协议（见 M1 设计 D2-A）。

### 协议层实现原则（强制）

- **对照移植，少自创**：`mysql/packet`、`auth`、握手与 COM 处理以 [`go-mysql-org/go-mysql`](https://github.com/go-mysql-org/go-mysql) 的包结构与 API 约定为第一参考（如 `WritePacket` 前 4 字节预留 header、`Sequence` 字段）；auth/能力协商细节对照 Vitess `go/mysql`。
- 代码第一方维护，**不**把 go-mysql / Vitess 整库当运行时黑盒依赖；可复制/改编必要片段并注明来源。
- 不发明与上述项目不一致的线格式或帧语义；压缩等复杂路径 P0 不做，需要时再按开源实现补。
