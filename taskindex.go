package main

import (
	"sync"
	"time"

	"github.com/chuanqq/remote-cli/internal/control"
)

// taskIndex is the master-side handle -> node mapping (design 04 §3.3). It
// stores no results: only enough information to route a follow-up call that
// omitted node_id. Expired entries disappear; the handle may still exist on
// the slaver, in which case callers must pass node_id explicitly.
type taskIndex struct {
	mu      sync.Mutex
	entries map[string]indexEntry
}

type indexEntry struct {
	nodeID    string
	nodeName  string
	tool      string
	expiresAt time.Time
}

const maxTaskIndexEntries = 4096

func newTaskIndex() *taskIndex {
	return &taskIndex{entries: make(map[string]indexEntry)}
}

func (i *taskIndex) record(handle, nodeID, nodeName, tool string, expiresAt time.Time) {
	if handle == "" {
		return
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if len(i.entries) >= maxTaskIndexEntries {
		i.pruneLocked(time.Now())
		if len(i.entries) >= maxTaskIndexEntries {
			// Still full: drop the oldest-expiring entry.
			var oldest string
			var oldestAt time.Time
			for k, v := range i.entries {
				if oldest == "" || v.expiresAt.Before(oldestAt) {
					oldest, oldestAt = k, v.expiresAt
				}
			}
			delete(i.entries, oldest)
		}
	}
	i.entries[handle] = indexEntry{nodeID: nodeID, nodeName: nodeName, tool: tool, expiresAt: expiresAt}
}

func (i *taskIndex) lookup(handle string) (indexEntry, bool) {
	if handle == "" {
		return indexEntry{}, false
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	e, ok := i.entries[handle]
	if !ok {
		return indexEntry{}, false
	}
	if time.Now().After(e.expiresAt) {
		delete(i.entries, handle)
		return indexEntry{}, false
	}
	return e, true
}

func (i *taskIndex) pruneLocked(now time.Time) {
	for k, v := range i.entries {
		if now.After(v.expiresAt) {
			delete(i.entries, k)
		}
	}
}

// handleKeys are the response fields that identify a node-scoped task.
var handleKeys = []string{"execution_id", "scan_id", "wait_id", "job_id", "session_id", "id"}

// recordResult indexes every handle-like field of a remote JSON result.
func (i *taskIndex) recordResult(tool string, rec control.NodeRecord, obj map[string]any, expiresAt time.Time) {
	for _, key := range handleKeys {
		v, ok := obj[key].(string)
		if !ok || v == "" {
			continue
		}
		i.record(v, rec.NodeID, rec.NodeName, tool, expiresAt)
	}
}
