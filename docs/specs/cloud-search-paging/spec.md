# Spec：云端搜索分页（5 + 10）与封面懒加载

> 状态：待开工
> 取代：`fix-cover-trigger.md`（frontier 分批）与 `fix-search-covers.md` §4.2/§4.3 的配额+哨兵机制；
> `cover_batch`、`max_display` 两个插件设置一并删除。
> 后续修订：单源没有「加载更多」的 hasMore 判定见 [`fix-single-source-hasmore.md`](fix-single-source-hasmore.md)。
> 约束：AI 不做图形化验证；观感由人工确认。
> 抓取参考：`any-listen-extension-online-metadata`（kg 封面 `src/onlineResource/kg/pic.ts`）——见 §6。

---

## 1. 目标

1. 云端搜索结果**首屏 5 行**（多源合并后的总数）。
2. 点「加载更多 ⬇️」**每次增加 10 行**，直到取完或达到上限。
3. 删除封面"按批加载"机制：不再有每源配额、frontier 行、批次计数器。
4. **图片懒加载**：行进入视口（提前 200px）才向宿主请求封面；没进入视野的行不发请求。
5. 删除插件设置：`cover_batch`（封面批次）、`max_display`（多源合并后的显示条数）。

不在范围：本地搜索页、排序算法本身、封面缓存存储形态、视觉改版、播放/下载流程。

---

## 2. 语义（先定死）

| 概念 | 定义 |
|---|---|
| 首屏 | `limit=5, offset=0` |
| 第 n 次「加载更多」 | `limit=10, offset=当前已显示行数` |
| `limit`/`offset` | 都是**合并后**的行数，不是每源行数 |
| 每源取数 | 窗口 `offset+limit` 行（`offset=0`），合并/排序后切 `[offset, offset+limit)` |
| 上限 | `offset+limit ≤ 100`（`MaxCatalogSearchPageSize` 从 50 放宽到 100）；到上限后 `hasMore=false`，按钮隐藏 |

这样顺序稳定：同一 query 下，第 2 页取的是同一排序结果的下 10 行，不会因为"每源翻页"而重复或漏行。

---

## 3. 宿主改动

### 3.1 分页

`internal/app/catalog/service.go`：

- `Search()`：**删除** `maxDisplay` 截断；`rankMultiSource` 调用不再传上限；
- `searchSources()` 已经按 `offset+limit` 取窗口（`Offset: 0`），保持；只需确认上限用它来裁剪；
- `pageOf(ranked, offset, limit)` 保持不变（单次切片）；
- `hasMore`：`pageOf` 返回值即可（窗口内已覆盖到尾部时自动 false）。

`internal/app/catalog/ranking.go`：

- `rankMultiSource(query, sourceOrder, results)`：去掉 `maxDisplay int` 参数与末尾的 `merged = merged[:maxDisplay]`；tie-break 规则不变（保持跨页稳定）。

`internal/domain/catalog_search.go`：

- 删除 `CatalogSettingMaxDisplay`、`DefaultCatalogMaxDisplay`、`MinCatalogMaxDisplay`、`MaxCatalogMaxDisplay`；
- 删除 `CatalogSettingCoverBatch`、`DefaultCatalogCoverBatch`、`Min/MaxCatalogCoverBatch`；
- 删除 `CatalogSearchPage.CoverBatch`；
- `MaxCatalogSearchPageSize` 保留但**从 50 放宽到 100**，作为合并窗口上限。

> 放宽的原因：插件删掉了 `max_display`，合并后的长度不再由"显示条数"设置控制，而是由分页窗口决定。
> 100 是折中——kg 上游单页最多 30 条（实测，见 §6），窗口 100 时每源最多 4 个请求；
> 若以后要 200，把本常量与插件 `MAX_LIMIT` 同时提高即可，分页逻辑不用改。

`internal/app/catalog/service.go`：删除 `maxDisplay()` 与 `coverBatchValue()`。

`internal/infra/wails/catalog_service.go`：

- `CatalogSearchResultDTO` 删除 `CoverBatch`；
- 重新生成 bindings（它会改 `frontend/bindings/**`，不要手改）。

### 3.2 封面

- `Covers()` / `GetCatalogCovers` / `covercache` **全部不变**：仍然"给定一行 → 下载/命中缓存 → 返回本地 `/cover-cache/<hash>`"。
- 区别只在**调用时机**：从前端"每批 5 行"变成"每行进入视口时"。

---

## 4. 插件改动

`plugins/wy/wrapper.js` 的 `SETTINGS`：

- 删除 `cover_batch`、`max_display` 两项；
- `cover_cache_mb` 保留。

**放宽单次取数上限**

