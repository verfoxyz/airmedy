# 计划：远端封面获取与持久缓存

> 状态：待开工
> 前置：`aggregate-search-tx-kg-plan.md`（多源、排行、`max_display`）
> 硬约束：AI 不做图形化测试；封面观感由人工确认。

---

## 1. 目标

1. **kg 也能出封面**（此前结论作废，见 §2）；
2. 封面**分批加载**：静止时每个源最多 5 张，滚动再取下一批；
3. 封面**持久缓存**在本地，上限默认 **1024 MB**，超限删最旧的。

---

## 2. kg 封面：可行，一次批量请求

参考实现 `any-listen-extension-online-metadata/src/onlineResource/kg/pic.ts`，实测（2026-11）：

```
POST http://media.store.kugou.com/v1/get_res_privilege
Content-Type: application/json
KG-RC: 1
KG-THash: expand_search_manager.cpp:852736169:451
User-Agent: KuGou2012-9020-ExpandSearchManager

{ "appid":1001, "area_code":"1", "behavior":"play", "clientver":"9020",
  "need_hash_offset":1, "relate":1, "token":"", "userid":2626431536, "vip":1,
  "resource":[ {"album_audio_id":…, "album_id":…, "hash":…, "id":0,
                "name":"歌手 - 歌名.mp3", "type":"audio"}, … ] }
```

返回每项：

```json
"image": "http://imge.kugou.com/stdmusic/{size}/20230920/20230920142503632013.jpg",
"imgsize": [480, 400, 240, 150, ...]
```

- `{size}` 换成 240 后实测 **200 + 真 JPEG（29.9 KB）**，不是占位图；
- `resource` 是**数组**：两首实测一次返回两条，**每页只需一次 POST**；
- **失败模式**：只要有一条 hash 无效，整批返回 `status: 0`、`data: []`。所以
  **一批失败时按单条重试**，仍失败的行保持无封面；
- 这批请求在**插件内**完成，宿主只收最终的 `cover_url` / `cover_url_large`。

---

## 3. 分批加载

| 规则 | 说明 |
|---|---|
| 初始批量 | **每个源最多 5 张**，与「没有动作时不加载」一致 |
| 触发 | 用户滚动（滚动事件）→ 请求下一批 5 张/源；不是"进入视口即取" |
| 并发 | 同时在途的封面请求 ≤ 4 |
| 顺序 | 按行在结果里的顺序，先上后下 |
| 失败 | 该行保持占位图，不重试（本页内） |

**为什么不是纯视口触发**：`max_display` 允许多源混排几十行，一次可见就可能十几张；
滚动分批能让首屏更快出结果文字。

> 已有实现复盘：`CatalogResults.vue` 现在用 IntersectionObserver + 200px 预取 + 4 并发。
> 本轮把"进入视口"改成"滚动推进批次"，其余（占位、失败回退、清理）沿用。

---

## 4. 持久缓存

### 4.1 放哪

**宿主侧**（`internal/infra/` 新增 `covercache`），不放插件里。原因：

- 插件是 Node 子进程，**无法把图片交给 WebView**；只能返回 URL；
- 宿主已有 artwork 资产服务（`artwork/` 目录 + asset handler），复用同一套下发方式；
- 缓存的淘汰策略属于存储层，放宿主更自然。

目录：`%LOCALAPPDATA%\airmedy\cover-cache\`，文件名用 URL 的 sha256，保持扩展名。

### 4.2 索引与淘汰

SQLite 新表（迁移）：

```sql
CREATE TABLE cover_cache (
  url_hash   TEXT PRIMARY KEY,
  path       TEXT NOT NULL,
  bytes      INTEGER NOT NULL,
  mime       TEXT NOT NULL,
  last_used  INTEGER NOT NULL     -- unix 秒
);
CREATE INDEX idx_cover_cache_last_used ON cover_cache(last_used);
```

- 写入：先落文件，再插/更新索引；
- **命中**：更新 `last_used`（读写放大可接受，一次 UPDATE）；
- **淘汰**：写入后若 `sum(bytes) > limit`，按 `last_used` 升序删文件与行，直到低于上限的 95%
  （留一点余量，避免每写一张就淘汰一次）；
- **上限来源**：插件设置 `cover_cache_mb`（默认 **1024**，范围 128–8192）。

### 4.3 下载

- 宿主按 `cover_url_large`（没有则 `cover_url`）下载，带 UA 与 Referer（kg 的图床要 Referer）；
- 单张上限 2 MB，超限视为失败；
- 失败不写缓存，前端回落占位图；
- **下载发生在宿主**，因为要落盘；前端只负责挑哪些行需要图。

### 4.4 前端怎么拿到图

宿主在 `CatalogSearchResult` 之外新增：

```
GetCatalogCovers(identities[] / urls[]) -> [{ identity, url } | { identity, error }]
```

- 前端对"需要封面的行"调用它，拿到宿主本地缓存的 URL（asset scheme）后设给 `<img>`；
- 已有 `cover_url` 字段仍保留（远端原始 URL），用于导出/调试，不直接渲染。

---

## 5. 设置项（插件 `describe.settings`）

| 键 | 类型 | 默认 | 范围 | 作用 |
|---|---|---|---|---|
| `cover_batch` | number | 5 | 1–20 | 静止时每源先取几张，滚动每批多少张 |
| `cover_cache_mb` | number | 1024 | 128–8192 | 封面缓存上限（MB） |

两者都复用上一轮已经实现的 `type: "number"`。

---

## 6. 工作分解

| # | 任务 | 估时 |
|---|---|---|
| C1 | 插件：kg 批量封面（重试、失败降级、240 尺寸） | 1 天 |
| C2 | 宿主：`covercache`（下载、落盘、索引、LRU 淘汰、上限读取） | 1.5 天 |
| C3 | 宿主：`GetCatalogCovers` RPC 与资产下发 | 1 天 |
| C4 | 前端：改成滚动分批（替换视口触发）、占位与失败回退 | 0.5 天 |
| C5 | 插件设置 `cover_batch` / `cover_cache_mb` | 0.5 天 |
| C6 | 测试：插件封面批处理、缓存命中/淘汰、并发上限、RPC 契约 | 1 天 |

**合计约 5.5 天。**

---

## 7. 验收

**自动**

- 插件：两首一批返回两条 image；一批含坏 hash → 回退单条重试；静默失败；
- 缓存：第二次相同 URL **不发网络请求**；写入后总量超限时按最旧删除且降到 95% 以下；
- 缓存：`last_used` 命中即更新；文件丢失时索引行被清理；
- 前端：初始只请求每源 5 张；滚动后追加下一批；同时在途 ≤4。

**人工（由人做，AI 不做）**

1. wy+kg 混排：首屏**每个源最多 5 张封面**，其余是占位；
2. 向下滚动：占位逐批变成封面；
3. 断网后重新搜索同一关键词：**已缓存的行仍有封面**；
4. 把缓存上限改成 128 MB 再搜很多歌：旧封面被清掉，磁盘目录不超过上限；
5. 亮/暗/黑三主题下占位与图片观感正常。

---

## 8. 风险

| 风险 | 应对 |
|---|---|
| kg 伪装头失效 | 只影响 kg 封面，占位图兜底；接口变化时单独修 |
| 批量请求被限流 | 每页一次 POST；失败退化为无封面，不重试轰炸 |
| 缓存目录被外部清理 | 索引查不到文件时删除该行并重新下载 |
| 1 GB 上限的写放大 | 淘汰留 5% 余量；索引按 `last_used` 建索引 |
| 前端重复请求同一批 | 请求前按 identity 去重，宿主侧也做在途合并 |
