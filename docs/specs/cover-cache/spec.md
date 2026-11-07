# Spec：远端封面获取与持久缓存

> 对应计划：`docs/cover-cache-plan.md`
> 状态：待批准后实施
> 约束：AI 不做图形化测试；亮/暗/黑观感由人工确认。

## 1. 目标与范围

1. kg 搜索结果通过插件内一次批量请求获得封面，失败降级为无封面。
2. 宿主新增持久封面缓存：下载、落盘、SQLite 索引、LRU 淘汰。
3. 宿主新增 `GetCatalogCovers` RPC，只向 WebView 下发本地 asset URL。
4. 前端从「进入视口即取」改为「滚动分批」：初始每源 `cover_batch` 张，滚动追加。
5. 插件设置新增 `cover_batch`、`cover_cache_mb`，复用 number 类型。

不在范围：UI 视觉改版、人工图形化验收、kg 搜索端点变更。

## 2. 现状结论（已核实）

- 插件 `plugins/wy/src/search.js` 同时服务 `wy` 与 `kg`；kg 行当前无封面。
- 宿主 `internal/app/catalog` 已通过 `domain.CatalogSourceSettingReader` 读取插件 number 设置（`max_display`）。
- `internal/infra/wails/assets.go` 已提供 `/artwork/` 资产下发，可扩展 `/cover-cache/`。
- `internal/app/config/config.go` 的 `DataDir` 在 Windows 即 `%LOCALAPPDATA%\airmedy`（prod）或 `airmedy-dev`（dev）。
- SQLite 迁移目录 `internal/infra/sqlite/migrations` 当前最高为 `000088`，下一版本为 `000089`。
- 前端 `CatalogResults.vue` 现有 IntersectionObserver + FIFO + 4 并发预热；本轮只替换触发方式。

## 3. 插件（C1 / C5）

### 3.1 kg 批量封面

文件：`plugins/wy/src/search.js`

- 新常量：
  - `KG_COVER_ENDPOINT = 'http://media.store.kugou.com/v1/get_res_privilege'`
  - `KG_COVER_UA = 'KuGou2012-9020-ExpandSearchManager'`
  - `KG_COVER_SIZE = 240`，`KG_COVER_LARGE_SIZE = 400`
- 新函数：
  - `buildKugouCoverResource(info)`：生成 `resource` 每项
    `{album_audio_id, album_id, hash, id:0, name:'歌手 - 歌名.mp3', type:'audio'}`；
    `album_audio_id`/`album_id` 缺失时用 `0`，`hash` 为空的行返回 null。
  - `buildKugouCoverBody(resources)`：序列化完整请求体。
  - `kugouCoverURL(image, size)`：把 `{size}` 替换为指定尺寸；无 `{size}` 时原样返回。
  - `fetchKugouCovers(fetchImpl, timeoutMs, items)`：
    1. 一次 `POST`，头为 `Content-Type: application/json`、`KG-RC: 1`、
       `KG-THash: expand_search_manager.cpp:852736169:451`、`User-Agent: KG_COVER_UA`；
    2. 响应 `status === 1` 且 `data` 非空 → 按 `hash`（回退 `album_audio_id`）映射；
    3. 批量失败（`status !== 1` 或 `data` 为空）→ 对每个 resource 单独再 `POST`；
    4. 任意单条仍失败 → 该条保持无封面；整个过程不抛错、不写日志、不影响搜索。
- `searchKG` 改为：先取搜索页，再在 `request.includeCover !== false` 时调用
  `fetchKugouCovers`，把结果写入 `row.cover_url` / `row.cover_url_large`。
  `cover_url` 用 240；`cover_url_large` 在 `imgsize` 含 400 或 480 时取较大者，否则同 240。
- `mapKugouRow` 保持不产生合成封面 URL。

### 3.2 设置

文件：`plugins/wy/wrapper.js` 的 `SETTINGS` 追加：

| key | type | default | min | max | step |
|---|---|---|---|---|---|
| `cover_batch` | number | 5 | 1 | 20 | 1 |
| `cover_cache_mb` | number | 1024 | 128 | 8192 | 128 |

