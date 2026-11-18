package control

import (
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"
)

// Node approval modes (design 05 §4.3).
const (
	ApprovalAuto      = "auto"
	ApprovalManual    = "manual"
	ApprovalAllowlist = "allowlist"
)

type NodeStatus string

const (
	NodePending     NodeStatus = "pending"
	NodeReady       NodeStatus = "ready"
	NodeStale       NodeStatus = "stale"
	NodeOffline     NodeStatus = "offline"
	NodeRevoked     NodeStatus = "revoked"
	NodeUnavailable NodeStatus = "unavailable"
)

var (
	ErrNodeNotFound       = errors.New("node not found")
	ErrNodeRevoked        = errors.New("node revoked")
	ErrNodeNotAllowlisted = errors.New("node not in allowlist")
	ErrDuplicateNodeName  = errors.New("node_name already in use")
	ErrEpochSuperseded    = errors.New("connection epoch superseded")
	ErrForgetNotAllowed   = errors.New("node must be revoked or offline before it can be forgotten")
)

// Limits mirrors the slaver capability snapshot. Master never forwards these
// as tool schemas; they are informational for node listing.
type Limits struct {
	MaxTimeoutSec  int32 `json:"max_timeout_sec,omitempty"`
	MaxOutput      int64 `json:"max_output,omitempty"`
	CallBudgetSec  int32 `json:"call_budget_sec,omitempty"`
	MaxActiveScans int32 `json:"max_active_scans,omitempty"`
	MaxActiveWaits int32 `json:"max_active_waits,omitempty"`
}

type Capabilities struct {
	Operations         []string `json:"operations,omitempty"`
	ReadOnly           bool     `json:"readonly"`
	Limits             Limits   `json:"limits,omitempty"`
	FSRoots            []string `json:"fs_roots,omitempty"`
	DenyCommands       string   `json:"deny_commands,omitempty"`
	CapabilityRevision uint64   `json:"capability_revision,omitempty"`
}

type LoadInfo struct {
	RunningExecutions uint32 `json:"running_executions"`
	ActiveScans       uint32 `json:"active_scans"`
	RunningJobs       uint32 `json:"running_jobs"`
}

// NodeRecord is the persistent view of a slaver. Time fields are serialized
// as RFC3339; internally the lease is always evaluated with relative times so
// clock skew cannot flip security decisions.
type NodeRecord struct {
	NodeID          string            `json:"node_id"`
	NodeName        string            `json:"node_name,omitempty"`
	Status          NodeStatus        `json:"status"`
	InstanceID      string            `json:"instance_id"`
	ConnectionEpoch uint64            `json:"connection_epoch"`
	Version         string            `json:"version"`
	Hostname        string            `json:"hostname"`
	Platform        string            `json:"platform"`
	Labels          map[string]string `json:"labels,omitempty"`
	Capabilities    Capabilities      `json:"capabilities"`
	ReadOnly        bool              `json:"readonly"`
	RegisteredAt    time.Time         `json:"registered_at"`
	LastSeenAt      time.Time         `json:"last_seen_at"`
	LeaseExpiresAt  time.Time         `json:"lease_expires_at"`
	ApprovedBy      string            `json:"approved_by,omitempty"`
	ApprovedAt      *time.Time        `json:"approved_at,omitempty"`
	RemoteAddr      string            `json:"remote_addr,omitempty"`
	CertFingerprint string            `json:"cert_fingerprint,omitempty"`
	Load            LoadInfo          `json:"load,omitempty"`
}

// RegisterInfo is what a validated register frame contributes.
type RegisterInfo struct {
	NodeID       string
	NodeName     string
	InstanceID   string
	Version      string
	Hostname     string
	Platform     string
	Labels       map[string]string
	Capabilities Capabilities
	ReadOnly     bool
}

