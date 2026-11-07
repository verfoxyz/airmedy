# 修复：库里已有的本地曲目在云端搜索不显示「已在库中」

> 状态：待开工
> 现象来源：人工验收（2026-11-07），例：`Rude Buster (Camellia Remix)`
> 约束：AI 不做图形化验证

---

## 1. 现象

云端搜索里，本地已经导入、能正常播放的曲目仍然显示成"未在库中"（没有 `in_library` 徽标，仍然提供下载）。
例：`Rude Buster (Camellia Remix)` 在本地库中（`D:\MYHOME\MUSIC\...flac`），云端 wy 也搜得到同一首，但没有徽标。

## 2. 根因

链路是两段：

1. **身份匹配**：`tracks.provider` / `tracks.external_id`。本地导入的文件这两列为空，所以 `CatalogRowStates` 查不到 → 进入回退；
2. **回退的本地匹配**：`applyStates()` → `domain.CatalogRowStateResolver.CatalogLocalMatches()` →
   `tracks.FindPlayableByDuration()`（`internal/infra/sqlite/track_repository.go:508`）。

问题在第 2 段：

```sql
WHERE COALESCE(t.availability, 'ready') = 'ready'
```

`COALESCE` 只处理 **NULL**。而本地库里大量行的 `availability` 是**空串**（不是 NULL）：

```
sqlite> select quote(availability), count(*) from tracks group by 1;
''|1864
'ready'|4
```

空串经 `COALESCE` 仍是空串，`= 'ready'` 不成立 → **这些行全部被候选查询过滤掉**，
`CatalogLocalMatches` 拿不到任何候选，徽标自然不会出现。

为什么空串是"正常"的：代码其余地方都把空串当作 ready——

- `domain.Track.IsRemote()`：`Availability != "" && != AvailabilityReady` 才算远程；
- `internal/infra/sqlite/columns.go`：`COALESCE(t.availability, 'ready')`（同样只处理 NULL，但只用于显示）；
- `track_repository.go` 的 upsert：`WHEN excluded.availability = '' THEN tracks.availability`。

也就是说**语义是"空串 = ready"，只有这一条候选查询按字符串比较**，与全仓不一致。导入路径把空串写了进去，所以绝大多数行都中招。

### 实测证据（2026-11-07）

- 本地库该曲目：`duration=348`，`raw_artist_names="Toby Fox;かめりあ"`，`provider=''`，`external_id=''`，`availability=''`；
- 云端 wy 同一行：标题 `Rude Buster (Camellia Remix)`、艺人 `["Toby Fox","かめりあ"]`、`duration=349`；
- 标题归一化后一致、主艺人一致、时长差 1 秒（在 ±3 秒内）——**除了被 SQL 过滤，其它条件都满足**。

## 3. 修法

### 3.1 必改：候选查询把空串当 ready

`internal/infra/sqlite/track_repository.go:520`：

```sql
- WHERE COALESCE(t.availability, 'ready') = 'ready'
+ WHERE COALESCE(NULLIF(t.availability, ''), 'ready') = 'ready'
```

其余条件（`path NOT LIKE 'remote://%'`、`duration BETWEEN ? AND ?`、`LIMIT 2000`）不动。

### 3.2 可选但建议：写入时归一化

`track_repository.go` 的 upsert（`:743` 附近）把空串写成 `'ready'`，避免继续产生空串行：

```go
availability := track.Availability
if availability == "" {
    availability = domain.AvailabilityReady
}
Availability: sql.NullString{String: availability, Valid: true},
```

### 3.3 可选：数据迁移（与 3.2 二选一也可以都做）

新增 `internal/infra/sqlite/migrations/000090_availability_ready.up.sql`：

```sql
-- Empty availability has always meant "ready" in code (see domain.Track.IsRemote);
-- normalise the legacy rows so SQL filters do not need NULLIF everywhere.
UPDATE tracks SET availability = 'ready' WHERE availability = '';
```

`down.sql` 写成 no-op 注释即可（空串与 ready 语义等价，回退无意义）。

> 只做 3.1 就能修复现象；3.2/3.3 是防止以后再踩同一个坑。若不想动迁移，至少做 3.1 + 3.2。

## 4. 测试

`internal/infra/sqlite/`（新建 `track_repository_test.go` 或并入现有 sqlite 测试）：

- 插入一条 `availability=''`、`path` 为本地路径、`duration=348` 的曲目 → `FindPlayableByDuration(346, 352)` 能返回它；
- 插入 `availability='remote'` / `path='remote://...'` 的曲目 → 不返回；
- 插入 `availability='ready'` → 返回（回归）。

`internal/app/download/catalog_state_test.go`：

- 已有用例保持；如使用假 repo 未覆盖 SQL，可补一条集成式用例（用真 sqlite 临时库）断言空串行能匹配。

回归：

```bash
go build ./... && go test ./internal/infra/sqlite/... ./internal/app/download/... ./internal/app/catalog/...
```

## 5. 验收

自动：§4 全绿。

人工：

1. 搜 `Rude Buster` → wy 的 `Rude Buster (Camellia Remix)` 行出现「已在库中」，不再提供普通下载（仍保留"下载到库"的强制入口）；
2. 搜一首只在本地的曲子（provider 为空）→ 同样出现徽标；
3. 一首确实不在库里的歌 → 仍然可以下载；
4. 已有的云端下载曲目（provider 有值）徽标行为不变。

## 6. 风险

- 候选集合从少量 ready 行扩大到上千行：查询本身按 duration 过滤 + `LIMIT 2000`，且结果只用于标题/艺人比对，开销可控；
- 标题/艺人匹配仍较严格（同一归一化标题 + 主艺人 + ±3 秒），修好后可能仍有边界情况（例如平台把主艺人写成 `かめりあ`、本地写 `Toby Fox`）——那是匹配策略问题，另行处理，不在本单混改。
