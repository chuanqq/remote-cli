package main

import (
	"context"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// registerJobTools adds the background-job tools: remote_spawn starts a
// detached job and returns at once; remote_job_status / remote_job_list /
// remote_job_logs / remote_job_kill operate on it by job id.
func registerJobTools(s *server.MCPServer, jobs *JobManager, cfg *Config) {
	if cfg.toolEnabled("remote_spawn") {
		s.AddTool(mcp.NewTool("remote_spawn",
			mcp.WithDescription("Start a long-running command as a detached background job and return immediately with a job_id. Use this instead of `nohup ... &` in remote_execute: the job runs in its own session (it survives the call and is not killed by a timeout of the caller), stdout+stderr go to a server-managed log file, and the job_id works with remote_job_status, remote_job_logs (with follow), remote_job_kill and remote_wait_for(condition=process_exit, target=job_id)."),
			mcp.WithString("command", mcp.Required(), mcp.Description("Shell command to run in the background."), mcp.MaxLength(10000)),
			mcp.WithString("working_directory", mcp.Description("Working directory on the remote server.")),
			mcp.WithObject("environment", mcp.Description("Additional environment variables as key-value pairs.")),
			mcp.WithNumber("timeout_seconds", mcp.Description("Kill the job (whole process group) after this many seconds. Default 0 = no limit.")),
			mcp.WithString("shell", mcp.Description("Shell binary to use (defaults to server config).")),
		), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			command := req.GetString("command", "")
			if command == "" {
				return mcp.NewToolResultError("command is required"), nil
			}
			dir := req.GetString("working_directory", "")
			noteAudit(ctx, func(n *auditNote) { n.Command = command; n.WorkingDirectory = dir })

			job, err := jobs.Spawn(SpawnRequest{
				Command:          command,
				WorkingDirectory: dir,
				Environment:      extractEnv(req, "environment"),
				Shell:            req.GetString("shell", ""),
				TimeoutSeconds:   req.GetInt("timeout_seconds", 0),
			})
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			noteAudit(ctx, func(n *auditNote) { n.ExecID = job.ID })
			return jsonResult(job.info()), nil
		})
	}

	if cfg.toolEnabled("remote_job_status") {
		s.AddTool(mcp.NewTool("remote_job_status",
			mcp.WithDescription("Return the state of a background job started with remote_spawn: running / exited / killed / timed_out, exit code, pid, elapsed time, log path and size."),
			mcp.WithString("job_id", mcp.Required(), mcp.Description("Job ID returned by remote_spawn.")),
		), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			id := req.GetString("job_id", "")
			noteAudit(ctx, func(n *auditNote) { n.Command = "job_status " + id; n.ExecID = id })
			job := jobs.Get(id)
			if job == nil {
				return mcp.NewToolResultError("job not found: " + id), nil
			}
			return jsonResult(job.info()), nil
		})
	}

	if cfg.toolEnabled("remote_job_list") {
		s.AddTool(mcp.NewTool("remote_job_list",
			mcp.WithDescription("List background jobs known to the server (newest first). Finished jobs are kept for 24h."),
			mcp.WithBoolean("running_only", mcp.Description("Only list running jobs. Default false.")),
		), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			runningOnly := req.GetBool("running_only", false)
			out := make([]JobInfo, 0)
			for _, j := range jobs.List() {
				info := j.info()
				if runningOnly && info.State != JobRunning {
					continue
				}
				out = append(out, info)
			}
			noteAudit(ctx, func(n *auditNote) { n.Command = "job_list"; n.OutputBytes = len(out) })
			return jsonResult(struct {
				Jobs  []JobInfo `json:"jobs"`
				Count int       `json:"count"`
			}{Jobs: out, Count: len(out)}), nil
		})
	}

	if cfg.toolEnabled("remote_job_logs") {
		s.AddTool(mcp.NewTool("remote_job_logs",
			mcp.WithDescription("Read the combined stdout/stderr log of a background job. Same cursor model as remote_tail_log: last N lines by default; pass since_offset=end_offset from the previous call to read only new output, and follow_seconds to wait for it. The response includes the job state."),
			mcp.WithString("job_id", mcp.Required(), mcp.Description("Job ID returned by remote_spawn.")),
			mcp.WithNumber("lines", mcp.Description("Tail mode: return the last N lines. Default 100.")),
			mcp.WithNumber("since_offset", mcp.Description("Byte cursor: return output after this offset (end_offset of the previous call).")),
			mcp.WithNumber("follow_seconds", mcp.Description("While the job runs, wait up to N seconds (max 300) for new output.")),
			mcp.WithString("filter_regex", mcp.Description("RE2: return only matching lines.")),
			mcp.WithString("encoding", mcp.Description("Source encoding; auto-detected if omitted."), mcp.Enum("utf-8", "gbk", "gb2312", "gb18030")),
		), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			id := req.GetString("job_id", "")
			noteAudit(ctx, func(n *auditNote) { n.Command = "job_logs " + id; n.ExecID = id })
			job := jobs.Get(id)
			if job == nil {
				return mcp.NewToolResultError("job not found: " + id), nil
			}
			follow := req.GetInt("follow_seconds", 0)
			if !job.running() {
				follow = 0 // nothing more will arrive
			}
			// The job log lives in the server-managed job dir, outside any
			// FS root by design, so it is read without the sandbox.
			tail, err := TailLog(TailLogRequest{
				Path:          job.LogPath,
				Lines:         req.GetInt("lines", 0),
				SinceOffset:   int64(req.GetInt("since_offset", 0)),
				FollowSeconds: follow,
				FilterRegex:   req.GetString("filter_regex", ""),
				Encoding:      req.GetString("encoding", ""),
			}, nil)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			noteAudit(ctx, func(n *auditNote) { n.OutputBytes = len(tail.Content); n.Truncated = tail.Truncated })
			return jsonResult(struct {
				Job JobInfo        `json:"job"`
				Log *TailLogResult `json:"log"`
			}{Job: job.info(), Log: tail}), nil
		})
	}

	if cfg.toolEnabled("remote_job_kill") {
		s.AddTool(mcp.NewTool("remote_job_kill",
			mcp.WithDescription("Send a signal to a background job's whole process group (TERM by default, or INT / KILL)."),
			mcp.WithString("job_id", mcp.Required(), mcp.Description("Job ID returned by remote_spawn.")),
			mcp.WithString("signal", mcp.Description("Signal to send. Default TERM."), mcp.Enum("TERM", "INT", "KILL")),
		), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			id := req.GetString("job_id", "")
			sig := req.GetString("signal", "")
			noteAudit(ctx, func(n *auditNote) { n.Command = fmt.Sprintf("job_kill %s %s", id, sig); n.ExecID = id })
			job, err := jobs.Kill(id, sig)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			return jsonResult(job.info()), nil
		})
	}
}

