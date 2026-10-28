package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// remote_read_file windows
//
// A read returns one window of the file: a byte range (default), a line range
// (start_line/end_line) or the last N lines (tail_lines). Line windows are
// found by streaming, so any line of any file can be read (an earlier version
// sliced lines out of the first max_bytes only: line 40000 of a 3MB log came
// back empty). A window that stops before EOF ends on a line boundary when it
// holds one and never inside a character, and the result says where the next
// window starts (next_offset / next_start_line).
// ---------------------------------------------------------------------------

// maxLineScanBytes bounds the scans that only count lines (total_lines, the
// number of the first line of a byte window); beyond it they are omitted.
const maxLineScanBytes = 16 << 20

const (
	defaultReadMaxBytes = 64 << 10 // one page: Claude Code cuts MCP results at ~100k characters
	maxReadMaxBytes     = 16 << 20
)

type readMode int

const (
	readBytes  readMode = iota // offset_bytes / truncate_mode
	readLines                  // start_line / end_line
	readTail                   // tail_lines
	readPicked                 // lines=[...]
)

type ReadFileRequest struct {
	Path         string
	Encoding     string // source encoding; empty auto-detects
	StartLine    int    // 1-based, 0 or negative means from start
	EndLine      int    // 1-based inclusive; 0 or negative means to end
	MaxBytes     int    // read cap, 0 uses the 64KB default, max 16MB
	TailLines    int    // >0: read the last N lines (seek-based, wins over line range)
	OffsetBytes  int64  // start reading at this byte offset
	TruncateMode string // "head" (default) keeps the first max_bytes; "tail" keeps the last
	LineNumbers  bool   // prefix every line with its number, cat -n style
	Lines        []int  // only these line numbers (always numbered); wins over every other mode
}

// ReadFileResult puts content last: Claude Code cuts a result over its MCP
// output cap (25k tokens) down to its first 100,000 characters, and such a
// cut must not take the cursors with it.
type ReadFileResult struct {
	Path          string `json:"path"`
	Encoding      string `json:"encoding,omitempty"` // source encoding used ("unknown": raw bytes)
	IsBinary      bool   `json:"is_binary"`
	TotalSize     int64  `json:"total_size"`               // file size in bytes
	TotalLines    int    `json:"total_lines,omitempty"`    // omitted when not counted (files over 16MB)
	Offset        int64  `json:"offset"`                   // byte offset the content starts at
	StartLine     int    `json:"start_line,omitempty"`     // number of the first returned line
	ReturnedLines int    `json:"returned_lines,omitempty"` // lines in content
	// Truncated: the content stops short of what was asked for (the end of
	// the file, end_line, or tail_lines lines); see NextOffset/NextStartLine.
	Truncated     bool   `json:"truncated"`
	NextOffset    int64  `json:"next_offset,omitempty"`     // offset_bytes of the next window
	NextStartLine int    `json:"next_start_line,omitempty"` // start_line of the next window
	MissingLines  []int  `json:"missing_lines,omitempty"`   // lines mode: requested numbers past the end of the file
	Hint          string `json:"hint,omitempty"`
	Content       string `json:"content"` // UTF-8

	mode readMode // how the window was chosen (for format=text)
	end  int64    // file offset after the window's last byte (for format=text)
}

// readWindow is the raw bytes of one window and where they sit in the file.
type readWindow struct {
	data     []byte
	offset   int64 // file offset of data[0]
	more     bool  // file content follows data
	capped   bool  // max_bytes stopped the read before the requested end
	nextLine int   // line windows: start_line of the following window
	cutLine  bool  // line windows: the first line alone exceeded max_bytes
}

