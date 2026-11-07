# 任务：五源「原生」歌词（lyric.fetch 扩展）

> 状态：待开工
> 前置：`docs/specs/cloud-search-sources/spec.md` 已实现（5 个搜索源）
> 参考实现：`reference/any-listen-extension-online-metadata/src/onlineResource/*/lyric.ts`
> 约束：AI 不做图形化验证

---

## 1. 背景与目标

云端搜索已支持 5 个源（wy/kg/kw/tx/mg），但插件的 `lyric.fetch` 只接受 `provider === "wy"`。
其余 4 个源下载后走宿主内置兜底（`lrclib` / `kugou` 内置 provider），按标题+艺人+时长匹配，日文、翻唱、同名版本会错配或取不到。

目标：让 **kg / kw / tx / mg 也返回平台原生歌词**，宿主仍然保留“插件失败 → 内置兜底”的降级链。

非目标：逐字歌词（word-by-word）展示、翻译/音译之外的格式、歌词编辑。

## 2. 现状（已核实）

宿主契约（`internal/domain/plugin.go`）：

```go
type PluginLyricRequest struct {
    Provider   string   `json:"provider"`
    ExternalID string   `json:"external_id"`
    Title      string   `json:"title,omitempty"`
    Artists    []string `json:"artists,omitempty"`
    Album      string   `json:"album,omitempty"`
    Duration   int      `json:"duration,omitempty"`   // 秒
}

type PluginLyricResult struct {
    Lyric  string `json:"lyric"`
    TLyric string `json:"tlyric,omitempty"`
    Source string `json:"source,omitempty"`
    Synced bool   `json:"synced"`
}
```

- 宿主会填好 title / artists / album / duration（`internal/app/plugin/lyrics.go`），插件可以直接用；
- 宿主**要求返回内容带时间轴**（`^\[\d{1,3}:\d{2}`），否则视为失败并降级；
- 降级链：插件 `lyric.fetch` → 内置 provider（`lrclib` / `kugou`）→ 标签内嵌歌词；
- 现状日志（tx 下载实测）：`lyric.fetch failed; falling back... source=lrclib-synced`。

插件侧（`plugins/wy/src/lyric.js`）：

- `PROVIDER = 'wy'`，`validateParams` 硬校验 provider 只能是 wy；
- `buildURL(externalID)` → `music.163.com/api/song/lyric`，解析 `lrc.lyric` / `tlyric.lyric`；
- `fixTimeLabels` 把 `[mm:ss:xx]` 归一成 `[mm:ss.xx]`；`hasTimeline` 校验时间轴；
- 返回 `{lyric, tlyric, source: 'wy', synced: true}`。

## 3. 逐源实现

> 参考路径前缀：`reference/any-listen-extension-online-metadata/src/onlineResource/`
> 每个源都有**自己的加密/编码格式**，必须逐字节移植参考实现，不要凭格式猜。

### 3.1 kg（酷狗）— 中等

| 项 | 内容 |
|---|---|
| 参考 | `kg/lyric.ts`、`kg/utils`（`decodeKrc`） |
| 搜索 | `GET http://lyrics.kugou.com/search?ver=1&man=yes&client=pc&keyword={title}&hash={external_id}&timelength={duration*1000}&lrctxt=1`（头同 kg 封面：`KG-RC`/`KG-THash`/`KuGou2012-…` UA） |
| 下载 | 取 `candidates[0]` 的 `id` + `accesskey`，`GET /download?ver=1&client=pc&id=&accesskey=&fmt=lrc|krc&charset=utf8` |
| 解码 | `fmt=krc` 时用 `decodeKrc`；`[language:…]` 里是 base64 的翻译/音译 JSON（`type: 0/1`） |
| 行时间 | 内容可能是 `[ms,dur]` 形式，需转成 `[mm:ss.xxx]` |
| 输入 | 需要 `title`、`external_id`（=hash）、`duration`（秒 → 毫秒） |

### 3.2 kw（酷我）— 中等

| 项 | 内容 |
|---|---|
| 参考 | `kw/lyric.ts`、`kw/decodeLyric.ts`、`kw/util.ts`（`lrcTools`、`sortLrcArr`、`transformLrc`） |
| 请求 | `GET http://mlyric.kuwo.cn/mobi.s?f=web&type=lyric&lrcx={0|1}&rid={external_id}&encode=utf8`，需要**原始响应体**（`needRaw`） |
| 解码 | `decodeLyric(raw, lrcx)` 处理酷我私有编码；`lrcx=1` 才有逐字/翻译信息 |
| 输出 | 解析 `[mm:ss.xx]` 行，翻译行去词时间标记；无时间轴视为失败 |

### 3.3 tx（QQ 音乐）— 较难

| 项 | 内容 |
|---|---|
| 参考 | `tx/lyric.ts`、`tx/qrcDecode.ts`、`tx/musicInfo.ts` |
| 前提 | 接口要 **songID（数字 id）**，而搜索行的 `external_id` 是 `mid`。参考做法：`getMusicInfo(mid)` 取 `meta.songId`（多一次请求），或宿主请求里没有 songId 时按同样方式补取 |
| 请求 | `POST https://u.y.qq.com/cgi-bin/musicu.fcg`，body `{comm:{ct:'19',cv:'1859',uin:'0'}, req:{module:'music.musichallSong.PlayLyricInfo', method:'GetPlayLyricInfo', param:{format:'json', crypt:1, qrc:1, roma:1, trans:1, songID: <number>, …}}}`，`Referer: https://y.qq.com` |
| 响应 | `req.data.lyric` / `.trans` / `.roma` 是 **加密的 QRC/Base64**，用 `decodeQrc` 解密 |
| 输出 | 解出的 LRC 可能仍带 `[ms,dur]` 行时间与 `(ms,dur)` 词时间，需转换/剔除 |

