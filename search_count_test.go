package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSearchCountTotalsIndependentOfPage(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"a.log": "hit hit\nmiss\nhit\n",
		"b.log": "hit\nhit\nhit\n",
		"c.log": "miss\n",
	})
	for offset := 0; offset < 2; offset++ {
		r, err := SearchContent(SearchContentRequest{Path: root, Pattern: "hit", OutputMode: SearchCountMode,
			MaxResults: 1, Offset: offset}, []string{root})
		if err != nil {
			t.Fatal(err)
		}
		if r.TotalMatches != 5 || r.FilesMatched != 2 || r.FilesSearched != 3 ||
			r.CountsComplete == nil || !*r.CountsComplete || len(r.Files) != 1 {
			t.Fatalf("offset=%d: %+v", offset, r)
		}
		wantFile, wantCount := "a.log", 2
		if offset == 1 {
			wantFile, wantCount = "b.log", 3
		}
		if filepath.Base(r.Files[0].File) != wantFile || r.Files[0].Count != wantCount || r.Files[0].Partial {
			t.Fatalf("offset=%d: files=%+v", offset, r.Files)
		}
		if offset == 0 && (!r.Truncated || r.NextOffset != 1) || offset == 1 && r.Truncated {
			t.Fatalf("offset=%d: truncated=%v next=%d", offset, r.Truncated, r.NextOffset)
		}
	}
}

func TestSearchCountZeroAndIncomplete(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"a.log": "no match\n"})
	req := SearchContentRequest{Path: root, Pattern: "hit", OutputMode: SearchCountMode}
	r, err := SearchContent(req, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(r)
	if err != nil || !strings.Contains(string(data), `"total_matches":0`) ||
		!strings.Contains(string(data), `"counts_complete":true`) {
		t.Fatalf("zero count must be explicit: %s %v", data, err)
	}
	req.Deadline = time.Now().Add(-time.Second)
	r, err = SearchContent(req, []string{root})
	if err != nil || !r.Truncated || r.CountsComplete == nil || *r.CountsComplete || r.NextOffset != 0 {
		t.Fatalf("expired deadline: %+v %v", r, err)
	}
	req.Deadline = time.Time{}
	req.MaxFileSize = 1
	r, err = SearchContent(req, []string{root})
	if err != nil || r.CountsComplete == nil || *r.CountsComplete || r.SkipReasons["too_large"] != 1 {
		t.Fatalf("skipped file: %+v %v", r, err)
	}
}

func TestTextScanCancellationAtReadBoundary(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"a.log": strings.Repeat("hit\n", 10000)})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lines := 0
	r, err := scanTextFiles(SearchContentRequest{Path: root, Pattern: ".*", Context: ctx}, []string{root},
		func(_ string, _ int, _ string, _ bool) {
			lines++
			cancel()
		})
	if err != nil || !r.Truncated || lines == 0 || lines >= 10000 || r.FilesSkipped != 0 {
		t.Fatalf("cancelled scan must stop without an unreadable-file error: lines=%d result=%+v err=%v", lines, r, err)
	}
}
