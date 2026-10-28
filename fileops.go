package main

import (
	"bytes"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"
)

const (
	defaultDownloadMaxBytes = 4194304 // 4MB
	binarySniffLen          = 8000
)

// decodeToUTF8 converts raw bytes in the given encoding to a UTF-8 string.
// Supported: utf-8/"" (returned as-is), gbk, gb2312, gb18030, and "unknown"
// (detectEncoding found no fit): the bytes are kept as-is, so ASCII stays
// readable and searchable and JSON encoding shows each invalid byte as U+FFFD.
func decodeToUTF8(data []byte, encoding string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "", "utf-8", "utf8", "unknown":
		return string(data), nil
	case "gbk", "gb2312":
		out, _, err := transform.Bytes(simplifiedchinese.GBK.NewDecoder(), data)
		if err != nil {
			return "", err
		}
		return string(out), nil
	case "gb18030":
		out, _, err := transform.Bytes(simplifiedchinese.GB18030.NewDecoder(), data)
		if err != nil {
			return "", err
		}
		return string(out), nil
	default:
		return "", fmt.Errorf("unknown encoding: %s", encoding)
	}
}

// encodeFromUTF8 converts a UTF-8 string to bytes in the target encoding.
// Supported: utf-8/"" (returned as-is), gbk, gb2312, gb18030, and "unknown"
// (bytes as-is: an edit of a file decodeToUTF8 kept raw writes its untouched
// bytes back unchanged).
func encodeFromUTF8(s string, encoding string) ([]byte, error) {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "", "utf-8", "utf8", "unknown":
		return []byte(s), nil
	case "gbk", "gb2312":
		out, _, err := transform.Bytes(simplifiedchinese.GBK.NewEncoder(), []byte(s))
		if err != nil {
			return nil, err
		}
		return out, nil
	case "gb18030":
		out, _, err := transform.Bytes(simplifiedchinese.GB18030.NewEncoder(), []byte(s))
		if err != nil {
			return nil, err
		}
		return out, nil
	default:
		return nil, fmt.Errorf("unknown encoding: %s", encoding)
	}
}

// minUTF8Evidence is how many multi-byte UTF-8 characters a line of a GBK
// file needs to be taken as UTF-8: one or two GBK characters are valid UTF-8
// surprisingly often ("预" is D4 A4, which UTF-8 reads as "Ԥ"), three or
// more practically never.
const minUTF8Evidence = 3

// decodeLine decodes one line (without its newline) to UTF-8. A forced
// encoding is applied as is. Otherwise ASCII stays as it is; in a file
// detected as GBK-family the line is decoded as such unless it is valid UTF-8
// with strong evidence (minUTF8Evidence); in a UTF-8 or unknown file valid
// UTF-8 stays and anything else is decoded as detected for that line alone,
// so a GBK comment in a UTF-8 source file still reads right.
func decodeLine(line []byte, fileEnc, forced string) string {
	if forced != "" {
		if s, err := decodeToUTF8(line, forced); err == nil {
			return s
		}
		return string(line)
	}
	if isASCII(line) {
		return string(line)
	}
	valid := utf8.Valid(line)
	enc := fileEnc
	switch {
	case isGBFamily(fileEnc):
		if valid && multiByteRunes(line) >= minUTF8Evidence {
			return string(line)
		}
	case valid:
		return string(line)
	default:
		enc = detectEncoding(line)
	}
	if s, err := decodeToUTF8(line, enc); err == nil {
		return s
	}
	return string(line)
}

// decodeText decodes data, a run of whole lines, to UTF-8: with a forced
// encoding as one block, else line by line with decodeLine, so lines in
// another encoding than the file's (detected as fileEnc) still read right.
func decodeText(data []byte, fileEnc, forced string) (string, error) {
	if forced != "" {
		return decodeToUTF8(data, forced)
	}
	if !isGBFamily(fileEnc) && utf8.Valid(data) {
		return string(data), nil
	}
	var b strings.Builder
	b.Grow(len(data) + len(data)/2)
	for len(data) > 0 {
		i := bytes.IndexByte(data, '\n')
		line := data
		if i >= 0 {
			line = data[:i]
		}
		cr := len(line) > 0 && line[len(line)-1] == '\r'
		if cr {
			line = line[:len(line)-1]
		}
		b.WriteString(decodeLine(line, fileEnc, ""))
		if cr {
			b.WriteByte('\r')
		}
		if i < 0 {
			break
		}
		b.WriteByte('\n')
		data = data[i+1:]
	}
	return b.String(), nil
}

func isASCII(b []byte) bool {
	for _, c := range b {
		if c >= 0x80 {
			return false
		}
	}
	return true
}

// multiByteRunes counts the multi-byte characters of valid UTF-8 b.
func multiByteRunes(b []byte) int {
	n := 0
	for len(b) > 0 {
		_, size := utf8.DecodeRune(b)
		if size > 1 {
			n++
		}
		b = b[size:]
	}
	return n
}

