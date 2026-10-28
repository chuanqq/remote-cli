package main

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// remote_diff: compare two files, or two directory trees, on the server.
//
// The production audit log shows agents comparing a new deployment with an
// old one by hand (`for app in new old target; do grep ... $app/conf/...;
// done`, sha256sum over bin/ conf/ lib/), which read-only mode cannot run.
// Files get a unified diff (the remote_write_file dry_run engine); trees get
// added / removed / changed lists, compared by size then content.
// ---------------------------------------------------------------------------

const (
	defaultDiffMaxBytes = 64 << 10
	maxDiffFileBytes    = 8 << 20 // larger files are compared by hash only
	maxDiffTreeEntries  = 20000
	maxDiffListed       = 500
	maxTreeDiffs        = 20 // changed text files shown with their diff in tree mode
)

type DiffRequest struct {
	Left, Right   string
	IgnoreSpace   bool     // ignore whitespace changes within lines (diff -b)
	ExcludeGlob   []string // tree mode: entries to skip
	IncludeHidden bool
	MaxBytes      int       // cap of the diff text (default 64KB)
	ShowDiffs     bool      // tree mode: include the unified diff of changed text files
	Deadline      time.Time // tree mode: stop comparing contents at this time; zero = no limit
}

type DiffResult struct {
	Left         string     `json:"left"`
	Right        string     `json:"right"`
	Mode         string     `json:"mode"` // "file" | "tree"
	Identical    bool       `json:"identical"`
	Encoding     string     `json:"encoding,omitempty"` // file mode: "left/right" when they differ
	LinesAdded   int        `json:"lines_added,omitempty"`
	LinesRemoved int        `json:"lines_removed,omitempty"`
	Added        []string   `json:"added,omitempty"`   // tree: only in right (relative paths)
	Removed      []string   `json:"removed,omitempty"` // tree: only in left
	Changed      []string   `json:"changed,omitempty"` // tree: in both, content differs
	TypeChanged  []string   `json:"type_changed,omitempty"`
	SameCount    int        `json:"same_count,omitempty"`
	Truncated    bool       `json:"truncated"`
	Hint         string     `json:"hint,omitempty"`
	Diffs        []FileDiff `json:"diffs,omitempty"` // tree + show_diffs
	Diff         string     `json:"diff,omitempty"`  // file mode: unified diff, left -> right
}

// FileDiff is the unified diff of one changed file in tree mode.
type FileDiff struct {
	Path string `json:"path"`
	Diff string `json:"diff"`
}

// Diff compares two files or two directory trees inside the roots.
func Diff(req DiffRequest, roots []string) (*DiffResult, error) {
	left, err := validatePath(req.Left, roots)
	if err != nil {
		return nil, fmt.Errorf("left: %w", err)
	}
	right, err := validatePath(req.Right, roots)
	if err != nil {
		return nil, fmt.Errorf("right: %w", err)
	}
	li, err := os.Stat(left)
	if err != nil {
		return nil, fmt.Errorf("left: %w", err)
	}
	ri, err := os.Stat(right)
	if err != nil {
		return nil, fmt.Errorf("right: %w", err)
	}
	maxBytes := req.MaxBytes
	if maxBytes <= 0 {
		maxBytes = defaultDiffMaxBytes
	}
	maxBytes = min(maxBytes, maxReadMaxBytes)
	res := &DiffResult{Left: left, Right: right}
	switch {
	case li.IsDir() && ri.IsDir():
		res.Mode = "tree"
		return res, diffTrees(res, req, maxBytes)
	case li.IsDir() != ri.IsDir():
		return nil, errors.New("left and right must both be files or both be directories")
	case !li.Mode().IsRegular() || !ri.Mode().IsRegular():
		return nil, errors.New("left and right must be regular files (reading a pipe or device could block)")
	}
	res.Mode = "file"
	diff, added, removed, same, note, truncated, err := diffFiles(left, right, li.Size(), ri.Size(), req.IgnoreSpace, maxBytes)
	if err != nil {
		return nil, err
	}
	res.Identical, res.Diff, res.LinesAdded, res.LinesRemoved, res.Truncated = same, diff, added, removed, truncated
	res.Encoding, res.Hint = note.encoding, note.hint
	return res, nil
}

