# 任务（插件侧）：ListenBrainz 推荐插件

> 状态：待开工
> 插件 id：`listenbrainz`
> 能力：`recommend.tracks`（宿主需先实现该能力，见 `host-recommend-capability.md`）
> 目的：用 ListenBrainz 的协同过滤推荐补充「发现」列表

---

## 1. 数据来源（已核实）

ListenBrainz 公开推荐接口（实验性）：

```
GET https://api.listenbrainz.org/1/cf/recommendation/user/{user_name}/recording?count=50&offset=0
```

返回：

```json
{
  "payload": {
    "entity": "recording",
    "mbids": [
      { "recording_mbid": "526bd613-…", "score": 9.345 }
    ],
    "count": 10,
    "total_mbid_count": 30,
    "offset": 10,
    "user_name": "unclejohn69"
  }
}
```

- 读取接口**不需要 token**；用户反馈接口才需要；
- 返回的是 **MusicBrainz recording MBID**，没有标题/艺人 → 需要二次解析元数据；
- 302/204：该用户还没有生成推荐（返回空）。

## 2. 插件职责

1. 调用上面的接口拿到 MBID + score；
2. 把 MBID 解析成 `title / artists / album / duration / cover_url`；
3. 按 `recommend.tracks` 协议返回；
4. 内部缓存，控制请求量。

### 2.1 元数据解析

- 首选 ListenBrainz 的批量元数据接口（若可用），否则用 MusicBrainz：
  ```
  GET https://musicbrainz.org/ws/2/recording/{mbid}?inc=artist-credits+releases&fmt=json
  ```
- MusicBrainz 限制：**1 请求/秒**，必须带 `User-Agent: Airmedy/<version> (contact)`；
- 每次推荐最多解析 **20 条**，其余按需分页；
- 解析失败的 MBID 跳过，不阻塞其它；
- `cover_url` 可留空（宿主会走源封面）。

## 3. 设置（插件自定义）

| key | 类型 | 默认 | 说明 |
|---|---|---|---|
| `user_name` | text | 空 | ListenBrainz 用户名（必填） |
| `count` | number | 30 | 每次拉取条数，最大 100 |
| `resolve_metadata` | switch | true | 关闭时只返回 MBID（宿主无法匹配，不建议） |

- 未填 `user_name` 时返回 `auth` 类错误，设置页提示“请填写 ListenBrainz 用户名”；
- token 本期不需要（只读推荐）。

## 4. 缓存与限流

- 推荐结果缓存 **30 分钟**（key = user_name + count + offset）；
- MBID 元数据缓存 **7 天**（MusicBrainz 数据变化慢）；
- 全局请求串行：MusicBrainz 1 req/s，ListenBrainz 自建间隔 ≥500ms；
- 单次 `recommend.tracks` 调用总耗时上限 15s，超出返回已解析的部分（`hasMore=true`）。

## 5. manifest 示例

```json
{
  "id": "listenbrainz",
  "name": "ListenBrainz 推荐",
  "version": "0.1.0",
  "apiVersion": "^1.0",
  "runtime": { "type": "process", "command": "node", "args": ["wrapper.js"] },
  "capabilities": ["recommend.tracks"],
  "permissions": {
    "network": ["api.listenbrainz.org", "musicbrainz.org"]
  },
  "settings": [ … ]
}
```

## 6. 实现要点

- 与现有插件同一套 NDJSON JSON-RPC 循环：
  - `initialize` / `describe` / `ping` / `lifecycle.unload`；
  - 新增 `recommend.tracks` 处理；
- `describe` 返回能力与设置描述；
- 关闭时停止在途请求（复用现有 `lifecycle.unload` 语义）；
- 错误分类：用户不存在/无推荐 → `unavailable`（宿主跳过），限流 → `rate_limited`，未配置 → `auth`。

## 7. 测试

**插件单元测试（node --test）**

- 组装推荐请求 URL 与分页参数；
- 解析 `mbids`（含空数组/204/404）；
- MusicBrainz 解析成功/失败/限流分支；
- 缓存命中不重复请求；
- 用户名为空 → `auth`；
- 部分 MBID 解析失败仍返回其余行。

**联调（宿主）**

- 启用插件、填写用户名 → 发现列表出现候选；
- 未填写 → 插件设置提示，宿主不报错；
- 禁用插件 → 候选消失。

## 8. 验收

- 插件单测全绿；
- 联调后「发现」里出现 ListenBrainz 来源的、可匹配到源并播放的曲目。

## 9. 风险

- ListenBrainz 推荐是实验性接口，可能变更；
- 冷门账号可能没有推荐（204）→ 宿主显示为空即可；
- MusicBrainz 1 req/s 限制使“边拉边解析”较慢，必须缓存与限量；
- 只读推荐不需要 token，但用户可能误以为要登录；设置页需写明。
