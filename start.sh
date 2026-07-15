#!/usr/bin/env bash
#
# Remote Shell API Server - 启动脚本 (HTTP 模式，无 TLS)
#
# 用法:
#   ./start.sh start       # 后台启动
#   ./start.sh stop        # 停止服务
#   ./start.sh restart     # 重启服务
#   ./start.sh status      # 查看状态
#   ./start.sh foreground  # 前台运行（适用于 systemd / supervisor）
#
# 配置方式（优先级从高到低）:
#   1. 环境变量导出: export SHELL_API_TOKEN=xxx && ./start.sh start
#   2. 修改本脚本中的默认值
#   3. 在同目录下创建 .env 文件（可选，会被 source）

set -euo pipefail

# ──────────────────────────────────────────────
# 基本配置
# ──────────────────────────────────────────────
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$SCRIPT_DIR"

BINARY="${SCRIPT_DIR}/remote-shell-server"
PID_FILE="${SCRIPT_DIR}/remote-shell-server.pid"
LOG_FILE="${SCRIPT_DIR}/remote-shell-server.log"

# ──────────────────────────────────────────────
# 环境变量默认值（可通过环境变量或 .env 覆盖）
# ──────────────────────────────────────────────
: "${SHELL_API_PORT:=8080}"
: "${SHELL_API_TOKEN:=}"
: "${SHELL_API_MAX_TIMEOUT:=300}"
: "${SHELL_API_MAX_OUTPUT:=1048576}"
: "${SHELL_API_RATE_LIMIT:=60}"
: "${SHELL_API_DEFAULT_SHELL:=bash}"
: "${SHELL_API_FS_ROOT:=}"
: "${SHELL_API_DISABLED_TOOLS:=}"

# 加载 .env 文件（如果存在，且变量允许被覆盖）
if [[ -f "${SCRIPT_DIR}/.env" ]]; then
    # shellcheck source=/dev/null
    set -a
    source "${SCRIPT_DIR}/.env"
    set +a
fi

# ──────────────────────────────────────────────
# 辅助函数
# ──────────────────────────────────────────────

# 生成随机 Token
generate_token() {
    # 优先用 openssl，其次用 /dev/urandom，最后用日期兜底
    if command -v openssl &>/dev/null; then
        openssl rand -hex 32
    elif [[ -r /dev/urandom ]]; then
        head -c 32 /dev/urandom | xxd -p -c 256 2>/dev/null || od -A n -t x1 -N 32 /dev/urandom | tr -d ' \n'
    else
        echo "auto-generated-token-$(date +%s)-$(shuf -i 10000-99999 -n 1 2>/dev/null || echo $$)"
    fi
}

# 颜色输出
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
NC='\033[0m' # No Color

log_info()  { echo -e "${GREEN}[INFO]${NC}  $(date '+%Y-%m-%d %H:%M:%S') $*"; }
log_warn()  { echo -e "${YELLOW}[WARN]${NC}  $(date '+%Y-%m-%d %H:%M:%S') $*"; }
log_error() { echo -e "${RED}[ERROR]${NC} $(date '+%Y-%m-%d %H:%M:%S') $*"; }

# ──────────────────────────────────────────────
# 命令: build  编译二进制
# ──────────────────────────────────────────────
cmd_build() {
    log_info "编译 ${BINARY} ..."
    if ! command -v go &>/dev/null; then
        log_error "未找到 Go 编译器，请先安装 Go 1.23+"
        exit 1
    fi
    go build -o "${BINARY}" .
    log_info "编译完成: ${BINARY}"
}

