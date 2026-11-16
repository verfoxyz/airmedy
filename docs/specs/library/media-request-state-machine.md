# 重构：以「媒体请求」为唯一权威的保存/提升/下载状态机

> 状态：待开工（重构设计）
> 目的：根治“提升逻辑混乱”——重复任务、漏触发、状态分散、触发模型反复打补丁
> 关联现状：`fix-save-prefers-promotion.md`、`task-promotion-worker.md`、`downloads-tasks.md`、`fix-bulk-playlist-add.md`、`cache-task-history.md`

---

## 1. 为什么要重构（证据）

| 现象 | 根因 |
|---|---|
| `Rhuzerv` 出现 queued + done 两条提升任务 | 同一首歌被重复触发，没有唯一键 |
| `misery.` 批量加歌单后没有提升 | 多入口各自实现，批量路径漏了钩子 |
| 缓存 flags（`pending_promote`/`promoted`/`discard_pending`）与任务表不一致 | 状态分散在 3 张表 + 内存 worker |
| 触发模型改过：轮询 → 事件 → 事件+看门狗 | 没有单一状态机，靠补丁 |
| 搜索行、任务页、Last.fm 行状态来源不同 | 各自读自己的数据源 |

结论：**缺少“一个 identity 一个请求、一个 worker 推进”的模型。**

---

## 2. 目标

1. 所有“用户想把这歌存进库”的入口，只 upsert 一条**媒体请求**；
2. 只有一个 worker 推进请求，串行、幂等、可恢复；
3. 下载、提升、缓存复用都变成请求的**执行细节**；
4. UI 的所有状态从请求派生；
5. 迁移可灰度、可回滚。

## 3. 非目标

- 不重写下载器/StreamCache 本身；
- 不改播放链路；
- 不在本重构里改音质策略（沿用现有降级链与比较规则）。

---

## 4. 数据模型

### 4.1 `media_requests`（唯一权威）

```sql
CREATE TABLE media_requests (
    id            TEXT PRIMARY KEY,          -- uuid
    provider      TEXT NOT NULL,
    external_id   TEXT NOT NULL,
    track_id      TEXT,                      -- 最新指向的曲目行（可能被 merge 替换）
    desired       TEXT NOT NULL,             -- download | promote
    quality       TEXT NOT NULL DEFAULT '',  -- 用户请求音质
    state         TEXT NOT NULL DEFAULT 'wanted',   -- wanted|queued|running|done|failed|canceled
    phase         TEXT NOT NULL DEFAULT '',         -- resolving|downloading|promoting|verifying
    attempts      INTEGER NOT NULL DEFAULT 0,
    last_error    TEXT NOT NULL DEFAULT '',
    next_attempt_at DATETIME,
    created_at    DATETIME NOT NULL,
    updated_at    DATETIME NOT NULL,
    finished_at   DATETIME,
    UNIQUE (provider, external_id)
);
CREATE INDEX idx_media_requests_state ON media_requests(state, next_attempt_at);
```

- **唯一键保证幂等**：重复收藏/重复加入歌单不会产生第二条；
- `track_id` 随 merge 更新；
- `desired/quality` 记录用户意图，worker 执行时再和当前缓存/设置比较。

### 4.2 历史（可选，供任务页）

```sql
CREATE TABLE media_request_events (
    id TEXT PRIMARY KEY,
    request_id TEXT NOT NULL,
    state TEXT NOT NULL,
    phase TEXT NOT NULL,
    detail TEXT NOT NULL DEFAULT '',
    created_at DATETIME NOT NULL
);
```

- 任务页可以只显示请求当前状态（推荐），事件表用于“已完成/出错”的历史明细；
- 也可以让 `ListTasks` 以 `media_requests` 为主、下载/缓存表为补充。

### 4.3 旧表降级

| 旧数据 | 新角色 |
|---|---|
| `download_jobs` | 下载器执行细节（字节、重试），由请求驱动 |
| `promotion_tasks` | **删除**，历史由请求/事件表替代 |
| `stream_cache.pending_promote` | **删除**；提升需求只存在于请求 |
| `stream_cache.promoted` | **删除**；文件是否已移动由请求/本地行判断 |
| `stream_cache.discard_pending` | **删除**；缓存清理由请求完成事件驱动 |
| `stream_cache` 其余列 | 保持（complete/progress/quality 等） |
| `stream_cache_tasks` | 保留（缓存留痕，不属于请求） |

---

## 5. 状态机

### 5.1 状态

| state | 含义 |
|---|---|
| `wanted` | 用户已请求，等待 worker 处理 |
| `queued` | 已排到执行队列（可选，用于 UI） |
| `running` | 正在解析/下载/提升 |
| `done` | 已在库（本地文件存在） |
| `failed` | 最近一次失败，等待退避重试 |
| `canceled` | 用户取消（本期不做 UI，保留状态） |

`phase` 仅用于 `running` 的子阶段：`resolving`（取流地址）、`downloading`（补全/下载）、`promoting`（移动+入库）、`verifying`（校验本地行）。

### 5.2 推进规则（worker 每一步只做一件事）

