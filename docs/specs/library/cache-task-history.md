# 任务：流缓存也在「下载/任务」里留痕

> 状态：待开工
> 目的：「下载/任务」页同时显示缓存行为（开始/完成/失败），不再只有下载与提升
> 关联：`downloads-tasks.md`（三标签 + 清空）、`task-promotion-worker.md`

---

## 1. 目标

任务页多一种 kind：**缓存**。

- 在线播放一首歌、StreamCache 开始拉流 → 出现一条「缓存」任务（进行中）；
- 缓存完成 → 移入「已完成」；
- 失败（解析失败/网络/磁盘）→ 移入「出错」，带错误信息；
- 缓存命中（秒开、没有实际下载）**不记录**，避免页面被空任务刷屏；
- 淘汰/删除缓存**不记录**。

## 2. 数据

迁移 `000102`：

```sql
CREATE TABLE stream_cache_tasks (
    id           TEXT PRIMARY KEY,
    provider     TEXT NOT NULL,
    external_id  TEXT NOT NULL,
    title        TEXT NOT NULL DEFAULT '',
    artist       TEXT NOT NULL DEFAULT '',
    quality      TEXT NOT NULL DEFAULT '',
    state        TEXT NOT NULL DEFAULT 'active',  -- active|done|failed
    bytes_total  INTEGER NOT NULL DEFAULT 0,
    bytes_done   INTEGER NOT NULL DEFAULT 0,
    error        TEXT NOT NULL DEFAULT '',
    started_at   DATETIME NOT NULL,
    finished_at  DATETIME
);
CREATE INDEX idx_stream_cache_tasks_state ON stream_cache_tasks(state, started_at DESC);
CREATE INDEX idx_stream_cache_tasks_identity ON stream_cache_tasks(provider, external_id);
```

- 每次 **实际的** StreamCache session（非缓存命中）写一行；
- 同一首歌重复播放：每次新 session 一行，便于看到历史；
- 终态保留：与另外两类一致，只保留最近 500 条终态（清理逻辑放在 `ClearTaskHistory` 同处）。

## 3. 写入时机

| 时机 | 动作 |
|---|---|
| session 启动、开始写盘 | 插入 `state=active`、`started_at=now`（标题/艺人/音质来自 track） |
| 下载完成（`complete=1`） | `state=done`、`bytes_done/total`、`finished_at=now` |
| session 失败 | `state=failed`、`error`、`finished_at=now` |
| 应用关闭时仍在 active | 退出前标记 `failed`（error=interrupted），或保留 active 由下次启动清理为 failed |
| 缓存命中（无 session） | 不写 |

实现位置：`internal/app/stream/cache.go` 的 `startSession` / 完成回调 / `fail`，通过一个窄接口写入仓储（与 download/promotion 一样的端口注入），不阻塞播放路径。

## 4. 任务页

- `TaskRun.Kind` 增加 `cache`；
- `ListTasks` 合并三张表：
  - 排序键：`finished_at`（有则）否则 `started_at`，倒序；
  - 状态映射：`active→active`、`done→done`、`failed→failed`；
- 前端 kind 标签：下载 / 提升 / **缓存**；
- 「正在进行」：active 的缓存任务（尝试中/下载中）；
- 「已完成」：完成的缓存；
- 「出错」：失败的缓存（显示错误摘要）；
- 「清空记录」：同样删除缓存的终态行；
- 数量上限与 500 条保留一致。

## 5. 测试

**Go**

- session 启动写 active；完成写 done（字节数正确）；失败写 failed（错误非空）；
- 缓存命中不写；
- 同一首重复播放产生两行；
- `ListTasks` 三表合并、排序、状态映射正确；
- `ClearTaskHistory` 删除缓存的终态行，active 保留；
- 500 条保留策略覆盖缓存任务；
- 退出时 active 行被标记为 interrupted。

**前端**

- 任务页出现「缓存」kind 与三种状态；
- 清空后缓存历史消失，进行中的仍在。

**人工**

1. 播放一首未缓存的歌 → 「进行中」出现缓存任务；
2. 播完 → 移到「已完成」；
3. 播放一首无链接的歌 → 「出错」有记录；
4. 缓存命中（再播已完整缓存的歌）→ 不新增任务。

## 6. 验收

- 自动：§5 全绿；
- 人工：§6 四条成立。

## 7. 风险

- 任务页条数会明显变多（每次播放都可能产生），需要保留上限与节流；
- 缓存命中判定必须准确，否则会产生大量 `done` 噪音；
- 写库发生在播放路径上：只做一次 insert/update，失败仅记日志，不影响播放。
