# 任务：「不喜欢」标签（本地过滤与降权）

> 状态：待开工
> 目的：推荐/电台可以排除用户不喜欢的曲目与艺人；Last.fm 无公开的“不喜欢”接口，全部本地实现
> 关联：`recommendation-quality.md`、`discovery.md`、`recommendation-lists.md`

---

## 1. 数据

迁移 `000110_dislikes`：

```sql
CREATE TABLE dislikes (
    id         TEXT PRIMARY KEY,
    kind       TEXT NOT NULL,          -- track | artist
    match_key  TEXT NOT NULL,          -- track: normalized artist+title; artist: ArtistSetKey
    title      TEXT NOT NULL DEFAULT '',
    artist     TEXT NOT NULL DEFAULT '',
    created_at DATETIME NOT NULL,
    UNIQUE (kind, match_key)
);
CREATE INDEX idx_dislikes_kind ON dislikes(kind, created_at DESC);
```

- `match_key` 必须与匹配缓存/列表使用同一归一化规则；
- 同一曲目只存一条；重复「不喜欢」幂等。

## 2. 语义

| 操作 | 效果 |
|---|---|
| 不喜欢曲目 | 发现/电台排除该曲；该曲不再作为种子 |
| 不喜欢艺人 | 发现/电台排除该艺人的曲目；该艺人不再作为种子 |
| 取消不喜欢 | 从表删除，之后的刷新/补充恢复 |

- 只影响**推荐与电台**，不影响本地库可见性、不影响手动播放；
- 不喜欢不删除任何文件、不修改 Last.fm 数据。

## 3. 入口

- 推荐行右键/菜单：「不喜欢此曲」「不喜欢此艺人」；
- 电台播放中：footer/队列行的「不喜欢」→ 立即跳过当前曲并加入到排除；
- 设置页新增「不喜欢的曲目/艺人」列表，可批量移除；
- 播放器里普通曲目的右键菜单暂不加（避免误操作）。

## 4. 过滤与降权

- 发现列表 `expandDiscover` 后过滤 `match_key` 与艺人集合；
- 电台补充时同样过滤（含“跳过”后立即过滤）；
- 种子构建时跳过不喜欢的曲目/艺人；
- 多样性限制不变；
- 过滤在**内存与持久化**两处都生效：持久化列表刷新时应用，`LoadMore` 也不返回被排除项。

## 5. API

- Wails：`Dislike(kind, matchKey, title, artist)`、`Undislike(id)`、`ListDislikes()`；
- 事件：`dislike:updated`（列表/电台页面据此刷新或本地过滤）；
- 前端 store：`dislikeStore`，缓存集合用于快速过滤。

## 6. 测试

**Go**

- 不喜欢曲目后，发现/电台不再返回该曲；
- 不喜欢艺人后，该艺人所有曲目被排除；
- 取消不喜欢后恢复；
- 种子构建跳过不喜欢的对象；
- 重复添加幂等；
- 迁移 up/down。

**前端**

- 行菜单/设置页操作与状态；
- 电台「不喜欢」会跳过当前曲并继续；
- 刷新后排除项仍生效。

**人工**

1. 发现里不喜欢一首 → 刷新后不再出现；
2. 电台里不喜欢 → 跳到下一首且不再推该艺人（艺人级）；
3. 设置页移除 → 下次刷新恢复；
4. 本地库中该曲仍可正常播放。

## 7. 验收

- 自动：§6 全绿；
- 人工：§7 四条成立；
- 排除项在所有推荐入口一致生效。

## 8. 风险

- 归一化不一致会导致排除失效，必须以 `match_key` 统一；
- 不喜欢艺人会明显缩小候选，需保证过滤后仍有足够内容（不足时提示）；
- 不删除文件，避免用户误以为“不喜欢=删除”。
