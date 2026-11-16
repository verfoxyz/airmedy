# 任务：流缓存改为有界预取（只拉播放位置前方一段，不浪费流量）

> 状态：✅ 已完成（`prefetchAheadSeconds=45` 水位、进度唤醒、解码器 demand 抬升、`EnsureComplete` 解除限流；测试覆盖见 cache_test.go）
> 后续：解码器读到写入边界时由 demand 规则继续下载，见 `fix-prefetch-seek-deadlock.md`
> 目的：不再一次性下载整首；只预取到「当前播放位置 + N 秒」，播放推进后再继续；已拉取数据全部保留
> 关联：`task-prebuffer.md`、`quality-fallback.md`、`media-request-state-machine.md`
> 注意：提升流程已改为 `media_requests` 状态机（见 media-request-state-machine.md），本任务的“解除限流”由请求 worker 触发

---

## 1. 目标

- 播放开始：仍然先等启动预缓冲（1 MiB / 3s），保证不卡；
- 之后下载器**只拉取到水位**：

```
水位 = 当前播放位置 + prefetchAheadSeconds（默认 45s）
```

- 到达水位 → 暂停下载（不占带宽）；
- 播放继续前进、水位前移 → 自动恢复下载；
- **解码器需求优先**：播放器每 500ms 上报解码器当前文件偏移（`SetDecodeOffset`），
  偏移距写入边界 <2 MiB 时抬水位持续下载，>4 MiB 后恢复水位（`fix-prefetch-seek-deadlock.md`）；
- 已下载的字节**全部保留**（支持向前缓存、向后 seek）；
- 用户保存/收藏（有提升请求）→ **立即解除限流**，把整首下载完（提升需要完整文件）。

## 2. 不改的部分

- Range 代理、`bytes=N-` 语义、阻塞读逻辑不变；
- 缓存文件格式/元数据不变；
- 淘汰规则不变。

## 3. 实现

### 3.1 水位计算

下载循环（`fetch`）每写入一块后检查：

```go
aheadBytes := s.received - positionBytes(s.progress)
limitBytes := s.receivedLimitAt(s.progress)  // 见下
if !s.promoteRequested && aheadBytes >= limitBytes {
    s.waitForProgress()   // 阻塞直到 progress 变化、promote 或取消
}
```

- `positionBytes(pos)`：
  - 已知 `total_bytes` 与 `duration` → `pos / duration * total_bytes`；
  - 未知 duration → 用码率估算：`bytesPerSecond`（quality 推导：flac≈150KB/s、320k≈40KB/s、128k≈16KB/s），或固定 10 MiB 兜底；
- `limitBytes = prefetchAheadSeconds * bytesPerSecond`；
- 水位按字节算，避免每秒频繁换算。

### 3.2 进度通知

- 播放器已有 500ms 进度上报（`ReportProgress`）；
- 在 session 里新增 `progressChanged chan struct{}`（容量 1），`ReportProgress` 变化时非阻塞唤醒；
- `waitForProgress` 用 select 等：progress 通道 / 取消 / 1s 超时（超时重算，防止位置长时间不动卡死）。

### 3.3 提升/下载请求

重构后“提升”由 `media_requests` 驱动，不再有 `pending_promote` 标记或旧提升 worker：

- session 增加 `completeRequested` 标记；
- 请求 worker 决定要完整文件时（提升前补全、下载），调用 `EnsureComplete`，同时置该标记并唤醒下载；
- 仅当该 identity 有健康的播放会话时才走“补全缓存”，否则回落到普通下载队列（失败有退避与错误上报）；
- 标记存在时忽略水位，下载到 EOF 并完成；
- 请求结束（session 关闭）后标记清空。

### 3.4 结束条件

- 播放位置 + 水位 ≥ 结尾 → 下载剩余部分并完成；
- 用户 seek 到接近结尾（位置跳变）→ 立即把水位推到头；
- 用户停止/切歌 → session 关闭，已下载部分保留，下次续传。

## 4. 配置

- 常量 `prefetchAheadSeconds = 45`（可后续做成设置项「预取时长」）；
- `bytesPerSecond` 推导函数；
- 若 quality 未知，按 320k 估算。

## 5. 测试

**Go**

- 进度停在 0：下载最多到水位（45s 对应字节）、之后不再增长；
- 推进进度到 30s：恢复下载到新水位，且**已下载字节不减少**；
- 向后 seek：不触发新下载；
- 向前 seek 超过水位：立即恢复下载；
- 设置 complete 请求（模拟 `EnsureComplete`）：忽略水位，下载到 EOF 并写 `complete=1`；
- duration 未知：按码率估算水位生效；
- 取消/关闭：下载协程退出，文件保留，行 metadata 正确。

**人工**

1. 播放一首未缓存的歌，观察 `stream-cache` 文件只增长到约“播放位置 + 45s”；
2. 暂停播放：文件不再增长；
3. 继续播放：文件继续增长；
4. 拖动到未缓存位置：立即恢复下载；
5. 保存/收藏：文件被完整下载并提升。

## 6. 验收

- 自动：§5 全绿；
- 人工：§5 五条成立；
- 正常播放时缓存文件不再一次性达到整首大小。

## 7. 风险

- 码率估算不准会提前/延后停；下限保护（至少预取 30s）避免卡顿；
- 极慢网络下 45s 水位可能不够，保留现有启动预缓冲与卡顿重试；
- 播放器进度上报 500ms，下载暂停有最多半秒延迟（可接受）；
- 提升/下载完成路径必须无视水位，否则会有“永远提不完”。
