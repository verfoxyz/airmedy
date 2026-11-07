# Spec：云端搜索源扩展（kw / tx / mg）

> 状态：待开工
> 参考实现：`reference/any-listen-extension-online-metadata`（本地已 clone，逐源对照）
> 前置：`docs/specs/cloud-search-paging/spec.md` 已实现（5+10 分页、行级懒加载、`MAX_LIMIT=100`）
> 后续修订：tx 搜索返回 0 行（响应层级读错）见 [`fix-tx-response-shape.md`](fix-tx-response-shape.md)。
> 后续任务：五源原生歌词见 [`native-lyrics.md`](native-lyrics.md)。
> 约束：AI 不做图形化验证；观感由人工确认

---

## 1. 目标

聚合插件 `plugins/wy` 的搜索源从 2 个扩到 5 个：

| 源 | 名称 | 现状 |
|---|---|---|
| `wy` | 网易云音乐 | 已有 |
| `kg` | 酷狗音乐 | 已有（内部翻页 30/页） |
| `kw` | 酷我音乐 | **新增** |
| `tx` | QQ 音乐 | **新增** |
| `mg` | 咪咕音乐 | **新增** |

每个新源都要像现有源一样：返回 `catalog.search` 行（`externalId` / `title` / `artists` / `album` / `duration` + 封面）、参与多源合并与分页、失败按 `kind` 分类且不影响其它源。

### 非目标

- **下载/播放**：聚合插件不声明 `stream.resolve`，新增源的行可以搜到，但下载仍需要声明了对应 provider 的解析插件。是否补解析能力单独立项。
- 歌词：`lyric.fetch` 目前只覆盖 wy，不在本单扩展。
- 榜单、歌单、评论、歌手/专辑页。

## 2. 现状（已核实）

- `plugins/wy/src/search.js`
  - `PROVIDERS` / `SOURCE_NAMES` / `SEARCH_SOURCES` 三个常量驱动 `describe` 的源表；
  - `createSearcher()` 里 `bySource = { wy: searchWY(...), kg: searchKG(...) }` 路由；
  - 每源函数签名统一 `search(fetchImpl, timeoutMs) → async (request) => {rows, hasMore}`；
  - `MAX_LIMIT = 100`，kg 用 `KUGOU_PAGE_SIZE = 30` 内部翻页；
  - 行形状：`{externalId, title, artists[], album, duration, vip, cover_url, cover_url_large}`；
  - `include_cover === false` 时不做封面请求。
- `plugins/wy/airmedy-plugin.json`：`sources` 列表 + `permissions.network` 白名单，宿主启动时校验。
- 宿主不需要改动：源列表来自插件 `describe`，`SearchSources` 直接遍历；分页/合并/评分与源数量无关。

## 3. 每源实现要点

> 以下字段均以参考实现的对应文件为准；移植时**逐行对照**，不要按记忆重写。
> 参考路径统一前缀 `reference/any-listen-extension-online-metadata/src/onlineResource/`。

### 3.1 kw（酷我）

| 项 | 值 |
|---|---|
| 参考文件 | `kw/musicSearch.ts`、`kw/pic.ts` |
| 搜索 | `GET http://search.kuwo.cn/r.s?client=kt&all={query}&pn={page-1}&rn={limit}&uid=794762570&ver=kwplayer_ar_9.2.2.1&vipver=1&show_copyright_off=1&newver=1&ft=music&cluster=0&strategy=2012&encoding=utf8&rformat=json&vermerge=1&mobi=1&issubtitle=1` |
| 响应 | `abslist[]`；`TOTAL` / `SHOW` 判断是否有更多 |
| 行映射 | `externalId = MUSICRID.replace('MUSIC_','')`；`title = SONGNAME`；`artists = ARTIST` 拆分；`album = ALBUM`；`duration = parseInt(DURATION)` |
| 封面 | 搜索**不带图**。逐行 `GET http://artistpicserver.kuwo.cn/pic.web?corp=kuwo&type=rid_pic&pictype=500&size=500&rid={id}`，响应体是 URL 文本；`http://…kwcdn.kuwo.cn` 要替换成 `https://…kuwo.cn` |
| 分页 | `pn` 从 0 开始；`rn` 的实际上限需实测（参考默认 30） |

封面是逐行请求：本页最多 `cover_batch` 行（现在页大小 5/10），串行 + 总预算 2s，失败静默；`include_cover=false` 时跳过。

### 3.2 tx（QQ 音乐）

| 项 | 值 |
|---|---|
| 参考文件 | `tx/musicSearch.ts`、`tx/utils/index.ts`、`tx/utils/crypto.ts`、`tx/pic.ts` |
| 搜索 | `POST https://u.y.qq.com/cgi-bin/musics.fcg?sign={zzcSign(JSON.stringify(payload))}`，头 `User-Agent: QQMusic 14090508(android 12)` |
| body | `comm` + `req`（`module: music.search.SearchCgiService`、`method: DoSearchForQQMusicDesktop`、`param: {grp, num_per_page, page_num, query, search_type, searchid, ...}`），完整字段见参考 |
| 签名 | `zzcSign`：`sha1(text)` → 按固定索引取两段 + 固定常量与哈希字节异或 → base64 去 `\/+=`，拼 `zzc…`。Node 用 `crypto.createHash('sha1')`，**必须逐字节对齐**参考常量 |
| 行映射 | `req.data.body.song.list[]` → `externalId = mid`；`title = title`；`artists = singer[].name`；`album = album.name`；`duration = interval`（秒） |
| 封面 | 由 `album.mid` 拼：`https://y.gtimg.cn/music/photo_new/T002R500x500M000{albumMid}.jpg`；`album.mid` 为空或 `'空'` 时退回歌手 `T001R500x500M000{singer[0].mid}.jpg`；都没有则不设封面。**无需额外请求** |
| 分页 | `num_per_page` 上限实测（参考 30）；`page_num` 从 1 开始 |

