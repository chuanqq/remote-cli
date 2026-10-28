package main

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// A byte window cut inside a character, or a stray invalid byte, must not
// flip UTF-8 text to GBK (the GBK decoder never fails, so it used to win).
func TestDetectEncodingWindows(t *testing.T) {
	utf := []byte(strings.Repeat("计费策略配置说明\n", 100))
	for _, cut := range []int{1000, 1001, 1002} {
		if got := detectEncoding(utf[:cut]); got != "utf-8" {
			t.Errorf("utf-8 cut at %d detected as %s", cut, got)
		}
	}
	if got := detectEncoding(utf[1:500]); got != "utf-8" {
		t.Errorf("utf-8 window starting mid-character detected as %s", got)
	}
	stray := append([]byte("# 预算配置\nbudget = 100\n"), 0xff, '\n')
	if got := detectEncoding(stray); got != "utf-8" {
		t.Errorf("utf-8 with one stray byte detected as %s", got)
	}
	gbk, _ := encodeFromUTF8(strings.Repeat("计费策略配置说明\n", 100), "gbk")
	if got := detectEncoding(gbk[:1001]); got != "gbk" {
		t.Errorf("gbk cut mid-character detected as %s", got)
	}
	if got := detectEncoding([]byte("plain ascii\n")); got != "utf-8" {
		t.Errorf("ascii detected as %s", got)
	}
	rnd := rand.New(rand.NewSource(1))
	for i := 0; i < 50; i++ {
		b := make([]byte, 512)
		rnd.Read(b)
		if got := detectEncoding(b); got == "gbk" {
			t.Fatalf("random bytes detected as gbk")
		}
	}
}

func TestWindowEdges(t *testing.T) {
	s := []byte("计费") // 2 x 3 bytes
	if st, en := windowEdges(s[1:], "utf-8", true, false); st != 2 || en != 5 {
		t.Errorf("utf-8 head: %d,%d", st, en)
	}
	if st, en := windowEdges(s[:4], "utf-8", false, true); st != 0 || en != 3 {
		t.Errorf("utf-8 tail: %d,%d", st, en)
	}
	g, _ := encodeFromUTF8("计费", "gbk") // 2 x 2 bytes
	if _, en := windowEdges(g[:3], "gbk", false, true); en != 2 {
		t.Errorf("gbk tail: end=%d, want 2", en)
	}
}

// Paging with next_offset must return every byte exactly once and decode
// every page correctly, for UTF-8 and GBK alike.
func TestReadFilePagingRoundTrip(t *testing.T) {
	root := t.TempDir()
	var sb strings.Builder
	for i := 0; i < 300; i++ {
		fmt.Fprintf(&sb, "第%d行 计费请求处理完成 用户=%d 状态=成功\n", i, 84468720+i)
	}
	text := sb.String()
	for _, enc := range []string{"utf-8", "gbk"} {
		data, _ := encodeFromUTF8(text, enc)
		p := filepath.Join(root, enc+".log")
		os.WriteFile(p, data, 0o644)
		var got strings.Builder
		var off int64
		for pages := 0; ; pages++ {
			if pages > 1000 {
				t.Fatalf("%s: paging does not advance", enc)
			}
			r, err := ReadFileContent(ReadFileRequest{Path: p, OffsetBytes: off, MaxBytes: 1001}, []string{root})
			if err != nil {
				t.Fatal(err)
			}
			if r.Encoding != enc {
				t.Fatalf("%s: page at %d detected as %s", enc, off, r.Encoding)
			}
			got.WriteString(r.Content)
			if !r.Truncated {
				break
			}
			if r.NextOffset <= off {
				t.Fatalf("%s: next_offset %d does not advance from %d", enc, r.NextOffset, off)
			}
			off = r.NextOffset
		}
		if got.String() != text {
			t.Errorf("%s: pages do not reassemble the file", enc)
		}
	}
}

