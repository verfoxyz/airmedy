# 修复：搜索页云端封面（kg 无图、每源 5 张后停滞）

> 状态：待开工
> 来源：人工验收（2026-11-07）三项现象
> 前置：`spec.md` 第一轮实现已在工作区（未提交，见 §7）
> 约束：AI 不做图形化验证；封面观感由人工确认
> 关联：缓存策略见 [`fix-r2.md`](fix-r2.md)，本单**不改缓存层**
> 后续：触发时机的人工验收反馈见 [`fix-cover-trigger.md`](fix-cover-trigger.md)（把触发点从列表末尾改到 frontier 行）
> **注意**：分批加载整体已被删除，改为分页 + 行级懒加载，见
> [`docs/specs/cloud-search-paging/spec.md`](../cloud-search-paging/spec.md)。本文的 §4.2（配额）与
> §4.3（哨兵）作废；§3 R1（kg 字段）、§4.1 仍然有效。

---

## 1. 现象与目标

人工验收观察到的现象：

1. 云端搜索里 **kg 的结果行全部没有封面**。
2. **wy 的结果行只有前 5 张出图**；继续下滑，第 6 张起一直是占位图标。
3. 判断：所有源大概率都有"只出第一批"的问题，kg 只是第一批也拿不到封面，所以看不出"分批"。

期望行为：

- kg 行有封面：`cover_url` 为 240 列表图，`cover_url_large` 在响应声明 400/480 时取大者。
- 每个源初始出 `cover_batch` 张（默认 5），继续滚动时按批追加，已加载/已请求的行不重复请求。
- **没有 `cover_url` 的行不占配额**，不挤掉同源其它行。
- 新搜索、切源时批次状态重置。

不在范围：宿主缓存实现、`/cover-cache/` 下发、搜索合并/排序/分页、视觉样式、`cover_batch` 设置语义。

---

## 2. 现状（已核实）

数据流：

```
插件 catalog.search 返回 cover_url / cover_url_large
  → CatalogResults 按源挑行
  → GetCatalogCovers（宿主下载、cover_cache 索引 + 文件缓存）
  → 返回 /cover-cache/<hash>
  → 前端预加载完成后再渲染 <img>
```

关键代码位置（行号以当前工作区为准）：

| 文件 | 符号 | 作用 |
|---|---|---|
| `frontend/src/components/catalog/CatalogResults.vue` | `collectCoverBatch()` L165–186 | 每批挑行 |
| 同上 | `hasPendingCovers()` L189–201 | 是否还有下一批 |
| 同上 | `advanceCoverBatch()` L253–260 | 提高每源额度并发起请求 |
| 同上 | `onScroll()` L263–279 | 距底 200px 判定 |
| 同上 | `findScrollParent()` L154–162 | 猜滚动容器 |
| 同上 | `onMounted` L304–310 / `onUnmounted` L312–324 | 绑定/解绑 |
| `frontend/src/views/SearchView.vue` | L188 `div.flex-1.overflow-y-auto` | 实际滚动容器 |
| `plugins/wy/src/search.js` | `coverEntriesFromBody()` L469 | 按 hash 映射返回项 |
| 同上 | `fetchKugouCovers()` L502 | 批量 + 单条重试 |
| 同上 | `applyKugouCovers()` L527 | 把封面写到行上 |
| 同上 | `kugouLargeSize()` L432 | 选大图尺寸 |
| `internal/app/catalog/service.go` | `Covers()` L316 | 不改 |
| `internal/infra/covercache/cache.go` | 整个包 | 不改 |

---

## 3. 根因

### R1 kg 读错字段（必现）

`get_res_privilege` 的返回形状是：

```json
{ "status": 1,
  "data": [ { "hash": "...", "album_audio_id": 32100650,
              "info": { "image": "http://imge.kugou.com/stdmusic/{size}/...jpg",
                        "imgsize": [480, 400, 240] } } ] }
```

`applyKugouCovers()` 读的是 `entry.image`，`kugouLargeSize()` 读的是 `entry.imgsize`——都在 `entry.info` 下面，顶层没有这两个字段。取到 `undefined` 后 `kugouCoverURL()` 返回空串，整行没有封面。

### R2 无封面行占用配额（确定存在）

```ts
// collectCoverBatch() 现状
const count = (counts[row.provider] ?? 0) + 1
counts[row.provider] = count
if (count > coverLimit(row.provider)) continue
if (!row.cover_url || !row.external_id) continue   // 先计数，后判封面
```

