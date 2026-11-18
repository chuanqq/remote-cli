package main

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/chuanqq/remote-cli/internal/control"
)

// A control plane adds exactly the node tools: read tools always, admin tools
// only behind REMOTECTL_ENABLE_NODE_ADMIN_TOOLS.
func TestNodeToolsFollowControlPlane(t *testing.T) {
	reg, err := control.NewNodeRegistry(control.RegistryOptions{
		Approval: control.ApprovalAuto,
		Logger:   newLogger(io.Discard, "error"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Register(control.RegisterInfo{
		NodeID: "node-1", NodeName: "one", InstanceID: "i1", Hostname: "one", Platform: "test/test",
	}, "127.0.0.1:1"); err != nil {
		t.Fatal(err)
	}

	rt := testLocalRuntime(t)
	rt.nodes = reg
	ops := buildOperations(rt)
	for _, name := range []string{"remote_node_list", "remote_node_status"} {
		if _, ok := ops.Get(name); !ok {
			t.Errorf("node read tool %s missing with a registry", name)
		}
	}
	for _, name := range []string{"remote_node_approve", "remote_node_revoke", "remote_node_forget"} {
		if _, ok := ops.Get(name); ok {
			t.Errorf("admin tool %s registered without REMOTECTL_ENABLE_NODE_ADMIN_TOOLS", name)
		}
	}

	res, err := ops.Call(context.Background(), "remote_node_list", nil)
	if err != nil {
		t.Fatalf("remote_node_list failed: %v", err)
	}
	if res.IsError || !strings.Contains(resultText(res), "node-1") {
		t.Fatalf("remote_node_list output = %s", resultText(res))
	}

	// Admin tools appear when explicitly enabled.
	rt.cfg.EnableNodeAdminTools = true
	ops = buildOperations(rt)
	for _, name := range []string{"remote_node_approve", "remote_node_revoke", "remote_node_forget"} {
		if _, ok := ops.Get(name); !ok {
			t.Errorf("admin tool %s missing after enabling", name)
		}
	}
}

func testLocalRuntime(t *testing.T) *localRuntime {
	t.Helper()
	cfg := &Config{
		Port:          "0",
		Listen:        "127.0.0.1:0",
		MaxTimeout:    30,
		MaxOutput:     4096,
		RateLimit:     120,
		RateBurst:     60,
		DefaultShell:  "bash",
		JobDir:        t.TempDir(),
		FSRoots:       []string{t.TempDir()},
		DisabledTools: map[string]bool{},
	}
	return newLocalRuntime(
		NewExecutor(cfg),
		NewSessionManager(),
		testJobManager(t),
		NewWaitManager(),
		NewAuditLogger(newLogger(io.Discard, "info")),
		cfg,
	)
}

func TestOperationSetMatchesInventory(t *testing.T) {
	rt := testLocalRuntime(t)
	ops := buildOperations(rt)
	expected := withoutNodeTools(allToolNames)

	registered := make(map[string]bool, ops.Len())
	for _, op := range ops.List() {
		if registered[op.Name] {
			t.Errorf("duplicate operation registration: %s", op.Name)
		}
		registered[op.Name] = true
	}

	for _, name := range expected {
		if !registered[name] {
			t.Errorf("allToolNames lists %s but buildOperations did not register it", name)
		}
	}
	for name := range registered {
		if !slices.Contains(expected, name) {
			t.Errorf("operation %s is registered but not expected without a control plane", name)
		}
	}

	// Policy metadata drives remote retry decisions (D17) and the read-only
	// intersection; verify the representative cases.
	for _, tc := range []struct {
		name       string
		readOnly   bool
		sideEffect bool
	}{
		{"remote_status", true, false},
		{"remote_read_file", true, false},
		{"remote_cancel", false, false},
		{"remote_execute", false, true},
		{"remote_spawn", false, true},
	} {
		op, ok := ops.Get(tc.name)
		if !ok {
			t.Fatalf("operation %s missing", tc.name)
		}
		if op.ReadOnly != tc.readOnly || op.SideEffect != tc.sideEffect {
			t.Errorf("%s: readonly=%t side_effect=%t, want readonly=%t side_effect=%t",
				tc.name, op.ReadOnly, op.SideEffect, tc.readOnly, tc.sideEffect)
		}
	}
}

func TestOperationSetCall(t *testing.T) {
	ops := buildOperations(testLocalRuntime(t))

	res, err := ops.Call(context.Background(), "remote_status", nil)
	if err != nil {
		t.Fatalf("remote_status call failed: %v", err)
	}
	if res == nil || res.IsError || len(res.Content) == 0 {
		t.Fatalf("remote_status returned an empty/error result: %+v", res)
	}

	_, err = ops.Call(context.Background(), "remote_not_a_tool", nil)
	var opErr *OperationError
	if !errors.As(err, &opErr) || opErr.Code != "capability_not_supported" {
		t.Fatalf("unknown operation error = %v, want capability_not_supported", err)
	}
}

func TestDispatchers(t *testing.T) {
	ops := buildOperations(testLocalRuntime(t))
	local := newLocalDispatcher(ops)

	res, err := local.Dispatch(context.Background(), Call{Tool: "remote_status", OperationID: "op-1"})
	if err != nil {
		t.Fatalf("local dispatch failed: %v", err)
	}
	if res.Route != "local" || res.OperationID != "op-1" || res.Content == nil {
		t.Fatalf("unexpected local dispatch result: %+v", res)
	}

	remote := &remoteDispatcher{}
	_, err = remote.Dispatch(context.Background(), Call{Tool: "remote_status", NodeID: "node-1"})
	var opErr *OperationError
	if !errors.As(err, &opErr) || opErr.Code != "capability_not_supported" {
		t.Fatalf("remote stub error = %v, want capability_not_supported", err)
	}
}

// A tool blacklist must shrink the operation set exactly like it shrinks the
// MCP surface.
func TestOperationSetHonorsDisabledTools(t *testing.T) {
	rt := testLocalRuntime(t)
	rt.cfg.DisabledTools["remote_execute"] = true
	ops := buildOperations(rt)
	if _, ok := ops.Get("remote_execute"); ok {
		t.Fatal("disabled tool remote_execute must not be registered")
	}
	if _, ok := ops.Get("remote_status"); !ok {
		t.Fatal("remote_status should stay registered")
	}
}

// Read-only mode is allowlist-based: only readOnlyTools survive.
func TestOperationSetHonorsReadOnly(t *testing.T) {
	rt := testLocalRuntime(t)
	rt.cfg.applyReadOnly(true)
	t.Cleanup(func() { readOnlyEnabled.Store(false) })

	ops := buildOperations(rt)
	if _, ok := ops.Get("remote_execute"); ok {
		t.Fatal("read-only mode must not register remote_execute")
	}
	if _, ok := ops.Get("remote_write_file"); ok {
		t.Fatal("read-only mode must not register remote_write_file")
	}
	for _, name := range ops.Names() {
		if !readOnlyTools[name] {
			t.Errorf("read-only mode registered non-read-only tool %s", name)
		}
	}
	want := 0
	for name := range readOnlyTools {
		if !slices.Contains(nodeToolNames, name) {
			want++
		}
	}
	if ops.Len() != want {
		t.Fatalf("read-only operation count = %d, want %d", ops.Len(), want)
	}
}

// withoutNodeTools is the standalone/slaver inventory: node management tools
// only exist when a control plane is attached.
func withoutNodeTools(names []string) []string {
	out := make([]string, 0, len(names))
	for _, name := range names {
		if !slices.Contains(nodeToolNames, name) {
			out = append(out, name)
		}
	}
	return out
}
