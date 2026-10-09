package main

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Structured logging
//
// Every line this server writes is one JSON object (log/slog). Two record
// types matter operationally and share the req_id key, so an access line can
// always be joined with the audit line of the same request:
//
//	{"type":"access","req_id":"r-1a2b3c4d","remote":"10.0.0.1","method":"POST","path":"/mcp","status":200,"dur_ms":28050,"mcp_method":"tools/call","tool":"remote_execute"}
//	{"type":"audit","req_id":"r-1a2b3c4d","exec_id":"...","tool":"remote_execute","command":"...","exit_code":0}
// ---------------------------------------------------------------------------

// newLogger builds the process logger. level is debug|info|warn|error.
func newLogger(w io.Writer, level string) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: parseLogLevel(level)}))
}

func parseLogLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// requestInfo is the per-request correlation record. The access-log middleware
// creates it and stores a pointer in the request context; the MCP context func
// and tool middleware enrich it (tool name, MCP session) so the access line
// written when the request completes carries them.
type requestInfo struct {
	ID     string
	Remote string // peer host, port stripped

	mu         sync.Mutex
	tool       string
	mcpSession string
	mcpMethod  string
}

func (ri *requestInfo) setMethod(m string) {
	ri.mu.Lock()
	if ri.mcpMethod == "" {
		ri.mcpMethod = m
	}
	ri.mu.Unlock()
}

func (ri *requestInfo) setTool(name string) {
	ri.mu.Lock()
	ri.tool = name
	ri.mu.Unlock()
}

func (ri *requestInfo) setMCPSession(id string) {
	ri.mu.Lock()
	ri.mcpSession = id
	ri.mu.Unlock()
}

func (ri *requestInfo) snapshot() (tool, mcpSession, mcpMethod string) {
	ri.mu.Lock()
	defer ri.mu.Unlock()
	return ri.tool, ri.mcpSession, ri.mcpMethod
}

type requestInfoKey struct{}

func withRequestInfo(ctx context.Context, ri *requestInfo) context.Context {
	return context.WithValue(ctx, requestInfoKey{}, ri)
}

// requestInfoFrom returns the request's correlation record, or nil outside an
// HTTP request (e.g. unit tests calling handlers directly).
func requestInfoFrom(ctx context.Context) *requestInfo {
	ri, _ := ctx.Value(requestInfoKey{}).(*requestInfo)
	return ri
}

// remoteHost strips the port from an http.Request.RemoteAddr. Rate limiting
// and audit both key on the host: the port changes with every TCP connection.
func remoteHost(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return host
}

func newRequestID() string {
	return "r-" + uuid.New().String()[:8]
}

// statusRecorder captures the response status and size for the access log. It
// must keep http.Flusher working: /mcp GET and /api/execute/stream are SSE.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(p)
	r.bytes += int64(n)
	return n, err
}

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := r.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, errors.New("hijack not supported")
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// isMCPStream reports whether r is the long-lived MCP server-push stream.
func isMCPStream(r *http.Request) bool {
	return r.Method == http.MethodGet && r.URL.Path == "/mcp"
}

// LoggingMiddleware assigns the request id, exposes it as X-Request-Id and
// writes one access line when the request completes.
//
// The MCP GET stream is logged at DEBUG: it is a keep-alive channel that
// clients reopen every few minutes and carried ~40% of all access lines in
// production while telling an operator nothing. Set SHELL_API_LOG_LEVEL=debug
// to see it.
func LoggingMiddleware(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ri := &requestInfo{ID: newRequestID(), Remote: remoteHost(r.RemoteAddr)}
		w.Header().Set("X-Request-Id", ri.ID)
		rec := &statusRecorder{ResponseWriter: w}

		next.ServeHTTP(rec, r.WithContext(withRequestInfo(r.Context(), ri)))

		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}
		level := slog.LevelInfo
		if isMCPStream(r) {
			level = slog.LevelDebug
		}
		attrs := []slog.Attr{
			slog.String("type", "access"),
			slog.String("req_id", ri.ID),
			slog.String("remote", ri.Remote),
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", status),
			slog.Int64("bytes", rec.bytes),
			slog.Int64("dur_ms", time.Since(start).Milliseconds()),
		}
		tool, sess, method := ri.snapshot()
		if method != "" {
			attrs = append(attrs, slog.String("mcp_method", method))
		}
		if tool != "" {
			attrs = append(attrs, slog.String("tool", tool))
		}
		if sess != "" {
			attrs = append(attrs, slog.String("mcp_session", sess))
		}
		logger.LogAttrs(context.Background(), level, "http", attrs...)
	})
}
