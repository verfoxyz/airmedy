# 计划：用 ffmpeg 探测时长，替换 taglib 作为时长来源

> 状态：待开工
> 触发问题：`心似烟火 - 陈壹千.flac` 在歌词信息里显示 **37:15**（库中 `duration=2235`），
> 实际文件 271.6 秒（`ffprobe` 读数），播放正常。
> 硬约束：**AI 不做图形化测试**；界面由人工确认。

---

## 1. 根因

| 事实 | 证据 |
|---|---|
| 库中该行 `duration = 2235` | `select duration from tracks where title='心似烟火'` |
| 文件真实时长 271.63s | `ffprobe -show_entries format=duration` |
| 容器是 **raw FLAC** | `format_long_name="raw FLAC"` |
| 文件解码无错误 | `ffmpeg -f null -` 无输出 |

网易云返回的流是 raw FLAC（缺少标准 metadata block 链），**taglib 在这种容器上算时长不可靠**，
两个方向都会错：

- 上一次：`前前前世 (movie ver.)` → 报 **1 秒**；
- 这一次：`心似烟火` → 报 **2235 秒**（真实 271）。

上一轮的守卫只拦了"过小"（`<2s` 且原值 `>=5s`），"过大"没拦，所以直接写进了库。

更麻烦的是：**修复动作也用同一个 taglib 读**，对这类文件它会"修"成 2235，等于把错误固化。

---

## 2. 目标与不变量

**目标**：时长只有一个可信来源——**解码器/容器探测（ffmpeg）**；标签里的时长只作兜底。

不变量（写完代码后应一直成立）：

1. 写入 `tracks.duration` 的值必须来自探测，或来自**未被探测推翻**的已有值；
2. 探测失败时才回落 taglib，且必须与已有值做合理性比对；
3. `RepairDurations` 只在探测成功且结果合理时写入；探测失败就报"文件不可读"，
   绝不把另一个不可靠读数写回去；
4. 远程行（`remote://`）永远不参与时长探测。

---

## 3. 方案

### 3.1 新增一个轻量探测入口（C 侧）

在 `internal/infra/audio/ffmpeg_analyzer.h` 增加一个**不解码**的探测函数：

```
int ffmpeg_probe_duration(const char *path, double *out_seconds, volatile int *cancel)
```

- 复用文件里已有的 `ffmpeg_probe_mu` 互斥与 `avformat_open_input` / `avformat_find_stream_info`；
- 只读容器/流头的时长（`av_stream_get_duration` 或 `st->duration * time_base`）；
- 返回码区分：打不开、无音频流、时长未知、成功；
- **不**执行解码，不建 filter graph，成本与现有"解码整首"的分析完全不是一个量级。

### 3.2 端口与适配（Go 侧）

- `domain` 新增窄端口，例如 `MediaDurationProbe { Probe(ctx, path) (time.Duration, error) }`；
- `internal/infra/audio` 实现它（同包内复用 cgo 封装与路径转换）；
- `main.go` 绑定到端口，`app` 层只认端口，不 import `infra/audio`。

### 3.3 三处调用点改为"探测优先"

| 调用点 | 现状 | 改为 |
|---|---|---|
| 下载收尾 `applyDecodedDuration` | 用 `FinalizeResult.DurationSeconds`（taglib 回读） | 先探测；成功则写入，失败则沿用守卫后的 taglib 值 |
| 导入（`ImportFileWithID` 路径） | 用标签里的时长 | 探测优先；探测失败保留标签值 |
| `RepairDurations` | 重新 `Extract`（taglib） | 探测优先；探测失败 → 计入 `Failed` 并说明"容器不可读" |

保留现有守卫（`<2s` 且原值可信时拒绝写入）作为**第二道防线**，但不再是唯一手段。

---

## 4. 历史数据修复

- 探测上线后，用户触发一次 `RepairDurations`：`2235 → 271`、`1 → 284` 都能被纠正。
- 被旧版打标写坏的 FLAC（ID3v2 混进音频流）：探测可能仍报可读时长，
  但解码会失败——这类文件应通过"重新下载"修复，**不要**用修复动作掩盖。
  建议修复结果里区分两类：`Repaired`（写入成功）与 `Unreadable`（文件损坏，建议重新下载）。
- 不做启动全库扫描（成本与网络盘风险都不值得）。

---

## 5. 验收

**自动**

- C 侧：探测同一份 raw FLAC、MP3、以及一个损坏文件，断言返回值分类正确；
- Go 侧端口：`Probe` 对可读文件返回与 `ffprobe` 相近的秒数（±1s）；
- `applyDecodedDuration`：探测值 271 / taglib 值 2235 → 写入 271；
- `RepairDurations`：探测失败的文件进 `Unreadable`，不写入任何时长。

**人工（由人做，AI 不做）**

1. 歌词信息里 `心似烟火` 显示 4:31，而不是 37:15；
2. 播放进度与显示一致；
3. 触发修复后库中该行变成 271；
4. 损坏文件（前前前世）提示"文件不可读"，修复不成功、也不写坏数据。

**只读校验**

```bash
ffprobe -v error -show_entries format=duration -of default=nw=1 "<file>"
sqlite3 -readonly "%LOCALAPPDATA%/airmedy/airmedy.db" \
  "select title,duration from tracks where duration>3600 or duration<=1;"
```

---

## 6. 风险

| 风险 | 应对 |
|---|---|
| cgo 探测与后台分析抢解码器 | 复用同一个互斥；探测只读头，持锁时间远短于分析 |
| Windows 路径非 UTF-8 | 沿用现有 `C.CString` 路径约定；前后端统一 UTF-8，不额外转换 |
| 某些容器时长未知（`AV_NOPTS`） | 返回"未知"，回落 taglib + 现有守卫，并在日志标记 |
| 误伤真实超长音频（播客、现场） | 只做"与已有值偏差过大"的判断，不设绝对上限；真实长音频探测值本身就正确 |
| 探测比 taglib 慢 | 只读头，实际是毫秒级；如遇网络盘再评估缓存 |

---

## 7. 估时

| 任务 | 估时 |
|---|---|
| C 探测函数 + 错误码 + 复用互斥 | 0.5 天 |
| Go 端口 + 适配 + main 接线 | 0.5 天 |
| 三处调用点改为探测优先（含守卫保留） | 0.5 天 |
| 测试（C/Go/修复分类）+ 文档 | 0.5 天 |

**合计约 2 天。**

---

## 8. 交接

- 仓库：`D:\code\music\airmedy`；基线 `a77ecb4`。
- 编译测试版：
  ```bash
  export PATH="/d/PFilesD/MSYS/mingw64/bin:$(go env GOPATH)/bin:$PATH"
  export CGO_ENABLED=1 GOTMPDIR="$PWD/.gotmp" TMP="$PWD/.gotmp" TEMP="$PWD/.gotmp"
  wails3 generate bindings -d frontend/bindings
  ( cd frontend && npx vite build --mode production )
  go build -tags production -trimpath -buildvcs=false -o "$LOCALAPPDATA/Temp/airmedy-test.exe" .
  ```
- 验证：`go test ./...`、`golangci-lint run`、`gofmt -l`、`node scripts/check-docs.mjs`。
- 改动会触及 `catalog/library`（导入时长来源）与 `catalog/download`（收尾写入），需同步契约。
- **不做图形化测试**；不要自行 `git commit`，先给拆分建议。
