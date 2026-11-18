#!/usr/bin/env bash
#
# Remote Shell API Server - 启动脚本 (HTTP 模式，无 TLS)
#
# 用法:
#   ./start.sh start        # 后台启动
#   ./start.sh start-ro     # 后台启动（只读模式，一键）
#   ./start.sh stop         # 停止服务
#   ./start.sh restart      # 重启服务
#   ./start.sh status       # 查看状态
#   ./start.sh foreground   # 前台运行（适用于 systemd / supervisor）
#
# 配置方式（优先级从高到低）:
#   1. 环境变量导出: export SHELL_API_TOKEN=xxx && ./start.sh start
#   2. 修改本脚本中的默认值
#   3. 在同目录下创建 .env 文件（可选，会被 source）
#
# 只读模式（SHELL_API_READONLY=true）优先级最高：一旦开启，任何其他配置
# （包括空的 SHELL_API_DISABLED_TOOLS）都无法重新打开写能力。

set -euo pipefail

# ──────────────────────────────────────────────
# 基本配置
# ──────────────────────────────────────────────
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$SCRIPT_DIR"

# Prefer the 2.0 primary binary; fall back to the 1.x compatibility copy.
BINARY="${SCRIPT_DIR}/agentic-remotectl"
if [[ ! -x "${BINARY}" && -x "${SCRIPT_DIR}/remote-agent-proxy" ]]; then
    BINARY="${SCRIPT_DIR}/remote-agent-proxy"
fi
# PID/log names stay 1.x for upgrade-script compatibility (design 07 §3.2).
PID_FILE="${SCRIPT_DIR}/remote-agent-proxy.pid"
LOG_FILE="${SCRIPT_DIR}/remote-agent-proxy.log"

# Script/binary must be the same release (D21).
SCRIPT_VERSION="2.0.0"

# ──────────────────────────────────────────────
# 环境变量默认值（可通过环境变量或 .env 覆盖）
# ──────────────────────────────────────────────
: "${SHELL_API_PORT:=8080}"
: "${SHELL_API_TOKEN:=}"
: "${SHELL_API_TOKEN_HASH:=}"
: "${SHELL_API_MAX_TIMEOUT:=300}"
: "${SHELL_API_MAX_OUTPUT:=1048576}"
: "${SHELL_API_RATE_LIMIT:=120}"
: "${SHELL_API_RATE_BURST:=60}"
: "${SHELL_API_TRUSTED_PROXIES:=}"
: "${SHELL_API_DEFAULT_SHELL:=bash}"
: "${SHELL_API_FS_ROOT:=}"
: "${SHELL_API_DISABLED_TOOLS:=}"
: "${SHELL_API_READONLY:=}"
# Token used when neither SHELL_API_TOKEN nor SHELL_API_TOKEN_HASH is configured.
# Fixed value, no random generation.
: "${SHELL_API_DEFAULT_TOKEN:=remote-agent-proxy-default-token}"

# 加载 .env 文件（如果存在，且变量允许被覆盖）
if [[ -f "${SCRIPT_DIR}/.env" ]]; then
    # shellcheck source=/dev/null
    set -a
    source "${SCRIPT_DIR}/.env"
    set +a
fi

# Effective listen/port: REMOTECTL_* wins over SHELL_API_* (matches the Go
# config dual-read).
EFFECTIVE_PORT="${REMOTECTL_PORT:-${SHELL_API_PORT}}"
EFFECTIVE_LISTEN="${REMOTECTL_LISTEN:-${SHELL_API_LISTEN:-:${EFFECTIVE_PORT}}}"
EFFECTIVE_MODE="${REMOTECTL_MODE:-standalone}"
# Effective HTTP credential: REMOTECTL_* wins over the legacy SHELL_API_*.
EFFECTIVE_TOKEN="${REMOTECTL_TOKEN:-${SHELL_API_TOKEN:-}}"
EFFECTIVE_TOKEN_HASH="${REMOTECTL_TOKEN_HASH:-${SHELL_API_TOKEN_HASH:-}}"

