package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// 使用生产路由及中间件，验证旧接口在读写和只读模式下都已移除。
func TestHTTPOnlyMCP(t *testing.T) {
	for _, readOnly := range []bool{false, true} {
		name := "read-write"
		if readOnly {
			name = "read-only"
		}
		t.Run(name, func(t *testing.T) {
			for _, hashed := range []bool{false, true} {
				mode := "token"
				stored := ""
				token := "http-regression-token"
				plain := token
				if hashed {
					mode = "token-hash"
					stored = generateTokenHash(token)
					plain = ""
				}
				t.Run(mode, func(t *testing.T) {
					cfg := &Config{MaxTimeout: 10, MaxOutput: 4096, DefaultShell: "bash",
						RateLimit: 10000, RateBurst: 10000, DisabledTools: map[string]bool{}}
					if readOnly {
						cfg.applyReadOnly(true)
						t.Cleanup(func() { readOnlyEnabled.Store(false) })
					}
					auth, _, err := buildTokenAuth(plain, stored)
					if err != nil {
						t.Fatal(err)
					}
					logger := newLogger(io.Discard, "info")
					mcpHandler := NewMCPHandler(NewExecutor(cfg), &SessionManager{}, testJobManager(t),
						NewWaitManager(), NewAuditLogger(logger), cfg)
					h := newHTTPHandler(mcpHandler, auth, cfg, logger, context.Background())

					for _, path := range []string{
						"/api/status", "/api/execute", "/api/execute/stream", "/api/sessions",
						"/api/sessions/test/execute", "/api/sessions/test", "/api/executions/test",
						"/", "/health", "/metrics", "/mcp/",
					} {
						for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodDelete, http.MethodOptions} {
							for _, credential := range []string{"", "wrong-token", token} {
								req := httptest.NewRequest(method, path, strings.NewReader(`{"command":"echo unexpected"}`))
								if credential != "" {
									req.Header.Set("Authorization", "Bearer "+credential)
								}
								rec := httptest.NewRecorder()
								h.ServeHTTP(rec, req)
								want := http.StatusUnauthorized
								if credential == token {
									want = http.StatusNotFound
								}
								if rec.Code != want {
									t.Fatalf("%s %s: status=%d, want=%d", method, path, rec.Code, want)
								}
							}
						}
					}

					post := func(sid, credential, body string) *httptest.ResponseRecorder {
						req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
						req.Header.Set("Content-Type", "application/json")
						req.Header.Set("Accept", "application/json, text/event-stream")
						if credential != "" {
							req.Header.Set("Authorization", "Bearer "+credential)
						}
						if sid != "" {
							req.Header.Set(server.HeaderKeySessionID, sid)
						}
						rec := httptest.NewRecorder()
						h.ServeHTTP(rec, req)
						return rec
					}
					initBody := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"http-test","version":"1"}}}`
					for _, credential := range []string{"", "wrong-token"} {
						if rec := post("", credential, initBody); rec.Code != http.StatusUnauthorized {
							t.Fatalf("MCP 未拒绝无效凭据: %d", rec.Code)
						}
					}
					init := post("", token, initBody)
					sid := init.Header().Get(server.HeaderKeySessionID)
					if init.Code != http.StatusOK || sid == "" || !strings.Contains(init.Body.String(), fullVersion()) {
						t.Fatalf("MCP 握手失败: status=%d body=%s", init.Code, init.Body.String())
					}
					post(sid, token, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
					status := post(sid, token, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"remote_status","arguments":{}}}`)
					var response struct {
						Result mcp.CallToolResult `json:"result"`
					}
					if err := json.Unmarshal(status.Body.Bytes(), &response); err != nil {
						t.Fatal(err)
					}
					var health StatusResponse
					if err := json.Unmarshal([]byte(resultText(&response.Result)), &health); err != nil {
						t.Fatal(err)
					}
					if status.Code != http.StatusOK || response.Result.IsError || health.Status != "healthy" ||
						health.Version != serverVersion || health.ReadOnly != readOnly {
						t.Fatalf("MCP remote_status 返回异常: %s", status.Body.String())
					}
				})
			}
		})
	}
}
