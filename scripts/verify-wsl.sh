#!/usr/bin/env bash
# verify-wsl.sh — 在 WSL 里跑跨平台等价的检查。
#
# 用途：Windows 上 pnpm 的全局锁目录被安全软件挡住、且部分工具版本对不齐，
# 这里提供一个干净的 Linux 侧验证入口。Windows 产物构建与实机验证仍在
# Windows 上做，本脚本不碰 native 库、不产出二进制。
#
# 用法：
#   bash scripts/verify-wsl.sh            # 全部
#   bash scripts/verify-wsl.sh go         # 只跑 Go 测试
#   bash scripts/verify-wsl.sh lint       # 只跑 golangci-lint
#   bash scripts/verify-wsl.sh fmt        # 只检查 gofmt
#   bash scripts/verify-wsl.sh plugin     # 只跑 plugins/*/ 的 node 测试
#   bash scripts/verify-wsl.sh ui         # 只跑前端 vitest
#   bash scripts/verify-wsl.sh docs       # 只检查 catalog/ 文档
#   bash scripts/verify-wsl.sh deps       # 只装前端依赖
#
# 退出码：任一步失败即非 0。

set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

# Go 的临时目录必须留在磁盘上可写的位置；/mnt/d 上跨 VM 边界较慢，
# 但放在这里能避免与 Windows 侧的 .gotmp 抢文件。
export GOTMPDIR="${GOTMPDIR:-$HOME/.cache/airmedy-gotmp}"
export GOCACHE="${GOCACHE:-$HOME/.cache/airmedy-gocache}"
mkdir -p "$GOTMPDIR" "$GOCACHE"

# Linux 侧不需要手工编的 ffmpeg/aubio/fftw3 静态库——
# 那些只在 windows/ 与 darwin/ 下存在，Linux 走系统库。
failed=()
step() { printf '\n\033[1m==> %s\033[0m\n' "$1"; }
fail() { failed+=("$1"); printf '\033[31m  ✗ %s\033[0m\n' "$1"; }

run_go() {
    step "go test ./... (Linux)"
    # main 包需要 Linux 版 native 库，缺失时跳过而不是失败；
    # 它唯一的测试是跨平台的 fx 图校验，Windows 上已覆盖。
    if go test ./... -count=1 2>&1 | tee /tmp/airmedy-gotest.log | grep -qE '^(FAIL|--- FAIL)'; then
        fail "go test"
        grep -E '^(FAIL|--- FAIL)' /tmp/airmedy-gotest.log | head -10
    else
        echo "  全部通过"
    fi
}

run_lint() {
    step "golangci-lint run"
    if ! command -v golangci-lint >/dev/null 2>&1; then
        echo "  跳过：未安装（pacman -S golangci-lint 或 go install ...@v2.15.0）"
        return
    fi
    if golangci-lint run; then
        echo "  0 issues"
    else
        fail "golangci-lint"
    fi
}

run_fmt() {
    step "gofmt -l"
    local out
    # 只检查受版本控制的 Go 文件，避免把 node_modules 扫进来。
    out="$(git ls-files '*.go' | xargs -r gofmt -l)"
    if [[ -n "$out" ]]; then
        fail "gofmt"
        printf '%s\n' "$out" | sed 's/^/  /'
    else
        echo "  干净"
    fi
}

run_plugin() {
    # 每个 plugins/<id>/ 都是独立进程、独立测试集：新增一个插件只多一个目录，
    # 不需要在这里加分支。
    local found=0
    local dir
    for dir in "$ROOT"/plugins/*/; do
        [[ -d "$dir/test" ]] || continue
        found=1
        step "$(basename "$dir") 插件 node --test"
        if ( cd "$dir" && node --test test/*.test.js ); then
            echo "  全部通过"
        else
            fail "node --test ($(basename "$dir"))"
        fi
    done
    if [[ "$found" -eq 0 ]]; then
        echo "  跳过：没有 plugins/*/test 目录"
    fi
}

run_ui() {
    step "前端 vitest"
    if [[ ! -d "$ROOT/node_modules" ]]; then
        echo "  跳过：node_modules 不存在，先跑 'bash scripts/verify-wsl.sh deps'"
        return
    fi
    if ( cd "$ROOT/frontend" && npx vitest run ); then
        echo "  全部通过"
    else
        fail "vitest"
    fi
}

run_docs() {
    step "catalog/ 文档检查"
    if node scripts/check-docs.mjs; then
        echo "  通过"
    else
        fail "check-docs"
    fi
}

run_deps() {
    step "安装前端依赖（Linux 版）"
    # Windows 的 node_modules 里有 Windows 原生二进制，Linux 下不能用。
    if [[ -d "$ROOT/node_modules" ]] && [[ ! -x "$ROOT/node_modules/.bin/vitest" ]]; then
        rm -rf "$ROOT/node_modules"
    fi
    if pnpm install --frozen-lockfile; then
        echo "  完成"
    else
        fail "pnpm install"
    fi
}

case "${1:-all}" in
    go)     run_go ;;
    lint)   run_lint ;;
    fmt)    run_fmt ;;
    plugin) run_plugin ;;
    docs)   run_docs ;;
    ui)     run_ui ;;
    deps)   run_deps ;;
    all)    run_fmt; run_docs; run_go; run_lint; run_plugin; run_ui ;;
    *)      echo "未知步骤: $1"; exit 2 ;;
esac

echo
if (( ${#failed[@]} )); then
    printf '\033[31m失败：%s\033[0m\n' "${failed[*]}"
    exit 1
fi
printf '\033[32m通过\033[0m\n'
