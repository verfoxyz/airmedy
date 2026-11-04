# 需求：禁止插件测试版的软件内更新

> 状态：待开工
> 目标：这个 alpha 构建**不查询、不下载、不提示**任何更新，并在「关于」里写明原因。
> 硬约束：AI 不做图形化测试；界面由人工确认。

---

## 1. 要达成的行为

| 位置 | 现状 | 改为 |
|---|---|---|
| 启动时自动检查更新 | 请求 `api.github.com/repos/misa198/airmedy/releases/latest` | **不发请求** |
| 设置 → 关于 →「检查更新」按钮 | 可点击，会弹更新对话框 | **移除按钮** |
| 设置 → 关于 → 状态文案 | 「已是最新」/「发现新版本」 | **固定显示「插件测试版，已禁止软件内更新」** |
| 侧栏更新徽章 | 有更新时出现 | 永不出现 |
| 更新对话框 / 应用更新 | 可用 | 不可达 |

**为什么必须连后端一起关**：只藏前端按钮，`UpdaterService.CheckForUpdate` 仍是可调用绑定，
而且启动流程依然会发网络请求；那既拖慢启动，也仍会弹出"发现新版本"。

---

## 2. 实现要点

### 2.1 一个开关，两处生效

在 `internal/app/config/meta.go` 增加一个编译期常量，例如：

```go
// UpdatesDisabled marks builds that must never talk to the release feed.
// The plugins alpha ships without in-app updates: it shares the official
// update channel, and accepting an update would silently replace the fork.
UpdatesDisabled = true
```

### 2.2 后端

`internal/app/updater/service.go`：

- `CheckForUpdate`：`UpdatesDisabled` 为真时**直接返回 `(nil, nil)`**，不发请求。
  返回 nil 而不是错误，是因为"没有更新"本来就是合法状态，前端不需要区分。
- `DownloadAndApply`：同样直接返回一个可读错误（例如
  `fmt.Errorf("in-app updates are disabled in this build")`），保证即使被调用也不会写盘。
- 不要删除这两个方法：绑定和类型还在，删了会让生成的绑定和前端类型对不上。

### 2.3 前端

`frontend/src/components/settings/AboutSettings.vue`：

- 删除「检查更新」按钮、「已是最新」状态块、以及"发现新版本"卡片；
- 在原位置放一行静态状态：图标 + `{{ t('settings.about.updates_disabled') }}`，
  用现有 secondary text token（`text-dim` / `text-subdued`），不引入新颜色；
- `checkUpdate()` 可以从该组件里删掉（`appStore.checkForUpdate` 仍需保留，别的地方可能引用）。

`frontend/src/stores/app.ts`：

- 启动时的 `checkForUpdate()` 调用可以保留（后端已返回 nil，不会再发请求），但更干净的做法是
  用一个 `updatesDisabled` 常量判断后跳过，避免一次无意义的 IPC。

### 2.4 文案（12 个 locale 同批）

新增键 `settings.about.updates_disabled`：

| 语言 | 文案 |
|---|---|
| zh | 插件测试版，已禁止软件内更新 |
| en | Plugin test build — in-app updates are disabled |
| 其余 10 种 | 按语义翻译，长度尽量与 en 接近 |

不要复用 `up_to_date`：那句话的含义是"已是最新"，与"不检查更新"是两件事。

---

## 3. 验收

**自动**

- `go build ./...`、`go test ./internal/app/updater/`（新增用例：禁用时 `CheckForUpdate` 不返回数据、
  且不产生 HTTP 请求——可用一个计数 transport 断言零请求）；
- `vue-tsc --noEmit`、相关组件 spec（关于页不再渲染检查按钮、渲染新文案）；
- 12 locale 键位对照测试通过。

**人工（由人做，AI 不做）**

1. 断网启动：没有任何更新相关弹窗或报错；
2. 设置 → 关于：只看到「插件测试版，已禁止软件内更新」，没有按钮；
3. 侧栏没有更新徽章；
4. 版本号仍显示 `1.3.3-plugins-alpha.1`。

---

## 4. 风险与边界

| 风险 | 说明 |
|---|---|
| 用户手动去 Release 页下载官方版 | 这个开关管不到；README 已有"会替换官方版/共享数据目录"的警告 |
| 将来想恢复更新 | 把常量改回 false 即可，前端文案需要一起改回来 |
| 与上游同步时的冲突 | `meta.go` 新增常量、`AboutSettings.vue` 改动，都是局部冲突，好解 |
