# 修复：保存在线曲目优先用流缓存提升（事件驱动）

> 状态：✅ 已完成（代码已合入 wip/stream-playback）
> 现象：在线播放过（缓存已完整）的曲目，在搜索页保存/收藏后走的是普通下载，而不是提升
> 关联：[task-promotion-worker.md](task-promotion-worker.md)（持久任务与 worker）、[row-task-state.md](../catalog/row-task-state.md)（行状态）
> 设计取向：**尽量事件驱动**，保存动作不再直接决定“下载还是提升”

---

## 1. 现象与证据

- 曲目 `QuomodocunquizE - かめりあ`：有 `download_jobs` 的 `done` 任务，`promotion_tasks` 为 0；
- `stream_cache` 里没有该条目（下载完成后被清掉），说明保存时直接走了下载；
- 用户预期：既然刚在线播放过、缓存就在本地，保存时应该直接提升（移动），不该重新下载。

## 2. 根因

1. `internal/app/catalog/actions.go` 的保存动作在 `download == true` 时**直接调用** `EnqueueWithCover`；
2. 提升入口 `PromoteOnSave` 只在「歌单 hook」和「收藏变更监听」里调用，搜索页保存路径不经过它们；
3. 歌单 hook 的顺序是 `AutoDownloadHook` 先入队，`PromoteOnSave` 随后看到活动下载任务 → 主动跳过；
4. 默认 `auto_download_on_playlist_add = true`，所以保存必然先产生下载。

## 3. 目标

- 保存/收藏一个 remote 行时，只要有该 identity 的流缓存条目 → **只写提升任务**，不产生下载任务；
- 没有缓存条目 → 保持现有行为（设置允许则下载，否则 metadata-only 保存）；
- 缓存音质低于下载音质时，交给提升 worker 现有的“重新拉取”分支；
- 搜索行显示「提升」（见 row-task-state 文档）；
- 收藏与歌单加入走同一套判断（收藏也优先提升）。

## 4. 事件驱动设计

### 4.1 事件

新增一个进程内事件（不经过前端 Wails 事件，避免 UI 依赖）：

```go
// domain/events.go（或 library 的 listener 约定）
type RemoteTrackSaved struct {
    Track      *domain.TrackDTO
    PlaylistID string   // 空表示仅收藏
    Explicit   bool     // 用户显式保存（收藏/加入歌单），区别于自动同步
}
```

- 发射点：
  1. `catalog` 保存动作：完成注册、歌单成员、收藏之后发一次；
  2. `playlist` 的 `SetAutoDownload` hook：改为只发事件，不再直接入队；
  3. `library` 的收藏监听：收藏为 true 时发一次。
- 发射方**不做业务判断**：不查缓存、不查设置、不入队。

### 4.2 订阅方（唯一的决策点）

新增 `internal/app/promotion`（或 download 服务内的 coordinator），启动时注册：

```go
func (c *Coordinator) OnRemoteTrackSaved(ctx context.Context, ev domain.RemoteTrackSaved) {
    // 1. 已有缓存条目？ -> 只写持久提升任务，唤醒 worker，emit task:updated
    // 2. 没有缓存 + 设置允许自动下载 -> 走普通下载（保持旧行为）
    // 3. 没有缓存 + 不允许 -> 什么也不做（metadata-only 保存）
}
```

- 查缓存用 `CachedFileInfo`（不创建条目、不联网）；
- 写任务用 `MarkPendingPromote`（幂等）；
- 下载入队用现有 `Enqueue`；
- 所有分支 emit `task:updated`，行状态由 resolver 查询决定。

### 4.3 缓存侧事件

- `stream.Cache` 的 `OnSessionClosed`（已有设计）→ coordinator 唤醒维护 worker；
- 缓存条目完成时也发一次内部事件（`EntryComplete`），让行状态/搜索页及时刷新；
- 事件回调只做“标记需要扫描”，实际工作在 worker 里串行执行，避免在写路径上做 I/O 重活。

### 4.4 幂等与顺序

- 同一曲目短时间内多次保存/收藏：`MarkPendingPromote` 幂等；已有活动下载任务时跳过；
- 事件只在启动时注册一次（fx 生命周期），不重复订阅；
- 事件处理失败只记日志，不回滚已完成的保存操作。

## 5. 代码改动

| 位置 | 改动 |
|---|---|
| `internal/app/catalog/actions.go` | 删除 `EnqueueWithCover` 调用；保存完成后发 `RemoteTrackSaved` |
| `internal/app/catalog/service.go` | 注入事件发射器（窄接口），不直接依赖 download |
| `internal/app/playlist` | `SetAutoDownload` hook 改为发事件（保留签名兼容或替换） |
| `internal/app/module.go` | 注册 coordinator；把收藏监听改为发事件 |
| `internal/app/download` | coordinator 落地：查缓存 → 提升 or 下载 |
| `internal/app/stream` | 新增/复用 `OnSessionClosed`、条目完成通知 |

## 6. 测试

**Go**

- 保存时已有缓存 → 不发下载任务，`pending_promote=1`，emit `task:updated`；
- 保存时无缓存 + 自动下载开 → 入队下载（旧行为）；
- 保存时无缓存 + 自动下载关 → 不入队、状态 `saved`；
- 收藏路径同样优先提升；
- 多次保存幂等：只有一个提升任务；
- 已有活动下载任务时不再写提升任务；
- coordinator 处理失败只记日志，不影响保存结果；
- 事件订阅只注册一次（可用计数断言）。

**人工**

1. 在线播放一首 → 搜索页保存 → 不产生下载任务，显示「提升」，结束后入库；
2. 未播放过的曲目保存 → 按设置下载；
3. 在线播放后收藏 → 同样走提升；
4. 快速连续保存两次 → 只出现一条任务。

## 7. 验收

- 自动：§6 全绿，`go build ./...` 通过；
- 人工：§6 四条成立；
- 保存在线曲目时，`download_jobs` 不再出现无谓的下载任务。

## 8. 不做

- 不引入持久化事件表/队列（进程内事件 + 持久任务即可）；
- 不改前端保存按钮的交互；
- 不处理“用户显式要求重新下载”的场景（“重新下载”菜单仍然直接入队）。

## 9. 风险

- 事件发射与订阅必须都在启动阶段完成，错过事件会导致保存后无任务；
- 收藏/歌单/搜索保存三条发射点要覆盖全，否则某些入口会退回旧行为；
- coordinator 与下载 worker 共用缓存查询，注意不要在事件处理里阻塞太久（只做标记 + 唤醒）。
