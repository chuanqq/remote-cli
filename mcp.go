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

func NewMCPHandler(executor *Executor, sessions *SessionManager, jobs *JobManager, waits *WaitManager, audit *AuditLogger, cfg *Config) http.Handler {
	opts := []server.ServerOption{
		server.WithToolCapabilities(true),
		// Every tool call is audited by one middleware (tool name, duration,
		// outcome, request id, peer, MCP session); see mcp_audit.go.
		server.WithToolHandlerMiddleware(withAudit(audit)),
		server.WithHooks(mcpMethodHooks()),
	}
	name := "remote-shell"
	if !cfg.ReadOnly {
		// Handshake-time limits: the agent learns them before its first call
		// instead of from a failure (or from hand-kept notes).
		opts = append(opts, server.WithInstructions(serverInstructions(cfg)))
	}
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
				"if a change is required, report the exact command or diff for a human to apply. "+
				fmt.Sprintf("Limits: one call blocks at most %ds (remote_tail_log follow_seconds is capped to it); "+
					"rate limit %d requests/min per client host, burst %d (HTTP 429 carries retry_after_ms).",
					cfg.callBudgetSec(), cfg.RateLimit, cfg.RateBurst),
		))
	}
	s := server.NewMCPServer(name, fullVersion(), opts...)
	startTime := time.Now()

	registerFileTools(s, cfg)
	registerSystemTools(s, cfg)
	registerSessionTools(s, sessions, cfg)
	registerJobTools(s, jobs, cfg)
	registerWaitTool(s, executor, jobs, waits, cfg)
	registerExecutionResultTool(s, executor, cfg)

	budget := cfg.callBudget()
	if !cfg.toolEnabled("remote_execution_result") {
		// Without a way to fetch a result later, a call waits for the
		// command itself (it still survives a client that gives up).
		budget = time.Duration(cfg.MaxTimeout)*time.Second + execWaitDelay + time.Second
	}

	if cfg.toolEnabled("remote_execute") {
		s.AddTool(mcp.NewTool("remote_execute",
			mcp.WithDescription(fmt.Sprintf("Execute a single shell command on the remote server; returns state, exit code, stdout, stderr and timing. "+
				"The call blocks at most %[1]ds (call budget, below MCP client request timeouts). A command still running then is NOT killed: "+
				"the call returns state=running with execution_id — never re-run it, fetch the final result with remote_execution_result. "+
				"Cancelling or timing out the MCP request does not stop the command either; only its own timeout_ms or remote_cancel do. "+
				"If a call was cut off before any reply, remote_execution_result without execution_id lists recent executions. "+
				"timeout_ms: default 30000, server max %[2]d; a command killed at its limit returns state=timed_out and names the limit in error. "+
				"Output: max_output_bytes per stream, default and max %[3]d bytes; stdout_total_bytes tells how much there was — redirect huge output to a file and read it back with remote_read_file/remote_tail_log. "+
				"Prefer working_directory over `cd X && ...`. For searching, logs and file inspection prefer the dedicated tools (remote_search_content, remote_find_files, remote_tail_log, remote_read_file, ...): structured results, no failure on zero matches. "+
				"Do NOT poll with `sleep N; check` — use remote_wait_for. Do NOT start background processes with `nohup ... &` — use remote_spawn. "+
				"Paths are resolved on the remote server, not on the client machine.", cfg.callBudgetSec(), cfg.MaxTimeout*1000, cfg.MaxOutput)),
			mcp.WithString("command", mcp.Required(), mcp.Description("Shell command to execute."), mcp.MaxLength(10000)),
			mcp.WithString("working_directory", mcp.Description("Working directory on the remote server. Prefer this over `cd X && ...` prefixes.")),
			mcp.WithObject("environment", mcp.Description("Additional environment variables as key-value pairs.")),
			mcp.WithNumber("timeout_ms", mcp.Description(fmt.Sprintf("Kill the command (whole process group) after this many milliseconds. Default 30000, max %d. Independent of the %ds call budget: a longer command keeps running and is fetched with remote_execution_result.", cfg.MaxTimeout*1000, cfg.callBudgetSec()))),
			mcp.WithNumber("max_output_bytes", mcp.Description(fmt.Sprintf("Maximum captured bytes for stdout and stderr each. Default and max %d.", cfg.MaxOutput))),
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

			ex, result := launchAndAwait(ctx, executor, audit, execReq, budget, "remote_execute", "")
			if result == nil {
				notePendingAudit(ctx, ex, "")
				return pendingResult(ex, budget), nil
			}
			noteExecAudit(ctx, result, execReq.Command, execReq.WorkingDirectory, "")

			resp := execResultResponse(result)
			resp.Hint = commandHint(command)
			if resp.Hint == "" && (resp.StdoutTruncated || resp.StderrTruncated) {
				resp.Hint = "output truncated at max_output_bytes (stdout_total_bytes / stderr_total_bytes give the full " +
					"size): redirect it to a file and page through it with remote_read_file or remote_tail_log"
			}
			return jsonResult(resp), nil
		})
	}

	if cfg.toolEnabled("remote_session_execute") {
		s.AddTool(mcp.NewTool("remote_session_execute",
			mcp.WithDescription(fmt.Sprintf("Execute a shell command within a persistent session (create one with remote_session_create). Session cwd, shell, and env are applied; a successful bare `cd <dir>` persists the session cwd for subsequent calls, so commands can be short and free of path prefixes. Every call renews the session's idle TTL (session_expires_at in the result). Same execution semantics as remote_execute: the call blocks at most %ds; a command still running then returns state=running with execution_id (it is NOT killed) — fetch it with remote_execution_result instead of re-running it.", cfg.callBudgetSec())),
			mcp.WithString("session_id", mcp.Required(), mcp.Description("Active session ID returned by remote_session_create or the REST session API.")),
			mcp.WithString("command", mcp.Required(), mcp.Description("Shell command to execute. A bare `cd <dir>` updates the session cwd on success."), mcp.MaxLength(10000)),
			mcp.WithObject("environment", mcp.Description("Extra environment variables for this call (merged over the session env).")),
			mcp.WithNumber("timeout_ms", mcp.Description(fmt.Sprintf("Kill the command after this many milliseconds. Default 30000, max %d.", cfg.MaxTimeout*1000))),
			mcp.WithString("output_encoding", mcp.Description("Decode stdout/stderr to UTF-8 from this encoding: utf-8 (default), gbk, gb2312, gb18030, auto."), mcp.Enum("utf-8", "gbk", "gb2312", "gb18030", "auto")),
		), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			sessionID := req.GetString("session_id", "")
			if sessionID == "" {
				return mcp.NewToolResultError("session_id is required"), nil
			}

			sess := sessions.Touch(sessionID)
			if sess == nil {
				return mcp.NewToolResultError("session not found or expired (sessions expire after ttl_seconds without use; " +
					"its cwd/env are gone — create a new one with remote_session_create)"), nil
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

			ex, result := launchAndAwait(ctx, executor, audit, execReq, budget, "remote_session_execute", sessionID)
			if result == nil {
				notePendingAudit(ctx, ex, sessionID)
				return pendingResult(ex, budget), nil
			}

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
			resp := execResultResponse(result)
			resp.SessionExpiresAt = sess.Expiry().Format(time.RFC3339)
			return jsonResult(resp), nil
		})
	}

	if cfg.toolEnabled("remote_cancel") {
		s.AddTool(mcp.NewTool("remote_cancel",
			mcp.WithDescription("Cancel a running execution by its ID (kills its whole process group); the final result (state=cancelled) stays fetchable with remote_execution_result. For background work started with remote_spawn use remote_job_kill. Cancelling or timing out the MCP request itself does NOT stop a remote_execute command — this tool does."),
			mcp.WithString("execution_id", mcp.Required(), mcp.Description("Execution ID returned by remote_execute or remote_session_execute (also listed by remote_execution_result).")),
		), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			id := req.GetString("execution_id", "")
			if id == "" {
				return mcp.NewToolResultError("execution_id is required"), nil
			}
			noteAudit(ctx, func(n *auditNote) { n.Command = "cancel " + id; n.ExecID = id })

			if ex := executor.Lookup(id); ex != nil {
				if r, _ := ex.Result(); r != nil {
					return mcp.NewToolResultError(fmt.Sprintf("execution %s already finished: state=%s exit_code=%d",
						id, execState(r), r.ExitCode)), nil
				}
				executor.Cancel(id)
				// Killing the group is immediate; report the settled state.
				ex.Await(ctx, execWaitDelay+time.Second)
				return jsonResult(ex.info()), nil
			}
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

// serverInstructions is the MCP handshake text (initialize.instructions):
// this instance's limits and the call semantics an agent needs to plan
// long-running work.
func serverInstructions(cfg *Config) string {
	host, _ := os.Hostname()
	var b strings.Builder
	fmt.Fprintf(&b, "Remote shell server on host %s: every path and command is resolved on THAT host, not on the client machine. ", host)
	fmt.Fprintf(&b, "Limits of this instance: one tool call blocks at most %ds (call budget, below typical MCP client request timeouts); "+
		"remote_execute timeout_ms max %ds (default 30s); stdout/stderr capped at %d bytes each; "+
		"rate limit %d requests/min per client host, burst %d (HTTP 429 carries retry_after_ms). ",
		cfg.callBudgetSec(), cfg.MaxTimeout, cfg.MaxOutput, cfg.RateLimit, cfg.RateBurst)
	b.WriteString("Long work: remote_execute / remote_session_execute return state=running with an execution_id when the command " +
		"is still running at the budget — it is NOT killed; fetch it with remote_execution_result instead of re-running it " +
		"(remote_execution_result without execution_id lists recent executions after a lost reply). " +
		"remote_wait_for returns state=pending with a wait_id; call again with that wait_id to keep waiting on the same server-side watcher. " +
		"Commands that must outlive the server or run longer than the timeout_ms max go through remote_spawn. ")
	if len(cfg.FSRoots) > 0 {
		fmt.Fprintf(&b, "File tools are sandboxed to: %s. ", strings.Join(cfg.FSRoots, ", "))
	}
	if cfg.DenyCommand != nil {
		b.WriteString("Some commands are denied by server policy (see remote_get_env_info deny_commands). ")
	}
	b.WriteString("remote_get_env_info returns the full profile (tools, versions, limits).")
	return b.String()
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
		n.State = execState(r)
	})
}

func execResultResponse(r *ExecResult) ExecuteResponse {
	return ExecuteResponse{
		ID:               r.ID,
		State:            execState(r),
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