### 3.4 mg（咪咕）— 较难

| 项 | 内容 |
|---|---|
| 参考 | `mg/lyric.ts`、`mg/utils/mrc.ts`（`decrypt`）、`mg/musicDetail.ts` |
| 前提 | 先用 `getMusicInfo(songId)` 拿歌词地址（`lrcUrl` / `trcUrl` / `mrcUrl`），再按 URL 取文本 |
| 请求 | 歌词文本：`GET <url>`，头 `Referer: https://app.c.nf.migu.cn/`、`channel: 0146921`、移动端 UA |
| 解码 | MRC 格式需要 `decrypt`；LRC 格式直接用 |
| 输出 | `[ms,dur]` 转 `[mm:ss.xxx]`，词时间 `(ms,dur)` 去掉；翻译来自 `trcUrl` |

### 3.5 wy（已实现）

保留现有实现与测试；只把结构改成“按 provider 分派”。

## 4. 改造点

### 4.1 插件（`plugins/wy/src/lyric.js`）

```js
const FETCHERS = {
  wy: fetchWY,
  kg: fetchKG,
  kw: fetchKW,
  tx: fetchTX,
  mg: fetchMG,
};
```

- `validateParams`：provider 允许 5 个值，未知 provider 仍报 `InvalidParamsError`；
- 每个 `fetchX(request)` 返回 `{lyric, tlyric?, source: '<provider>', synced: true}`；
- 统一在返回前过 `fixTimeLabels` / `hasTimeline`；无时间轴 → 抛 `SearchError(kind: 'unavailable')`（让宿主降级）；
- 新增 `buildLyricURL` / `decode` 等函数时保持可注入（`fetchImpl`），保证单测不联网；
- `wrapper.js` 无需改动（`lyric.fetch` 已声明），只需确保传入的是完整 request（provider/external_id/title/artists/album/duration）。

### 4.2 宿主

**不改**。`internal/app/plugin/lyrics.go` 已经传齐字段；降级链已存在。

### 4.3 manifest

**不改**（`lyric.fetch` 已在 capabilities；没有新的网络域名？有——需把各平台歌词域名加入 `permissions.network`）：

| 源 | 追加域名 |
|---|---|
| kg | `lyrics.kugou.com` |
| kw | `mlyric.kuwo.cn` |
| tx | `u.y.qq.com`（已有，歌词走同一主机）、`c.y.qq.com`（若补取 songId 走 `musicInfo`） |
| mg | `app.c.nf.migu.cn`、`d.musicapp.migu.cn`、`jadeite.migu.cn`（详情/歌词地址） |

## 5. 测试

`plugins/wy/test/lyric.test.js`（新增或扩展）：

- 每源至少三条：
  1. **真实样本**：把参考实现抓到的一份响应（base64/加密字符串）放进 fixture，断言解出的 LRC 首行与时间轴正确；
  2. **失败**：HTTP 429 → `rate_limited`；空内容/无时间轴 → `unavailable`；未知 provider → `InvalidParamsError`；
  3. **参数使用**：kg 断言 `timelength = duration*1000`；tx 断言会先取 songId；mg 断言取歌词 URL 的顺序。
- 回归：`cd plugins/wy && node --test`（当前 70 passed，目标 ≥ 80）。

联调（不依赖 GUI）：

```bash
cd plugins/wy && node -e "
const { createLyricFetcher } = require('./src/lyric.js');
createLyricFetcher({})({ provider: 'tx', external_id: '00128N3r2SYKMF', title: '兰亭序', artists: ['周杰伦'], duration: 256 })
  .then(r => console.log(r.source, r.synced, r.lyric.slice(0, 40)))
  .catch(e => console.log('ERR', e.kind, e.message));
"
```

## 6. 验收

自动：§5 全绿。

人工：分别用 tx / kw / kg / mg 下载一首歌，检查：

1. 日志出现 `download: lyrics saved` 且 `source` 是该 provider（**不再是 `lrclib-synced`**）；
2. 播放器里歌词能滚动、时间轴对齐；
3. 某源歌词确实不存在时，自动降级到 LRCLIB（日志有 `falling back`），不阻塞下载。

## 7. 风险与假设

- **加密格式是唯一难点**：krc / qrc / mrc 各有私有算法，参考实现里已有，必须原样移植并用真实样本验；无法移植时保留降级，不要交付“看起来有歌词但时间轴错”的实现。
- **tx 需要额外一次详情请求**（mid → songID），会多一次网络往返；可在同一次下载里缓存。
- **上游漂移**：歌词接口比搜索更容易变更；失败必须走 `unavailable` 让宿主降级，不能抛未分类错误。
- **网络白名单**：新增域名如果没加进 manifest，调用会被宿主拒绝（表现为 `auth`/`unavailable`），部署时一起改。
- **插件部署**：`plugins/` 不入库，改完需同步到 `%LOCALAPPDATA%\airmedy\plugins\wy` 并重启。
