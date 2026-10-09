package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "-v" || os.Args[1] == "version") {
		fmt.Println(versionBanner())
		return
	}

	cfg := LoadConfig()

	// One structured JSON logger for everything: access lines, audit lines,
	// and (via SetDefault) any stray log.Printf from other packages.
	logger := newLogger(os.Stderr, cfg.LogLevel)
	slog.SetDefault(logger)

	if cfg.Token == "" {
		logger.Error("SHELL_API_TOKEN environment variable is required")
		os.Exit(1)
	}

	executor := NewExecutor(cfg)
	sessions := NewSessionManager()
	jobs := NewJobManager(cfg)
	audit := NewAuditLogger(logger)

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
	mux.Handle("/mcp", NewMCPHandler(executor, sessions, jobs, audit, cfg))

	rateLimiter := NewRateLimiter(cfg.RateLimit, cfg.RateBurst)

	// endStreams ends long-lived MCP GET streams when shutdown begins, so
	// they do not hold Shutdown open for the whole grace period.
	streamsCtx, endStreams := context.WithCancel(context.Background())

	// Wrapping order (outermost last) gives the request flow:
	//   Logging -> RateLimit -> Auth -> ReadOnly -> Drain -> mux
	// Auth precedes the read-only filter so an unauthenticated caller still
	// gets 401 and learns nothing about the server's mode. /api/status skips
	// auth internally, so it still reaches ReadOnlyMiddleware and carries the
	// X-Read-Only header. Logging is outermost so 401/429 are logged too.
	var handler http.Handler = mux
	handler = drainStreamsMiddleware(streamsCtx, handler)
	handler = ReadOnlyMiddleware(cfg.ReadOnly, handler)
	handler = AuthMiddleware(cfg.Token, handler)
	handler = RateLimitMiddleware(rateLimiter, handler)
	handler = LoggingMiddleware(logger, handler)

	// baseCtx is the parent of every request context. It is cancelled only
	// when the shutdown grace period runs out, which kills whatever is still
	// executing (Execute inherits the request context).
	baseCtx, cancelBase := context.WithCancel(context.Background())
	defer cancelBase()

	srv := &http.Server{
		Addr:    ":" + cfg.Port,
		Handler: handler,
		// Slowloris protection. No ReadTimeout / WriteTimeout on purpose:
		// remote_execute calls legitimately run up to MaxTimeout and the MCP
		// GET stream stays open indefinitely.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
		BaseContext:       func(net.Listener) context.Context { return baseCtx },
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}

	logStartup(logger, cfg)

	serveErr := make(chan error, 1)
	go func() {
		if cfg.TLSCert != "" && cfg.TLSKey != "" {
			serveErr <- srv.ListenAndServeTLS(cfg.TLSCert, cfg.TLSKey)
		} else {
			logger.Warn("running without TLS; set SHELL_API_TLS_CERT and SHELL_API_TLS_KEY for production")
			serveErr <- srv.ListenAndServe()
		}
	}()

	sigCtx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stopSignals()

	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server failed", "err", err.Error())
			os.Exit(1)
		}
		return
	case <-sigCtx.Done():
	}
	stopSignals() // a second signal now terminates immediately

	gracefulShutdown(logger, srv, executor, jobs, endStreams, cancelBase, cfg.ShutdownGrace)
}

// gracefulShutdown stops accepting connections, lets in-flight executions
// finish for up to grace, then kills what is left (their whole process groups)
// and closes the server. Background jobs from remote_spawn run in their own
// sessions and are deliberately left running.
func gracefulShutdown(logger *slog.Logger, srv *http.Server, executor *Executor, jobs *JobManager,
	endStreams, cancelBase context.CancelFunc, grace time.Duration) {
	logger.Info("shutdown: signal received, draining",
		"running_executions", executor.Running(), "grace_sec", int(grace.Seconds()))
	endStreams()

	ctx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	err := srv.Shutdown(ctx)
	if err != nil {
		n := executor.CancelAll()
		cancelBase()
		// Give the killed calls' handlers a moment to write their audit
		// lines before the process exits (bounded: kill + WaitDelay).
		deadline := time.Now().Add(execWaitDelay + time.Second)
		for executor.Running() > 0 && time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
		}
		time.Sleep(100 * time.Millisecond)
		_ = srv.Close()
		logger.Warn("shutdown: grace period expired, killed remaining executions", "killed", n)
	}
	if n := jobs.Running(); n > 0 {
		logger.Info("shutdown: background jobs left running (detached)", "jobs", n)
	}
	logger.Info("shutdown: complete")
}

// drainStreamsMiddleware ties the MCP GET stream to done, so shutdown can end
// these keep-alive connections without cancelling real work.
func drainStreamsMiddleware(done context.Context, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isMCPStream(r) {
			next.ServeHTTP(w, r)
			return
		}
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		stop := context.AfterFunc(done, cancel)
		defer stop()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func logStartup(logger *slog.Logger, cfg *Config) {
	attrs := []any{
		"version", serverVersion,
		"commit", gitCommit,
		"build_time", buildTime,
		"addr", ":" + cfg.Port,
		"tls", cfg.TLSCert != "",
		"max_timeout_sec", cfg.MaxTimeout,
		"max_output", cfg.MaxOutput,
		"rate_limit_per_min", cfg.RateLimit,
		"rate_burst", cfg.RateBurst,
		"mcp_heartbeat_sec", int(cfg.MCPHeartbeat.Seconds()),
		"job_dir", cfg.JobDir,
		"log_level", cfg.LogLevel,
	}
	if len(cfg.FSRoots) > 0 {
		attrs = append(attrs, "fs_roots", strings.Join(cfg.FSRoots, ","))
	}
	if cfg.DenyCommand != nil {
		attrs = append(attrs, "deny_commands", cfg.DenyCommand.String())
	}
	logger.Info("Remote Shell API Server starting (MCP endpoint /mcp, Streamable HTTP)", attrs...)

	if cfg.ReadOnly {
		// Report the EFFECTIVE tool set: read-only allows a fixed set, but an
		// operator blacklist can narrow it further, and logging the raw
		// allowlist size would misstate what clients actually see.
		var enabled []string
		for _, name := range sortedReadOnlyToolNames() {
			if cfg.toolEnabled(name) {
				enabled = append(enabled, name)
			}
		}
		logger.Info("READ-ONLY MODE: ENABLED (highest priority); REST: only GET/HEAD under /api/ are served",
			"enabled_tools", strings.Join(enabled, ","),
			"disabled_tools", strings.Join(readOnlyDisabledTools(), ","))
	}
}
