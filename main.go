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
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/chuanqq/remote-cli/internal/control"
)

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "-v" || os.Args[1] == "version") {
		fmt.Println(versionBanner())
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "hash-token" {
		runHashToken()
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "cluster-token" {
		runClusterToken(os.Args[2:])
		return
	}

	cfg := LoadConfig()

	// One structured JSON logger for everything: access lines, audit lines,
	// and (via SetDefault) any stray log.Printf from other packages.
	logger := newLogger(os.Stderr, cfg.LogLevel)
	slog.SetDefault(logger)

	switch cfg.Mode {
	case modeStandalone:
		runStandalone(cfg, logger)
	case modeMaster:
		runMaster(cfg, logger)
	case modeSlaver:
		runSlaver(cfg, logger)
	default:
		logger.Error("unsupported REMOTECTL_MODE", "mode", cfg.Mode)
		os.Exit(1)
	}
}

// runStandalone is the 1.8.0-compatible path: a local runtime exposed over the
// single public MCP endpoint.
func runStandalone(cfg *Config, logger *slog.Logger) {
	auth, authMode := startAuth(cfg, logger)
	app := newServerApp(cfg, logger, authMode)
	handler := newHTTPHandler(
		newMCPHandlerFromOperations(buildOperations(app.runtime), app.audit, cfg),
		auth, cfg, logger, app.streamsCtx)
	app.serve(handler, nil)
}

// runMaster exposes the public MCP surface plus the control-plane listener and
// node registry.
func runMaster(cfg *Config, logger *slog.Logger) {
	if err := cfg.validateMasterConfig(); err != nil {
		logger.Error("invalid master configuration", "err", err.Error())
		os.Exit(1)
	}
	auth, authMode := startAuth(cfg, logger)
	app := newServerApp(cfg, logger, authMode)

	master, err := control.NewMaster(control.MasterOptions{
		Listen:            cfg.ControlListen,
		StorePath:         filepath.Join(cfg.StateDir, "master", "nodes.json"),
		Approval:          cfg.NodeApproval,
		Allowlist:         cfg.NodeAllowlist,
		VerifyToken:       newClusterTokenVerifier(cfg.ClusterTokenHash, clusterTokensPath(cfg.StateDir)),
		MasterVersion:     fullVersion(),
		HeartbeatInterval: cfg.HeartbeatInterval,
		LeaseTTL:          cfg.LeaseTTL,
		StaleAfter:        cfg.StaleAfter,
		OfflineAfter:      cfg.OfflineAfter,
		Logger:            logger,
	})
	if err != nil {
		logger.Error("initialize master failed", "err", err.Error())
		os.Exit(1)
	}
	if err := master.Start(); err != nil {
		logger.Error("start control channel failed", "err", err.Error())
		os.Exit(1)
	}
	app.runtime.nodes = master.Registry()
	app.runtime.nodeAdmin = master

	router := newMasterRouter(cfg, master, logger)
	handler := newHTTPHandler(
		newMCPHandlerFromOperationsWithMiddleware(buildOperations(app.runtime), app.audit, cfg,
			router.middleware()),
		auth, cfg, logger, app.streamsCtx)

	logger.Info("master control plane ready",
		"control_addr", master.Addr(), "approval", cfg.NodeApproval,
		"master_local", cfg.MasterLocal, "state_dir", cfg.StateDir)
	app.serve(handler, master.Stop)
}

// runSlaver runs the local execution core only: no HTTP listener, no MCP
// surface. It dials the master and serves invokes over the control stream.
func runSlaver(cfg *Config, logger *slog.Logger) {
	if err := cfg.validateSlaverConfig(); err != nil {
		logger.Error("invalid slaver configuration", "err", err.Error())
		os.Exit(1)
	}

	nodeID, err := control.EnsureNodeID(cfg.StateDir, cfg.NodeIDOverride)
	if err != nil {
		logger.Error("initialize node identity failed", "err", err.Error())
		os.Exit(1)
	}
	// Job manifests are labelled with the stable node identity.
	cfg.ResolvedNodeID = nodeID

	executor := NewExecutor(cfg)
	sessions := NewSessionManager()
	jobs := NewJobManager(cfg)
	waits := NewWaitManager()
	audit := NewAuditLogger(logger)
	rt := newLocalRuntime(executor, sessions, jobs, waits, audit, cfg)
	ops := buildOperations(rt)

	hostname, _ := os.Hostname()
	nodeName := cfg.NodeName
	if nodeName == "" {
		nodeName = hostname
	}
	deny := ""
	if cfg.DenyCommand != nil {
		deny = cfg.DenyCommand.String()
	}
	slaver, err := control.NewSlaver(control.SlaverOptions{
		MasterURL:    cfg.MasterURL,
		ClusterToken: cfg.ClusterToken,
		NodeID:       nodeID,
		NodeName:     nodeName,
		Version:      fullVersion(),
		Hostname:     hostname,
		Platform:     runtime.GOOS + "/" + runtime.GOARCH,
		Capabilities: control.Capabilities{
			Operations: ops.Names(),
			ReadOnly:   cfg.ReadOnly,
			Limits: control.Limits{
				MaxTimeoutSec:  int32(cfg.MaxTimeout),
				MaxOutput:      int64(cfg.MaxOutput),
				CallBudgetSec:  int32(cfg.callBudgetSec()),
				MaxActiveScans: int32(maxActiveScans),
				MaxActiveWaits: int32(maxActiveWaits),
			},
			FSRoots:      cfg.FSRoots,
			DenyCommands: deny,
		},
		Executor:          &operationExecutor{ops: ops},
		StateDir:          cfg.StateDir,
		HeartbeatInterval: cfg.HeartbeatInterval,
		LeaseTTL:          cfg.LeaseTTL,
		Logger:            logger,
	})
	if err != nil {
		logger.Error("initialize slaver failed", "err", err.Error())
		os.Exit(1)
	}

	logger.Info("slaver starting", "node_id", nodeID, "node_name", nodeName,
		"master", cfg.MasterURL, "state_dir", cfg.StateDir, "readonly", cfg.ReadOnly)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	runErr := slaver.Run(ctx)
	slaverDrain(logger, executor, jobs, waits, cfg.ShutdownGrace)
	if runErr != nil {
		logger.Error("slaver stopped with error", "err", runErr.Error())
		os.Exit(1)
	}
}

// startAuth validates the 1.x HTTP contract shared by standalone and master.
func startAuth(cfg *Config, logger *slog.Logger) (*tokenAuth, string) {
	// D6: the built-in default token is public and may only be used on a
	// loopback listener.
	if err := cfg.validateListenSecurity(); err != nil {
		logger.Error("refusing to start", "err", err.Error())
		os.Exit(1)
	}
	if cfg.TokenIsDefault {
		logger.Warn("no explicit credential configured: using the built-in default token; " +
			"only loopback listeners are allowed — set REMOTECTL_TOKEN / SHELL_API_TOKEN (or *_TOKEN_HASH) before exposing this server")
	}

	// Exactly one of TOKEN / TOKEN_HASH must be set; the former stores the
	// plaintext, the latter only a salted digest.
	auth, authMode, err := buildTokenAuth(cfg.Token, cfg.TokenHash)
	if err != nil {
		logger.Error("invalid auth configuration", "err", err.Error())
		os.Exit(1)
	}
	if (cfg.TLSCert == "") != (cfg.TLSKey == "") {
		logger.Error("invalid TLS configuration: TLS_CERT and TLS_KEY must be set together")
		os.Exit(1)
	}
	return auth, authMode
}

// serverApp bundles the local execution core and the HTTP lifecycle shared by
// standalone and master modes.
type serverApp struct {
	cfg      *Config
	logger   *slog.Logger
	authMode string

	executor *Executor
	sessions *SessionManager
	jobs     *JobManager
	waits    *WaitManager
	audit    *AuditLogger
	runtime  *localRuntime

	streamsCtx context.Context
	endStreams context.CancelFunc
	baseCtx    context.Context
	cancelBase context.CancelFunc
}

func newServerApp(cfg *Config, logger *slog.Logger, authMode string) *serverApp {
	executor := NewExecutor(cfg)
	sessions := NewSessionManager()
	jobs := NewJobManager(cfg)
	waits := NewWaitManager()
	audit := NewAuditLogger(logger)
	streamsCtx, endStreams := context.WithCancel(context.Background())
	baseCtx, cancelBase := context.WithCancel(context.Background())
	return &serverApp{
		cfg:        cfg,
		logger:     logger,
		authMode:   authMode,
		executor:   executor,
		sessions:   sessions,
		jobs:       jobs,
		waits:      waits,
		audit:      audit,
		runtime:    newLocalRuntime(executor, sessions, jobs, waits, audit, cfg),
		streamsCtx: streamsCtx,
		endStreams: endStreams,
		baseCtx:    baseCtx,
		cancelBase: cancelBase,
	}
}

// serve starts the public MCP server and blocks until a signal or a fatal
// serve error, then drains local work. beforeShutdown runs first (master stops
// its control channel there).
func (a *serverApp) serve(handler http.Handler, beforeShutdown func()) {
	srv := &http.Server{
		Addr:    a.cfg.Listen,
		Handler: handler,
		// Slowloris protection. No ReadTimeout / WriteTimeout on purpose:
		// remote_execute calls legitimately run up to MaxTimeout and the MCP
		// GET stream stays open indefinitely.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
		BaseContext:       func(net.Listener) context.Context { return a.baseCtx },
		ErrorLog:          slog.NewLogLogger(a.logger.Handler(), slog.LevelWarn),
	}

	logStartup(a.logger, a.cfg, a.authMode)

	serveErr := make(chan error, 1)
	go func() {
		if a.cfg.TLSCert != "" && a.cfg.TLSKey != "" {
			serveErr <- srv.ListenAndServeTLS(a.cfg.TLSCert, a.cfg.TLSKey)
		} else {
			a.logger.Warn("running without TLS; set REMOTECTL_TLS_CERT / SHELL_API_TLS_CERT and the key for production")
			serveErr <- srv.ListenAndServe()
		}
	}()

	sigCtx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stopSignals()

	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			a.logger.Error("server failed", "err", err.Error())
			os.Exit(1)
		}
		return
	case <-sigCtx.Done():
	}
	stopSignals() // a second signal now terminates immediately
	if beforeShutdown != nil {
		beforeShutdown()
	}

	gracefulShutdown(a.logger, srv, a.executor, a.jobs, a.waits, a.endStreams, a.cancelBase, a.cfg.ShutdownGrace)
}

