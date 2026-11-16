# Bug：seek 后立即 EOF（AVIO 把声明总长当成文件大小）

> 状态：✅ 已完成（`AVSEEK_SIZE`/`SEEK_END` 只暴露已写入字节；seek 到未下载区等待下载追上，超出声明总长报错；read 仅在真实结束处 EOF；单测覆盖见 `internal/infra/audio/ffmpeg_cache_io_test.go`）
> 现象：远端曲目 seek 后无声；日志 `verdict=engine_not_consuming`，但真正原因是解码器 seek 后读到 EOF
> 关联：`cache-file-decoder.md`、`diagnose-seek-silence.md`

---

## 1. 日志证据

```
seek: track=… from=5.85 to=9.30219 remote=true cached=true covers=true player_seek_rc=0
seek: after 1s: reads=585 frames=280800 empty=0 waits=1 wait_ms=320
  cursor=9.3021875 file_pos=26205845 file_size=9772366 expected_size=26206870
  verdict=engine_not_consuming
```

关键：

- 实际文件 **9.7 MB**（未下完），`expected_size` = **26.2 MB**（声明总长）；
- seek 后 `file_pos = 26205845`（≈ 尾部），超过真实文件大小；
- 该位置在代码里满足 `pos >= expected_size - 1023` 的 EOF 判定 → 解码器立刻收到 EOF；
- `reads=585 / frames=280800` 只是 seek 前预读的缓冲，播完就静音；
- 光标停在 `to=9.3021`，不再前进。

结论：**`AVSEEK_SIZE` 返回声明总长，seek 把游标移到尚未下载的区域，随后 read 把它们判成 EOF。**

## 2. 修法

### 2.1 `AVSEEK_SIZE` / `size()`

- 文件未完成（`complete == false`）→ 返回**当前已写入大小**；
- 文件已完成 → 返回 `expected_size`（两者应相等）；
- 不允许把“未来才会存在的字节”暴露成可读区域。

### 2.2 seek 越界

`ffmpeg_cache_seek` 里：

```c
if (target > current_size && expected_size > 0 && target <= expected_size) {
    wait_until_file_reaches(target);   // 5–10ms 轮询，检查 cancel，超时 30s
}
if (target > current_size && !complete) return AVERROR(EIO); // 真越界
```

- 等待期间允许 FFmpeg 的分段 seek 继续（会缩短二分范围）；
- 超时返回错误，由播放层提示失败。

### 2.3 read 的 EOF 判定

- EOF 仅在 `pos >= 当前已写入大小` **且**（`complete` 或下载已失败/结束）时返回；
- 文件正在增长时不返回 EOF，继续等待（现有 §3.1.1 语义不变）。

## 3. 测试

**Go / C**

- 未完成文件：`AVSEEK_SIZE` 返回当前大小，不返回 `expected_size`；
- seek 到当前大小之后、`expected_size` 之内 → 阻塞至文件增长后成功；
- seek 到 `expected_size` 之外 → 返回错误；
- read 在文件增长时不返回 EOF；
- 完成文件：seek 到任意位置正常；
- 回归：完整文件播放、看门狗、缓冲判定不变。

**人工**

1. 播放未下完的远端曲目，5s → 9s 拖动 → 有声且位置正常前进；
2. 拖到接近结尾（未下载）→ 等待下载后继续；
3. 下载完成后随机 seek → 正常。

## 4. 验收

- seek 后不再出现“位置停住/无声音”；
- `file_pos` 始终不超过当前文件大小（除非等待完成后）；
- 不出现误判 EOF。

## 5. 风险

- 返回“当前大小”会让 FFmpeg 认为时长偏短，进度条总时长需要继续用 `expected_size/duration`（来自元数据），不要用 AVIO size；
- seek 等待要可取消，避免拖慢退出；
- 二分 seek 可能多次等待，日志中记录累计等待时间。
