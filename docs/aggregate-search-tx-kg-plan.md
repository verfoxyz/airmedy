# 计划：聚合搜索加入 tx / kg

> 状态：待开工
> 前置：`plugins/wy` 已能搜 wy，且行契约、封面、歌词链路都已通（见提交 `2d50855`）。
> 硬约束：AI 不做图形化测试；搜索质量由人工确认。

---

## 1. 目标

把一个源（wy）扩成三个源（wy + **tx** + **kg**）。插件仍是一个，显示名「聚合搜索」，
`describe.sources` 从 `["wy"]` 变成 `["wy","tx","kg"]`，设置页的源按钮自动多出两个。

**对外一律用 id**：`wy` / `tx` / `kg`，不写平台全名。展示名放在插件自己的 locale 里
（`wy → 网易云`、`tx → QQ音乐`、`kg → 酷狗`），宿主只渲染它给的字符串。

---

## 2. 每个源要做什么

三件事缺一不可：**搜索**（列表）、**封面**（缩略图 + 大图）、**歌词**（`lyric.fetch`）。

| 源 | 搜索接口 | 关键差异 | 估时 |
|---|---|---|---|
| wy | 已有（`cloudsearch/pc`） | — | 已完成 |
| **tx** | `c.y.qq.com/soso/fcgi-bin/search_for_qq_cp`（或 `smartbox_new`） | 需要 `Referer: y.qq.com`；字段是 `data.song.list[]`，时长 `interval`（秒）；封面在 `albummid` 拼 URL，**没有完整 URL**，要按 `https://y.gtimg.cn/music/photo_new/T002R500x500M000{albummid}.jpg` 拼 | 1–1.5 天 |
| **kg** | `mobilecdn.kugou.com/api/v3/search/song` 或 `complexsearch` | 需要签名与 UA 伪装；字段 `data.info[]`，时长 `duration`（秒）；封面 `album_id` + `imgurl` 拼 `https://imge.kugou.com/stdmusic/{size}/{hash}.jpg` | 1–1.5 天 |

每个源都要实现：

1. `search(query, page, limit, includeCover)` → 统一 row：`{ provider, external_id, title, artists[], album, duration, cover_url(100), cover_url_large(500) }`；
2. `lyric.fetch(provider, external_id, ...)` → `{ lyric, tlyric?, source, synced }`，**必须校验时间轴**；
3. 失败分类：`unavailable` / `rate_limited` / `auth`，与现有 wy 一致。

---

## 3. 宿主侧改动（很小）

| 点 | 现状 | 需要 |
|---|---|---|
| 扇出 | `catalog.Service` 已按 `describe.sources` 调用插件 | 不用改：插件自己返回多个源的 row，宿主按 `provider` 去重 |
| 可下载性 | 已有 `downloadable` + `blocked_reason` | 不用改：tx/kg 没有解析源时自动置灰，提示"需要安装支持 tx 的源" |
| 名称展示 | 行里显示 `provider` 原值 | 改为插件上报的 `source name`（`describe.sources[].name`），没有就回落 id |
| 设置页 | 源标签取 manifest `sources` | 自动生效，无需改 |

**唯一要确认的契约**：`describe.sources` 的每一项要带 `id` 与 `name`，宿主用它渲染按钮与行内"艺人 · 源"。

---

## 4. 工作分解

| # | 任务 | 估时 |
|---|---|---|
| A1 | 插件骨架支持多源：`sources` 表 + 按 provider 分发 search/lyric | 0.5 天 |
| A2 | tx 源：搜索、封面 URL 拼接、歌词 | 1–1.5 天 |
| A3 | kg 源：搜索、签名/UA、封面、歌词 | 1–1.5 天 |
| A4 | 宿主：行内显示源名（`describe.sources[].name`） | 0.5 天 |
| A5 | 测试：每源一个真实接口回归 + 契约测试（多源扇出、去重、失败隔离） | 0.5 天 |

**合计约 3.5–4.5 天。** 之后每加一个源（kw/mg）沿用同一模板，约 1–1.5 天。

---

## 5. 验收

**自动**

- 插件测试：三源各自映射正确（含时长单位、封面 URL 拼接）、单源失败不影响其它源；
- 宿主契约：`describe.sources` 三项都能渲染成按钮；同一首歌出现在两源时按 `(provider, external_id)` 去重；
- 歌词：三源都能返回带时间轴的歌词，翻译可缺省。

**人工（由人做，AI 不做）**

1. 云端搜索"周杰伦"：`全部` 下三个源各有结果，按钮为「全部 / wy / tx / kg」；
2. 行内显示"艺人 · 源名"（源名来自插件，不是 id）；
3. tx/kg 的行在没装解析源时置灰，且说明原因；
4. 装上解析源后能下载、能出封面与歌词。

---

## 6. 风险

| 风险 | 应对 |
|---|---|
| tx/kg 接口有签名/风控 | 每源独立实现，失败只影响自己；接口变更是常态，预留维护时间 |
| 封面 URL 靠拼接 | 拼接规则写进插件并加测试；拿不到就返回空，前端已回退占位图 |
| 一个插件崩了三个源全挂 | 已知取舍（见 `aggregate-search-plugin-plan.md` 早期讨论）；进程崩溃由宿主重启预算兜底 |
| 时长单位不统一 | 统一到秒：kg `duration` 已是秒、tx `interval` 是秒、wy `dt` 是毫秒 |
