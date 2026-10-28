package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	defaultSearchMaxResults  = 200
	defaultSearchMaxFileSize = 256 << 20 // files are streamed, so this only bounds the work per file
	defaultFindMaxResults    = 500
	maxFindResults           = 10000
	maxSearchResults         = 5000
	// maxFindScanEntries bounds the candidates collected for sort_by mtime/size.
	maxFindScanEntries = 100000
	maxSearchContext   = 20
	// maxSearchLineLen caps each returned line: one match on a minified JSON
	// line used to return the whole 380KB line.
	maxSearchLineLen = 2000
	// maxSearchOutputBytes caps the text of all returned lines (one page,
	// like remote_read_file's default window).
	maxSearchOutputBytes = 64 << 10
	searchLineBuf        = 1 << 20 // longer lines are matched on their first 1MB
)

// isHiddenName reports whether the base name is a dot-file/dir.
func isHiddenName(name string) bool {
	return strings.HasPrefix(name, ".")
}

// gnuAlternation finds GNU-grep style \| (and \( \)) in an RE2 pattern: RE2
// reads \| as a literal |, so such a pattern silently matches nothing.
var gnuAlternation = regexp.MustCompile(`(^|[^\\])(\\\\)*\\[|()]`)

// patternHint explains a zero-match search caused by GNU grep habits.
func patternHint(pattern string) string {
	if gnuAlternation.MatchString(pattern) {
		return `pattern contains \| or \( \): in RE2 these match a literal |, ( or ). ` +
			`Use a bare | for alternation and ( ) for groups, e.g. "foo|bar"`
	}
	return ""
}

// ---------------------------------------------------------------------------
// remote_search_content
// ---------------------------------------------------------------------------

// Output modes, named after Claude Code's own Grep tool.
const (
	SearchContentMode = "content"            // matching lines (with context)
	SearchFilesMode   = "files_with_matches" // grep -l: files that match
	SearchCountMode   = "count"              // grep -c: matches per file
)

type SearchContentRequest struct {
	Path          string
	Pattern       string // RE2 regexp (literal text with FixedStrings), required
	IncludeGlob   []string
	ExcludeGlob   []string // files and directories to skip (directories are not entered)
	IgnoreCase    bool
	FixedStrings  bool   // Pattern is literal text (grep -F)
	OutputMode    string // SearchContentMode (default) | SearchFilesMode | SearchCountMode
	ContextLines  int    // lines before and after each match (max 20)
	BeforeLines   int    // when > 0: lines before each match (grep -B), overrides ContextLines
	AfterLines    int    // when > 0: lines after each match (grep -A), overrides ContextLines
	MaxResults    int    // matches (content) or files (other modes) to return
	Offset        int    // skip this many results first: next_offset of a truncated call
	MaxFileSize   int64
	IncludeHidden bool
	Encoding      string    // force the source encoding; empty detects it per file (and per line)
	Deadline      time.Time // stop scanning at this time (truncated, with a hint); zero = no limit
}

type SearchMatch struct {
	File    string   `json:"file"`
	Line    int      `json:"line"`
	Content string   `json:"content"`
	Before  []string `json:"before,omitempty"`
	After   []string `json:"after,omitempty"`
}

// SearchFile is one file of a files_with_matches / count search.
type SearchFile struct {
	File  string `json:"file"`
	Count int    `json:"count,omitempty"` // count mode: matching lines in the file
}

// SearchContentResult puts the results last, so a client that cuts an
// oversized result keeps the summary and the cursor.
type SearchContentResult struct {
	Path          string         `json:"path"`
	Pattern       string         `json:"pattern"`
	OutputMode    string         `json:"output_mode"`
	TotalMatches  int            `json:"total_matches,omitempty"` // matching lines seen (offset ones included); not counted in files_with_matches mode
	FilesMatched  int            `json:"files_matched"`
	FilesSearched int            `json:"files_searched"`
	FilesSkipped  int            `json:"files_skipped"`          // see skip_reasons
	SkipReasons   map[string]int `json:"skip_reasons,omitempty"` // binary, too_large, unreadable, not_regular, symlink_dir, outside_roots, broken_symlink
	SymlinkDirs   []string       `json:"symlink_dirs,omitempty"` // symlinked directories met and not entered (search them by path)
	Truncated     bool           `json:"truncated"`              // stopped early: max_results, the output cap or the time budget
	NextOffset    int            `json:"next_offset,omitempty"`  // offset of the next page when truncated by max_results / output cap
	Hint          string         `json:"hint,omitempty"`
	Files         []SearchFile   `json:"files,omitempty"`
	Matches       []SearchMatch  `json:"matches,omitempty"`

	context bool // context lines were requested: format=text separates groups with --
}

