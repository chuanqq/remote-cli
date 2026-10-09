package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func testExecutor() *Executor {
	return NewExecutor(&Config{MaxTimeout: 30, MaxOutput: 65536, DefaultShell: "bash"})
}

func skipOnWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell")
	}
}

// ---- 3.1 rate limiter -------------------------------------------------------

func TestRateLimiterKeysOnHostNotPort(t *testing.T) {
	rl := NewRateLimiter(60, 3)
	h := RateLimitMiddleware(rl, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	codes := []int{}
	for port := 40000; port < 40005; port++ {
		req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		req.RemoteAddr = "10.0.0.1:" + strconv.Itoa(port) // new TCP connection each time
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		codes = append(codes, rec.Code)
	}
	want := []int{200, 200, 200, 429, 429}
	for i := range want {
		if codes[i] != want[i] {
			t.Fatalf("codes = %v, want %v (bucket must be shared across source ports)", codes, want)
		}
	}
	if rl.Len() != 1 {
		t.Errorf("buckets = %d, want 1 per host", rl.Len())
	}
}

func TestRateLimiterRefillAndSweep(t *testing.T) {
	now := time.Unix(1000, 0)
	rl := NewRateLimiter(60, 2) // 1 token/s
	rl.now = func() time.Time { return now }
	for i := 0; i < 2; i++ {
		if ok, _ := rl.Allow("h"); !ok {
			t.Fatal("burst denied")
		}
	}
	if ok, _ := rl.Allow("h"); ok {
		t.Fatal("over burst allowed")
	}
	now = now.Add(1100 * time.Millisecond)
	if ok, _ := rl.Allow("h"); !ok {
		t.Fatal("refill did not grant a token after 1s")
	}
	rl.Allow("other")
	now = now.Add(bucketIdleTTL + time.Second)
	rl.sweep()
	if rl.Len() != 0 {
		t.Errorf("idle buckets not swept: %d left", rl.Len())
	}
}

func TestRateLimitExemptsMCPStream(t *testing.T) {
	rl := NewRateLimiter(60, 1)
	h := RateLimitMiddleware(rl, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
		req.RemoteAddr = "10.0.0.2:1"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("GET /mcp #%d limited: %d", i, rec.Code)
		}
	}
}

// ---- 3.13 auth ---------------------------------------------------------------

func TestAuthConstantTime(t *testing.T) {
	h := AuthMiddleware("secret-token", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	for _, c := range []struct {
		hdr  string
		want int
	}{
		{"Bearer secret-token", 200},
		{"Bearer secret-toke", 401},
		{"Bearer secret-tokenX", 401},
		{"", 401},
	} {
		req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		if c.hdr != "" {
			req.Header.Set("Authorization", c.hdr)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%q: %d, want %d", c.hdr, rec.Code, c.want)
		}
	}
}

// ---- 3.4b truncateToLimit ----------------------------------------------------

func TestTruncateToLimitGBK(t *testing.T) {
	// Leading invalid byte: the old implementation returned "".
	s := "\xb2\xe2" + strings.Repeat("a", 2<<20)
	start := time.Now()
	out := truncateToLimit(s, 1<<20, false)
	if len(out) != 1<<20 {
		t.Errorf("head len = %d, want %d", len(out), 1<<20)
	}
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Errorf("head took %v (must be O(1) at the cut)", d)
	}
	mid := strings.Repeat("a", 1000) + "\xb2\xe2" + strings.Repeat("a", 4000)
	if got := truncateToLimit(mid, 2000, true); len(got) != 2000 {
		t.Errorf("tail len = %d, want 2000", len(got))
	}
	zh := strings.Repeat("中文", 10)
	for m := 1; m <= len(zh); m++ {
		h, tl := truncateToLimit(zh, m, false), truncateToLimit(zh, m, true)
		if !utf8.ValidString(h) || !utf8.ValidString(tl) || len(h) > m || len(tl) > m {
			t.Fatalf("max=%d: head %q tail %q", m, h, tl)
		}
	}
}

// ---- 3.4a/c bounded capture & output_encoding --------------------------------

