# Bug：删除本地文件后，再次保存/加歌单不会重新提升

> 状态：✅ 已完成（删除本地文件：`DeleteTrackFile` 经 `MediaRequestInvalidator` 把该 identity 的 `done` 请求重置为 `wanted` 并唤醒 worker，同时 `StreamCacheDiscarder` 清缓存；`Upsert` 遇 `done` 同意图时用 `HasLocalTrack` 校验，文件不存在则重开；worker 启动时对账所有 `done` 行。测试：`go test ./internal/app/mediarequest/ ./internal/app/library/ ./internal/infra/sqlite/`）
> 现象：应用内「删除文件」→ 重新播放 → 添加到歌单，但没有任何提升/下载，文件不会回来
> 关联：`media-request-state-machine.md`、`fix-bulk-playlist-add.md`、`fix-promoted-cover.md`

---

## 1. 复现与数据

1. 曲目已提升，本地文件存在，`media_requests.state = done`；
2. 应用内「删除文件」→ 本地行删除，文件删除；
3. 重新播放 → 生成新的 remote 行（正常）；
4. 添加到歌单 → `Upsert(identity, desired=promote)`；
5. 但 `media_requests` 仍是 `done` → 同意图 upsert 不重开 → **没有任务**。

数据表现：

- `tracks`：只剩 `remote://wy/28667025`；
- 本地文件不存在；
- `media_requests`：`state=done`（旧完成记录）；
- `stream_cache`：残留一条未完成缓存（11.2 MB / 31.4 MB）。

## 2. 根因

1. 删除文件只删了 `tracks` 行与文件，**没有让同一 identity 的请求失效**；
2. `Upsert` 的幂等规则：相同意图 + `done` → 不重开；
3. 因此后续保存/加歌单都变成空操作；
4. 提升成功后也没有清理该 identity 的缓存行。

## 3. 修法

### 3.1 删除文件时同步请求

- 「删除文件」成功后：
  - 该 identity 的 `media_requests`：`done` → `wanted`（保留历史，不删除）；
  - `next_attempt_at = now`，唤醒 worker；
  - 同时删除该 identity 的 `stream_cache` 行与文件（或标记待清理）。

### 3.2 worker 对账

- `ListActive`/执行前校验：`state == done` 但本地文件不存在（或行不是本地可播）→ 重置为 `wanted`；
- 校验频率：启动扫描一次 + 每次 upsert 时一次（轻量 `stat`）。

### 3.3 提升成功后的清理

- 提升完成后删除该 identity 的 `stream_cache` 行与缓存文件；
- 避免“文件已移动、缓存残留未完成”的脏数据。

### 3.4 `Upsert` 语义微调

- 相同意图 + `done`：仍需确认“本地文件存在”才视为完成；
- 否则按 `wanted` 处理，并记录一条事件（`reopened: local file missing`）。

## 4. 测试

**Go**

- 删除文件 → 请求从 `done` 变 `wanted`，worker 被唤醒；
- 删除文件后 `Upsert(promote)` → 重开并执行提升；
- `done` 但文件被外部删除 → 启动扫描重置；
- 提升成功 → 缓存行与文件被删除；
- 文件正常存在 → `Upsert` 仍幂等（不重开）。

**人工**

1. 提升 → 删除文件 → 重新播放 → 加歌单 → 出现提升任务并完成；
2. 提升 → 删除文件 → 直接点保存 → 同样能重新提升；
3. 提升后缓存目录无该曲残留；
4. 正常保存仍不会重复建任务。

## 5. 验收

- 自动：§4 全绿；
- 人工：§5 四条成立。

## 6. 风险

- “文件存在”校验要覆盖移动后的路径（重新扫描后路径可能变化），以 identity + 本地行状态为准；
- 重置为 `wanted` 后若用户已不再需要，可由后续取消/清理机制处理；
- 删除缓存要保证不误删正在播放的条目（检查播放占用）。