// readByteWindow reads up to maxBytes from offset. A window that stops before
// EOF ends on a line boundary when its last newline lies in its second half,
// so short lines are never split while one huge line still comes back.
func readByteWindow(f *os.File, size, offset int64, maxBytes int) (readWindow, error) {
	offset = min(max(offset, 0), size)
	buf := make([]byte, min(int64(maxBytes), size-offset))
	n, err := f.ReadAt(buf, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return readWindow{}, err
	}
	w := readWindow{data: buf[:n], offset: offset}
	if w.more = offset+int64(n) < size; w.more {
		w.capped = true
		if i := bytes.LastIndexByte(w.data, '\n'); i >= 0 && i >= len(w.data)/2 {
			w.data = w.data[:i+1]
		}
	}
	return w, nil
}

// readLineWindow streams f from its start and returns lines start..end
// (1-based, inclusive; end <= 0 means to EOF), at most maxBytes of them. A
// first line longer than maxBytes is returned cut (cutLine).
func readLineWindow(f *os.File, start, end, maxBytes int) (readWindow, error) {
	br := bufio.NewReaderSize(f, 64<<10)
	var w readWindow
	line := 1
	for line < start {
		chunk, err := br.ReadSlice('\n')
		w.offset += int64(len(chunk))
		switch {
		case err == nil:
			line++
		case errors.Is(err, bufio.ErrBufferFull):
		case errors.Is(err, io.EOF):
			return w, nil // start is past the last line
		default:
			return w, err
		}
	}
	lineStart := 0 // where the line being read starts in w.data
	for end <= 0 || line <= end {
		chunk, err := br.ReadSlice('\n')
		w.data = append(w.data, chunk...)
		if len(w.data) > maxBytes {
			w.more, w.capped = true, true
			if lineStart > 0 {
				w.data, w.nextLine = w.data[:lineStart], line
			} else {
				w.data, w.nextLine, w.cutLine = w.data[:maxBytes], line+1, true
			}
			return w, nil
		}
		switch {
		case err == nil:
			line++
			lineStart = len(w.data)
		case errors.Is(err, bufio.ErrBufferFull):
		case errors.Is(err, io.EOF):
			return w, nil
		default:
			return w, err
		}
	}
	if _, err := br.Peek(1); err == nil {
		w.more, w.nextLine = true, line
	}
	return w, nil
}

// lineCount returns the number of lines in the file (a final line without a
// newline counts), or 0 when the file is larger than maxLineScanBytes.
func lineCount(f *os.File, size int64) int {
	if size == 0 || size > maxLineScanBytes {
		return 0
	}
	n, last, ok := countNewlinesAt(f, size)
	if !ok {
		return 0
	}
	if last != '\n' {
		n++
	}
	return n
}

// lineNumberAt returns the number of the line containing byte offset, or 0
// when finding it would scan more than maxLineScanBytes.
func lineNumberAt(f *os.File, offset int64) int {
	if offset > maxLineScanBytes {
		return 0
	}
	n, _, ok := countNewlinesAt(f, offset)
	if !ok {
		return 0
	}
	return n + 1
}

// countNewlinesAt counts the newlines in the first `to` bytes of f and
// returns the last byte read. ReadAt leaves the file position alone.
func countNewlinesAt(f *os.File, to int64) (n int, last byte, ok bool) {
	buf := make([]byte, 64<<10)
	for off := int64(0); off < to; {
		chunk := buf[:min(int64(len(buf)), to-off)]
		k, err := f.ReadAt(chunk, off)
		n += bytes.Count(chunk[:k], []byte{'\n'})
		if k > 0 {
			last = chunk[k-1]
		}
		off += int64(k)
		if err != nil {
			return n, last, errors.Is(err, io.EOF)
		}
	}
	return n, last, true
}

// windowLines counts the lines in data; a final unterminated line counts.
func windowLines(data []byte) int {
	n := bytes.Count(data, []byte{'\n'})
	if len(data) > 0 && data[len(data)-1] != '\n' {
		n++
	}
	return n
}

