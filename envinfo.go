package main

import (
	"context"
	"os"
	"os/exec"
	"os/user"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// probeToolList is the set of binaries probed for the environment profile.
// Ordered by how often they matter to remote operations.
var probeToolList = []string{
	"bash", "sh", "zsh",
	"python3", "python",
	"rg", "grep", "find", "awk", "sed", "jq",
	"git", "curl", "wget",
	"tar", "gzip", "unzip",
	"mysql", "redis-cli",
	"gcc", "g++", "make",
	"ss", "netstat", "lsof", "nc",
	"rsync", "ssh", "scp",
	"gssh",
	"docker", "kubectl", "systemctl", "crontab",
	"getconf", "file",
}

// ToolProbe reports one binary's availability and version banner.
type ToolProbe struct {
	Name    string `json:"name"`
	Found   bool   `json:"found"`
	Path    string `json:"path,omitempty"`
	Version string `json:"version,omitempty"` // first line of `--version`
}

// ServerProfile is the operator-facing server configuration snapshot.
type ServerProfile struct {
	Version       string   `json:"version"`
	FSRoots       []string `json:"fs_roots,omitempty"`
	DisabledTools []string `json:"disabled_tools,omitempty"`
	MaxTimeoutSec int      `json:"max_timeout_sec"`
	MaxOutput     int      `json:"max_output"`
	RateLimit     int      `json:"rate_limit"`
}

// EnvInfoResult is a one-call environment profile: replaces the usual
// battery of which/--version/uname probe commands.
type EnvInfoResult struct {
	Hostname     string        `json:"hostname"`
	OS           string        `json:"os"`
	Kernel       string        `json:"kernel,omitempty"`
	Arch         string        `json:"arch"`
	CPUs         int           `json:"cpus"`
	User         string        `json:"user"`
	Home         string        `json:"home"`
	DefaultShell string        `json:"default_shell"`
	EnvShell     string        `json:"env_shell,omitempty"`
	Locale       string        `json:"locale,omitempty"`
	MemoryMB     uint64        `json:"memory_mb"`
	LoadAverage  []float64     `json:"load_average"`
	Tools        []ToolProbe   `json:"tools"`
	Server       ServerProfile `json:"server"`
}

// GetEnvInfo builds the environment profile. Tool probing runs concurrently
// with per-tool timeouts so a hanging binary cannot stall the call.
func GetEnvInfo(cfg *Config) *EnvInfoResult {
	hostname, _ := os.Hostname()

	username := ""
	if u, err := user.Current(); err == nil {
		username = u.Username
	}
	home, _ := os.UserHomeDir()

	locale := os.Getenv("LC_ALL")
	if locale == "" {
		locale = os.Getenv("LC_CTYPE")
	}
	if locale == "" {
		locale = os.Getenv("LANG")
	}

	var disabled []string
	for name := range cfg.DisabledTools {
		disabled = append(disabled, name)
	}
	sort.Strings(disabled)

	return &EnvInfoResult{
		Hostname:     hostname,
		OS:           runtime.GOOS,
		Kernel:       kernelRelease(),
		Arch:         runtime.GOARCH,
		CPUs:         runtime.NumCPU(),
		User:         username,
		Home:         home,
		DefaultShell: cfg.DefaultShell,
		EnvShell:     os.Getenv("SHELL"),
		Locale:       locale,
		MemoryMB:     getMemoryMB(),
		LoadAverage:  getLoadAverage(),
		Tools:        probeTools(probeToolList),
		Server: ServerProfile{
			Version:       serverVersion,
			FSRoots:       cfg.FSRoots,
			DisabledTools: disabled,
			MaxTimeoutSec: cfg.MaxTimeout,
			MaxOutput:     cfg.MaxOutput,
			RateLimit:     cfg.RateLimit,
		},
	}
}

// probeTools checks every tool concurrently; version banners are fetched
// with a 1s per-tool timeout.
func probeTools(names []string) []ToolProbe {
	results := make([]ToolProbe, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			results[i] = ToolProbe{Name: name}
			path, err := exec.LookPath(name)
			if err != nil {
				return
			}
			results[i].Found = true
			results[i].Path = path
			results[i].Version = toolVersion(name)
		}(i, name)
	}
	wg.Wait()
	return results
}

// toolVersion returns the first line of `<tool> --version`, truncated.
func toolVersion(name string) string {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, "--version").CombinedOutput()
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	return truncateString(line, 120)
}

// kernelRelease returns `uname -sr` output where available.
func kernelRelease() string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "uname", "-sr").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
