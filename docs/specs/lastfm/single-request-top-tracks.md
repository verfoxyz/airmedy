# 任务（MVP）：单请求常听曲目 + 五源队列匹配

> 状态：待开工
> 目标：Last.fm 页先做一个最小可用版本——**1 个 API 请求拿到常听曲目，全部显示，匹配成功的打标签**
> 取代：本阶段不再做四份推荐列表、种子相似、artist.getSimilar 等（后续再加）

---

## 1. 页面行为

1. 进入页面，只发 **1 个 Last.fm 请求**：`user.getTopTracks`（`period=7day`，`limit=50`）；
2. **全部曲目立即显示**（标题、艺人、专辑）；
3. 每行有一个状态：
   - `匹配中`（还没轮到 / 正在查）；
   - `已匹配` + 源标签（`wy` / `kg` / `tx` / `kw` / `mg`），出现播放/保存按钮；
   - `无匹配`（5 个源都试过仍没有）。
4. 不使用本地过滤：本地已有的曲目也算匹配（匹配后可直接播放）。

## 2. 匹配调度（按你的要求）

- **5 个源 = 5 个并发 worker**，每个 worker 一次只处理 1 首；
- 每首曲目初始进入待匹配队列，由空闲 worker 领取；
- 一个 worker 只查询**自己负责的源**：
  - 命中 → 该行标记 `已匹配 + 源`；
  - 未命中 → 把曲目放回重试队列（供其它源领取），并记录“该源已试过”；
- 所有源都试过仍未命中 → 标记 `无匹配`，不再重试；
- 每首曲目每个源最多查 1 次，总搜索次数 ≤ 50 首 × 5 源，但由 5 个 worker 并发控制，不会同时爆发；
- 单次查询超时 15s，失败视为未命中并换源；
- 匹配结果缓存 30 分钟（按 artist+title+来源），刷新页面不重复查。

## 3. 接口与事件

### 3.1 数据

`GetTopTrackRecommendations()`（新绑定，或改现有 `GetRecommendations`）：

```go
type TopTrackItem struct {
    Title         string
    Artist        string
    Album         string
    Duration      int
    // 匹配状态
    Matching      bool     // 仍在队列/查询中
    Matched       bool
    NoSource      bool     // 5 源均未命中
    Provider      string   // 命中时
    ExternalID    string
    State         string   // 命中后的行状态：in_library / downloading / saved ...
    Downloadable  bool
    MatchKey      string   // artist + "\x00" + title，用于前端定位
}

type TopTrackPage struct {
    Status string          // ok | not_connected
    Items  []TopTrackItem  // 全部曲目，立即返回
}
```

### 3.2 事件

沿用 `lastfm:recommendations-matched`，payload 改为：

```go
type TopTrackUpdate struct {
    Items []TopTrackItem  // 状态变化（已匹配 / 无匹配）
    Done  bool            // 队列全部处理完
}
```

- 每个 worker 完成一首后可推送（合并 300–500ms 一次）；
- `Done=true` 表示没有 `Matching` 的行了。

## 4. 前端

- 页面只保留一个列表区（「常听曲目」），顶部保留刷新；
- 每行右侧状态 chip：
  - `匹配中…`（灰色，无按钮）
  - 源标签（如 `wy`）+ 播放/保存按钮
  - `无匹配`（灰色）
- 监听事件按 `MatchKey` 原地更新；
- `Done` 后不残留 `匹配中`；
- 未连接 Last.fm：引导去设置；
- 12 语言复用/新增：`lastfm.matching`、`lastfm.no_match`、`lastfm.matched_from`。

## 5. 不做（本阶段）

- 四份推荐（综合/最近/收藏/常听）分别生成；
- `track.getSimilar`、`artist.getSimilar`、标签/榜单兜底；
- 推荐分/共识度计分。

这些在 MVP 验证后再加。

## 6. 测试

**Go**

- `GetTopTrackRecommendations` 只发 1 次 Last.fm 请求，返回全部曲目（`Matching=true`）；
- 调度：5 个 worker 各自只查自己的源；每首每源最多 1 次；
- 命中后标记与源正确；失败后换源；
- 5 源全失败 → `NoSource=true`；
- 事件合并推送、`Done` 最终为 true；
- 缓存命中不重复请求。

**前端**

- 全部行立即渲染；
- 状态 chip 三态正确；
- 事件到达后原地更新；按钮只对已匹配行出现；
- `Done` 后无残留「匹配中」。

**人工**

1. 打开页面 → 列表立即出现；
2. 几秒内陆续出现源标签和播放按钮；
3. 少数行最终显示「无匹配」；
4. 刷新后明显更快（缓存）。

## 7. 验收

- 自动：§6 全绿；
- 人工：§7 四条成立；
- 页面在 1 次 Last.fm 请求下就能用，不再出现整页空白或只有相似艺人。

## 8. 风险

- 5 个源并发搜索可能让插件负载高（每源一个 worker），需要保留现有插件超时；
- 冷门曲目大多会「无匹配」，属数据问题；
- 若某个源整体不可用（如 kg 连接失败），该源 worker 会很快把队列转给其它源，不会卡住。