# ──────────────────────────────────────────────
# 命令: start  后台启动
# ──────────────────────────────────────────────
cmd_start() {
    # 检查是否已在运行
    if [[ -f "${PID_FILE}" ]]; then
        local pid
        pid=$(cat "${PID_FILE}")
        if kill -0 "${pid}" 2>/dev/null; then
            log_warn "服务已在运行中 (PID: ${pid})"
            return 0
        else
            log_warn "PID 文件存在但进程不存在，清理后重新启动"
            rm -f "${PID_FILE}"
        fi
    fi

    # 检查二进制文件
    if [[ ! -f "${BINARY}" ]]; then
        log_info "二进制文件不存在，自动编译..."
        cmd_build
    fi

    # 自动生成 Token（如果未设置）
    if [[ -z "${SHELL_API_TOKEN}" ]]; then
        SHELL_API_TOKEN=$(generate_token)
        log_warn "未设置 SHELL_API_TOKEN，已自动生成随机 Token"
        echo "  Token: ${SHELL_API_TOKEN}"
        echo "  请保存此 Token，下次启动时可通过环境变量或 .env 文件指定"
    fi

    # 导出环境变量
    export SHELL_API_PORT
    export SHELL_API_TOKEN
    export SHELL_API_MAX_TIMEOUT
    export SHELL_API_MAX_OUTPUT
    export SHELL_API_RATE_LIMIT
    export SHELL_API_DEFAULT_SHELL
    export SHELL_API_FS_ROOT
    export SHELL_API_DISABLED_TOOLS
    # 不设置 TLS 相关变量，确保使用 HTTP

    echo ""
    log_info "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    log_info "Remote Shell API Server 启动中..."
    log_info "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo "  端口:      ${SHELL_API_PORT}"
    echo "  协议:      HTTP（明文，未启用 TLS）"
    echo "  超时上限:  ${SHELL_API_MAX_TIMEOUT}s"
    echo "  输出上限:  ${SHELL_API_MAX_OUTPUT} 字节"
    echo "  限流:      ${SHELL_API_RATE_LIMIT}/分钟"
    echo "  默认 Shell: ${SHELL_API_DEFAULT_SHELL}"
    if [[ -n "${SHELL_API_FS_ROOT}" ]]; then
        echo "  文件沙箱:  ${SHELL_API_FS_ROOT}"
    else
        echo "  文件沙箱:  未限制"
    fi
    if [[ -n "${SHELL_API_DISABLED_TOOLS}" ]]; then
        echo "  禁用工具:  ${SHELL_API_DISABLED_TOOLS}"
    fi
    echo "  日志文件:  ${LOG_FILE}"
    echo ""

    # 后台启动
    nohup "${BINARY}" >> "${LOG_FILE}" 2>&1 &
    local pid=$!
    echo "${pid}" > "${PID_FILE}"

    # 等待一小段时间检查是否启动成功
    sleep 1
    if kill -0 "${pid}" 2>/dev/null; then
        log_info "服务启动成功 (PID: ${pid})"
        echo ""
        echo -e "${CYAN}快速验证:${NC}"
        echo "  curl http://localhost:${SHELL_API_PORT}/api/status"
        echo ""
        echo -e "${CYAN}执行命令:${NC}"
        echo "  curl -X POST http://localhost:${SHELL_API_PORT}/api/execute \\"
        echo "    -H \"Authorization: Bearer ${SHELL_API_TOKEN}\" \\"
        echo "    -H \"Content-Type: application/json\" \\"
        echo "    -d '{\"command\":\"uname -a\"}'"
        echo ""
        echo -e "${CYAN}MCP 端点:${NC}"
        echo "  http://localhost:${SHELL_API_PORT}/mcp"
    else
        log_error "服务启动失败，请查看日志: ${LOG_FILE}"
        rm -f "${PID_FILE}"
        exit 1
    fi
}

# ──────────────────────────────────────────────
# 命令: stop  停止后台服务
# ──────────────────────────────────────────────
cmd_stop() {
    if [[ ! -f "${PID_FILE}" ]]; then
        log_warn "服务未在运行（PID 文件不存在）"
        return 0
    fi

    local pid
    pid=$(cat "${PID_FILE}")

    if ! kill -0 "${pid}" 2>/dev/null; then
        log_warn "进程 ${pid} 不存在，清理 PID 文件"
        rm -f "${PID_FILE}"
        return 0
    fi

    log_info "正在停止服务 (PID: ${pid}) ..."
    kill "${pid}"

    # 等待进程退出（最多等 10 秒）
    local waited=0
    while kill -0 "${pid}" 2>/dev/null && [[ ${waited} -lt 10 ]]; do
        sleep 1
        ((waited++))
    done

    if kill -0 "${pid}" 2>/dev/null; then
        log_warn "进程未响应，强制终止..."
        kill -9 "${pid}" 2>/dev/null || true
        sleep 1
    fi

    rm -f "${PID_FILE}"
    log_info "服务已停止"
}

# ──────────────────────────────────────────────
# 命令: restart  重启服务
# ──────────────────────────────────────────────
cmd_restart() {
    cmd_stop
    sleep 1
    cmd_start
}

