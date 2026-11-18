package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// toolsList performs the MCP handshake and returns initialize.serverInfo plus
// the tools/list payload.
func toolsList(t *testing.T, cfg *Config) (name, version string, tools []mcp.Tool) {
	t.Helper()
	h := NewMCPHandler(NewExecutor(cfg), NewSessionManager(), testJobManager(t), NewWaitManager(),
		NewAuditLogger(newLogger(io.Discard, "info")), cfg)

	post := func(sid, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if sid != "" {
			req.Header.Set(server.HeaderKeySessionID, sid)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	initResp := post("", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"compat","version":"1"}}}`)
	sid := initResp.Header().Get(server.HeaderKeySessionID)
	if sid == "" {
		t.Fatalf("initialize failed: %s", initResp.Body.String())
	}
	var init struct {
		Result struct {
			ServerInfo struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"serverInfo"`
		} `json:"result"`
	}
	if err := json.Unmarshal(initResp.Body.Bytes(), &init); err != nil {
		t.Fatalf("decode initialize: %v", err)
	}
	post(sid, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)

	var list struct {
		Result struct {
			Tools []mcp.Tool `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(post(sid, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`).Body.Bytes(), &list); err != nil {
		t.Fatalf("decode tools/list: %v", err)
	}
	return init.Result.ServerInfo.Name, init.Result.ServerInfo.Version, list.Result.Tools
}

func compatConfig(t *testing.T) *Config {
	t.Helper()
	return &Config{
		Port:          "0",
		Listen:        "127.0.0.1:0",
		MaxTimeout:    30,
		MaxOutput:     4096,
		RateLimit:     120,
		RateBurst:     60,
		DefaultShell:  "bash",
		JobDir:        t.TempDir(),
		DisabledTools: map[string]bool{},
	}
}

// 1.8.0 contract: the standalone tool list is the full inventory except
// remote_delete_file, which requires an FSRoot. No tool advertises node_id in
// standalone mode (routing is a master-only addition).
func TestStandaloneToolListCompat(t *testing.T) {
	name, version, tools := toolsList(t, compatConfig(t))
	if name != "agentic-remotectl" {
		t.Errorf("serverInfo.name = %q, want agentic-remotectl (2.0 default)", name)
	}
	if !strings.HasPrefix(version, serverVersion+"+") && version != serverVersion {
		t.Errorf("serverInfo.version = %q, want %s[+commit]", version, serverVersion)
	}

	// remote_delete_file needs an FSRoot; node tools need a control plane.
	want := len(allToolNames) - 1 - len(nodeToolNames)
	if len(tools) != want {
		t.Fatalf("tools/list returned %d tools, want %d", len(tools), want)
	}
	seen := make(map[string]bool, len(tools))
	for _, tool := range tools {
		seen[tool.Name] = true
		if _, ok := tool.InputSchema.Properties["node_id"]; ok {
			t.Errorf("standalone tool %s must not advertise node_id yet", tool.Name)
		}
	}
	if seen["remote_delete_file"] {
		t.Error("remote_delete_file must not be registered without an FSRoot")
	}
	for _, nodeTool := range nodeToolNames {
		if seen[nodeTool] {
			t.Errorf("standalone mode must not expose node tool %s", nodeTool)
		}
	}
	for _, known := range allToolNames {
		if known == "remote_delete_file" || slices.Contains(nodeToolNames, known) {
			continue
		}
		if !seen[known] {
			t.Errorf("tool %s missing from tools/list", known)
		}
	}
}

// With an FSRoot the full 35-tool inventory is exposed, matching 1.8.0.
func TestStandaloneToolListWithFSRoot(t *testing.T) {
	cfg := compatConfig(t)
	cfg.FSRoots = []string{t.TempDir()}
	_, _, tools := toolsList(t, cfg)
	if want := len(allToolNames) - len(nodeToolNames); len(tools) != want {
		t.Fatalf("tools/list returned %d tools, want %d", len(tools), want)
	}
	found := false
	for _, tool := range tools {
		if tool.Name == "remote_delete_file" {
			found = true
		}
	}
	if !found {
		t.Fatal("remote_delete_file should be registered when FSRoot is configured")
	}
}
