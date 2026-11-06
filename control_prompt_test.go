package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestControlMCPPrompt(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("需要 bash")
	}
	script, err := os.ReadFile("control.sh")
	if err != nil {
		t.Fatal(err)
	}
	tokenHash := generateTokenHash("hash-mode-client-token")
	for _, tc := range []struct {
		name       string
		config     map[string]string
		env        []string
		args       []string
		wantName   string
		wantURL    string
		wantBearer string
		wantError  bool
	}{
		{
			name:     "plaintext and JSON escaping",
			config:   map[string]string{"SHELL_API_TOKEN": `token-"quotes"-\backslash-$(literal)`},
			args:     []string{"https://mcp.example.com/mcp", "example-mcp"},
			wantName: "example-mcp", wantURL: "https://mcp.example.com/mcp",
			wantBearer: `Bearer token-"quotes"-\backslash-$(literal)`,
		},
		{
			name:     "configured URL and service name",
			config:   map[string]string{"MCP_PUBLIC_URL": "https://mcp.example.com/custom/mcp", "MCP_SERVER_NAME": "configured-mcp", "SHELL_API_TOKEN": "test-token"},
			wantName: "configured-mcp", wantURL: "https://mcp.example.com/custom/mcp", wantBearer: "Bearer test-token",
		},
		{
			name:     "arguments override configuration",
			config:   map[string]string{"MCP_PUBLIC_URL": "https://unused.example.com/mcp", "MCP_SERVER_NAME": "configured-mcp"},
			args:     []string{"http://127.0.0.1:8080/mcp", "argument-mcp"},
			wantName: "argument-mcp", wantURL: "http://127.0.0.1:8080/mcp", wantBearer: "Bearer <CLIENT_TOKEN>",
		},
		{
			name:     "hash requires client token",
			config:   map[string]string{"SHELL_API_TOKEN_HASH": tokenHash},
			args:     []string{"https://mcp.example.com/mcp"},
			wantName: "remote-shell", wantURL: "https://mcp.example.com/mcp", wantBearer: "Bearer <CLIENT_TOKEN>",
		},
		{
			name:   "hash with CHECK_TOKEN",
			config: map[string]string{"SHELL_API_TOKEN_HASH": tokenHash},
			env:    []string{"CHECK_TOKEN=hash-mode-client-token"}, args: []string{"https://mcp.example.com/mcp"},
			wantName: "remote-shell", wantURL: "https://mcp.example.com/mcp", wantBearer: "Bearer hash-mode-client-token",
		},
		{
			name:   "CHECK_TOKEN has priority",
			config: map[string]string{"SHELL_API_TOKEN": "configured-token"},
			env:    []string{"CHECK_TOKEN=client-token"}, args: []string{"https://mcp.example.com/mcp"},
			wantName: "remote-shell", wantURL: "https://mcp.example.com/mcp", wantBearer: "Bearer client-token",
		},
		{name: "missing URL", wantError: true},
		{name: "unsupported scheme", args: []string{"ssh://mcp.example.com/mcp"}, wantError: true},
		{name: "missing host", args: []string{"https:///mcp"}, wantError: true},
		{name: "query without host", args: []string{"https://?query=1"}, wantError: true},
		{name: "URL control characters", args: []string{"https://mcp.example.com/mcp\x01"}, wantError: true},
		{name: "URL has whitespace", args: []string{"https://mcp.example.com/m cp"}, wantError: true},
		{name: "URL has fragment", args: []string{"https://mcp.example.com/mcp#fragment"}, wantError: true},
		{name: "URL has embedded credentials", args: []string{"https://user:password@mcp.example.com/mcp"}, wantError: true},
		{name: "invalid service name", args: []string{"https://mcp.example.com/mcp", "bad name"}, wantError: true},
		{name: "extra argument", args: []string{"https://mcp.example.com/mcp", "example", "extra"}, wantError: true},
		{name: "control characters in token", config: map[string]string{"SHELL_API_TOKEN": "invalid\ntoken"}, args: []string{"https://mcp.example.com/mcp"}, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "control.sh")
			if err := os.WriteFile(path, script, 0o600); err != nil {
				t.Fatal(err)
			}
			var conf strings.Builder
			for key, value := range tc.config {
				conf.WriteString(key + "='" + strings.ReplaceAll(value, "'", "'\\''") + "'\n")
			}
			if err := os.WriteFile(filepath.Join(dir, "env.conf"), []byte(conf.String()), 0o600); err != nil {
				t.Fatal(err)
			}
			args := append([]string{path, "mcp-prompt"}, tc.args...)
			cmd := exec.Command(bash, args...)
			for _, variable := range os.Environ() {
				key := strings.SplitN(variable, "=", 2)[0]
				if strings.HasPrefix(key, "SHELL_API_") || key == "CHECK_TOKEN" || key == "MCP_PUBLIC_URL" || key == "MCP_SERVER_NAME" || key == "BASH_ENV" {
					continue
				}
				cmd.Env = append(cmd.Env, variable)
			}
			cmd.Env = append(cmd.Env, tc.env...)
			output, err := cmd.CombinedOutput()
			if tc.wantError {
				if err == nil {
					t.Fatalf("invalid input accepted: %s", output)
				}
				return
			}
			if err != nil {
				t.Fatalf("mcp-prompt: %v: %s", err, output)
			}
			text := string(output)
			if strings.Contains(text, tokenHash) {
				t.Fatal("prompt leaked the server-side token hash")
			}
			blocks := strings.SplitN(text, "```json\n", 2)
			if len(blocks) != 2 {
				t.Fatalf("missing JSON configuration: %s", output)
			}
			body := strings.SplitN(blocks[1], "\n```", 2)[0]
			var config struct {
				Servers map[string]struct {
					Type    string            `json:"type"`
					URL     string            `json:"url"`
					Headers map[string]string `json:"headers"`
				} `json:"mcpServers"`
			}
			if err := json.Unmarshal([]byte(body), &config); err != nil {
				t.Fatalf("invalid generated JSON: %v: %s", err, body)
			}
			entry, ok := config.Servers[tc.wantName]
			if len(config.Servers) != 1 || !ok || entry.Type != "http" || entry.URL != tc.wantURL || entry.Headers["Authorization"] != tc.wantBearer {
				t.Fatalf("unexpected generated MCP config: %s", body)
			}
			if strings.Contains(tc.wantBearer, "<CLIENT_TOKEN>") && !strings.Contains(text, "交互输入") {
				t.Fatal("missing instruction to obtain the plaintext client token")
			}
			for _, generated := range []string{"remote-agent-proxy.pid", "remote-agent-proxy.log", "remote-agent-proxy"} {
				if _, err := os.Stat(filepath.Join(dir, generated)); !os.IsNotExist(err) {
					t.Errorf("mcp-prompt unexpectedly created %s", generated)
				}
			}
		})
	}
}