# 只读模式判定：脚本层只用于展示与提示，真正的强制在服务端。
is_readonly() {
    case "$(echo "${REMOTECTL_READONLY:-${SHELL_API_READONLY:-}}" | tr '[:upper:]' '[:lower:]' | tr -d '[:space:]')" in
        1|true|yes|on|enabled) return 0 ;;
        *) return 1 ;;
    esac
}

# ──────────────────────────────────────────────
# 辅助函数
# ──────────────────────────────────────────────

# 颜色输出
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
NC='\033[0m' # No Color

log_info()  { echo -e "${GREEN}[INFO]${NC}  $(date '+%Y-%m-%d %H:%M:%S') $*"; }
log_warn()  { echo -e "${YELLOW}[WARN]${NC}  $(date '+%Y-%m-%d %H:%M:%S') $*"; }
log_error() { echo -e "${RED}[ERROR]${NC} $(date '+%Y-%m-%d %H:%M:%S') $*"; }

# check_binary_version refuses to launch when the script and the binary come
# from different releases (D21).
check_binary_version() {
    local out version
    out=$("${BINARY}" --version 2>/dev/null) || {
        log_error "binary cannot run: ${BINARY} (${out})"
        exit 1
    }
    version=$(printf '%s\n' "${out}" | sed -nE 's/^[^ ]+ ([0-9]+\.[0-9]+\.[0-9]+).*/\1/p')
    if [[ "${version}" != "${SCRIPT_VERSION}" ]]; then
        log_error "script/binary version mismatch: script ${SCRIPT_VERSION}, binary ${version:-unknown}"
        echo "  upgrade every file in this directory together (binary + start.sh + .env)"
        exit 1
    fi
}

# listen_host extracts the host part of host:port, including [::1]:port.
listen_host() {
    local listen="$1" host
    if [[ "$listen" == \[*\]:* ]]; then
        host="${listen%%]:*}"
        host="${host#[}"
    else
        host="${listen%:*}"
    fi
    printf '%s' "${host}"
}

# is_loopback_listen is conservative: an empty host, a wildcard address and
# any hostname other than localhost are NOT loopback. This mirrors the server
# guard in config.go (D6).
is_loopback_listen() {
    local host
    host=$(listen_host "$1" | tr '[:upper:]' '[:lower:]')
    case "${host}" in
        localhost) return 0 ;;
        127.*) return 0 ;;
        ::1) return 0 ;;
        *) return 1 ;;
    esac
}

# require_secure_credential enforces D6 before launch: the built-in default
# token is public, so it may only be used on a loopback listener.
require_secure_credential() {
    # A slaver has no public HTTP listener, so D6 does not apply to it.
    if [[ "${EFFECTIVE_MODE}" == "slaver" ]]; then
        return 0
    fi
    if [[ -n "${EFFECTIVE_TOKEN}" || -n "${EFFECTIVE_TOKEN_HASH}" ]]; then
        return 0
    fi
    if is_loopback_listen "${EFFECTIVE_LISTEN}"; then
        return 0
    fi
    log_error "no explicit credential configured and listen address ${EFFECTIVE_LISTEN} is not loopback"
    echo "  Set REMOTECTL_TOKEN / SHELL_API_TOKEN (or *_TOKEN_HASH), or restrict the listener:"
    echo "  e.g. REMOTECTL_LISTEN=127.0.0.1:${EFFECTIVE_PORT} $0 start"
    exit 1
}