// registerWaitTool adds remote_wait_for.
func registerWaitTool(s *server.MCPServer, executor *Executor, jobs *JobManager, cfg *Config) {
	if !cfg.toolEnabled("remote_wait_for") {
		return
	}
	s.AddTool(mcp.NewTool("remote_wait_for",
		mcp.WithDescription("Wait server-side until a condition holds, then return immediately with what matched — instead of `sleep N; check` polling through remote_execute. Conditions: file_exists (target=path); file_contains (target=path, pattern=RE2 over the whole file); log_regex (target=log path, pattern=RE2, only lines appended after the call starts count); process_exit (target=pid or remote_spawn job_id); port_listen (target=port); command_exit0 (target=shell command, re-run every interval until it exits 0). Returns satisfied=false, timed_out=true when timeout_seconds elapses first."),
		mcp.WithString("condition", mcp.Required(), mcp.Description("Condition type."),
			mcp.Enum(WaitFileExists, WaitFileContains, WaitLogRegex, WaitProcessExit, WaitPortListen, WaitCommandExit0)),
		mcp.WithString("target", mcp.Required(), mcp.Description("Path, pid / job id, port, or shell command, depending on condition.")),
		mcp.WithString("pattern", mcp.Description("RE2 pattern for file_contains / log_regex.")),
		mcp.WithBoolean("ignore_case", mcp.Description("Case-insensitive pattern. Default false.")),
		mcp.WithNumber("timeout_seconds", mcp.Description("Give up after this many seconds. Default 60, max 300.")),
		mcp.WithNumber("interval_ms", mcp.Description("Check interval. Default 1000, min 200.")),
		mcp.WithString("encoding", mcp.Description("File conditions: source encoding of the file (e.g. gbk) so patterns can match Chinese text."), mcp.Enum("utf-8", "gbk", "gb2312", "gb18030")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		wreq := WaitForRequest{
			Condition:  req.GetString("condition", ""),
			Target:     req.GetString("target", ""),
			Pattern:    req.GetString("pattern", ""),
			IgnoreCase: req.GetBool("ignore_case", false),
			TimeoutSec: req.GetInt("timeout_seconds", 0),
			IntervalMs: req.GetInt("interval_ms", 0),
			Encoding:   req.GetString("encoding", ""),
		}
		noteAudit(ctx, func(n *auditNote) {
			n.Command = fmt.Sprintf("wait_for %s %s", wreq.Condition, wreq.Target)
			if wreq.Pattern != "" {
				n.Command += " /" + wreq.Pattern + "/"
			}
		})
		res, err := WaitFor(ctx, wreq, waitDeps{roots: cfg.FSRoots, executor: executor, jobs: jobs})
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		noteAudit(ctx, func(n *auditNote) { n.TimedOut = res.TimedOut })
		return jsonResult(res), nil
	})
}
