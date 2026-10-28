package main

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

// ---------------------------------------------------------------------------
// format=text for the reading tools
//
// Claude Code hands the model JSON.stringify(structuredContent) whenever a
// result has structured content, and ignores its text then. File content in a
// JSON string pays for it: every newline, tab and quote is escaped, and
// per-match objects repeat the file path and field names (clients that show
// the text fallback instead also get Go's \u003c \u003e \u0026 for < > &).
// Measured end to end: 100 search matches with context come out at 57% of
// the JSON size as text, a filtered tail at 76%, a 160-line window at 97%.
// With format=text a tool returns plain text only — a short header with the
// facts and cursors, then the content exactly as `cat -n` / `grep -n` print it.
// ---------------------------------------------------------------------------

// textFormat reports whether the call asked for format=text.
func textFormat(req mcp.CallToolRequest) bool {
	return strings.EqualFold(strings.TrimSpace(req.GetString("format", "")), "text")
}

// formatOption is the shared format parameter of the reading tools.
func formatOption() mcp.ToolOption {
	return mcp.WithString("format", mcp.Description("Result format: json (default, structured) or text: "+
		"a short header with the facts and cursors, then the content as plain text (cat -n / grep -n style). "+
		"text is more compact and easier to read; the cursors are the same."), mcp.Enum("json", "text"))
}

// textResult is a text-only tool result: no structured content, so clients
// show the model the text itself.
func textResult(text string) *mcp.CallToolResult {
	return mcp.NewToolResultText(text)
}

// header joins non-empty key=value facts into one bracketed line.
func header(facts ...string) string {
	var kept []string
	for _, f := range facts {
		if f != "" {
			kept = append(kept, f)
		}
	}
	return "[" + strings.Join(kept, " ") + "]"
}

func factIf(cond bool, format string, args ...any) string {
	if !cond {
		return ""
	}
	return fmt.Sprintf(format, args...)
}

