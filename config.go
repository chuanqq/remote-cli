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

	return cfg
}

// toolEnabled reports whether the named MCP tool should be registered.
func (c *Config) toolEnabled(name string) bool {
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
