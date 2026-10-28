package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/server"
)

// format=text renders the read result as a header plus the plain content.
func TestReadFileText(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "a.cpp")
	var sb strings.Builder
	for i := 1; i <= 5000; i++ {
		fmt.Fprintf(&sb, "if (a && b->c > %d) { x <<= 1; }\n", i)
	}
	os.WriteFile(p, []byte(sb.String()), 0o644)

	r, err := ReadFileContent(ReadFileRequest{Path: p, StartLine: 10, EndLine: 11, LineNumbers: true}, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	got := readFileText(r)
	// The byte range is the raw window (line 10 up to line 12), not the
	// length of the numbered, decoded content.
	want := p + " [lines 10-11 of 5000 bytes " + strconv.FormatInt(r.Offset, 10) + "-" +
		strconv.FormatInt(r.NextOffset, 10) + " of " + strconv.Itoa(sb.Len()) +
		" next_start_line=12 next_offset=" + strconv.FormatInt(r.NextOffset, 10) + "]\n" +
		"    10\tif (a && b->c > 10) { x <<= 1; }\n    11\tif (a && b->c > 11) { x <<= 1; }\n"
	if got != want {
		t.Errorf("text:\n%s\nwant:\n%s", got, want)
	}

	// The text form is shorter than the JSON form and carries no escapes.
	r, _ = ReadFileContent(ReadFileRequest{Path: p}, []string{root})
	js, _ := json.Marshal(r)
	text := readFileText(r)
	if len(text) >= len(js) || strings.Contains(text, `\u003c`) || !strings.Contains(text, "truncated") ||
		!strings.Contains(text, "next_offset=") || !strings.Contains(text, "hint: ") {
		t.Errorf("text=%d bytes json=%d bytes, head=%q", len(text), len(js), text[:min(len(text), 200)])
	}

	// Picked lines and a binary file.
	r, _ = ReadFileContent(ReadFileRequest{Path: p, Lines: []int{3, 4999, 9999}}, []string{root})
	if got := readFileText(r); !strings.Contains(got, "[2 requested lines of 5000 size=") || !strings.Contains(got, "missing_lines=[9999]") {
		t.Errorf("picked lines text: %q", got[:min(len(got), 200)])
	}
	bin := filepath.Join(root, "x.bin")
	os.WriteFile(bin, []byte{0, 1, 2}, 0o644)
	r, _ = ReadFileContent(ReadFileRequest{Path: bin}, []string{root})
	if got := readFileText(r); !strings.Contains(got, "binary file, 3 bytes") {
		t.Errorf("binary text: %q", got)
	}
}

// format=text for search mirrors grep -n: file headers, N:match, N-context,
// and -- between separate groups of one file.
func TestSearchText(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"a.log": "x\nERROR one\ny\nz\nw\nv\nERROR two\nu\n",
		"b.log": "ERROR three\n",
	})
	res, err := SearchContent(SearchContentRequest{Path: root, Pattern: "ERROR", ContextLines: 1}, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	got := searchText(res)
	a, b := filepath.Join(root, "a.log"), filepath.Join(root, "b.log")
	want := "\n" + a + "\n1-x\n2:ERROR one\n3-y\n--\n6-v\n7:ERROR two\n8-u\n\n" + b + "\n1:ERROR three\n"
	if !strings.HasSuffix(got, want) || !strings.HasPrefix(got, "ERROR in "+root+" [2 matching files 2 files searched 3 matches]") {
		t.Errorf("search text:\n%s\nwant suffix:\n%s", got, want)
	}

	res, _ = SearchContent(SearchContentRequest{Path: root, Pattern: "ERROR", OutputMode: SearchCountMode}, []string{root})
	if got := searchText(res); !strings.Contains(got, a+":2\n") || !strings.Contains(got, b+":1\n") {
		t.Errorf("count text: %q", got)
	}
}

// format=text for remote_diff and remote_tail_log.
func TestDiffAndTailText(t *testing.T) {
	root := t.TempDir()
	old, nw := filepath.Join(root, "old"), filepath.Join(root, "new")
	writeTree(t, old, map[string]string{"a.conf": "port : 8080\n", "gone.conf": "x\n"})
	writeTree(t, nw, map[string]string{"a.conf": "port : 9090\n", "new.conf": "y\n"})
	r, err := Diff(DiffRequest{Left: old, Right: nw, ShowDiffs: true}, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	got := diffText(r)
	for _, want := range []string{"[1 added, 1 removed, 1 changed, 0 same]", "only in right: new.conf\n",
		"only in left:  gone.conf\n", "changed:       a.conf\n", "-port : 8080\n+port : 9090\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("tree diff text lacks %q:\n%s", want, got)
		}
	}

	log := filepath.Join(root, "app.log")
	writeLines(t, log, 10)
	tl, _ := TailLog(TailLogRequest{Path: log, Lines: 2}, []string{root})
	size := strconv.FormatInt(tl.Size, 10) // 9 lines of 11 bytes + "log line 10\n"
	if got := tailLogText(tl); size != "111" || got != log+" [size=111 end_offset=111]\nlog line 9\nlog line 10\n" {
		t.Errorf("tail text: %q", got)
	}
	tl, _ = TailLog(TailLogRequest{Path: log, SinceLine: 8}, []string{root})
	if got := tailLogText(tl); !strings.Contains(got, "end_line=10") {
		t.Errorf("tail since_line text: %q", got)
	}
}

// Over MCP, format=text must return text only: with structured content
// present, Claude Code shows the model the JSON and ignores the text.
func TestMCPFormatTextHasNoStructuredContent(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "a.conf")
	os.WriteFile(p, []byte("port : 8080\n"), 0o644)
	cfg := &Config{MaxTimeout: 30, MaxOutput: 4096, DefaultShell: "bash", JobDir: t.TempDir(),
		DisabledTools: map[string]bool{}, RateLimit: 120, RateBurst: 60, FSRoots: []string{root}}
	cfg.applyReadOnly(true)
	t.Cleanup(func() { readOnlyEnabled.Store(false) })
	h := NewMCPHandler(NewExecutor(cfg), &SessionManager{}, testJobManager(t), NewWaitManager(),
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
	sid := post("", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`).
		Header().Get(server.HeaderKeySessionID)
	post(sid, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)

	call := func(id int, args map[string]any) map[string]any {
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call",
			"params": map[string]any{"name": "remote_read_file", "arguments": args}})
		var resp struct {
			Result map[string]any `json:"result"`
		}
		if err := json.Unmarshal(post(sid, string(body)).Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		return resp.Result
	}
	text := call(2, map[string]any{"path": p, "format": "text"})
	if _, ok := text["structuredContent"]; ok {
		t.Errorf("format=text result carries structuredContent: %v", text)
	}
	content, _ := text["content"].([]any)
	if len(content) != 1 || !strings.HasSuffix(content[0].(map[string]any)["text"].(string), "\nport : 8080\n") {
		t.Errorf("format=text content: %v", text["content"])
	}
	if js := call(3, map[string]any{"path": p}); js["structuredContent"] == nil {
		t.Errorf("default result lacks structuredContent: %v", js)
	}
}
