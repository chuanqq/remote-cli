package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

type scanTestClient struct {
	t    *testing.T
	h    http.Handler
	sid  string
	next int
}

func newScanTestClient(t *testing.T, root string, budget time.Duration, disabled map[string]bool) (*scanTestClient, *Executor) {
	t.Helper()
	cfg := &Config{MaxTimeout: 30, MaxOutput: 4096, DefaultShell: "bash", JobDir: t.TempDir(),
		DisabledTools: disabled, CallBudget: budget, RateLimit: 120, RateBurst: 60, FSRoots: []string{root}}
	cfg.applyReadOnly(true)
	t.Cleanup(func() { readOnlyEnabled.Store(false) })
	executor := NewExecutor(cfg)
	t.Cleanup(func() {
		list := executor.scans.List()
		executor.scans.CancelAll()
		for _, scan := range list {
			scan.Await(context.Background(), time.Second)
		}
	})
	c := &scanTestClient{t: t, h: NewMCPHandler(executor, &SessionManager{}, testJobManager(t), NewWaitManager(),
		NewAuditLogger(newLogger(io.Discard, "info")), cfg), next: 2}
	r := c.post(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"scan-test","version":"1"}}}`)
	c.sid = r.Header().Get(server.HeaderKeySessionID)
	if c.sid == "" {
		t.Fatalf("no MCP session: %s", r.Body.String())
	}
	c.post(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	return c, executor
}

func (c *scanTestClient) post(body string) *httptest.ResponseRecorder {
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

func (c *scanTestClient) call(tool string, args map[string]any) *mcp.CallToolResult {
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

func (c *scanTestClient) json(tool string, args map[string]any) map[string]any {
	c.t.Helper()
	r := c.call(tool, args)
	if r.IsError {
		c.t.Fatalf("%s: %s", tool, resultText(r))
	}
	out, ok := r.StructuredContent.(map[string]any)
	if !ok {
		c.t.Fatalf("%s missing structured content: %+v", tool, r)
	}
	return out
}

func TestMCPReadOnlyCountReturnsLineCounts(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"a.log": "hit hit\nhit\n", "b.log": "hit\n"})
	c, _ := newScanTestClient(t, root, time.Second, nil)
	r := c.json("remote_search_content", map[string]any{"path": root, "pattern": "hit", "output_mode": "count", "max_results": 1})
	files, _ := r["files"].([]any)
	if r["total_matches"] != float64(3) || r["files_matched"] != float64(2) || r["counts_complete"] != true || len(files) != 1 {
		t.Fatalf("count response: %v", r)
	}
	if files[0].(map[string]any)["count"] != float64(2) {
		t.Fatalf("per-file count missing: %v", files)
	}
	zero := c.json("remote_search_content", map[string]any{"path": root, "pattern": "absent", "output_mode": "count"})
	if zero["total_matches"] != float64(0) || zero["counts_complete"] != true {
		t.Fatalf("zero count omitted: %v", zero)
	}
}

func TestMCPReadOnlyScanSurvivesBudgetAndReusesResult(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "many.log")
	const lines = 400000
	if err := os.WriteFile(path, []byte(strings.Repeat("hit payload payload payload payload payload payload payload\n", lines)), 0600); err != nil {
		t.Fatal(err)
	}
	c, executor := newScanTestClient(t, root, 5*time.Millisecond, nil)
	pending := c.json("remote_search_content", map[string]any{"path": path, "pattern": "hit", "output_mode": "count", "timeout_ms": 10000})
	id, _ := pending["scan_id"].(string)
	if pending["state"] != ScanRunning || id == "" {
		t.Fatalf("expected a scan beyond the call budget: %v", pending)
	}
	scan := executor.scans.Lookup(id)
	if scan == nil {
		t.Fatal("running scan missing from manager")
	}
	scan.Await(context.Background(), 10*time.Second)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	// 删除源文件后续取仍成功，证明使用同一任务保留的结果，而没有重扫。
	for i := 0; i < 2; i++ {
		final := c.json("remote_scan_result", map[string]any{"scan_id": id, "wait_ms": 0})
		if final["state"] != ScanCompleted || final["total_matches"] != float64(lines) || final["counts_complete"] != true {
			t.Fatalf("final result: %v", final)
		}
	}
	list := c.json("remote_scan_result", map[string]any{})
	if list["count"] != float64(1) {
		t.Fatalf("lost-reply lookup list: %v", list)
	}
	listedText := c.call("remote_scan_result", map[string]any{"format": "text"})
	if listedText.StructuredContent != nil || !strings.Contains(resultText(listedText), id) {
		t.Fatalf("text scan list: %+v", listedText)
	}
	text := c.call("remote_scan_result", map[string]any{"scan_id": id, "format": "text", "wait_ms": 0})
	if text.IsError || text.StructuredContent != nil || !strings.Contains(resultText(text), "counts_complete=true") || !strings.Contains(resultText(text), "400000") {
		t.Fatalf("text continuation: %+v", text)
	}
}

func TestMCPReadOnlyCancelScanAndValidateTiming(t *testing.T) {
	root := t.TempDir()
	c, executor := newScanTestClient(t, root, time.Second, nil)
	scan, err := executor.scans.Start("remote_search_content", root, time.Minute, func(ctx context.Context) (any, error) {
		<-ctx.Done()
		complete := false
		return &SearchContentResult{Path: root, OutputMode: SearchCountMode, TotalMatches: 7, Truncated: true, CountsComplete: &complete}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	r := c.json("remote_scan_result", map[string]any{"scan_id": scan.ID, "cancel": true})
	if r["state"] != ScanCancelled || r["total_matches"] != float64(7) || r["counts_complete"] != false {
		t.Fatalf("cancelled partial result: %v", r)
	}
	for _, args := range []map[string]any{
		{"path": root, "pattern": "x", "timeout_ms": 0},
		{"path": root, "pattern": "x", "timeout_ms": 3600001},
		{"path": root, "pattern": "x", "wait_ms": -1},
	} {
		if r := c.call("remote_search_content", args); !r.IsError {
			t.Fatalf("invalid timing accepted: %v", args)
		}
	}
	if r := c.call("remote_scan_result", map[string]any{"cancel": true}); !r.IsError {
		t.Fatal("cancel without scan_id accepted")
	}
}

func TestMCPScanResultDisabledKeepsSynchronousReads(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"a.log": "hit\n"})
	c, executor := newScanTestClient(t, root, time.Second, map[string]bool{"remote_scan_result": true})
	r := c.json("remote_search_content", map[string]any{"path": root, "pattern": "hit", "output_mode": "count", "wait_ms": 0})
	if r["total_matches"] != float64(1) || r["scan_id"] != nil || len(executor.scans.List()) != 0 {
		t.Fatalf("disabled continuation left an inaccessible scan: %v", r)
	}
}
