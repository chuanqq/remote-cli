package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"
)

const (
	defaultTailLines    = 100
	maxTailLines        = 10000
	defaultTailMaxBytes = 64 << 10 // one page: Claude Code cuts MCP results at ~100k characters
	tailBlockSize       = 32 << 10
	maxFollowSeconds    = 300
	followPollInterval  = 500 * time.Millisecond
	// maxTailScanBytes bounds the backward scan of tail mode with a filter
	// (the last N matching lines may lie far before the last N lines).
	maxTailScanBytes = 64 << 20
)

type TailLogRequest struct {
	Path          string
	Lines         int    // tail mode: last N lines (with FilterRegex: last N matching lines; default 100, max 10000)
	FilterRegex   string // optional: keep only matching lines
	SinceLine     int    // line cursor: return lines after this 1-based number
	SinceOffset   int64  // byte cursor: return content after this offset (wins over SinceLine)
	FollowSeconds int    // wait up to N seconds for new content (max 300)
	Encoding      string // source encoding; empty auto-detects
	MaxBytes      int    // cap on collected content bytes (default 64KB)
}

// TailLogResult puts content last, so a client that cuts an oversized result
// keeps the cursors (see ReadFileResult).
type TailLogResult struct {
	Path        string `json:"path"`
	Encoding    string `json:"encoding"`
	StartLine   int    `json:"start_line,omitempty"`  // first returned line (line-cursor mode)
	EndLine     int    `json:"end_line"`              // last line of file seen; next since_line cursor
	TotalLines  int    `json:"total_lines,omitempty"` // line-cursor mode only (requires full scan)
	StartOffset int64  `json:"start_offset"`          // byte offset of returned content
	EndOffset   int64  `json:"end_offset"`            // byte offset after last byte read; next since_offset cursor
	Size        int64  `json:"size"`                  // file size at read time
	Truncated   bool   `json:"truncated"`             // content capped by max_bytes / max lines / scan limit
	Filtered    bool   `json:"filtered"`              // filter_regex was applied
	Followed    bool   `json:"followed"`              // follow wait was engaged
	TimedOut    bool   `json:"timed_out"`             // follow elapsed without new content
	Rotated     bool   `json:"rotated,omitempty"`     // file shrank below since_offset; read restarted from head
	IsBinary    bool   `json:"is_binary,omitempty"`
	Hint        string `json:"hint,omitempty"`
	Content     string `json:"content"`

	cursor string // "offset" | "line" | "" (tail mode): for format=text
}

// splitLinesRaw splits bytes into lines, dropping a trailing empty segment
// after a final newline and trimming CR. Returned strings share no state
// with data beyond the call.
func splitLinesRaw(data []byte) []string {
	s := string(data)
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil
	}
	parts := strings.Split(s, "\n")
	for i := range parts {
		parts[i] = strings.TrimSuffix(parts[i], "\r")
	}
	return parts
}

// readTailBytes reads enough bytes from the end of f to cover the last n
// lines, capped at maxBytes. The returned data starts on a line boundary
// unless the byte cap was hit or the file head was reached.
func readTailBytes(f *os.File, size int64, n int, maxBytes int) (data []byte, startOffset int64, truncated bool, err error) {
	if size == 0 || n <= 0 {
		return nil, size, false, nil
	}

	var buf []byte
	offset := size
	for offset > 0 && len(buf) < maxBytes {
		block := int64(tailBlockSize)
		if block > offset {
			block = offset
		}
		if int64(len(buf))+block > int64(maxBytes) {
			block = int64(maxBytes) - int64(len(buf))
		}
		chunk := make([]byte, block)
		if _, err = f.ReadAt(chunk, offset-block); err != nil && err != io.EOF {
			return nil, 0, false, err
		}
		buf = append(chunk, buf...)
		offset -= block
		if countNewlines(buf) > n {
			break
		}
	}

	if offset > 0 && len(buf) >= maxBytes {
		// Stopped early because of the byte cap, before reaching the file
		// head; the caller may be missing lines it asked for.
		truncated = true
	}

	// Find the start of the last n lines.
	pos := 0
	if nl := countNewlines(buf); nl > n {
		skip := nl - n // start after the skip-th newline
		seen := 0
		for i, b := range buf {
			if b == '\n' {
				seen++
				if seen == skip {
					pos = i + 1
					break
				}
			}
		}
	} else if truncated {
		// Cap hit mid-file without enough lines: drop the partial first line.
		if i := indexByte(buf, '\n'); i >= 0 {
			pos = i + 1
		}
	}

	return buf[pos:], offset + int64(pos), truncated, nil
}