// searchOpts is a validated SearchContentRequest.
type searchOpts struct {
	pattern          string
	re               *regexp.Regexp
	mode             string
	include, exclude *globSet
	before, after    int
	maxResults, skip int
	maxFileSize      int64
	encoding         string
	includeHidden    bool
	deadline         time.Time
	roots            []string
}

func newSearchOpts(req SearchContentRequest, roots []string) (*searchOpts, error) {
	if strings.TrimSpace(req.Pattern) == "" {
		return nil, errors.New("pattern is empty")
	}
	o := &searchOpts{pattern: req.Pattern, mode: req.OutputMode, includeHidden: req.IncludeHidden,
		encoding: req.Encoding, deadline: req.Deadline, roots: roots}
	expr := req.Pattern
	if req.FixedStrings {
		expr = regexp.QuoteMeta(expr)
	}
	if req.IgnoreCase {
		expr = "(?i)" + expr
	}
	var err error
	if o.re, err = regexp.Compile(expr); err != nil {
		return nil, fmt.Errorf("invalid pattern: %w (RE2 syntax; set fixed_strings=true to search for literal text)", err)
	}
	switch o.mode {
	case "":
		o.mode = SearchContentMode
	case SearchContentMode, SearchFilesMode, SearchCountMode:
	default:
		return nil, fmt.Errorf("invalid output_mode %q (want %s, %s or %s)", o.mode, SearchContentMode, SearchFilesMode, SearchCountMode)
	}
	if o.include, err = compileGlobs(req.IncludeGlob, false); err != nil {
		return nil, fmt.Errorf("include_glob: %w", err)
	}
	if o.exclude, err = compileGlobs(req.ExcludeGlob, false); err != nil {
		return nil, fmt.Errorf("exclude_glob: %w", err)
	}
	if o.encoding != "" {
		if _, err := decodeToUTF8(nil, o.encoding); err != nil {
			return nil, err
		}
	}
	o.before, o.after = clampContext(req.ContextLines), clampContext(req.ContextLines)
	if req.BeforeLines > 0 {
		o.before = clampContext(req.BeforeLines)
	}
	if req.AfterLines > 0 {
		o.after = clampContext(req.AfterLines)
	}
	o.maxResults = req.MaxResults
	if o.maxResults <= 0 {
		o.maxResults = defaultSearchMaxResults
	}
	o.maxResults = min(o.maxResults, maxSearchResults)
	o.skip = max(req.Offset, 0)
	if o.maxFileSize = req.MaxFileSize; o.maxFileSize <= 0 {
		o.maxFileSize = defaultSearchMaxFileSize
	}
	return o, nil
}

func clampContext(n int) int { return min(max(n, 0), maxSearchContext) }

// SearchContent runs a server-side regexp search over a file or directory
// tree. Files are streamed line by line (never loaded whole), encodings are
// detected per line (a UTF-8 file with GBK lines still matches Chinese text),
// overlapping context is emitted once, and every skipped file is counted by
// reason. No matches is a success, not an error. Paths are sandboxed by roots.
func SearchContent(req SearchContentRequest, roots []string) (*SearchContentResult, error) {
	abs, err := validatePath(req.Path, roots)
	if err != nil {
		return nil, err
	}
	o, err := newSearchOpts(req, roots)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	s := &searchRun{opts: o, res: &SearchContentResult{Path: abs, Pattern: req.Pattern, OutputMode: o.mode,
		context: o.before+o.after > 0}}
	if !info.IsDir() {
		s.searchFile(abs, info) // an explicitly named file is searched whatever its name
	} else if err := s.walk(abs); err != nil {
		return nil, err
	}
	s.finish()
	return s.res, nil
}

