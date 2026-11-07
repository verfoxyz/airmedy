# 修订：封面分批改为"跟着下一条待加载行"触发

> **已被取代**：分批机制整体删除，改为「首屏 5 行 + 每次 10 行」分页与行级懒加载，
> 见 [`docs/specs/cloud-search-paging/spec.md`](../cloud-search-paging/spec.md)。
> 以下内容仅保留作历史记录，不要再按本文实施。

> 状态：待开工
> 来源：人工验收（2026-11-07）—"5 张之后要滚到列表最下面才开始加载下一批"
> 前置：`fix-search-covers.md` 的哨兵方案已合入
> 约束：AI 不做图形化验证

---

## 1. 现象

- 每源先出 5 张（`cover_batch`）之后，**必须一路滚到列表最底部**，下一批才开始加载。
- 从第 6 行继续往下滚的整段过程中没有任何新封面出现，体验上像是"卡住了"。

## 2. 根因

哨兵被放在**整个列表的末尾**（"加载更多"按钮之后）：

```html
<div v-if="hasMore">…加载更多…</div>

<!-- End-of-list sentinel -->
<div ref="sentinelEl" data-test="catalog-cover-sentinel" class="h-px w-full" />
```

`IntersectionObserver` 的 `rootMargin` 只有 200px，所以只有滚到最后 200px 内、哨兵真正进入视口时才触发一次
`advanceCoverBatch()`。列表一页有 `max_display` 行（默认 20），前 5 张之后还有十几行，
这段距离里触发点根本到不了。

换句话说：**触发点是"列表的尾巴"，而应该跟着"下一条还没加载封面的行"走。**

## 3. 目标行为

- 首屏打开时，封面至少覆盖当前视野里的行；若还有空位，在接近视野底部时自动补下一批。
- 继续下滚时，**下一条待加载的行接近视口底部就补一批**，不再要求滚到底。
- 全部行都已放行后不再触发；`rows` 追加（加载更多）后在新尾巴上重新挂触发。
- 保持 `cover_batch` 的语义（每次每源 +N），不改成一次性全量。

## 4. 修法（推荐：直接观察"下一条待加载的行"）

不再依赖"列表末尾的哨兵"，改为观察 **frontier 行**——第一个还没有被放行（超出该源当前额度）的行。

### 4.1 计算 frontier

```ts
/** frontierIndex is the row index of the first row still waiting for its source's quota. */
const frontierIndex = computed(() => {
  const counts: Record<string, number> = {}
  for (let index = 0; index < props.rows.length; index++) {
    const row = props.rows[index]
    if (!row.cover_url || !row.external_id) continue
    const count = (counts[row.provider] ?? 0) + 1
    counts[row.provider] = count
    if (count > coverLimit(row.provider)) return index
  }
  return props.rows.length
})
```

`coverLimits` 已经是 `reactive`，`advanceCoverBatch()` 改额度后该 computed 会自动前移。
每行加一个稳定的定位属性：`:data-row-index="index"`，并给列表容器一个 `ref="rowsEl"`。

### 4.2 观察 frontier 行

```ts
const rowsEl = ref<HTMLElement | null>(null)

function frontierRowEl(): HTMLElement | null {
  const index = frontierIndex.value
  if (index >= props.rows.length) return null
  return rowsEl.value?.querySelector<HTMLElement>(`[data-row-index="${index}"]`) ?? null
}

function observeCoverFrontier() {
  coverObserver?.disconnect()
  coverObserver = null
  const target = frontierRowEl()
  if (!target || disposed) return

  const root = findScrollParent(rootEl.value)
  coverObserver = new IntersectionObserver(
    (entries) => {
      if (!entries.some((entry) => entry.isIntersecting)) return
      if (!advanceOnce()) return
      if (hasPendingCovers()) void nextTick(observeCoverFrontier)
    },
    { root, rootMargin: '0px 0px 200px 0px', threshold: 0 },
  )
  coverObserver.observe(target)
}
```

- 目标行进入视野（含提前 200px）→ 追加一批 → frontier 前移 → 重新挂到新的目标行；
- `advanceOnce()`（比较 `requestedCovers.size`）保持不变，用来防止空转；
- 全部放行后 `frontierRowEl()` 返回 `null`，不再观察；
- `coverObserver?.disconnect()` 仍在 `onUnmounted` 里。

### 4.3 重新挂载的时机

在现有的三处调用点，把 `observeCoverSentinel()` 换成 `observeCoverFrontier()`：

- `rows` 变化的 `nextTick`（列表内容变了，目标行可能换元素）；
- `showList` 变为 true 时（列表从无到有）；
- `requestCovers()` 的 `finally`（一批结束后 frontier 可能前移）。

建议再加一个 `watch(frontierIndex, () => void nextTick(observeCoverFrontier))`，
覆盖"额度变化但没有走 requestCovers"的路径。

### 4.4 删除

- 模板末尾的 1px 哨兵 `div` 及其 `ref`；
- `sentinelEl`、`observeCoverSentinel`（如果没人再用）。

### 4.5 备选（不推荐）

- 只把 `rootMargin` 调大（如 800px）：触发点仍绑在列表末尾，只是提前一点，长列表照样要滚到底；
- 把 `cover_batch` 默认值调大：一次性请求变多，且失去"分批"的意义。

## 5. 取舍

首屏如果一屏就能放下十几行，frontier 会连续触发几次，直到目标行落到视野之外——
也就是"把看得见的那一屏补满"，这正是想要的。单次仍只请求 `cover_batch` 行/源，
下行总量由视野高度自然限制。

## 6. 测试

`frontend/src/components/catalog/CatalogResults.spec.ts`：

- 8 wy + 8 kg（kg 无 `cover_url`）：
  - 首批请求 5 条 wy；
  - 断言 observer 的目标是 `[data-row-index="5"]` 那一行；
  - 模拟该行 `isIntersecting: true` → 再请求 5 条 wy；
  - 全部放行后 observer 不再创建（`FakeIO.instances` 数量不再增长）。
- 观测参数仍是 `rootMargin: 0px 0px 200px 0px`。
- `unmount()` 后 `disconnect()` 被调用。
- 新搜索时 frontier 回到第 5 行之后（批次状态重置）。
- 原有"无封面行不占配额""预加载后渲染 `/cover-cache/` 地址""失败占位""并发 ≤4"用例保持通过。

回归：

```bash
pnpm --dir frontend exec vue-tsc --noEmit
pnpm --dir frontend exec vitest run src/components/catalog/CatalogResults.spec.ts src/stores/catalog.spec.ts
```

## 7. 验收

自动：§6 全绿。

人工：

1. 全选源搜一个常见关键词：首屏的行基本都有封面；
2. 向下滚：**还没滚到底**就开始逐批出图，滚动过程中持续补齐；
3. 停在中间停留几秒，视野内不再出现长时间占位；
4. 点"加载更多"后，新增行同样边滚边出。

## 8. 风险

- 行元素在 `v-for` 中重建时，observer 目标会失效；因此必须在 `rows` 变化后重新 `observe`（§4.3）。
- `data-row-index` 必须用行在 `props.rows` 里的下标，不是封面计数。
- 首屏连续触发时，宿主下载并发由现有图片队列（≤4）兜底，不会瞬间打满。
