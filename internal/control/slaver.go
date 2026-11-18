package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	controlpb "github.com/chuanqq/remote-cli/internal/control/proto"
)

// LocalExecutor is implemented by main's operation adapter. It runs one tool
// against the local execution core and returns the tool's content block.
type LocalExecutor interface {
	Operations() []string
	ReadOnly() bool
	Invoke(ctx context.Context, tool string, argsJSON []byte) (resultText string, isJSON, isError bool, err error)
}

type SlaverOptions struct {
	MasterURL    string
	ClusterToken string
	NodeID       string
	NodeName     string
	Version      string
	Hostname     string
	Platform     string
	Labels       map[string]string
	Capabilities Capabilities
	Executor     LocalExecutor
	StateDir     string

	HeartbeatInterval time.Duration
	LeaseTTL          time.Duration
	Logger            *slog.Logger
}

// RejectError is a non-retryable (or deliberately retryable) registration
// rejection.
type RejectError struct {
	Code      string
	Message   string
	Retryable bool
}

func (e *RejectError) Error() string {
	return fmt.Sprintf("registration rejected: %s: %s", e.Code, e.Message)
}

type Slaver struct {
	opts SlaverOptions
	// instanceID identifies this process across reconnects (design 03 §5.4).
	// It must not change when the control stream reconnects.
	instanceID string
	logger     *slog.Logger

	mu        sync.Mutex
	results   map[string]*controlpb.Result
	order     []string
	inflight  map[string]context.CancelFunc
	idem      map[string]string // idempotency_key -> operation_id
	idemOrder []string
}

const maxCachedResults = 200

func NewSlaver(opts SlaverOptions) (*Slaver, error) {
	if opts.Executor == nil {
		return nil, errors.New("local executor is required")
	}
	if strings.TrimSpace(opts.MasterURL) == "" {
		return nil, errors.New("REMOTECTL_MASTER_URL is required")
	}
	if strings.TrimSpace(opts.ClusterToken) == "" {
		return nil, errors.New("REMOTECTL_CLUSTER_TOKEN (or _FILE) is required")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.HeartbeatInterval <= 0 {
		opts.HeartbeatInterval = 10 * time.Second
	}
	if len(opts.Capabilities.Operations) == 0 {
		opts.Capabilities.Operations = opts.Executor.Operations()
	}
	opts.Capabilities.ReadOnly = opts.Executor.ReadOnly()
	return &Slaver{
		opts:       opts,
		instanceID: uuid.NewString(),
		logger:     opts.Logger,
		results:    make(map[string]*controlpb.Result),
		inflight:   make(map[string]context.CancelFunc),
		idem:       make(map[string]string),
	}, nil
}

// Run keeps the control connection alive until ctx is cancelled. Transient
// failures back off exponentially (1s..30s); a revoked node is terminal.
func (s *Slaver) Run(ctx context.Context) error {
	backoff := time.Second
	for {
		start := time.Now()
		err := s.runOnce(ctx)
		if ctx.Err() != nil {
			s.logger.Info("slaver stopped")
			return nil
		}
		var rej *RejectError
		if errors.As(err, &rej) && !rej.Retryable {
			return err
		}
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		s.logger.Warn("control connection lost; reconnecting",
			"err", errString(err), "backoff_sec", int(backoff/time.Second))
		select {
		case <-ctx.Done():
			s.logger.Info("slaver stopped")
			return nil
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
	}
}

func (s *Slaver) runOnce(ctx context.Context) error {
	target := strings.TrimPrefix(strings.TrimPrefix(s.opts.MasterURL, "https://"), "http://")
	target = strings.TrimSuffix(target, "/")
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer conn.Close()

	stream, err := controlpb.NewControlClient(conn).Connect(ctx)
	if err != nil {
		return err
	}
	instanceID := s.instanceID
	req := &controlpb.RegisterRequest{
		ClusterToken:    s.opts.ClusterToken,
		SoftwareVersion: s.opts.Version,
		Capabilities:    capabilitiesToProto(s.opts.Capabilities),
		Labels:          s.opts.Labels,
		Hostname:        s.opts.Hostname,
		Platform:        s.opts.Platform,
		NodeName:        s.opts.NodeName,
	}
	if err := stream.Send(registerFrame(s.opts.NodeID, instanceID, req)); err != nil {
		return err
	}
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	switch p := first.GetPayload().(type) {
	case *controlpb.Frame_RegisterReject:
		return &RejectError{
			Code:      p.RegisterReject.GetCode(),
			Message:   p.RegisterReject.GetMessage(),
			Retryable: p.RegisterReject.GetRetryable(),
		}
	case *controlpb.Frame_RegisterAck:
		ack := p.RegisterAck
		heartbeat := s.opts.HeartbeatInterval
		if ack.GetHeartbeatIntervalSec() > 0 {
			heartbeat = time.Duration(ack.GetHeartbeatIntervalSec()) * time.Second
		}
		s.logger.Info("registered with master",
			"node_id", s.opts.NodeID, "node_name", s.opts.NodeName,
			"status", ack.GetStatus(), "epoch", ack.GetConnectionEpoch(),
			"heartbeat_sec", int(heartbeat/time.Second))
		snd := &streamSender{stream: stream}
		hbCtx, hbCancel := context.WithCancel(ctx)
		defer hbCancel()
		go s.heartbeatLoop(hbCtx, snd, instanceID, ack.GetConnectionEpoch(), heartbeat)
		return s.readLoop(ctx, stream, snd, instanceID, ack.GetConnectionEpoch())
	default:
		return fmt.Errorf("unexpected first control frame %q", first.GetMessageType())
	}
}

func (s *Slaver) readLoop(ctx context.Context, stream controlpb.Control_ConnectClient,
	snd *streamSender, instanceID string, epoch uint64) error {
	for {
		f, err := stream.Recv()
		if err != nil {
			return err
		}
		switch p := f.GetPayload().(type) {
		case *controlpb.Frame_Invoke:
			go s.handleInvoke(ctx, snd, instanceID, epoch,
				f.GetOperationId(), f.GetTraceId(), f.GetIdempotencyKey(), p.Invoke)
		case *controlpb.Frame_Query:
			s.handleQuery(snd, instanceID, epoch, p.Query)
		case *controlpb.Frame_Cancel:
			s.handleCancel(snd, instanceID, epoch, p.Cancel)
		case *controlpb.Frame_RegisterAck:
			s.logger.Info("node status updated",
				"status", p.RegisterAck.GetStatus(), "epoch", p.RegisterAck.GetConnectionEpoch())
		case *controlpb.Frame_Error:
			code := p.Error.GetCode()
			s.logger.Warn("control error from master", "code", code, "message", p.Error.GetMessage())
			if code == "epoch_superseded" || code == "node_revoked" {
				return &RejectError{Code: code, Message: p.Error.GetMessage(), Retryable: code == "epoch_superseded"}
			}
		case *controlpb.Frame_Close:
			return nil
		}
	}
}

func (s *Slaver) heartbeatLoop(ctx context.Context, snd *streamSender, instanceID string, epoch uint64, interval time.Duration) {
	var sequence uint64
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sequence++
			if err := snd.send(heartbeatFrame(s.opts.NodeID, instanceID, epoch, sequence, LoadInfo{})); err != nil {
				return
			}
		}
	}
}

