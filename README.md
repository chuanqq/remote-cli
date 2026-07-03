# Remote Shell API Server

一个用 Go 编写的远程 Shell 执行与文件操作服务,同时提供 REST API 和 MCP(Streamable HTTP)两种接入方式,专为让 AI Agent(如 Claude Code)通过网络安全地管理远程主机而设计。

同一个进程、同一个端口同时承载 REST 与 MCP,共用 TLS 和 Bearer Token 鉴权。

## 特性

- **命令执行**:同步执行、SSE 流式输出、可取消,基于进程组(SIGKILL)的硬超时控制
- **持久会话**:维护 cwd / shell / 环境变量,`cd` 后自动同步工作目录,带 TTL 自动回收
- **文件操作**:读 / 写 / 编辑 / 列目录 / stat,支持 UTF-8 / GBK / GB2312 / GB18030 编码互转与自动探测,大文件支持 base64 分块上传下载
- **MCP 接入**:`/mcp` 端点暴露 11 个工具,可直接接入 Claude Code、Cursor 等 MCP 客户端
- **安全控制**:Bearer Token 鉴权、按 IP 限流、输出字节上限、超时上限、可选 `FSRoot` 文件系统沙箱
- **可观测**:结构化审计日志,记录每次命令与文件操作

## 快速开始

```bash
# 构建
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
| `SHELL_API_FS_ROOT` | (空) | 文件操作沙箱根目录,留空则不限制(等同 shell 信任级别) |

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

暴露的工具:

- `remote_execute` / `remote_session_execute` / `remote_cancel` / `remote_status`
- `remote_write_file` / `remote_read_file` / `remote_edit_file`
- `remote_list_dir` / `remote_stat`
- `remote_upload_base64` / `remote_download_base64`

## 安全须知

本服务在网络边界上等同于一个交互式 shell,部署前请务必:

1. **启用 TLS**,避免 Token 与命令内容在传输中泄露
2. **使用强随机 Token**,不要硬编码进代码仓库
3. **设置 `FSRoot`** 把文件操作限制在指定目录,缩小爆炸半径
4. **不要暴露到公网**,应放在 VPN / 内网之后,或配合反向代理与额外鉴权
5. 按 `AuditLogger` 输出的 `[AUDIT]` 日志做集中收集与审计

## 开发

```bash
# 运行测试(覆盖文件操作与编码转换)
go test ./...

# 跨平台编译(支持 Linux / macOS)
GOOS=linux GOARCH=amd64 go build -o remote-shell-server
```

仅依赖标准库加少量三方包(`google/uuid`、`mark3labs/mcp-go`、`golang.org/x/text`),无外部运行时依赖,单二进制部署。

## License

[MIT](./LICENSE)
