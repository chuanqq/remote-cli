package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ---------------------------------------------------------------------------
// remote_move_file
// ---------------------------------------------------------------------------

type MoveFileRequest struct {
	Src       string
	Dst       string
	Overwrite bool // replace an existing destination
}

type MoveFileResult struct {
	Src         string `json:"src"`
	Dst         string `json:"dst"`
	Moved       bool   `json:"moved"`
	Overwritten bool   `json:"overwritten"` // an existing dst was replaced
}

// MoveFile renames/moves a file or directory. Falls back to copy+remove
// across filesystem boundaries. Both paths are sandboxed by roots.
func MoveFile(req MoveFileRequest, roots []string) (*MoveFileResult, error) {
	srcAbs, err := validatePath(req.Src, roots)
	if err != nil {
		return nil, err
	}
	dstAbs, err := validatePath(req.Dst, roots)
	if err != nil {
		return nil, err
	}
	if _, err := os.Lstat(srcAbs); err != nil {
		return nil, err
	}

	overwritten := false
	if _, err := os.Lstat(dstAbs); err == nil {
		if !req.Overwrite {
			return nil, errors.New("destination exists (set overwrite to replace)")
		}
		if err := os.RemoveAll(dstAbs); err != nil {
			return nil, fmt.Errorf("remove existing destination: %w", err)
		}
		overwritten = true
	}

	if err := os.Rename(srcAbs, dstAbs); err != nil {
		// Cross-filesystem move: copy then remove.
		srcInfo, statErr := os.Stat(srcAbs)
		if statErr != nil {
			return nil, statErr
		}
		if srcInfo.IsDir() {
			if _, err := copyDirRecursive(srcAbs, dstAbs); err != nil {
				return nil, fmt.Errorf("cross-device move (copy): %w", err)
			}
			if err := os.RemoveAll(srcAbs); err != nil {
				return nil, fmt.Errorf("cross-device move (cleanup): %w", err)
			}
		} else {
			if _, err := copyOneFile(srcAbs, dstAbs); err != nil {
				return nil, fmt.Errorf("cross-device move (copy): %w", err)
			}
			if err := os.Remove(srcAbs); err != nil {
				return nil, fmt.Errorf("cross-device move (cleanup): %w", err)
			}
		}
	}

	return &MoveFileResult{Src: srcAbs, Dst: dstAbs, Moved: true, Overwritten: overwritten}, nil
}

// ---------------------------------------------------------------------------
// remote_copy_file
// ---------------------------------------------------------------------------

type CopyFileRequest struct {
	Src       string
	Dst       string
	Overwrite bool
}

type CopyFileResult struct {
	Src           string `json:"src"`
	Dst           string `json:"dst"`
	Copied        bool   `json:"copied"`
	BytesCopied   int64  `json:"bytes_copied,omitempty"`   // single file
	EntriesCopied int    `json:"entries_copied,omitempty"` // directory tree
	Overwritten   bool   `json:"overwritten"`
}

// CopyFile copies a single file (preserving mode) or a directory tree.
// An existing destination directory is an error (no implicit merging).
func CopyFile(req CopyFileRequest, roots []string) (*CopyFileResult, error) {
	srcAbs, err := validatePath(req.Src, roots)
	if err != nil {
		return nil, err
	}
	dstAbs, err := validatePath(req.Dst, roots)
	if err != nil {
		return nil, err
	}

	srcInfo, err := os.Lstat(srcAbs)
	if err != nil {
		return nil, err
	}

	if _, err := os.Lstat(dstAbs); err == nil {
		if !req.Overwrite {
			return nil, errors.New("destination exists (set overwrite to replace)")
		}
	}

	if srcInfo.IsDir() {
		if _, err := os.Lstat(dstAbs); err == nil {
			return nil, errors.New("destination directory exists (refusing to merge)")
		}
		n, err := copyDirRecursive(srcAbs, dstAbs)
		if err != nil {
			return nil, err
		}
		return &CopyFileResult{Src: srcAbs, Dst: dstAbs, Copied: true, EntriesCopied: n}, nil
	}

	overwritten := false
	if _, err := os.Lstat(dstAbs); err == nil {
		overwritten = true
	}
	bytesCopied, err := copyOneFile(srcAbs, dstAbs)
	if err != nil {
		return nil, err
	}
	return &CopyFileResult{Src: srcAbs, Dst: dstAbs, Copied: true, BytesCopied: bytesCopied, Overwritten: overwritten}, nil
}

// copyOneFile copies a regular file preserving its permission bits.
func copyOneFile(src, dst string) (int64, error) {
	srcInfo, err := os.Stat(src)
	if err != nil {
		return 0, err
	}
	if !srcInfo.Mode().IsRegular() {
		return 0, fmt.Errorf("not a regular file: %s", src)
	}
	in, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, srcInfo.Mode().Perm())
	if err != nil {
		return 0, err
	}
	n, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return 0, copyErr
	}
	if closeErr != nil {
		return 0, closeErr
	}
	return n, nil
}

