# 最终修复：预取死锁 + seek 边界二分

> 状态：待开工
> 现象：明明有缓存，拖动后仍无声 / 停在 0s 也无法恢复
> 关联：`cache-file-decoder.md`、`fix-cache-seek-eof.md`、`diagnose-seek-silence.md`、`bounded-prefetch.md`

---

## 1. 完整因果链（已由日志证实）

```
FFmpeg avformat_seek_file 在整个文件范围内二分查找
  → 读到文件边界（file_pos == file_size，日志已确认）
  → 边界正是“预取水位暂停”的位置
  → read 阻塞
  → 播放进度不前进
  → waitForPrefetch 永远等不到进度
  → 下载不再继续
  → 死锁：有缓存的目标位置也放不了
```

日志证据：

```
seek: after 1s to=3.927 verdict=decode_blocked reads=0
  file_pos=9790420 file_size=9788491 expected_size=26206870
```

目标位置（3.9s）远在缓存内，但读光标停在文件末尾——二分探测越过了目标。

## 2. 两个必须同时做的修复

### 2.1 demand：解码器的读需求优先于预取水位

- `Cache` 新增 `SetDecodeOffset(sessionID string, offset int64)`；
- 播放层每 500ms 用解码器当前文件偏移调用一次（诊断里已有 `file_pos`）；
- `prefetchLimitReachedLocked` 增加判定：

```go
// Decoder needs the bytes ahead of the water mark.
if s.decodeOffset > 0 && s.received - s.decodeOffset < demandWindowBytes {  // 2 MiB
    return false
}
// Only resume the water mark once the decoder is far behind again.
```

- 恢复水位的条件：`received - decodeOffset > resumeWindowBytes`（4 MiB）；
- `waitForPrefetch` 同时监听 `demandChanged` 通道，避免 1 秒延迟；
- 这样：解码器读到哪，下载就补到哪；读完不追，水位照常生效。

### 2.2 seek 不做全文件二分

未完成文件（`received < total`）时：

- 计算目标字节偏移：

```
offset = seconds / duration * total
```

- **直接 seek 到该字节附近**，而不是 `avformat_seek_file(INT64_MIN, ts, ts)`；
- 上限钳制：`offset = min(offset, received - 256KiB)`，避免定位到下载边界；
- 若格式支持随机访问（FLAC STREAMINFO/seektable），从该偏移向前解析即可；
- 找不到索引时退化为“从钳制后的位置顺序解码到目标”；
- 只有文件已完成时才使用原来的 `avformat_seek_file`。

实现建议：

- `ffmpeg_seek` 增加 `int seek_mode`（0=完整文件二进制搜索，1=未完成文件按字节钳制定位）；
- 新增 `ffmpeg_seek_bytes(h, byte_offset)`，内部 `avio_seek` + `avformat_flush`，重置解码器；
- 失败时回退为“重开解码器 + 顺序解码”（兜底）。

## 3. 测试

**Go / C**

- demand：解码偏移接近接收末尾时，下载不暂停；偏移远离后恢复水位暂停；
- 未完成文件 seek 3.9s：读光标不落在文件边界、不产生边界读；
- 未完成文件 seek 到超过接收位置：等待（或按钳制顺序解码），不返回 EOF；
- 完成文件 seek：仍走原二分，行为不变；
- 0s seek：立即可播；
- 回归：`fix-cache-seek-eof` 的用例全部通过。

**人工（一次验证清单）**

1. 5s → 7s（都在缓存内）→ 立即有声；
2. 0s → 有声；
3. 拖到缓存外 → 显示缓冲，下载继续，到达后有声；
4. 播放过程中缓存文件只按需增长，不会无缘无故补全整首（除播放到一半规则）。

## 4. 验收

- 有缓存的任意位置 seek 都能立即播放；
- 不再出现 `decode_blocked` 死锁；
- 不因为 seek 触发整首下载（除非满足“过半补全”规则）。

## 5. 风险

- 按字节定位对没有索引的容器精度较差，需要顺序解码兜底；
- demand 窗口过小会导致频繁启停，2MiB/4MiB 起步可调；
- `SetDecodeOffset` 调用频率不要过高（500ms 足够），避免锁竞争。
