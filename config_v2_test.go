package main

import (
	"strings"
	"testing"
)

// clearConfigEnv removes both REMOTECTL_* and SHELL_API_* variants so a test
// starts from a known state regardless of the developer shell.
func clearConfigEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"PORT", "LISTEN", "TOKEN", "TOKEN_HASH", "TLS_CERT", "TLS_KEY",
		"MAX_TIMEOUT", "MAX_OUTPUT", "RATE_LIMIT", "RATE_BURST", "DEFAULT_SHELL",
		"LOG_LEVEL", "MCP_HEARTBEAT", "SHUTDOWN_GRACE", "JOB_DIR", "TRUSTED_PROXIES",
		"CALL_BUDGET", "DENY_COMMANDS", "BLOCK_JUMP_HOST", "FS_ROOT",
		"DISABLED_TOOLS", "READONLY", "MODE", "STATE_DIR", "MCP_SERVER_NAME",
	} {
		t.Setenv("REMOTECTL_"+name, "")
		t.Setenv("SHELL_API_"+name, "")
	}
}

func TestParseProcessMode(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", modeStandalone, false},
		{"standalone", modeStandalone, false},
		{"master", modeMaster, false},
		{"SLAVER", modeSlaver, false},
		{" master ", modeMaster, false},
		{"cluster", "", true},
	} {
		got, err := parseProcessMode(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseProcessMode(%q) = %q, want error", tc.in, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("parseProcessMode(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
}

func TestListenIsLoopback(t *testing.T) {
	for _, tc := range []struct {
		listen string
		want   bool
	}{
		{"127.0.0.1:8099", true},
		{"127.0.0.2:8099", true},
		{"[::1]:8099", true},
		{"localhost:8099", true},
		{"LocalHost:8099", true},
		{":8099", false},
		{"0.0.0.0:8099", false},
		{"[::]:8099", false},
		{"10.0.0.1:8099", false},
		{"example.com:8099", false},
		{"not-an-address", false},
	} {
		if got := listenIsLoopback(tc.listen); got != tc.want {
			t.Errorf("listenIsLoopback(%q) = %t, want %t", tc.listen, got, tc.want)
		}
	}
}

func TestValidateListenSecurity(t *testing.T) {
	defaultToken := &Config{Listen: "0.0.0.0:8099", Token: defaultAPIToken, TokenIsDefault: true}
	if err := defaultToken.validateListenSecurity(); err == nil {
		t.Fatal("default token on a non-loopback listener must be refused")
	}

	loopback := &Config{Listen: "127.0.0.1:8099", Token: defaultAPIToken, TokenIsDefault: true}
	if err := loopback.validateListenSecurity(); err != nil {
		t.Fatalf("default token on loopback should be allowed: %v", err)
	}

	explicit := &Config{Listen: "0.0.0.0:8099", Token: "s3cret"}
	if err := explicit.validateListenSecurity(); err != nil {
		t.Fatalf("explicit token on a public listener should be allowed: %v", err)
	}

	badListen := &Config{Listen: "no-port", TokenIsDefault: true}
	if err := badListen.validateListenSecurity(); err == nil {
		t.Fatal("an unparsable listen address must be refused")
	}
}

func TestLoadConfigEnvPrecedence(t *testing.T) {
	clearConfigEnv(t)

	t.Setenv("REMOTECTL_PORT", "9999")
	t.Setenv("SHELL_API_PORT", "8888")
	cfg := LoadConfig()
	if cfg.Port != "9999" {
		t.Fatalf("REMOTECTL_PORT should win, got %q", cfg.Port)
	}
	for _, legacy := range cfg.LegacyEnvs {
		if strings.Contains(legacy, "PORT") {
			t.Fatalf("legacy PORT should not be recorded when REMOTECTL_PORT is set: %v", cfg.LegacyEnvs)
		}
	}

	clearConfigEnv(t)
	t.Setenv("SHELL_API_PORT", "8888")
	cfg = LoadConfig()
	if cfg.Port != "8888" {
		t.Fatalf("legacy SHELL_API_PORT should be honored, got %q", cfg.Port)
	}
	if !containsString(cfg.LegacyEnvs, "SHELL_API_PORT") {
		t.Fatalf("legacy SHELL_API_PORT should be recorded, got %v", cfg.LegacyEnvs)
	}
}

func TestLoadConfigListenDefault(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("SHELL_API_PORT", "8099")
	cfg := LoadConfig()
	if cfg.Listen != ":8099" {
		t.Fatalf("default listen = %q, want :8099", cfg.Listen)
	}
}

func TestLoadConfigDefaultToken(t *testing.T) {
	clearConfigEnv(t)
	cfg := LoadConfig()
	if cfg.Token != defaultAPIToken || !cfg.TokenIsDefault {
		t.Fatalf("no credential should fall back to the built-in default token, got token=%q default=%t",
			cfg.Token, cfg.TokenIsDefault)
	}

	clearConfigEnv(t)
	t.Setenv("REMOTECTL_TOKEN", "explicit-secret")
	cfg = LoadConfig()
	if cfg.Token != "explicit-secret" || cfg.TokenIsDefault {
		t.Fatalf("explicit token must not be treated as default, got token=%q default=%t",
			cfg.Token, cfg.TokenIsDefault)
	}

	clearConfigEnv(t)
	t.Setenv("REMOTECTL_TOKEN", defaultAPIToken)
	cfg = LoadConfig()
	if !cfg.TokenIsDefault {
		t.Fatal("explicitly setting the public default token must still be treated as default")
	}

	clearConfigEnv(t)
	t.Setenv("REMOTECTL_TOKEN_HASH", "sha256:0001020304050607:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	cfg = LoadConfig()
	if cfg.TokenIsDefault {
		t.Fatal("a token hash is an explicit credential and must not be treated as default")
	}
}

func TestLoadConfigMode(t *testing.T) {
	clearConfigEnv(t)
	if cfg := LoadConfig(); cfg.Mode != modeStandalone {
		t.Fatalf("default mode = %q, want standalone", cfg.Mode)
	}

	clearConfigEnv(t)
	t.Setenv("REMOTECTL_MODE", "master")
	if cfg := LoadConfig(); cfg.Mode != modeMaster {
		t.Fatalf("REMOTECTL_MODE=master not honored: %q", cfg.Mode)
	}

	clearConfigEnv(t)
	if cfg := LoadConfig(); cfg.MCPServerName != "new" {
		t.Fatalf("default MCP server name mode = %q, want new", cfg.MCPServerName)
	}
}

func TestMCPServerName(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
		want string
	}{
		{"default standalone", Config{}, "agentic-remotectl"},
		{"default master", Config{Mode: modeMaster}, "agentic-remotectl-master"},
		{"default readonly", Config{ReadOnly: true}, "agentic-remotectl-readonly"},
		{"compat standalone", Config{MCPServerName: "compat"}, "remote-shell"},
		{"compat readonly", Config{MCPServerName: "compat", ReadOnly: true}, "remote-shell-readonly"},
		{"custom", Config{MCPServerName: "my-gateway"}, "my-gateway"},
	} {
		if got := mcpServerName(&tc.cfg); got != tc.want {
			t.Errorf("%s: mcpServerName = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
