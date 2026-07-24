package main

import (
	"regexp"
)

// redactRules scrub secrets out of command strings before they hit the
// audit log. Each rule replaces the sensitive portion with "***" while
// keeping enough context for troubleshooting.
var redactRules = []struct {
	re   *regexp.Regexp
	repl string
}{
	// mysql -pSecret / mysql -p'Secret'. The "mysql" match is case-insensitive
	// but -p stays case-sensitive so `-P3306` (port) is preserved.
	{regexp.MustCompile(`((?i:\bmysql\b)[^\n|;]*?)(-p)(?:\s*'[^']*'|\s*"[^"]*"|\S+)`), "${1}${2}***"},
	// Authorization: Bearer xxx (case-insensitive header form).
	{regexp.MustCompile(`(?i)(authorization\s*:\s*bearer\s+)[^\s"']+`), "${1}***"},
	// password=xxx / token=xxx / secret=xxx / api_key=xxx (URL & env forms).
	{regexp.MustCompile(`(?i)(\b(?:password|passwd|token|secret|api[_-]?key)\b\s*=\s*)[^\s&"']+`), "${1}***"},
}

// RedactCommand applies redactRules to a command string.
func RedactCommand(cmd string) string {
	for _, rule := range redactRules {
		cmd = rule.re.ReplaceAllString(cmd, rule.repl)
	}
	return cmd
}