// detectEncoding guesses the encoding of data, which is often a window cut
// out of a larger file (max_bytes, offset_bytes, a tail block): an incomplete
// UTF-8 sequence at either edge does not count against UTF-8, and neither do
// a few stray invalid bytes in otherwise valid UTF-8 text. GBK wins only when
// the high bytes pair up as GBK characters. The GBK decoder never fails (it
// substitutes U+FFFD), so "decodes as GBK" is no evidence at all: an earlier
// version used it as the test and turned every cut UTF-8 window into mojibake.
// Returns "utf-8", "gbk" or "unknown" (neither fits; decoded as raw bytes).
func detectEncoding(data []byte) string {
	s, e := utf8Edges(data)
	t := data[s:e]
	if utf8.Valid(t) {
		return "utf-8"
	}
	// A stray byte in UTF-8 text breaks GBK pairing at least as often as it
	// breaks UTF-8, while GBK text is mostly invalid as UTF-8. Random high
	// bytes pair up as GBK about 3 times in 4, so GBK must be near-clean.
	ug, ub := utf8Sequences(t)
	gg, gb := gbkPairs(data)
	switch {
	case ug > 0 && (ub*50 <= ug || ub <= gb):
		return "utf-8"
	case gg > 0 && gb*20 <= gg && gb < ub:
		return "gbk"
	}
	return "unknown"
}

// utf8Edges returns the bounds of data without a partial UTF-8 sequence cut
// off at either end (UTF-8 resynchronizes within 3 bytes).
func utf8Edges(data []byte) (start, end int) {
	end = len(data)
	for start < end && start < utf8.UTFMax-1 && !utf8.RuneStart(data[start]) {
		start++
	}
	for i := end - 1; i >= start && i >= end-utf8.UTFMax; i-- {
		if utf8.RuneStart(data[i]) {
			if !utf8.FullRune(data[i:end]) {
				end = i
			}
			break
		}
	}
	return start, end
}

// isGBFamily reports whether encoding is gbk, gb2312 or gb18030.
func isGBFamily(encoding string) bool {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "gbk", "gb2312", "gb18030":
		return true
	}
	return false
}

// utf8Sequences counts valid multi-byte UTF-8 sequences and invalid bytes.
func utf8Sequences(data []byte) (good, bad int) {
	for len(data) > 0 {
		r, size := utf8.DecodeRune(data)
		switch {
		case r == utf8.RuneError && size == 1:
			bad++
		case size > 1:
			good++
		}
		data = data[size:]
	}
	return good, bad
}

// gbkPairs counts high bytes that form a GBK double-byte character (lead
// 0x81-0xFE, trail 0x40-0xFE except 0x7F) and high bytes that do not. A lead
// byte that ends data is neither: the window was cut inside that character.
func gbkPairs(data []byte) (good, bad int) {
	for i := 0; i < len(data); i++ {
		b := data[i]
		if b < 0x80 {
			continue
		}
		if b >= 0x81 && b <= 0xFE {
			if i+1 == len(data) {
				break
			}
			if t := data[i+1]; t >= 0x40 && t <= 0xFE && t != 0x7F {
				good++
				i++
				continue
			}
		}
		bad++
	}
	return good, bad
}

// looksBinary reports whether the first bytes contain a NUL (0x00).
func looksBinary(data []byte) bool {
	n := len(data)
	if n > binarySniffLen {
		n = binarySniffLen
	}
	for i := 0; i < n; i++ {
		if data[i] == 0x00 {
			return true
		}
	}
	return false
}

// validatePath resolves path to an absolute cleaned path. When roots is
// non-empty, the path must stay within at least one of the roots (sandbox),
// both as written and with its symlinks resolved: a link inside a root that
// points outside it would otherwise let reads, and writes, escape. An empty
// roots slice means no filesystem restriction. The path is returned as
// written (absolute, cleaned), not resolved.
func validatePath(path string, roots []string) (string, error) {
	if path == "" {
		return "", errors.New("path is empty")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	if len(roots) == 0 {
		return abs, nil
	}
	if !underRoots(abs, roots) {
		return "", errors.New("path escapes allowed roots")
	}
	real, err := resolvePath(abs, 0)
	if err != nil {
		return "", resolveError(abs, err)
	}
	// The messages name only the caller's own path: where a link leads
	// outside the roots is not for the caller to learn (remote_stat shows
	// the link target itself).
	if real != abs && !underRoots(real, resolveRoots(roots)) {
		return "", fmt.Errorf("path escapes allowed roots: %s leads through a symlink outside them "+
			"(to read there, add the target directory to SHELL_API_FS_ROOT)", abs)
	}
	return abs, nil
}

// resolveError reports a failure to resolve abs without the path it failed
// on, which may lie outside the roots.
func resolveError(abs string, err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		err = pe.Err
	}
	return fmt.Errorf("cannot resolve %s: %v", abs, err)
}

