package main

import (
	"fmt"
	"log"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Process roles selected by REMOTECTL_MODE. 2.0 adds master/slaver; the
// default is standalone, which behaves exactly like 1.8.0.
const (
	modeStandalone = "standalone"
	modeMaster     = "master"
	modeSlaver     = "slaver"
)

// defaultAPIToken is the fallback credential used when neither a token nor a
// token hash is configured. It is a public constant, so it must never reach a
// network listener: startup refuses a non-loopback listen while it is in
// effect (design decision D6).
const defaultAPIToken = "remote-agent-proxy-default-token"

type Config struct {
	Port string
	// Listen is the public MCP listen address ("host:port"). It defaults to
	// ":"+Port, preserving 1.x behavior. Configured with REMOTECTL_LISTEN /
	// SHELL_API_LISTEN.
	Listen string
	Token  string
	// TokenHash, when set instead of Token, is "sha256:<salt-hex>:<hash-hex>"
	// (hash = SHA-256(salt || token)): the server stores no plaintext
	// credential. Mutually exclusive with Token; enforced at startup.
	TokenHash string
	// TokenIsDefault marks the built-in fallback credential (no explicit
	// token/token-hash configured). Such a credential is only allowed on a
	// loopback listener (D6).
	TokenIsDefault bool
	// Mode is standalone (default), master or slaver (REMOTECTL_MODE).
	Mode string
	// MCPServerName selects the MCP serverInfo.name: new (default), compat
	// (remote-shell*) or a custom value (REMOTECTL_MCP_SERVER_NAME, D20).
	MCPServerName string
	// StateDir holds node identity and control-plane state
	// (REMOTECTL_STATE_DIR). It defaults to <executable dir>/state.
	StateDir string

	// --- Control plane (master/slaver, 2.0) ---
	ControlListen string // master: control gRPC listen address (no default, D33)
	MasterURL     string // slaver: master control endpoint
	ClusterToken  string // slaver: pre-shared plaintext token
	// ClusterTokenHash is master-side "sha256:<salt>:<hash>"; the master never
	// stores the plaintext token.
	ClusterTokenHash     string
	MasterLocal          bool   // master executes calls without node_id (D2)
	NodeApproval         string // auto | manual | allowlist
	NodeAllowlist        []string
	EnableNodeAdminTools bool
	NodeName             string
	NodeIDOverride       string
	// ResolvedNodeID is filled at runtime by the slaver after reading or
	// generating state/slaver/node-id. It labels persisted state (job
	// manifests) and is not an environment setting.
	ResolvedNodeID    string
	HeartbeatInterval time.Duration
	LeaseTTL          time.Duration
	StaleAfter        time.Duration
	OfflineAfter      time.Duration
	// LegacyEnvs records which SHELL_API_* variables were used instead of
	// their REMOTECTL_* equivalents, for one deprecation warning at startup.
	LegacyEnvs []string

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
	// TrustedProxies 仅允许这些 TCP 对端提供客户端 IP，空列表不信任任何转发头。
	TrustedProxies []netip.Prefix
	// LogLevel filters the structured log: debug|info|warn|error. The MCP GET
	// keep-alive stream is only logged at debug.
	LogLevel string
	// MCPHeartbeat is the ping interval on the MCP GET stream; it keeps idle
	// middleboxes from cutting the connection. 0 disables.
	MCPHeartbeat time.Duration
	// ShutdownGrace is how long in-flight executions may keep running after
	// SIGTERM/SIGINT before they are killed.
	ShutdownGrace time.Duration
	// CallBudget is the longest a single MCP tool call blocks. MCP clients
	// abort requests on their own timeout (60s in the TypeScript SDK; seen in
	// production as calls cancelled at exactly 60s), so work that takes longer
	// returns a handle instead: remote_execute state=running (fetch with
	// remote_execution_result), remote_wait_for state=pending (resume by
	// wait_id); follow_seconds is capped to it.
	CallBudget time.Duration
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
	// mutating tool is disabled regardless of any other setting. This flag has
	// the HIGHEST priority — nothing overrides it.
	ReadOnly bool
}

// defaultCallBudgetSec stays clear of the common 60s client request timeout.
const defaultCallBudgetSec = 50

// callBudget returns the effective call budget (configs built by hand in
// tests may leave it zero).
func (c *Config) callBudget() time.Duration {
	if c == nil || c.CallBudget <= 0 {
		return defaultCallBudgetSec * time.Second
	}
	return c.CallBudget
}

// callBudgetSec is callBudget in whole seconds, for messages and descriptions.
func (c *Config) callBudgetSec() int {
	return int(c.callBudget() / time.Second)
}

func LoadConfig() *Config {
	cfg := &Config{}

	// Every 1.x setting is read through the dual-read helper: REMOTECTL_<NAME>
	// wins, SHELL_API_<NAME> remains the compatibility fallback.
	cfg.Port = cfg.env("PORT", "8080")
	cfg.Listen = cfg.env("LISTEN", ":"+cfg.Port)
	cfg.Token = cfg.env("TOKEN", "")
	cfg.TokenHash = cfg.env("TOKEN_HASH", "")
	cfg.TLSCert = cfg.env("TLS_CERT", "")
	cfg.TLSKey = cfg.env("TLS_KEY", "")
	cfg.MaxTimeout = cfg.envInt("MAX_TIMEOUT", 300)
	cfg.MaxOutput = cfg.envInt("MAX_OUTPUT", 1048576)
	cfg.RateLimit = cfg.envInt("RATE_LIMIT", 120)
	cfg.RateBurst = cfg.envInt("RATE_BURST", 60)
	cfg.DefaultShell = cfg.env("DEFAULT_SHELL", "bash")
	cfg.LogLevel = cfg.env("LOG_LEVEL", "info")
	cfg.MCPHeartbeat = time.Duration(cfg.envInt("MCP_HEARTBEAT", 30)) * time.Second
	cfg.ShutdownGrace = time.Duration(cfg.envInt("SHUTDOWN_GRACE", 30)) * time.Second
	cfg.JobDir = cfg.env("JOB_DIR", filepath.Join(os.TempDir(), "remote-agent-proxy-jobs"))

	mode, err := parseProcessMode(getEnv("REMOTECTL_MODE", modeStandalone))
	if err != nil {
		log.Fatalf("REMOTECTL_MODE: %v", err)
	}
	cfg.Mode = mode
	cfg.MCPServerName = getEnv("REMOTECTL_MCP_SERVER_NAME", "new")
	cfg.StateDir = getEnv("REMOTECTL_STATE_DIR", defaultStateDir())

	// Control-plane identity and bootstrap (2.0). These are new variables, so
	// there is no legacy fallback.
	cfg.ControlListen = getEnv("REMOTECTL_CONTROL_LISTEN", "")
	cfg.MasterURL = getEnv("REMOTECTL_MASTER_URL", "")
	cfg.ClusterToken = getEnv("REMOTECTL_CLUSTER_TOKEN", "")
	if cfg.ClusterToken == "" {
		if f := getEnv("REMOTECTL_CLUSTER_TOKEN_FILE", ""); f != "" {
			data, err := os.ReadFile(f)
			if err != nil {
				log.Fatalf("REMOTECTL_CLUSTER_TOKEN_FILE: %v", err)
			}
			cfg.ClusterToken = strings.TrimSpace(string(data))
		}
	}
	cfg.ClusterTokenHash = getEnv("REMOTECTL_CLUSTER_TOKEN_HASH", "")
	cfg.MasterLocal = !isOffValue(getEnv("REMOTECTL_MASTER_LOCAL", "on"))
	cfg.NodeApproval = strings.ToLower(strings.TrimSpace(getEnv("REMOTECTL_NODE_APPROVAL", "manual")))
	switch cfg.NodeApproval {
	case "auto", "manual", "allowlist":
	case "":
		cfg.NodeApproval = "manual"
	default:
		log.Fatalf("REMOTECTL_NODE_APPROVAL: must be auto|manual|allowlist, got %q", cfg.NodeApproval)
	}
	for _, id := range strings.Split(getEnv("REMOTECTL_NODE_ALLOWLIST", ""), ",") {
		if id = strings.TrimSpace(id); id != "" {
			cfg.NodeAllowlist = append(cfg.NodeAllowlist, id)
		}
	}
	cfg.EnableNodeAdminTools = parseReadOnly(getEnv("REMOTECTL_ENABLE_NODE_ADMIN_TOOLS", ""))
	cfg.NodeName = getEnv("REMOTECTL_NODE_NAME", "")
	cfg.NodeIDOverride = getEnv("REMOTECTL_NODE_ID", "")
	cfg.HeartbeatInterval = time.Duration(getEnvInt("REMOTECTL_HEARTBEAT_SEC", 10)) * time.Second
	cfg.LeaseTTL = time.Duration(getEnvInt("REMOTECTL_LEASE_TTL_SEC", 30)) * time.Second
	cfg.StaleAfter = time.Duration(getEnvInt("REMOTECTL_STALE_AFTER_SEC", 30)) * time.Second
	cfg.OfflineAfter = time.Duration(getEnvInt("REMOTECTL_OFFLINE_AFTER_SEC", 50)) * time.Second

	trusted, err := parseTrustedProxies(cfg.env("TRUSTED_PROXIES", ""))
	if err != nil {
		log.Fatalf("REMOTECTL_TRUSTED_PROXIES / SHELL_API_TRUSTED_PROXIES: %v", err)
	}
	cfg.TrustedProxies = trusted

	if b := cfg.envInt("CALL_BUDGET", defaultCallBudgetSec); b > 0 {
		cfg.CallBudget = time.Duration(b) * time.Second
	} else {
		cfg.CallBudget = defaultCallBudgetSec * time.Second
	}

	cfg.DenyCommand = buildDenyPattern(
		cfg.env("DENY_COMMANDS", ""),
		parseReadOnly(cfg.env("BLOCK_JUMP_HOST", "")),
	)

	// FSRoot accepts a comma-separated list of directory prefixes. Comma (not
	// colon) is the separator so Windows drive paths keep working.
	for _, p := range strings.Split(cfg.env("FS_ROOT", ""), ",") {
		if p = strings.TrimSpace(p); p != "" {
			cfg.FSRoots = append(cfg.FSRoots, filepath.Clean(p))
		}
	}

	// DISABLED_TOOLS is a comma-separated tool blacklist applied at
	// registration time.
	cfg.DisabledTools = make(map[string]bool)
	for _, name := range strings.Split(cfg.env("DISABLED_TOOLS", ""), ",") {
		if name = strings.TrimSpace(name); name != "" {
			cfg.DisabledTools[name] = true
		}
	}

	// No explicit credential: fall back to the built-in token. It is only
	// accepted on a loopback listener (validated before the server starts).
	if cfg.Token == "" && cfg.TokenHash == "" {
		cfg.Token = defaultAPIToken
		cfg.TokenIsDefault = true
	} else if cfg.TokenHash == "" && cfg.Token == defaultAPIToken {
		// Explicitly setting the public default value does not make it secret.
		cfg.TokenIsDefault = true
	}

	// READONLY is applied LAST so it wins over every setting above: it
	// force-disables all mutating tools and latches the process-wide guard.
	cfg.applyReadOnly(parseReadOnly(cfg.env("READONLY", "")))

	return cfg
}

// env reads REMOTECTL_<name> first, then SHELL_API_<name>, and records the
// legacy name for a single startup deprecation warning.
func (c *Config) env(name, fallback string) string {
	if v := os.Getenv("REMOTECTL_" + name); v != "" {
		return v
	}
	if v := os.Getenv("SHELL_API_" + name); v != "" {
		c.LegacyEnvs = append(c.LegacyEnvs, "SHELL_API_"+name)
		return v
	}
	return fallback
}

func (c *Config) envInt(name string, fallback int) int {
	if v := os.Getenv("REMOTECTL_" + name); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
		return fallback
	}
	if v := os.Getenv("SHELL_API_" + name); v != "" {
		c.LegacyEnvs = append(c.LegacyEnvs, "SHELL_API_"+name)
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
	}
	return fallback
}

