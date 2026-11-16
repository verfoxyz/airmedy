# Bug：远端流曲目拖动进度条后无声，且不会恢复

> 状态：✅ 已完成（remote seek = 关闭旧 session + `loadInactiveAtPosition` + 恢复播放/暂停；300ms 防抖；2s 卡住看门狗每段只重载一次；`player:playback-failed` 进入 footer 的 playError）
> 现象：播放缓存中的在线曲目，拖动进度条（哪怕拖回 0:00）后完全无声；切走到别的曲目再切回来才恢复
> 范围：播放器 seek 路径；不改 StreamCache 协议

---

## 1. 现象与数据

- 曲目 `All Eyes On Me` 的缓存行：`complete=0`，`received/total ≈ 69%`；
- 拖到 5s、0:00 都无声（0:00 在已下载区，排除“字节没到”）；
- 切走再切回 → 正常（重新加载曲目 = 新 session/新解码器）；
- 日志里没有任何 seek/加载失败记录。

## 2. 根因分析

远端曲目经 FFmpeg（miniaudio 数据源）读 `http://127.0.0.1/stream/<id>`：

- 拖动时走的是**原地 seek**（`av_seek_frame` / `ma_decoder_seek_to_pcm_frame`）；
- HTTP 数据源上的原地 seek 失败后，解码器留在无效状态，播放线程不再出帧；
- 没有“检测卡住并重建解码器”的逻辑，也没有日志；
- 只有重新加载曲目（新解码器 + 新 session）才恢复。

## 3. 修法

### 3.1 远端曲目的 seek = 重载（推荐）

对 remote 行，`Seek(position)` 不再调用解码器原地 seek，而是：

1. 记住目标位置与当前播放/暂停状态；
2. 关闭当前流 session（`dropStreamSession`）；
3. 用 `loadInactiveAtPosition(track, position, paused)` 或等价路径重新打开并定位；
4. 保持队列/currentTrack 不变。

- 与“切走再切回”等价，已被证明可恢复；
- 本地文件仍走原地 seek（成本低、体验好）。

### 3.2 seek 防抖

- 拖动过程只更新 UI 位置，**松手时**才触发一次重载；
- 连续 seek 合并为最后一次（300ms 窗口），避免反复重建 session。

### 3.3 卡住看门狗（兜底）

- 状态为 playing、位置在 2 秒内没有前进 → 记录 warning 并自动重载一次（限一次，防循环）；
- 失败后显示「播放失败，请重试」。

### 3.4 日志

- seek 时记录：track id、目标位置、是否 remote、重载耗时、结果；
- 卡住重载记录原因。

## 4. 测试

**Go**

- remote 行 seek → 关闭旧 session、按目标位置重新加载、状态保持（playing/paused）；
- 连续多次 seek → 只重载一次（最后一次）；
- 本地行 seek 仍走原地 seek（回归）；
- 卡住看门狗：位置不前进 → 触发一次重载，不重复循环；
- 重载失败 → `playError` 可见。

**人工**

1. 播放缓存未完成的在线曲目，拖动到任意位置 → 立即恢复出声；
2. 拖回 0:00 → 从头播放；
3. 拖动中反复移动 → 松手后只加载一次；
4. 本地文件拖动行为不变。

## 5. 验收

- 自动：§4 全绿；
- 人工：§4 四条成立；
- 远端曲目 seek 后不再出现“静音且无法恢复”。

## 6. 风险

- 每次 seek 都重建解码器会增加启动延迟（流媒体可接受）；
- 防抖窗口过大会让 seek 显得迟钝，300ms 起步；
- 看门狗误判（音频确实停顿）会导致重载，需要限一次并记录。
