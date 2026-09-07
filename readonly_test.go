package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// withReadOnly latches read-only mode for the duration of a test and clears it
// afterwards. Production code has no disable path on purpose, so tests poke the
// atomic directly. Tests using this must not run in parallel (none here do).
func withReadOnly(t *testing.T) {
	t.Helper()
	readOnlyEnabled.Store(true)
	t.Cleanup(func() { readOnlyEnabled.Store(false) })
}

func TestParseReadOnly(t *testing.T) {
	truthy := []string{"1", "true", "TRUE", "True", "yes", "YES", "on", "enabled", " true "}
	for _, v := range truthy {
		if !parseReadOnly(v) {
			t.Errorf("parseReadOnly(%q) = false, want true", v)
		}
	}
	falsy := []string{"", "0", "false", "no", "off", "disabled", "maybe", "2"}
	for _, v := range falsy {
		if parseReadOnly(v) {
			t.Errorf("parseReadOnly(%q) = true, want false", v)
		}
	}
}

// Read-only must beat the operator's own configuration: even an explicitly
// empty blacklist cannot re-enable a mutating tool.
func TestReadOnlyOverridesBlacklist(t *testing.T) {
	cfg := &Config{DisabledTools: map[string]bool{}}
	cfg.applyReadOnly(true)
	t.Cleanup(func() { readOnlyEnabled.Store(false) })

	mutating := []string{
		"remote_execute", "remote_session_execute", "remote_cancel",
		"remote_write_file", "remote_edit_file", "remote_upload_base64",
		"remote_move_file", "remote_copy_file", "remote_delete_file",
		"remote_make_dir", "remote_session_create", "remote_session_list",
		"remote_session_close",
	}
	for _, name := range mutating {
		if cfg.toolEnabled(name) {
			t.Errorf("%s enabled under read-only mode", name)
		}
	}
	for name := range readOnlyTools {
		if !cfg.toolEnabled(name) {
			t.Errorf("read-only tool %s was disabled", name)
		}
	}
	// An unknown (future) tool must be denied by default: allowlist semantics.
	if cfg.toolEnabled("remote_some_new_tool") {
		t.Error("unknown tool was allowed under read-only mode")
	}
	if !cfg.ReadOnly || !isReadOnly() {
		t.Error("read-only latch not set")
	}
}

// applyReadOnly(false) must never clear an already-latched read-only state.
func TestReadOnlyCannotBeDowngraded(t *testing.T) {
	cfg := &Config{DisabledTools: map[string]bool{}}
	cfg.applyReadOnly(true)
	t.Cleanup(func() { readOnlyEnabled.Store(false) })

	cfg.applyReadOnly(false)
	if !cfg.ReadOnly || !isReadOnly() {
		t.Error("applyReadOnly(false) cleared an enabled read-only mode")
	}
	if cfg.toolEnabled("remote_execute") {
		t.Error("remote_execute re-enabled after downgrade attempt")
	}
}

// LoadConfig must honour SHELL_API_READONLY from the environment.
func TestLoadConfigReadOnlyFromEnv(t *testing.T) {
	t.Setenv("SHELL_API_TOKEN", "test-token")
	t.Setenv("SHELL_API_READONLY", "true")
	t.Cleanup(func() { readOnlyEnabled.Store(false) })

	cfg := LoadConfig()
	if !cfg.ReadOnly {
		t.Fatal("SHELL_API_READONLY=true did not enable read-only mode")
	}
	if cfg.toolEnabled("remote_write_file") {
		t.Error("remote_write_file enabled after SHELL_API_READONLY=true")
	}
	if !cfg.DisabledTools["remote_execute"] {
		t.Error("forced blacklist not reflected in DisabledTools (env info would misreport)")
	}
}

// Layer 4: every mutating file operation fails closed, even when called
// directly with a valid sandboxed path.
func TestReadOnlyBlocksFileMutations(t *testing.T) {
	root := t.TempDir()
	existing := filepath.Join(root, "existing.txt")
	if err := os.WriteFile(existing, []byte("hello\n"), 0644); err != nil {
		t.Fatal(err)
	}
	roots := []string{root}

	withReadOnly(t)

	target := filepath.Join(root, "new.txt")

	if _, err := WriteFileContent(WriteFileRequest{Path: target, Content: "x"}, roots); err == nil {
		t.Error("WriteFileContent succeeded under read-only mode")
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Error("WriteFileContent created a file under read-only mode")
	}

	if _, err := EditFileContent(EditFileRequest{Path: existing, OldString: "hello", NewString: "bye"}, roots); err == nil {
		t.Error("EditFileContent succeeded under read-only mode")
	}
	if _, err := UploadBase64(UploadBase64Request{Path: target, DataB64: "eA=="}, roots); err == nil {
		t.Error("UploadBase64 succeeded under read-only mode")
	}
	if _, err := MoveFile(MoveFileRequest{Src: existing, Dst: target}, roots); err == nil {
		t.Error("MoveFile succeeded under read-only mode")
	}
	if _, err := CopyFile(CopyFileRequest{Src: existing, Dst: target}, roots); err == nil {
		t.Error("CopyFile succeeded under read-only mode")
	}
	if _, err := MakeDir(MakeDirRequest{Path: filepath.Join(root, "sub"), Parents: true}, roots); err == nil {
		t.Error("MakeDir succeeded under read-only mode")
	}
	if _, err := DeleteFile(DeleteFileRequest{Path: existing, Confirm: true}, roots); err == nil {
		t.Error("DeleteFile succeeded under read-only mode")
	}
	// dry_run is refused too: the tool does not exist in this mode at all.
	if _, err := DeleteFile(DeleteFileRequest{Path: existing, DryRun: true}, roots); err == nil {
		t.Error("DeleteFile dry_run succeeded under read-only mode")
	}

	// The original file must be untouched by any of the above.
	data, err := os.ReadFile(existing)
	if err != nil {
		t.Fatalf("original file damaged: %v", err)
	}
	if string(data) != "hello\n" {
		t.Errorf("original content changed to %q", data)
	}
}