// copyDirRecursive copies a directory tree, preserving modes and rebuilding
// symlinks. Returns the number of entries (files+dirs+links) copied.
func copyDirRecursive(src, dst string) (int, error) {
	count := 0
	err := filepath.Walk(src, func(p string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			if err := os.MkdirAll(target, info.Mode().Perm()); err != nil {
				return err
			}
			count++
			return nil
		}
		if info.Mode()&os.ModeSymlink != 0 {
			linkTarget, err := os.Readlink(p)
			if err != nil {
				return err
			}
			_ = os.Remove(target)
			if err := os.Symlink(linkTarget, target); err != nil {
				return err
			}
			count++
			return nil
		}
		if _, err := copyOneFile(p, target); err != nil {
			return err
		}
		count++
		return nil
	})
	return count, err
}

// ---------------------------------------------------------------------------
// remote_delete_file
// ---------------------------------------------------------------------------

type DeleteFileRequest struct {
	Path      string
	Recursive bool // required for non-empty directories
	Confirm   bool // required to actually delete
	DryRun    bool // report what would be deleted, delete nothing
}

type DeleteFileResult struct {
	Path    string   `json:"path"`
	Deleted bool     `json:"deleted"`
	DryRun  bool     `json:"dry_run"`
	Entries int      `json:"entries"` // entries removed (or that would be)
	Sample  []string `json:"sample"`  // up to 100 affected paths
	IsDir   bool     `json:"is_dir"`
}

const deleteSampleLimit = 100

// DeleteFile removes a file or directory tree. It refuses to run without
// confirm=true; dry_run returns the affected entries instead. As a blast
// radius guard it also refuses to delete an FS root itself.
func DeleteFile(req DeleteFileRequest, roots []string) (*DeleteFileResult, error) {
	abs, err := validatePath(req.Path, roots)
	if err != nil {
		return nil, err
	}

	// Never delete a sandbox root itself.
	for _, root := range roots {
		if abs == filepath.Clean(root) {
			return nil, errors.New("refusing to delete an FS root itself")
		}
	}

	info, err := os.Lstat(abs)
	if err != nil {
		return nil, err
	}

	result := &DeleteFileResult{Path: abs, IsDir: info.IsDir(), DryRun: req.DryRun}

	if info.IsDir() {
		empty := true
		entries, walkErr := os.ReadDir(abs)
		if walkErr == nil && len(entries) > 0 {
			empty = false
		}
		if !empty && !req.Recursive {
			return nil, errors.New("directory is not empty (set recursive to delete its contents)")
		}
		// Enumerate what would be deleted.
		sample := []string{}
		count := 0
		_ = filepath.WalkDir(abs, func(p string, d os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return nil
			}
			if p == abs {
				return nil
			}
			count++
			if len(sample) < deleteSampleLimit {
				sample = append(sample, p)
			}
			return nil
		})
		result.Entries = count
		result.Sample = sample
	} else {
		result.Entries = 1
		result.Sample = []string{abs}
	}

	if req.DryRun {
		return result, nil
	}
	if !req.Confirm {
		return nil, fmt.Errorf("confirm=true is required to delete (%d entries would be removed)", result.Entries)
	}

	if err := os.RemoveAll(abs); err != nil {
		return nil, err
	}
	result.Deleted = true
	return result, nil
}

// ---------------------------------------------------------------------------
// remote_make_dir
// ---------------------------------------------------------------------------

type MakeDirRequest struct {
	Path    string
	Mode    string // octal, default 0755
	Parents bool   // create parents (mkdir -p); false = single level
}

type MakeDirResult struct {
	Path    string `json:"path"`
	Created bool   `json:"created"` // false when the directory already existed
	Mode    string `json:"mode"`
}

// MakeDir creates a directory, optionally with parents.
func MakeDir(req MakeDirRequest, roots []string) (*MakeDirResult, error) {
	abs, err := validatePath(req.Path, roots)
	if err != nil {
		return nil, err
	}

	mode, err := parseMode(req.Mode)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.Mode) == "" {
		mode = 0755
	}

	if info, statErr := os.Stat(abs); statErr == nil {
		if !info.IsDir() {
			return nil, errors.New("path exists and is not a directory")
		}
		return &MakeDirResult{Path: abs, Created: false, Mode: info.Mode().Perm().String()}, nil
	}

	if req.Parents {
		err = os.MkdirAll(abs, mode)
	} else {
		err = os.Mkdir(abs, mode)
	}
	if err != nil {
		return nil, err
	}

	modeStr := ""
	if info, statErr := os.Stat(abs); statErr == nil {
		modeStr = info.Mode().Perm().String()
	}
	return &MakeDirResult{Path: abs, Created: true, Mode: modeStr}, nil
}