type RegistryOptions struct {
	Approval          string
	Allowlist         []string
	StorePath         string
	HeartbeatInterval time.Duration
	LeaseTTL          time.Duration
	StaleAfter        time.Duration
	OfflineAfter      time.Duration
	Now               func() time.Time
	Logger            *slog.Logger
}

type NodeRegistry struct {
	mu     sync.RWMutex
	nodes  map[string]*NodeRecord
	byName map[string]string
	allow  map[string]bool
	opts   RegistryOptions
}

const nodesSchemaVersion = 1

type nodesFile struct {
	SchemaVersion int          `json:"schema_version"`
	UpdatedAt     time.Time    `json:"updated_at"`
	Nodes         []NodeRecord `json:"nodes"`
}

func NewNodeRegistry(opts RegistryOptions) (*NodeRegistry, error) {
	switch opts.Approval {
	case "":
		opts.Approval = ApprovalManual
	case ApprovalAuto, ApprovalManual, ApprovalAllowlist:
	default:
		return nil, fmt.Errorf("invalid node approval mode %q (want auto|manual|allowlist)", opts.Approval)
	}
	if opts.HeartbeatInterval <= 0 {
		opts.HeartbeatInterval = 10 * time.Second
	}
	if opts.LeaseTTL <= 0 {
		opts.LeaseTTL = 30 * time.Second
	}
	if opts.StaleAfter <= 0 {
		opts.StaleAfter = 3 * opts.HeartbeatInterval
	}
	if opts.OfflineAfter <= 0 {
		opts.OfflineAfter = 5 * opts.HeartbeatInterval
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	allow := make(map[string]bool, len(opts.Allowlist))
	for _, id := range opts.Allowlist {
		if id = strings.TrimSpace(id); id != "" {
			allow[id] = true
		}
	}
	r := &NodeRegistry{
		nodes:  make(map[string]*NodeRecord),
		byName: make(map[string]string),
		allow:  allow,
		opts:   opts,
	}
	if err := r.load(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *NodeRegistry) load() error {
	if r.opts.StorePath == "" {
		return nil
	}
	var f nodesFile
	found, err := ReadJSONFile(r.opts.StorePath, &f)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	if f.SchemaVersion > nodesSchemaVersion {
		return fmt.Errorf("%w: %s has schema %d, binary supports %d",
			ErrStateNewer, r.opts.StorePath, f.SchemaVersion, nodesSchemaVersion)
	}
	for i := range f.Nodes {
		rec := f.Nodes[i]
		// A process restart invalidates every live connection. Pending and
		// revoked records survive as-is; routable states become offline until
		// the slaver reconnects.
		switch rec.Status {
		case NodeReady, NodeStale, NodeUnavailable:
			rec.Status = NodeOffline
		}
		cp := cloneNode(&rec)
		r.nodes[cp.NodeID] = cp
		if cp.NodeName != "" {
			r.byName[cp.NodeName] = cp.NodeID
		}
	}
	return nil
}

// Save persists the registry. Heartbeats do not persist (they are high-rate
// and reconstructable); registration, approval and revocation do.
func (r *NodeRegistry) Save() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.saveLocked()
}

func (r *NodeRegistry) saveLocked() error {
	if r.opts.StorePath == "" {
		return nil
	}
	f := nodesFile{SchemaVersion: nodesSchemaVersion, UpdatedAt: r.opts.Now()}
	for _, rec := range r.nodes {
		f.Nodes = append(f.Nodes, *cloneNode(rec))
	}
	sort.Slice(f.Nodes, func(i, j int) bool { return f.Nodes[i].NodeID < f.Nodes[j].NodeID })
	if err := WriteJSONAtomic(r.opts.StorePath, &f); err != nil {
		r.opts.Logger.Error("persist node registry failed", "path", r.opts.StorePath, "err", err.Error())
		return err
	}
	return nil
}

// Register applies a validated register request and allocates a new epoch.
func (r *NodeRegistry) Register(info RegisterInfo, remoteAddr string) (NodeRecord, error) {
	if info.NodeID == "" {
		return NodeRecord{}, errors.New("node_id is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.opts.Now()

	rec, exists := r.nodes[info.NodeID]
	if exists && rec.Status == NodeRevoked {
		return NodeRecord{}, fmt.Errorf("%w: %s", ErrNodeRevoked, info.NodeID)
	}
	if r.opts.Approval == ApprovalAllowlist && !r.allow[info.NodeID] {
		return NodeRecord{}, fmt.Errorf("%w: %s", ErrNodeNotAllowlisted, info.NodeID)
	}
	if !exists {
		rec = &NodeRecord{NodeID: info.NodeID, Status: NodePending, RegisteredAt: now}
		r.nodes[info.NodeID] = rec
	}
	if info.NodeName != "" {
		if owner, taken := r.byName[info.NodeName]; taken && owner != info.NodeID {
			return NodeRecord{}, fmt.Errorf("%w: %s", ErrDuplicateNodeName, info.NodeName)
		}
		if rec.NodeName != "" && rec.NodeName != info.NodeName {
			delete(r.byName, rec.NodeName)
		}
		r.byName[info.NodeName] = info.NodeID
		rec.NodeName = info.NodeName
	}
	rec.InstanceID = info.InstanceID
	rec.Version = info.Version
	rec.Hostname = info.Hostname
	rec.Platform = info.Platform
	rec.Labels = cloneMap(info.Labels)
	rec.Capabilities = cloneCapabilities(info.Capabilities)
	rec.ReadOnly = info.ReadOnly
	rec.RemoteAddr = remoteAddr
	rec.LastSeenAt = now
	rec.LeaseExpiresAt = now.Add(r.opts.LeaseTTL)
	rec.ConnectionEpoch++ // strictly increasing, never reused

	switch r.opts.Approval {
	case ApprovalAuto, ApprovalAllowlist:
		rec.Status = NodeReady
	default: // manual
		if rec.ApprovedAt != nil {
			rec.Status = NodeReady
		} else {
			rec.Status = NodePending
		}
	}
	if err := r.saveLocked(); err != nil {
		return NodeRecord{}, err
	}
	return *cloneNode(rec), nil
}

// ApplyHeartbeat refreshes the lease and rejects frames from superseded
// connections.
func (r *NodeRegistry) ApplyHeartbeat(nodeID string, epoch uint64, load LoadInfo) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.nodes[nodeID]
	if !ok {
		return ErrNodeNotFound
	}
	if rec.Status == NodeRevoked {
		return ErrNodeRevoked
	}
	if epoch != rec.ConnectionEpoch {
		return ErrEpochSuperseded
	}
	now := r.opts.Now()
	rec.LastSeenAt = now
	rec.LeaseExpiresAt = now.Add(r.opts.LeaseTTL)
	rec.Load = load
	switch rec.Status {
	case NodeStale, NodeOffline, NodeUnavailable:
		rec.Status = NodeReady
	}
	return nil
}

// MarkDisconnected moves a node offline when its connection ends, but only if
// the epoch still matches (a newer connection owns the record otherwise).
func (r *NodeRegistry) MarkDisconnected(nodeID string, epoch uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.nodes[nodeID]
	if !ok || rec.ConnectionEpoch != epoch {
		return
	}
	switch rec.Status {
	case NodeReady, NodeStale:
		rec.Status = NodeOffline
		_ = r.saveLocked()
	}
}

// Reap applies the stale/offline transitions. It returns the records that
// changed so the caller can log them.
func (r *NodeRegistry) Reap() []NodeRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.opts.Now()
	var changed []NodeRecord
	for _, rec := range r.nodes {
		age := now.Sub(rec.LastSeenAt)
		switch rec.Status {
		case NodeReady:
			if age >= r.opts.OfflineAfter {
				rec.Status = NodeOffline
				changed = append(changed, *cloneNode(rec))
			} else if age >= r.opts.StaleAfter {
				rec.Status = NodeStale
				changed = append(changed, *cloneNode(rec))
			}
		case NodeStale:
			if age >= r.opts.OfflineAfter {
				rec.Status = NodeOffline
				changed = append(changed, *cloneNode(rec))
			}
		}
	}
	if len(changed) > 0 {
		_ = r.saveLocked()
	}
	return changed
}

func (r *NodeRegistry) Get(ref string) (NodeRecord, bool) {
	rec, err := r.Resolve(ref)
	return rec, err == nil
}

// Resolve matches an exact node_id first, then a unique node_name (D9).
func (r *NodeRegistry) Resolve(ref string) (NodeRecord, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if rec, ok := r.nodes[ref]; ok {
		return *cloneNode(rec), nil
	}
	if id, ok := r.byName[ref]; ok {
		if rec, ok := r.nodes[id]; ok {
			return *cloneNode(rec), nil
		}
	}
	return NodeRecord{}, fmt.Errorf("%w: %s", ErrNodeNotFound, ref)
}

func (r *NodeRegistry) List(status, label string) []NodeRecord {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []NodeRecord
	for _, rec := range r.nodes {
		if status != "" && string(rec.Status) != status {
			continue
		}
		if label != "" && !matchLabel(rec.Labels, label) {
			continue
		}
		out = append(out, *cloneNode(rec))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].NodeName != out[j].NodeName {
			return out[i].NodeName < out[j].NodeName
		}
		return out[i].NodeID < out[j].NodeID
	})
	return out
}

