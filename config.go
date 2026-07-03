package main

import (
	"os"
	"strconv"
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
	// FSRoot, when set, sandboxes all file operation tools to this directory.
	// Empty means no filesystem restriction (file ops span the whole host,
	// same trust level as shell execution).
	FSRoot string
}

func LoadConfig() *Config {
	return &Config{
		Port:         getEnv("SHELL_API_PORT", "8080"),
		Token:        getEnv("SHELL_API_TOKEN", ""),
		TLSCert:      getEnv("SHELL_API_TLS_CERT", ""),
		TLSKey:       getEnv("SHELL_API_TLS_KEY", ""),
		MaxTimeout:   getEnvInt("SHELL_API_MAX_TIMEOUT", 300),
		MaxOutput:    getEnvInt("SHELL_API_MAX_OUTPUT", 1048576),
		RateLimit:    getEnvInt("SHELL_API_RATE_LIMIT", 60),
		DefaultShell: getEnv("SHELL_API_DEFAULT_SHELL", "bash"),
		FSRoot:       getEnv("SHELL_API_FS_ROOT", ""),
	}
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
