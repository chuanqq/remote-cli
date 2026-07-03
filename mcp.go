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

func NewMCPHandler(executor *Executor, sessions *SessionManager, audit *AuditLogger, cfg *Config) http.Handler {
	s := server.NewMCPServer("remote-shell", "1.0.0", server.WithToolCapabilities(true))
	startTime := time.Now()

	registerFileTools(s, audit, cfg)

	s.AddTool(mcp.NewTool("remote_execute",
		mcp.WithDescription("Execute a single shell command on the remote server and return exit code, stdout, stderr, and timing."),
		mcp.WithString("command", mcp.Required(), mcp.Description("Shell command to execute."), mcp.MaxLength(10000)),
		mcp.WithString("working_directory", mcp.Description("Working directory for the command.")),
		mcp.WithObject("environment", mcp.Description("Additional environment variables as key-value pairs.")),
		mcp.WithNumber("timeout_ms", mcp.Description("Execution timeout in milliseconds.")),
		mcp.WithNumber("max_output_bytes", mcp.Description("Maximum captured bytes for stdout and stderr.")),
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
		}

		result := executor.Execute(execReq)

		audit.Log(AuditEntry{
			RequestID:        result.ID,
			SourceIP:         sourceIP(req.Header),
			Command:          execReq.Command,
			WorkingDirectory: execReq.WorkingDirectory,
			ExitCode:         result.ExitCode,
			DurationMs:       result.DurationMs,
			OutputBytes:      len(result.Stdout) + len(result.Stderr),
			TimedOut:         result.TimedOut,
		})

		return jsonResult(execResultResponse(result)), nil
	})

	s.AddTool(mcp.NewTool("remote_session_execute",
		mcp.WithDescription("Execute a shell command within a persistent session. Session cwd, shell, and env are applied; a successful cd updates the session cwd."),
		mcp.WithString("session_id", mcp.Required(), mcp.Description("Active session ID returned by the REST session API.")),
		mcp.WithString("command", mcp.Required(), mcp.Description("Shell command to execute."), mcp.MaxLength(10000)),
		mcp.WithNumber("timeout_ms", mcp.Description("Execution timeout in milliseconds.")),
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

		execReq := ExecuteRequest{
			Command:          command,
			WorkingDirectory: sess.WorkingDirectory,
			Shell:            sess.Shell,
			TimeoutMs:        req.GetInt("timeout_ms", 0),
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

		result := executor.Execute(execReq)

		if strings.HasPrefix(strings.TrimSpace(command), "cd ") && !strings.ContainsAny(command, "&;|><\x60()") && result.ExitCode == 0 {
			pwdResult := executor.ExecuteInDir(sess.Shell, command+" && pwd", sess.WorkingDirectory, sess.Environment, 5000)
			if pwdResult.ExitCode == 0 {
				newDir := strings.TrimSpace(pwdResult.Stdout)
				if newDir != "" {
					sessions.UpdateWorkingDirectory(sessionID, newDir)
				}
			}
		}

		audit.Log(AuditEntry{
			RequestID:        result.ID,
			SourceIP:         sourceIP(req.Header),
			Command:          execReq.Command,
			WorkingDirectory: sess.WorkingDirectory,
			ExitCode:         result.ExitCode,
			DurationMs:       result.DurationMs,
			OutputBytes:      len(result.Stdout) + len(result.Stderr),
			TimedOut:         result.TimedOut,
		})

		return jsonResult(execResultResponse(result)), nil
	})

	s.AddTool(mcp.NewTool("remote_cancel",
		mcp.WithDescription("Cancel a running execution by its ID."),
		mcp.WithString("execution_id", mcp.Required(), mcp.Description("Execution ID returned by remote_execute or remote_session_execute.")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id := req.GetString("execution_id", "")
		if id == "" {
			return mcp.NewToolResultError("execution_id is required"), nil
		}

		if executor.Cancel(id) {
			return mcp.NewToolResultText(fmt.Sprintf("execution %s cancelled", id)), nil
		}
		return mcp.NewToolResultError("execution not found or already completed: " + id), nil
	})

	s.AddTool(mcp.NewTool("remote_status",
		mcp.WithDescription("Return server health, version, uptime, active session count, and system information."),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		hostname, _ := os.Hostname()
		status := StatusResponse{
			Status:         "healthy",
			Version:        "1.0.0",
			UptimeSeconds:  int64(time.Since(startTime).Seconds()),
			ActiveSessions: sessions.Count(),
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

	return server.NewStreamableHTTPServer(s)
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

func sourceIP(header http.Header) string {
	if header != nil {
		if xff := header.Get("X-Forwarded-For"); xff != "" {
			return strings.TrimSpace(strings.Split(xff, ",")[0])
		}
		if xri := header.Get("X-Real-IP"); xri != "" {
			return strings.TrimSpace(xri)
		}
	}
	return "mcp"
}

func jsonResult(v any) *mcp.CallToolResult {
	res, err := mcp.NewToolResultJSON(v)
	if err != nil {
		return mcp.NewToolResultError("failed to encode result: " + err.Error())
	}
	return res
}
