# Bug：队列出现同一首歌的「幽灵」重复

> 状态：待开工
> 现象：播放队列里出现很多条同一首歌（如 `Aegleseeker ("Afterworld" Full Version)`）
> 触发现场（用户回忆）：先在其它页面点「下一首播放」，随后在 Last.fm 歌单点「播放全部」

---

## 1. 已核实的部分

- 持久化队列 `player_state.queue_track_ids`：50 条、**id 全唯一、无缺失行**；
- 因此“幽灵”不是数据库里真有重复行，而是**运行时队列/渲染**层面的重复。

## 2. 可疑链路

### 2.1 `PlayNext` 与 `PlayTracks` 的交错

- `PlayNext` → `queue.InsertAfterCurrent` + `resyncPreQueue`；
- `PlayTracks`（播放全部）→ `queue.SetQueue(tracks, startIndex)` → `loadAndPlay`；
- 若「下一首播放」插入后，预排队（gapless `nextPreQueued`）还持有旧对象，`SetQueue` 替换队列时**没有清掉预排队/正在加载的对象**，可能出现旧 item 残留或重复渲染；
- `PlayTracks` 不会去重：如果调用方传入的 `tracks` 里本身有重复（Last.fm 行 → `preparePlayback` 映射），会原样进入队列。

### 2.2 前端渲染 key

- 队列面板若用 `index` 或 `title` 作为 key，队列替换/插入时可能复用行，出现“视觉重影”；
- 播放中合并（`track:merged`）会 `ReplaceTrack`，若事件与队列刷新交错，旧行可能短暂残留。

### 2.3 事件顺序

- `player:queue-updated` 是“整队快照”，正常不会累加；
- 但若前端 store 在收到事件时做了合并而不是替换，或事件与本地乐观更新交错，就会显示重复。

## 3. 修法方向

1. **队列入口去重（后端）**：
   - `SetQueue` / `AppendTracks` / `InsertAfterCurrent` 统一经过 `normalizeQueue`：
     - 按 `track.ID` 去重（保留第一次出现的位置）；
     - 同一首歌的多次“手动重复添加”如果需要支持，另存计数，不在本轮；
2. **替换队列时清理预排队**：
   - `SetQueue` 里清空 `nextPreQueued`、取消进行中的 gapless 预加载；
   - 确保 `loadAndPlay` 之后不会再次启动旧对象；
3. **前端 key**：
   - 队列行 key 用 `track.id`（同 id 只应出现一次）；若允许重复，用 `id + occurrence`；
4. **诊断日志**：
   - `emitQueue` 时记录队列长度与前 5 个 id；
   - 队列变更（PlayNext / PlayTracks / merge）各记一条 info，便于下次复现定位。

## 4. 测试

**Go**

- `PlayNext(A)` → `PlayTracks([B,C])` → 队列为 `[B,C]`，无 A 残留、无重复；
- `SetQueue` 传入含重复 id 的列表 → 去重后播放；
- `SetQueue` 后 `nextPreQueued` 被清空；
- 合并 `ReplaceTrack` 后队列无重复、无旧 id。

**前端**

- 队列面板用 id 作 key，重复数据只渲染一行；
- 连续收到两次 `queue-updated` 不累加。

**人工**

1. 「下一首播放」A → Last.fm 歌单「播放全部」→ 队列无 A 重影；
2. 播放中合并/提升 → 队列无幽灵行；
3. 重启后队列与关闭前一致。

## 5. 验收

- 自动：§4 全绿；
- 人工：§5 三条成立；
- 队列中不再出现同一 track id 的多行。

## 6. 风险

- 去重会禁止“故意把同一首歌加进队列两次”；若产品需要，后续用 occurrence 计数支持；
- 清理预排队要避免打断正在进行的 gapless 播放（仅在新 `SetQueue` 时）；
- 前端 key 变更需要同步测试快照。
