# 修复：流缓存相关的三处封面缺口

> 状态：待开工
> 现象来源：人工验收（2026-11-11）
> 前置：M4–M6 已实现（含 T8 流播放封面）

---

## 1. 现象

1. **首页等列表**：播放/缓存过的流曲目（库里 remote 行）不显示封面；
2. **歌曲信息抽屉**：切歌后变成无封面；
3. **提升入库**：加入歌单/收藏后落成永久文件，文件没有封面。

## 2. 根因

### 2.1 列表无封面

流曲目的 `tracks.artwork_key` 是空的——封面只存在于 `cover_cache`（按 URL hash + identity），没有进入 artwork 管线。列表组件统一用 `buildArtworkUrl(track.artwork_key)`，所以必空。

### 2.2 抽屉切歌后无封面

`TrackInfoDrawer` 读 `store.trackInfoTrack`，而它只在 `openTrackInfo()` 时赋值，**不跟随 `currentTrack`**。T8 的修复只在「抽屉显示的曲目 == 当前曲目」时用 `status.stream_artwork_url`；切歌后抽屉仍显示旧远程曲目 → 走本地 `artwork_key` → 空。

### 2.3 提升后文件无封面

`promote.go` 取封面用的是内存里的 `s.coverURL(track.ID)`（catalog 的 `rememberCovers`），重启后或本次没有搜索过该行时为空。而 `cover_cache` 只存了 `url_hash/path/mime/last_used/provider/external_id`，**没有存远端 URL**，拿不到图交给 finalizer。

---

## 3. 修法

### 3.1 封面物化（解决 1，也为 2、3 铺路）

把流曲目的封面**物化进 artwork 管线**，写回 `tracks.artwork_key`，之后所有现有 UI 自动生效。

1. 迁移 `000093`：`cover_cache` 增加 `url TEXT NOT NULL DEFAULT ''`，`upsert` 时写入远端 URL；
2. `covercache` 新增：
   - `RemoteForIdentity(ctx, provider, externalID) (string, error)`
   - `BytesForIdentity(ctx, provider, externalID) (data []byte, mime string, err error)`（直接读已缓存文件，避免二次下载）
3. 新增物化步骤（建议在 `catalog.EnsureStreamableTrack` 与播放器加载远程曲目时各调一次）：
   - `track.artwork_key` 为空 && provider/external_id 有效 → `BytesForIdentity` → `artwork.Save(data, mime)` → 得到 key；
   - 更新 track 行 `artwork_key`，emit `library:track-updated`；
   - 幂等：已有 `artwork_key` 直接跳过。

> 这样首页、列表、抽屉、播放器全部走现有 artwork 管线，不需要给每处加 fallback。

### 3.2 抽屉不跟随，封面按“所显示曲目自己的身份”解析（解决 2）

**抽屉保持打开时的那首曲目，不跟随 `currentTrack`。** 切歌只影响播放，不改变抽屉内容。

封面也不再依赖“它是不是当前播放曲目”，而是按该曲目自己的 identity 解析：

1. 优先 `track.artwork_key`（3.1 物化后的结果）；
2. 若为空且是 remote 行 → 调用新绑定 `ResolveTrackCover(trackID) string`：
   - 后端用 `covercache.AssetForIdentity(provider, externalID)` 取本地资产 URL；
   - 该 identity 还没缓存封面时，用 `covercache` 的 URL 下载一次（`cover_cache.url` 列，见 3.1）；
   - 返回 `/cover-cache/<hash>.<ext>`，失败返回空串（前端回退占位图）；
3. 前端在 store 里按 `trackID` 缓存解析结果（`Map<trackID, url>`），重复打开不再请求；
4. 抽屉的 `artworkUrl` 改为：`resolveCache[track.id] ?? buildArtworkUrl(track.artwork_key, 'md')`，**删除“当前播放曲目才用 stream_artwork_url”的特判**。

这样：切歌后抽屉仍显示原曲目及其封面；重新打开另一首时按它的身份解析。

### 3.3 提升时带封面（解决 3）

- `promote.go`：
  1. 优先 `s.coverURL(track.ID)`（本次搜索记住的远端 URL）；
  2. 否则 `covercache.RemoteForIdentity(provider, externalID)`；
  3. 若物化已产生 `artwork_key`，把 artwork 文件路径交给 finalizer（见下）。
- `FinalizeInput` 增加 `CoverPath string`（本地封面文件）；`attachCover` 优先读 `CoverPath`，没有再 `Fetch(CoverURL)`。
- finalizer 写标签时同时写 `artwork_key`，保证入库后列表也能显示。

### 3.4 不做

- 不改封面淘汰/关联规则（T3 已完成）；
- 不为 remote 行引入新的 DTO 字段（物化后不需要）。

---

## 4. 测试

**Go**

- `covercache`：`url` 列写入/读出；`RemoteForIdentity`、`BytesForIdentity` 命中与未命中；
- 物化：远程行保存后 `artwork_key` 非空；重复调用不重复 `Save`；
- `promote`：有远端 URL 时 `MetadataUpdate.ArtworkData` 非空；只有本地 `CoverPath` 时也能内嵌；
- 迁移 `000093` up/down。

**前端**

- `TrackInfoDrawer.spec.ts`：抽屉打开后切歌 → 仍显示原曲目及其封面（不跟随）；
- 打开另一首 remote 曲目 → 调用 `ResolveTrackCover` 并显示返回的封面；
- 现有 artwork 用例保持通过。

**人工**

1. 播放一首云端曲目 → 首页/列表里该行出现封面；
2. 打开歌曲信息 → 有封面；切下一首 → 抽屉仍是原曲目、封面不变；关闭后打开新曲目 → 显示新曲目的封面；
3. 加歌单提升完成后 → 文件内嵌封面、库里显示封面；
4. 升级后旧 remote 行：再次播放时补齐封面。

---

## 5. 风险

- 物化会把封面复制进 artwork 缓存，占用少量额外磁盘（可被现有 artwork 清理回收）；
- `cover_cache` 新增 `url` 列后，历史行该列为空 → 提升旧曲目时仍可能没有远端 URL（但物化后的 `artwork_key` 可以兜底）；
- 远程行 `artwork_key` 一旦写入，之后更换封面需要走现有「重新获取封面」逻辑，和本地曲目一致。
