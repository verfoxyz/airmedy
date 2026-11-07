# 修订 R2：封面缓存保持文件存储（不改为数据库 BLOB）

> 状态：结论已定（2026-11-07 讨论后修正原 R2 §3）
> 本文只管**缓存存储形态**。原 R2 里的搜索页问题（kg 无封面、每源 5 张后停滞）
> 已迁到 [`fix-search-covers.md`](fix-search-covers.md)，并补充了根因与测试要求；
> 两单互不依赖，可以分开执行。

---

## 1. 结论

封面缓存继续使用**本地文件 + 主库 `cover_cache` 索引表**，维持第一轮实现：

- 图片文件落在 `<data dir>/cover-cache/`；
- `cover_cache` 存 `url_hash / path / bytes / mime / last_used`，只做索引与 LRU 依据；
- 不新建 `cover-cache.db`，不把图片内容存成 BLOB，不新增 `000090`。

## 2. 依据

| 维度 | 文件 + 索引（采用） | 数据库 BLOB（否决） |
|---|---|---|
| 体积 | 主库不随图片增长；实测 `artwork/` 已 256 MB，封面按设计可到 1 GB | 主库或独立库文件膨胀，备份、`VACUUM`、跨设备同步都受累 |
| 读取 | asset handler 直接按文件下发，流式，不经 SQLite 页缓存 | 整块读进内存再写回，1 GB 级缓存的内存峰值不可控 |
| 回收 | 删文件立即回收，无 `VACUUM` | 删除 BLOB 不缩文件，需要 `incremental_vacuum` 重写 |
| 一致性 | 缓存可再生；文件缺失时 `hit()` 删行并重下，写文件后才 upsert | 事务上更简单，但这里没有需要它的强一致场景 |
| 现有模式 | 与 `internal/infra/artwork` 一致 | 需要第二套存储模型 |

唯一偏向 BLOB 的场景（单文件分发、跨设备同步、图片极小且要求强一致事务）当前都不成立：
桌面单机、缓存可重建、上限 1 GB。

## 3. 保持不动的实现

以下文件/行为**不要**为了"把缓存放进数据库"而改动：

| 位置 | 语义 |
|---|---|
| `internal/infra/covercache/cache.go` | 下载（UA/Referer、2 MB 上限、MIME 白名单）、单航班、文件先写、索引 upsert、命中只 `UPDATE last_used`、`SUM(bytes)` 超限按 `last_used` 淘汰到 95% |
| `internal/infra/sqlite/migrations/000089_cover_cache.*` | 主库索引表与 `last_used` 索引；本机库已升到 89，删除它会让现有数据目录打不开 |
| `internal/infra/wails/assets.go` | `/cover-cache/<hash>` 下发与路径校验 |
| `internal/app/catalog/service.go` `Covers()` | URL 回退、`cover_cache_mb` 解析与钳制、逐项错误 |
| `frontend` 侧 | 只认 `/cover-cache/<hash>`，与存储形态无关 |

## 4. 记录在案、但本轮不做的加固

列在这里是为了避免后续重复提议；没有实际症状前不动：

- 孤儿文件清理（upsert 失败或并发写留下的无索引文件）——目前无害，只占空间；
- 启动时对已超限缓存做一次收敛——现在只在写入后淘汰，超限只可能来自外部拷贝；
- `last_used` 秒级粒度在大量同秒写入时的淘汰顺序抖动——不影响正确性。

## 5. 验收

- `go test ./internal/infra/covercache/...`
- `go build ./...`、`go test ./internal/... .`
- 人工（可选）：把 `cover_cache_mb` 调小后连续搜索，缓存目录回落到上限的 95% 以下；同一 URL 第二次请求不再走网络。

> 本单不需要改动任何代码。若评审后仍决定改用数据库，需要重开一份新文档，并先解决
> 迁移冲突（本机库已在 89 且已建表）。
