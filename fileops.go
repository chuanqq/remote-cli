package main

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
}

type WriteFileResult struct {
	Path         string `json:"path"`
	BytesWritten int    `json:"bytes_written"`
	Created      bool   `json:"created"` // whether the file did not exist before
	Encoding     string `json:"encoding"`
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

	return &WriteFileResult{
		Path:         abs,
		BytesWritten: n,
		Created:      created,
		Encoding:     encoding,
	}, nil
}

type ReadFileRequest struct {
	Path      string
	Encoding  string // source encoding; empty auto-detects
	StartLine int    // 1-based, 0 or negative means from start
	EndLine   int    // 1-based inclusive; 0 or negative means to end
	MaxBytes  int    // read cap, 0 uses default 1MB
}

type ReadFileResult struct {
	Path          string `json:"path"`
	Content       string `json:"content"`  // UTF-8
	Encoding      string `json:"encoding"` // actual source encoding used
	IsBinary      bool   `json:"is_binary"`
	Truncated     bool   `json:"truncated"`
	TotalLines    int    `json:"total_lines,omitempty"`
	ReturnedLines int    `json:"returned_lines,omitempty"`
}

// ReadFileContent reads a file, decodes it to UTF-8, and optionally slices
// by 1-based line range. Binary files are reported without decoding.
func ReadFileContent(req ReadFileRequest, roots []string) (*ReadFileResult, error) {
	abs, err := validatePath(req.Path, roots)
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, err
	}

	maxBytes := req.MaxBytes
	if maxBytes <= 0 {
		maxBytes = defaultReadMaxBytes
	}
	truncated := false
	if len(data) > maxBytes {
		data = data[:maxBytes]
		truncated = true
	}

	result := &ReadFileResult{
		Path:      abs,
		Truncated: truncated,
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

	// Slice by line range only when requested.
	if req.StartLine > 0 || req.EndLine > 0 {
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
	} else {
		result.Content = content
	}

	return result, nil
}

type EditFileRequest struct {
	Path       string
	OldString  string
	NewString  string
	ReplaceAll bool
	Encoding   string // source encoding, empty detects; written back with same encoding
}

type EditFileResult struct {
	Path         string `json:"path"`
	Replacements int    `json:"replacements"`
	Encoding     string `json:"encoding"`
}

// EditFileContent performs an exact string replacement in a file, preserving
// the file's encoding and permissions.
func EditFileContent(req EditFileRequest, roots []string) (*EditFileResult, error) {
	abs, err := validatePath(req.Path, roots)
	if err != nil {
		return nil, err
	}

	if req.OldString == "" {
		return nil, errors.New("old_string is empty")
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

	count := strings.Count(content, req.OldString)
	if count == 0 {
		return nil, errors.New("old_string not found")
	}
	if !req.ReplaceAll && count > 1 {
		return nil, fmt.Errorf("old_string is not unique (%d occurrences), set replace_all or provide more context", count)
	}

	var updated string
	if req.ReplaceAll {
		updated = strings.ReplaceAll(content, req.OldString, req.NewString)
	} else {
		updated = strings.Replace(content, req.OldString, req.NewString, 1)
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

	replacements := count
	if !req.ReplaceAll {
		replacements = 1
	}
	return &EditFileResult{
		Path:         abs,
		Replacements: replacements,
		Encoding:     encoding,
	}, nil
}

type DirEntry struct {
	Name    string `json:"name"`
	IsDir   bool   `json:"is_dir"`
	Size    int64  `json:"size"`
	Mode    string `json:"mode"`     // e.g. "-rw-r--r--"
	ModTime string `json:"mod_time"` // RFC3339
}

type ListDirResult struct {
	Path    string     `json:"path"`
	Entries []DirEntry `json:"entries"`
	Count   int        `json:"count"`
}

// ListDirectory lists the entries of a directory with basic metadata.
func ListDirectory(path string, roots []string) (*ListDirResult, error) {
	abs, err := validatePath(path, roots)
	if err != nil {
		return nil, err
	}

	dirEntries, err := os.ReadDir(abs)
	if err != nil {
		return nil, err
	}

	entries := make([]DirEntry, 0, len(dirEntries))
	for _, de := range dirEntries {
		info, infoErr := de.Info()
		if infoErr != nil {
			// Skip entries we cannot stat (e.g. removed during listing).
			continue
		}
		entries = append(entries, DirEntry{
			Name:    de.Name(),
			IsDir:   de.IsDir(),
			Size:    info.Size(),
			Mode:    info.Mode().String(),
			ModTime: info.ModTime().Format(time.RFC3339),
		})
	}

	return &ListDirResult{
		Path:    abs,
		Entries: entries,
		Count:   len(entries),
	}, nil
}

type StatResult struct {
	Path     string `json:"path"`
	Exists   bool   `json:"exists"`
	IsDir    bool   `json:"is_dir"`
	Size     int64  `json:"size"`
	Mode     string `json:"mode"`
	ModTime  string `json:"mod_time"`
	IsBinary bool   `json:"is_binary,omitempty"` // sniffed only for regular files (first 8000 bytes)
}

// StatFile returns metadata about a path. A missing path yields
// Exists:false without an error.
func StatFile(path string, roots []string) (*StatResult, error) {
	abs, err := validatePath(path, roots)
	if err != nil {
		return nil, err
	}

	result := &StatResult{Path: abs}

	info, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			result.Exists = false
			return result, nil
		}
		return nil, err
	}

	result.Exists = true
	result.IsDir = info.IsDir()
	result.Size = info.Size()
	result.Mode = info.Mode().String()
	result.ModTime = info.ModTime().Format(time.RFC3339)

	if info.Mode().IsRegular() {
		if f, openErr := os.Open(abs); openErr == nil {
			buf := make([]byte, binarySniffLen)
			n, _ := f.Read(buf)
			f.Close()
			result.IsBinary = looksBinary(buf[:n])
		}
	}

	return result, nil
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

	return &UploadBase64Result{
		Path:         abs,
		BytesWritten: n,
		Created:      created,
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
