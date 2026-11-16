# 重构：远端流播放改由 FFmpeg 直读本地缓存文件（去掉 HTTP 解码路径）

> 状态：✅ 已完成（HTTP 解码路径已移除；AVIO read 必须填满缓冲，短读只发生在真实 EOF；未知总长由下载完成信号 `MarkStreamComplete` 产生 EOF；`size()`/`SEEK_END` 在下载完成前只暴露已写入字节，seek 到未下载区会等待而不是被判 EOF；看门狗把「缓存仍在增长」视为缓冲中，不再重载，仅下载失败/停滞才报错或重试）
> 目的：彻底消除 `ma_player_load failed: -10` —— 根因是 FFmpeg 通过 HTTP 打开远端流时的大范围探测/超时
> 关联：`fix-stream-seek.md`、`fix-seek-reload-failure.md`、`fix-cached-range-502.md`、`half-played-complete-cache.md`
> 退出/取消语义见 `docs/specs/player/fix-exit-hang.md`：阻塞 read 必须响应 abort，`Stop` 有短超时

---

## 1. 为什么要改架构

现状链路：`miniaudio → FFmpeg → http://127.0.0.1:<port>/stream/<hash> → StreamCache 文件`

问题都来自“让解复用器做网络 I/O”：

- `ffmpeg_decoder.h` 设置 `probesize=32MB`、`rw_timeout=15s`，远端打开时会探测到 32MB 并读取文件尾部；缓存不足或下载停滞 → 15s 超时 → `-10`；
- 已缓存区间也要经过 HTTP、Range、等待上游，任一环节阻塞都会让“打开”失败；
- seek 需要重建 session，重建又触发一次探测，形成循环失败；
- 之前的所有补丁（等就绪、缓存区间直返、缓存内原地 seek）都只是绕过症状。

结论：**解码器不应该看到网络**。让 FFmpeg 直接读本地缓存文件，用自定义 `AVIOContext` 在“数据还没下到”时阻塞等待。

## 2. 目标架构

```
下载协程 ──写入──> stream-cache/<hash>.audio（本地文件，持续增长）
                        ▲
                        │ 读取（阻塞等待新字节）
FFmpeg 自定义 AVIOContext ──> 解码 ──> miniaudio
```

- 播放器拿到的是**本地文件路径** + 预期总字节数（或“未知”）；
- FFmpeg 的 probe、seek、读取全部发生在本地文件上，速度与本地播放一致；
- 数据未就绪时，自定义 read 回调等待（轮询文件大小 / 条件变量），到达后继续；
- 不再使用 HTTP 代理解码路径。

## 3. 实现

### 3.1 新增 C 层：本地文件 AVIO

新文件 `internal/infra/audio/ffmpeg_cache_io.h/.c`：

```c
typedef struct {
    FILE* fp;
    int64_t expected_size;   /* 0 = 未知 */
    int64_t written;         /* 由文件大小实时读取 */
    volatile int aborted;    /* ffmpeg_cancel 时置位 */
    int wait_ms;             /* 单次等待上限，默认 30000 */
} FFmpegCacheIO;
```

- `read(pkt)`：
  1. 当前文件大小 < 需要读取的偏移 + 请求长度时，`sleep 5–10ms` 重新 `stat` 文件；
  2. 达到 `expected_size` 且已读完 → 返回 EOF（`AVERROR_EOF`）；
  3. 超过 `wait_ms` 仍无进展 → 返回 `AVERROR(EIO)`（由上层显示失败）；
  4. `aborted` 时立即返回 `AVERROR_EXIT`；
- `seek(offset, whence)`：移动游标（`SEEK_END` 基于下面 `size()` 的返回值）；目标在当前写入边界之后、`expected_size` 之内时等待下载追上，超出 `expected_size` 报错（见 `fix-cache-seek-eof.md`）；
- `size()`：下载未完成时返回**当前已写入大小**，完成后返回 `expected_size`；不能把未来才会存在的字节暴露为可读区域；
- 与现有 `ffmpeg_probe_mu` 锁、`interrupt_callback` 保持一致。

#### 3.1.1 read / size 的精确语义（关键，易错）

缓存文件是**持续增长**的，read 必须区分“暂时没数据”和“真的结束”：

1. **只有** `offset >= 当前已写入大小` **且**（下载已完成或已写入 `expected_size`）时返回 `AVERROR_EOF`；文件仍在增长时只等待，不返回 EOF；
2. 当前文件大小 < `offset + 请求长度` 时：
   - 轮询 `stat`（5–10ms）；
   - 每轮检查 `aborted`；
   - **绝不返回 0 / 短读 0**（否则 FFmpeg 认为流结束，解码器停止前进）；
   - 等待超过上限（默认 30s）才返回 `AVERROR(EIO)`；
