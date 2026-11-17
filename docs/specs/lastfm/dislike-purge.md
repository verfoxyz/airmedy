# 任务：标记「不喜欢」时清除该曲的推荐源数据

> 状态：待开工
> 目的：不喜欢后，该曲不再占用推荐列表/封面/匹配缓存；设置页仍能看到它的名称与歌手
> 关联：`dislike-tag.md`、`fix-dislike-covers.md`

---

## 1. 行为

用户点「不喜欢此曲 / 不喜欢此艺人」时：

1. 写入 `dislikes`（保留 `title / artist / match_key / kind / created_at`）——**这些元数据永不删除**；
2. 删除该曲在推荐源数据中的痕迹：
   - `lastfm_recommendation_items` 中 `match_key` 匹配的行（艺人级：该艺人的所有行）；
   - 对应列表的 `track_count` 由读取时重算（见 `fix-dislike-covers.md`），不缓存旧值；
3. 删除 `lastfm_match_cache` 中对应 `match_key`(s)；
4. 从当前候选池（discover pool `payload`）中剔除；
5. **不碰**本地曲库、本地文件、`tracks` 行、播放历史。

> 备注：流缓存（`stream_cache`）与封面缓存默认**不删除**（它们属于播放缓存，不是推荐数据）；如需一并清理，用单独的设置开关。

## 2. 数据流

```
Dislike(kind, matchKey, title, artist)
  → 写入 dislikes（元数据）
  → 删除 items（match_key / artist）
  → 删除 match_cache（match_key）
  → 剔除候选池
  → emit dislike:updated
```

- 全部在一个事务内完成（SQLite），失败回滚；
- 艺人级：先按艺人 key 找出所有 match_key，再批量删除。

## 3. 设置页展示

「不喜欢」列表显示：

- 曲目：`title` + `artist`（来自 dislikes 表，不依赖源数据）；
- 艺人：名称；
- 时间、来源（手动/歌曲/艺人）。

即使源数据已删除，展示完全可用。

## 4. 恢复

- 「取消不喜欢」→ 只删除 `dislikes` 行；
- 已删除的推荐数据不会自动恢复；下次刷新/构建时重新出现（若仍被推荐）。

## 5. 测试

**Go**

- Dislike 后 `lastfm_recommendation_items` 中该行被删除；
- `dislikes` 行保留 title/artist；
- `match_cache` 对应项被删除；
- 艺人级删除该艺人全部行；
- 候选池中不再包含；
- 事务失败时三处都不变；
- 本地 `tracks` 行与文件不受影响。

**前端**

- 设置页列表仍显示名称/歌手；
- 取消后下次刷新恢复。

**人工**

1. 标不喜欢 → 列表/卡片立即减少，封面不再出现；
2. 设置页「不喜欢」中仍能看到该曲名称+歌手；
3. 取消不喜欢 → 刷新后重新出现（若仍被推荐）。

## 6. 验收

- 自动：§5 全绿；
- 人工：§6 三条成立；
- 元数据与源数据分离：删源不删标记信息。

## 7. 风险

- 删除 items 会影响列表 position 连续性，读取时重新编号；
- 大量删除时需批量 SQL，避免逐行；
- 不清理流缓存/封面缓存是关键取舍，需在设置页说明。