// numberLines renders content in `cat -n` style, numbering from first: the
// line number right-aligned in 6 columns, a tab, then the line verbatim. The
// caller ensures content holds at least one line (an empty line is "").
func numberLines(content string, first int) string {
	content = strings.TrimSuffix(content, "\n")
	lines := strings.Split(content, "\n")
	var b strings.Builder
	b.Grow(len(content) + 8*len(lines))
	for i, l := range lines {
		if i > 0 {
			b.WriteByte('\n')
		}
		writeNumbered(&b, first+i, l)
	}
	return b.String()
}

// writeNumbered appends one `cat -n` style line to b: the number
// right-aligned in 6 columns, a tab, then the text.
func writeNumbered(b *strings.Builder, n int, text string) {
	num := strconv.Itoa(n)
	for pad := 6 - len(num); pad > 0; pad-- {
		b.WriteByte(' ')
	}
	b.WriteString(num)
	b.WriteByte('\t')
	b.WriteString(text)
}

// numberedLen is the length writeNumbered gives line n with text.
func numberedLen(n int, text string) int {
	return max(6, len(strconv.Itoa(n))) + 1 + len(text)
}

// readTailWindow returns the last n lines of f, at most maxBytes of them.
func readTailWindow(f *os.File, size int64, n, maxBytes int) (readWindow, error) {
	data, off, capped, err := readTailBytes(f, size, n, maxBytes)
	if err != nil {
		return readWindow{}, err
	}
	// readTailBytes counts newlines, so a final line without one leaves an
	// extra line at the head.
	for windowLines(data) > n {
		i := bytes.IndexByte(data, '\n')
		data, off = data[i+1:], off+int64(i+1)
	}
	// Capped: fewer lines than asked for, or a last line longer than
	// maxBytes that comes back without its head.
	capped = capped && (windowLines(data) < n || !lineStartsAt(f, off))
	return readWindow{data: data, offset: off, capped: capped}, nil
}

// lineStartsAt reports whether a line starts at offset off of f.
func lineStartsAt(f *os.File, off int64) bool {
	if off <= 0 {
		return true
	}
	var prev [1]byte
	_, err := f.ReadAt(prev[:], off-1)
	return err == nil && prev[0] == '\n'
}

// windowEdges returns the bounds of data without the characters cut by the
// edges of a read window: a partial character at the head (head=true: the
// window starts mid-line) or at the tail (tail=true: the file continues).
// UTF-8 resynchronizes within 3 bytes. The GBK family does not (a trail byte
// can look like a lead byte): its cut head needs the bytes before the window
// (gbHeadSkip), so head is ignored for it and data must start on a character
// boundary; its cut tail is found by parsing from the last byte that can only
// be a single-byte character. The dropped bytes belong to the neighbouring
// window.
func windowEdges(data []byte, encoding string, head, tail bool) (start, end int) {
	end = len(data)
	switch {
	case isGBFamily(encoding):
		if tail {
			gb18030 := isGB18030(encoding)
			base := max(gbLastBoundary(data, gb18030), 0)
			end = base + gbWholeChars(data[base:], gb18030)
		}
	case strings.EqualFold(strings.TrimSpace(encoding), "utf-8"), strings.EqualFold(strings.TrimSpace(encoding), "utf8"):
		s, e := utf8Edges(data)
		if head {
			start = s
		}
		if tail {
			end = max(e, start)
		}
	}
	return start, end
}

func isGB18030(encoding string) bool {
	return strings.EqualFold(strings.TrimSpace(encoding), "gb18030")
}

// gbCharLen is the length of the GBK-family character that starts b, as the
// decoder reads it (an invalid sequence is read one byte at a time); a
// character cut off by the end of b counts in full.
func gbCharLen(b []byte, gb18030 bool) int {
	if b[0] < 0x81 || b[0] == 0xFF {
		return 1
	}
	if len(b) < 2 {
		return 2
	}
	switch t := b[1]; {
	case t >= 0x40 && t != 0x7F && t != 0xFF:
		return 2
	case gb18030 && t >= 0x30 && t <= 0x3F: // four-byte sequence, as x/text reads it
		if len(b) > 2 && (b[2] < 0x81 || b[2] == 0xFF) || len(b) > 3 && (b[3] < 0x30 || b[3] > 0x39) {
			return 1
		}
		if len(b) > 3 {
			// Outside both mapped ranges the decoder reads the lead byte alone.
			r := ((int(b[0]-0x81)*10+int(t-0x30))*126+int(b[2]-0x81))*10 + int(b[3]-0x30)
			if r >= 39420 && (r < 189000 || r >= 189000+0x100000) {
				return 1
			}
		}
		return 4
	}
	return 1
}

