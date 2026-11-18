package main

import (
	"context"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
)

// OperationError is the stable internal error model (design 03 §8). The remote
// dispatcher maps it to an MCP isError result with a stable code.
type OperationError struct {
	Code      string
	Message   string
	Retryable bool
}

func (e *OperationError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// ErrorCode exposes the stable code to the control layer, which cannot import
// main (it type-asserts an interface with this method).
func (e *OperationError) ErrorCode() string {
	if e == nil {
		return ""
	}
	return e.Code
}

// Call is one tool invocation as seen by a dispatcher.
type Call struct {
	Tool        string
	Args        map[string]any
	NodeID      string
	PrincipalID string
	TraceID     string
	OperationID string
}

// DispatchResult is the dispatcher output before MCP result rendering.
type DispatchResult struct {
	Content        *mcp.CallToolResult
	NodeID         string
	NodeName       string
	OperationID    string
	Route          string // local | remote
	OutcomeUnknown bool   // D16: connection lost, task outcome unknown
	Hint           string
}

// Dispatcher selects where a call runs. Phase 1 provides the local path; the
// remote implementation arrives with the control channel (Phase 3).
type Dispatcher interface {
	Dispatch(ctx context.Context, call Call) (DispatchResult, error)
}

// localDispatcher runs the call against the local operation registry, exactly
// like 1.x handler execution.
type localDispatcher struct{ ops *OperationSet }

func newLocalDispatcher(ops *OperationSet) *localDispatcher { return &localDispatcher{ops: ops} }

func (d *localDispatcher) Dispatch(ctx context.Context, call Call) (DispatchResult, error) {
	res, err := d.ops.Call(ctx, call.Tool, call.Args)
	if err != nil {
		return DispatchResult{}, err
	}
	return DispatchResult{
		Content:     res,
		OperationID: call.OperationID,
		Route:       "local",
	}, nil
}

// remoteDispatcher is the Phase 3 placeholder. Keeping the failure explicit
// (instead of silently running locally) is the fail-closed property the
// design requires: an unreachable node must never fall back to the master.
type remoteDispatcher struct{}

func (d *remoteDispatcher) Dispatch(ctx context.Context, call Call) (DispatchResult, error) {
	return DispatchResult{}, &OperationError{
		Code:    "capability_not_supported",
		Message: "remote node routing is not implemented in this build",
	}
}