func countNewlines(b []byte) int {
	n := 0
	for _, c := range b {
		if c == '\n' {
			n++
		}
	}
	return n
}

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}

// TailLog reads the tail of a (typically large, growing) log file without
// loading it into memory. It supports line and byte cursors for incremental
// polling, an optional follow wait for new content, and regex filtering.
func TailLog(req TailLogRequest, roots []string) (*TailLogResult, error) {
	abs, err := validatePath(req.Path, roots)
	if err != nil {
		return nil, err
	}

	lines := req.Lines
	if lines <= 0 {
		lines = defaultTailLines
	}
	if lines > maxTailLines {
		lines = maxTailLines
	}
	maxBytes := req.MaxBytes
	if maxBytes <= 0 {
		maxBytes = defaultTailMaxBytes
	}
	maxBytes = min(maxBytes, maxReadMaxBytes) // the read buffer is allocated up front

	var filter *regexp.Regexp
	if req.FilterRegex != "" {
		filter, err = regexp.Compile(req.FilterRegex)
		if err != nil {
			return nil, fmt.Errorf("invalid filter_regex: %w", err)
		}
	}

	follow := req.FollowSeconds
	if follow < 0 {
		follow = 0
	}
	if follow > maxFollowSeconds {
		follow = maxFollowSeconds
	}

	readOnce := func() (*TailLogResult, bool, error) {
		return tailReadOnce(abs, req, lines, maxBytes, filter)
	}

	res, hasNew, err := readOnce()
	if err != nil {
		return nil, err
	}
	if follow > 0 {
		deadline := time.Now().Add(time.Duration(follow) * time.Second)
		initialSize := res.Size
		for {
			if req.SinceLine > 0 || req.SinceOffset > 0 {
				if hasNew {
					break
				}
			} else {
				// Tail mode: "new content" means the file changed since the
				// first read; re-read so the new lines show up in the tail.
				if st, statErr := os.Stat(abs); statErr == nil && st.Size() != initialSize {
					res, _, err = readOnce()
					if err != nil {
						return nil, err
					}
					break
				}
			}
			if time.Now().After(deadline) {
				res.TimedOut = true
				break
			}
			time.Sleep(followPollInterval)
			if req.SinceLine > 0 || req.SinceOffset > 0 {
				res, hasNew, err = readOnce()
				if err != nil {
					return nil, err
				}
			}
		}
		// Set after the loop: re-reads replace res wholesale.
		res.Followed = true
	}
	if filter != nil && res.Content == "" && !res.IsBinary {
		if h := patternHint(req.FilterRegex); h != "" {
			res.Hint = strings.TrimSpace(res.Hint + " " + h)
		}
	}
	return res, nil
}

