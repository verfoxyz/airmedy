# 本机环境与构建（Windows）

> 只记**已实测**的约束，防止重复排查。最后更新：2026-11-06

## 1. 环境变量（每次新 shell 都要重设）

```bash
# MSYS2 的 mingw-w64 gcc（scoop 装的 msys2，工具链用 pacman 装在里面）。
# 不要用 scoop 单独的 `gcc` 包：它的 mingw 头缺少 SMTC 声明，见 §1.1。
export PATH="/c/Users/vervea/scoop/apps/msys2/current/mingw64/bin:$(go env GOPATH)/bin:$PATH"
export CGO_ENABLED=1
export GOTMPDIR="$PWD/.gotmp" TMP="$PWD/.gotmp" TEMP="$PWD/.gotmp"   # 见 §2
export CGO_LDFLAGS="-static -static-libgcc"
mkdir -p "$PWD/.gotmp"
```

### 1.1 工具链：必须用 MSYS2 的 mingw-w64，不能用 scoop 的 `gcc`

2026-11 重装后实测：

| 工具链 | 结果 |
|---|---|
| scoop `gcc` 15.2（nuwen/winlibs） | ❌ `windows.media.h` 里没有 `ISystemMediaTransportControls`，`smtc_windows.cpp` 编译失败 |
| scoop `msys2` + `pacman -S mingw-w64-x86_64-gcc`（16.2.0） | ✅ `go build ./...` 通过 |

在 MSYS2 里装工具链的两个坑：

1. **mirror.msys2.org 与 tuna 都返回 403**；用 `https://mirrors.ustc.edu.cn/msys2/` 可以。
2. **`pacman-key --init` 会卡住很久**（gpg 生成密钥），不想等就在自定义配置里写
   `SigLevel = Never`：

```bash
M=/c/Users/vervea/scoop/apps/msys2/current
# 最小配置：只保留 msys / mingw64 / ucrt64，指向 USTC
$M/usr/bin/pacman.exe --config /c/Users/vervea/scoop/apps/msys2/current/etc/pacman-nosig.conf \
  -Sy && $M/usr/bin/pacman.exe --config <同上> -S --noconfirm --needed mingw-w64-x86_64-gcc
```

其他工具（均由 scoop 安装，已在 PATH）：`wails3`（beta.26）、`golangci-lint` 2.15.0、
`nsis`（`makensis`，打安装包用）、`task`（只用 `setup:hooks`）、`ffmpeg`/`ffprobe` 9.0.2（核对用）。

前置文件（缺则编译不过）：

| 项 | 说明 |
|---|---|
| `internal/app/config/secrets.go` | gitignored。从 `secrets.example.go` 复制，CI 用 dummy 值 |
| `internal/infra/audio/{ffmpeg,aubio,fftw3}_libs/windows/amd64/*.a` | 静态库，已就位 |

## 2. 两条方向相反的临时目录约束（都已实测）

| 对象 | 必须放 | 否则 |
|---|---|---|
| Go **编译临时目录**（`GOTMPDIR`/`TMP`/`TEMP`） | **工作区内**（`$PWD/.gotmp`，已在 `.gitignore`） | 杀软把 `%TEMP%` 下的新 exe 当可疑文件删，`go test` 在 `fork/exec` 阶段报 `Access is denied` |
| **构建出的 exe** | **`%TEMP%`** | 从工作区路径启动的进程写不了 `%LOCALAPPDATA%`，`config.NewConfig()`/`logging.NewFileLogger()` 失败后 `os.Exit(1)`；此时日志尚未建立，只看到进程静默消失 |

实测对照：exe 放 `build/bin/` → 进程静默退出、无日志；放 `%TEMP%` → 正常启动并写日志。
`go test ./internal/infra/downloader/` 用默认 temp → `fork/exec ...downloader.test.exe: Access is denied`；
用 `$PWD/.gotmp` → `ok`。

## 3. 构建与启动

`wails3 task windows:build` / `wails3 task verify` 在 Git Bash 下**不可用**：go-task 把
`ROOT_DIR` 的反斜杠当转义符，路径变成 `D:codemusicairmedy/...`，且检测不到 pnpm。

用等价命令：

```bash
wails3 generate bindings -d frontend/bindings
( cd frontend && npx vite build --mode production )
go build -tags production -trimpath -buildvcs=false   -ldflags="-w -s -H windowsgui" -o "$LOCALAPPDATA/Temp/airmedy-test.exe" .
"$LOCALAPPDATA/Temp/airmedy-test.exe"
```

