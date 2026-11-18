#!/usr/bin/env bash
#
# upgrade.sh —— 经 MCP 自升级 agentic-remotectl / remote-agent-proxy
# (并排目录 + 金丝雀 + 自动回滚 + state 迁移)
#
# 必须用 remote_spawn 以后台任务运行:任务在独立会话里,旧服务退出时不受影响。
# 切换期间调用方的 MCP 连接必然中断,所以检查与回滚全部在本脚本内完成,不依赖调用方。
#
# 用法: bash upgrade.sh <旧目录> <新目录> <服务端口> <金丝雀端口> <期望版本>
#   新目录须已备好 agentic-remotectl(或兼容副本 remote-agent-proxy)/ control.sh / env.conf;
#   旧目录全程不改,回滚即用它重启。
#   版本检查通过已鉴权的 MCP initialize,不依赖 remote_status 工具是否启用。
#   slaver 模式没有 HTTP 端点:跳过 live 金丝雀,以切换后的 "registered with master" 日志为准。
#   哈希鉴权时需传 CHECK_TOKEN(新服务明文凭据);旧服务凭据不同时另传 OLD_CHECK_TOKEN。
# 退出码: 0 升级成功 | 2 预检或金丝雀失败,线上未动 | 3 新版本未就绪,已回滚 | 4 回滚失败,需人工
set -uo pipefail

OLD=${1:?旧目录} NEW=${2:?新目录} PORT=${3:?服务端口} CANARY=${4:?金丝雀端口} WANT=${5:?期望版本}
LOG="$NEW/upgrade-$(date +%Y%m%d-%H%M%S).log"

log() { echo "$(date '+%F %T') $*" | tee -a "$LOG"; }

# 凭据只在子 shell 内加载,不污染另一个部署目录的配置,也不写入日志。
token_from() (
    local dir=$1 explicit=$2 fallback=$3 v token
    for v in $(compgen -e); do
        [[ "$v" == SHELL_API_* || "$v" == REMOTECTL_* ]] && unset "$v"
    done
    cd "$dir" || return 1
    if [[ -f env.conf ]]; then
        source ./env.conf >/dev/null 2>&1 || return 1
    fi
    token=${explicit:-${REMOTECTL_TOKEN:-${SHELL_API_TOKEN:-$fallback}}}
    [[ -n "$token" ]] || return 1
    printf '%s' "$token"
)

# 端口上服务报告的版本来自 MCP 握手;认证失败或无响应时返回失败。
version_on() {
    local port=$1 token=$2 url response headers sid reported
    url="http://127.0.0.1:${port}/mcp"
    local payload='{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"upgrade-check","version":"1"}}}'
    headers=$(mktemp) || return 1
    if command -v curl >/dev/null 2>&1; then
        response=$(curl -fsS --max-time 3 -D "$headers" -X POST "$url" \
            -H "Authorization: Bearer $token" -H 'Content-Type: application/json' \
            -H 'Accept: application/json, text/event-stream' -d "$payload" 2>/dev/null) || response=""
    else
        response=$(wget -q -S -T 3 -t 1 -O - \
            --header="Authorization: Bearer $token" --header='Content-Type: application/json' \
            --header='Accept: application/json, text/event-stream' --post-data="$payload" \
            "$url" 2>"$headers") || response=""
    fi
    reported=$(printf '%s\n' "$response" | sed -nE 's/.*"serverInfo"[[:space:]]*:[[:space:]]*\{[^}]*"version"[[:space:]]*:[[:space:]]*"([^"]+)".*/\1/p')
    sid=$(sed -nE 's/^[[:space:]]*[Mm][Cc][Pp]-[Ss][Ee][Ss][Ss][Ii][Oo][Nn]-[Ii][Dd]:[[:space:]]*([^[:space:]]+).*/\1/p' "$headers" | tr -d '\r')
    rm -f "$headers"
    # curl 支持主动释放探测会话;旧版 wget 仍可完成握手检查。
    if [[ -n "$sid" ]] && command -v curl >/dev/null 2>&1; then
        curl -s --max-time 3 -o /dev/null -X DELETE "$url" \
            -H "Authorization: Bearer $token" -H "Mcp-Session-Id: $sid" 2>/dev/null || true
    fi
    [[ -n "$reported" ]] || return 1
    printf '%s\n' "${reported%%+*}"
}

