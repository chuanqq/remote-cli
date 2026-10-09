package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func NewMCPHandler(executor *Executor, sessions *SessionManager, jobs *JobManager, audit *AuditLogger, cfg *Config) http.Handler {
	opts := []server.ServerOption{
		server.WithToolCapabilities(true),
		// Every tool call is audited by one middleware (tool name, duration,
		// outcome, request id, peer, MCP session); see mcp_audit.go.
		server.WithToolHandlerMiddleware(withAudit(audit)),
		server.WithHooks(mcpMethodHooks()),
	}
	name := "remote-shell"
	if cfg.ReadOnly {
		// Announce the mode in the handshake so clients (and the agents driving
		// them) know upfront that no write tool exists, instead of discovering
		// it by a failed call.
		name = "remote-shell-readonly"
		opts = append(opts, server.WithInstructions(
			"This server runs in READ-ONLY mode. Only inspection tools are available: "+
				"file reads (remote_read_file, remote_list_dir, remote_stat, remote_search_content, "+
				"remote_find_files, remote_tail_log, remote_download_base64) and host introspection "+
				"(remote_list_processes, remote_check_port, remote_get_env_info, remote_status). "+
				"Shell execution, sessions, and every write/edit/delete/move/copy/mkdir tool are "+
				"disabled and cannot be re-enabled at runtime. Do not attempt to modify the host; "+
				"if a change is required, report the exact command or diff for a human to apply.",
		))
	}
	s := server.NewMCPServer(name, fullVersion(), opts...)
	startTime := time.Now()

	registerFileTools(s, cfg)
	registerSystemTools(s, cfg)
	registerSessionTools(s, sessions, cfg)
	registerJobTools(s, jobs, cfg)
	registerWaitTool(s, executor, jobs, cfg)

	if cfg.toolEnabled("remote_execute") {
		s.AddTool(mcp.NewTool("remote_execute",
			mcp.WithDescription("Execute a single shell command on the remote server and wait for it, returning exit code, stdout, stderr, and timing. Default timeout 30s (timeout_ms raises it up to the server max). Prefer working_directory over `cd X && ...` prefixes. For searching, log viewing, or file inspection prefer the dedicated tools (remote_search_content, remote_find_files, remote_tail_log, remote_read_file, ...): they return structured results and never fail on zero matches. Do NOT poll with `sleep N; check` — use remote_wait_for. Do NOT start background processes with `nohup ... &` — use remote_spawn. Output beyond max_output_bytes is dropped (stdout_total_bytes tells how much there was); redirect huge output to a file and read it back with remote_read_file/remote_tail_log. Paths are resolved on the remote server, not on the client machine."),
			mcp.WithString("command", mcp.Required(), mcp.Description("Shell command to execute."), mcp.MaxLength(10000)),
			mcp.WithString("working_directory", mcp.Description("Working directory on the remote server. Prefer this over `cd X && ...` prefixes.")),
			mcp.WithObject("environment", mcp.Description("Additional environment variables as key-value pairs.")),
			mcp.WithNumber("timeout_ms", mcp.Description("Execution timeout in milliseconds. Default 30000.")),
			mcp.WithNumber("max_output_bytes", mcp.Description("Maximum captured bytes for stdout and stderr each.")),
			mcp.WithString("truncate_mode", mcp.Description("Which end to keep when output exceeds max_output_bytes: \"head\" (default) or \"tail\" (better for logs)."), mcp.Enum("head", "tail")),
			mcp.WithString("output_encoding", mcp.Description("Decode stdout/stderr to UTF-8 from this encoding: utf-8 (default, raw), gbk, gb2312, gb18030, or auto (keep valid UTF-8, else decode as GBK). Use gbk/auto for programs and logs that print GBK Chinese."), mcp.Enum("utf-8", "gbk", "gb2312", "gb18030", "auto")),
			mcp.WithString("shell", mcp.Description("Shell binary to use (defaults to server config).")),
		), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			command := req.GetString("command", "")
			if command == "" {
				return mcp.NewToolResultError("command is required"), nil
			}
			if len(command) > 10000 {
				return mcp.NewToolResultError("command exceeds maximum length of 10000 characters"), nil
			}

			execReq := ExecuteRequest{
				Command:          command,
				WorkingDirectory: req.GetString("working_directory", ""),
				Environment:      extractEnv(req, "environment"),
				TimeoutMs:        req.GetInt("timeout_ms", 0),
				MaxOutputBytes:   req.GetInt("max_output_bytes", 0),
				Shell:            req.GetString("shell", ""),
				TruncateMode:     req.GetString("truncate_mode", ""),
				OutputEncoding:   req.GetString("output_encoding", ""),
			}

			result := executor.Execute(ctx, execReq)
			noteExecAudit(ctx, result, execReq.Command, execReq.WorkingDirectory, "")

			resp := execResultResponse(result)
			resp.Hint = commandHint(command)
			return jsonResult(resp), nil
		})
	}

	if cfg.toolEnabled("remote_session_execute") {
		s.AddTool(mcp.NewTool("remote_session_execute",
			mcp.WithDescription("Execute a shell command within a persistent session (create one with remote_session_create). Session cwd, shell, and env are applied; a successful bare `cd <dir>` persists the session cwd for subsequent calls, so commands can be short and free of path prefixes."),
			mcp.WithString("session_id", mcp.Required(), mcp.Description("Active session ID returned by remote_session_create or the REST session API.")),
			mcp.WithString("command", mcp.Required(), mcp.Description("Shell command to execute. A bare `cd <dir>` updates the session cwd on success."), mcp.MaxLength(10000)),
			mcp.WithObject("environment", mcp.Description("Extra environment variables for this call (merged over the session env).")),
			mcp.WithNumber("timeout_ms", mcp.Description("Execution timeout in milliseconds.")),
			mcp.WithString("output_encoding", mcp.Description("Decode stdout/stderr to UTF-8 from this encoding: utf-8 (default), gbk, gb2312, gb18030, auto."), mcp.Enum("utf-8", "gbk", "gb2312", "gb18030", "auto")),
		), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			sessionID := req.GetString("session_id", "")
			if sessionID == "" {
				return mcp.NewToolResultError("session_id is required"), nil
			}

			sess := sessions.Get(sessionID)
			if sess == nil {
				return mcp.NewToolResultError("session not found or expired"), nil
			}

			command := req.GetString("command", "")
			if command == "" {
				return mcp.NewToolResultError("command is required"), nil
			}

			cwd := sess.Cwd()
			execReq := ExecuteRequest{
				Command:          command,
				WorkingDirectory: cwd,
				Shell:            sess.Shell,
				TimeoutMs:        req.GetInt("timeout_ms", 0),
				OutputEncoding:   req.GetString("output_encoding", ""),
				Environment:      make(map[string]string),
			}
			for _, e := range sess.Environment {
				parts := strings.SplitN(e, "=", 2)
				if len(parts) == 2 {
					execReq.Environment[parts[0]] = parts[1]
				}
			}
			for k, v := range extractEnv(req, "environment") {
				execReq.Environment[k] = v
			}

			result := executor.Execute(ctx, execReq)

			if strings.HasPrefix(strings.TrimSpace(command), "cd ") && !strings.ContainsAny(command, "&;|><\x60()") && result.ExitCode == 0 {
				pwdResult := executor.ExecuteInDir(ctx, sess.Shell, command+" && pwd", cwd, sess.Environment, 5000)
				if pwdResult.ExitCode == 0 {
					newDir := strings.TrimSpace(pwdResult.Stdout)
					if newDir != "" {
						sessions.UpdateWorkingDirectory(sessionID, newDir)
					}
				}
			}

			noteExecAudit(ctx, result, execReq.Command, cwd, sessionID)
			return jsonResult(execResultResponse(result)), nil
		})
	}

	if cfg.toolEnabled("remote_cancel") {
		s.AddTool(mcp.NewTool("remote_cancel",
			mcp.WithDescription("Cancel a running execution by its ID (kills its whole process group). For background work started with remote_spawn use remote_job_kill. Note that cancelling the MCP request itself (or disconnecting) also stops a remote_execute call."),
			mcp.WithString("execution_id", mcp.Required(), mcp.Description("Execution ID returned by remote_execute or remote_session_execute.")),
		), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			id := req.GetString("execution_id", "")
			if id == "" {
				return mcp.NewToolResultError("execution_id is required"), nil
			}
			noteAudit(ctx, func(n *auditNote) { n.Command = "cancel " + id; n.ExecID = id })

			if executor.Cancel(id) {
				return mcp.NewToolResultText(fmt.Sprintf("execution %s cancelled", id)), nil
			}
			return mcp.NewToolResultError("execution not found or already completed: " + id), nil
		})
	}

	if cfg.toolEnabled("remote_status") {
		s.AddTool(mcp.NewTool("remote_status",
			mcp.WithDescription("Return server health, version, uptime, active session count, and system information."),
		), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			hostname, _ := os.Hostname()
			status := StatusResponse{
				Status:         "healthy",
				Version:        serverVersion,
				Commit:         gitCommit,
				BuildTime:      buildTime,
				UptimeSeconds:  int64(time.Since(startTime).Seconds()),
				ActiveSessions: sessions.Count(),
				ReadOnly:       cfg.ReadOnly,
				System: SystemInfo{
					Hostname:    hostname,
					OS:          runtime.GOOS,
					Arch:        runtime.GOARCH,
					CPUs:        runtime.NumCPU(),
					MemoryMB:    getMemoryMB(),
					LoadAverage: getLoadAverage(),
				},
			}
			return jsonResult(status), nil
		})
	}

	return server.NewStreamableHTTPServer(s,
		// Inject the real peer address (and request correlation) into every
		// MCP request context.
		server.WithHTTPContextFunc(mcpHTTPContext),
		// Ping the GET stream so idle middleboxes stop cutting it (~5 min in
		// production, followed by a full re-initialize each time).
		server.WithHeartbeatInterval(cfg.MCPHeartbeat),
	)
}

