package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestAggregateOutputWindowPreservesStatistics(t *testing.T) {
	root := t.TempDir()
	var lines strings.Builder
	for i := 1; i <= 1000; i++ {
		fmt.Fprintf(&lines, "%d %s\n", i, strings.Repeat("x", 2000))
	}
	writeTree(t, root, map[string]string{"a.log": lines.String()})
	r, err := AggregateContent(AggregateRequest{Path: root, Field: 1, TopN: 1000, MaxLinePreview: 2000}, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	if r.Count != 1000 || r.Min.Value != "1" || r.Max.Value != "1000" || r.Truncated || !r.ScanComplete || !r.TopTruncated || len(r.Top) >= 1000 {
		t.Fatalf("output cap changed scan statistics: %+v", r)
	}
	data, err := json.Marshal(r)
	if err != nil || len(data) > maxSearchOutputBytes {
		t.Fatalf("output window exceeded: %d bytes, %v", len(data), err)
	}
}

func TestAggregateTopKeepsFirstTiedRecords(t *testing.T) {
	root := t.TempDir()
	for _, order := range []string{"asc", "desc"} {
		var lines strings.Builder
		for _, value := range []int{1, 2, 2, 3, 3, 4, 3} {
			if order == "asc" {
				value = -value
			}
			fmt.Fprintln(&lines, value)
		}
		writeTree(t, root, map[string]string{order + ".log": lines.String()})
		r, err := AggregateContent(AggregateRequest{Path: filepath.Join(root, order+".log"), Field: 1, TopN: 3, SortOrder: order}, []string{root})
		if err != nil {
			t.Fatal(err)
		}
		if len(r.Top) != 3 || r.Top[0].Line != 6 || r.Top[1].Line != 4 || r.Top[2].Line != 5 {
			t.Fatalf("%s: tied records must keep input order: %+v", order, r.Top)
		}
	}
}

func TestAggregateResourceLimitsAndNeededColumns(t *testing.T) {
	for _, req := range []AggregateRequest{
		{Field: maxAggField + 1},
		{Field: 1, Filters: make([]AggregateFilter, maxAggFilters+1)},
		{Field: 1, Delimiter: strings.Repeat("|", maxAggDelimiterLen+1)},
		{Field: 1, Filters: []AggregateFilter{{Field: maxAggField + 1, Op: "eq", Value: "1"}}},
		{Field: 1, Filters: []AggregateFilter{{Field: 1, Op: "eq", Value: strings.Repeat("x", maxAggFilterValueLen+1)}}},
	} {
		if _, err := newAggOpts(req); err == nil {
			t.Fatalf("unbounded request accepted: %+v", req)
		}
	}
	for _, delim := range []string{"|", " "} {
		text := "10" + delim + strings.Repeat("x"+delim, 400000)
		fields := splitAggFields(text, strings.TrimSpace(delim), 1)
		if len(fields) != 1 || fields[0] != "10" {
			t.Fatalf("unneeded columns retained: %v", fields)
		}
	}
}
