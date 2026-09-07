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

BINARY="${SCRIPT_DIR}/remote-agent-proxy"
PID_FILE="${SCRIPT_DIR}/remote-agent-proxy.pid"
LOG_FILE="${SCRIPT_DIR}/remote-agent-proxy.log"

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
: "${SHELL_API_READONLY:=}"

# 加载 .env 文件（如果存在，且变量允许被覆盖）
if [[ -f "${SCRIPT_DIR}/.env" ]]; then
    # shellcheck source=/dev/null
    set -a
    source "${SCRIPT_DIR}/.env"
    set +a
fi

# 只读模式判定：脚本层只用于展示与提示，真正的强制在服务端。
is_readonly() {
    case "$(echo "${SHELL_API_READONLY:-}" | tr '[:upper:]' '[:lower:]' | tr -d '[:space:]')" in
        1|true|yes|on|enabled) return 0 ;;
        *) return 1 ;;
    esac
}

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
    export SHELL_API_READONLY
    # 不设置 TLS 相关变量，确保使用 HTTP

    echo ""
    log_info "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    if is_readonly; then
        log_info "Remote Shell API Server 启动中... [只读模式]"
    else
        log_info "Remote Shell API Server 启动中..."
    fi
    log_info "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo "  端口:      ${SHELL_API_PORT}"
    echo "  协议:      HTTP（明文，未启用 TLS）"
    if is_readonly; then
        echo -e "  运行模式:  ${CYAN}只读（READ-ONLY，最高优先级）${NC}"
        echo "             仅保留 11 个只读工具；命令执行 / 写 / 编辑 / 删除 / 移动 / 复制 / 建目录 全部关闭"
        echo "             REST 仅放行 /api/ 下的 GET / HEAD"
    else
        echo -e "  运行模式:  ${YELLOW}读写（完整权限，等同交互式 shell）${NC}"
    fi
    echo "  超时上限:  ${SHELL_API_MAX_TIMEOUT}s"
    echo "  输出上限:  ${SHELL_API_MAX_OUTPUT} 字节"
    echo "  限流:      ${SHELL_API_RATE_LIMIT}/分钟"
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
        echo -e "${CYAN}快速验证:${NC}"
        echo "  curl http://localhost:${SHELL_API_PORT}/api/status"
        echo ""
        if is_readonly; then
            echo -e "${CYAN}只读模式校验（应返回 403 read_only_mode）:${NC}"
            echo "  curl -i -X POST http://localhost:${SHELL_API_PORT}/api/execute \\"
            echo "    -H \"Authorization: Bearer ${SHELL_API_TOKEN}\" \\"
            echo "    -H \"Content-Type: application/json\" \\"
            echo "    -d '{\"command\":\"id\"}'"
            echo ""
            echo -e "${CYAN}只读模式确认（read_only 应为 true）:${NC}"
            echo "  curl -s http://localhost:${SHELL_API_PORT}/api/status | grep read_only"
        else
            echo -e "${CYAN}执行命令:${NC}"
            echo "  curl -X POST http://localhost:${SHELL_API_PORT}/api/execute \\"
            echo "    -H \"Authorization: Bearer ${SHELL_API_TOKEN}\" \\"
            echo "    -H \"Content-Type: application/json\" \\"
            echo "    -d '{\"command\":\"uname -a\"}'"
        fi
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
    # 尝试获取端口
    if command -v lsof &>/dev/null; then
        local port
        port=$(lsof -p "${pid}" -i TCP -s TCP:LISTEN -Fn 2>/dev/null | grep -o '[0-9]*$' | head -1 || true)
        if [[ -n "${port}" ]]; then
            echo "  端口:      ${port}"
        fi
    fi

    # 尝试健康检查（顺带读出服务端自报的运行模式，这是权威来源）
    local port="${SHELL_API_PORT:-8080}"
    if command -v curl &>/dev/null; then
        local body resp
        body=$(curl -s "http://localhost:${port}/api/status" 2>/dev/null || true)
        resp=$(curl -s -o /dev/null -w "%{http_code}" "http://localhost:${port}/api/status" 2>/dev/null || true)
        if [[ "${resp}" == "200" ]]; then
            echo "  健康检查:  正常 (HTTP ${resp})"
            if [[ "${body}" == *'"read_only":true'* ]]; then
                echo -e "  运行模式:  ${CYAN}只读（服务端确认）${NC}"
            elif [[ "${body}" == *'"read_only":false'* ]]; then
                echo -e "  运行模式:  ${YELLOW}读写（服务端确认）${NC}"
            fi
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
    export SHELL_API_READONLY

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
    if [[ -z "${SHELL_API_TOKEN}" ]]; then
        log_error "SHELL_API_TOKEN 未设置"
        echo "  请设置环境变量: export SHELL_API_TOKEN=your-token"
        echo "  或在 .env 文件中添加: SHELL_API_TOKEN=your-token"
        exit 1
    fi
    echo "当前 Token: ${SHELL_API_TOKEN}"
}

