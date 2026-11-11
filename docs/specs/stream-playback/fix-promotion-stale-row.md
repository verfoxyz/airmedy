# Bug：提升完成后搜索行仍显示「下载中」

> 状态：待开工
> 现象：在线播放过、保存后已提升入库（文件已移动、缓存已删除），搜索行仍一直显示「下载中」
> 关联：[fix-save-prefers-promotion.md](fix-save-prefers-promotion.md)、[row-task-state.md](../catalog/row-task-state.md)

---

## 1. 现象与证据

以 `Compute It With Some Devilish Alcoholic Steampunk Engines - かめりあ` 为例：

- `tracks` 行已是本地文件（`C:\Users\vervea\Music\Airmedy\...flac`），identity 保留；
- `download_jobs` **没有任何记录**，日志中也没有 `download: enqueued` / `download: finished`；
- 只有一条 `download: imported row replaced the remote one`（20:37:24）→ 说明是**提升移动**（`PromoteCachedFile` → finalize → import），不是下载；
- 搜索行 chip 却停在「下载中」。

## 2. 根因

1. 前端 `catalog` store 只在 `download:done` / `download:failed` 事件里重读行状态；
2. **提升完成不发这两个事件**，只发 `task:updated`（提升历史）和 `library:track-updated`（导入行更新）；
3. `catalog` store 没有监听 `task:updated` / `library:track-updated`，所以 chip 永远不会从「下载中」纠正为「已在库」；
4. 同时保存动作的返回值对“只创建了提升任务”的路径返回 `saved`（而不是 `promoting`），一旦前端此前已把状态设为 `downloading`（旧构建或上一轮保存），就没有任何路径再刷新它。

结果：行状态是“写入后只读”，提升完成没有事件能触发重读。

## 3. 修法

### 3.1 后端事件

- `PromoteCachedFile` 成功（移动 + import + merge）后，除现有 `task:updated` 外，确保发出：
  - `task:updated`（提升历史完成）；
  - `library:track-updated`（本地行更新）——已有导入路径会发，确认提升路径复用同一处；
- 失败时发 `task:updated`（状态 failed），行状态仍由查询决定。

> 不新增第三套事件；复用现有两个。

### 3.2 前端 `catalog` store

新增监听（与现有 `download:done` / `download:failed` 并列）：

```ts
const _offTask = Events.On('task:updated', () => void refreshVisibleRows())
const _offTrackUpdated = Events.On('library:track-updated', (ev) => {
  const trackId = (ev.data as TrackDTO | null)?.id
  if (trackId) void refreshRow(trackId)
})
```

- `refreshVisibleRows()` 对当前页可见行按 identity 批量调用 `GetCatalogRowStates`（一次请求批量，避免 N 次）；
- 节流：300–500 ms 合并同一批事件，避免提升/下载并发时抖动；
- `dispose()` 释放两个新的 off-fn。

### 3.3 保存返回值与 chip

- `OnRemoteTrackSaved` 在“有缓存、只创建提升任务”分支返回 `CatalogRowStateDownloading`（对外统一表示“已创建任务”，见 row-task-state 文档）；
- `CatalogResults.vue` 的 `downloading` chip 文案统一为「已创建任务」；
- 保存后立即显示「已创建任务」，事件到达后刷新为「已在库中」。

## 4. 测试

**Go**

- 提升成功路径 emit `task:updated` 与 `library:track-updated`（用 fake event sink 断言）；
- 提升失败路径 emit `task:updated`（failed）；
- `OnRemoteTrackSaved` 缓存命中返回 `downloading`（统一任务态）；
- 下载完成路径仍 emit `download:done`（回归）。

**前端**

- `catalog.spec.ts`：触发 `task:updated` / `library:track-updated` 后，可见行状态被重读（mock `GetCatalogRowStates` 返回 `in_library`，断言 chip 变化）；
- 事件节流：连续多次事件只发一批查询；
- `state: 'promoting'` 渲染 `cloud_search.promoting` chip；
- `dispose()` 后事件不再触发查询（防泄漏）。

**人工**

1. 在线播放一首 → 保存 → chip 显示「已创建任务」→ 完成后变为「已在库中」；
2. 下载路径 → chip 从「下载」变为「已在库」；
3. 提升失败 → chip 变为「已保存」或「下载失败」并可在任务页看到错误；
4. 页面停留 5 分钟不重新搜索，状态也能自动更新。

## 5. 验收

- 自动：§4 全绿；
- 人工：§4 四条成立；
- 不再出现“文件已入库、缓存已删，chip 还显示下载中”的情况。

## 6. 风险

- 批量刷新要按 identity（合并后 track id 会变），不能按 track id；
- 事件风暴（多个提升同时完成）需要节流，否则每次事件都查一页；
- `library:track-updated` 在导入/元数据编辑时也会发，刷新需只针对可见行，避免全量查询。
