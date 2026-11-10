package main

import (
	"fmt"
	"strings"
)

func aggregateText(r *AggregateResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s\n", r.Path, header(
		fmt.Sprintf("field=%d count=%d", r.Field, r.Count),
		fmt.Sprintf("lines_scanned=%d lines_matched=%d", r.LinesScanned, r.LinesMatched),
		fmt.Sprintf("files_searched=%d scan_complete=%t", r.FilesSearched, r.ScanComplete),
		factIf(r.FilesSkipped > 0, "files_skipped=%d %v", r.FilesSkipped, r.SkipReasons),
		factIf(len(r.LinesSkipped) > 0, "lines_skipped=%v", r.LinesSkipped),
		factIf(r.Truncated, "truncated"), factIf(r.TopTruncated, "top_truncated")))
	if r.Hint != "" {
		b.WriteString("hint: " + r.Hint + "\n")
	}
	for _, item := range []struct {
		name   string
		record *AggregateRecord
	}{{"min", r.Min}, {"max", r.Max}} {
		if item.record != nil {
			fmt.Fprintf(&b, "%s=%s %s:%d\n", item.name, item.record.Value, item.record.File, item.record.Line)
		}
	}
	fmt.Fprintf(&b, "top (%s):\n", r.SortOrder)
	for _, rec := range r.Top {
		fmt.Fprintf(&b, "%s %s:%d\n", rec.Value, rec.File, rec.Line)
		if rec.Content != "" {
			b.WriteString(rec.Content + "\n")
		}
	}
	return b.String()
}