1. 读请求 → 若本地行已是可播文件 → `done`；
2. 查流缓存：完整 → `phase=promoting`；不完整/不存在 → `phase=resolving`；
3. 质量比较：
   - 缓存/文件音质 ≥ 期望 → 提升；
   - 低于期望 → 走下载（按降级链）；
4. 执行前置条件：
   - 无活动 stream session；
   - 播放器未持有该曲目（`PlayingTrack` 且在 `Playing/Paused`）；
   - 不满足 → 保留 `wanted/queued`，等事件，不轮询；
5. 成功 → `done` + 更新 track 行/队列/UI；
6. 失败 → `failed`、`attempts++`、指数退避（1m/5m/30m/2h/6h，20 次上限）；
7. 全部处理按 identity 串行，禁止并发同一 identity。

## 6. 入口：只 upsert

| 入口 | 行为 |
|---|---|
| 收藏 | `Upsert(identity, desired=promote, quality=设置)` |
| 单曲加入歌单 | 同上 + `playlist_id` 只影响成员，不进请求 |
| 批量加入歌单 | 对每个 remote 行 upsert（本地行跳过） |
| 搜索页保存 | 同上（显式保存：无缓存则下载） |
| Last.fm 推荐保存 | 同上 |
| 重新下载（显式） | upsert `desired=download` 且 `quality=指定`，若已 done 则重置为 wanted |

- 入口**不做**任何缓存/任务判断；
- upsert 后只唤醒 worker（事件），不阻塞 UI。

## 7. 派生：UI 只读请求

- 任务页：一行 = 一个请求（phase → 「解析中/下载中/提升中/已完成/失败」）；缓存留痕仍单独一类；
- 搜索行 chip：读取对应 identity 的请求状态（无请求且已在库 → 已在库中；有请求 → 已创建任务/失败）；
- Last.fm 行：本地命中且无请求 → 已在库中；有请求 → 任务状态；
- 事件：`media-request:updated` 携带整行快照，前端按 identity 替换。

## 8. 迁移计划（按顺序）

1. 迁移 `000103`：建 `media_requests` + `media_request_events`；
2. 新服务 `MediaRequestService`（upsert/list/get/cancel）+ `MediaRequestWorker`（状态机）；
3. **backfill**（启动时幂等执行一次）：
   - `stream_cache.pending_promote=1` → 请求 `desired=promote`；
   - `download_jobs.state in (queued,active)` → 请求 `desired=download`；
   - `promotion_tasks.state in (queued,running)` → 请求 `desired=promote`；
   - 已存在唯一键则合并；
4. 入口切换：所有保存/加歌单路径改为 upsert（加一个临时开关，新旧并行一版）；
5. 任务页与 chip 改为读请求；
6. 删除旧路径读写：`promotion_tasks`、三个缓存 flag、`maybeAutoDownload`/`OnRemoteTrackSaved` 的直接任务创建；
7. 清理逻辑（dedupe/孤儿/启动清扫）改为“只跳过有活动请求的 identity”。

## 9. 测试

**Go**

- 同一 identity 连续 upsert 10 次 → 1 行、1 条执行；
- 任意事件顺序（收藏→加歌单→播放结束→merge）→ 最终 `done`（收敛）；
- 有活动 session / 播放器持有时 → 不移动，等事件；
- 缓存完整 + 质量足够 → 提升；质量不足 → 下载并记录实际音质；
- 失败退避、不新建第二行；
- backfill 正确（旧 flags/任务映射，重复不重复插入）；
- 旧表删除后（阶段 6）所有测试通过。

**前端**

- 任务页一行一请求；chip 与任务页一致；
- 重复保存只出现一条任务。

**迁移验证**

- 用一份旧数据库副本启动，请求数与旧任务数对得上、无重复、无丢失。

## 10. 验收

- 同一首歌无论触发多少次，只会有一条请求、一个最终文件；
- 批量加歌单、收藏、搜索保存、Last.fm 保存行为一致；
- 任务页/搜索行/Last.fm 行状态来源统一；
- 旧表清理后无残留逻辑。

## 11. 风险与取舍

- 改动面大（数据库 + 服务 + 入口 + UI），但可灰度；
- 请求表会成为新的“热点写”：需要按 identity 串行与写入节流；
- 删除旧 flags 前必须完成 backfill 与灰度期；
- `download_jobs` 保留会让“一处状态两处写”，必须明确：**只有 worker 能创建/推进 job**。

## 12. 任务拆解（建议交给编程 AI 的顺序）

1. `000103` 迁移 + 领域模型/仓储 + 单元测试；
2. `MediaRequestService`（upsert/去重/状态 API）+ 测试；
3. `MediaRequestWorker`（状态机、事件触发、启动扫描、看门狗）+ 测试；
4. backfill 迁移逻辑 + 旧库验证测试；
5. 入口切换（收藏/单曲/批量/搜索保存/Last.fm 保存）+ 回归；
6. 任务页与 chip 改为请求派生；
7. 删除旧路径与旧表写入（`promotion_tasks`、缓存 flags、旧事件）；
8. 清理逻辑适配 + 全量回归 + 人工验收。
