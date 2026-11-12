# Bug：从 Last.fm 推荐播放时，左下角与歌曲详情没有封面

> 状态：✅ 已完成（代码已合入 wip/stream-playback）
> 现象：Last.fm 页那一行有缩略图，但点播放后左下角/歌曲详情没有封面
> 范围：流播放前的封面预热；不改 Last.fm 取数

---

## 1. 根因

播放器的封面解析按 **identity** 走封面缓存：

- `PlayerStatus.stream_artwork_url` = `covercache.AssetForIdentity(provider, external_id)`；
- 首次加载远程曲目时用 `BytesForIdentity` 物化到 `artwork_key`；
- 歌曲详情用 `ResolveCatalogTrackCover(trackID)`，同样按 identity 查。

但 Last.fm 推荐页**没有走搜索页的 `GetCatalogCovers`**：

- 行里的 `cover_url` 只是 Last.fm 或匹配源给的原始 URL；
- `EnsureStreamableTrack` 只把 `CoverURL` 传给 `EnsureRemoteTrack`（用于将来下载时写标签），**没有把它写进封面缓存**；
- 于是 `covercache` 里没有这个 identity 的记录 → 播放器/详情按 identity 查不到 → 没有封面。

Last.fm 页那一行能显示缩略图，是因为它直接用 `item.cover_url` 渲染，不经过封面缓存。

## 2. 修法

### 2.1 播放前预热封面缓存

`EnsureStreamableTrack`（catalog）在拿到非空 `CoverURL` 时，best-effort 调用封面缓存。

封面 URL 的选择（与推荐页一致）：

1. Last.fm 的**真实图片**（非默认占位图）→ 用它；
2. Last.fm 是占位图 → 用命中音乐源的封面；
3. 都没有 → 不预热，保持无封面。

> Last.fm 图最大约 300×300；按用户要求优先使用。

```go
// Warm the identity → cover mapping so the player and the track info drawer
// resolve the artwork by identity without a catalogue page request.
// coverURL is the Last.fm real image when present, otherwise the matched
// source cover.
if coverURL != "" && s.coverAssets != nil {
    if _, err := s.coverAssets.Get(ctx, coverURL, provider, externalID, maxCoverBytes); err != nil {
        s.logger.Debug("catalog: cannot warm the cover cache", ...)
    }
}
```

- 失败不影响播放；
- 需要给 catalog service 注入现有的 `CoverAssets` 端口（`PlayerStreamCoverProvider` 同一实现）。

### 2.2 兜底：没有 cover URL 的旧行

- 已存在的 remote 行如果从未缓存过封面，播放时会再次调用 `EnsureStreamableTrack`（带该行的 cover URL，如果来源提供）；
- 如果连 URL 都没有（旧数据），保持无封面，不阻塞播放；
- 可选（本期不做）：在 `stream_cache` 里存 `cover_url`，重启后也能预热。

### 2.3 前端

- 不需要改：播放器状态与详情已经优先用 `stream_artwork_url` / `ResolveCatalogTrackCover`；
- Last.fm 推荐行（播放前）继续显示原始缩略图。

## 3. 测试

**Go**

- `EnsureStreamableTrack` 带 `CoverURL` → 调用 `coverAssets.Get`，并写入 identity 映射；
- `CoverURL` 为空 → 不调用；
- `Get` 失败 → 不回滚注册、播放路径不报错；
- 预热后 `AssetForIdentity` 能返回本地封面 URL。

**人工**

1. 从 Last.fm 页点播放 → 左下角有封面；
2. 打开歌曲详情 → 有封面；
3. 未匹配/无封面的行仍可播放，只是没有封面。

## 4. 验收

- 自动：§3 全绿；
- 人工：§3 三条成立。

## 5. 风险

- 预热会额外发一次封面下载请求（每首一次，量小）；
- 封面 URL 失效时保持无封面（可重试逻辑暂不做）；
- 不要把它做成阻塞播放：`Get` 有超时（现有 20s 太长，建议预热时用 5s 上限）。
