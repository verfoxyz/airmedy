# Bug：共享专辑封面只记住一个 identity，导致个别曲目拿不到封面

> 状态：待开工
> 现象：在线播放「Heart of Android : Even If It's Only By Mechanism」时没有封面，其它曲目正常
> 范围：封面缓存的 identity 关联；不改播放/缓存音频逻辑

---

## 1. 现象与证据

- 曲目：`wy / 1336656391 / Heart of Android : Even If It's Only By Mechanism`，`tracks.artwork_key` 为空；
- 它的封面 URL 是 `https://p2.music.126.net/5yoSxiYtRU9kINBf7LMzew==/109951165777230224.jpg?param=500y500`；
- 该 URL **已经在 `cover_cache` 里**（41897 字节），但记录的是：

```
provider = wy, external_id = 1336689106   -- 同专辑的另一首（Tojita Sekai）
```

- `BytesForIdentity(wy, 1336656391)` 查不到 → 播放器无法把封面物化进 artwork 管线 → `artwork_key` 为空；
- 之前 `GetCatalogCovers` 查询该 identity 时也命中不到，因此状态一直为空。

> 同专辑多首曲目共用同一个 `picUrl`；缓存按 **URL** 去重，但 `cover_cache.provider/external_id` 是**单值列**，只能记住写它的最后一首曲目。

## 2. 根因

1. `covercache` 以 `url_hash` 为唯一键（正确：同一张图只下载一次）；
2. 但 identity 记录在 `cover_cache` 行的 `provider/external_id` 上，是一对一；
3. 同一封面被多首曲目共享时，只有一条 identity 生效；其余曲目按 identity 查询永远 miss；
4. 播放器的封面物化只按 identity 查（`BytesForIdentity`），没有 URL 可回退，所以没有封面。

## 3. 修法（建议：identity 改为多对多）

### 3.1 迁移 `000097`

```sql
CREATE TABLE cover_cache_links (
    url_hash    TEXT NOT NULL,
    provider    TEXT NOT NULL,
    external_id TEXT NOT NULL,
    created_at  INTEGER NOT NULL,
    PRIMARY KEY (url_hash, provider, external_id)
);
CREATE INDEX idx_cover_cache_links_identity ON cover_cache_links(provider, external_id);
CREATE INDEX idx_cover_cache_links_hash ON cover_cache_links(url_hash);
```

- 保留 `cover_cache` 作为“URL → 文件”的唯一记录；
- identity 关联移到 links，多对多；
- 旧 `cover_cache.provider/external_id` 保留一版用于回滚，读取路径全部走 links（迁移时把旧列非空的行插入 links，作为回填）。

### 3.2 `internal/infra/covercache`

- `Get(ctx, remoteURL, provider, externalID, limit)`：命中/下载后写 `cover_cache_links`（`INSERT OR IGNORE`），不再只写单值列；
- `AssetForIdentity` / `BytesForIdentity`：按 `cover_cache_links` join `cover_cache` 查询；
- `Pin` / `Unpin`：按 links 的 identity 更新（pin 仍落在 `cover_cache.pinned`；一个 URL 被任一 identity pin 即算 pin）；
- 淘汰：pin 判定改为 `EXISTS(links for pinned identities)` 或继续用 `cover_cache.pinned`（推荐后者，保持简单）。

### 3.3 调用方

- `catalog.GetCatalogCovers` 已经带 identity 与 URL，无需改签名；
- 播放器物化（`player_service.go`）仍用 `BytesForIdentity`，links 修好后即可命中；
- 老数据：迁移时从 `cover_cache` 的旧 identity 列回填 links；之后每次 `Get` 补链。

## 4. 备选方案（不推荐）

- **在 track 行存 `cover_url`**，物化时按 URL 查：能修这一例，但要新增列、维护 URL 生命周期，且解决不了“按 identity 查询已缓存的封面”这个根本问题；
- **每次物化都重新搜索**：引入网络请求，不可取。

## 5. 测试

**Go**

- 同一 URL 两个 identity：两次 `Get` 后，`BytesForIdentity` 两个都能命中；
- 迁移 `000097` up/down；旧行的 identity 回填到 links；
- `Pin`/`Unpin` 对一个被多 identity 共享的 URL 行为正确（任一 pin 生效）；
- 淘汰仍按 `last_used`，pin 的 URL 后删；
- 现有 `covercache` 测试保持通过。

**人工**

1. 播放「Heart of Android」→ 出现封面（下载后 `tracks.artwork_key` 非空）；
2. 同专辑另一首播放 → 同样有封面；
3. 清空一次封面缓存后重播两首，仍然都有封面；
4. 重启后仍显示（artwork_key 已持久化）。

## 6. 验收

- 自动：§5 全绿；
- 人工：§5 四条成立；
- 不再出现“同一张专辑封面只有一首歌有封面”的情况。

## 7. 风险

- links 表让删除/淘汰需要同时清理（`ON DELETE CASCADE` 或显式 delete）；
- 迁移回填只覆盖已有的单值 identity，历史的空 identity 行仍无法恢复（但下次 `Get` 会补链）；
- 封面 pin 的语义从“行”变为“URL 被任一 identity pin”，与现有 pin 淘汰逻辑保持一致即可。
