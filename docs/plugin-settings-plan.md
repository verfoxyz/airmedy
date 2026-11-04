# 计划：插件扩展设置页（L1 自有设置 / L2 注入核心分组 / L3 声明式子页面）

> 状态：待开工
> 硬约束：**AI 不做图形化测试**；界面由人工确认。
> 相关：`internal/domain/plugin.go` 的 `CapabilityUISettingsSchema`（能力名早已预留，从未实现）。

---

## 0. 一句话

**插件给结构，宿主画界面、存值、校验。** 插件永远不碰 DOM、不写核心设置表。

---

## 1. 现状

| 项 | 现状 |
|---|---|
| 设置页 | `SettingsView.vue` 用 `activeCategory` 切分 9 个硬编码分类（general / library / integrations / playback / download / plugins / remote / mobile-devices / about） |
| 设置值 | 核心设置是强类型结构体（`domain.AppSettings`），走 `appsettings` 服务与 SQLite |
| 插件设置 | 只有插件级开关（启用/停用）；没有"插件自己的设置项" |
| 能力 | `ui.settingsSchema` 已定义，未实现 |

---

## 2. 三档能力（对应三种需求）

### L1 插件自有设置区

插件声明自己的设置分组，宿主在「设置 → 插件」下按插件列出。
值存插件 KV（`permissions.kv`，默认拒绝、按插件隔离）。

适用：插件自己的偏好（优先源、是否走代理、缓存上限…）。

### L2 注入核心分组

插件往核心已有分组里插一行，例如「设置 → 下载」里加"下载完成后自动导出歌词"。

**前提**：核心分组必须暴露稳定**锚点**，例如：

```
download.after_quality
download.before_directory
playback.end
general.appearance.end
```

没有锚点就只能"随便插"，顺序不可控、布局会乱，所以锚点是硬要求。

### L3 声明式子页面

插件申请一个新的设置子页（左侧分类多一项，例如「音频可视化」），页面结构仍是声明式：

- 页面 = 标题 + 若干分组 + 若干行（开关 / 下拉 / 数字 / 文本 / 按钮）；
- 宿主渲染，插件拿不到 DOM；
- **实时预览类 UI 不做**：可视化预览属于播放器界面，设置页只放参数。

---

## 3. 值的归属（最关键的一条）

| 插件想做的事 | 值存哪 | 谁能写 |
|---|---|---|
| 只影响插件自己 | 插件 KV | 插件（通过 `storage.*`）与宿主渲染器 |
| **控制宿主提供的接口/行为** | **核心设置表** | **只有核心**，且只通过白名单 |

**白名单机制**：核心声明一批"可被插件控制"的设置项：

```json
{ "id": "download.export_lyrics_after_download", "type": "bool",
  "default": false, "label_key": "download.export_lyrics" }
```

- 插件只能引用**已注册**的 id；引用不存在的 id → 该行不渲染并记日志。
- 一个 id 只能被一个插件绑定；第二个插件绑定同一 id → 两边都失效（沿用 provider 冲突规则）。
- 写入必须走核心校验（类型、范围），并记一条审计日志（哪个插件改了什么）。

这条不做，任何插件都能改下载目录、自动更新开关这类设置，属于权限事故。

---

## 4. Schema 契约（草案）

```json
{
  "version": 1,
  "pages": [
    {
      "id": "visualizer",
      "title": { "zh": "音频可视化", "en": "Audio visualizer" },
      "anchor": "settings.category",
      "rows": [
        { "key": "style", "type": "select", "label": { "zh": "样式", "en": "Style" },
          "options": [ { "value": "bars", "label": { "zh": "条形", "en": "Bars" } } ],
          "default": "bars" },
        { "key": "sensitivity", "type": "number", "label": { "zh": "灵敏度", "en": "Sensitivity" },
          "min": 0, "max": 10, "step": 0.5, "default": 5 }
      ]
    }
  ],
  "rows": [
    { "anchor": "download.end", "key": "export_lyrics", "type": "switch",
      "label": { "zh": "下载后导出歌词", "en": "Export lyrics after download" },
      "binds": "download.export_lyrics_after_download" }
  ]
}
```