### 3.3 mg（咪咕）

| 项 | 值 |
|---|---|
| 参考文件 | `mg/musicSearch.ts`、`mg/utils/index.ts`、`mg/pic.ts` |
| 搜索 | `GET https://jadeite.migu.cn/music_search/v3/search/searchAll?isCorrect=0&isCopyright=1&searchSwitch=…&pageSize={limit}&text={query}&pageNo={page}&sort=0&sid=USS` |
| 必需头 | `uiVersion: A_music_3.6.1`、`deviceId`、`timestamp`、`sign`、`channel: 0146921`、移动端 UA |
| 签名 | `sign = md5(`${query}${signatureMd5}yyapp2d16148780a1dcc7408e06336b98cfd50${deviceId}${timestamp}`)`，`deviceId`/`signatureMd5` 为参考里的固定常量 |
| 响应 | `code === '000000'`；`songResultData.resultList`、`totalCount` |
| 行映射 | `externalId = songId`；`title = songName`；`artists = artists[].name`；`album = album`；`duration = parse('mm:ss')` |
| 封面 | `albumImgs[0].img`，相对路径补 `http://d.musicapp.migu.cn` 前缀；搜索自带，无需额外请求 |
| 分页 | `pageSize` 上限实测（参考 20）；`pageNo` 从 1 开始 |

## 4. 公共改动

1. **源表**：`PROVIDERS`、`SOURCE_NAMES`、`SEARCH_SOURCES` 增加 `kw` / `tx` / `mg` 及中文名。
2. **路由**：`createSearcher` 的 `bySource` 增加三项；`validateParams` 里对源的校验随之放宽。
3. **manifest**：
   - `sources` 加 3 项；
   - `permissions.network` 追加：`search.kuwo.cn`、`artistpicserver.kuwo.cn`、`u.y.qq.com`、`y.gtimg.cn`、`jadeite.migu.cn`、`d.musicapp.migu.cn`。
4. **失败分类**（沿用现有 `Kind`）：
   - HTTP 429 / 403 → `rate_limited`；
   - 超时、连接失败、5xx → `unavailable`；
   - 签名被拒 / 需要登录（如 tx 返 `code != 0`、mg 非 `000000`）→ `auth`（wording 由宿主决定）。
5. **内部翻页**：与 kg 相同策略——从上限 100 的窗口出发，按上游单页上限循环补齐并去重；循环次数封顶，不死循环。
6. **查询构造**：宿主传原始 query，不要像参考那样自拼 `name+artist+album`（那是它自己的调用方行为）。

## 5. 测试（`plugins/wy/test`，cwd `plugins/wy`）

每源一组伪 `fetchImpl` 用例：

- 请求形状：URL/方法/头/body 与参考一致（tx 断言 `sign` 前缀 `zzc`；mg 断言 `sign`/`deviceId`/`timestamp` 存在）；
- 字段映射：`externalId`、标题、艺人拆分、时长、专辑；
- 封面：tx/mg 直接得到 URL；kw 用伪响应返回图片 URL 并断言只对本页行请求；
- 失败：429 → `rate_limited`、超时 → `unavailable`、签名拒绝 → `auth`；
- 分页：伪上游每页 N 条，断言循环次数与去重；
- `include_cover=false` 时不发封面请求。

回归：

```bash
cd plugins/wy && node --test
node scripts/check-docs.mjs        # 若同时更新了 catalog/plugins 文档
```

> 宿主与前端没有改动，`go test` / `vitest` 只需跑一遍确认无回归。

## 6. 验收

自动：§5 全绿。

人工（由人做）：

1. 搜索页出现 5 个源按钮（wy/kg/kw/tx/mg）；
2. 逐源单选搜索：都能出 5 行结果，封面正常（kw 可能少量缺失）；点「加载更多」到 15/25…；
3. 全选：合并列表按现有规则排序、无重复、源标签正确；
4. 断掉某个源（比如 tx 签名被拒）→ 只有该源显示失败提示，其它源结果不受影响；
5. 分页到 100 行后按钮消失。

## 7. 风险与假设

- **签名是硬编码**：tx/mg 的常量随上游更新会失效，失效时表现为 403/401 或空结果；需要在错误里保留上游响应片段，便于排查。
- **反爬**：kw/tx/mg 可能要求 cookie/设备指纹；若线上返回空，按参考实现的头部逐项对齐，不要自创参数。
- **kw 封面是逐行请求**：串行 + 预算，失败静默；页面行数 5/10，不会拖垮搜索。
- **上游单页上限**：kw/tx/mg 各不同，必须实测后再定 `PAGE_SIZE` 常量；与 kg 一样，窗口 100 时最多 4 页。
- **下载不可用**：新源行只能搜、不能下（无 `stream.resolve`）。需要先确认用户接受这一点，否则要单独立项补解析插件。
- **合规**：只做用户发起的搜索；不抓取播放地址、不绕过付费。

## 8. 执行提示

- 先读参考实现对应文件，逐源移植；**不要动** wy/kg 的现有实现（除了把它们纳入新的公共结构）。
- 一次加一个源：kw → tx → mg，每加一个跑一次 `node --test`，失败隔离更难在大改动里定位。
- 插件不入库（`plugins/` 在 `.gitignore`），完成后同步到 `%LOCALAPPDATA%\airmedy\plugins\wy` 并重启应用；manifest 的 `sources`/`permissions` 改动必须让宿主重新加载插件。
- 改完同步 `catalog/plugins/README.md` 的源列表与能力说明（英文）。
