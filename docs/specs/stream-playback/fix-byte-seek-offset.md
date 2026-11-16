# Bug：字节 seek 落点错误（拖到 10s 实际跳到 ~1s，随后切歌）

> 状态：⛔ 已被 `seek-metadata-duration.md` 取代（字节 seek 分支与回退状态已全部删除；远端 seek 改为补数据 + 重开解码器 + 时间戳 seek）
> 现象：从 5s 可以拖回 0s；拖到 10s 却直接切下一首
> 关联：`fix-prefetch-seek-deadlock.md`（字节 seek 实现）、`cache-file-decoder.md`

---

## 1. 日志证据

```
seek to=8.888   after 1s: cursor=0.97  file_pos=262144  file_size=29808996 expected_size=29808996
seek to=12.402  after 1s: cursor=0.98  file_pos=262144
seek to=10.955  after 1s: cursor=0.97  file_pos=262144
```

- 目标 8.9 / 12.4 / 11.0 秒；
- 实际读位置恒为 **262144 字节（256 KiB）**，光标回到 **~1 秒**；
- 即使文件已完成（`file_size == expected_size`）也走了字节 seek；
- 落点错误后播放很快到达末尾 → 触发“播放结束 → 下一首”。

## 2. 根因

1. 字节定位公式 `offset = seconds / duration * total` 的入参有问题（`duration` 或 `total` 为 0 时退化）；
2. 钳制逻辑错误：结果被压到固定的 256 KiB 附近，而不是“目标偏移”；
3. “未完成文件才用字节 seek”的判断没有生效，完成文件也走了该路径；
4. 落点错误没有校验，直接继续播放。

## 3. 修法

### 3.1 入口条件

只有同时满足以下条件才走字节 seek：

- cache 模式（文件未完成）；
- `duration > 0 && total > 0`；
- `received > 0`。

否则回退 `avformat_seek_file`。

### 3.2 偏移计算与钳制

```c
offset = (int64_t)(seconds / duration * total);
if (offset < 0) offset = 0;
int64_t limit = received - 256*1024;      // 已写入边界留余量
if (limit < 0) limit = 0;
if (offset > limit) offset = limit;
if (offset > total) offset = total;       // 双保险
```

- **`received` 是“已写入字节”**，不是 `total`、不是“文件大小 - 常量”；
- 修复后 `file_pos` 应随目标线性变化（10s ≈ 1.3MB 量级，而不是 256 KiB）。

### 3.3 seek 后校验

- 记录 `byte_seek requested time, offset, clamped, received, total, duration`；
- 若 1 秒内 `cursor` 与目标偏差 > 2s，视为失败，回退时间戳 seek（并标记该容器不做字节 seek）。

### 3.4 顺序解码兜底

- 若格式随机访问不可靠（无索引），从钳制偏移顺序解码到目标；
- 失败则重开解码器并走时间戳路径。

## 4. 测试

**Go / C**

- 完成文件 seek 10s → 走 `avformat_seek_file`，落点误差 < 1s；
- 未完成文件 seek 10s（received 足够）→ 字节偏移 ≈ `10/duration*total`，误差 < 2s；
- 未完成文件 seek 到 received 之外 → 钳到 `received - 256KiB`，随后通过 demand 继续下载；
- `duration` 或 `total` 为 0 → 回退时间戳 seek，不崩溃；
- seek 后 cursor 偏差 > 2s → 回退并记录；
- 回归：0s、5s、缓存外 seek。

**人工（一次验证）**

1. 5s → 10s：立即播放，位置正确（不切歌）；
2. 5s → 0s：从 0 播放；
3. 拖到缓存外：缓冲后继续，不切歌；
4. 4 次拖动后仍正常播放。

## 5. 验收

- 拖动落点误差 < 2s；
- 不再出现“拖动后切下一首”；
- 完成文件与未完成文件行为一致（都落点正确）。

## 6. 风险

- 无索引容器的字节定位天然有误差，需要顺序解码校正；
- 校验失败回退会多一次 seek，注意不要形成循环（每个会话最多回退一次）。
