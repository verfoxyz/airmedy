# 修复：缓存/远程曲目无法从库里删除

> 状态：待开工
> 现象：右键「删除」对流曲目不可用（按钮置灰），删不掉缓存条目
> 范围：删除对话框 + 库服务；不改缓存淘汰/播放逻辑

---

## 1. 根因

- 右键的删除对话框只有两个动作：「从播放列表移除」和「删除文件」；
- 「删除文件」在 `hasLocalFile == false` 时禁用——流曲目的 `path` 是 `remote://...`，所以永远不满足；
- 后端 `LibraryService.DeleteTrackFile` 同样拒绝 remote 行（`track.IsRemote()` → 返回 "has no local file"）；
- 结果：缓存/流曲目在库里没有任何删除入口。

缓存本身已有现成接口：`stream.Cache.DiscardEntry(ctx, track)` 会删除音频文件 + `stream_cache` 行（并解除封面 pin）。

## 2. 目标

- 远程/缓存曲目可以「从音乐库移除」：删除曲目行、删除它的流缓存、清理索引与孤儿实体；
- 本地文件曲目行为不变（删除文件 + 行）；
- 在播放列表里打开时，仍保留「仅从列表移除」。

## 3. 修法

### 3.1 后端

新增 `LibraryService.RemoveTrackFromLibrary(trackID string) error`：

1. 读曲目；不存在 → 错误；
2. 若**不是** remote 行 → 返回错误（本地行请走 `DeleteTrackFile`，避免误删文件）；
3. 删除流缓存：调用注入的窄接口 `DiscardEntry(ctx, track)`（`internal/app/stream` 已实现），失败只记 warning，不阻塞删除；
4. 事务内：`trackRepo.Delete(id)` + `downloadJobs.DeleteByTrack(id)`；
5. `searchService.DeleteTracksFromIndex`；
6. `cleanupOrphanedEntities` + `CleanupOrphanedArtworks`；
7. 若该曲目正在播放：先 `playerService.Stop()`（或跳过当前曲目），避免删除后播放器指向已删行；
8. `notifyTracksDeleted` + emit `library:track-deleted` / `library:updated`。

依赖注入：`library.Service` 增加 `cachedAudio` 窄接口（`DiscardEntry(context.Context, *domain.TrackDTO) error`），在 `module.go` 里绑定 `stream.Cache`。

### 3.2 前端（`DeleteTrackDialog.vue`）

- 当 `!hasLocalFile` 时，把禁用的「删除文件」替换为可用的「从音乐库移除」：
  - 标题：`delete_track.remove_from_library`
  - 说明：`delete_track.remove_from_library_hint`（"移除库记录并清掉本地缓存，不影响其他曲目"）
  - 危险色沿用现有 `destructive` 样式；
  - 点击调用 `LibraryService.RemoveTrackFromLibrary(track.id)`，错误内联展示；
- `hasLocalFile` 分支保持现状（删除文件）。

新增 i18n key（12 个语言同改）：

- `delete_track.remove_from_library`
- `delete_track.remove_from_library_hint`

## 4. 测试

**Go**

- remote 行：`RemoveTrackFromLibrary` 删除行、删除缓存音频与 `stream_cache` 行、发出事件；
- 本地行：调用返回错误，文件与行保持不变；
- 缓存不存在时：仍然删行成功（幂等）；
- 正在播放的曲目：播放器被停止/切走，不残留指向已删行的状态。

**前端**

- `DeleteTrackDialog.spec`：remote 行显示「从音乐库移除」且点击调用新绑定；本地行仍显示「删除文件」；
- 错误信息内联显示。

**人工**

1. 播放一首云端曲目（生成缓存）→ 右键删除 → 选「从音乐库移除」→ 行消失、`stream-cache/` 下对应文件消失；
2. 本地文件曲目 → 右键删除 → 仍走「删除文件」；
3. 歌单里打开 remote 行 → 同时有「仅从列表移除」与「从音乐库移除」。

## 5. 风险

- 删除正在播放的流曲目必须先停播，否则解码器还持有代理 URL；
- 提升（pending_promote）中的条目：删除时应一并清掉 pending 标记（`DiscardEntry` 已经删行，无需额外处理）；
- 与「重新下载」互不影响。
