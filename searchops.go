package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	defaultSearchMaxResults  = 200
	defaultSearchMaxFileSize = 32 << 20 // 32MB
	defaultFindMaxResults    = 500
	maxFindResults           = 10000
	maxSearchResults         = 5000
)

// matchAnyGlob reports whether base name matches any of the glob patterns
// (filepath.Match syntax). Empty patterns match everything. When ignoreCase
// is set, both name and patterns are lower-cased first.
func matchAnyGlob(name string, patterns []string, ignoreCase bool) bool {
	if len(patterns) == 0 {
		return true
	}
	if ignoreCase {
		name = strings.ToLower(name)
	}
	for _, p := range patterns {
		if ignoreCase {
			p = strings.ToLower(p)
		}
		if ok, err := filepath.Match(p, name); err == nil && ok {
			return true
		}
	}
	return false
}

// isHiddenName reports whether the base name is a dot-file/dir.
func isHiddenName(name string) bool {
	return strings.HasPrefix(name, ".")
}

// ---------------------------------------------------------------------------
// remote_search_content
// ---------------------------------------------------------------------------

type SearchContentRequest struct {
	Path          string
	Pattern       string // RE2 regexp, required
	IncludeGlob   []string
	IgnoreCase    bool
	ContextLines  int
	MaxResults    int
	MaxFileSize   int64
	IncludeHidden bool
}

type SearchMatch struct {
	File    string   `json:"file"`
	Line    int      `json:"line"`
	Content string   `json:"content"`
	Before  []string `json:"before,omitempty"`
	After   []string `json:"after,omitempty"`
}

type SearchContentResult struct {
	Path          string        `json:"path"`
	Pattern       string        `json:"pattern"`
	Matches       []SearchMatch `json:"matches"`
	TotalMatches  int           `json:"total_matches"` // capped by max_results when truncated
	FilesSearched int           `json:"files_searched"`
	FilesSkipped  int           `json:"files_skipped"` // too large / binary / unreadable
	Truncated     bool          `json:"truncated"`     // scan stopped early due to max_results
}

// SearchContent runs a server-side regexp search over a file or directory
// tree. No matches is a success with an empty list (not an error), mirroring
// tool semantics rather than grep's exit code. Paths are sandboxed by roots.
func SearchContent(req SearchContentRequest, roots []string) (*SearchContentResult, error) {
	abs, err := validatePath(req.Path, roots)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.Pattern) == "" {
		return nil, errors.New("pattern is empty")
	}

	pattern := req.Pattern
	if req.IgnoreCase {
		pattern = "(?i)" + pattern
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("invalid pattern: %w", err)
	}

	maxResults := req.MaxResults
	if maxResults <= 0 {
		maxResults = defaultSearchMaxResults
	}
	if maxResults > maxSearchResults {
		maxResults = maxSearchResults
	}
	maxFileSize := req.MaxFileSize
	if maxFileSize <= 0 {
		maxFileSize = defaultSearchMaxFileSize
	}
	contextLines := req.ContextLines
	if contextLines < 0 {
		contextLines = 0
	}
	if contextLines > 20 {
		contextLines = 20
	}

	info, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}

	// Collect candidate files.
	var files []string
	if info.IsDir() {
		walkErr := filepath.WalkDir(abs, func(p string, d os.DirEntry, walkErr error) error {
			if walkErr != nil {
				if d != nil && d.IsDir() {
					return filepath.SkipDir // unreadable dir: skip, keep walking
				}
				return nil // unreadable file: skip
			}
			if !req.IncludeHidden && p != abs && isHiddenName(d.Name()) {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if d.IsDir() {
				return nil
			}
			if !matchAnyGlob(d.Name(), req.IncludeGlob, false) {
				return nil
			}
			files = append(files, p)
			return nil
		})
		if walkErr != nil {
			return nil, walkErr
		}
	} else {
		files = []string{abs}
	}

	result := &SearchContentResult{
		Path:    abs,
		Pattern: req.Pattern,
		Matches: []SearchMatch{},
	}

	for _, file := range files {
		if result.Truncated {
			break
		}
		fi, statErr := os.Stat(file)
		if statErr != nil || !fi.Mode().IsRegular() {
			result.FilesSkipped++
			continue
		}
		if fi.Size() > maxFileSize {
			result.FilesSkipped++
			continue
		}
		data, readErr := os.ReadFile(file)
		if readErr != nil {
			result.FilesSkipped++
			continue
		}
		if looksBinary(data) {
			result.FilesSkipped++
			continue
		}
		content, decErr := decodeToUTF8(data, detectEncoding(data))
		if decErr != nil {
			result.FilesSkipped++
			continue
		}
		result.FilesSearched++

		lines := strings.Split(content, "\n")
		for i, line := range lines {
			line = strings.TrimSuffix(line, "\r")
			if !re.MatchString(line) {
				continue
			}
			m := SearchMatch{File: file, Line: i + 1, Content: line}
			if contextLines > 0 {
				start := i - contextLines
				if start < 0 {
					start = 0
				}
				for j := start; j < i; j++ {
					m.Before = append(m.Before, strings.TrimSuffix(lines[j], "\r"))
				}
				end := i + 1 + contextLines
				if end > len(lines) {
					end = len(lines)
				}
				for j := i + 1; j < end; j++ {
					m.After = append(m.After, strings.TrimSuffix(lines[j], "\r"))
				}
			}
			result.Matches = append(result.Matches, m)
			if len(result.Matches) >= maxResults {
				result.Truncated = true
				break
			}
		}
	}

	result.TotalMatches = len(result.Matches)
	return result, nil
}

