package main

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// LintResult is the outcome of an advisory syntax check after a write.
// Linting never fails the write itself; a missing interpreter is a skip.
type LintResult struct {
	Lang    string `json:"lang"`
	OK      bool   `json:"ok"`
	Skipped bool   `json:"skipped,omitempty"`
	Output  string `json:"output,omitempty"`
}

// LintFile runs a cheap syntax check on a freshly written file:
// "bash" -> bash -n, "python" -> python3 -m py_compile.
func LintFile(absPath, lang string) *LintResult {
	switch strings.ToLower(strings.TrimSpace(lang)) {
	case "bash", "sh":
		return lintWith("bash", "bash", "-n", absPath)
	case "python", "py":
		if _, err := exec.LookPath("python3"); err == nil {
			return lintWith("python", "python3", "-m", "py_compile", absPath)
		}
		return lintWith("python", "python", "-m", "py_compile", absPath)
	default:
		return &LintResult{Lang: lang, Skipped: true, Output: "unsupported lint language"}
	}
}

func lintWith(lang, bin string, args ...string) *LintResult {
	if _, err := exec.LookPath(bin); err != nil {
		return &LintResult{Lang: lang, Skipped: true, Output: bin + " not found in PATH"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, args...).CombinedOutput()
	res := &LintResult{Lang: lang, OK: err == nil}
	if err != nil {
		s := strings.TrimSpace(string(out))
		if s == "" {
			s = err.Error()
		}
		if len(s) > 2000 {
			s = s[:2000] + "..."
		}
		res.Output = s
	}
	return res
}
