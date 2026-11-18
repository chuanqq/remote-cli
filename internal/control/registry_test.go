package control

import (
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testRegisterInfo(nodeID, name string) RegisterInfo {
	return RegisterInfo{
		NodeID:     nodeID,
		NodeName:   name,
		InstanceID: "inst-1",
		Version:    "2.0.0",
		Hostname:   name,
		Platform:   "linux/amd64",
		Labels:     map[string]string{"env": "test"},
		Capabilities: Capabilities{
			Operations: []string{"remote_status"},
		},
	}
}

func TestRegistryAutoApprovalAndEpochs(t *testing.T) {
	reg, err := NewNodeRegistry(RegistryOptions{Approval: ApprovalAuto, Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	rec, err := reg.Register(testRegisterInfo("n1", "node-one"), "127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != NodeReady {
		t.Fatalf("auto approval status = %s, want ready", rec.Status)
	}
	if rec.ConnectionEpoch != 1 {
		t.Fatalf("first epoch = %d, want 1", rec.ConnectionEpoch)
	}

	rec2, err := reg.Register(testRegisterInfo("n1", "node-one"), "127.0.0.1:2")
	if err != nil {
		t.Fatal(err)
	}
	if rec2.ConnectionEpoch != 2 {
		t.Fatalf("reconnect epoch = %d, want 2", rec2.ConnectionEpoch)
	}
	if err := reg.ApplyHeartbeat("n1", 1, LoadInfo{}); !errors.Is(err, ErrEpochSuperseded) {
		t.Fatalf("stale epoch heartbeat error = %v, want epoch superseded", err)
	}
	if err := reg.ApplyHeartbeat("n1", 2, LoadInfo{RunningJobs: 3}); err != nil {
		t.Fatalf("current epoch heartbeat failed: %v", err)
	}
	got, err := reg.Resolve("node-one")
	if err != nil || got.Load.RunningJobs != 3 {
		t.Fatalf("heartbeat load not applied: %+v, %v", got.Load, err)
	}
}

func TestRegistryManualApproval(t *testing.T) {
	store := filepath.Join(t.TempDir(), "nodes.json")
	reg, err := NewNodeRegistry(RegistryOptions{
		Approval: ApprovalManual, StorePath: store, Logger: discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	rec, err := reg.Register(testRegisterInfo("n1", ""), "")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != NodePending {
		t.Fatalf("manual approval status = %s, want pending", rec.Status)
	}
	rec, err = reg.Approve("n1", "tester")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != NodeReady || rec.ApprovedAt == nil || rec.ApprovedBy != "tester" {
		t.Fatalf("approve result = %+v", rec)
	}

	// A restart must keep the approval and mark the node offline until it
	// reconnects.
	reloaded, err := NewNodeRegistry(RegistryOptions{
		Approval: ApprovalManual, StorePath: store, Logger: discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reloaded.Get("n1")
	if !ok || got.Status != NodeOffline || got.ApprovedAt == nil {
		t.Fatalf("reloaded record = %+v (ok=%t)", got, ok)
	}
}

func TestRegistryAllowlist(t *testing.T) {
	reg, err := NewNodeRegistry(RegistryOptions{
		Approval: ApprovalAllowlist, Allowlist: []string{"allowed"}, Logger: discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Register(testRegisterInfo("denied", ""), ""); !errors.Is(err, ErrNodeNotAllowlisted) {
		t.Fatalf("denied node error = %v, want not allowlisted", err)
	}
	rec, err := reg.Register(testRegisterInfo("allowed", ""), "")
	if err != nil || rec.Status != NodeReady {
		t.Fatalf("allowed node = %+v, %v", rec, err)
	}
}

func TestRegistryRevokeForget(t *testing.T) {
	reg, err := NewNodeRegistry(RegistryOptions{Approval: ApprovalAuto, Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Register(testRegisterInfo("n1", "one"), ""); err != nil {
		t.Fatal(err)
	}
	if err := reg.Forget("n1"); !errors.Is(err, ErrForgetNotAllowed) {
		t.Fatalf("forget ready node error = %v, want not allowed", err)
	}
	if _, err := reg.Revoke("n1"); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Register(testRegisterInfo("n1", "one"), ""); !errors.Is(err, ErrNodeRevoked) {
		t.Fatalf("revoked node re-register error = %v", err)
	}
	if err := reg.Forget("n1"); err != nil {
		t.Fatalf("forget revoked node: %v", err)
	}
	if _, ok := reg.Get("n1"); ok {
		t.Fatal("node should be forgotten")
	}
}

func TestRegistryDuplicateName(t *testing.T) {
	reg, err := NewNodeRegistry(RegistryOptions{Approval: ApprovalAuto, Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Register(testRegisterInfo("n1", "same"), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Register(testRegisterInfo("n2", "same"), ""); !errors.Is(err, ErrDuplicateNodeName) {
		t.Fatalf("duplicate node_name error = %v", err)
	}
}

func TestRegistryReap(t *testing.T) {
	now := time.Now()
	reg, err := NewNodeRegistry(RegistryOptions{
		Approval:          ApprovalAuto,
		HeartbeatInterval: 10 * time.Millisecond,
		LeaseTTL:          30 * time.Millisecond,
		StaleAfter:        30 * time.Millisecond,
		OfflineAfter:      50 * time.Millisecond,
		Now:               func() time.Time { return now },
		Logger:            discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Register(testRegisterInfo("n1", ""), ""); err != nil {
		t.Fatal(err)
	}
	now = now.Add(35 * time.Millisecond)
	changed := reg.Reap()
	if len(changed) != 1 || changed[0].Status != NodeStale {
		t.Fatalf("reap after 35ms = %+v, want stale", changed)
	}
	now = now.Add(20 * time.Millisecond)
	changed = reg.Reap()
	if len(changed) != 1 || changed[0].Status != NodeOffline {
		t.Fatalf("reap after 55ms = %+v, want offline", changed)
	}
	if err := reg.ApplyHeartbeat("n1", 1, LoadInfo{}); err != nil {
		t.Fatal(err)
	}
	got, _ := reg.Get("n1")
	if got.Status != NodeReady {
		t.Fatalf("heartbeat should restore ready, got %s", got.Status)
	}
}
