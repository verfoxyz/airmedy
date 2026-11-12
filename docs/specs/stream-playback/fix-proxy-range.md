# Bug：缓存已完整且文件有效，播放仍报 `ma_player_load failed: -10`

> 状态：待开工
> 案例：`Flamewall`（wy/1482677427）
> 关联：`task-prebuffer.md`、`spec.md`（StreamCache 代理）

---

## 1. 证据

- `stream_cache` 行：`complete=1, total_bytes=received_bytes=97,721,783`；
- 缓存文件存在且大小一致：

```
stream-cache/81712cde…​.audio   97,721,783 bytes
```

- 文件本身是**合法 FLAC**：
  - `ffprobe` 识别 `flac / 44100Hz / stereo / s32`；
  - `ffmpeg -i file -f null -` 完整解码 **exit 0**，无错误；
- 应用日志（三次重试）：

```
ERROR "failed to load track" track=remote://wy/1482677427
  error="ma_player_load failed: -10 (path: http://127.0.0.1:26266/stream/81712cde…)"
```

- 同一进程内其它远程曲目可以播放；`-10` 来自播放器加载路径，不是网络下载失败（没有 `Stream cache download failed`）。

**结论：不是“页面数据陈旧/缓存信息没更新”，文件与行都是完整有效的；失败发生在 HTTP 代理 → FFmpeg 解码器这一段。**

## 2. 最可疑的根因：Range 处理不完整

`internal/app/stream/cache.go` 的 `parseRangeHeader` 只支持 `bytes=N-`：

```go
if first == "" { return ..., errors.New("suffix ranges are not supported") }
```

而 FFmpeg/HTTP 客户端在探测可 seek 的流时，常会发：

- `Range: bytes=-N`（读文件尾部，用于探测/seek）；
- `Range: bytes=N-M`（有界区间）。

当前行为：

1. **后缀 Range** → 返回 `416`；FFmpeg 可能直接判为不可读；
2. **有界 Range** → 解析只取 start，却把 `Content-Range` 写成 `bytes N-(total-1)/total`，**范围比请求大**，不符合 HTTP 语义；
3. 没有 Range 时返回 200；有 Range 时返回 206——这部分是对的。

不同文件的探测方式不同，可以解释“只有部分歌曲失败”。

## 3. 修法

### 3.1 完整解析 Range

```go
type byteRange struct { start, end int64; suffix bool }
// bytes=N-M / bytes=N- / bytes=-N
```

- 有界：`Content-Range: bytes N-M/total`，`Content-Length: M-N+1`；
- 开放：`N-(total-1)`；
- 后缀：最后 N 字节；
- 越界 → 416 并带 `Content-Range: bytes */total`；
- 不支持多段 → 416（现状可保留）。

### 3.2 增加诊断日志

在 `handleStream` 里记录（debug 级）：

- 请求的 `Range`、返回状态、`Content-Range`、实际写出字节数；
- 解码器一侧在 `ma_player_load` 失败时记录 HTTP 状态（若有）。

### 3.3 验证

- 用 `curl -H 'Range: bytes=-1024'` 请求一个完整缓存：应返回 206 + 1024 字节，而不是 416；
- 用 `curl -H 'Range: bytes=0-99'`：应返回 206 + 100 字节；
- 播放 Flamewall：不再出现 `-10`。

## 4. 测试

**Go（httptest）**

- `bytes=0-99` → 206、100 字节、`Content-Range: bytes 0-99/total`；
- `bytes=100-` → 206、到 EOF；
- `bytes=-1024` → 206、最后 1024 字节；
- 越界 → 416 + `bytes */total`；
- 多段 → 416；
- 无 Range → 200 + `Accept-Ranges: bytes`；
- 流未完成时请求未到达的位置仍阻塞等待（现有行为不变）。

**人工**

1. 播放 Flamewall（完整缓存）→ 正常开始；
2. 拖动进度条到任意位置（触发新的 Range）→ 正常；
3. 播放中切歌/暂停 → 无 `-10`。

## 5. 验收

- 自动：§4 全绿；
- 人工：§4 三条成立；
- 完整缓存的远程曲目不再因 Range 形式不同而加载失败。

## 6. 风险

- 修改 Range 语义要保证“边下边播阻塞”不受影响（未到达的字节仍等待）；
- 解码器可能同时发多个 Range 请求，需保持并发安全；
- 如果 -10 与 Range 无关，诊断日志（§3.2）应能直接暴露真实 HTTP 状态，再据实修正。
