# 任务：StreamCache 启动预缓冲（M2.5）

> 状态：待开工
> 前置：M2（StreamCache 最小版）已完成
> 目的：在 M3（播放器接入）之前，消除“播放刚开始时解码器读阻塞”的干扰项
> 范围：只改 `internal/app/stream/cache.go` 与其测试；不动播放器、不做 UI

---

## 1. 背景

现在的 `Cache.Open()` 起完下载 goroutine 就立刻返回本地 URL。FFmpeg 随即开始读代理，而代理在数据没到时按 50 ms 轮询阻塞等待——第一次读会直接卡在音频线程上（miniaudio 的数据源回调），表现为开播延迟、断音甚至爆音。

M3 接入真实流后，这会和“播放器接入是否正确”混在一起，难以定位。所以先把“开播前等一个安全水位”做掉。

## 2. 目标

- `Open()` 在返回本地 URL 之前，等待缓存达到一个**安全水位**，或超时/取消；
- 水位到了之后，FFmpeg 的启动读基本都能立即命中，不再阻塞音频线程；
- 缓存已完整时立即返回；
- 等待必须可取消（切歌、停止、下载失败）。

## 3. 设计

### 3.1 常量

```go
const (
    // prebufferBytes is the floor that must be on disk before playback starts.
    prebufferBytes int64 = 1 << 20 // 1 MiB
    // prebufferSeconds approximates a safe lead when the total size is known.
    prebufferSeconds = 3.0
    // prebufferTimeout bounds the wait; the caller gets an error past it.
    prebufferTimeout = 10 * time.Second
)
```

水位判定（取先满足者）：

1. 缓存已完整（`complete`）→ 直接返回；
2. `received >= prebufferBytes`；
3. 已知 `total > 0` 且 `track.Duration > 0`：`received >= min(prebufferBytes, total/duration*prebufferSeconds)`，用于低码率/小文件避免等太久；
4. 超时（`prebufferTimeout`）→ 返回错误；
5. `ctx` 取消或 session 下载失败 → 返回错误。

### 3.2 实现位置

`internal/app/stream/cache.go`：

- 新增 `func (c *Cache) waitForPrebuffer(ctx context.Context, s *cacheSession) error`；
- `Open()` 在 `go c.runSession(...)` 之后（完整缓存的分支不等待）、返回 `Session` 之前调用它；
- 复用已有 session 的分支同样调用（`received` 可能为 0，尤其是刚创建的 session）；
- 等待用 ticker（20–50 ms）轮询 `received`/`complete`/`failed`，不要忙等；
- 错误信息带上 identity 与已收到的字节数，便于日志定位。

### 3.3 错误语义

- 超时：返回 `fmt.Errorf("stream prebuffer timeout: %s (%d bytes)", id, received)`，使用包内已有的错误包装方式；
- 下载失败：返回该失败（与 `runSession` 记录的 `failed` 一致）；
- 取消：返回 `ctx.Err()`。

调用方（M3 的播放器）会把这些统一显示成“播放失败 + 重试”。

### 3.4 不做的事

- 不引入设置项（水位/超时先写死常量）；
- 不做“缓冲中”UI（M3）；
- 不改代理的阻塞读逻辑（预缓冲只是保证开播前有数据）；
- 不处理播放中途的卡顿（M3 实测后再决定是否上独立解码线程）。

## 4. 测试（`internal/app/stream/cache_test.go`）

用本地 httptest 服务模拟上游：

1. **慢上游**：分块发送、每块间隔 20 ms → `Open()` 阻塞到水位后返回，且返回时 `received >= prebufferBytes`；
2. **完整缓存**：预先写满缓存 + meta → `Open()` 立即返回，不等待；
3. **超时**：上游卡住不发数据 → `Open()` 在约 `prebufferTimeout` 后返回错误；
4. **取消**：等待期间取消 ctx → 立即返回 `ctx.Err()`；
5. **下载失败**：上游直接 500 → 返回错误，不进入播放；
6. **复用**：第一次 `Open` 部分下载后 `Close`，第二次 `Open` 只等待剩余部分并命中已有字节。

回归：

```bash
go test ./internal/app/stream/ ./internal/domain/ ./internal/infra/sqlite/
go build ./...
```

## 5. 验收

- 自动：§4 全绿；
- 手工（可先在测试里验证，不依赖 GUI）：本地 http 提供一首 5 分钟 MP3/FLAC，经 StreamCache 播放，`Open` 返回后再开始解码，前 3 秒内无阻塞读；
- M3 接入后再用真实远端流复验。

## 6. 风险

- 水位过高会让“点播放到出声”变慢：1 MiB / 3 秒是折中，实测后再调；
- 上游返回 `200` 忽略 Range 时下载会从头开始，预缓冲等待会因此变长——现有 fetch 已有重来逻辑，等待逻辑无需特殊处理，但要确认超时错误信息里能看出这种情况；
- 预缓冲只覆盖“开播前”，播放中网络抖动仍可能导致阻塞，这是 M3 的重点验证项，不在本任务范围。
