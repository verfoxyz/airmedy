# 计划：封面与歌词的获取（v1）

> 状态：待开工
> 参考实现：`reference/lx-music-desktop`（只读参考，不引入依赖）
> 硬约束：**AI 不做图形化测试**；界面与播放由人工确认。

---

## 0. 一句话

**封面：能拿到的 URL 一个都别丢，拿不到就用专辑/内嵌/同目录图兜底。**
**歌词：能拿到的时间轴必须是真的，拿不到就明确失败，绝不留空行。**

---

## 1. 现状

### 封面

| 环节 | 现状 | 问题 |
|---|---|---|
| 下载时嵌入 | 搜索结果的 `cover_url` → 入队 → finalize 写进标签（FLAC `METADATA_BLOCK_PICTURE`、MP3 APIC） | 已通；但`cover_url`缺失时没有任何兜底 |
| 列表展示 | 读 `tracks.artwork_key` → 封面缓存 | 没有就空着，**不回退专辑封面、也没有占位图** |
| 本地导入 | 只从音频标签里取内嵌图 | 不支持同目录 `cover.jpg` / `folder.jpg` |
| 缺失补齐 | 无 | 已入库但没有封面的曲目不会自动修复 |

### 歌词

| 环节 | 现状 | 问题 |
|---|---|---|
| 来源 | 内置 LRCLIB + 酷狗（`internal/infra/lyrics`）+ 本地 `.lrc` | 酷狗用的是 `krcs.kugou.com`，**没有 KG-RC/KG-THash 头、没有专用 UA**，与 `reference/lx-music-desktop/src/renderer/utils/musicSdk/kg/lyric.js` 的可用路径不一致 |
| 校验 | 无 | provider 返回空字符串也照存 → 库里 1762 条空歌词行 |
| 失败处理 | 只在最外层记一条 `lastErr` | 分不清是哪家失败、为什么；LRCLIB 503 时没有退避重试 |
| 兜底 | 无 | 一家失败就结束，不去别的源按"歌名+歌手+时长"找同一首 |
| 缓存 | 有（`lyrics` 表） | 存取都缺"必须含时间轴"的校验 |

---

## 2. 参考实现给出的四件事（`reference/lx-music-desktop`）

1. **时间轴校验**：`existTimeExp = /\[\d{1,2}:.*\d{1,4}\]/`，没有时间轴的歌词视为失败（`core/music/utils.ts:18`）。
2. **每平台一套请求，带自己的头和重试**：酷狗用 `lyrics.kugou.com/search?client=pc` + `KG-RC`/`KG-THash` + `KuGou2012-9020-ExpandSearchManager`，非 200 重试最多 5 次（`musicSdk/kg/lyric.js`）；网易云走 eapi 加密（成本高，本轮不做）。
3. **跨源兜底**：当前源失败后，用歌名+歌手+时长到其它源搜同一首再取词（`core/music/utils.ts:420`）。
4. **封面按平台取**：网易云直接 `al.picUrl`，酷狗走资源接口；本地文件优先内嵌图（`musicSdk/*/pic.js`）。

---

## 3. 决策

| # | 决策 | 理由 |
|---|---|---|
| D1 | 歌词仍由**核心**提供（内置 provider + 本地 `.lrc`），暂不做 `lyric.fetch` 插件能力 | 公开接口本身可用（LRCLIB 带 UA 返 200、酷狗返候选），问题在实现细节；插件化留到公共源确不够时再开 |
| D2 | 歌词**必须含时间轴**才算成功，空内容一律不落库 | 直接消掉 1762 条假歌词与"有歌词但不滚动"的误判 |
| D3 | 酷狗客户端按 lx 的端点与请求头重写，并加重试 | 现路径是猜的，lx 的路径经过长期验证 |
| D4 | 封面三级兜底：搜索 `cover_url` → 专辑封面 → 同目录图 → 占位图 | 用户看到的永远是"有封面"或"明确的占位" |
| D5 | 已入库但缺封面/歌词的曲目，提供**手动补齐**入口（右键"补齐封面/歌词"） | 历史数据无法自动回填，至少给用户一个动作 |
| D6 | 不做网易云 eapi 加密歌词（需要移植 eapi + cookie 状态） | 成本高、易失效；等酷狗 + LRCLIB 修好后看覆盖率再定 |

