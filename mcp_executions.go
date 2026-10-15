package main

import (
	"context"
	"fmt"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// maxListedExecs caps the execution list, which lands in the agent's context.
const maxListedExecs = 20

// launchAndAwait starts req detached from the request (see executions.go)
// and waits for it up to budget, or until the client gives up. It returns the
// result when the command finished in time; nil means it is still running,
// and its outcome is audited by a second line once it ends.
func launchAndAwait(ctx context.Context, executor *Executor, audit *AuditLogger, req ExecuteRequest,
	budget time.Duration, tool, sessionID string) (*Execution, *ExecResult) {
	ex := executor.Launch(req, lateAudit(ctx, audit, tool, sessionID))
	ex.Await(ctx, budget)
	return ex, ex.claim()
}

// lateAudit returns the callback that audits an execution which outlived its
// call. The call's own line says state=running; this one carries the same
// exec_id and the final state, exit code and duration.
func lateAudit(ctx context.Context, audit *AuditLogger, tool, sessionID string) func(*ExecResult) {
	base := AuditEntry{ReqID: reqIDFrom(ctx), SourceIP: sourceIPFrom(ctx), Tool: tool, SessionID: sessionID}
	if cs := server.ClientSessionFromContext(ctx); cs != nil {
		base.MCPSession = cs.SessionID()
		base.Client = clientLabel(cs)
	}
	return func(r *ExecResult) {
		e := base
		e.ExecID = r.ID
		e.Command = r.Command
		e.WorkingDirectory = r.WorkingDirectory
		e.ExitCode = r.ExitCode
		e.DurationMs = r.DurationMs
		e.OutputBytes = len(r.Stdout) + len(r.Stderr)
		e.Truncated = r.StdoutTruncated || r.StderrTruncated
		e.TimedOut = r.TimedOut
		e.Error = r.Error
		e.State = execState(r)
		audit.Log(e)
	}
}

// notePendingAudit fills the audit record of a call that returned while its
// command was still running.
func notePendingAudit(ctx context.Context, ex *Execution, sessionID string) {
	noteAudit(ctx, func(n *auditNote) {
		n.Command = ex.Command
		n.WorkingDirectory = ex.WorkingDirectory
		n.ExecID = ex.ID
		n.SessionID = sessionID
		n.State = ExecRunning
	})
}

// pendingResult answers a call whose command did not finish within budget.
func pendingResult(ex *Execution, budget time.Duration) *mcp.CallToolResult {
	return jsonResult(ExecutionPending{
		ExecutionInfo: ex.info(),
		Hint: fmt.Sprintf("did not finish within the %ds call budget and was NOT killed: it stops only at its "+
			"timeout_ms or via remote_cancel. Do not re-run the command; call remote_execution_result with "+
			"execution_id=%q to wait for and fetch the final result.", int(budget/time.Second), ex.ID),
	})
}

// registerExecutionResultTool adds remote_execution_result, the companion of
// remote_execute / remote_session_execute for commands that outlive a call.
func registerExecutionResultTool(s *server.MCPServer, executor *Executor, cfg *Config) {
	if !cfg.toolEnabled("remote_execution_result") ||
		!(cfg.toolEnabled("remote_execute") || cfg.toolEnabled("remote_session_execute")) {
		return
	}
	budget := cfg.callBudget()
	s.AddTool(mcp.NewTool("remote_execution_result",
		mcp.WithDescription(fmt.Sprintf("Fetch the outcome of a remote_execute / remote_session_execute command that returned state=running: "+
			"waits up to wait_seconds (default and max %[1]d) for it to finish, then returns the full result "+
			"(state completed / timed_out / cancelled, exit_code, stdout, stderr), or state=running again — just call again. "+
			"Without execution_id it lists the newest %[4]d executions of the last %[2]d minutes with state and exit code: "+
			"use that when a call was cut off before any reply, to learn whether and how the command ran instead of re-running it. "+
			"The newest %[3]d finished executions also keep their stdout/stderr, so a result whose reply was lost can be fetched again.",
			int(budget/time.Second), int(execRetention/time.Minute), maxRetainedOutputs, maxListedExecs)),
		mcp.WithString("execution_id", mcp.Description("execution_id from a state=running reply. Omit to list recent executions.")),
		mcp.WithNumber("wait_seconds", mcp.Description(fmt.Sprintf("How long to wait for a running command, 0 = just check. Default and max %d.", int(budget/time.Second)))),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id := req.GetString("execution_id", "")
		if id == "" {
			list := executor.Executions()
			infos := make([]ExecutionInfo, 0, min(len(list), maxListedExecs))
			for _, ex := range list[:min(len(list), maxListedExecs)] {
				infos = append(infos, ex.info())
			}
			noteAudit(ctx, func(n *auditNote) { n.Command = "execution_list"; n.OutputBytes = len(infos) })
			return jsonResult(struct {
				Executions []ExecutionInfo `json:"executions"`
				Count      int             `json:"count"`
				Total      int             `json:"total"`
			}{Executions: infos, Count: len(infos), Total: len(list)}), nil
		}

		noteAudit(ctx, func(n *auditNote) { n.Command = "execution_result " + id; n.ExecID = id })
		ex := executor.Lookup(id)
		if ex == nil {
			return mcp.NewToolResultError(fmt.Sprintf("execution not found: %s (executions are listed for %d minutes "+
				"after they end; call without execution_id to list them)", id, int(execRetention/time.Minute))), nil
		}
		wait := time.Duration(req.GetInt("wait_seconds", int(budget/time.Second))) * time.Second
		if wait > budget {
			wait = budget
		}
		r, dropped := ex.Await(ctx, wait)
		if r == nil {
			noteAudit(ctx, func(n *auditNote) { n.State = ExecRunning })
			return pendingResult(ex, budget), nil
		}
		noteAudit(ctx, func(n *auditNote) {
			n.State = execState(r)
			n.ExitCode = r.ExitCode
			n.TimedOut = r.TimedOut
		})
		resp := execResultResponse(r)
		if dropped {
			resp.Hint = fmt.Sprintf("stdout/stderr are no longer retained (only the newest %d finished executions keep "+
				"their output); state and exit_code are authoritative", maxRetainedOutputs)
		}
		return jsonResult(resp), nil
	})
}
