package main

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync/atomic"
)

// ---------------------------------------------------------------------------
// Read-only mode
//
// Read-only mode is the highest-priority switch in this server: once it is on,
// every mutating capability is off and nothing can turn it back on — not the
// tool blacklist, not a request parameter, not a newly registered tool.
//
// It is enforced in four independent layers, so a mistake in any single layer
// still cannot open a write path:
//
//	1. MCP registration — mutating tools are never registered (invisible).
//	2. REST routing     — mutating endpoints answer 403 instead of executing.
//	3. HTTP middleware  — any non-GET/HEAD request under /api/ is rejected.
//	4. Operation guards — the mutating functions themselves fail closed.
//
// Layer 4 is the backstop: it lives inside WriteFileContent, MoveFile,
// Executor.Execute, ... so a code path added later that forgets a registration
// check still refuses to touch the host.
// ---------------------------------------------------------------------------

// readOnlyTools is an ALLOWLIST: under read-only mode a tool is registered only
// if it is named here. Allowlist (not blacklist) semantics are deliberate — a
// tool added to this server in the future stays denied until someone audits it
// and adds it here, so forgetting to update this file fails closed.
//
// Every entry below was checked to perform no host mutation: file tools open
// their targets O_RDONLY and are sandboxed by FSRoots; the introspection tools
// parse /proc (Linux) or spawn fixed-argv probes with no caller-supplied
// arguments (`ps`, `lsof`, `uname -sr`, `<known-binary> --version` on darwin).
var readOnlyTools = map[string]bool{
	// File reads
	"remote_read_file":       true,
	"remote_list_dir":        true,
	"remote_stat":            true,
	"remote_search_content":  true,
	"remote_find_files":      true,
	"remote_tail_log":        true,
	"remote_download_base64": true,
	// Host introspection
	"remote_list_processes": true,
	"remote_check_port":     true,
	"remote_get_env_info":   true,
	"remote_status":         true,
}

// mutatingRESTPaths lists the ServeMux patterns for REST endpoints that can
// change host state. They are still routed under read-only mode (so clients get
// a clear 403 rather than a confusing 404) but never reach the executor.
// Trailing-slash entries are Go ServeMux subtree patterns and cover every path
// below them.
var mutatingRESTPaths = []string{
	"/api/execute",
	"/api/execute/stream",
	"/api/sessions",
	"/api/sessions/",   // subtree: {id}, {id}/execute
	"/api/executions/", // subtree: {id} cancel
}

// readOnlyEnabled is the process-wide latch consulted by the layer-4 operation
// guards. It is an atomic (not a Config field) for two reasons: the guards live
// in leaf functions that have no *Config in scope, and a one-way latch cannot be
// flipped back by later configuration handling.
var readOnlyEnabled atomic.Bool

// ErrReadOnly is returned by every mutating operation while read-only mode is on.
var ErrReadOnly = errors.New("server is in read-only mode: mutating operations are disabled")

// enableReadOnly latches read-only mode on for the lifetime of the process.
// There is no disable counterpart by design.
func enableReadOnly() {
	readOnlyEnabled.Store(true)
}

// isReadOnly reports whether read-only mode is latched on.
func isReadOnly() bool {
	return readOnlyEnabled.Load()
}

// guardReadOnly is the layer-4 backstop. Mutating operations call it first and
// return its error unchanged, so a forgotten registration check downgrades to a
// refused call instead of a write.
func guardReadOnly(op string) error {
	if isReadOnly() {
		return fmt.Errorf("%w (rejected: %s)", ErrReadOnly, op)
	}
	return nil
}

// readOnlyDisabledTools returns the tools denied under read-only mode: every
// tool this server knows about that is not in readOnlyTools. Sorted so startup
// logs and remote_get_env_info output are stable.
func readOnlyDisabledTools() []string {
	var out []string
	for _, name := range allToolNames {
		if !readOnlyTools[name] {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// allToolNames is the full tool inventory of this server. Keep it in sync when
// adding a tool: a name missing here is simply not force-disabled by read-only
// mode, but its registration is still gated by cfg.toolEnabled, which consults
// readOnlyTools — so the fail-closed property holds either way.
var allToolNames = []string{
	"remote_execute",
	"remote_session_execute",
	"remote_cancel",
	"remote_status",
	"remote_session_create",
	"remote_session_list",
	"remote_session_close",
	"remote_list_processes",
	"remote_check_port",
	"remote_get_env_info",
	"remote_read_file",
	"remote_write_file",
	"remote_edit_file",
	"remote_list_dir",
	"remote_stat",
	"remote_upload_base64",
	"remote_download_base64",
	"remote_search_content",
	"remote_find_files",
	"remote_tail_log",
	"remote_move_file",
	"remote_copy_file",
	"remote_delete_file",
	"remote_make_dir",
}

// parseReadOnly interprets the SHELL_API_READONLY value. Accepted truthy forms:
// 1/true/yes/on/enabled (case-insensitive). Anything else is off.
func parseReadOnly(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on", "enabled":
		return true
	default:
		return false
	}
}

// readOnlyFromEnv reads the switch from the environment.
func readOnlyFromEnv() bool {
	return parseReadOnly(os.Getenv("SHELL_API_READONLY"))
}

// sortedReadOnlyToolNames lists the allowed tools, sorted, for startup logging.
func sortedReadOnlyToolNames() []string {
	out := make([]string, 0, len(readOnlyTools))
	for name := range readOnlyTools {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// readOnlyRESTDenied is the layer-2 handler substituted for every mutating REST
// endpoint under read-only mode.
func readOnlyRESTDenied(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Read-Only", "true")
	writeError(w, http.StatusForbidden, "read_only_mode",
		"Server is in read-only mode: "+r.URL.Path+" is disabled")
}
