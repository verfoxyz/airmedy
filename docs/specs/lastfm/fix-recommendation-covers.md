# Bug：推荐行没有封面（Last.fm 默认占位图 / 命中源无封面）

> 状态：待开工
> 现象：Last.fm 常听曲目里某些行（如 `Synthetic Fracture - Gardens`）没有封面
> 范围：Last.fm 推荐页的封面来源与占位

---

## 1. 根因

已实测该曲目在 `user.getTopTracks` 的返回：

```json
"image": [
  { "#text": "https://lastfm-img.freetls.fastly.net/i/u/34s/2a96cbd8b46e442fc41c2b86b821562f.png" },
  ...
]
```

`2a96cbd8…` 是 **Last.fm 的默认星形占位图**（不是真实封面）。同时 `album` 字段为空。

有两种“没有封面”的情况：

1. **未匹配行**：直接用 Last.fm 的 `image`，而它是占位图 → 显示占位或空白；
2. **已匹配行**：封面取自命中源的行，但部分源（尤其 `kg`/`kw` 的某些结果）`cover_url` 为空 → 命中后也没有封面。

前端没有把“占位图 URL”当缺失处理，也没有兜底来源。

### 1.1 封面优先级（已确认）

1. **Last.fm 返回真实图片** → 用 Last.fm 的图；
2. Last.fm 是默认星形占位图（`2a96cbd8…`）→ 用**命中音乐源**的封面；
3. 都没有 → 占位块。

> Last.fm 的图最大约 300×300，分辨率有限，但按用户要求优先使用。

## 2. 修法

### 2.1 后端：区分“真实图”与“默认占位图”

- `usableTrack` / 行转换时：若 `image` URL 包含 `2a96cbd8b46e442fc41c2b86b821562f`，视为**占位图**（`CoverURL=""`，但保留 `HasLastFmCover=false` 语义）；
- 其它 Last.fm 真实图片照常使用，且**优先于命中源的封面**。

### 2.2 后端：占位图时改用命中源封面（或封面缓存）

- 命中后：
  - Last.fm 有真实图 → **保留 Last.fm 图**，忽略源封面；
  - Last.fm 是占位图 / 无图 → 用命中行的 `cover_url_large || cover_url`；
  - **本地命中行（`in_library`）没有 cover_url** → 用该行 `artwork_key` 生成 `/artwork/<key>`（md/sm 变体）；
  - 命中行也没有封面 → 用 `provider + external_id` 查 `covercache.AssetForIdentity` → `/cover-cache/<hash>.jpg`；
  - 仍没有 → 保持空，前端显示占位块。
- 未匹配 → 保持空，前端显示占位块。

### 2.3 前端：统一占位

- `cover_url` 为空、或仍是 Last.fm 占位图 URL → 渲染**首字母/音符占位块**（与相似艺人一致），不显示破图；
- 本地命中行的封面来自后端给的 `/artwork/<key>`，不需要前端额外请求；
- 匹配成功后若新事件带来封面，替换占位；
- 不新增网络请求（封面来自后端字段）。

## 3. 测试

**Go**

- Last.fm 占位图 URL 被识别并置空；
- 真实图片 URL 保留；
- 命中行无 `cover_url` 时从 `covercache.AssetForIdentity` 取到本地 URL；
- 缓存没有该 identity 时保持空，不报错。

**前端**

- `cover_url` 为空 → 渲染占位块；
- 占位图 URL（`2a96cbd8…`）也被当作空；
- 有封面时正常渲染；
- 事件更新后占位被替换。

**人工**

1. `Synthetic Fracture - Gardens` 行不再显示空白/星形图，而是占位块；
2. 命中且源有封面的行正常显示；
3. 命中但源无封面的行显示占位，不破图。

## 4. 验收

- 自动：§3 全绿；
- 人工：§3 三条成立。

## 5. 风险

- Last.fm 占位图的 hash 判断是硬编码，若 Last.fm 更换占位图需要更新；
- 封面缓存命中率取决于该 identity 是否被 `GetCatalogCovers` 缓存过；没有就显示占位（可接受）。
