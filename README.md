# remote-cli

[![Go Version](https://img.shields.io/github/go-mod/go-version/chuanqq/remote-cli)](https://go.dev/)
[![Go Report Card](https://goreportcard.com/badge/github.com/chuanqq/remote-cli)](https://goreportcard.com/report/github.com/chuanqq/remote-cli)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](./LICENSE)

**English:** A single-binary remote shell & file-operations server written in Go, exposing both a REST API and an MCP (Streamable HTTP) endpoint on the same port — designed to let AI agents (such as Claude Code) securely operate remote hosts over the network, with 24 dedicated tools that cover search, log tailing, process/port inspection and file management without relying on shell commands.

一个用 Go 编写的远程 Shell 执行与文件操作服务,同时提供 REST API 和 MCP(Streamable HTTP)两种接入方式,专为让 AI Agent(如 Claude Code)通过网络安全地管理远程主机而设计。内置 24 个专用工具,覆盖内容搜索、日志跟踪、进程/端口洞察与文件管理,绝大多数远程操作无需再拼 shell 命令。

同一个进程、同一个端口同时承载 REST 与 MCP,共用 TLS 和 Bearer Token 鉴权;仅依赖少量三方库,编译产物为单二进制,零外部运行时依赖。

## 特性

- **命令执行**:同步执行、SSE 流式输出、可取消,基于进程组(SIGKILL)的硬超时控制,输出截断可选保留头部或尾部(`truncate_mode`)
- **持久会话**:维护 cwd / shell / 环境变量,`cd` 后自动同步工作目录,带 TTL 自动回收;REST 与 MCP 均可创建/列出/销毁会话
- **文件操作**:读 / 写 / 编辑 / 列目录 / stat / 移动 / 复制 / 删除 / 建目录,支持 UTF-8 / GBK / GB2312 / GB18030 编码互转与自动探测,大文件支持 base64 分块上传下载;写入类操作返回 sha256/mode 自检,可选 bash/python 语法 lint
- **内容搜索与日志**:服务端实现的内容正则搜索(`remote_search_content`)、文件名查找(`remote_find_files`)、大日志尾部/增量/跟随读取(`remote_tail_log`),不依赖目标机 grep/rg/find
- **主机洞察**:进程枚举(`remote_list_processes`)、监听端口检查(`remote_check_port`)、一次性环境画像(`remote_get_env_info`)
- **MCP 接入**:`/mcp` 端点暴露 24 个工具,可直接接入 Claude Code、Cursor 等 MCP 客户端,支持按名单禁用工具
- **安全控制**:Bearer Token 鉴权、按 IP 限流、输出字节上限、超时上限、可选多目录 `FSRoot` 文件系统沙箱、工具黑名单、删除工具双重确认
- **可观测**:结构化审计日志,记录每次命令与文件操作(含 tool 名、session、截断标记),并对 mysql 密码、Bearer token 等敏感信息自动脱敏

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
go build -o remote-shell-server

# 运行(最低要求:设置 Token)
SHELL_API_TOKEN=your-secret-token ./remote-shell-server

# 生产部署建议启用 TLS
SHELL_API_TOKEN=your-secret-token \
SHELL_API_TLS_CERT=./cert.pem \
SHELL_API_TLS_KEY=./key.pem \
./remote-shell-server
```

快速验证:

```bash
# 健康检查(无需鉴权)
curl http://localhost:8080/api/status

# 执行一条命令
curl -X POST http://localhost:8080/api/execute \
  -H "Authorization: Bearer your-secret-token" \
  -H "Content-Type: application/json" \
  -d '{"command":"uname -a"}'
```

## 配置项

所有配置通过环境变量传入,均带默认值。

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `SHELL_API_PORT` | `8080` | 监听端口 |
| `SHELL_API_TOKEN` | (必填) | Bearer Token,客户端鉴权凭据 |
| `SHELL_API_TLS_CERT` | (空) | TLS 证书路径,留空则监听明文 HTTP |
| `SHELL_API_TLS_KEY` | (空) | TLS 私钥路径 |
| `SHELL_API_MAX_TIMEOUT` | `300` | 单次命令超时上限(秒) |
| `SHELL_API_MAX_OUTPUT` | `1048576` | stdout / stderr 字节上限 |
| `SHELL_API_RATE_LIMIT` | `60` | 每 IP 每分钟请求数上限 |
| `SHELL_API_DEFAULT_SHELL` | `bash` | 默认 shell |
| `SHELL_API_FS_ROOT` | (空) | 文件操作沙箱根目录,支持逗号分隔多个目录前缀,留空则不限制(等同 shell 信任级别) |
| `SHELL_API_DISABLED_TOOLS` | (空) | 工具黑名单,逗号分隔的 MCP 工具名,启动时不注册,对客户端不可见 |

## REST API

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `POST` | `/api/execute` | 同步执行命令,返回 stdout / stderr / exit_code |
| `POST` | `/api/execute/stream` | SSE 流式输出,带 15s 心跳 |
| `POST` | `/api/sessions` | 创建持久会话,返回 session_id |
| `POST` | `/api/sessions/{id}/execute` | 在会话上下文中执行命令 |
| `DELETE` | `/api/sessions/{id}` | 销毁会话 |
| `DELETE` | `/api/executions/{id}` | 取消正在运行的执行 |
| `GET` | `/api/status` | 服务健康、版本、系统信息(无需鉴权) |

请求体示例:

```json
{
  "command": "ls -la /var/log",
  "working_directory": "/tmp",
  "environment": { "FOO": "bar" },
  "timeout_ms": 10000,
  "max_output_bytes": 65536,
  "shell": "bash"
}
```

## MCP

MCP 端点位于 `/mcp`(Streamable HTTP),客户端配置示例:

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

暴露的工具(共 24 个):

- 命令与会话:`remote_execute` / `remote_session_execute` / `remote_cancel` / `remote_status` / `remote_session_create` / `remote_session_list` / `remote_session_close`
- 文件读写:`remote_write_file` / `remote_read_file` / `remote_edit_file` / `remote_list_dir` / `remote_stat` / `remote_upload_base64` / `remote_download_base64`
- 搜索与日志:`remote_search_content` / `remote_find_files` / `remote_tail_log`
- 文件管理:`remote_move_file` / `remote_copy_file` / `remote_delete_file` / `remote_make_dir`
- 主机洞察:`remote_list_processes` / `remote_check_port` / `remote_get_env_info`

### 文件沙箱(FSRoot)

`SHELL_API_FS_ROOT` 把**所有文件操作工具**(读 / 写 / 编辑 / 列目录 / stat / base64 上传下载)限制在指定目录内。原理是:每次操作前将传入路径解析为绝对路径并 `Clean`(消除 `..`、`./`),再校验它是否落在允许的根目录之下,借此拦截路径穿越(`../../etc/passwd`)和前缀欺骗(`/data/projectxxx`)。

- 支持**多个目录前缀**,用逗号分隔;路径只要命中任意一个根目录即放行:

  ```bash
  SHELL_API_FS_ROOT=/data/project,/tmp/workspace ./remote-shell-server
  ```

- 留空则不做任何限制,文件操作可覆盖整台主机(与 shell 执行同等信任级别)。
- **注意①**:该沙箱只约束文件操作工具,不约束 `remote_execute` / `remote_session_execute`;shell 执行本身即完整权限,如需限制请配合工具黑名单。
- **注意②**:`remote_delete_file` 仅在设置了 `FSRoot` 时才会注册;删除动作还要求 `confirm: true`,支持 `dry_run` 预览,且拒绝删除 FS 根目录本身。

### 工具黑名单(DisabledTools)

`SHELL_API_DISABLED_TOOLS` 是一份逗号分隔的 MCP 工具名单,列出的工具在启动时**不会注册**,对客户端完全不可见(而非运行时拒绝)。可用于按最小权限裁剪服务能力,例如做成"只读文件"服务:

```bash
# 禁用命令执行与一切写操作,仅保留只读文件与状态查询
SHELL_API_DISABLED_TOOLS=remote_execute,remote_session_execute,remote_cancel,remote_write_file,remote_edit_file,remote_upload_base64 \
./remote-shell-server
```

可禁用的工具名即上文列出的 24 个。

## 审计日志

每次命令执行与文件操作都会输出一行 `[AUDIT]` JSON,字段:`timestamp / request_id / source_ip / tool / session_id / command / working_directory / exit_code / duration_ms / output_bytes / truncated / timed_out`。

- `request_id` 缺失时自动生成(`audit-` 前缀),保证每条记录可关联
- `command` 字段在落盘前自动脱敏:`mysql -p<密码>`、`Authorization: Bearer <token>`、`password= / token= / secret= / api_key=` 等形态替换为 `***`

## 安全须知

本服务在网络边界上等同于一个交互式 shell,部署前请务必:

1. **启用 TLS**,避免 Token 与命令内容在传输中泄露
2. **使用强随机 Token**,不要硬编码进代码仓库
3. **设置 `FSRoot`** 把文件操作限制在指定目录,缩小爆炸半径;可传入多个目录前缀
4. **按需启用工具黑名单** 用 `SHELL_API_DISABLED_TOOLS` 禁掉不需要的能力(如命令执行),遵循最小权限原则
5. **不要暴露到公网**,应放在 VPN / 内网之后,或配合反向代理与额外鉴权
6. 按 `AuditLogger` 输出的 `[AUDIT]` 日志做集中收集与审计

如发现安全漏洞,请通过 GitHub Security Advisory 私下报告,不要直接开公开 Issue。

## 开发

```bash
# 运行全部测试(文件操作 / 搜索 / 日志 / 删除安全 / 脱敏 / 进程解析等)
go test ./...

# 跨平台编译
GOOS=linux GOARCH=amd64 go build -o remote-shell-server
```

仅依赖标准库加少量三方包(`google/uuid`、`mark3labs/mcp-go`、`golang.org/x/text`),无外部运行时依赖,单二进制部署。

## 贡献

欢迎 Issue 与 Pull Request。提交 PR 前请确保:

- `go test ./...` 全部通过,新功能附带测试
- `go vet ./...` 无告警,代码经 `gofmt` 格式化
- 涉及新工具时同步更新本 README 的工具清单

## License

[MIT](./LICENSE)
