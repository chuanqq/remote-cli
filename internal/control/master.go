package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/peer"

	controlpb "github.com/chuanqq/remote-cli/internal/control/proto"
)

// TokenVerifier validates the pre-shared cluster token presented at register
// time. The master stores only a salted hash; the verifier lives in main so
// the crypto format stays in one place.
type TokenVerifier func(token string) bool

type MasterOptions struct {
	Listen            string
	StorePath         string
	Approval          string
	Allowlist         []string
	VerifyToken       TokenVerifier
	MasterVersion     string
	HeartbeatInterval time.Duration
	LeaseTTL          time.Duration
	StaleAfter        time.Duration
	OfflineAfter      time.Duration
	Logger            *slog.Logger
	Now               func() time.Time
}

// Master owns the control-plane listener and the node registry. Phase 2
// handles register/heartbeat/revoke; invoke routing is layered on in Phase 3
// without changing the wire protocol.
type Master struct {
	controlpb.UnimplementedControlServer

	opts     MasterOptions
	registry *NodeRegistry
	logger   *slog.Logger

	grpc *grpc.Server
	lis  net.Listener

	mu    sync.Mutex
	conns map[string]*nodeConn

	// pending correlates an operation_id with the caller waiting for its
	// result. Phase 3 builds the retry/query policy on top of this.
	pendingMu sync.Mutex
	pending   map[string]chan *controlpb.Result

	done     chan struct{}
	stopOnce sync.Once
}

func NewMaster(opts MasterOptions) (*Master, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.MasterVersion == "" {
		opts.MasterVersion = "dev"
	}
	reg, err := NewNodeRegistry(RegistryOptions{
		Approval:          opts.Approval,
		Allowlist:         opts.Allowlist,
		StorePath:         opts.StorePath,
		HeartbeatInterval: opts.HeartbeatInterval,
		LeaseTTL:          opts.LeaseTTL,
		StaleAfter:        opts.StaleAfter,
		OfflineAfter:      opts.OfflineAfter,
		Now:               opts.Now,
		Logger:            opts.Logger,
	})
	if err != nil {
		return nil, err
	}
	return &Master{
		opts:     opts,
		registry: reg,
		logger:   opts.Logger,
		conns:    make(map[string]*nodeConn),
		pending:  make(map[string]chan *controlpb.Result),
		done:     make(chan struct{}),
	}, nil
}

func (m *Master) Registry() *NodeRegistry { return m.registry }

// Start listens on REMOTECTL_CONTROL_LISTEN (no default by design: D33).
func (m *Master) Start() error {
	if m.opts.Listen == "" {
		return errors.New("control listen address is required (REMOTECTL_CONTROL_LISTEN)")
	}
	lis, err := net.Listen("tcp", m.opts.Listen)
	if err != nil {
		return fmt.Errorf("listen control port: %w", err)
	}
	return m.Serve(lis)
}

// Serve accepts control connections on an already-bound listener (tests use
// 127.0.0.1:0 to avoid fixed ports).
func (m *Master) Serve(lis net.Listener) error {
	m.lis = lis
	m.grpc = grpc.NewServer(
		grpc.MaxRecvMsgSize(MaxMessageBytes),
		grpc.MaxSendMsgSize(MaxMessageBytes),
	)
	controlpb.RegisterControlServer(m.grpc, m)
	go func() {
		if err := m.grpc.Serve(lis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			m.logger.Error("control server stopped", "err", err.Error())
		}
	}()
	go m.reapLoop()
	m.logger.Info("control channel listening", "addr", lis.Addr().String(),
		"approval", m.opts.Approval, "auth", "cluster-token (mTLS TODO-1)")
	return nil
}

func (m *Master) Addr() string {
	if m.lis == nil {
		return ""
	}
	return m.lis.Addr().String()
}

