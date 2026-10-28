package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func setupSearchTree(t *testing.T) string {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "sub"), 0755)
	os.MkdirAll(filepath.Join(root, ".git"), 0755)
	os.WriteFile(filepath.Join(root, "a.conf"), []byte("port = 3222\n# comment\nhost = 10.0.0.1\n"), 0644)
	os.WriteFile(filepath.Join(root, "b.log"), []byte("INFO start\nFATAL boom\nINFO end\n"), 0644)
	os.WriteFile(filepath.Join(root, "sub", "c.conf"), []byte("PORT = 4000\n"), 0644)
	os.WriteFile(filepath.Join(root, ".git", "d.conf"), []byte("port = 9999\n"), 0644)
	os.WriteFile(filepath.Join(root, "bin.dat"), []byte{0x00, 0x01, 'p', 'o', 'r', 't'}, 0644)
	return root
}

func TestSearchContentBasic(t *testing.T) {
	root := setupSearchTree(t)

	res, err := SearchContent(SearchContentRequest{Path: root, Pattern: "port"}, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	// a.conf:1, sub/c.conf skipped (case), .git skipped (hidden), bin.dat skipped (binary).
	if res.TotalMatches != 1 {
		t.Errorf("matches=%d, want 1: %+v", res.TotalMatches, res.Matches)
	}
	if res.Matches[0].Line != 1 {
		t.Errorf("line=%d", res.Matches[0].Line)
	}

	// Case-insensitive picks up sub/c.conf too.
	res, _ = SearchContent(SearchContentRequest{Path: root, Pattern: "port", IgnoreCase: true}, []string{root})
	if res.TotalMatches != 2 {
		t.Errorf("icase matches=%d, want 2", res.TotalMatches)
	}

	// No match is a success with an empty list.
	res, err = SearchContent(SearchContentRequest{Path: root, Pattern: "zzz-not-here"}, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	if res.TotalMatches != 0 || res.Truncated {
		t.Errorf("no-match should be empty success: %+v", res)
	}
}

func TestSearchContentGlobAndContext(t *testing.T) {
	root := setupSearchTree(t)

	res, err := SearchContent(SearchContentRequest{
		Path:         root,
		Pattern:      "FATAL",
		IncludeGlob:  []string{"*.log"},
		ContextLines: 1,
	}, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	if res.TotalMatches != 1 {
		t.Fatalf("matches=%d, want 1", res.TotalMatches)
	}
	m := res.Matches[0]
	if len(m.Before) != 1 || m.Before[0] != "INFO start" {
		t.Errorf("before=%v", m.Before)
	}
	if len(m.After) != 1 || m.After[0] != "INFO end" {
		t.Errorf("after=%v", m.After)
	}
}

func TestSearchContentMaxResults(t *testing.T) {
	root := t.TempDir()
	var content string
	for i := 0; i < 10; i++ {
		content += "hit line\n"
	}
	os.WriteFile(filepath.Join(root, "many.txt"), []byte(content), 0644)

	res, err := SearchContent(SearchContentRequest{Path: root, Pattern: "hit", MaxResults: 3}, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	if res.TotalMatches != 3 || !res.Truncated {
		t.Errorf("max_results: matches=%d truncated=%v", res.TotalMatches, res.Truncated)
	}
}

func TestSearchContentInvalidPattern(t *testing.T) {
	root := t.TempDir()
	if _, err := SearchContent(SearchContentRequest{Path: root, Pattern: "["}, []string{root}); err == nil {
		t.Error("expected invalid pattern error")
	}
	if _, err := SearchContent(SearchContentRequest{Path: root, Pattern: ""}, []string{root}); err == nil {
		t.Error("expected empty pattern error")
	}
}

func TestFindFilesBasic(t *testing.T) {
	root := setupSearchTree(t)

	res, err := FindFiles(FindFilesRequest{Path: root, NameGlob: []string{"*.conf"}}, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	// a.conf + sub/c.conf; .git/d.conf excluded (hidden).
	if res.Count != 2 {
		t.Errorf("count=%d, want 2: %+v", res.Count, res.Entries)
	}

	res, _ = FindFiles(FindFilesRequest{Path: root, Type: "dir", IncludeHidden: true}, []string{root})
	if res.Count != 2 { // sub + .git
		t.Errorf("dir count=%d, want 2", res.Count)
	}

	res, _ = FindFiles(FindFilesRequest{Path: root, MaxDepth: 1}, []string{root})
	// depth1: a.conf b.log bin.dat .git .hidden? (.git dir is hidden -> excluded)
	for _, e := range res.Entries {
		if e.Path == filepath.Join(root, "sub", "c.conf") {
			t.Errorf("depth limit violated: %s", e.Path)
		}
	}

	res, _ = FindFiles(FindFilesRequest{Path: root, NameGlob: []string{"*.CONF"}, IgnoreCase: true}, []string{root})
	if res.Count != 2 {
		t.Errorf("icase glob count=%d, want 2", res.Count)
	}
}

func TestFindFilesTruncation(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 10; i++ {
		os.WriteFile(filepath.Join(root, "f"+string(rune('0'+i))+".txt"), []byte("x"), 0644)
	}
	res, err := FindFiles(FindFilesRequest{Path: root, MaxResults: 4}, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	if res.Count != 4 || !res.Truncated {
		t.Errorf("truncation: count=%d truncated=%v", res.Count, res.Truncated)
	}
}

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(root, rel)
		os.MkdirAll(filepath.Dir(p), 0755)
		if err := os.WriteFile(p, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
}

// Path globs, ** and {a,b} alternation work in include_glob and name_glob.
func TestSearchGlobs(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"conf/a.conf":       "port : 8080\n",
		"conf/sub/b.conf":   "port : 9090\n",
		"src/x.h":           "int port;\n",
		"src/x.cpp":         "int port = 1;\n",
		"src/x.go":          "var port int\n",
		"log/app.log":       "port bound\n",
		"third_party/y.cpp": "int port;\n",
	})
	roots := []string{root}
	cases := []struct {
		include, exclude []string
		want             int
	}{
		{[]string{"*.conf"}, nil, 2},
		{[]string{"conf/*.conf"}, nil, 1},
		{[]string{"**/*.conf"}, nil, 2},
		{[]string{"conf/**"}, nil, 2},
		{[]string{"*.{h,cpp}"}, nil, 3},
		{[]string{"*.{h,cpp}"}, []string{"third_party"}, 2},
		{nil, []string{"log", "*.go"}, 5},
	}
	for _, c := range cases {
		res, err := SearchContent(SearchContentRequest{Path: root, Pattern: "port", IncludeGlob: c.include, ExcludeGlob: c.exclude}, roots)
		if err != nil {
			t.Fatal(err)
		}
		if res.FilesMatched != c.want {
			t.Errorf("include=%v exclude=%v: %d files, want %d", c.include, c.exclude, res.FilesMatched, c.want)
		}
	}
	f, _ := FindFiles(FindFilesRequest{Path: root, NameGlob: []string{"**/*.{h,cpp}"}, ExcludeGlob: []string{"third_party"}}, roots)
	if f.Count != 2 {
		t.Errorf("find **/*.{h,cpp}: %d, want 2: %+v", f.Count, f.Entries)
	}
	if _, err := SearchContent(SearchContentRequest{Path: root, Pattern: "x", IncludeGlob: []string{"*.{h"}}, roots); err == nil {
		t.Error("invalid glob accepted")
	}
}

// files_with_matches / count modes, fixed strings, and a hint for \|.
func TestSearchOutputModes(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"a.log": "ERROR one\nINFO\nERROR two\n",
		"b.log": "ERROR three\n",
		"c.log": "INFO only\n",
	})
	roots := []string{root}
	res, _ := SearchContent(SearchContentRequest{Path: root, Pattern: "ERROR", OutputMode: SearchFilesMode}, roots)
	if len(res.Files) != 2 || res.Matches != nil || res.FilesMatched != 2 {
		t.Errorf("files_with_matches: %+v", res)
	}
	res, _ = SearchContent(SearchContentRequest{Path: root, Pattern: "ERROR", OutputMode: SearchCountMode}, roots)
	counts := map[string]int{}
	for _, f := range res.Files {
		counts[filepath.Base(f.File)] = f.Count
	}
	if counts["a.log"] != 2 || counts["b.log"] != 1 || res.TotalMatches != 3 {
		t.Errorf("count: %+v", res)
	}
	res, _ = SearchContent(SearchContentRequest{Path: root, Pattern: `ERROR\|INFO`}, roots)
	if res.FilesMatched != 0 || !strings.Contains(res.Hint, "bare |") {
		t.Errorf("GNU alternation hint missing: %+v", res)
	}
	os.WriteFile(filepath.Join(root, "re.conf"), []byte("a.b(c)\naXb(c)\n"), 0644)
	res, _ = SearchContent(SearchContentRequest{Path: filepath.Join(root, "re.conf"), Pattern: "a.b(c)", FixedStrings: true}, roots)
	if res.TotalMatches != 1 {
		t.Errorf("fixed_strings: %d matches, want 1", res.TotalMatches)
	}
}

// Overlapping context is emitted once; -B/-A are independent.
func TestSearchContextMerge(t *testing.T) {
	root := t.TempDir()
	var sb strings.Builder
	for i := 1; i <= 30; i++ {
		if i >= 10 && i <= 14 {
			sb.WriteString("ERROR at " + strconv.Itoa(i) + "\n")
		} else {
			sb.WriteString("info " + strconv.Itoa(i) + "\n")
		}
	}
	p := filepath.Join(root, "ctx.log")
	os.WriteFile(p, []byte(sb.String()), 0644)
	res, _ := SearchContent(SearchContentRequest{Path: p, Pattern: "ERROR", ContextLines: 5}, []string{root})
	lines := 0
	for _, m := range res.Matches {
		lines += 1 + len(m.Before) + len(m.After)
	}
	if len(res.Matches) != 5 || lines != 15 {
		t.Errorf("merged context: %d matches, %d lines, want 5 and 15", len(res.Matches), lines)
	}
	if b := res.Matches[0].Before; len(b) != 5 || b[0] != "info 5" {
		t.Errorf("before of first match: %v", b)
	}
	if a := res.Matches[4].After; len(a) != 5 || a[4] != "info 19" {
		t.Errorf("after of last match: %v", a)
	}
	res, _ = SearchContent(SearchContentRequest{Path: p, Pattern: "ERROR at 10", BeforeLines: 2, AfterLines: 0}, []string{root})
	if m := res.Matches[0]; len(m.Before) != 2 || len(m.After) != 0 {
		t.Errorf("-B2 -A0: %+v", m)
	}
}

// Paging with offset returns each match once; truncation is reported.
func TestSearchPaging(t *testing.T) {
	root := t.TempDir()
	var sb strings.Builder
	for i := 1; i <= 25; i++ {
		sb.WriteString("hit " + strconv.Itoa(i) + "\n")
	}
	p := filepath.Join(root, "many.txt")
	os.WriteFile(p, []byte(sb.String()), 0644)
	seen := map[int]bool{}
	offset := 0
	for page := 0; page < 10; page++ {
		res, err := SearchContent(SearchContentRequest{Path: p, Pattern: "hit", MaxResults: 10, Offset: offset}, []string{root})
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range res.Matches {
			if seen[m.Line] {
				t.Fatalf("line %d returned twice", m.Line)
			}
			seen[m.Line] = true
		}
		if !res.Truncated {
			break
		}
		if res.NextOffset != offset+10 || !strings.Contains(res.Hint, "offset=") {
			t.Fatalf("page %d: next_offset=%d hint=%q", page, res.NextOffset, res.Hint)
		}
		offset = res.NextOffset
	}
	if len(seen) != 25 {
		t.Errorf("paging returned %d distinct matches, want 25", len(seen))
	}
	// Exactly max_results matches is not truncated.
	res, _ := SearchContent(SearchContentRequest{Path: p, Pattern: "hit", MaxResults: 25}, []string{root})
	if res.Truncated || len(res.Matches) != 25 {
		t.Errorf("exact page: truncated=%v matches=%d", res.Truncated, len(res.Matches))
	}
}

// Large files are streamed (no max_file_size skip at a few MB), long lines
// are excerpted, and a stray invalid byte does not hide UTF-8 matches.
func TestSearchStreamingAndEncoding(t *testing.T) {
	root := t.TempDir()
	var sb strings.Builder
	for i := 1; i <= 100000; i++ {
		sb.WriteString("line " + strconv.Itoa(i) + " payload payload payload\n")
	}
	big := filepath.Join(root, "big.log")
	os.WriteFile(big, []byte(sb.String()), 0644)
	res, _ := SearchContent(SearchContentRequest{Path: big, Pattern: "^line 99999 "}, []string{root})
	if res.TotalMatches != 1 || res.Matches[0].Line != 99999 || res.FilesSkipped != 0 {
		t.Errorf("streamed big file: %+v", res)
	}

	long := filepath.Join(root, "min.json")
	os.WriteFile(long, []byte(`{"a":"`+strings.Repeat("v", 300000)+`","needle":1}`+"\n"), 0644)
	res, _ = SearchContent(SearchContentRequest{Path: long, Pattern: "needle"}, []string{root})
	if len(res.Matches) != 1 || len(res.Matches[0].Content) > maxSearchLineLen+64 || !strings.Contains(res.Matches[0].Content, "needle") {
		t.Errorf("long line excerpt: %d bytes", len(res.Matches[0].Content))
	}

	mixed := filepath.Join(root, "mixed.conf")
	os.WriteFile(mixed, []byte("# 预算配置\nbudget = 100\n\xff\n"), 0644)
	res, _ = SearchContent(SearchContentRequest{Path: mixed, Pattern: "预算"}, []string{root})
	if res.TotalMatches != 1 {
		t.Errorf("utf-8 with a stray byte: %d matches", res.TotalMatches)
	}
	gbk, _ := encodeFromUTF8("计费配置\n扣费失败\n", "gbk")
	os.WriteFile(filepath.Join(root, "gbk.conf"), gbk, 0644)
	res, _ = SearchContent(SearchContentRequest{Path: filepath.Join(root, "gbk.conf"), Pattern: "扣费"}, []string{root})
	if res.TotalMatches != 1 || res.Matches[0].Content != "扣费失败" {
		t.Errorf("gbk file: %+v", res)
	}
}

// Skipped files are counted by reason.
func TestSearchSkipReasons(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"big.txt": strings.Repeat("x", 2000), "ok.txt": "x\n"})
	os.WriteFile(filepath.Join(root, "bin.dat"), []byte{0, 1, 'x'}, 0644)
	res, _ := SearchContent(SearchContentRequest{Path: root, Pattern: "x", MaxFileSize: 1000}, []string{root})
	if res.SkipReasons["too_large"] != 1 || res.SkipReasons["binary"] != 1 || res.FilesSearched != 1 {
		t.Errorf("skip reasons: %+v", res)
	}
	if !strings.Contains(res.Hint, "max_file_size") {
		t.Errorf("too_large hint missing: %q", res.Hint)
	}
}

