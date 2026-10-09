#!/usr/bin/env bash
#
# remote-agent-proxy 部署机控制脚本（与线上 /home/work/chuanqz/agent-assistant/control.sh 同源）
#
# 用法: ./control.sh {start|stop|restart|status|foreground|build|version|token|help}
#
# 部署目录只需: control.sh + remote-agent-proxy(二进制) + env.conf(可选，覆盖下方默认值)。
# build 子命令需在源码目录执行，会通过 -ldflags 注入版本 / commit / 构建时间，
# 运行态可用 `./control.sh version`、/api/status、remote_status 查看。

set -euo pipefail

# ──────────────────────────────────────────────
# 基本配置
# ──────────────────────────────────────────────
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$SCRIPT_DIR"

BINARY="${SCRIPT_DIR}/remote-agent-proxy"
PID_FILE="${SCRIPT_DIR}/remote-agent-proxy.pid"
LOG_FILE="${SCRIPT_DIR}/remote-agent-proxy.log"

# ──────────────────────────────────────────────
# 环境变量默认值（可通过环境变量或 env.conf 覆盖）
# ──────────────────────────────────────────────
: "${SHELL_API_PORT:=8099}"
: "${SHELL_API_TOKEN:=sk-123456}"
: "${SHELL_API_MAX_TIMEOUT:=900}"
: "${SHELL_API_MAX_OUTPUT:=1048576}"
: "${SHELL_API_RATE_LIMIT:=120}"
: "${SHELL_API_RATE_BURST:=60}"
: "${SHELL_API_DEFAULT_SHELL:=bash}"
: "${SHELL_API_FS_ROOT:=}"
: "${SHELL_API_DISABLED_TOOLS:=}"
: "${SHELL_API_SHUTDOWN_GRACE:=30}"

# 加载 env.conf 文件（如果存在，且变量允许被覆盖）
if [[ -f "${SCRIPT_DIR}/env.conf" ]]; then
    # shellcheck source=/dev/null
    set -a
    source "${SCRIPT_DIR}/env.conf"
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

# 导出服务端读取的全部环境变量
export_env() {
    export SHELL_API_PORT
    export SHELL_API_TOKEN
    export SHELL_API_MAX_TIMEOUT
    export SHELL_API_MAX_OUTPUT
    export SHELL_API_RATE_LIMIT
    export SHELL_API_RATE_BURST
    export SHELL_API_DEFAULT_SHELL
    export SHELL_API_FS_ROOT
    export SHELL_API_DISABLED_TOOLS
    export SHELL_API_SHUTDOWN_GRACE
    # 以下可选项仅在 env.conf / 环境中设置时导出，未设置则用服务端默认值
    local v
    for v in SHELL_API_LOG_LEVEL SHELL_API_MCP_HEARTBEAT SHELL_API_JOB_DIR \
             SHELL_API_DENY_COMMANDS SHELL_API_BLOCK_JUMP_HOST SHELL_API_READONLY; do
        if [[ -n "${!v:-}" ]]; then export "${v?}"; fi
    done
    # 不设置 TLS 相关变量，确保使用 HTTP
}

