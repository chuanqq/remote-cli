package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"reflect"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/server"
)

func TestParseTrustedProxies(t *testing.T) {
	for _, tc := range []struct {
		name, value string
		want        []string
	}{
		{"empty", "", nil},
		{"blank", " , , ", nil},
		{"addresses", " 127.0.0.1, ::1 ", []string{"127.0.0.1/32", "::1/128"}},
		{"networks", "192.0.2.9/24,2001:db8:1::9/48", []string{"192.0.2.0/24", "2001:db8:1::/48"}},
		{"mapped", "::ffff:192.0.2.9,::ffff:192.0.2.9/120", []string{"192.0.2.9/32", "192.0.2.0/24"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prefixes, err := parseTrustedProxies(tc.value)
			if err != nil {
				t.Fatal(err)
			}
			if got := trustedProxyStrings(prefixes); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
	for _, invalid := range []string{
		"localhost", "*", "127.0.0.1:8080", "[::1]", "127.0.0.1/33", "::1/129",
		"192.0.2.1,not-an-ip", "fe80::1%eth0", "fe80::1%eth0/64", "::ffff:192.0.2.1/80",
	} {
		if _, err := parseTrustedProxies(invalid); err == nil {
			t.Errorf("invalid trusted proxies accepted: %q", invalid)
		}
	}
}

func testTrustedProxies(t *testing.T, value string) []netip.Prefix {
	t.Helper()
	prefixes, err := parseTrustedProxies(value)
	if err != nil {
		t.Fatal(err)
	}
	return prefixes
}

func TestLoadConfigTrustedProxies(t *testing.T) {
	t.Setenv("SHELL_API_READONLY", "false")
	t.Setenv("SHELL_API_DENY_COMMANDS", "")
	t.Setenv("SHELL_API_TRUSTED_PROXIES", "127.0.0.1,::1")
	if got := trustedProxyStrings(LoadConfig().TrustedProxies); !reflect.DeepEqual(got, []string{"127.0.0.1/32", "::1/128"}) {
		t.Fatalf("config trusted proxies = %v", got)
	}
	t.Setenv("SHELL_API_TRUSTED_PROXIES", "")
	if got := LoadConfig().TrustedProxies; len(got) != 0 {
		t.Fatalf("trusted proxies should be disabled by default: %v", got)
	}
}

func TestResolveClientIP(t *testing.T) {
	trusted := testTrustedProxies(t, "127.0.0.1,::1,192.0.2.0/24,2001:db8:1::/48")
	for _, tc := range []struct {
		name, peer string
		xff, real  []string
		want       string
	}{
		{"direct forged headers", "198.51.100.9:1234", []string{"203.0.113.8"}, []string{"203.0.113.9"}, "198.51.100.9"},
		{"trusted no headers", "127.0.0.1:1234", nil, nil, "127.0.0.1"},
		{"single proxy", "127.0.0.1:1234", []string{"198.51.100.9"}, nil, "198.51.100.9"},
		{"xff has priority", "127.0.0.1:1234", []string{"198.51.100.9"}, []string{"203.0.113.9"}, "198.51.100.9"},
		{"spoofed leftmost", "127.0.0.1:1234", []string{"203.0.113.8,198.51.100.9"}, nil, "198.51.100.9"},
		{"multiple proxies", "192.0.2.10:1234", []string{"198.51.100.9,192.0.2.20,192.0.2.30"}, nil, "198.51.100.9"},
		{"untrusted middle boundary", "127.0.0.1:1234", []string{"203.0.113.8,198.51.100.9,192.0.2.20"}, nil, "198.51.100.9"},
		{"all hops trusted", "127.0.0.1:1234", []string{"192.0.2.10,192.0.2.20"}, nil, "192.0.2.10"},
		{"repeated xff", "127.0.0.1:1234", []string{"203.0.113.8", "198.51.100.9,192.0.2.20"}, nil, "198.51.100.9"},
		{"spaces", "127.0.0.1:1234", []string{" 198.51.100.9 , 192.0.2.20 "}, nil, "198.51.100.9"},
		{"real ip fallback", "127.0.0.1:1234", nil, []string{"198.51.100.9"}, "198.51.100.9"},
		{"invalid xff blocks real ip", "127.0.0.1:1234", []string{"unknown"}, []string{"198.51.100.9"}, "127.0.0.1"},
		{"empty xff blocks real ip", "127.0.0.1:1234", []string{""}, []string{"198.51.100.9"}, "127.0.0.1"},
		{"invalid leftmost", "127.0.0.1:1234", []string{"unknown,198.51.100.9"}, nil, "127.0.0.1"},
		{"empty hop", "127.0.0.1:1234", []string{"198.51.100.9,,192.0.2.20"}, nil, "127.0.0.1"},
		{"ipv4 with port", "127.0.0.1:1234", []string{"198.51.100.9:80"}, nil, "127.0.0.1"},
		{"quoted address", "127.0.0.1:1234", []string{`"198.51.100.9"`}, nil, "127.0.0.1"},
		{"repeated real ip", "127.0.0.1:1234", nil, []string{"198.51.100.9", "203.0.113.9"}, "127.0.0.1"},
		{"comma in real ip", "127.0.0.1:1234", nil, []string{"198.51.100.9,203.0.113.9"}, "127.0.0.1"},
		{"invalid real ip", "127.0.0.1:1234", nil, []string{"not-an-ip"}, "127.0.0.1"},
		{"ipv6 peer", "[::1]:1234", []string{"2001:db8:2::9"}, nil, "2001:db8:2::9"},
		{"ipv6 proxy chain", "[2001:db8:1::10]:1234", []string{"2001:db8:2::9,2001:db8:1::20"}, nil, "2001:db8:2::9"},
		{"normalize ipv6", "[::1]:1234", nil, []string{"2001:0db8:0002::0009"}, "2001:db8:2::9"},
		{"mapped peer", "[::ffff:127.0.0.1]:1234", []string{"198.51.100.9"}, nil, "198.51.100.9"},
		{"mapped header", "127.0.0.1:1234", []string{"::ffff:198.51.100.9"}, nil, "198.51.100.9"},
		{"direct mapped peer", "[::ffff:198.51.100.9]:1234", nil, nil, "198.51.100.9"},
		{"zone in header", "127.0.0.1:1234", []string{"fe80::1%eth0"}, nil, "127.0.0.1"},
		{"bracketed ipv6 header", "127.0.0.1:1234", []string{"[2001:db8:2::9]"}, nil, "127.0.0.1"},
		{"invalid peer", "not-an-ip", []string{"198.51.100.9"}, nil, "not-an-ip"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
			r.RemoteAddr = tc.peer
			for _, value := range tc.xff {
				r.Header.Add("X-Forwarded-For", value)
			}
			for _, value := range tc.real {
				r.Header.Add("X-Real-IP", value)
			}
			if got := resolveClientIP(r, trusted); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
			_, peer := peerClientIP(tc.peer)
			if got := resolveClientIP(r, nil); got != peer {
				t.Fatalf("default trusted headers unexpectedly: %q, want peer %q", got, peer)
			}
		})
	}
}

func TestClientIPContextPreservesPeer(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("X-Forwarded-For", "198.51.100.9")
	if ip := clientIPFromRequest(r); ip != "127.0.0.1" {
		t.Fatalf("accessor trusted a header without middleware: %s", ip)
	}
	h := ClientIPMiddleware(testTrustedProxies(t, "127.0.0.1"), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ip := clientIPFromRequest(r); ip != "198.51.100.9" {
			t.Errorf("resolved context IP = %s", ip)
		}
		if r.RemoteAddr != "127.0.0.1:1234" {
			t.Errorf("TCP peer was overwritten: %s", r.RemoteAddr)
		}
		ctx := mcpHTTPContext(r.Context(), r)
		if ip := sourceIPFrom(ctx); ip != "198.51.100.9" {
			t.Errorf("MCP context disagrees with client IP: %s", ip)
		}
	}))
	h.ServeHTTP(httptest.NewRecorder(), r)

	// 没有实现的转发头不能意外扩大信任范围。
	r.Header.Del("X-Forwarded-For")
	r.Header.Set("Forwarded", "for=198.51.100.9")
	if got := resolveClientIP(r, testTrustedProxies(t, "127.0.0.1")); got != "127.0.0.1" {
		t.Fatalf("unexpected Forwarded support: %s", got)
	}
}

