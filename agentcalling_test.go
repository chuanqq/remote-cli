package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// ---- detached executions (executions.go) -------------------------------------

func waitUntil(t *testing.T, d time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
	return true
}

func TestLaunchOutlivesCallBudget(t *testing.T) {
	skipOnWindows(t)
	e := testExecutor()
	var late atomic.Int32
	ex := e.Launch(ExecuteRequest{Command: "sleep 0.5; echo done"}, func(*ExecResult) { late.Add(1) })
	if r, _ := ex.Await(context.Background(), 50*time.Millisecond); r != nil || ex.claim() != nil {
		t.Fatal("finished too early")
	}
	if st := ex.info().State; st != ExecRunning {
		t.Fatalf("state = %s, want running", st)
	}
	// A caller that gives up (cancelled context) does not stop the command.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if r, _ := ex.Await(ctx, time.Second); r != nil {
		t.Fatal("Await ignored the cancelled context")
	}
	r, dropped := e.Lookup(ex.ID).Await(context.Background(), 5*time.Second)
	if r == nil || dropped || execState(r) != ExecCompleted || strings.TrimSpace(r.Stdout) != "done" {
		t.Fatalf("result = %+v dropped=%v", r, dropped)
	}
	if !waitUntil(t, 2*time.Second, func() bool { return e.Running() == 0 }) || late.Load() != 1 {
		t.Errorf("late reports = %d, running = %d", late.Load(), e.Running())
	}
}

func TestLaunchInlineResultIsNotReportedLate(t *testing.T) {
	skipOnWindows(t)
	e := testExecutor()
	var late atomic.Int32
	ex := e.Launch(ExecuteRequest{Command: "echo hi"}, func(*ExecResult) { late.Add(1) })
	ex.Await(context.Background(), 5*time.Second)
	if r := ex.claim(); r == nil || strings.TrimSpace(r.Stdout) != "hi" {
		t.Fatalf("claim = %+v", r)
	}
	waitUntil(t, time.Second, func() bool { return e.Running() == 0 })
	if late.Load() != 0 {
		t.Error("an inline result was reported again as late")
	}
}

func TestLaunchStates(t *testing.T) {
	skipOnWindows(t)
	e := testExecutor()
	ex := e.Launch(ExecuteRequest{Command: "sleep 30"}, nil)
	time.Sleep(200 * time.Millisecond) // let it start
	if !e.Cancel(ex.ID) {
		t.Fatal("cancel: execution not found")
	}
	r, _ := ex.Await(context.Background(), 5*time.Second)
	if r == nil || execState(r) != ExecCancelled || !r.Cancelled {
		t.Fatalf("cancel: %+v", r)
	}

	ex = e.Launch(ExecuteRequest{Command: "sleep 30", TimeoutMs: 200}, nil)
	r, _ = ex.Await(context.Background(), 5*time.Second)
	if r == nil || execState(r) != ExecTimedOut || !strings.Contains(r.Error, "timeout_ms=200") {
		t.Fatalf("timeout must name the limit: %+v", r)
	}

	ex = e.Launch(ExecuteRequest{Command: "true", WorkingDirectory: "/no/such/dir"}, nil)
	r, _ = ex.Await(context.Background(), 5*time.Second)
	if r == nil || execState(r) != ExecNotStarted {
		t.Fatalf("not started: %+v", r)
	}
}

func TestExecutionRetention(t *testing.T) {
	skipOnWindows(t)
	e := testExecutor()
	var ids []string
	for i := 0; i < maxRetainedOutputs+3; i++ {
		ex := e.Launch(ExecuteRequest{Command: "echo out"}, nil)
		ex.Await(context.Background(), 5*time.Second)
		ids = append(ids, ex.ID)
		time.Sleep(2 * time.Millisecond) // distinct start times
	}
	if n := len(e.Executions()); n != len(ids) {
		t.Fatalf("listed %d executions, want %d", n, len(ids))
	}
	if _, dropped := e.Lookup(ids[0]).Result(); !dropped {
		t.Error("output kept beyond maxRetainedOutputs")
	}
	if r, dropped := e.Lookup(ids[len(ids)-1]).Result(); dropped || strings.TrimSpace(r.Stdout) != "out" {
		t.Error("newest output dropped")
	}
	e.mu.Lock()
	e.pruneLocked(time.Now().Add(execRetention + time.Minute))
	n := len(e.execs)
	e.mu.Unlock()
	if n != 0 {
		t.Errorf("%d executions survived the retention period", n)
	}
}

