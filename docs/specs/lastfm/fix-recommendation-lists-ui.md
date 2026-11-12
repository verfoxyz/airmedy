# 修复/增强：Last.fm 歌单卡片——路由、单曲刷源、渐进加载

> 状态：待开工
> 关联：[recommendation-lists.md](recommendation-lists.md)、[single-request-top-tracks.md](single-request-top-tracks.md)
> 三个问题都来自同一轮人工验收

---

## 1. Bug：点任何卡片都进「为你推荐」

### 根因

`LastFmListView.vue` 只在 `onMounted` 里 `load()`；`kind` 是 `route.params.kind` 的 computed，但**没有 watch**。

从一张卡片跳到另一张时，路由参数变了、组件被复用（同一路由名），`onMounted` 不再触发 → 列表内容仍是第一次加载的「为你推荐」。

### 修法

- 监听 kind：

```ts
watch(kind, () => { void load() })
```

- `onMount` 时 load 一次；
- 切换时分页重置：`list`、`pendingItems`、`requeued`、`loadError` 全部清空；
- 事件订阅保持一次，但按新 kind 过滤；
- 顺手确认：卡片跳转使用 `list.kind`（现在是 `/lastfm/lists/${list.kind}`，正确）。

## 2. 单曲右键「刷新此曲的源」

右键行菜单新增一项（`TopTrackRow` 的 context menu 或复用 `TrackContextMenu`）：

- 名称：「刷新此曲的源」；
- 行为：
  1. 该行 `matching=true`、清空 `no_source`/`provider`/`external_id`/`state`；
  2. 在匹配缓存里**只清除这一行**的所有源结果（`artist+title` + 各源）；
  3. 把这一行重新放入五源匹配队列（每源一次）；
  4. 更新数据库该行、推送事件（前端原地变「匹配中」）；
  5. 命中后正常显示源 + 播放/保存；全部失败 → 「无匹配」。
- 已在匹配中的行禁用该菜单项；
- 不影响其它行，也不触发 Last.fm 重新拉取（只重跑源匹配）。

后端 API：

```go
// RefreshRecommendationItem re-runs source matching for one row.
RefreshRecommendationItem(ctx, kind, matchKey string) error
```

## 3. 渐进加载（一边拉一边显示）

### 3.1 常听曲目

`user.getTopTracks` 一次返回全部，本身无法分页流式；但可以在**响应到达时立即写库并渲染**（现在已接近）。没有额外优化空间。

### 3.2 为你推荐（相似）

相似推荐是**按种子逐个请求**的，可以边拉边显示：

1. 先写入空列表（或已有的第一页），后端每完成一个种子（`track.getSimilar` 或艺人兜底）：
   - 把新曲目追加到该 kind 的持久化行（按去重规则）；
   - emit 新事件 `lastfm:recommendation-items-added`：

```go
type RecommendationItemsAdded struct {
    Kind  string              `json:"kind"`
    Items []TopTrackItem      `json:"items"`  // 追加行，matching=true
}
```

2. 前端收到后按 `position` 追加到列表末尾；
3. 全部种子处理完 → 发送 done（复用现有事件）。

### 3.3 顺序与去重

- 追加顺序 = 种子顺序 + 每个种子的结果顺序；
- 同一首歌被多个种子推荐 → 保留第一份，后续只更新分数（不再追加）；
- 上限 50 条，达到上限后停止追加（仍在匹配之前已加入的行）。

## 4. 测试

**前端**

- 从卡片 A 进入列表 → 返回 → 点卡片 B → 标题与内容为 B（不是 A）；
- 右键「刷新此曲的源」→ 该行变「匹配中」，其它行不变；
- 渐进加载：分多次到达的事件按顺序追加，不重复、不超过 50 条。

**Go**

- `RefreshRecommendationItem` 只清该行缓存并重跑匹配，其它行缓存保留；
- 渐进构建：每完成一个种子写库并 emit 追加事件；最终 done；
- 去重：多种子推荐同一首只出现一次；
- 达到上限停止追加。

**人工**

1. 两张卡片分别点开，标题/内容正确；
2. 右键某行 → 刷新此曲的源 → 行变匹配中 → 命中新源或最终无匹配；
3. 打开「为你推荐」→ 曲目逐步出现，不是等全部完成才出现。

## 5. 验收

- 自动：§4 全绿；
- 人工：§5 三条成立。

## 6. 风险

- 渐进追加与用户滚动位置：追加到末尾，不打断阅读；
- 刷新单曲会绕过 30 分钟缓存（只针对该行）；
- 渐进加载使页面出现多次写库，需事务/批量合并写入频率（每种子一次即可）。
