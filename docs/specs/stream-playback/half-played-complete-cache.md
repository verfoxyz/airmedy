# 任务：播放过半后自动补全整首缓存

> 状态：✅ 已完成（`ReportProgress` 在 `position >= duration/2` 时置一次 `halfPlayed` + `completeRequested`，无视预取水位下载到 EOF；duration=0 不触发；重开 session 后按首次上报位置同样触发）
> 目的：只要听到（或拖到）歌曲一半，就把整首缓存下载完，避免后半段再卡顿、也保证这只歌后续可用
> 关联：`bounded-prefetch.md`（有界预取）、`fix-stream-seek.md`（远端 seek 重载）

---

## 1. 规则

播放位置达到歌曲时长的一半（`position >= duration / 2`）时：

- 触发该 session 的**完整下载**：忽略预取水位，一直下载到 EOF；
- 只触发一次（会话内标记）；
- 与“是否收藏/提升”无关；
- 通过**拖动**到达一半、或正常播放到一半，都算。

## 2. 实现

### 2.1 session 标记

`cacheSession` 增加：

```go
halfPlayed bool   // 已触发补全
completeRequested bool
```

`ReportProgress(session, seconds)`（播放器每 500ms 调用）：

```go
if !s.halfPlayed && s.duration > 0 && seconds >= s.duration/2 {
    s.halfPlayed = true
    s.completeRequested = true
    s.wakeDownload()   // 解除水位限制
}
```

- `completeRequested` 与提升/下载请求共用同一标记；
- 下载循环检查：标记为真时忽略水位、下载到 EOF。

### 2.2 时长未知

- `duration <= 0`（元数据缺失）→ 不触发（无法判断一半）；
- 若 `total_bytes > 0` 且能估算，可退化为“播放进度按字节比例 ≥ 50%”再触发（可选，默认不做）。

### 2.3 seek 后

- 远端 seek 会重载 session（见 `fix-stream-seek.md`）；
- 新 session 在 `Open` 时若初始位置 ≥ 一半，直接置 `completeRequested`。

### 2.4 与有界预取的关系

- 一半之前：按 `bounded-prefetch.md` 只拉“播放位置 + 45s”；
- 一半之后：无视水位，拉到 EOF；
- 已完整缓存的曲目：no-op。

### 2.5 与提升/淘汰

- 补全只是“缓存完整”，**不会自动提升**（提升仍需用户保存/收藏）；
- 完整条目按已有淘汰规则处理（超容量时与其它条目一起排序淘汰）；
- 该会话关闭后，条目保持完整（可秒开）。

## 3. 测试

**Go**

- 进度 49% → 仍按水位停（有界预取生效）；
- 进度到 50% → 忽略水位，下载到 EOF，`complete=1`；
- 继续上报进度不会重复触发；
- `duration=0` → 不触发；
- seek 到 60% 后重开 session → 立即补全；
- 已完整条目 → 无额外请求；
- 取消/切歌时下载协程正常退出。

**人工**

1. 播放一首未缓存的歌，播到一半后观察缓存文件增长到整首大小；
2. 直接拖到 60% → 同样补全；
3. 未到一半时切歌 → 缓存只保留已下载部分（不补全）；
4. 补全后重播该歌 → 秒开。

## 4. 验收

- 自动：§3 全绿；
- 人工：§4 四条成立。

## 5. 风险

- 半首歌后补全会消耗额外带宽/磁盘（用户明确要求）；
- 大量快速切歌不会触发补全（不到一半），磁盘占用可控；
- `duration` 不准（Last.fm/标签误差）时“一半”会偏早或偏晚，可接受。
