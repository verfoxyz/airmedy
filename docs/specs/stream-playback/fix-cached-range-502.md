# Bug：已缓存范围也返回 502，拖动即无法播放

> 状态：✅ 已完成（`handleStream` 先判定 Range 是否完全落盘，命中则直接写文件返回 206/200，不调用 `waitReady`；未命中才等上游并保留 502；`CoversPosition` 让已缓存目标的 seek 直接原地 seek、不重建 session；日志提升为 Info/失败 Warn 并带 received/total）
> 现象：远端曲目只要拖动进度条（哪怕 5s→7s 都在缓存内）就无声；未缓存的歌同样
> 日志：`ma_player_load failed: -10`；代理侧静默（`Stream proxy response` 是 Debug 级）
> 关联：`fix-stream-seek.md`、`fix-seek-reload-failure.md`

---

## 1. 根因

`handleStream` 在任何请求上都先等上游握手：

```go
if !s.waitReady(r.Context(), 30*time.Second) {
    status = http.StatusBadGateway   // 502
    http.Error(w, "stream unavailable", status)
    return
}
```

- 拖动 → 重建 session → 新 session 的上游握手尚未完成（或解析失败/URL 过期）；
- 即使请求的字节**已经落盘**，也先被 `waitReady` 拦住；
- 超时后返回 502 → miniaudio `-10` → 无声；
- 因此“缓存里的部分也放不了”，与是否缓存无关。

## 2. 修法

### 2.1 已缓存范围立即返回

处理顺序改为：

1. 解析 Range；
2. 计算请求区间 `[start, end]`；
3. 若 `end <= received_bytes`（区间已下载）→ **直接写文件并返回**，不调用 `waitReady`；
4. 只有 `end > received_bytes` 时才等上游就绪（且只等到能覆盖该区间）；
5. 等待失败 → 502（保留现有诊断）。

### 2.2 seek 命中缓存时不重建 session

- `runRemoteSeek` 先检查目标位置对应字节是否已缓存：
  - 已缓存 → 直接本地 seek/重开解码器（不需要新上游握手）；
  - 未缓存 → 才等待/启动下载。
- 这样 5s→7s 这类拖动完全不触网。

### 2.3 诊断日志

- `Stream proxy response` 从 Debug 提升到 Info（失败时至少 Warn）；
- 记录：`range / status / content_range / written / first_byte_ms / elapsed_ms / received_bytes / total_bytes`；
- `waitReady` 超时记录 session 状态（是否有上游、已收字节）。

## 3. 测试

**Go**

- 请求区间完全在 `received_bytes` 内 → 不调用上游、返回 206，内容正确；
- 请求区间部分超出 → 等待上游后返回；
- 上游等待超时且区间未下载 → 502；
- 边界：`end == received_bytes` 视为已缓存；
- seek 到已缓存位置 → 不创建新 session / 不请求解析；
- 未缓存位置 → 走原有下载逻辑。

**人工**

1. 播放有部分缓存的远端曲目，5s→7s 拖动 → 正常出声；
2. 拖到缓存外位置 → 等待加载后播放；
3. 断网后拖动到已缓存位置 → 仍可播放；
4. 断网后拖到未缓存位置 → 提示失败。

## 4. 验收

- 自动：§3 全绿；
- 人工：§4 四条成立；
- 已缓存区间不再依赖上游可用性。

## 5. 风险

- 已缓存判定要用实际文件长度与 `received_bytes` 的较小值，避免 metadata 与文件不一致；
- 部分覆盖的区间仍要等上游，注意 Range 语义（206/416）；
- 日志级别提高可能增加日志量，只记录每次请求一行。