`airmedy-plugin.json`：
- `version` 升为 `1.2.0`；
- `permissions.network` 追加 `media.store.kugou.com`。

## 4. 宿主缓存（C2）

### 4.1 领域契约

文件：`internal/domain/catalog_search.go`

新增常量：

```go
CatalogSettingCoverBatch   = "cover_batch"
DefaultCatalogCoverBatch   = 5
MinCatalogCoverBatch       = 1
MaxCatalogCoverBatch       = 20
CatalogSettingCoverCacheMB = "cover_cache_mb"
DefaultCatalogCoverCacheMB = 1024
MinCatalogCoverCacheMB     = 128
MaxCatalogCoverCacheMB     = 8192
```

新增类型/端口：

```go
type CatalogCoverRequest struct {
    Identity CatalogIdentity
    URL      string
}
type CatalogCoverResult struct {
    Identity CatalogIdentity
    URL      string // host-local /cover-cache/<hash>.<ext>
    Error    string // machine-free short reason; empty on success
}
type CatalogCoverCache interface {
    Get(ctx context.Context, remoteURL string, limitBytes int64) (assetURL string, err error)
}
```

`CatalogSearchPage` 增加 `CoverBatch int`。

### 4.2 `internal/infra/covercache`

新文件：`internal/infra/covercache/cache.go`

```go
type Config struct {
    Dir       string
    DB        *sql.DB
    Client    *http.Client
    Logger    *slog.Logger
    Now       func() time.Time
}
func New(cfg Config) (*Cache, error)
func (c *Cache) Get(ctx context.Context, remoteURL string, limitBytes int64) (string, error)
```

行为：

1. 只接受 `http` / `https` URL；否则失败。
2. `url_hash = sha256(remoteURL)`；文件名为 `url_hash + ext`。
3. 命中条件：索引行存在且文件存在 → `UPDATE cover_cache SET last_used=unix秒 WHERE url_hash=?`，返回 `/cover-cache/<file>`。
4. 索引行存在但文件丢失 → 删除该行，按未命中处理。
5. 未命中：单航班（同 `url_hash` 并发合并）下载：
   - `GET`，UA `airmedy/cover-fetch`（复用现有语义），Referer 按主机：
     `imge.kugou.com` → `https://www.kugou.com/`，其余 → `https://music.163.com/`；
   - 超时 20s，`LimitReader` 2 MB + 1，超出视为失败；
   - `http.DetectContentType` 仅接受 `image/jpeg`、`image/png`、`image/webp`；
   - 先写临时文件再 rename 落盘，随后 `INSERT ... ON CONFLICT DO UPDATE`；
   - 失败不落盘、不写索引。
6. 写入后按 `limitBytes` 淘汰：`SUM(bytes) > limit` 时按 `last_used ASC`
   逐条删除文件与索引，直到总量 ≤ `limit * 95 / 100`。
7. 返回 asset URL `/cover-cache/<file>`。

### 4.3 迁移

新文件：

- `internal/infra/sqlite/migrations/000089_cover_cache.up.sql`
- `internal/infra/sqlite/migrations/000089_cover_cache.down.sql`

```sql
CREATE TABLE cover_cache (
  url_hash  TEXT PRIMARY KEY,
  path      TEXT NOT NULL,
  bytes     INTEGER NOT NULL,
  mime      TEXT NOT NULL,
  last_used INTEGER NOT NULL
);
CREATE INDEX idx_cover_cache_last_used ON cover_cache(last_used);
```

down：`DROP INDEX` + `DROP TABLE`。

### 4.4 路径与装配

- `internal/app/config/config.go` 增加 `CoverCachePath()` = `filepath.Join(c.DataDir, "cover-cache")`。
- `internal/app/module.go` 增加 provider：由 `*sqlite.DB`、`*config.Config`、`*slog.Logger`
  构造 `covercache.New`，绑定为 `domain.CatalogCoverCache`；启动时 `MkdirAll`。

## 5. 宿主 RPC（C3 / C4 数据源）

