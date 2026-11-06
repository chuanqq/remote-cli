#!/usr/bin/env bash
#
# remote-agent-proxy 部署机控制脚本（与线上 /home/work/chuanqz/agent-assistant/control.sh 同源）
#
# 用法: ./control.sh {start|stop|restart|status|foreground|build|version|token|mcp-prompt|help}
#
# 部署目录只需: control.sh + remote-agent-proxy(二进制) + env.conf(可选，覆盖下方默认值)。
# build 子命令需在源码目录执行，会通过 -ldflags 注入版本 / commit / 构建时间，
# 运行态可用 `./control.sh version`、MCP 客户端或 remote_status 查看。

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
: "${SHELL_API_TOKEN_HASH:=}"
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

# 空串兜底保证 set -u 下后续引用安全；未配置时由 cmd_start/cmd_foreground
# 生成新的随机 Token。
: "${SHELL_API_TOKEN:=}"

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
    # 鉴权二选一：导出已设置的那个（都设置时启动检查会提前报错）
    if [[ -n "${SHELL_API_TOKEN:-}" ]]; then
        export SHELL_API_TOKEN
    fi
    if [[ -n "${SHELL_API_TOKEN_HASH:-}" ]]; then
        export SHELL_API_TOKEN_HASH
    fi
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
             SHELL_API_DENY_COMMANDS SHELL_API_BLOCK_JUMP_HOST SHELL_API_READONLY \
             SHELL_API_CALL_BUDGET SHELL_API_TRUSTED_PROXIES; do
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
    # 鉴权互斥检查：服务端会 fatal，这里提前报错提示更友好
    if [[ -n "${SHELL_API_TOKEN:-}" && -n "${SHELL_API_TOKEN_HASH}" ]]; then
        log_error "SHELL_API_TOKEN 与 SHELL_API_TOKEN_HASH 互斥，只能设置其中一个"
        exit 1
    fi

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

    # 自动生成 Token（如果 Token 与 Token Hash 都未设置；哈希模式无法自动
    # 生成——明文必须由用户持有并配置到客户端）
    if [[ -z "${SHELL_API_TOKEN}" && -z "${SHELL_API_TOKEN_HASH}" ]]; then
        SHELL_API_TOKEN=$(generate_token)
        log_warn "未设置 SHELL_API_TOKEN / SHELL_API_TOKEN_HASH，已自动生成随机 Token"
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
    if [[ -n "${SHELL_API_TOKEN_HASH}" ]]; then
        echo -e "  鉴权:      ${CYAN}哈希模式（盐化 SHA-256，服务端不存明文 Token）${NC}"
    else
        echo "  鉴权:      明文 Token 模式"
    fi
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
        echo "  MCP 端点: http://localhost:${SHELL_API_PORT}/mcp"
        echo ""
        echo "  使用 MCP 客户端连接并调用工具"
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

    # 实际监听端口。lsof 的 -p 与 -i 默认取并集，必须加 -a 取交集，否则会
    # 列出机器上所有监听进程（-P -n 禁用服务名/主机名解析）。探测不到时
    # 回退到配置值，MCP 端点检查也用它。
    local port=""
    if command -v lsof &>/dev/null; then
        port=$(lsof -a -P -n -p "${pid}" -i TCP -s TCP:LISTEN -Fn 2>/dev/null \
               | sed -nE 's/^n.*:([0-9]+)$/\1/p' | head -1 || true)
    fi
    [[ -n "${port}" ]] || port="${SHELL_API_PORT:-unknown}"
    echo "  端口:      ${port}"

    # MCP 端点探测（未携带凭据时应返回 401，说明服务已接受连接）
    if command -v curl &>/dev/null; then
        local resp
        resp=$(curl -s --max-time 3 -o /dev/null -w "%{http_code}" "http://localhost:${port}/mcp" 2>/dev/null || true)
        if [[ "${resp}" == "401" ]]; then
            echo "  MCP 检查:   正常（HTTP ${resp}，端点要求鉴权）"
        else
            echo "  MCP 检查:   异常（HTTP ${resp:-无响应}）"
        fi
    fi

    echo "  日志:      ${LOG_FILE}"
}

