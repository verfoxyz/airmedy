# 任务：刷新时保留匹配结果 + 匹配数据共享完善

> 状态：待开工
> 目的：刷新歌单不再整屏重新匹配；同一首曲目在所有列表与重启后复用匹配结果
> 关联：`recommendation-lists.md`、`discovery.md`、`recommendation-quality.md`

---

## 现状

1. 刷新会整表替换 items，`Matching: true` 重置 → 全部重新匹配；
2. 已有内存匹配缓存（`recommend.go` 的 `matches`，key = 归一化 artist+title，TTL 30 分钟）：
   - 只存在内存，重启即丢；
   - 只缓存“匹配成功”的结果，`no_source` 不缓存；
   - 30 分钟对冷门歌太短，对热门歌偏长。

## 目标

1. **刷新保留匹配**：同 `match_key` 的行直接继承上一版结果；
2. **匹配共享持久化**：所有列表、所有切换、重启后共用一份匹配缓存；
3. 只有真正的新歌或过期项才发起源请求。

---

## 1. 刷新时继承（列表内）

刷新前读出旧 items，建 `match_key → item` 索引；重建时：

| 字段 | 处理 |
|---|---|
| `provider` / `external_id` | 继承 |
| `local_track_id` | 继承（优先） |
| `matching` / `no_source` | 继承 |
| `suspicious` | 继承 |
| `cover_url` / `cover_url_large` | 继承（除非本次拿到更好的 Last.fm 真实图） |
| `duration` | 以本次 Last.fm/源数据为准 |
| 播放次数等 Last.fm 字段 | 用新值 |

- 继承到 `local_track_id` 的行：直接 `matching=false`、`no_source=false`，不再进入匹配队列；
- 继承到 `provider` 的行：直接可播，不进入队列；
- `no_source` 继承但带有效期（见 §2）。

## 2. 持久化匹配缓存

### 2.1 表结构（迁移 `000109_lastfm_match_cache`）

```sql
CREATE TABLE lastfm_match_cache (
    match_key     TEXT PRIMARY KEY,     -- normalized artist + title
    provider      TEXT NOT NULL DEFAULT '',
    external_id   TEXT NOT NULL DEFAULT '',
    local_track_id TEXT NOT NULL DEFAULT '',
    no_source     INTEGER NOT NULL DEFAULT 0,
    suspicious    INTEGER NOT NULL DEFAULT 0,
    cover_url     TEXT NOT NULL DEFAULT '',
    duration      INTEGER NOT NULL DEFAULT 0,
    updated_at    DATETIME NOT NULL,
    expires_at    DATETIME NOT NULL
);
CREATE INDEX idx_lastfm_match_cache_expiry ON lastfm_match_cache(expires_at);
```

### 2.2 TTL

| 结果 | TTL | 理由 |
|---|---|---|
| 匹配成功（有 provider） | 14 天 | 源身份较稳定 |
| 本地命中（local_track_id） | 30 天 | 本地库变化慢 |
| 无匹配（no_source） | 3 天 | 源可能后来上架 |
| 可疑（duration 差异大） | 7 天 | 允许重试更准的结果 |

### 2.3 读写规则

- 匹配 worker 开始前先查缓存：命中且未过期 → 直接应用，不发源请求；
- 匹配成功后写入缓存（含封面、时长、可疑标记）；
- 无匹配写入 `no_source` 行（短 TTL）；
- 过期行由启动清扫或写入时顺手删除（保留最近 5000 条上限）。

### 2.4 失效

- 库内新增/导入曲目（`library:track-updated`）→ 删除对应 `match_key` 的缓存（可能变成本地命中）；
- 手动「刷新此曲的源」→ 强制绕过缓存并刷新该行；
- 源整体失败（如插件不可用）→ 不写 `no_source`，避免污染。

## 3. 数据流

```
刷新列表
  → 读旧 items（继承）
  → 未继承的行查 match_cache（持久）
    → 命中：直接应用
    → 未命中：入匹配队列 → 结果写回列表 + match_cache
```

## 4. 测试

**Go**

- 刷新后相同 `match_key` 的行保留 provider/local/no_source，不进入队列；
- 新增歌曲才进入队列；
- 缓存命中（成功/本地/无匹配）不调用源；
- TTL 过期后重新匹配；
- 手动刷新绕过缓存；
- 本地库新增后缓存失效；
- 迁移 up/down 与清理逻辑。

**前端**

- 刷新后行状态不闪回「匹配中」；
- 只有新行显示匹配中。

**人工**

1. 刷新「为你推荐」→ 已有行状态立即保留，新歌才转圈；
2. 重启应用后打开列表 → 无需重新匹配；
3. 添加本地同名曲目 → 该行变为「已在库中」。

## 5. 验收

- 自动：§4 全绿；
- 人工：§5 三条成立；
- 刷新与重启不再造成全量重新匹配。

## 6. 风险

- 缓存会让源下架后的行继续“可播放”，失败在播放时才暴露 → 播放失败时删除该 `match_key` 缓存并标记重新匹配；
- `match_key` 归一化规则必须与列表一致，否则命中率低；
- 表会持续增长，需要上限与定期清理。
