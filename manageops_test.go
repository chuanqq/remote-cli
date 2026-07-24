package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMoveFile(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "old.txt")
	os.WriteFile(src, []byte("data"), 0644)

	dst := filepath.Join(root, "new.txt")
	res, err := MoveFile(MoveFileRequest{Src: src, Dst: dst}, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Moved || res.Overwritten {
		t.Errorf("move: %+v", res)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Error("src still exists")
	}
	if got, _ := os.ReadFile(dst); string(got) != "data" {
		t.Errorf("dst content: %q", got)
	}

	// Existing dst without overwrite -> error.
	os.WriteFile(src, []byte("x"), 0644)
	if _, err := MoveFile(MoveFileRequest{Src: src, Dst: dst}, []string{root}); err == nil {
		t.Error("expected destination-exists error")
	}

	// With overwrite.
	if _, err := MoveFile(MoveFileRequest{Src: src, Dst: dst, Overwrite: true}, []string{root}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(dst); string(got) != "x" {
		t.Errorf("overwrite content: %q", got)
	}
}

func TestCopyFileAndDir(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "f.txt")
	os.WriteFile(src, []byte("hello"), 0644)

	dst := filepath.Join(root, "f2.txt")
	res, err := CopyFile(CopyFileRequest{Src: src, Dst: dst}, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Copied || res.BytesCopied != 5 {
		t.Errorf("copy: %+v", res)
	}

	// Directory tree copy.
	tree := filepath.Join(root, "tree")
	os.MkdirAll(filepath.Join(tree, "sub"), 0755)
	os.WriteFile(filepath.Join(tree, "a.txt"), []byte("a"), 0644)
	os.WriteFile(filepath.Join(tree, "sub", "b.txt"), []byte("b"), 0644)

	dstTree := filepath.Join(root, "tree2")
	dres, err := CopyFile(CopyFileRequest{Src: tree, Dst: dstTree}, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	if dres.EntriesCopied != 4 { // 2 dirs + 2 files
		t.Errorf("entries=%d, want 4", dres.EntriesCopied)
	}
	if got, _ := os.ReadFile(filepath.Join(dstTree, "sub", "b.txt")); string(got) != "b" {
		t.Errorf("tree copy content: %q", got)
	}

	// Existing directory destination is refused (no merging).
	if _, err := CopyFile(CopyFileRequest{Src: tree, Dst: dstTree, Overwrite: true}, []string{root}); err == nil {
		t.Error("expected directory-merge refusal")
	}
}

func TestDeleteFileSafeguards(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "delme")
	os.MkdirAll(filepath.Join(target, "sub"), 0755)
	os.WriteFile(filepath.Join(target, "sub", "x.txt"), []byte("x"), 0644)

	// Non-empty dir without recursive -> error.
	if _, err := DeleteFile(DeleteFileRequest{Path: target}, []string{root}); err == nil {
		t.Error("expected recursive-required error")
	}

	// Without confirm -> error, nothing deleted.
	if _, err := DeleteFile(DeleteFileRequest{Path: target, Recursive: true}, []string{root}); err == nil {
		t.Error("expected confirm-required error")
	}
	if _, err := os.Stat(target); err != nil {
		t.Error("directory deleted without confirm")
	}

	// Dry run reports entries, deletes nothing.
	dry, err := DeleteFile(DeleteFileRequest{Path: target, Recursive: true, DryRun: true}, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	if dry.Deleted || dry.Entries != 2 || len(dry.Sample) != 2 {
		t.Errorf("dry run: %+v", dry)
	}
	if _, err := os.Stat(target); err != nil {
		t.Error("dry run deleted the directory")
	}

	// Confirmed delete.
	res, err := DeleteFile(DeleteFileRequest{Path: target, Recursive: true, Confirm: true}, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Deleted {
		t.Errorf("delete: %+v", res)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Error("directory still exists after confirm delete")
	}
}

func TestDeleteFileRefusesFSRoot(t *testing.T) {
	root := t.TempDir()
	if _, err := DeleteFile(DeleteFileRequest{Path: root, Recursive: true, Confirm: true}, []string{root}); err == nil {
		t.Error("expected FS-root deletion refusal")
	}
}

func TestMakeDir(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "a", "b", "c")

	res, err := MakeDir(MakeDirRequest{Path: target, Parents: true}, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Created {
		t.Errorf("make dir: %+v", res)
	}
	if info, _ := os.Stat(target); info == nil || !info.IsDir() {
		t.Error("directory not created")
	}

	// Existing dir -> Created=false.
	res2, _ := MakeDir(MakeDirRequest{Path: target, Parents: true}, []string{root})
	if res2.Created {
		t.Error("existing dir reported as created")
	}

	// Without parents, nested path fails.
	if _, err := MakeDir(MakeDirRequest{Path: filepath.Join(root, "x", "y"), Parents: false}, []string{root}); err == nil {
		t.Error("expected single-level mkdir failure")
	}
}