# ──────────────────────────────────────────────
# 命令: status  查看状态
# ──────────────────────────────────────────────
cmd_status() {
    if [[ ! -f "${PID_FILE}" ]]; then
        echo -e "${YELLOW}状态: 未运行${NC}"
        return 1
    fi

    local pid
    pid=$(cat "${PID_FILE}")

    if ! kill -0 "${pid}" 2>/dev/null; then
        echo -e "${RED}状态: PID 文件存在但进程不存在（可能异常退出）${NC}"
        rm -f "${PID_FILE}"
        return 1
    fi

    echo -e "${GREEN}状态: 运行中${NC}"
    echo "  PID:       ${pid}"

    # 尝试获取端口
    if command -v lsof &>/dev/null; then
        local port
        port=$(lsof -p "${pid}" -i TCP -s TCP:LISTEN -Fn 2>/dev/null | grep -o '[0-9]*$' | head -1 || true)
        if [[ -n "${port}" ]]; then
            echo "  端口:      ${port}"
        fi
    fi

    # 尝试健康检查
    local port="${SHELL_API_PORT:-8080}"
    if command -v curl &>/dev/null; then
        local resp
        resp=$(curl -s -o /dev/null -w "%{http_code}" "http://localhost:${port}/api/status" 2>/dev/null || true)
        if [[ "${resp}" == "200" ]]; then
            echo "  健康检查:  正常 (HTTP ${resp})"
        else
            echo "  健康检查:  异常 (HTTP ${resp:-无响应})"
        fi
    fi

    echo "  日志:      ${LOG_FILE}"
}

# ──────────────────────────────────────────────
# 命令: foreground  前台运行
# ──────────────────────────────────────────────
cmd_foreground() {
    # 检查二进制
    if [[ ! -f "${BINARY}" ]]; then
        log_info "二进制文件不存在，自动编译..."
        cmd_build
    fi

    # 自动生成 Token
    if [[ -z "${SHELL_API_TOKEN}" ]]; then
        SHELL_API_TOKEN=$(generate_token)
        log_warn "未设置 SHELL_API_TOKEN，已自动生成随机 Token: ${SHELL_API_TOKEN}"
    fi

    export SHELL_API_PORT
    export SHELL_API_TOKEN
    export SHELL_API_MAX_TIMEOUT
    export SHELL_API_MAX_OUTPUT
    export SHELL_API_RATE_LIMIT
    export SHELL_API_DEFAULT_SHELL
    export SHELL_API_FS_ROOT
    export SHELL_API_DISABLED_TOOLS

    log_info "前台启动 (HTTP 模式，Ctrl+C 停止)..."
    exec "${BINARY}"
}

# ──────────────────────────────────────────────
# 命令: token  显示当前 Token
# ──────────────────────────────────────────────
cmd_token() {
    if [[ -z "${SHELL_API_TOKEN}" ]]; then
        log_error "SHELL_API_TOKEN 未设置"
        echo "  请设置环境变量: export SHELL_API_TOKEN=your-token"
        echo "  或在 .env 文件中添加: SHELL_API_TOKEN=your-token"
        exit 1
    fi
    echo "当前 Token: ${SHELL_API_TOKEN}"
}

# ──────────────────────────────────────────────
# 主入口
# ──────────────────────────────────────────────
case "${1:-}" in
    start)
        cmd_start
        ;;
    stop)
        cmd_stop
        ;;
    restart)
        cmd_restart
        ;;
    status)
        cmd_status
        ;;
    foreground|fg|run)
        cmd_foreground
        ;;
    build)
        cmd_build
        ;;
    token)
        cmd_token
        ;;
    help|--help|-h)
        echo "用法: $0 {start|stop|restart|status|foreground|build|token|help}"
        echo ""
        echo "  start       后台启动服务"
        echo "  stop        停止服务"
        echo "  restart     重启服务"
        echo "  status      查看服务运行状态"
        echo "  foreground  前台运行（Ctrl+C 停止）"
        echo "  build       仅编译二进制文件"
        echo "  token       显示当前 Token"
        echo "  help        显示此帮助"
        echo ""
        echo "环境变量（可在脚本内或 .env 文件中设置）:"
        echo "  SHELL_API_PORT         监听端口（默认: 8080）"
        echo "  SHELL_API_TOKEN        Bearer Token（必填，未设置时会自动生成）"
        echo "  SHELL_API_MAX_TIMEOUT  命令超时上限秒数（默认: 300）"
        echo "  SHELL_API_MAX_OUTPUT   输出字节上限（默认: 1048576）"
        echo "  SHELL_API_RATE_LIMIT   每 IP 每分钟请求上限（默认: 60）"
        echo "  SHELL_API_DEFAULT_SHELL 默认 shell（默认: bash）"
        echo "  SHELL_API_FS_ROOT      文件沙箱根目录，逗号分隔可多个（默认: 不限制）"
        echo "  SHELL_API_DISABLED_TOOLS 禁用的 MCP 工具，逗号分隔（默认: 不禁用）"
        ;;
    *)
        echo "未知命令: ${1:-}"
        echo "用法: $0 {start|stop|restart|status|foreground|build|token|help}"
        exit 1
        ;;
esac
