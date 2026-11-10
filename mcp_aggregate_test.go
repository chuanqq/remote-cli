package main

import (
	"strings"
	"testing"
	"time"
)

func TestMCPReadOnlyAggregateExactNumbers(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"a.log": "a|ok|9007199254740992\nb|ok|9007199254740993\nc|skip|99999999999999999\nd|ok|-10.5\n"})
	c, _ := newScanTestClient(t, root, time.Second, nil)
	args := map[string]any{
		"path": root, "delimiter": "|", "field": 3, "top_n": 1,
		"filters": []map[string]any{{"field": 2, "op": "eq", "value": "ok"}, {"field": 3, "op": "gt", "value": "0"}},
	}
	r := c.json("remote_aggregate", args)
	if r["count"] != float64(2) || r["state"] != ScanCompleted {
		t.Fatalf("aggregate count: %v", r)
	}
	max, maxOK := r["max"].(map[string]any)
	min, minOK := r["min"].(map[string]any)
	if !maxOK || !minOK || max["value"] != "9007199254740993" || min["value"] != "9007199254740992" || max["line"] != float64(2) {
		t.Fatalf("aggregate lost integer precision or location: %v", r)
	}
	top, _ := r["top"].([]any)
	if len(top) != 1 || top[0].(map[string]any)["value"] != "9007199254740993" {
		t.Fatalf("aggregate top: %v", r)
	}
	args["format"] = "text"
	text := c.call("remote_aggregate", args)
	if text.IsError || text.StructuredContent != nil || !strings.Contains(resultText(text), "9007199254740993") {
		t.Fatalf("aggregate text: %+v", text)
	}
}

func TestMCPAggregateRejectsInvalidColumnsAndFilterTypes(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"a.log": "1|2\n"})
	c, _ := newScanTestClient(t, root, time.Second, nil)
	for _, args := range []map[string]any{
		{"path": root, "field": 1.5},
		{"path": root, "field": 0},
		{"path": root, "field": 1, "filters": []map[string]any{{"field": 1, "op": "gt", "value": 9007199254740993}}},
		{"path": root, "field": 1, "filters": []map[string]any{{"field": 1, "op": "exec", "value": "x"}}},
	} {
		if r := c.call("remote_aggregate", args); !r.IsError {
			t.Fatalf("invalid aggregate arguments accepted: %v", args)
		}
	}
	outside := t.TempDir()
	if r := c.call("remote_aggregate", map[string]any{"path": outside, "field": 1}); !r.IsError {
		t.Fatal("aggregate escaped FSRoots")
	}
}
