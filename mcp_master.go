package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/chuanqq/remote-cli/internal/control"
)

// masterRouter implements node_id routing (design 04 §3):
//
//   - no node_id: run locally when MASTER_LOCAL=on; otherwise serve the
//     control-plane views and reject target calls;
//   - node_id: resolve the node (UUID or unique node_name), check status and
//     capabilities, then invoke it over the control channel. It never falls
//     back to the master host.
//
// For handle-taking tools without node_id, a local "not found" is followed by
// the master task index lookup, which routes to the node that produced the
// handle.
type masterRouter struct {
	cfg      *Config
	registry *control.NodeRegistry
	master   *control.Master
	index    *taskIndex
	logger   *slog.Logger
}

func newMasterRouter(cfg *Config, master *control.Master, logger *slog.Logger) *masterRouter {
	return &masterRouter{
		cfg:      cfg,
		registry: master.Registry(),
		master:   master,
		index:    newTaskIndex(),
		logger:   logger,
	}
}

func (r *masterRouter) middleware() server.ToolHandlerMiddleware {
	return func(next server.ToolHandlerFunc) server.ToolHandlerFunc {
		return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			// Node management tools take node_id as their own argument.
			if strings.HasPrefix(req.Params.Name, "remote_node_") {
				return next(ctx, req)
			}
			if ref := req.GetString("node_id", ""); ref != "" {
				return r.remote(ctx, ref, req)
			}
			return r.local(ctx, next, req)
		}
	}
}

func (r *masterRouter) local(ctx context.Context, next server.ToolHandlerFunc, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	// D26: {"scope":"master"} always reports the control plane, even when the
	// master also executes locally.
	if req.Params.Name == "remote_status" && req.GetString("scope", "") == "master" {
		return r.controlPlaneStatus(), nil
	}
	if !r.cfg.MasterLocal {
		// Before rejecting, honor a handle that the master already routed.
		if node, ok := r.lookupHandleNode(req); ok {
			return r.remote(ctx, node, req)
		}
		if req.Params.Name == "remote_status" {
			return r.controlPlaneStatus(), nil
		}
		return toolError("master_local_disabled",
			"this master does not execute locally; pass node_id (see remote_node_list) to target a slaver"), nil
	}

	noteAudit(ctx, func(n *auditNote) { n.Route = "local" })
	res, err := next(ctx, req)
	if err == nil && res != nil && res.IsError {
		// The local runtime did not find the handle; try the task index.
		if node, ok := r.lookupHandleNode(req); ok {
			return r.remote(ctx, node, req)
		}
	}
	return res, err
}

func (r *masterRouter) controlPlaneStatus() *mcp.CallToolResult {
	nodesTotal, nodesReady := 0, 0
	if r.registry != nil {
		nodes := r.registry.List("", "")
		nodesTotal = len(nodes)
		for _, n := range nodes {
			if n.Status == control.NodeReady || n.Status == control.NodeStale {
				nodesReady++
			}
		}
	}
	out := map[string]any{
		"status":       "healthy",
		"mode":         "master",
		"master_local": r.cfg.MasterLocal,
		"nodes_total":  nodesTotal,
		"nodes_ready":  nodesReady,
		"hint":         "Use remote_node_list for per-node details; pass node_id to target a slaver.",
	}
	if !r.cfg.MasterLocal {
		out["hint"] = "This master does not execute locally. Pass node_id (see remote_node_list) to target a slaver."
	}
	return jsonResult(out)
}

