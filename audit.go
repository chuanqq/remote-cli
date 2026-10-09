package main

import (
	"context"
	"log/slog"
)

// AuditLogger writes one structured (type=audit) line per tool call / REST
// command, through the same slog logger as the access log.
type AuditLogger struct {
	logger *slog.Logger
}

func NewAuditLogger(logger *slog.Logger) *AuditLogger {
	if logger == nil {
		logger = slog.Default()
	}
	return &AuditLogger{logger: logger}
}

func (a *AuditLogger) Log(entry AuditEntry) {
	// Never persist secrets (passwords, tokens) to the log.
	cmd := RedactCommand(entry.Command)
	attrs := []slog.Attr{
		slog.String("type", "audit"),
		slog.String("req_id", entry.ReqID),
		slog.String("remote", entry.SourceIP),
		slog.String("tool", entry.Tool),
		slog.String("command", cmd),
		slog.Int("exit_code", entry.ExitCode),
		slog.Int64("dur_ms", entry.DurationMs),
		slog.Int("output_bytes", entry.OutputBytes),
		slog.Bool("timed_out", entry.TimedOut),
	}
	if entry.ExecID != "" {
		attrs = append(attrs, slog.String("exec_id", entry.ExecID))
	}
	if entry.MCPSession != "" {
		attrs = append(attrs, slog.String("mcp_session", entry.MCPSession))
	}
	if entry.SessionID != "" {
		attrs = append(attrs, slog.String("session_id", entry.SessionID))
	}
	if entry.WorkingDirectory != "" {
		attrs = append(attrs, slog.String("working_directory", entry.WorkingDirectory))
	}
	if entry.Truncated {
		attrs = append(attrs, slog.Bool("truncated", true))
	}
	if entry.Error != "" {
		attrs = append(attrs, slog.String("error", RedactCommand(entry.Error)))
	}
	a.logger.LogAttrs(context.Background(), slog.LevelInfo, "audit", attrs...)
}
