package main

import (
	"encoding/json"
	"net/http"
	"time"
)

type ExecuteHandler struct {
	executor *Executor
	audit    *AuditLogger
}

func NewExecuteHandler(executor *Executor, audit *AuditLogger) *ExecuteHandler {
	return &ExecuteHandler{executor: executor, audit: audit}
}

func (h *ExecuteHandler) Handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Use POST")
		return
	}

	var req ExecuteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "Invalid JSON body: "+err.Error())
		return
	}

	if req.Command == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "command is required")
		return
	}

	if len(req.Command) > 10000 {
		writeError(w, http.StatusBadRequest, "invalid_request", "command exceeds maximum length of 10000 characters")
		return
	}

	result := h.executor.Execute(req)

	h.audit.Log(AuditEntry{
		RequestID:        result.ID,
		SourceIP:         r.RemoteAddr,
		Tool:             "execute",
		Command:          req.Command,
		WorkingDirectory: req.WorkingDirectory,
		ExitCode:         result.ExitCode,
		DurationMs:       result.DurationMs,
		OutputBytes:      len(result.Stdout) + len(result.Stderr),
		Truncated:        result.StdoutTruncated || result.StderrTruncated,
		TimedOut:         result.TimedOut,
	})

	status := http.StatusOK
	if result.TimedOut {
		status = http.StatusRequestTimeout
	}

	writeJSON(w, status, ExecuteResponse{
		ID:               result.ID,
		Command:          result.Command,
		ExitCode:         result.ExitCode,
		Stdout:           result.Stdout,
		Stderr:           result.Stderr,
		DurationMs:       result.DurationMs,
		StartedAt:        result.StartedAt.Format(time.RFC3339Nano),
		CompletedAt:      result.CompletedAt.Format(time.RFC3339Nano),
		WorkingDirectory: result.WorkingDirectory,
		TimedOut:         result.TimedOut,
		StdoutTruncated:  result.StdoutTruncated,
		StderrTruncated:  result.StderrTruncated,
	})
}

func (h *ExecuteHandler) HandleCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Use DELETE")
		return
	}

	id := r.URL.Path[len("/api/executions/"):]
	if id == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "execution id is required")
		return
	}

	if h.executor.Cancel(id) {
		w.WriteHeader(http.StatusAccepted)
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "cancelled"})
	} else {
		writeError(w, http.StatusNotFound, "not_found", "Execution not found or already completed")
	}
}
