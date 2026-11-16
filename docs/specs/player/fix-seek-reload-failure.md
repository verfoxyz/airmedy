# Bug：拖动后重载失败，界面继续走进度但无声

> 状态：✅ 已完成（重载前 `StreamCache.WaitReady` 等上游响应；就绪/加载失败即暂停并 emit `player:playback-failed`；看门狗 2s/10s/60s 退避、最多 3 次，手动 Play 清零；代理日志增加首字节耗时与 session/URL）
> 现象：远端曲目拖动进度条后没有声音；进度条继续前进，不暂停、不提示
> 日志：`ma_player_load failed: -10`、`stream stall reload failed`，看门狗反复重试
> 关联：`fix-stream-seek.md`（远端 seek 改为重载）

---

## 1. 现象与日志

```
00:50:15 WARN  stream playback stalled; reloading the session once position=62.45
00:50:37 ERROR failed to reload inactive track track=remote://wy/36307902
      error="ma_player_load failed: -10 (.../stream/b2662a0c…)"
00:50:37 ERROR stream stall reload failed position=62.45
00:50:44 WARN  stream playback stalled; reloading the session once position=67.07
```

- 重载**立即失败**（同一毫秒返回），不是等数据超时；
- 失败后播放状态没有改变，位置仍在推进；
- 看门狗每 2 秒重试一次，同样失败。

## 2. 根因

1. **重载时机**：`loadInactiveAtPosition` 直接 `ma_player_load` 新的 `/stream/<hash>` URL，
   但此时新 stream session 可能尚未建立/尚未达到预缓冲，代理立即返回错误（或空响应），
   FFmpeg 打开失败 → `-10`；
2. **失败不落状态**：加载失败只打日志，没有把播放器置为暂停/卡住，也没有 emit
   `player:playback-failed`，前端继续按原来的“播放中”刷新进度；
3. **看门狗盲重试**：不检查 session 是否就绪，也不退避，反复失败。

## 3. 修法

### 3.1 重载前确保 session 就绪

- `runRemoteSeek` 里先走与正常播放相同的准备流程：
  - 建立/复用 stream session（`Open`）；
  - 等待预缓冲（1 MiB / 3s，带超时）；
  - 再 `loadInactiveAtPosition`；
- 若 session 建立失败，直接按 3.2 处理，不调用 `ma_player_load`。

### 3.2 加载失败必须落到状态

- `loadInactiveAtPosition` 返回错误时：
  - 停止进度推进（`Pause`/置 `stalled` 状态）；
  - emit `player:playback-failed`（前端已有 `playError` 展示）；
  - 记录错误与目标位置；
- 不再“假播放”。

### 3.3 看门狗

- 仅对“播放中且位置 2 秒未前进”的远端曲目触发；
- 重载失败后进入退避（2s → 10s → 60s，最多 3 次），并显示错误；
- 用户手动点播放/重试时重置退避。

### 3.4 诊断日志

- 记录代理返回的状态码/首字节等待情况（在 `handleStream` 侧）；
- 记录 `ma_player_load` 失败的 URL 与 session id，便于区分“代理未就绪”和“源不可用”。

## 4. 测试

**Go**

- seek 重载：session 未就绪时先等待，就绪后加载成功；
- session 建立失败 → 状态置 stalled、事件发出、位置不再前进；
- 加载失败 → 同上，且不继续空转；
- 看门狗：失败后退避，不无限重试；
- 本地文件 seek 不受影响。

**人工**

1. 播放缓存未完成的远端曲目，拖动 → 有声，或失败时明确提示并停止进度；
2. 拖动到很大位置（未缓存）→ 等待加载，进度不空走；
3. 故意断网拖动 → 提示失败，进度停止。

## 5. 验收

- 自动：§4 全绿；
- 人工：§4 三条成立；
- 不再出现“有进度、无声音、无提示”的状态。

## 6. 风险

- 等待 session 就绪会增加 seek 延迟（流媒体可接受）；
- 看门狗退避可能让恢复变慢，需要 `play` 手动重试立即清零。