func TestCappedBuffer(t *testing.T) {
	head := newCappedBuffer(5, false)
	head.Write([]byte("abc"))
	head.Write([]byte("defgh"))
	if string(head.Bytes()) != "abcde" || head.total != 8 || !head.truncated() {
		t.Errorf("head: %q total=%d", head.Bytes(), head.total)
	}
	tail := newCappedBuffer(5, true)
	for _, p := range []string{"ab", "cd", "efg", "h", "ijklmnop", "q"} {
		tail.Write([]byte(p))
	}
	if got := string(tail.Bytes()); got != "mnopq" {
		t.Errorf("tail = %q, want mnopq", got)
	}
	if tail.total != 17 {
		t.Errorf("tail total = %d", tail.total)
	}
}

func TestExecuteBoundedOutput(t *testing.T) {
	skipOnWindows(t)
	r := testExecutor().Execute(context.Background(), ExecuteRequest{
		Command:        "head -c 300000 /dev/zero | tr '\\0' a",
		MaxOutputBytes: 1000,
		TruncateMode:   "tail",
	})
	if len(r.Stdout) != 1000 || !r.StdoutTruncated || r.StdoutTotalBytes != 300000 {
		t.Errorf("len=%d truncated=%v total=%d", len(r.Stdout), r.StdoutTruncated, r.StdoutTotalBytes)
	}
}

func TestExecuteOutputEncoding(t *testing.T) {
	skipOnWindows(t)
	// "中文" in GBK.
	for _, enc := range []string{"gbk", "auto"} {
		r := testExecutor().Execute(context.Background(), ExecuteRequest{
			Command:        `printf '\326\320\316\304'`,
			OutputEncoding: enc,
		})
		if r.Stdout != "中文" || r.OutputEncoding != "gbk" {
			t.Errorf("%s: stdout=%q enc=%q", enc, r.Stdout, r.OutputEncoding)
		}
	}
	r := testExecutor().Execute(context.Background(), ExecuteRequest{Command: "printf 中文", OutputEncoding: "auto"})
	if r.Stdout != "中文" || r.OutputEncoding != "utf-8" {
		t.Errorf("auto utf-8: %q %q", r.Stdout, r.OutputEncoding)
	}
	r = testExecutor().Execute(context.Background(), ExecuteRequest{Command: "true", OutputEncoding: "latin9"})
	if !r.NotStarted || !strings.Contains(r.Error, "output_encoding") {
		t.Errorf("bad encoding not rejected: %+v", r)
	}
}

// ---- 3.5 WaitDelay / 3.6 start errors / 3.8 ctx ------------------------------

func TestExecuteBackgroundDoesNotHang(t *testing.T) {
	skipOnWindows(t)
	start := time.Now()
	// The background sleep inherits stdout: before WaitDelay this blocked
	// until the timeout and returned timed_out=true.
	r := testExecutor().Execute(context.Background(), ExecuteRequest{
		Command:   "sleep 20 & echo started",
		TimeoutMs: 15000,
	})
	if d := time.Since(start); d > 8*time.Second {
		t.Fatalf("Execute took %v", d)
	}
	if r.TimedOut || r.ExitCode != 0 || !strings.Contains(r.Stdout, "started") {
		t.Errorf("result: exit=%d timed_out=%v stdout=%q err=%q", r.ExitCode, r.TimedOut, r.Stdout, r.Error)
	}
}

func TestExecuteReportsStartErrors(t *testing.T) {
	r := testExecutor().Execute(context.Background(), ExecuteRequest{
		Command:          "pwd",
		WorkingDirectory: "/Users/nobody/definitely/not/here",
	})
	if r.ExitCode != -1 || !r.NotStarted || !strings.Contains(r.Error, "does not exist on remote host") {
		t.Errorf("bad dir: %+v", r)
	}
	r = NewExecutor(&Config{MaxTimeout: 5, MaxOutput: 100, DefaultShell: "/no/such/shell"}).
		Execute(context.Background(), ExecuteRequest{Command: "true"})
	if !strings.Contains(r.Error, "failed to start command") || r.Stderr == "" {
		t.Errorf("bad shell: %+v", r)
	}
}

func TestExecuteInheritsContext(t *testing.T) {
	skipOnWindows(t)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(300*time.Millisecond, cancel)
	start := time.Now()
	r := testExecutor().Execute(ctx, ExecuteRequest{Command: "sleep 10", TimeoutMs: 20000})
	if time.Since(start) > 5*time.Second {
		t.Fatal("cancelled request context did not stop the command")
	}
	if r.TimedOut || !strings.Contains(r.Error, "cancelled") {
		t.Errorf("result: %+v", r)
	}
}

// ---- 3.16 deny pattern & hints -----------------------------------------------

