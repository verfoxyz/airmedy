# 任务：流播放与下载的音质降级链

> 状态：待开工
> 目的：请求的音质在某个源没有链接时，自动降级到下一个可用音质，而不是直接失败
> 关联：`fix-save-prefers-promotion.md`（音质优先级）、`stream-playback/spec.md`

---

## 1. 问题

- 用户设置 `stream_quality = flac`（或下载音质 flac）；
- 某些源对曲目没有 flac 链接（VIP/付费/无版权/未上传），`stream.resolve` 直接失败：

```
stream unavailable: plugin "lx": no lx source could resolve "kg"
  (音质 flac 无链接)
```

- 现在没有降级，整首歌无法播放/下载。

## 2. 规则

音质等级：`flac > 320k > 128k`。

- 按**配置音质开始、向下**逐个尝试：`flac → 320k → 128k`；
- 用户配置 320k 时只尝试 `320k → 128k`（不向上）；
- 用户配置 128k 时只尝试 `128k`；
- 第一个成功的音质即为结果；**记录实际音质**（缓存行/下载任务），不是请求音质；
- 全部失败才报错（错误信息包含尝试过的音质）；
- 降级只针对“**该音质无链接/无版权**”类错误；网络错误不降级（应由换源/重试处理）。

## 3. 实现

### 3.1 流播放

`internal/app/stream` 的解析路径（`streamQuality` → `plugin Resolve`）：

```go
func (s *Cache) resolveWithFallback(ctx, track, configured string) (ref, actualQuality, error) {
    for _, q := range qualityFallbacks(configured) {
        ref, err := s.resolver.Resolve(ctx, track, q)
        if err == nil { return ref, q, nil }
        if !isQualityUnavailable(err) { return ref, "", err }
    }
    return ..., fmt.Errorf("no playable quality among %v", tried)
}
```

- `qualityFallbacks` 生成降级序列；
- `isQualityUnavailable` 依据插件返回的错误（`kind`/错误码，不是文案）判断；
- 缓存里记录 `quality = actualQuality`（`stream_cache.quality`）。

### 3.2 下载

`download.Enqueue` / 任务执行时同样按降级链解析；任务的 `quality` 字段记录实际音质（可另加 `requested_quality`）。

### 3.3 跨源

- 单源内降级已在 3.1；
- 如果某源所有音质都失败，可换下一个源（现有匹配/换源机制）；
- 两者顺序：**先本源降级，再换源**（避免频繁换源）。

### 3.4 UI

- 降级发生时记 info 日志（`requested=flac actual=320k`）；
- 可选：播放器/任务页显示「已降级为 320k」，不弹窗；
- 设置页保持“偏好音质”，不改成实际值。

## 4. 测试

**Go**

- flac 无链接 → 自动尝试 320k 成功，缓存记录 320k；
- 320k 也失败 → 尝试 128k；
- 全部失败 → 返回错误且包含尝试列表；
- 配置 320k → 不会尝试 flac；
- 网络错误 → 不降级（直接返回）；
- 下载任务记录实际音质；
- 提升时按**实际音质**与下载音质比较（复用现有规则）。

**人工**

1. 设置 flac，播放报错的那首 kg 曲目 → 正常播放（320k）；
2. 缓存行/任务页显示实际音质；
3. 设置 320k → 不尝试 flac；
4. 无任何音质的曲目 → 仍然报错，提示清晰。

## 5. 验收

- 自动：§4 全绿；
- 人工：§4 四条成立；
- “音质无链接”类失败不再直接导致不可播放。

## 6. 风险

- 降级会多一次 `stream.resolve` 请求（最多 3 次），需按 identity 缓存结果减少重复；
- 插件返回的错误类型必须可区分“无该音质”和“网络/源不可用”，否则会错误降级；
- 实际音质与请求音质不一致时，提升/淘汰规则要使用实际值（缓存行已记录）。