# 占用检查只判断端口是否响应,不把 HTTP 错误当成服务健康。
port_responds() {
    local url="http://127.0.0.1:$1/mcp" code rc
    if command -v curl >/dev/null 2>&1; then
        code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 3 "$url" 2>/dev/null || true)
        [[ -n "$code" && "$code" != 000 ]]
    else
        wget -q -T 3 -t 1 -O /dev/null "$url" 2>/dev/null
        rc=$?
        [[ $rc -eq 0 || $rc -eq 8 ]]
    fi
}

# 最多等 secs 轮,直到端口上出现 want 版本;输出实际握手返回的版本。
wait_version() {
    local port=$1 want=$2 secs=$3 token=$4 i v
    for ((i = 0; i < secs * 2; i++)); do
        v=$(version_on "$port" "$token")
        if [[ -n "$v" && "$v" == "$want" ]]; then
            echo "$v"
            return 0
        fi
        sleep 0.5
    done
    return 1
}

# mode_of reads REMOTECTL_MODE from the directory's own env.conf in a clean
# shell. A missing value means standalone (the 1.x-compatible default).
mode_of() (
    local dir=$1
    cd "$dir" 2>/dev/null || return 1
    if [[ -f env.conf ]]; then
        set -a
        source ./env.conf >/dev/null 2>&1 || true
        set +a
    fi
    printf '%s' "${REMOTECTL_MODE:-standalone}"
)

# find_binary accepts both the 2.0 primary name and the 1.x compatibility copy.
find_binary() {
    local dir=$1 b
    for b in agentic-remotectl remote-agent-proxy; do
        if [[ -f "$dir/$b" ]]; then
            printf '%s' "$dir/$b"
            return 0
        fi
    done
    return 1
}

binary_version() {
    local bin=$1 out
    out=$("$bin" --version 2>/dev/null) || return 1
    printf '%s\n' "$out" | sed -nE 's/^[^ ]+ ([0-9]+\.[0-9]+\.[0-9]+).*/\1/p'
}

slaver_registered_count() {
    local n
    n=$(grep -c 'registered with master' "$1" 2>/dev/null || true)
    printf '%s' "${n:-0}"
}

wait_slaver_registered() {
    local file=$1 before=$2 secs=$3 i now
    for ((i = 0; i < secs * 2; i++)); do
        now=$(slaver_registered_count "$file")
        [[ "$now" -gt "$before" ]] && return 0
        sleep 0.5
    done
    return 1
}

# 在 dir 下执行 control.sh <cmd>。本任务继承了旧服务进程的环境(其 control.sh 导出的
# SHELL_API_* / REMOTECTL_*),先全部清掉,让每个目录只按自己的 env.conf 启动——否则继承的
# TOKEN 与新 env.conf 的 TOKEN_HASH 会触发互斥拒绝启动,继承的 REMOTECTL_MODE 也可能串味。
# start 时再用 setsid,让服务进程脱离本任务的进程组。
# (用字符串拼 -u 选项而非数组:test00 的 bash 3.0 不支持 arr+=(...) 追加语法)
ctl() {
    local dir=$1 cmd=$2 v flags=""
    for v in $(compgen -e); do
        [[ "$v" == SHELL_API_* || "$v" == REMOTECTL_* ]] && flags="$flags -u $v"
    done
    if [[ "$cmd" == start ]] && command -v setsid >/dev/null 2>&1; then
        (cd "$dir" && env -u CHECK_TOKEN -u OLD_CHECK_TOKEN $flags setsid bash ./control.sh start) >>"$LOG" 2>&1
    else
        (cd "$dir" && env -u CHECK_TOKEN -u OLD_CHECK_TOKEN $flags bash ./control.sh "$cmd") >>"$LOG" 2>&1
    fi
}

