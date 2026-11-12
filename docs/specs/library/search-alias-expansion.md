# 任务：本地搜索支持艺人别名（camellia ↔ かめりあ）

> 状态：待开工
> 目的：本地搜索输入其中一个名字，能搜到另一个名字下的曲目/专辑/艺人
> 关联：`artist-aliases.md`（别名表已实现）、`bleve.go`（本地索引）

---

## 1. 现状

- 别名表 `artist_aliases` 已存在并用于**匹配**（本地优先、云端匹配、去重）；
- 本地搜索（Bleve，`internal/infra/bleve/bleve.go` 的 `Search`）直接用查询词匹配索引，
  **没有做别名展开**；
- 所以搜 `camellia` 找不到 `かめりあ` 的曲目。

## 2. 方案：查询时展开（不重建索引）

在**应用层**扩展搜索，而不是改 Bleve：

1. 读取启用的别名（`enabled=1`，`ArtistAliasRepository`）；
2. 把查询归一化（`FoldUnicode`/`NormalizationKey`）后与别名 key / canonical key 比较：
   - 整串查询等于某个 key → 生成 {原词, 别名} 两个查询；
   - 多词查询时，对**每个词**做同样检查，命中则整句替换该词生成一个变体（变体数量上限 4，避免爆炸）；
3. 对每个变体调用 `search.Search`，合并结果：
   - 按 `type + id` 去重；
   - 保留原词结果的排序优先；
   - 总数不超过现有上限（200）；
4. 若没有命中别名，行为完全不变（只发一次查询）。

### 2.1 接入点

- `internal/infra/wails/search_service.go` 目前只做翻译层；把展开逻辑放在
  `internal/app/library`（已有 `ArtistAliasService`）新增：

```go
// ExpandSearchQueries returns the original query plus variants that replace a
// known artist alias. Empty/unknown queries return just the original.
func (s *ArtistAliasService) ExpandSearchQueries(ctx context.Context, query string) ([]string, error)
```

- Wails `SearchService.Search` 调 `ExpandSearchQueries` → 对每个变体 `search.Search` → 合并；
- 移动端同步搜索（如使用同一入口）自动受益。

### 2.2 别名读取与缓存

- 每次搜索读一次 `ListArtistAliases(enabled only)`（数据量小）；
- 进程内缓存 30 秒，别名新增/禁用后最多 30 秒生效（或监听别名变更事件立即失效）。

## 3. 规则

- 只使用 `enabled=1` 的别名；
- 只做**整词**匹配（词边界），不做子串替换，避免 `camellia` 替换到别的词里；
- 大小写/全半角由归一化处理；
- 结果去重，保持原词结果优先；
- 不做“跨语言音译”（如中文名 ↔ 日文名）的自动推断，只认别名表。

## 4. 测试

**Go**

- 别名对 `camellia ↔ かめりあ`（enabled）：搜 `camellia` 返回两组结果合并、去重；
- 搜 `かめりあ` 同样命中 `camellia` 的曲目；
- 禁用该别名 → 只返回原词结果；
- 未命中别名的查询 → 只发一次搜索（fake 断言调用次数）；
- 多词查询命中一个词 → 生成变体且不超过上限；
- 合并后总数不超过 200、去重正确。

**人工**

1. 搜索框输入 `camellia` → 出现 `かめりあ` 的曲目/专辑；
2. 反过来输入 `かめりあ` → 出现 `camellia` 的曲目；
3. 设置里禁用该别名后，再搜不再出现对方结果；
4. 普通关键词搜索无变化、无变慢。

## 5. 验收

- 自动：§4 全绿；
- 人工：§5 四条成立。

## 6. 风险

- 查询变体导致请求数增加（最多 4 次），需要短缓存与上限；
- 别名过多时匹配词表可能误替换普通英文词（只匹配整串/整词，且别名是艺名，风险低）；
- 结果排序合并后可能略有变化（原词优先可接受）。
