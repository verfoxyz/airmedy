# 插件接口设计（未实现部分 + 已落地决策）

> 只记**还没做**的接口形状与决策，外加**已做完那批的决策记录**（防止重提）。
> 行为细节看 `catalog/plugins/README.md`：那份是现状的唯一真相，本文与它冲突时以它为准。
> 最后更新：2026-11-04

## 现状一句话

已实现：`stream.resolve`、`describe`（含 `searchSources` 与最小 `settings` 声明）、
`source.validate`、进程生命周期、provider 路由、`permissions.network`（**只声明不校验**）、
`storage.*` + `permissions.kv`（**校验并默认拒绝**）、`lifecycle.unload`
（`disabled` / `reload` / `shutdown`）、**`catalog.search`（在线目录搜索）**、
**`lyric.fetch`（源插件提供歌词）**。
宿主对**其余**入站请求仍回 `-32601`。

## 已否决（不再重提）

- **播放地址不内置解析**：`stream.resolve` 只由插件提供；内置 `wy` 解析已退役，
  未声明该 provider 的音源一律不可下载/播放，由宿主给出可读错误。
- **不提供 `pic.fetch` 路由**：封面随搜索结果返回 URL，由前端按需加载；插件不需要
  抓取图片字节。`lyric.fetch` 已改为由**拥有该 source 的搜索插件**提供，见下方实现记录。

## 待定（准备冻结，未实现）

| 接口 | 形状 | 关键约束 |
|---|---|---|
| `plugins.*` 插件间调用 | 需 manifest `exposes` 显式声明才可被调 | 未声明即不可见。**搜索功能没有用到它**：可搜源走 `describe` 上报，见下方「已实现」 |
| 提权确认 | 更新新增权限时必须重新确认 | 不能静默通过；当前 `kv` 从关到开依赖用户重装插件，没有单独的确认 UI |

## 已实现（保留决策记录，防止重提）

### `lyric.fetch`（源插件提供歌词）

插件在 manifest 的 `sources` 里声明它服务的平台，并声明 `capability: lyric.fetch`。
宿主按 `track.provider` 找到运行中的插件，调用
`{provider, external_id, title, artists[], album, duration}`，返回
`{lyric, tlyric, source, synced}`。**必须带 `[mm:ss]` 时间轴**，否则按失败处理；
失败或没有时间轴时回落内置 LRCLIB/酷狗。这样搜索到的日文/翻唱曲目不再依赖第二次
标题+歌手反查。

### `describe.settings`（最小设置声明）

插件可在 `describe` 返回 `settings: [{key, type:"switch", default, label}]`。宿主把开关
渲染在该插件行内，值存插件 KV 的 `host.setting.` 前缀下，并在受影响的调用里下发
（目前是 `catalog.search` 的 `include_cover`）。这不是完整的 `ui.settingsSchema`，
等后者落地再迁移；插件不声明时按默认值处理。

### `catalog.search`（在线目录搜索）

插件声明 `catalog.search` 能力后，宿主可按下标调用它搜**一个**源。可搜源由插件在
`describe` 的 `searchSources: [{id, name}]` 上报 —— 与 lx 上报路由同一套机制，
**零新增协议方法**（即 §6.1 的结论：不需要插件互调）。

编排全在宿主（`internal/app/catalog`）：每源一次并发调用、各有 10s 预算；失败源与结果
并列返回而不是让整页空白；名次融合 `Σ 1/(rank+3)` 只吃**行序**、不吃平台自报分数；
身份是 `(source, externalId)`，同源去重、跨源不合并；50/页、`hasMore` 由切片推导；
**可下载性**由解析器是否认领该 provider 决定，否则行内以 `no_resolver` 禁用（"可搜 ≠ 可下"）。

### `storage.*`（插件 → 宿主 KV）

`storage.get` / `set` / `delete` / `keys`。按 `plugin_id` 隔离；值 ≤ 64 KiB、键 ≤ 128 字节、
每插件 ≤ 256 个键；`value` 是插件自己的 JSON 字符串，宿主不解析。存储实现在
`app/pluginkv` + `plugin_kv`（migration 000082），**插件永远不直接碰数据库**。
键上限只拦**新键**：已经满额的插件仍可覆盖自己已有的键。

### `lifecycle.unload`（宿主 → 插件通知）

回收进程前发送，无 id 的通知，`reason: disabled\|reload\|shutdown`。它是**提示不是同步点**：
无论插件是否理会，进程都按 shutdown → EOF → terminate → kill 的同一套阶梯回收。插件可以借此
清定时器、取消在途请求、自行退出（`plugins/lx` 即如此）；不实现它的插件行为不变。