type diffNote struct{ encoding, hint string }

// diffFiles returns the unified diff from left to right. Files larger than
// maxDiffFileBytes, and binary files, are compared by content hash only.
func diffFiles(left, right string, lsize, rsize int64, ignoreSpace bool, maxBytes int) (string, int, int, bool, diffNote, bool, error) {
	var note diffNote
	if lsize > maxDiffFileBytes || rsize > maxDiffFileBytes {
		same, err := sameContent(left, right, lsize, rsize)
		note.hint = fmt.Sprintf("files over %d MB are compared by content only (no line diff)", maxDiffFileBytes>>20)
		return "", 0, 0, same, note, false, err
	}
	a, err := os.ReadFile(left)
	if err != nil {
		return "", 0, 0, false, note, false, err
	}
	b, err := os.ReadFile(right)
	if err != nil {
		return "", 0, 0, false, note, false, err
	}
	if bytes.Equal(a, b) {
		return "", 0, 0, true, note, false, nil
	}
	if looksBinary(a) || looksBinary(b) {
		note.hint = "binary content differs (no line diff)"
		return "", 0, 0, false, note, false, nil
	}
	ea, eb := detectEncoding(a), detectEncoding(b)
	if ea != eb {
		note.encoding = ea + "/" + eb
	}
	ta, _ := decodeText(a, ea, "")
	tb, _ := decodeText(b, eb, "")
	if ignoreSpace {
		ta, tb = squeezeSpace(ta), squeezeSpace(tb)
		if ta == tb {
			note.hint = "the files differ only in whitespace"
			return "", 0, 0, true, note, false, nil
		}
	}
	diff, added, removed, truncated := unifiedDiff(ta, tb, maxBytes)
	if truncated {
		note.hint = fmt.Sprintf("diff cut at max_bytes=%d", maxBytes)
	}
	if note.encoding != "" && added+removed == 0 {
		note.hint = "the files differ only in encoding: the decoded text is the same"
	}
	return diff, added, removed, false, note, truncated, nil
}

// squeezeSpace collapses runs of spaces and tabs and trims line ends, so
// whitespace-only changes compare equal (diff -b).
func squeezeSpace(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.Join(strings.Fields(l), " ")
	}
	return strings.Join(lines, "\n")
}

// sameContent compares two files by size, then by streamed SHA-256.
func sameContent(left, right string, lsize, rsize int64) (bool, error) {
	if lsize != rsize {
		return false, nil
	}
	hl, err := fileDigest(left)
	if err != nil {
		return false, err
	}
	hr, err := fileDigest(right)
	if err != nil {
		return false, err
	}
	return bytes.Equal(hl, hr), nil
}

func fileDigest(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}

// treeEntry is one entry of a tree walked for diffing.
type treeEntry struct {
	kind string // "file" | "dir" | "symlink" | "other"
	size int64
	link string
}

// walkForDiff lists the entries below root by relative path.
func walkForDiff(root string, req DiffRequest, exclude *globSet) (map[string]treeEntry, bool, error) {
	out := map[string]treeEntry{}
	capped := false
	err := walkTree(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if p == root {
			return nil
		}
		name, rel := d.Name(), relPath(root, p)
		if !req.IncludeHidden && isHiddenName(name) || exclude.matchAny(name, rel, p) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if len(out) >= maxDiffTreeEntries {
			capped = true
			return filepath.SkipAll
		}
		e := treeEntry{kind: "other"}
		switch {
		case d.IsDir():
			e.kind = "dir"
		case d.Type()&fs.ModeSymlink != 0:
			e.kind = "symlink"
			e.link, _ = os.Readlink(p)
		case d.Type().IsRegular():
			e.kind = "file"
			if fi, err := d.Info(); err == nil {
				e.size = fi.Size()
			}
		}
		out[filepath.ToSlash(rel)] = e
		return nil
	})
	return out, capped, err
}

