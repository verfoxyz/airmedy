# Bug：remote 行左下角不显示歌手、专辑点不开、详情缺信息

> 状态：✅ 已完成（代码已合入 wip/stream-playback）
> 现象：播放 `remote://wy/2731039681` 时，左下角没有歌手，专辑无法打开，曲目详情只显示少数几项
> 范围：remote/未入库行的展示；不改入库/合并逻辑

---

## 1. 现象

- 播放器左下角：歌手位置显示占位文案（「选择曲目」），而不是 `かめりあ; 重音テト`；
- 专辑：显示/点击都没有反应；
- 歌曲详情抽屉：只有「艺术家 / 播放次数 / 时长 / 文件路径」等，**没有专辑**；头部也没有艺人/专辑行。

## 2. 根因

remote 行只有**文本快照**，没有实体关联：

| 字段 | remote 行 | 本地行 |
|---|---|---|
| `raw_artist_names` | 有（`かめりあ; 重音テト`） | 有 |
| `raw_album_name` | 有 | 有 |
| `artists []`（`Artist` 实体） | **空** | 有 |
| `album`（`Album` 实体） | **空** | 有 |

而 UI 各处只读实体：

1. `PlayerFooter.vue` 的 `trackArtist`：只读 `currentTrack.artists`，为空 → 显示「选择曲目」；
2. `TrackInfoDrawer.vue` 头部：用 `raw_album_artist_names`（remote 行通常为空）+ `album?.title`（空）→ 头部没有信息；
3. 专辑跳转：用 `album.id`（`/albums/:id`），remote 行没有 → 点了没反应；
4. 详情列表：没有「专辑」项，只有 `raw_artist_names`。

## 3. 修法

原则：**有实体用实体，没有实体用文本快照；没有实体就不做可点击链接**。

### 3.1 左下角歌手

`PlayerFooter.vue`：

```ts
const trackArtist = computed(() => {
  const track = store.currentTrack
  if (!track) return t('player.not_playing')
  const names = (track.artists ?? [])
    .filter((a): a is NonNullable<typeof a> => a !== null)
    .map(a => a.name)
  if (names.length > 0) return names.join(', ')
  const raw = track.raw_artist_names?.trim()
  if (raw) return raw
  return t('library.unknown_artist')
})
```

（占位文案只在没有当前曲目时出现。）

### 3.2 详情抽屉头部

- 艺人：`artists` 名字 → 否则 `raw_artist_names` → 否则 `unknown_artist`；
- 专辑：`album?.title` → 否则 `raw_album_name`；
- 两者都有时中间用 `•` 分隔；都为空则不显示这一行。

### 3.3 详情列表

新增一行「专辑」：`album?.title || raw_album_name`（i18n key `track_info.album`，若已存在则复用）。

### 3.4 专辑跳转

- 统一规则：**只有 `album?.id` 非空**才渲染可点击链接；
- remote 行渲染为普通文本（无 hover、无点击），可加 `title` 提示「曲目尚未入库」；
- 涉及位置：`TrackTable.vue` 专辑列、`PlayerQueuePanel` 的 `navigate-album`、其它用到 `/albums/${album.id}` 的入口；
- 不新建占位专辑实体（本期不做）。

## 4. 测试

**前端**

- `PlayerFooter.spec`：remote 行（`artists` 为空、`raw_artist_names` 有值）→ 显示原始艺人名，不显示占位；
- `TrackInfoDrawer.spec`：remote 行头部显示艺人 + `raw_album_name`；详情含「专辑」项；
- 专辑链接：`album` 为空时不渲染链接、点击不跳转；有 `album.id` 时仍正常；
- 现有本地行用例保持通过。

**人工**

1. 播放搜索里的一首 remote 曲目 → 左下角显示歌手；
2. 打开歌曲详情 → 头部有艺人/专辑，详情里有专辑；
3. 专辑名不可点（没有实体），不出现死链接；
4. 本地曲目行为不变。

## 5. 验收

- 自动：§4 全绿；
- 人工：§4 四条成立；
- 任何地方不再出现“专辑名可点但无反应”。

## 6. 风险

- 文本快照与实体可能不一致（入库后标题/艺人被修正）：优先实体，符合预期；
- 未来若给 remote 行建占位专辑，需要再改跳转规则，本期先按“文本不可点”处理。
