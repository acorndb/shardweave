# 运维面设计（ops-plane-design，M4a）

> 状态：设计稿（2026-11-01）。**开发未启动**——待存量问题人工 review 完成后按本文实施。
> 范围：管控 HTTP 服务、连接池与后端健康、优雅升级。
> 前置：[../architecture/README.md](../architecture/README.md) §8（里程碑 M4）。

## 0. 结论

M4 的第一个切片（**M4a 可运维性**）：**看得见**（metrics/healthz）、**探得到**
（池状态机）、**可安全重启**（drain）。三点构成一条依赖链，合计约一周：

```
① admin 骨架 + healthz + /metrics + reload   ← 其他两点的载体（2–3 天）
② 池健康强化（后台探测 + 状态机 + 冷却）      ← metrics 出口就绪（2–3 天）
③ drain 接线 + readyz 联动 + 升级文档         ← 依赖 ① 的 readyz（1–2 天）
```

**不在本批**（M4b/M4c 另行立项）：查询总超时与 `KILL QUERY`（ctx 接线）、
caching_sha2_password / TLS、每连接能力协商、plan cache。

## 1. 管控 HTTP 服务（pkg/admin）

### 1.1 现状与资产

- 现状：仅 `SIGUSR1` 热更 users/log；无 metrics / healthz / admin 面。
- **现成资产**：`config.Runtime.Apply` 的热更门禁已实现（listen / mode / 拓扑 /
  server / pool 变更拒绝并保留旧快照）——admin 只是给这套机制加结构化入口，
  不需要新的配置生效语义。

### 1.2 开源对照

| 项目 | 形态 |
|---|---|
| dbpack | Prometheus metrics 独立端口（18888） |
| ProxySQL | MySQL 协议 admin（6032）+ stats 表 + 三层配置（memory/runtime/disk） |
| Vitess | 全 HTTP（/healthz /metrics /debug/status /api） |
| Gaea | 无本地 admin（cc + etcd 下发） |

### 1.3 端点契约

| 方法与路径 | 语义 | 备注 |
|---|---|---|
| `GET /healthz` | 进程存活 | 永远 200（除非进程死了） |
| `GET /readyz` | 可服务 | drain 中翻 503，联动 §3 |
| `GET /metrics` | Prometheus 文本 | §2 的探测/池指标出口 |
| `GET /admin/config` | 运行时配置快照 | 密码脱敏 |
| `POST /admin/reload` | 触发配置重载 | 复用 `Runtime.Apply`；返回结构化结果（逐段 accepted / rejected + 原因） |
| `GET /admin/pools` | 池状态 | 每 slice × master/slave：active/idle/max + up/down |
| `POST /admin/shutdown` | 优雅退出 | 触发 §3 的 drain 流程 |
| `GET /admin/version` | build info | commit / 版本 / go 版本 |

### 1.4 配置与装配

```yaml
admin:
  listen: "127.0.0.1:8080"   # 默认仅本地；生产绑定内网
  token: ""                  # 可选 bearer token；空 = 不鉴权（默认仅限本地回环）
```

- 新建 `pkg/admin`（architecture §3 预留的后置模块，此刻建它）；
  依赖 `config.Runtime` 与 `server.Server` 的只读句柄，装配点在 `cmd/shardweave`。
- `pkg/admin` 不 import `backend`——池状态经 `execute.Executor` 暴露的只读接口获取
  （保持依赖规则 3）。
- `/admin/reload` 与 SIGUSR1 共用同一入口，行为一致（双通道）。

### 1.5 TDD 切分

1. admin 服务生命周期（起停、token 鉴权）；
2. healthz/readyz/version 端点；
3. reload 端点：接受 users/log、拒绝拓扑变更并返回结构化原因（表驱动，
   复用 `runtime_test.go` 的门禁用例）；
4. config/pools 快照脱敏与字段正确性。

## 2. 连接池与后端健康

### 2.1 现状与差距

| 已有 | 缺 |
|---|---|
| acquire 时对超过 `HealthCheckInterval` 的 idle 连接 ping | **后台主动探测**：长期闲置的坏连接一直占 idle 位 |
| idle reaper / MaxLifetime | **坏从库标记与冷却**：从库挂掉后每次查询撞一次失败 RTT 再回落 master |
| 从库 acquire 失败回落 master | **master read_only 检测**：主库被切走后写持续失败，无降级信号 |
| `HealthCheckInterval` 配置 | 探测结果无处可看（metrics 依赖 §1） |

对照事实：Gaea 为被动式（`connection_pool.go:32` `pingPeriod=4s`，取用时 ping，
与当前实现同型）；ProxySQL 的 shun（坏主机自动屏蔽 + 冷却恢复）是对齐目标。

### 2.2 设计

**Pool 层——后台探测循环**