// searchRun is the state of one SearchContent call.
type searchRun struct {
	opts     *searchOpts
	res      *SearchContentResult
	skipped  int // results skipped for offset so far
	returned int // results returned (matches, or files in the other modes)
	outBytes int // text bytes of the returned lines
	stopped  bool
	timedOut bool
}

func (s *searchRun) skip(reason string) {
	if s.res.SkipReasons == nil {
		s.res.SkipReasons = map[string]int{}
	}
	s.res.SkipReasons[reason]++
	s.res.FilesSkipped++
}

// walk searches every file below root (root may itself be a symlink to a
// directory). Symlinked files are searched when their target stays inside
// the roots; symlinked directories are not entered: those inside the roots
// are listed in symlink_dirs, links leading outside count as outside_roots.
func (s *searchRun) walk(root string) error {
	o := s.opts
	return walkTree(root, func(p string, d fs.DirEntry, err error) error {
		if s.stopped {
			return filepath.SkipAll
		}
		if err != nil {
			s.skip("unreadable")
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if p == root {
			return nil
		}
		name, rel := d.Name(), relPath(root, p)
		if !o.includeHidden && isHiddenName(name) || o.exclude.matchAny(name, rel, p) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		isLink := d.Type()&fs.ModeSymlink != 0
		var info os.FileInfo
		var statErr error
		if isLink {
			// A symlinked directory is reported whatever include_glob says
			// (that filters files): only one inside the roots can be searched
			// by passing its path, so only those are listed.
			if info, statErr = os.Stat(p); statErr == nil && info.IsDir() {
				if !s.insideRoots(p) {
					s.skip("outside_roots")
				} else {
					s.skip("symlink_dir")
					if len(s.res.SymlinkDirs) < 20 {
						s.res.SymlinkDirs = append(s.res.SymlinkDirs, p)
					}
				}
				return nil
			}
		}
		if !o.include.match(name, rel, p) {
			return nil
		}
		if !isLink {
			info, statErr = os.Stat(p)
		}
		switch {
		case statErr != nil && isLink:
			s.skip("broken_symlink")
		case statErr != nil:
			s.skip("unreadable")
		case isLink && !s.insideRoots(p):
			s.skip("outside_roots")
		default:
			s.searchFile(p, info)
		}
		return nil
	})
}

func (s *searchRun) insideRoots(p string) bool {
	_, err := validatePath(p, s.opts.roots)
	return err == nil
}

// ctxLine is a line kept for before-context.
type ctxLine struct {
	n    int
	text string
}

// searchFile scans one file line by line.
func (s *searchRun) searchFile(path string, info os.FileInfo) {
	o := s.opts
	switch {
	case !info.Mode().IsRegular():
		s.skip("not_regular")
		return
	case info.Size() > o.maxFileSize:
		s.skip("too_large")
		return
	case s.pastDeadline():
		return
	}
	f, err := os.Open(path)
	if err != nil {
		s.skip("unreadable")
		return
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, 64<<10)
	head, _ := br.Peek(64 << 10)
	if looksBinary(head) {
		s.skip("binary")
		return
	}
	fileEnc := o.encoding
	if fileEnc == "" {
		fileEnc = detectEncoding(head)
	}
	s.res.FilesSearched++

	var ring []ctxLine // the last o.before lines, oldest first
	cur := -1          // index in res.Matches of the match collecting after-context
	afterLeft, emitted, count := 0, 0, 0
	var buf []byte
	for n := 1; !s.stopped; n++ {
		line, used, err := readCappedLine(br, buf[:0])
		buf = line
		if used == 0 && err != nil {
			break
		}
		if n%4096 == 0 && s.pastDeadline() {
			break
		}
		text := decodeLine(line, fileEnc, o.encoding)
		if o.re.MatchString(text) {
			if o.mode == SearchContentMode && s.full() {
				s.stop() // a full page and one more match: truncated
				break
			}
			count++
			switch o.mode {
			case SearchFilesMode:
				s.addFile(path, 0)
				return
			case SearchContentMode:
				if s.skipped < o.skip {
					s.skipped++
					afterLeft = 0
					break
				}
				m := SearchMatch{File: path, Line: n, Content: excerpt(text, o.re, maxSearchLineLen)}
				for _, c := range ring {
					if c.n > emitted && c.n >= n-o.before {
						m.Before = append(m.Before, c.text)
						s.outBytes += len(c.text)
					}
				}
				s.res.Matches = append(s.res.Matches, m)
				s.outBytes += len(m.Content)
				s.returned++
				cur, emitted, afterLeft = len(s.res.Matches)-1, n, o.after
			}
		} else if afterLeft > 0 && cur >= 0 {
			c := excerpt(text, nil, maxSearchLineLen)
			s.res.Matches[cur].After = append(s.res.Matches[cur].After, c)
			s.outBytes += len(c)
			emitted = n
			afterLeft--
		}
		if o.before > 0 && o.mode == SearchContentMode {
			if len(ring) == o.before {
				ring = ring[1:]
			}
			ring = append(ring, ctxLine{n, excerpt(text, nil, maxSearchLineLen)})
		}
		if err != nil {
			break
		}
	}
	if count == 0 {
		return
	}
	switch o.mode {
	case SearchCountMode:
		s.addFile(path, count)
	case SearchContentMode:
		s.res.FilesMatched++
		s.res.TotalMatches += count
	}
}

// addFile records a result of the files_with_matches / count modes; a full
// page stops the search instead.
func (s *searchRun) addFile(path string, count int) {
	if s.full() {
		s.stop()
		return
	}
	s.res.FilesMatched++
	s.res.TotalMatches += count
	if s.skipped < s.opts.skip {
		s.skipped++
		return
	}
	s.res.Files = append(s.res.Files, SearchFile{File: path, Count: count})
	s.outBytes += len(path) + 16
	s.returned++
}

// full reports whether a page is complete; one more result means truncated.
func (s *searchRun) full() bool {
	return s.returned >= s.opts.maxResults || s.outBytes >= maxSearchOutputBytes
}

// stop ends the search because another result exists beyond a full page.
func (s *searchRun) stop() {
	s.stopped = true
	s.res.Truncated = true
	s.res.NextOffset = s.opts.skip + s.returned
}

func (s *searchRun) pastDeadline() bool {
	if s.opts.deadline.IsZero() || time.Now().Before(s.opts.deadline) {
		return false
	}
	s.stopped, s.timedOut = true, true
	s.res.Truncated = true
	return true
}

// readCappedLine reads the next line of br into buf, without its line
// ending, and returns how many bytes of the file it consumed. A line longer
// than searchLineBuf is cut there and the rest of it consumed, so one huge
// line costs no more memory than the cap.
func readCappedLine(br *bufio.Reader, buf []byte) ([]byte, int, error) {
	used := 0
	for {
		chunk, err := br.ReadSlice('\n')
		used += len(chunk)
		if room := searchLineBuf - len(buf); room > 0 {
			buf = append(buf, chunk[:min(len(chunk), room)]...)
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		buf = bytes.TrimSuffix(bytes.TrimSuffix(buf, []byte("\n")), []byte("\r"))
		return buf, used, err
	}
}

// excerpt shortens a line longer than limit bytes to a window around the
// first match of re (its head when re is nil), marking what was left out.
func excerpt(text string, re *regexp.Regexp, limit int) string {
	if len(text) <= limit {
		return text
	}
	start := 0
	if re != nil {
		if loc := re.FindStringIndex(text); loc != nil {
			start = max(0, loc[0]-limit/4)
		}
	}
	end := min(len(text), start+limit)
	for start < end && !utf8.RuneStart(text[start]) {
		start++
	}
	for end < len(text) && end > start && !utf8.RuneStart(text[end]) {
		end--
	}
	var b strings.Builder
	if start > 0 {
		fmt.Fprintf(&b, "…[%d bytes] ", start)
	}
	b.WriteString(text[start:end])
	if end < len(text) {
		fmt.Fprintf(&b, " …[%d more bytes]", len(text)-end)
	}
	return b.String()
}

// finish explains a truncated or empty result.
func (s *searchRun) finish() {
	r, o := s.res, s.opts
	var hints []string
	switch {
	case s.timedOut:
		r.NextOffset = 0
		hints = append(hints, fmt.Sprintf("time budget reached after %d files: the results are partial; "+
			"narrow path / include_glob, or use output_mode=files_with_matches", r.FilesSearched))
	case r.Truncated:
		hints = append(hints, fmt.Sprintf("more results: call again with the same arguments and offset=%d", r.NextOffset))
		if s.outBytes >= maxSearchOutputBytes {
			hints = append(hints, "the output cap was reached: output_mode=files_with_matches or count gives an overview")
		}
	case r.FilesMatched == 0:
		if h := patternHint(o.pattern); h != "" {
			hints = append(hints, h)
		}
		if r.FilesSearched == 0 && r.FilesSkipped == 0 && (o.include != nil || o.exclude != nil) {
			hints = append(hints, "no file matched include_glob / exclude_glob (a glob without / matches base names; "+
				"\"dir/*.conf\" and \"**/*.conf\" match paths below the search path)")
		}
	}
	if n := r.SkipReasons["too_large"]; n > 0 {
		hints = append(hints, fmt.Sprintf("%d files over max_file_size=%d were skipped", n, o.maxFileSize))
	}
	if len(r.SymlinkDirs) > 0 {
		hints = append(hints, "symlinked directories are not entered: search them by passing their path")
	}
	r.Hint = strings.Join(hints, "; ")
}

// ---------------------------------------------------------------------------
// remote_find_files
// ---------------------------------------------------------------------------

type FindFilesRequest struct {
	Path          string
	NameGlob      []string
	ExcludeGlob   []string // entries to skip (directories are not entered)
	Type          string   // "file" | "dir" | "symlink" | "any" (default any)
	MaxDepth      int      // 0 = unlimited; 1 = direct children only
	MaxResults    int
	IncludeHidden bool
	IgnoreCase    bool
	NewerThan     string // RFC3339 time or a duration ago ("30m", "2h", "7d"): find -newer / -mmin
	OlderThan     string // same forms
	MinSize       int64  // bytes; files only
	MaxSize       int64  // bytes; files only (0 = no limit)
	SortBy        string // "path" (default, walk order) | "mtime" (newest first) | "size" (largest first)
}

type FindFileEntry struct {
	Path    string `json:"path"`
	Type    string `json:"type"` // "file" | "dir" | "symlink" | "other"
	Size    int64  `json:"size"`
	ModTime string `json:"mod_time"` // RFC3339
	Target  string `json:"target,omitempty"`
}

type FindFilesResult struct {
	Path        string          `json:"path"`
	Count       int             `json:"count"`
	Truncated   bool            `json:"truncated"`
	DirsSkipped int             `json:"dirs_skipped"` // unreadable directories
	Hint        string          `json:"hint,omitempty"`
	Entries     []FindFileEntry `json:"entries"`
}

// parseTimeBound parses an RFC3339 time or a duration ago ("90s", "30m",
// "2h", "7d").
func parseTimeBound(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if strings.HasSuffix(s, "d") {
		var days float64
		if _, err := fmt.Sscanf(s, "%gd", &days); err == nil && days >= 0 {
			return now.Add(-time.Duration(days * 24 * float64(time.Hour))), nil
		}
	}
	if d, err := time.ParseDuration(s); err == nil && d >= 0 {
		return now.Add(-d), nil
	}
	return time.Time{}, fmt.Errorf("invalid time %q (want RFC3339 like 2026-10-22T08:00:00+08:00, or an age like 30m, 2h, 7d)", s)
}

// FindFiles locates files/dirs by name glob under a directory, sandboxed by
// roots. Unlike a shell `find /` it stays inside the sandbox and reports
// truncation explicitly instead of dying on permission errors. With sort_by
// mtime/size it walks the whole tree and returns the top entries.
func FindFiles(req FindFilesRequest, roots []string) (*FindFilesResult, error) {
	abs, err := validatePath(req.Path, roots)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(abs); err != nil {
		return nil, err
	}
	findType := strings.ToLower(strings.TrimSpace(req.Type))
	switch findType {
	case "":
		findType = "any"
	case "file", "dir", "symlink", "any":
	default:
		return nil, fmt.Errorf("invalid type %q (want file|dir|symlink|any)", req.Type)
	}
	sortBy := strings.ToLower(strings.TrimSpace(req.SortBy))
	switch sortBy {
	case "", "path", "mtime", "size":
	default:
		return nil, fmt.Errorf("invalid sort_by %q (want path|mtime|size)", req.SortBy)
	}
	names, err := compileGlobs(req.NameGlob, req.IgnoreCase)
	if err != nil {
		return nil, fmt.Errorf("name_glob: %w", err)
	}
	exclude, err := compileGlobs(req.ExcludeGlob, req.IgnoreCase)
	if err != nil {
		return nil, fmt.Errorf("exclude_glob: %w", err)
	}
	now := time.Now()
	var newer, older time.Time
	if req.NewerThan != "" {
		if newer, err = parseTimeBound(req.NewerThan, now); err != nil {
			return nil, fmt.Errorf("newer_than: %w", err)
		}
	}
	if req.OlderThan != "" {
		if older, err = parseTimeBound(req.OlderThan, now); err != nil {
			return nil, fmt.Errorf("older_than: %w", err)
		}
	}
	maxResults := req.MaxResults
	if maxResults <= 0 {
		maxResults = defaultFindMaxResults
	}
	maxResults = min(maxResults, maxFindResults)
	// A sorted result needs every candidate: collect up to this many first.
	collectCap := maxResults
	if sortBy == "mtime" || sortBy == "size" {
		collectCap = maxFindScanEntries
	}

	result := &FindFilesResult{Path: abs, Entries: []FindFileEntry{}}
	matched := 0
	err = walkTree(abs, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if d != nil && d.IsDir() {
				result.DirsSkipped++
				return filepath.SkipDir
			}
			return nil
		}
		if p == abs {
			return nil
		}
		name, rel := d.Name(), relPath(abs, p)
		if !req.IncludeHidden && isHiddenName(name) || exclude.matchAny(name, rel, p) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		depth := strings.Count(rel, string(os.PathSeparator)) + 1
		if req.MaxDepth > 0 && depth > req.MaxDepth {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !names.match(name, rel, p) {
			return nil
		}
		entryType := "other"
		switch {
		case d.IsDir():
			entryType = "dir"
		case d.Type()&fs.ModeSymlink != 0:
			entryType = "symlink"
		case d.Type().IsRegular():
			entryType = "file"
		}
		if findType != "any" && entryType != findType {
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			return nil
		}
		if !newer.IsZero() && !info.ModTime().After(newer) || !older.IsZero() && !info.ModTime().Before(older) {
			return nil
		}
		if (req.MinSize > 0 || req.MaxSize > 0) && (entryType != "file" ||
			info.Size() < req.MinSize || req.MaxSize > 0 && info.Size() > req.MaxSize) {
			return nil
		}
		matched++
		if len(result.Entries) >= collectCap {
			result.Truncated = true
			return filepath.SkipAll
		}
		entry := FindFileEntry{Path: p, Type: entryType, Size: info.Size(), ModTime: info.ModTime().Format(time.RFC3339)}
		if entryType == "symlink" {
			entry.Target, _ = os.Readlink(p)
		}
		result.Entries = append(result.Entries, entry)
		return nil
	})
	if err != nil {
		return nil, err
	}

	switch sortBy {
	case "mtime":
		sort.SliceStable(result.Entries, func(i, j int) bool { return result.Entries[i].ModTime > result.Entries[j].ModTime })
	case "size":
		sort.SliceStable(result.Entries, func(i, j int) bool { return result.Entries[i].Size > result.Entries[j].Size })
	}
	if len(result.Entries) > maxResults {
		result.Entries = result.Entries[:maxResults]
		result.Truncated = true
	}
	result.Count = len(result.Entries)
	if result.Truncated {
		if (sortBy == "mtime" || sortBy == "size") && matched > maxFindScanEntries {
			result.Hint = fmt.Sprintf("over %d candidates: the sort covers only the first %d found; narrow path or name_glob",
				maxFindScanEntries, maxFindScanEntries)
		} else {
			result.Hint = fmt.Sprintf("max_results=%d reached: narrow path, name_glob, type or the time/size filters", maxResults)
		}
	}
	return result, nil
}