# ──────────────────────────────────────────────
# 命令: build  编译二进制（注入版本信息）
# ──────────────────────────────────────────────
cmd_build() {
    log_info "编译 ${BINARY} ..."
    if ! command -v go &>/dev/null; then
        log_error "未找到 Go 编译器，请先安装 Go 1.23+"
        exit 1
    fi
    local version commit build_time
    version="${VERSION:-$(grep -oE 'serverVersion = "[^"]+"' types.go 2>/dev/null | grep -oE '[0-9]+\.[0-9]+\.[0-9]+' || true)}"
    version="${version:-dev}"
    commit="$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
    build_time="$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
    go build -ldflags "-X main.serverVersion=${version} -X main.gitCommit=${commit} -X main.buildTime=${build_time}" \
        -o "${BINARY}" .
    log_info "编译完成: ${BINARY} ($("${BINARY}" --version))"
}

# ──────────────────────────────────────────────
# 命令: version  显示二进制版本
# ──────────────────────────────────────────────
cmd_version() {
    if [[ ! -x "${BINARY}" ]]; then
        log_error "二进制文件不存在: ${BINARY}"
        exit 1
    fi
    "${BINARY}" --version
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
        echo "  请保存此 Token，下次启动时可通过环境变量或 env.conf 文件指定"
    fi

    export_env

    echo ""
    log_info "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    log_info "Remote Shell API Server 启动中..."
    log_info "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo "  版本:      $("${BINARY}" --version 2>/dev/null || echo unknown)"
    echo "  端口:      ${SHELL_API_PORT}"
    echo "  协议:      HTTP（明文，未启用 TLS）"
    echo "  超时上限:  ${SHELL_API_MAX_TIMEOUT}s"
    echo "  输出上限:  ${SHELL_API_MAX_OUTPUT} 字节"
    echo "  限流:      ${SHELL_API_RATE_LIMIT}/分钟（突发 ${SHELL_API_RATE_BURST}，按客户端 IP）"
    echo "  默认 Shell: ${SHELL_API_DEFAULT_SHELL}"
    if [[ -n "${SHELL_API_FS_ROOT}" ]]; then
        echo "  文件沙箱:  ${SHELL_API_FS_ROOT}"
    else
        echo "  文件沙箱:  未限制"
    fi
    if [[ -n "${SHELL_API_DISABLED_TOOLS}" ]]; then
        echo "  禁用工具:  ${SHELL_API_DISABLED_TOOLS}"
    fi
    echo "  日志文件:  ${LOG_FILE}（JSON 结构化日志）"
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
        echo "    -H \"Authorization: Bearer \$SHELL_API_TOKEN\" \\"
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

    # SIGTERM 触发服务端优雅退出：停止接收新请求，正在执行的命令最多再跑
    # SHELL_API_SHUTDOWN_GRACE 秒，之后被杀。这里多等 5 秒再兜底 kill -9。
    local limit=$(( SHELL_API_SHUTDOWN_GRACE + 5 ))
    log_info "正在停止服务 (PID: ${pid})，最多等待 ${limit}s ..."
    kill "${pid}"

    local waited=0
    while kill -0 "${pid}" 2>/dev/null && [[ ${waited} -lt ${limit} ]]; do
        sleep 1
        ((waited++)) || true
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

    # 尝试健康检查（同时输出运行中进程报告的版本）
    local port="${SHELL_API_PORT:-8080}"
    if command -v curl &>/dev/null; then
        local body code
        body=$(curl -s -w '\n%{http_code}' "http://localhost:${port}/api/status" 2>/dev/null || true)
        code="${body##*$'\n'}"
        if [[ "${code}" == "200" ]]; then
            echo "  健康检查:  正常 (HTTP ${code})"
            local ver commit
            ver=$(echo "${body}" | grep -oE '"version":"[^"]*"' | cut -d'"' -f4 || true)
            commit=$(echo "${body}" | grep -oE '"commit":"[^"]*"' | cut -d'"' -f4 || true)
            echo "  运行版本:  ${ver:-unknown} (commit ${commit:-unknown})"
        else
            echo "  健康检查:  异常 (HTTP ${code:-无响应})"
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

    export_env

    log_info "前台启动 (HTTP 模式，Ctrl+C 优雅停止)..."
    exec "${BINARY}"
}

# ──────────────────────────────────────────────
# 命令: token  显示当前 Token
# ──────────────────────────────────────────────
cmd_token() {
    if [[ -z "${SHELL_API_TOKEN}" ]]; then
        log_error "SHELL_API_TOKEN 未设置"
        echo "  请设置环境变量: export SHELL_API_TOKEN=your-token"
        echo "  或在 env.conf 文件中添加: SHELL_API_TOKEN=your-token"
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
    version|--version|-v)
        cmd_version
        ;;
    token)
        cmd_token
        ;;
    help|--help|-h)
        echo "用法: $0 {start|stop|restart|status|foreground|build|version|token|help}"
        echo ""
        echo "  start       后台启动服务"
        echo "  stop        优雅停止服务（SIGTERM，超时后 kill -9）"
        echo "  restart     重启服务"
        echo "  status      查看服务运行状态与运行版本"
        echo "  foreground  前台运行（Ctrl+C 停止）"
        echo "  build       编译二进制文件（注入版本/commit/构建时间，需在源码目录）"
        echo "  version     显示二进制版本"
        echo "  token       显示当前 Token"
        echo "  help        显示此帮助"
        echo ""
        echo "环境变量（可在脚本内或 env.conf 文件中设置）:"
        echo "  SHELL_API_PORT            监听端口（默认: 8099）"
        echo "  SHELL_API_TOKEN           Bearer Token（必填，未设置时会自动生成）"
        echo "  SHELL_API_MAX_TIMEOUT     命令超时上限秒数（默认: 900）"
        echo "  SHELL_API_MAX_OUTPUT      输出字节上限（默认: 1048576）"
        echo "  SHELL_API_RATE_LIMIT      每客户端 IP 每分钟请求补充速率（默认: 120）"
        echo "  SHELL_API_RATE_BURST      每客户端 IP 突发容量（默认: 60）"
        echo "  SHELL_API_DEFAULT_SHELL   默认 shell（默认: bash）"
        echo "  SHELL_API_FS_ROOT         文件沙箱根目录，逗号分隔可多个（默认: 不限制）"
        echo "  SHELL_API_DISABLED_TOOLS  禁用的 MCP 工具，逗号分隔（默认: 不禁用）"
        echo "  SHELL_API_SHUTDOWN_GRACE  优雅退出时等待执行中命令的秒数（默认: 30）"
        echo "  SHELL_API_LOG_LEVEL       日志级别 debug|info|warn|error（默认: info；GET /mcp 仅 debug 记录）"
        echo "  SHELL_API_MCP_HEARTBEAT   MCP GET 流心跳间隔秒数，0 关闭（默认: 30）"
        echo "  SHELL_API_JOB_DIR         remote_spawn 后台任务日志目录（默认: \$TMPDIR/remote-agent-proxy-jobs）"
        echo "  SHELL_API_BLOCK_JUMP_HOST 设为 true 拦截 ssh/gssh/scp/sftp/sshpass 跳板命令（默认: 关闭）"
        echo "  SHELL_API_DENY_COMMANDS   自定义命令拦截 RE2 正则（默认: 空）"
        ;;
    *)
        echo "未知命令: ${1:-}"
        echo "用法: $0 {start|stop|restart|status|foreground|build|version|token|help}"
        exit 1
        ;;
esac
