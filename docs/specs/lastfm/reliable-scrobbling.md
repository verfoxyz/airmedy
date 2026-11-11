# 任务：可靠的 scrobble（失败重试 + 本地待上传队列）

> 状态：待开工
> 现象：网络抖动/Last.fm 限流时 scrobble 直接失败并丢失（日志只有 `lastfm post failed`）
> 范围：只改 scrobble（现在播放/Now Playing 失败可忽略）

---

## 1. 目标

- 达到 scrobble 条件的播放**不丢**：发送失败先落本地队列；
- 网络恢复/下次启动时自动补传；
- 尽量批量提交，减少请求数；
- 失败有上限，避免无限堆积。

## 2. 数据模型

迁移 `000098`（若与网络代理任务并行，代理迁移改用 `000099`）：

```sql
CREATE TABLE lastfm_pending_scrobbles (
    id            TEXT PRIMARY KEY,        -- uuid
    artist        TEXT NOT NULL,
    title         TEXT NOT NULL,
    album         TEXT NOT NULL DEFAULT '',
    album_artist  TEXT NOT NULL DEFAULT '',
    track_number  INTEGER NOT NULL DEFAULT 0,
    duration      INTEGER NOT NULL DEFAULT 0,
    started_at    INTEGER NOT NULL,        -- Unix 秒，scrobble 的 timestamp
    attempts      INTEGER NOT NULL DEFAULT 0,
    last_error    TEXT NOT NULL DEFAULT '',
    next_retry_at DATETIME,
    created_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (artist, title, started_at)
);
CREATE INDEX idx_lastfm_pending_retry ON lastfm_pending_scrobbles(next_retry_at);
```

- 唯一键防止同一播放重复入队；
- 保留 30 天或最多 5000 条，超出删最旧。

## 3. 发送流程

### 3.1 正常路径

1. 播放器触发 scrobble（现有阈值不变）；
2. **先写库**（`status=待发送`），再尝试发送；
3. 成功 → 删除该行；
4. 失败（网络/EOF/5xx/429）→ 保留行，`attempts++`，计算 `next_retry_at`（指数退避：1m、5m、30m、2h、6h，最多 20 次；429 优先用 `Retry-After`）。

### 3.2 补传 worker

- 启动时、每 5 分钟、以及网络请求成功一次后触发；
- 取 `next_retry_at <= now` 的行，按 `started_at` 升序，**每批最多 50 条**（Last.fm 批量 scrobble 上限）；
- 一次请求提交一批（`track.scrobble` 的 `artist[i]`/`track[i]`/`timestamp[i]` 形式）；
- 部分失败：按返回的 ignored 列表处理（标记错误、不再重试）；
- 成功批次从队列删除；
- 超过 20 次或超过 30 天的行丢弃（记日志）。

### 3.3 去重与顺序

- 同一 `(artist,title,started_at)` 只入队一次；
- 按时间先后发送，避免 Last.fm 时间线错乱。

## 4. 代码改动

| 位置 | 改动 |
|---|---|
| `internal/domain` | `PendingScrobble` 模型 + repository 接口 |
| `internal/infra/sqlite` | 迁移 `000098` + repository 实现 |
| `internal/app/lastfm` | `handleScrobble` 改为“先入队再发送”；新增 `flushPendingScrobbles(ctx)` worker（启动/定时/成功后触发）；`scrobbleBatch` 支持批量 |
| `internal/app/module.go` | 注册 worker 生命周期 |

- 发送请求复用现有签名与限流器（并发 2、间隔 ≥400ms，见限流文档）；
- Now Playing 失败不重试、不入队。

## 5. 测试

**Go**

- 发送失败 → 行保留，`attempts=1`，`next_retry_at` 已设置；
- 重试成功 → 行删除；
- 指数退避序列正确；429 使用 `Retry-After`；
- 批量提交 ≤50 条/请求，且按时间升序；
- ignored 列表的处理；
- 超过 20 次/30 天的最旧行被清理；
- 唯一键不重复入队；
- 启动时 flush 已到期行。

**人工**

1. 断网播放一首 → 队列里有记录；
2. 恢复网络 → 几分钟内 Last.fm 出现该 scrobble；
3. 连续离线多首 → 恢复后一次性补传；
4. 不做重复上传（Last.fm 记录只有一条）。

## 6. 验收

- 自动：§5 全绿；
- 人工：§5 四条成立；
- 短时断网/限流不再丢 scrobble。

## 7. 风险

- Last.fm 的 scrobble 时间戳过旧可能被忽略（Last.fm 只接收最近 14 天内的 scrobble；超出直接丢弃并记日志）；
- 批量接口的字段细节（`sk`、签名、数组参数）需按官方文档实现并测试；
- 队列增长由保留策略控制；
- 与“Now Playing”不同，队列只存**已经达到条件**的播放，不会重复计数。
