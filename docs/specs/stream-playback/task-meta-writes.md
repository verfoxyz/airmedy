# 任务：降低 stream-cache 元数据写盘频率

> 状态：待开工
> 前置：M2（StreamCache）已完成
> 范围：只改 `internal/app/stream/cache.go` 与其测试

---

## 1. 背景

播放中每 500 ms 上报一次进度（`ReportProgress`），每次都会 `persistMeta`：写临时文件再 `os.Rename` 覆盖。

- 磁盘上每秒产生 2 次写；
- 资源管理器里选中该 `.json` 会被“取消选择”（文件被替换成新对象）；
- 而**淘汰策略本来就会跳过活动 session**，播放期间的落盘进度没有任何读取方。

## 2. 目标

- 播放中**不再**每次进度都写盘；
- 进度只在关键时刻落盘，读取方（`Open`、`enforceLimit`）行为不变；
- 写盘量从“每首每秒 2 次”降到“每首几次”。

## 3. 设计

### 3.1 `ReportProgress`

只更新内存：

```go
func (c *Cache) ReportProgress(session *Session, seconds float64) {
    // update s.progress under lock; no persistMeta here
}
```

### 3.2 落盘时机

在这些位置保留/新增一次 `persistMeta`：

| 时机 | 位置 |
|---|---|
| session 关闭 / 停止 / 硬切歌 | `Close`、`stopSession` |
| 暂停 | 播放器暂停时调用（需要一个新的轻量入口，例如 `ReportProgress` 加 `flush` 参数，或 `FlushMeta(session)`） |
| 下载完成 | `fetch` 结束后 |
| 下载失败 | `fail` |
| 标记 pending promote | 现有标记处 |
| 应用退出 | `Stop` 时对所有活动 session flush 一次 |

### 3.3 兜底（可选）

长时间播放且不暂停时，崩溃会丢失内存中的进度（只影响淘汰排序）。可加：

```go
const progressFlushInterval = 30 * time.Second
// 或 progressFlushDelta = 60 * time.Second（进度变化超过一分钟才写）
```

二选一即可，优先选“时间间隔”，实现最简单。

### 3.4 不做

- 不迁数据库（`stream-cache` 仍是目录 + `<id>.json`）；
- 不改淘汰规则、不改代理、不改 `Open` 的读取逻辑；
- 不处理封面缓存（见 spec §4.5 的独立子任务）。

## 4. 测试

在 `internal/app/stream/cache_test.go`：

1. 播放中多次 `ReportProgress` → `.json` 的 mtime/内容不变（或按兜底间隔才变）；
2. `Close` 后 → `.json` 包含最新进度；
3. 暂停 flush 后 → 进度已落盘；
4. 重新 `Open` → 读回进度，淘汰排序仍生效（构造两个条目，进度不同，断言先删进度小的）；
5. 下载完成/失败路径仍写一次。

回归：

```bash
go test ./internal/app/stream/ ./internal/app/player/
go build ./...
```

## 5. 验收

- 自动：§4 全绿；
- 手工：播放一首流曲目时，`stream-cache` 下对应 `.json` 的修改时间在播放期间基本不变；关闭/暂停后更新。

## 6. 风险

- 进程被强杀时，最多丢失最近一段进度（只影响缓存淘汰的先后，不影响播放或缓存数据有效性）；
- 若将来有别的功能依赖“实时进度”（例如跨设备同步播放位置），需要重新评估——目前没有。
