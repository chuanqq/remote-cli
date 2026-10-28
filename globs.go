package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// ---------------------------------------------------------------------------
// Globs for include_glob / exclude_glob / name_glob / filter_glob
//
// A pattern without "/" matches the base name. A pattern with "/" matches the
// path relative to the search root ("conf/*.conf"); one starting with "/"
// matches the absolute path. "**" spans directories ("**/*.conf",
// "src/**/test_*.cpp") and "{a,b}" alternates ("*.{h,cpp}"), as in the globs
// of rg and of Claude Code's own Grep/Glob tools. An earlier version used
// filepath.Match on the base name only, so "conf/*.conf", "**/*.conf" and
// "*.{h,cpp}" silently matched nothing (and invalid globs too).
// ---------------------------------------------------------------------------

type globSet struct {
	name []*regexp.Regexp // patterns without "/": base name
	rel  []*regexp.Regexp // relative patterns with "/": path below the root
	abs  []*regexp.Regexp // patterns starting with "/": absolute path
}

// compileGlobs compiles patterns; it returns nil (match everything) when
// there are none. Invalid patterns are an error, not a silent non-match.
func compileGlobs(patterns []string, ignoreCase bool) (*globSet, error) {
	var g globSet
	for _, p := range patterns {
		p = strings.TrimPrefix(filepath.ToSlash(strings.TrimSpace(p)), "./")
		if p == "" {
			continue
		}
		re, err := globRegexp(p, ignoreCase)
		if err != nil {
			return nil, fmt.Errorf("invalid glob %q: %w", p, err)
		}
		switch {
		case strings.HasPrefix(p, "/"):
			g.abs = append(g.abs, re)
		case strings.Contains(p, "/"):
			g.rel = append(g.rel, re)
		default:
			g.name = append(g.name, re)
		}
	}
	if len(g.name)+len(g.rel)+len(g.abs) == 0 {
		return nil, nil
	}
	return &g, nil
}

// match reports whether an entry matches any pattern. name is its base name,
// rel its path below the search root, abs its absolute path. A nil set
// matches everything.
func (g *globSet) match(name, rel, abs string) bool {
	if g == nil {
		return true
	}
	for _, re := range g.name {
		if re.MatchString(name) {
			return true
		}
	}
	if len(g.rel) > 0 {
		rel = filepath.ToSlash(rel)
		for _, re := range g.rel {
			if re.MatchString(rel) {
				return true
			}
		}
	}
	if len(g.abs) > 0 {
		abs = filepath.ToSlash(abs)
		for _, re := range g.abs {
			if re.MatchString(abs) {
				return true
			}
		}
	}
	return false
}

// matchAny is match for a nil-means-nothing set (exclude_glob).
func (g *globSet) matchAny(name, rel, abs string) bool {
	return g != nil && g.match(name, rel, abs)
}

// globRegexp translates one glob into an anchored RE2 expression.
func globRegexp(p string, ignoreCase bool) (*regexp.Regexp, error) {
	var b strings.Builder
	if ignoreCase {
		b.WriteString("(?i)")
	}
	b.WriteString("^")
	braces := 0
	for i := 0; i < len(p); i++ {
		switch c := p[i]; c {
		case '*':
			if i+1 < len(p) && p[i+1] == '*' {
				segStart := i == 0 || p[i-1] == '/'
				j := i + 2
				switch {
				case segStart && j < len(p) && p[j] == '/':
					b.WriteString("(?:[^/]*/)*") // "**/": any number of directories
					i = j
				case segStart && j == len(p):
					b.WriteString(".*") // trailing "**": everything below
					i = j - 1
				default:
					b.WriteString("[^/]*")
					i = j - 1
				}
				continue
			}
			b.WriteString("[^/]*")
		case '?':
			b.WriteString("[^/]")
		case '[':
			j := i + 1
			neg := j < len(p) && (p[j] == '!' || p[j] == '^')
			if neg {
				j++
			}
			start := j
			if j < len(p) && p[j] == ']' {
				j++
			}
			for j < len(p) && p[j] != ']' {
				j++
			}
			if j >= len(p) {
				return nil, errors.New("unclosed [")
			}
			b.WriteByte('[')
			if neg {
				b.WriteString("^/")
			}
			for k := start; k < j; k++ {
				if ch := p[k]; ch == '\\' || ch == '[' || ch == ']' || ch == '^' {
					b.WriteByte('\\')
				}
				b.WriteByte(p[k])
			}
			b.WriteByte(']')
			i = j
		case '{':
			braces++
			b.WriteString("(?:")
		case '}':
			if braces == 0 {
				b.WriteString(`\}`)
				continue
			}
			braces--
			b.WriteString(")")
		case ',':
			if braces > 0 {
				b.WriteString("|")
			} else {
				b.WriteString(",")
			}
		case '\\':
			if i+1 < len(p) {
				i++
			}
			b.WriteString(regexp.QuoteMeta(p[i : i+1]))
		default:
			b.WriteString(regexp.QuoteMeta(p[i : i+1]))
		}
	}
	if braces > 0 {
		return nil, errors.New("unclosed {")
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}

// walkTree is filepath.WalkDir, except that the root may be a symlink to a
// directory (app/log -> /ssd1/logs/app): it is followed, as grep -r and
// find -H do, and paths are still reported under root. Symlinks met during
// the walk are not followed; fn sees them as entries.
func walkTree(root string, fn fs.WalkDirFunc) error {
	if fi, err := os.Lstat(root); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		if real, err := filepath.EvalSymlinks(root); err == nil {
			if st, err := os.Stat(real); err == nil && st.IsDir() {
				return filepath.WalkDir(real, func(p string, d fs.DirEntry, err error) error {
					return fn(root+p[len(real):], d, err)
				})
			}
		}
	}
	return filepath.WalkDir(root, fn)
}

// relPath is p relative to root ("." for root itself).
func relPath(root, p string) string {
	if rel, err := filepath.Rel(root, p); err == nil {
		return rel
	}
	return p
}
