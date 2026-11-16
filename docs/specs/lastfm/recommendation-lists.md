# 任务：Last.fm 推荐改为持久化的「歌单卡片」

> 状态：⚠️ 部分完成（持久化歌单/刷新/详情已实现；**卡片借用 AlbumCard 样式、详情页对齐专辑详情**为最新要求，尚未实现）
> 目的：Last.fm 页不再平铺所有行，而是把每个推荐集合做成**歌单卡片**；推荐内容持久化，只有刷新才会变，支持单个歌单刷新
> 关联：`single-request-top-tracks.md`、`similar-tracks-recommendations.md`

---

## 1. 页面形态

- Last.fm 页顶部是**歌单卡片网格**，把现在页面上的两个区块改成歌单：
  - **常听曲目**（`top_tracks`，现页面「常听曲目」，来自 `user.getTopTracks`）
  - **为你推荐**（`similar`，现页面「为你推荐」，来自 `track.getSimilar` + 艺人兜底）
  - 卡片顺序沿用现页面：为你推荐在上，常听曲目在下；
  - 名称沿用现有 i18n key（`lastfm.similar_tracks` / 词条），不新增命名；
- **卡片样式直接借用「专辑」页面的 `AlbumCard`**（`frontend/src/components/AlbumCard.vue`）：
  - 封面：`aspect-square` + `rounded-lg` + `ring-1 ring-foreground/[0.06]`，悬停 `scale-105`；
  - 无封面时显示居中图标（歌单用音符/列表图标，不是专辑的 `Disc`）；
  - 悬停遮罩：`bg-background/20` 淡入 + 居中的圆形操作按钮（主按钮样式与 `AlbumCard` 的播放按钮一致）；
  - 文字区：`px-1`，标题 `font-medium text-sm truncate`；第二行 `text-xs text-dim` 显示 **`曲目数 · 总时长`**（不显示艺人/作曲家）；
  - 网格与尺寸沿用专辑网格：`VirtualizedGrid` / `min-column-width = 180`、`gap = 45`、`square-items = true`；
- 悬停操作（叠在封面遮罩里）：
  - 主按钮：**播放全部**（与专辑播放按钮同位置/样式）；
  - 次级图标按钮：**随机播放**；
  - 右上角：**更多**（菜单：刷新此歌单、刷新全部）；单卡刷新按钮保留；
- 与 `AlbumCard` 的差异仅限：封面是拼图（1–4 首）、第二行是曲目数+总时长、没有艺人链接；
- 点击卡片 → 进入该歌单详情（路由 `/lastfm/lists/:kind`），列表行复用现有 `TopTrackRow`（匹配状态、播放、保存、封面）；
- 每张卡片右上角有**单个刷新**按钮；页面顶部另有「刷新全部」。

## 2. 持久化（核心）

推荐内容落库，**打开页面不重新请求 Last.fm**：

- 首次没有数据时构建一次并写入；
- 之后每次打开都读库，直到用户点刷新；
- 匹配状态（匹配中/源/无匹配/本地命中）也写入库，重启后仍在；
- 匹配仍在后台继续（补全未匹配的行），结果写回库并推送事件。

### 2.1 迁移 `000100`

```sql
CREATE TABLE lastfm_recommendation_lists (
    kind       TEXT PRIMARY KEY,        -- 'top_tracks' | 'similar' | ...
    title      TEXT NOT NULL,
    payload    TEXT NOT NULL,           -- JSON: []StoredRecommendationItem
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE lastfm_recommendation_items (
    kind       TEXT NOT NULL,
    position   INTEGER NOT NULL,
    match_key  TEXT NOT NULL,
    title      TEXT NOT NULL,
    artist     TEXT NOT NULL,
    album      TEXT NOT NULL DEFAULT '',
    duration   INTEGER NOT NULL DEFAULT 0,
    cover_url  TEXT NOT NULL DEFAULT '',
    matching   INTEGER NOT NULL DEFAULT 1,
    no_source  INTEGER NOT NULL DEFAULT 0,
    provider   TEXT NOT NULL DEFAULT '',
    external_id TEXT NOT NULL DEFAULT '',
    state      TEXT NOT NULL DEFAULT '',
    local_track_id TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (kind, match_key)
);
```

- `lastfm_recommendation_lists` 只存 kind/title/时间，便于列表页快速渲染；
- 明细按行存，匹配事件直接更新对应行（不用整块重写 JSON）；
- `position` 保持 Last.fm 原始顺序。

## 3. 刷新语义

