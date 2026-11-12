# Bug：常听曲目列表为空（Last.fm 的 artist 字段有两种 JSON 形状）

> 状态：✅ 已完成（代码已合入 wip/stream-playback）
> 现象：Last.fm 页显示「数据不足，暂时没有推荐」，但 `user.getTopTracks` 实际返回 200 且有数据
> 范围：Last.fm 客户端解析；不改页面/匹配

---

## 1. 现象与证据

- 应用日志没有请求错误（没有 `last.fm ... EOF`）；
- 页面进入空状态（`items` 为空，`status=ok`）；
- 用同一个 key 直接请求：

```json
{
  "toptracks": {
    "track": [
      { "name": "新·与天齐",
        "artist": { "name": "指尖笑", "mbid": "69a5aa40-..." },
        ... }
    ]
  }
}
```

注意 `artist` 是 **`{"name": ...}`**。

## 2. 根因

`internal/app/lastfm/client.go` 的 wire 结构只认 `#text`：

```go
type lastFmArtistRef struct {
    Text string `json:"#text"`
    MBID string `json:"mbid"`
}
```

但 Last.fm 不同接口的 JSON 形状不同：

| 接口 | artist 字段 |
|---|---|
| `user.getRecentTracks` | `{"#text": "指尖笑", "mbid": "..."}` |
| `user.getTopTracks` | `{"name": "指尖笑", "mbid": "..."}` |
| `track.getSimilar` / `artist.getTopTracks` | `{"name": "..."}` |

于是 `toTrack()` 拿到的 `Artist` 是空串 → `usableTrack()`（要求 artist 和 title 都非空）把所有行都丢掉 → 接口无错误、列表为空 → 页面显示「数据不足」。

这也能解释之前“只剩相似艺人”：曲目相关的解析全部受影响。

## 3. 修法

1. `lastFmArtistRef` 增加 `Name`：

```go
type lastFmArtistRef struct {
    Text string `json:"#text"`
    Name string `json:"name"`
    MBID string `json:"mbid"`
}
```

2. `toTrack()` 取值顺序：`Name` → `Text`（两者都可能出现，不能只认一个）：

```go
artist := strings.TrimSpace(w.Artist.Name)
if artist == "" {
    artist = strings.TrimSpace(w.Artist.Text)
}
```

3. 同样检查 `album` 字段：`getTopTracks` 没有 album；其它接口可能用 `{"#text": ...}`，保持现状即可。
4. 增加“**真实响应形状**”的解析测试，而不是只用一种 fixture：
   - `artist: {"name": "X"}`（topTracks 形状）
   - `artist: {"#text": "X"}`（recentTracks 形状）
   - 两者同时存在时的优先级。

## 4. 测试

- 解析 `toptracks` 形状（`name`）→ 返回非空曲目；
- 解析 `recenttracks` 形状（`#text`）→ 仍正常；
- 两者都有 → 优先 `name`；
- 缺 artist → 该行被过滤（保持现状）；
- 现有 client/推荐测试全部通过。

## 5. 验收

- 自动：§4 全绿；
- 人工：打开 Last.fm 页 → 常听曲目列表非空；匹配状态随后更新。

## 6. 风险

- 其它字段（album/image/duration）也可能存在 shape 差异，本轮只修 artist；如果同样出现空行再按同一方式处理；
- 测试 fixture 应尽量贴近真实响应，避免再次出现“单测通过、真实为空”。
