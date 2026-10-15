package main

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Detached executions (MCP remote_execute / remote_session_execute)
//
// MCP clients abort a tool call on their own request timeout (60s in the
// TypeScript SDK). In production two `ctrl.sh restart` calls and two waits
// were cut at exactly 60.0s, and the server, which bound commands to the
// request context, killed the commands with the request. The agent was left
// not knowing whether a side-effecting command had run.
//
// Launch runs the command detached from the request instead. A call waits at
// most the call budget (SHELL_API_CALL_BUDGET, below the client timeout): a
// command that finishes in time is answered inline as before; one that does
// not keeps running — only its own timeout_ms, remote_cancel or server
// shutdown stop it — and the call returns state=running with the execution
// id, which remote_execution_result turns into the final result later.
//
// Every execution stays listed for execRetention after it ends, so an agent
// whose reply got lost can still find out what ran and how it exited — that
// happened in production too: a 16s restart answered with status 200 never
// reached the client. Delivered or not, the newest maxRetainedOutputs
// finished executions keep their stdout/stderr, which bounds the memory held.
// ---------------------------------------------------------------------------

const (
	execRetention      = 30 * time.Minute
	maxRetainedExecs   = 200 // finished executions listed (metadata)
	maxRetainedOutputs = 16  // ... of which still hold their stdout/stderr
)

// Execution states reported to MCP clients.
const (
	ExecRunning    = "running"     // still running, NOT killed: fetch the result later
	ExecCompleted  = "completed"   // exited on its own; see exit_code
	ExecTimedOut   = "timed_out"   // killed at timeout_ms
	ExecCancelled  = "cancelled"   // killed by remote_cancel or server shutdown
	ExecNotStarted = "not_started" // refused or failed to start: nothing ran
)

// Execution is a command started by Launch.
type Execution struct {
	ID               string
	Command          string
	WorkingDirectory string
	StartedAt        time.Time
	Timeout          time.Duration

	done chan struct{}

	mu            sync.Mutex
	result        *ExecResult
	endedAt       time.Time
	handedOff     bool // a call returned without the result
	outputDropped bool
	onLate        func(*ExecResult) // reports the outcome of a handed-off execution
}

// ExecutionInfo is the JSON view of an execution without its output.
type ExecutionInfo struct {
	ExecutionID      string `json:"execution_id"`
	State            string `json:"state"`
	Command          string `json:"command"`
	WorkingDirectory string `json:"working_directory,omitempty"`
	StartedAt        string `json:"started_at"`
	ElapsedMs        int64  `json:"elapsed_ms"`
	TimeoutMs        int64  `json:"timeout_ms"`
	ExitCode         *int   `json:"exit_code,omitempty"`
}

// ExecutionPending answers a call that returned before the command finished.
type ExecutionPending struct {
	ExecutionInfo
	Hint string `json:"hint"`
}

// execState classifies a finished execution.
func execState(r *ExecResult) string {
	switch {
	case r.NotStarted:
		return ExecNotStarted
	case r.TimedOut:
		return ExecTimedOut
	case r.Cancelled:
		return ExecCancelled
	}
	return ExecCompleted
}

// Launch starts req detached from any request context and returns at once.
// onLate, if non-nil, receives the result when the execution finishes after
// a caller already returned without it, so the outcome still reaches the
// audit log.
func (e *Executor) Launch(req ExecuteRequest, onLate func(*ExecResult)) *Execution {
	ex := &Execution{
		ID:               uuid.New().String(),
		Command:          req.Command,
		WorkingDirectory: req.WorkingDirectory,
		StartedAt:        time.Now(),
		Timeout:          e.resolveTimeout(req.TimeoutMs),
		done:             make(chan struct{}),
		onLate:           onLate,
	}
	ctx, cancel := context.WithCancel(context.Background())
	// Registered before the goroutine starts, so remote_cancel works at once.
	e.running.Store(ex.ID, cancel)

	e.mu.Lock()
	e.pruneLocked(time.Now())
	e.execs[ex.ID] = ex
	e.mu.Unlock()

	go func() {
		r := e.execute(ctx, ex.ID, req)
		cancel()

		ex.mu.Lock()
		ex.result = r
		ex.endedAt = time.Now()
		late := ex.handedOff
		ex.mu.Unlock()
		close(ex.done)
		if late && ex.onLate != nil {
			ex.onLate(r)
		}
		// Last, so shutdown keeps draining until the outcome is audited.
		e.running.Delete(ex.ID)
	}()
	return ex
}

