# 任务清单：流播放 M4–M6（按顺序执行）

> 用途：交给编程 AI 直接执行
> 依据：[spec.md](spec.md)（契约）、[task-cache-db.md](task-cache-db.md)、[task-prebuffer.md](task-prebuffer.md)、[task-meta-writes.md](task-meta-writes.md)
> 前置：M1（FFmpeg 网络）、M2（StreamCache）、M3（播放器接入）已完成；`go build ./...` 与现有测试全绿

## 执行规则

- 严格按 T1 → T9 顺序，**T1 完成前不要动 M4 的淘汰/关联逻辑**（它们都依赖数据库表）；
- 每个任务自己跑测试，绿了再进下一个；
- 新增/修改的面向用户文案必须同步 **12 个 locale**；不新增依赖；迁移必须有 down；
- 提交信息用 Conventional Commits；每个任务单独提交；
- 不做 spec「§8 后续优化」里的音质降级链和跨源找音质。

---

## T1 · stream-cache 元数据迁入数据库（M4 基础）

**依赖**：无
**文档**：`task-cache-db.md`
**改动**：

- 迁移 `000091_stream_cache.up/down.sql`：`stream_cache` 表（`cache_key`、`provider`、`external_id`、`title/title_key`、`artist/artist_key`、`artists`、`album`、`duration_ms`、`quality`、`audio_path`、`total_bytes`、`received_bytes`、`complete`、`playback_progress`、`pending_promote`、`created_at`、`last_used` + 三个索引）；
- `internal/infra/sqlite/stream_cache_repository.go` + 领域接口/模型；
- `internal/app/stream/cache.go`：删掉 JSON 读写，改仓储；`Open` 先按身份查、再按 `title_key+artist_key+duration(±5s)` 跨源匹配；记录请求的 `quality`；
- 启动时清理 `stream-cache/` 下的旧 `.json` 与无引用的 `.audio`。

**测试**：仓储 CRUD、身份/跨源命中、超出时长容差不命中、时长为 0 跳过、迁移 up/down、淘汰仍按完整度→播放进度。
**验收**：`go test ./internal/app/stream/ ./internal/infra/sqlite/ ./internal/domain/` 全绿；目录里只剩 `.audio`。

---

## T2 · 进度写盘降频

**依赖**：T1
**文档**：`task-meta-writes.md`（把 `persistMeta` 换成 `UpdateProgress`）
**改动**：`ReportProgress` 只改内存；在关闭/暂停/完成/失败/退出时写；加 30 秒兜底写一次。
**测试**：播放中不写、关闭写、暂停写、重开后进度可读、淘汰排序仍生效。
**验收**：播放时观察 DB/文件写入明显减少；资源管理器不再丢选中（JSON 已消失）。

---

## T3 · 封面缓存与音乐缓存关联（M4 收尾）

**依赖**：T1
**文档**：spec §4.5
**改动**：

- 迁移 `000092`：`cover_cache` 增加 `provider`、`external_id`；
- `GetCatalogCovers` 写入时记录 identity；
- StreamCache 创建/完成时 pin 对应封面，淘汰/删除时 unpin；
- 淘汰排序：先删未引用的（按 `last_used`），再删引用的（同样按 `last_used`）；
- 合并行（多 identity）只要任一有音乐缓存就算引用。

**测试**：pin/unpin、分层淘汰顺序、无 identity 时回退纯 LRU。
**验收**：测试全绿；手工把封面上限调很小，缓存音乐的封面最后才被删。

---

## T4 · 下载管线新增「提升缓存文件」入口

**依赖**：T1
**改动**：`internal/app/download` 新增 `PromoteCachedFile(ctx, trackID, localPath, quality)`（命名可调）：

- 复用现有下载完成后的步骤：写标签、封面、歌词、与 remote 行 merge；
- 不重新触发网络下载；
- 幂等：重复调用不产生重复文件/行。

**测试**：用临时文件模拟缓存，断言标签写入、remote 行合并、重复调用幂等。
**验收**：`go test ./internal/app/download/` 全绿。