// noteExecAudit fills the audit record of a command-running tool.
func noteExecAudit(ctx context.Context, r *ExecResult, command, dir, sessionID string) {
	noteAudit(ctx, func(n *auditNote) {
		n.Command = command
		n.WorkingDirectory = dir
		n.ExecID = r.ID
		n.SessionID = sessionID
		n.ExitCode = r.ExitCode
		n.OutputBytes = len(r.Stdout) + len(r.Stderr)
		n.Truncated = r.StdoutTruncated || r.StderrTruncated
		n.TimedOut = r.TimedOut
		n.Error = r.Error
	})
}

func execResultResponse(r *ExecResult) ExecuteResponse {
	return ExecuteResponse{
		ID:               r.ID,
		Command:          r.Command,
		ExitCode:         r.ExitCode,
		Stdout:           r.Stdout,
		Stderr:           r.Stderr,
		DurationMs:       r.DurationMs,
		StartedAt:        r.StartedAt.Format(time.RFC3339Nano),
		CompletedAt:      r.CompletedAt.Format(time.RFC3339Nano),
		WorkingDirectory: r.WorkingDirectory,
		TimedOut:         r.TimedOut,
		StdoutTruncated:  r.StdoutTruncated,
		StderrTruncated:  r.StderrTruncated,
		StdoutTotalBytes: r.StdoutTotalBytes,
		StderrTotalBytes: r.StderrTotalBytes,
		OutputEncoding:   r.OutputEncoding,
		Error:            r.Error,
	}
}

func extractEnv(req mcp.CallToolRequest, key string) map[string]string {
	args := req.GetArguments()
	if args == nil {
		return nil
	}
	raw, ok := args[key]
	if !ok {
		return nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	env := make(map[string]string, len(m))
	for k, v := range m {
		env[k] = fmt.Sprint(v)
	}
	return env
}

func jsonResult(v any) *mcp.CallToolResult {
	res, err := mcp.NewToolResultJSON(v)
	if err != nil {
		return mcp.NewToolResultError("failed to encode result: " + err.Error())
	}
	return res
}