func (m *Master) reapLoop() {
	interval := m.opts.HeartbeatInterval
	if interval <= 0 || interval > time.Second {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-m.done:
			return
		case <-ticker.C:
			for _, rec := range m.registry.Reap() {
				m.logger.Info("node status changed", "node_id", rec.NodeID,
					"node_name", rec.NodeName, "status", string(rec.Status))
			}
		}
	}
}

// Connect implements the single bidirectional control stream.
func (m *Master) Connect(stream controlpb.Control_ConnectServer) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	reg := first.GetRegister()
	if reg == nil {
		_ = stream.Send(rejectFrame(first.GetNodeId(), "invalid_argument", "first frame must be register", false))
		return nil
	}
	nodeID := first.GetNodeId()
	instanceID := first.GetInstanceId()
	if nodeID == "" || instanceID == "" {
		_ = stream.Send(rejectFrame(nodeID, "invalid_argument", "node_id and instance_id are required", false))
		return nil
	}
	if first.GetProtocolMajor() != ProtocolMajor {
		_ = stream.Send(rejectFrame(nodeID, "protocol_incompatible",
			fmt.Sprintf("control protocol major %d, master supports %d", first.GetProtocolMajor(), ProtocolMajor), false))
		return nil
	}
	if m.opts.VerifyToken == nil || !m.opts.VerifyToken(reg.GetClusterToken()) {
		m.logger.Warn("control register rejected: invalid cluster token", "node_id", nodeID, "peer", peerAddr(stream.Context()))
		_ = stream.Send(rejectFrame(nodeID, "unauthenticated", "cluster token invalid", false))
		return nil
	}

	// Two live processes must never share a node_id (design 03 §5.4). A
	// reconnect from the same process keeps its instance_id, so only a real
	// duplicate identity is rejected. The rejection is retryable: after a
	// crash the old stream is reaped shortly and the restart succeeds.
	m.mu.Lock()
	liveConn := m.conns[nodeID]
	m.mu.Unlock()
	if liveConn != nil && liveConn.instanceID != instanceID {
		m.logger.Warn("control register rejected: identity conflict",
			"node_id", nodeID, "connected_instance", liveConn.instanceID,
			"new_instance", instanceID, "peer", peerAddr(stream.Context()))
		_ = stream.Send(rejectFrame(nodeID, "identity_conflict",
			"another process instance of this node_id is connected", true))
		return nil
	}

	info := RegisterInfo{
		NodeID:       nodeID,
		NodeName:     reg.GetNodeName(),
		InstanceID:   instanceID,
		Version:      reg.GetSoftwareVersion(),
		Hostname:     reg.GetHostname(),
		Platform:     reg.GetPlatform(),
		Labels:       reg.GetLabels(),
		ReadOnly:     reg.GetCapabilities().GetReadonly(),
		Capabilities: capabilitiesFromProto(reg.GetCapabilities()),
	}
	rec, err := m.registry.Register(info, peerAddr(stream.Context()))
	if err != nil {
		code, retryable := registerErrorCode(err)
		m.logger.Warn("control register rejected", "node_id", nodeID, "err", err.Error())
		_ = stream.Send(rejectFrame(nodeID, code, err.Error(), retryable))
		return nil
	}

	conn := &nodeConn{
		nodeID:     nodeID,
		instanceID: instanceID,
		epoch:      rec.ConnectionEpoch,
		stream:     stream,
		send:       make(chan *controlpb.Frame, 64),
		done:       make(chan struct{}),
	}
	m.mu.Lock()
	old := m.conns[nodeID]
	m.conns[nodeID] = conn
	m.mu.Unlock()
	go conn.writeLoop()

	var hbSec, leaseSec uint32
	if m.opts.HeartbeatInterval > 0 {
		hbSec = uint32(m.opts.HeartbeatInterval / time.Second)
	}
	if m.opts.LeaseTTL > 0 {
		leaseSec = uint32(m.opts.LeaseTTL / time.Second)
	}
	conn.trySend(registerAckFrame(nodeID, instanceID, rec.ConnectionEpoch, string(rec.Status),
		hbSec, leaseSec, m.opts.MasterVersion, ""))
	m.logger.Info("slaver registered", "node_id", nodeID, "node_name", rec.NodeName,
		"status", string(rec.Status), "epoch", rec.ConnectionEpoch,
		"peer", peerAddr(stream.Context()), "version", rec.Version)

	if old != nil {
		old.trySend(errorFrame(nodeID, "epoch_superseded",
			fmt.Sprintf("connection superseded by epoch %d", rec.ConnectionEpoch), true))
		old.close()
	}

	defer func() {
		m.mu.Lock()
		current := m.conns[nodeID] == conn
		if current {
			delete(m.conns, nodeID)
		}
		m.mu.Unlock()
		conn.close()
		if current {
			m.registry.MarkDisconnected(nodeID, conn.epoch)
			m.logger.Info("slaver disconnected", "node_id", nodeID, "epoch", conn.epoch)
		}
	}()

	for {
		f, err := stream.Recv()
		if err != nil {
			return nil
		}
		switch p := f.GetPayload().(type) {
		case *controlpb.Frame_Heartbeat:
			load := LoadInfo{}
			if l := p.Heartbeat.GetLoad(); l != nil {
				load = LoadInfo{
					RunningExecutions: l.GetRunningExecutions(),
					ActiveScans:       l.GetActiveScans(),
					RunningJobs:       l.GetRunningJobs(),
				}
			}
			if err := m.registry.ApplyHeartbeat(nodeID, conn.epoch, load); err != nil {
				if errors.Is(err, ErrEpochSuperseded) {
					conn.trySend(errorFrame(nodeID, "epoch_superseded", err.Error(), true))
					return nil
				}
				if errors.Is(err, ErrNodeRevoked) {
					conn.trySend(errorFrame(nodeID, "node_revoked", err.Error(), false))
					return nil
				}
			}
			conn.trySend(heartbeatAckFrame(p.Heartbeat.GetSequence()))
		case *controlpb.Frame_Query:
			// Result correlation arrives with the Phase 3 dispatcher.
			conn.trySend(queryResultFrame(nodeID, instanceID, conn.epoch, &controlpb.QueryResult{
				OperationId: p.Query.GetOperationId(),
				State:       "task_not_found_or_expired",
			}))
		case *controlpb.Frame_Cancel:
			conn.trySend(cancelAckFrame(nodeID, instanceID, conn.epoch,
				p.Cancel.GetOperationId(), "cancel_requested"))
		case *controlpb.Frame_Result:
			m.deliverResult(p.Result)
		case *controlpb.Frame_QueryResult:
			if p.QueryResult.GetResult() != nil {
				m.deliverResult(p.QueryResult.GetResult())
			}
		case *controlpb.Frame_Accepted:
			m.logger.Debug("invoke accepted", "node_id", nodeID,
				"operation_id", p.Accepted.GetOperationId(), "local_task_id", p.Accepted.GetLocalTaskId())
		case *controlpb.Frame_Close:
			return nil
		}
	}
}

