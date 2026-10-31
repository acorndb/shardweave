# 三 Executor（single / rwsplit / sharding）

> 状态：**已被 [module-map.md](module-map.md) 取代**：改为单 Pipeline + 三个 Router，
> 不再使用三 Executor；本文保留为历史决策记录。
> 日期：2026-10-30
> 上位：[backend-server-single-rwsplit-design.md](backend-server-single-rwsplit-design.md)、[m1-passthrough-pipeline-design.md](m1-passthrough-pipeline-design.md)

## 0. 一句话

配置仍是 `backend_server.mode` 三态；数据面用 **同接口、三实现** 的 `pkg/executor`（对照 dbpack），差异在选源策略。共享 `forward`（Align + 透传 + sticky）；`proxy.Serve` 只做协议循环与会话字段预处理。

## 1. 接口与输入

```go
type Conn struct {
    Front      *packet.Conn
    State      *session.State
    Sticky     backend_server.Backend
    StickyPool *backend_server.Pool
}

type Request struct {
    Cmd     byte
    Payload []byte // 整包（含 cmd），同今日 data
    SQL     string // COM_QUERY 文本；其余可空
}

type Executor interface {
    Mode() string
    Handle(ctx context.Context, c *Conn, req Request) error
    ReleaseSticky(c *Conn, discard bool) error
    Close() error
}
```

| 输入 | 谁提供 | 说明 |
|---|---|---|
| `ctx` | Serve | 取消 |
| `*Conn` | 每条前端连接一份 | 可变：State / sticky |
| `Request` | Serve 从 COM 包构造 | 不含 mode / Group |

Ping / Quit 不进 Executor。

## 2. 三种策略

| 类型 | 选源 |
|---|---|
| `SingleExecutor` | 始终 master（`RoleMaster`）；仍尊重 sticky / `NeedsSticky` |
| `RWSplitExecutor` | `router.Pick` → `Group.Acquire`（slave 失败回落 master） |
| `ShardingExecutor` | `Handle` → 明确未实现；配置 `Validate` 仍拒绝 sharding |

## 3. 装配

`server.New`：建 `router.Group` 后 `executor.New(mode, group)` → `proxy.SessionConfig.Exec`。

## 4. 非目标

真 sharding Plan/Merge；dbpack QueryAll / Filter；本切片不改 `session.State` 命名。