func (s *Slaver) handleInvoke(ctx context.Context, snd *streamSender, instanceID string, epoch uint64,
	operationID, traceID, idempotencyKey string, inv *controlpb.Invoke) {
	if operationID == "" || inv == nil {
		return
	}
	// Idempotent replay: the same key returns the original result instead of
	// re-executing the tool (D17, network replay protection).
	if idempotencyKey != "" {
		s.mu.Lock()
		prevOp, seen := s.idem[idempotencyKey]
		var cached *controlpb.Result
		if seen {
			cached = s.results[prevOp]
		}
		s.mu.Unlock()
		if cached != nil {
			clone := proto.Clone(cached).(*controlpb.Result)
			clone.OperationId = operationID
			clone.Sequence = 2
			clone.FinishedAt = timestamppb.Now()
			_ = snd.send(acceptedFrame(s.opts.NodeID, instanceID, epoch, operationID, "", ""))
			_ = snd.send(resultFrame(s.opts.NodeID, instanceID, epoch, clone))
			s.cacheResult(operationID, clone)
			s.logger.Info("idempotent replay", "tool", inv.GetTool(),
				"operation_id", operationID, "idempotency_key", idempotencyKey)
			return
		}
	}
	callCtx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	s.inflight[operationID] = cancel
	s.mu.Unlock()
	defer func() {
		cancel()
		s.mu.Lock()
		delete(s.inflight, operationID)
		s.mu.Unlock()
	}()

	_ = snd.send(acceptedFrame(s.opts.NodeID, instanceID, epoch, operationID, "", ""))

	start := time.Now()
	text, isJSON, isError, err := s.opts.Executor.Invoke(callCtx, inv.GetTool(), []byte(inv.GetArgumentsJson()))
	res := &controlpb.Result{
		OperationId: operationID,
		Sequence:    2,
		State:       "completed",
		ResultText:  text,
		IsJson:      isJSON,
		IsError:     isError,
		FinishedAt:  timestamppb.Now(),
	}
	if err != nil {
		res.State = "failed"
		res.IsError = true
		res.ErrorCode = "internal"
		res.ErrorMessage = err.Error()
		if ce, ok := err.(interface{ ErrorCode() string }); ok && ce.ErrorCode() != "" {
			res.ErrorCode = ce.ErrorCode()
		}
	}
	if err := snd.send(resultFrame(s.opts.NodeID, instanceID, epoch, res)); err != nil {
		s.logger.Warn("send invoke result failed", "operation_id", operationID, "err", err.Error())
	}
	s.cacheResult(operationID, res)
	if idempotencyKey != "" {
		s.recordIdempotency(idempotencyKey, operationID)
	}

	exitCode := 0
	if res.IsJson {
		var obj map[string]any
		if json.Unmarshal([]byte(text), &obj) == nil {
			if v, ok := obj["exit_code"].(float64); ok {
				exitCode = int(v)
			}
		}
	}
	// Slaver-side audit line: the master records the routing decision, this
	// records the actual local execution (design 05 §6).
	s.logger.Info("audit", "type", "audit",
		"node_id", s.opts.NodeID, "trace_id", traceID, "operation_id", operationID,
		"tool", inv.GetTool(), "state", res.State, "exit_code", exitCode,
		"is_error", res.IsError, "dur_ms", time.Since(start).Milliseconds())
}

