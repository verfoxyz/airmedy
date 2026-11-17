# Bug：标为「不喜欢」后，歌单封面仍显示该曲

> 状态：待开工
> 现象：在「发现 / 为你推荐」里标记某曲不喜欢，列表行消失了，但卡片封面拼图仍包含它的封面
> 关联：`dislike-tag.md`

---

## 1. 证据

- `dislikes` 表有记录（如 `pupsies / misery.`）；
- 但 `lastfm_recommendation_items` 里仍有该行：

```
similar | pupsies | misery. | wy | <cover>
discover | pupsies | misery. | kw | <cover>
```

- 卡片封面/曲目数来自**存储的 items**（前 1–4 首封面），没有在读取时过滤不喜欢。

## 2. 根因

不喜欢过滤只在“构建/保存列表时”生效（刷新后才干净），读取路径没有过滤：

- `ListRecommendationLists`（卡片 summary：track_count、封面列表）直接读库；
- `GetRecommendationList`（详情行）同样读库。

所以标记后：

- 前端行级过滤让行立即消失；
- 但卡片summary 已经用旧 items 算好，封面/计数仍含被排除的曲目。

## 3. 修法

**读取时统一过滤**（不依赖刷新）：

1. `ListRecommendationLists`：
   - 读取该列表 items 后，先剔除 `match_key` 命中不喜欢的曲目、艺人级别的曲目；
   - 用**过滤后**的前 4 首封面生成拼图，`track_count` 用过滤后的数量；
2. `GetRecommendationList`：返回前同样过滤（与前端行级过滤一致，避免重复逻辑分叉）；
3. 过滤逻辑复用 `dislike.go` 的同一组谓词（曲目 match_key、艺人 key），禁止另写一套；
4. 缓存：不要缓存过滤结果，读时过滤即可（dislikes 变化即时生效）。

## 4. 测试

**Go**

- 标记不喜欢后，`ListRecommendationLists` 的 track_count 减 1、封面不含该曲；
- `GetRecommendationList` 不再返回该行；
- 艺人级不喜欢：该艺人所有行被过滤、封面不出现；
- 取消不喜欢：读取结果立即恢复（无需刷新）；
- 空列表时封面为空、计数为 0。

**前端**

- 标记后卡片封面立即更新（事件触发重新加载）；
- 列表与卡片数量一致。

**人工**

1. 在「发现」标记不喜欢 → 卡片封面立刻变化、曲目数减一；
2. 详情页行消失；
3. 取消不喜欢 → 立即恢复。

## 5. 验收

- 自动：§4 全绿；
- 人工：§5 三条成立；
- 不刷新也能看到封面/计数更新。

## 6. 风险

- 读时过滤会给每次列表查询增加一次 dislikes 读取，需要缓存 dislikes 快照（已有快照缓存可复用）；
- 过滤后列表可能变短，`has_more` 需要按过滤后结果重新判断。