### `permissions` 运行时校验

`permissions.kv` 在每次 `storage.*` 调用时按**注册表中的实时 manifest**校验，缺省即拒绝（`-32003`）。
与 `permissions.network` 不同：网络是声明（宿主是普通子进程，看不到请求），KV 是宿主自己提供的
能力，所以有真正的闸门。见 `catalog/plugins/README.md`「Permissions: declared and enforced」。

`http.request`（宿主代理网络）**不做**，理由见 `catalog/plugins/README.md`
「Permissions: declared and enforced」：响应体接近 stdio 单行 8 MiB 上限就会打断会话，
代理比插件现在的直连更弱。真正的控制点需要另一条通道。

## 第三批（跟随需求，先不冻结）

- **右键菜单** `ui.registerContextMenu` → 宿主回调 `command.invoke` 带上下文。
  待定：子菜单/图标/快捷键；插件未启用时核心菜单项如何降级。
- **搜索页** `ui.pages[]` + `entry`。待定：宿主渲染方式（iframe / webview / 本地 HTTP）；
  页面调核心的桥接形式；结果曲目怎么表示（`TrackDTO` 要 `path`，远程曲目没有）。
- **流播放**：不新增插件接口，是核心改造。FFmpeg 已禁网络但**不影响边下边播**——
  `ma_ffmpeg_data_source` 自己实现 `read`/`seek`，FFmpeg 只解码。链路：
  下载器（Go，有网络）→ `.part` 文件 → FFmpeg 读文件 → miniaudio 输出。
  要改：播放器支持"读增长中的文件"、seek 限已下载范围、`availability=remote|downloading`。
  「只是听一下」的产品模型一并定在这里：未收藏、未加入自建歌单的曲目按**最低音质**下载并
  边下边播，落在**应用缓存**（用户可清理）；收藏或加入歌单才按设置音质正式入库。
  缓存与库是**两份文件，不互相升级**——入库时重新下载，旧缓存删除；缓存目录必须在
  数据目录下且**不进媒体库扫描范围**，否则会被自动导入，缓存形同虚设；正在播放的缓存
  文件不可被清理。
- **登录其他平台** `auth.openLogin` / `auth.callback`。凭据走现有 keyring，
  **不进插件 KV**——KV 只隔离、不加密，凭据放进去等于把密钥写在明文表里。
- **首页推荐**：形态已定——复用首页现有切换按钮追加第三个视图，不替换首页。
  无插件提供时按钮不渲染（不是渲染后禁用），插件停用则回落 `overview`。v1 只允许一个。
  > 放弃"替换首页"的原因：会让核心首页代码永远不可达。

## 核心能力（插件需要，但归属核心）

判断标准：**要窗口、要进程、要平台 API 的，都是核心。** 插件只能提供数据、响应命令。

| 能力 | 归属 | 备注 |
|---|---|---|
| 桌面歌词 | 核心 | 难点是鼠标穿透（Win `WS_EX_TRANSPARENT` / mac `ignoresMouseEvents`，Linux 明确降级）。估 6–8 天，穿透占 2–3 |
| 流播放代理与播放器改造 | 核心 | 插件只给 URL |
| 系统媒体控制 / 全局快捷键 / 托盘 | 核心 | 插件无法访问 |

## 明确不做（防止反复讨论）

| 能力 | 理由 |
|---|---|
| 插件自动更新 | 没有分发渠道，属于空转 |
| CPU/内存熔断 | 桌面应用插件数以个位计，不是服务器场景 |
| 多音轨 Provider | 小众需求，且要改播放器 |
| 音频 DSP 滤镜 | 涉及 cgo 音频管线，真实需求未出现 |
| 无缝切歌 `onPreload` | 依赖流播放先落地，那时才知道要不要 |

## 落地顺序

```
① ~~第一批接口 + 宿主实现~~（已落地：storage.* / lifecycle.unload / permissions.kv）
② ~~搜索页原型~~（已落地：`/search` 本地/云端切换、插件上报源、逐行状态、50/页）
③ 右键菜单 + 下载编排
④ 首页视图切换
⑤ 流播放（核心改造） —— 最大一块，最后做
⑥ 桌面歌词等平台能力 —— 非近期
```

**判断接口够不够的标准**：做第二个插件时是否需要改宿主。答"否"才算冻结成功。
