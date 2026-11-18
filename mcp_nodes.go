package main

import (
	"context"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/chuanqq/remote-cli/internal/control"
)

// registerNodeTools adds the master-only node management surface. Read tools
// are always registered when a control plane exists; write tools require
// REMOTECTL_ENABLE_NODE_ADMIN_TOOLS=true (D10/D29). Standalone passes a nil
// registry, so none of these tools exist there.
func registerNodeTools(s toolSink, rt *localRuntime) {
	if rt.nodes == nil {
		return
	}
	reg := rt.nodes
	cfg := rt.cfg

	if cfg.toolEnabled("remote_node_list") {
		s.AddTool(mcp.NewTool("remote_node_list",
			readOnlyHints(),
			mcp.WithDescription("List slaver nodes known to this master: node_id, node_name, status, version, labels and read-only flag. Use node_id (or the unique node_name alias) as the node_id argument of target tools."),
			mcp.WithString("status", mcp.Description("Filter by status: ready, stale, offline, pending, revoked, unavailable.")),
			mcp.WithString("label", mcp.Description("Exact label filter in key=value form, e.g. env=test.")),
			mcp.WithBoolean("include_capabilities", mcp.Description("Include each node's capability snapshot (operations, limits, FS roots). Default false.")),
		), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			status := req.GetString("status", "")
			label := req.GetString("label", "")
			nodes := reg.List(status, label)
			if !req.GetBool("include_capabilities", false) {
				for i := range nodes {
					nodes[i].Capabilities = control.Capabilities{}
				}
			}
			noteAudit(ctx, func(n *auditNote) { n.Command = "node_list" })
			return jsonResult(map[string]any{"nodes": nodes, "count": len(nodes)}), nil
		})
	}

	if cfg.toolEnabled("remote_node_status") {
		s.AddTool(mcp.NewTool("remote_node_status",
			readOnlyHints(),
			mcp.WithDescription("Return one node's full record: status, epoch, lease, capabilities, load, labels and last heartbeat."),
			mcp.WithString("node_id", mcp.Required(), mcp.Description("Node UUID or its unique node_name alias.")),
		), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			ref := req.GetString("node_id", "")
			if ref == "" {
				return mcp.NewToolResultError("node_id is required"), nil
			}
			rec, err := reg.Resolve(ref)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			noteAudit(ctx, func(n *auditNote) { n.Command = "node_status " + ref })
			return jsonResult(rec), nil
		})
	}

	if !cfg.EnableNodeAdminTools {
		return
	}

	if cfg.toolEnabled("remote_node_approve") {
		s.AddTool(mcp.NewTool("remote_node_approve",
			mcp.WithDescription("Approve a pending node (manual approval mode). The slaver becomes routable immediately without reconnecting."),
			mcp.WithString("node_id", mcp.Required(), mcp.Description("Node UUID or unique node_name alias.")),
			mcp.WithString("note", mcp.Description("Optional approval note kept in the audit log.")),
		), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			rec, err := resolveForAdmin(reg, req.GetString("node_id", ""))
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			if rt.nodeAdmin == nil {
				return mcp.NewToolResultError("node administration is unavailable"), nil
			}
			updated, err := rt.nodeAdmin.ApproveNode(rec.NodeID)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			noteAudit(ctx, func(n *auditNote) { n.Command = "node_approve " + rec.NodeID })
			return jsonResult(updated), nil
		})
	}

	if cfg.toolEnabled("remote_node_revoke") {
		s.AddTool(mcp.NewTool("remote_node_revoke",
			mcp.WithDescription("Revoke a node: it stops being routable, its current control connection is closed immediately, and reconnects are rejected until an operator clears the record."),
			mcp.WithString("node_id", mcp.Required(), mcp.Description("Node UUID or unique node_name alias.")),
			mcp.WithString("reason", mcp.Description("Optional revocation reason kept in the audit log.")),
		), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			rec, err := resolveForAdmin(reg, req.GetString("node_id", ""))
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			if rt.nodeAdmin == nil {
				return mcp.NewToolResultError("node administration is unavailable"), nil
			}
			updated, err := rt.nodeAdmin.RevokeNode(rec.NodeID)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			noteAudit(ctx, func(n *auditNote) { n.Command = "node_revoke " + rec.NodeID })
			return jsonResult(updated), nil
		})
	}

	if cfg.toolEnabled("remote_node_forget") {
		s.AddTool(mcp.NewTool("remote_node_forget",
			mcp.WithDescription("Delete a revoked or offline node record. Requires confirm=true. A connected node must be revoked first."),
			mcp.WithString("node_id", mcp.Required(), mcp.Description("Node UUID or unique node_name alias.")),
			mcp.WithBoolean("confirm", mcp.Required(), mcp.Description("Must be true; guards against accidental deletion.")),
		), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			if !req.GetBool("confirm", false) {
				return mcp.NewToolResultError("confirm=true is required"), nil
			}
			rec, err := resolveForAdmin(reg, req.GetString("node_id", ""))
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			if rt.nodeAdmin == nil {
				return mcp.NewToolResultError("node administration is unavailable"), nil
			}
			if err := rt.nodeAdmin.ForgetNode(rec.NodeID); err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			noteAudit(ctx, func(n *auditNote) { n.Command = "node_forget " + rec.NodeID })
			return jsonResult(map[string]any{"node_id": rec.NodeID, "forgotten": true}), nil
		})
	}
}

// nodeAdmin is implemented by control.Master; keeping it as an interface lets
// standalone tests build node tools without a control server.
type nodeAdmin interface {
	ApproveNode(nodeID string) (control.NodeRecord, error)
	RevokeNode(nodeID string) (control.NodeRecord, error)
	ForgetNode(nodeID string) error
}

func resolveForAdmin(reg *control.NodeRegistry, ref string) (control.NodeRecord, error) {
	if ref == "" {
		return control.NodeRecord{}, &OperationError{Code: "invalid_argument", Message: "node_id is required"}
	}
	return reg.Resolve(ref)
}