// ---------------------------------------------------------------------------
// remote_find_files
// ---------------------------------------------------------------------------

type FindFilesRequest struct {
	Path          string
	NameGlob      []string
	Type          string // "file" | "dir" | "any" (default any)
	MaxDepth      int    // 0 = unlimited; 1 = direct children only
	MaxResults    int
	IncludeHidden bool
	IgnoreCase    bool
}

type FindFileEntry struct {
	Path    string `json:"path"`
	Type    string `json:"type"` // "file" | "dir" | "symlink" | "other"
	Size    int64  `json:"size"`
	ModTime string `json:"mod_time"` // RFC3339
}

type FindFilesResult struct {
	Path        string          `json:"path"`
	Entries     []FindFileEntry `json:"entries"`
	Count       int             `json:"count"`
	Truncated   bool            `json:"truncated"`
	DirsSkipped int             `json:"dirs_skipped"` // unreadable directories
}

// FindFiles locates files/dirs by name glob under a directory, sandboxed by
// roots. Unlike a shell `find /` it stays inside the sandbox and reports
// truncation explicitly instead of dying on permission errors.
func FindFiles(req FindFilesRequest, roots []string) (*FindFilesResult, error) {
	abs, err := validatePath(req.Path, roots)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(abs); err != nil {
		return nil, err
	}

	findType := strings.ToLower(strings.TrimSpace(req.Type))
	if findType == "" {
		findType = "any"
	}
	if findType != "file" && findType != "dir" && findType != "any" {
		return nil, fmt.Errorf("invalid type %q (want file|dir|any)", req.Type)
	}

	maxResults := req.MaxResults
	if maxResults <= 0 {
		maxResults = defaultFindMaxResults
	}
	if maxResults > maxFindResults {
		maxResults = maxFindResults
	}

	result := &FindFilesResult{Path: abs, Entries: []FindFileEntry{}}

	err = filepath.WalkDir(abs, func(p string, d os.DirEntry, walkErr error) error {
		if result.Truncated {
			return filepath.SkipAll
		}
		if walkErr != nil {
			if d != nil && d.IsDir() {
				result.DirsSkipped++
				return filepath.SkipDir
			}
			return nil
		}
		if p == abs {
			return nil // the root itself is not a result
		}
		if !req.IncludeHidden && isHiddenName(d.Name()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		// Depth: direct children of abs are depth 1.
		if req.MaxDepth > 0 {
			rel, relErr := filepath.Rel(abs, p)
			if relErr == nil {
				depth := strings.Count(rel, string(os.PathSeparator)) + 1
				if depth > req.MaxDepth {
					if d.IsDir() {
						return filepath.SkipDir
					}
					return nil
				}
			}
		}

		if !matchAnyGlob(d.Name(), req.NameGlob, req.IgnoreCase) {
			return nil
		}

		entryType := "other"
		switch {
		case d.IsDir():
			entryType = "dir"
		case d.Type()&os.ModeSymlink != 0:
			entryType = "symlink"
		case d.Type().IsRegular():
			entryType = "file"
		}
		if findType == "file" && entryType != "file" {
			return nil
		}
		if findType == "dir" && entryType != "dir" {
			return nil
		}

		entry := FindFileEntry{Path: p, Type: entryType}
		if info, infoErr := d.Info(); infoErr == nil {
			entry.Size = info.Size()
			entry.ModTime = info.ModTime().Format(time.RFC3339)
		}
		result.Entries = append(result.Entries, entry)
		if len(result.Entries) >= maxResults {
			result.Truncated = true
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	result.Count = len(result.Entries)
	return result, nil
}
