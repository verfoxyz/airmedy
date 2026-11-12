# 任务：艺术家别名（如 Camellia / かめりあ）参与匹配

> 状态：待开工
> 现象：`crystallized - Camellia` 与本地 `crystallized - かめりあ` 永远匹配不到，导致重复行/去云端
> 关联：`fix-artist-variant-duplicates.md`（艺人集合归一化与本地优先）

---

## 1. 事实与限制

- 同一个艺人在不同源/文件里有多个名字：`Camellia` ↔ `かめりあ`（还有 ·、空格等写法差异）；
- 实测 Last.fm 的 `artist.getCorrection` / `track.getCorrection` **不会**把它们合并：两个名字是不同 artist 实体、不同 MBID；
- 所以不能指望远端帮我们做别名解析，需要本地维护别名关系。

## 2. 数据模型

迁移 `000099`：

```sql
CREATE TABLE artist_aliases (
    alias_key   TEXT NOT NULL,      -- ArtistSetKey/单艺人归一化 key（如 "camellia"）
    canonical_key TEXT NOT NULL,    -- 另一个写法（如 "かめりあ"）
    evidence    TEXT NOT NULL DEFAULT '',  -- "merge" | "manual"
    created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (alias_key, canonical_key)
);
CREATE INDEX idx_artist_aliases_canonical ON artist_aliases(canonical_key);
```

- 关系按**无序对**存储（A→B 与 B→A 都写入，或读取时双向展开）；
- 只有单艺人 key（`ArtistSetKey` 拆分后的单个 artist），不是整个集合。

## 3. 别名从哪来

1. **自动学习（主要来源）**：下载/提升合并时，如果远端行与本地行是同一首歌并成功合并：
   - 比较远端艺人集合与本地艺人集合；
   - 对数量相同或一方为另一方子集的组合，记录 `(远端艺人, 本地艺人)` 别名对；
   - 例：远端 `Camellia` 合并到本地 `かめりあ` → 记录一对；
2. **手动**（后续可做设置 UI，本期不做）；
3. 不内置硬编码列表（会过时），完全靠学习。

## 4. 匹配时展开

新增：

```go
// ExpandArtists returns the artist keys plus their known aliases (one hop).
func ExpandArtists(ctx context.Context, keys []string) ([]string, error)
```

- 只展开**一跳**（A→B），不做无限传递，避免误合并链；
- 使用位置：
  - 本地优先匹配（`MatchLocalTrack`）：本地集合与目标集合先各自展开再比较；
  - 云端 `MatchTrack` 的艺人判断：目标艺人展开后与行的任一艺人比较；
  - stream cache 跨源匹配；
  - 缓存去重分组。
- 比较规则沿用 `fix-artist-variant-duplicates.md` 的**子集相容**。

## 5. 记录时机

- `merge.go` 的 `carryIdentity` / 合并成功处：拿到远端行与存活行的艺人后写入别名对（`INSERT OR IGNORE`）；
- 提升（`PromoteCachedFile`）走同一 merge，自动覆盖；
- 只学习“同一首歌成功合并”这一强证据，避免从搜索结果里瞎猜。

## 6. 测试

**Go**

- 合并 `Camellia` → `かめりあ` 后写入别名对；
- `ExpandArtists` 双向展开、只一跳；
- 本地优先匹配：Last.fm 的 `Camellia` 能命中本地 `かめりあ` 行；
- 云端匹配：目标 `Camellia` 能命中行 `かめりあ`（展开后）；
- 无别名的艺人行为不变；
- 迁移 `000099` up/down。

**人工**

1. 首次合并后，再次搜索 `crystallized` → 命中本地 `かめりあ` 文件，不再新建远程行；
2. 其它已知别名（如 `かめりあ` / `Camellia` 的反向）同样生效。

## 7. 验收

- 自动：§6 全绿；
- 人工：§6 两条成立。

## 8. 风险

- 自动学习依赖“合并成功”，首次仍可能匹配不到（需要一次手动纠正/播放后合并）；
- 只有一跳，复杂别名链（A→B→C）不会全展开；
- 误合并会把错误别名持久化，需要能在数据库里删除（后续 UI）。