// Line windows stream: any line of a file larger than max_bytes is readable,
// total_lines counts a final newline once, and next_start_line pages on.
func TestReadFileLineWindows(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "big.log")
	var sb strings.Builder
	for i := 1; i <= 50000; i++ {
		fmt.Fprintf(&sb, "line %06d some log payload here\n", i)
	}
	os.WriteFile(p, []byte(sb.String()), 0o644)
	roots := []string{root}

	r, err := ReadFileContent(ReadFileRequest{Path: p, StartLine: 40000, EndLine: 40002}, roots)
	if err != nil {
		t.Fatal(err)
	}
	want := "line 040000 some log payload here\nline 040001 some log payload here\nline 040002 some log payload here"
	if r.Content != want || r.StartLine != 40000 || r.ReturnedLines != 3 || r.TotalLines != 50000 {
		t.Errorf("deep line range: %+v", r)
	}
	if r.Truncated || r.NextStartLine != 40003 {
		t.Errorf("deep line range cursors: truncated=%v next_start_line=%d", r.Truncated, r.NextStartLine)
	}

	// max_bytes stops a line window on a line boundary.
	r, _ = ReadFileContent(ReadFileRequest{Path: p, StartLine: 10, MaxBytes: 100}, roots)
	if r.ReturnedLines != 2 || !r.Truncated || r.NextStartLine != 12 || r.Hint == "" {
		t.Errorf("capped line window: %+v", r)
	}

	// Past the end: empty, not truncated, with a hint.
	r, _ = ReadFileContent(ReadFileRequest{Path: p, StartLine: 60000}, roots)
	if r.Content != "" || r.Truncated || !strings.Contains(r.Hint, "past the end") {
		t.Errorf("past end: %+v", r)
	}

	three := filepath.Join(root, "three.txt")
	os.WriteFile(three, []byte("a\nb\nc\n"), 0o644)
	r, _ = ReadFileContent(ReadFileRequest{Path: three, StartLine: 1}, roots)
	if r.TotalLines != 3 || r.ReturnedLines != 3 || r.Content != "a\nb\nc" {
		t.Errorf("final newline counted as a line: %+v", r)
	}

	if _, err := ReadFileContent(ReadFileRequest{Path: three, StartLine: 3, EndLine: 2}, roots); err == nil {
		t.Error("end_line before start_line accepted")
	}
}

// A single line longer than max_bytes comes back cut, with both cursors.
func TestReadFileLongLine(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "min.json")
	os.WriteFile(p, []byte("head\n"+strings.Repeat("x", 5000)+"\ntail\n"), 0o644)
	r, err := ReadFileContent(ReadFileRequest{Path: p, StartLine: 2, MaxBytes: 1000}, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Content) != 1000 || !r.Truncated || r.NextStartLine != 3 || r.NextOffset != 5+1000 {
		t.Errorf("long line: len=%d %+v", len(r.Content), r)
	}
}

// line_numbers renders cat -n style from the right line number, in every mode.
func TestReadFileLineNumbers(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "a.conf")
	os.WriteFile(p, []byte("one\ntwo\nthree\nfour\n"), 0o644)
	roots := []string{root}

	r, _ := ReadFileContent(ReadFileRequest{Path: p, StartLine: 2, EndLine: 3, LineNumbers: true}, roots)
	if r.Content != "     2\ttwo\n     3\tthree" {
		t.Errorf("line range numbering: %q", r.Content)
	}
	r, _ = ReadFileContent(ReadFileRequest{Path: p, TailLines: 2, LineNumbers: true}, roots)
	if r.Content != "     3\tthree\n     4\tfour" || r.StartLine != 3 {
		t.Errorf("tail numbering: %q start=%d", r.Content, r.StartLine)
	}
	r, _ = ReadFileContent(ReadFileRequest{Path: p, OffsetBytes: 4, LineNumbers: true}, roots)
	if r.Content != "     2\ttwo\n     3\tthree\n     4\tfour" || r.StartLine != 2 {
		t.Errorf("byte window numbering: %q start=%d", r.Content, r.StartLine)
	}
}

// Claude Code keeps only the first 100,000 characters of an oversized result:
// the cursors must come before the content in the JSON.
func TestReadFileResultFieldOrder(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "big.conf")
	os.WriteFile(p, []byte(strings.Repeat("key_name : some_value_here\n", 12000)), 0o644)
	r, err := ReadFileContent(ReadFileRequest{Path: p, MaxBytes: 200000}, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(r)
	head := string(out[:min(len(out), 100000)])
	for _, k := range []string{`"truncated":true`, `"total_size"`, `"next_offset"`, `"hint"`} {
		if !strings.Contains(head, k) {
			t.Errorf("%s lost in a 100k-character cut", k)
		}
	}
	// The default window stays well under the client cut.
	r, _ = ReadFileContent(ReadFileRequest{Path: p}, []string{root})
	if out, _ = json.Marshal(r); len(out) > 80000 || !r.Truncated || r.NextStartLine == 0 {
		t.Errorf("default window: %d bytes, truncated=%v next_start_line=%d", len(out), r.Truncated, r.NextStartLine)
	}
}