// validateLinkPath is validatePath for operations on a path itself rather
// than on what it points to (stat, delete, rename): its final component may
// be a symlink leading anywhere, but its parent directories must resolve
// inside the roots.
func validateLinkPath(path string, roots []string) (string, error) {
	if path == "" {
		return "", errors.New("path is empty")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	if len(roots) == 0 {
		return abs, nil
	}
	if !underRoots(abs, roots) {
		return "", errors.New("path escapes allowed roots")
	}
	for _, root := range roots {
		if abs == filepath.Clean(root) {
			return abs, nil
		}
	}
	parent, err := resolvePath(filepath.Dir(abs), 0)
	if err != nil {
		return "", resolveError(abs, err)
	}
	if !underRoots(parent, resolveRoots(roots)) {
		return "", fmt.Errorf("path escapes allowed roots: the directory of %s leads through a symlink outside them", abs)
	}
	return abs, nil
}

// underRoots reports whether path is one of roots or lies below one.
func underRoots(path string, roots []string) bool {
	for _, root := range roots {
		root = filepath.Clean(root)
		prefix := root
		if !strings.HasSuffix(prefix, string(os.PathSeparator)) {
			prefix += string(os.PathSeparator)
		}
		if path == root || strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

// resolveRoots returns roots with their symlinks resolved (a missing root is
// resolved as far as it exists, like a write target).
func resolveRoots(roots []string) []string {
	out := make([]string, len(roots))
	for i, root := range roots {
		if real, err := resolvePath(filepath.Clean(root), 0); err == nil {
			out[i] = real
		} else {
			out[i] = filepath.Clean(root)
		}
	}
	return out
}

// maxSymlinkHops bounds how many dangling symlinks resolvePath follows.
const maxSymlinkHops = 40

// resolvePath resolves every symlink in path, which may not exist yet (a write
// target): missing trailing components are kept as written, while a dangling
// symlink is followed to where it points, since writing through it would
// create its target.
func resolvePath(path string, hops int) (string, error) {
	real, err := filepath.EvalSymlinks(path)
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		return real, err
	}
	if fi, lerr := os.Lstat(path); lerr == nil && fi.Mode()&os.ModeSymlink != 0 {
		if hops >= maxSymlinkHops {
			return "", errors.New("too many levels of symbolic links")
		}
		target, rerr := os.Readlink(path)
		if rerr != nil {
			return "", rerr
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(path), target)
		}
		return resolvePath(filepath.Clean(target), hops+1)
	}
	parent := filepath.Dir(path)
	if parent == path {
		return path, nil
	}
	rp, err := resolvePath(parent, hops)
	if err != nil {
		return "", err
	}
	return filepath.Join(rp, filepath.Base(path)), nil
}

// parseMode parses an octal mode string, defaulting to 0644 when empty.
func parseMode(mode string) (os.FileMode, error) {
	if strings.TrimSpace(mode) == "" {
		return 0644, nil
	}
	v, err := strconv.ParseUint(mode, 8, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid mode %q: %w", mode, err)
	}
	return os.FileMode(v), nil
}

type WriteFileRequest struct {
	Path     string
	Content  string // UTF-8 text
	Encoding string // target on-disk encoding, default utf-8
	Append   bool   // true appends, false overwrites
	MakeDirs bool   // true auto-creates parent dirs
	Mode     string // optional octal string like "0644", empty defaults 0644
	Lint     string // optional post-write syntax check: "bash" | "python"
	DryRun   bool   // report what would be written (with a diff) without writing
}

type WriteFileResult struct {
	Path         string      `json:"path"`
	BytesWritten int         `json:"bytes_written"`
	Created      bool        `json:"created"` // whether the file did not exist before
	Encoding     string      `json:"encoding"`
	SHA256       string      `json:"sha256"`         // digest of the bytes written this call
	Mode         string      `json:"mode"`           // resulting permission bits, e.g. "0644"
	Lint         *LintResult `json:"lint,omitempty"` // only when lint requested
	// DryRun: nothing was written; bytes_written / sha256 / created / mode
	// describe what the write would do, and Preview shows the change.
	DryRun  bool          `json:"dry_run,omitempty"`
	Preview *WritePreview `json:"preview,omitempty"`
}

// WritePreview is the dry-run view of a write: a unified diff (3 lines of
// context) from the current file content to the would-be content.
type WritePreview struct {
	Unchanged     bool   `json:"unchanged"`
	LinesAdded    int    `json:"lines_added"`
	LinesRemoved  int    `json:"lines_removed"`
	Diff          string `json:"diff,omitempty"`
	DiffTruncated bool   `json:"diff_truncated,omitempty"`
	Note          string `json:"note,omitempty"`
}

const (
	maxPreviewDiffBytes = 64 << 10
	maxPreviewFileBytes = 8 << 20
)

// WriteFileContent writes text content to a file, encoding it to the target
// encoding, optionally appending and creating parent directories.
func WriteFileContent(req WriteFileRequest, roots []string) (*WriteFileResult, error) {
	if err := guardReadOnly("write_file"); err != nil {
		return nil, err
	}
	abs, err := validatePath(req.Path, roots)
	if err != nil {
		return nil, err
	}

	mode, err := parseMode(req.Mode)
	if err != nil {
		return nil, err
	}
	if req.DryRun {
		return previewWrite(req, abs, mode)
	}

	if req.MakeDirs {
		if err := os.MkdirAll(filepath.Dir(abs), 0755); err != nil {
			return nil, err
		}
	}

	created := false
	if _, statErr := os.Stat(abs); statErr != nil {
		if os.IsNotExist(statErr) {
			created = true
		} else {
			return nil, statErr
		}
	}

	encoding := req.Encoding
	if encoding == "" {
		encoding = "utf-8"
	}
	data, err := encodeFromUTF8(req.Content, encoding)
	if err != nil {
		return nil, err
	}

	flags := os.O_WRONLY | os.O_CREATE
	if req.Append {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(abs, flags, mode)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	n, err := f.Write(data)
	if err != nil {
		return nil, err
	}

	sum := sha256.Sum256(data)
	modeStr := ""
	if fi, statErr := os.Stat(abs); statErr == nil {
		modeStr = fi.Mode().Perm().String()
	}

	result := &WriteFileResult{
		Path:         abs,
		BytesWritten: n,
		Created:      created,
		Encoding:     encoding,
		SHA256:       hex.EncodeToString(sum[:]),
		Mode:         modeStr,
	}
	if req.Lint != "" && req.Lint != "none" {
		result.Lint = LintFile(abs, req.Lint)
	}
	return result, nil
}

// previewWrite is WriteFileContent's dry run: the same validation, then a
// report of the would-be write — nothing on disk is touched.
func previewWrite(req WriteFileRequest, abs string, mode os.FileMode) (*WriteFileResult, error) {
	encoding := req.Encoding
	if encoding == "" {
		encoding = "utf-8"
	}
	data, err := encodeFromUTF8(req.Content, encoding)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	result := &WriteFileResult{
		Path:         abs,
		BytesWritten: len(data),
		Encoding:     encoding,
		SHA256:       hex.EncodeToString(sum[:]),
		DryRun:       true,
		Preview:      &WritePreview{},
	}
	if req.Lint != "" && req.Lint != "none" {
		result.Preview.Note = "lint is not run in dry_run. "
	}

	fi, statErr := os.Stat(abs)
	switch {
	case statErr == nil && fi.IsDir():
		return nil, fmt.Errorf("%s is a directory", abs)
	case statErr == nil:
		result.Mode = fi.Mode().Perm().String()
	case os.IsNotExist(statErr):
		result.Created = true
		result.Mode = mode.Perm().String()
		if dir := filepath.Dir(abs); !req.MakeDirs {
			if _, err := os.Stat(dir); err != nil {
				return nil, fmt.Errorf("parent directory %s does not exist (set make_dirs=true)", dir)
			}
		}
	default:
		return nil, statErr
	}

	var old []byte
	if !result.Created {
		if fi.Size() > maxPreviewFileBytes {
			result.Preview.Note += fmt.Sprintf("existing file is %d bytes: too large to diff.", fi.Size())
			return result, nil
		}
		if old, err = os.ReadFile(abs); err != nil {
			return nil, err
		}
		result.Preview.Unchanged = bytes.Equal(old, encodedResult(old, data, req.Append))
		if looksBinary(old) {
			result.Preview.Note += "existing file is binary: no diff."
			return result, nil
		}
	}
	oldText := ""
	if len(old) > 0 {
		if oldText, err = decodeToUTF8(old, detectEncoding(old)); err != nil {
			result.Preview.Note += "existing content could not be decoded: no diff."
			return result, nil
		}
	}
	newText := req.Content
	if req.Append {
		newText = oldText + req.Content
		result.Preview.Note += "append mode: the diff shows the file after appending."
	}
	if len(newText) > maxPreviewFileBytes {
		result.Preview.Note += "new content too large to diff."
		return result, nil
	}
	diff, added, removed, truncated := unifiedDiff(oldText, newText, maxPreviewDiffBytes)
	result.Preview.Diff = diff
	result.Preview.LinesAdded = added
	result.Preview.LinesRemoved = removed
	result.Preview.DiffTruncated = truncated
	result.Preview.Note = strings.TrimSpace(result.Preview.Note)
	return result, nil
}

// encodedResult is the file content a write of data would leave.
func encodedResult(old, data []byte, appendMode bool) []byte {
	if appendMode {
		return append(append([]byte{}, old...), data...)
	}
	return data
}

type EditPair struct {
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all,omitempty"`
}

// EditChange summarizes one applied edit (also the dry-run preview unit).
type EditChange struct {
	Index        int    `json:"index"`
	OldString    string `json:"old_string"`
	NewString    string `json:"new_string"`
	Replacements int    `json:"replacements"`
}

type EditFileRequest struct {
	Path       string
	OldString  string
	NewString  string
	ReplaceAll bool
	Encoding   string     // source encoding, empty detects; written back with same encoding
	Edits      []EditPair // multi-edit mode; wins over OldString/NewString
	UseRegex   bool       // treat old_string as an RE2 pattern (implies replace-all; new_string supports $1 groups)
	DryRun     bool       // compute and report changes without writing
	Lint       string     // optional post-write syntax check: "bash" | "python"
}

type EditFileResult struct {
	Path         string       `json:"path"`
	Replacements int          `json:"replacements"`
	Encoding     string       `json:"encoding"`
	DryRun       bool         `json:"dry_run,omitempty"`
	Changes      []EditChange `json:"changes,omitempty"`
	SHA256       string       `json:"sha256,omitempty"`
	Mode         string       `json:"mode,omitempty"`
	Lint         *LintResult  `json:"lint,omitempty"`
}

// previewString truncates long strings for change previews.
func previewString(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "..."
}

// EditFileContent applies exact-string (or RE2) replacements in a file,
// preserving its encoding and permissions. All edits are applied in memory
// first: any failure aborts the whole call without touching the file
// (atomic multi-edit). DryRun reports the would-be changes without writing.
func EditFileContent(req EditFileRequest, roots []string) (*EditFileResult, error) {
	// Refused wholesale (even for dry_run): the tool is not registered under
	// read-only mode, so reaching here means a caller bypassed registration.
	if err := guardReadOnly("edit_file"); err != nil {
		return nil, err
	}
	abs, err := validatePath(req.Path, roots)
	if err != nil {
		return nil, err
	}

	edits := req.Edits
	if len(edits) == 0 {
		if req.OldString == "" {
			return nil, errors.New("old_string is empty")
		}
		edits = []EditPair{{OldString: req.OldString, NewString: req.NewString, ReplaceAll: req.ReplaceAll}}
	}

	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, err
	}

	encoding := req.Encoding
	if encoding == "" {
		encoding = detectEncoding(data)
	}
	content, err := decodeToUTF8(data, encoding)
	if err != nil {
		return nil, err
	}

	updated := content
	changes := make([]EditChange, 0, len(edits))
	total := 0

	for i, e := range edits {
		if e.OldString == "" {
			return nil, fmt.Errorf("edit #%d: old_string is empty", i+1)
		}
		var count int
		if req.UseRegex {
			re, compErr := regexp.Compile(e.OldString)
			if compErr != nil {
				return nil, fmt.Errorf("edit #%d: invalid pattern: %w", i+1, compErr)
			}
			count = len(re.FindAllStringIndex(updated, -1))
			if count == 0 {
				return nil, fmt.Errorf("edit #%d: pattern not found", i+1)
			}
			updated = re.ReplaceAllString(updated, e.NewString)
		} else {
			count = strings.Count(updated, e.OldString)
			if count == 0 {
				return nil, fmt.Errorf("edit #%d: old_string not found", i+1)
			}
			if !e.ReplaceAll && count > 1 {
				return nil, fmt.Errorf("edit #%d: old_string is not unique (%d occurrences), set replace_all or provide more context", i+1, count)
			}
			if e.ReplaceAll {
				updated = strings.ReplaceAll(updated, e.OldString, e.NewString)
			} else {
				updated = strings.Replace(updated, e.OldString, e.NewString, 1)
				count = 1
			}
		}
		total += count
		changes = append(changes, EditChange{
			Index:        i + 1,
			OldString:    previewString(e.OldString, 200),
			NewString:    previewString(e.NewString, 200),
			Replacements: count,
		})
	}

	result := &EditFileResult{
		Path:         abs,
		Replacements: total,
		Encoding:     encoding,
		DryRun:       req.DryRun,
		Changes:      changes,
	}
	if req.DryRun {
		return result, nil
	}

	out, err := encodeFromUTF8(updated, encoding)
	if err != nil {
		return nil, err
	}

	mode := os.FileMode(0644)
	if info, statErr := os.Stat(abs); statErr == nil {
		mode = info.Mode().Perm()
	}
	if err := os.WriteFile(abs, out, mode); err != nil {
		return nil, err
	}

	sum := sha256.Sum256(out)
	result.SHA256 = hex.EncodeToString(sum[:])
	result.Mode = mode.String()
	if req.Lint != "" && req.Lint != "none" {
		result.Lint = LintFile(abs, req.Lint)
	}
	return result, nil
}

type DirEntry struct {
	Name          string `json:"name"`
	IsDir         bool   `json:"is_dir"`
	Type          string `json:"type"` // "dir" | "file" | "other"
	Size          int64  `json:"size"`
	Mode          string `json:"mode"`     // e.g. "-rw-r--r--"
	ModTime       string `json:"mod_time"` // RFC3339
	Owner         string `json:"owner,omitempty"`
	Group         string `json:"group,omitempty"`
	IsSymlink     bool   `json:"is_symlink,omitempty"`
	SymlinkTarget string `json:"symlink_target,omitempty"`
}

type ListDirRequest struct {
	Path          string
	SortBy        string   // "name" (default, ascending) | "mtime" (newest first) | "size" (largest first)
	FilterGlob    []string // keep entries whose name matches any glob
	IncludeHidden bool     // include dot-files
}

// ListDirResult puts the entries last, so a client that cuts an oversized
// result keeps the count.
type ListDirResult struct {
	Path    string     `json:"path"`
	Count   int        `json:"count"`
	Entries []DirEntry `json:"entries"`
}

// ListDirectory lists the entries of a directory with metadata including
// owner/group and symlink targets. Hidden entries are skipped unless
// IncludeHidden is set; results are filtered then sorted per request.
func ListDirectory(req ListDirRequest, roots []string) (*ListDirResult, error) {
	abs, err := validatePath(req.Path, roots)
	if err != nil {
		return nil, err
	}

	dirEntries, err := os.ReadDir(abs)
	if err != nil {
		return nil, err
	}
	filter, err := compileGlobs(req.FilterGlob, false)
	if err != nil {
		return nil, fmt.Errorf("filter_glob: %w", err)
	}

	entries := make([]DirEntry, 0, len(dirEntries))
	for _, de := range dirEntries {
		if !req.IncludeHidden && isHiddenName(de.Name()) {
			continue
		}
		if !filter.match(de.Name(), de.Name(), filepath.Join(abs, de.Name())) {
			continue
		}
		info, infoErr := de.Info() // follows symlinks: IsDir reflects the target
		if infoErr != nil {
			// Skip entries we cannot stat (e.g. removed during listing).
			continue
		}
		entry := DirEntry{
			Name:    de.Name(),
			IsDir:   info.IsDir(),
			Size:    info.Size(),
			Mode:    info.Mode().String(),
			ModTime: info.ModTime().Format(time.RFC3339),
		}
		switch {
		case info.IsDir():
			entry.Type = "dir"
		case info.Mode().IsRegular():
			entry.Type = "file"
		default:
			entry.Type = "other"
		}
		entry.Owner, entry.Group = ownerGroup(info)
		if de.Type()&os.ModeSymlink != 0 {
			entry.IsSymlink = true
			if target, linkErr := os.Readlink(filepath.Join(abs, de.Name())); linkErr == nil {
				entry.SymlinkTarget = target
			}
		}
		entries = append(entries, entry)
	}

	switch strings.ToLower(req.SortBy) {
	case "mtime":
		sort.SliceStable(entries, func(i, j int) bool { return entries[i].ModTime > entries[j].ModTime })
	case "size":
		// Files first (largest first), directories after them in name order.
		// A directory's st_size is a filesystem artifact — 4096 on ext4, 64 on
		// APFS, none of it related to the contents — so comparing it against
		// file sizes would let every directory outrank real files and defeat
		// the purpose of this sort. Excluding dirs from the comparison also
		// makes the order identical across platforms.
		sort.SliceStable(entries, func(i, j int) bool {
			if entries[i].IsDir != entries[j].IsDir {
				return !entries[i].IsDir
			}
			if entries[i].IsDir {
				return false // both dirs: keep ReadDir's name order
			}
			return entries[i].Size > entries[j].Size
		})
	default: // name ascending
		sort.SliceStable(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	}

	return &ListDirResult{
		Path:    abs,
		Entries: entries,
		Count:   len(entries),
	}, nil
}

type StatRequest struct {
	Path            string
	IncludeHash     string // "md5" | "sha256" | "" (none)
	IncludeEncoding bool   // detect text encoding of regular files
	IncludeText     bool   // text facts of regular files: line count, line endings, BOM (wc -l / file)
}

// TextInfo holds the text facts of a regular file (remote_stat include_text):
// what `wc -l`, `file` and a BOM/CRLF probe used to be run for.
type TextInfo struct {
	Lines        int    `json:"lines"`                  // like wc -l, plus a final line without a newline
	LineEndings  string `json:"line_endings,omitempty"` // "lf" | "crlf" | "mixed" | "cr" | "none"
	FinalNewline bool   `json:"final_newline"`
	BOM          string `json:"bom,omitempty"` // "utf-8" | "utf-16le" | "utf-16be"
	Encoding     string `json:"encoding,omitempty"`
	MaxLineLen   int    `json:"max_line_length"` // bytes
	Skipped      string `json:"skipped,omitempty"`
}

type StatResult struct {
	Path          string `json:"path"`
	Exists        bool   `json:"exists"`
	IsDir         bool   `json:"is_dir"`
	Size          int64  `json:"size"`
	Mode          string `json:"mode"`
	ModTime       string `json:"mod_time"`
	IsBinary      bool   `json:"is_binary,omitempty"` // sniffed only for regular files (first 8000 bytes)
	Owner         string `json:"owner,omitempty"`
	Group         string `json:"group,omitempty"`
	Nlink         uint64 `json:"nlink,omitempty"`
	IsSymlink     bool   `json:"is_symlink,omitempty"`
	SymlinkTarget string `json:"symlink_target,omitempty"`
	// TargetOutsideRoots: the path is a symlink whose target lies outside the
	// FS roots; only the link itself is described.
	TargetOutsideRoots bool      `json:"target_outside_roots,omitempty"`
	Encoding           string    `json:"encoding,omitempty"`     // only when include_encoding=true
	Hash               string    `json:"hash,omitempty"`         // hex digest, only when include_hash set
	HashAlgo           string    `json:"hash_algo,omitempty"`    // "md5" | "sha256"
	HashSkipped        bool      `json:"hash_skipped,omitempty"` // file exceeded the hash size cap
	Text               *TextInfo `json:"text,omitempty"`         // only when include_text=true
	Error              string    `json:"error,omitempty"`        // multi-path stat: why this path failed
}

// maxStatHashBytes caps files eligible for content hashing.
const maxStatHashBytes = 256 << 20 // 256MB

// StatFile returns metadata about a path, optionally with content hash and
// detected encoding. A missing path yields Exists:false without an error.
// Symlinks are reported as themselves (Lstat) with their target.
func StatFile(req StatRequest, roots []string) (*StatResult, error) {
	abs, err := validateLinkPath(req.Path, roots)
	if err != nil {
		return nil, err
	}

	result := &StatResult{Path: abs}

	info, err := os.Lstat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			result.Exists = false
			return result, nil
		}
		return nil, err
	}

	result.Exists = true
	result.IsSymlink = info.Mode()&os.ModeSymlink != 0
	if result.IsSymlink {
		if target, linkErr := os.Readlink(abs); linkErr == nil {
			result.SymlinkTarget = target
		}
		// The link itself may be stat'ed, but a target outside the roots is
		// reported as the link alone: its content (hash, encoding) and its
		// metadata stay out of reach.
		if _, err := validatePath(abs, roots); err != nil {
			result.Size = info.Size()
			result.Mode = info.Mode().String()
			result.ModTime = info.ModTime().Format(time.RFC3339)
			result.TargetOutsideRoots = true
			return result, nil
		}
	}

	// Type/size/mode report the link target when the path is a symlink,
	// matching how callers think about "the file at this path".
	statInfo := info
	if result.IsSymlink {
		if targetInfo, statErr := os.Stat(abs); statErr == nil {
			statInfo = targetInfo
		}
	}

	result.IsDir = statInfo.IsDir()
	result.Size = statInfo.Size()
	result.Mode = statInfo.Mode().String()
	result.ModTime = statInfo.ModTime().Format(time.RFC3339)
	result.Owner, result.Group = ownerGroup(statInfo)
	result.Nlink = nlinkOf(statInfo)

	if !statInfo.Mode().IsRegular() {
		return result, nil
	}

	algo := strings.ToLower(strings.TrimSpace(req.IncludeHash))
	if algo != "" && algo != "md5" && algo != "sha256" {
		return nil, fmt.Errorf("invalid include_hash %q (want md5|sha256)", req.IncludeHash)
	}
	if algo != "" {
		if statInfo.Size() > maxStatHashBytes {
			result.HashSkipped = true
		} else {
			digest, hashErr := hashFile(abs, algo)
			if hashErr != nil {
				return nil, hashErr
			}
			result.Hash = digest
			result.HashAlgo = algo
		}
	}

	// Binary sniff + optional encoding detection share one head read.
	var head []byte
	if f, openErr := os.Open(abs); openErr == nil {
		buf := make([]byte, binarySniffLen)
		n, _ := f.Read(buf)
		f.Close()
		head = buf[:n]
	}
	result.IsBinary = looksBinary(head)
	if req.IncludeEncoding && !result.IsBinary {
		result.Encoding = detectEncoding(head)
	}
	if req.IncludeText {
		result.Text = textInfo(abs, statInfo.Size(), result.IsBinary, head)
	}

	return result, nil
}