// gbWholeChars returns the length of the longest prefix of data (data[0]
// starts a character) that does not end inside a GBK-family character.
func gbWholeChars(data []byte, gb18030 bool) int {
	i := 0
	for i < len(data) {
		n := gbCharLen(data[i:], gb18030)
		if i+n > len(data) {
			break
		}
		i += n
	}
	return i
}

// gbLastBoundary returns the index just past the last byte of data that can
// only be a single-byte character — below 0x40 in GBK, below 0x30 in GB18030
// (whose four-byte characters hold digits) — so a character starts there; -1
// when data has no such byte.
func gbLastBoundary(data []byte, gb18030 bool) int {
	limit := byte(0x40)
	if gb18030 {
		limit = 0x30
	}
	for i := len(data) - 1; i >= 0; i-- {
		if data[i] < limit {
			return i + 1
		}
	}
	return -1
}

// gbSyncScan bounds how far gbHeadSkip looks back for a character boundary.
const gbSyncScan = 64 << 10

// gbHeadSkip returns how many bytes at the head of data, which starts at
// file offset off, belong to a GBK-family character begun before off (0 when
// off is a character boundary, as every next_offset is). It parses forward
// from the nearest earlier byte that can only be a single-byte character;
// without one within gbSyncScan bytes it takes off as a boundary.
func gbHeadSkip(f *os.File, off int64, data []byte, gb18030 bool) int {
	if off <= 0 || len(data) == 0 {
		return 0
	}
	n := min(off, gbSyncScan)
	back := make([]byte, n, n+4)
	if _, err := f.ReadAt(back, off-n); err != nil && !errors.Is(err, io.EOF) {
		return 0
	}
	p := gbLastBoundary(back, gb18030)
	if p < 0 {
		if off > n {
			return 0 // no boundary in reach
		}
		p = 0 // the file starts on one
	}
	seq := append(back, data[:min(len(data), 4)]...)
	for p < len(back) {
		p += gbCharLen(seq[p:], gb18030)
	}
	return min(p-len(back), len(data))
}

