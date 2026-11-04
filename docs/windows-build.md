# 本机环境与构建（Windows）

> 只记**已实测**的约束，防止重复排查。最后更新：2026-11-03

## 1. 环境变量（每次新 shell 都要重设）

```bash
export PATH="/d/PFilesD/MSYS/mingw64/bin:$(go env GOPATH)/bin:$PATH"
export CGO_ENABLED=1
export GOTMPDIR="$PWD/.gotmp" TMP="$PWD/.gotmp" TEMP="$PWD/.gotmp"   # 见 §2
mkdir -p "$PWD/.gotmp"
```

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
go build -tags production -trimpath -buildvcs=false -o "$LOCALAPPDATA/Temp/airmedy-test.exe" .
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

## 6. 已失效的旧结论（别再照做）

- ~~`TestPlaylistMutationLWWIsDurableAndDeterministic` 在 Windows 必失败，不要修~~
  —— 那是 `%TEMP%` 被拦造成的误诊；现在默认 temp 和工作区 temp 都通过。
- ~~插件需要宿主代理网络（`http.request`）才能受控~~
  —— stdio 单行 8 MiB 上限使代理比直连更弱，见 `catalog/plugins/README.md`。
