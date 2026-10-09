package main

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// ---------------------------------------------------------------------------
// MCP request context & unified audit
//
// mcpHTTPContext (WithHTTPContextFunc) injects the real peer address into the
// context of every MCP request. Previously the source IP was taken from
// X-Forwarded-For / X-Real-IP only and fell back to the literal "mcp", so all
// 3584 production audit records carried source_ip="mcp".
//
// withAudit (a tool-handler middleware) audits EVERY registered tool in one
// place: tool name (the registered name, never a hand-typed short form),
// duration, outcome and failure reason, request id, peer and MCP session.
// Handlers only describe what they did through noteAudit; a tool that forgets
// to is still audited with its name, timing and result.
// ---------------------------------------------------------------------------

type sourceIPKey struct{}

// mcpHTTPContext is the StreamableHTTPServer context func.
func mcpHTTPContext(ctx context.Context, r *http.Request) context.Context {
	ctx = context.WithValue(ctx, sourceIPKey{}, remoteHost(r.RemoteAddr))
	if ri := requestInfoFrom(r.Context()); ri != nil {
		if sid := r.Header.Get(server.HeaderKeySessionID); sid != "" {
			ri.setMCPSession(sid)
		}
	}
	return ctx
}

// sourceIPFrom returns the peer host injected by mcpHTTPContext (or by the
// access-log middleware for REST calls).
func sourceIPFrom(ctx context.Context) string {
	if ip, ok := ctx.Value(sourceIPKey{}).(string); ok && ip != "" {
		return ip
	}
	if ri := requestInfoFrom(ctx); ri != nil {
		return ri.Remote
	}
	return ""
}

func reqIDFrom(ctx context.Context) string {
	if ri := requestInfoFrom(ctx); ri != nil {
		return ri.ID
	}
	return ""
}

// auditNote is what a tool handler reports about its call. Fields left empty
// are filled by withAudit from the call result.
type auditNote struct {
	set              bool
	Command          string
	WorkingDirectory string
	ExecID           string
	SessionID        string
	ExitCode         int
	OutputBytes      int
	Truncated        bool
	TimedOut         bool
	Error            string
}

type auditNoteKey struct{}

// noteAudit lets a handler describe its call for the audit record.
func noteAudit(ctx context.Context, fn func(n *auditNote)) {
	if n, ok := ctx.Value(auditNoteKey{}).(*auditNote); ok {
		fn(n)
		n.set = true
	}
}

// maxAuditErrorLen caps the failure reason copied from a tool result.
const maxAuditErrorLen = 300

// withAudit is the tool-handler middleware that writes one audit record per
// tool call.
func withAudit(audit *AuditLogger) server.ToolHandlerMiddleware {
	return func(next server.ToolHandlerFunc) server.ToolHandlerFunc {
		return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			start := time.Now()
			tool := req.Params.Name
			if ri := requestInfoFrom(ctx); ri != nil {
				ri.setTool(tool)
			}
			note := &auditNote{}
			ctx = context.WithValue(ctx, auditNoteKey{}, note)

			res, err := next(ctx, req)

			entry := AuditEntry{
				ReqID:            reqIDFrom(ctx),
				ExecID:           note.ExecID,
				SourceIP:         sourceIPFrom(ctx),
				Tool:             tool,
				SessionID:        note.SessionID,
				Command:          note.Command,
				WorkingDirectory: note.WorkingDirectory,
				ExitCode:         note.ExitCode,
				DurationMs:       time.Since(start).Milliseconds(),
				OutputBytes:      note.OutputBytes,
				Truncated:        note.Truncated,
				TimedOut:         note.TimedOut,
				Error:            note.Error,
			}
			if cs := server.ClientSessionFromContext(ctx); cs != nil {
				entry.MCPSession = cs.SessionID()
			}
			if entry.Command == "" {
				// Never fall back to raw arguments: they may carry file
				// content (write_file) or secrets.
				entry.Command = tool
			}
			switch {
			case err != nil:
				entry.ExitCode = nonZero(entry.ExitCode)
				if entry.Error == "" {
					entry.Error = err.Error()
				}
			case res != nil && res.IsError:
				entry.ExitCode = nonZero(entry.ExitCode)
				if entry.Error == "" {
					entry.Error = resultText(res)
				}
			}
			if len(entry.Error) > maxAuditErrorLen {
				entry.Error = entry.Error[:maxAuditErrorLen] + "..."
			}
			audit.Log(entry)
			return res, err
		}
	}
}

func nonZero(code int) int {
	if code == 0 {
		return 1
	}
	return code
}

// resultText concatenates the text content of a tool result.
func resultText(res *mcp.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		if tc, ok := c.(mcp.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	return strings.Join(parts, " ")
}

// mcpMethodHooks records the JSON-RPC method of every MCP request on the
// request's correlation record, so access lines say initialize / tools/list /
// tools/call instead of an opaque "POST /mcp".
func mcpMethodHooks() *server.Hooks {
	hooks := &server.Hooks{}
	hooks.AddBeforeAny(func(ctx context.Context, _ any, method mcp.MCPMethod, _ any) {
		if ri := requestInfoFrom(ctx); ri != nil {
			ri.setMethod(string(method))
		}
	})
	return hooks
}
