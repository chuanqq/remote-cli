package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Config struct {
	Port         string
	Token        string
	TLSCert      string
	TLSKey       string
	MaxTimeout   int
	MaxOutput    int
	RateLimit    int
	DefaultShell string
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
		Port:         getEnv("SHELL_API_PORT", "8080"),
		Token:        getEnv("SHELL_API_TOKEN", ""),
		TLSCert:      getEnv("SHELL_API_TLS_CERT", ""),
		TLSKey:       getEnv("SHELL_API_TLS_KEY", ""),
		MaxTimeout:   getEnvInt("SHELL_API_MAX_TIMEOUT", 300),
		MaxOutput:    getEnvInt("SHELL_API_MAX_OUTPUT", 1048576),
		RateLimit:    getEnvInt("SHELL_API_RATE_LIMIT", 60),
		DefaultShell: getEnv("SHELL_API_DEFAULT_SHELL", "bash"),
	}

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
