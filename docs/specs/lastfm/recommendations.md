# 任务：左侧栏新增 Last.fm 页（推荐 + 相似艺人）

> 状态：🗑️ 已废弃（页面已改为歌单卡片（recommendation-lists.md）与单请求常听（single-request-top-tracks.md））
> 位置：左侧栏「搜索」下方
> 目的：基于 Last.fm 数据生成可播放的推荐
> 范围：新页面 + 只读 Last.fm API；不改现有 scrobble/love

---

## 1. 目标

1. 侧边栏「搜索」下面新增 **Last.fm** 入口；
2. 页面区块：
   - **综合推荐**：最近播放、收藏、常听三路种子合并去重后的推荐；
   - **最近播放推荐**：只用最近播放做种子；
   - **收藏推荐**：只用收藏（loved）做种子；
   - **常听推荐**：只用常听曲目做种子；
   - **相似艺人**：由 `artist.getSimilar` 生成，点击可跳转到搜索该艺人；
   - 四份推荐都匹配到 5 个源，可直接播放/保存；
3. 未连接 Last.fm 时显示引导（去设置连接）；
4. 支持手动刷新；有缓存，不每次进页面都打 API。

## 2. 已确认的需求

- 区块：综合推荐、最近播放推荐、收藏推荐、常听推荐、相似艺人；
- 四份推荐分别由三类种子生成，再合并出综合推荐；
- 推荐必须能播放（匹配到源），匹配不到的**不显示**；
- 只读取 Last.fm，不写。

## 3. 数据来源与算法

### 3.1 种子

| 来源 | 方法 | 数量 |
|---|---|---|
| 最近播放 | `user.getRecentTracks`（limit 50） | 取前 20 首 |
| 常听曲目 | `user.getTopTracks`（`7day` 与 `1month`，各 20） | 合并取前 20 |
| 收藏 | `user.getLovedTracks`（limit 50） | 取前 20 |

- 按 `NormalizationKey(artist) + NormalizationKey(title)` 去重；
- 本地已有的曲目**保留**：在匹配阶段直接标记为可播（见 §4），不额外过滤；
- 最终种子上限 50。

### 3.2 推荐生成

1. 每个种子调 `track.getSimilar`（limit 10，`autocorrect=1`）；
2. 合并所有结果，按 `artist + title` 去重；
3. 按种子来源分别生成三份列表：
   - 最近播放 → `tracks_recent`
   - 收藏 → `tracks_loved`
   - 常听 → `tracks_top`
   每份各自去重、按被几首种子命中排序，各取前 40 条；
4. 综合列表 = 三份合并去重，计分 = 被多少个种子命中 × 来源权重（最近 1.0、常听 0.8、收藏 0.6），取前 60 条；
5. 四份列表都过滤掉种子本身；
6. 进入 §4 的源匹配。

### 3.3 限流

- Last.fm 约 5 请求/秒；实现为**并发上限 3 + 每次请求间隔 ≥ 250ms**；
- 种子相似查询失败（单条）只跳过该条，不中断整体；
- 超过 15 秒的请求视为失败，页面显示部分结果。

### 3.4 相似艺人

1. 种子艺人的集合（去重，上限 20）；
2. 每个调 `artist.getSimilar`（limit 10）；
3. 合并计分（出现次数），取前 20；
4. 每个艺人附一张图片（`image` 字段）与听众人数（可选）。

## 4. 匹配到可播放源

复用 catalog 搜索：

1. 对每个推荐曲目，用 `"<title> <artist>"` 在启用的源里搜索（先查本地库：`FindByRemoteIdentity`/标题匹配）；
2. 命中规则与跨源匹配一致：`NormalizationKey(title)` + `NormalizationKey(artist)` 相等，时长 ±5s；
3. 选择顺序：
   - 已有本地文件 → 直接可播；
   - 已缓存（`stream_cache`）→ 可播；
   - 搜索命中且 `resolver.CanResolve(provider)` → 可播/可保存；
   - 都不满足 → **丢弃该条**（不显示）；
4. 匹配结果按推荐分排序；
5. 最多展示 50 条。

> 匹配本身消耗大量搜索请求：每首歌最多查 1–2 个源，串行、限速；结果缓存 30 分钟。

## 5. 后端

### 5.1 Last.fm 客户端

`internal/app/lastfm` 增加只读方法：

```go
GetRecentTracks(ctx, limit) ([]LastFmTrack, error)
GetTopTracks(ctx, period string, limit) ([]LastFmTrack, error)
GetLovedTracks(ctx, limit) ([]LastFmTrack, error)
GetSimilarTracks(ctx, artist, title string, limit int) ([]LastFmTrack, error)
GetSimilarArtists(ctx, artist string, limit int) ([]LastFmArtist, error)
```