// Reads must keep working under read-only mode.
func TestReadOnlyAllowsReads(t *testing.T) {
	root := t.TempDir()
	f := filepath.Join(root, "log.txt")
	if err := os.WriteFile(f, []byte("line1\nline2\n"), 0644); err != nil {
		t.Fatal(err)
	}
	roots := []string{root}

	withReadOnly(t)

	rr, err := ReadFileContent(ReadFileRequest{Path: f}, roots)
	if err != nil {
		t.Fatalf("ReadFileContent failed under read-only mode: %v", err)
	}
	if !strings.Contains(rr.Content, "line1") {
		t.Errorf("unexpected content: %q", rr.Content)
	}
	if _, err := ListDirectory(ListDirRequest{Path: root}, roots); err != nil {
		t.Errorf("ListDirectory failed under read-only mode: %v", err)
	}
	if _, err := StatFile(StatRequest{Path: f}, roots); err != nil {
		t.Errorf("StatFile failed under read-only mode: %v", err)
	}
	if _, err := SearchContent(SearchContentRequest{Path: root, Pattern: "line"}, roots); err != nil {
		t.Errorf("SearchContent failed under read-only mode: %v", err)
	}
	if _, err := FindFiles(FindFilesRequest{Path: root}, roots); err != nil {
		t.Errorf("FindFiles failed under read-only mode: %v", err)
	}
	if _, err := TailLog(TailLogRequest{Path: f, Lines: 1}, roots); err != nil {
		t.Errorf("TailLog failed under read-only mode: %v", err)
	}
	if _, err := DownloadBase64(DownloadBase64Request{Path: f}, roots); err != nil {
		t.Errorf("DownloadBase64 failed under read-only mode: %v", err)
	}
}

// Layer 4 for shell execution: the executor refuses to spawn anything.
func TestReadOnlyBlocksExecutor(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(root, "written-by-shell")
	exec := NewExecutor(&Config{MaxTimeout: 30, MaxOutput: 65536, DefaultShell: "bash"})

	withReadOnly(t)

	res := exec.Execute(ExecuteRequest{Command: "touch " + marker})
	if res.ExitCode == 0 {
		t.Error("Execute reported success under read-only mode")
	}
	if !strings.Contains(res.Stderr, "read-only") {
		t.Errorf("Execute stderr did not explain the refusal: %q", res.Stderr)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Error("shell command ran under read-only mode")
	}

	var sawError bool
	var exitCode int
	exec.ExecuteStream(ExecuteRequest{Command: "touch " + marker + "-stream"}, func(e StreamEvent) {
		if e.Type == "error" {
			sawError = true
		}
		if e.Type == "exit" {
			exitCode = e.ExitCode
		}
	})
	if !sawError || exitCode == 0 {
		t.Error("ExecuteStream did not refuse under read-only mode")
	}
	if _, err := os.Stat(marker + "-stream"); !os.IsNotExist(err) {
		t.Error("streamed shell command ran under read-only mode")
	}
}

// Linting spawns an interpreter, so it must be skipped under read-only mode.
func TestReadOnlySkipsLint(t *testing.T) {
	withReadOnly(t)
	res := LintFile("/tmp/whatever.sh", "bash")
	if !res.Skipped {
		t.Error("LintFile did not skip under read-only mode")
	}
}