// parseProcessMode validates REMOTECTL_MODE. An empty value means standalone.
func parseProcessMode(s string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", modeStandalone:
		return modeStandalone, nil
	case modeMaster:
		return modeMaster, nil
	case modeSlaver:
		return modeSlaver, nil
	default:
		return "", fmt.Errorf("must be one of standalone|master|slaver, got %q", s)
	}
}

// defaultStateDir mirrors D34: state lives next to the deployed binary.
func defaultStateDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "state"
	}
	return filepath.Join(filepath.Dir(exe), "state")
}

// parseListenHost returns the host part of a "host:port" listen address.
func parseListenHost(listen string) (string, error) {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return "", fmt.Errorf("listen address %q must be host:port: %w", listen, err)
	}
	return host, nil
}

// listenIsLoopback reports whether listen accepts loopback connections only.
// A missing host (":8099"), a wildcard address, an unresolvable hostname and
// an unparsable value are all treated as non-loopback: the D6 guard must fail
// closed.
func listenIsLoopback(listen string) bool {
	host, err := parseListenHost(listen)
	if err != nil || host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// validateListenSecurity enforces D6: the built-in default token may only be
// used on a loopback listener. Non-loopback listeners require an explicit
// credential, so the process never exposes a public, well-known token.
func (c *Config) validateListenSecurity() error {
	if _, err := parseListenHost(c.Listen); err != nil {
		return err
	}
	if !c.TokenIsDefault || listenIsLoopback(c.Listen) {
		return nil
	}
	return fmt.Errorf("refusing to start with the built-in default token on non-loopback listen %q: "+
		"set REMOTECTL_TOKEN / SHELL_API_TOKEN (or the *_TOKEN_HASH variant), or restrict the listener with "+
		"REMOTECTL_LISTEN=127.0.0.1:<port>", c.Listen)
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
			log.Fatalf("REMOTECTL_DENY_COMMANDS / SHELL_API_DENY_COMMANDS is not a valid RE2 regex: %v", err)
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

// isOffValue interprets REMOTECTL_MASTER_LOCAL: only an explicit off-like
// value disables local execution.
func isOffValue(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "off", "false", "0", "no", "disabled":
		return true
	default:
		return false
	}
}

// validateMasterConfig enforces the master-mode startup contract: the control
// port has no default (D33) and the cluster token hash must be well formed.
func (c *Config) validateMasterConfig() error {
	if strings.TrimSpace(c.ControlListen) == "" {
		return fmt.Errorf("REMOTECTL_CONTROL_LISTEN is required in master mode (the control port has no default)")
	}
	if c.ClusterTokenHash != "" {
		if _, _, err := parseTokenHash(c.ClusterTokenHash); err != nil {
			return fmt.Errorf("REMOTECTL_CLUSTER_TOKEN_HASH: %w", err)
		}
		return nil
	}
	if _, err := os.Stat(clusterTokensPath(c.StateDir)); err != nil {
		return fmt.Errorf("no cluster token configured: set REMOTECTL_CLUSTER_TOKEN_HASH or run `cluster-token create`/`rotate`")
	}
	return nil
}

// validateSlaverConfig enforces the slaver bootstrap contract.
func (c *Config) validateSlaverConfig() error {
	if strings.TrimSpace(c.MasterURL) == "" {
		return fmt.Errorf("REMOTECTL_MASTER_URL is required in slaver mode")
	}
	if strings.TrimSpace(c.ClusterToken) == "" {
		return fmt.Errorf("REMOTECTL_CLUSTER_TOKEN or REMOTECTL_CLUSTER_TOKEN_FILE is required in slaver mode")
	}
	return nil
}
