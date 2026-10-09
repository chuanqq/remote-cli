package main

import (
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Port       string
	Token      string
	TLSCert    string
	TLSKey     string
	MaxTimeout int
	MaxOutput  int
	RateLimit  int
	// RateBurst is the token-bucket capacity per client host: how many
	// requests may arrive back to back before the RateLimit/min refill rate
	// takes over.
	RateBurst    int
	DefaultShell string
	// LogLevel filters the structured log: debug|info|warn|error. The MCP GET
	// keep-alive stream is only logged at debug.
	LogLevel string
	// MCPHeartbeat is the ping interval on the MCP GET stream; it keeps idle
	// middleboxes from cutting the connection. 0 disables.
	MCPHeartbeat time.Duration
	// ShutdownGrace is how long in-flight executions may keep running after
	// SIGTERM/SIGINT before they are killed.
	ShutdownGrace time.Duration
	// JobDir holds the combined stdout/stderr log of every remote_spawn job.
	JobDir string
	// DenyCommand, when non-nil, rejects any shell command (execute, session
	// execute, spawn, wait_for command) matching it before anything runs.
	DenyCommand *regexp.Regexp
	// FSRoots, when non-empty, sandboxes all file operation tools to these
	// directories: a path is allowed if it falls under ANY of the roots.
	// Empty means no filesystem restriction (file ops span the whole host,
	// same trust level as shell execution).
	FSRoots []string
	// DisabledTools names MCP tools that must NOT be registered at startup
	// (e.g. "remote_execute"). A disabled tool is invisible to MCP clients.
	DisabledTools map[string]bool
	// ReadOnly, when true, forces the server into read-only mode: every
	// mutating tool and REST endpoint is disabled regardless of any other
	// setting. This flag has the HIGHEST priority — nothing overrides it.
	ReadOnly bool
}

func LoadConfig() *Config {
	cfg := &Config{
		Port:          getEnv("SHELL_API_PORT", "8080"),
		Token:         getEnv("SHELL_API_TOKEN", ""),
		TLSCert:       getEnv("SHELL_API_TLS_CERT", ""),
		TLSKey:        getEnv("SHELL_API_TLS_KEY", ""),
		MaxTimeout:    getEnvInt("SHELL_API_MAX_TIMEOUT", 300),
		MaxOutput:     getEnvInt("SHELL_API_MAX_OUTPUT", 1048576),
		RateLimit:     getEnvInt("SHELL_API_RATE_LIMIT", 120),
		RateBurst:     getEnvInt("SHELL_API_RATE_BURST", 60),
		DefaultShell:  getEnv("SHELL_API_DEFAULT_SHELL", "bash"),
		LogLevel:      getEnv("SHELL_API_LOG_LEVEL", "info"),
		MCPHeartbeat:  time.Duration(getEnvInt("SHELL_API_MCP_HEARTBEAT", 30)) * time.Second,
		ShutdownGrace: time.Duration(getEnvInt("SHELL_API_SHUTDOWN_GRACE", 30)) * time.Second,
		JobDir:        getEnv("SHELL_API_JOB_DIR", filepath.Join(os.TempDir(), "remote-agent-proxy-jobs")),
	}

	cfg.DenyCommand = buildDenyPattern(
		getEnv("SHELL_API_DENY_COMMANDS", ""),
		parseReadOnly(getEnv("SHELL_API_BLOCK_JUMP_HOST", "")),
	)

	// SHELL_API_FS_ROOT accepts a comma-separated list of directory prefixes.
	// Comma (not colon) is the separator so Windows drive paths keep working.
	for _, p := range strings.Split(getEnv("SHELL_API_FS_ROOT", ""), ",") {
		if p = strings.TrimSpace(p); p != "" {
			cfg.FSRoots = append(cfg.FSRoots, filepath.Clean(p))
		}
	}

	// SHELL_API_DISABLED_TOOLS is a comma-separated tool blacklist applied at
	// registration time.
	cfg.DisabledTools = make(map[string]bool)
	for _, name := range strings.Split(getEnv("SHELL_API_DISABLED_TOOLS", ""), ",") {
		if name = strings.TrimSpace(name); name != "" {
			cfg.DisabledTools[name] = true
		}
	}

	// SHELL_API_READONLY is applied LAST so it wins over every setting above:
	// it force-disables all mutating tools and latches the process-wide guard.
	cfg.applyReadOnly(readOnlyFromEnv())

	return cfg
}

// jumpHostPattern matches a command that starts (or chains into) a login to
// ANOTHER host: ssh/gssh/scp/sftp/sshpass at the start of a pipeline segment,
// optionally behind sudo. Anything run over such a hop escapes this server's
// audit log entirely. `ssh-keygen`, `sshd` etc. are not matched.
const jumpHostPattern = "(?:^|[;&|()`\\n]|\\$\\()\\s*(?:sudo\\s+)?(?:gssh|ssh|scp|sftp|sshpass)(?:\\s|$)"

// buildDenyPattern combines the operator regex (SHELL_API_DENY_COMMANDS) with
// the built-in jump-host pattern (SHELL_API_BLOCK_JUMP_HOST). An operator
// regex that fails to compile is fatal: silently ignoring a security control
// is worse than refusing to start.
func buildDenyPattern(custom string, blockJump bool) *regexp.Regexp {
	var parts []string
	if blockJump {
		parts = append(parts, jumpHostPattern)
	}
	if custom = strings.TrimSpace(custom); custom != "" {
		if _, err := regexp.Compile(custom); err != nil {
			log.Fatalf("SHELL_API_DENY_COMMANDS is not a valid RE2 regex: %v", err)
		}
		parts = append(parts, "(?:"+custom+")")
	}
	if len(parts) == 0 {
		return nil
	}
	return regexp.MustCompile(strings.Join(parts, "|"))
}

// applyReadOnly turns read-only mode on when enabled is true. It is idempotent
// and one-way: calling it with false never clears a previously enabled state,
// so read-only cannot be downgraded by a later config pass.
func (c *Config) applyReadOnly(enabled bool) {
	if !enabled {
		return
	}
	c.ReadOnly = true
	if c.DisabledTools == nil {
		c.DisabledTools = make(map[string]bool)
	}
	// Fold the forced set into DisabledTools so remote_get_env_info and the
	// startup log report the effective blacklist, not just the operator's.
	for _, name := range readOnlyDisabledTools() {
		c.DisabledTools[name] = true
	}
	enableReadOnly()
}

// toolEnabled reports whether the named MCP tool should be registered.
// Read-only mode is checked FIRST and uses allowlist semantics: an unknown or
// mutating tool is denied even if the operator did not blacklist it.
func (c *Config) toolEnabled(name string) bool {
	if c.ReadOnly && !readOnlyTools[name] {
		return false
	}
	return !c.DisabledTools[name]
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
	}
	return fallback
}
