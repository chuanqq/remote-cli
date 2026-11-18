package main

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

func TestMasterInstructionsMentionNodeRouting(t *testing.T) {
	master := &Config{Mode: modeMaster, MasterLocal: true, MaxTimeout: 300, MaxOutput: 1 << 20, RateLimit: 120, RateBurst: 60}
	if instr := serverInstructions(master); !strings.Contains(instr, "Multi-node") || !strings.Contains(instr, "node_id") {
		t.Fatalf("master instructions missing multi-node guidance: %s", instr)
	}
	master.MasterLocal = false
	if instr := serverInstructions(master); !strings.Contains(instr, "requires node_id") {
		t.Fatalf("MASTER_LOCAL=off instructions missing node_id requirement: %s", instr)
	}

	standalone := &Config{MaxTimeout: 300, MaxOutput: 1 << 20, RateLimit: 120, RateBurst: 60}
	if instr := serverInstructions(standalone); strings.Contains(instr, "Multi-node") {
		t.Fatalf("standalone instructions must not advertise node routing: %s", instr)
	}
}

// Node management tools take node_id as their own argument and must never be
// treated as a routing selector (regression: remote_node_status was blocked by
// the Phase 2 guard).
func TestMasterRouterExemptsNodeTools(t *testing.T) {
	cfg := &Config{MasterLocal: false}
	r := &masterRouter{cfg: cfg, logger: newLogger(io.Discard, "error")}

	called := false
	next := func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		called = true
		return mcp.NewToolResultText("ok"), nil
	}
	handler := r.middleware()(next)

	req := mcp.CallToolRequest{Params: mcp.CallToolParams{
		Name:      "remote_node_status",
		Arguments: map[string]any{"node_id": "node-1"},
	}}
	res, err := handler(context.Background(), req)
	if err != nil || !called || res.IsError {
		t.Fatalf("node tool should pass the router: called=%t res=%+v err=%v", called, res, err)
	}

	// A target tool with node_id must not execute locally; with no control
	// plane attached it fails closed.
	called = false
	req.Params.Name = "remote_execute"
	req.Params.Arguments = map[string]any{"command": "true", "node_id": "node-1"}
	res, err = handler(context.Background(), req)
	if err != nil || called || !res.IsError {
		t.Fatalf("routed call must fail closed: called=%t res=%+v err=%v", called, res, err)
	}

	// MASTER_LOCAL=off rejects local execution but still serves the
	// control-plane remote_status view.
	called = false
	req.Params.Name = "remote_execute"
	req.Params.Arguments = map[string]any{"command": "true"}
	res, err = handler(context.Background(), req)
	if err != nil || called || !res.IsError {
		t.Fatalf("master_local=off must reject local execution: called=%t res=%+v err=%v", called, res, err)
	}

	req.Params.Name = "remote_status"
	req.Params.Arguments = map[string]any{}
	res, err = handler(context.Background(), req)
	if err != nil || called || res.IsError {
		t.Fatalf("control-plane status should be served locally: called=%t res=%+v err=%v", called, res, err)
	}

	// scope=master reports the control plane even with local execution on.
	cfg.MasterLocal = true
	called = false
	req.Params.Arguments = map[string]any{"scope": "master"}
	res, err = handler(context.Background(), req)
	if err != nil || called || res.IsError {
		t.Fatalf("scope=master should be served by the control plane: called=%t res=%+v err=%v", called, res, err)
	}
}