# ──────────────────────────────────────────────
# 命令: build  编译二进制
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
    check_binary_version

    # 鉴权互斥检查：服务端会 fatal，这里提前报错提示更友好
    if [[ -n "${EFFECTIVE_TOKEN}" && -n "${EFFECTIVE_TOKEN_HASH}" ]]; then
        log_error "token 与 token hash 互斥（REMOTECTL_TOKEN/REMOTECTL_TOKEN_HASH 或对应 SHELL_API_*），只能设置其中一个"
        exit 1
    fi

    # D6: refuse a public listener without an explicit credential.
    require_secure_credential

    # No random generation: fall back to the fixed default token when the
    # operator configured neither a token nor a token hash.
    if [[ -z "${EFFECTIVE_TOKEN}" && -z "${EFFECTIVE_TOKEN_HASH}" ]]; then
        SHELL_API_TOKEN="${SHELL_API_DEFAULT_TOKEN}"
        log_warn "No explicit credential configured; using the built-in default token (loopback listeners only)"
        echo "  Configure SHELL_API_TOKEN / REMOTECTL_TOKEN (or *_TOKEN_HASH) in .env for network exposure"
    fi

    # 导出环境变量
    export SHELL_API_PORT
    if [[ -n "${REMOTECTL_PORT:-}" ]]; then export REMOTECTL_PORT; fi
    if [[ -n "${REMOTECTL_LISTEN:-}" ]]; then export REMOTECTL_LISTEN; fi
    export SHELL_API_TOKEN
    export SHELL_API_TOKEN_HASH
    export SHELL_API_MAX_TIMEOUT
    export SHELL_API_MAX_OUTPUT
    export SHELL_API_RATE_LIMIT
    export SHELL_API_RATE_BURST
    export SHELL_API_TRUSTED_PROXIES
    export SHELL_API_DEFAULT_SHELL
    export SHELL_API_FS_ROOT
    export SHELL_API_DISABLED_TOOLS
    export SHELL_API_READONLY
    local v
    for v in REMOTECTL_MODE REMOTECTL_CONTROL_LISTEN REMOTECTL_MASTER_URL \
             REMOTECTL_CLUSTER_TOKEN REMOTECTL_CLUSTER_TOKEN_FILE REMOTECTL_CLUSTER_TOKEN_HASH \
             REMOTECTL_MASTER_LOCAL REMOTECTL_NODE_APPROVAL REMOTECTL_NODE_ALLOWLIST \
             REMOTECTL_ENABLE_NODE_ADMIN_TOOLS REMOTECTL_NODE_NAME REMOTECTL_NODE_ID \
             REMOTECTL_STATE_DIR REMOTECTL_HEARTBEAT_SEC REMOTECTL_LEASE_TTL_SEC \
             REMOTECTL_STALE_AFTER_SEC REMOTECTL_OFFLINE_AFTER_SEC REMOTECTL_READONLY; do
        if [[ -n "${!v:-}" ]]; then export "${v?}"; fi
    done
    # 不设置 TLS 相关变量，确保使用 HTTP

    echo ""
    log_info "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    if is_readonly; then
        log_info "Remote Shell API Server 启动中... [只读模式]"
    else
        log_info "Remote Shell API Server 启动中..."
    fi
    log_info "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo "  监听:      ${EFFECTIVE_LISTEN}"
    echo "  端口:      ${EFFECTIVE_PORT}"
    echo "  协议:      HTTP（明文，未启用 TLS）"
    if [[ -n "${SHELL_API_TOKEN_HASH}" ]]; then
        echo -e "  鉴权:      ${CYAN}哈希模式（盐化 SHA-256，服务端不存明文 Token）${NC}"
    else
        echo "  鉴权:      明文 Token 模式"
    fi
    if is_readonly; then
        echo -e "  运行模式:  ${CYAN}只读（READ-ONLY，最高优先级）${NC}"
        echo "             仅保留 15 个只读工具；命令执行 / 写 / 编辑 / 删除 / 移动 / 复制 / 建目录 全部关闭"
        echo "             仅保留只读 MCP 工具"
    else
        echo -e "  运行模式:  ${YELLOW}读写（完整权限，等同交互式 shell）${NC}"
    fi
    echo "  超时上限:  ${SHELL_API_MAX_TIMEOUT}s"
    echo "  输出上限:  ${SHELL_API_MAX_OUTPUT} 字节"
    echo "  限流:      ${SHELL_API_RATE_LIMIT}/分钟（突发 ${SHELL_API_RATE_BURST}，按客户端 IP）"
    echo "  默认 Shell: ${SHELL_API_DEFAULT_SHELL}"
    if [[ -n "${SHELL_API_FS_ROOT}" ]]; then
        echo "  文件沙箱:  ${SHELL_API_FS_ROOT}"
    else
        echo "  文件沙箱:  未限制"
        if is_readonly; then
            log_warn "只读模式下建议同时设置 SHELL_API_FS_ROOT，限制可读取的目录范围"
        fi
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
        echo -e "${CYAN}MCP 端点:${NC}"
        echo "  http://localhost:${EFFECTIVE_PORT}/mcp"
        echo "  请使用 MCP 客户端通过该端点连接并调用工具"
    else
        log_error "服务启动失败，请查看日志: ${LOG_FILE}"
        rm -f "${PID_FILE}"
        exit 1
    fi
}

