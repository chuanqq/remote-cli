package main

import "fmt"

// Build metadata. serverVersion keeps a source default so `go build` without
// ldflags still reports something sensible; build.sh / control.sh / start.sh
// override all three via -ldflags "-X main.serverVersion=... -X main.gitCommit=...
// -X main.buildTime=...". They must stay package-level vars (not consts) for
// -X to take effect.
var (
	serverVersion = "1.1.0"
	gitCommit     = "unknown"
	buildTime     = "unknown"
)

// fullVersion is the single-line version string used by the startup log,
// --version and the MCP handshake (serverInfo.version), e.g. "1.1.0+a5e9ecc".
func fullVersion() string {
	if gitCommit == "" || gitCommit == "unknown" {
		return serverVersion
	}
	return serverVersion + "+" + gitCommit
}

// versionBanner is the human-readable output of --version.
func versionBanner() string {
	return fmt.Sprintf("remote-agent-proxy %s (commit %s, built %s)", serverVersion, gitCommit, buildTime)
}

type ExecuteRequest struct {
	Command          string            `json:"command"`
	WorkingDirectory string            `json:"working_directory,omitempty"`
	Environment      map[string]string `json:"environment,omitempty"`
	TimeoutMs        int               `json:"timeout_ms,omitempty"`
	MaxOutputBytes   int               `json:"max_output_bytes,omitempty"`
	Shell            string            `json:"shell,omitempty"`
	// TruncateMode picks which end of an over-limit stdout/stderr to keep:
	// "head" (default, first bytes) or "tail" (last bytes, better for logs).
	TruncateMode string `json:"truncate_mode,omitempty"`
	// OutputEncoding converts captured stdout/stderr to UTF-8 before returning:
	// "" / "utf-8" (raw bytes, default), "gbk", "gb2312", "gb18030", or "auto"
	// (valid UTF-8 is kept as-is, otherwise decoded as GBK).
	OutputEncoding string `json:"output_encoding,omitempty"`
}

type ExecuteResponse struct {
	ID               string `json:"id"`
	Command          string `json:"command"`
	ExitCode         int    `json:"exit_code"`
	Stdout           string `json:"stdout"`
	Stderr           string `json:"stderr"`
	DurationMs       int64  `json:"duration_ms"`
	StartedAt        string `json:"started_at"`
	CompletedAt      string `json:"completed_at"`
	WorkingDirectory string `json:"working_directory"`
	TimedOut         bool   `json:"timed_out"`
	StdoutTruncated  bool   `json:"stdout_truncated"`
	StderrTruncated  bool   `json:"stderr_truncated"`
	// Total bytes the command wrote, before truncation. When larger than
	// len(stdout)/len(stderr) the difference was dropped by max_output_bytes.
	StdoutTotalBytes int64 `json:"stdout_total_bytes"`
	StderrTotalBytes int64 `json:"stderr_total_bytes"`
	// OutputEncoding is the source encoding stdout/stderr were decoded from
	// (only set when output_encoding was requested).
	OutputEncoding string `json:"output_encoding,omitempty"`
	// Error explains a failure to launch the command at all (bad working
	// directory, missing shell, denied by policy). Empty when it ran.
	Error string `json:"error,omitempty"`
	// Hint is an advisory nudge towards a better-suited tool (MCP only).
	Hint string `json:"hint,omitempty"`
}

type StreamEvent struct {
	Type      string `json:"type"`
	Line      string `json:"line,omitempty"`
	ExitCode  int    `json:"exit_code,omitempty"`
	Duration  int64  `json:"duration_ms,omitempty"`
	Timestamp string `json:"timestamp"`
}

type SessionCreateRequest struct {
	WorkingDirectory string            `json:"working_directory,omitempty"`
	Environment      map[string]string `json:"environment,omitempty"`
	Shell            string            `json:"shell,omitempty"`
	TTLSeconds       int               `json:"ttl_seconds,omitempty"`
}

type SessionResponse struct {
	SessionID        string `json:"session_id"`
	WorkingDirectory string `json:"working_directory"`
	CreatedAt        string `json:"created_at"`
	ExpiresAt        string `json:"expires_at"`
	Shell            string `json:"shell"`
}

type StatusResponse struct {
	Status         string `json:"status"`
	Version        string `json:"version"`
	Commit         string `json:"commit"`
	BuildTime      string `json:"build_time"`
	UptimeSeconds  int64  `json:"uptime_seconds"`
	ActiveSessions int    `json:"active_sessions"`
	// ReadOnly reports whether the server refuses all mutating operations.
	ReadOnly bool       `json:"read_only"`
	System   SystemInfo `json:"system"`
}

type SystemInfo struct {
	Hostname    string    `json:"hostname"`
	OS          string    `json:"os"`
	Arch        string    `json:"arch"`
	CPUs        int       `json:"cpus"`
	MemoryMB    uint64    `json:"memory_mb"`
	LoadAverage []float64 `json:"load_average"`
}

type ErrorResponse struct {
	Error     string `json:"error"`
	Message   string `json:"message"`
	RequestID string `json:"request_id,omitempty"`
}

// AuditEntry is one audit record. It is emitted as a flat structured log line
// (type=audit) sharing req_id with the access log line of the same request.
type AuditEntry struct {
	ReqID            string // HTTP request correlation id (X-Request-Id)
	ExecID           string // execution / job id, for command-running tools
	SourceIP         string // real peer host
	MCPSession       string // Mcp-Session-Id, MCP calls only
	Tool             string // registered tool name, or REST endpoint label
	SessionID        string // remote_session_* session
	Command          string // command line or synthetic "op path" descriptor
	WorkingDirectory string
	ExitCode         int
	DurationMs       int64
	OutputBytes      int
	Truncated        bool
	TimedOut         bool
	Error            string // failure reason, when the call failed
}
