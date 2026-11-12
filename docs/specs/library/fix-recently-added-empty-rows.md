# Bug：「最近添加」出现未播放、未缓存的 Last.fm 曲目

> 状态：待开工
> 现象：在 Last.fm 歌单点「播放全部」后，尚未播放、也没有缓存的曲目出现在「最近添加」
> 范围：最近添加的查询过滤；不改变点击即播放的行为

---

## 1. 根因

1. 「播放全部」会先对歌单里的每一行调用 `preparePlayback`（前端 `lastfmTracks.ts`），
   为了拿到可播放的 track id，**所有行都会被注册成库里的 remote 行**；
2. `GetRecentlyAdded`（`track_repository.go`）只是 `ORDER BY created_at DESC`，
   不区分“真正播放过/有缓存”的 remote 行；
3. 于是一次播放全部 = 往「最近添加」塞进几十条未播放、未缓存的空壳。

## 2. 目标

「最近添加」只显示：

- 本地文件行（非 remote）；
- 或者**已经有用例证明可用**的 remote 行：
  - `play_count > 0`（播放过），或
  - stream cache 有且完整（`complete = 1`）。

仅注册（remote、未播放、无完整缓存）的行不显示。

## 3. 修法

`GetRecentlyAdded` 的 SQL 增加过滤：

```sql
WHERE t.path NOT LIKE 'remote://%'
   OR t.play_count > 0
   OR EXISTS (
        SELECT 1 FROM stream_cache sc
        WHERE sc.provider = t.provider
          AND sc.external_id = t.external_id
          AND sc.complete = 1
   )
```

- 本地行永远显示；
- remote 行要有“播放过”或“缓存完整”的资格；
- 排序与 limit 不变。

> 备选（更大改动，本期不做）：把「播放全部」改成**按需注册**——只有真正开始播放的那一首才 `preparePlayback`，其余保持 Last.fm 行。这样从源头减少空壳行。

## 4. 测试

**Go**

- 刚注册、未播放、无缓存的 remote 行 → 不出现在 `GetRecentlyAdded`；
- 播放过（`play_count>0`）的 remote 行 → 出现；
- 有完整缓存的 remote 行 → 出现；
- 本地文件行 → 始终出现；
- 排序与 limit 不变。

**人工**

1. Last.fm 歌单点「播放全部」→「最近添加」不出现未播放的曲目；
2. 实际播放其中一首 → 该曲目出现在「最近添加」；
3. 本地导入/下载的曲目仍正常出现。

## 5. 验收

- 自动：§4 全绿；
- 人工：§4 三条成立。

## 6. 风险

- 正在流式播放但尚未 complete 的 remote 行：`play_count` 会在达到阈值后增加，短暂不显示属正常；
- SQL 里的 `stream_cache` EXISTS 需要 identity 索引（已有 provider/external_id 索引）；
- 如果之后把“最近添加”解释为“注册时间”，这个过滤需要重新讨论（当前语义是“值得看的最近添加”）。
