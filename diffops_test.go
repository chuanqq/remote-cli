package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDiffFiles(t *testing.T) {
	root := t.TempDir()
	roots := []string{root}
	a := filepath.Join(root, "old.conf")
	b := filepath.Join(root, "new.conf")
	os.WriteFile(a, []byte("port : 8080\nmode : old\nlog : on\n"), 0o644)
	os.WriteFile(b, []byte("port : 8080\nmode : new\nlog : on\n"), 0o644)

	r, err := Diff(DiffRequest{Left: a, Right: b}, roots)
	if err != nil {
		t.Fatal(err)
	}
	want := "@@ -1,3 +1,3 @@\n port : 8080\n-mode : old\n+mode : new\n log : on\n"
	if r.Mode != "file" || r.Identical || r.Diff != want || r.LinesAdded != 1 || r.LinesRemoved != 1 {
		t.Errorf("file diff: %+v", r)
	}

	// Whitespace-only changes with ignore_space; encoding-only changes.
	os.WriteFile(b, []byte("port :  8080\nmode : old\t\nlog : on\n"), 0o644)
	if r, _ = Diff(DiffRequest{Left: a, Right: b, IgnoreSpace: true}, roots); !r.Identical {
		t.Errorf("ignore_space: %+v", r)
	}
	gbk, _ := encodeFromUTF8("# 预算配置\nbudget : 100\n", "gbk")
	os.WriteFile(a, []byte("# 预算配置\nbudget : 100\n"), 0o644)
	os.WriteFile(b, gbk, 0o644)
	if r, _ = Diff(DiffRequest{Left: a, Right: b}, roots); r.LinesAdded+r.LinesRemoved != 0 || r.Encoding != "utf-8/gbk" {
		t.Errorf("encoding-only change: %+v", r)
	}

	if _, err := Diff(DiffRequest{Left: a, Right: root}, roots); err == nil {
		t.Error("file vs directory accepted")
	}
	if _, err := Diff(DiffRequest{Left: a, Right: "/etc/hosts"}, roots); err == nil {
		t.Error("right outside the roots accepted")
	}
}

func TestDiffTrees(t *testing.T) {
	root := t.TempDir()
	old, nw := filepath.Join(root, "toad_policy_old"), filepath.Join(root, "toad_policy_new")
	writeTree(t, old, map[string]string{
		"conf/a.conf":    "port : 8080\n",
		"conf/b.conf":    "same\n",
		"conf/gone.conf": "x\n",
		"bin/start.sh":   "#!/bin/sh\n",
		"log/app.log":    "noise\n",
	})
	writeTree(t, nw, map[string]string{
		"conf/a.conf":   "port : 9090\n",
		"conf/b.conf":   "same\n",
		"conf/new.conf": "y\n",
		"bin/start.sh":  "#!/bin/sh\n",
		"log/app.log":   "other noise\n",
	})
	r, err := Diff(DiffRequest{Left: old, Right: nw, ExcludeGlob: []string{"log"}, ShowDiffs: true}, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	if r.Mode != "tree" || r.Identical ||
		strings.Join(r.Added, ",") != "conf/new.conf" ||
		strings.Join(r.Removed, ",") != "conf/gone.conf" ||
		strings.Join(r.Changed, ",") != "conf/a.conf" {
		t.Fatalf("tree diff: %+v", r)
	}
	if r.SameCount != 4 { // bin, bin/start.sh, conf, conf/b.conf
		t.Errorf("same_count=%d, want 4", r.SameCount)
	}
	if len(r.Diffs) != 1 || !strings.Contains(r.Diffs[0].Diff, "-port : 8080\n+port : 9090") {
		t.Errorf("show_diffs: %+v", r.Diffs)
	}
	r, _ = Diff(DiffRequest{Left: filepath.Join(old, "bin"), Right: filepath.Join(nw, "bin")}, []string{root})
	if !r.Identical {
		t.Errorf("identical trees: %+v", r)
	}
}

// Past the deadline, files on both sides are no longer compared, but the
// added / removed lists are still complete and the result says so.
func TestDiffTreesDeadline(t *testing.T) {
	root := t.TempDir()
	old, nw := filepath.Join(root, "old"), filepath.Join(root, "new")
	writeTree(t, old, map[string]string{"a.conf": "1\n", "gone.conf": "x\n"})
	writeTree(t, nw, map[string]string{"a.conf": "2\n", "new.conf": "y\n"})
	r, err := Diff(DiffRequest{Left: old, Right: nw, Deadline: time.Now().Add(-time.Second)}, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Truncated || r.Identical || len(r.Changed) != 0 || strings.Join(r.Added, ",") != "new.conf" ||
		strings.Join(r.Removed, ",") != "gone.conf" || !strings.Contains(r.Hint, "1 files present on both sides were not compared") {
		t.Errorf("deadline: %+v", r)
	}
}
