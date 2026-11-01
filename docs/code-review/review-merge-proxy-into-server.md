---
title: pkg/proxy 并入 pkg/server（A15）review
files:
  - pkg/server/serve.go
  - pkg/server/server.go
---

## Must Fix

无。这是一次行为保持的包合并（git mv + 改包名），验证锚点：

- `pkg/server` 全部测试（原 `proxy` 命令级用例 + 原 `server` Accept/上限用例）
  `-race -short` 全绿，合并进同一包后无符号冲突；
- 三个外部消费方（`test/differential`、`test/passthrough`）改 import 后编译、
  `-short` 运行通过；
- 全仓库 `shardweave/pkg/proxy` 引用清零；`go build` / `go vet ./...` 干净。

## Should Fix

### 「连接循环不触碰 backend」从包级规则降级为文件级约定
- status: fixed
- file: pkg/server/serve.go

合并前「proxy 不 import backend」由编译器保证；合并后 `pkg/server` 因装配需要
import backend（`buildCluster`）。该约束降级为**文件级约定**：`serve.go` 的
import 清单不含 `backend`（已核对：仅 errs/execute/mysql/packet/mysqlserver/
session/sql/parser/zerocopy）。守护方式：`docs/architecture/assembly.md` 不变量 2
明确写出该约定与降级原因，依赖规则 3 同步改写（README §3），并新增决策 A15。
后续 review `serve.go` 时须检查 import 清单未引入 backend。

### 错误消息前缀随包名变更
- status: fixed
- file: pkg/server/serve.go

`"proxy: nil executor"` → `"server: nil executor"`。全仓库无测试断言旧文案
（已 grep 核实），无消费方匹配该字符串，属安全的日志文案变更。

## Nit

### 测试 helper 命名仍含 "proxy" 字样
- status: wontfix
- file: pkg/server/serve_test.go

`newProxySession` 等 helper 名保留：进程本身仍是代理，helper 描述的是「面向
前端的会话」角色，改名纯属折腾，无清晰度收益。

### server.SessionConfig 命名略显冗余
- status: wontfix
- file: pkg/server/serve.go

包内 `SessionConfig` 带 `server.` 前缀略重复，但保留原名让三个 test 消费方的
diff 降到最小，且名字本身准确（单连接装配参数）。不值得为命名美学扰动调用方。
