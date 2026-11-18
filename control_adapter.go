package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// operationExecutor adapts the local OperationSet to control.LocalExecutor, so
// a slaver can run the exact same tool handlers as an MCP server without
// exposing an MCP endpoint.
type operationExecutor struct{ ops *OperationSet }

func (e *operationExecutor) Operations() []string { return e.ops.Names() }
func (e *operationExecutor) ReadOnly() bool       { return isReadOnly() }

func (e *operationExecutor) Invoke(ctx context.Context, tool string, argsJSON []byte) (string, bool, bool, error) {
	var args map[string]any
	if len(argsJSON) > 0 {
		if err := json.Unmarshal(argsJSON, &args); err != nil {
			return "", false, true, fmt.Errorf("invalid arguments: %w", err)
		}
	}
	res, err := e.ops.Call(ctx, tool, args)
	if err != nil {
		return "", false, true, err
	}
	text := resultText(res)
	return text, looksLikeJSON(text), res.IsError, nil
}

// looksLikeJSON distinguishes mcp.NewToolResultJSON output from plain-text
// tool results so the master can re-wrap the content faithfully.
func looksLikeJSON(s string) bool {
	t := strings.TrimSpace(s)
	if t == "" || (t[0] != '{' && t[0] != '[') {
		return false
	}
	var probe any
	return json.Unmarshal([]byte(t), &probe) == nil
}