`hasPendingCovers()` 是同一段逻辑的副本。kg 每行都没有 `cover_url`，却仍然吃掉每源 1–5 的名额。修好 R1 之前，kg 行会长期占用额度，wy 的有效额度也被稀释。

### R3 触发下一批的开关只有一次性的 scroll 监听（实测未触发）

- `onMounted` 只绑一次 `scroll`：绑到 `findScrollParent(rootEl)` 猜出来的容器；
- 猜不到就退化成 `window`，而列表实际在 `SearchView.vue:188` 的内部容器里滚动，`window` 永远收不到事件，之后不会再有任何一批；
- 容器被替换或重新布局后不会重绑；
- 判定还依赖 `scrollHeight / scrollTop / clientHeight` 三者都正确。

结论：触发机制与"容器识别"强耦合且失败时静默，改成与容器解耦的哨兵方案，不再依赖猜中容器。

### R4 没有测试覆盖真实滚动路径

现有用例两种写法：

- 直接调用 expose 出来的 `advanceCoverBatch()`；
- `window.dispatchEvent(new Event('scroll'))`——jsdom 没有布局，必然走 `window` 分支。

两种都覆盖不到"内部容器滚动 → 哨兵进入视口"，R3 的回归没有测试能挡住。

---

## 4. 修法

### 4.1 插件：读 `entry.info`

`plugins/wy/src/search.js`：

```js
/** applyKugouCovers stamps resolved artwork onto the matching rows. */
function applyKugouCovers(rows, covers) {
  for (const row of rows) {
    const entry = covers.get(row.externalId);
    const info = entry && entry.info && typeof entry.info === 'object' ? entry.info : null;
    if (!info) continue;
    const image = kugouCoverURL(info.image, KG_COVER_SIZE);
    if (image === '') continue;
    row.cover_url = image;
    row.cover_url_large = kugouCoverURL(info.image, kugouLargeSize(info));
  }
}
```

- `kugouLargeSize(info)` 的参数改为 privilege 返回项的 `info`，读 `info.imgsize`；
- `coverEntriesFromBody()`、批量/单条重试、共享预算、静默失败**全部不变**；
- 边界：`info` 缺失、`info.image` 非字符串或为空 → 该行保持无封面，不抛错、不写日志、不影响搜索。

### 4.2 前端：配额只统计"有封面的行"

`collectCoverBatch()` 与 `hasPendingCovers()` 都把封面判断提前：

```ts
for (const row of props.rows) {
  if (!row.cover_url || !row.external_id) continue
  const count = (counts[row.provider] ?? 0) + 1
  counts[row.provider] = count
  if (count > coverLimit(row.provider)) continue

  const key = coverKey(row)
  if (requestedCovers.has(key) || failedCovers.has(key) || loadedCovers.has(key)) continue
  ...
}
```

两处保持一致；`advanceCoverBatch()` 的"每 provider +batchSize"可以保留。

### 4.3 前端：哨兵 + IntersectionObserver 替换 scroll 监听

模板：结果列表末尾（`showList` 块内，"加载更多"按钮之后）加一个 1px 哨兵：

```html
<div
  ref="sentinelEl"
  data-test="catalog-cover-sentinel"
  aria-hidden="true"
  class="h-px w-full"
/>
```

脚本：

```ts
const sentinelEl = ref<HTMLElement | null>(null)
let coverObserver: IntersectionObserver | null = null

/** advanceOnce reports whether the trigger actually queued new rows. */
function advanceOnce(): boolean {
  const before = requestedCovers.size
  advanceCoverBatch()
  return requestedCovers.size > before
}

/** observeCoverSentinel (re)creates the observer after the sentinel moved. */
function observeCoverSentinel() {
  coverObserver?.disconnect()
  coverObserver = null
  const target = sentinelEl.value
  if (!target || disposed) return

  // The sentinel lives in the list's own scroller; when that cannot be found,
  // null means the viewport, which still sees the sentinel move on scroll.
  const root = findScrollParent(rootEl.value) as Element | null
  coverObserver = new IntersectionObserver(
    (entries) => {
      if (!entries.some((entry) => entry.isIntersecting)) return
      // Still in view after a batch means the list is shorter than the viewport
      // (or the user reached the end): keep feeding until nothing is pending.
      if (advanceOnce() && hasPendingCovers()) observeCoverSentinel()
    },
    { root, rootMargin: '0px 0px 200px 0px', threshold: 0 },
  )
  coverObserver.observe(target)
}
```

规则：

- 哨兵进入视口（含提前 200px）→ `advanceCoverBatch()` 追加一批；
- 若追加后哨兵仍在视口内且仍有未取封面的行，**重新 observe**（用"本轮 `requestedCovers.size` 是否增长"做保护，避免空转）；这样短列表（没有滚动条）也能补齐，不会永远停在 5 张；