// InvokeResult is the master-side view of a slaver tool result.
type InvokeResult struct {
	State        string
	ExitCode     int32
	Text         string
	IsJSON       bool
	IsError      bool
	ErrorCode    string
	ErrorMessage string
}

// InvokeError is a routing/invoke failure with a stable code (design 03 §8).
type InvokeError struct {
	Code      string
	Message   string
	Retryable bool
}

func (e *InvokeError) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// InvokeRequest is one master -> slaver tool call.
type InvokeRequest struct {
	NodeID      string
	OperationID string
	TraceID     string
	// IdempotencyKey is set for side-effecting tools (D17). The slaver caches
	// key -> result and replays it instead of re-executing.
	IdempotencyKey string
	Tool           string
	Args           map[string]any
	Budget         time.Duration
}

// InvokeTool sends one invoke to a connected node and waits for its result.
func (m *Master) InvokeTool(ctx context.Context, req InvokeRequest) (InvokeResult, error) {
	nodeID := req.NodeID
	operationID := req.OperationID
	args := req.Args
	budget := req.Budget
	m.mu.Lock()
	conn := m.conns[nodeID]
	m.mu.Unlock()
	if conn == nil {
		return InvokeResult{}, &InvokeError{Code: "node_unavailable", Message: "node has no active control connection"}
	}
	argsJSON, err := json.Marshal(args)
	if err != nil {
		return InvokeResult{}, &InvokeError{Code: "invalid_argument", Message: "encode arguments: " + err.Error()}
	}
	if budget <= 0 {
		budget = 30 * time.Second
	}
	ch := make(chan *controlpb.Result, 1)
	m.pendingMu.Lock()
	m.pending[operationID] = ch
	m.pendingMu.Unlock()
	defer func() {
		m.pendingMu.Lock()
		delete(m.pending, operationID)
		m.pendingMu.Unlock()
	}()

	f := frame("invoke")
	f.NodeId = nodeID
	f.InstanceId = conn.instanceID
	f.ConnectionEpoch = conn.epoch
	f.OperationId = operationID
	f.TraceId = req.TraceID
	f.RequestId = req.TraceID
	f.IdempotencyKey = req.IdempotencyKey
	f.Payload = &controlpb.Frame_Invoke{Invoke: &controlpb.Invoke{
		Tool:          req.Tool,
		ArgumentsJson: string(argsJSON),
		CallBudgetMs:  int32(budget / time.Millisecond),
	}}
	if !conn.trySend(f) {
		return InvokeResult{}, &InvokeError{Code: "node_unavailable", Message: "control send queue is full or connection closed"}
	}

	timer := time.NewTimer(budget)
	defer timer.Stop()
	select {
	case res := <-ch:
		return InvokeResult{
			State:        res.GetState(),
			ExitCode:     res.GetExitCode(),
			Text:         res.GetResultText(),
			IsJSON:       res.GetIsJson(),
			IsError:      res.GetIsError(),
			ErrorCode:    res.GetErrorCode(),
			ErrorMessage: res.GetErrorMessage(),
		}, nil
	case <-ctx.Done():
		return InvokeResult{}, ctx.Err()
	case <-timer.C:
		return InvokeResult{}, &InvokeError{Code: "deadline_exceeded", Message: "call budget expired waiting for the node result"}
	case <-conn.done:
		return InvokeResult{}, &InvokeError{Code: "node_unavailable", Message: "control connection closed while waiting for the result"}
	}
}

