#!/usr/bin/env bash
#
# upgrade.sh —— 经 MCP 自升级 remote-agent-proxy(并排目录 + 金丝雀 + 自动回滚)
#
# 必须用 remote_spawn 以后台任务运行:任务在独立会话里,旧服务退出时不受影响。
# 切换期间调用方的 MCP 连接必然中断,所以检查与回滚全部在本脚本内完成,不依赖调用方。
#
# 用法: bash upgrade.sh <旧目录> <新目录> <服务端口> <金丝雀端口> <期望版本>
#   新目录须已备好 remote-agent-proxy / control.sh / env.conf;旧目录全程不改,回滚即用它重启。
#   版本检查通过已鉴权的 MCP initialize,不依赖 remote_status 工具是否启用。
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
        [[ "$v" == SHELL_API_* ]] && unset "$v"
    done
    cd "$dir" || return 1
    if [[ -f env.conf ]]; then
        source ./env.conf >/dev/null 2>&1 || return 1
    fi
    token=${explicit:-${SHELL_API_TOKEN:-$fallback}}
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

# 在 dir 下执行 control.sh <cmd>。本任务继承了旧服务进程的环境(其 control.sh 导出的
# SHELL_API_*),先全部清掉,让每个目录只按自己的 env.conf 启动——否则继承的
# SHELL_API_TOKEN 与新 env.conf 的 TOKEN_HASH 会触发互斥拒绝启动。start 时再用 setsid,
# 让服务进程脱离本任务的进程组。
# (用字符串拼 -u 选项而非数组:test00 的 bash 3.0 不支持 arr+=(...) 追加语法)
ctl() {
    local dir=$1 cmd=$2 v flags=""
    for v in $(compgen -e); do
        [[ "$v" == SHELL_API_* ]] && flags="$flags -u $v"
    done
    if [[ "$cmd" == start ]] && command -v setsid >/dev/null 2>&1; then
        (cd "$dir" && env -u CHECK_TOKEN -u OLD_CHECK_TOKEN $flags setsid bash ./control.sh start) >>"$LOG" 2>&1
    else
        (cd "$dir" && env -u CHECK_TOKEN -u OLD_CHECK_TOKEN $flags bash ./control.sh "$cmd") >>"$LOG" 2>&1
    fi
}

