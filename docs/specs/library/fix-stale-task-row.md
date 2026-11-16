# Bug：任务页停在「下载中…」，行仍显示合并前的旧 track id

> 状态：待开工
> 现象：`All Eyes On Me` 的任务行显示「下载中…」和旧 id `fdf69ffd…`；实际 `media_requests` 已是 `done`、文件已入库
> 范围：任务行状态刷新与身份/标题快照；不涉及下载器

---

## 1. 证据

- `media_requests`：`desired=promote`、`state=done`、`track_id=4a208680…`（本地行），16:25:42 完成；
- 曲目行：`4a208680…` 本地 ready；旧的 `fdf69ffd…`（remote）已不存在；
- 界面：仍显示 `fdf69ffd…` + 「下载中…」。

结论：**后端已完成，前端任务页拿着合并前的旧快照**。

## 2. 根因

1. 提升/合并会把请求的 `track_id` 从 remote 行改成本地行（或反过来），行被删除；
2. `ListTasks` 用 `request.track_id` 反查标题；前端把整行快照按旧 id 缓存；
3. 请求状态变化后：
   - 若 `task:updated` 没发/没到，前端不会重新拉取；
   - 即使重新拉取，若行 id 已变，旧行不会按 id 合并替换；
4. 任务页没有“可见时刷新”的兜底。

## 3. 修法

### 3.1 后端：请求携带身份与标题快照

迁移 `000107` 给 `media_requests` 增加：

```sql
ALTER TABLE media_requests ADD COLUMN title TEXT NOT NULL DEFAULT '';
ALTER TABLE media_requests ADD COLUMN artist TEXT NOT NULL DEFAULT '';
```

- 创建请求时写入标题/艺人快照；
- track_id 失效时，`ListTasks` 用快照显示，不再回退成裸 id；
- 同时返回 `provider/external_id`，前端按 identity 合并行。

### 3.2 后端：track_id 变化时发事件

- 提升/合并更新 `request.track_id` 后立即 `Emit("task:updated")`；
- `ListTasks` 返回 `request_id`（稳定键），前端用它作为行 key。

### 3.3 前端：按 request_id 合并 + 可见时刷新

- `tasks` store 的列表 key 用 `request_id`；
- `task:updated` 时整表 `hydrate()`（已是现状，补上后端事件即可）；
- 任务页 `onMounted`、路由 `onActivated`、窗口 `focus` 时各刷新一次；
- 行不显示裸 track id，改为标题 + 状态；需要 id 时用 request_id。

### 3.4 防御

- 前端拿到的行若 `track_id` 查不到（陈旧），仍按 request_id 展示并等待下一次 hydrate；
- 不做“自动删除行”，避免丢历史。

## 4. 测试

**Go**

- 创建请求写入 title/artist 快照；
- 合并更新 track_id 后 emit `task:updated`；
- `ListTasks` 在 track_id 指向已删除行时仍返回标题快照与稳定 request_id。

**前端**

- 合并后重新 hydrate：同一行从「下载中」变为「已完成」，id 不会出现两个；
- 页面重新可见时触发刷新；
- 行 key 为 request_id，事件更新不产生重复行。

**人工**

1. 播放并提升一首在线曲目；
2. 任务页该行从「提升中」自动变为「已完成」，不出现旧 id / 卡住状态；
3. 切走再回来、重启后状态一致。

## 5. 验收

- 自动：§4 全绿；
- 人工：§5 三条成立。

## 6. 风险

- 新增快照列后，历史请求 title 为空，需要回退到 track 反查（现有逻辑）；
- 频繁 emit `task:updated` 可能导致任务页抖动，事件可做 300ms 合并；
- 前端 key 从 track_id 改为 request_id 需要同步测试快照。
