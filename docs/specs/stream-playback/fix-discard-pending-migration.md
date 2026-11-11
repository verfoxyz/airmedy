# Bug：修改已应用的迁移导致 `stream_cache` 读取失败（播放失败请重试）

> 状态：待开工
> 影响：搜索页点在线播放 → 「播放失败请重试」；提升 worker 每 30 秒报错
> 范围：迁移文件与迁移测试；不涉及业务逻辑

---

## 1. 现象

1. 搜索页点击在线曲目播放，提示「播放失败请重试」；
2. 日志每 30 秒重复：

```
WARN promote: cannot list pending discards  error="failed to list pending discards: no such column: discard_pending"
WARN promote: cannot list pending entries   error="failed to list pending promotions: no such column: discard_pending"
WARN Failed to list stream cache entries for startup cleanup error="... no such column: discard_pending"
```

## 2. 根因

**已应用过的迁移 `000094_stream_cache_promoted` 被事后修改，新增了列。**

- 当前代码（`stream_cache_repository.go` 的 SELECT/UPDATE）已经使用 `discard_pending`；
- 但迁移文件最初只包含 `promoted`，用户数据库已经执行到版本 **95**，`000094` 被标记为已应用，**不会重跑**；
- 于是数据库里 `stream_cache` 缺少 `discard_pending` 列，所有读取该表的路径（`Cache.Open`、启动清理、维护 worker）全部失败 → 播放失败。

验证（本机）：

```sql
PRAGMA table_info(stream_cache);          -- 只有 promoted，没有 discard_pending
SELECT version FROM schema_migrations;     -- 95
```

## 3. 修法

### 3.1 迁移文件

1. **还原 `000094_stream_cache_promoted.up.sql`**：只保留 `promoted` 一列（把它改回“已应用版本”的内容）；
2. **新增 `000096_stream_cache_discard_pending.up/down.sql`**：

```sql
-- up
ALTER TABLE stream_cache ADD COLUMN discard_pending INTEGER NOT NULL DEFAULT 0;

-- down
ALTER TABLE stream_cache DROP COLUMN discard_pending;
```

3. 不要动 `000095`（`promotion_tasks`，已应用）。

> 编号说明：`000094` = promoted（已应用）、`000095` = promotion_tasks（已应用）、`000096` = discard_pending（新）。

### 3.2 已升级的数据库

- 迁移框架会自动补跑 `000096`，无需手工改库；
- 不要再建议用户手工 `ALTER TABLE`，除非是已经手工修过的开发机（那种情况 `schema_migrations` 与列状态不一致，需先统一）。

### 3.3 代码侧确认

- `000096` 应用后，`stream_cache` 的 SELECT/UPDATE/列裁剪保持一致；
- `promoted` 与 `discard_pending` 的语义见 [task-promotion-worker.md](task-promotion-worker.md)；
- 确认 `SetDiscardPending`、`ListPendingDiscard`、`DiscardEntry` 在列存在后正常工作。

## 4. 测试（必须补）

1. **迁移链路测试**：从一个只到 `000095` 的数据库开始，跑 `Migrate` 到最新，断言：
   - `stream_cache` 同时有 `promoted` 与 `discard_pending`；
   - `promotion_tasks` 存在；
   - 无 dirty 状态；
2. **回滚测试**：`000096.down` 能删列且其它迁移不受影响；
3. **回归**：`stream_cache_repository_test.go` 在迁移后通过（此前它测的是“新建库”，没覆盖“旧库升级”，所以没发现）；
4. **启动冒烟**：用旧版本数据库副本启动应用，日志不再出现 `no such column: discard_pending`。

## 5. 验收

- 自动：§4 全绿，`go build ./...` 通过；
- 人工：
  1. 用现有（已升级到 95）数据库启动新构建 → 无迁移错误；
  2. 搜索页点在线曲目播放 → 正常出声，不再提示「播放失败请重试」；
  3. 提升 worker 日志不再刷 `no such column`。

## 6. 教训 / 规则

- **永远不要修改已经发布或已被本地数据库执行过的迁移文件**；需要新列/新表时，新增迁移；
- 迁移测试要覆盖“旧版本升级”，不能只测“空库新建”；
- 合并前用一份旧数据库副本启动一次应用（冒烟），比只跑空库测试更能发现问题。

## 7. 风险

- 若有人已经手工给开发库加过 `discard_pending`，再跑 `000096` 会因列已存在而失败：先确认 `schema_migrations` 与列状态一致，再决定手工标记版本；正式用户不会遇到（他们不会手工改库）。
