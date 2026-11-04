# Checklist：把「插件版」作为上游的一个分支发布

> 目标形态：**Airmedy 的一个分支**（带插件能力的构建），不是"另做一个新应用"。
> 上游：`misa198/airmedy`，**GPL-3.0**。
> 硬约束：AI 不做图形化测试；推送、force push、打 tag 属高风险操作，执行前需你确认。

---

## 0. 形态决定了哪些事不用做

| 事项 | 独立新应用 | **分支（本方案）** |
|---|---|---|
| 改产品名 / 包名 / 图标 | 必须 | **不必**：它就是 Airmedy 的一个分支 |
| 保留上游署名 | 必须 | 必须（而且 GitHub 的 "forked from" 自动帮你标出来） |
| 数据目录 | 建议改名避免冲突 | **看是否要共存**（见 §4） |
| 商标风险 | 高 | **低**：分支持有原名是常见做法，只要不冒充官方发布 |

**结论**：分支形态下，`productName`、`productIdentifier`、图标、`RepoOwner/RepoName`
都可以保留原样；真正必须处理的是**插件**和**声明"这是非官方分支"**。

---

## 1. GPL-3.0 合规（四件必须做的）

1. 保留 `LICENSE`，你的分支同样以 GPL-3.0 发布；
2. 保留原作者署名（README 里 "GPL-3.0 © misa198" 不能删）；
3. **标注你的修改**：README 加一节 "Plugins branch"，说明这个分支加了什么、基于哪个提交；
4. 公开完整源码。

---

## 2. 分支怎么命名与标识

| 对象 | 建议 | 理由 |
|---|---|---|
| GitHub 仓库 | 直接 **fork**，不改仓库名 | 保留 "forked from misa198/airmedy" 标签，来源一目了然 |
| 分支名 | `plugins`（长期分支）或 `feat/plugin-host` | 让人一眼知道这是插件分支 |
| Release tag | `v1.3.3-plugins` / `v1.3.3+plugins.1` | 与上游版本号挂钩，避免和官方 release 混淆 |
| 应用内版本号 | 加后缀，例如 `1.3.3-plugins` | 用户报问题时能区分是哪个构建 |
| README 顶部 | 一段 "This is an unofficial fork of Airmedy that adds plugin support." | 这是"不冒充官方"的关键声明 |

**不要做**：把构建产物发布到上游的 release 页、用上游的下载链接名义发布、把分支描述成"官方插件版"。

---

## 3. 插件：仍然不要进仓库（唯一实质风险）

分支可以带**宿主能力**（插件加载、协议、权限），但**不要分发插件本身**：

```bash
git rm -r --cached plugins
printf '\n/plugins/*\n!/plugins/README.md\n' >> .gitignore
```

或者更彻底：把 `plugins/` 移出仓库，仓库里只留 `plugins/README.md`
（"插件独立安装到 `<数据目录>/plugins`"）。

理由：宿主能力是通用技术；具体插件里针对第三方平台的抓取代码，风险来自**平台服务条款**，
与 GPL 无关，也最容易给分支带来麻烦。

---

## 4. 数据目录：要不要共存

| 场景 | 做法 |
|---|---|
| 分支替代官方版（推荐） | 保持 `appDataFolder = "airmedy"`，用户卸载原版再装分支，库和设置直接沿用 |
| 两个版本同时装 | 必须改成 `airmedy-plugins`（`internal/app/config/dev.go` / `prod.go`），否则数据库、插件、缓存互相污染 |

注意：改名后等于全新档案，**首次启动是空的**；要沿用旧库需要自己做一次迁移。

---

## 5. 发布流程

```bash
# 1. 在 GitHub 上 fork misa198/airmedy（保持名字）
git remote rename origin upstream
git remote add origin https://github.com/<你>/airmedy.git

# 2. 建分支
git checkout -b plugins

# 3. 移除插件跟踪 + 写 README 的 "Plugins branch" 段
git rm -r --cached plugins
# 编辑 README，标注修改内容与"非官方分支"

git commit -m "feat: plugin-host branch (unofficial), plugins shipped separately"

# 4. 推送（高风险，需你确认后执行）
git push -u origin plugins
```

上游继续更新时：`git fetch upstream && git merge upstream/master`。
若上游仍在 `plugins/` 下加文件，合并后重跑一次 §3 的取消跟踪。

---

## 6. 提交者身份（已完成的部分）

仓库里已有 `.mailmap`，把 AI/工具身份统一显示为 `DS AI <ai@airmedy.local>`：

- `git shortlog` / `git log --use-mailmap` / `git blame` 显示统一名字；
- **不改哈希、不用 force push**；
- 上游作者（misa198 / Thanh Vu）保持原样——那是他们的作品，也是 GPL 署名的一部分。

如果之后要连历史一起改：`git filter-repo --mailmap .mailmap`，会改所有哈希、需要 force push，另行确认。

---

## 7. 发布前自检

- [ ] 仓库是 fork（显示 "forked from misa198/airmedy"）
- [ ] 分支名 `plugins`，README 有 "unofficial fork / plugins branch" 声明
- [ ] `LICENSE`、`NOTICES`、"GPL-3.0 © misa198" 完整保留
- [ ] `git ls-files plugins` 为空（或只剩 `plugins/README.md`）
- [ ] 文档不再声称本分支自带插件；提到平台时写"由用户自行安装的插件提供"
- [ ] Release tag 带 `-plugins` 后缀，且没有发布到上游 release 页
- [ ] 若两个版本共存，`appDataFolder` 已改；否则保持 `airmedy`
- [ ] `node scripts/check-docs.mjs` 通过（文档链接与目录块）

---

## 8. 边界

- 本文不构成法律意见。
- 分支形态降低了商标风险，但**没有**降低"分发针对第三方平台抓取代码"的风险——那块靠"不进仓库"解决。
- 已公开的历史无法收回；改历史只是让云端不再显示，不等于没发生过。