main() {
    local f pid cur banner v old_token new_token c="$NEW/.canary" old_mode new_mode OLD_BIN NEW_BIN
    log "开始: $OLD -> $NEW (服务端口 $PORT, 金丝雀端口 $CANARY, 期望版本 $WANT)"

    # 1. 预检:不停止旧服务
    [[ "$(cd "$OLD" 2>/dev/null && pwd -P)" != "$(cd "$NEW" 2>/dev/null && pwd -P)" ]] ||
        { log "预检失败: 旧目录与新目录相同,回滚将无从谈起"; exit 2; }
    [[ "$PORT" != "$CANARY" ]] || { log "预检失败: 服务端口与金丝雀端口不能相同"; exit 2; }
    [[ -f "$NEW/control.sh" ]] || { log "预检失败: 缺少 $NEW/control.sh"; exit 2; }
    [[ -f "$NEW/env.conf" ]] || { log "预检失败: 缺少 $NEW/env.conf"; exit 2; }
    NEW_BIN=$(find_binary "$NEW") ||
        { log "预检失败: $NEW 下缺少 agentic-remotectl 或 remote-agent-proxy"; exit 2; }
    OLD_BIN=$(find_binary "$OLD") ||
        { log "预检失败: $OLD 下缺少 agentic-remotectl 或 remote-agent-proxy"; exit 2; }
    old_mode=$(mode_of "$OLD") || old_mode=standalone
    new_mode=$(mode_of "$NEW") || new_mode=standalone
    [[ "$old_mode" == "$new_mode" ]] ||
        { log "预检失败: 模式不一致 (旧=$old_mode 新=$new_mode);不支持跨模式就地升级"; exit 2; }
    if [[ ! -f "$OLD/control.sh" || ! -f "$OLD/remote-agent-proxy.pid" ]]; then
        log "预检失败: $OLD 下没有 control.sh 或 PID 文件(服务不是由该目录的 control.sh 启动的?)"
        exit 2
    fi
    pid=$(cat "$OLD/remote-agent-proxy.pid")
    kill -0 "$pid" 2>/dev/null || { log "预检失败: PID 文件中的进程 $pid 不存在"; exit 2; }
    # PID 文件可能过期、PID 被别的进程复用:确认它运行的正是旧目录的二进制,免得误杀
    if [[ -e "/proc/$pid/exe" ]]; then
        local exe want_exe
        exe=$(readlink "/proc/$pid/exe")
        exe=${exe% (deleted)}
        want_exe="$(cd "$OLD" && pwd -P)/$(basename "$OLD_BIN")"
        [[ "$exe" == "$want_exe" ]] ||
            { log "预检失败: PID $pid 运行的是 $exe,不是 $want_exe"; exit 2; }
    fi

    if [[ "$new_mode" == "slaver" ]]; then
        # slaver 没有 HTTP 端点:版本取自旧二进制 banner。
        cur=$(binary_version "$OLD_BIN") ||
            { log "预检失败: 无法读取旧二进制版本 ($OLD_BIN)"; exit 2; }
        old_token=""
        new_token=""
    else
        command -v curl >/dev/null 2>&1 || command -v wget >/dev/null 2>&1 ||
            { log "预检失败: 本机既无 curl 也无 wget,无法做 MCP 检查"; exit 2; }
        old_token=$(token_from "$OLD" "${OLD_CHECK_TOKEN:-}" "${CHECK_TOKEN:-}") ||
            { log "预检失败: 无法取得旧服务明文凭据;哈希模式请传 OLD_CHECK_TOKEN 或 CHECK_TOKEN"; exit 2; }
        new_token=$(token_from "$NEW" "${CHECK_TOKEN:-}" "") ||
            { log "预检失败: 无法取得新服务明文凭据;哈希模式请传 CHECK_TOKEN"; exit 2; }
        cur=$(version_on "$PORT" "$old_token")
        [[ -n "$cur" ]] || { log "预检失败: 端口 $PORT 的 MCP 握手失败或凭据无效"; exit 2; }
        if port_responds "$CANARY"; then
            log "预检失败: 金丝雀端口 $CANARY 已被占用"
            exit 2
        fi
        # 金丝雀会改写端口,测不出新 env.conf 的端口配错:这里先核对(8099 是 control.sh 的默认值)
        local newport
        newport=$(cd "$NEW" && env -i PATH="$PATH" bash -c 'set -a; source ./env.conf >/dev/null 2>&1; echo "${REMOTECTL_PORT:-${SHELL_API_PORT:-8099}}"' 2>/dev/null)
        [[ "$newport" == "$PORT" ]] ||
            { log "预检失败: $NEW/env.conf 的端口为 $newport,与服务端口 $PORT 不一致"; exit 2; }
    fi
    chmod +x "$NEW_BIN" "$NEW/control.sh"
    banner=$("$NEW_BIN" --version 2>&1) || { log "预检失败: 新二进制无法运行: $banner"; exit 2; }
    [[ "$banner" == *" $WANT "* ]] || { log "预检失败: 新二进制版本不符: $banner"; exit 2; }
    log "预检通过: 线上 $cur (PID $pid); 新二进制 $banner; mode $new_mode"

    # 2. 金丝雀:同一环境、同一 control.sh 与 env.conf,只换端口,完整走一遍 start -> 握手 -> stop
    if [[ "$new_mode" == "slaver" ]]; then
        log "金丝雀跳过: slaver 无 HTTP 端点,且重复注册会抢占节点身份;以切换后的注册日志为准"
    else
        rm -rf "$c" && mkdir -p "$c"
        cp -p "$NEW_BIN" "$NEW/control.sh" "$NEW/env.conf" "$c/"
        cp -p "$NEW_BIN" "$c/agentic-remotectl"
        # 金丝雀使用自己的 state 副本: 既不污染真实 state,又能读到 cluster-tokens.json
        if [[ -d "$NEW/state" ]]; then
            cp -a "$NEW/state" "$c/state"
        fi
        printf '\nSHELL_API_PORT=%s\nREMOTECTL_PORT=%s\nREMOTECTL_LISTEN=:%s\nREMOTECTL_STATE_DIR=%s\n' \
            "$CANARY" "$CANARY" "$CANARY" "$c/state" >>"$c/env.conf"
        if [[ "$new_mode" == "master" ]]; then
            local canary_control=$((CANARY + 1000))
            if port_responds "$canary_control"; then
                log "预检失败: 金丝雀控制端口 $canary_control 已被占用"
                exit 2
            fi
            printf 'REMOTECTL_CONTROL_LISTEN=127.0.0.1:%s\n' "$canary_control" >>"$c/env.conf"
        fi
        if ! ctl "$c" start || ! v=$(wait_version "$CANARY" "$WANT" 10 "$new_token"); then
            ctl "$c" stop
            log "金丝雀失败: 端口 $CANARY 未就绪,线上未动。金丝雀目录保留供排查: $c (见其中 remote-agent-proxy.log 与本日志)"
            exit 2
        fi
        ctl "$c" stop
        rm -rf "$c"
        log "金丝雀通过: 端口 $CANARY 返回 $v"
    fi

    # 3. 切换:此后调用方的 MCP 连接中断,直到新服务起来
    log "停止旧服务 ($OLD)"
    ctl "$OLD" stop
    # D34: state 迁移发生在旧服务停止之后、新服务启动之前。复制而非移动,
    # 回滚时旧目录的 state 仍然完整。
    if [[ -d "$OLD/state" ]]; then
        rm -rf "$NEW/state"
        cp -a "$OLD/state" "$NEW/state"
        log "已迁移 state: $OLD/state -> $NEW/state"
    fi
    log "启动新服务 ($NEW)"
    if [[ "$new_mode" == "slaver" ]]; then
        local new_before
        new_before=$(slaver_registered_count "$NEW/remote-agent-proxy.log")
        if ctl "$NEW" start && wait_slaver_registered "$NEW/remote-agent-proxy.log" "$new_before" 20; then
            log "升级成功: slaver 已重新注册到 master"
            exit 0
        fi
    else
        if ctl "$NEW" start && v=$(wait_version "$PORT" "$WANT" 20 "$new_token"); then
            log "升级成功: 端口 $PORT 运行 $v"
            exit 0
        fi
    fi

    # 4. 回滚:旧目录未改动,直接用它重启并核对原版本
    log "新服务未就绪,回滚到 $OLD"
    ctl "$NEW" stop
    if [[ "$new_mode" == "slaver" ]]; then
        local old_before
        old_before=$(slaver_registered_count "$OLD/remote-agent-proxy.log")
        ctl "$OLD" start
        if wait_slaver_registered "$OLD/remote-agent-proxy.log" "$old_before" 20; then
            log "已回滚: slaver 已重新注册到 master"
            exit 3
        fi
    else
        ctl "$OLD" start
        if v=$(wait_version "$PORT" "$cur" 20 "$old_token"); then
            log "已回滚: 端口 $PORT 运行 $v"
            exit 3
        fi
    fi
    log "回滚失败: 端口 $PORT 无服务,需人工登录处理: cd $OLD && ./control.sh start"
    exit 4
}

main