func (s *Slaver) recordIdempotency(key, operationID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.idem[key]; !exists {
		s.idemOrder = append(s.idemOrder, key)
	}
	s.idem[key] = operationID
	for len(s.idemOrder) > maxCachedResults {
		oldest := s.idemOrder[0]
		s.idemOrder = s.idemOrder[1:]
		delete(s.idem, oldest)
	}
}

func (s *Slaver) handleQuery(snd *streamSender, instanceID string, epoch uint64, q *controlpb.Query) {
	opID := q.GetOperationId()
	s.mu.Lock()
	res := s.results[opID]
	s.mu.Unlock()
	qr := &controlpb.QueryResult{OperationId: opID}
	if res == nil {
		qr.State = "task_not_found_or_expired"
	} else {
		qr.State = res.State
		qr.Sequence = res.Sequence
		qr.Result = res
	}
	_ = snd.send(queryResultFrame(s.opts.NodeID, instanceID, epoch, qr))
}

func (s *Slaver) handleCancel(snd *streamSender, instanceID string, epoch uint64, c *controlpb.Cancel) {
	opID := c.GetOperationId()
	s.mu.Lock()
	cancel := s.inflight[opID]
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	// Cancel is a request; the final state still arrives as a result.
	_ = snd.send(cancelAckFrame(s.opts.NodeID, instanceID, epoch, opID, "cancel_requested"))
}

func (s *Slaver) cacheResult(operationID string, res *controlpb.Result) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.results[operationID]; !exists {
		s.order = append(s.order, operationID)
	}
	s.results[operationID] = res
	for len(s.order) > maxCachedResults {
		oldest := s.order[0]
		s.order = s.order[1:]
		delete(s.results, oldest)
	}
}

type streamSender struct {
	mu     sync.Mutex
	stream controlpb.Control_ConnectClient
}

// send serializes concurrent senders: gRPC permits only one writer per stream.
func (s *streamSender) send(f *controlpb.Frame) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stream.Send(f)
}

func capabilitiesToProto(in Capabilities) *controlpb.Capabilities {
	out := &controlpb.Capabilities{
		Operations:         append([]string(nil), in.Operations...),
		Readonly:           in.ReadOnly,
		FsRoots:            append([]string(nil), in.FSRoots...),
		DenyCommands:       in.DenyCommands,
		CapabilityRevision: in.CapabilityRevision,
		Limits: &controlpb.Limits{
			MaxTimeoutSec:  in.Limits.MaxTimeoutSec,
			MaxOutput:      in.Limits.MaxOutput,
			CallBudgetSec:  in.Limits.CallBudgetSec,
			MaxActiveScans: in.Limits.MaxActiveScans,
			MaxActiveWaits: in.Limits.MaxActiveWaits,
		},
	}
	return out
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