// A GBK byte window that ends right after a newline plus half a character
// keeps the whole characters after the newline (not just up to it).
func TestWindowEdgesGBKTail(t *testing.T) {
	g, _ := encodeFromUTF8("计费\n预算", "gbk") // 4 + 1 + 4 bytes
	if _, en := windowEdges(g[:8], "gbk", false, true); en != 7 {
		t.Errorf("gbk tail after newline: end=%d, want 7", en)
	}
	if _, en := windowEdges(g, "gbk", false, true); en != len(g) {
		t.Errorf("gbk whole window: end=%d, want %d", en, len(g))
	}
}

// A window smaller than one character still advances the next_offset cursor.
func TestReadFileTinyWindowAdvances(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "cjk.txt")
	os.WriteFile(p, []byte("计费策略"), 0o644)
	var off int64
	for i := 0; i < 20; i++ {
		r, err := ReadFileContent(ReadFileRequest{Path: p, OffsetBytes: off, MaxBytes: 2}, []string{root})
		if err != nil {
			t.Fatal(err)
		}
		if !r.Truncated {
			return
		}
		if r.NextOffset <= off {
			t.Fatalf("max_bytes=2: next_offset %d does not advance from %d", r.NextOffset, off)
		}
		off = r.NextOffset
	}
	t.Fatal("paging with max_bytes=2 did not reach the end")
}

// lines=[...] returns just those lines, ascending and numbered, decoding each
// line on its own; numbers past the end are reported, not an error.
func TestReadFilePickedLines(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "charge_model.cpp")
	var sb strings.Builder
	for i := 1; i <= 2000; i++ {
		fmt.Fprintf(&sb, "line %d\n", i)
	}
	os.WriteFile(p, []byte(sb.String()), 0o644)
	roots := []string{root}

	r, err := ReadFileContent(ReadFileRequest{Path: p, Lines: []int{1658, 604, 620, 604}}, roots)
	if err != nil {
		t.Fatal(err)
	}
	want := "   604\tline 604\n   620\tline 620\n  1658\tline 1658"
	if r.Content != want || r.ReturnedLines != 3 || r.StartLine != 604 || r.Truncated || r.MissingLines != nil {
		t.Errorf("picked lines: %q %+v", r.Content, r)
	}
	if r.TotalLines != 2000 {
		t.Errorf("total_lines=%d, want 2000", r.TotalLines)
	}

	r, _ = ReadFileContent(ReadFileRequest{Path: p, Lines: []int{1999, 2000, 2001, 5000}}, roots)
	if r.ReturnedLines != 2 || len(r.MissingLines) != 2 || r.MissingLines[0] != 2001 || !strings.Contains(r.Hint, "past the end") {
		t.Errorf("past the end: %+v", r)
	}

	// The byte cap stops between lines and says where.
	r, _ = ReadFileContent(ReadFileRequest{Path: p, Lines: []int{10, 11, 12, 13}, MaxBytes: 40}, roots)
	if !r.Truncated || r.ReturnedLines != 2 || r.NextStartLine != 12 {
		t.Errorf("capped: %+v", r)
	}

	// A GBK line inside an otherwise UTF-8 file.
	mixed := filepath.Join(root, "mixed.cpp")
	gbk, _ := encodeFromUTF8("// 扣费失败", "gbk")
	os.WriteFile(mixed, append([]byte("// 计费入口\nint a;\n"), append(gbk, '\n')...), 0o644)
	r, _ = ReadFileContent(ReadFileRequest{Path: mixed, Lines: []int{1, 3}}, roots)
	if r.Content != "     1\t// 计费入口\n     3\t// 扣费失败" {
		t.Errorf("per-line decoding: %q", r.Content)
	}

	if _, err := ReadFileContent(ReadFileRequest{Path: p, Lines: []int{0, 3}}, roots); err == nil {
		t.Error("line number 0 accepted")
	}
}

