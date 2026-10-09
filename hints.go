package main

import (
	"regexp"
	"strings"
)

// ---------------------------------------------------------------------------
// Tool hints for remote_execute (advisory only, never enforced)
//
// Production audit showed 432 shell calls whose first command was ls / grep /
// cat / ps / tail / sed -n / head / find / ss, 436 `sleep N; check` polls and
// 217 `cd X && ...` prefixes. The dedicated tools return structured results
// and do not fail on zero matches, so the response nudges the agent towards
// them. Commands still run exactly as given.
// ---------------------------------------------------------------------------

var (
	leadingCdRe = regexp.MustCompile(`^\s*cd\s+[^;&|]+\s*(&&|;)`)
	sleepPollRe = regexp.MustCompile(`(^|[;&|\n])\s*sleep\s+\d+`)
	firstWordRe = regexp.MustCompile(`^\s*(?:sudo\s+)?([A-Za-z0-9_.-]+)(\s+-n)?`)
)

var firstWordHints = map[string]string{
	"cat":     "use remote_read_file (encoding-aware, line ranges via start_line/end_line, tail_lines)",
	"head":    "use remote_read_file with end_line or max_bytes",
	"tail":    "use remote_tail_log (cursors, regex filter, follow_seconds) or remote_read_file tail_lines",
	"grep":    "use remote_search_content (recursive RE2 search, context_lines, no failure on zero matches)",
	"rg":      "use remote_search_content",
	"find":    "use remote_find_files (name globs, type, max_depth, sandboxed)",
	"ls":      "use remote_list_dir (structured entries, sort_by, filter_glob)",
	"stat":    "use remote_stat (also hash and encoding detection)",
	"ps":      "use remote_list_processes (regex filter on full command line)",
	"pgrep":   "use remote_list_processes",
	"ss":      "use remote_check_port",
	"netstat": "use remote_check_port",
	"lsof":    "use remote_check_port",
}

// commandHint returns an advisory hint for command, or "".
func commandHint(command string) string {
	if sleepPollRe.MatchString(command) {
		return "polling with sleep: use remote_wait_for (returns as soon as a file/log/process/port/command " +
			"condition holds) or remote_tail_log follow_seconds; for long-running work use remote_spawn"
	}
	if strings.Contains(command, "nohup ") || strings.HasSuffix(strings.TrimSpace(command), "&") {
		return "background process: use remote_spawn (detached, server-managed log, job id for status/logs/kill)"
	}
	if m := firstWordRe.FindStringSubmatch(command); m != nil {
		word := m[1]
		if word == "sed" && m[2] != "" {
			return "use remote_read_file with start_line/end_line instead of sed -n"
		}
		if h, ok := firstWordHints[word]; ok {
			return h
		}
	}
	if leadingCdRe.MatchString(command) {
		return "pass working_directory instead of a leading `cd X &&`"
	}
	return ""
}