// ReadFileContent reads one window of a file (see the top of this file) and
// decodes it to UTF-8. Binary files are reported without decoding.
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
	size := info.Size()
	maxBytes := req.MaxBytes
	if maxBytes <= 0 {
		maxBytes = defaultReadMaxBytes
	}
	maxBytes = min(maxBytes, maxReadMaxBytes)
	if len(req.Lines) > 0 {
		return readPickedLines(f, &ReadFileResult{Path: abs, TotalSize: size}, req, maxBytes)
	}

	mode := readBytes
	switch {
	case req.TailLines > 0:
		mode = readTail
	case req.StartLine > 0 || req.EndLine > 0:
		mode = readLines
	}
	start := max(req.StartLine, 1)
	var w readWindow
	switch mode {
	case readTail:
		w, err = readTailWindow(f, size, req.TailLines, maxBytes)
	case readLines:
		if req.EndLine > 0 && req.EndLine < start {
			return nil, fmt.Errorf("end_line %d is before start_line %d", req.EndLine, start)
		}
		w, err = readLineWindow(f, start, req.EndLine, maxBytes)
	default:
		offset := req.OffsetBytes
		tail := strings.EqualFold(req.TruncateMode, "tail") && size-int64(maxBytes) > offset
		if tail {
			offset = size - int64(maxBytes)
		}
		if w, err = readByteWindow(f, size, offset, maxBytes); err == nil && tail {
			alignToLine(f, &w)
		}
	}
	if err != nil {
		return nil, err
	}

	res := &ReadFileResult{Path: abs, TotalSize: size, Offset: w.offset, mode: mode}
	if looksBinary(w.data) {
		res.IsBinary = true
		res.Content = "<binary file, use remote_download_base64>"
		return res, nil
	}
	enc := req.Encoding
	if enc == "" {
		enc = detectEncoding(w.data)
	}
	// Drop the characters cut by the window edges: a window that starts
	// mid-line may start inside one, and a window that stops before EOF may
	// end inside one. A window smaller than one character is kept as it is,
	// so that paging by next_offset still advances.
	headCut, skip := !lineStartsAt(f, w.offset), 0
	if headCut && isGBFamily(enc) {
		skip, headCut = gbHeadSkip(f, w.offset, w.data, isGB18030(enc)), false
	}
	if s, e := windowEdges(w.data[skip:], enc, headCut, w.more); e > s {
		w.data, w.offset = w.data[skip+s:skip+e], w.offset+int64(skip+s)
	}
	content, err := decodeText(w.data, enc, req.Encoding)
	if err != nil {
		return nil, err
	}
	res.Encoding, res.Offset = enc, w.offset
	res.end = w.offset + int64(len(w.data))
	res.TotalLines = lineCount(f, size)
	res.ReturnedLines = windowLines(w.data)
	switch {
	case len(w.data) == 0:
	case mode == readLines:
		res.StartLine = start
	case mode == readTail && res.TotalLines > 0:
		res.StartLine = res.TotalLines - res.ReturnedLines + 1
	default:
		res.StartLine = lineNumberAt(f, w.offset)
	}
	if mode != readBytes {
		content = trimFinalNewline(content)
	}
	if req.LineNumbers && res.StartLine > 0 {
		content = numberLines(content, res.StartLine)
	}
	res.Content = content
	res.Hint = readCursors(res, w, mode, req, start, maxBytes)
	if req.LineNumbers && res.StartLine == 0 && len(w.data) > 0 {
		res.Hint = strings.TrimSpace(res.Hint + " line_numbers not applied: the line number at this offset " +
			"is not counted beyond the first 16MB; read by start_line instead (it streams, any line of any file).")
	}
	return res, nil
}

// maxPickedLines bounds the line numbers one lines-mode read may ask for.
const maxPickedLines = 1000

// readPickedLines returns only the requested line numbers, in ascending order
// and numbered cat -n style (the numbers show the gaps). It replaces loops
// like `for L in 604 620 1658; do sed -n "${L}p" f | iconv -f gbk; done`
// seen in production. The file is streamed once, up to the largest requested
// line; each line is decoded on its own, so a GBK line in a UTF-8 file reads
// right.
func readPickedLines(f *os.File, res *ReadFileResult, req ReadFileRequest, maxBytes int) (*ReadFileResult, error) {
	res.mode = readPicked
	want := slices.Clone(req.Lines)
	slices.Sort(want)
	want = slices.Compact(want)
	if len(want) > maxPickedLines {
		return nil, fmt.Errorf("lines: at most %d line numbers per call, got %d", maxPickedLines, len(want))
	}
	if want[0] < 1 {
		return nil, fmt.Errorf("lines: line numbers start at 1, got %d", want[0])
	}
	br := bufio.NewReaderSize(f, 64<<10)
	head, _ := br.Peek(64 << 10)
	if looksBinary(head) {
		res.IsBinary = true
		res.Content = "<binary file, use remote_download_base64>"
		return res, nil
	}
	enc := req.Encoding
	if enc == "" {
		enc = detectEncoding(head)
	}
	res.Encoding = enc
	res.TotalLines = lineCount(f, res.TotalSize) // ReadAt: the stream position is untouched

	var b strings.Builder
	var buf []byte
	var off int64
	seen, i := 0, 0 // lines read so far; next index in want
	for i < len(want) {
		line, used, err := readCappedLine(br, buf[:0])
		buf = line
		if used == 0 {
			if err != nil && !errors.Is(err, io.EOF) {
				return nil, err
			}
			break
		}
		seen++
		if seen == want[i] {
			text := decodeLine(line, enc, req.Encoding)
			if b.Len() > 0 && b.Len()+1+numberedLen(seen, text) > maxBytes {
				res.Truncated, res.NextStartLine = true, seen
				res.Hint = fmt.Sprintf("max_bytes=%d reached: lines from %d on were not returned; "+
					"call again with the remaining line numbers", maxBytes, seen)
				break
			}
			if b.Len() == 0 {
				res.StartLine, res.Offset = seen, off
				if numberedLen(seen, text) > maxBytes {
					text = excerpt(text, nil, max(maxBytes-8, 1))
				}
			} else {
				b.WriteByte('\n')
			}
			writeNumbered(&b, seen, text)
			res.ReturnedLines++
			i++
		}
		off += int64(used)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				return nil, err
			}
			break
		}
	}
	if !res.Truncated && i < len(want) {
		res.MissingLines = want[i:]
		res.Hint = fmt.Sprintf("%d of the requested lines are past the end of the file (%d lines)", len(res.MissingLines), seen)
	}
	res.Content = b.String()
	return res, nil
}

