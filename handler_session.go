package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

type SessionHandler struct {
	sessions *SessionManager
	executor *Executor
	audit    *AuditLogger
}

func NewSessionHandler(sessions *SessionManager, executor *Executor, audit *AuditLogger) *SessionHandler {
	return &SessionHandler{sessions: sessions, executor: executor, audit: audit}
}

func (h *SessionHandler) HandleCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Use POST")
		return
	}

	var req SessionCreateRequest
	if r.Body != nil {
		json.NewDecoder(r.Body).Decode(&req)
	}

	sess := h.sessions.Create(req)

	writeJSON(w, http.StatusCreated, SessionResponse{
		SessionID:        sess.ID,
		WorkingDirectory: sess.WorkingDirectory,
		CreatedAt:        sess.CreatedAt.Format(time.RFC3339),
		ExpiresAt:        sess.ExpiresAt.Format(time.RFC3339),
		Shell:            sess.Shell,
	})
}

func (h *SessionHandler) HandleExecute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Use POST")
		return
	}

	// Extract session ID: /api/sessions/{id}/execute
	path := r.URL.Path
	path = strings.TrimPrefix(path, "/api/sessions/")
	parts := strings.SplitN(path, "/", 2)
	if len(parts) < 2 || parts[1] != "execute" {
		writeError(w, http.StatusBadRequest, "invalid_request", "Invalid path")
		return
	}
	sessionID := parts[0]

	sess := h.sessions.Get(sessionID)
	if sess == nil {
		writeError(w, http.StatusNotFound, "not_found", "Session not found or expired")
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

	// Use session's working directory and environment
	req.WorkingDirectory = sess.WorkingDirectory
	req.Shell = sess.Shell
	if req.Environment == nil {
		req.Environment = make(map[string]string)
	}
	for _, e := range sess.Environment {
		parts := strings.SplitN(e, "=", 2)
		if len(parts) == 2 {
			if _, exists := req.Environment[parts[0]]; !exists {
				req.Environment[parts[0]] = parts[1]
			}
		}
	}

	result := h.executor.Execute(req)

	// Sync session cwd after a pure cd. Run "cmd && pwd" in one subprocess so
	// the directory change takes effect before pwd reports it; skip compound
	// commands to avoid re-running side effects (e.g. "cd x && ls").
	if strings.HasPrefix(strings.TrimSpace(req.Command), "cd ") && !strings.ContainsAny(req.Command, "&;|><\x60()") && result.ExitCode == 0 {
		pwdResult := h.executor.ExecuteInDir(sess.Shell, req.Command+" && pwd", sess.WorkingDirectory, sess.Environment, 5000)
		if pwdResult.ExitCode == 0 {
			newDir := strings.TrimSpace(pwdResult.Stdout)
			if newDir != "" {
				h.sessions.UpdateWorkingDirectory(sessionID, newDir)
			}
		}
	}

	h.audit.Log(AuditEntry{
		RequestID:        result.ID,
		SourceIP:         r.RemoteAddr,
		Command:          req.Command,
		WorkingDirectory: sess.WorkingDirectory,
		ExitCode:         result.ExitCode,
		DurationMs:       result.DurationMs,
		OutputBytes:      len(result.Stdout) + len(result.Stderr),
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
		WorkingDirectory: sess.WorkingDirectory,
		TimedOut:         result.TimedOut,
		StdoutTruncated:  result.StdoutTruncated,
		StderrTruncated:  result.StderrTruncated,
	})
}

func (h *SessionHandler) HandleDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Use DELETE")
		return
	}

	// Extract session ID: /api/sessions/{id}
	sessionID := strings.TrimPrefix(r.URL.Path, "/api/sessions/")
	if sessionID == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "session id is required")
		return
	}

	if h.sessions.Delete(sessionID) {
		w.WriteHeader(http.StatusNoContent)
	} else {
		writeError(w, http.StatusNotFound, "not_found", "Session not found")
	}
}
