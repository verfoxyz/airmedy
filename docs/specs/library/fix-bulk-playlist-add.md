# Bug：多选/批量加入歌单不会触发下载或提升

> 状态：✅ 已完成（AddTracks 按去重后的 track id 逐个补发保存钩子；M3U 导入共用该入口）
> 现象：把 `misery.` 加进「英语-小众」歌单后，没有下载任务、没有提升任务，`stream_cache.pending_promote` 仍为 0
> 关联：`fix-save-prefers-promotion.md`（保存事件驱动）

---

## 1. 数据

- 曲目：`remote://wy/3375858474`，流缓存完整（flac，21,348,006 B）；
- 歌单成员：存在（`playlist_tracks` 有行）；
- `stream_cache.pending_promote = 0`、`promotion_tasks` 无记录、`download_jobs` 无记录。

即：**成员加进去了，但“保存”事件没有触发**。

## 2. 根因

歌单服务里只有**单曲**添加会触发保存钩子：

```go
func (s *PlaylistService) AddTrack(...) error {
    ... repo.AddTrack(...)
    s.maybeAutoDownload(ctx, playlistID, trackID)   // ✅ 触发下载/提升
}

func (s *PlaylistService) AddTracks(...) error {
    return s.repo.AddTracks(ctx, playlistID, trackIDs)  // ❌ 没有任何钩子
}
```

- 多选“添加到歌单”、拖拽批量加入等 UI 走的是 `AddTracks`；
- 因此不会进入 `OnRemoteTrackSaved` → 既不下载也不提升；
- 只有单曲加入、搜索页保存/收藏才会触发。

## 3. 修法

### 3.1 `AddTracks` 补发保存事件

```go
func (s *PlaylistService) AddTracks(ctx, playlistID string, trackIDs []string) error {
    if err := s.guardNotSmart(...); err != nil { return err }
    if err := s.repo.AddTracks(ctx, playlistID, trackIDs); err != nil { return err }
    for _, id := range trackIDs {
        s.maybeAutoDownload(ctx, playlistID, id)   // 只对 remote 行生效
    }
    return nil
}
```

- `maybeAutoDownload` 已是幂等安全（有缓存走提升、无缓存按设置下载、本地行直接跳过）；
- 去重：同一批里重复 id 只发一次（先用 set 去重）；
- 已经存在于歌单的条目：repo 层忽略重复，事件仍可能重复触发——由 `MarkPendingPromote`/活动任务检查兜底，不会重复建任务。

### 3.2 其它批量入口

- 导入歌单（M3U）如果走 `AddTracks`，同样受益；
- 移动端同步/远程添加若走另一条路径，需要确认是否也补发（列出风险）。

## 4. 测试

**Go**

- 批量加入 2 首 remote → 每首各触发一次 hook；
- 批量里含本地行 → 本地行不触发；
- 批量含重复 id → 只触发一次；
- 单曲 `AddTrack` 行为不变（回归）；
- hook 内部走 `OnRemoteTrackSaved`：有缓存 → 提升任务；无缓存 → 按设置决定是否下载。

**人工**

1. 多选两首在线曲目 → 添加到歌单 → 任务页出现提升/下载任务；
2. 有缓存的那首提升、无缓存的按设置下载；
3. 单曲加入、搜索页保存行为不变。

## 5. 验收

- 自动：§4 全绿；
- 人工：§5 三条成立。

## 6. 风险

- 批量加入大量曲目会一次性触发很多事件（每首一次），需要限流或串行；建议复用事件驱动的调度，不阻塞添加操作；
- 钩子失败不能回滚已完成的成员添加（与单曲一致）。
