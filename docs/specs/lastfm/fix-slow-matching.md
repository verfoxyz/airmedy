# Bug：兜底后仍然没有推荐歌曲（改为先出列表、匹配慢慢补）

> 状态：🗑️ 已废弃（单请求 + 五源队列（single-request-top-tracks.md）已取代）
> 现象：Last.fm 页能显示相似艺人，但四份推荐仍然为空
> 设计方案：**先返回 Last.fm 的歌曲列表，源匹配在后台渐进补全，通过事件把结果推给页面**
> 关联：[recommendations.md](recommendations.md)、[fix-empty-recommendations.md](fix-empty-recommendations.md)

---

## 1. 网络验证结论（先排除网络）

用应用同一套 Go `http.Get` 直连 Last.fm，本机无代理环境变量（`HTTP_PROXY` 为空），6 个接口全部 200：

`user.getRecentTracks`、`user.getTopTracks`、`track.getSimilar`、`artist.getSimilar`、`artist.getTopTracks`、`chart.getTopTracks`。

**Last.fm 直连可用；推荐为空不是网络/代理问题。**

## 2. 根因

插件搜索实测：

| 源 | 单次搜索耗时（冷启动） |
|---|---|
| `kg` | ~2–3s |
| `kw` | **~8–10s** |

当前实现把“拿推荐”和“匹配源”放在同一个请求里同步完成：

- 每个候选对 2 个源各搜一次（≈10–13s）；
- 总预算 25s、并发 2 → 只能完成约 4 个候选；
- 这四个没严格命中，页面就一条歌都没有。

## 3. 新方案：先出列表，匹配后台补

### 3.1 两段式返回

`GetRecommendations` 不再等待全量匹配：

1. **立即返回** Last.fm 侧的结果（四份列表 + 相似艺人），字段为：
   - `Title` / `Artists` / `AlbumName` / `Duration` / `Score` / `Source`；
   - `Provider` / `ExternalID` / `State` 等**暂时为空**，并标记 `Matching: true`；
2. 同时启动后台匹配任务（single-flight，按 username + 列表缓存键去重）；
3. 命中一条就通过事件推给页面，页面原地更新（播放/保存按钮随之出现）。

### 3.2 后台匹配

- 只对**当前返回的列表**（四份合计去重后，上限 60）匹配；
- 每候选**一次只打一个源**，失败再换下一个（最多 2 个），按快速源优先：
  `wy → kg → tx → kw → mg`；
- 并发 4，单候选 15s 超时，整体无总时限（可以慢慢跑完）；
- 结果写入现有匹配缓存（30 分钟），并保存“每个候选是否已匹配/失败”，避免重复打源；
- 艺人匹配放宽为：`row.Artists` 中**任意一个**归一化后与目标艺人相等（标题仍严格，时长 ±5s）。

### 3.3 事件

新增 Wails 事件：

```go
// lastfm:recommendations-matched
type RecommendationMatchUpdate struct {
    Username string              `json:"username"`
    Items    []RecommendedTrack  `json:"items"`   // 已命中、带 provider/external_id 的行
    Done     bool                `json:"done"`    // 后台匹配结束（可能仍有未命中）
}
```

- 每命中 1–3 条或每 500ms 合并推送一次，避免事件风暴；
- `Done=true` 后页面不再等待；
- 事件只发给当前页面会话（无会话时丢弃，不缓存事件）。

### 3.4 前端

`LastFmView.vue`：

- 首次加载：列表立即渲染；不可播放的行显示「匹配中…」灰色状态，不显示播放/保存按钮；
- 监听 `lastfm:recommendations-matched`：按 `artist + title` 原地替换对应行，状态从「匹配中」变为可播放/已入库；
- `Done=true` 后，仍未命中的行显示「未找到可播放源」（可选隐藏）；
- 刷新按钮：重新请求（可带 `force`），列表回到匹配中状态；
- 相似艺人不受影响。

### 3.5 空状态

- Last.fm 侧一条歌都没有 → 显示「Last.fm 数据不足」（现有的兜底链已尽量保证有数据）；
- 有歌但全部匹配失败 → 列表可见但标「未找到可播放源」，并提示源不可用。

## 4. 测试

**Go**

- `GetRecommendations` 在匹配未完成时返回非空列表（`Matching=true`，provider 为空）；
- 后台匹配命中后 emit 事件，`Items` 带 provider/external_id；
- 事件按 500ms 合并；`Done` 在全部候选处理完后发出；
- 每候选单源优先、失败回退；
- 匹配结果写入缓存，第二次打开不重复请求；
- 艺人匹配“任意艺人”规则；
- 匹配失败/源不可用时 `Done=true` 且不报错。

**前端**

- 列表先渲染，未匹配行显示「匹配中」且不可播放；
- 收到事件后原地更新，按钮出现；
- `Done` 后未命中行显示「未找到可播放源」；
- 刷新重置状态；事件监听在 `dispose()` 释放。

**人工**

1. 打开 Last.fm 页 → 歌曲列表**立即**出现（灰色不可播）；
2. 1–5 秒内陆续变为可播放；
3. 播放/保存正常；
4. 全部结束后没有一直「匹配中」的行。

## 5. 验收

- 自动：§4 全绿；
- 人工：§5 四条成立；
- 页面不再因为匹配慢而显示空列表。

## 6. 风险

- 事件推送与页面刷新竞争：用 `artist+title` 作为稳定 key；
- 页面关闭后后台仍会匹配一段（有缓存，可接受）；应在无订阅者时继续但降低优先级；
- 未匹配行不可播放，用户可能疑惑；用「匹配中/未找到」明确状态；
- 请求量增加（每个候选 1–2 次源搜索），靠并发 4 与缓存控制，勿再叠加限速冲突。