# ──────────────────────────────────────────────
# 命令: foreground  前台运行
# ──────────────────────────────────────────────
cmd_foreground() {
    # 鉴权互斥检查（与 cmd_start 一致）
    if [[ -n "${SHELL_API_TOKEN:-}" && -n "${SHELL_API_TOKEN_HASH}" ]]; then
        log_error "SHELL_API_TOKEN 与 SHELL_API_TOKEN_HASH 互斥，只能设置其中一个"
        exit 1
    fi

    # 检查二进制
    if [[ ! -f "${BINARY}" ]]; then
        log_info "二进制文件不存在，自动编译..."
        cmd_build
    fi

    # 自动生成 Token（两者都未设置时；哈希模式无法自动生成）
    if [[ -z "${SHELL_API_TOKEN}" && -z "${SHELL_API_TOKEN_HASH}" ]]; then
        SHELL_API_TOKEN=$(generate_token)
        log_warn "未设置 SHELL_API_TOKEN / SHELL_API_TOKEN_HASH，已自动生成随机 Token: ${SHELL_API_TOKEN}"
    fi

    export_env

    log_info "前台启动 (HTTP 模式，Ctrl+C 优雅停止)..."
    exec "${BINARY}"
}

# ──────────────────────────────────────────────
# 命令: mcp-prompt  输出给 agent 客户端的 MCP 添加指令
# ──────────────────────────────────────────────
json_quote() {
    local value=$1
    value=${value//\\/\\\\}
    value=${value//\"/\\\"}
    printf '"%s"' "$value"
}

cmd_mcp_prompt() {
    local url="${1:-${MCP_PUBLIC_URL:-}}"
    local name="${2:-${MCP_SERVER_NAME:-remote-shell}}"
    local token="${CHECK_TOKEN:-${SHELL_API_TOKEN:-}}"
    local auth="Bearer <CLIENT_TOKEN>" authority
    if [[ $# -gt 2 || -z "$url" ]]; then
        log_error "用法: $0 mcp-prompt <客户端可达的 MCP URL> [服务名]，或配置 MCP_PUBLIC_URL"
        return 1
    fi
    authority=${url#*://}
    authority=${authority%%/*}
    authority=${authority%%\?*}
    if [[ "$url" != http://* && "$url" != https://* ]] ||
       [[ "$url" =~ [[:space:][:cntrl:]] || "$url" == *'#'* || -z "$authority" || "$authority" == *'@'* ]]; then
        log_error "MCP URL 必须是无空白、片段或内嵌凭据的完整 HTTP/HTTPS 地址"
        return 1
    fi
    if [[ ! "$name" =~ ^[a-zA-Z0-9_-]+$ ]] || [[ "$token" =~ [[:cntrl:]] ]]; then
        log_error "服务名只能包含字母、数字、下划线或连字符；Token 不能包含控制字符"
        return 1
    fi
    if [[ -n "$token" ]]; then
        auth="Bearer $token"
    fi

    printf '请在当前 agent 客户端中添加以下 MCP 服务，传输方式为 Streamable HTTP。\n'
    printf '请按当前客户端支持的格式配置，并优先保存到客户端用户级配置，不要提交到代码仓库。\n'
    if [[ -z "$token" ]]; then
        printf '添加前请让我通过交互输入提供客户端明文 Token，替换 <CLIENT_TOKEN> 后再连接。\n'
        printf '服务端仅存 Token Hash 时无法据此生成客户端凭据，请使用我提供的明文 Token。\n'
    else
        printf '以下配置包含客户端鉴权凭据，请仅用于本机客户端配置，不要公开分享。\n'
    fi
    printf '\n```json\n{\n  "mcpServers": {\n    %s: {\n      "type": "http",\n      "url": %s,\n      "headers": {"Authorization": %s}\n    }\n  }\n}\n```\n' \
        "$(json_quote "$name")" "$(json_quote "$url")" "$(json_quote "$auth")"
    printf '\n如客户端使用其他配置结构，请使用等价的 HTTP URL 和 Authorization 请求头配置。\n'
    printf '完成后通过 MCP initialize 和 tools/list 验证连接，并报告添加结果。\n'
}

# ──────────────────────────────────────────────
# 命令: token  显示当前 Token
# ──────────────────────────────────────────────
cmd_token() {
    if [[ -n "${SHELL_API_TOKEN}" ]]; then
        echo "当前 Token: ${SHELL_API_TOKEN}"
        return 0
    fi
    if [[ -n "${SHELL_API_TOKEN_HASH}" ]]; then
        echo "当前为哈希模式（服务端只存盐化 SHA-256，无明文可显示）"
        echo "  Token Hash: ${SHELL_API_TOKEN_HASH}"
        echo "  明文 Token 仅客户端持有；重置时生成新哈希更新 SHELL_API_TOKEN_HASH 即可"
        return 0
    fi
    log_error "SHELL_API_TOKEN / SHELL_API_TOKEN_HASH 均未设置"
    echo "  请设置环境变量: export SHELL_API_TOKEN=your-token"
    echo "  或在 env.conf 文件中添加: SHELL_API_TOKEN=your-token"
    echo "  或使用哈希模式: printf '%s' 'your-token' | ${BINARY} hash-token"
    exit 1
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
    mcp-prompt)
        cmd_mcp_prompt "${@:2}"
        ;;
    help|--help|-h)
        echo "用法: $0 {start|stop|restart|status|foreground|build|version|token|mcp-prompt|help}"
        echo ""
        echo "  start       后台启动服务"
        echo "  stop        优雅停止服务（SIGTERM，超时后 kill -9）"
        echo "  restart     重启服务"
        echo "  status      查看进程状态与 MCP 端点可达性"
        echo "  foreground  前台运行（Ctrl+C 停止）"
        echo "  build       编译二进制文件（注入版本/commit/构建时间，需在源码目录）"
        echo "  version     显示二进制版本"
        echo "  token       显示当前 Token"
        echo "  mcp-prompt [URL] [服务名]  输出可复制到 agent 终端的 MCP 添加指令"
        echo "  help        显示此帮助"
        echo ""
        echo "环境变量（可在脚本内或 env.conf 文件中设置）:"
        echo "  SHELL_API_PORT            监听端口（默认: 8099）"
        echo "  SHELL_API_TOKEN           Bearer Token（必填，未设置时会自动生成）"
        echo "  SHELL_API_TOKEN_HASH      Token 的盐化 SHA-256（sha256:盐:哈希），与 SHELL_API_TOKEN 二选一，"
        echo "                            服务端不落明文。生成: printf '%s' 'your-token' | ${BINARY} hash-token"
        echo "  SHELL_API_MAX_TIMEOUT     命令超时上限秒数（默认: 900）"
        echo "  SHELL_API_MAX_OUTPUT      输出字节上限（默认: 1048576）"
        echo "  SHELL_API_RATE_LIMIT      每客户端 IP 每分钟请求补充速率（默认: 120）"
        echo "  SHELL_API_RATE_BURST      每客户端 IP 突发容量（默认: 60）"
        echo "  SHELL_API_TRUSTED_PROXIES 可信代理 IP/CIDR，逗号分隔（默认: 不信任转发头）"
        echo "  SHELL_API_DEFAULT_SHELL   默认 shell（默认: bash）"
        echo "  SHELL_API_FS_ROOT         文件沙箱根目录，逗号分隔可多个（默认: 不限制）"
        echo "  SHELL_API_DISABLED_TOOLS  禁用的 MCP 工具，逗号分隔（默认: 不禁用）"
        echo "  SHELL_API_SHUTDOWN_GRACE  优雅退出时等待执行中命令的秒数（默认: 30）"
        echo "  SHELL_API_CALL_BUDGET     单次 MCP 调用最长阻塞秒数，需小于客户端请求超时（默认: 50）；"
        echo "                            超出的命令不杀，返回 execution_id 续取，等待返回 wait_id 续等"
        echo "  SHELL_API_LOG_LEVEL       日志级别 debug|info|warn|error（默认: info；GET /mcp 仅 debug 记录）"
        echo "  SHELL_API_MCP_HEARTBEAT   MCP GET 流心跳间隔秒数，0 关闭（默认: 30）"
        echo "  SHELL_API_JOB_DIR         remote_spawn 后台任务日志目录（默认: \$TMPDIR/remote-agent-proxy-jobs）"
        echo "  SHELL_API_BLOCK_JUMP_HOST 设为 true 拦截 ssh/gssh/scp/sftp/sshpass 跳板命令（默认: 关闭）"
        echo "  SHELL_API_DENY_COMMANDS   自定义命令拦截 RE2 正则（默认: 空）"
        echo ""
        echo "mcp-prompt 专用参数（不改变服务端配置）:"
        echo "  MCP_PUBLIC_URL   客户端可达的完整 MCP URL，可由命令参数覆盖"
        echo "  MCP_SERVER_NAME  客户端中的服务名（默认: remote-shell）"
        echo "  CHECK_TOKEN      客户端明文 Token，优先于 SHELL_API_TOKEN；哈希模式可用它生成完整指令"
        echo "  未提供明文 Token 时输出占位符，由 agent 在添加前交互获取；不会输出 Token Hash"
        ;;
    *)
        echo "未知命令: ${1:-}"
        echo "用法: $0 {start|stop|restart|status|foreground|build|version|token|mcp-prompt|help}"
        exit 1
        ;;
esac
