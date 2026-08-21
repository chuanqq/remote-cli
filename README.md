# remote-cli

[![Go Version](https://img.shields.io/github/go-mod/go-version/chuanqq/remote-cli)](https://go.dev/)
[![Go Report Card](https://goreportcard.com/badge/github.com/chuanqq/remote-cli)](https://goreportcard.com/report/github.com/chuanqq/remote-cli)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](./LICENSE)

**English:** A single-binary MCP (Model Context Protocol) server written in Go that lets AI agents (such as Claude Code) securely operate remote hosts. It exposes 24 dedicated tools over Streamable HTTP — shell execution with persistent sessions, file read/write/edit, server-side content search, log tailing with cursors and follow, process/port inspection and an environment profile — so the agent rarely needs to fall back to raw shell commands.

一个用 Go 编写的 MCP(Model Context Protocol)服务端,让 AI Agent(如 Claude Code)通过网络安全地操作远程主机。通过 Streamable HTTP 暴露 24 个专用工具:命令执行与持久会话、文件读写编辑、服务端内容搜索、带游标与 follow 的日志跟踪、进程/端口洞察与环境画像——绝大多数远程操作无需再拼 shell 命令。

编译产物为单二进制,零外部运行时依赖;TLS 与 Bearer Token 鉴权开箱即用。

> **说明**:早期版本附带的 REST API 仍保留在代码中以兼容旧客户端,但不再是推荐接入方式,本文档不再展开;新接入请一律使用 MCP。

## 特性

- **命令执行**:同步执行、可取消,基于进程组的硬超时控制,输出截断可选保留头部或尾部(`truncate_mode`)
- **持久会话**:维护 cwd / shell / 环境变量,`cd` 后自动同步工作目录,带 TTL 自动回收;会话的创建/列出/销毁均有对应 MCP 工具
- **文件操作**:读 / 写 / 编辑 / 列目录 / stat / 移动 / 复制 / 删除 / 建目录,支持 UTF-8 / GBK / GB2312 / GB18030 编码互转与自动探测,大文件支持 base64 分块上传下载;写入类操作返回 sha256/mode 自检,可选 bash/python 语法 lint
- **内容搜索与日志**:服务端实现的内容正则搜索、文件名查找、大日志尾部/增量/跟随读取,不依赖目标机 grep/rg/find
- **主机洞察**:进程枚举、监听端口检查、一次性环境画像(36 个常用工具链探测)
- **安全控制**:Bearer Token 鉴权、按 IP 限流、输出字节上限、超时上限、可选多目录 `FSRoot` 文件系统沙箱、工具黑名单、删除工具双重确认
- **可观测**:结构化审计日志,记录每次调用(含 tool 名、session、截断标记),并对 mysql 密码、Bearer token 等敏感信息自动脱敏

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

连通后客户端 `tools/list` 应能看到全部 24 个工具。服务另有免鉴权的 `GET /api/status` 健康探针,可用于负载均衡或监控探活。

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

## 工具一览(24 个)

| 分组 | 工具 |
| --- | --- |
| 命令与会话 | `remote_execute` `remote_session_execute` `remote_cancel` `remote_status` `remote_session_create` `remote_session_list` `remote_session_close` |
| 文件读写 | `remote_write_file` `remote_read_file` `remote_edit_file` `remote_list_dir` `remote_stat` `remote_upload_base64` `remote_download_base64` |
| 搜索与日志 | `remote_search_content` `remote_find_files` `remote_tail_log` |
| 文件管理 | `remote_move_file` `remote_copy_file` `remote_delete_file` `remote_make_dir` |
| 主机洞察 | `remote_list_processes` `remote_check_port` `remote_get_env_info` |

下面按分组给出每个工具的参数与用法。参数表中"必填"列标 `*` 的为必填;示例为 MCP `tools/call` 的 `arguments` 内容。

### 命令与会话

#### `remote_execute`

执行单条 shell 命令,返回 exit code / stdout / stderr / 耗时。搜索、看日志、查文件优先用专用工具(结构化返回、零匹配不报错);产出超大输出的命令建议重定向到文件后用 `remote_read_file` / `remote_tail_log` 读回。

| 参数 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `command` | string | * | 要执行的命令,最长 10000 字符 |
| `working_directory` | string | | 工作目录,优先于 `cd X && ...` 前缀 |
| `environment` | object | | 追加的环境变量键值对 |
| `timeout_ms` | number | | 超时毫秒数,默认取服务端配置 |
| `max_output_bytes` | number | | stdout/stderr 捕获上限,默认取服务端配置 |
| `truncate_mode` | string | | 超限保留 `head`(默认)或 `tail`(看日志更友好) |
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

`remote_session_list` 无参数,返回存活会话的 id / cwd / shell / 过期时间;`remote_session_close` 仅需 `session_id`(必填)。

#### `remote_session_execute`

在持久会话中执行命令。成功的裸 `cd <dir>` 会持久化会话 cwd,后续命令无需再带路径前缀。

| 参数 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `session_id` | string | * | `remote_session_create` 返回的会话 ID |
| `command` | string | * | 要执行的命令;裸 `cd <dir>` 成功后更新会话 cwd |
| `environment` | object | | 本次调用追加的环境变量(覆盖会话级同名变量) |
| `timeout_ms` | number | | 超时毫秒数 |

```json
{"session_id": "s-xxxx", "command": "cd /var/log/nginx"}
```

#### `remote_cancel` / `remote_status`

`remote_cancel` 按执行 ID 取消运行中的命令(参数 `execution_id`,必填,来自 execute 的返回)。`remote_status` 无参数,返回服务健康、版本、运行时长、活跃会话数与系统信息。

### 文件读写

#### `remote_read_file`

读取远程文件并返回 UTF-8 内容。seek 式读取,大文件不会整体载入内存;自动探测编码,识别二进制文件(二进制请用 `remote_download_base64`)。等效 shell 映射:`cat` = 只传 `path`;`sed -n 'X,Yp'` = `start_line`/`end_line`;`tail -n +N` = `start_line=N`;`tail -N` = `tail_lines=N`;`head -c N` = `max_bytes=N`。

| 参数 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `path` | string | * | 文件路径(绝对或相对) |
| `encoding` | string | | 源编码,缺省自动探测(utf-8/gbk/gb2312/gb18030) |
| `start_line` / `end_line` | number | | 1 起始止行号(含),0 或缺省表示文件头/尾 |
| `tail_lines` | number | | 读最后 N 行(seek 实现,支持超大文件),优先于行区间 |
| `offset_bytes` | number | | 从该字节偏移开始读,默认 0 |
| `max_bytes` | number | | 读取字节上限,默认 1MB |
| `truncate_mode` | string | | 超限保留 `head`(默认)或 `tail` |

```json
{"path": "/data/app/logs/app.log", "tail_lines": 200}
```

#### `remote_write_file`

写入 UTF-8 文本,支持目标编码转换、追加模式、自动建父目录;返回 sha256/mode 写后自检,可选写后 lint。二进制请用 `remote_upload_base64`。

| 参数 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `path` | string | * | 目标文件路径 |
| `content` | string | * | UTF-8 文本内容 |
| `encoding` | string | | 落盘编码:utf-8(默认)/gbk/gb2312/gb18030 |
| `append` | boolean | | 追加而非覆盖,默认 false |
| `make_dirs` | boolean | | 自动创建父目录,默认 false |
| `mode` | string | | 新文件权限(八进制),默认 `0644` |
| `lint` | string | | 写后语法检查:`bash`(bash -n)/`python`(py_compile),结果仅供参考 |

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
| `sort_by` | string | | `name`(默认,升序)/ `mtime`(最新在前)/ `size`(最大在前) |
| `filter_glob` | array | | 只保留文件名匹配的条目,如 `["*.conf"]` |
| `include_hidden` | boolean | | 包含点文件,默认 false |

#### `remote_stat`

路径元数据:存在性、类型、大小、权限、owner/group、nlink、mtime、symlink 目标、二进制嗅探;可选内容哈希与编码探测。一次调用替代 `stat -c ...; file -I; md5sum` 三连;路径不存在返回 `exists=false` 而非报错。

| 参数 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `path` | string | * | 文件或目录路径 |
| `include_hash` | string | | `md5` / `sha256`,流式计算(常规文件 ≤256MB) |
| `include_encoding` | boolean | | 探测文本编码(utf-8/gbk/unknown) |

```json
{"path": "/data/app/dist.tar.gz", "include_hash": "sha256"}
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

服务端实现的 RE2 内容搜索——结构化版 `grep -rn`,目标机无需 grep/rg。**无匹配返回空列表而非报错**;自动跳过二进制/超大文件,默认不含隐藏文件。

| 参数 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `path` | string | * | 文件或目录(目录递归) |
| `pattern` | string | * | RE2 正则,如 `"FATAL\|ERROR\|failed"` |
| `include_glob` | array | | 只搜文件名匹配的文件,如 `["*.conf", "*.log"]` |
| `ignore_case` | boolean | | 忽略大小写,默认 false |
| `context_lines` | number | | 每个命中附前后各 N 行,默认 0,最大 20 |
| `max_results` | number | | 命中上限,默认 200,最大 5000;`truncated` 标记是否有更多 |
| `max_file_size` | number | | 跳过超过该字节数的文件,默认 32MB |
| `include_hidden` | boolean | | 包含点文件/点目录,默认 false |

```json
{"path": "/data/app", "pattern": "FATAL|ERROR", "include_glob": ["*.log"], "context_lines": 2}
```

#### `remote_find_files`

按文件名查找——结构化版 `find -name/-iname`,受 FSRoot 约束(杜绝 `find /`),显式返回截断标记。

| 参数 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `path` | string | * | 递归查找的根目录 |
| `name_glob` | array | | 文件名 glob,如 `["*.cpp", "*.h"]`;留空匹配全部 |
| `type` | string | | `file` / `dir` / `any`(默认) |
| `max_depth` | number | | 最大目录深度(1 = 仅直接子项),默认不限 |
| `max_results` | number | | 上限,默认 500,最大 10000;`truncated` 标记是否有更多 |
| `ignore_case` | boolean | | 忽略大小写(等效 `find -iname`),默认 false |
| `include_hidden` | boolean | | 包含点文件/点目录,默认 false |

```json
{"path": "/data/project", "name_glob": ["*.pem", "*.key"], "type": "file", "max_depth": 4}
```

#### `remote_tail_log`

读(可能巨大且持续增长的)日志尾部,不载入内存。替代 `tail -n N`、`tail -n +N`(用 `since_line=N-1`)、`sleep 70; tail`(用 `follow_seconds=70`)。支持正则过滤与两种增量游标;轮询推荐 `since_offset`(最省,取上次返回的 `end_offset`);文件轮转(变小)时自动从头重读并置 `rotated=true`。

| 参数 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `path` | string | * | 日志文件路径 |
| `lines` | number | | 尾部模式:返回最后 N 行,默认 100,最大 10000 |
| `filter_regex` | string | | RE2 过滤,只返回匹配行;游标推进不受过滤影响 |
| `since_line` | number | | 行游标:返回该行号(1 基)之后的行(取上次 `end_line`) |
| `since_offset` | number | | 字节游标:返回该偏移之后的内容(取上次 `end_offset`),优先于 `since_line` |
| `follow_seconds` | number | | 最多等待 N 秒(≤300)新内容;无新内容时 `timed_out=true` |
| `encoding` | string | | 源编码,缺省自动探测 |
| `max_bytes` | number | | 收集内容字节上限,默认 1MB |

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

一次性环境画像,无参数:OS/内核/架构、主机名、用户、shell、locale、内存/负载、36 个常用工具链(python3、rg、rsync、mysql 等)的可用性与版本、服务端配置(FSRoots、工具黑名单、限额)。会话开局调一次,替代一串 `which` / `--version` / `uname` 探测。

## 安全机制

### 文件沙箱(FSRoot)

`SHELL_API_FS_ROOT` 把**所有文件操作工具**(读 / 写 / 编辑 / 列目录 / stat / 搜索 / 日志 / base64 上传下载 / 移动 / 复制 / 删除 / 建目录)限制在指定目录内。原理是:每次操作前将传入路径解析为绝对路径并 `Clean`(消除 `..`、`./`),再校验它是否落在允许的根目录之下,借此拦截路径穿越(`../../etc/passwd`)和前缀欺骗(`/data/projectxxx`)。

- 支持**多个目录前缀**,用逗号分隔;路径只要命中任意一个根目录即放行:

  ```bash
  SHELL_API_FS_ROOT=/data/project,/tmp/workspace ./remote-agent-proxy
  ```

- 留空则不做任何限制,文件操作可覆盖整台主机(与 shell 执行同等信任级别)。
- **注意①**:该沙箱只约束文件操作工具,不约束 `remote_execute` / `remote_session_execute`;shell 执行本身即完整权限,如需限制请配合工具黑名单。
- **注意②**:`remote_delete_file` 仅在设置了 `FSRoot` 时才会注册;删除动作还要求 `confirm: true`,支持 `dry_run` 预览,且拒绝删除 FS 根目录本身。

### 工具黑名单(DisabledTools)

`SHELL_API_DISABLED_TOOLS` 是一份逗号分隔的工具名单,列出的工具在启动时**不会注册**,对客户端完全不可见(而非运行时拒绝)。可用于按最小权限裁剪服务能力,例如做成"只读文件"服务:

```bash
# 禁用命令执行与一切写操作,仅保留只读文件与状态查询
SHELL_API_DISABLED_TOOLS=remote_execute,remote_session_execute,remote_cancel,remote_write_file,remote_edit_file,remote_upload_base64 \
./remote-agent-proxy
```

可禁用的工具名即上文列出的 24 个。

### 审计日志

每次工具调用都会输出一行 `[AUDIT]` JSON,字段:`timestamp / request_id / source_ip / tool / session_id / command / working_directory / exit_code / duration_ms / output_bytes / truncated / timed_out`。

- `request_id` 缺失时自动生成(`audit-` 前缀),保证每条记录可关联
- `command` 字段在落盘前自动脱敏:`mysql -p<密码>`、`Authorization: Bearer <token>`、`password= / token= / secret= / api_key=` 等形态替换为 `***`

## 安全须知

本服务在网络边界上等同于一个交互式 shell,部署前请务必:

1. **启用 TLS**,避免 Token 与命令内容在传输中泄露
2. **使用强随机 Token**,不要硬编码进代码仓库
3. **设置 `FSRoot`** 把文件操作限制在指定目录,缩小爆炸半径;可传入多个目录前缀
4. **按需启用工具黑名单** 用 `SHELL_API_DISABLED_TOOLS` 禁掉不需要的能力(如命令执行),遵循最小权限原则
5. **不要暴露到公网**,应放在 VPN / 内网之后,或配合反向代理与额外鉴权
6. 将 `[AUDIT]` 审计日志做集中收集与审计

如发现安全漏洞,请通过 GitHub Security Advisory 私下报告,不要直接开公开 Issue。

## 开发

```bash
# 运行全部测试(文件操作 / 搜索 / 日志 / 删除安全 / 脱敏 / 进程解析等)
go test ./...

# 跨平台编译
GOOS=linux GOARCH=amd64 go build -o remote-agent-proxy
```

### 构建脚本

`build.sh` 封装了多平台交叉编译,产物统一输出到 `dist/`(命名 `remote-agent-proxy-<版本>-<os>-<arch>`,版本号自动从 `types.go` 提取):

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

仅依赖标准库加少量三方包(`google/uuid`、`mark3labs/mcp-go`、`golang.org/x/text`),无外部运行时依赖,单二进制部署。

## 贡献

欢迎 Issue 与 Pull Request。提交 PR 前请确保:

- `go test ./...` 全部通过,新功能附带测试
- `go vet ./...` 无告警,代码经 `gofmt` 格式化
- 涉及新工具时同步更新本 README 的工具清单

## License

[MIT](./LICENSE)
