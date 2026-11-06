# 计划：左下封面改为进全屏，右下全屏按钮改为「歌曲信息」

> 状态：待开工
> 范围：`frontend/src/components/PlayerFooter.vue`、12 个 locale
> 硬约束：AI 不做图形化测试；交互由人工确认。

---

## 1. 目标

把播放条两个入口对调一下：

| 位置 | 现状 | 改为 |
|---|---|---|
| **左下角封面** | 单击打开**歌曲信息**抽屉 | 单击进入**全屏播放器** |
| **右下角「最大化」按钮** | 进入全屏 | 打开**歌曲信息**抽屉（图标换成 `Info`） |

其余不变：封面右键菜单仍打开封面相关菜单；歌词按钮、队列按钮、迷你播放器按钮都不动；
全屏内的布局与页签不改。

---

## 2. 交互细节

### 2.1 左下封面 → 全屏

- 单击 / Enter / Space → `store.setPlayerMode('fullscreen')`；
- 封面改为**可聚焦按钮**：`aria-label` = 曲名 + "进入全屏"；
- **hover / focus 提示**「点击进入全屏」（复用现有 `title` 机制，不新造 tooltip）；
- **没有当前曲目时不进入全屏**，按钮 `aria-disabled`；
- 右键菜单保留（`@contextmenu.prevent="openArtworkContextMenu"` 不动）；
- hover 时可用一层极轻的主题遮罩提示可点（`bg-foreground/[0.04]` 级别，禁止白/黑透明度）。

### 2.2 右下按钮 → 歌曲信息

- 保留按钮位置与尺寸，图标从 `Maximize` 换成 `Info`；
- `@click` 改为 `store.openTrackInfo(store.currentTrack)`；
- `title` / `aria-label` 用歌曲信息的文案（`track_info.title` 或新增 `player.track_info`）；
- **没有当前曲目时禁用**，与封面保持一致。

### 2.3 全屏内

不改。现在全屏里点标题/歌手会退出全屏再开抽屉——这条**保持原样**，不在本次范围。

---

## 3. 文案

| 键 | 用途 | 备注 |
|---|---|---|
| `player.open_fullscreen_hint` | 封面提示「点击进入全屏」 | 12 语言同批新增 |
| `player.track_info` | 右下按钮的 title/aria | 若 `track_info.title` 已存在可复用，不重复加 |

---

## 4. 需要改的文件

| 文件 | 改动 |
|---|---|
| `frontend/src/components/PlayerFooter.vue` | 封面按钮化 + 全屏动作；右下按钮换图标与动作 |
| `frontend/src/locales/*.json`（12 个） | 新增/复用上述键 |
| `PlayerFooter.spec.ts` | 封面点击进全屏、无曲目时不进、右键菜单仍触发、右下按钮打开信息 |

---

## 5. 验收

**自动**

- 组件 spec：封面点击调用 `setPlayerMode('fullscreen')`；无当前曲目时调用被拒绝；
- 组件 spec：右下按钮调用 `openTrackInfo`；图标为 `Info`；
- 12 locale 键位一致测试通过。

**人工（由人做，AI 不做）**

1. 点封面 → 进全屏；hover 有提示；
2. 点右下按钮 → 打开歌曲信息抽屉；
3. 没有播放曲目时两个按钮都不动作；
4. 封面右键菜单仍能打开；
5. 亮/暗/黑三主题各看一眼。

---

## 6. 风险与边界

| 项 | 说明 |
|---|---|
| 发现性 | 全屏入口从图标变成封面，同时给 hover 提示；右下按钮仍有图标 |
| 老用户习惯 | 原来点封面看信息，现在点右下；两者都在原位附近，成本可接受 |
| 不做 | 全屏内的"点标题退出全屏看详情"保持原样；迷你播放器不动 |