func (r *masterRouter) remote(ctx context.Context, ref string, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if r.master == nil || r.registry == nil {
		return toolError("capability_not_supported", "remote node routing is unavailable"), nil
	}
	rec, err := r.registry.Resolve(ref)
	if err != nil {
		return toolError("node_not_found", err.Error()), nil
	}
	switch rec.Status {
	case control.NodeReady:
	case control.NodeStale:
		r.logger.Warn("routing to stale node", "node_id", rec.NodeID, "node_name", rec.NodeName)
	case control.NodePending:
		return toolError("node_unavailable", fmt.Sprintf("node %s is pending approval", nodeLabel(rec))), nil
	case control.NodeRevoked:
		return toolError("node_revoked", fmt.Sprintf("node %s is revoked", nodeLabel(rec))), nil
	default:
		msg := fmt.Sprintf("node %s is %s", nodeLabel(rec), rec.Status)
		if !rec.LastSeenAt.IsZero() {
			msg += " (last seen " + rec.LastSeenAt.Format(time.RFC3339) + ")"
		}
		return toolError("node_unavailable", msg), nil
	}
	if len(rec.Capabilities.Operations) > 0 && !slices.Contains(rec.Capabilities.Operations, req.Params.Name) {
		return toolError("capability_not_supported",
			fmt.Sprintf("node %s does not expose %s", nodeLabel(rec), req.Params.Name)), nil
	}

	args := cloneArgs(req.GetArguments())
	delete(args, "node_id")
	operationID := "op-" + uuid.NewString()
	traceID := reqIDFrom(ctx)
	idempotencyKey := ""
	if toolHasSideEffects(req.Params.Name) {
		idempotencyKey = "idem-" + uuid.NewString()
	}
	noteAudit(ctx, func(n *auditNote) {
		n.Route = "remote"
		n.TargetNodeID = rec.NodeID
		n.OperationID = operationID
		n.TraceID = traceID
	})
	budget := r.cfg.callBudget() + 10*time.Second
	start := time.Now()

	res, err := r.master.InvokeTool(ctx, control.InvokeRequest{
		NodeID:         rec.NodeID,
		OperationID:    operationID,
		TraceID:        traceID,
		IdempotencyKey: idempotencyKey,
		Tool:           req.Params.Name,
		Args:           args,
		Budget:         budget,
	})
	if err != nil {
		var ie *control.InvokeError
		if errors.As(err, &ie) {
			switch ie.Code {
			case "deadline_exceeded":
				// D16: never claim the task did not run, and never invite a
				// replay of a side-effecting call.
				return jsonResult(map[string]any{
					"state":        "running",
					"outcome":      "unknown",
					"node_id":      rec.NodeID,
					"node_name":    rec.NodeName,
					"operation_id": operationID,
					"hint":         "The result is unknown; do NOT re-run the operation. After the node recovers, query it with the same node_id and handle (the master logs the operation_id).",
				}), nil
			case "node_unavailable":
				msg := fmt.Sprintf("node %s is unavailable", nodeLabel(rec))
				if !rec.LastSeenAt.IsZero() {
					msg += " (last seen " + rec.LastSeenAt.Format(time.RFC3339) + ")"
				}
				return toolError("node_unavailable", msg), nil
			default:
				return toolError(ie.Code, ie.Message), nil
			}
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return toolError("internal", err.Error()), nil
	}

	r.logger.Info("remote invoke completed",
		"tool", req.Params.Name, "node_id", rec.NodeID, "node_name", rec.NodeName,
		"operation_id", operationID, "state", res.State, "is_error", res.IsError,
		"dur_ms", time.Since(start).Milliseconds())
	return r.renderResult(req.Params.Name, rec, operationID, res), nil
}

func (r *masterRouter) renderResult(tool string, rec control.NodeRecord, operationID string, res control.InvokeResult) *mcp.CallToolResult {
	if res.IsError {
		msg := res.Text
		if res.ErrorMessage != "" {
			msg = res.ErrorMessage
		}
		if res.ErrorCode != "" {
			msg = res.ErrorCode + ": " + msg
		}
		if msg == "" {
			msg = "remote tool failed"
		}
		return mcp.NewToolResultError(msg)
	}
	if res.IsJSON {
		var obj map[string]any
		if err := json.Unmarshal([]byte(res.Text), &obj); err == nil {
			obj["node_id"] = rec.NodeID
			if rec.NodeName != "" {
				obj["node_name"] = rec.NodeName
			}
			obj["operation_id"] = operationID
			obj["route"] = "remote"
			r.index.recordResult(tool, rec, obj, time.Now().Add(30*time.Minute))
			return jsonResult(obj)
		}
	}
	// format=text and other plain-text results keep their 1.x shape (Q4).
	return mcp.NewToolResultText(res.Text)
}

// lookupHandleNode finds the node that produced a handle named in the call
// arguments. Only node-scoped handle tools are considered.
func (r *masterRouter) lookupHandleNode(req mcp.CallToolRequest) (string, bool) {
	if r.index == nil {
		return "", false
	}
	for _, key := range []string{"execution_id", "scan_id", "wait_id", "job_id", "session_id"} {
		handle := req.GetString(key, "")
		if handle == "" {
			continue
		}
		if e, ok := r.index.lookup(handle); ok {
			return e.nodeID, true
		}
	}
	return "", false
}

// withMasterToolParams advertises master-only arguments: node_id on every
// target tool, and the control-plane scope on remote_status. Standalone and
// slaver surfaces never call this.
func withMasterToolParams(tool mcp.Tool) mcp.Tool {
	if tool.InputSchema.Properties == nil {
		tool.InputSchema.Properties = make(map[string]any)
	}
	if _, exists := tool.InputSchema.Properties["node_id"]; !exists {
		tool.InputSchema.Properties["node_id"] = map[string]any{
			"type": "string",
			"description": "Target slaver node: its UUID or unique node_name alias from remote_node_list. " +
				"Omit to operate on the master host. Unknown, offline or revoked nodes fail the call; " +
				"they never fall back to another node.",
		}
	}
	if tool.Name == "remote_status" {
		tool.InputSchema.Properties["scope"] = map[string]any{
			"type":        "string",
			"enum":        []string{"master"},
			"description": "master: control-plane summary (node counts). Omit for the master host status; pass node_id for a slaver node status.",
		}
	}
	return tool
}

func nodeLabel(rec control.NodeRecord) string {
	if rec.NodeName != "" {
		return fmt.Sprintf("%s (%s)", rec.NodeName, rec.NodeID)
	}
	return rec.NodeID
}

func toolError(code, message string) *mcp.CallToolResult {
	if code == "" {
		return mcp.NewToolResultError(message)
	}
	return mcp.NewToolResultError(code + ": " + message)
}

func cloneArgs(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