// Await waits up to d, or until ctx ends, for the execution to finish. It
// returns the result (nil while still running) and whether its stdout/stderr
// have been dropped from the registry.
func (ex *Execution) Await(ctx context.Context, d time.Duration) (*ExecResult, bool) {
	if d > 0 {
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case <-ex.done:
		case <-timer.C:
		case <-ctx.Done():
		}
	}
	return ex.Result()
}

// claim is the launching call's final decision: the result if the execution
// has finished, else nil — and from then on the outcome is reported through
// onLate. Check and hand-off happen under one lock, so exactly one of the
// call and onLate reports the outcome.
func (ex *Execution) claim() *ExecResult {
	ex.mu.Lock()
	defer ex.mu.Unlock()
	if ex.result == nil {
		ex.handedOff = true
	}
	return ex.result
}

// Result returns the final result (nil while running) and whether its
// stdout/stderr were dropped from the registry.
func (ex *Execution) Result() (*ExecResult, bool) {
	ex.mu.Lock()
	defer ex.mu.Unlock()
	return ex.result, ex.outputDropped
}

// dropOutput releases the retained stdout/stderr. It swaps in a copy rather
// than editing the result, which a handler may be encoding concurrently.
func (ex *Execution) dropOutput() {
	ex.mu.Lock()
	defer ex.mu.Unlock()
	if ex.result == nil || ex.outputDropped {
		return
	}
	c := *ex.result
	c.Stdout, c.Stderr = "", ""
	ex.result = &c
	ex.outputDropped = true
}

func (ex *Execution) info() ExecutionInfo {
	ex.mu.Lock()
	defer ex.mu.Unlock()
	out := ExecutionInfo{
		ExecutionID:      ex.ID,
		State:            ExecRunning,
		Command:          truncateString(ex.Command, 500),
		WorkingDirectory: ex.WorkingDirectory,
		StartedAt:        ex.StartedAt.Format(time.RFC3339Nano),
		TimeoutMs:        ex.Timeout.Milliseconds(),
	}
	end := time.Now()
	if ex.result != nil {
		out.State = execState(ex.result)
		code := ex.result.ExitCode
		out.ExitCode = &code
		end = ex.endedAt
	}
	out.ElapsedMs = end.Sub(ex.StartedAt).Milliseconds()
	return out
}

// Lookup returns a known execution, or nil.
func (e *Executor) Lookup(id string) *Execution {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.pruneLocked(time.Now())
	return e.execs[id]
}

// Executions lists the known executions, newest first.
func (e *Executor) Executions() []*Execution {
	e.mu.Lock()
	e.pruneLocked(time.Now())
	out := make([]*Execution, 0, len(e.execs))
	for _, ex := range e.execs {
		out = append(out, ex)
	}
	e.mu.Unlock()
	sort.Slice(out, func(a, b int) bool { return out[a].StartedAt.After(out[b].StartedAt) })
	return out
}

// pruneLocked forgets finished executions older than execRetention or beyond
// the newest maxRetainedExecs, and drops the output of all but the newest
// maxRetainedOutputs. Running executions are never touched. Caller holds e.mu.
func (e *Executor) pruneLocked(now time.Time) {
	var finished []*Execution
	for id, ex := range e.execs {
		ex.mu.Lock()
		ended, over := ex.endedAt, ex.result != nil
		ex.mu.Unlock()
		if !over {
			continue
		}
		if now.Sub(ended) > execRetention {
			delete(e.execs, id)
			continue
		}
		finished = append(finished, ex)
	}
	sort.Slice(finished, func(a, b int) bool { return finished[a].StartedAt.After(finished[b].StartedAt) })
	withOutput := 0
	for i, ex := range finished {
		if i >= maxRetainedExecs {
			delete(e.execs, ex.ID)
			continue
		}
		if _, dropped := ex.Result(); !dropped {
			if withOutput++; withOutput > maxRetainedOutputs {
				ex.dropOutput()
			}
		}
	}
}