// tailReadOnce performs a single read pass and reports whether new content
// relative to the request cursor was found.
func tailReadOnce(abs string, req TailLogRequest, lines, maxBytes int, filter *regexp.Regexp) (*TailLogResult, bool, error) {
	f, err := os.Open(abs)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, false, err
	}
	if info.IsDir() {
		return nil, false, errors.New("path is a directory")
	}
	size := info.Size()

	res := &TailLogResult{Path: abs, Size: size}
	var raw []byte
	var hasNew bool

	switch {
	case req.SinceOffset > 0:
		// Byte-cursor mode: read everything after the offset.
		res.cursor = "offset"
		offset := req.SinceOffset
		if offset > size {
			offset = size
		}
		if size < req.SinceOffset {
			// File shrank (log rotation/truncation): restart from head.
			offset = 0
			res.Rotated = true
		}
		toRead := size - offset
		if toRead > int64(maxBytes) {
			toRead = int64(maxBytes)
			res.Truncated = true
		}
		buf := make([]byte, toRead)
		n, err := f.ReadAt(buf, offset)
		if err != nil && err != io.EOF {
			return nil, false, err
		}
		raw = buf[:n]
		if res.Truncated {
			// Capped by max_bytes: end on a line boundary when the window holds
			// one in its second half, and never inside a character, so the next
			// call (since_offset=end_offset) resumes cleanly.
			if i := bytes.LastIndexByte(raw, '\n'); i >= 0 && i >= len(raw)/2 {
				raw = raw[:i+1]
			}
			enc := req.Encoding
			if enc == "" {
				enc = detectEncoding(raw)
			}
			// A window smaller than one character is kept as it is, so that
			// the cursor still advances.
			if _, e := windowEdges(raw, enc, false, true); e > 0 {
				raw = raw[:e]
			}
			res.Hint = fmt.Sprintf("max_bytes reached: %d bytes after end_offset not shown yet; call again with since_offset=%d",
				size-offset-int64(len(raw)), offset+int64(len(raw)))
		}
		res.StartOffset = offset
		res.EndOffset = offset + int64(len(raw))
		hasNew = len(raw) > 0 || res.Rotated

	case req.SinceLine > 0:
		// Line-cursor mode: full streaming scan, counting line numbers.
		res.cursor = "line"
		collected, total, truncated, err := collectLinesAfter(f, req.SinceLine, maxTailLines, maxBytes)
		if err != nil {
			return nil, false, err
		}
		res.TotalLines = total
		res.EndLine = total
		res.Truncated = truncated
		if len(collected) > 0 {
			res.StartLine = req.SinceLine + 1
		}
		if truncated {
			// end_line is the cursor of the next call: past the lines
			// returned, not past the whole file, or the rest would be skipped.
			res.EndLine = req.SinceLine + len(collected)
			res.Hint = fmt.Sprintf("max_bytes / %d lines reached: %d lines not shown yet; call again with since_line=%d",
				maxTailLines, total-res.EndLine, res.EndLine)
		}
		hasNew = total > req.SinceLine
		return finishTail(res, nil, collected, req, filter), hasNew, nil

	default:
		if filter != nil {
			// With a filter, "last N lines" means the last N matching ones.
			res, err := tailMatches(f, res, lines, maxBytes, filter, req.Encoding)
			return res, true, err
		}
		// Tail mode: seek from the end for the last N lines.
		data, startOffset, truncated, err := readTailBytes(f, size, lines, maxBytes)
		if err != nil {
			return nil, false, err
		}
		raw = data
		res.StartOffset = startOffset
		res.EndOffset = size
		res.Truncated = truncated
		hasNew = true // no cursor; always considered new
	}

	return finishTail(res, raw, nil, req, filter), hasNew, nil
}