func (r *NodeRegistry) Approve(nodeID, actor string) (NodeRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.nodes[nodeID]
	if !ok {
		return NodeRecord{}, fmt.Errorf("%w: %s", ErrNodeNotFound, nodeID)
	}
	if rec.Status == NodeRevoked {
		return NodeRecord{}, fmt.Errorf("%w: %s", ErrNodeRevoked, nodeID)
	}
	now := r.opts.Now()
	rec.ApprovedAt = &now
	rec.ApprovedBy = actor
	rec.Status = NodeReady
	if err := r.saveLocked(); err != nil {
		return NodeRecord{}, err
	}
	return *cloneNode(rec), nil
}

func (r *NodeRegistry) Revoke(nodeID string) (NodeRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.nodes[nodeID]
	if !ok {
		return NodeRecord{}, fmt.Errorf("%w: %s", ErrNodeNotFound, nodeID)
	}
	rec.Status = NodeRevoked
	if err := r.saveLocked(); err != nil {
		return NodeRecord{}, err
	}
	return *cloneNode(rec), nil
}

func (r *NodeRegistry) Forget(nodeID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.nodes[nodeID]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNodeNotFound, nodeID)
	}
	if rec.Status != NodeRevoked && rec.Status != NodeOffline {
		return ErrForgetNotAllowed
	}
	delete(r.nodes, nodeID)
	if rec.NodeName != "" {
		delete(r.byName, rec.NodeName)
	}
	return r.saveLocked()
}

func matchLabel(labels map[string]string, expr string) bool {
	key, value, ok := strings.Cut(expr, "=")
	if !ok {
		return false
	}
	return labels[key] == value
}

func cloneNode(rec *NodeRecord) *NodeRecord {
	if rec == nil {
		return nil
	}
	out := *rec
	out.Labels = cloneMap(rec.Labels)
	out.Capabilities = cloneCapabilities(rec.Capabilities)
	if rec.ApprovedAt != nil {
		t := *rec.ApprovedAt
		out.ApprovedAt = &t
	}
	return &out
}

func cloneMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cloneCapabilities(in Capabilities) Capabilities {
	out := in
	out.Operations = append([]string(nil), in.Operations...)
	out.FSRoots = append([]string(nil), in.FSRoots...)
	return out
}
