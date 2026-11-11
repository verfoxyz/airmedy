# 任务：提升任务持久化 + 待清理缓存 worker（覆盖提升与普通下载两条路径）

> 状态：待开工
> 前置：M5 已实现（`promote.go`、`stream_cache.pending_promote`）
> 联动：`promotion_tasks` 历史表与「下载/任务」页见 [downloads-tasks.md](../library/downloads-tasks.md)（本任务负责写这张表，不另建历史机制）
> 现象：播放中的曲目被提升/下载完成后，原缓存文件没有被删除

---

## 1. 根因

缓存清理入口只有一个 `DiscardEntry`，遇到活动 session 就跳过，且**没有任何重试**：

1. **提升路径**：`PromoteOnSave` → `promoteSaved` 立即补全、**移动**文件、再 `DiscardEntry`；播放中移动/删除都会被 session 挡住；
2. **普通下载路径**：下载完成后的 finalize 也调用一次 `DiscardEntry`（`download/service.go` 的 “drop the temporary stream copy” 处）；此时用户往往还在播放缓存，于是同样跳过；
3. 两条路径的跳过都只是 `return nil`，没有留下“稍后删除”的状态；淘汰只在超容量时发生，所以缓存文件和行一直留着，`pending_promote` 也一直是 1。

## 2. 已确认的设计决定

- 提升使用**移动**，因此必须等该曲目**没有活动 session**（播放结束/切歌）后再执行；
- 用户点提升的**瞬间**就在数据库落持久任务，播放中关闭应用也不丢；
- 移动成功后再删除缓存；删除失败不能丢任务，要能下次继续；
- **普通下载完成后同样适用**：文件已经入库，缓存只是副本，延后删除，不重新下载。

---

## 3. 数据模型

迁移编号（注意：`000094` 已应用、只含 `promoted`，**不要修改它**；新增列用新迁移）：

- `000094_stream_cache_promoted.up.sql`：`promoted INTEGER NOT NULL DEFAULT 0`（已应用，保持原样）
- `000096_stream_cache_discard_pending.up.sql`：`discard_pending INTEGER NOT NULL DEFAULT 0`

> 背景见 [fix-discard-pending-migration.md](fix-discard-pending-migration.md)。

两个独立状态：

| 列 | 含义 | 设置者 |
|---|---|---|
| `promoted` | 音频已**移动**进音乐库（现有实现） | 提升 worker 移动成功后 |
| `discard_pending` | 缓存不再需要，空闲后删除 | 提升移动成功，或普通下载完成时 `DiscardEntry` 被延后 |

状态机：

| 状态 | worker 动作 |
|---|---|
| `discard_pending=1` | **优先处理**：无活动 session → 删音频 + 行（文件不存在也成功）；有 session → 跳过 |
| `pending_promote=1, promoted=0` | 无活动 session → 补全 → 音质规则 → 移动 → `promoted=1` 且 `discard_pending=1` → 尝试删除 |
| `pending_promote=1, promoted=1` | 已移动，只剩清理 → 同第一行 |
| 两者都为 0 | 忽略 |

`promoted=1` 或 `discard_pending=1` 的条目**不得**再走 `EnsureComplete`（文件已移走/已入库，否则会重新下载）。

`domain.StreamCacheRepository` 新增：

- `ListPendingPromote(ctx) ([]*StreamCacheEntry, error)`（按 `pending_promote=1`）
- `ListPendingDiscard(ctx) ([]*StreamCacheEntry, error)`（按 `discard_pending=1`）
- `SetDiscardPending(ctx, cacheKey string, pending bool) error`

## 4. 代码改动

### 4.1 `internal/app/stream`（缓存侧）

```go
// PendingPromotions returns every entry with pending_promote=1.
func (c *Cache) PendingPromotions(ctx context.Context) ([]*domain.StreamCacheEntry, error)

// PendingDiscards returns every entry whose cache must be deleted when idle.
func (c *Cache) PendingDiscards(ctx context.Context) ([]*domain.StreamCacheEntry, error)

// HasActiveSession reports whether a playback/download session still holds
// the entry; the worker must not move or delete the file then.
func (c *Cache) HasActiveSession(provider, externalID string) bool

// OnSessionClosed registers one callback the cache calls after a session is
// removed (Close / stop / fail), so the worker can run immediately.
func (c *Cache) OnSessionClosed(fn func(provider, externalID string))
```

- `DiscardEntry`：活动 session 存在时返回 `ErrDiscardDeferred`（新错误）；`promoted=1` 或 `discard_pending=1` 时文件不存在视为成功；
- `Open` 命中一个 `discard_pending=1` 的条目且文件仍在：清掉标记继续用缓存（用户又播了一次）；文件已不在则按未命中处理；
- 删除成功后删行并解除封面 pin（现有逻辑）。

### 4.2 `internal/app/download`（worker）

`CachedAudio` 端口新增：

```go
PendingPromotions(ctx) ([]*domain.StreamCacheEntry, error)
PendingDiscards(ctx) ([]*domain.StreamCacheEntry, error)
HasActiveSession(provider, externalID string) bool
SetDiscardPending(ctx context.Context, track *domain.TrackDTO, pending bool) error
```

新增 `RunCacheMaintenanceSweep(ctx)`（幂等、单飞）：

