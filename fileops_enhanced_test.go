package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestReadFileTailLines(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "big.log")

	var sb strings.Builder
	for i := 1; i <= 500; i++ {
		sb.WriteString("line-xxx-000-" + strconv.Itoa(i) + "\n")
	}
	os.WriteFile(target, []byte(sb.String()), 0644)

	res, err := ReadFileContent(ReadFileRequest{Path: target, TailLines: 3}, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(res.Content, "\n")
	if len(lines) != 3 || !strings.HasSuffix(lines[2], strconv.Itoa(500)) || !strings.HasSuffix(lines[0], strconv.Itoa(498)) {
		t.Errorf("tail lines mismatch: %q (lines=%d)", res.Content, len(lines))
	}
	if res.ReturnedLines != 3 {
		t.Errorf("returned_lines=%d, want 3", res.ReturnedLines)
	}
	if res.TotalSize != int64(len(sb.String())) {
		t.Errorf("total_size mismatch: %d", res.TotalSize)
	}
}

func TestReadFileOffsetAndTruncateTail(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "data.txt")
	os.WriteFile(target, []byte("0123456789abcdef"), 0644)

	res, err := ReadFileContent(ReadFileRequest{Path: target, OffsetBytes: 4, MaxBytes: 4}, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "4567" || res.Offset != 4 || !res.Truncated {
		t.Errorf("offset read: content=%q offset=%d truncated=%v", res.Content, res.Offset, res.Truncated)
	}

	res2, err := ReadFileContent(ReadFileRequest{Path: target, MaxBytes: 6, TruncateMode: "tail"}, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	if res2.Content != "abcdef" {
		t.Errorf("truncate tail: %q", res2.Content)
	}
	if res2.Offset != 10 {
		t.Errorf("truncate tail offset=%d, want 10", res2.Offset)
	}

	// Default head mode keeps the front.
	res3, _ := ReadFileContent(ReadFileRequest{Path: target, MaxBytes: 6}, []string{root})
	if res3.Content != "012345" {
		t.Errorf("truncate head: %q", res3.Content)
	}
}

func TestReadFileDirError(t *testing.T) {
	root := t.TempDir()
	if _, err := ReadFileContent(ReadFileRequest{Path: root}, []string{root}); err == nil {
		t.Error("expected error reading a directory")
	}
}

func TestStatFileEnhanced(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "a.txt")
	os.WriteFile(target, []byte("hello world"), 0644)

	st, err := StatFile(StatRequest{Path: target, IncludeHash: "sha256", IncludeEncoding: true}, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	if st.HashAlgo != "sha256" || len(st.Hash) != 64 {
		t.Errorf("hash missing: %+v", st)
	}
	if st.Encoding != "utf-8" {
		t.Errorf("encoding=%q", st.Encoding)
	}

	md5st, err := StatFile(StatRequest{Path: target, IncludeHash: "md5"}, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	if md5st.Hash != "5eb63bbbe01eeed093cb22bb8f5acdc3" {
		t.Errorf("md5=%q", md5st.Hash)
	}

	if _, err := StatFile(StatRequest{Path: target, IncludeHash: "crc32"}, []string{root}); err == nil {
		t.Error("expected invalid hash algo error")
	}

	// Symlink reporting (skip on platforms without permission).
	link := filepath.Join(root, "link.txt")
	if err := os.Symlink(target, link); err == nil {
		lst, err := StatFile(StatRequest{Path: link}, []string{root})
		if err != nil {
			t.Fatal(err)
		}
		if !lst.IsSymlink || lst.SymlinkTarget != target {
			t.Errorf("symlink: %+v", lst)
		}
		if lst.IsDir || lst.Size != 11 {
			t.Errorf("symlink target attrs: %+v", lst)
		}
	}
}

func TestListDirectoryEnhanced(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "a.txt"), []byte("aaaa"), 0644)
	os.WriteFile(filepath.Join(root, "b.conf"), []byte("b"), 0644)
	os.WriteFile(filepath.Join(root, ".hidden"), []byte("h"), 0644)
	os.Mkdir(filepath.Join(root, "sub"), 0755)

	// Hidden excluded by default.
	res, err := ListDirectory(ListDirRequest{Path: root}, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	if res.Count != 3 {
		t.Errorf("count=%d, want 3 (hidden excluded)", res.Count)
	}

	res, _ = ListDirectory(ListDirRequest{Path: root, IncludeHidden: true}, []string{root})
	if res.Count != 4 {
		t.Errorf("count=%d, want 4 (hidden included)", res.Count)
	}

	res, _ = ListDirectory(ListDirRequest{Path: root, FilterGlob: []string{"*.conf"}}, []string{root})
	if res.Count != 1 || res.Entries[0].Name != "b.conf" {
		t.Errorf("filter glob: %+v", res.Entries)
	}

	res, _ = ListDirectory(ListDirRequest{Path: root, SortBy: "size"}, []string{root})
	if res.Entries[0].Name != "a.txt" {
		t.Errorf("size sort first=%s, want a.txt", res.Entries[0].Name)
	}

	// Entries carry type; on unix they also carry owner/group.
	for _, e := range res.Entries {
		if e.Type == "" {
			t.Errorf("entry type empty: %+v", e)
		}
	}
}

func TestEditFileMultiAndDryRun(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "multi.txt")
	os.WriteFile(target, []byte("alpha beta gamma"), 0644)

	// Dry run: reports changes, writes nothing.
	dry, err := EditFileContent(EditFileRequest{
		Path: target,
		Edits: []EditPair{
			{OldString: "alpha", NewString: "ALPHA"},
			{OldString: "gamma", NewString: "GAMMA"},
		},
		DryRun: true,
	}, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	if !dry.DryRun || dry.Replacements != 2 || len(dry.Changes) != 2 {
		t.Errorf("dry run: %+v", dry)
	}
	raw, _ := os.ReadFile(target)
	if string(raw) != "alpha beta gamma" {
		t.Errorf("dry run modified the file: %q", raw)
	}

	// Real multi-edit.
	res, err := EditFileContent(EditFileRequest{
		Path: target,
		Edits: []EditPair{
			{OldString: "alpha", NewString: "ALPHA"},
			{OldString: "gamma", NewString: "GAMMA"},
		},
	}, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	if res.Replacements != 2 || res.SHA256 == "" {
		t.Errorf("edit: %+v", res)
	}
	raw, _ = os.ReadFile(target)
	if string(raw) != "ALPHA beta GAMMA" {
		t.Errorf("edit result: %q", raw)
	}
}

func TestEditFileAtomicFailure(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "atomic.txt")
	os.WriteFile(target, []byte("one two"), 0644)

	// Second edit cannot match: nothing must be written.
	_, err := EditFileContent(EditFileRequest{
		Path: target,
		Edits: []EditPair{
			{OldString: "one", NewString: "ONE"},
			{OldString: "missing", NewString: "X"},
		},
	}, []string{root})
	if err == nil {
		t.Fatal("expected error")
	}
	raw, _ := os.ReadFile(target)
	if string(raw) != "one two" {
		t.Errorf("non-atomic write happened: %q", raw)
	}
}

func TestEditFileRegex(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "re.txt")
	os.WriteFile(target, []byte("port = 3222\nhost = 10.0.0.1"), 0644)

	res, err := EditFileContent(EditFileRequest{
		Path:      target,
		OldString: `port = \d+`,
		NewString: "port = 4000",
		UseRegex:  true,
	}, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	if res.Replacements != 1 {
		t.Errorf("regex replacements=%d", res.Replacements)
	}
	raw, _ := os.ReadFile(target)
	if !strings.Contains(string(raw), "port = 4000") {
		t.Errorf("regex edit: %q", raw)
	}

	if _, err := EditFileContent(EditFileRequest{
		Path: target, OldString: "nomatch[0-9]", NewString: "x", UseRegex: true,
	}, []string{root}); err == nil {
		t.Error("expected pattern-not-found error")
	}
}

func TestWriteFileSelfCheck(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "script.sh")

	res, err := WriteFileContent(WriteFileRequest{
		Path:    target,
		Content: "#!/bin/bash\necho hi\n",
	}, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.SHA256) != 64 || res.Mode == "" {
		t.Errorf("self-check fields missing: %+v", res)
	}

	// Lint is advisory: only assert behavior when bash exists.
	if _, err := exec.LookPath("bash"); err == nil {
		okRes, err := WriteFileContent(WriteFileRequest{Path: target, Content: "echo ok\n", Lint: "bash"}, []string{root})
		if err != nil {
			t.Fatal(err)
		}
		if okRes.Lint == nil || !okRes.Lint.OK {
			t.Errorf("lint should pass: %+v", okRes.Lint)
		}
		badRes, _ := WriteFileContent(WriteFileRequest{Path: target, Content: "if then fi\n", Lint: "bash"}, []string{root})
		if badRes.Lint == nil || badRes.Lint.OK {
			t.Errorf("lint should fail: %+v", badRes.Lint)
		}
	}
}
