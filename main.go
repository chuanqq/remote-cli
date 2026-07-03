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
	mux.HandleFunc("/api/status", statusHandler.Handle)
	mux.Handle("/mcp", NewMCPHandler(executor, sessions, audit, cfg))

	rateLimiter := NewRateLimiter(cfg.RateLimit)

	var handler http.Handler = mux
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