// Symlinks: a link inside the root that points outside it is refused (read
// and search), a symlinked directory given as the path is followed, and one
// met during a walk is reported, not silently dropped.
func TestSymlinkSandbox(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("TOP-SECRET\n"), 0600)
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(root, "innocent.txt"))
	roots := []string{root}

	if _, err := ReadFileContent(ReadFileRequest{Path: filepath.Join(root, "link", "secret.txt")}, roots); err == nil {
		t.Error("read through a symlinked dir escaped the root")
	}
	if _, err := ReadFileContent(ReadFileRequest{Path: filepath.Join(root, "innocent.txt")}, roots); err == nil {
		t.Error("read through a symlinked file escaped the root")
	}
	res, _ := SearchContent(SearchContentRequest{Path: root, Pattern: "SECRET"}, roots)
	if res.TotalMatches != 0 || res.SkipReasons["outside_roots"] != 2 || len(res.SymlinkDirs) != 0 {
		t.Errorf("search followed a link out of the root, or lists an unsearchable dir: %+v", res)
	}
	// A symlinked dir is reported even when include_glob filters files.
	res, _ = SearchContent(SearchContentRequest{Path: root, Pattern: "SECRET", IncludeGlob: []string{"*.log"}}, roots)
	if res.SkipReasons["outside_roots"] != 1 {
		t.Errorf("symlinked dir hidden by include_glob: %+v", res)
	}
	st, err := StatFile(StatRequest{Path: filepath.Join(root, "innocent.txt"), IncludeHash: "sha256"}, roots)
	if err != nil || !st.IsSymlink || !st.TargetOutsideRoots || st.Hash != "" {
		t.Errorf("stat of an escaping link: %+v %v", st, err)
	}
	// Writes are refused the same way.
	if _, err := WriteFileContent(WriteFileRequest{Path: filepath.Join(root, "link", "planted.txt"), Content: "x"}, roots); err == nil {
		t.Error("write through a symlinked dir escaped the root")
	}

	// A symlinked log dir inside the root (app/log -> real-logs).
	logs := filepath.Join(root, "real-logs")
	os.MkdirAll(logs, 0755)
	os.WriteFile(filepath.Join(logs, "app.log"), []byte("FATAL boom\n"), 0644)
	os.Symlink(logs, filepath.Join(root, "log"))
	res, _ = SearchContent(SearchContentRequest{Path: filepath.Join(root, "log"), Pattern: "FATAL"}, roots)
	if res.TotalMatches != 1 || !strings.HasPrefix(res.Matches[0].File, filepath.Join(root, "log")) {
		t.Errorf("symlinked dir as path: %+v", res)
	}
	f, _ := FindFiles(FindFilesRequest{Path: filepath.Join(root, "log"), NameGlob: []string{"*.log"}}, roots)
	if f.Count != 1 {
		t.Errorf("find in a symlinked dir: %+v", f)
	}
	res, _ = SearchContent(SearchContentRequest{Path: root, Pattern: "FATAL"}, roots)
	if res.TotalMatches != 1 || res.SkipReasons["symlink_dir"] < 1 || len(res.SymlinkDirs) == 0 {
		t.Errorf("walk over a symlinked dir: %+v", res)
	}
}

