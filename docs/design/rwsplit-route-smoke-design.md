# P7：rwsplit 选源冒烟（stub，无 Docker）

> 状态：**已定稿 / 本切片实现**  
> 日期：2026-10-31  
> 上位：[backend-server-single-rwsplit-plan.md](backend-server-single-rwsplit-plan.md) P7、[executor-three-mode-design.md](executor-three-mode-design.md)

## 0. 目标

证明 **RWSplitExecutor 把只读打到 slave 池、事务/写打到 master**。  
不验证复制延迟、不验证结果集保真（那是 single 差分 / 后置真拓扑）。

## 1. 方法

- stub `backend_server.Pool`（tag = `master` / `slave`），经 `withBackend` 观察 Acquire 标签与 open 计数。  
- `-short` 可跑；**禁止**依赖 Docker / 真 MySQL。  
- 语料放 `pkg/executor`（与策略同包），表驱动覆盖常见 COM_QUERY。

## 2. 验收清单

| 场景 | 期望池 |
|---|---|
| 自动提交 `SELECT` / `SHOW` / `EXPLAIN` | slave |
| `INSERT` / `UPDATE` / `DELETE` / `SET` / `BEGIN` | master |
| `BEGIN` 后 `SELECT`（sticky） | 仍 master，不再开 slave |
| `SingleExecutor` 即使有 slave 池 | 只碰 master |

## 3. 非目标

真主从容器、改默认 `mode: rwsplit`、COM_FIELD_LIST 接线。