# ──────────────────────────────────────────────
# 命令: start-ro  一键以只读模式启动
# ──────────────────────────────────────────────
cmd_start_readonly() {
    SHELL_API_READONLY=true
    export SHELL_API_READONLY
    log_info "已强制开启只读模式（SHELL_API_READONLY=true，优先级最高）"
    cmd_start
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

    # SIGTERM 触发服务端优雅退出：执行中的命令最多再跑 SHELL_API_SHUTDOWN_GRACE
    # 秒（默认 30），这里多等 5 秒再兜底 kill -9。
    local limit=$(( ${SHELL_API_SHUTDOWN_GRACE:-30} + 5 ))
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
# 命令: restart-ro  以只读模式重启
# ──────────────────────────────────────────────
cmd_restart_readonly() {
    cmd_stop
    sleep 1
    cmd_start_readonly
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
    [[ -n "${port}" ]] || port="${EFFECTIVE_PORT:-unknown}"
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
    # 检查二进制
    if [[ ! -f "${BINARY}" ]]; then
        log_info "二进制文件不存在，自动编译..."
        cmd_build
    fi
    check_binary_version

    # 鉴权互斥检查（与 cmd_start 一致）
    if [[ -n "${EFFECTIVE_TOKEN}" && -n "${EFFECTIVE_TOKEN_HASH}" ]]; then
        log_error "token 与 token hash 互斥（REMOTECTL_TOKEN/REMOTECTL_TOKEN_HASH 或对应 SHELL_API_*），只能设置其中一个"
        exit 1
    fi

    # D6: same guard as cmd_start.
    require_secure_credential

    # No random generation: fall back to the fixed default token.
    if [[ -z "${EFFECTIVE_TOKEN}" && -z "${EFFECTIVE_TOKEN_HASH}" ]]; then
        SHELL_API_TOKEN="${SHELL_API_DEFAULT_TOKEN}"
        log_warn "No explicit credential configured; using the built-in default token (loopback listeners only)"
    fi

    export SHELL_API_PORT
    if [[ -n "${REMOTECTL_PORT:-}" ]]; then export REMOTECTL_PORT; fi
    if [[ -n "${REMOTECTL_LISTEN:-}" ]]; then export REMOTECTL_LISTEN; fi
    export SHELL_API_TOKEN
    export SHELL_API_TOKEN_HASH
    export SHELL_API_MAX_TIMEOUT
    export SHELL_API_MAX_OUTPUT
    export SHELL_API_RATE_LIMIT
    export SHELL_API_RATE_BURST
    export SHELL_API_TRUSTED_PROXIES
    export SHELL_API_DEFAULT_SHELL
    export SHELL_API_FS_ROOT
    export SHELL_API_DISABLED_TOOLS
    export SHELL_API_READONLY
    local v
    for v in REMOTECTL_MODE REMOTECTL_CONTROL_LISTEN REMOTECTL_MASTER_URL \
             REMOTECTL_CLUSTER_TOKEN REMOTECTL_CLUSTER_TOKEN_FILE REMOTECTL_CLUSTER_TOKEN_HASH \
             REMOTECTL_MASTER_LOCAL REMOTECTL_NODE_APPROVAL REMOTECTL_NODE_ALLOWLIST \
             REMOTECTL_ENABLE_NODE_ADMIN_TOOLS REMOTECTL_NODE_NAME REMOTECTL_NODE_ID \
             REMOTECTL_STATE_DIR REMOTECTL_HEARTBEAT_SEC REMOTECTL_LEASE_TTL_SEC \
             REMOTECTL_STALE_AFTER_SEC REMOTECTL_OFFLINE_AFTER_SEC REMOTECTL_READONLY; do
        if [[ -n "${!v:-}" ]]; then export "${v?}"; fi
    done

    if is_readonly; then
        log_info "前台启动 [只读模式] (HTTP 模式，Ctrl+C 停止)..."
    else
        log_info "前台启动 (HTTP 模式，Ctrl+C 停止)..."
    fi
    exec "${BINARY}"
}

# ──────────────────────────────────────────────
# 命令: foreground-ro  一键以只读模式前台运行
# ──────────────────────────────────────────────
cmd_foreground_readonly() {
    SHELL_API_READONLY=true
    export SHELL_API_READONLY
    cmd_foreground
}

# ──────────────────────────────────────────────
# 命令: token  显示当前 Token
# ──────────────────────────────────────────────
cmd_token() {
    if [[ -n "${EFFECTIVE_TOKEN}" ]]; then
        echo "当前 Token: ${EFFECTIVE_TOKEN}"
        return 0
    fi
    if [[ -n "${EFFECTIVE_TOKEN_HASH}" ]]; then
        echo "当前为哈希模式（服务端只存盐化 SHA-256，无明文可显示）"
        echo "  Token Hash: ${EFFECTIVE_TOKEN_HASH}"
        echo "  明文 Token 仅客户端持有；重置时生成新哈希更新 SHELL_API_TOKEN_HASH 即可"
        return 0
    fi
    echo "No explicit credential configured; the server will use the built-in default token (loopback listeners only):"
    echo "  Token: ${SHELL_API_DEFAULT_TOKEN}"
    echo "  Set SHELL_API_TOKEN / REMOTECTL_TOKEN or *_TOKEN_HASH in .env for network exposure."
    echo "  Generate a hash: printf '%s' 'your-token' | ./remote-agent-proxy hash-token"
    exit 1
}

# ──────────────────────────────────────────────
# 命令: readonly-check  验证只读模式是否真正生效
# ──────────────────────────────────────────────
cmd_readonly_check() {
    local port="${EFFECTIVE_PORT:-8080}"
    local base="http://localhost:${port}"
    local failures=0

    if ! command -v curl &>/dev/null; then
        log_error "需要 curl 才能执行校验"
        exit 1
    fi
    # 校验需要调用受鉴权的端点，因此必须有明文 Token。明文模式直接用
    # SHELL_API_TOKEN；哈希模式（服务端只有哈希）需通过 CHECK_TOKEN 传入。
    : "${CHECK_TOKEN:=${EFFECTIVE_TOKEN}}"
    if [[ -z "${CHECK_TOKEN}" ]]; then
        log_error "未设置明文 Token，无法调用受鉴权的端点"
        echo "  哈希模式（SHELL_API_TOKEN_HASH）下请传入: CHECK_TOKEN=your-token $0 readonly-check"
        exit 1
    fi

    log_info "校验目标: ${base}"

    # 先验证握手声明的只读模式,再检查真实工具清单;错误响应不能算作通过。
    local hdr_file tools sid init_body
    hdr_file=$(mktemp)
    init_body=$(curl -fsS --max-time 10 -D "${hdr_file}" -X POST "${base}/mcp" \
        -H "Authorization: Bearer ${CHECK_TOKEN}" \
        -H "Content-Type: application/json" \
        -H "Accept: application/json, text/event-stream" \
        -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"readonly-check","version":"1"}}}' \
        2>/dev/null || true)
    sid=$(grep -i '^mcp-session-id:' "${hdr_file}" 2>/dev/null | tr -d '\r' | awk '{print $2}')
    rm -f "${hdr_file}"

    if [[ -z "${sid}" ]]; then
        echo -e "  ${RED}✗${NC} MCP initialize 握手失败，无法校验工具清单"
        failures=$((failures + 1))
    else
        if grep -qE '"name"[[:space:]]*:[[:space:]]*"(remote-shell-readonly|agentic-remotectl-readonly)"' <<<"${init_body}"; then
            echo -e "  ${GREEN}✓${NC} MCP 握手确认只读模式"
        else
            echo -e "  ${RED}✗${NC} MCP 握手未声明只读模式"
            failures=$((failures + 1))
        fi
        curl -fsS --max-time 10 -o /dev/null -X POST "${base}/mcp" \
            -H "Authorization: Bearer ${CHECK_TOKEN}" \
            -H "Content-Type: application/json" \
            -H "Accept: application/json, text/event-stream" \
            -H "Mcp-Session-Id: ${sid}" \
            -d '{"jsonrpc":"2.0","method":"notifications/initialized"}' 2>/dev/null || true
        tools=$(curl -fsS --max-time 10 -X POST "${base}/mcp" \
            -H "Authorization: Bearer ${CHECK_TOKEN}" \
            -H "Content-Type: application/json" \
            -H "Accept: application/json, text/event-stream" \
            -H "Mcp-Session-Id: ${sid}" \
            -d '{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}' 2>/dev/null || true)
        # 必须确实拿到工具清单，否则不能判定"无写工具"
        if [[ "${tools}" != *'"tools"'* ]]; then
            echo -e "  ${RED}✗${NC} 未取到 MCP 工具清单，无法判定（响应: ${tools:0:60}）"
            failures=$((failures + 1))
        else
            local leaked=""
            for t in remote_execute remote_session_execute remote_cancel remote_write_file \
                     remote_edit_file remote_upload_base64 remote_move_file remote_copy_file \
                     remote_delete_file remote_make_dir remote_session_create \
                     remote_session_list remote_session_close remote_spawn remote_job_status \
                     remote_job_list remote_job_logs remote_job_kill remote_wait_for remote_execution_result; do
                if [[ "${tools}" == *"\"${t}\""* ]]; then
                    leaked="${leaked} ${t}"
                fi
            done
            if [[ -z "${leaked}" ]]; then
                local n
                n=$(grep -o '"name":"remote_[a-z0-9_]*"' <<<"${tools}" | sort -u | wc -l | tr -d ' ')
                echo -e "  ${GREEN}✓${NC} MCP 工具清单无写工具（共 ${n} 个只读工具）"
            else
                echo -e "  ${RED}✗${NC} MCP 工具清单泄露写工具:${leaked}"
                failures=$((failures + 1))
            fi
        fi
    fi

    if [[ -n "${sid}" ]]; then
        curl -s --max-time 10 -o /dev/null -X DELETE "${base}/mcp" \
            -H "Authorization: Bearer ${CHECK_TOKEN}" -H "Mcp-Session-Id: ${sid}" 2>/dev/null || true
    fi
    echo ""
    if [[ ${failures} -eq 0 ]]; then
        log_info "只读模式校验通过"
    else
        log_error "只读模式校验失败: ${failures} 项"
        exit 1
    fi
}

