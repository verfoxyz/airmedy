# 任务：提升任务持久化 + 提升 worker（解决缓存文件占用的残留）

> 状态：待开工
> 前置：M5 已实现（`promote.go`、`stream_cache.pending_promote`）
> 联动：`promotion_tasks` 历史表与「下载/任务」页见 [downloads-tasks.md](../library/downloads-tasks.md)（本任务负责写这张表，不另建历史机制）
> 现象：播放中的曲目被提升后，原缓存文件没有被删除

---

## 1. 根因

1. `PromoteOnSave` 触发后立即在后台跑 `promoteSaved`：标记 `pending_promote`、补全缓存、**移动**文件（`os.Rename`）、再 `DiscardEntry`；
2. `DiscardEntry` 发现该条目还有活动 session 时**直接返回 nil**，不删文件、不删行；
3. 没有任何重试：淘汰只在超容量时发生，所以缓存文件和行一直留着；
4. 反过来，如果先移动、后删除失败（文件被占用），也会留下残留；而 `pending_promote` 仍为真，下一条执行路径可能重新补全、再次移动（文件已经不在了）。

## 2. 已确认的设计决定

- 提升使用**移动**，因此必须等该曲目**没有活动 session**（播放结束/切歌）后再执行；
- 用户点提升的**瞬间**就在数据库落持久任务，播放中关闭应用也不丢；
- 移动成功后再删除缓存；删除失败不能丢任务，要能下次继续。

---

## 3. 数据模型

迁移 `000094`：

```sql
ALTER TABLE stream_cache ADD COLUMN promoted INTEGER NOT NULL DEFAULT 0;
```

状态机（`pending_promote` + `promoted`）：

| 状态 | 含义 | worker 动作 |
|---|---|---|
| `pending_promote=1, promoted=0` | 已请求提升，尚未移动 | 无活动 session → 补全 → 按音质规则处理 → 移动 → `promoted=1` → 尝试删除 |
| `pending_promote=1, promoted=1` | 已移动，缓存待清理 | 无活动 session → 只删除缓存文件 + 行（文件不存在也成功） |
| `pending_promote=0` | 无任务 | 忽略 |

`promoted=1` 的条目**不得**再走 `EnsureComplete`（文件已移走，否则会重新下载）。

`domain.StreamCacheRepository` 新增：

- `ListPendingPromote(ctx) ([]*StreamCacheEntry, error)`
- `SetPromoted(ctx, cacheKey string, promoted bool) error`

## 4. 代码改动

### 4.1 `internal/app/stream`（缓存侧）

新增只读查询与会话状态：

```go
// PendingPromotions returns every entry with pending_promote=1.
func (c *Cache) PendingPromotions(ctx context.Context) ([]*domain.StreamCacheEntry, error)

// HasActiveSession reports whether a playback/download session still holds
// the entry; promotion must not move or delete the file then.
func (c *Cache) HasActiveSession(provider, externalID string) bool

// OnSessionClosed registers one callback the cache calls after a session is
// removed (Close / stop / fail). Used to wake the promotion worker instead of
// waiting for the next tick.
func (c *Cache) OnSessionClosed(fn func(provider, externalID string))
```

- `DiscardEntry` 改为：活动 session 存在时返回 `ErrDiscardDeferred`（新错误），由调用方决定重试；`promoted=1` 时允许文件不存在（`os.IsNotExist` 视为成功）；
- 删除时先 `os.Remove`，失败保留行并返回错误；成功后删除行、解除封面 pin。

### 4.2 `internal/app/download`（提升 worker）

`CachedAudio` 端口新增：

```go
PendingPromotions(ctx) ([]*domain.StreamCacheEntry, error)
HasActiveSession(provider, externalID string) bool
```

新增 `RunPromotionSweep(ctx)`（幂等、单飞，`sync.Mutex`/`atomic.Bool` 防重入）：