### 5.1 app 层

文件：`internal/app/catalog/service.go`

- `Deps` 增加 `Covers domain.CatalogCoverCache`；`Service` 保存该字段。
- 新增：

```go
func (s *Service) Covers(ctx context.Context, requests []domain.CatalogCoverRequest) []domain.CatalogCoverResult
```

- 每条请求：
  - 规范化 identity；`URL` 为空时回退 `s.cachedCover(identity)`；
  - 仍为空 → `Error: "no_cover_url"`；
  - 读取 `cover_cache_mb`：`sourceSettings.SourceInt(ctx, provider, CatalogSettingCoverCacheMB, 1024)`，钳制到 128–8192，换算字节；
  - 调用 `s.covers.Get`；错误 → `Error: "unavailable"`，不使整页失败；
  - `s.covers == nil` → 全部 `Error: "cache_unavailable"`。
- 搜索页填充 `CoverBatch`：`sourceSettings.SourceInt(ctx, selected[0], CatalogSettingCoverBatch, 5)`，
  钳制 1–20；单源和多源都填。

文件：`internal/app/catalog/module.go`

- `NewService` 的 fx 构造参数追加 `domain.CatalogCoverCache`；
  由于该能力在 app.Module 中提供，非 optional；若无缓存实现则测试直接传 nil。

### 5.2 Wails

文件：`internal/infra/wails/catalog_service.go`

新增 DTO：

```go
type CatalogCoverRequestDTO struct {
    Provider   string `json:"provider"`
    ExternalID string `json:"external_id"`
    URL        string `json:"url"`
}
type CatalogCoverDTO struct {
    Provider   string `json:"provider"`
    ExternalID string `json:"external_id"`
    URL        string `json:"url"`
    Error      string `json:"error"`
}
```

新增方法：

```go
func (s *CatalogService) GetCatalogCovers(requests []CatalogCoverRequestDTO) ([]CatalogCoverDTO, error)
```

只做翻译：调用 `svc.Covers`，逐条映射；单项失败放在 `error`，不返回 Go error。

`CatalogSearchResultDTO` 增加 `CoverBatch int `json:"cover_batch"``。

### 5.3 资产下发

文件：`internal/infra/wails/assets.go`

- `NewAssetHandler(assets, artworkCache, coverCacheDir string)`。
- `"/cover-cache/"` 分支：取 `filepath.Base` 校验文件名（拒绝 `..`、分隔符），
  文件存在则 `http.ServeFile`，否则 404。
- `main.go` 调用处传入 `bootCfg.CoverCachePath()`。
- `assets_test.go` 跟进新签名并新增一个 `/cover-cache/` 命中/穿越测试。

## 6. 前端（C4）

### 6.1 store

文件：`frontend/src/stores/catalog.ts`

- 新增 `coverBatch = ref(5)`；`run()` 收到页面后 `coverBatch.value = page.cover_batch ?? 5`。
- `clearResults()` 重置为 5。
- 导出 `coverBatch`。

### 6.2 CatalogResults

文件：`frontend/src/components/catalog/CatalogResults.vue`

- Props 增加 `coverBatch: number`。
- 删除 IntersectionObserver 相关状态与逻辑。
- 新状态：
  - `coverLimit = reactive<Record<string, number>>({})`：每源当前允许张数；
  - `coverURLs = reactive(new Map<string, string>())`：identity → 宿主本地 URL；
  - `loadedCovers` / `failedCovers` / `requestedCovers` 保留；
  - `coverQueue`、`coverInFlight`、`inFlightImages`、`MAX_CONCURRENT_COVERS = 4` 保留；
  - `coverRpcInFlight = false`。
- 初次与换页：
  - `resetCoverBatch()`：每源上限设为 `max(1, coverBatch || 5)`；
  - `watch(() => props.rows, ...)`：首行 identity 变化或长度变小 → 重置；仅追加 → 保留上限。
