package main

import (
	"fmt"
	"strings"
)

// ---------------------------------------------------------------------------
// Line diff for remote_write_file dry_run previews.
//
// Common leading/trailing lines are stripped first; the changed middle is
// diffed with an LCS table when it is small enough, otherwise reported as one
// replace block. The output is unified-diff text with 3 lines of context.
// ---------------------------------------------------------------------------

const (
	diffContext  = 3
	maxDiffCells = 1 << 20 // LCS table cells (int32) for the changed middle
)

type diffOp struct {
	kind   byte // ' ', '-', '+'
	text   string
	oldIdx int // 0-based index in old (next old line for '+')
	newIdx int // 0-based index in new (next new line for '-')
}

// splitDiffLines splits s into lines; a trailing newline does not start an
// extra empty line.
func splitDiffLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	for i := range lines {
		lines[i] = strings.TrimSuffix(lines[i], "\r")
	}
	return lines
}

// unifiedDiff returns the diff from oldText to newText (capped at maxBytes),
// the numbers of added and removed lines, and whether the text was capped.
func unifiedDiff(oldText, newText string, maxBytes int) (string, int, int, bool) {
	a, b := splitDiffLines(oldText), splitDiffLines(newText)

	p := 0
	for p < len(a) && p < len(b) && a[p] == b[p] {
		p++
	}
	q := 0
	for q < len(a)-p && q < len(b)-p && a[len(a)-1-q] == b[len(b)-1-q] {
		q++
	}

	var ops []diffOp
	for i := max(0, p-diffContext); i < p; i++ {
		ops = append(ops, diffOp{kind: ' ', text: a[i], oldIdx: i, newIdx: i})
	}
	ops = append(ops, diffMiddle(a[p:len(a)-q], b[p:len(b)-q], p, p)...)
	for k := 0; k < min(q, diffContext); k++ {
		i, j := len(a)-q+k, len(b)-q+k
		ops = append(ops, diffOp{kind: ' ', text: a[i], oldIdx: i, newIdx: j})
	}

	added, removed := 0, 0
	for _, op := range ops {
		switch op.kind {
		case '+':
			added++
		case '-':
			removed++
		}
	}
	if added == 0 && removed == 0 {
		if oldText != newText {
			return "(only line endings or the final newline differ)\n", 0, 0, false
		}
		return "", 0, 0, false
	}

	var sb strings.Builder
	truncated := false
	write := func(s string) bool {
		if sb.Len()+len(s) > maxBytes {
			truncated = true
			return false
		}
		sb.WriteString(s)
		return true
	}
	for start := 0; start < len(ops); {
		// Find the next change, then extend the hunk while changes are
		// separated by at most 2*context equal lines.
		first := start
		for first < len(ops) && ops[first].kind == ' ' {
			first++
		}
		if first == len(ops) {
			break
		}
		last := first
		for i := first + 1; i < len(ops); i++ {
			if ops[i].kind != ' ' {
				if i-last-1 > 2*diffContext {
					break
				}
				last = i
			}
		}
		lo := max(start, first-diffContext)
		hi := min(len(ops)-1, last+diffContext)

		oldCount, newCount := 0, 0
		for _, op := range ops[lo : hi+1] {
			if op.kind != '+' {
				oldCount++
			}
			if op.kind != '-' {
				newCount++
			}
		}
		oldStart, newStart := ops[lo].oldIdx+1, ops[lo].newIdx+1
		if oldCount == 0 {
			oldStart--
		}
		if newCount == 0 {
			newStart--
		}
		if !write(fmt.Sprintf("@@ -%d,%d +%d,%d @@\n", oldStart, oldCount, newStart, newCount)) {
			break
		}
		for _, op := range ops[lo : hi+1] {
			if !write(string(op.kind) + op.text + "\n") {
				break
			}
		}
		if truncated {
			break
		}
		start = hi + 1
	}
	return sb.String(), added, removed, truncated
}

// diffMiddle diffs the changed middle a/b, whose first lines sit at old index
// ao and new index bo.
func diffMiddle(a, b []string, ao, bo int) []diffOp {
	var ops []diffOp
	if len(a)*len(b) > maxDiffCells {
		for i, s := range a {
			ops = append(ops, diffOp{kind: '-', text: s, oldIdx: ao + i, newIdx: bo})
		}
		for j, s := range b {
			ops = append(ops, diffOp{kind: '+', text: s, oldIdx: ao + len(a), newIdx: bo + j})
		}
		return ops
	}
	// lcs[i][j] = LCS length of a[i:] and b[j:].
	w := len(b) + 1
	lcs := make([]int32, (len(a)+1)*w)
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i*w+j] = lcs[(i+1)*w+j+1] + 1
			} else {
				lcs[i*w+j] = max(lcs[(i+1)*w+j], lcs[i*w+j+1])
			}
		}
	}
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		switch {
		case i < len(a) && j < len(b) && a[i] == b[j]:
			ops = append(ops, diffOp{kind: ' ', text: a[i], oldIdx: ao + i, newIdx: bo + j})
			i++
			j++
		case i < len(a) && (j == len(b) || lcs[(i+1)*w+j] >= lcs[i*w+j+1]):
			// Ties go to the deletion, so a replaced line reads -old +new.
			ops = append(ops, diffOp{kind: '-', text: a[i], oldIdx: ao + i, newIdx: bo + j})
			i++
		default:
			ops = append(ops, diffOp{kind: '+', text: b[j], oldIdx: ao + i, newIdx: bo + j})
			j++
		}
	}
	return ops
}
