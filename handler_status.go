package main

import (
	"net/http"
	"os"
	"runtime"
	"time"
)

type StatusHandler struct {
	startTime time.Time
	sessions  *SessionManager
}

func NewStatusHandler(sessions *SessionManager) *StatusHandler {
	return &StatusHandler{
		startTime: time.Now(),
		sessions:  sessions,
	}
}

func (h *StatusHandler) Handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Use GET")
		return
	}

	hostname, _ := os.Hostname()

	var loadAvg []float64
	loadAvg = getLoadAverage()

	var memMB uint64
	memMB = getMemoryMB()

	writeJSON(w, http.StatusOK, StatusResponse{
		Status:         "healthy",
		Version:        serverVersion,
		UptimeSeconds:  int64(time.Since(h.startTime).Seconds()),
		ActiveSessions: h.sessions.Count(),
		System: SystemInfo{
			Hostname:    hostname,
			OS:          runtime.GOOS,
			Arch:        runtime.GOARCH,
			CPUs:        runtime.NumCPU(),
			MemoryMB:    memMB,
			LoadAverage: loadAvg,
		},
	})
}