// alignToLine moves the start of a truncate_mode=tail window that begins
// mid-line to the next line, when that lies in the window's first half.
func alignToLine(f *os.File, w *readWindow) {
	if lineStartsAt(f, w.offset) {
		return
	}
	if i := bytes.IndexByte(w.data, '\n'); i >= 0 && i < len(w.data)/2 {
		w.data, w.offset = w.data[i+1:], w.offset+int64(i+1)
	}
}

// trimFinalNewline drops the newline ending the last line of a line or tail
// window: such content is the lines themselves, joined by "\n". Lines are
// otherwise verbatim (a CRLF file keeps its "\r").
func trimFinalNewline(s string) string {
	return strings.TrimSuffix(s, "\n")
}

// readCursors sets Truncated and the next-window cursors of res, and returns
// a hint on how to go on (empty when the read is complete).
func readCursors(res *ReadFileResult, w readWindow, mode readMode, req ReadFileRequest, start, maxBytes int) string {
	end := w.offset + int64(len(w.data))
	if w.more {
		res.NextOffset = end
	}
	switch mode {
	case readTail:
		if res.Truncated = w.capped; res.Truncated {
			return fmt.Sprintf("max_bytes=%d holds only the last %d of the %d requested lines; raise max_bytes for more",
				maxBytes, res.ReturnedLines, req.TailLines)
		}
		return ""
	case readLines:
		res.Truncated, res.NextStartLine = w.capped, w.nextLine
		switch {
		case len(w.data) == 0 && !w.more:
			if res.TotalLines > 0 {
				return fmt.Sprintf("start_line %d is past the end of the file (%d lines)", start, res.TotalLines)
			}
			return fmt.Sprintf("start_line %d is past the end of the file", start)
		case w.cutLine:
			return fmt.Sprintf("line %d alone is longer than max_bytes=%d and was cut: the rest of it starts at "+
				"offset_bytes=%d, the next line is start_line=%d", start, maxBytes, res.NextOffset, res.NextStartLine)
		case res.Truncated:
			return fmt.Sprintf("max_bytes=%d reached before end_line; continue with start_line=%d", maxBytes, res.NextStartLine)
		}
		return ""
	}
	if strings.EqualFold(req.TruncateMode, "tail") && w.offset > 0 {
		res.Truncated = true
		return fmt.Sprintf("truncate_mode=tail: the first %d bytes are not shown (read them with offset_bytes=0 or start_line)", w.offset)
	}
	if res.Truncated = w.more; !w.more {
		return ""
	}
	if res.StartLine > 0 && len(w.data) > 0 && w.data[len(w.data)-1] == '\n' {
		res.NextStartLine = res.StartLine + res.ReturnedLines
	}
	return fmt.Sprintf("the file continues: %d of %d bytes not shown; read on with offset_bytes=%d", res.TotalSize-end, res.TotalSize, end)
}

