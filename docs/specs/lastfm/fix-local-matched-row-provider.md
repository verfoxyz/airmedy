# Bug：提升后播放已用本地文件，推荐行仍显示「来自 wy」

> 状态：待开工
> 现象：`GALAXY FRAGMENTATION` 提升完成后，播放确实走本地文件，但 Last.fm 推荐行仍显示「来自 wy」
> 关联：`recommendation-lists.md`、`fix-promotion-while-playing.md`

---

## 1. 现象

- 提升/合并完成后，数据库里该曲目已是本地行（有 `local_track_id`）；
- 播放也确实从本地文件加载；
- 但推荐行 chip 仍是「来自 wy」，不是「已在库中」。

## 2. 原因

1. **本地命中行仍保留 provider**：匹配结果 `State=in_library`，但 `Provider/ExternalID` 没有清空（本地行本身也带 identity）→ 前端优先渲染 `provider`；
2. **提升完成后没有刷新该行**：推荐行匹配状态是持久化的，`track:merged`/库更新没有触发对应 `match_key` 的重新解析，旧状态（来自 wy）一直留着。

## 3. 修法

### 3.1 后端：本地命中行清空来源字段

在 `applyTopTrackMatch` / 本地匹配结果写行时：

- `state == in_library` 或 `local_track_id != ""` →
  - `provider = ""`、`external_id = ""`、`sources = []`；
  - `matched = true`、`matching = false`、`no_source = false`；
- 写入 `lastfm_recommendation_items` 时同步这些字段。

### 3.2 后端：合并/提升后刷新行

监听 `track:merged`（from/to）与 `library:track-updated`：

- 若 pending/已匹配行的 `provider+external_id` 等于 `from` 的 identity（或 match_key 命中），把该行更新为 `in_library` + `local_track_id=to`，清空 provider；
- emit `lastfm:recommendations-matched`（只带该行）刷新前端；
- 同时刷新匹配缓存中该 `match_key` 的结果，避免下次读回旧值。

### 3.3 前端 chip 优先级

`TopTrackRow.vue`：

- `state === 'in_library'` 或 `local_track_id` 非空 → 优先显示「已在库中」；
- 其次才是 `provider` 的「来自 {源}」；
- 两者都为空的已匹配行按现有逻辑处理。

## 4. 测试

**Go**

- 本地命中行写库后 `provider=""`、`state=in_library`；
- `track:merged` 后，已匹配行更新为本地行并 emit；
- 匹配缓存同步更新，下一次读取不再返回旧 provider。

**前端**

- `state=in_library` + 有 `provider` 的旧数据 → 仍显示「已在库中」（优先级）；
- 事件刷新后 chip 从「来自 wy」变为「已在库中」。

**人工**

1. 推荐行点保存 → 提升完成 → 该行变「已在库中」；
2. 播放走本地文件；
3. 重启后仍显示「已在库中」。

## 5. 验收

- 自动：§4 全绿；
- 人工：§5 三条成立。

## 6. 风险

- 清空 provider 后，“来自哪个源”信息只在任务页可见（可接受）；
- 合并事件里 identity 可能已被移动，刷新要靠 `match_key` 兜底；
- 多个列表（kind）引用同一首时都要刷新。