func (m *Master) deliverResult(res *controlpb.Result) {
	if res == nil || res.GetOperationId() == "" {
		return
	}
	m.pendingMu.Lock()
	ch := m.pending[res.GetOperationId()]
	delete(m.pending, res.GetOperationId())
	m.pendingMu.Unlock()
	if ch != nil {
		select {
		case ch <- res:
		default:
		}
	}
}

// ApproveNode promotes a pending node and pushes a fresh register_ack so the
// slaver observes readiness without reconnecting.
func (m *Master) ApproveNode(nodeID string) (NodeRecord, error) {
	rec, err := m.registry.Approve(nodeID, "mcp")
	if err != nil {
		return rec, err
	}
	m.sendRegisterAck(rec)
	return rec, nil
}

func (m *Master) sendRegisterAck(rec NodeRecord) {
	m.mu.Lock()
	conn := m.conns[rec.NodeID]
	m.mu.Unlock()
	if conn == nil || conn.epoch != rec.ConnectionEpoch {
		return
	}
	var hbSec, leaseSec uint32
	if m.opts.HeartbeatInterval > 0 {
		hbSec = uint32(m.opts.HeartbeatInterval / time.Second)
	}
	if m.opts.LeaseTTL > 0 {
		leaseSec = uint32(m.opts.LeaseTTL / time.Second)
	}
	conn.trySend(registerAckFrame(rec.NodeID, rec.InstanceID, rec.ConnectionEpoch, string(rec.Status),
		hbSec, leaseSec, m.opts.MasterVersion, ""))
}