约束：

- **文案**：插件自带 locale map，缺失回落英文；**不往我们的 12 语言包里加键**（那是编译期资源）。
- **控件类型**：`switch` / `select` / `number` / `text` / `button`（按钮只能触发插件命令）。未知类型跳过并记日志。
- **长度上限**：标题、标签、说明各 ≤ 200 字符；单个插件 ≤ 3 页、每页 ≤ 20 行。
- **纯文本**：不允许 HTML/Markdown 渲染。
- **版本**：`version` 不认识 → 整体忽略该插件的设置（保留其它功能），不报错。

---

## 5. 交互与视觉

- 复用现有组件：`Switch`、`Select`、`Input`、`IconButton`（`@airmedy/ui`），不新造样式。
- 核心分组里注入的行与核心行**同款布局**（标签左、控件右、说明在下）。
- L3 子页面进入左侧分类列表，排在「插件」之后；插件停用则隐藏、值保留。
- 插件卸载：设置区消失，KV 保留（重装能恢复）；绑定白名单的项回到默认值并在 UI 上提示。

---

## 6. 工作分解

| # | 任务 | 估时 | 完成标准 |
|---|---|---|---|
| S1 | schema 契约 + 校验（类型、上限、locale map、版本） | 1 天 | 单测：未知类型跳过、超长截断、坏 JSON 不影响其它插件 |
| S2 | 宿主侧：读取插件 schema（`ui.settingsSchema` 方法，`describe` 同款机制）、缓存与失效 | 0.5 天 | 插件重启后 schema 刷新；未实现该方法的插件不受影响 |
| S3 | 值读写：插件 KV 通道 + `settings.changed` 通知 | 0.5 天 | 改开关后插件能收到通知；KV 默认拒绝仍生效 |
| S4 | 前端 L1：插件设置区渲染 | 1 天 | 组件 spec：switch/select/number 渲染与回写 |
| S5 | 前端 L3：声明式子页面 + 左侧分类动态项 | 1 天 | 页面在分类里出现/消失；停用即隐藏 |
| S6 | L2 锚点 + 核心白名单 + 冲突规则 + 审计日志 | 1.5 天 | 两插件绑同一 id → 都失效并给出原因 |
| S7 | 文档与测试 | 0.5 天 | `catalog/plugins` 补协议；12 locale 只加框架文案 |

**合计约 6 天**（L1+L3 约 4 天可先上线，L2 增量 1.5 天，收尾 0.5 天）。

---

## 7. 验收

**自动**

- schema 校验单测（未知类型 / 超限 / 坏 JSON / 缺 locale）
- 值读写单测：KV 默认拒绝、白名单 id 校验、冲突双失效
- 前端组件 spec：三类控件的渲染、改值回写、禁用态

**人工（由人做，AI 不做）**

1. 启用一个带设置的示例插件 → 插件设置区出现，改值后重启应用仍生效
2. 注入到「设置 → 下载」的行位置正确、与核心行视觉一致
3. 声明式子页面出现在分类里；停用插件后分类消失，值保留
4. 两个插件绑同一白名单 id → 都失效且有原因
5. 亮/暗/黑三主题各看一眼

---

## 8. 风险

| 风险 | 应对 |
|---|---|
| 插件注入行破坏核心分组布局 | 只允许行级插入 + 固定锚点；不允许整块替换 |
| 插件改核心设置导致用户配置被劫持 | 白名单 + 单绑定 + 审计日志 |
| schema 版本漂移 | `version` 不识别就整体跳过，且日志说明 |
| 文案无法统一 | 接受：插件自带 locale，宿主只保证排版与对齐 |
| 设置项越来越多导致页面失控 | 插件设置默认折叠；每插件最多 3 页 |

---

## 9. 本轮不做

- `ui.pages`（插件自带 HTML/WebView 容器）：需要本地服务、CSP、桥接与提权模型，单独立项
- 实时预览类 UI（可视化、试听）
- 插件之间的设置共享/继承
- 跨设备同步插件设置
