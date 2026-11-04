#!/usr/bin/env bash
# check-env.sh —— 下载器分支的环境自检
#
#   bash scripts/check-env.sh          # 只检查前置
#   bash scripts/check-env.sh --test   # 检查 + 跑测试
#
# 只读：不会修改仓库、不会联网、不会 push。

set -uo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
REPO="${REPO:-$ROOT}"
RUN_TESTS=0
[ "${1:-}" = "--test" ] && RUN_TESTS=1

fail=0
ok()   { printf '\033[32m  ✓\033[0m %s\n' "$1"; }
bad()  { printf '\033[31m  ✗\033[0m %s\n' "$1"; fail=1; }
warn() { printf '\033[33m  !\033[0m %s\n' "$1"; }

echo "== 仓库 =="
# `git worktree` 的 .git 是文件（gitdir 指针），不能只用 -d 判断
if git -C "$REPO" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  ok "仓库存在：$REPO"
  branch="$(git -C "$REPO" branch --show-current 2>/dev/null)"
  if [ "$branch" = "master" ]; then
    ok "分支：$branch"
  else
    warn "当前分支是 '$branch'，交接分支应为 master"
  fi
else
  bad "找不到仓库：$REPO（可用 REPO=/path/to/airmedy 覆盖）"
fi

echo "== 构建前置 =="
[ -f "$REPO/internal/app/config/secrets.go" ] \
  && ok "secrets.go" \
  || bad "缺 internal/app/config/secrets.go（CI 里是 dummy Last.fm key）"

libs=$(ls "$REPO"/internal/infra/audio/ffmpeg_libs/windows/amd64/*.a 2>/dev/null | wc -l | tr -d ' ')
if [ "$libs" -ge 5 ]; then ok "ffmpeg 静态库：$libs 个"; else bad "ffmpeg 静态库只有 $libs 个（应为 5）"; fi

[ -f "$REPO/internal/infra/audio/miniaudio/miniaudio.h" ] \
  && ok "miniaudio.h" \
  || bad "缺 internal/infra/audio/miniaudio/miniaudio.h（跑 scripts/fetch-miniaudio.sh）"

if ls "$REPO"/internal/infra/audio/aubio_libs 2>/dev/null | grep -q .; then
  ok "aubio 静态库"
else
  warn "缺 aubio/fftw3 → go build ./internal/... 会在 audio 包失败（预期，别修）"
fi

echo "== 工具链 =="
for t in go gcc pkg-config; do
  if command -v "$t" >/dev/null 2>&1; then ok "$t $($t --version 2>/dev/null | head -1 | cut -c1-40)"; else bad "缺 $t"; fi
done

if [ "$RUN_TESTS" = "1" ]; then
  [ "$fail" = "1" ] && { echo; echo "前置未通过，跳过测试。"; exit 1; }
  echo "== 测试 =="
  cd "$REPO" || exit 1
  export PATH="/d/PFilesD/MSYS/mingw64/bin:$PATH"

  # Windows 上杀软常按文件名拦截 <pkg>.test.exe（例如 downloader.test.exe），
  # 报 "fork/exec ...: Access is denied"。把 Go 的临时目录放进工作区（信任区），
  # 二进制路径就落在 music/ 下面。设 GOTMPDIR 的前置：music/.gotmp 已 gitignore。
  export GOTMPDIR="${GOTMPDIR:-$ROOT/.gotmp}"
  # 测试里的 os.MkdirTemp("") 走 TMP/TEMP；不指进工作区时会被拦在系统临时目录。
  export TMP="${TMP_IN_TRUSTED:-$GOTMPDIR}"
  export TEMP="$TMP"
  export TMPDIR="$TMP"
  mkdir -p "$GOTMPDIR" "$TMP"

  run() {
    printf '  → %s\n' "$*"
    if "$@" >"$GOTMPDIR/check-env.out" 2>&1; then ok "通过"; else
      bad "失败：$*"; tail -20 "$GOTMPDIR/check-env.out" | sed 's/^/      /'
    fi
  }

  run go test ./internal/domain/ ./internal/app/library/ ./internal/app/playlist/ ./internal/app/player/ -count=1
  run go test ./internal/infra/sqlite/ -count=1 -skip TestPlaylistMutationLWWIsDurableAndDeterministic
  # 只 vet 不依赖 audio 的包：audio 需要 aubio/fftw3，缺库时无法编译。
  run go vet ./internal/domain/ ./internal/infra/sqlite/     ./internal/app/library/ ./internal/app/playlist/ ./internal/app/player/
  rm -f "$GOTMPDIR/check-env.out"
fi

echo
if [ "$fail" = "1" ]; then
  echo "结果：有失败项（见上）"
  exit 1
fi
echo "结果：全部通过"
