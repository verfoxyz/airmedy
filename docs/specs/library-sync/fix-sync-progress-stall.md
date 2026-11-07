# 修复：同步/优化搜索的进度弹窗卡死并跟随到其他页面

> 状态：待开工
> 发现方式：本地复现（2026-11-07 22:57），截图 + 日志 + 进程采样
> 约束：AI 不做图形化验证；本文的“前端停住”部分有一个未证实的环节，见 §6

---

## 1. 现象

在 设置 → 音乐库 点击 **优化搜索**（`ReindexAll`）后：

- 后端**已经完成**（日志 `Starting full library re-indexing` → `Finished`，4900 项只用 **0.66 秒**）；
- 前端进度弹窗停在 **3901 / 4900**（上一次停在 3897），一分钟以上不动；
- 切到其他页面（曲目/首页）后弹窗**仍然悬在页面上**；
- 上一轮复现里，连播放器的时间显示都冻在 `3:16 / 3:27` —— **整个前端事件流**都停了，不只是进度。

## 2. 复现证据

| 时间 | 事件 |
|---|---|
| 22:57:17.235 | 日志 `Starting full library re-indexing` |
| 22:57:17.898 | 日志 `Finished full library re-indexing`（**后端完成**） |
| 22:57:2x | 截图 1：弹窗 `3901 / 4900`，标题「正在同步音乐库…」 |
| 22:58:1x | 截图 2：仍是 `3901 / 4900`，完全一致 |

补充观察：

- 用户点的是「优化搜索」，但弹窗标题是「正在同步音乐库」——说明组件状态已被重建，后来的进度事件又把它重新打开（`syncType` 丢了）；
- 两次复现都停在 3897 / 3901，且都显示同一个艺人 `Artist: Jawsh 685` —— **确定性**，不是随机竞态；
- 主进程 CPU 增量 0（空闲），WebView2 渲染进程持续 ~30% CPU（JS 一直在忙）。

## 3. 根因链

1. `ReindexAll` 对 **每一项**（曲目+专辑+艺人+作曲家+歌单，共约 4900 项）发一条 `library:sync-progress`，全部在 0.66 秒内发完（≈7400 条/秒）。
2. Wails v3 的前端事件走一条 **FIFO mailbox**：`Event.Emit` 入队后由单个 goroutine 逐条投递（`pkg/application/events.go` + `internal/mailbox`），每个窗口还有 64 条上限的队列（`enqueueEventJS`，满了会阻塞发送方）。`sync-finished` 排在全部进度事件**之后**。
3. 前端消费速度跟不上这 4900 条，队列越积越长；中途一旦出现一个长任务/异常，投递就停住，后面的进度和 `sync-finished` 永远到不了 → 弹窗停在最后收到的条数上。
4. 播放器的时间显示也冻住，是因为播放状态事件排在同一个 FIFO 后面（不是播放引擎卡了）。
5. 弹窗跨页面：`Modal` 用 `Teleport to="body"`，而设置页在 `MainLayout` 的 `<KeepAlive :max="3">` 里；切页只触发 deactivate，teleport 出去的内容没有移除，于是悬在其他页面上。
6. 标题变成「同步音乐库」而不是「优化搜索」：设置页可能被 KeepAlive 淘汰/重建，`syncType` 归零；迟到的进度事件把弹窗重新打开（`handleSyncProgress` 只设 `isSyncing`）。

## 4. 修法

### 4.1 后端：进度事件节流（核心）

三处发出点（`internal/app/library/service.go`）：

- `:808` 文件夹导入循环
- `:1419` 重新拆分（re-split）
- `:2022` 全量重建索引（reindex）

统一规则：**最多每 100ms 发一条；最后一项必定发一条 100%；完成事件不受节流影响。**

```go
// syncProgressInterval bounds how often a progress event reaches the webview.
// A full reindex of ~5k items used to emit ~5k events in under a second; the
// frontend could not keep up and the finished event sat behind the backlog.
const syncProgressInterval = 100 * time.Millisecond

func newProgressEmitter(show bool) func(current, total int, path string) {
    last := time.Time{}
    return func(current, total int, path string) {
        if !show {
            return
        }
        // Always let the first and the final item through.
        if current != 1 && current != total && time.Since(last) < syncProgressInterval {
            return
        }
        app := application.Get()
        if app == nil || app.Event == nil {
            return
        }
        last = time.Now()
        app.Event.Emit("library:sync-progress", domain.SyncProgress{
            Current: current, Total: total, Path: path,
        })
    }
}
```

