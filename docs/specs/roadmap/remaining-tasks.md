# 任务清单：剩余工作（按顺序执行）

> 状态：Step 0–3 已完成；Step 4（曲目右键）与 Step 5（网络代理）待开工
> 用途：交给编程 AI，一步步做
> 依据：`docs/specs/` 下各任务文档
> 已完成的代码可能还在工作区未提交：先做 Step 0

---

## Step 0 · 基线确认（不改功能）

**目标**：确认当前工作区可构建、可测试，并提交为基线。

1. `go build ./...`、`go test ./internal/... .`；
2. `vue-tsc --noEmit`、前端相关 vitest；
3. 检查工作区里未提交的实现（提升 worker、`promotion_tasks`、`discard_pending`、`000097_cover_cache_links`、保存优先提升）是否完整；
4. 通过后提交（建议一个 commit），再继续后续步骤。

**完成标志**：干净基线，后续每步都能独立回滚。

---

## Step 1 · 搜索行 chip 收敛

**文档**：[catalog/row-task-state.md](../catalog/row-task-state.md)
**依赖**：无
**改动**：

- 后端：`CatalogRowStates` 把活动提升任务也归入 `downloading`（不再返回 `promoting`），批量查询 `ActivePromotions`；
- 前端：`stateKey` 去掉 `saved`（返回 null），`downloading` 文案改「已创建任务」，`in_library` 文案改「已在库中」；
- 12 个 locale 同步；
- 测试：resolver 四态、`saved` 不渲染 chip、文案 key。

**完成标志**：搜索页只有「已在库中」「已创建任务」「下载失败」三种 chip。

---

## Step 2 · 提升完成后刷新搜索行

**文档**：[stream-playback/fix-promotion-stale-row.md](../stream-playback/fix-promotion-stale-row.md)
**依赖**：Step 1
**改动**：

- 后端：提升成功/失败 emit `task:updated`；确认导入路径 emit `library:track-updated`；
- 前端 `catalog` store：监听 `task:updated` / `library:track-updated`，按 identity 批量重读可见行，300–500 ms 节流，`dispose()` 释放；
- `OnRemoteTrackSaved` 缓存命中返回 `downloading`（Step 1 的统一任务态）；
- 测试：事件触发刷新、节流、释放、chip 从「已创建任务」到「已在库中」。

**完成标志**：提升完成后不重新搜索，chip 也自动变「已在库中」。

---

## Step 3 · 「下载/任务」页（三标签 + 清空记录）

**文档**：[library/downloads-tasks.md](../library/downloads-tasks.md)
**依赖**：Step 0（`promotion_tasks` 已存在）
**改动**：

- 后端：`TaskService.ListTasks()`（合并 `download_jobs` + `promotion_tasks`）、`ClearTaskHistory()`（只删终态）、`task:updated` 事件；
- 提升 worker 写 `promotion_tasks` 的 queued/running/done/failed（若 Step 0 未完成）；
- 前端：`DownloadsView` 增加「正在进行 / 已完成 / 出错」三个标签与数量；kind 标签（下载/提升）；「清空记录」→ `ConfirmDialog` → 调 API 并刷新；
- i18n（12 语言）按文档 §4.3；
- 测试：合并排序、清空只删终态、500 条保留、前端标签/确认框。

**完成标志**：三个标签有数据、清空需二次确认、进行中不受影响。

---

## Step 4 · 曲目右键：刷新此曲信息 + 导入本地歌词

**文档**：[track-actions/spec.md](../track-actions/spec.md)
**依赖**：`cover_cache.url`（已有 `000093`）
**改动**：

- 右键新增「刷新此曲信息」：严格标题+艺人+时长容差匹配，刷新封面/元数据；无 identity 置灰；
- 编辑元数据 → 歌词标签页新增「导入本地歌词文件」（后端 `ImportLyricFile`）；
- 12 语言 key；
- 测试按文档 §6。

**完成标志**：下载曲目可刷新封面/信息；歌词文件可导入并保存。

---

## Step 5 · 宿主网络代理

**文档**：[network-proxy/spec.md](../network-proxy/spec.md)
**依赖**：无（可与 Step 4 并行，但代码量大，建议单独做）
**改动**：

- 迁移 `000098`：`app_settings.proxy_mode` / `proxy_rules`；
- 新增 `internal/infra/netproxy`（规则匹配、系统代理读取、transport 缓存）；
- 按文档 §5.1 替换 9 处自建 HTTP client；
- 设置 UI（模式 + 规则表）；
- 测试：匹配器、代理命中/直连、迁移、注册表解析（平台跳过）。

**完成标志**：只给 `api.deezer.com` 配代理时，艺人头像能拉取，其它请求直连；插件网络不受影响。

---

## Step 6 · 收尾回归

1. `go build ./...`、`go test ./internal/... .`；
2. `vue-tsc --noEmit`、前端全量 vitest；
3. 人工验收清单：
   - 搜索播放、保存（已创建任务 → 已在库中）、下载、提升；
   - 下载/任务页三标签与清空；
   - 刷新此曲信息、导入歌词；
   - 代理规则（若 Step 5 完成）；
   - 重启后状态与历史正确；
4. 汇总改动，准备提交（push 前询问用户）。

---

## 执行约定

- 每个 Step 单独提交，Conventional Commits；
- 每步都要跑该步的测试，不要攒到最后；
- **不要修改已应用的迁移**（下一个编号从 `000098` 起）；
- 新增用户文案必须同步 12 个 locale；
- 遇到与文档不一致的现状，先记下来再问，不要擅自改契约。
