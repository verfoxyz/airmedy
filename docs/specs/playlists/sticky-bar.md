# 任务：所有歌单详情改为整体滚动 + 悬浮栏

> 状态：✅ 已完成（抽出 `StickyDetailBar.vue`（IntersectionObserver + 56px 玻璃 toolbar + 搜索展开 + 更多菜单 + 卸载 disconnect），Last.fm 页改为复用；普通歌单（含收藏虚拟歌单）改为整页滚动：`body-fill-height=false`、TrackTable `virtual-scroll=false auto-height`、标题后哨兵接入悬浮栏；Vue 测试覆盖显隐、动作、搜索、断开与歌单接线）
> 目的：普通歌单与 Last.fm 歌单行为一致——整页滚动，hero 滚出视口后出现悬浮操作栏
> 关联：`detail-sticky-bar.md`（Last.fm 已实现）、`list-table-layout.md`、`playlists`

---

## 1. 范围

所有歌单类详情页：

| 页面 | 现状 |
|---|---|
| Last.fm 歌单（`/lastfm/lists/:kind`） | ✅ 已实现（整体滚动 + 悬浮栏） |
| 普通歌单（`/playlists/:id`） | ❌ hero 固定 / 只滚歌曲区 |
| 收藏、最近添加等“歌曲列表”页 | ❌ 同上（如果结构相同） |

目标：全部改为**整页滚动 + hero 滚出后悬浮栏**。

## 2. 行为（与 Last.fm 一致）

1. 页面只有**一个滚动容器**（整个内容区），hero 随内容一起滚；
2. hero 标题滚出视口 → 悬浮栏从顶部滑入：
   ```
   [← 返回] [歌单标题] [▶ 播放] [🔀 随机] [⋯ 更多]        [🔍 搜索]
   ```
3. 向上滚回标题可见 → 悬浮栏滑出；
4. 使用 `IntersectionObserver` 观察 hero 标题后的哨兵元素，不监听 scroll；
5. 搜索框展开/收起、与 hero 搜索共用同一过滤状态；
6. 悬浮栏玻璃质感、高度约 56px、`role="toolbar"`。

## 3. 实现

### 3.1 抽公共组件

把 Last.fm 的内联实现抽成 `frontend/src/components/StickyDetailBar.vue`：

- props：`title`、`canPlay`、`canShuffle`、`onBack`、`onPlay`、`onShuffle`、`moreItems`、`search`（v-model + placeholder）；
- 自带 `IntersectionObserver`、滑入/滑出过渡、无障碍属性；
- Last.fm 页面改为使用该组件（行为不变，仅去重）。

### 3.2 歌单页接入

`PlaylistDetailView.vue`：

- 把当前“固定 hero + 独立滚动列表”改为“整页滚动”；
- hero 保持现状视觉，只是不再固定；
- 标题后插入哨兵 `<span ref="titleSentinel" class="block h-px" />`；
- 接入 `StickyDetailBar`：标题、播放、随机、更多（现有菜单）、搜索（现有过滤）；
- `TrackTable` 的高度改为随页面滚动（不再自身滚动）。

### 3.3 其它列表页

若「收藏 / 最近添加」等页面使用同结构，一并用该组件；没有 hero 的页面不适用。

## 4. 测试

**前端**

- Last.fm 页面回归：悬浮栏显示/隐藏、搜索展开；
- 歌单页：hero 滚出 → 悬浮栏出现；回滚 → 消失；
- 悬浮栏按钮触发与 hero 相同动作（播放/随机/更多/搜索）；
- 搜索状态与列表过滤同步；
- 组件卸载时 `IntersectionObserver.disconnect()`；
- 窄屏布局不遮挡表头。

**人工**

1. 打开普通歌单 → 整页一起滚；下滑后出现悬浮栏；
2. 悬浮栏播放/随机/更多/搜索可用；
3. 上滑回顶部 → 悬浮栏消失、与 hero 不重叠；
4. Last.fm 歌单行为与之前一致。

## 5. 验收

- 自动：§4 全绿；
- 人工：§5 四条成立；
- 所有歌单页面行为一致。

## 6. 风险

- 整页滚动会改变 `TrackTable` 的虚拟滚动前提（它通常自己滚动），需要改成窗口内滚动或保留其内部滚动但外层不滚；
- 过渡期间 hero 高度变化（封面加载）可能导致哨兵抖动，需在图片加载后重算；
- 悬浮栏与表头/过滤栏层级要理清，避免遮挡。
