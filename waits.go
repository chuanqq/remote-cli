package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Server-side watchers for remote_wait_for
//
// A wait used to live inside its tool call, so it could never outlast the
// client's request timeout: production waits with timeout_seconds=300 were
// cancelled at exactly 60s ("wait cancelled"), and agents fell back to local
// sleep plus ~15 rounds of probing per 90-minute rollout step.
//
// A wait now runs as a watcher owned by the server, for up to its own
// timeout_seconds (max 24h). The call blocks at most the call budget; if the
// condition has not resolved by then it returns state=pending with a wait_id,
// and calling remote_wait_for again with that wait_id resumes the SAME
// watcher — a log_regex wait keeps its original offset, so a line written
// between two calls is never missed. A dropped connection does not end the
// watcher either.
// ---------------------------------------------------------------------------

const (
	waitRetention  = 30 * time.Minute // resolved waits stay fetchable this long
	maxActiveWaits = 64
	maxKeptWaits   = 200
)

// Wait states.
const (
	WaitSatisfied = "satisfied"
	WaitTimedOut  = "timed_out"
	WaitPending   = "pending" // not resolved yet: resume with wait_id
	WaitCancelled = "cancelled"
)

// Wait is a running or resolved watcher.
type Wait struct {
	ID        string
	StartedAt time.Time
	Deadline  time.Time

	cancel context.CancelFunc
	done   chan struct{}

	mu      sync.Mutex
	last    WaitForResult  // latest observation while pending
	final   *WaitForResult // set when resolved
	err     error          // permanent probe failure
	endedAt time.Time
}

type WaitManager struct {
	mu    sync.Mutex
	waits map[string]*Wait
}

func NewWaitManager() *WaitManager {
	return &WaitManager{waits: make(map[string]*Wait)}
}

// Start runs p as a watcher detached from any request.
func (wm *WaitManager) Start(p *waitPlan) (*Wait, error) {
	wm.mu.Lock()
	wm.pruneLocked(time.Now())
	active := 0
	for _, w := range wm.waits {
		if !w.resolved() {
			active++
		}
	}
	if active >= maxActiveWaits {
		wm.mu.Unlock()
		return nil, fmt.Errorf("too many active waits (%d): resume them by wait_id, or stop unneeded ones with "+
			"wait_id + cancel=true", active)
	}
	ctx, cancel := context.WithCancel(context.Background())
	now := time.Now()
	w := &Wait{
		ID:        "wait-" + uuid.New().String()[:8],
		StartedAt: now,
		Deadline:  now.Add(p.timeout),
		cancel:    cancel,
		done:      make(chan struct{}),
		last:      *p.res,
	}
	wm.waits[w.ID] = w
	wm.mu.Unlock()

	go func() {
		res, err := p.run(ctx, w.observe)
		cancel()
		w.mu.Lock()
		w.endedAt = time.Now()
		if err != nil && !errors.Is(err, errWaitCancelled) {
			w.err = err
		} else {
			w.final = res
		}
		w.mu.Unlock()
		close(w.done)
	}()
	return w, nil
}

// Get returns a known wait, or nil.
func (wm *WaitManager) Get(id string) *Wait {
	wm.mu.Lock()
	defer wm.mu.Unlock()
	wm.pruneLocked(time.Now())
	return wm.waits[id]
}

// CancelAll stops every active watcher and returns how many there were.
func (wm *WaitManager) CancelAll() int {
	wm.mu.Lock()
	defer wm.mu.Unlock()
	n := 0
	for _, w := range wm.waits {
		if !w.resolved() {
			w.Cancel()
			n++
		}
	}
	return n
}

// pruneLocked forgets resolved waits older than waitRetention or beyond the
// newest maxKeptWaits. Caller holds wm.mu.
func (wm *WaitManager) pruneLocked(now time.Time) {
	var resolved []*Wait
	for id, w := range wm.waits {
		if !w.resolved() {
			continue
		}
		w.mu.Lock()
		ended := w.endedAt
		w.mu.Unlock()
		if now.Sub(ended) > waitRetention {
			delete(wm.waits, id)
			continue
		}
		resolved = append(resolved, w)
	}
	if len(resolved) <= maxKeptWaits {
		return
	}
	sort.Slice(resolved, func(a, b int) bool { return resolved[a].StartedAt.After(resolved[b].StartedAt) })
	for _, w := range resolved[maxKeptWaits:] {
		delete(wm.waits, w.ID)
	}
}

func (w *Wait) resolved() bool {
	select {
	case <-w.done:
		return true
	default:
		return false
	}
}

func (w *Wait) observe(r WaitForResult) {
	w.mu.Lock()
	w.last = r
	w.mu.Unlock()
}

// Cancel stops the watcher; it then resolves with state=cancelled.
func (w *Wait) Cancel() { w.cancel() }

// Await waits up to d, or until ctx ends, for the watcher to resolve, and
// returns its current state. An error is a permanent probe failure.
func (w *Wait) Await(ctx context.Context, d time.Duration) (*WaitForResult, error) {
	if d > 0 {
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case <-w.done:
		case <-timer.C:
		case <-ctx.Done():
		}
	}
	return w.snapshot()
}

func (w *Wait) snapshot() (*WaitForResult, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return nil, w.err
	}
	var r WaitForResult
	switch {
	case w.final == nil:
		r = w.last
		r.State = WaitPending
		r.WaitedMs = time.Since(w.StartedAt).Milliseconds()
		r.Hint = fmt.Sprintf("not resolved yet; the watcher keeps running server-side until expires_at. "+
			"Call remote_wait_for with wait_id=%q to keep waiting (same watcher: nothing that happens in "+
			"between is missed), or add cancel=true to stop it.", w.ID)
	case w.final.Satisfied:
		r = *w.final
		r.State = WaitSatisfied
	case w.final.TimedOut:
		r = *w.final
		r.State = WaitTimedOut
	default:
		r = *w.final
		r.State = WaitCancelled
	}
	r.WaitID = w.ID
	r.ExpiresAt = w.Deadline.Format(time.RFC3339)
	return &r, nil
}