1. `PendingPromotions` 遍历；
2. `promoted=1`：无活动 session → `DiscardEntry`，成功即结束；
3. `promoted=0`：有活动 session → 跳过（下一轮再来）；
4. 无 session：`EnsureComplete` → 等完成 → `CachedFileInfo`；
5. 质量规则（复用现有逻辑）：
   - 缓存音质 ≥ 下载音质 → `PromoteCachedFile` → `SetPromoted(true)` → `DiscardEntry`；
   - 下载音质更高 → `Enqueue(下载音质)`，成功后清掉 `pending_promote`（普通下载完成时会 `DiscardEntry`；失败则下轮重试队列）；
   - 缓存音质未知 → 视为最低；
6. 任一步失败：保留状态，记日志，下一轮重试（不阻塞其它条目）。

触发时机：

- `Service.Start` 里启动 worker：启动后立即扫一次（覆盖“播放中关闭应用”的情况），之后每 30 秒一次；
- `Cache.OnSessionClosed` → 触发一次扫描（放在 fx 装配里把回调设为 `service.WakePromotionSweep`，不引入循环依赖：download 已依赖 `CachedAudio` 端口，stream 不依赖 download）。

### 4.3 历史记录（与「下载/任务」页联动）

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

### 4.4 `PromoteOnSave`

- 仍然立即 `MarkPendingPromote`（持久任务），但**不再**在调用线程里 `EnsureComplete`/移动；
- 只 `WakePromotionSweep()` 后返回；
- 有活动下载任务时保持现状：不重复提升。

### 4.5 `internal/app/library` 删除路径

`RemoveTrackFromLibrary` 删除 remote 行时，若该条目有活动 session，`DiscardEntry` 会返回 `ErrDiscardDeferred`，删除行仍然继续；残留文件由缓存启动清理/下一轮 sweep 收掉（与本次任务共用 `promoted` 语义，不另做机制）。

---

## 5. 测试

**Go**

- 状态机：`pending` 无 session → 移动 → `promoted=1` → 删除成功；
- 播放中：sweep 跳过，缓存文件与行保持不变；
- session 关闭回调触发 sweep，随后移动 + 删除；
- 移动成功、删除失败（模拟 `os.Remove` 失败）→ 行保留且 `promoted=1`；下一轮只删不重建（**断言不会重新下载**）；
- 启动场景：构造 `pending`/`promoted` 行，`Start` 后自动处理；
- 质量更高：`Enqueue` 被调用，`pending_promote` 被清掉；
- `DiscardEntry` 对 `promoted=1` 且文件已不存在时返回成功；
- 迁移 `000094` up/down；
- 历史：queued→running→done/failed 各有对应 `promotion_tasks` 行；重复点提升不新增 `queued` 行；每次写入后 emit `task:updated`。

**人工**

1. 播放云端曲目 → 加入歌单 → 播放结束/切歌后：文件进入下载目录、`stream-cache` 下对应文件消失；
2. 播放中关闭应用 → 重开：任务自动完成；
3. 提升后文件被其他程序占用（模拟）→ 下一轮自动删除，不重新下载；
4. 下载音质更高时：走普通下载，完成后缓存消失且只保留一份文件。

## 6. 验收

- 自动：§5 Go 测试全绿，`go build ./...` 通过；
- 人工：§5 的四条都成立；
- 不再出现“提升后 `stream-cache` 仍残留且不会被清理”的情况。

## 7. 不做

- 不改成“复制”方案（已确认用移动）；
- 不做用户可见的“提升中/取消提升”UI；
- 不做多会话并发提升（单飞即可）。

## 8. 风险

- session 关闭回调需在会话真正移除之后调用（`stopSession` 完成后），否则 worker 会再次跳过；
- `promoted=1` 的行在删除前若用户再次播放同一曲目，缓存已无文件：`Open` 应重新拉流（`promoted` 状态不阻止重新缓存）——重新播放时把 `promoted` 清 0，避免误删新一轮缓存；
- 迁移后旧行 `promoted=0`，行为与现在一致。
