# Bug：Last.fm 页只有相似艺人，推荐列表为空

> 状态：待开工
> 现象：Last.fm 页面只显示相似艺人；综合/最近/收藏/常听四份推荐全空；相似艺人也没有头像
> **优先级：推荐歌曲是主要目标**；艺人头像只是附带
> 结论：不是解析 bug，是 Last.fm 数据不足 + 实现缺少兜底

---

## 0. 数据验证（本机实测）

- `track.getSimilar`：热门曲目有结果；当前账号的种子全部为空；
- `artist.getSimilar`：正常返回相似艺人；
- `artist.getTopTracks`：正常返回歌曲（如 Kobaryo 的 `Invisible Frenzy` / `Bookmaker`）；
- `user.getTopTags`：**当前账号为空** → 标签兜底必须从种子艺人聚合，不能只用用户标签；
- `chart.getTopTracks`：作为最终兜底始终有数据。

## 1. 根因

### 1.1 推荐为空

已用当前 key 实际请求验证：

- 种子接口正常：`user.getRecentTracks` / `user.getTopTracks` / `user.getLovedTracks` 都有数据；
- `track.getSimilar` 对热门曲目能返回结果（如 `Daft Punk - One More Time`）；
- 但对当前用户的实际种子（`指尖笑 - 新·与天齐`、`Hyun - Grin`、`Kobaryo - Bookmaker` 等）返回：

```json
{"similartracks":{"track":[],"@attr":{...}}}
```

即 **Last.fm 没有这些曲目的相似数据**（听众/播放量不够）。实现拿到空数组就视为“无推荐”，没有兜底，所以四份列表全空，只剩相似艺人。

### 1.2 相似艺人没有头像

`artist.getSimilar` 返回的 `image` 多数是 Last.fm 的**默认占位图**：

```
https://lastfm-img.freetls.fastly.net/i/u/.../2a96cbd8b46e442fc41c2b86b821562f.png
```

（`2a96cbd8…` 是 Last.fm 的默认 star 图。）实现直接使用该 URL，或占位图加载失败后显示空白，看起来就是“没有头像”。

## 2. 修法

### 2.1 推荐兜底链（主要问题）

每个种子按顺序尝试，直到拿到结果：

1. `track.getSimilar(artist, title, limit 10)`；
2. 若为空 → `artist.getSimilar(seed.artist, limit 3)`，取相似艺人；
3. 对每个相似艺人 → `artist.getTopTracks(artist, limit 5)`；
4. 汇总后按“出现次数”排序，排除种子本身；
5. 若仍为空 → 该种子放弃（记 debug 日志，不计入失败）；
6. **全部种子都为空时**（冷门账号兜底，保证页面至少有一份歌曲推荐）：
   - 从种子艺人聚合 `artist.getTopTags`（`user.getTopTags` 对很多账号为空，不能只靠它），取前 5 个标签 → `tag.getTopTracks`（每个标签 10 首）；
   - 仍为空 → `chart.getTopTracks`（全站榜 20 首）；
   - 兜底结果单独标记 `source=fallback`，在页面上仍按普通推荐展示。

限制：

- 只对**前 10 个种子**执行兜底（避免请求爆炸）；
- 兜底每种子最多 1 次 `artist.getSimilar` + 3 次 `artist.getTopTracks`；
- 全部走现有并发 ≤3 / 间隔 ≥250ms 的限速；
- 结果进入现有 30 分钟缓存。

四份列表各自独立执行该链；综合列表仍按原权重合并。

### 2.2 相似艺人头像

- 识别 Last.fm 默认占位图（URL 包含 `2a96cbd8b46e442fc41c2b86b821562f` 或图片尺寸全为空）→ 视为**无头像**；
- 无头像时渲染首字母占位块（沿用项目现有占位样式），不显示破图；
- 可选（本期可不做）：用艺人名去 5 源搜索取一张源封面作为兜底。

### 2.3 空状态

- 四份推荐都为空时，先走 §2.1 第 6 步的全局兜底（tag / chart）；仍为空才显示「Last.fm 数据不足，暂时没有推荐」；
- 相似艺人仍正常显示。

## 3. 测试

**Go**

- `track.getSimilar` 返回空 → 触发 `artist.getSimilar` + `artist.getTopTracks`，结果非空；
- 兜底结果排除种子自身、去重、按出现次数排序；
- 兜底只对前 10 个种子执行，请求数符合上限；
- 全部为空 → 该种子无结果，不报错；
- 所有种子为空 → 触发 `user.getTopTags` + `tag.getTopTracks`，再空则 `chart.getTopTracks`；
- 兜底结果标记 `source=fallback`；
- 默认占位图被识别为“无头像”；
- 缓存命中时不重复请求。

**前端**

- 无头像渲染首字母占位块；
- 四份推荐为空时显示空状态文案；
- 相似艺人区块正常。

**人工**

1. 打开 Last.fm 页 → 至少有一份推荐有内容（用当前账号的种子）；
2. 相似艺人不再出现破图；
3. 请求数在限流范围内（看日志/网络）。

## 4. 验收

- 自动：§3 全绿；
- 人工：§3 三条成立；
- 当前账号下不再“只有相似艺人”。

## 5. 风险

- 兜底把请求量放大约 4 倍，必须严格限制种子数并复用缓存；
- 冷门曲目可能兜底后仍为空，属于数据限制，页面要能解释；
- `artist.getTopTracks` 返回的曲目仍需走 5 源匹配，匹配失败会继续丢弃（可能仍显少，属正常）。
