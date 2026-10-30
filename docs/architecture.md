# prism 架构设计（草案 v0.1）

> 状态：讨论定稿中。本文档是项目的 north star，实现偏离此处记录的设计时先改文档。
> 模块路径：`github.com/acorndb/prism`（GitHub 仓库名可随后改为 `prism`）。

## 1. 设计原则

1. **正确性优先于功能**：每类 SQL 构造只有三种合法状态——完全支持（跨分片语义正确）/ 可证明单分片（直通，行为与裸 MySQL 一致）/ 明确报错（带文档链接）。永不静默返回错误结果。
2. **协议层是第一方代码**：MySQL 线协议的保真度是本项目差异化所在，可以参考 go-mysql / vitess 的实现，但不做黑盒依赖。
3. **依赖单向、逐层可独立测试**：下层不知道上层存在。协议层可以在没有路由/分片的情况下单独跑差分测试（透传模式），这是"测试流水线跟着产品长"的结构保证。
4. **依赖面极小**：运行时直接依赖目标 < 10 个。gaea 依赖全家桶集体冻结是前车之鉴。

## 2. 各层级选型与参考组件

| 层级 | 策略 | 直接依赖 / 参考组件 | 参考什么 |
|---|---|---|---|
| 线协议（packet/auth/前后端） | **自建**（第一方） | 参考 `go-mysql-org/go-mysql` | 包结构、握手流程、charset 处理 |
| | | 参考 `vitessio/vitess` 的 `go/mysql` | auth 插件全度、server status、测试纪律 |
| | | MySQL server 源码 | 协议行为真值 |
| SQL 解析 | **引入** | `github.com/pingcap/tidb/parser`（module 引入，锁版本跟进） | 直接使用；包一层做 panic→error 边界 |
| SQL 改写 | 自建 | 参考 vitess sqlparser 的 rewrite 机制 | AST 遍历/重建模式 |
| 路由/分片规则 | 自建 | 参考 gaea `proxy/router`（承自 kingshard 方式） | hash/range 规则模型、注解路由 |
| 归并引擎 | 自建 | 参考 vitess engine | 流式归并思路；反面教材：kingshard 全缓冲 |
| 后端连接池 | **自建**（不用 database/sql） | 参考 gaea `backend`、vitess 池 + 粘性管理 | 池语义；粘性矩阵 vitess 最完整 |
| 会话状态 | 自建 | MySQL 5.7+ session tracking | 用 server 上报而非 proxy 猜测 |
| 配置 | 自建 | gaea 的 namespace 模型 | 多租户隔离；etcd 用现代模块 `go.etcd.io/etcd/client/v3`（v2 阶段引入） |
| 可观测 | 引入 | `log/slog`、`prometheus/client_golang`、OpenTelemetry（可选） | — |
| 测试基建 | **自建（产品级）** | `testcontainers-go`、Go 原生 fuzz、mtr 语料转写 | 差分 + 变形 + fuzz，详见 testing-pipeline 文档（待补） |

明确不引入：gin/grpc 全家桶（管理面用 `net/http`）、zap（用 slog）、SQL 防火墙/加解密/影子库类功能。

## 3. 模块划分