// readFileText renders a remote_read_file result.
func readFileText(r *ReadFileResult) string {
	var b strings.Builder
	b.WriteString(r.Path + " " + readFileFacts(r) + "\n")
	if r.Hint != "" {
		b.WriteString("hint: " + r.Hint + "\n")
	}
	if r.Content != "" {
		b.WriteString(r.Content)
		if !strings.HasSuffix(r.Content, "\n") {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// readFileFacts is the header of one remote_read_file / read_files result.
func readFileFacts(r *ReadFileResult) string {
	if r.IsBinary {
		return header(fmt.Sprintf("binary file, %d bytes: use remote_download_base64", r.TotalSize))
	}
	lines := ""
	switch {
	case r.ReturnedLines == 0:
		lines = "no lines"
	case r.mode == readPicked:
		lines = fmt.Sprintf("%d requested lines", r.ReturnedLines)
	case r.StartLine > 0:
		lines = fmt.Sprintf("lines %d-%d", r.StartLine, r.StartLine+r.ReturnedLines-1)
	default:
		lines = fmt.Sprintf("%d lines", r.ReturnedLines)
	}
	if r.TotalLines > 0 {
		lines += fmt.Sprintf(" of %d", r.TotalLines)
	}
	span := fmt.Sprintf("bytes %d-%d of %d", r.Offset, r.end, r.TotalSize)
	if r.mode == readPicked {
		span = fmt.Sprintf("size=%d", r.TotalSize) // the picked lines are not one byte range
	}
	return header(
		lines,
		span,
		factIf(r.Encoding != "" && r.Encoding != "utf-8", "encoding=%s", r.Encoding),
		factIf(r.Truncated, "truncated"),
		factIf(r.NextStartLine > 0, "next_start_line=%d", r.NextStartLine),
		factIf(r.NextOffset > 0, "next_offset=%d", r.NextOffset),
		factIf(len(r.MissingLines) > 0, "missing_lines=%v", r.MissingLines),
	)
}

// readFilesText renders a remote_read_files result: one section per file.
func readFilesText(r *ReadFilesResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", header(fmt.Sprintf("%d files read", r.FilesRead),
		factIf(r.FilesFailed > 0, "%d failed", r.FilesFailed), factIf(r.Truncated, "truncated")))
	if r.Hint != "" {
		b.WriteString("hint: " + r.Hint + "\n")
	}
	if len(r.NotRead) > 0 {
		b.WriteString("not_read: " + strings.Join(r.NotRead, " ") + "\n")
	}
	for _, f := range r.Files {
		b.WriteString("\n==> " + f.Path + " ")
		if f.Error != "" {
			b.WriteString(header("error: "+f.Error) + "\n")
			continue
		}
		b.WriteString(readFileFacts(f.ReadFileResult) + " <==\n")
		if f.Content != "" {
			b.WriteString(f.Content)
			if !strings.HasSuffix(f.Content, "\n") {
				b.WriteByte('\n')
			}
		}
	}
	return b.String()
}

// searchText renders a remote_search_content result like `grep -n`: matches
// grouped under their file, `N:line` for a match, `N-line` for context, and
// `--` between non-adjacent groups (grep -C style).
func searchText(r *SearchContentResult) string {
	var b strings.Builder
	facts := []string{
		fmt.Sprintf("%d matching files", r.FilesMatched),
		fmt.Sprintf("%d files searched", r.FilesSearched),
		factIf(r.OutputMode != SearchFilesMode, "%d matches", r.TotalMatches),
		factIf(r.FilesSkipped > 0, "%d skipped %v", r.FilesSkipped, r.SkipReasons),
		factIf(r.Truncated, "truncated"),
		factIf(r.NextOffset > 0, "next_offset=%d", r.NextOffset),
	}
	b.WriteString(r.Pattern + " in " + r.Path + " " + header(facts...) + "\n")
	if r.Hint != "" {
		b.WriteString("hint: " + r.Hint + "\n")
	}
	if len(r.SymlinkDirs) > 0 {
		b.WriteString("symlink_dirs (not entered): " + strings.Join(r.SymlinkDirs, " ") + "\n")
	}
	for _, f := range r.Files {
		if r.OutputMode == SearchCountMode {
			fmt.Fprintf(&b, "%s:%d\n", f.File, f.Count)
		} else {
			b.WriteString(f.File + "\n")
		}
	}
	file, last := "", 0
	for _, m := range r.Matches {
		first := m.Line - len(m.Before)
		if m.File != file {
			b.WriteString("\n" + m.File + "\n")
			file = m.File
		} else if r.context && first > last+1 {
			b.WriteString("--\n")
		}
		for i, l := range m.Before {
			fmt.Fprintf(&b, "%d-%s\n", first+i, l)
		}
		fmt.Fprintf(&b, "%d:%s\n", m.Line, m.Content)
		for i, l := range m.After {
			fmt.Fprintf(&b, "%d-%s\n", m.Line+1+i, l)
		}
		last = m.Line + len(m.After)
	}
	return b.String()
}

// tailLogText renders a remote_tail_log result.
func tailLogText(r *TailLogResult) string {
	var b strings.Builder
	cursor := fmt.Sprintf("end_offset=%d", r.EndOffset)
	if r.cursor == "line" {
		cursor = fmt.Sprintf("end_line=%d", r.EndLine)
	}
	b.WriteString(r.Path + " " + header(
		fmt.Sprintf("size=%d", r.Size),
		cursor,
		factIf(r.Encoding != "" && r.Encoding != "utf-8", "encoding=%s", r.Encoding),
		factIf(r.Filtered, "filtered"),
		factIf(r.Truncated, "truncated"),
		factIf(r.Rotated, "rotated"),
		factIf(r.Followed && r.TimedOut, "follow timed out: no new content"),
	) + "\n")
	if r.Hint != "" {
		b.WriteString("hint: " + r.Hint + "\n")
	}
	if r.Content != "" {
		b.WriteString(r.Content + "\n")
	}
	return b.String()
}

// diffText renders a remote_diff result: the unified diff of two files, or
// the change lists of two trees (diff -rq style) followed by any diffs.
func diffText(r *DiffResult) string {
	var b strings.Builder
	facts := []string{factIf(r.Identical, "identical"), factIf(r.Truncated, "truncated"),
		factIf(r.Encoding != "", "encoding=%s", r.Encoding)}
	if r.Mode == "file" {
		facts = append(facts, factIf(!r.Identical, "+%d -%d lines", r.LinesAdded, r.LinesRemoved))
	} else {
		facts = append(facts, fmt.Sprintf("%d added, %d removed, %d changed, %d same",
			len(r.Added), len(r.Removed), len(r.Changed), r.SameCount))
	}
	fmt.Fprintf(&b, "--- %s\n+++ %s\n%s\n", r.Left, r.Right, header(facts...))
	if r.Hint != "" {
		b.WriteString("hint: " + r.Hint + "\n")
	}
	if r.Mode == "file" {
		b.WriteString(r.Diff)
		return b.String()
	}
	for _, x := range r.Added {
		b.WriteString("only in right: " + x + "\n")
	}
	for _, x := range r.Removed {
		b.WriteString("only in left:  " + x + "\n")
	}
	for _, x := range r.Changed {
		b.WriteString("changed:       " + x + "\n")
	}
	for _, x := range r.TypeChanged {
		b.WriteString("type changed:  " + x + "\n")
	}
	for _, d := range r.Diffs {
		fmt.Fprintf(&b, "\ndiff %s %s\n%s", filepath.Join(r.Left, d.Path), filepath.Join(r.Right, d.Path), d.Diff)
		if !strings.HasSuffix(d.Diff, "\n") {
			b.WriteByte('\n')
		}
	}
	return b.String()
}