- 每个 Pool 一个 goroutine：每 `HealthCheckInterval` 对全部 idle 连接 ping；
  失败连接剔除并关闭。补齐「取用时被动检查」的盲区。
- 复用现有 `opTimeout`；探测循环随 `Pool.Close` 退出（复用 stop channel）。

**Slice 层——池状态机**

```
up ──探测失败──► down（记录 lastFailure，开始 cooldown）
down ──冷却期满且探测成功──► up
down 期间：从库不参与 Acquire（直接走 master 兜底，消掉失败 RTT）；
         master down：slice 整体不可用，Acquire 返回明确错误
```

- 状态挂在 `Slice`（每个 master/slave Pool 一份），探测命令 `COM_PING`；
  可配置探测 SQL（默认空 = 仅 ping）。
- 新配置：`pool.unhealthy_cooldown`（默认 5s）。
- **行为兼容**：现有「从库 acquire 失败回落 master」保留为最后防线；
  状态机只是让它不再被反复触发。

**后续可选（本批不做，记录方向）**

- master 周期 `SELECT @@read_only`，只读时标记 slice 降级；
- 从库延迟阈值（`Seconds_Behind_Master`）参与路由权重。

### 2.3 metrics 清单（经 §1 的 /metrics 暴露）

- `shardweave_pool_connections{slice,role,state=active|idle}` gauge
- `shardweave_pool_up{slice,role}` gauge（状态机）
- `shardweave_pool_probe_total{slice,role,result=ok|fail}` counter
- `shardweave_pool_acquire_total{slice,role,result=ok|fail|exhausted}` counter
- `shardweave_backend_conn_lifecycle_total{reason=dirty|undrained|error|expired|idle}`
  （连接淘汰原因，验证 A12 生效情况）

### 2.4 TDD 切分

1. 后台探测循环：坏 idle 连接被剔除（stubConn 计数）；
2. 状态机：up→down→cooldown→up 迁移（表驱动 + 假时钟）；
3. Acquire 行为：down 从库被跳过、master 兜底不变；
4. metrics 序列化（Prometheus 文本格式断言）。

## 3. 优雅升级（单实例）

### 3.1 判断

真正的控制面（多实例 + etcd 配置中心 + 统一升级编排，Gaea cc / Vitess 形态）
**远期单独立项**。v1 目标收敛为：**单实例可被安全升级** = 滚动重启不丢请求。

### 3.2 drain 时序（termination_drain 接线——配置已解析、从未生效的欠账）

```
收到 SIGTERM / POST /admin/shutdown
  ① 停止 Accept（listener 关闭；新连接立刻被拒）
  ② /readyz 翻 503（LB / k8s 自动摘流量）
  ③ 等待活跃会话结束，上限 termination_drain（默认 5s）
  ④ 超时后对残余会话发 ERR 并关闭（MySQL 语义：断连）
  ⑤ 关闭全部池（idle 关闭，active 由会话结束路径处理）
```

- `Server.Close` 扩展为上述序列；`termination_drain` 从 config 接到 server。
- 活跃会话集合已有计数（`activeConns`），drain 判据直接复用。

### 3.3 升级操作手册（随本批交付文档）

- **systemd**：`ExecReload`/重启序列 + drain 时长建议；
- **k8s**：Deployment 滚动更新 + readinessProbe(/readyz) + preStop(sleep ≥ drain) +
  terminationGracePeriodSeconds；
- 回滚 = 同一流程（配置不兼容时 `Runtime.Apply` 会拒绝热更，只能重启回退——
  这是设计内的保护）。

### 3.4 TDD 切分

1. drain：活跃会话排空后干净退出；
2. drain 超时：残余会话被关闭且前端收到断连；
3. readyz 在 drain 各阶段的翻转；
4. 集成冒烟：重启过程中差分校准不出现半响应（可后置到 e2e）。

## 4. 顺序与工作量汇总

| 步 | 内容 | 天数 |
|---|---|---|
| ① | `pkg/admin` 骨架 + healthz/readyz/metrics + reload/config/pools/version | 2–3 |
| ② | Pool 后台探测 + Slice 状态机 + cooldown + metrics | 2–3 |
| ③ | termination_drain 接线 + readyz 联动 + 升级手册 | 1–2 |

每步按 AGENTS 流程：红测 → 实现 → 聚焦测试 + lint → code-review 报告。
协议/语义行为变更（drain 断连语义）进差分语料。

## 5. 开放问题（review 时拍板）

1. `/admin/shutdown` 是否默认开启（还是仅允许 Unix 信号触发）？
2. metrics 命名空间与 label 集合是否如 §2.3（上线后难改）？
3. master down 时 slice 不可用的错误码（现有 pool exhausted 映射是否够用）？
4. 探测 SQL 可配置（`pool.probe_query`）是否本批就需要，还是固定 COM_PING？
