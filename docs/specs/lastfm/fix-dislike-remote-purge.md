# Bug：不喜欢「彻底删除」未清理远程行、流缓存与残留列表项

> 状态：待开工
> 现象：打开「不喜欢时同时删除本地文件」后标记不喜欢，流缓存音频仍在、remote 行仍在、推荐列表项残留
> 关联：`dislike-purge-files.md`、`dislike-purge.md`、`fix-dislike-covers.md`

---

## 1. 证据

曲目：`HIT THE JACKPOT! (Hakari Dance)` / GameboyJones

| 项 | 实际 |
|---|---|
| 本地文件 | 不存在（本来就是 remote 行） |
| 流缓存 | **仍在**：`stream-cache/5bf65bdd….audio`，29,808,996 B，complete |
| 曲目行 | `tx/002ncO5A4fBJQl`、`kw/550571517` 两条 remote 行仍在 |
| 推荐列表 | `similar` 中仍有一条 `gameboyjones` 行 |
| 不喜欢标记 | 已写入 |

## 2. 根因

1. `PurgeDislikeTargets` 只按 `tracks` 的本地行处理（删文件/行/封面/歌词）；
2. **remote 行**（`path=remote://…`）被跳过，因此：
   - 行不删；
   - `stream_cache` 条目与音频文件不删（缓存属于该 identity，不是本地文件）；
3. 推荐列表项清理与 `dislikes` 写入可能不在同一次刷新视图中，出现残留（若先标记、后读取，读取侧应过滤，见 `fix-dislike-covers.md`）。

## 3. 修法

### 3.1 覆盖 remote 行

`PurgeDislikeTargets` 对匹配到的**所有** `tracks` 行：

- 本地行：现有删除流程（文件、行、封面、歌词）；
- remote 行：
  - 删除该 identity 的 `stream_cache` 行与缓存文件；
  - 删除 remote 行（或标记 `availability=deleted`，与本地一致）；
  - 删除其已保存歌词。

### 3.2 清理时机

- 先删推荐源数据（items/match_cache/候选池），再按 §3.1 清理；
- 同一事务删除 DB 行；文件删除失败只记录 `PurgeError`；

### 3.3 列表残留

- 读取侧过滤（`fix-dislike-covers.md`）必须保证即使 items 残留也不显示；
- 清理逻辑应同时删除 items，避免计数/`has_more` 偏差。

## 4. 测试

**Go**

- 不喜欢仅 remote 行时：`stream_cache` 行与文件被删、remote 行被删；
- 本地行 + remote 行同时存在：两者都清理；
- 文件删除失败：标记保留、返回 `PurgeError`；
- items 与候选池被清空；
- 关闭开关时不做任何文件/缓存删除。

**人工**

1. 打开开关 → 对付费/远程曲目标不喜欢 → 缓存目录不再有该文件；
2. 曲目从列表与详情消失；
3. 本地文件曲目同样被删。

## 5. 验收

- 自动：§4 全绿；
- 人工：§5 三条成立；
- “彻底删除”对 remote 与 local 一致生效。

## 6. 风险

- 删除 remote 行会影响播放队列中的引用（先移出队列/停止播放）；
- 缓存文件可能被同一 identity 的其它行共用（按 identity 删除需确认无其它引用）；
- 与 `fix-dislike-covers.md` 的读取侧过滤需同时存在，防止竞态残留。