func TestJumpHostDeny(t *testing.T) {
	cfg := &Config{DenyCommand: buildDenyPattern("", true)}
	denied := []string{
		"ssh 10.0.0.1 uptime",
		"gssh 10.0.0.1 \"ls\"",
		"cd /tmp && scp a b:/c",
		"echo x; sshpass -p x ssh h",
		"sudo ssh h",
		"x=$(ssh h cat /f)",
	}
	for _, c := range denied {
		if cfg.checkCommand(c) == nil {
			t.Errorf("not denied: %q", c)
		}
	}
	allowed := []string{"ssh-keygen -l -f k", "ls ~/.ssh", "grep sshd /var/log/x", "echo ssh"}
	for _, c := range allowed {
		if err := cfg.checkCommand(c); err != nil {
			t.Errorf("wrongly denied %q: %v", c, err)
		}
	}
	if (&Config{}).checkCommand("ssh h") != nil {
		t.Error("deny must be off by default")
	}
	custom := &Config{DenyCommand: buildDenyPattern(`\brm\s+-rf\s+/\s*$`, false)}
	if custom.checkCommand("rm -rf /") == nil || custom.checkCommand("ssh h") != nil {
		t.Error("custom deny pattern misapplied")
	}
}

func TestCommandHint(t *testing.T) {
	cases := map[string]string{
		"cat /etc/hosts":          "remote_read_file",
		"sed -n '1,5p' f":         "remote_read_file",
		"sleep 28; grep x log":    "remote_wait_for",
		"nohup ./x.sh > l 2>&1 &": "remote_spawn",
		"cd /tmp && make":         "working_directory",
		"ps -ef | grep java":      "remote_list_processes",
		"python3 run.py --check":  "",
	}
	for cmd, want := range cases {
		got := commandHint(cmd)
		if want == "" && got != "" || !strings.Contains(got, want) {
			t.Errorf("hint(%q) = %q, want ~%q", cmd, got, want)
		}
	}
}

// ---- 3.17 session lock ---------------------------------------------------------

func TestSessionCwdConcurrent(t *testing.T) {
	sm := &SessionManager{}
	sess := sm.Create(SessionCreateRequest{WorkingDirectory: "/tmp"})
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func(i int) { defer wg.Done(); sm.UpdateWorkingDirectory(sess.ID, "/d"+strconv.Itoa(i)) }(i)
		go func() { defer wg.Done(); _ = sess.Cwd() }()
	}
	wg.Wait()
	if !strings.HasPrefix(sess.Cwd(), "/d") {
		t.Errorf("cwd = %q", sess.Cwd())
	}
}

// ---- 3.5/3.8 jobs & 3.7 wait_for ----------------------------------------------

func testJobManager(t *testing.T) *JobManager {
	return &JobManager{
		cfg:  &Config{DefaultShell: "bash", JobDir: t.TempDir()},
		jobs: make(map[string]*Job),
	}
}