- `rows` 变化的 `nextTick` 里调用 `observeCoverSentinel()`（哨兵随 `showList` 出现/重建）；
- `onUnmounted`：`coverObserver?.disconnect()`，并清空引用；
- 删除 `scrollElement` / `scrollListener` / `onScroll` / `SCROLL_THRESHOLD_PX`；
- `findScrollParent()` 保留，只用于给 observer 找 `root`；找不到传 `null`，**不要**再退化成 window 监听。

取舍：用户滚到列表底部时，剩余批次会被连续加载完（因为哨兵一直可见）。这是有意的——避免短列表永远只出 5 张；列表顶部到中段的行为仍是"每次进入一批"。

### 4.4 明确不做

- 不改宿主 `Covers()`、`covercache`、asset handler、`000089` 迁移；
- 不改合并、排序、分页、`load-more`；
- 不改 `cover_batch` 默认值与范围；
- 不新增依赖（用原生 `IntersectionObserver`）；
- 不改视觉与 i18n 文案。

---

## 5. 测试

### 5.1 插件（cwd `plugins/wy`，`node --test`）

新增用例：

- 真实形状：`{status:1, data:[{hash, album_audio_id, info:{image:'http://imge.kugou.com/stdmusic/{size}/20230920/x.jpg', imgsize:[480,400,240]}}]}` → 行 `cover_url` 含 `/240/`，`cover_url_large` 含 `/480/`；
- `info` 缺失、`image` 为空串 → 行无 `cover_url`，不抛错；
- 既有"批量失败走单条重试"用例保持通过。

### 5.2 前端（`pnpm --dir frontend exec vitest run src/components/catalog/CatalogResults.spec.ts`）

改写/新增：

- **配额**：8 条 wy（有 `cover_url`）+ 8 条 kg（无 `cover_url`）→ 首批只请求 5 条 wy；触发一次哨兵 → 再请求 5 条 wy；
- **哨兵**：用 `vi.stubGlobal('IntersectionObserver', FakeIO)`，断言构造参数的 `rootMargin` 含 `200px`；模拟 `isIntersecting: true` → 触发下一批；`unmount()` 后 `disconnect()` 被调用；
- **不再监听 scroll**：原"advances on a real scroll event"用例删除，改为断言没有向 window/容器注册 `scroll`；
- **新搜索重置**：新页面首批仍只出 5 条/源；
- 既有用例（预加载后渲染 `/cover-cache/` 地址、失败占位、并发 ≤4）保持通过。

### 5.3 回归

```bash
pnpm --dir frontend exec vue-tsc --noEmit
pnpm --dir frontend exec vitest run src/components/catalog/CatalogResults.spec.ts src/stores/catalog.spec.ts
```

---

## 6. 验收

自动：§5 全绿。

人工（由人做，AI 不做）：

1. 只选 kg 搜一个常见关键词 → 结果行有封面；
2. 全选源 → 每源约 5 张；滚到底逐批出图，不重复请求已加载的行；
3. 新搜索 / 切源后回到"每源 5 张"；
4. 断网后重搜同一关键词 → 已缓存的封面仍然显示。

---

## 7. 假设、风险与前置状态

**假设**

- privilege 响应的图片字段确为 `info.image`（2026-11-07 实测，形状见 §3 R1）。交付时若仍有行无封面，
  用 `node -e` 打一次真实响应确认字段，按实测改，不要凭猜。
- WebView2 支持 `IntersectionObserver`（现代 Edge 内核对）。
- `cover_batch` 最小值为 1 时，哨兵会频繁触发；可接受，默认 5。

**前置状态**

- 当前工作区是第一轮实现（未提交）：`internal/infra/covercache/`、`000089_cover_cache`、`CatalogResults.vue` 的滚动分批、插件 kg 封面等。
- `plugins/wy/` 在 `.gitignore` 里（只 `plugins/README.md` 入库），所以 §4.1 的插件改动**不会进版本库**，交付时需要单独说明。
- 建议先按 `tasks.md` 的拆分提交第一轮，再单独提交本修复，便于回滚。

**执行提示**

- 先跑一遍基线（`node --test` + 前端两个 spec），记录现状再动手；插件与前端可以并行。
- 前端改动完成后再同步 `catalog/ui/README.md` 的封面加载描述（"滚动接近底部触发" → "列表末尾哨兵进入视口触发"），其余 catalog 不动——宿主缓存契约没变。
- 明确记录未做图形化验证（`AGENTS.md` §6）。