// maxTextScanBytes caps files scanned for include_text.
const maxTextScanBytes = 256 << 20

// textInfo scans a regular file for its line count, line endings and BOM.
func textInfo(path string, size int64, binary bool, head []byte) *TextInfo {
	ti := &TextInfo{}
	switch {
	case binary:
		ti.Skipped = "binary file"
		return ti
	case size > maxTextScanBytes:
		ti.Skipped = fmt.Sprintf("larger than %d MB", maxTextScanBytes>>20)
		return ti
	}
	switch {
	case bytes.HasPrefix(head, []byte{0xEF, 0xBB, 0xBF}):
		ti.BOM = "utf-8"
	case bytes.HasPrefix(head, []byte{0xFF, 0xFE}):
		ti.BOM = "utf-16le"
	case bytes.HasPrefix(head, []byte{0xFE, 0xFF}):
		ti.BOM = "utf-16be"
	}
	ti.Encoding = detectEncoding(head)
	f, err := os.Open(path)
	if err != nil {
		ti.Skipped = err.Error()
		return ti
	}
	defer f.Close()
	var lf, crlf, cr, lineLen int
	var prev, last byte
	buf := make([]byte, 64<<10)
	for {
		n, err := f.Read(buf)
		for _, c := range buf[:n] {
			switch c {
			case '\n':
				lf++
				if prev == '\r' {
					crlf++
				}
				ti.MaxLineLen = max(ti.MaxLineLen, lineLen)
				lineLen = 0
			case '\r':
			default:
				if prev == '\r' {
					cr++
				}
				lineLen++
			}
			prev, last = c, c
		}
		if err != nil {
			break
		}
	}
	ti.MaxLineLen = max(ti.MaxLineLen, lineLen)
	ti.Lines = lf
	ti.FinalNewline = last == '\n'
	if size > 0 && !ti.FinalNewline {
		ti.Lines++
	}
	switch {
	case lf == 0 && cr == 0:
		ti.LineEndings = "none"
	case cr > 0 && lf == 0:
		ti.LineEndings = "cr"
	case crlf == lf && cr == 0:
		ti.LineEndings = "crlf"
	case crlf == 0 && cr == 0:
		ti.LineEndings = "lf"
	default:
		ti.LineEndings = "mixed"
	}
	return ti
}