// diffTrees fills res with the differences between two directory trees.
func diffTrees(res *DiffResult, req DiffRequest, maxBytes int) error {
	exclude, err := compileGlobs(req.ExcludeGlob, false)
	if err != nil {
		return fmt.Errorf("exclude_glob: %w", err)
	}
	left, lcap, err := walkForDiff(res.Left, req, exclude)
	if err != nil {
		return err
	}
	right, rcap, err := walkForDiff(res.Right, req, exclude)
	if err != nil {
		return err
	}
	var hints []string
	if lcap || rcap {
		res.Truncated = true
		hints = append(hints, fmt.Sprintf("a tree has more than %d entries: only the first ones were compared; "+
			"narrow with exclude_glob or compare subdirectories", maxDiffTreeEntries))
	}
	names := make([]string, 0, len(left)+len(right))
	for n := range left {
		names = append(names, n)
	}
	for n := range right {
		if _, ok := left[n]; !ok {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	diffBytes, unchecked := 0, 0
	for _, n := range names {
		l, inL := left[n]
		r, inR := right[n]
		switch {
		case !inR:
			res.Removed = append(res.Removed, n)
		case !inL:
			res.Added = append(res.Added, n)
		case l.kind != r.kind:
			res.TypeChanged = append(res.TypeChanged, fmt.Sprintf("%s (%s -> %s)", n, l.kind, r.kind))
		case l.kind == "symlink":
			if l.link != r.link {
				res.Changed = append(res.Changed, n)
			} else {
				res.SameCount++
			}
		case l.kind == "file":
			// Past the time budget, files on both sides are no longer read;
			// the cheap added / removed lists are still completed.
			if unchecked > 0 || !req.Deadline.IsZero() && time.Now().After(req.Deadline) {
				unchecked++
				continue
			}
			lp, rp := filepath.Join(res.Left, n), filepath.Join(res.Right, n)
			same, err := sameContent(lp, rp, l.size, r.size)
			if err != nil {
				res.Changed = append(res.Changed, n+" (unreadable: "+err.Error()+")")
				continue
			}
			if same {
				res.SameCount++
				continue
			}
			res.Changed = append(res.Changed, n)
			if req.ShowDiffs && len(res.Diffs) < maxTreeDiffs && diffBytes < maxBytes {
				if d, _, _, _, note, _, err := diffFiles(lp, rp, l.size, r.size, req.IgnoreSpace, maxBytes-diffBytes); err == nil {
					if d == "" {
						d = "(" + firstNonEmpty(note.hint, "no line diff") + ")"
					}
					res.Diffs = append(res.Diffs, FileDiff{Path: n, Diff: d})
					diffBytes += len(d)
				}
			}
		default:
			res.SameCount++
		}
	}
	for _, list := range []*[]string{&res.Added, &res.Removed, &res.Changed, &res.TypeChanged} {
		if len(*list) > maxDiffListed {
			hints = append(hints, fmt.Sprintf("a list was cut at %d entries", maxDiffListed))
			*list = (*list)[:maxDiffListed]
			res.Truncated = true
		}
	}
	if unchecked > 0 {
		res.Truncated = true
		hints = append(hints, fmt.Sprintf("time budget reached: %d files present on both sides were not compared; "+
			"narrow with exclude_glob or compare subdirectories", unchecked))
	}
	if req.ShowDiffs && len(res.Changed) > len(res.Diffs) {
		hints = append(hints, fmt.Sprintf("%d of %d changed files have a diff shown; compare the others one by one",
			len(res.Diffs), len(res.Changed)))
	}
	res.Identical = len(res.Added)+len(res.Removed)+len(res.Changed)+len(res.TypeChanged) == 0 && !res.Truncated
	res.Hint = strings.Join(hints, "; ")
	return nil
}
