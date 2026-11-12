# Bug：提升/合并完成后，若「歌曲详细」打开则界面卡住

> 状态：✅ 已完成（代码已合入 wip/stream-playback）
> 现象：曲目播放结束触发缓存提升，合并完成后，如果「歌曲详细」抽屉是打开的，界面会卡住
> 范围：`track:merged` 事件处理与 `RemapTrack`；不改提升决策

---

## 1. 链路

1. 播放结束 → 提升 worker 移动缓存文件 → merge 删除 remote 行 → emit `track:merged`；
2. `internal/app/module.go` 在 `app.Event.On("track:merged", …)` 里**同步**调用：

```go
playerSvc.RemapTrack(context.Background(), from, to)
```

3. `RemapTrack` 会：
   - 读库、替换队列；
   - 加 `s.mu` 锁；
   - 调 `s.player.GetStatus()`；
   - `s.dropStreamSession()`（可能等待 session 关闭，最长 5s）；
   - 根据当前播放状态**重新加载并播放** survivor（`loadAndPlayAtPosition` / `loadInactiveAtPosition`）。

## 2. 为什么“抽屉打开”才明显

Wails v3 的 `app.Event.On` 回调运行在**主线程/事件循环**上；`RemapTrack` 里包含
I/O（读库）、锁等待、关闭流 session、以及重新加载音频。

- 抽屉打开时，前端在同一阶段还会调用 `ResolveCatalogTrackCover` 等 RPC，
  主线程阻塞会让界面看起来完全卡死；
- 没有抽屉时问题同样存在，只是页面处于空闲、不容易察觉；
- 如果曲目已经播放结束（`PlaybackStateStopped`），`RemapTrack` 仍会按 stop 分支
  `loadInactiveAtPosition` **重新加载**已结束的曲目，进一步放大阻塞。

## 3. 修法

### 3.1 事件回调不再同步做重活

```go
offTrackMerged = app.Event.On("track:merged", func(ev *application.CustomEvent) {
    payload := ...
    go func() {
        ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
        defer cancel()
        playerSvc.RemapTrack(ctx, from, to)
    }()
})
```

- 回调只负责派发；
- 用带超时的 context，避免无限等待；
- 失败只记日志。

### 3.2 `RemapTrack` 只做必要的重载

- 先取一次原子状态快照（current track、playback state、position），不要长时间持锁；
- 如果 `status.PlaybackState == Stopped`：
  - 只替换 `currentTrack` / 队列 / 统计 id，**不调用** `loadInactiveAtPosition`；
  - 不重开音频，避免“播放结束又加载”；
- 只有 `Playing`/`Paused` 才按原逻辑重载；
- `dropStreamSession` 在独立 goroutine 或带超时（现有 5s）里执行，不阻塞主线程。

### 3.3 抽屉侧

- `ResolveCatalogTrackCover` 是只读查询，不应阻塞；但主线程被占时它表现为卡；
- 无需改前端逻辑，修好 3.1/3.2 后抽屉打开也不再卡。

## 4. 测试

**Go**

- `track:merged` 回调立即返回（用 fake player 记录调用耗时，断言主线程不被阻塞）；
- `RemapTrack` 在 `Stopped` 状态下不调用任何 load/play；
- `Playing`/`Paused` 状态下仍重载并保持位置；
- `RemapTrack` 超时/失败只记日志，不 panic；
- 合并发生时队列替换正确（含多处同 id）。

**人工**

1. 播放一首已缓存曲目，打开「歌曲详细」，播完触发提升 → 界面不卡；
2. 抽屉打开时切歌/暂停后触发合并 → 不卡；
3. 播放中合并（重新加载）仍能从原位置继续。

## 5. 验收

- 自动：§4 全绿；
- 人工：§5 三条成立。

## 6. 风险

- 异步 remap 与自动切歌可能竞争：需要按 id 幂等，重复调用无副作用；
- `Stopped` 不重载后，用户再点播放会按新 id 正常加载；
- 超时设得太短可能让 97MB 级缓存的重载失败，10s 起步。