func TestTrustedProxyRateLimitAndAccessLog(t *testing.T) {
	for _, trust := range []string{"", "127.0.0.1"} {
		t.Run("trusted="+trust, func(t *testing.T) {
			var buf bytes.Buffer
			cfg := &Config{RateLimit: 1, RateBurst: 1, TrustedProxies: testTrustedProxies(t, trust)}
			h := newHTTPHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
				newPlainAuth("test-token"), cfg, newLogger(&buf, "info"), context.Background())
			for i, xff := range []string{"198.51.100.9", "203.0.113.9,198.51.100.9", "198.51.100.10"} {
				r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
				r.RemoteAddr = "127.0.0.1:1234"
				r.Header.Set("X-Forwarded-For", xff)
				r.Header.Set("Authorization", "Bearer test-token")
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, r)
				want := http.StatusTooManyRequests
				if i == 0 || (i == 2 && trust != "") {
					want = http.StatusOK
				}
				if rec.Code != want {
					t.Fatalf("request %d: status=%d, want=%d", i, rec.Code, want)
				}
			}
			lines := parseLog(t, &buf).ofType("access")
			if len(lines) != 3 {
				t.Fatalf("access lines = %d, want 3", len(lines))
			}
			for i, line := range lines {
				want := "127.0.0.1"
				if trust != "" {
					want = "198.51.100.9"
					if i == 2 {
						want = "198.51.100.10"
					}
				}
				if line["remote"] != want {
					t.Errorf("access remote = %v, want %s", line["remote"], want)
				}
			}
		})
	}
}