main() {
    local f pid cur banner v old_token new_token c="$NEW/.canary"
    log "开始: $OLD -> $NEW (服务端口 $PORT, 金丝雀端口 $CANARY, 期望版本 $WANT)"

    # 1. 预检:不停止旧服务
    [[ "$(cd "$OLD" 2>/dev/null && pwd -P)" != "$(cd "$NEW" 2>/dev/null && pwd -P)" ]] ||
        { log "预检失败: 旧目录与新目录相同,回滚将无从谈起"; exit 2; }
    [[ "$PORT" != "$CANARY" ]] || { log "预检失败: 服务端口与金丝雀端口不能相同"; exit 2; }
    for f in remote-agent-proxy control.sh env.conf; do
        [[ -f "$NEW/$f" ]] || { log "预检失败: 缺少 $NEW/$f"; exit 2; }
    done
    if [[ ! -f "$OLD/control.sh" || ! -f "$OLD/remote-agent-proxy.pid" ]]; then
        log "预检失败: $OLD 下没有 control.sh 或 PID 文件(服务不是由该目录的 control.sh 启动的?)"
        exit 2
    fi
    command -v curl >/dev/null 2>&1 || command -v wget >/dev/null 2>&1 ||
        { log "预检失败: 本机既无 curl 也无 wget,无法做 MCP 检查"; exit 2; }
    old_token=$(token_from "$OLD" "${OLD_CHECK_TOKEN:-}" "${CHECK_TOKEN:-}") ||
        { log "预检失败: 无法取得旧服务明文凭据;哈希模式请传 OLD_CHECK_TOKEN 或 CHECK_TOKEN"; exit 2; }
    new_token=$(token_from "$NEW" "${CHECK_TOKEN:-}" "") ||
        { log "预检失败: 无法取得新服务明文凭据;哈希模式请传 CHECK_TOKEN"; exit 2; }
    pid=$(cat "$OLD/remote-agent-proxy.pid")
    kill -0 "$pid" 2>/dev/null || { log "预检失败: PID 文件中的进程 $pid 不存在"; exit 2; }
    # PID 文件可能过期、PID 被别的进程复用:确认它运行的正是旧目录的二进制,免得误杀
    if [[ -e "/proc/$pid/exe" ]]; then
        local exe want_exe
        exe=$(readlink "/proc/$pid/exe")
        exe=${exe% (deleted)}
        want_exe="$(cd "$OLD" && pwd -P)/remote-agent-proxy"
        [[ "$exe" == "$want_exe" ]] ||
            { log "预检失败: PID $pid 运行的是 $exe,不是 $want_exe"; exit 2; }
    fi
    cur=$(version_on "$PORT" "$old_token")
    [[ -n "$cur" ]] || { log "预检失败: 端口 $PORT 的 MCP 握手失败或凭据无效"; exit 2; }
    if port_responds "$CANARY"; then
        log "预检失败: 金丝雀端口 $CANARY 已被占用"
        exit 2
    fi
    # 金丝雀会改写端口,测不出新 env.conf 的端口配错:这里先核对(8099 是 control.sh 的默认值)
    local newport
    newport=$(cd "$NEW" && env -i PATH="$PATH" bash -c 'set -a; source ./env.conf >/dev/null 2>&1; echo "${SHELL_API_PORT:-8099}"')
    [[ "$newport" == "$PORT" ]] ||
        { log "预检失败: $NEW/env.conf 的端口为 $newport,与服务端口 $PORT 不一致"; exit 2; }
    chmod +x "$NEW/remote-agent-proxy" "$NEW/control.sh"
    banner=$("$NEW/remote-agent-proxy" --version 2>&1) || { log "预检失败: 新二进制无法运行: $banner"; exit 2; }
    [[ "$banner" == *" $WANT "* ]] || { log "预检失败: 新二进制版本不符: $banner"; exit 2; }
    log "预检通过: 线上 $cur (PID $pid); 新二进制 $banner"

    # 2. 金丝雀:同一环境、同一 control.sh 与 env.conf,只换端口,完整走一遍 start -> MCP 握手 -> stop
    rm -rf "$c" && mkdir -p "$c"
    cp -p "$NEW/remote-agent-proxy" "$NEW/control.sh" "$NEW/env.conf" "$c/"
    printf '\nSHELL_API_PORT=%s\n' "$CANARY" >>"$c/env.conf"
    if ! ctl "$c" start || ! v=$(wait_version "$CANARY" "$WANT" 10 "$new_token"); then
        ctl "$c" stop
        log "金丝雀失败: 端口 $CANARY 未就绪,线上未动。金丝雀目录保留供排查: $c (见其中 remote-agent-proxy.log 与本日志)"
        exit 2
    fi
    ctl "$c" stop
    rm -rf "$c"
    log "金丝雀通过: 端口 $CANARY 返回 $v"

    # 3. 切换:此后调用方的 MCP 连接中断,直到新服务起来
    log "停止旧服务 ($OLD)"
    ctl "$OLD" stop
    log "启动新服务 ($NEW)"
    if ctl "$NEW" start && v=$(wait_version "$PORT" "$WANT" 20 "$new_token"); then
        log "升级成功: 端口 $PORT 运行 $v"
        exit 0
    fi

    # 4. 回滚:旧目录未改动,直接用它重启并核对原版本
    log "新服务未就绪,回滚到 $OLD"
    ctl "$NEW" stop
    ctl "$OLD" start
    if v=$(wait_version "$PORT" "$cur" 20 "$old_token"); then
        log "已回滚: 端口 $PORT 运行 $v"
        exit 3
    fi
    log "回滚失败: 端口 $PORT 无服务,需人工登录处理: cd $OLD && ./control.sh start"
    exit 4
}

main