// Layer 3: the method filter rejects any non-GET/HEAD request under /api/.
func TestReadOnlyMiddleware(t *testing.T) {
	reached := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})
	h := ReadOnlyMiddleware(true, next)

	cases := []struct {
		method, path string
		wantStatus   int
		wantReached  bool
	}{
		{http.MethodPost, "/api/execute", http.StatusForbidden, false},
		{http.MethodPost, "/api/sessions", http.StatusForbidden, false},
		{http.MethodDelete, "/api/executions/abc", http.StatusForbidden, false},
		{http.MethodPut, "/api/anything-new", http.StatusForbidden, false},
		{http.MethodGet, "/api/status", http.StatusOK, true},
		// MCP rides on POST by protocol and is gated by tool registration.
		{http.MethodPost, "/mcp", http.StatusOK, true},
	}

	for _, c := range cases {
		reached = false
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, nil))
		if rec.Code != c.wantStatus {
			t.Errorf("%s %s: status %d, want %d", c.method, c.path, rec.Code, c.wantStatus)
		}
		if reached != c.wantReached {
			t.Errorf("%s %s: handler reached = %v, want %v", c.method, c.path, reached, c.wantReached)
		}
		if rec.Header().Get("X-Read-Only") != "true" {
			t.Errorf("%s %s: missing X-Read-Only header", c.method, c.path)
		}
	}

	// Disabled mode must be a pass-through with no added header.
	reached = false
	rec := httptest.NewRecorder()
	ReadOnlyMiddleware(false, next).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/execute", nil))
	if !reached || rec.Code != http.StatusOK {
		t.Error("ReadOnlyMiddleware(false) did not pass the request through")
	}
}

// The allowlist and the inventory must stay consistent, and no tool may be
// allowed that is not part of the known inventory.
func TestReadOnlyToolListsConsistent(t *testing.T) {
	inventory := make(map[string]bool, len(allToolNames))
	for _, n := range allToolNames {
		if inventory[n] {
			t.Errorf("duplicate tool in allToolNames: %s", n)
		}
		inventory[n] = true
	}
	for n := range readOnlyTools {
		if !inventory[n] {
			t.Errorf("readOnlyTools contains %s which is not in allToolNames", n)
		}
	}
	disabled := readOnlyDisabledTools()
	if got, want := len(disabled)+len(readOnlyTools), len(allToolNames); got != want {
		t.Errorf("allowed+disabled = %d, want %d (inventory size)", got, want)
	}
	for _, n := range disabled {
		if readOnlyTools[n] {
			t.Errorf("%s is both allowed and disabled", n)
		}
	}
}

// allToolNames must match what the MCP server actually registers, otherwise the
// read-only accounting silently drifts from reality.
func TestToolInventoryMatchesRegistrations(t *testing.T) {
	registered := map[string]bool{}
	for _, f := range []string{"mcp.go", "mcp_fileops.go", "mcp_system.go"} {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for _, m := range regexp.MustCompile(`mcp\.NewTool\("([a-z0-9_]+)"`).FindAllStringSubmatch(string(src), -1) {
			registered[m[1]] = true
		}
	}
	for name := range registered {
		if !slices.Contains(allToolNames, name) {
			t.Errorf("tool %s is registered but missing from allToolNames "+
				"(read-only mode would not report it as disabled)", name)
		}
	}
	for _, name := range allToolNames {
		if !registered[name] {
			t.Errorf("allToolNames lists %s but no registration was found", name)
		}
	}
}

// Layer-4 completeness check. Every function containing a filesystem-mutating
// syscall must either call guardReadOnly itself or be an unexported helper only
// reachable from a guarded caller. This is what stops a future write path from
// landing without a guard.
func TestEveryMutatingFuncIsGuarded(t *testing.T) {
	// Unexported helpers whose only callers are already-guarded entry points.
	// Keep this list short and justified; anything else must guard itself.
	allowedHelpers := map[string]string{
		"copyOneFile":      "reached only from MoveFile / CopyFile",
		"copyDirRecursive": "reached only from MoveFile / CopyFile",
	}

	writeSyscall := regexp.MustCompile(
		`os\.(Create|WriteFile|OpenFile|Remove|RemoveAll|Rename|Mkdir|MkdirAll|Chmod|Chown|Symlink|Link|Truncate)\(`)
	funcDecl := regexp.MustCompile(`^func (?:\([^)]*\) )?([A-Za-z_][A-Za-z0-9_]*)\(`)

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}

	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}

		var fn string
		bodies := map[string]*strings.Builder{}
		for _, line := range strings.Split(string(src), "\n") {
			if m := funcDecl.FindStringSubmatch(line); m != nil {
				fn = m[1]
				if bodies[fn] == nil {
					bodies[fn] = &strings.Builder{}
				}
				continue
			}
			if fn != "" && bodies[fn] != nil {
				bodies[fn].WriteString(line + "\n")
			}
		}

		for name, body := range bodies {
			text := body.String()
			if !writeSyscall.MatchString(text) {
				continue
			}
			if strings.Contains(text, "guardReadOnly(") {
				continue
			}
			if _, ok := allowedHelpers[name]; ok {
				continue
			}
			t.Errorf("%s: %s mutates the filesystem but never calls guardReadOnly "+
				"(add the guard, or document it in allowedHelpers if its callers are guarded)",
				file, name)
		}
	}
}