func TestTrustedProxyMCPAudit(t *testing.T) {
	var buf bytes.Buffer
	logger := newLogger(&buf, "info")
	cfg := &Config{MaxTimeout: 10, MaxOutput: 4096, DefaultShell: "bash", RateLimit: 120, RateBurst: 60,
		TrustedProxies: testTrustedProxies(t, "127.0.0.1"), DisabledTools: map[string]bool{}}
	mcpHandler := NewMCPHandler(NewExecutor(cfg), &SessionManager{}, testJobManager(t), NewWaitManager(), NewAuditLogger(logger), cfg)
	h := newHTTPHandler(mcpHandler, newPlainAuth("test-token"), cfg, logger, context.Background())
	post := func(sid, token, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
		r.RemoteAddr = "127.0.0.1:1234"
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("X-Forwarded-For", "203.0.113.8,198.51.100.9")
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "application/json, text/event-stream")
		if sid != "" {
			r.Header.Set(server.HeaderKeySessionID, sid)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}
	init := post("", "test-token", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"proxy-test","version":"1"}}}`)
	sid := init.Header().Get(server.HeaderKeySessionID)
	if init.Code != http.StatusOK || sid == "" {
		t.Fatalf("initialize failed: %s", init.Body.String())
	}
	post(sid, "test-token", `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	call := post(sid, "test-token", `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"remote_status","arguments":{}}}`)
	if call.Code != http.StatusOK {
		t.Fatalf("remote_status failed: %s", call.Body.String())
	}
	if denied := post(sid, "wrong-token", "{}"); denied.Code != http.StatusUnauthorized {
		t.Fatalf("invalid token status=%d", denied.Code)
	}
	lines := parseLog(t, &buf)
	for _, line := range lines {
		if line["remote"] != "198.51.100.9" {
			t.Errorf("log disagrees with client IP: %v", line)
		}
	}
	audits := lines.ofType("audit")
	if len(audits) != 1 || audits[0]["tool"] != "remote_status" || audits[0]["mcp_session"] != sid {
		t.Fatalf("MCP audit = %v", audits)
	}
	// 未启用可信代理时的 SDK 直连仍沿用 TCP 对端。
	r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	r.RemoteAddr = "198.51.100.10:1234"
	ctx := mcpHTTPContext(context.Background(), r)
	if got := sourceIPFrom(ctx); got != "198.51.100.10" {
		t.Errorf("direct MCP source = %s", got)
	}
}
