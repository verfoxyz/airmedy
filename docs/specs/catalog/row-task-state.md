# 任务：搜索行区分「提升」与「下载」状态

> 状态：待开工
> 前置：搜索行状态（`CatalogRowStateResolver`）、提升任务（`promotion_tasks`，见 [downloads-tasks.md](../library/downloads-tasks.md)）已存在
> 范围：搜索结果的 row chip 文案与状态来源；不改保存/播放逻辑

---

## 1. 目标

搜索结果每一行的状态标签（chip）区分当前真正在发生的事：

| 状态 | chip 文案 |
|---|---|
| 已有本地文件 | 已在库 |
| 下载任务 queued/active | **下载** |
| 提升任务 queued/running（且没有活动下载） | **提升** |
| 上次下载失败、无活动任务 | 下载失败 |
| 已注册但空闲 | 已保存 |

即：**正在提升就显示「提升」，正在下载就显示「下载」**，不再统一显示「下载中」。

## 2. 现状

- 行状态来自 `domain.CatalogRowStateResolver`：`in_library` / `downloading` / `failed` / `saved`；
- `internal/app/download/catalog_state.go` 只查下载任务：有活动 job → `downloading`；
- 提升任务目前不参与行状态，所以用户点了提升，搜索行要么没变化、要么显示「已保存」；
- 前端 `CatalogResults.vue` 的 `stateKey()` 把状态映射到 chip；文案 key 在 `cloud_search.*`。

## 3. 后端

### 3.1 新状态

`internal/domain/catalog_search.go`：

```go
// CatalogRowStatePromoting means a promotion task is queued or running for
// this identity. A remote row being moved into the library has no download job,
// so this is distinct from CatalogRowStateDownloading.
CatalogRowStatePromoting = "promoting"
```

判定优先级（从高到低）：

1. 本地行 → `in_library`；
2. 活动下载 job → `downloading`（提升转下载时也走这里）；
3. 活动提升任务（`queued`/`running`）→ `promoting`；
4. 上次下载失败且无活动任务 → `failed`；
5. 其余 remote 行 → `saved`。

### 3.2 查询提升任务

新增窄接口（放在 `internal/app/download` 需要的端口处，实现放 sqlite）：

```go
// ActivePromotions reports which identities currently have a queued or running
// promotion task. Batch by identity: the search page resolves a whole page of
// rows per request, so this must not be one query per row.
ActivePromotions(ctx context.Context, identities []domain.CatalogIdentity) (map[domain.CatalogIdentity]bool, error)
```

- 实现：`SELECT provider, external_id FROM promotion_tasks WHERE state IN ('queued','running')`，按 identity 过滤；
- 表结构见 [downloads-tasks.md](../library/downloads-tasks.md)（迁移 `000095`）；
- 绑定到 download 服务（`CatalogRowStates` 的调用方），不新增循环依赖。

### 3.3 修改 `CatalogRowStates`

`internal/app/download/catalog_state.go` 的循环里，在 `job` 判断之后、`failed` 之前插入：

```go
case promoted[key] && (job == nil || !domain.IsActiveState(job.State)):
    states[key] = domain.CatalogRowStatePromoting
```

（活动下载优先；提升任务在交给下载后会变成 `done`，所以不会同时出现。）

## 4. 前端

### 4.1 `CatalogResults.vue`

- `stateKey()` 增加 `'promoting'`；
- 新增 chip 分支（样式沿用现有 chip）：`data-test="catalog-state"`，文案 `$t('cloud_search.promoting')`；
- 已有 `downloading` chip 的文案改为「下载」（值变化，key 不变）。

### 4.2 i18n（12 语言同改）

- 新增 `cloud_search.promoting`：
  - zh: `提升`，en: `Promoting`
- 修改 `cloud_search.downloading` 的值：
  - zh: `下载`（原「下载中」），en: `Downloading` → `Download`
  - 其它语言同理，取「下载」而非「下载中」
- `in_library` / `saved` / `failed` 不变。

### 4.3 状态刷新

- 提升任务开始/结束时后端 emit `task:updated`（已有约定）；
- 搜索页监听该事件时重新拉取当前页行状态（或与现有 `library:track-updated` 一起节流刷新），保证 chip 从「提升」切到「已在库」。

## 5. 测试

**Go**

- 活动提升 + 无下载 → `promoting`；
- 活动提升 + 活动下载 → `downloading`；
- 已在库 → 不变；
- 提升完成（任务 `done`）→ 不再 `promoting`（落回 `saved` 或 `in_library`）；
- 批量查询不会对每个 identity 各发一条 SQL（可用 count 断言或注入 fake 计数）。

**前端**

- `CatalogResults.spec.ts`：`state: 'promoting'` 渲染 `cloud_search.promoting` chip；
- `state: 'downloading'` 渲染新文案 key（key 不变，断言 key）。

## 6. 验收

- 自动：§5 全绿，`go build ./...` 与 `vue-tsc --noEmit` 通过；
- 人工：
  1. 搜索结果点保存（触发提升）→ 行显示「提升」；
  2. 提升需要转成下载（下载音质更高）→ chip 变为「下载」；
  3. 完成后 → 变为「已在库」；
  4. 普通下载 → 全程显示「下载」。

## 7. 不做

- 不改行的播放/保存按钮行为；
- 不做进度百分比（chip 只显示状态词）；
- 不改「已保存」的含义。

## 8. 风险

- 提升任务与下载任务并发时以下载为准（提升在交给下载后即标记 `done`），需要保证这个顺序，否则会短暂显示「提升」；
- 搜索页若只刷新当前页，跨页的行要等下次搜索才更新（可接受）；
- 12 个语言的 `downloading` 文案变化会影响已有测试里的断言（测试用 key，不受影响）。