| 操作 | 行为 |
|---|---|
| 打开页面 | 只读库；库为空才构建一次 |
| 点某张卡的刷新 | 只重建该 `kind` 的 Last.fm 内容：重新拉取、重置该 kind 的匹配状态为 `matching`、写库、推送事件；**其它 kind 完全不动** |
| 点「刷新全部」 | 依次重建所有 known kinds |
| 关闭/重启应用 | 内容与匹配状态保持，直到刷新 |

- 刷新是显式操作，不因进入页面/切歌/定时器自动触发；
- 刷新失败保留旧内容并提示（不把列表清空）；
- 匹配 worker 只处理库里 `matching=1` 的行。

## 4. 后端

```go
ListRecommendationLists(ctx) ([]RecommendationListSummary, error)   // 卡片
GetRecommendationList(ctx, kind) (RecommendationList, error)       // 详情
RefreshRecommendationList(ctx, kind) error                          // 单个
RefreshAllRecommendationLists(ctx) error
```

- 构建逻辑复用现有 `top_tracks.go` / `similar` 的采集函数；
- 构建成功 → 事务内替换该 kind 的行；
- 匹配 worker 读取所有 `matching=1` 行（按 kind 分配），命中后更新行 + emit `lastfm:recommendations-matched`（payload 带 kind）；
- 封面优先级沿用 `fix-recommendation-covers.md`（Last.fm 真实图 → 源封面 → 本地 artwork → 占位）。

## 5. 前端

- `/lastfm`：卡片网格（复用现有卡片/封面拼图样式），顶部「刷新全部」；
- `/lastfm/lists/:kind`：**几乎与「专辑详情」一致**（参照 `AlbumDetailView.vue`）：
  - 复用 `DetailHero`：顶部返回、渐变背景、方形封面（拼图）、标题、元信息行 `曲目数 · 总时长`；
  - 复用 `DetailsButton` 操作区：**播放**（主按钮）、**随机播放**（描边）、**更多**（菜单：刷新此歌单、刷新全部）；
  - 复用专辑详情的**搜索框**（右上角 `Search` + `Input`），按标题/艺人本地过滤列表；
  - 列表行沿用 `TopTrackRow`（保留匹配状态 chip、播放/保存、右键刷新此曲的源），但行密度/间距对齐 `TrackTable`；
  - 时长与来源不一致的行显示「此曲有疑问」chip（不阻止播放；保存/下载需用户自己点）；
  - 与专辑详情的差异仅限：列表行带“匹配中/来自源/无匹配/已在库中”状态，以及没有艺人/发行日期等专辑元信息；
  - 详情页的封面/时长/可播放行解析与卡片共用 `lastfmTracks.ts`；
  - 不显示艺人/作曲家头部信息（与卡片一致）；
- 每卡刷新按钮：请求中禁用、完成后刷新卡片与（若在详情页）列表；
- 事件按 `kind + match_key` 更新对应行与卡片计数；
- 复用现有 `lastfm.similar_tracks` 等词条；只新增：
  - `lastfm.refresh_list`
  - `lastfm.refresh_all`
  - `lastfm.list_empty`
  - 卡片标题直接用两个集合现有的标题文案。

## 6. 测试

**Go**

- 首次打开：库为空 → 构建一次并写入；
- 第二次打开：不调用 Last.fm（fake client 计数为 0）；
- 单个刷新：只更新该 kind，其它 kind 行与时间戳不变；
- 刷新失败：旧内容保留；
- 匹配事件更新对应行并持久化（重启后仍在）；
- 迁移 `000100` up/down。

**前端**

- 卡片网格渲染（标题/数量/拼图）；
- 点卡片进入详情；返回后内容不变；
- 单卡刷新只刷新该卡；「刷新全部」刷新所有卡；
- 匹配事件更新详情行与卡片计数。

**人工**

1. 首次进入 Last.fm 页 → 出现「为你推荐」「常听曲目」两张卡片；
2. 点开卡片 → 有曲目、匹配状态与播放/保存；
3. 返回/重启 → 内容不变；
4. 单卡刷新 → 只有该卡变化，另一张保持不变；
5. 「刷新全部」→ 两张都重建。

## 7. 验收

- 自动：§6 全绿；
- 人工：§6 五条成立。

## 8. 风险

- 持久化会让内容“变旧”，用户需要主动刷新（这是需求本身）；
- 刷新失败必须保留旧数据，否则体验倒退；
- 匹配状态持久化后，源失效时旧状态可能过期，刷新会重置；
- 歌单名称是产品文案，后续新增集合只需加 kind + 采集函数。
