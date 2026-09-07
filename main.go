package main

import (
	"fmt"
	"log"
	"net/http"
	"strings"
)

func main() {
	cfg := LoadConfig()

	if cfg.Token == "" {
		log.Fatal("SHELL_API_TOKEN environment variable is required")
	}

	executor := NewExecutor(cfg)
	sessions := NewSessionManager()
	audit := NewAuditLogger()

	executeHandler := NewExecuteHandler(executor, audit)
	streamHandler := NewStreamHandler(executor, audit)
	sessionHandler := NewSessionHandler(sessions, executor, audit)
	statusHandler := NewStatusHandler(sessions)

	mux := http.NewServeMux()

	// Layer 2: mutating REST endpoints are replaced by a 403 responder under
	// read-only mode. They stay routed so clients get an explanatory error
	// instead of a 404 that looks like a deployment problem.
	if cfg.ReadOnly {
		for _, p := range mutatingRESTPaths {
			mux.HandleFunc(p, readOnlyRESTDenied)
		}
	} else {
		mux.HandleFunc("/api/execute", executeHandler.Handle)
		mux.HandleFunc("/api/execute/stream", streamHandler.Handle)
		mux.HandleFunc("/api/sessions", sessionHandler.HandleCreate)
		mux.HandleFunc("/api/sessions/", func(w http.ResponseWriter, r *http.Request) {
			path := strings.TrimPrefix(r.URL.Path, "/api/sessions/")
			if strings.HasSuffix(path, "/execute") {
				sessionHandler.HandleExecute(w, r)
			} else if r.Method == http.MethodDelete {
				sessionHandler.HandleDelete(w, r)
			} else {
				writeError(w, http.StatusNotFound, "not_found", "Not found")
			}
		})
		mux.HandleFunc("/api/executions/", executeHandler.HandleCancel)
	}

	mux.HandleFunc("/api/status", statusHandler.Handle)
	mux.Handle("/mcp", NewMCPHandler(executor, sessions, audit, cfg))

	rateLimiter := NewRateLimiter(cfg.RateLimit)

	// Wrapping order (outermost last) gives the request flow:
	//   Logging -> RateLimit -> Auth -> ReadOnly -> mux
	// Auth precedes the read-only filter so an unauthenticated caller still
	// gets 401 and learns nothing about the server's mode. /api/status skips
	// auth internally, so it still reaches ReadOnlyMiddleware and carries the
	// X-Read-Only header.
	var handler http.Handler = mux
	handler = ReadOnlyMiddleware(cfg.ReadOnly, handler)
	handler = AuthMiddleware(cfg.Token, handler)
	handler = RateLimitMiddleware(rateLimiter, handler)
	handler = LoggingMiddleware(handler)

	addr := ":" + cfg.Port
	log.Printf("Remote Shell API Server starting on %s", addr)
	log.Printf("  TLS: %v", cfg.TLSCert != "")
	log.Printf("  Max timeout: %ds", cfg.MaxTimeout)
	log.Printf("  Max output: %d bytes", cfg.MaxOutput)
	log.Printf("  Rate limit: %d/min", cfg.RateLimit)
	log.Printf("  MCP endpoint: /mcp (Streamable HTTP)")
	if cfg.ReadOnly {
		// Report the EFFECTIVE tool set: read-only allows 11, but an operator
		// blacklist can narrow it further, and logging the raw allowlist size
		// would misstate what clients actually see.
		var enabled []string
		for _, name := range sortedReadOnlyToolNames() {
			if cfg.toolEnabled(name) {
				enabled = append(enabled, name)
			}
		}
		log.Printf("  READ-ONLY MODE: ENABLED (highest priority)")
		log.Printf("    Enabled tools (%d): %s", len(enabled), strings.Join(enabled, ", "))
		log.Printf("    Disabled tools (%d): %s", len(readOnlyDisabledTools()), strings.Join(readOnlyDisabledTools(), ", "))
		log.Printf("    REST: only GET/HEAD under /api/ are served")
	}
	if len(cfg.FSRoots) > 0 {
		log.Printf("  FS roots: %s", strings.Join(cfg.FSRoots, ", "))
	}

	var err error
	if cfg.TLSCert != "" && cfg.TLSKey != "" {
		err = http.ListenAndServeTLS(addr, cfg.TLSCert, cfg.TLSKey, handler)
	} else {
		fmt.Println("  WARNING: Running without TLS. Use SHELL_API_TLS_CERT and SHELL_API_TLS_KEY for production.")
		err = http.ListenAndServe(addr, handler)
	}

	if err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}