---

## 4. 工作分解

| # | 任务 | 估时 | 完成标准 |
|---|---|---|---|
| L1 | 时间轴校验 + 空内容不落库 + 清理历史空行（一次迁移） | 0.5 天 | 空歌词不再入库；DB 里 `content=''` 的行为 0；读取时把空行当无歌词 |
| L2 | 酷狗客户端重写：`lyrics.kugou.com/search`（client=pc）+ `KG-RC`/`KG-THash` 头 + 专用 UA + lrc/krc 两种格式 | 0.5 天 | 用一首有词的歌验证能拿到 LRC；失败时有明确日志 |
| L3 | 重试与退避（429/503），每个 provider 独立记录成功/失败/耗时 | 0.5 天 | LRCLIB 503 时自动退避重试，日志能看出是哪家、第几次 |
| L4 | 跨源兜底：一级源失败后，按 歌名+歌手+时长 到另一源再取一次 | 1 天 | 只开一家源也能出词的场景有测试覆盖 |
| F1 | 封面三级兜底（列表侧）：`artwork_key` → 专辑封面 → 占位图 | 0.5 天 | 缺封面的曲目显示专辑图或占位，不再空白 |
| F2 | 本地导入支持同目录 `cover.jpg`/`folder.jpg` | 0.5 天 | 无内嵌图但有目录图时能提取并入库 |
| F3 | 下载时 `cover_url` 缺失的兜底：用专辑图或跳过并记录 | 0.5 天 | 无 URL 时不再静默无封面 |
| T1 | 测试与文档：provider 单测（含空/无时间轴/503）、封面兜底单测、`catalog/lyrics` 与 `catalog/artwork` 同步 | 0.5 天 | 全部通过 |

**合计约 4 天。**

---

## 5. 验收

**自动**

- `go test ./...`、`golangci-lint run`、`gofmt -l` 干净
- 新增用例：无时间轴歌词被拒；空内容不入库；429/503 重试；跨源兜底；封面三级兜底
- `select count(*) from lyrics where content=''` 为 0

**人工（由人做，AI 不做）**

1. 下载三首不同平台的歌：都有封面、都有歌词、歌词能滚动
2. 断开一家源（设置里只留一家）仍能出词
3. 找一首本来没封面的本地曲目：列表中显示专辑图或占位图，不空白
4. 右键"补齐歌词"后能出词

**验证命令（只读）**

```bash
ffprobe -v error -show_entries format_tags -of default=nw=1 <file>   # 封面/标签
sqlite3 -readonly "%LOCALAPPDATA%/airmedy/airmedy.db" \
  "select count(*) from lyrics where content='';"
sqlite3 -readonly "%LOCALAPPDATA%/airmedy/airmedy.db" \
  "select track_id, source, length(content) from lyrics order by updated_at desc limit 5;"
```

---

## 6. 风险

| 风险 | 应对 |
|---|---|
| 酷狗/网易云改接口或加风控 | 每平台一实现、失败只影响自身；跨源兜底降级 |
| LRCLIB 限流（实测：无 UA 503、带 UA 200） | 固定 UA + 退避重试 + 单源失败不影响另一家 |
| 空歌词行清理误删真实数据 | 迁移只删 `content=''`，删除前有备份（SQLite 文件级） |
| 封面外链失效 | 下载时立刻取图写盘/入库，长期不依赖外链 |

---

## 7. 本轮不做

- `lyric.fetch` 插件能力与第三方歌词源
- 网易云 eapi 加密歌词、QQ 音乐 qrc 解码
- 逐字歌词（lx 的 `lxlyric`）渲染
- 在线按需补封面（Deezer/Last.fm 已有艺术家图，曲目封面留到下一轮评估）