```
prism/
├── cmd/prism/        # 入口：装配、信号处理、优雅退出（单二进制）
├── mysql/                     # 【协议层】第一方代码，参考 go-mysql
│   ├── packet/                #   包编解码：header、长度编码、压缩协议
│   ├── auth/                  #   认证：握手、native/caching_sha2（含 RSA/TLS 全 auth）
│   ├── server/                #   前端：listener、会话建立、能力协商
│   ├── client/                #   后端：raw conn（非 database/sql）
│   └── charset/               #   字符集/排序规则
├── sql/                       # 【SQL 处理层】
│   ├── parser/                #   tidb/parser 包装：panic→error，统一三态报错出口
│   ├── rewrite/               #   改写：LIMIT/OFFSET、INSERT VALUES、聚合拆分
│   └── fingerprint/           #   慢 SQL 指纹
├── router/                    # 【路由层】
│   ├── rule/                  #   分片规则：hash/range、全局表
│   └── route/                 #   路由决策：AST → 目标分片集合
├── executor/                  # 【执行层】
│   ├── plan/                  #   三态契约判定：直通 / 改写 / 明确报错
│   ├── dispatch/              #   并行下发，errors.Join 聚合多分片错误
│   └── merge/                 #   流式 k 路归并：排序堆、聚合、GROUP BY 二次归并
├── session/                   # 【会话层】
│   ├── state/                 #   会话状态机：变量跟踪（session tracking）
│   └── stickiness/            #   粘性矩阵：哪些状态阻塞后端连接复用
├── pool/                      # 【资源层】后端连接池（自研）
│   ├── pool.go                #   per-user/per-backend 池、借还、COM_RESET_CONNECTION
│   └── health.go              #   健康检查、只读检测（read_only/super_read_only）
├── config/                    # 【配置层】
│   ├── schema.go              #   严格类型定义（protobuf 起草）
│   ├── loader.go              #   YAML + watch（v1）；etcd v3（v2）
│   └── validate.go            #   校验 + dry-run
├── admin/                     # 【管理面】REST(net/http) + OpenAPI
│   ├── explain.go             #   explain route for <SQL>
│   └── record.go              #   会话录制（packet 级）与回放
├── obs/                       # 【可观测】metrics（prometheus）、logging（slog 基建）
└── test/                      # 【测试基建】
    ├── differential/          #   差分 harness：裸 MySQL vs proxy 逐包对比
    ├── metamorphic/           #   变形 harness：分片 vs 单库结果一致
    ├── corpus/                #   SQL 语料库（版本化，bug→corpus）
    └── e2e/                   #   testcontainers 集成用例
```

## 4. 数据通路

```
client ──▶ mysql/server + session        （协议层前端）
                │
                ▼
         executor/plan                    三态判定
                │
      ┌─────────┴──────────────┐
      ▼                        ▼
  fast path（直通）          slow path
  最小解析直接转发          sql/parser → router → sql/rewrite
      │                        │
      ▼                        ▼
  pool/ 后端连接 ◀──── executor/dispatch（并行多分片）
      │                        │
      └──▶ executor/merge（流式 k 路归并）◀──┘
                 │
client ◀── mysql/server（响应编码）
```

## 5. 依赖规则（强制）

- `mysql/` 不允许 import `router/`、`executor/`、`config/` 之外的任何上层——协议层必须可在透传模式下独立运行与差分测试。
- 依赖方向：`mysql` ← `session`/`pool` ← `sql`/`router` ← `executor`；`config` 被所有层读；`obs`/`admin` 横切。
- `admin/` 只读内部状态、下发控制指令，不参与数据面路径。
- 每个顶层包对外只暴露小接口（Go 惯例：接口定义在消费方）。

## 6. 模块与测试流水线的映射

| 模块 | L0 单元 | L1 fuzz/property | L2 集成 | L3 差分 | L4 变形 |
|---|---|---|---|---|---|
| mysql/* | codec 往返 | packet/auth fuzz | auth、charset、prepare | **透传模式即差分** | — |
| sql/* | 改写变换 | 改写幂等、parser 不 panic | — | 改写后语义对比 | 聚合拆分 |
| router/* | 规则→分片 | 路由确定性 | — | — | 不均匀分片 |
| executor/* | 归并堆 | — | 多分片下发 | — | **分片 vs 单库** |
| pool/* | 粘性状态机 | — | 故障注入（杀容器） | — | — |
| config/* | 校验/dry-run | — | 热加载不中断 | — | — |

## 7. v1 明确不做

在线扩缩容（做手动迁移 + 校验工具替代）、SQL 防火墙、数据加解密、影子库、自研 parser、gnet/evio 事件循环（goroutine-per-connection 足够）。

## 8. 里程碑

1. **M1 透传**：mysql/ + pool/ + session/ + 配置，差分 harness 跑通（协议保真度开始被验证）。
2. **M2 单分片**：parser + router + plan 三态判定 + explain route。
3. **M3 跨分片**：dispatch + merge，变形 harness 上线。
4. **M4 生产化**：粘性矩阵完整、故障注入通过、性能门禁、发布认证。
