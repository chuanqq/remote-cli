package main

import (
	"time"
)

type ExecuteRequest struct {
	Command          string            `json:"command"`
	WorkingDirectory string            `json:"working_directory,omitempty"`
	Environment      map[string]string `json:"environment,omitempty"`
	TimeoutMs        int               `json:"timeout_ms,omitempty"`
	MaxOutputBytes   int               `json:"max_output_bytes,omitempty"`
	Shell            string            `json:"shell,omitempty"`
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
	Status         string     `json:"status"`
	Version        string     `json:"version"`
	UptimeSeconds  int64      `json:"uptime_seconds"`
	ActiveSessions int        `json:"active_sessions"`
	System         SystemInfo `json:"system"`
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

type AuditEntry struct {
	Timestamp        time.Time `json:"timestamp"`
	RequestID        string    `json:"request_id"`
	SourceIP         string    `json:"source_ip"`
	Command          string    `json:"command"`
	WorkingDirectory string    `json:"working_directory"`
	ExitCode         int       `json:"exit_code"`
	DurationMs       int64     `json:"duration_ms"`
	OutputBytes      int       `json:"output_bytes"`
	TimedOut         bool      `json:"timed_out"`
}
