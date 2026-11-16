# 任务（宿主侧）：新增插件能力 `recommend.tracks`

> 状态：待开工
> 目的：让插件能提供“推荐曲目候选”（如 ListenBrainz），由宿主的发现流程消费
> 关联：`docs/specs/lastfm/discovery.md`（发现列表）、`docs/plugin-api.md`、`catalog/plugins/README.md`
> 配套插件文档：`docs/specs/plugins/plugin-listenbrainz.md`

---

## 1. 为什么需要宿主改动

现有插件能力只有 `catalog.search` / `lyric.fetch` / `stream.resolve`：

- 推荐不是“按关键词搜索”，无法复用 `catalog.search`；
- 让插件冒充搜索结果会污染语义与缓存。

因此新增一个只读能力 **`recommend.tracks`**：插件返回候选曲目元数据，宿主负责新颖性过滤、五源匹配、持久化和播放。

## 2. 协议

### 2.1 请求

```json
{
  "jsonrpc": "2.0",
  "id": 12,
  "method": "recommend.tracks",
  "params": {
    "limit": 50,
    "offset": 0,
    "locale": "zh",
    "seed": { "artist": "", "title": "" }
  }
}
```

- `limit`：1–100；
- `offset`：分页；
- `seed`：可选，空表示“按账号推荐”；
- 插件决定数据来源与内部缓存。

### 2.2 响应

```json
{
  "result": {
    "rows": [
      {
        "title": "Song",
        "artists": ["Artist"],
        "album": "Album",
        "duration": 215,
        "cover_url": "https://...",
        "recording_mbid": "…",
        "score": 9.34
      }
    ],
    "hasMore": true
  }
}
```

- `cover_url` 可为空；
- `score` 用于排序（可选，宿主缺失时按返回顺序）；
- 宿主**不要求** `provider/external_id`：匹配到哪个源由宿主决定。

### 2.3 错误

- 复用现有插件错误模型（`kind`: `unavailable` / `rate_limited` / `auth` / `invalid_params`）；
- `auth` 表示插件需要用户在插件设置里填 token。

## 3. 宿主实现

1. `domain/plugin.go` 增加 `MethodRecommendTracks = "recommend.tracks"` 与请求/响应类型；
2. 插件宿主（`internal/infra/pluginhost` / `internal/app/plugins`）：
   - 握手后记录支持该能力的插件；
   - 新增调用方法 `RecommendTracks(ctx, pluginID, params)`；
   - 沿用现有超时、错误映射与生命周期；
3. 多插件支持：多个插件可同时声明该能力，发现列表按插件设置顺序取前 N 条，失败只跳过该插件；
4. 发现流程接入：
   - `discovery` 候选来源增加“插件推荐”；
   - 先做新颖性过滤（本地库、最近 scrobble）；
   - 再走五源匹配，只保留可播放；
   - 结果写入现有的推荐列表持久化（kind = `discover`）；
5. 设置：
   - 插件列表里显示该能力与插件自定义设置（token 等由插件声明）；
   - 插件默认关闭，用户显式启用后才参与推荐。

## 4. 与 ListenBrainz 的边界

- 宿主**不知道** ListenBrainz 的存在；
- 任何“用户账号/token/API 细节”都在插件内；
- 宿主只消费标准 `recommend.tracks` 结果。

## 5. 测试

**Go**

- 能力声明解析：仅声明 `recommend.tracks` 的插件被识别；
- 调用成功/失败/超时/错误 kind 映射；
- 多插件合并、失败降级、顺序；
- 发现流程：插件候选 → 新颖性过滤 → 匹配 → 持久化；
- 插件未启用时不被调用。

**人工**

1. 安装并启用一个实现该能力的插件；
2. Last.fm「发现」里出现来自插件的候选（匹配成功可播放）；
3. 禁用插件后不再出现。

## 6. 验收

- 自动：§5 全绿；
- 人工：§6 三条成立。

## 7. 风险

- 新能力会扩大插件权限面：manifest 必须声明 `recommend.tracks`，设置页明确展示；
- 插件返回的数据可能不干净（缺失艺人/时长），宿主需按现有 `usable` 规则过滤；
- 多插件结果质量参差，排序需按插件优先级 + score。