- `plugins/wy/src/search.js`：`MAX_LIMIT` 从 50 提到 **100**（注释里"Keep in step with domain.MaxCatalogSearchPageSize"同步）。
- **kg 必须自己翻页**：上游 `songsearch.kugou.com` 实测单页最多返回 **30 条**（`pagesize=30/50/100` 都只回 30）。
  现在 `searchKG` 只请求一页，所以窗口超过 30 时 kg 永远补不满。需要：
  - 按 `page` 循环（每页 30），直到凑够 `limit` 或 `hasMore=false`；
  - 逐页 map，按 `externalId` 去重；
  - 封面仍只对本页集合做一次 `get_res_privilege`（或按页分别取，合并后统一 `applyKugouCovers`）；
  - 循环有上限（`Math.ceil(limit / 30)` 次），不使用共享预算之外的额外超时。
- wy 实测 `limit=100` 正常返回 100 条，不需要翻页。

`plugins/wy/test/wrapper.test.js`：同步断言（设置列表不再包含这两项）。

`plugins/wy/test/search.test.js`：新增 kg 翻页用例（伪 fetch 每页返回 30 条、断言请求次数与去重）。

`plugins/wy/README.md` 与 `catalog/plugins/README.md`：设置表、`catalog.search` 的 `limit/offset` 说明同步。

已存库的旧设置值不再被读取；宿主按 key 读取设置，缺失即用默认值（`internal/app/plugin/settings.go` 行为不变）。

---

## 5. 前端改动

### 5.1 store（`frontend/src/stores/catalog.ts`）

- 新增 `PAGE_INITIAL = 5`、`PAGE_STEP = 10`，替换 `PAGE_SIZE = 50` 的用法；
- `run(reset)`：`limit = reset ? PAGE_INITIAL : PAGE_STEP`；`offset = reset ? 0 : rows.value.length`；
- 删除 `coverBatch` 状态与 `page.cover_batch` 读取；
- `hasMore` 直接取 `page.has_more`。

### 5.2 `CatalogResults.vue`

**删除**（分批相关全部）：

- `coverLimits` / `coverLimit()` / `batchSize()` / `COVER_BATCH_FALLBACK`；
- `frontierIndex` / `frontierRowEl()` / `advanceCoverBatch()` / `advanceOnce()`；
- 封面版的 `hasPendingCovers()`（保留"行是否有封面"的判断即可）；
- `cover_batch` prop 与 `SearchView.vue` 的传参。

**保留**：

- `loadedCovers` / `failedCovers` / `requestedCovers`（去重）、图片预加载队列（≤4 并发）、失败占位、淡入。

**新增：行级懒加载**

- 一个 `IntersectionObserver`，`root = findScrollParent(rootEl)`（找不到传 `null`），`rootMargin: '0px 0px 200px 0px'`；
- 观察**每一行**（`[data-row-index]` 元素）；
- 行进入视口且 `row.cover_url && row.external_id` 且未请求/未失败/未加载 → 收进 `pendingRows`；
- 同一轮（`nextTick` / 微任务）把 `pendingRows` 合成**一次** `GetCatalogCovers` 调用（每次最多 10 行，剩余的下一轮继续）；
- 请求过的行 `unobserve`，不再参与观察；
- `rows` 变化（追加新页）后对新行重新 `observe`；`onUnmounted` 时 `disconnect()`。

伪代码：

```ts
function lazyRequest(row: CatalogRowDTO, el: Element) {
  const key = coverKey(row)
  if (!row.cover_url || !row.external_id) return
  if (requestedCovers.has(key) || failedCovers.has(key) || loadedCovers.has(key)) return
  requestedCovers.add(key)
  pendingRows.push(row)
  coverObserver?.unobserve(el)
  scheduleFlush()
}
```

`scheduleFlush()` 合并同一帧内的可见行，调用 `CatalogService.GetCatalogCovers`，成功后把 URL 推进图片队列。

### 5.3 `SearchView.vue`

- 删除 `:cover-batch`；
- 「加载更多」文案可加 `⬇️`（i18n key 已有 `cloud_search.load_more`，12 个语言文件同改）。

---

## 6. 抓取参考：any-listen-extension-online-metadata

抓取策略**继续沿用**该项目的做法，不在本轮改动：

- kg 搜索页本身不带封面，插件对该页的 hash 发**一次** `POST media.store.kugou.com/v1/get_res_privilege`；
- 一条坏 hash 会让整批 `status:0`，失败时按单条重试（共享预算）；
- `cover_url` 取 `info.image` 的 240；`cover_url_large` 取 `info.imgsize` 里的 400/480；
- 抓取失败静默，不影响搜索。

参考实现的事实（2026-11-07 核对本地 `reference/any-listen-extension-online-metadata`）：