func TestSpawnJobLifecycle(t *testing.T) {
	skipOnWindows(t)
	jm := testJobManager(t)
	start := time.Now()
	job, err := jm.Spawn(SpawnRequest{Command: "echo hello; sleep 0.3; echo bye; exit 3"})
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("Spawn blocked")
	}
	select {
	case <-job.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("job did not finish")
	}
	info := job.info()
	if info.State != JobExited || info.ExitCode == nil || *info.ExitCode != 3 {
		t.Errorf("info = %+v", info)
	}
	data, _ := os.ReadFile(job.LogPath)
	if string(data) != "hello\nbye\n" {
		t.Errorf("log = %q", data)
	}

	long, err := jm.Spawn(SpawnRequest{Command: "sleep 30"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := jm.Kill(long.ID, "KILL"); err != nil {
		t.Fatal(err)
	}
	<-long.Done()
	if long.info().State != JobKilled {
		t.Errorf("state = %s", long.info().State)
	}

	to, _ := jm.Spawn(SpawnRequest{Command: "sleep 30", TimeoutSeconds: 1})
	select {
	case <-to.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("job timeout not enforced")
	}
	if to.info().State != JobTimeout {
		t.Errorf("state = %s", to.info().State)
	}

	jm.sweep(time.Now().Add(jobRetention + time.Hour))
	if len(jm.List()) != 0 {
		t.Error("finished jobs not pruned")
	}
	if _, err := os.Stat(job.LogPath); !os.IsNotExist(err) {
		t.Error("job log not removed")
	}
}

func TestSpawnRefusedUnderReadOnly(t *testing.T) {
	withReadOnly(t)
	if _, err := testJobManager(t).Spawn(SpawnRequest{Command: "true"}); err == nil {
		t.Error("spawn allowed under read-only mode")
	}
}

func TestWaitForConditions(t *testing.T) {
	skipOnWindows(t)
	dir := t.TempDir()
	logf := filepath.Join(dir, "app.log")
	os.WriteFile(logf, []byte("old READY line\n"), 0o644)
	deps := waitDeps{executor: testExecutor(), jobs: testJobManager(t)}

	// log_regex ignores content present before the call.
	time.AfterFunc(400*time.Millisecond, func() {
		f, _ := os.OpenFile(logf, os.O_APPEND|os.O_WRONLY, 0)
		f.WriteString("boot...\nservice READY on 8080\n")
		f.Close()
	})
	start := time.Now()
	res, err := WaitFor(context.Background(), WaitForRequest{
		Condition: WaitLogRegex, Target: logf, Pattern: "READY", TimeoutSec: 5, IntervalMs: 200,
	}, deps)
	if err != nil || !res.Satisfied || !strings.Contains(res.Detail, "8080") {
		t.Fatalf("log_regex: %+v %v", res, err)
	}
	if time.Since(start) < 300*time.Millisecond {
		t.Error("log_regex matched pre-existing content")
	}

	res, _ = WaitFor(context.Background(), WaitForRequest{
		Condition: WaitFileContains, Target: logf, Pattern: "old READY", TimeoutSec: 1}, deps)
	if !res.Satisfied {
		t.Errorf("file_contains: %+v", res)
	}

	res, _ = WaitFor(context.Background(), WaitForRequest{
		Condition: WaitFileExists, Target: filepath.Join(dir, "nope"), TimeoutSec: 1, IntervalMs: 200}, deps)
	if res.Satisfied || !res.TimedOut {
		t.Errorf("file_exists timeout: %+v", res)
	}

	job, _ := deps.jobs.Spawn(SpawnRequest{Command: "sleep 0.5"})
	res, _ = WaitFor(context.Background(), WaitForRequest{
		Condition: WaitProcessExit, Target: job.ID, TimeoutSec: 5, IntervalMs: 200}, deps)
	if !res.Satisfied || !strings.Contains(res.Detail, "exit_code=0") {
		t.Errorf("process_exit job: %+v", res)
	}
	res, _ = WaitFor(context.Background(), WaitForRequest{
		Condition: WaitProcessExit, Target: strconv.Itoa(job.PID), TimeoutSec: 2, IntervalMs: 200}, deps)
	if !res.Satisfied {
		t.Errorf("process_exit pid: %+v", res)
	}

	marker := filepath.Join(dir, "m")
	time.AfterFunc(300*time.Millisecond, func() { os.WriteFile(marker, nil, 0o644) })
	res, _ = WaitFor(context.Background(), WaitForRequest{
		Condition: WaitCommandExit0, Target: "test -f " + marker, TimeoutSec: 5, IntervalMs: 200}, deps)
	if !res.Satisfied || res.Checks < 2 {
		t.Errorf("command_exit0: %+v", res)
	}

	if _, err := WaitFor(context.Background(), WaitForRequest{Condition: "bogus", Target: "x"}, deps); err == nil {
		t.Error("unknown condition accepted")
	}
	if _, err := WaitFor(context.Background(), WaitForRequest{
		Condition: WaitFileExists, Target: "/etc/passwd"}, waitDeps{roots: []string{dir}}); err == nil {
		t.Error("FS root sandbox not applied")
	}
}

// ---- 3.9 access log / 3.2 + 3.10 MCP audit -----------------------------------

type logLines []map[string]any

func parseLog(t *testing.T, buf *bytes.Buffer) logLines {
	t.Helper()
	var out logLines
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		m := map[string]any{}
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line is not JSON: %q", line)
		}
		out = append(out, m)
	}
	return out
}

func (l logLines) ofType(typ string) logLines {
	var out logLines
	for _, m := range l {
		if m["type"] == typ {
			out = append(out, m)
		}
	}
	return out
}

