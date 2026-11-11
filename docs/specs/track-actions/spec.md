# 任务：曲目右键新增「刷新此曲信息 / 导入本地封面」

> 状态：待开工
> 范围：右键菜单 + 后端服务；不改播放/缓存核心逻辑
> 前置：`context_menu.edit_metadata` 已存在（打开 `MetadataEditDialog`），本任务不重复实现

---

## 1. 目标

1. **刷新此曲信息**：按曲目自己的在线身份重新拉取封面与元数据（标题/艺人/专辑），失败保留原数据；
2. **导入本地歌词文件**：在「编辑元数据 → 歌词」里选一个 `.lrc/.txt` 读入歌词框，校对后保存；
3. 两个动作都走「用户显式触发」路径。

## 2. 现状

- 曲目行持久化 `provider` + `external_id`（下载/提升的曲目有；本地导入的为空）；
- 右键菜单已有：歌曲信息、刷新歌词、查找歌词、导出歌词、编辑元数据、重新下载；
- `internal/app/catalog` 已能按源搜索（`catalog.search`），`covercache` 能按 identity 取封面，`artwork.Save` 可把图片写入 artwork 缓存；
- Wails 文件对话框：`application.Get().Dialog.OpenFile().CanChooseFiles(true)...`（参考 `SelectFolder`）。

---

## 3. 刷新此曲信息

### 3.1 可用性

- `provider` 或 `external_id` 为空 → 菜单项**置灰**，tooltip 说明「该曲目没有在线来源信息」；
- remote 行与本地文件行都可用（remote 只更新数据库，本地行可选写标签）。

### 3.2 匹配（严格 + 容差）

1. 用 `provider` 对应的源搜索 `"<title> <primary artist>"`（limit 20，`include_cover=true`）；
2. 优先取 `external_id` 相同的结果；
3. 否则按下面规则找唯一候选：
   - `NormalizationKey(title)` 与 `NormalizationKey(artist[0])` **完全相等**；
   - 双方 `duration > 0` 时 `|Δ| ≤ 5s`（与 stream-cache 跨源匹配同一阈值）；
   - 多个候选时取时长差最小的；
4. 仍无命中 → 失败，不修改任何数据。

### 3.3 更新内容

- 封面：取命中行的 `cover_url_large || cover_url` → `covercache.Get` → `artwork.Save` → 更新 `tracks.artwork_key`；
- 元数据：标题、主艺人、专辑、时长（与当前值不同才写）；
- 本地文件：默认**只在数据库更新**；若用户勾选/确认「同时写入文件标签」，调用现有元数据写入路径（与 `MetadataEditDialog` 保存一致）；
- 完成后 emit `library:track-updated`，前端立即刷新。

### 3.4 失败与并发

- 同一曲目重复点击：按钮进入 loading，忽略重复调用；
- 搜索/下载封面失败：保留原数据并提示错误；
- 不自动换源（只查曲目自己的 `provider`）。

---

## 4. 导入本地歌词文件（编辑元数据 → 歌词）

> 「导入本地封面」已经存在（`MetadataEditDialog` 的 `selectedImage` 流程），本任务不重复实现。

### 4.1 交互

- 在 `MetadataEditDialog` 的「歌词」标签页，textarea 旁增加按钮「导入本地歌词文件」；
- 点击 → 文件对话框，过滤 `*.lrc;*.txt`，单选；
- 选中后把文件内容填入歌词框（**不自动保存**），用户校对后点原有的保存按钮；
- 取消选择：无副作用；
- 失败（过大/非文本/读不出）：提示错误，不清空已填内容。

### 4.2 后端

新增 `LyricsService.ImportLyricFile() (string, error)`（binding 自动生成）：

1. `application.Get().Dialog.OpenFile()`，`CanChooseFiles(true)`，`AddFilter("Lyrics", "*.lrc;*.txt")`，单选；取消返回空串、不报错；
2. 读取文件：大小 ≤ 1 MB；
3. 校验：去掉 UTF-8 BOM；拒绝包含 NUL 字节的内容（二进制）；可按 UTF-8 解码，失败提示；
4. 返回文本内容（保留 LRC 时间轴原样，不解析、不规范化）。

### 4.3 注意

- 不直接写数据库/标签：保存仍由 `MetadataEditDialog` 现有的保存路径完成；
- 不做时间轴校验（允许纯文本歌词，和现有输入框一致）；
- 不自动匹配/下载在线歌词（那是「刷新歌词」的职责）。

---

## 5. 前端

- 右键菜单只增加**一项**：`刷新此曲信息`（放在「刷新歌词」之后）；「导入本地歌词文件」的按钮在 `MetadataEditDialog` 的歌词标签页里，不进入右键菜单；
- 新 i18n key（12 个语言同改）：
  - `context_menu.refresh_track_info`
  - `context_menu.refresh_track_info_desc`（可选）
  - `library.import_lyric_file`
  - `library.import_lyric_failed`
  - `context_menu.refresh_no_identity`
  - 成功/失败提示沿用现有 toast 体系；
- 数据通过生成的 bindings 调用，禁止手写 JS 请求。

## 6. 测试

**Go**

- 匹配：同 identity 优先、标题+艺人一致 + 时长容差命中、超出容差不命中、多候选取最近；
- 刷新：无 identity 返回错误且不改数据；封面/元数据更新成功；搜索失败保留原数据；
- 导入歌词：大小 > 1 MB 拒绝、NUL 字节拒绝、BOM 去除、取消返回空串、正常文件返回原内容。

**前端**

- 菜单项可用性（有/无 identity、本地/remote 行）；
- 点击调用对应绑定、loading 与错误提示。

**人工**

1. 下载过的曲目右键 → 刷新此曲信息 → 封面/标题更新；
2. 本地导入的曲目 → 刷新置灰；
3. 右键 → 编辑元数据 → 歌词标签页 → 导入一个 .lrc → 内容进框 → 保存后歌词生效；
4. 导入 .txt、取消选择、超大文件分别验证。

## 7. 风险

- 搜索结果可能命中翻唱/同名版本：严格标题+艺人 + 5 秒容差已经收紧，仍建议失败时**不写**而不是猜；
- 写标签是不可逆操作，必须由用户显式确认（默认不写）；
- 歌词文件的大小/编码校验必须在后端做，不能只信前端过滤器。
