# 诊断：seek 后位置在走但没有声音

> 状态：待开工（诊断任务，不是修复）
> 目的：在改代码前确定“seek 成功但无声”的确切环节
> 关联：`fix-stream-seek.md`、`fix-seek-reload-failure.md`、`cache-file-decoder.md`

---

## 1. 已确认的事实

| 事实 | 依据 |
|---|---|
| 缓存文件在 7s 处数据正常 | `ffmpeg -ss 7 -i <cache>.audio -t 2 -f null -` exit 0 |
| 前端确实调用后端 seek | `store.seek` → `PlayerService.Seek` |
| 后端 seek 返回成功 | 没有 `markStreamStalled` / 重载日志 |
| 没有加载失败 | 无 `ma_player_load failed` 日志 |
| 位置在走、暂停回到 7s | 引擎光标在推进 |

结论：问题在 **`player.Seek` 成功之后、音频输出之前**。

## 2. 待区分的两种可能

- **A. 解码阻塞**：seek 后 `ffmpeg_cache_read` 等待的字节未到达，引擎取不到样本 → 静音填充；
- **B. 输出异常**：解码正常出帧，但输出层是静音（通道/音量/重采样/数据源指针）。

## 3. 诊断埋点（最小改动）

### 3.1 解码器帧数

在 `ffmpeg_read`（`ffmpeg_decoder.h`）统计每次调用的返回值：

```c
h->diag_reads++;
h->diag_frames += read;
h->diag_last_read_ms = now_ms();
if (read <= 0) h->diag_empty_reads++;
```

`ma_player_seek` 后清零计数，播 1 秒再读取：

- `diag_empty_reads > 0` 或 `diag_frames == 0` → **A**（读取阻塞/无数据）；
- `diag_frames ≈ 采样率` 且无空读 → **B**（数据已产出，问题在输出层）。

### 3.2 缓存 AVIO 等待时间

在 `ffmpeg_cache_read` 记录等待次数与累计等待时长：

```
waits / wait_ms_total / pos / file_size / expected_size
```

- 等待时间长 → 说明读到了未下载区；
- 无等待 → 数据已在盘上，问题不在这里。

### 3.3 输出层

- `ma_player_get_cursor` 与 `ffmpeg` 当前帧数是否一致；
- 引擎 `ma_engine` 的 master volume、sound volume（seek 前后）；
- 是否有 underrun 计数（miniaudio 无直接 API，可用“输出回调请求帧数 vs 数据源提供帧数”统计）。

### 3.4 日志规范

每次 seek 输出一行 Info：

```
seek: track=<id> from=<pos> to=<pos> remote=<bool>
  cached=<bool> covers=<bool>
  player_seek_rc=<0/nonzero>
  after 1s: reads=<n> frames=<n> empty=<n> wait_ms=<ms> cursor=<s>
```

## 4. 判定表

| read 帧数 | 等待 | 光标 | 结论 | 下一步 |
|---|---|---|---|---|
| 0 / 阻塞 | 高 | 前进 | A：解码阻塞 | 修 AVIO seek 后的定位/预读 |
| 正常 | 低 | 前进 | B：输出静音 | 查音量/通道/数据源指针 |
| 0 | 低 | 前进 | 解码器 seek 后状态错误 | 查 `avformat_seek_file`/`is_eof` |
| 正常 | 低 | 不动 | 引擎未消费 | 查 sound 播放状态 |

## 5. 验收

- 能明确落到 A 或 B 之一；
- 记录复现步骤（曲目、缓存进度、seek 前后位置）；
- 根据结论再开修复任务，不再盲改。

## 6. 风险

- 埋点会改变时序，需确认不影响复现；
- `ffmpeg_read` 在音频线程调用，统计要用原子/无锁；
- 不要在生产构建保留高频日志，诊断后删除或降级。
