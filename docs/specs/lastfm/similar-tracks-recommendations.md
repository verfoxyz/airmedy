# 任务：Last.fm 页增加「相似歌曲推荐」（`track.getSimilar` 优先 + 艺人兜底）

> 状态：待开工
> 目的：在现有「常听曲目」之外，给用户推荐**没听过**的相似歌曲
> 关联：`single-request-top-tracks.md`（现有页面与匹配队列）、`fix-recommendation-covers.md`、`fix-recommendation-playback-cover.md`

---

## 1. 两条路一起用（已确认）

1. **直接相似歌曲**：`track.getSimilar`（热门曲目能给结果，最像“推荐”）；
2. **艺人兜底**：该曲目 `track.getSimilar` 为空时 →
   - `artist.getSimilar`（取相似艺人）；
   - `artist.getTopTracks`（取相似艺人的热门歌）。

不二选一：先路径 1，空则路径 2。

## 2. 种子与请求预算

- 种子：现有常听曲目（`user.getTopTracks`，`overall`）前 **10 首**；
- 每个种子最多 1 次 `track.getSimilar`（limit 10）；
- 只对**返回空**的种子走兜底：`artist.getSimilar`（1 次，limit 3）+ 每个相似艺人 `artist.getTopTracks`（最多 3 个，limit 5）；
- 最坏请求数：10（相似歌曲）+ 10（艺人相似）+ 30（艺人热门）≈ 50；实际大多数热门种子在第 1 步就有结果，通常 < 20；
- 全部走现有 limiter（并发 2 / 间隔 400ms）与 30 分钟缓存。

## 3. 过滤

- **不剔除本地已有的曲目**：本地命中的行保留，显示「已在库中」，可直接播放；
- 排除种子本身；
- 可选：排除 Last.fm 已 scrobble 过的曲目（需要再查接口，暂不做）；
- 标题+艺人集合去重；时长 ±5s 视为同一首。

## 4. 展示

- 页面新增区块「为你推荐」（放在「常听曲目」上方）；
- 行样式、匹配队列、封面优先级、播放/保存行为完全复用现有实现；
- 匹配仍走五源队列（每首每源最多一次），未命中的行最终显示「无匹配」；
- 与常听曲目共享同一份匹配缓存（按 artist+title+source）。

## 5. 后端

- `GetTopTrackRecommendations` 增加 `SimilarItems` 字段（或在 `TopTrackPage` 里新增 `Recommendations`）；
- 新增 `collectSimilarRecommendations(ctx, seeds)`：
  1. 种子前 10 → `track.getSimilar`；
  2. 空 → 艺人兜底；
  3. 聚合、过滤、去重（按热度/出现次数排序，取前 50）；
- 失败降级：部分失败只影响对应种子，`StatusPartial`。

## 6. 测试

**Go**

- 有相似数据 → 只用 `track.getSimilar`，不触发艺人兜底；
- 空相似 → 触发 `artist.getSimilar` + `artist.getTopTracks`；
- 请求数不超过预算（fake client 计数）；
- 本地已有的曲目**保留**并标记 `in_library`，不被过滤；
- 去重（同一首被多个种子推荐）；
- 部分失败 → 其余结果仍返回。

**前端**

- 新区块渲染、匹配状态与常听曲目一致；
- 无推荐时显示空状态，不影响常听曲目。

**人工**

1. 打开页面 → 「为你推荐」出现没听过的歌；
2. 点播放/保存正常；
3. 本地已有的歌出现在推荐里，并显示「已在库中」、可直接播放。

## 7. 验收

- 自动：§6 全绿；
- 人工：§6 三条成立。

## 8. 风险

- `track.getSimilar` 对冷门种子仍为空，兜底后可能仍少；
- 请求量比单请求版本大，需依赖缓存与限流；
- “没听过”只按本地库判断，Last.fm 云端听过但本地没有的仍会推荐（可选后续过滤）。
