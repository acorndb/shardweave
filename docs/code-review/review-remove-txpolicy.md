---
title: 移除未消费的 plan.TxPolicy
files:
  - pkg/plan/plan.go
---

## Must Fix

无。`TxPolicy` 全仓库只有声明、赋值和测试断言，没有任何执行路径读取它（`grep -rn
"\.Tx\b" pkg/ test/` 已核实），删除后 `plan` / `execute` / `server` 聚焦测试全绿。

## Should Fix

无。

## Nit

### 设计档案仍保留 TxPolicy 的历史代码块
- status: fixed
- file: docs/design/sharding-p0-design.md

`sharding-p0-design.md` 是 P0 设计档案，正文保留原始设计片段，只在代码块后加了
移除说明，并删掉了「`docs/design/` 被 .gitignore 忽略」这条已过时的提交提示
（该规则已在本轮之前的提交中删除）。

## 附：同类边界测试

同批补了三条 DELETE LIMIT 路由用例（`pkg/route/shard/router_test.go`）：
单片放行、无分片键 1235、跨片 IN 1235。均通过，无生产代码改动。
