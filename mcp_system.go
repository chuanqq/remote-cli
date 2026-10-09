package main

import (
	"context"
	"fmt"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// registerSystemTools adds host-introspection tools (processes, ports,
// environment profile) to the MCP server. These read system state only and
// are NOT sandboxed by FSRoots (they touch no file contents).
func registerSystemTools(s *server.MCPServer, cfg *Config) {
	// auditCall describes the call for the withAudit middleware, which adds
	// the registered tool name, timing and the failure reason.
	auditCall := func(ctx context.Context, descriptor string) {
		noteAudit(ctx, func(n *auditNote) { n.Command = descriptor })
	}

	if cfg.toolEnabled("remote_list_processes") {
		s.AddTool(mcp.NewTool("remote_list_processes",
			mcp.WithDescription("List running processes on the remote server (pid/ppid/user/state/elapsed/cmd/cmdline), replacing `ps -ef | grep ...`. Optional regex filter and user filter."),
			mcp.WithString("filter", mcp.Description("RE2 regex matched against the full command line, e.g. \"toadpolicy|bpget\".")),
			mcp.WithString("user", mcp.Description("Only processes owned by this user.")),
		), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			result, err := ListProcesses(ListProcessesRequest{
				Filter: req.GetString("filter", ""),
				User:   req.GetString("user", ""),
			})
			if err != nil {
				auditCall(ctx, "list_processes")
				return mcp.NewToolResultError(err.Error()), nil
			}
			auditCall(ctx, "list_processes")
			return jsonResult(result), nil
		})
	}

	if cfg.toolEnabled("remote_check_port") {
		s.AddTool(mcp.NewTool("remote_check_port",
			mcp.WithDescription("Check which ports are listening on the remote server and which process owns them, replacing `ss -lntp | grep <port>` / netstat / lsof. Returns an empty list when nothing matches."),
			mcp.WithNumber("port", mcp.Description("Port number to check. Omit or 0 to list all listening ports.")),
			mcp.WithString("process_name", mcp.Description("Case-insensitive substring filter on the owning process name.")),
		), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			port := req.GetInt("port", 0)
			result, err := CheckPort(CheckPortRequest{
				Port:        port,
				ProcessName: req.GetString("process_name", ""),
			})
			if err != nil {
				auditCall(ctx, fmt.Sprintf("check_port %d", port))
				return mcp.NewToolResultError(err.Error()), nil
			}
			auditCall(ctx, fmt.Sprintf("check_port %d", port))
			return jsonResult(result), nil
		})
	}

	if cfg.toolEnabled("remote_get_env_info") {
		s.AddTool(mcp.NewTool("remote_get_env_info",
			mcp.WithDescription("One-call environment profile of the remote server: OS/kernel/arch, hostname, user, shells, locale, memory/load, availability+version of common toolchains (python3, rg, rsync, mysql, ...), and the server config (FSRoots, disabled tools, limits). Call once at the start of a session instead of probing with which/--version/uname commands."),
		), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			auditCall(ctx, "get_env_info")
			return jsonResult(GetEnvInfo(cfg)), nil
		})
	}
}

// registerSessionTools adds the session lifecycle tools so MCP clients can
// create/list/close persistent sessions without falling back to the REST API.
func registerSessionTools(s *server.MCPServer, sessions *SessionManager, cfg *Config) {
	auditCall := func(ctx context.Context, sessionID, descriptor string) {
		noteAudit(ctx, func(n *auditNote) { n.Command = descriptor; n.SessionID = sessionID })
	}

	if cfg.toolEnabled("remote_session_create") {
		s.AddTool(mcp.NewTool("remote_session_create",
			mcp.WithDescription("Create a persistent session on the remote server (working directory + env + shell, TTL-bound). Use the returned session_id with remote_session_execute: a successful bare `cd <dir>` persists the session cwd, avoiding repeated `cd X && ...` prefixes."),
			mcp.WithString("working_directory", mcp.Description("Initial working directory. Defaults to the server user's home.")),
			mcp.WithString("shell", mcp.Description("Shell binary. Default bash.")),
			mcp.WithObject("environment", mcp.Description("Environment variables persisted for the session, as key-value pairs.")),
			mcp.WithNumber("ttl_seconds", mcp.Description("Session TTL. Default 3600, max 86400.")),
		), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			sess := sessions.Create(SessionCreateRequest{
				WorkingDirectory: req.GetString("working_directory", ""),
				Shell:            req.GetString("shell", ""),
				Environment:      extractEnv(req, "environment"),
				TTLSeconds:       req.GetInt("ttl_seconds", 0),
			})
			auditCall(ctx, sess.ID, "session_create "+sess.ID)
			return jsonResult(SessionResponse{
				SessionID:        sess.ID,
				WorkingDirectory: sess.Cwd(),
				CreatedAt:        sess.CreatedAt.Format(time.RFC3339),
				ExpiresAt:        sess.ExpiresAt.Format(time.RFC3339),
				Shell:            sess.Shell,
			}), nil
		})
	}

	if cfg.toolEnabled("remote_session_list") {
		s.AddTool(mcp.NewTool("remote_session_list",
			mcp.WithDescription("List live (non-expired) persistent sessions with their ids, working directories, shells and expiry times."),
		), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			list := sessions.List()
			sessionsOut := make([]SessionResponse, 0, len(list))
			for _, sess := range list {
				sessionsOut = append(sessionsOut, SessionResponse{
					SessionID:        sess.ID,
					WorkingDirectory: sess.Cwd(),
					CreatedAt:        sess.CreatedAt.Format(time.RFC3339),
					ExpiresAt:        sess.ExpiresAt.Format(time.RFC3339),
					Shell:            sess.Shell,
				})
			}
			auditCall(ctx, "", "session_list")
			return jsonResult(struct {
				Sessions []SessionResponse `json:"sessions"`
				Count    int               `json:"count"`
			}{Sessions: sessionsOut, Count: len(sessionsOut)}), nil
		})
	}

	if cfg.toolEnabled("remote_session_close") {
		s.AddTool(mcp.NewTool("remote_session_close",
			mcp.WithDescription("Destroy a persistent session by id."),
			mcp.WithString("session_id", mcp.Required(), mcp.Description("Session ID returned by remote_session_create.")),
		), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			sessionID := req.GetString("session_id", "")
			if sessionID == "" {
				return mcp.NewToolResultError("session_id is required"), nil
			}
			if sessions.Delete(sessionID) {
				auditCall(ctx, sessionID, "session_close "+sessionID)
				return mcp.NewToolResultText(fmt.Sprintf("session %s closed", sessionID)), nil
			}
			auditCall(ctx, sessionID, "session_close "+sessionID)
			return mcp.NewToolResultError("session not found: " + sessionID), nil
		})
	}
}
