package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

type sessionTestClient struct {
	t    *testing.T
	h    http.Handler
	sid  string
	next int
}

func (c *sessionTestClient) post(body string) *httptest.ResponseRecorder {
	c.t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	if c.sid != "" {
		r.Header.Set(server.HeaderKeySessionID, c.sid)
	}
	w := httptest.NewRecorder()
	c.h.ServeHTTP(w, r)
	return w
}

func (c *sessionTestClient) call(tool string, args map[string]any) *mcp.CallToolResult {
	c.t.Helper()
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": c.next, "method": "tools/call",
		"params": mcp.CallToolParams{Name: tool, Arguments: args}})
	if err != nil {
		c.t.Fatal(err)
	}
	c.next++
	r := c.post(string(body))
	var response struct {
		Result *mcp.CallToolResult `json:"result"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &response); err != nil || response.Result == nil {
		c.t.Fatalf("%s: %v: %s", tool, err, r.Body.String())
	}
	return response.Result
}

// D18: concurrent remote_session_execute calls on one session are rejected,
// and a command that outlives the call keeps the session busy until it ends.
func TestSessionExecuteConcurrencyRejected(t *testing.T) {
	cfg := &Config{
		MaxTimeout: 10, MaxOutput: 4096, DefaultShell: "bash", JobDir: t.TempDir(),
		DisabledTools: map[string]bool{}, RateLimit: 120, RateBurst: 60,
		CallBudget: 150 * time.Millisecond,
	}
	executor := NewExecutor(cfg)
	c := &sessionTestClient{
		t: t,
		h: NewMCPHandler(executor, NewSessionManager(), testJobManager(t), NewWaitManager(),
			NewAuditLogger(newLogger(io.Discard, "info")), cfg),
		next: 2,
	}
	t.Cleanup(func() {
		executor.CancelAll()
		executor.scans.CancelAll()
	})
	init := c.post(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"session-test","version":"1"}}}`)
	c.sid = init.Header().Get(server.HeaderKeySessionID)
	if c.sid == "" {
		t.Fatalf("no MCP session: %s", init.Body.String())
	}
	c.post(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)

	created := c.call("remote_session_create", map[string]any{"working_directory": t.TempDir()})
	sid, _ := created.StructuredContent.(map[string]any)["session_id"].(string)
	if sid == "" {
		t.Fatalf("session create failed: %s", resultText(created))
	}

	first := c.call("remote_session_execute", map[string]any{"session_id": sid, "command": "sleep 5"})
	if first.IsError {
		t.Fatalf("first execute failed: %s", resultText(first))
	}
	if state, _ := first.StructuredContent.(map[string]any)["state"].(string); state != "running" {
		t.Fatalf("first execute state = %q, want running (call budget 150ms)", state)
	}

	second := c.call("remote_session_execute", map[string]any{"session_id": sid, "command": "echo second"})
	if !second.IsError || !strings.Contains(resultText(second), "session_busy") {
		t.Fatalf("second execute should be rejected with session_busy, got: %s", resultText(second))
	}
}
