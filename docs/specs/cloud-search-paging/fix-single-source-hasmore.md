# 修复：单源搜索没有「加载更多」

> 状态：待开工
> 前置：`docs/specs/cloud-search-paging/spec.md` 已实现
> 约束：AI 不做图形化验证

---

## 1. 现象

只选**一个源**搜索时，结果超过 5 行也没有「加载更多 ⬇️」按钮。
多源时通常有按钮，所以之前的人工验收没发现。

## 2. 根因

`Search()` 的 `hasMore` 只来自 `pageOf()`：

```go
rows, hasMore := pageOf(ranked, normalised.Offset, limit)
if normalised.Offset+limit >= domain.MaxCatalogSearchPageSize {
    hasMore = false
}
```

而 `ranked` 只包含 **窗口内** 的行：`searchSources()` 每源只请求 `offset+limit` 行。

- 单源：`ranked` 长度 == 窗口（5）→ `pageOf(0, 5)` 的 `end == len(ranked)` → `hasMore=false`，按钮永远不出现；
- 多源：合并后长度可能大于窗口（两源各 5 行 → 10 行），所以偶尔能出现按钮——但这是巧合，不是正确判定；合并后恰好等于窗口时同样会消失。

插件侧 `hasMore` 是对的（kg 按 `total > offset + rows.length`，wy 用上游计数），宿主只是没用它。

## 3. 修法

`internal/app/catalog/service.go` 的 `Search()`：

```go
rows, hasMore := pageOf(ranked, normalised.Offset, limit)
// A page is only the fetched window: the source is the authority on whether
// rows exist past it. Without this a single source can never page.
if !hasMore {
    hasMore = sourceHasMore(selected, outcome.results)
}
if normalised.Offset+limit >= domain.MaxCatalogSearchPageSize {
    hasMore = false
}
```

新增辅助函数（放在 `Search` 附近）：

```go
// sourceHasMore reports whether any selected source answered with rows past
// the window the host fetched.
func sourceHasMore(selected []string, results map[string]domain.PluginSearchResult) bool {
    for _, source := range selected {
        if result, ok := results[source]; ok && result.HasMore {
            return true
        }
    }
    return false
}
```

要点：

- 只统计**本次选中**的源，别把未选源的 `HasMore` 算进来；
- 上限判断必须放在最后（`offset+limit >= 100` 时强制 false），否则窗口已满仍会显示按钮；
- 源失败时 `results` 里没有它，自然跳过。

## 4. 测试（`internal/app/catalog/service_test.go`）

- **单源有更多**：一个源返回 5 行 + `HasMore=true` → 页面 `HasMore=true`；
- **单源到底**：返回 5 行 + `HasMore=false` → `HasMore=false`；
- **多源混合**：源 A `HasMore=false`、源 B `HasMore=true` → `true`；
- **未选中的源不计入**：只有选中源参与判定；
- **上限**：`offset+limit >= 100` 时即使源还有更多也返回 `false`；
- 现有分页用例（首屏 5、`offset=5,limit=10`、切片无重复）保持通过。

回归：

```bash
go build ./... && go test ./internal/app/catalog/...
```

## 5. 验收

自动：§4 全绿。

人工：

1. 只选 wy 搜常见关键词 → 首屏 5 行，出现「加载更多」；
2. 点一次 → 变 15 行，再点 → 25 行，顺序连续；
3. 只选 kg 同样有按钮（kg 上游 30 条/页，插件会翻页补齐）；
4. 到 100 行或结果取完 → 按钮消失。

## 6. 风险

- 源声明 `HasMore=true` 但下一窗口实际取不到新行（上游计数虚高）：按钮会出现一次、点击后无新增，属于可接受的降级；不做额外探测。
- 不改插件与前端：`has_more` 字段、按钮绑定都是现成的。
