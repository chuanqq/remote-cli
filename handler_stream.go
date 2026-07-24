package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

type StreamHandler struct {
	executor *Executor
	audit    *AuditLogger
}

func NewStreamHandler(executor *Executor, audit *AuditLogger) *StreamHandler {
	return &StreamHandler{executor: executor, audit: audit}
}

func (h *StreamHandler) Handle(w http.ResponseWriter, r *http.Request) {
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

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "internal_error", "Streaming not supported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// Heartbeat goroutine
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				fmt.Fprintf(w, "event: heartbeat\ndata: {\"timestamp\":\"%s\"}\n\n",
					time.Now().Format(time.RFC3339Nano))
				flusher.Flush()
			}
		}
	}()

	var exitCode int
	var durationMs int64
	var outputLines int

	h.executor.ExecuteStream(req, func(event StreamEvent) {
		if event.Type == "stdout" || event.Type == "stderr" {
			outputLines++
		}
		if event.Type == "exit" {
			exitCode = event.ExitCode
			durationMs = event.Duration
		}
		data, _ := json.Marshal(event)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.Type, string(data))
		flusher.Flush()
	})

	close(done)

	h.audit.Log(AuditEntry{
		SourceIP:         r.RemoteAddr,
		Tool:             "execute_stream",
		Command:          req.Command,
		WorkingDirectory: req.WorkingDirectory,
		ExitCode:         exitCode,
		DurationMs:       durationMs,
		OutputBytes:      outputLines,
		TimedOut:         exitCode == -1,
	})
}
