package main

import (
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
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
	defaultReadMaxBytes     = 1048576 // 1MB
	defaultDownloadMaxBytes = 4194304 // 4MB
	binarySniffLen          = 8000
)

// decodeToUTF8 converts raw bytes in the given encoding to a UTF-8 string.
// Supported: utf-8/"" (returned as-is), gbk, gb2312, gb18030.
func decodeToUTF8(data []byte, encoding string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "", "utf-8", "utf8":
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
// Supported: utf-8/"" (returned as-is), gbk, gb2312, gb18030.
func encodeFromUTF8(s string, encoding string) ([]byte, error) {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "", "utf-8", "utf8":
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

// detectEncoding does a best-effort guess: valid UTF-8 -> "utf-8",
// else decodable as GBK -> "gbk", else "unknown".
func detectEncoding(data []byte) string {
	if utf8.Valid(data) {
		return "utf-8"
	}
	if _, _, err := transform.Bytes(simplifiedchinese.GBK.NewDecoder(), data); err == nil {
		return "gbk"
	}
	return "unknown"
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
// non-empty, the resolved path must stay within at least one of the roots
// (sandbox). An empty roots slice means no filesystem restriction.
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
	for _, root := range roots {
		cleanRoot := filepath.Clean(root)
		if abs == cleanRoot || strings.HasPrefix(abs, cleanRoot+string(os.PathSeparator)) {
			return abs, nil
		}
	}
	return "", errors.New("path escapes allowed roots")
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
}

type WriteFileResult struct {
	Path         string      `json:"path"`
	BytesWritten int         `json:"bytes_written"`
	Created      bool        `json:"created"` // whether the file did not exist before
	Encoding     string      `json:"encoding"`
	SHA256       string      `json:"sha256"`          // digest of the bytes written this call
	Mode         string      `json:"mode"`            // resulting permission bits, e.g. "0644"
	Lint         *LintResult `json:"lint,omitempty"` // only when lint requested
}

// WriteFileContent writes text content to a file, encoding it to the target
// encoding, optionally appending and creating parent directories.
func WriteFileContent(req WriteFileRequest, roots []string) (*WriteFileResult, error) {
	abs, err := validatePath(req.Path, roots)
	if err != nil {
		return nil, err
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

type ReadFileRequest struct {
	Path         string
	Encoding     string // source encoding; empty auto-detects
	StartLine    int    // 1-based, 0 or negative means from start
	EndLine      int    // 1-based inclusive; 0 or negative means to end
	MaxBytes     int    // read cap, 0 uses default 1MB
	TailLines    int    // >0: read the last N lines (seek-based, wins over line range)
	OffsetBytes  int64  // start reading at this byte offset
	TruncateMode string // "head" (default) keeps the first max_bytes; "tail" keeps the last
}

type ReadFileResult struct {
	Path          string `json:"path"`
	Content       string `json:"content"`  // UTF-8
	Encoding      string `json:"encoding"` // actual source encoding used
	IsBinary      bool   `json:"is_binary"`
	Truncated     bool   `json:"truncated"`
	TotalLines    int    `json:"total_lines,omitempty"`
	ReturnedLines int    `json:"returned_lines,omitempty"`
	TotalSize     int64  `json:"total_size"` // file size in bytes
	Offset        int64  `json:"offset"`     // byte offset the returned content starts at
}

// readHeadBytes reads up to maxBytes from the current position of f.
func readHeadBytes(f *os.File, maxBytes int) ([]byte, error) {
	buf := make([]byte, maxBytes)
	n, err := io.ReadFull(f, buf)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return nil, err
	}
	return buf[:n], nil
}

// ReadFileContent reads a file, decodes it to UTF-8, and optionally slices
// by 1-based line range or tail line count. Reads are seek/limit based so
// large files are never loaded wholesale. Binary files are reported without
// decoding.
func ReadFileContent(req ReadFileRequest, roots []string) (*ReadFileResult, error) {
	abs, err := validatePath(req.Path, roots)
	if err != nil {
		return nil, err
	}

	f, err := os.Open(abs)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return nil, errors.New("path is a directory, use remote_list_dir")
	}
	totalSize := info.Size()

	maxBytes := req.MaxBytes
	if maxBytes <= 0 {
		maxBytes = defaultReadMaxBytes
	}

	result := &ReadFileResult{Path: abs, TotalSize: totalSize}

	var data []byte

	switch {
	case req.TailLines > 0:
		// Seek-based tail: last N lines without reading the whole file.
		var capped bool
		data, result.Offset, capped, err = readTailBytes(f, totalSize, req.TailLines, maxBytes)
		if err != nil {
			return nil, err
		}
		// Consistent with head-mode semantics: the returned window does not
		// cover the whole file (either by design or by byte cap).
		result.Truncated = capped || result.Offset > 0

	case req.StartLine > 0 || req.EndLine > 0:
		// Line-range mode: cap from the head, then slice by line numbers.
		data, err = readHeadBytes(f, maxBytes)
		if err != nil {
			return nil, err
		}
		result.Truncated = totalSize > int64(len(data))

	default:
		offset := req.OffsetBytes
		if offset < 0 {
			offset = 0
		}
		if strings.EqualFold(req.TruncateMode, "tail") && totalSize-int64(maxBytes) > offset {
			offset = totalSize - int64(maxBytes)
		}
		if offset > totalSize {
			offset = totalSize
		}
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return nil, err
		}
		data, err = readHeadBytes(f, maxBytes)
		if err != nil {
			return nil, err
		}
		result.Offset = offset
		result.Truncated = offset+int64(len(data)) < totalSize
	}

	if looksBinary(data) {
		result.IsBinary = true
		result.Content = "<binary file, use remote_download_base64>"
		return result, nil
	}

	encoding := req.Encoding
	if encoding == "" {
		encoding = detectEncoding(data)
	}
	content, err := decodeToUTF8(data, encoding)
	if err != nil {
		return nil, err
	}
	result.Encoding = encoding

	switch {
	case req.StartLine > 0 || req.EndLine > 0:
		// Slice by line range only when requested.
		lines := strings.Split(content, "\n")
		total := len(lines)
		result.TotalLines = total

		start := req.StartLine
		if start <= 0 {
			start = 1
		}
		end := req.EndLine
		if end <= 0 || end > total {
			end = total
		}
		if start > total || start > end {
			result.Content = ""
			result.ReturnedLines = 0
			return result, nil
		}
		selected := lines[start-1 : end]
		result.Content = strings.Join(selected, "\n")
		result.ReturnedLines = len(selected)
	case req.TailLines > 0:
		lines := splitLinesRaw([]byte(content))
		if len(lines) > req.TailLines {
			lines = lines[len(lines)-req.TailLines:]
		}
		result.Content = strings.Join(lines, "\n")
		result.ReturnedLines = len(lines)
	default:
		result.Content = content
	}

	return result, nil
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

type ListDirResult struct {
	Path    string     `json:"path"`
	Entries []DirEntry `json:"entries"`
	Count   int        `json:"count"`
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

	entries := make([]DirEntry, 0, len(dirEntries))
	for _, de := range dirEntries {
		if !req.IncludeHidden && isHiddenName(de.Name()) {
			continue
		}
		if !matchAnyGlob(de.Name(), req.FilterGlob, false) {
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
		sort.SliceStable(entries, func(i, j int) bool { return entries[i].Size > entries[j].Size })
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
	Encoding      string `json:"encoding,omitempty"`   // only when include_encoding=true
	Hash          string `json:"hash,omitempty"`       // hex digest, only when include_hash set
	HashAlgo      string `json:"hash_algo,omitempty"`  // "md5" | "sha256"
	HashSkipped   bool   `json:"hash_skipped,omitempty"` // file exceeded the hash size cap
}

// maxStatHashBytes caps files eligible for content hashing.
const maxStatHashBytes = 256 << 20 // 256MB

// StatFile returns metadata about a path, optionally with content hash and
// detected encoding. A missing path yields Exists:false without an error.
// Symlinks are reported as themselves (Lstat) with their target.
func StatFile(req StatRequest, roots []string) (*StatResult, error) {
	abs, err := validatePath(req.Path, roots)
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

	return result, nil
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

type DownloadBase64Result struct {
	Path      string `json:"path"`
	DataB64   string `json:"data_b64"`
	BytesRead int    `json:"bytes_read"`
	Offset    int64  `json:"offset"`
	TotalSize int64  `json:"total_size"`
	EOF       bool   `json:"eof"` // whether the end of file was reached
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