> `frontend/bindings/` 是 gitignored 的生成产物，**不要手改**。`models.js` 只有 JSDoc，
> 加上 `noImplicitAny:false`，DTO 字段缺失 vue-tsc 抓不到——契约靠 Go 侧测试守。

快速循环用 WSL：`bash scripts/verify-wsl.sh [go|lint|fmt|plugin|ui|deps]`。
它替代不了 Windows 侧：cgo 静态库、WebView2、托盘、SMTC 与任何视觉检查仍需 Windows。

## 4. 播放随机崩溃（已修，`be5af28` + 一处补漏）

**曾经的错误结论**：归因于 `-H windowsgui`（带该 flag 的构建崩、不带的正常，三次对照都指向它）。**错的。**

**真因**：`AVChannelLayout` 栈变量未初始化，而 `av_channel_layout_copy()` 会先
`uninit` 目标——目标持栈垃圾时 `free` 野指针破坏堆，Windows 在下一次分配才报
`0xC0000374`（`STATUS_HEAP_CORRUPTION`）。

**为什么会被带偏**：崩溃不在 Go 层所以 `crash.log` 永远为空；`windowsgui` 只改变内存
布局，让损坏更早/更晚被检测到，不参与成因；是否触发取决于堆布局与文件声道布局。

**修复**：所有 `AVChannelLayout` 声明一律 `= {0}`。全仓共 **4 处**：`ffmpeg_decoder.h` 两处
(151/157) + `ffmpeg_analyzer.h` 两处 (258/322)。`be5af28` 当时只修了 3 处，
`ffmpeg_analyzer.h` 的 `ffmpeg_analyze()` 里那处是后来补的。

**性质是上游缺陷**，v1.3.3 已存在，同步上游时注意别被覆盖。排查同类崩溃时先
`grep -n "AVChannelLayout.*=" internal/infra/audio/*.h`，确认每一处都带 `= {0}`。

## 5. 数据库单向升级

开发版跑过后 `airmedy.db` 会升到当前 migration（写作时 **83**，`000083_download_content`），
而官方 1.3.3 只内嵌到 000080，启动即失败：

```
failed to run migrations: no migration found for version 83:
read down for version 83 migrations: file does not exist
```

**影响**：官方发布版从此打不开同一个数据目录。

- 备份：`D:\code\music\backup\airmedy-data-20261102-204832`（v80，含 db/bleve/logs）
- 回退：关应用 → 用备份覆盖 `%LOCALAPPDATA%\airmedy\airmedy.db` → 再启动官方版
- 数据目录：`%LOCALAPPDATA%\airmedy\`（`airmedy.db`、`logs/airmedy.log`、`plugins/lx/`）

## 6. 前端依赖（node_modules）损坏的恢复

**症状**：`npx vitest` 报包内容缺失、`.bin/vitest` 指向的目录条目比 `node_modules/.pnpm`
里的另一份少；`pnpm install` 认为"已是最新"不动它。

**原因**：`node_modules` 的链接树被破坏（安装被中断、杀软清理、`git clean -xdf`），
而 pnpm 的 store 本身通常是好的。**store 在仓库内**：`<repo>/.pnpm-store`。

**恢复顺序**（逐级加强，本次实测第一级即可）：

```bash
# 1. 跳过"已是最新"判断，按 lockfile 重建链接（秒级，不下载）
pnpm install --force --frozen-lockfile

# 2. 仍然缺文件 → store 里该包坏了：清条目后重装
pnpm store prune && pnpm install --force --frozen-lockfile

# 3. 彻底重来（会重新下载）
rm -rf node_modules frontend/node_modules packages/*/node_modules
pnpm install --frozen-lockfile
```

**验证**：`ls frontend/node_modules/vitest | wc -l` 应为 25 左右（残缺时是 22），
然后 `cd frontend && npx vitest run` 全绿。

**预防**：不要用 `git clean -xdf` 清仓库；store 与 `node_modules` 都在仓库内，
一次误清就要重新下载 500 MB。杀软对 `node_modules` 的扫描也会造成间歇性缺文件。

## 7. 已失效的旧结论（别再照做）

- ~~`TestPlaylistMutationLWWIsDurableAndDeterministic` 在 Windows 必失败，不要修~~
  —— 那是 `%TEMP%` 被拦造成的误诊；现在默认 temp 和工作区 temp 都通过。
- ~~插件需要宿主代理网络（`http.request`）才能受控~~
  —— stdio 单行 8 MiB 上限使代理比直连更弱，见 `catalog/plugins/README.md`。