// ---- watchers (waits.go) and incremental scanning ------------------------------

func TestWaitResumeKeepsOffset(t *testing.T) {
	skipOnWindows(t)
	dir := t.TempDir()
	logf := filepath.Join(dir, "app.log")
	os.WriteFile(logf, []byte("READY (old)\n"), 0o644)
	wm := NewWaitManager()
	plan, err := planWait(WaitForRequest{Condition: WaitLogRegex, Target: logf, Pattern: "READY",
		TimeoutSec: 30, IntervalMs: 200}, waitDeps{})
	if err != nil {
		t.Fatal(err)
	}
	w, err := wm.Start(plan)
	if err != nil {
		t.Fatal(err)
	}
	res, err := w.Await(context.Background(), 300*time.Millisecond)
	if err != nil || res.State != WaitPending || res.WaitID != w.ID || res.Hint == "" {
		t.Fatalf("first call: %+v %v", res, err)
	}

	// The line lands between two calls: the resumed watcher still sees it.
	f, _ := os.OpenFile(logf, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("boot\nservice READY on 8080\n")
	f.Close()
	time.Sleep(500 * time.Millisecond)
	res, err = wm.Get(w.ID).Await(context.Background(), 5*time.Second)
	if err != nil || res.State != WaitSatisfied || !strings.Contains(res.Detail, "8080") {
		t.Fatalf("resumed: %+v %v", res, err)
	}

	plan, _ = planWait(WaitForRequest{Condition: WaitFileExists, Target: filepath.Join(dir, "never"),
		TimeoutSec: 60, IntervalMs: 200}, waitDeps{})
	w, _ = wm.Start(plan)
	if n := wm.CancelAll(); n != 1 {
		t.Errorf("CancelAll = %d, want 1 active wait", n)
	}
	res, _ = w.Await(context.Background(), 5*time.Second)
	if res.State != WaitCancelled {
		t.Errorf("cancelled wait: %+v", res)
	}
}

func TestScanFileForMatchCursor(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f.log")
	re := regexp.MustCompile("zzz")
	os.WriteFile(p, []byte("aaa\nbbb\npartial"), 0o644)
	_, _, found, consumed, err := scanFileForMatch(p, 0, re, "")
	if err != nil || found || consumed != 8 {
		t.Fatalf("consumed = %d, want 8 (a half-written last line is read again)", consumed)
	}
	os.WriteFile(p, []byte("aaa\nbbb\npartial zzz\n"), 0o644)
	line, n, found, _, _ := scanFileForMatch(p, consumed, re, "")
	if !found || n != 1 || line != "partial zzz" {
		t.Fatalf("resume from cursor: %q line %d found=%v", line, n, found)
	}
	// An over-long line is skipped over without hiding what follows it.
	os.WriteFile(p, []byte(strings.Repeat("x", maxWaitLineBytes+100)+"\nneedle zzz\n"), 0o644)
	line, n, found, _, err = scanFileForMatch(p, 0, re, "")
	if err != nil || !found || n != 2 || line != "needle zzz" {
		t.Fatalf("after long line: %q line %d found=%v err=%v", line, n, found, err)
	}
}

// ---- sessions, rate limit, write dry_run ---------------------------------------

func TestSessionTTLIsIdleTimeout(t *testing.T) {
	sm := &SessionManager{}
	sess := sm.Create(SessionCreateRequest{WorkingDirectory: "/tmp", TTLSeconds: 1})
	first := sess.Expiry()
	time.Sleep(600 * time.Millisecond)
	if sm.Touch(sess.ID) == nil {
		t.Fatal("session expired early")
	}
	if !sess.Expiry().After(first) {
		t.Error("execute did not renew the expiry")
	}
	time.Sleep(600 * time.Millisecond) // 1.2s after create, 0.6s after use
	if sm.Get(sess.ID) == nil {
		t.Error("a session in use expired at the fixed creation-time deadline")
	}
	if resp := sessionResponse(sess); resp.TTLSeconds != 1 || resp.ExpiresAt == "" {
		t.Errorf("response = %+v", resp)
	}
}

func TestRateLimitRetryAfterMs(t *testing.T) {
	rl := NewRateLimiter(60, 1) // 1 token/s
	h := RateLimitMiddleware(rl, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	send := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		req.RemoteAddr = "10.0.0.9:1234"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	send()
	rec := send()
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d", rec.Code)
	}
	var body ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.RetryAfterMs <= 0 || body.RetryAfterMs > 1001 || rec.Header().Get("Retry-After") != "1" {
		t.Errorf("retry_after_ms = %d, Retry-After = %q", body.RetryAfterMs, rec.Header().Get("Retry-After"))
	}
}

func TestWriteFileDryRun(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "app.conf")
	os.WriteFile(p, []byte("a=1\nb=2\nc=3\n"), 0o640)
	r, err := WriteFileContent(WriteFileRequest{Path: p, Content: "a=1\nb=20\nc=3\n", DryRun: true}, []string{dir})
	if err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(p); string(data) != "a=1\nb=2\nc=3\n" {
		t.Fatal("dry_run wrote the file")
	}
	want := "@@ -1,3 +1,3 @@\n a=1\n-b=2\n+b=20\n c=3\n"
	if !r.DryRun || r.Created || r.Mode != "-rw-r-----" || r.Preview.Diff != want ||
		r.Preview.LinesAdded != 1 || r.Preview.LinesRemoved != 1 || r.Preview.Unchanged {
		t.Errorf("result = %+v preview = %+v", r, r.Preview)
	}

	r, _ = WriteFileContent(WriteFileRequest{Path: p, Content: "a=1\nb=2\nc=3\n", DryRun: true}, []string{dir})
	if !r.Preview.Unchanged || r.Preview.Diff != "" {
		t.Errorf("identical content: %+v", r.Preview)
	}
	r, err = WriteFileContent(WriteFileRequest{Path: filepath.Join(dir, "new.conf"), Content: "x\n", DryRun: true}, []string{dir})
	if err != nil || !r.Created || r.Preview.Diff != "@@ -0,0 +1,1 @@\n+x\n" {
		t.Errorf("new file: %+v %v", r, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "new.conf")); !os.IsNotExist(err) {
		t.Error("dry_run created the file")
	}
	if _, err := WriteFileContent(WriteFileRequest{Path: filepath.Join(dir, "sub", "x"), Content: "x", DryRun: true},
		[]string{dir}); err == nil {
		t.Error("missing parent dir not reported (the real write would fail)")
	}
}

