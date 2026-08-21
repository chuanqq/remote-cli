#!/usr/bin/env bash
#
# remote-agent-proxy 构建脚本 —— 支持多平台交叉编译
#
# 用法:
#   ./build.sh                       # 构建当前平台(native)
#   ./build.sh linux                 # linux/amd64(只给 os 时 arch 默认 amd64)
#   ./build.sh linux/arm64           # 指定 os/arch
#   ./build.sh linux windows/amd64   # 一次构建多个目标
#   ./build.sh all                   # 常用平台全套
#   ./build.sh list                  # 列出预设目标平台
#   ./build.sh -h | --help           # 帮助
#
# 环境变量:
#   OUT_DIR   产物输出目录(默认 dist)
#   STRIP     1=精简体积(-s -w,默认)  0=保留调试信息
#   VERSION   覆盖版本号(默认从 types.go 的 serverVersion 提取)
#
# 说明: darwin 目标依赖 cgo(sysinfo_darwin.go),只能在 macOS 主机上构建;
#       linux / windows 目标使用 CGO_ENABLED=0 静态编译,可在任意主机交叉编译。

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$SCRIPT_DIR"

BIN_NAME="remote-agent-proxy"
OUT_DIR="${OUT_DIR:-dist}"
STRIP="${STRIP:-1}"

# 版本号: 优先环境变量,其次从 types.go 提取,兜底 dev
VERSION="${VERSION:-$(grep -oE 'serverVersion = "[^"]+"' types.go 2>/dev/null | grep -oE '[0-9]+\.[0-9]+\.[0-9]+' || true)}"
VERSION="${VERSION:-dev}"

# 预设目标平台(all)
PRESET_TARGETS=(
    "linux/amd64"
    "linux/arm64"
    "darwin/amd64"
    "darwin/arm64"
    "windows/amd64"
)

# 颜色输出
GREEN='\033[0;32m'; YELLOW='\033[1;33m'; RED='\033[0;31m'; NC='\033[0m'
log_info()  { echo -e "${GREEN}[INFO]${NC}  $*"; }
log_warn()  { echo -e "${YELLOW}[WARN]${NC}  $*"; }
log_error() { echo -e "${RED}[ERROR]${NC} $*"; }

HOST_OS="$(go env GOOS 2>/dev/null || echo unknown)"

print_help() {
    sed -n '3,24p' "$0" | sed 's/^# \{0,1\}//'
}

print_list() {
    log_info "预设目标平台(./build.sh all):"
    for t in "${PRESET_TARGETS[@]}"; do echo "  - ${t}"; done
    echo
    log_info "当前主机平台: ${HOST_OS}/$(go env GOARCH)"
    log_warn "darwin 目标依赖 cgo,只能在 macOS 主机上构建;linux/windows 可任意主机交叉编译。"
}

# 规范化单个目标: 补全默认 arch,返回 "os/arch"
normalize_target() {
    local t="$1"
    if [[ "$t" != */* ]]; then
        t="${t}/amd64"   # 只给 os 时 arch 默认 amd64
    fi
    echo "$t"
}

# 构建单个目标
build_one() {
    local os="$1" arch="$2"
    local out="${OUT_DIR}/${BIN_NAME}-${VERSION}-${os}-${arch}"
    [[ "$os" == "windows" ]] && out="${out}.exe"

    # darwin 需要 cgo(sysinfo_darwin.go 有 import "C"),只能原生构建
    local cgo=0
    if [[ "$os" == "darwin" ]]; then
        if [[ "$HOST_OS" != "darwin" ]]; then
            log_warn "跳过 ${os}/${arch}: darwin 目标依赖 cgo,需在 macOS 主机构建"
            return 0
        fi
        cgo=1
    fi

    local ldflags=""
    [[ "$STRIP" == "1" ]] && ldflags="-s -w"

    log_info "构建 ${os}/${arch} (cgo=${cgo}) -> ${out}"
    CGO_ENABLED="${cgo}" GOOS="${os}" GOARCH="${arch}" \
        go build ${ldflags:+-ldflags="${ldflags}"} -o "${out}" .

    local size
    size=$(du -h "${out}" | cut -f1)
    log_info "完成: ${out} (${size})"
}

# 解析 "os/arch" 并构建
dispatch_target() {
    local t; t="$(normalize_target "$1")"
    local os="${t%%/*}" arch="${t##*/}"
    build_one "$os" "$arch"
}

main() {
    if ! command -v go &>/dev/null; then
        log_error "未找到 Go 编译器,请先安装 Go 1.23+"
        exit 1
    fi

    # 无参数: 构建当前平台
    if [[ $# -eq 0 ]]; then
        mkdir -p "${OUT_DIR}"
        log_info "版本: ${VERSION} | 输出目录: ${OUT_DIR} | strip: ${STRIP}"
        build_one "$(go env GOOS)" "$(go env GOARCH)"
        return
    fi

    case "$1" in
        -h|--help) print_help; return ;;
        list)      print_list; return ;;
    esac

    mkdir -p "${OUT_DIR}"
    log_info "版本: ${VERSION} | 输出目录: ${OUT_DIR} | strip: ${STRIP}"

    local targets=()
    if [[ "$1" == "all" ]]; then
        targets=("${PRESET_TARGETS[@]}")
    else
        targets=("$@")
    fi

    for t in "${targets[@]}"; do
        dispatch_target "$t"
    done

    echo
    log_info "全部完成,产物列表:"
    ls -lh "${OUT_DIR}"/ 2>/dev/null | grep "${BIN_NAME}" || true
}

main "$@"