// An empty line keeps its number in every numbered mode.
func TestReadFileNumberedEmptyLine(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "gaps.conf")
	os.WriteFile(p, []byte("a\n\nc\n"), 0o644)
	r, _ := ReadFileContent(ReadFileRequest{Path: p, StartLine: 2, EndLine: 2, LineNumbers: true}, []string{root})
	if r.Content != "     2\t" || r.ReturnedLines != 1 {
		t.Errorf("empty line numbering: %q %+v", r.Content, r)
	}
	r, _ = ReadFileContent(ReadFileRequest{Path: p, Lines: []int{2}}, []string{root})
	if r.Content != "     2\t" {
		t.Errorf("picked empty line: %q", r.Content)
	}
}

// remote_read_files: request order kept, small files whole, large ones cut
// with their cursors, a missing file reported in place, globs expanded inside
// the roots only.
func TestReadFiles(t *testing.T) {
	root := t.TempDir()
	roots := []string{root}
	os.MkdirAll(filepath.Join(root, "conf"), 0o755)
	os.WriteFile(filepath.Join(root, "conf", "a.conf"), []byte("port : 8080\n"), 0o644)
	os.WriteFile(filepath.Join(root, "conf", "b.conf"), []byte("port : 9090\n"), 0o644)
	os.WriteFile(filepath.Join(root, "conf", ".hidden.conf"), []byte("x\n"), 0o644)
	var sb strings.Builder
	for i := 1; i <= 20000; i++ {
		fmt.Fprintf(&sb, "log line %d\n", i)
	}
	os.WriteFile(filepath.Join(root, "big.log"), []byte(sb.String()), 0o644)

	res, err := ReadFiles(ReadFilesRequest{Files: []ReadFileRequest{
		{Path: filepath.Join(root, "big.log")},
		{Path: filepath.Join(root, "missing.conf")},
		{Path: filepath.Join(root, "conf", "*.conf")},
		{Path: filepath.Join(root, "big.log"), StartLine: 19999},
	}, MaxBytes: 8000}, roots)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range res.Files {
		names = append(names, filepath.Base(f.Path))
	}
	if strings.Join(names, ",") != "big.log,missing.conf,a.conf,b.conf,big.log" {
		t.Fatalf("order / glob expansion: %v", names)
	}
	if res.FilesRead != 4 || res.FilesFailed != 1 || res.Files[1].Error == "" || res.Files[1].ReadFileResult != nil {
		t.Errorf("counts / error item: %+v", res)
	}
	if res.Files[2].Content != "port : 8080\n" || res.Files[2].Truncated {
		t.Errorf("small file not whole: %+v", res.Files[2].ReadFileResult)
	}
	if res.Files[4].Content != "log line 19999\nlog line 20000" {
		t.Errorf("windowed file: %q", res.Files[4].Content)
	}
	if big := res.Files[0]; !big.Truncated || big.NextOffset == 0 || len(big.Content) > 8000 {
		t.Errorf("large file: truncated=%v next_offset=%d len=%d", big.Truncated, big.NextOffset, len(big.Content))
	}
	if !res.Truncated || !strings.Contains(res.Hint, "cut") {
		t.Errorf("truncation not reported: %+v", res)
	}
	total := 0
	for _, f := range res.Files {
		if f.ReadFileResult != nil {
			total += len(f.Content)
		}
	}
	if total > 8000 {
		t.Errorf("shared budget exceeded: %d bytes", total)
	}

	// A glob may not list outside the roots.
	res, err = ReadFiles(ReadFilesRequest{Files: []ReadFileRequest{{Path: filepath.Join(filepath.Dir(root), "*")}}}, roots)
	if err != nil {
		t.Fatal(err)
	}
	if res.FilesFailed != 1 || !strings.Contains(res.Files[0].Error, "escapes") {
		t.Errorf("glob outside the roots: %+v", res)
	}

	// line_numbers applies to every file.
	res, _ = ReadFiles(ReadFilesRequest{Files: []ReadFileRequest{{Path: filepath.Join(root, "conf", "a.conf")}}, LineNumbers: true}, roots)
	if res.Files[0].Content != "     1\tport : 8080" {
		t.Errorf("line_numbers: %q", res.Files[0].Content)
	}
}