- 统一走现有 `apiBase` + `api_key`；
- 需要 `user`（用户名）的接口使用设置里的 Last.fm 用户名；
- 未连接（无 session/username）→ 返回 `domain.ErrLastFmNotConnected`。

### 5.2 推荐服务

新增 `internal/app/recommend`（或 `app/lastfm/recommend.go`）：

```go
GetRecommendations(ctx, opts) (RecommendationPage, error)
type RecommendationPage struct {
    Status         string   // "ok" | "not_connected" | "partial"
    Tracks         []RecommendedTrack  // 综合
    RecentTracks   []RecommendedTrack  // 最近播放种子
    LovedTracks    []RecommendedTrack  // 收藏种子
    TopTracks      []RecommendedTrack  // 常听种子
    SimilarArtists []SimilarArtist
}
```

- 内部按 §3 生成四份列表，再按 §4 匹配；
- **源匹配结果按曲目共享缓存**（`provider + title + artist`），四份列表复用同一次搜索，避免请求翻倍；
- 30 分钟内存缓存（key = username + 种子哈希），并发请求合并（single-flight）；
- 可手动刷新（`force=true`）。

### 5.3 Wails

- `LastFmService.GetRecommendations(force bool)`；
- 返回结构包含每个曲目的：源、`external_id`、`title`、`artists`、`album`、`duration`、`cover_url`、是否已缓存/已入库；
- 前端可直接复用 catalog 的单行组件发起播放/保存。

## 6. 前端

### 6.1 侧边栏

- `SidebarNav` 增加 `Last.fm`（`/lastfm`），位于「搜索」项之后；
- 图标用 Lucide（如 `Radio` / `Sparkles`）；
- i18n `sidebar.lastfm`。

### 6.2 页面 `LastFmView.vue`

- 顶部标题 + 刷新按钮 + Last.fm 用户名/头像（已连接时）；
- **综合推荐**：网格卡片列表（封面 + 标题 + 艺人 + 源标签），悬停出现播放/保存按钮（复用 catalog row 的 action）；
- **最近播放推荐 / 收藏推荐 / 常听推荐**：同样式的小节，各自独立，可横向滚动；
- **相似艺人**：头像网格，点击 → 跳转搜索页并预填艺人名（`/search?q=...`）；
- 状态：
  - 未连接：引导卡片 → 设置 → 集成；
  - 加载中：骨架屏；
  - 部分结果：顶部提示「部分请求失败」；
  - 空：说明文案 + 刷新；
- 12 语言新增 `lastfm.*`；
- 刷新按钮触发 `force=true`。

## 7. 测试

**Go**

- 种子去重与上限；
- `track.getSimilar` 聚合、计分、过滤种子自身；
- 限流并发（用 fake client 断言并发 ≤3 与间隔）；
- 匹配：本地文件优先、缓存次之、源搜索命中；未命中被丢弃；
- 未连接返回 `not_connected`；
- 缓存命中不重复请求；`force` 跳过缓存；
- 部分失败返回 `partial`。

**前端**

- 侧边栏出现 Last.fm 入口且位于搜索之后；
- 未连接状态渲染引导；
- 四份列表分别渲染（综合 + 三个来源）；
- 推荐卡片播放/保存调用共用；
- 相似艺人点击跳搜索；
- 空状态与刷新；
- 12 locale key 覆盖。

**人工**

1. Last.fm 已连接 → 打开页面有综合、最近、收藏、常听四份推荐与相似艺人；
2. 点播推荐曲目 → 正常播放（走流播）；保存 → 出现「已创建任务」；
3. 未连接 → 显示去设置；
4. 刷新按钮有效，重复进入不会每次打满 API；
5. 相似艺人点击 → 搜索页带该艺人名。

## 8. 验收

- 自动：§7 全绿；
- 人工：§7 五条成立；
- 不连接 Last.fm 时页面不报错。

## 9. 不做

- 不做 Last.fm 电台（API 已废弃）；
- 不做 Last.fm 播放列表导入（接口已废弃）；
- 不做社交/好友推荐；
- 不在页面上直接修改 scrobble/love 行为。

## 10. 风险

- Last.fm 限流（5 req/s）：推荐生成 + 源匹配的请求量较大，必须限速与缓存，否则会被 429；
- 四份列表让种子相似查询与匹配请求变多：必须共享匹配缓存并严格限流；
- 推荐质量依赖种子：新账号数据少时推荐可能为空，要有空状态；
- 源匹配会丢弃很多推荐（标题/艺人/时长不严格一致），页面条数可能少于预期；
- 后端需复用现有 `NormalizationKey` 与匹配容差，避免两套规则。