// ---------------------------------------------------------------------------
// remote_read_files
//
// Agents read config sets and code excerpts with
// `for f in ...; do echo "=== $f"; cat $f; done` (173 such shell calls in the
// production audit log), which a read-only server cannot run. One call reads
// up to maxReadFiles files or windows, with one shared output budget.
// ---------------------------------------------------------------------------

const (
	maxReadFiles      = 50
	maxNotReadListed  = 100
	minReadFilesShare = 512 // bytes: the least a file is read with while budget is left
	readItemOverhead  = 200 // bytes of JSON metadata per returned file
)

type ReadFilesRequest struct {
	Files       []ReadFileRequest // each: a path (or a glob) and an optional window
	LineNumbers bool              // number the lines of every file, cat -n style
	MaxBytes    int               // content budget shared by all files (default 64KB, max 16MB)
}

// ReadFilesItem is one file of a remote_read_files result: its path and
// either an error or the remote_read_file result, inlined.
type ReadFilesItem struct {
	Path  string `json:"path"`
	Error string `json:"error,omitempty"`
	*ReadFileResult
}

type ReadFilesResult struct {
	FilesRead   int             `json:"files_read"`
	FilesFailed int             `json:"files_failed"`
	Truncated   bool            `json:"truncated"`          // a file was cut to its share, or files were left unread
	NotRead     []string        `json:"not_read,omitempty"` // left unread: the budget or the file limit ran out
	Hint        string          `json:"hint,omitempty"`
	Files       []ReadFilesItem `json:"files"`
}

// ReadFiles reads several files in one call, in request order. Small files
// are read first, each with an equal share of the budget left, so they come
// back whole while the large ones split the rest (and come back truncated,
// with their next_offset / next_start_line). A failing file carries an error
// and does not fail the call.
func ReadFiles(req ReadFilesRequest, roots []string) (*ReadFilesResult, error) {
	if len(req.Files) == 0 {
		return nil, errors.New("no files given (pass paths or files)")
	}
	budget := req.MaxBytes
	if budget <= 0 {
		budget = defaultReadMaxBytes
	}
	budget = min(budget, maxReadMaxBytes)

	type job struct {
		req  ReadFileRequest
		err  string
		size int64
	}
	var jobs []job
	var hints, notRead []string
	for _, fr := range req.Files {
		paths, note, err := expandReadPath(fr.Path, roots)
		if note != "" {
			hints = append(hints, note)
		}
		if err != nil {
			jobs = append(jobs, job{req: fr, err: err.Error()})
			continue
		}
		for _, p := range paths {
			r := fr
			r.Path = p
			jobs = append(jobs, job{req: r})
		}
	}
	if len(jobs) > maxReadFiles {
		for _, j := range jobs[maxReadFiles:] {
			notRead = append(notRead, j.req.Path)
		}
		jobs = jobs[:maxReadFiles]
		hints = append(hints, fmt.Sprintf("at most %d files are read per call", maxReadFiles))
	}

	order := make([]int, len(jobs))
	left := 0
	for i := range jobs {
		order[i] = i
		if jobs[i].err == "" {
			left++
			if fi, err := os.Stat(jobs[i].req.Path); err == nil {
				jobs[i].size = fi.Size()
			}
		}
	}
	sort.SliceStable(order, func(a, b int) bool { return jobs[order[a]].size < jobs[order[b]].size })

	res := &ReadFilesResult{Files: []ReadFilesItem{}}
	items := make([]ReadFilesItem, len(jobs))
	unread := make([]bool, len(jobs))
	remaining, cut := budget, 0
	for _, i := range order {
		j := jobs[i]
		items[i].Path = j.req.Path
		if j.err != "" {
			items[i].Error = j.err
			res.FilesFailed++
			continue
		}
		share := max(remaining/left, min(remaining, minReadFilesShare))
		left--
		if share <= 0 {
			unread[i] = true
			continue
		}
		r := j.req
		if r.MaxBytes <= 0 || r.MaxBytes > share {
			r.MaxBytes = share
		}
		r.LineNumbers = r.LineNumbers || req.LineNumbers
		out, err := ReadFileContent(r, roots)
		if err != nil {
			items[i].Error = err.Error()
			res.FilesFailed++
			continue
		}
		items[i].Path, items[i].ReadFileResult = out.Path, out
		res.FilesRead++
		if out.Truncated {
			cut++
		}
		remaining -= len(out.Content) + readItemOverhead
	}
	for i, it := range items {
		if unread[i] {
			notRead = append(notRead, it.Path)
		} else {
			res.Files = append(res.Files, it)
		}
	}

	if cut > 0 {
		hints = append(hints, fmt.Sprintf("%d files were cut (truncated=true): read on with remote_read_file at their "+
			"next_offset / next_start_line, or raise max_bytes (now %d, shared by all files)", cut, budget))
	}
	if len(notRead) > 0 {
		hints = append(hints, fmt.Sprintf("%d files were not read (not_read): call again with them", len(notRead)))
		if len(notRead) > maxNotReadListed {
			notRead = notRead[:maxNotReadListed]
		}
		res.NotRead = notRead
	}
	res.Truncated = cut > 0 || len(res.NotRead) > 0
	res.Hint = strings.Join(hints, "; ")
	return res, nil
}