// Many files: the budget runs out and the rest are listed in not_read.
func TestReadFilesBudgetExhausted(t *testing.T) {
	root := t.TempDir()
	var reqs []ReadFileRequest
	for i := 0; i < 30; i++ {
		p := filepath.Join(root, fmt.Sprintf("f%02d.txt", i))
		os.WriteFile(p, []byte(strings.Repeat("x", 2000)+"\n"), 0o644)
		reqs = append(reqs, ReadFileRequest{Path: p})
	}
	res, err := ReadFiles(ReadFilesRequest{Files: reqs, MaxBytes: 20000}, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	if res.FilesRead+len(res.NotRead) != 30 || len(res.NotRead) == 0 || !res.Truncated {
		t.Errorf("read=%d not_read=%d truncated=%v", res.FilesRead, len(res.NotRead), res.Truncated)
	}
}

// decodeLine: a GBK line inside a UTF-8 file, a UTF-8 line inside a GBK file,
// and a short GBK line that happens to be valid UTF-8.
func TestDecodeLineMixed(t *testing.T) {
	gbk, _ := encodeFromUTF8("扣费失败", "gbk")
	if got := decodeLine(gbk, "utf-8", ""); got != "扣费失败" {
		t.Errorf("gbk line in a utf-8 file: %q", got)
	}
	if got := decodeLine([]byte("计费请求处理完成"), "gbk", ""); got != "计费请求处理完成" {
		t.Errorf("utf-8 line in a gbk file: %q", got)
	}
	short, _ := encodeFromUTF8("预", "gbk") // D4 A4: also valid UTF-8 ("Ԥ")
	if got := decodeLine(short, "gbk", ""); got != "预" {
		t.Errorf("short gbk line: %q", got)
	}
	if got := decodeLine([]byte("plain"), "gbk", ""); got != "plain" {
		t.Errorf("ascii: %q", got)
	}
	text, _ := decodeText(append(append([]byte("// 计费\r\n"), gbk...), '\n'), "utf-8", "")
	if text != "// 计费\r\n扣费失败\n" {
		t.Errorf("decodeText keeps line endings: %q", text)
	}
}

// Paging through lines longer than max_bytes must not lose bytes: a window
// that starts mid-line at a next_offset is on a character boundary and must
// not be re-aligned (GBK cannot detect a cut head, so it must not try).
func TestReadFilePagingLongLines(t *testing.T) {
	root := t.TempDir()
	text := "短行\n" + strings.Repeat("计费策略配置说明", 300) + "\n尾行一\n" + strings.Repeat("预算", 700) + "\nend\n"
	for _, enc := range []string{"utf-8", "gbk", "gb18030"} {
		data, _ := encodeFromUTF8(text, enc)
		p := filepath.Join(root, enc+"-long.log")
		os.WriteFile(p, data, 0o644)
		// Many window sizes, so that some page starts mid-line shortly
		// before that line ends.
		for mb := 64; mb <= 1500; mb += 37 {
			var got strings.Builder
			var off int64
			for pages := 0; ; pages++ {
				if pages > 5000 {
					t.Fatalf("%s max_bytes=%d: paging does not advance", enc, mb)
				}
				r, err := ReadFileContent(ReadFileRequest{Path: p, OffsetBytes: off, MaxBytes: mb, Encoding: enc}, []string{root})
				if err != nil {
					t.Fatal(err)
				}
				got.WriteString(r.Content)
				if !r.Truncated {
					break
				}
				off = r.NextOffset
			}
			if got.String() != text {
				t.Errorf("%s max_bytes=%d: pages do not reassemble the file (%d of %d bytes)", enc, mb, got.Len(), len(text))
			}
			// truncate_mode=tail starts mid-line: whole characters only.
			r, _ := ReadFileContent(ReadFileRequest{Path: p, MaxBytes: mb, TruncateMode: "tail", Encoding: enc}, []string{root})
			if !strings.HasSuffix(text, r.Content) || strings.ContainsRune(r.Content, utf8.RuneError) {
				t.Errorf("%s max_bytes=%d truncate_mode=tail: %q", enc, mb, firstRunes(r.Content, 12))
			}
		}
	}
}

func firstRunes(s string, n int) string {
	rs := []rune(s)
	return string(rs[:min(n, len(rs))])
}