func TestLoggingMiddlewareStatusAndStreamLevel(t *testing.T) {
	var buf bytes.Buffer
	logger := newLogger(&buf, "info")
	h := LoggingMiddleware(logger, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := w.(http.Flusher); !ok {
			t.Error("statusRecorder hides http.Flusher")
		}
		w.WriteHeader(http.StatusTeapot)
	}))
	req := httptest.NewRequest(http.MethodPost, "/api/execute", nil)
	req.RemoteAddr = "10.1.2.3:5555"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/mcp", nil))

	lines := parseLog(t, &buf).ofType("access")
	if len(lines) != 1 {
		t.Fatalf("got %d access lines, want 1 (GET /mcp is debug-only)", len(lines))
	}
	l := lines[0]
	if l["status"] != float64(418) || l["remote"] != "10.1.2.3" || l["req_id"] != rec.Header().Get("X-Request-Id") {
		t.Errorf("access line = %v", l)
	}
}

// TestMCPAuditEndToEnd drives a real Streamable HTTP MCP session through the
// full middleware stack and checks that the audit record carries the real
// peer, the registered tool name, the req_id of the access line, and timing.
func TestMCPAuditEndToEnd(t *testing.T) {
	skipOnWindows(t)
	var buf bytes.Buffer
	var mu sync.Mutex
	logger := newLogger(&lockedWriter{w: &buf, mu: &mu}, "info")
	cfg := &Config{MaxTimeout: 10, MaxOutput: 4096, DefaultShell: "bash", JobDir: t.TempDir(),
		DisabledTools: map[string]bool{}}
	audit := NewAuditLogger(logger)
	mcpHandler := NewMCPHandler(NewExecutor(cfg), &SessionManager{}, testJobManager(t), audit, cfg)
	h := LoggingMiddleware(logger, mcpHandler)

	post := func(sid string, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
		req.RemoteAddr = "172.26.97.60:61234"
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if sid != "" {
			req.Header.Set(server.HeaderKeySessionID, sid)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	init := post("", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)
	sid := init.Header().Get(server.HeaderKeySessionID)
	if sid == "" {
		t.Fatalf("no session id; status %d body %s", init.Code, init.Body.String())
	}
	post(sid, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)

	call := func(id int, name string, args map[string]any) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call",
			"params": mcp.CallToolParams{Name: name, Arguments: args}})
		return post(sid, string(body))
	}
	r1 := call(2, "remote_execute", map[string]any{"command": "echo hi"})
	call(3, "remote_list_processes", map[string]any{"filter": "^no-such-process$"})
	call(4, "remote_read_file", map[string]any{"path": "/definitely/missing"})

	mu.Lock()
	lines := parseLog(t, &buf)
	mu.Unlock()
	audits := lines.ofType("audit")
	if len(audits) != 3 {
		t.Fatalf("audit lines = %d, want 3:\n%s", len(audits), buf.String())
	}
	byTool := map[string]map[string]any{}
	for _, a := range audits {
		byTool[a["tool"].(string)] = a
		if a["remote"] != "172.26.97.60" {
			t.Errorf("%s: remote = %v, want real peer host", a["tool"], a["remote"])
		}
		if a["mcp_session"] != sid {
			t.Errorf("%s: mcp_session = %v", a["tool"], a["mcp_session"])
		}
	}
	ex := byTool["remote_execute"]
	if ex == nil || ex["command"] != "echo hi" || ex["exec_id"] == "" || ex["req_id"] != r1.Header().Get("X-Request-Id") {
		t.Errorf("remote_execute audit = %v", ex)
	}
	if byTool["remote_list_processes"] == nil {
		t.Error("system tool audited under a non-registered name")
	}
	rf := byTool["remote_read_file"]
	if rf == nil || rf["exit_code"] == float64(0) || rf["error"] == nil {
		t.Errorf("failed read_file audit lacks failure reason: %v", rf)
	}

	var call2 logLines
	for _, a := range lines.ofType("access") {
		if a["req_id"] == r1.Header().Get("X-Request-Id") {
			call2 = append(call2, a)
		}
	}
	if len(call2) != 1 || call2[0]["tool"] != "remote_execute" || call2[0]["mcp_method"] != "tools/call" {
		t.Errorf("access line for the call = %v", call2)
	}
}

type lockedWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// ---- 3.12 version -------------------------------------------------------------

func TestFullVersion(t *testing.T) {
	old := gitCommit
	t.Cleanup(func() { gitCommit = old })
	gitCommit = "unknown"
	if fullVersion() != serverVersion {
		t.Errorf("fullVersion = %q", fullVersion())
	}
	gitCommit = "abc1234"
	if fullVersion() != serverVersion+"+abc1234" {
		t.Errorf("fullVersion = %q", fullVersion())
	}
}
