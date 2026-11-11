# 任务：搜索行只保留「已在库中」与「已创建任务」两个 chip

> 状态：待开工
> 前置：搜索行状态（`CatalogRowStateResolver`）、提升任务（`promotion_tasks`）已存在
> 本篇取代 `row-task-state.md` 早期版本里「提升 / 下载」分开显示的方案

---

## 1. 目标

搜索结果每一行最多只显示两个状态 chip：

| 状态 | chip 文案 |
|---|---|
| 本地已有文件 | **已在库中** |
| 存在排队/执行中的任务（下载**或**提升） | **已创建任务** |
| 上次失败且无活动任务 | 下载失败（保留） |
| 已注册但空闲（原 `saved`） | **不显示 chip** |
| 其它（无状态） | 不显示 |

即：

- 删除「已保存」chip；
- 删除「下载中」文案；
- **不再区分**「下载 / 提升」，排队或执行中统一显示「已创建任务」；
- `in_library` 文案由「已在库」改为「已在库中」。

## 2. 现状

- 后端状态：`in_library` / `downloading` / `failed` / `saved`（提升任务目前不参与行状态）；
- 前端 `CatalogResults.vue` 的 `stateKey()` 映射 `in_library` / `saved` / `downloading` / `failed`；
- 文案 key：`cloud_search.in_library`（已在库）、`downloading`（下载中）、`saved`（已保存）、`failed`（下载失败）。

## 3. 后端

### 3.1 状态取值

保持四个后端状态不变（便于 resolver 与任务页统计），但如果提升任务要参与判断：

- 有活动下载任务 → `downloading`；
- 无下载、有排队/执行中的提升任务 → 也返回 `downloading`（**不再返回独立的 `promoting`**，因为 UI 不区分）；
- 无任务 → `failed` 或 `saved`。

> 若此前实现已引入 `CatalogRowStatePromoting`，可以保留该常量但 resolver 不返回它；或直接删除，二者取其一，保持一致即可。

### 3.2 查询

`CatalogRowStates` 需要在下载任务判断之外补一次提升任务查询（批量）：

```go
ActivePromotions(ctx, identities) (map[domain.CatalogIdentity]bool, error)
```

- 数据来自 `promotion_tasks`（`queued`/`running`，迁移 `000095`）；
- 命中后与 `downloading` 合并，不再区分来源；
- 批量查询，避免逐行 N+1。

## 4. 前端

### 4.1 `CatalogResults.vue`

- `stateKey()` 只返回 `'in_library' | 'downloading' | 'failed' | null`；
- `saved` → `null`（不渲染 chip，也不渲染 blocked 文案）；
- `downloading` chip 文案改为「已创建任务」；
- `promoting`（若后端仍可能返回）按 `downloading` 渲染，同一文案；
- `in_library` 文案改为「已在库中」。

### 4.2 i18n（12 语言同改）

| key | 新值（zh） | 备注 |
|---|---|---|
| `cloud_search.in_library` | `已在库中` | 原「已在库」 |
| `cloud_search.downloading` | `已创建任务` | 原「下载中」 |
| `cloud_search.saved` | 保留 key，不再使用 | 可保留英文/旧值 |
| `cloud_search.promoting` | 若存在则改为 `已创建任务` | 或删除 |

英文对应：`In Library` / `Task created`。

## 5. 测试

**Go**

- 活动下载 → `downloading`；
- 活动提升（无下载）→ `downloading`（不再返回 `promoting`）；
- 已注册无任务 → `saved`；
- 本地文件 → `in_library`；
- 批量查询不会逐行发 SQL。

**前端**

- `stateKey` 对 `saved` 返回 null（不渲染 chip）；
- `downloading`/`promoting` 渲染同一文案 key；
- `in_library` 渲染新文案 key；
- 现有 chip 测试同步更新。

**人工**

1. 保存一首未播放过的歌 → 出现「已创建任务」（不显示「已保存」）；
2. 在线播放后保存 → 显示「已创建任务」，完成后「已在库中」；
3. 本地曲目 → 「已在库中」；
4. 失败 → 「下载失败」。

## 6. 验收

- 自动：§5 全绿；
- 人工：§5 四条成立；
- 搜索页不再出现「已保存」「下载中」文案。

## 7. 风险

- 不再区分下载与提升后，用户无法从搜索行判断任务类型；任务类型在「下载/任务」页可见；
- `saved` 行没有 chip 后，用户可能误以为行没有动作；保存按钮态仍显示已保存；
- 12 语言文案必须同步，否则部分语言仍显示旧词。
