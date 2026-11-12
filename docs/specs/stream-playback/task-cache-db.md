# 任务：stream-cache 元数据迁到数据库（支持跨源匹配）

> 状态：✅ 已完成（代码已合入 wip/stream-playback）
> 前置：M2 / M3 已完成
> 目标：目录里只保留 `<key>.audio`；元数据进 `airmedy.db`；同一首歌跨源可复用缓存

---

## 1. 要解决的两个问题

1. `stream-cache/` 里每个音频旁边都有一个 `.json`，播放中还会被原子重写（资源管理器丢选中、多余磁盘写）；
2. 缓存只按 `provider + external_id` 命中：在网易云缓存过的歌，换成酷狗搜到同一首，仍然要重新拉流。

## 2. 库的选择

**合并到现有 `airmedy.db`**，新增一张 `stream_cache` 表（迁移 `000091`）。

理由：元数据量小（几千行）；与 `cover_cache` 一致；迁移、连接池、生命周期都现成。独立库的收益（体积隔离、整体删除）在这里不存在。

## 3. 表结构（迁移 000091）

```sql
CREATE TABLE stream_cache (
    cache_key        TEXT PRIMARY KEY,      -- sha256(provider + "/" + external_id)
    provider         TEXT NOT NULL,
    external_id      TEXT NOT NULL,
    title            TEXT NOT NULL,
    title_key        TEXT NOT NULL,         -- domain.NormalizationKey(title)
    artist           TEXT NOT NULL,         -- 主艺人
    artist_key       TEXT NOT NULL,         -- domain.NormalizationKey(artist)
    artists          TEXT NOT NULL DEFAULT '', -- 全部艺人，'; ' 拼接，仅展示
    album            TEXT NOT NULL DEFAULT '',
    duration_ms      INTEGER NOT NULL DEFAULT 0,
    quality          TEXT NOT NULL DEFAULT '',  -- 拉流时请求的音质（128k/320k/flac）
    audio_path       TEXT NOT NULL,         -- 文件名，不含目录
    total_bytes      INTEGER NOT NULL DEFAULT 0,
    received_bytes   INTEGER NOT NULL DEFAULT 0,
    complete         INTEGER NOT NULL DEFAULT 0,
    playback_progress REAL NOT NULL DEFAULT 0,
    pending_promote  INTEGER NOT NULL DEFAULT 0,
    created_at       INTEGER NOT NULL,
    last_used        INTEGER NOT NULL
);

CREATE INDEX idx_stream_cache_identity ON stream_cache(provider, external_id);
CREATE INDEX idx_stream_cache_match    ON stream_cache(title_key, artist_key);
CREATE INDEX idx_stream_cache_lru      ON stream_cache(last_used);
```

- 音频文件仍叫 `<cache_key>.audio`，目录里不再有 `.json`；
- 旧 JSON 不需要迁移：缓存可重建。升级时直接清掉 `stream-cache/` 下的 `.json` 与无人引用的 `.audio`（启动时一次性清理或干脆整目录清空，缓存会按需重建）。

## 4. 匹配规则（严格）

播放某行时：

1. **身份命中**：`provider + external_id` → 直接返回该缓存；
2. **跨源命中**（同一首歌在别的源缓存过）：
   - `title_key` 与 `artist_key` 都相等（归一化后完全一致，不接受模糊匹配）；
   - 双方 `duration_ms > 0` 时，`|Δ| ≤ 5_000 ms`；
   - 多条命中时取 `last_used` 最新的一条；
3. 都不命中 → 新建缓存条目并开始拉流。

阈值先写成常量（`matchDurationToleranceMs = 5000`），**不急着做成设置项**；后续用户反馈误匹配再开放。BPM 不参与匹配，也不写入缓存表。

匹配成功时：返回命中条目的音频文件，但**播放上下文仍是当前行**（title/artist/cover 用当前行），不改变缓存条目的身份；提升时按当前行的身份走下载管线。

## 5. 代码改动

- `internal/infra/sqlite/stream_cache_repository.go`（新增）：`Upsert`、`GetByIdentity`、`MatchByTitleArtist`、`ListForEviction`、`UpdateProgress`、`UpdateComplete`、`SetPendingPromote`、`Delete`；
- `internal/domain/`：新增 `StreamCacheEntry` 与仓储接口；
- `internal/app/stream/cache.go`：去掉 JSON 读写，改为仓储调用；`Open` 先身份查、再跨源匹配；
- `internal/app/module.go`：注入仓储；
- 迁移 `000091_stream_cache.up/down.sql`。

> 迁移编号调整：spec §4.5 的封面-音乐关联迁移顺延为 `000092`。

## 6. 与写盘频率优化的关系

迁到数据库后，播放进度变成一条 `UPDATE`，不再有“重命名导致选中丢失”的问题；但**仍然不要每 500 ms 写一次**：沿用 `task-meta-writes.md` 的落地时机（关闭/暂停/完成/失败/退出 + 30 秒兜底），只是把 `persistMeta` 换成 `UpdateProgress`。

## 7. 测试

- 仓储：身份命中、跨源命中（标题/艺人一致 + 时长容差）、超出容差不命中、任一侧时长为 0 时不参与时长判定；
- `stream-cache`：新建条目、命中复用不重新拉流、淘汰仍按“完整度 → 播放进度”排序；
- 迁移：`000091` up/down 可逆（down 只删表）；
- 回归：`go test ./internal/app/stream/ ./internal/infra/sqlite/ ./internal/domain/`。

## 8. 验收

- 自动：§7 全绿；
- 手工：wy 缓存一首 → 用 kg 搜同一首播放 → 命中已有缓存（不重新下载），音质/时长符合预期；换一首同名不同版本 → 不命中。

## 9. 风险

- 跨源复用有误匹配风险（同名同艺人、时长接近的不同母带），所以规则保持“严格 + 容差小”；
- 旧缓存不迁移，升级后第一次播放会重新下载（可接受，缓存可再生）；
- 不比对 BPM：流曲目的 BPM 要等缓存完整后分析才有，参与匹配会让命中率不稳定；需要时再加。