---

## T5 · 提升触发与音质规则（M5）

**依赖**：T1、T4
**文档**：spec §4.4
**改动**：

- 加入歌单、收藏 → 若 remote 且未提升 → `pending_promote = 1`；
- 非播放中：后台补全缓存（Range 续传）；播放中：等播放结束/切走；
- 完成后按音质分支：
  - 缓存音质 == 下载音质 → 直接提升；
  - 缓存音质更高 → 直接提升；
  - 下载音质更高 → 按下载音质走普通下载，成功后删缓存（音频 + 行）；
  - 缓存音质未知 → 视为最低；
- 提升后歌单条目指向本地曲目。

**测试**：四种音质分支各一条；播放中延迟提升；重复触发幂等。
**验收**：手工：流播放中加歌单 → 结束后文件出现、条目变本地；改高下载音质再收藏 → 走重下载。

---

## T6 · 歌单 remote 行播放入口（M6-a）

**依赖**：T1（跨源匹配可选）
**改动**：

- 播放列表里 remote 行可点击播放（复用 M3 的 `preparePlayback` / 播放路径）；
- 播放失败统一显示「失败 + 重试」，不自动换源/下载；
- 12 个 locale 补文案。

**测试**：组件/service 测试：remote 行播放调用解析；失败展示重试。
**验收**：手工在歌单里播一首未下载曲目。

---

## T7 · 设置 UI：缓存上限与音质（M6-b）

**依赖**：无（后端已有字段）
**改动**：

- 设置 → 下载（或播放）：新增 `stream_cache_mb`（数字输入，256–20480）与 `stream_quality`（下拉 `128k/320k/flac`，默认 320k）；
- `stream_quality != download_quality` 时显示感叹号；流播放音质低于下载音质时提示「建议设置相同音频质量」（i18n 12 个语言）；
- 复用现有 `Select`/`Input`/`SettingRow`，不写裸 input。

**测试**：设置读写、范围钳制、提示条件。
**验收**：改设置后重启仍生效。

---

## T8 · 流播放封面（M6-c）

**依赖**：T1（建议 `stream_cache` 存 `cover_url`）或 T3
**改动**：

- 播放远程曲目时把封面解析成 `cover-cache` 本地 URL，随播放状态下发；
- 歌曲信息、左下角缩略、迷你播放器共用同一来源；无封面用占位图；
- 重启后仍能显示（来自 `stream_cache.cover_url` 或 `cover_cache` identity 关联）。

**测试**：远程曲目播放状态含封面 URL；无封面回退占位图。
**验收**：手工播放一首云端曲目，三处都有封面。

---

## T9 · 全量回归与人工验收

1. `go build ./...`、`go test ./internal/...`；
2. `vue-tsc --noEmit`、前端 vitest（已知 `desktopFonts.build.spec.ts` 在默认 5s 超时，可单独 `--testTimeout=20000`）；
3. 人工清单：
   - 搜索结果播放、拖动进度、暂停续播；
   - 播放中切歌（旧 session 关闭）；
   - 加歌单/收藏后的延迟提升、音质分支；
   - 跨源复用（wy 缓存 → kg 播同一首不重新下载）；
   - 缓存上限触发，淘汰顺序正确（完整度 → 进度，且被引用封面后删）；
   - 重启后队列恢复、remote 行可继续播、封面仍在；
   - 断网/源失效 → 提示重试，不崩溃。

---

## 提交拆分（建议）

1. `feat(infra): move stream cache metadata into sqlite`（T1）
2. `perf(stream): throttle cache progress writes`（T2）
3. `feat(stream): link cover cache to streamed audio`（T3）
4. `feat(download): promote an already cached file`（T4）
5. `feat(stream): promote cached tracks on playlist and favorite`（T5）
6. `feat(ui): play remote playlist rows and retry failed streams`（T6）
7. `feat(ui): stream cache and quality settings`（T7）
8. `feat(ui): cover for streamed playback`（T8）
