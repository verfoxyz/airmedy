# Bug：播放失败时退出应用很慢

> 状态：✅ 已完成（shutdown 先取消 service context → 停 ticker/看门狗 → 保存状态 → `Unload` 取消 FFmpeg 缓存读 → `Close`/`ma_player_destroy` 关闭设备；缓存 session 并行关闭、每个最多等 500ms 并告警；HTTP server 改为 `Close()`）
> 现象：远端曲目播放失败后退出应用要等很久（数秒到数十秒）
> 关联：`cache-file-decoder.md`（缓存文件直读）、`fix-seek-reload-failure.md`（WaitReady）

---

## 1. 原因

退出时主流程会等待播放/下载协程结束，而播放失败后这些协程正卡在**阻塞等待**里：

| 卡点 | 当前行为 |
|---|---|
| `StreamCache.WaitReady` | 最多等 30s，不响应退出 |
| 缓存文件 read（新架构） | 轮询文件增长，最长 30s |
| 下载协程的 HTTP 读 | 依赖网络超时（15s） |
| `Stop()` 等待协程 | 无上限等待 |

于是退出被这些超时拖慢。

## 2. 修法

### 2.1 退出信号：abort 优先

- `PlayerService.Stop` / 应用退出：
  1. 先置 session/decoder 的 `aborted`；
  2. 取消所有等待的 context；
  3. 再等待协程（短超时）。

### 2.2 所有阻塞等待必须可取消

- `WaitReady(ctx)`：监听 `ctx.Done()` 立即返回；
- 缓存文件 read：检查 `aborted`，返回 `AVERROR_EXIT`；
- 下载读：`http.Request` 带 context，退出时 Cancel；
- 定时轮询改用 `select { case <-ctx.Done(): ...; case <-time.After(...): }`。

### 2.3 有上限的收尾

- `Stop` 对每个协程最多等 **500ms**，超时放弃并记 warning；
- 应用退出不等待下载完成（session 落盘状态已持久化）；
- 播放失败后的重试（看门狗）在退出时立即停止。

### 2.4 顺序

```
Stop:
  mark aborted (player, session, decoder)
  cancel contexts
  close audio device
  wait goroutines ≤ 500ms
  emit final status
```

## 3. 测试

**Go**

- `Stop` 期间 `WaitReady` 在 100ms 内返回；
- 缓存 read 阻塞中被 Stop → 返回错误并退出，不泄漏 goroutine；
- 下载协程被取消，不写半行 metadata；
- 正常播放退出不受影响（无额外延迟）。

**人工**

1. 播放失败（无源/断网）后立即退出 → 1 秒内关闭；
2. 正常播放中退出 → 无延迟；
3. 连续快速退出/启动 5 次 → 无残留进程。

## 4. 验收

- 播放失败时退出 ≤ 1 秒；
- 无 goroutine/session 泄漏；
- 正常退出行为不变。

## 5. 风险

- 过早 abort 可能让正在收尾的写入中断，需要保证 metadata 写入是原子的（临时文件 + rename）；
- 超时放弃后，后台协程若仍存活需确保其操作对象已关闭，不产生 panic。
