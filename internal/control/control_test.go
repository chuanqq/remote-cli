package control

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	controlpb "github.com/chuanqq/remote-cli/internal/control/proto"
)

type fakeExecutor struct {
	result  string
	isJSON  bool
	isError bool

	mu    sync.Mutex
	calls int
}

func (f *fakeExecutor) Operations() []string { return []string{"fake_tool"} }
func (f *fakeExecutor) ReadOnly() bool       { return false }
func (f *fakeExecutor) Invoke(ctx context.Context, tool string, argsJSON []byte) (string, bool, bool, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	if tool != "fake_tool" {
		return "", false, true, fmt.Errorf("unknown tool %s", tool)
	}
	return f.result, f.isJSON, f.isError, nil
}

func (f *fakeExecutor) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func startTestMaster(t *testing.T, approval string, allow []string, verify TokenVerifier) *Master {
	t.Helper()
	m, err := NewMaster(MasterOptions{
		Approval:          approval,
		Allowlist:         allow,
		VerifyToken:       verify,
		MasterVersion:     "test-master",
		HeartbeatInterval: 50 * time.Millisecond,
		LeaseTTL:          200 * time.Millisecond,
		StaleAfter:        150 * time.Millisecond,
		OfflineAfter:      300 * time.Millisecond,
		Logger:            discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Serve(lis); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Stop)
	return m
}

func startTestSlaver(t *testing.T, addr, nodeID, token string, ex LocalExecutor) (context.CancelFunc, <-chan error) {
	t.Helper()
	sl, err := NewSlaver(SlaverOptions{
		MasterURL:         addr,
		ClusterToken:      token,
		NodeID:            nodeID,
		NodeName:          nodeID,
		Version:           "test-slaver",
		Hostname:          nodeID,
		Platform:          "test/test",
		Executor:          ex,
		StateDir:          t.TempDir(),
		HeartbeatInterval: 50 * time.Millisecond,
		Logger:            discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- sl.Run(ctx) }()
	t.Cleanup(cancel)
	return cancel, errCh
}

func waitFor(t *testing.T, timeout time.Duration, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func goodToken(v string) bool { return v == "good-token" }

func TestControlRegisterHeartbeatInvoke(t *testing.T) {
	m := startTestMaster(t, ApprovalAuto, nil, goodToken)
	_, errCh := startTestSlaver(t, m.Addr(), "node-a", "good-token", &fakeExecutor{result: `{"ok":true}`, isJSON: true})

	waitFor(t, 3*time.Second, "node-a ready", func() bool {
		rec, err := m.Registry().Resolve("node-a")
		return err == nil && rec.Status == NodeReady
	})

	res, err := m.InvokeTool(context.Background(), InvokeRequest{
		NodeID: "node-a", OperationID: "op-1", Tool: "fake_tool",
		Args: map[string]any{"x": 1}, Budget: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("invoke failed: %v", err)
	}
	if res.Text != `{"ok":true}` || !res.IsJSON || res.IsError {
		t.Fatalf("unexpected invoke result: %+v", res)
	}

	before, _ := m.Registry().Resolve("node-a")
	waitFor(t, 2*time.Second, "heartbeat refresh", func() bool {
		rec, err := m.Registry().Resolve("node-a")
		return err == nil && rec.LastSeenAt.After(before.LastSeenAt)
	})

	// Tool-level errors travel as data, not transport errors.
	res, err = m.InvokeTool(context.Background(), InvokeRequest{
		NodeID: "node-a", OperationID: "op-2", Tool: "unknown_tool", Budget: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("tool error should not be a transport error: %v", err)
	}
	if !res.IsError || res.State != "failed" {
		t.Fatalf("unknown tool result = %+v", res)
	}

	// Agent-side invoke of a missing node fails explicitly.
	if _, err := m.InvokeTool(context.Background(), InvokeRequest{
		NodeID: "missing", OperationID: "op-3", Tool: "fake_tool", Budget: time.Second,
	}); err == nil {
		t.Fatal("invoke on an unknown node should fail")
	}

	_ = errCh
}

func TestControlInvalidTokenRejected(t *testing.T) {
	m := startTestMaster(t, ApprovalAuto, nil, goodToken)
	_, errCh := startTestSlaver(t, m.Addr(), "node-a", "wrong-token", &fakeExecutor{result: "x"})
	select {
	case err := <-errCh:
		var rej *RejectError
		if !errors.As(err, &rej) || rej.Code != "unauthenticated" || rej.Retryable {
			t.Fatalf("invalid token error = %v, want non-retryable unauthenticated", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("invalid token registration did not terminate")
	}
	if _, err := m.Registry().Resolve("node-a"); err == nil {
		t.Fatal("rejected node must not be registered")
	}
}

func TestControlManualApproval(t *testing.T) {
	m := startTestMaster(t, ApprovalManual, nil, goodToken)
	startTestSlaver(t, m.Addr(), "node-a", "good-token", &fakeExecutor{result: "ok"})

	waitFor(t, 3*time.Second, "node-a pending", func() bool {
		rec, err := m.Registry().Resolve("node-a")
		return err == nil && rec.Status == NodePending
	})
	if _, err := m.ApproveNode("node-a"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, "node-a ready after approval", func() bool {
		rec, err := m.Registry().Resolve("node-a")
		return err == nil && rec.Status == NodeReady
	})
	if _, err := m.InvokeTool(context.Background(), InvokeRequest{
		NodeID: "node-a", OperationID: "op-1", Tool: "fake_tool", Budget: 2 * time.Second,
	}); err != nil {
		t.Fatalf("invoke after approval: %v", err)
	}
}

func TestControlAllowlistRejectsUnknownNode(t *testing.T) {
	m := startTestMaster(t, ApprovalAllowlist, []string{"allowed"}, goodToken)
	_, errCh := startTestSlaver(t, m.Addr(), "denied", "good-token", &fakeExecutor{result: "x"})
	select {
	case err := <-errCh:
		var rej *RejectError
		if !errors.As(err, &rej) || rej.Code != "permission_denied" {
			t.Fatalf("allowlist rejection = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("allowlist rejection did not terminate")
	}
}

func TestControlRevokeDisconnectsAndBlocksReconnect(t *testing.T) {
	m := startTestMaster(t, ApprovalAuto, nil, goodToken)
	cancel, _ := startTestSlaver(t, m.Addr(), "node-a", "good-token", &fakeExecutor{result: "ok"})
	waitFor(t, 3*time.Second, "node-a ready", func() bool {
		rec, err := m.Registry().Resolve("node-a")
		return err == nil && rec.Status == NodeReady
	})
	if _, err := m.RevokeNode("node-a"); err != nil {
		t.Fatal(err)
	}
	rec, err := m.Registry().Resolve("node-a")
	if err != nil || rec.Status != NodeRevoked {
		t.Fatalf("revoked record = %+v, %v", rec, err)
	}
	waitFor(t, 2*time.Second, "node unavailable after revoke", func() bool {
		_, err := m.InvokeTool(context.Background(), InvokeRequest{
			NodeID: "node-a", OperationID: "op-x", Tool: "fake_tool", Budget: 200 * time.Millisecond,
		})
		var ie *InvokeError
		return errors.As(err, &ie) && ie.Code == "node_unavailable"
	})
	cancel()

	// A new slaver with the revoked identity is refused.
	_, errCh := startTestSlaver(t, m.Addr(), "node-a", "good-token", &fakeExecutor{result: "x"})
	select {
	case err := <-errCh:
		var rej *RejectError
		if !errors.As(err, &rej) || rej.Code != "node_revoked" {
			t.Fatalf("reconnect after revoke = %v, want node_revoked", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("revoked node reconnect did not terminate")
	}
}

// rawRegister speaks the control protocol directly, as a would-be duplicate
// process would.
func rawRegister(t *testing.T, addr, nodeID, instanceID, token string) *controlpb.RegisterReject {
	t.Helper()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	stream, err := controlpb.NewControlClient(conn).Connect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	req := &controlpb.RegisterRequest{ClusterToken: token, SoftwareVersion: "raw-test"}
	if err := stream.Send(registerFrame(nodeID, instanceID, req)); err != nil {
		t.Fatal(err)
	}
	f, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	return f.GetRegisterReject()
}

func TestControlIdentityConflictAndReconnect(t *testing.T) {
	m := startTestMaster(t, ApprovalAuto, nil, goodToken)
	cancelA, _ := startTestSlaver(t, m.Addr(), "node-a", "good-token", &fakeExecutor{result: "1"})
	waitFor(t, 3*time.Second, "first registration", func() bool {
		rec, err := m.Registry().Resolve("node-a")
		return err == nil && rec.ConnectionEpoch >= 1
	})
	first, _ := m.Registry().Resolve("node-a")

	// A different process instance with the same node_id is rejected.
	rej := rawRegister(t, m.Addr(), "node-a", "intruder-instance", "good-token")
	if rej == nil || rej.GetCode() != "identity_conflict" || !rej.GetRetryable() {
		t.Fatalf("duplicate identity rejection = %+v", rej)
	}
	rec, _ := m.Registry().Resolve("node-a")
	if rec.ConnectionEpoch != first.ConnectionEpoch {
		t.Fatalf("conflicting registration must not change the epoch: %d -> %d",
			first.ConnectionEpoch, rec.ConnectionEpoch)
	}
	if _, err := m.InvokeTool(context.Background(), InvokeRequest{
		NodeID: "node-a", OperationID: "op-live", Tool: "fake_tool", Budget: 2 * time.Second,
	}); err != nil {
		t.Fatalf("live node should keep serving after a rejected duplicate: %v", err)
	}

	// After the original process disconnects, a new instance takes over with a
	// higher epoch.
	cancelA()
	waitFor(t, 3*time.Second, "node offline", func() bool {
		rec, err := m.Registry().Resolve("node-a")
		return err == nil && rec.Status == NodeOffline
	})
	startTestSlaver(t, m.Addr(), "node-a", "good-token", &fakeExecutor{result: "2"})
	waitFor(t, 3*time.Second, "epoch advance after reconnect", func() bool {
		rec, err := m.Registry().Resolve("node-a")
		return err == nil && rec.ConnectionEpoch > first.ConnectionEpoch
	})
}

func TestControlCapabilitiesReported(t *testing.T) {
	m := startTestMaster(t, ApprovalAuto, nil, goodToken)
	sl, err := NewSlaver(SlaverOptions{
		MasterURL:    m.Addr(),
		ClusterToken: "good-token",
		NodeID:       "caps-node",
		NodeName:     "caps-node",
		Version:      "test-slaver",
		Hostname:     "caps",
		Platform:     "test/test",
		Capabilities: Capabilities{
			Operations:   []string{"remote_execute", "remote_read_file"},
			Limits:       Limits{MaxTimeoutSec: 42, MaxOutput: 4096, CallBudgetSec: 7},
			FSRoots:      []string{"/tmp"},
			DenyCommands: "rm -rf",
		},
		Executor:          &fakeExecutor{result: "x"},
		StateDir:          t.TempDir(),
		HeartbeatInterval: 50 * time.Millisecond,
		Logger:            discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = sl.Run(ctx) }()
	waitFor(t, 3*time.Second, "caps-node ready", func() bool {
		rec, err := m.Registry().Resolve("caps-node")
		return err == nil && rec.Status == NodeReady
	})
	rec, _ := m.Registry().Resolve("caps-node")
	if len(rec.Capabilities.Operations) != 2 || rec.Capabilities.Limits.MaxTimeoutSec != 42 ||
		len(rec.Capabilities.FSRoots) != 1 || rec.Capabilities.DenyCommands != "rm -rf" {
		t.Fatalf("capabilities not propagated: %+v", rec.Capabilities)
	}
}

func TestControlIdempotentReplay(t *testing.T) {
	m := startTestMaster(t, ApprovalAuto, nil, goodToken)
	ex := &fakeExecutor{result: "once-json", isJSON: true}
	startTestSlaver(t, m.Addr(), "node-a", "good-token", ex)
	waitFor(t, 3*time.Second, "node-a ready", func() bool {
		rec, err := m.Registry().Resolve("node-a")
		return err == nil && rec.Status == NodeReady
	})

	first, err := m.InvokeTool(context.Background(), InvokeRequest{
		NodeID: "node-a", OperationID: "op-a", IdempotencyKey: "idem-1",
		Tool: "fake_tool", Budget: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.InvokeTool(context.Background(), InvokeRequest{
		NodeID: "node-a", OperationID: "op-b", IdempotencyKey: "idem-1",
		Tool: "fake_tool", Budget: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.Text != "once-json" || second.Text != "once-json" {
		t.Fatalf("replay results = %q / %q", first.Text, second.Text)
	}
	if got := ex.Calls(); got != 1 {
		t.Fatalf("executor calls = %d, want 1 (idempotent replay)", got)
	}

	// A different key is a different operation.
	if _, err := m.InvokeTool(context.Background(), InvokeRequest{
		NodeID: "node-a", OperationID: "op-c", IdempotencyKey: "idem-2",
		Tool: "fake_tool", Budget: 2 * time.Second,
	}); err != nil {
		t.Fatal(err)
	}
	if got := ex.Calls(); got != 2 {
		t.Fatalf("executor calls = %d, want 2 after a new idempotency key", got)
	}
}