func TestUnifiedDiffHunks(t *testing.T) {
	var a, b []string
	for i := 0; i < 20; i++ {
		a = append(a, "l"+strings.Repeat("x", i))
	}
	b = append(b, a...)
	b[2], b[15] = "changed-2", "changed-15"
	diff, added, removed, _ := unifiedDiff(strings.Join(a, "\n")+"\n", strings.Join(b, "\n")+"\n", 1<<20)
	if added != 2 || removed != 2 || strings.Count(diff, "@@ -") != 2 {
		t.Errorf("want two separate hunks:\n%s", diff)
	}
	if d, _, _, trunc := unifiedDiff("a\n", strings.Repeat("b\n", 1000), 64); !trunc || len(d) > 64 {
		t.Errorf("diff not capped: len=%d truncated=%v", len(d), trunc)
	}
}

// ---- end to end over MCP --------------------------------------------------------

// TestMCPExecuteBeyondCallBudget drives remote_execute through a real MCP
// session with a 1s call budget: the call answers state=running without
// killing the command, a cut-off client still finds it, the result is fetched
// by execution_id, and both audit lines (call + late outcome) carry the
// client label from the handshake.
func TestMCPExecuteBeyondCallBudget(t *testing.T) {
	skipOnWindows(t)
	var buf bytes.Buffer
	var mu sync.Mutex
	logger := newLogger(&lockedWriter{w: &buf, mu: &mu}, "info")
	cfg := &Config{MaxTimeout: 30, MaxOutput: 4096, DefaultShell: "bash", JobDir: t.TempDir(),
		DisabledTools: map[string]bool{}, CallBudget: time.Second, RateLimit: 120, RateBurst: 60}
	h := NewMCPHandler(NewExecutor(cfg), &SessionManager{}, testJobManager(t), NewWaitManager(), NewAuditLogger(logger), cfg)

	post := func(sid, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
		req.RemoteAddr = "172.26.64.64:5000"
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if sid != "" {
			req.Header.Set(server.HeaderKeySessionID, sid)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	init := post("", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"claude-code","version":"2.1"}}}`)
	sid := init.Header().Get(server.HeaderKeySessionID)
	if sid == "" {
		t.Fatalf("no session id: %s", init.Body.String())
	}
	if !strings.Contains(init.Body.String(), "call budget") || !strings.Contains(init.Body.String(), "4096 bytes") {
		t.Errorf("initialize instructions lack the instance limits: %s", init.Body.String())
	}
	post(sid, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)

	call := func(id int, name string, args map[string]any) map[string]any {
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call",
			"params": mcp.CallToolParams{Name: name, Arguments: args}})
		rec := post(sid, string(body))
		var resp struct {
			Result mcp.CallToolResult `json:"result"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("%s: %v: %s", name, err, rec.Body.String())
		}
		text := resultText(&resp.Result)
		if resp.Result.IsError {
			t.Fatalf("%s failed: %s", name, text)
		}
		out := map[string]any{}
		json.Unmarshal([]byte(text), &out)
		return out
	}

	start := time.Now()
	r := call(2, "remote_execute", map[string]any{"command": "sleep 2; echo finished"})
	if el := time.Since(start); el > 1800*time.Millisecond {
		t.Errorf("call blocked %v, beyond the 1s budget", el)
	}
	id, _ := r["execution_id"].(string)
	if r["state"] != ExecRunning || id == "" {
		t.Fatalf("want state=running + execution_id, got %v", r)
	}

	list := call(3, "remote_execution_result", map[string]any{})
	if n, _ := list["count"].(float64); n != 1 {
		t.Errorf("execution list = %v", list)
	}

	var final map[string]any
	for i := 0; i < 5; i++ {
		final = call(4+i, "remote_execution_result", map[string]any{"execution_id": id})
		if final["state"] != ExecRunning {
			break
		}
	}
	if final["state"] != ExecCompleted || final["exit_code"] != float64(0) ||
		strings.TrimSpace(final["stdout"].(string)) != "finished" {
		t.Fatalf("final result = %v", final)
	}

	var audits logLines
	waitUntil(t, 2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		audits = nil
		for _, a := range parseLog(t, &buf).ofType("audit") {
			if a["exec_id"] == id && a["tool"] == "remote_execute" {
				audits = append(audits, a)
			}
		}
		return len(audits) == 2
	})
	if len(audits) != 2 || audits[0]["state"] != ExecRunning || audits[1]["state"] != ExecCompleted {
		t.Fatalf("want a running line then the late completed line, got %v", audits)
	}
	for _, a := range audits {
		if a["client"] != "claude-code/2.1" || a["remote"] != "172.26.64.64" {
			t.Errorf("audit line lacks client/peer: %v", a)
		}
	}
}
