# 任务：把「下载」改为「下载/任务」页（进行中 / 已完成 / 出错 + 清空记录）

> 状态：待开工
> 前置：`/downloads` 页面已存在（`DownloadsView.vue`）；下载任务已保留历史（`download_jobs`，迁移 `000086`）
> 联动：[task-promotion-worker.md](../stream-playback/task-promotion-worker.md)（提升任务也写入本页的历史表，见 §4.2）

---

## 1. 目标

1. 侧边栏与页面标题由「下载」改为「下载/任务」；
2. 页面分三个子标签：
   - **正在进行**：`queued` / `active` 的下载 + 排队/执行中的提升；
   - **已完成**：`done` 的下载 + 成功完成的提升；
   - **出错**：`failed` / `canceled` 的下载 + 失败的提升；
3. **所有下载与提升的记录（历史）**都在这三个标签里显示，而不是只显示未完成的队列；
4. 「清空记录」按钮：清空已完成/出错的历史，**二次确认**；正在进行的不受影响。

## 2. 数据来源

| 类型 | 存储 | 说明 |
|---|---|---|
| 下载 | `download_jobs`（已有） | 已完成/失败/取消的行已经保留（`000086`） |
| 提升 | 新表 `promotion_tasks` | 新增，记录每次提升的发起、执行、结果 |

两者在应用层合并成统一的 `TaskRun` DTO，页面不关心来源。

### 2.1 迁移 `000095`

```sql
CREATE TABLE promotion_tasks (
    id           TEXT PRIMARY KEY,
    track_id     TEXT,
    provider     TEXT NOT NULL DEFAULT '',
    external_id  TEXT NOT NULL DEFAULT '',
    title        TEXT NOT NULL DEFAULT '',
    artist       TEXT NOT NULL DEFAULT '',
    state        TEXT NOT NULL DEFAULT 'queued',   -- queued|running|done|failed
    quality      TEXT NOT NULL DEFAULT '',
    error        TEXT NOT NULL DEFAULT '',
    created_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    started_at   DATETIME,
    finished_at  DATETIME
);
CREATE INDEX idx_promotion_tasks_state ON promotion_tasks(state, created_at DESC);
```

> 迁移编号约定：`000094` 归 [task-promotion-worker.md](../stream-playback/task-promotion-worker.md)（`stream_cache.promoted` / `discard_pending`），本任务的表用 `000095`，两者互不冲突。

## 3. 后端

### 3.1 统一 DTO 与查询

新增 `internal/app/tasks`（或放在 download 服务里，二选一，实现时取最小依赖）：

```go
type TaskRun struct {
    ID         string    // job id / promotion task id
    Kind       string    // "download" | "promote"
    State      string    // active|done|failed|canceled
    TrackID    string
    Title      string
    Artist     string
    Quality    string
    Error      string
    CreatedAt  time.Time
    StartedAt  *time.Time
    FinishedAt *time.Time
}

// ListTasks returns every run, newest first; the UI groups by state.
func (s *TaskService) ListTasks(ctx context.Context) ([]TaskRun, error)

// ClearTaskHistory deletes finished runs (done/failed/canceled) from both
// tables and emits task:updated. Active rows are never touched.
func (s *TaskService) ClearTaskHistory(ctx context.Context) error
```

- 下载侧：把 `download_jobs.state` 映射为 `active|done|failed|canceled`；
- 提升侧：`promotion_tasks.state` 同上；`queued|running` → `active`；
- 排序：`finished_at`（有则用）否则 `created_at`，倒序；
- 保留上限：两张表都只保留最近 500 条终态记录（插入/清理时修剪），避免无限增长。

### 3.2 提升 worker 写入历史

见 [task-promotion-worker.md](../stream-playback/task-promotion-worker.md) §4.2：发起时写 `queued`，执行时 `running`，成功 `done`，失败 `failed`（带 error）。本页直接读这张表，不再另存一份。

### 3.3 Wails API / 事件

- `TaskService.ListTasks()`；
- `TaskService.ClearTaskHistory()`；
- 事件 `task:updated`：下载/提升任一状态变化时发出，页面收到后刷新。

## 4. 前端

### 4.1 页面

- 路由仍是 `/downloads`；标题改为「下载/任务」；
- 三个标签（Tabs，参考现有 Radix/Tailwind 组件）：`正在进行` / `已完成` / `出错`，各自显示数量；
- 列表复用 `DownloadQueueItem` 的行视觉，但按 `kind` 显示「下载 / 提升」标签；已完成/出错的提升行显示质量、耗时或错误信息；
- 空状态按标签分别显示；
- 顶部右侧「清空记录」按钮（`Button` 组件，危险色）：点击打开 `ConfirmDialog`，确认后调用 `ClearTaskHistory()`，完成后刷新并提示；
- 「正在进行」里的下载行保留取消，提升行只读（不做取消提升 UI）。

### 4.2 侧边栏

- 文案改为「下载/任务」：`sidebar.download` 的 **值** 改为 `下载/任务`（en: `Downloads & Tasks`），key 不变，避免绑定重生成；
- 入口仍在「设置」上方。

### 4.3 i18n（12 语言同改）

```
sidebar.download                 -> 下载/任务
tasks.title                      -> 下载/任务
tasks.tab_active                 -> 正在进行
tasks.tab_done                   -> 已完成
tasks.tab_failed                 -> 出错
tasks.kind_download              -> 下载
tasks.kind_promote               -> 提升
tasks.empty_active               -> 当前没有正在进行的任务
tasks.empty_done                 -> 还没有已完成的任务
tasks.empty_failed               -> 没有出错的任务
tasks.clear                      -> 清空记录
tasks.clear_title                -> 清空任务记录？
tasks.clear_message              -> 将删除所有已完成和出错的记录，正在进行的任务不受影响。
tasks.clear_confirm              -> 清空
tasks.clear_cancel               -> 取消
tasks.clear_done                 -> 已清空记录
```

## 5. 测试

**Go**

- 合并列表：下载 + 提升，按时间倒序，状态映射正确；
- `ClearTaskHistory` 只删终态（done/failed/canceled），`queued/active` 保留；
- 保留上限：超过 500 条时删最旧；
- 迁移 `000095` up/down；
- 事件 `task:updated` 在下载完成/失败、提升完成/失败时发出。

**前端**

- 三个标签的切换、数量、空状态；
- 清空按钮：弹确认、取消不清空、确认后调用 API 并刷新；
- 提升行显示 kind 标签与错误信息。

## 6. 验收

- 自动：§5 全绿；
- 人工：
  1. 下载一首 → 出现在「正在进行」，完成后进入「已完成」；
  2. 提升一首 → 播放结束后出现在「已完成」，失败时进入「出错」；
  3. 清空记录 → 二次确认 → 「已完成/出错」清空，「正在进行」保留；
  4. 重启应用后历史仍在。

## 7. 不做

- 不做日志文件导出/查看；
- 不做按曲目筛选、搜索历史；
- 不做提升任务的取消。

## 8. 风险 / 待确认

- **清空下载历史**会不会影响「已在库/下载失败」这类徽章？实现时确认 `CatalogRowStateResolver` 不依赖 `done` 行；若依赖，清空时只清 `promotion_tasks` 与 `canceled/failed` 下载行，`done` 行保留；
- 提升任务的 `title/artist` 使用发起时的快照，曲目后续改名不会回填（可接受）；
- 500 条上限是初始值，后续可做成设置项。
