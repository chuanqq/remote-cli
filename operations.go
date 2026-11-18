package main

import (
	"context"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// ---------------------------------------------------------------------------
// Operation registry (2.0)
//
// The register* functions still own tool definitions and handlers, but they
// write into a toolSink instead of directly into an MCP server:
//
//   - mcpServerSink → standalone/master public MCP surface
//   - OperationSet  → reusable set for the slaver control adapter and tests
//
// Handlers are unchanged. mcp-go's CallToolRequest helpers read Params only,
// so a non-MCP caller can synthesize a request safely (see Call).
// ---------------------------------------------------------------------------

// toolSink receives registered tools.
type toolSink interface {
	AddTool(tool mcp.Tool, handler server.ToolHandlerFunc)
}

// mcpServerSink adapts *server.MCPServer to toolSink.
type mcpServerSink struct{ srv *server.MCPServer }

func (s mcpServerSink) AddTool(tool mcp.Tool, handler server.ToolHandlerFunc) {
	s.srv.AddTool(tool, handler)
}

// Operation is one registered tool plus the policy metadata the control plane
// needs.
type Operation struct {
	Name    string
	Tool    mcp.Tool
	Handler server.ToolHandlerFunc
	// ReadOnly mirrors readonly.go: the operation cannot mutate the host.
	ReadOnly bool
	// SideEffect marks operations that must never be retried automatically
	// (D17). Read-only queries and idempotent cancel operations are false.
	SideEffect bool
}

// OperationSet is a toolSink that keeps every registration for non-MCP
// invocation.
type OperationSet struct {
	ops   map[string]Operation
	order []string
}

func newOperationSet() *OperationSet {
	return &OperationSet{ops: make(map[string]Operation)}
}

// AddTool implements toolSink. The first registration of a name wins; the
// register* functions already gate on cfg.toolEnabled, so a duplicate is an
// inventory bug rather than a routing decision.
func (s *OperationSet) AddTool(tool mcp.Tool, handler server.ToolHandlerFunc) {
	if tool.Name == "" || handler == nil {
		return
	}
	if _, exists := s.ops[tool.Name]; exists {
		return
	}
	s.ops[tool.Name] = Operation{
		Name:       tool.Name,
		Tool:       tool,
		Handler:    handler,
		ReadOnly:   readOnlyTools[tool.Name],
		SideEffect: toolHasSideEffects(tool.Name),
	}
	s.order = append(s.order, tool.Name)
}

// List returns the operations in registration order.
func (s *OperationSet) List() []Operation {
	out := make([]Operation, 0, len(s.order))
	for _, name := range s.order {
		out = append(out, s.ops[name])
	}
	return out
}

// Get returns one operation by tool name.
func (s *OperationSet) Get(name string) (Operation, bool) {
	op, ok := s.ops[name]
	return op, ok
}

// Names returns the registered tool names in registration order.
func (s *OperationSet) Names() []string {
	return append([]string(nil), s.order...)
}

// Len is the number of registered operations.
func (s *OperationSet) Len() int { return len(s.order) }

// Call invokes an operation without an MCP transport (slaver control adapter,
// unit tests). Only Params is populated: every handler reads arguments through
// CallToolRequest helpers, which depend on Params alone.
func (s *OperationSet) Call(ctx context.Context, name string, args map[string]any) (*mcp.CallToolResult, error) {
	op, ok := s.ops[name]
	if !ok {
		return nil, &OperationError{Code: "capability_not_supported", Message: "operation not registered: " + name}
	}
	if args == nil {
		args = map[string]any{}
	}
	req := mcp.CallToolRequest{Params: mcp.CallToolParams{Name: name, Arguments: args}}
	return op.Handler(ctx, req)
}

// toolHasSideEffects implements the D17 retry policy: read-only tools are
// retry-safe, remote_cancel is an idempotent control operation, everything
// else may mutate host state and must not be replayed automatically.
func toolHasSideEffects(name string) bool {
	if readOnlyTools[name] {
		return false
	}
	return !idempotentControlTools[name]
}

var idempotentControlTools = map[string]bool{
	"remote_cancel": true,
}

// String is useful in tests and logs.
func (o Operation) String() string {
	return fmt.Sprintf("%s(readonly=%t side_effect=%t)", o.Name, o.ReadOnly, o.SideEffect)
}