# ──────────────────────────────────────────────
# 主入口
# ──────────────────────────────────────────────
case "${1:-}" in
    start)
        cmd_start
        ;;
    start-ro|start-readonly)
        cmd_start_readonly
        ;;
    stop)
        cmd_stop
        ;;
    restart)
        cmd_restart
        ;;
    restart-ro|restart-readonly)
        cmd_restart_readonly
        ;;
    status)
        cmd_status
        ;;
    foreground|fg|run)
        cmd_foreground
        ;;
    foreground-ro|fg-ro)
        cmd_foreground_readonly
        ;;
    build)
        cmd_build
        ;;
    token)
        cmd_token
        ;;
    readonly-check|ro-check)
        cmd_readonly_check
        ;;
    help|--help|-h)
        echo "用法: $0 {start|start-ro|stop|restart|restart-ro|status|foreground|foreground-ro|build|token|readonly-check|help}"
        echo ""
        echo "  start          后台启动服务（读写模式）"
        echo "  start-ro       后台启动服务（只读模式，一键）"
        echo "  stop           停止服务"
        echo "  restart        重启服务"
        echo "  restart-ro     以只读模式重启"
        echo "  status         查看进程状态与 MCP 端点可达性"
        echo "  foreground     前台运行（Ctrl+C 停止）"
        echo "  foreground-ro  前台运行（只读模式）"
        echo "  build          仅编译二进制文件"
        echo "  token          显示当前 Token"
        echo "  readonly-check 校验只读模式是否真正生效"
        echo "  help           显示此帮助"
        echo ""
        echo "环境变量（可在脚本内或 .env 文件中设置）:"
        echo "  SHELL_API_READONLY     只读模式开关，1/true/yes/on/enabled 开启（优先级最高）"
        echo "  SHELL_API_PORT         监听端口（默认: 8080）"
        echo "  REMOTECTL_PORT         Listen port (takes precedence over SHELL_API_PORT; default: 8080)"
        echo "  REMOTECTL_LISTEN       Full listen address host:port (default: :port); a non-loopback"
        echo "                         listener requires an explicit token or token hash"
        echo "  SHELL_API_TOKEN        Bearer Token（未设置时使用内置默认 Token，仅限回环监听）"
        echo "  SHELL_API_TOKEN_HASH   Token 的盐化 SHA-256（sha256:盐:哈希），与 SHELL_API_TOKEN 二选一，"
        echo "                         服务端不落明文。生成: printf '%s' 'your-token' | ./remote-agent-proxy hash-token"
        echo "  SHELL_API_MAX_TIMEOUT  命令超时上限秒数（默认: 300）"
        echo "  SHELL_API_MAX_OUTPUT   输出字节上限（默认: 1048576）"
        echo "  SHELL_API_RATE_LIMIT   每客户端 IP 每分钟请求补充速率（默认: 120）"
        echo "  SHELL_API_RATE_BURST   每客户端 IP 突发容量（默认: 60）"
        echo "  SHELL_API_TRUSTED_PROXIES 可信代理 IP/CIDR，逗号分隔（默认: 不信任转发头）"
        echo "  SHELL_API_DEFAULT_SHELL 默认 shell（默认: bash）"
        echo "  SHELL_API_FS_ROOT      文件沙箱根目录，逗号分隔可多个（默认: 不限制）"
        echo "  SHELL_API_DISABLED_TOOLS 禁用的 MCP 工具，逗号分隔（默认: 不禁用）"
        echo ""
        echo "只读模式说明:"
        echo "  开启后仅保留 15 个只读工具（文件读取 + 主机自省），命令执行、会话、"
        echo "  写/编辑/删除/移动/复制/建目录全部关闭，且无法在运行时恢复。"
        ;;
    *)
        echo "未知命令: ${1:-}"
        echo "用法: $0 {start|start-ro|stop|restart|restart-ro|status|foreground|foreground-ro|build|token|readonly-check|help}"
        exit 1
        ;;
esac
