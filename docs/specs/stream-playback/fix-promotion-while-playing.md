# Bug：提升后缓存文件仍残留（提升在播放器仍加载该曲目时执行）

> 状态：✅ 已完成（代码已合入 wip/stream-playback）
> 现象：提升完成后 `stream-cache` 里仍留着该曲目的 `.audio`
> 案例：`Speed Complexxx (feat.Srezcat)`（`eae2ebb0…audio`）

---

## 1. 现象与证据

- 提升任务 `eb41b0f2…`：`state=done`，21:05:26 完成；
- 同一时刻 `merge`：`kept=04cb0e14…`（本地行）、`removed=d3d27255…`（remote 行）；
- 但 21:05:33 日志：

```
"failed to record playback attempt start" track_id=d3d27255… error="FOREIGN KEY constraint failed"
```

即**播放器仍在播放刚被删除的 remote 行**；

- 21:05:33 缓存文件被重新写入（mtime），随后 `stream_cache` 出现一条**新的**条目：
  `promoted=0, pending_promote=0, discard_pending=0, complete=1`；
- 旧的缓存行在提升时已被正常删除——残留的是**提升之后重新生成的新缓存**。

## 2. 根因（两层）

### 2.1 提升只看缓存 session，不看播放器

提升 worker 的判断依据是 `Cache` 里是否还有该 identity 的活动 session；但播放器可能已经加载了该 remote 行（`currentTrack`），即使 session 暂时不在（例如切歌窗口、加载失败重试）。此时移动文件 → 播放器指向的路径失效 → 播放异常。

### 2.2 merge 删除 remote 行后没有通知播放器

`merge.go` 只把 remote 行折叠掉，返回 `RemovedTrackID`；调用方（下载/提升）**没有发事件**让播放器把 `currentTrack`/队列映射到存活行。于是：

- 播放器继续持有已删除的 `track_id`（FK 报错）；
- 之后重新播放/自动重试时按 identity 再开流 → 新建缓存条目。

## 3. 修法

### 3.1 提升门禁加“播放器当前曲目”

- 新增窄接口（worker 依赖端口，不直接引 player）：

```go
// PlayingTrack reports the id the player currently has loaded.
PlayingTrack(ctx context.Context) (trackID string, ok bool)
```

- worker 在移动前检查：`PlayingTrack() == track.ID` → 跳过本轮（事件驱动的 session close / 播放变化再重试）；
- 与现有 `HasActiveSession` 顺序：先 session，再播放器。

### 3.2 merge 事件 + 播放器重映射

- `MergeResult` 已有 `RemovedTrackID` / `SurvivorTrackID`；在下载/提升两条最终化路径里统一 emit：

```go
app.Event.Emit("track:merged", map[string]string{
    "from_track_id": removed,
    "to_track_id":   survivor,
})
```

- 播放器监听：
  - `currentTrack.id == from` → 换成 survivor（本地行）；
  - 队列里同 id 的条目一并替换；
  - 关闭正在使用的流 session（不再对流播）；
  - 之后按本地文件继续播放。

### 3.3 兜底清理

- 维护 worker（每 30 秒 / 启动）增加一条：`stream_cache` 条目若其 identity 已对应**非 remote** 的 track → 直接 `DiscardEntry`（覆盖崩溃、竞态残留）；
- `Cache.Open` 在创建/复用缓存前先查 identity 是否已是本地行：是则返回“已有本地文件”错误，让播放器走本地路径，**不再新建缓存**。

## 4. 测试

**Go**

- worker：`PlayingTrack` 命中 → 不移动、不删缓存；
- `track:merged`：两条最终化路径（下载完成、提升完成）都 emit，字段正确；
- 播放器：收到事件后 currentTrack/队列替换为新 id，流 session 被关闭；
- 兜底：构造 `stream_cache` 条目 + 本地同 identity 行 → 启动 sweep 删除；
- `Open`：identity 已是本地行 → 返回错误、不新建行/文件。

**人工**

1. 播放云端曲目 → 加入歌单 → 播放结束后提升：缓存消失、播放器切到本地文件、无 FK 报错；
2. 提升过程中再次点播放：worker 跳过，等播放结束再移动；
3. 重启后 `stream-cache` 里不存在“已提升曲目”的条目；
4. 提升完成后不会再出现同名新缓存。

## 5. 验收

- 自动：§4 全绿；
- 人工：§4 四条成立；
- `stream-cache` 中不再残留已入库曲目的缓存。

## 6. 风险

- `track:merged` 与自动切歌/队列前进可能竞争：重映射要放在播放状态锁内，避免把下一首覆盖成 survivor；
- 播放器重映射后，scrobble/收听统计需用新 id（否则 FK 失败）；
- 兜底清理依赖 identity 查询，注意批量与索引。