- `collectCoverBatch()`：
  - 按 `props.rows` 顺序，逐 `row.provider` 计数；
  - 只取 `count <= coverLimit[provider]`、`row.cover_url` 非空、未请求/未失败/未加载的行；
  - 生成 `{provider, external_id, url: row.cover_url_large || row.cover_url}`；
  - 标记 `requestedCovers` 后再发请求，防重复。
- `requestCovers()`：
  - 若 `coverRpcInFlight` 或无可取行 → 返回；
  - 调 `CatalogService.GetCatalogCovers(items)`；
  - 成功项写入 `coverURLs` 并入图片队列；`error` 项写入 `failedCovers`；
  - 结束后 `pumpCovers()`。
- `advanceCoverBatch()`：每个源上限 `+= max(1, coverBatch || 5)`，再次 `requestCovers()`。
- 滚动：
  - 目标为 `findScrollParent(rootEl)`，找不到则 `window`；
  - `addEventListener('scroll', onScroll, { passive: true })`；
  - 距底部 200px 内调用 `advanceCoverBatch()`；
  - `onUnmounted` 用同一引用 `removeEventListener`。
- 模板：
  - `<img v-if="coverLoaded(row)" :src="coverURLs.get(coverKey(row))">`；
  - 其余逻辑（占位、淡入、失败回退）不变。
- `defineExpose` 保留 `coverQueueSize`、`coverInFlightCount`，增加 `advanceCoverBatch` 供测试。

### 6.3 SearchView

文件：`frontend/src/views/SearchView.vue`

- `CatalogResults` 增加 `:cover-batch="catalogStore.coverBatch"`。

## 7. 测试（C6）

| 层 | 文件 | 断言 |
|---|---|---|
| 插件 | `plugins/wy/test/search.test.js` | 批量两首返回两条 image；坏 hash 整批失败后回退单条；静默失败不改行；`includeCover=false` 不发封面请求；请求头/URL/body 正确 |
| 宿主缓存 | `internal/infra/covercache/cache_test.go` | 第二次相同 URL 不发网络；`last_used` 命中更新；LRU 超限删最旧并降到 95% 以下；文件丢失清索引行；2 MB 上限；非图片拒绝 |
| 宿主 RPC | `internal/infra/wails/catalog_service_test.go` | `cover_batch` / cover DTO 字段名；成功/失败逐项返回；空 URL 回退 |
| 前端 | `frontend/src/components/catalog/CatalogResults.spec.ts` | 初始每源只请求 `cover_batch` 张；滚动追加下一批；在途图片 ≤4；失败不重试；卸载清理 |
| 前端 store | `frontend/src/stores/catalog.spec.ts` | `cover_batch` 透传与重置 |

## 8. 验证命令

```powershell
gofmt -w <changed-go-files>
go build ./...
go test ./internal/infra/covercache/... ./internal/app/catalog/... ./internal/infra/wails/...
golangci-lint run ./internal/infra/covercache/... ./internal/app/catalog/... ./internal/infra/wails/...
pnpm --dir frontend exec vue-tsc --noEmit
pnpm --dir frontend test
node --test   # cwd plugins/wy
node scripts/check-docs.mjs
wails3 generate bindings -d frontend/bindings
```

环境变量按 `AGENTS.md` / `docs/windows-build.md` 的 Windows 段设置。

## 9. 验收

自动项按 §7。人工项由人完成：

1. wy+kg 混排首屏每源最多 5 张封面；
2. 向下滚动逐批出图；
3. 断网后重搜同一关键词，已缓存行仍有图；
4. 上限改 128 MB 后大量搜索，目录不超限；
5. 亮/暗/黑三主题占位与图片正常。

## 10. 风险与取舍

- kg 伪装头失效 → 仅 kg 无封面，占位兜底。
- 批量被限流 → 只降级，不重试轰炸。
- 缓存目录被外部清理 → 索引缺失文件即清行并重下。
- 1 GB 写放大 → 淘汰留 5% 余量。
- 前端重复请求 → 请求前去重；缓存层按 URL 单航班合并。
- 宿主下载器含 kg Referer 属于基础设施边界内的必要适配，不进入 domain/app。