// collectLinesAfter streams f line by line and returns the lines after
// lineCursor (1-based), plus the total line count of the file.
func collectLinesAfter(f *os.File, lineCursor, maxLines, maxBytes int) (collected []string, total int, truncated bool, err error) {
	reader := io.Reader(f)
	buf := make([]byte, 0, 64*1024)
	chunk := make([]byte, 64*1024)
	pending := "" // partial line carried across reads
	byteCount := 0

	flushLine := func(line string) bool { // returns false to stop
		total++
		if total <= lineCursor {
			return true
		}
		if len(collected) >= maxLines || byteCount+len(line) > maxBytes {
			truncated = true
			return false
		}
		collected = append(collected, line)
		byteCount += len(line)
		return true
	}

	for {
		n, readErr := reader.Read(chunk)
		if n > 0 {
			buf = append(buf, chunk[:n]...)
			for {
				idx := indexByte(buf, '\n')
				if idx < 0 {
					break
				}
				line := pending + string(buf[:idx])
				buf = buf[idx+1:]
				pending = ""
				line = strings.TrimSuffix(line, "\r")
				if !flushLine(line) {
					// Keep counting total for the cursor: cheap scan of the rest.
					total += countNewlines(buf) + countChunkNewlines(reader, chunk)
					return collected, total, truncated, nil
				}
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return nil, 0, false, readErr
		}
	}
	if pending != "" {
		flushLine(strings.TrimSuffix(pending, "\r"))
	}
	return collected, total, truncated, nil
}

// countChunkNewlines drains a reader counting newlines (used to finish the
// total count after the collection cap was hit).
func countChunkNewlines(r io.Reader, chunk []byte) int {
	total := 0
	for {
		n, err := r.Read(chunk)
		if n > 0 {
			total += countNewlines(chunk[:n])
		}
		if err != nil {
			return total
		}
	}
}

// finishTail applies encoding detection/decoding and filtering, then joins
// the final content string.
func finishTail(res *TailLogResult, raw []byte, preSplit []string, req TailLogRequest, filter *regexp.Regexp) *TailLogResult {
	var lines []string
	encoding := req.Encoding

	if preSplit != nil {
		// Line-cursor mode collected raw strings; decode as one blob for
		// encoding fidelity (collected bytes are already capped).
		joined := strings.Join(preSplit, "\n")
		if encoding == "" {
			encoding = detectEncoding([]byte(joined))
		}
		decoded, err := decodeText([]byte(joined), encoding, req.Encoding)
		if err == nil {
			lines = splitLinesRaw([]byte(decoded))
		} else {
			lines = preSplit
		}
	} else {
		if looksBinary(raw) {
			res.IsBinary = true
			res.Content = "<binary file, use remote_download_base64>"
			res.Encoding = "utf-8"
			return res
		}
		if encoding == "" {
			encoding = detectEncoding(raw)
		}
		decoded, err := decodeText(raw, encoding, req.Encoding)
		if err != nil {
			res.Content = ""
			res.Encoding = encoding
			return res
		}
		lines = splitLinesRaw([]byte(decoded))
	}
	res.Encoding = encoding

	if filter != nil {
		res.Filtered = true
		kept := lines[:0]
		for _, l := range lines {
			if filter.MatchString(l) {
				kept = append(kept, l)
			}
		}
		lines = kept
	}
	res.Content = strings.Join(lines, "\n")
	return res
}

// tailMatches returns the last n lines matching filter (tail mode with
// filter_regex): `grep X log | tail -n N`. It scans backwards from the end of
// the file, so it reads only as much of the file as holds the last N matches,
// up to maxTailScanBytes. Lines are decoded one by one, so a block edge never
// splits a character.
func tailMatches(f *os.File, res *TailLogResult, n, maxBytes int, filter *regexp.Regexp, encoding string) (*TailLogResult, error) {
	res.Filtered = true
	res.StartOffset, res.EndOffset = res.Size, res.Size
	forced := encoding
	var matches []string // newest first
	collected := 0
	pos := res.Size // file offset of buf[0]
	var buf []byte  // unprocessed bytes from pos on; its first line may be partial
	for pos > 0 && len(matches) < n && !res.Truncated {
		scanned := res.Size - pos
		if scanned >= maxTailScanBytes {
			res.Truncated = true
			res.Hint = fmt.Sprintf("scanned the last %d MB without finding %d matching lines; use since_offset / "+
				"since_line, or remote_search_content, to look further back", maxTailScanBytes>>20, n)
			break
		}
		// A partial line that fills buf doubles the next read, so a huge
		// line costs O(its size) instead of one copy per block.
		block := min(max(int64(tailBlockSize), int64(len(buf))), pos, maxTailScanBytes-scanned)
		chunk := make([]byte, block, block+int64(len(buf)))
		if _, err := f.ReadAt(chunk, pos-block); err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		if pos == res.Size {
			if looksBinary(chunk) {
				res.IsBinary = true
				res.Encoding = "utf-8"
				res.Content = "<binary file, use remote_download_base64>"
				return res, nil
			}
			if encoding == "" {
				encoding = detectEncoding(chunk)
			}
		}
		pos -= block
		buf = append(chunk, buf...)
		for len(matches) < n {
			// The last line of buf starts after the newline before its last
			// byte; without one it may begin in an earlier block.
			cut := bytes.LastIndexByte(buf[:max(len(buf)-1, 0)], '\n')
			if cut < 0 && pos > 0 {
				break
			}
			line := decodeLine(bytes.TrimRight(buf[cut+1:], "\r\n"), encoding, forced)
			buf = buf[:cut+1]
			if filter.MatchString(line) {
				if collected+len(line) > maxBytes {
					res.Truncated = true
					res.Hint = fmt.Sprintf("max_bytes=%d holds only the last %d matching lines", maxBytes, len(matches))
					break
				}
				matches = append(matches, line)
				collected += len(line) + 1
				res.StartOffset = pos + int64(cut+1)
			}
			if cut < 0 {
				break
			}
		}
	}
	res.Encoding = firstNonEmpty(encoding, "utf-8")
	slices.Reverse(matches)
	res.Content = strings.Join(matches, "\n")
	return res, nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
