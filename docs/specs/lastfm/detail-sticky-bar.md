# 任务：Last.fm 歌单详情页下滑时显示悬浮操作栏

> 状态：待开工
> 页面：`/lastfm/lists/:kind`
> 目的：下滑后头部滚出视口时，仍能返回、看标题、播放全部、更多和搜索

---

## 1. 行为

- 页面顶部是专辑详情风格的 hero（封面、标题、曲目数·时长、播放/随机/更多、搜索框）；
- 当 **hero 的标题区域滚出视口**后，从顶部滑入一条**悬浮操作栏**：

```
[← 返回]  [歌单标题]  [▶ 播放全部]  [⋯ 更多]         [🔍 搜索]
```

- 向上滚回、标题重新可见时，悬浮栏滑出消失；
- 悬浮栏固定在**内容区顶部**（不是整个窗口，避开侧边栏/播放器）；
- 玻璃质感（`bg-glass` + `backdrop-blur` + `ring`，按 `catalog/ui` 的浮层规则），高度约 56px；
- 不用滚动事件监听，用 `IntersectionObserver` 观察 hero 里的标题哨兵元素。

## 2. 实现

### 2.1 显示/隐藏

```ts
const titleSentinel = ref<HTMLElement | null>(null)
const showStickyBar = ref(false)

let observer: IntersectionObserver | null = null
onMounted(() => {
  observer = new IntersectionObserver(
    ([entry]) => { showStickyBar.value = !entry.isIntersecting },
    { rootMargin: '-8px 0px 0px 0px', threshold: 0 },
  )
  if (titleSentinel.value) observer.observe(titleSentinel.value)
})
onUnmounted(() => observer?.disconnect())
```

- 哨兵放在 hero 标题下方（`<span ref="titleSentinel" class="block h-px" />`）；
- 滚动容器是页面内容区（`overflow-y-auto`），observer 的 root 设为该容器（不是 window）；
- 不使用 scroll 监听，避免频繁回调。

### 2.2 悬浮栏内容

| 位置 | 内容 |
|---|---|
| 左 | 返回按钮（`router.back()`，与 hero 一致） |
| 左 | 歌单标题：单行截断，hover 显示完整 title |
| 左 | 播放全部（主色圆形/文字按钮，与 hero 一致） |
| 左 | 更多菜单（刷新此歌单、刷新全部） |
| 右 | 搜索：图标按钮，点击展开为输入框 |

- 搜索框展开后宽度约 220px，`Esc`/失焦收起；
- 悬浮栏与 hero 的搜索**共用同一份查询状态**（过滤列表）；
- 搜索框展开时，右侧其它占位让位，标题可截断更早。

### 2.3 过渡与层级

- 进入/离开：`translate-y(-100%) → 0` + `opacity`，200ms；
- `z-index` 低于弹窗/上下文菜单，高于列表内容；
- 不遮挡曲目列表的可点击区域（列表容器顶部加 padding 或 `scroll-margin`）；
- 遵守主题 token，不写死颜色。

### 2.4 可访问性

- 悬浮栏 `role="toolbar"`，`aria-label` 为歌单标题；
- 返回/播放/更多/搜索都有 `aria-label` 与 `title`；
- 键盘 Tab 顺序：返回 → 标题（不可聚焦）→ 播放全部 → 更多 → 搜索。

## 3. 测试

**前端**

- 哨兵不在视口 → 悬浮栏显示；回到视口 → 隐藏；
- 悬浮栏点击返回调用 `router.back()`；
- 播放全部/更多调用与 hero 相同的处理函数；
- 搜索展开、输入过滤列表、`Esc` 收起；
- 组件卸载时 `IntersectionObserver.disconnect()`（防泄漏）；
- 不出现滚动监听（可用 spy 断言未注册 `scroll`）。

**人工**

1. 打开歌单详情，下滑 → 悬浮栏滑入，内容正确；
2. 点击返回/播放/更多/搜索均可正常使用；
3. 上滑回顶部 → 悬浮栏消失，与 hero 不重叠；
4. 切换页面再回来，状态正确（不残留）。

## 4. 验收

- 自动：§3 全绿；
- 人工：§4 四条成立。

## 5. 风险

- 虚拟列表滚动容器与 observer root 不一致会导致不触发；
- 悬浮栏与顶部安全区（窗口标题栏）重叠，需要按现有 `App` 布局留白；
- hero 高度变化（窗口 resize）时需重新计算，observer 会自动处理。