// find_files: time/size filters and sorting.
func TestFindFilesFilters(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"old.log": "x", "new.log": strings.Repeat("y", 100), "mid.log": "zz"})
	now := time.Now()
	os.Chtimes(filepath.Join(root, "old.log"), now.Add(-72*time.Hour), now.Add(-72*time.Hour))
	os.Chtimes(filepath.Join(root, "mid.log"), now.Add(-2*time.Hour), now.Add(-2*time.Hour))
	roots := []string{root}

	f, err := FindFiles(FindFilesRequest{Path: root, NewerThan: "1d"}, roots)
	if err != nil || f.Count != 2 {
		t.Errorf("newer_than=1d: %+v %v", f, err)
	}
	f, _ = FindFiles(FindFilesRequest{Path: root, OlderThan: "1h"}, roots)
	if f.Count != 2 {
		t.Errorf("older_than=1h: %+v", f)
	}
	f, _ = FindFiles(FindFilesRequest{Path: root, MinSize: 50}, roots)
	if f.Count != 1 || filepath.Base(f.Entries[0].Path) != "new.log" {
		t.Errorf("min_size: %+v", f)
	}
	f, _ = FindFiles(FindFilesRequest{Path: root, SortBy: "mtime"}, roots)
	if f.Count != 3 || filepath.Base(f.Entries[0].Path) != "new.log" || filepath.Base(f.Entries[2].Path) != "old.log" {
		t.Errorf("sort_by mtime: %+v", f.Entries)
	}
	f, _ = FindFiles(FindFilesRequest{Path: root, SortBy: "size", MaxResults: 1}, roots)
	if f.Count != 1 || !f.Truncated || filepath.Base(f.Entries[0].Path) != "new.log" {
		t.Errorf("sort_by size top 1: %+v", f)
	}
	if _, err := FindFiles(FindFilesRequest{Path: root, NewerThan: "yesterday"}, roots); err == nil {
		t.Error("invalid newer_than accepted")
	}
}

// Line numbers stay right across empty lines, CRLF endings and a final line
// without a newline.
func TestSearchLineNumbering(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "crlf.conf")
	os.WriteFile(p, []byte("a\r\n\r\nkey = 1\r\n\nkey = 2"), 0644)
	res, _ := SearchContent(SearchContentRequest{Path: p, Pattern: "^key"}, []string{root})
	if len(res.Matches) != 2 || res.Matches[0].Line != 3 || res.Matches[1].Line != 5 ||
		res.Matches[0].Content != "key = 1" {
		t.Errorf("line numbering: %+v", res.Matches)
	}
}