1. **先处理 `PendingDiscards`**：无活动 session → `DiscardEntry`；失败保留，下轮重试；
2. **再处理 `PendingPromotions`**：
   - 有活动 session → 跳过；
   - 无 session → `EnsureComplete` → 等完成 → `CachedFileInfo`；
   - 质量规则（复用现有逻辑）：
     - 缓存音质 ≥ 下载音质 → `PromoteCachedFile` → `SetPromoted(true)` + `SetDiscardPending(true)` → 尝试 `DiscardEntry`（失败留给下一轮）；
     - 下载音质更高 → `Enqueue(下载音质)`；下载完成路径会设置 `discard_pending`；
     - 缓存音质未知 → 视为最低；
3. 任一步失败：保留状态，记日志，下一轮重试（不阻塞其它条目）。

触发时机：

- `Service.Start` 里启动 worker：启动后立即扫一次（覆盖“播放中关闭应用”），之后每 30 秒一次；
- `Cache.OnSessionClosed` → 唤醒一次扫描（fx 装配把回调设为 `service.WakeCacheMaintenance`，download 已依赖 `CachedAudio` 端口，stream 不依赖 download）。

### 4.3 普通下载完成后的清理

`download/service.go` 中 finalize 之后调用 `DiscardEntry` 的位置改成：

```go
if err := s.cached.DiscardEntry(ctx, track); err != nil {
    if errors.Is(err, stream.ErrDiscardDeferred) {
        // The library owns the audio; delete the stream copy once playback ends.
        _ = s.cached.SetDiscardPending(ctx, track, true)
    } else {
        s.logger.Debug("download: cannot discard the stream cache", ...)
    }
}
```

`SetDiscardPending` 在任何 `DiscardEntry` 失败（占用）时都应兜底设置，保证下一轮能收尾。

### 4.4 历史记录（与「下载/任务」页联动）

提升的每次状态变化都写入 `promotion_tasks`（表结构、迁移 `000095`、清空/保留策略由 [downloads-tasks.md](../library/downloads-tasks.md) 定义）：

| 时机 | 写入 |
|---|---|
| 用户点提升、落持久任务 | 插入一行 `state='queued'`（track/provider/external_id/quality 快照） |
| worker 开始执行该条目 | `state='running'`, `started_at=now` |
| 移动成功并删除缓存 | `state='done'`, `finished_at=now` |
| 任一步失败 | `state='failed'`, `error=...`, `finished_at=now` |
| 走“下载音质更高”分支 | 视为 `done`（请求已交给下载队列；下载结果由下载任务自己记录） |

约定：

- 一个 `stream_cache` 条目同一时刻只允许一行 `queued/running` 的 `promotion_tasks`；重复点提升复用该行；
- 每次状态写入后 emit `task:updated`（事件统一定义在 downloads-tasks 文档）；
- 本任务**不新增**历史表、不清空、不裁剪，全部交给 downloads-tasks 的实现。

### 4.5 `PromoteOnSave`

- 仍然立即 `MarkPendingPromote`（持久任务），但**不再**在调用线程里 `EnsureComplete`/移动；
- 只 `WakeCacheMaintenance()` 后返回；
- 有活动下载任务时保持现状：不重复提升。

### 4.6 `internal/app/library` 删除路径

`RemoveTrackFromLibrary` 删除 remote 行时，若该条目有活动 session，`DiscardEntry` 返回 `ErrDiscardDeferred` → 设置 `discard_pending`；删除行继续，残留文件由 worker 收掉。

---

## 5. 测试

**Go**

- 提升状态机：`pending` 无 session → 移动 → `promoted=1, discard_pending=1` → 删除成功；
- 播放中：sweep 跳过，缓存文件与行保持不变；
- session 关闭回调触发 sweep，随后移动 + 删除；
- 移动成功、删除失败（模拟 `os.Remove` 失败）→ 行保留且 `promoted=1, discard_pending=1`；下一轮只删不重建（**断言不会重新下载**）；
- **下载完成而播放仍在**：`DiscardEntry` 返回 deferred → 行被标记 `discard_pending`（不设 `promoted`）；session 关闭后 sweep 删除；不重复下载；
- 启动场景：构造 `pending_promote` / `promoted` / `discard_pending` 行，`Start` 后自动处理；
- 重新播放 `discard_pending` 且文件仍在的条目：清标记、复用缓存；
- 质量更高：`Enqueue` 被调用，`pending_promote` 被清掉；
- 迁移：`000094` 保持原样，`000096` up/down；
- 历史：queued→running→done/failed 各有对应 `promotion_tasks` 行；重复点提升不新增 `queued` 行；每次写入后 emit `task:updated`。

**人工**

1. 播放云端曲目 → 加入歌单（触发自动下载）→ 播放结束/切歌后：入库文件保留、`stream-cache` 下对应文件消失；
2. 播放中关闭应用 → 重开：任务自动完成；
3. 提升后文件被其他程序占用（模拟）→ 下一轮自动删除，不重新下载；
4. 下载音质更高时：走普通下载，完成后缓存消失且只保留一份文件。

## 6. 验收

- 自动：§5 Go 测试全绿，`go build ./...` 通过；
- 人工：§5 的四条都成立；
- 不再出现“提升/下载完成后 `stream-cache` 仍残留且不会被清理”的情况。

## 7. 不做

- 不改成“复制”方案（已确认用移动）；
- 不做用户可见的“提升中/取消提升”UI；
- 不做多会话并发提升（单飞即可）。

## 8. 风险

- session 关闭回调需在会话真正移除之后调用（`stopSession` 完成后），否则 worker 会再次跳过；
- `discard_pending=1` 的行若用户再次播放且文件还在：清标记（`promoted` 保持）复用；若文件已移走：重新拉流并清标记，避免误删新一轮缓存；
- 迁移后旧行 `discard_pending=0`，行为与现在一致。