# ──────────────────────────────────────────────
# 命令: readonly-check  验证只读模式是否真正生效
# ──────────────────────────────────────────────
cmd_readonly_check() {
    local port="${SHELL_API_PORT:-8080}"
    local base="http://localhost:${port}"
    local failures=0

    if ! command -v curl &>/dev/null; then
        log_error "需要 curl 才能执行校验"
        exit 1
    fi
    if [[ -z "${SHELL_API_TOKEN}" ]]; then
        log_error "SHELL_API_TOKEN 未设置，无法调用受鉴权的端点"
        exit 1
    fi

    log_info "校验目标: ${base}"

    # 1. 服务端自报模式
    local status_body
    status_body=$(curl -s "${base}/api/status" 2>/dev/null || true)
    if [[ "${status_body}" == *'"read_only":true'* ]]; then
        echo -e "  ${GREEN}✓${NC} /api/status 自报 read_only=true"
    else
        echo -e "  ${RED}✗${NC} /api/status 未自报只读（服务可能未开启只读模式）"
        ((failures++))
    fi

    # 2. REST 执行端点必须 403
    local code
    for path in "/api/execute" "/api/execute/stream" "/api/sessions"; do
        code=$(curl -s -o /dev/null -w "%{http_code}" -X POST "${base}${path}" \
            -H "Authorization: Bearer ${SHELL_API_TOKEN}" \
            -H "Content-Type: application/json" \
            -d '{"command":"id"}' 2>/dev/null || true)
        if [[ "${code}" == "403" ]]; then
            echo -e "  ${GREEN}✓${NC} POST ${path} -> 403"
        else
            echo -e "  ${RED}✗${NC} POST ${path} -> ${code:-无响应}（期望 403）"
            ((failures++))
        fi
    done

    # 3. MCP 工具清单里不能出现写工具
    #    必须先完成 initialize 握手拿到 session id，否则服务返回
    #    "Invalid session ID"——那段文本恰好不含任何工具名，会造成假通过。
    local hdr_file tools sid
    hdr_file=$(mktemp)
    curl -s -D "${hdr_file}" -o /dev/null -X POST "${base}/mcp" \
        -H "Authorization: Bearer ${SHELL_API_TOKEN}" \
        -H "Content-Type: application/json" \
        -H "Accept: application/json, text/event-stream" \
        -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"readonly-check","version":"1"}}}' \
        2>/dev/null || true
    sid=$(grep -i '^mcp-session-id:' "${hdr_file}" 2>/dev/null | tr -d '\r' | awk '{print $2}')
    rm -f "${hdr_file}"

    if [[ -z "${sid}" ]]; then
        echo -e "  ${RED}✗${NC} MCP initialize 握手失败，无法校验工具清单"
        ((failures++))
    else
        tools=$(curl -s -X POST "${base}/mcp" \
            -H "Authorization: Bearer ${SHELL_API_TOKEN}" \
            -H "Content-Type: application/json" \
            -H "Accept: application/json, text/event-stream" \
            -H "Mcp-Session-Id: ${sid}" \
            -d '{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}' 2>/dev/null || true)
        # 必须确实拿到工具清单，否则不能判定"无写工具"
        if [[ "${tools}" != *'"tools"'* ]]; then
            echo -e "  ${RED}✗${NC} 未取到 MCP 工具清单，无法判定（响应: ${tools:0:60}）"
            ((failures++))
        else
            local leaked=""
            for t in remote_execute remote_session_execute remote_cancel remote_write_file \
                     remote_edit_file remote_upload_base64 remote_move_file remote_copy_file \
                     remote_delete_file remote_make_dir remote_session_create \
                     remote_session_list remote_session_close; do
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
                ((failures++))
            fi
        fi
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
        echo "  status         查看服务运行状态（含服务端自报的运行模式）"
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
        echo "  SHELL_API_TOKEN        Bearer Token（必填，未设置时会自动生成）"
        echo "  SHELL_API_MAX_TIMEOUT  命令超时上限秒数（默认: 300）"
        echo "  SHELL_API_MAX_OUTPUT   输出字节上限（默认: 1048576）"
        echo "  SHELL_API_RATE_LIMIT   每 IP 每分钟请求上限（默认: 60）"
        echo "  SHELL_API_DEFAULT_SHELL 默认 shell（默认: bash）"
        echo "  SHELL_API_FS_ROOT      文件沙箱根目录，逗号分隔可多个（默认: 不限制）"
        echo "  SHELL_API_DISABLED_TOOLS 禁用的 MCP 工具，逗号分隔（默认: 不禁用）"
        echo ""
        echo "只读模式说明:"
        echo "  开启后仅保留 11 个只读工具（文件读取 + 主机自省），命令执行、会话、"
        echo "  写/编辑/删除/移动/复制/建目录全部关闭，且无法在运行时恢复。"
        ;;
    *)
        echo "未知命令: ${1:-}"
        echo "用法: $0 {start|start-ro|stop|restart|restart-ro|status|foreground|foreground-ro|build|token|readonly-check|help}"
        exit 1
        ;;
esac
