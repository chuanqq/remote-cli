package main

import (
	"os"
	"path/filepath"
	"testing"
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
