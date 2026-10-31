# 实现计划：`backend_server` single + rwsplit

> 状态：**P0–P5 已完成**；**P7 stub 选源冒烟已定**（无 Docker）；**P6 差分仍待用户明示再跑**  

> 设计：[backend-server-single-rwsplit-design.md](backend-server-single-rwsplit-design.md)  
> 约束：TDD；聚焦包测试；**禁止**未要求时跑全量 `make test-unit`；未实现 mode 拒绝；规则禁止自创（对照设计 §5 + 附录 A）

---

## 总原则

1. **先测试语料，再实现**（§5.11 A/B/C 先红后绿）。  
2. **先会话状态机，再选源，再装配，最后 proxy 接线**。  
3. 每步结束：变更包 `go test -race -short -count=1` + `go vet`；涉及协议保真再跑差分（用户明示时）。  
4. 每步对应 code review 报告（提交前）；本计划不自动 commit。

```mermaid
flowchart LR
  P0[P0 设计冻结] --> P1[P1 config]
  P1 --> P2[P2 session AC/sticky]
  P2 --> P3[P3 router Pick]
  P3 --> P4[P4 BackendGroup]
  P4 --> P5[P5 proxy/cmd 接线]
  P5 --> P6[P6 single 回归]
  P6 --> P7[P7 rwsplit 冒烟]
```

---

## P0 — 设计冻结（文档）

**退出**：设计文状态改为「已定稿」；开放项有默认值。

| 开放项 | 默认锁定 |
|---|---|
| `mode` 缺省 | `single` |
| `EXPLAIN`/`DESC` | slave（S3） |
| 集成测进 CI | 有主从夹具后再加；本阶段单测为主 |
| Align 系统变量白名单 | 本阶段仅 database + charset |

**动作**：把上文默认值写回设计 §9；附录 A 保持。

---

## P1 — 配置：`backend_server`（TDD）

**包**：`pkg/config`

1. **Red**：`LoadFile`/`Validate` 表驱动  
   - `mode: single` + address/user OK  
   - 缺 mode → 视为 single  
   - `mode: rwsplit` 要 master；slaves 可空  
   - `mode: sharding` / 未知 mode → error  
   - 旧键 `backend:` → KnownFields 失败  
   - single 与 master 双写 → error  
2. **Green**：`BackendServer` / `BackendEndpoint` 结构 + Validate  
3. 改 `etc/prism.yaml`、现有 config 测例、`cmd`/`server` 读新字段（可先只支持 single 装配，rwsplit 结构能 Load）

**退出**：config 包测试全绿；示例 YAML 为 `backend_server.mode: single`。

---

## P2 — 会话：`Autocommit` + `NeedsSticky`（TDD）

**包**：`pkg/session`（必要时薄改 `pkg/proxy` 只测状态辅助函数）

1. **Red**：§5.9 / §5.11 A5–A7  
   - `IsInTransaction = InTx || !Autocommit`  
   - `NeedsSticky = IsInTransaction || Reserved`  
   - `SetAutocommit(true)` 清 InTx（跟 K/G）  
2. **Green**：扩展 `State`；保持现有 InTx/Reserved 测例  
3. **proxy**：解析 `SET autocommit` 启发式更新 State（可与 P3 同刀，但状态测必须先绿）

**退出**：session 包全绿；文档 B9 满足。

---

## P3 — 路由：`Classify` + `Pick`（TDD，核心）

**包**：新建 `pkg/router`（纯函数，无 Docker）

1. **Red（先提交语料）**：把设计 §5.11 A/B/C 落成 `route_test.go`，全部失败  
   - 每例注释：`ref: Gaea checkExecuteFromSlave` / `kingshard getBackendConn` 等  
2. **Green**：`ClassifyWantSlave`、`IsInTransaction`、`Pick` ≡ §5.10  
3. **Refactor**：前缀表与 M#/S# 常量对齐设计 §5.12  

**退出**：router 包全绿；禁止本步改 proxy 行为（可接口预留）。

---

## P4 — `BackendGroup`（多池叶子）

**包**：`pkg/pool` 旁或 `pkg/router`/`pkg/topology`（避免 pool 感知主从）

1. **Red**：mock Pool：round_robin；slave 失败回落 master；无 slave 全 master  
2. **Green**：`Group{Master, Slaves[], LB}` + `Acquire(target)`  
3. sticky 句柄仍由 proxy 持有（Group 只按 role 出池）

**退出**：Group 单测绿；仍不接真 MySQL。

---

## P5 — 装配接线：`server` / `cmd` / `proxy`

**包**：`pkg/server`、`cmd/prism`、`pkg/proxy`

1. **Red**：proxy 测：给定 fake Group + sticky，事务中 SELECT 不换连接（可接口注入 Pick）  
2. **Green**：  
   - `SessionConfig` 持有 Group + Pick  
   - `withBackend`：先 Pick → Acquire → Align → 执行 → NeedsSticky? keep : Release  
   - `mode=single`：Group 仅 master（行为=今日）  
3. cmd：按 config 建池；热更仍拒绝改 backend_server 地址  

**退出**：proxy/server/cmd 相关测绿；手动 `single` 可连。

---

## P6 — single 差分回归

**仅在用户要求或本步明确执行时**：`test/differential` 现有 scenarios。  

**退出**：与改前同等通过；无 rwsplit 夹具亦可。

---

## P7 — rwsplit 选源冒烟（stub，无 Docker）

1. **已定**：用 tagged stub 池证明 SELECT→slave、写/事务→master（见 [rwsplit-route-smoke-design.md](rwsplit-route-smoke-design.md)）  
2. 不强制 Docker 一主一从；真拓扑冒烟后置  
3. code review 可并入 executor 报告或本切片短报  
4. `sharding` 仍拒绝  

## 执行顺序口令（更新）

**P7 stub 冒烟** →（可选）P6 single 差分 → M1 收尾 / M2。

---

## 明确不做（本计划边界）

- `sharding` 实现  
- dbpack QueryAll、weight DSL `r0w10`  
- Gaea 只读用户体系 / keep-session 全套  
- 健康检查摘流（可留接口）  
- 完整 SQL AST 依赖  

---