// maxStatPaths bounds a multi-path remote_stat call.
const maxStatPaths = 200

// StatFiles stats several paths; a failing path carries its error in place.
func StatFiles(paths []string, req StatRequest, roots []string) ([]*StatResult, error) {
	if len(paths) > maxStatPaths {
		return nil, fmt.Errorf("paths: at most %d per call, got %d", maxStatPaths, len(paths))
	}
	out := make([]*StatResult, 0, len(paths))
	for _, p := range paths {
		r := req
		r.Path = p
		st, err := StatFile(r, roots)
		if err != nil {
			st = &StatResult{Path: p, Error: err.Error()}
		}
		out = append(out, st)
	}
	return out, nil
}

// hashFile streams a file through the named hash algorithm.
func hashFile(path, algo string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	var h hash.Hash
	switch algo {
	case "md5":
		h = md5.New()
	default:
		h = sha256.New()
	}
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

type UploadBase64Request struct {
	Path     string
	DataB64  string // base64-encoded content
	Append   bool   // append for chunked upload
	MakeDirs bool
	Mode     string
}

type UploadBase64Result struct {
	Path         string `json:"path"`
	BytesWritten int    `json:"bytes_written"`
	Created      bool   `json:"created"`
	SHA256       string `json:"sha256"` // digest of the bytes written this call
	Mode         string `json:"mode"`
}

// UploadBase64 decodes base64 content and writes it to a file, optionally
// appending (for chunked uploads) and creating parent directories.
func UploadBase64(req UploadBase64Request, roots []string) (*UploadBase64Result, error) {
	if err := guardReadOnly("upload_base64"); err != nil {
		return nil, err
	}
	abs, err := validatePath(req.Path, roots)
	if err != nil {
		return nil, err
	}

	data, err := base64.StdEncoding.DecodeString(req.DataB64)
	if err != nil {
		return nil, fmt.Errorf("invalid base64 data: %w", err)
	}

	mode, err := parseMode(req.Mode)
	if err != nil {
		return nil, err
	}

	if req.MakeDirs {
		if err := os.MkdirAll(filepath.Dir(abs), 0755); err != nil {
			return nil, err
		}
	}

	created := false
	if _, statErr := os.Stat(abs); statErr != nil {
		if os.IsNotExist(statErr) {
			created = true
		} else {
			return nil, statErr
		}
	}

	flags := os.O_WRONLY | os.O_CREATE
	if req.Append {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(abs, flags, mode)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	n, err := f.Write(data)
	if err != nil {
		return nil, err
	}

	sum := sha256.Sum256(data)
	modeStr := ""
	if fi, statErr := os.Stat(abs); statErr == nil {
		modeStr = fi.Mode().Perm().String()
	}

	return &UploadBase64Result{
		Path:         abs,
		BytesWritten: n,
		Created:      created,
		SHA256:       hex.EncodeToString(sum[:]),
		Mode:         modeStr,
	}, nil
}

type DownloadBase64Request struct {
	Path     string
	Offset   int64 // starting byte offset, supports chunked download
	MaxBytes int   // max bytes this call, 0 uses default (4MB)
}

// DownloadBase64Result puts the payload last, so a client that cuts an
// oversized result keeps total_size / eof and can ask for smaller chunks.
type DownloadBase64Result struct {
	Path      string `json:"path"`
	BytesRead int    `json:"bytes_read"`
	Offset    int64  `json:"offset"`
	TotalSize int64  `json:"total_size"`
	EOF       bool   `json:"eof"` // whether the end of file was reached
	DataB64   string `json:"data_b64"`
}

// DownloadBase64 reads a byte range from a file starting at Offset and
// returns it base64-encoded, supporting chunked downloads.
func DownloadBase64(req DownloadBase64Request, roots []string) (*DownloadBase64Result, error) {
	abs, err := validatePath(req.Path, roots)
	if err != nil {
		return nil, err
	}

	f, err := os.Open(abs)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	info, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	totalSize := info.Size()

	offset := req.Offset
	if offset < 0 {
		offset = 0
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, err
	}

	maxBytes := req.MaxBytes
	if maxBytes <= 0 {
		maxBytes = defaultDownloadMaxBytes
	}
	buf := make([]byte, maxBytes)
	n, err := io.ReadFull(f, buf)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return nil, err
	}
	buf = buf[:n]

	eof := offset+int64(n) >= totalSize

	return &DownloadBase64Result{
		Path:      abs,
		DataB64:   base64.StdEncoding.EncodeToString(buf),
		BytesRead: n,
		Offset:    offset,
		TotalSize: totalSize,
		EOF:       eof,
	}, nil
}
