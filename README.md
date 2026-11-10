# remote-cli

[![Go Version](https://img.shields.io/github/go-mod/go-version/chuanqq/remote-cli)](https://go.dev/)
[![Go Report Card](https://goreportcard.com/badge/github.com/chuanqq/remote-cli)](https://goreportcard.com/report/github.com/chuanqq/remote-cli)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](./LICENSE)

**English:** A single-binary MCP (Model Context Protocol) server written in Go that lets AI agents (such as Claude Code) securely operate remote hosts. It exposes 35 dedicated tools through one Streamable HTTP MCP endpoint — shell execution with persistent sessions, file read/write/edit, server-side content search, log tailing with cursors and follow, process/port inspection and an environment profile — so the agent rarely needs to fall back to raw shell commands.

一个用 Go 编写的 MCP(Model Context Protocol)服务端,让 AI Agent(如 Claude Code)通过网络安全地操作远程主机。通过 Streamable HTTP 暴露 35 个专用工具:命令执行与持久会话、超出调用时长可续取的执行与服务端等待、后台任务、文件读写编辑与批量读取、服务端内容搜索与文件/目录对比、带游标与 follow 的日志跟踪、进程/端口洞察与环境画像——绝大多数远程操作无需再拼 shell 命令。

编译产物为单二进制,零外部运行时依赖;TLS 与 Bearer Token 鉴权开箱即用。

> **说明**:自 v1.6.0 起服务只保留 MCP Streamable HTTP 入口,早期版本的 REST 兼容接口已移除;客户端请统一使用 MCP。

## 特性

- **命令执行**:可取消、可续取结果的执行,基于进程组的硬超时控制,有界输出捕获可选保留头部或尾部(`truncate_mode`),可选 GBK 等输出解码(`output_encoding`)
- **后台任务与等待**:`remote_spawn` 启动脱离调用的后台任务并按 job id 查询/读日志/终止;`remote_wait_for` 服务端等待文件/日志/进程/端口/命令条件,满足即返回
- **持久会话**:维护 cwd / shell / 环境变量,`cd` 后自动同步工作目录,带 TTL 自动回收;会话的创建/列出/销毁均有对应 MCP 工具
- **文件操作**:读 / 写 / 编辑 / 列目录 / stat / 移动 / 复制 / 删除 / 建目录,支持 UTF-8 / GBK / GB2312 / GB18030 编码互转与自动探测,大文件支持 base64 分块上传下载;写入类操作返回 sha256/mode 自检,可选 bash/python 语法 lint
- **内容搜索、聚合与日志**:服务端内容正则搜索、逐文件行数统计、分列过滤及数值极值/top-N 聚合；搜索和聚合支持后台续取；另有文件名查找、大日志尾部/增量/跟随读取，不依赖目标机 grep/rg/find
- **主机洞察**:进程枚举、监听端口检查、一次性环境画像(36 个常用工具链探测)
- **运维**:SIGTERM 优雅退出、构建时注入版本与 commit、MCP 长连接心跳
- **只读模式**:`SHELL_API_READONLY=true` 一键将服务降级为纯读取代理,工具注册与操作兜底两层强制、运行时不可恢复,优先级高于其他所有配置
- **安全控制**:Bearer Token 鉴权(常量时间比较)、按客户端 IP 的令牌桶限流、可选跳板命令拦截、输出字节上限、超时上限、可选多目录 `FSRoot` 文件系统沙箱、工具黑名单、删除工具双重确认
- **可观测**:JSON 结构化访问日志(含状态码)与审计日志,通过 `req_id` 关联,记录每次调用的真实来源 IP、工具名、耗时与失败原因,并对 mysql 密码、Bearer token 等敏感信息自动脱敏

## 平台支持

| 平台 | 构建与测试 | `remote_list_processes` / `remote_check_port` | 其余工具 |
| --- | --- | --- | --- |
| Linux | ✅ | ✅ 完整实现(读 `/proc`) | ✅ |
| macOS | ✅(本机构建,交叉编译受 cgo 限制) | ✅ 基于 `ps` / `lsof` 封装 | ✅ |
| Windows | ✅ | ⚠️ 返回 unsupported | ✅ |

## 快速开始

```bash
# 方式一:直接安装(需要 Go 1.23+)
go install github.com/chuanqq/remote-cli@latest

# 方式二:源码构建
git clone https://github.com/chuanqq/remote-cli.git
cd remote-cli
go build -o remote-agent-proxy

# 运行(最低要求:设置 Token)
SHELL_API_TOKEN=your-secret-token ./remote-agent-proxy

# 哈希模式(服务端不落明文 Token):客户端照旧发送明文 Bearer,
# 服务端只存盐化 SHA-256,两者与 SHELL_API_TOKEN 二选一
printf '%s' 'your-secret-token' | ./remote-agent-proxy hash-token
# 输出形如 sha256:<salt-hex>:<hash-hex>,写入:
SHELL_API_TOKEN_HASH=sha256:...:... ./remote-agent-proxy

# 只读模式(一键):仅暴露读取能力,禁止任何修改
SHELL_API_TOKEN=your-secret-token SHELL_API_READONLY=true ./remote-agent-proxy
# 或用启动脚本
./start.sh start-ro

# 生产部署建议启用 TLS
SHELL_API_TOKEN=your-secret-token \
SHELL_API_TLS_CERT=./cert.pem \
SHELL_API_TLS_KEY=./key.pem \
./remote-agent-proxy
```

接入 Claude Code:

```bash
claude mcp add --transport http remote-shell https://your-host:8080/mcp \
  --header "Authorization: Bearer your-secret-token"
```

或写入 MCP 客户端配置(Claude Code、Cursor 等通用):

```json
{
  "mcpServers": {
    "remote-shell": {
      "url": "https://your-host:8080/mcp",
      "headers": { "Authorization": "Bearer your-secret-token" }
    }
  }
}
```

连通后客户端 `tools/list` 应能看到全部 35 个工具(未设置 `SHELL_API_FS_ROOT` 时 `remote_delete_file` 不注册,为 34 个)。服务端只提供上述 MCP 端点,不存在独立的 REST HTTP 接口。

## v1.8.0 升级说明

新增只读数值聚合工具 `remote_aggregate`：按分隔符取第 N 列，以行正则和字段条件过滤，在服务器上直接计算 count、min、max 与 top-N，避免下载完整日志。数值比较保留大整数精度，结果带文件名和行号。

`remote_search_content` 的 count 模式始终返回 `total_matches`，包括零命中；总命中行数、总命中文件数覆盖完整扫描，逐文件 `files[].count` 明细单独分页。`counts_complete` 区分完整统计与超时、跳过文件或超长行造成的部分统计，单文件未扫完时标记 `partial`。

搜索和聚合支持独立的 `timeout_ms`（默认 300000，最大 3600000），不再在单次调用预算耗尽时终止扫描。等待到 `wait_ms` 后仍在运行会返回 `scan_id`，用新增的 `remote_scan_result` 续取同一任务；`wait_ms: 0` 可立即返回。两个新工具均在只读模式可用，总工具数为 35，只读工具数为 15。升级后客户端需刷新 MCP 工具清单。

## v1.7.0 升级说明

新增可信代理配置 `SHELL_API_TRUSTED_PROXIES`,支持逗号分隔的 IP 和 CIDR。只有 TCP 对端属于该列表时才读取 `X-Forwarded-For` / `X-Real-IP`,解析后的客户端 IP 统一用于限流、访问日志和 MCP 审计。默认不信任任何转发头,直连部署无需调整;非法代理配置会拒绝启动。详细规则及 nginx 示例见下文。

部署脚本新增 `./control.sh mcp-prompt [URL] [服务名]`,可输出一段用于 agent 客户端添加 MCP 服务的 prompt。URL 可预先配置为 `MCP_PUBLIC_URL`;哈希鉴权时用 `CHECK_TOKEN` 提供客户端明文 Token,缺失时输出交互获取凭据的占位符,不输出服务端哈希。

## v1.6.0 升级说明

本版本删除全部旧 REST 接口,包括 `/api/status`、`/api/execute`、`/api/execute/stream`、`/api/sessions*` 和 `/api/executions/*`。携带有效凭据访问这些路径时返回 404;未鉴权请求统一返回 401。唯一 HTTP 入口为 `/mcp`,所有 MCP 请求均要求 Bearer Token。

- 命令执行、会话管理和取消操作迁移到对应的 `remote_*` MCP 工具。
- 运行中的健康状态、版本和只读标志通过 MCP 的 `remote_status` 查询;握手 `initialize` 也会返回服务版本。
- `start.sh status` 与 `control.sh status` 检查进程和 `/mcp` 的 401 鉴权响应,只表示端点可达;不再读取匿名状态数据。二进制版本使用 `--version`,实际运行版本通过 MCP 查询。
- `readonly-check` 改为校验 MCP 握手的只读声明和工具清单;哈希模式仍需传入 `CHECK_TOKEN` 明文凭据。
- `upgrade.sh` 通过已鉴权的 MCP 握手确认金丝雀、新服务和回滚版本。明文模式从各部署目录的 `env.conf` 读取 Token;哈希模式须传 `CHECK_TOKEN`,旧服务凭据不同时另传 `OLD_CHECK_TOKEN`。缺少探测凭据会在停止旧服务前拒绝升级。
- `control.sh` 未配置凭据时会生成随机 Token,固定默认 Token 已删除;已部署实例仍须轮换旧凭据。
- TLS 只配置证书或私钥时拒绝启动,避免配置不完整时回退到明文 HTTP。

## 配置项

所有配置通过环境变量传入,均带默认值。

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `SHELL_API_READONLY` | (空) | **只读模式开关,优先级最高**。取 `1` / `true` / `yes` / `on` / `enabled` 开启;开启后所有写能力强制关闭,无法被其他配置覆盖 |
| `SHELL_API_PORT` | `8080` | 监听端口 |
| `SHELL_API_TOKEN` | (必填) | Bearer Token,客户端鉴权凭据 |
| `SHELL_API_TOKEN_HASH` | (二选一) | Token 的盐化 SHA-256,格式 `sha256:<salt-hex>:<hash-hex>`(hash = SHA-256(salt ‖ token)),与 `SHELL_API_TOKEN` 互斥且必须恰好设置其一。服务端不落明文,客户端仍发送明文 Bearer。生成: `printf '%s' 'your-token' \| ./remote-agent-proxy hash-token` |
| `SHELL_API_TLS_CERT` | (空) | TLS 证书路径;与 `SHELL_API_TLS_KEY` 同时留空时使用 HTTP,仅设置其中一项则拒绝启动 |
| `SHELL_API_TLS_KEY` | (空) | TLS 私钥路径;与 `SHELL_API_TLS_CERT` 必须同时设置 |
| `SHELL_API_MAX_TIMEOUT` | `300` | 单次命令超时上限(秒) |
| `SHELL_API_MAX_OUTPUT` | `1048576` | stdout / stderr 字节上限(`max_output_bytes` 不能超过它) |
| `SHELL_API_RATE_LIMIT` | `120` | 限流令牌桶补充速率(每客户端 IP 每分钟),按 IP 计数(不含端口);`GET /mcp` 长连接不计数 |
| `SHELL_API_RATE_BURST` | `60` | 限流令牌桶容量(每客户端 IP 可连续突发的请求数) |
| `SHELL_API_TRUSTED_PROXIES` | (空) | 可提供客户端 IP 的可信 TCP 代理,逗号分隔 IP/CIDR;默认忽略所有转发头。非法配置拒绝启动 |
| `SHELL_API_DEFAULT_SHELL` | `bash` | 默认 shell |
| `SHELL_API_FS_ROOT` | (空) | 文件操作沙箱根目录,支持逗号分隔多个目录前缀,留空则不限制(等同 shell 信任级别) |
| `SHELL_API_DISABLED_TOOLS` | (空) | 工具黑名单,逗号分隔的 MCP 工具名,启动时不注册,对客户端不可见 |
| `SHELL_API_LOG_LEVEL` | `info` | 日志级别 `debug` / `info` / `warn` / `error`;`GET /mcp` 长连接的访问行只在 `debug` 下输出 |
| `SHELL_API_MCP_HEARTBEAT` | `30` | MCP `GET /mcp` 推送流心跳(ping)间隔秒数,防止中间设备空闲断连;`0` 关闭 |
| `SHELL_API_CALL_BUDGET` | `50` | 单次 MCP 工具调用最长阻塞秒数,应小于客户端请求超时(TypeScript SDK 默认 60s)。超出的命令不被杀,返回 `state=running` + `execution_id` 后续取;等待返回 `state=pending` + `wait_id` 续等;搜索/聚合返回 `scan_id` 后续取；`follow_seconds` 也以此为上限 |
| `SHELL_API_SHUTDOWN_GRACE` | `30` | 收到 SIGTERM/SIGINT 后,执行中命令(含超出调用时长仍在跑的)的最长宽限秒数,超时后连同进程组一并杀掉 |
| `SHELL_API_JOB_DIR` | `$TMPDIR/remote-agent-proxy-jobs` | `remote_spawn` 后台任务的日志目录 |
| `SHELL_API_BLOCK_JUMP_HOST` | (空) | 设为 `true` 时拦截 `ssh` / `gssh` / `scp` / `sftp` / `sshpass` 跳板命令(在跳板机上执行的命令不进入本服务审计) |
| `SHELL_API_DENY_COMMANDS` | (空) | 自定义命令拦截 RE2 正则,命中即拒绝执行(作用于 execute / session_execute / spawn / wait_for 的 command_exit0);正则非法时拒绝启动 |

## 工具一览(35 个)

| 分组 | 工具 | 只读模式 |
| --- | --- | --- |
| 命令与会话 | `remote_execute` `remote_session_execute` `remote_execution_result` `remote_cancel` `remote_session_create` `remote_session_list` `remote_session_close` | ✗ |
| 命令与会话 | `remote_status` | ✓ |
| 后台任务与等待 | `remote_spawn` `remote_job_status` `remote_job_list` `remote_job_logs` `remote_job_kill` `remote_wait_for` | ✗ |
| 文件读写 | `remote_write_file` `remote_edit_file` `remote_upload_base64` | ✗ |
| 文件读写 | `remote_read_file` `remote_read_files` `remote_list_dir` `remote_stat` `remote_download_base64` | ✓ |
| 搜索、聚合、对比与日志 | `remote_search_content` `remote_aggregate` `remote_scan_result` `remote_find_files` `remote_diff` `remote_tail_log` | ✓ |
| 文件管理 | `remote_move_file` `remote_copy_file` `remote_delete_file` `remote_make_dir` | ✗ |
| 主机洞察 | `remote_list_processes` `remote_check_port` `remote_get_env_info` | ✓ |

"只读模式"列标 ✓ 的 15 个工具在 `SHELL_API_READONLY=true` 下仍然可用,标 ✗ 的 20 个会被强制下线。

下面按分组给出每个工具的参数与用法。参数表中"必填"列标 `*` 的为必填;示例为 MCP `tools/call` 的 `arguments` 内容。

### 命令与会话

#### `remote_execute`

执行单条 shell 命令,返回 `state` / exit code / stdout / stderr / 耗时。默认超时 30s。

**调用时长与命令时长解耦**:一次调用最多阻塞 `SHELL_API_CALL_BUDGET`(默认 50s,低于常见客户端 60s 请求超时)。命令在此之前结束则照常内联返回;仍在运行则**不杀**,立即返回 `state=running` 与 `execution_id`,用 `remote_execution_result` 续取最终结果——不要重发命令。客户端请求超时、取消或断连同样**不会**终止命令,只有它自己的 `timeout_ms`、`remote_cancel` 或服务退出会。最终 `state` 取值:`completed`(自然退出,看 `exit_code`)、`timed_out`(到 `timeout_ms` 被杀,`error` 写明阈值)、`cancelled`、`not_started`(被拦截/未能启动,什么都没执行)。

搜索、看日志、查文件优先用专用工具(结构化返回、零匹配不报错);需要等待某个条件时用 `remote_wait_for`,不要 `sleep N; check`;需要常驻后台的进程用 `remote_spawn`,不要 `nohup ... &`。

- 输出按 `max_output_bytes` 有界捕获(内存不随输出增长),返回 `stdout_total_bytes` / `stderr_total_bytes` 说明原始输出量
- 命令未能启动(工作目录不存在、shell 不存在、被拦截)时返回 `exit_code=-1` 与 `error` 说明原因
- shell 已退出但后台子进程仍占用输出管道时,最多再等 2s 即返回,不会卡到超时
- 超出调用预算仍在运行的命令,结束时审计日志再写一行最终状态(同一 `exec_id`)
- 响应可能带 `hint` 字段,建议更合适的专用工具(仅提示,不影响执行)

| 参数 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `command` | string | * | 要执行的命令,最长 10000 字符 |
| `working_directory` | string | | 工作目录,优先于 `cd X && ...` 前缀 |
| `environment` | object | | 追加的环境变量键值对 |
| `timeout_ms` | number | | 命令超时毫秒数,默认 30000,上限 `SHELL_API_MAX_TIMEOUT`;与调用预算无关 |
| `max_output_bytes` | number | | stdout/stderr 捕获上限,默认取服务端配置 |
| `truncate_mode` | string | | 超限保留 `head`(默认)或 `tail`(看日志更友好) |
| `output_encoding` | string | | 把输出从该编码转为 UTF-8:`utf-8`(默认,原样)、`gbk`、`gb2312`、`gb18030`、`auto`(合法 UTF-8 原样保留,否则按 GBK 解码) |
| `shell` | string | | 指定 shell,默认取服务端配置 |

```json
{"command": "systemctl status nginx", "timeout_ms": 10000, "truncate_mode": "tail"}
```

#### `remote_session_create` / `remote_session_list` / `remote_session_close`

管理持久会话:会话持有工作目录、shell 与环境变量,带 TTL 自动回收。

`remote_session_create` 参数:

| 参数 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `working_directory` | string | | 初始工作目录,默认为服务端用户 home |
| `shell` | string | | shell 路径,默认 bash |
| `environment` | object | | 会话级持久环境变量 |
| `ttl_seconds` | number | | 会话 TTL,默认 3600,最大 86400 |

```json
{"working_directory": "/data/project", "environment": {"ENV": "prod"}, "ttl_seconds": 7200}
```

`ttl_seconds` 是**空闲超时**:每次 `remote_session_execute` 都把过期时间续到"当前 + TTL"(结果里的 `session_expires_at`),长任务中持续使用的会话不会中途过期;过期会话连同 cwd/env 一起消失。`remote_session_list` 无参数,返回存活会话的 id / cwd / shell / `ttl_seconds` / 过期时间;`remote_session_close` 仅需 `session_id`(必填)。

#### `remote_session_execute`

在持久会话中执行命令。成功的裸 `cd <dir>` 会持久化会话 cwd,后续命令无需再带路径前缀。执行语义与 `remote_execute` 相同(超出调用预算返回 `state=running` + `execution_id`),并续期会话 TTL。

| 参数 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `session_id` | string | * | `remote_session_create` 返回的会话 ID |
| `command` | string | * | 要执行的命令;裸 `cd <dir>` 成功后更新会话 cwd |
| `environment` | object | | 本次调用追加的环境变量(覆盖会话级同名变量) |
| `timeout_ms` | number | | 超时毫秒数 |

```json
{"session_id": "s-xxxx", "command": "cd /var/log/nginx"}
```

#### `remote_execution_result`

取回超出调用预算的命令结果。传 `execution_id`:最多等 `wait_seconds`(默认且最大为调用预算)直到命令结束,返回完整结果;仍未结束则再次返回 `state=running`,继续调用即可。不传 `execution_id`:列出最近 30 分钟内最新的 20 条执行(含 `state` / `exit_code`,`total` 为总数)——调用在拿到任何响应前就被切断时,用它确认命令是否执行过、结果如何,而不是盲目重发。最近 16 条已结束执行保留 stdout/stderr。

```json
{"execution_id": "d8e32e24-3d3e-4434-a1ad-6c69a40bcd8e"}
```

#### `remote_cancel` / `remote_status`

`remote_cancel` 按执行 ID 取消运行中的命令(参数 `execution_id`,必填,来自 execute 的返回),返回取消后的状态;结果仍可用 `remote_execution_result` 查看。`remote_status` 无参数,返回服务健康、版本(含 `commit` / `build_time`)、运行时长、活跃会话数与系统信息。

### 后台任务与等待

#### `remote_spawn` / `remote_job_*`

`remote_spawn` 以独立会话(setsid)启动后台任务并**立即返回** `job_id`:任务不属于调用方进程组,不受调用超时影响,服务重启也不会被杀;stdout+stderr 写入服务端管理的日志文件(`SHELL_API_JOB_DIR`)。参数:`command`(必填)、`working_directory`、`environment`、`timeout_seconds`(到时杀掉整个进程组,默认不限)、`shell`。

| 工具 | 说明 |
| --- | --- |
| `remote_job_status` | `job_id` → 状态 `running` / `exited` / `killed` / `timed_out`、退出码、pid、耗时、日志路径与大小 |
| `remote_job_list` | 列出任务(新到旧),`running_only` 只看运行中;已结束任务保留 24h |
| `remote_job_logs` | 读任务日志,参数同 `remote_tail_log`(`lines` / `since_offset` / `follow_seconds` / `filter_regex` / `encoding`),附带任务状态 |
| `remote_job_kill` | 向任务整个进程组发信号,`signal` 取 `TERM`(默认)/ `INT` / `KILL` |

```json
{"command": "python3 -u long_job.py", "working_directory": "/data/work", "timeout_seconds": 3600}
```

#### `remote_wait_for`

服务端轮询,条件满足即返回(附匹配内容),取代 `sleep N; check`。等待以**服务端 watcher** 运行,最长 `timeout_seconds`(上限 24h),不受客户端请求超时限制:一次调用最多阻塞调用预算,届时未决则返回 `state=pending` + `wait_id`,再次调用只传 `wait_id` 即续等**同一个** watcher(`log_regex` 保留偏移,两次调用之间写入的行不会漏);`wait_id` + `cancel=true` 停止。最终 `state`:`satisfied` / `timed_out` / `cancelled`。

| condition | target | 说明 |
| --- | --- | --- |
| `file_exists` | 路径 | 文件出现 |
| `file_contains` | 路径 | 全文件任一行匹配 `pattern` |
| `log_regex` | 日志路径 | 调用开始**之后**追加的行匹配 `pattern`(文件截断/轮转后从头扫) |
| `process_exit` | pid 或 `job_id` | 进程 / 后台任务退出 |
| `port_listen` | 端口号 | 端口开始监听 |
| `command_exit0` | shell 命令 | 每个间隔重跑一次,直到退出码为 0 |

其他参数:`pattern`(RE2)、`ignore_case`、`timeout_seconds`(默认等于调用预算,最大 86400)、`interval_ms`(默认 1000,最小 200;`command_exit0` 最小 1000)、`encoding`(文件条件的源编码,如 `gbk`)、`wait_id`、`cancel`。

```json
{"condition": "log_regex", "target": "/data/app/log/app.log", "pattern": "init success|FATAL", "timeout_seconds": 5400}
{"wait_id": "wait-1a2b3c4d"}
```

### 文件读写

#### `remote_read_file`

读取远程文件并返回 UTF-8 内容。每次返回一个"窗口"(默认 64KB),大文件不会整体载入内存;自动探测编码(utf-8 / gbk,窗口切在多字节字符中间也不会误判),识别二进制文件(二进制请用 `remote_download_base64`)。

- 行区间流式定位:`start_line` / `end_line` 可读任意大小文件的任意行。
- 窗口不会在字符中间截断;未到文件尾时返回 `truncated: true`,并给出续读游标 `next_offset` / `next_start_line` 和 `hint`。
- 元数据字段排在 `content` 之前:客户端截断超大结果(Claude Code 只保留前 100,000 字符)时游标不会丢。

等效 shell 映射:`cat` = 只传 `path`;`sed -n 'X,Yp'` = `start_line`/`end_line`;`nl -ba` / `cat -n` = `line_numbers: true`;`tail -N` = `tail_lines=N`;`head -c N` = `max_bytes=N`。

| 参数 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `path` | string | * | 文件路径(绝对或相对) |
| `encoding` | string | | 源编码,缺省自动探测(utf-8/gbk/gb2312/gb18030) |
| `start_line` / `end_line` | number | | 1 起始止行号(含),0 或缺省表示文件头/尾;流式定位,不受 `max_bytes` 限制 |
| `tail_lines` | number | | 读最后 N 行(seek 实现,支持超大文件),优先于行区间 |
| `offset_bytes` | number | | 从该字节偏移开始读(如上次的 `next_offset`),默认 0 |
| `max_bytes` | number | | 本次读取字节上限,默认 64KB,最大 16MB |
| `truncate_mode` | string | | 超限保留 `head`(默认)或 `tail` |
| `line_numbers` | boolean | | 每行前加行号与制表符(`cat -n` 格式),默认 false |
| `lines` | array | | 只读这些行号(任意顺序,最多 1000 个),按行号升序返回并带行号;优先于其他模式。超出文件末尾的行号列在 `missing_lines` |
| `format` | string | | `json`(默认)或 `text`,见下文「文本格式输出」 |

```json
{"path": "/data/app/src/charge_model.cpp", "start_line": 600, "end_line": 760, "line_numbers": true}
```

#### `remote_read_files`

一次读取多个文件,替代 `for f in ...; do echo "=== $f"; cat $f; done`。最多 50 个文件共享一个 `max_bytes` 预算(默认 64KB):小文件优先完整返回,大文件分摊剩余预算并以 `truncated: true` 加续读游标返回(同 `remote_read_file`)。单个文件失败只在该项带 `error`,不影响其余文件;预算或数量耗尽时未读的文件列在 `not_read`。

| 参数 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `paths` | array | *¹ | 按顺序整文件读取;路径不存在且含 `*` `?` `[` 时按 glob 展开(只匹配根目录内的普通文件,排序) |
| `files` | array | *¹ | 每项可带窗口:`{path, start_line?, end_line?, tail_lines?, lines?, offset_bytes?, max_bytes?, encoding?}`,含义同 `remote_read_file` |
| `line_numbers` | boolean | | 所有文件都输出 `cat -n` 格式,默认 false |
| `max_bytes` | number | | 所有文件共享的内容预算,默认 64KB,最大 16MB |
| `format` | string | | `json`(默认)或 `text`,见下文「文本格式输出」 |

¹ `paths` 与 `files` 至少传一个。

```json
{"paths": ["/data/app/conf/*.conf"], "files": [{"path": "/data/app/src/main.cpp", "lines": [604, 620, 1658]}]}
```

#### `remote_write_file`

写入 UTF-8 文本,支持目标编码转换、追加模式、自动建父目录;返回 sha256/mode 写后自检,可选写后 lint。二进制请用 `remote_upload_base64`。`dry_run=true` 不写盘,返回将写入的字节数 / sha256 / 是否新建 / 权限,以及与现有内容的 unified diff(`preview.diff`,3 行上下文)。

| 参数 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `path` | string | * | 目标文件路径 |
| `content` | string | * | UTF-8 文本内容 |
| `encoding` | string | | 落盘编码:utf-8(默认)/gbk/gb2312/gb18030 |
| `append` | boolean | | 追加而非覆盖,默认 false |
| `make_dirs` | boolean | | 自动创建父目录,默认 false |
| `mode` | string | | 新文件权限(八进制),默认 `0644` |
| `lint` | string | | 写后语法检查:`bash`(bash -n)/`python`(py_compile),结果仅供参考 |
| `dry_run` | boolean | | 只预览不写盘,返回 `preview.diff`;默认 false |

```json
{"path": "/data/app/deploy.sh", "content": "#!/bin/bash\nset -e\n...", "mode": "0755", "lint": "bash"}
```

#### `remote_edit_file`

对远程文件做精确字符串替换,保留原编码与权限。支持多处编辑(原子:任一失败不落盘)、RE2 正则、dry-run 预览与写后 lint。

| 参数 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `path` | string | * | 文件路径 |
| `old_string` / `new_string` | string | | 单处编辑模式;`old_string` 须唯一(除非 `replace_all`) |
| `edits` | array | | 多处编辑:`[{old_string, new_string, replace_all?}]`,按序应用、原子落盘 |
| `replace_all` | boolean | | 单处模式:替换全部出现,默认 false |
| `use_regex` | boolean | | 把 `old_string` 当 RE2 正则,`new_string` 支持 `$1` 分组展开;隐含替换全部 |
| `dry_run` | boolean | | 只返回变更预览不写盘,默认 false |
| `encoding` | string | | 源编码,缺省自动探测,写回保持原编码 |
| `lint` | string | | 写后语法检查:`bash` / `python` |

```json
{"path": "/data/app/config.yaml", "edits": [{"old_string": "port: 8080", "new_string": "port: 9090"}, {"old_string": "debug: true", "new_string": "debug: false"}], "dry_run": true}
```

#### `remote_list_dir`

列目录,返回 name / type / size / mode / owner / group / mtime / symlink 目标——结构化版 `ls -la`。

| 参数 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `path` | string | * | 目录路径 |
| `sort_by` | string | | `name`(默认,升序)/ `mtime`(最新在前)/ `size`(文件按大小降序在前,目录排在其后) |
| `filter_glob` | array | | 只保留文件名匹配的条目,如 `["*.conf"]` |
| `include_hidden` | boolean | | 包含点文件,默认 false |

#### `remote_stat`

路径元数据:存在性、类型、大小、权限、owner/group、nlink、mtime、symlink 目标、二进制嗅探;可选内容哈希、编码探测与文本信息。一次调用替代 `stat -c ...; file -I; md5sum; wc -l` 一串命令;路径不存在返回 `exists=false` 而非报错。传 `paths` 可一次查询多个路径,每个结果单独带 `error`。

| 参数 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `path` | string | *¹ | 文件或目录路径 |
| `paths` | array | *¹ | 一次查询多个路径(最多 200 个),返回 `{count, results: [...]}` |
| `include_hash` | string | | `md5` / `sha256`,流式计算(常规文件 ≤256MB) |
| `include_encoding` | boolean | | 探测文本编码(utf-8/gbk/unknown) |
| `include_text` | boolean | | 文本信息(常规文件 ≤256MB):行数(同 `wc -l`,无结尾换行的末行也计入)、换行符 `lf` / `crlf` / `mixed` / `cr`、是否以换行结尾、BOM、最长行字节数 |

¹ `path` 与 `paths` 至少传一个。

```json
{"paths": ["/data/app/bin/server", "/data/app/conf/app.conf"], "include_hash": "sha256", "include_text": true}
```

#### `remote_upload_base64` / `remote_download_base64`

二进制与大文件的分块传输通道。

`remote_upload_base64`:`path`*、`data_b64`*(标准 base64)、`append`(分块续传)、`make_dirs`、`mode`(默认 0644);返回 sha256/mode 自检。

`remote_download_base64`:`path`*、`offset`(起始字节,默认 0)、`max_bytes`(本次上限,默认 4MB);返回中的 `eof` 标记最后一块。

```json
{"path": "/data/app/core.dump", "offset": 0, "max_bytes": 4194304}
```

### 搜索与日志

#### `remote_search_content`

服务端实现的 RE2 内容搜索——结构化版 `grep -rn`,目标机无需 grep/rg。**无匹配是成功结果(`files_matched: 0`)而非报错**;文件流式扫描(不整体载入),按行探测 GBK / UTF-8;跳过的文件按原因计数(`skip_reasons`:binary / too_large / unreadable / symlink_dir / outside_roots 等),默认不含隐藏文件。

注意 RE2 的交替是裸 `|`:GNU grep 的 `\|` 在 RE2 里匹配字面量 `|`。这类模式 0 命中时,`hint` 会提示改写;要搜字面文本用 `fixed_strings: true`。

| 参数 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `path` | string | * | 文件或目录(目录递归;作为 path 传入的目录软链会被跟随) |
| `pattern` | string | * | RE2 正则,如 `"FATAL\|ERROR\|failed"`(交替用裸竖线) |
| `output_mode` | string | | `content`(默认,命中行)/ `files_with_matches`(只列文件,同 `grep -l`)/ `count`(每文件命中数,同 `grep -c`) |
| `include_glob` | array | | 只搜匹配的文件:`*.conf` 匹配文件名,`conf/*.conf`、`**/*.conf` 匹配相对路径,`*.{h,cpp}` 多选 |
| `exclude_glob` | array | | 跳过匹配的文件和目录(目录不进入),如 `["log", "*.bak*"]` |
| `ignore_case` | boolean | | 忽略大小写,默认 false |
| `fixed_strings` | boolean | | 按字面文本搜索(同 `grep -F`),默认 false |
| `context_lines` | number | | 每个命中附前后各 N 行,默认 0,最大 20;重叠的上下文只输出一次 |
| `before_lines` / `after_lines` | number | | 分别指定前 / 后行数(同 `grep -B` / `-A`),覆盖 `context_lines` |
| `max_results` | number | | 每页明细数(命中行或文件),默认 200,最大 5000;输出另有 64KB 上限；count 模式仍扫描全量文件计算总数 |
| `offset` | number | | 跳过前 N 个结果:截断时传上次的 `next_offset` 翻页 |
| `max_file_size` | number | | 跳过超过该字节数的文件,默认 256MB |
| `include_hidden` | boolean | | 包含点文件/点目录,默认 false |
| `encoding` | string | | 强制源编码,缺省按文件、按行自动探测 |
| `format` | string | | `json`(默认)或 `text`(同 `grep -n` 的分组输出),见下文「文本格式输出」 |

`count` 的 `total_matches` 是命中行数（一行出现多次匹配仍计 1），`files_matched` 是命中文件数，`files[].count` 是本页各命中文件的行数。总数不受 `max_results` 或 `offset` 影响；即使明细 `truncated: true`，`counts_complete: true` 仍表示总数已扫完。`counts_complete: false` 表示超时、跳过文件或超长行影响了统计，文件中途停止时该项 `partial: true`。没有命中也明确返回 `total_matches: 0`。

超长行只返回命中附近约 2000 字节的片段；原行超过约 1MB 时只匹配前缀，`long_lines` 和 `hint` 会说明。搜索的 `timeout_ms` / `wait_ms` 与 `remote_scan_result` 用法见下文；扫描超出本次等待预算后继续运行，只有扫描自身超时或被取消才提前停止。

```json
{"path": "/data/app", "pattern": "FATAL|ERROR", "include_glob": ["*.log"], "exclude_glob": ["*.gz"], "before_lines": 3, "after_lines": 1}
```

#### `remote_aggregate`

服务端流式分列、过滤与数值聚合，一次扫描同时计算 `count`、`min`、`max` 和 top-N。读取范围、glob、编码识别、二进制文件跳过与软链接沙箱规则均与 `remote_search_content` 相同，不依赖 shell、awk 或目标机运行时，只读模式下可用。

| 参数 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `path` | string | * | 文件或递归扫描的目录 |
| `field` | number | * | 数值列号，从 1 开始，必须是整数，最大 1024 |
| `delimiter` | string | | 字面分隔符，如 `"|"`、逗号、制表符；省略或空串按连续空白分列，字面模式保留空列，不解析 CSV 引号；最大 256 字节 |
| `pattern` | string | | 可选整行 RE2 预过滤；省略时逐行处理全部文本 |
| `fixed_strings` | boolean | | 把行 pattern 按字面文本匹配 |
| `ignore_case` | boolean | | 行 pattern 及文本字段过滤忽略大小写 |
| `filters` | array | | 最多 32 个 `{field, op, value}` 条件，全部通过才保留该行；value 为字符串，保留大整数精度 |
| `top_n` | number | | top 明细数量，默认 10，最大 1000；不限制扫描量或 count/min/max |
| `sort_order` | string | | `desc`（默认）取最大 N 条，`asc` 取最小 N 条；数值并列时保留先出现的行 |
| `include_content` | boolean | | 在记录中附带最多约 200 字节的行片段，默认 false；默认仅返回原数值、文件和行号 |
| `include_glob` / `exclude_glob` | array | | 文件与目录过滤，语义与搜索工具相同 |
| `max_file_size` | number | | 跳过超过该字节数的文件，默认 256MB |
| `include_hidden` | boolean | | 扫描点文件和点目录，默认 false |
| `encoding` | string | | 强制源编码；省略时按文件、按行自动识别 |
| `timeout_ms` / `wait_ms` | number | | 整次扫描超时与本次等待时间，见下一节 |
| `format` | string | | `json`（默认）或 `text` |

字段过滤的 `eq` / `ne` / `contains` / `regex` 按文本比较，`gt` / `ge` / `lt` / `le` 按数值精确比较。数值支持符号、小数和科学计数法；尾数最多 4096 位、指数绝对值最大 10000，拒绝 NaN、Inf、十六进制和分数。目标数值字段会去掉两端空白，返回的 `value` 始终是字符串，保留其余原始写法，避免客户端把大整数转成浮点数后丢失精度。

`count` 是通过整行及字段过滤且目标字段为有效数值的行数；`lines_scanned` 是扫描行数，`lines_matched` 是通过整行及字段过滤的行数。缺列、无效数值和超过约 1MB 的行分别计入 `lines_skipped.missing_field` / `invalid_number` / `line_cut`；超长行整行跳过，不把截断的字段当作极值。没有有效数值时返回 `count: 0` 和空 `top`，省略 min/max。

`min` / `max` / `top[]` 均返回 `{value, file, line, content?}`，`scan_complete` 表示没有超时、跳过文件或超长行；`truncated` 表示扫描提前结束。top 明细另受约 64KB 响应窗口限制，超限时 `top_truncated: true`，总计数和极值仍保留，`hint` 说明范围。

```json
{"path": "/data/app/log", "include_glob": ["*.log*"], "delimiter": "|", "field": 7, "filters": [{"field": 3, "op": "eq", "value": "charge"}, {"field": 7, "op": "gt", "value": "100"}], "top_n": 20, "timeout_ms": 900000, "wait_ms": 0}
```

该例在服务器上过滤第 3 列为 charge、且第 7 列大于 100 的记录，统计有效行数及第 7 列的最小值、最大值和最大 20 条记录。

#### 长扫描与 `remote_scan_result`

`remote_search_content` 和 `remote_aggregate` 共用以下参数，完整扫描无需由调用方按天拆批：

| 参数 | 类型 | 说明 |
| --- | --- | --- |
| `timeout_ms` | number | 扫描自身的总超时，默认 300000（5 分钟），范围 1–3600000（1 小时），独立于命令超时配置 |
| `wait_ms` | number | 本次调用等待时间；默认接近调用预算并预留响应时间，超出会被限制；0 表示立即返回 |

扫描完成时保持原工具的顶层结果字段，额外返回 `scan_id`、`tool`、`state`、`started_at`、`elapsed_ms`、`timeout_ms`。尚未完成时返回 `state: "running"` 与 `scan_id`，用以下调用继续等待；续取可切换 `format: "json"` / `"text"`。客户端断连或请求取消不会终止任务，也不应重新提交同一查询。

```json
{"path": "/data/app/log", "pattern": "ERROR", "include_glob": ["app.log.2026110{1,2,3}*"], "output_mode": "count", "timeout_ms": 900000, "wait_ms": 0}
```

```json
{"scan_id": "上次返回的 scan_id", "wait_ms": 45000, "format": "text"}
```

`remote_scan_result` 的参数为 `scan_id`（省略时列出近期任务及路径）、`wait_ms`（0 只查状态）、`cancel`（true 时取消此读取任务，需要 scan_id）和 `format`。任务状态为 `running` / `completed` / `timed_out` / `cancelled` / `failed`；超时或取消时保留已经计算的部分结果，`failed` 带错误。`completed` 表示任务正常结束，仍需检查 `counts_complete`、`skip_reasons`、`truncated` 等结果字段确认覆盖范围。

最多同时运行 4 个扫描；已完成结果最多保留最近 16 份，完成后保留 30 分钟，服务重启后清空。列表可用于找回丢失回复后的 scan_id，重复取结果不会重新读取文件。服务退出会取消在途只读扫描。如果通过黑名单禁用 `remote_scan_result`，搜索和聚合退回调用预算内的同步扫描，超时返回部分结果，不留下无法取回的后台任务。`remote_diff`、`remote_find_files` 保持各自原有的扫描方式。

#### `remote_find_files`

按名称、时间、大小查找——结构化版 `find -name/-iname/-newer/-size`,受 FSRoot 约束(杜绝 `find /`),显式返回截断标记。

| 参数 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `path` | string | * | 递归查找的根目录(作为 path 传入的目录软链会被跟随) |
| `name_glob` | array | | glob 规则同 `include_glob`,如 `["*.cpp", "*.h"]`、`["conf/**/*.conf"]`;留空匹配全部 |
| `exclude_glob` | array | | 跳过匹配的条目(目录不进入) |
| `type` | string | | `file` / `dir` / `symlink` / `any`(默认) |
| `max_depth` | number | | 最大目录深度(1 = 仅直接子项),默认不限 |
| `newer_than` / `older_than` | string | | 修改时间晚于 / 早于:RFC3339 时间,或 `30m`、`2h`、`7d` 这类时长 |
| `min_size` / `max_size` | number | | 文件大小范围(字节);设置后只返回大小符合的普通文件,不返回目录 |
| `sort_by` | string | | `path`(默认,遍历顺序)/ `mtime`(最新在前)/ `size`(最大在前) |
| `max_results` | number | | 上限,默认 500,最大 10000;`truncated` 标记是否有更多 |
| `ignore_case` | boolean | | 忽略大小写(等效 `find -iname`),默认 false |
| `include_hidden` | boolean | | 包含点文件/点目录,默认 false |

```json
{"path": "/data/app/logs", "name_glob": ["*.log*"], "newer_than": "2h", "sort_by": "mtime", "max_results": 20}
```

#### `remote_diff`

在服务端比较两个文件或两个目录树,替代 `diff` / `cmp` 和对两套部署逐个 `sha256sum`。

- 两个文件:返回从 `left` 到 `right` 的 unified diff(3 行上下文)。GBK 与 UTF-8 都会先解码,所以只改了编码的文件显示为文本相同,并在 `encoding` 中标出 `utf-8/gbk`。
- 两个目录:返回相对路径列表,`added`(只在 right)、`removed`(只在 left)、`changed`(两边都有但内容不同,先比大小再比 SHA-256)、`type_changed`,以及相同条目数 `same_count`。`show_diffs: true` 时附带最多 20 个变更文本文件的 diff。
- 超过 8MB 的文件和二进制文件只比较内容是否相同,不给行级 diff。
- 目录模式接近调用预算时停止读取文件内容,已比较的结果照常返回,`added` / `removed` 列表仍然完整,并以 `truncated: true` 和 `hint` 说明有多少两侧都存在的文件未比较。

| 参数 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `left` | string | * | 旧文件或目录(`-` 侧) |
| `right` | string | * | 新文件或目录(`+` 侧) |
| `ignore_space` | boolean | | 忽略行内空白数量的变化(同 `diff -b`),默认 false |
| `exclude_glob` | array | | 目录模式:跳过匹配的条目,如 `["log", "*.bak*", "data/**"]` |
| `include_hidden` | boolean | | 目录模式:包含点文件/点目录,默认 false |
| `show_diffs` | boolean | | 目录模式:附带变更文本文件的 diff,默认 false |
| `max_bytes` | number | | diff 文本上限,默认 64KB |
| `format` | string | | `json`(默认)或 `text`,见下文「文本格式输出」 |

```json
{"left": "/ssd1/toad_policy_test/toad_policy_old/conf", "right": "/home/work/toadpolicy2dianshang/conf", "exclude_glob": ["*.bak*"], "show_diffs": true}
```

#### `remote_tail_log`

读(可能巨大且持续增长的)日志尾部,不载入内存。替代 `tail -n N`、`grep X log | tail -n N`(`lines=N` 加 `filter_regex=X`:从文件尾倒序扫描,返回最后 N 条**匹配行**)、`tail -n +N`(用 `since_line=N-1`)、`sleep 70; tail`(用 `follow_seconds=70`)。支持正则过滤与两种增量游标;轮询推荐 `since_offset`(最省,取上次返回的 `end_offset`);文件轮转(变小)时自动从头重读并置 `rotated=true`。

| 参数 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `path` | string | * | 日志文件路径 |
| `lines` | number | | 尾部模式:返回最后 N 行(带 `filter_regex` 时为最后 N 条匹配行),默认 100,最大 10000 |
| `filter_regex` | string | | RE2 过滤,只返回匹配行(交替用裸竖线);游标推进不受过滤影响 |
| `since_line` | number | | 行游标:返回该行号(1 基)之后的行(取上次 `end_line`) |
| `since_offset` | number | | 字节游标:返回该偏移之后的内容(取上次 `end_offset`),优先于 `since_line` |
| `follow_seconds` | number | | 最多等待 N 秒新内容(上限为调用预算);无新内容时 `timed_out=true`,用 `since_offset=end_offset` 再调用继续跟 |
| `encoding` | string | | 源编码,缺省自动探测 |
| `max_bytes` | number | | 收集内容字节上限,默认 64KB;字节游标模式下达到上限时在行边界截断,`hint` 给出续读偏移 |
| `format` | string | | `json`(默认)或 `text`,见下文「文本格式输出」 |

```json
{"path": "/data/app/logs/app.log", "filter_regex": "ERROR", "follow_seconds": 60}
```

### 文件管理

#### `remote_move_file` / `remote_copy_file`

移动/重命名(跨设备自动降级为 copy+remove)与复制(保留权限;目录树递归,**拒绝合并已有目录**)。`src` / `dst` 均必填且都须在 FSRoot 内;`overwrite`(默认 false)允许替换已存在的目标。

```json
{"src": "/data/app/config.yaml.bak", "dst": "/data/app/config.yaml", "overwrite": true}
```

#### `remote_delete_file`

删除文件或目录树。**仅当设置了 `FSRoot` 时才会注册**;必须 `confirm: true` 才真正删除;建议先 `dry_run: true` 预览影响范围;拒绝删除 FS 根本身。

| 参数 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `path` | string | * | 待删除路径(文件或目录) |
| `recursive` | boolean | | 删除非空目录时必填 true |
| `dry_run` | boolean | | 只列出将删除的条目,不实际删除 |
| `confirm` | boolean | * | 必须为 true 才真正执行删除 |

```json
{"path": "/data/app/tmp", "recursive": true, "dry_run": true}
```

#### `remote_make_dir`

创建目录(默认 `mkdir -p` 语义)。参数:`path`*、`mode`(八进制,默认 `0755`)、`parents`(默认 true)。

### 主机洞察

#### `remote_list_processes`

进程枚举(pid/ppid/user/state/elapsed/cmd/cmdline),替代 `ps -ef | grep`。Linux 读 `/proc`,macOS 走 `ps`,Windows 返回 unsupported。

| 参数 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `filter` | string | | RE2 正则,匹配完整命令行,如 `"nginx\|postgres"` |
| `user` | string | | 只看该用户的进程 |

```json
{"filter": "mysqld|redis-server"}
```

#### `remote_check_port`

查监听端口及其属主进程,替代 `ss -lntp | grep` / netstat / lsof;无匹配返回空列表。

| 参数 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `port` | number | | 端口号;0 或缺省列出全部监听端口 |
| `process_name` | string | | 属主进程名的大小写不敏感子串过滤 |

```json
{"port": 3306}
```

#### `remote_get_env_info`

一次性环境画像,无参数:OS/内核/架构、主机名、用户、shell、locale、内存/负载、36 个常用工具链(python3、rg、rsync、mysql 等)的可用性与版本、服务端配置(只读模式标志、FSRoots、工具黑名单、限额、可信代理)。会话开局调一次,替代一串 `which` / `--version` / `uname` 探测。

## 安全机制

### 文本格式输出(format=text)

`remote_read_file`、`remote_read_files`、`remote_search_content`、`remote_tail_log`、`remote_diff` 支持 `format: "text"`。默认的 `json` 格式返回结构化结果;`text` 格式先给一行方括号头部(行号范围、字节范围、编码、`truncated`、`next_offset` / `next_start_line` 等游标),后面是纯文本内容:读取结果与 `cat -n` 相同,搜索结果与 `grep -n` 相同(文件名分组,`N:` 为命中行,`N-` 为上下文,不相邻的组之间用 `--` 分隔),目录对比与 `diff -rq` 相近并附 unified diff。

文本格式的结果只含 `text` 内容,不含 `structuredContent`。原因是 Claude Code 在结果带 `structuredContent` 时只把它的 JSON 交给模型,文本内容会被忽略。JSON 里的文件内容要转义换行、制表符和引号,搜索结果的每条命中还要重复文件路径和字段名。按模型实际看到的形式端到端实测:100 条带上下文的搜索结果,文本约为 JSON 的 57%;过滤后的日志尾部约 76%;160 行的读取窗口约 97%(内容本身占大头,主要收益是可读性)。

```json
{"path": "/data/app/src", "pattern": "set_coupon_rate", "include_glob": ["*.{h,cpp}"], "context_lines": 2, "format": "text"}
```

### 只读模式(ReadOnly)

`SHELL_API_READONLY` 是本服务**优先级最高**的开关。开启后服务退化为纯读取代理:只保留 15 个读取类工具,命令执行、会话、写 / 编辑 / 删除 / 移动 / 复制 / 建目录全部下线,且**运行时无法恢复**——它不受 `SHELL_API_DISABLED_TOOLS`、请求参数或任何后续配置影响。

```bash
# 一键开启(取值 1 / true / yes / on / enabled,大小写不敏感)
SHELL_API_TOKEN=xxx SHELL_API_READONLY=true ./remote-agent-proxy

# 启动脚本方式
./start.sh start-ro          # 后台启动
./start.sh foreground-ro     # 前台启动
./start.sh restart-ro        # 只读模式重启
./start.sh readonly-check    # 校验只读是否真正生效
```

两层独立强制,任何单层失效都不会打开写通路:

| 层 | 位置 | 作用 |
| --- | --- | --- |
| 1. 工具注册 | `config.go` `toolEnabled` + 各 `register*` | 20 个写工具**不注册**,`tools/list` 里根本不存在 |
| 2. 操作兜底 | `readonly.go` `guardReadOnly` | `WriteFileContent` / `EditFileContent` / `UploadBase64` / `MoveFile` / `CopyFile` / `DeleteFile` / `MakeDir` / `Executor.Execute` / `LintFile` 内部直接失败 |

第 1 层用的是**白名单**(`readonly.go` 的 `readOnlyTools`):将来新增的工具在未被显式审计并加入白名单前一律禁用,漏改代码的后果是"少一个读工具",而不是"多一个写通路"。第 2 层保证即使注册层被绕过(例如后续误加了一个未走 `toolEnabled` 的注册点),底层操作仍然拒绝执行。

只读模式下允许的 15 个工具:

```
remote_read_file  remote_read_files  remote_list_dir  remote_stat
remote_search_content  remote_aggregate  remote_scan_result
remote_find_files  remote_diff  remote_tail_log
remote_download_base64  remote_list_processes  remote_check_port
remote_get_env_info  remote_status
```

可与黑名单叠加进一步收窄(只能更严,不能更宽):

```bash
SHELL_API_READONLY=true \
SHELL_API_DISABLED_TOOLS=remote_download_base64,remote_list_processes \
./remote-agent-proxy      # 最终只剩 13 个工具
```

模式对客户端是自描述的:MCP handshake 里 server name 变为 `remote-shell-readonly`,instructions 写明主机名、FSRoot、可用工具清单和读取约定(分页游标、RE2 交替写法、glob 规则);每个只读工具都带 `readOnlyHint: true` / `destructiveHint: false` 注解(mcp-go 默认是相反值,Claude Code 据此判断工具能否并发执行),`remote_status` 与 `remote_get_env_info` 返回 `read_only: true`。

**建议同时设置 `SHELL_API_FS_ROOT`**:只读模式管住"能不能改",FSRoot 管住"能读到哪";两者正交。若要更强的保证,可再用一个对目标目录只有读权限的系统账户运行本服务。

### 文件沙箱(FSRoot)

`SHELL_API_FS_ROOT` 把**所有文件操作工具**(读 / 写 / 编辑 / 列目录 / stat / 搜索 / 日志 / base64 上传下载 / 移动 / 复制 / 删除 / 建目录)限制在指定目录内。原理是:每次操作前将传入路径解析为绝对路径并 `Clean`(消除 `..`、`./`),校验它落在允许的根目录之下,再解析路径上的全部软链接,校验真实路径同样落在根目录(解析后的根目录)之下。借此拦截路径穿越(`../../etc/passwd`)、前缀欺骗(`/data/projectxxx`),以及根目录内指向外部的软链接。

- 软链接本身可以 `remote_stat`(目标在根外时返回 `target_outside_roots: true`,不暴露目标内容)、删除或移动;读取、写入、搜索则按真实路径校验。搜索遍历不进入目录软链:指向根外的文件或目录计入 `skip_reasons.outside_roots`;指向根内的目录软链计入 `symlink_dir` 并列在 `symlink_dirs`(可把该目录作为 `path` 单独搜索)。若某个软链目录确需可读,把它的真实路径加入 `SHELL_API_FS_ROOT`。

- 支持**多个目录前缀**,用逗号分隔;路径只要命中任意一个根目录即放行:

  ```bash
  SHELL_API_FS_ROOT=/data/project,/tmp/workspace ./remote-agent-proxy
  ```

- 留空则不做任何限制,文件操作可覆盖整台主机(与 shell 执行同等信任级别)。
- **注意①**:该沙箱只约束文件操作工具,不约束 `remote_execute` / `remote_session_execute`;shell 执行本身即完整权限,如需限制请配合工具黑名单。
- **注意②**:`remote_delete_file` 仅在设置了 `FSRoot` 时才会注册;删除动作还要求 `confirm: true`,支持 `dry_run` 预览,且拒绝删除 FS 根目录本身。

### 工具黑名单(DisabledTools)

`SHELL_API_DISABLED_TOOLS` 是一份逗号分隔的工具名单,列出的工具在启动时**不会注册**,对客户端完全不可见(而非运行时拒绝)。可用于按最小权限裁剪服务能力,做精细化的能力剪裁:

```bash
# 保留命令执行但禁掉删除与上传
SHELL_API_DISABLED_TOOLS=remote_delete_file,remote_upload_base64 ./remote-agent-proxy
```

可禁用的工具名即上文列出的 35 个。

如果目标就是"只读服务",**请直接用 `SHELL_API_READONLY=true`**,不要靠手写黑名单:黑名单只作用于 MCP 工具注册,漏写一个工具名就可能留下一条写通路。

### 可信代理与客户端 IP

`SHELL_API_TRUSTED_PROXIES` 指定哪些 TCP 对端可以提供客户端 IP,支持 IPv4、IPv6 和 CIDR。单个地址按 `/32` 或 `/128` 处理,CIDR 会规范化到网段,IPv4-mapped IPv6 会统一为 IPv4。默认列表为空,即使请求带有转发头,仍只使用 TCP 对端地址。列表中的非法条目会导致启动失败,不会静默跳过。

```bash
# 仅信任本机 nginx 的回环来源,不默认信任整个内网
SHELL_API_TRUSTED_PROXIES=127.0.0.1/32,::1/128 ./remote-agent-proxy
```

解析规则:

1. 先检查 TCP 对端是否匹配可信代理列表;不匹配时忽略全部转发头。
2. 优先解析 `X-Forwarded-For`。多个同名头按顺序合并,所有值必须是纯 IP;从右向左跨过可信代理,取遇到的第一个不可信地址。若整条链都可信,取最左侧地址。
3. 只有 `X-Forwarded-For` 不存在时,才读取单个 `X-Real-IP`。
4. 空值、非法 IP、`unknown`、端口、引号、IPv6 zone 等格式错误会回退到 TCP 对端。已存在但非法的 `X-Forwarded-For` 不会再让 `X-Real-IP` 覆盖;重复的 `X-Real-IP` 也不接受。`Forwarded` 等其他头不参与解析。

客户端 IP 在请求进入日志和限流前解析一次,同时供 MCP 上下文及异步执行审计使用,不会改写原始 `RemoteAddr`。因此代理后的不同客户端拥有独立限流桶,访问日志和审计日志中的 `remote` 也保持一致。启动日志和 `remote_get_env_info.server.trusted_proxies` 会显示规范化后的可信代理配置(未配置时环境画像中省略该字段)。

只配置实际的代理地址或必要的最小网段,不要使用 `0.0.0.0/0`、`::/0` 或把所有内网客户端都标为代理。代理应覆盖外部传入的来源头,或在已验证的代理链末尾追加真实对端;否则真实来源无法可靠判定。该配置只控制来源头的信任,不替代 Bearer 鉴权,也不限制服务监听地址或阻止后端端口被直接访问。

### 审计日志

服务的所有日志都是一行一个 JSON 对象(`log/slog`),输出到 stderr。两类记录通过 `req_id` 关联,同一请求的 `req_id` 也在响应头 `X-Request-Id` 中返回:

- **访问日志** `type=access`:`req_id / remote / method / path / status / bytes / dur_ms`,MCP 请求另带 `mcp_method`(`initialize` / `tools/list` / `tools/call` …)、`tool` 与 `mcp_session`。401 / 429 也会记录。`GET /mcp` 长连接只在 `SHELL_API_LOG_LEVEL=debug` 下记录
- **审计日志** `type=audit`:每次工具调用一行,由统一的 MCP 工具中间件写入,字段 `req_id / exec_id / remote / mcp_session / client / tool / session_id / command / working_directory / exit_code / dur_ms / output_bytes / truncated / timed_out / state / error`。`client` 是 MCP 握手 `clientInfo` 的 `name/version`;超出调用预算的命令先记一行 `state=running`,结束时再以同一 `exec_id` 记一行最终状态。`tool` 一律为注册工具名,`remote` 为统一解析后的客户端 IP(不含端口;无可信转发来源时使用 TCP 对端),失败时 `error` 给出原因

```json
{"time":"...","level":"INFO","msg":"audit","type":"audit","req_id":"r-4497e438","remote":"198.51.100.24","tool":"remote_execute","command":"echo hi","exit_code":0,"dur_ms":5,"output_bytes":3,"timed_out":false,"exec_id":"04034b32-...","mcp_session":"mcp-session-..."}
```

- `command` 字段在落盘前自动脱敏:`mysql -p<密码>`、`Authorization: Bearer <token>`、`password= / token= / secret= / api_key=` 等形态替换为 `***`

## 安全须知

本服务在默认(读写)模式下,在网络边界上等同于一个交互式 shell,部署前请务必:

1. **只需要读就开只读模式** 设 `SHELL_API_READONLY=true`,这是唯一能真正关闭全部写通路的开关,优先级高于其他一切配置
2. **启用 TLS**,避免 Token 与命令内容在传输中泄露。注意哈希存储(`SHELL_API_TOKEN_HASH`)只保护服务端落盘的配置,客户端发送的仍是明文 Bearer,传输层的保护仍需 TLS
3. **使用强随机 Token**,不要硬编码进代码仓库;并把 `SHELL_API_TOKEN` 换成 `SHELL_API_TOKEN_HASH`(盐化 SHA-256,`printf '%s' 'your-token' \| ./remote-agent-proxy hash-token` 生成),这样服务器上的 `.env` / `env.conf` / 环境变量泄露时攻击者拿到的只是哈希,既不能反推明文也不能直接登录。若坚持使用人类可记的弱口令,请知悉:泄露的哈希可被离线穷举,此时应改用 `openssl rand -hex 32` 生成的高熵 Token
4. **设置 `FSRoot`** 把文件操作限制在指定目录,缩小爆炸半径;可传入多个目录前缀
5. **按需启用工具黑名单** 用 `SHELL_API_DISABLED_TOOLS` 禁掉不需要的能力,遵循最小权限原则
6. **不要暴露到公网**,应放在 VPN / 内网之后,或配合反向代理与额外鉴权
7. 将 `type=audit` 审计日志做集中收集与审计;如不希望经本机跳转到其他主机执行命令,开启 `SHELL_API_BLOCK_JUMP_HOST=true`
8. 用**只有读权限的系统账户**运行服务,作为与应用层无关的兜底

如发现安全漏洞,请通过 GitHub Security Advisory 私下报告,不要直接开公开 Issue。

## 开发

```bash
# 运行全部测试(文件操作 / 搜索 / 日志 / 删除安全 / 脱敏 / 进程解析 / 只读模式等)
go test ./...

# 仅运行只读模式相关测试
go test -run 'ReadOnly' -v ./...

# 跨平台编译
GOOS=linux GOARCH=amd64 go build -o remote-agent-proxy
```

### 构建脚本

`build.sh` 封装了多平台交叉编译,产物统一输出到 `dist/`(命名 `remote-agent-proxy-<版本>-<os>-<arch>`,版本号自动从 `types.go` 提取)。构建时通过 `-ldflags -X` 注入 `serverVersion` / `gitCommit`(工作区有未提交改动时带 `-dirty`)/ `buildTime`,运行态可用 `./remote-agent-proxy --version`、`remote_status`、`remote_get_env_info` 或 MCP 握手的 `serverInfo.version` 查看:

```bash
./build.sh                 # 构建当前平台
./build.sh linux           # linux/amd64(只给 os 时 arch 默认 amd64)
./build.sh linux/arm64     # 指定 os/arch
./build.sh linux windows   # 一次构建多个目标
./build.sh all             # 常用平台全套(linux/darwin/windows)
./build.sh list            # 列出预设目标平台
./build.sh --help          # 帮助
```

- linux / windows 目标以 `CGO_ENABLED=0` 静态编译,可在任意主机交叉编译,拷到低版本 glibc 机器(如 CentOS 7)也能直接跑。
- darwin 目标依赖 cgo(`sysinfo_darwin.go`),只能在 macOS 主机构建;在非 macOS 主机会自动跳过。
- 环境变量:`OUT_DIR` 改输出目录、`STRIP=0` 保留调试信息、`VERSION` 覆盖版本号。

### 部署机控制脚本

`control.sh` 是部署机上使用的控制脚本(部署目录放 `control.sh` + 二进制 + 可选的 `env.conf`):`start` / `stop`(SIGTERM 优雅退出,等待 `SHELL_API_SHUTDOWN_GRACE + 5` 秒后兜底 `kill -9`)/ `restart` / `status`(进程状态和 MCP 端点可达性)/ `foreground` / `build`(注入版本信息)/ `version`(磁盘上的二进制版本) / `token` / `mcp-prompt`(输出客户端添加服务的 prompt)。`start.sh` 面向本地开发,额外带只读模式相关子命令。脚本的端点检查面向本机 HTTP 部署;启用 TLS 时应通过对应的 HTTPS MCP 地址检查。

`upgrade.sh` 保留并排目录、金丝雀验证和自动回滚流程,探测使用 `curl` 或 `wget` 发送 MCP `initialize`,并核对 `serverInfo.version`。哈希模式示例:

```bash
# 凭据由调用环境提供,不要写进脚本或命令历史
# CHECK_TOKEN: 新服务的明文 Token;OLD_CHECK_TOKEN: 旧服务 Token 不同时设置
bash upgrade.sh /path/to/old /path/to/new 8099 18099 1.7.0
```

### nginx HTTPS 反向代理示例

以下域名、证书路径和凭据均为占位示例,后端使用默认端口 8080。nginx 负责 TLS 终止,通过同机 HTTP 访问 MCP;应用仍验证客户端 Bearer Token。

服务端 `env.conf` 示例(哈希值须替换为 `hash-token` 子命令实际生成的结果):

```bash
SHELL_API_PORT=8080
SHELL_API_TOKEN_HASH='sha256:<SALT_HEX>:<HASH_HEX>'
SHELL_API_TRUSTED_PROXIES=127.0.0.1/32,::1/128
SHELL_API_TLS_CERT=
SHELL_API_TLS_KEY=
```

nginx 示例:

```nginx
server {
    listen 443 ssl;
    server_name mcp.example.com;

    ssl_certificate     /etc/nginx/ssl/example/fullchain.pem;
    ssl_certificate_key /etc/nginx/ssl/example/privkey.pem;
    ssl_protocols TLSv1.2 TLSv1.3;

    location = /mcp {
        # 不附加 URI 或尾斜杠,保留后端的 /mcp 路径。
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Connection "";

        proxy_set_header Host $host;
        proxy_set_header Authorization $http_authorization;
        # 单层入口代理覆盖来源头,不保留客户端伪造的地址链。
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $remote_addr;
        proxy_set_header X-Forwarded-Proto $scheme;

        proxy_buffering off;
        proxy_cache off;
        gzip off;
        proxy_connect_timeout 5s;
        proxy_read_timeout 3600s;
        proxy_send_timeout 3600s;
        send_timeout 3600s;
        proxy_intercept_errors off;
        proxy_next_upstream off;
        client_max_body_size 32m;
    }
}
```

Streamable HTTP 使用 JSON-RPC 和 SSE,不需要 WebSocket Upgrade 配置。请求与响应中的 `Mcp-Session-Id`、协议版本等头按 nginx 默认规则透传。`proxy_buffering off` 保证事件和心跳及时送达;`proxy_read_timeout` 是上游读取的空闲超时,不会改变客户端超时或命令的 `timeout_ms`。32m 限制针对单次 JSON 请求,上传分块应计入 base64 膨胀。

已有 HTTPS 站点只需把 location 加入原 server 块,或在该 server 块内 include 片段;不要将裸 location 放入 http 层的 `conf.d/*.conf`。TLS 由 nginx 处理时,后端的证书和私钥变量同时留空。程序目前仍监听所有地址,应通过防火墙或安全组限制后端端口直连;可信代理配置本身不会收紧监听范围。

客户端直接连接 `https://mcp.example.com/mcp`,继续发送 `Authorization: Bearer <CLIENT_TOKEN>`。应用会使用可信 nginx 提供的来源 IP 分别限流和审计。若 nginx 前面还有代理/CDN,需单独核实并配置该链路的真实地址来源,不能直接套用任意外部转发头。

应用 nginx 配置前执行 `nginx -t`,检查通过后再重载。无凭据请求应返回 401;进一步通过 MCP initialize、tools/list 和可用的 remote_status 验证协议及工具调用,并检查日志中的 `remote` 是否为预期客户端 IP。

### 输出添加 MCP 的 agent prompt

部署机上执行以下命令,将输出整段复制到需要添加服务的 agent 终端:

```bash
./control.sh mcp-prompt https://mcp.example.com/mcp example-mcp
```

也可在 `env.conf` 中预先设置脚本专用的 URL 和服务名,之后一条命令输出:

```bash
MCP_PUBLIC_URL=https://mcp.example.com/mcp
MCP_SERVER_NAME=example-mcp
# 将上述两项写入 env.conf 后执行:
./control.sh mcp-prompt
```

命令参数优先于 `MCP_PUBLIC_URL` / `MCP_SERVER_NAME`,服务名默认 `remote-shell`,仅允许字母、数字、下划线和连字符。URL 必须是对客户端可达的完整 HTTP/HTTPS MCP 地址,脚本不推测公网地址。该命令仅输出 prompt,不会启动/重启服务、修改客户端配置或发起网络请求。

输出包含 Streamable HTTP 类型、URL、服务名、Authorization 请求头及握手验证要求,供 agent 按所在客户端支持的配置格式添加。明文模式自动使用 `SHELL_API_TOKEN`;如提供 `CHECK_TOKEN`,则优先使用它。哈希模式可通过临时客户端环境变量补全凭据:

```bash
# CLIENT_TOKEN 是占位符,实际使用客户端持有的明文 Token。
CHECK_TOKEN='<CLIENT_TOKEN>' ./control.sh mcp-prompt https://mcp.example.com/mcp example-mcp
```

未取得明文 Token 时,输出 `Bearer <CLIENT_TOKEN>` 占位符并要求 agent 在添加前交互获取。脚本不会输出或把 `SHELL_API_TOKEN_HASH` 当成 Bearer 凭据。`CHECK_TOKEN` 仅供本次操作使用,不要为此将客户端明文 Token 长期写入哈希模式的服务端配置。带有真实 Token 的输出应仅用于受信任的客户端配置,不要提交到代码仓库或公开分享。

仅依赖标准库加少量三方包(`google/uuid`、`mark3labs/mcp-go`、`golang.org/x/text`),无外部运行时依赖,单二进制部署。

## 贡献

欢迎 Issue 与 Pull Request。提交 PR 前请确保:

- `go test ./...` 全部通过,新功能附带测试
- `go vet ./...` 无告警,代码经 `gofmt` 格式化
- 涉及新工具时同步更新本 README 的工具清单

## License

[MIT](./LICENSE)