// newHTTPHandler 只注册 MCP 入口，所有请求统一经过鉴权与访问日志。
func newHTTPHandler(mcpHandler http.Handler, auth *tokenAuth, cfg *Config, logger *slog.Logger, streamsCtx context.Context) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/mcp", mcpHandler)

	// 请求顺序：ClientIP -> Logging -> RateLimit -> Auth -> Drain -> mux。
	var handler http.Handler = mux
	handler = drainStreamsMiddleware(streamsCtx, handler)
	handler = AuthMiddleware(auth, handler)
	handler = RateLimitMiddleware(NewRateLimiter(cfg.RateLimit, cfg.RateBurst), handler)
	handler = LoggingMiddleware(logger, handler)
	return ClientIPMiddleware(cfg.TrustedProxies, handler)
}

// gracefulShutdown stops accepting connections, lets in-flight executions
// (including commands that outlived their MCP call) finish for up to grace,
// then kills what is left (their whole process groups) and closes the server.
// Background jobs from remote_spawn run in their own sessions and are
// deliberately left running.
func gracefulShutdown(logger *slog.Logger, srv *http.Server, executor *Executor, jobs *JobManager, waits *WaitManager,
	endStreams, cancelBase context.CancelFunc, grace time.Duration) {
	logger.Info("shutdown: signal received, draining",
		"running_executions", executor.Running(), "grace_sec", int(grace.Seconds()))
	endStreams()
	scanTasks := executor.scans.List()
	if n := executor.scans.CancelAll(); n > 0 {
		logger.Info("shutdown: cancelled active scans", "scans", n)
	}
	// Watchers only observe; stopping them first also ends pending
	// remote_wait_for calls (state=cancelled) instead of holding Shutdown.
	if n := waits.CancelAll(); n > 0 {
		logger.Info("shutdown: cancelled active waits", "waits", n)
	}

	ctx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	err := srv.Shutdown(ctx)
	// 后台读取不属于 HTTP 请求，也需在同一退出宽限内释放文件句柄。
	for _, scan := range scanTasks {
		scan.Await(ctx, grace)
	}
	// Commands that outlived their MCP call (executions.go) belong to no
	// request, so Shutdown does not wait for them: they get the rest of the
	// grace period as well.
	for err == nil && executor.Running() > 0 && ctx.Err() == nil {
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil || executor.Running() > 0 {
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

// slaverDrain is the HTTP-less shutdown path: stop new work, let in-flight
// executions finish for up to grace, leave detached jobs running.
func slaverDrain(logger *slog.Logger, executor *Executor, jobs *JobManager, waits *WaitManager, grace time.Duration) {
	logger.Info("shutdown: draining", "running_executions", executor.Running(), "grace_sec", int(grace.Seconds()))
	scanTasks := executor.scans.List()
	if n := executor.scans.CancelAll(); n > 0 {
		logger.Info("shutdown: cancelled active scans", "scans", n)
	}
	if n := waits.CancelAll(); n > 0 {
		logger.Info("shutdown: cancelled active waits", "waits", n)
	}
	ctx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	for _, scan := range scanTasks {
		scan.Await(ctx, grace)
	}
	for executor.Running() > 0 && ctx.Err() == nil {
		time.Sleep(50 * time.Millisecond)
	}
	if executor.Running() > 0 {
		n := executor.CancelAll()
		deadline := time.Now().Add(execWaitDelay + time.Second)
		for executor.Running() > 0 && time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
		}
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

func logStartup(logger *slog.Logger, cfg *Config, authMode string) {
	if len(cfg.LegacyEnvs) > 0 {
		logger.Warn("legacy SHELL_API_* environment variables are deprecated; REMOTECTL_* takes precedence",
			"legacy_envs", strings.Join(cfg.LegacyEnvs, ","))
	}
	if cfg.TokenIsDefault {
		logger.Warn("built-in default token is in use (loopback listener only)")
	}
	attrs := []any{
		"version", serverVersion,
		"commit", gitCommit,
		"build_time", buildTime,
		"mode", cfg.Mode,
		"addr", cfg.Listen,
		"tls", cfg.TLSCert != "" && cfg.TLSKey != "",
		"auth_mode", authMode,
		"max_timeout_sec", cfg.MaxTimeout,
		"max_output", cfg.MaxOutput,
		"rate_limit_per_min", cfg.RateLimit,
		"rate_burst", cfg.RateBurst,
		"trusted_proxies", trustedProxyStrings(cfg.TrustedProxies),
		"call_budget_sec", cfg.callBudgetSec(),
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
		logger.Info("READ-ONLY MODE: ENABLED (highest priority); mutating MCP tools are disabled",
			"enabled_tools", strings.Join(enabled, ","),
			"disabled_tools", strings.Join(readOnlyDisabledTools(), ","))
	}
}
