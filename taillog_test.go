package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func writeLines(t *testing.T, path string, n int) {
	t.Helper()
	var sb strings.Builder
	for i := 1; i <= n; i++ {
		sb.WriteString("log line " + strconv.Itoa(i) + "\n")
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestTailLogBasic(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "app.log")
	writeLines(t, target, 100)

	res, err := TailLog(TailLogRequest{Path: target, Lines: 5}, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(res.Content, "\n")
	if len(lines) != 5 || lines[4] != "log line 100" || lines[0] != "log line 96" {
		t.Errorf("tail: %q", res.Content)
	}
	if res.EndOffset != res.Size {
		t.Errorf("end_offset=%d size=%d", res.EndOffset, res.Size)
	}
	if res.Truncated {
		t.Error("unexpected truncated")
	}
}

func TestTailLogSinceOffsetCursor(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "app.log")
	writeLines(t, target, 10)

	first, err := TailLog(TailLogRequest{Path: target, Lines: 3}, []string{root})
	if err != nil {
		t.Fatal(err)
	}

	// Append more lines, then poll from the byte cursor.
	f, _ := os.OpenFile(target, os.O_APPEND|os.O_WRONLY, 0644)
	f.WriteString("log line 11\nlog line 12\n")
	f.Close()

	second, err := TailLog(TailLogRequest{Path: target, SinceOffset: first.EndOffset}, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	if second.Content != "log line 11\nlog line 12" {
		t.Errorf("since_offset content: %q", second.Content)
	}
	if second.StartOffset != first.EndOffset {
		t.Errorf("start_offset=%d, want %d", second.StartOffset, first.EndOffset)
	}

	// No new content: empty result, no error.
	third, err := TailLog(TailLogRequest{Path: target, SinceOffset: second.EndOffset}, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	if third.Content != "" {
		t.Errorf("expected empty content, got %q", third.Content)
	}
}

func TestTailLogRotation(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "app.log")
	writeLines(t, target, 50)

	st, _ := os.Stat(target)
	oldSize := st.Size()

	// Simulate rotation: file recreated smaller.
	writeLines(t, target, 5)

	res, err := TailLog(TailLogRequest{Path: target, SinceOffset: oldSize}, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Rotated {
		t.Errorf("rotation not detected: %+v", res)
	}
	if !strings.Contains(res.Content, "log line 5") {
		t.Errorf("post-rotation content: %q", res.Content)
	}
}

func TestTailLogSinceLineAndFilter(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "app.log")
	os.WriteFile(target, []byte("INFO a\nFATAL b\nINFO c\nFATAL d\n"), 0644)

	res, err := TailLog(TailLogRequest{Path: target, SinceLine: 2}, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	if res.StartLine != 3 || res.EndLine != 4 || res.TotalLines != 4 {
		t.Errorf("cursor: %+v", res)
	}
	if res.Content != "INFO c\nFATAL d" {
		t.Errorf("since_line content: %q", res.Content)
	}

	// Filter keeps only FATAL lines, but the cursor still advances past INFO.
	res, err = TailLog(TailLogRequest{Path: target, SinceLine: 0, Lines: 100, FilterRegex: "FATAL"}, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "FATAL b\nFATAL d" {
		t.Errorf("filter content: %q", res.Content)
	}
	if !res.Filtered {
		t.Error("filtered flag missing")
	}

	if _, err := TailLog(TailLogRequest{Path: target, FilterRegex: "["}, []string{root}); err == nil {
		t.Error("expected invalid filter_regex error")
	}
}

func TestTailLogFollowTimeout(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "app.log")
	writeLines(t, target, 5)

	start := time.Now()
	res, err := TailLog(TailLogRequest{Path: target, SinceLine: 5, FollowSeconds: 1}, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Followed || !res.TimedOut {
		t.Errorf("follow flags: %+v", res)
	}
	if time.Since(start) < 900*time.Millisecond {
		t.Errorf("follow returned too early: %v", time.Since(start))
	}
}

func TestTailLogFollowReceivesNewContent(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "app.log")
	writeLines(t, target, 5)

	go func() {
		time.Sleep(300 * time.Millisecond)
		f, _ := os.OpenFile(target, os.O_APPEND|os.O_WRONLY, 0644)
		f.WriteString("log line 6\n")
		f.Close()
	}()

	res, err := TailLog(TailLogRequest{Path: target, SinceLine: 5, FollowSeconds: 5}, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	if res.TimedOut {
		t.Error("follow timed out despite new content")
	}
	if res.Content != "log line 6" {
		t.Errorf("follow content: %q", res.Content)
	}
}

func TestTailLogBinary(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "bin.log")
	os.WriteFile(target, []byte{0x00, 0x01, 0x02, 0x03, '\n'}, 0644)

	res, err := TailLog(TailLogRequest{Path: target, Lines: 5}, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsBinary {
		t.Errorf("binary not detected: %+v", res)
	}
}

// With filter_regex, tail mode returns the last N MATCHING lines (grep X |
// tail -N), not the matches among the last N lines.
func TestTailLogFilterLastMatches(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "app.log")
	var sb strings.Builder
	for i := 1; i <= 20000; i++ {
		if i%50 == 0 {
			sb.WriteString("ERROR #" + strconv.Itoa(i) + " 扣费失败\n")
		} else {
			sb.WriteString("INFO " + strconv.Itoa(i) + " 计费请求处理完成 " + strings.Repeat("x", 40) + "\n")
		}
	}
	os.WriteFile(target, []byte(sb.String()), 0644)

	res, err := TailLog(TailLogRequest{Path: target, Lines: 10, FilterRegex: "ERROR"}, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(res.Content, "\n")
	if len(lines) != 10 || lines[0] != "ERROR #19550 扣费失败" || lines[9] != "ERROR #20000 扣费失败" {
		t.Errorf("last 10 matches: %q", res.Content)
	}
	if !res.Filtered || res.Truncated || res.EndOffset != res.Size || res.Encoding != "utf-8" {
		t.Errorf("flags: %+v", res)
	}
	// start_offset points at the first returned line.
	if got := sb.String()[res.StartOffset:]; !strings.HasPrefix(got, "ERROR #19550") {
		t.Errorf("start_offset=%d points at %q", res.StartOffset, got[:20])
	}

	// Fewer matches than asked for: all of them, not truncated.
	res, _ = TailLog(TailLogRequest{Path: target, Lines: 1000, FilterRegex: "#1000 "}, []string{root})
	if res.Content != "ERROR #1000 扣费失败" || res.Truncated {
		t.Errorf("single match: %q truncated=%v", res.Content, res.Truncated)
	}

	// A GBK log is decoded line by line.
	gbk, _ := encodeFromUTF8("INFO 正常\nERROR 扣费失败\nINFO 正常\n", "gbk")
	os.WriteFile(target, gbk, 0644)
	res, _ = TailLog(TailLogRequest{Path: target, Lines: 5, FilterRegex: "扣费"}, []string{root})
	if res.Content != "ERROR 扣费失败" || res.Encoding != "gbk" {
		t.Errorf("gbk: %q enc=%s", res.Content, res.Encoding)
	}

	// A last line without a final newline still counts.
	os.WriteFile(target, []byte("ERROR a\nINFO b\nERROR c"), 0644)
	res, _ = TailLog(TailLogRequest{Path: target, Lines: 5, FilterRegex: "ERROR"}, []string{root})
	if res.Content != "ERROR a\nERROR c" {
		t.Errorf("unterminated last line: %q", res.Content)
	}
}

// A filter written with GNU grep's \| matches nothing in RE2: say so.
func TestTailLogGNUAlternationHint(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "app.log")
	os.WriteFile(target, []byte("INFO a\nFATAL b\n"), 0644)
	res, err := TailLog(TailLogRequest{Path: target, Lines: 10, FilterRegex: `FATAL\|ERROR`}, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "" || !strings.Contains(res.Hint, "bare |") {
		t.Errorf("GNU alternation hint missing: %+v", res)
	}
}

// A since_line read cut by max_bytes must hand back a cursor that resumes
// after the returned lines, not after the whole file.
func TestTailLogSinceLineTruncatedCursor(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "app.log")
	writeLines(t, target, 1000)
	res, err := TailLog(TailLogRequest{Path: target, SinceLine: 1, MaxBytes: 200}, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	n := len(strings.Split(res.Content, "\n"))
	if !res.Truncated || res.EndLine != 1+n || res.TotalLines != 1000 || !strings.Contains(res.Hint, "since_line=") {
		t.Fatalf("truncated cursor: lines=%d %+v", n, res)
	}
	next, _ := TailLog(TailLogRequest{Path: target, SinceLine: res.EndLine, MaxBytes: 200}, []string{root})
	if !strings.HasPrefix(next.Content, "log line "+strconv.Itoa(res.EndLine+1)+"\n") {
		t.Errorf("resume: %q", next.Content[:30])
	}
}

// Mixed encodings: each line of the tail is decoded on its own.
func TestTailLogMixedEncodingLines(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "app.log")
	gbk, _ := encodeFromUTF8("ERROR 扣费失败", "gbk")
	os.WriteFile(target, append([]byte("INFO 计费请求处理完成\n"), append(gbk, '\n')...), 0644)
	res, err := TailLog(TailLogRequest{Path: target, Lines: 5}, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "INFO 计费请求处理完成\nERROR 扣费失败" {
		t.Errorf("mixed lines: %q", res.Content)
	}
}
