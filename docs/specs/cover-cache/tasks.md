# Tasks：cover-cache

> 规格：`docs/specs/cover-cache/spec.md`
> 实施顺序按依赖排列；每项含验证点。
> 后续修订：缓存存储结论见 [`fix-r2.md`](fix-r2.md)；搜索页封面 bug 见
> [`fix-search-covers.md`](fix-search-covers.md)。

## T1 插件 kg 批量封面

- [x] `plugins/wy/src/search.js`
  - 新增 `KG_COVER_ENDPOINT`、`KG_COVER_UA`、`buildKugouCoverResource`、
        `kugouCoverURL`、`fetchKugouCovers`。
  - `searchKG` 在 `includeCover` 为真时批量取封面；整批失败逐条重试；静默失败。
  - 240 为列表封面；`cover_url_large` 取 `imgsize` 中 400/480，否则 240。
- [x] `plugins/wy/airmedy-plugin.json`：`version` 1.2.0，network 加 `media.store.kugou.com`。
- [x] `plugins/wy/test/search.test.js`：批处理、坏 hash 重试、静默失败、请求形状。
- 验证：`node --test`（cwd `plugins/wy`）— 54 passed。

## T2 插件设置

- [x] `plugins/wy/wrapper.js` `SETTINGS` 追加 `cover_batch`、`cover_cache_mb`。
- [x] `plugins/wy/test/wrapper.test.js`：describe 返回两个新设置及范围。
- 验证：`node --test` — passed。

## T3 领域契约与常量

- [x] `internal/domain/catalog_search.go`
  - `CatalogSettingCoverBatch` / `DefaultCatalogCoverBatch` / `Min` / `Max`
  - `CatalogSettingCoverCacheMB` / `Default` / `Min` / `Max`
  - `CatalogCoverRequest`、`CatalogCoverResult`、`CatalogCoverCache`
  - `CatalogSearchPage.CoverBatch`
- 验证：`go build ./...` — passed。

## T4 covercache 基础设施与迁移

- [x] `internal/infra/covercache/cache.go`
  - `New` 建目录；`Get` 命中更新 `last_used`；文件丢失清行；
  - 单航班下载；UA/Referer；2 MB 上限；MIME 白名单；
  - 先文件后索引；`SUM(bytes)` 超限按 `last_used` 清到 95%。
- [x] `internal/infra/sqlite/migrations/000089_cover_cache.up.sql` / `.down.sql`
- [x] `internal/infra/covercache/cache_test.go`
  - 命中免网络、last_used 更新、LRU、文件丢失、2 MB、非图片、单航班。
- 验证：`go test ./internal/infra/covercache/...` — passed。

## T5 装配与资产下发

- [x] `internal/app/config/config.go`：`CoverCachePath()`。
- [x] `internal/app/module.go`：提供 `domain.CatalogCoverCache`。
- [x] `internal/infra/wails/assets.go`：`NewAssetHandler(..., coverCacheDir)` + `/cover-cache/`。
- [x] `main.go`：传入 `bootCfg.CoverCachePath()`。
- [x] `internal/infra/wails/assets_test.go`：新签名、命中、路径穿越拒绝。
- 验证：`go build ./...`、`go test ./internal/infra/wails/...` — passed。

## T6 app 层 Covers 与 cover_batch

- [x] `internal/app/catalog/service.go`
  - `Deps.Covers`、`Service.covers`
  - `Covers(ctx, requests)`：URL 回退、`cover_cache_mb` 解析与钳制、逐项错误
  - 搜索页 `CoverBatch` 解析与钳制
- [x] `internal/app/catalog/module.go`：注入 `domain.CatalogCoverCache`。
- [x] 新增 `internal/app/catalog/service_covers_test.go`：URL 回退、clamp、错误不扩散。
- 验证：`go test ./internal/app/catalog/...` — passed。

## T7 Wails RPC 与绑定

- [x] `internal/infra/wails/catalog_service.go`
  - `CatalogCoverRequestDTO`、`CatalogCoverDTO`
  - `GetCatalogCovers`
  - `CatalogSearchResultDTO.CoverBatch`
- [x] `internal/infra/wails/catalog_service_test.go`：JSON 字段、逐项成功/失败。
- [x] 生成绑定：`wails3 generate bindings -d frontend/bindings` — 208 methods / 76 models。
- 验证：`go test ./internal/infra/wails/...` — passed。

## T8 前端滚动分批

- [x] `frontend/src/stores/catalog.ts`：`coverBatch`、页面透传、清空重置。
- [x] `frontend/src/views/SearchView.vue`：传 `cover-batch`。
- [x] `frontend/src/components/catalog/CatalogResults.vue`
  - 删除 IntersectionObserver；
  - 初始每源 `coverBatch`；滚动每批追加；
  - 调用 `GetCatalogCovers`，本地 URL 交图片队列；
  - 并发 ≤4、失败不重试、卸载清理 `scroll` 监听与图片回调。
- [x] `frontend/src/components/catalog/CatalogResults.spec.ts`：重写封面相关用例。
- [x] `frontend/src/stores/catalog.spec.ts`：`cover_batch` 透传/重置。
- 验证：`pnpm --dir frontend exec vue-tsc --noEmit`、`pnpm --dir frontend exec vitest run <两文件>` — 45 passed。

## T9 文档同步（AGENTS §8 要求，原稿缺失）

- [x] `catalog/ui/README.md`：封面加载从「进入视口即取」改为「滚动分批」，
      写明每源初始 `cover_batch` 张、滚动追加、并发 ≤4；旧的 IntersectionObserver 契约删除。
- [x] `catalog/plugins/README.md`：插件设置新增 `cover_batch` / `cover_cache_mb` 两项及范围；
      补充 kg 批量封面与宿主缓存职责。
- [x] `catalog/database/README.md`：新增 `cover_cache` 表（列、索引、LRU 淘汰语义）。
- 验证：`node scripts/check-docs.mjs` — `catalog/: 20 areas, 21 files — ok`。

## T10 全量验证

- [x] `gofmt -w` 所有改动 Go 文件
- [x] `go build ./...`
- [x] `go test ./internal/... .`
- [x] `golangci-lint run` 改动包 — `0 issues`
- [x] `pnpm --dir frontend exec vue-tsc --noEmit`
- [x] `pnpm --dir frontend test`（本机 `build/desktopFonts.build.spec.ts` 在默认 5s 超时；以
      `--testTimeout=20000` 单独复跑 3/3 通过。该测试与本次改动无关）
- [x] `node --test`（cwd `plugins/wy`）— 54 passed
- [x] `node scripts/check-docs.mjs`
- [x] 明确说明：未做图形化验证。

## 提交拆分建议

> `plugins/` 在 `.gitignore` 里（只 `plugins/README.md` 入库），所以第 1 条提交不可行，
> 插件改动只作为本机文件保留；其余按下列拆分。

1. `feat(plugin): kg 批量封面与 cover_batch/cover_cache_mb 设置`（插件侧，不入库）
2. `feat(infra): cover-cache 持久缓存与 000089 迁移`
3. `feat(catalog): GetCatalogCovers RPC 与 cover_batch 下发`
4. `feat(ui): 搜索结果滚动分批加载封面`
5. `docs(meta): cover cache contract and batch loading`
6. `test: 插件封面、缓存淘汰与前端分批`