// expandReadPath returns the files a remote_read_files path names: the path
// itself, or, when it does not exist and holds glob characters (* ? [), the
// regular files matching it, sorted (dot-files only when the pattern's last
// element starts with a dot, as in a shell). The directories before the first
// glob character must lie inside the roots, and matches leading outside them
// are left out, so a pattern cannot list anything outside the roots.
func expandReadPath(path string, roots []string) ([]string, string, error) {
	if strings.TrimSpace(path) == "" {
		return nil, "", errors.New("path is empty")
	}
	if _, err := os.Lstat(path); err == nil || !strings.ContainsAny(path, "*?[") {
		// A plain path, or an existing name that merely holds glob characters.
		abs, err := validatePath(path, roots)
		if err != nil {
			return nil, "", err
		}
		return []string{abs}, "", nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, "", err
	}
	fixed := abs
	for strings.ContainsAny(fixed, "*?[") {
		fixed = filepath.Dir(fixed)
	}
	if _, err := validatePath(fixed, roots); err != nil {
		return nil, "", err
	}
	matches, err := filepath.Glob(abs)
	if err != nil {
		return nil, "", fmt.Errorf("invalid glob %q: %w", path, err)
	}
	dotOK := isHiddenName(filepath.Base(abs))
	var files, notes []string
	outside := 0
	for _, m := range matches {
		if len(files) == maxReadFiles {
			notes = append(notes, fmt.Sprintf("%s matches more than the %d files read per call: narrow the pattern, "+
				"or list the files with remote_find_files", path, maxReadFiles))
			break
		}
		if !dotOK && isHiddenName(filepath.Base(m)) {
			continue
		}
		if _, err := validatePath(m, roots); err != nil {
			outside++
			continue
		}
		if fi, err := os.Stat(m); err != nil || !fi.Mode().IsRegular() {
			continue
		}
		files = append(files, m)
	}
	if outside > 0 {
		notes = append(notes, fmt.Sprintf("%s: %d matches lead outside the allowed roots and were left out", path, outside))
	}
	note := strings.Join(notes, "; ")
	if len(files) == 0 {
		return nil, note, fmt.Errorf("no regular file matches %s", path)
	}
	return files, note, nil
}