// RevokeNode marks the node revoked and immediately closes its connection
// (D28).
func (m *Master) RevokeNode(nodeID string) (NodeRecord, error) {
	rec, err := m.registry.Revoke(nodeID)
	if err != nil {
		return rec, err
	}
	m.mu.Lock()
	conn := m.conns[nodeID]
	delete(m.conns, nodeID)
	m.mu.Unlock()
	if conn != nil {
		conn.trySend(errorFrame(nodeID, "node_revoked", "node revoked by administrator", false))
		conn.close()
	}
	return rec, nil
}

func (m *Master) ForgetNode(nodeID string) error {
	m.mu.Lock()
	_, connected := m.conns[nodeID]
	m.mu.Unlock()
	if connected {
		return errors.New("node is connected; revoke it before forgetting the record")
	}
	return m.registry.Forget(nodeID)
}

func (m *Master) Stop() {
	m.stopOnce.Do(func() {
		close(m.done)
		m.mu.Lock()
		conns := make([]*nodeConn, 0, len(m.conns))
		for _, c := range m.conns {
			conns = append(conns, c)
		}
		m.conns = make(map[string]*nodeConn)
		m.mu.Unlock()
		for _, c := range conns {
			c.trySend(closeFrame(c.nodeID, "master shutting down"))
			c.close()
		}
		if m.grpc != nil {
			stopped := make(chan struct{})
			go func() {
				m.grpc.GracefulStop()
				close(stopped)
			}()
			select {
			case <-stopped:
			case <-time.After(5 * time.Second):
				m.grpc.Stop()
			}
		}
		if err := m.registry.Save(); err != nil {
			m.logger.Warn("persist node registry on shutdown failed", "err", err.Error())
		}
	})
}

type nodeConn struct {
	nodeID     string
	instanceID string
	epoch      uint64
	stream     controlpb.Control_ConnectServer
	send       chan *controlpb.Frame
	done       chan struct{}
	closeOnce  sync.Once
}

func (c *nodeConn) close() { c.closeOnce.Do(func() { close(c.done) }) }

func (c *nodeConn) trySend(f *controlpb.Frame) bool {
	select {
	case <-c.done:
		return false
	default:
	}
	select {
	case c.send <- f:
		return true
	case <-c.done:
		return false
	default:
		// Queue exhausted: drop the connection instead of blocking the master.
		c.close()
		return false
	}
}

func (c *nodeConn) writeLoop() {
	for {
		select {
		case <-c.done:
			return
		case f := <-c.send:
			select {
			case <-c.done:
				return
			default:
			}
			if err := c.stream.Send(f); err != nil {
				c.close()
				return
			}
		}
	}
}

func peerAddr(ctx context.Context) string {
	if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
		return p.Addr.String()
	}
	return ""
}

func registerErrorCode(err error) (string, bool) {
	switch {
	case errors.Is(err, ErrNodeRevoked):
		return "node_revoked", false
	case errors.Is(err, ErrNodeNotAllowlisted):
		return "permission_denied", false
	case errors.Is(err, ErrDuplicateNodeName):
		return "identity_conflict", false
	default:
		return "internal", true
	}
}

func capabilitiesFromProto(in *controlpb.Capabilities) Capabilities {
	if in == nil {
		return Capabilities{}
	}
	out := Capabilities{
		Operations:         append([]string(nil), in.GetOperations()...),
		ReadOnly:           in.GetReadonly(),
		FSRoots:            append([]string(nil), in.GetFsRoots()...),
		DenyCommands:       in.GetDenyCommands(),
		CapabilityRevision: in.GetCapabilityRevision(),
	}
	if l := in.GetLimits(); l != nil {
		out.Limits = Limits{
			MaxTimeoutSec:  l.GetMaxTimeoutSec(),
			MaxOutput:      l.GetMaxOutput(),
			CallBudgetSec:  l.GetCallBudgetSec(),
			MaxActiveScans: l.GetMaxActiveScans(),
			MaxActiveWaits: l.GetMaxActiveWaits(),
		}
	}
	return out
}