3. `expected_size == 0`（未知总长）时，只有在“下载已失败/结束”的情况下才返回 EOF，否则一直等到 `aborted` 或超时；
4. `size()` 必须**实时**反映真实可读范围：未完成时是当前文件大小，完成后是 `expected_size`，不能缓存打开时的值；
5. 每次 read 成功后刷新内部 `written`，供 stall 检测与日志使用。

### 3.2 解码器

`ffmpeg_decoder.h` 的 open：

- 本地文件：保持现状（32MB probesize、rw_timeout）；
- 缓存模式：用自定义 AVIO 打开，`probesize` 可保持 32MB（本地读很快），**删除 rw_timeout 依赖**；
- 新增参数：`cache_total_bytes`（会话已知总长时传入；未知传 0）。

### 3.3 Go 侧

- `TrackDTO`/播放参数增加缓存模式：
  - `StreamCachePath`：缓存文件绝对路径；
  - `StreamTotalBytes`：已知总字节（`total_bytes`），未知 0；
- `ma_player_load` 增加对应参数（或新增 `ma_player_load_cache`）；
- 播放前确保头部可用：等待 `min(2MB, total)` 已落盘（本地读，不再用 HTTP 语义），最多 15s；
- `Stop`/切歌 → 置 `aborted`，阻塞中的 read 立刻退出。

### 3.6 卡住判定与看门狗（与缓冲区分）

- **下载仍在进行**（`received_bytes` 在增长）：这是“缓冲中”，**不重载**；
  - UI 显示缓冲状态；播放线程继续阻塞在 read 上；
  - 位置不前进是正常的。
- **下载已停滞**（`received_bytes` 超过 N 秒不变、且未完成）：
  - 先尝试恢复下载（重新解析/续传）一次；
  - 仍无进展才允许重载解码器；
- **只有真正失败**（session failed、上游无链接、磁盘错误）才显示播放失败；
- 看门狗不得把“等待缓冲”当成“卡住”，否则每次缓冲都重载，表现为一声一停。

### 3.4 seek

- seek 仍由“重开解码器到目标位置”实现（或直接 `av_seek_frame`，本地文件可靠）；
- 目标区间未下载时，read 阻塞等待下载到达，显示“缓冲中”，不再报 502/`-10`；
- 用户停止/切歌 → abort。

### 3.5 HTTP 代理

- 播放路径不再引用；保留 handler 仅用于调试/兼容，标注 deprecated；
- 后续可删除 `stream_proxy` 及 `fix-cached-range-502` 相关代码。

## 4. 测试

**Go / C**

- 缓存文件增长中：读到的位置始终跟上下载，**不出现 EOF**（关键回归）；
- 文件大小在 read 等待期间增长：read 返回新数据而不是 0；
- `expected_size > 0` 且读到末尾：返回 EOF；
- 未知总长 + 下载结束：返回 EOF；
- 下载停滞：看门狗只在“停滞”时重载，缓冲期间不重载；
- 缓存文件完整：正常播放；
- 缓存文件只到一半：从头播放，读到一半阻塞，下载继续后恢复；
- 下载失败 / session failed：阻塞 read 在超时后返回错误，播放器显示失败；
- seek 到已下载位置：立即定位；
- seek 到未下载位置：等待下载后继续（不返回 `-10`）；
- 停止/切歌：阻塞 read 100ms 内退出，无泄漏；
- 本地文件播放回归不变。

**人工**

1. 播放远端曲目 → 正常出声；
2. 5s→7s 拖动 → 立即播放；
3. 拖到未缓存位置 → 显示缓冲，下载到达后继续；
4. 断网 → 已经在缓存内的部分可继续播放；超出后提示失败；
5. 快速切歌 10 次 → 无卡顿、无僵尸 session。

## 5. 验收

- 不再出现 `ma_player_load failed: -10`；
- 已缓存区间播放与 seek 不依赖网络；
- 未缓存区间等待而不是报错；
- 本地文件行为不变。

## 6. 风险

- 自定义 AVIO 需要正确实现 seek/size/EOF 语义，否则 FFmpeg 会误判时长；
- 阻塞 read 必须响应 abort，避免线程卡死；
- 文件增长时 `size()` 变化需要 FFmpeg 不缓存过期的 size（必要时在 read 中刷新）；
- 移除 HTTP 解码路径后，`ma_player_load` 的调用方（footer/队列/重映射）都要改参数，回归范围大。