| 项 | 参考实现 | 我们 |
|---|---|---|
| kg 封面取图 | `onlineResource/kg/pic.ts` 的 `getPic()` **一次 1 张**，`resource` 只有一项；扩展不做并发控制，由宿主逐首调用 | 插件对整页 hash 一次 POST（`get_res_privilege` 支持数组），请求数更少 |
| 封面并发 | 无（1 次 1 张），并发度在宿主程序里 | 宿主下载队列 **≤4** 并发 |
| 批量 | 只有歌曲详情 `musicDetail.ts` 才批量：`maxBatchSize = 100`、`Promise.all`、`maxRetryNum = 3` | 搜索窗口 100 行（`MaxCatalogSearchPageSize`） |

**上游单页上限（2026-11-07 实测）**：

| 源 | 请求 limit | 实际返回 | 说明 |
|---|---|---|---|
| kg | 30 / 50 / 100 | 30 / 30 / 30 | 上游固定 30 条/页，只能靠 `page` 翻页 |
| wy | 30 / 50 / 100 | 30 / 50 / 100 | 单次可取到 100 |

所以"窗口 100"对 kg 来说是 4 次上游请求（30×4），插件内部循环补齐，宿主只看到一次 `catalog.search`。

即：**插件负责"这一页有哪些封面 URL"，宿主负责"用户看到了才下载"**。本轮只改后者的时机，不改前者的实现。若后续要新增源或调整抓取，以该项目的对应实现为参照，不另起一套。

---

## 7. 测试

### 7.1 Go

`internal/app/catalog/service_test.go`：

- 首屏 `limit=5, offset=0` → 返回 5 行（跨两源合并后），`HasMore=true`；
- `limit=10, offset=5` → 返回第 6–15 行，且与上一页无重复；
- 请求到 100 行上限后 `HasMore=false`；
- 删除 `max_display` 截断相关用例（原本断言合并后最多 20 行）。

`internal/app/catalog/ranking_test.go`（若有）：`rankMultiSource` 不再截断。

`internal/infra/wails/catalog_service_test.go`：DTO 不再有 `cover_batch` 字段。

### 7.2 前端

`frontend/src/stores/catalog.spec.ts`：

- 首次搜索请求 `limit=5, offset=0`；
- `loadMore` 请求 `limit=10, offset=5`，再点一次 `offset=15`；
- 新搜索重置为 5；
- 删除 `cover_batch` 透传用例。

`frontend/src/components/catalog/CatalogResults.spec.ts`：

- 伪 `IntersectionObserver`：行进入视口 → 只对**该行**调用 `GetCatalogCovers`；
- 未进入视口的行不调用；
- 同一行只请求一次（重复触发不重复调用）；
- 追加新行后被观察；
- 卸载后 `disconnect()`；
- 删掉所有批次/配额/frontier 用例。

### 7.3 插件与文档

```bash
cd plugins/wy && node --test
node scripts/check-docs.mjs
pnpm --dir frontend exec vue-tsc --noEmit
pnpm --dir frontend exec vitest run src/stores/catalog.spec.ts src/components/catalog/CatalogResults.spec.ts
go build ./... && go test ./internal/... .
```

---

## 8. 验收

自动：§7 全绿。

人工：

1. 搜任意关键词：首屏 5 行；
2. 点「加载更多 ⬇️」：每次 +10 行，顺序连续、无重复；
3. 慢速下滚：行进入视野才出图，没看到的行不请求（可看 `cover-cache/` 新文件增长）；
4. 到 100 行或结果取完：按钮消失；
5. 设置页：不再出现「封面批次」「多源合并后的显示条数」。

---

## 9. 风险与假设

- **窗口重取**：每次翻页都会重取 `offset+limit` 行/源（5→15→25…→100）；kg 每页只给 30 条，100 行窗口是 4 次上游请求，可接受。若以后放宽到 200，请求数线性增长，需要先评估。
- **kg 翻页上限**：上游只回 30 条/页，插件必须循环；循环次数按 `ceil(limit/30)` 封顶，避免死循环。
- **排序稳定性**：`rankMultiSource` 的 tie-break 依赖 source 顺序 + 标题；跨页重复/漏行由 §7.1 的"无重复"用例守住。
- **绑定字段删除**：`CoverBatch` 从 DTO 移除会改 bindings，旧前端代码若还读 `cover_batch` 会变 `undefined`——必须同一提交改完。
- **旧设置残留**：数据库里旧的 `cover_batch` / `max_display` 值不会再被读取，无需迁移。
- **插件部署**：`plugins/` 不入库，改完必须手动同步到 `%LOCALAPPDATA%\airmedy\plugins\wy`，否则运行的是旧插件。
- **i18n**：若「加载更多」文案加 ⬇️，12 个语言文件必须在同一改动里更新。
