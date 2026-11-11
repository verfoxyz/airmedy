# 任务：设置 → 下载 →「删除重复缓存」

> 状态：待开工
> 位置：设置 → 下载
> 范围：只清理 `<DataDir>/stream-cache/` 的音频与 `stream_cache` 行；不碰 `cover-cache`、不碰永久音乐目录

---

## 1. 目标

一个「删除重复缓存」按钮：

1. 扫描 `stream-cache`，找出可删除的缓存；
2. 先给出报告（可删 N 条、约 X MB）；
3. 二次确认后清理；
4. 尽力而为：不确定的不删。

## 2. 可删除的三类

### 2.1 跨源重复

同一首歌在多个源各缓存一份。分组键（与跨源匹配一致）：

- `NormalizationKey(title)` + `NormalizationKey(artist)` 相等；
- 双方 `duration_ms > 0` 时 `|Δ| ≤ 5_000 ms`。

每组只留一份，删除其余。

### 2.2 已入库未删缓存

条目的 identity（`provider + external_id`）已经对应一个**非 remote** 的 track → 缓存不再需要，删除。

### 2.3 无效条目

- `complete = 0` 且 `received_bytes = 0`（可选再加 `os.Stat` 文件大小为 0）→ 删除；
- 典型来源：拉流/解析失败，只创建了空文件和未完成行；
- 跳过条件：有活动 session、`pending_promote=1`、`promoted=1`、`discard_pending=1`；
- 判断只用数据库字段（+ 元数据 stat），**不读文件内容**。

### 2.4 孤儿文件

`stream-cache/` 下没有对应 `stream_cache` 行的 `.audio` 文件 → 删除（扫目录与表对比）。

## 3. 保留优先级（满足用户要求：保留更高音质）

同一组里按以下顺序选“保留者”，其余可删：

1. **音质高**：`flac > 320k > 128k`（未知视为最低）；
2. 同级时：`complete=1` 优先；
3. 再同级：`last_used` 更新者优先；
4. 仍相同（无法区分）：**不删**（best-effort）。

## 4. 播放中的处理（保留播放队列）

用户要求：清理时暂停播放以解除文件占用，但**保留播放队列**。

流程：

1. 若待删除条目中有当前播放 session 使用的条目：
   - **暂停播放**（`player.Pause()`），不清空队列、不动 `currentTrack`；
   - 关闭该曲目的流 session（`Cache.Close`），等待文件句柄释放（最多 2 秒，轮询）；
   - 若仍无法删除 → 跳过该条，继续其它条目；
2. 删除其余条目；
3. 清理完成后**自动恢复播放**：同一曲目、同一位置；
   - 当前曲目的缓存若被删除但同组有保留者 → `Open` 走跨源匹配复用保留条目；
   - 组内没有保留者 → 重新拉流；
4. 队列、播放模式、进度均不变。

> 队列/当前曲目本来就存在播放器状态里，暂停/恢复不会影响它们，因此“保留播放队列”成立；唯一需要处理的是当前曲目可能引用了被删除的缓存，用跨源复用/重拉覆盖。

## 5. 跳过条件

- 条目有活动 session 且关闭后仍占用（见 §4）；
- `pending_promote=1`（还有提升任务）；
- 无法确定保留者（§3.4）；
- 读取/删除失败：记录并继续，不中断整批。

## 6. 后端 API

```go
// ScanDuplicateCache reports what a cleanup would remove; it never deletes.
ScanDuplicateCache(ctx) (DuplicateCacheReport, error)

// CleanDuplicateCache deletes the entries the report marked removable.
// It pauses playback when needed and resumes it afterwards.
CleanDuplicateCache(ctx) (DuplicateCacheReport, error)
```

```go
type DuplicateCacheReport struct {
    DuplicateGroups int      `json:"duplicate_groups"`
    Removable       int      `json:"removable"`      // 条目数
    RemovableBytes  int64    `json:"removable_bytes"`
    InvalidEntries  int      `json:"invalid_entries"`
    OrphanFiles     int      `json:"orphan_files"`
    PromotedLeftovers int    `json:"promoted_leftovers"`
    Skipped         []string `json:"skipped"`        // 跳过原因摘要
}
```

- 扫描与清理共用同一套判定，避免“报告说删、实际没删”；
- 清理逐条执行，单条失败不影响其它；
- 完成后 emit `task:updated` / 刷新缓存统计（若有）。

## 7. 前端

- `DownloadSettings.vue` 增加一行：标题「删除重复缓存」+ 说明 + 按钮；
- 点击 → 调 `ScanDuplicateCache` → `ConfirmDialog` 显示报告（「可删除 N 条，约 X MB」）；
- 确认 → `CleanDuplicateCache` → toast 结果（删除 N 条、释放 X MB、跳过 M 条）；
- 进行中按钮禁用（防重复点击）；
- 12 个 locale 新增 key（`settings.download.dedupe_*`）。

## 8. 测试

**Go**

- 跨源重复：同组保留最高音质；同音质保留完整/最近；无法区分时不删；
- 已入库条目被识别并删除；
- 无效条目（`complete=0` 且 `received_bytes=0`）被识别并删除；有活动 session 或 pending_promote 时跳过；
- 孤儿 `.audio` 被识别并删除；
- `pending_promote` 与活动 session 跳过；
- 当前播放条目：暂停 → 关闭 session → 删除 → 恢复播放（断言队列与进度不变）；
- 单条删除失败不影响其它；
- 扫描与清理结果一致。

**前端**

- 按钮 → 扫描 → 确认框（显示报告）→ 清理 → 结果 toast；
- 取消不删除；
- 请求中按钮禁用。

**人工**

1. 制造两源重复（同一首在 wy/kg 各缓存）→ 清理后只剩高质量那份；
2. 已提升曲目残留的缓存被清掉；
3. 播放中执行清理：自动暂停、清理、恢复播放，队列不变；
4. 清理后播放当前曲目仍正常。

## 9. 验收

- 自动：§8 全绿；
- 人工：§8 四条成立；
- 只影响 `stream-cache`，`cover-cache` 与音乐目录不受影响。

## 10. 风险

- 同名同艺人、时长接近的不同母带可能被误判为重复；已在 §3.4 用“无法区分不删”降低风险；
- 暂停/恢复会打断用户操作（可接受，清理是显式操作）；
- 跨源匹配的时长容差与现有实现保持一致，避免两套规则。
