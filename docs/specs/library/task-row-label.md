# 任务：任务页的下载行显示真实曲名 + 浅色 ID

> 状态：待开工
> 现象：「下载/任务」里已合并的下载任务只显示 UUID（如 `ebdbf6a9-a70c-4caf-94af-67a13bd0a7b7`），看不到曲名

---

## 1. 现象与根因

- 下载任务在 enqueue 时只存 `track_id`，不存曲名快照；
- 下载完成合并后，remote 行被删除（`imported row replaced the remote one`），`download_jobs.track_id` 指向的行已不存在；
- `ListTasks` 关联 `tracks` 取标题 → 空 → 前端回退显示 `track_id`。

提升任务不受影响：`promotion_tasks` 自带 `title` / `artist` 快照。

## 2. 目标

任务行的主标签显示**真实曲名**，后面跟一个**浅色的 ID**：

```
QuomodocunquizE                      ebdbf6a9-a70c-4caf-94af-67a13bd0a7b7
かめりあ
```

- 主标签：正常颜色；
- ID：`text-dimmer`、等宽/更小字号；
- 曲名与 ID 之间留间距，ID 过长可截断（`truncate`）但保留可复制能力（可选 `title` 提示）。

## 3. 曲名解析（后端，`ListTasks`）

按顺序取第一个非空值：

1. `tracks.title`（任务关联的 track 仍存在）；
2. `promotion_tasks.title`（提升任务自带快照）；
3. **从 `download_jobs.target_path` 推导**（合并后最常见的路径）：
   - 取文件名，去掉扩展名；
   - 按当前 `download_naming_order` 设置拆分：
     - `title-artist`（默认）→ ` - ` 之前是标题；
     - `artist-title` → ` - ` 之后是标题；
   - 文件名不含分隔符时，直接使用整个文件名；
   - 推导失败返回空；
4. 仍为空 → `track_id`。

艺人同理：

1. `tracks.artists` / `raw_artist_names`；
2. `promotion_tasks.artist`；
3. 从文件名推导（命名顺序的反方向部分）；
4. 空 → 不显示。

> 不新增迁移：全部在读取时解析；历史任务同样受益。

## 4. 前端

- `DownloadsView` / `DownloadQueueItem`（或任务行组件）：
  - 主标签 = `title || track_id`；
  - 副标签（浅色）= `track_id`（仅当主标签不是 ID 时显示；若主标签就是 ID，避免重复）；
  - 艺人显示在标题下方（已有样式）；
- 提升任务同样显示「曲名 + 浅色 ID」；
- 12 语言不需要新 key（不新增文案）；
- 右侧状态、错误信息、取消/重试按钮位置不变。

## 5. 测试

**Go**

- 合并后任务（track 已删）：标题从 `target_path` 推导，命名顺序两种都覆盖；
- 文件名无分隔符：整段作为标题；
- 任务关联的 track 仍存在：优先用真实标题；
- 提升任务：用 `promotion_tasks.title`；
- 全部为空：回退 `track_id`；
- `target_path` 为空：不影响其它字段。

**前端**

- 主标签显示曲名，副标签为浅色 ID；
- 主标签等于 ID 时不重复渲染副标签；
- 长 ID 截断不撑破布局。

**人工**

1. 打开「下载/任务」→ 已完成的下载显示曲名 + 浅色 ID；
2. 提升任务同样；
3. 找不到曲名的旧任务仍显示 ID（不空白）。

## 6. 验收

- 自动：§5 全绿；
- 人工：§5 三条成立；
- 任务页不再出现“只有一串 UUID”的行。

## 7. 风险

- 从文件名推导标题依赖命名顺序设置，历史任务可能与该设置不一致；推导结果只用于展示，不写库；
- 文件名被用户手动改过时可能不准，但比 UUID 可读；
- 若将来给 `download_jobs` 加标题快照列，可移除该回退逻辑（本期不做）。