三个循环改成调用它；循环结束后再显式发一次 `current = total`（保证 100%）。

### 4.2 前端：弹窗变成全局组件

- 新建 `frontend/src/stores/librarySync.ts`：
  - 状态：`isSyncing` / `type`（`sync|optimize|deleting`）/ `progress` / `complete`；
  - `init()` 里 `Events.On` 三个事件（`sync-started` / `sync-progress` / `sync-finished`），收集 off 函数并暴露 `dispose()`（`AGENTS.md` §2.6）；
  - `finish()` 后 1.5s 清空 progress/complete；
  - `runOptimize()` / `runSync()` 负责设置 type 并 `await` 绑定调用，**在 `finally` 里兜底清理**（不依赖事件必达）。
- `SyncProgressDialog` 从 `LibrarySettings.vue` 移到 `App.vue`（在 `KeepAlive` 之外），绑定 store。
- `LibrarySettings.vue` 删除本地的 `isSyncing/syncType/syncProgress/syncComplete/backgroundSyncActive` 与三个事件订阅，只保留按钮（调用 store 的方法）和文件夹删除逻辑。
- 这样：切页不会丢状态；弹窗要么全局显示（有意），要么在 finish 后关闭；不存在“teleport 残留”。

### 4.3 顺手修

- `SyncProgressDialog` 的标题改用 store 的 `type`；`handleSyncProgress` 不再擅自把 `type` 置空。
- 若产品上不希望切页时还显示弹窗：在 `onDeactivated`（KeepAlive）里保留数据但不渲染 —— 由全局 store 控制 `visible`，与路由无关。

## 5. 测试

**Go**（`internal/app/library/service_test.go` 或新建）：

- 用假 emitter 记录调用次数：1000 项、无节流应 ~1000 次；加节流后 **≤ 12 次**（100ms 窗口内），且**最后一次一定是 `current == total`**；
- `background` 同步一条都不发（现有行为不能回退）；
- 完成事件与进度事件无依赖：即使进度被节流，`sync-finished` 仍发出。

**前端**（Vitest）：

- store：`sync-started` → `progress` → `finished` 后 `isSyncing=false`、`complete` 1.5s 后清空；
- `runOptimize()` 在绑定 resolve 后即使没有 finish 事件也能清空状态；
- 组件：弹窗随 `open` 打开/关闭；`unmount` 时 off 函数被调用（内存规则）。

## 6. 未证实的环节 / 后续

- 前端**为什么**恰好在 ~3900 处停住（而不是均匀变慢）没有拿到 JS 层证据：Wails v3 自己接管 WebView2 启动参数，`WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS=--remote-debugging-port` 不生效，CDP 连不上。
- 节流（4.1）把每轮事件从 4900 条降到约 5–15 条，理论上足以消除这个症状，且可以单独验证。
- 若节流后仍复现，下一步：用 Wails dev 构建（自带 devtools）或给窗口加 `DevToolsEnabled`，抓 console 异常与主线程堆栈，确认是否有某个事件触发了死循环/长任务。

## 7. 验收

自动：§5 全绿。

人工：

1. 4900 项的「优化搜索」：进度大约每 100ms 跳一次，后端完成后 **1 秒内弹窗关闭**；
2. 同步过程中切到任意页面：不残留弹窗；播放器时间继续正常走动；
3. 「同步音乐库」与后台自动同步行为不变（后台同步不弹窗）；
4. 中途失败（例如文件夹不可读）时，弹窗也能关闭并显示完成态。

## 8. 风险

- 节流后进度粒度变粗（约 10 次/秒），可接受；关键是**完成事件必达**。
- 全局 store 引入新的生命周期代码，要遵守 `AGENTS.md` §2.6（`Events.On` 的 off 函数、`dispose()`）。
- 若将来有其它高频事件（分析进度等）走同一条 FIFO，也应套用同样的节流策略。
