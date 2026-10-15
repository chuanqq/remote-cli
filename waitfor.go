package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// remote_wait_for: block until a condition holds, then return immediately.
//
// Production logs showed agents waiting with `sleep N; check` — 436 such
// commands, 73% of all execution time, 96 of them killed by the 30s default
// timeout. This tool polls server-side and returns as soon as the condition
// is met (or the deadline passes), together with what matched.
// ---------------------------------------------------------------------------

const (
	defaultWaitTimeoutSec  = 60
	maxWaitTimeoutSec      = 24 * 3600 // a wait outlives calls: see waits.go
	defaultWaitInterval    = time.Second
	minWaitInterval        = 200 * time.Millisecond
	minCommandWaitInterval = time.Second
	maxWaitScanBytes       = 64 << 20 // one scan reads at most this much
	maxWaitLineBytes       = 1 << 20  // longer lines are matched on their head
	maxWaitMatchLen        = 2000
	waitProbeTimeoutMs     = 10000 // per-probe timeout for command_exit0
)

// Supported conditions.
const (
	WaitFileExists   = "file_exists"   // target = path
	WaitFileContains = "file_contains" // target = path, pattern = regex (whole file)
	WaitLogRegex     = "log_regex"     // target = path, pattern = regex (content appended after the call started)
	WaitProcessExit  = "process_exit"  // target = pid or job id
	WaitPortListen   = "port_listen"   // target = port number
	WaitCommandExit0 = "command_exit0" // target = shell command, polled until it exits 0
)

type WaitForRequest struct {
	Condition  string
	Target     string
	Pattern    string
	TimeoutSec int
	IntervalMs int
	Encoding   string // file conditions: source encoding of the file
	IgnoreCase bool
}

type WaitForResult struct {
	Condition string `json:"condition"`
	Target    string `json:"target"`
	Satisfied bool   `json:"satisfied"`
	TimedOut  bool   `json:"timed_out"`
	WaitedMs  int64  `json:"waited_ms"`
	Checks    int    `json:"checks"`
	// Detail describes what satisfied the condition (matched line, exit code,
	// listener, ...) or the last observed state when it timed out.
	Detail string `json:"detail,omitempty"`
	// Output is the last probe's stdout+stderr for command_exit0.
	Output string `json:"output,omitempty"`

	// Set by the MCP tool (waits.go): the server-side watcher id, its state
	// (satisfied / timed_out / pending / cancelled), when it gives up, and
	// what to do next.
	WaitID    string `json:"wait_id,omitempty"`
	State     string `json:"state,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
	Hint      string `json:"hint,omitempty"`
}

type waitDeps struct {
	roots    []string
	executor *Executor
	jobs     *JobManager
}

// probe is one condition check: (satisfied, detail, error). A non-nil error
// is permanent (bad target) and aborts the wait.
type probe func(ctx context.Context) (bool, string, error)

// errWaitCancelled ends a wait stopped by its caller before it resolved.
var errWaitCancelled = errors.New("wait cancelled")

// waitPlan is a validated wait, ready to run.
type waitPlan struct {
	check    probe
	res      *WaitForResult
	timeout  time.Duration
	interval time.Duration
}

// planWait validates req and builds its probe. Errors (bad target, pattern,
// sandbox) are reported here, before anything waits.
func planWait(req WaitForRequest, deps waitDeps) (*waitPlan, error) {
	timeout := req.TimeoutSec
	if timeout <= 0 {
		timeout = defaultWaitTimeoutSec
	}
	if timeout > maxWaitTimeoutSec {
		timeout = maxWaitTimeoutSec
	}
	interval := defaultWaitInterval
	if req.IntervalMs > 0 {
		interval = time.Duration(req.IntervalMs) * time.Millisecond
	}
	if interval < minWaitInterval {
		interval = minWaitInterval
	}
	if req.Condition == WaitCommandExit0 && interval < minCommandWaitInterval {
		// Each check spawns a shell, and a watcher may now run for hours.
		interval = minCommandWaitInterval
	}
	if strings.TrimSpace(req.Target) == "" {
		return nil, errors.New("target is required")
	}

	res := &WaitForResult{Condition: req.Condition, Target: req.Target}
	check, err := buildProbe(req, deps, res)
	if err != nil {
		return nil, err
	}
	return &waitPlan{check: check, res: res, timeout: time.Duration(timeout) * time.Second, interval: interval}, nil
}

// WaitFor blocks until the condition holds, the timeout passes or ctx ends.
func WaitFor(ctx context.Context, req WaitForRequest, deps waitDeps) (*WaitForResult, error) {
	p, err := planWait(req, deps)
	if err != nil {
		return nil, err
	}
	return p.run(ctx, nil)
}

// run polls until the condition holds or the timeout passes. observe, if
// set, receives a snapshot after every check. A ctx cancelled by the caller
// returns the last state with errWaitCancelled.
func (p *waitPlan) run(ctx context.Context, observe func(WaitForResult)) (*WaitForResult, error) {
	res := p.res
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		ok, detail, err := p.check(ctx)
		res.Checks++
		res.Detail = detail
		res.WaitedMs = time.Since(start).Milliseconds()
		if err != nil {
			return nil, err
		}
		if ok {
			res.Satisfied = true
			return res, nil
		}
		if observe != nil {
			observe(*res)
		}
		select {
		case <-ctx.Done():
			res.WaitedMs = time.Since(start).Milliseconds()
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				res.TimedOut = true
				return res, nil
			}
			return res, errWaitCancelled
		case <-ticker.C:
		}
	}
}

func compileWaitPattern(pattern string, ignoreCase bool) (*regexp.Regexp, error) {
	if pattern == "" {
		return nil, errors.New("pattern is required for this condition")
	}
	if ignoreCase {
		pattern = "(?i)" + pattern
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("invalid pattern: %w", err)
	}
	return re, nil
}

func buildProbe(req WaitForRequest, deps waitDeps, res *WaitForResult) (probe, error) {
	switch req.Condition {
	case WaitFileExists:
		abs, err := validatePath(req.Target, deps.roots)
		if err != nil {
			return nil, err
		}
		return func(context.Context) (bool, string, error) {
			fi, err := os.Stat(abs)
			if err != nil {
				return false, "not present", nil
			}
			return true, fmt.Sprintf("exists, size=%d, mtime=%s", fi.Size(), fi.ModTime().Format(time.RFC3339)), nil
		}, nil

	case WaitFileContains:
		abs, err := validatePath(req.Target, deps.roots)
		if err != nil {
			return nil, err
		}
		re, err := compileWaitPattern(req.Pattern, req.IgnoreCase)
		if err != nil {
			return nil, err
		}
		// A watcher may poll for hours: skip the rescan while the file has
		// provably not changed since the last one — same inode, size and
		// mtime, where the mtime was already 2s old when that scan ran, so a
		// later write cannot share it even on filesystems with coarse mtimes.
		var scanned os.FileInfo
		var scannedAt time.Time
		return func(context.Context) (bool, string, error) {
			fi, err := os.Stat(abs)
			if err != nil {
				return false, "not present", nil
			}
			if scanned != nil && os.SameFile(fi, scanned) && fi.Size() == scanned.Size() &&
				fi.ModTime().Equal(scanned.ModTime()) && scannedAt.Sub(fi.ModTime()) > 2*time.Second {
				return false, "no match yet (file unchanged)", nil
			}
			at := time.Now()
			line, n, found, _, err := scanFileForMatch(abs, 0, re, req.Encoding)
			if err != nil {
				return false, err.Error(), nil
			}
			if found {
				return true, fmt.Sprintf("line %d: %s", n, line), nil
			}
			scanned, scannedAt = fi, at
			return false, "no match yet", nil
		}, nil

	case WaitLogRegex:
		abs, err := validatePath(req.Target, deps.roots)
		if err != nil {
			return nil, err
		}
		re, err := compileWaitPattern(req.Pattern, req.IgnoreCase)
		if err != nil {
			return nil, err
		}
		// Only content appended after the wait started counts. The cursor
		// advances over every complete line scanned, so each appended byte is
		// read once however long the watcher runs (an earlier version
		// rescanned from the start offset every check, capped at 64MB: a log
		// growing faster than that during a long wait hid later matches).
		var offset int64
		var seen os.FileInfo
		if fi, err := os.Stat(abs); err == nil {
			offset, seen = fi.Size(), fi
		}
		return func(context.Context) (bool, string, error) {
			fi, err := os.Stat(abs)
			if err != nil {
				return false, "log not present", nil
			}
			if fi.Size() < offset || (seen != nil && !os.SameFile(fi, seen)) {
				offset = 0 // truncated, or rotated to a new file: start over
			}
			seen = fi
			line, _, found, consumed, err := scanFileForMatch(abs, offset, re, req.Encoding)
			if err != nil {
				return false, err.Error(), nil
			}
			if found {
				return true, line, nil
			}
			offset += consumed
			return false, fmt.Sprintf("no new matching line (scanned up to offset %d)", offset), nil
		}, nil

	case WaitProcessExit:
		if deps.jobs != nil {
			if job := deps.jobs.Get(req.Target); job != nil {
				return func(context.Context) (bool, string, error) {
					info := job.info()
					if info.State == JobRunning {
						return false, fmt.Sprintf("job %s running (pid %d)", job.ID, job.PID), nil
					}
					return true, fmt.Sprintf("job %s %s, exit_code=%d", job.ID, info.State, *info.ExitCode), nil
				}, nil
			}
		}
		pid, err := strconv.Atoi(strings.TrimSpace(req.Target))
		if err != nil || pid <= 0 {
			return nil, fmt.Errorf("target must be a pid or a job id, got %q", req.Target)
		}
		return func(context.Context) (bool, string, error) {
			alive, err := pidAlive(pid)
			if err != nil {
				return false, err.Error(), nil
			}
			if alive {
				return false, fmt.Sprintf("pid %d alive", pid), nil
			}
			return true, fmt.Sprintf("pid %d exited", pid), nil
		}, nil

	case WaitPortListen:
		port, err := strconv.Atoi(strings.TrimSpace(req.Target))
		if err != nil || port <= 0 || port > 65535 {
			return nil, fmt.Errorf("target must be a port number, got %q", req.Target)
		}
		return func(context.Context) (bool, string, error) {
			r, err := CheckPort(CheckPortRequest{Port: port})
			if err != nil {
				return false, "", err
			}
			if r.Count == 0 {
				return false, fmt.Sprintf("port %d not listening", port), nil
			}
			l := r.Listeners[0]
			return true, fmt.Sprintf("%s %s:%d pid=%d %s", l.Proto, l.Address, l.Port, l.PID, l.Process), nil
		}, nil

	case WaitCommandExit0:
		if deps.executor == nil {
			return nil, errors.New("command_exit0 is not available on this server")
		}
		return func(ctx context.Context) (bool, string, error) {
			r := deps.executor.Execute(ctx, ExecuteRequest{
				Command:        req.Target,
				TimeoutMs:      waitProbeTimeoutMs,
				MaxOutputBytes: 8192,
				TruncateMode:   "tail",
			})
			res.Output = strings.TrimSpace(r.Stdout + r.Stderr)
			if r.NotStarted {
				// Never ran (denied / read-only / bad shell): permanent.
				return false, "", errors.New(r.Error)
			}
			if r.ExitCode == 0 && !r.TimedOut {
				return true, "exit_code=0", nil
			}
			return false, fmt.Sprintf("exit_code=%d timed_out=%v", r.ExitCode, r.TimedOut), nil
		}, nil
	}
	return nil, fmt.Errorf("unknown condition %q (want %s, %s, %s, %s, %s or %s)", req.Condition,
		WaitFileExists, WaitFileContains, WaitLogRegex, WaitProcessExit, WaitPortListen, WaitCommandExit0)
}

// scanFileForMatch scans path from offset line by line. It returns the first
// line matching re (truncated for display), its 1-based number relative to
// offset, whether one was found, and how many bytes of complete lines it
// scanned without a match: the caller's next offset, so successive scans of a
// growing file read each byte once, and a half-written last line is read
// again once complete. One call reads at most maxWaitScanBytes; lines longer
// than maxWaitLineBytes are matched on their first maxWaitLineBytes.
func scanFileForMatch(path string, offset int64, re *regexp.Regexp, encoding string) (string, int, bool, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, false, 0, err
	}
	defer f.Close()
	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return "", 0, false, 0, err
		}
	}
	br := bufio.NewReaderSize(io.LimitReader(f, maxWaitScanBytes), maxWaitLineBytes)
	var consumed, total int64
	var head []byte   // first maxWaitLineBytes of the current line
	var lineLen int64 // full length of the current line so far
	n := 0
	for {
		chunk, err := br.ReadSlice('\n')
		if room := maxWaitLineBytes - len(head); room > 0 {
			head = append(head, chunk[:min(len(chunk), room)]...)
		}
		lineLen += int64(len(chunk))
		total += int64(len(chunk))
		if errors.Is(err, bufio.ErrBufferFull) {
			continue // over-long line: consume the rest of it
		}
		if lineLen > 0 {
			n++
			line := strings.TrimRight(string(head), "\r\n")
			if encoding != "" {
				if dec, decErr := decodeToUTF8([]byte(line), encoding); decErr == nil {
					line = dec
				}
			}
			if re.MatchString(line) {
				return truncateString(line, maxWaitMatchLen), n, true, consumed + lineLen, nil
			}
			if err == nil { // ReadSlice found the newline: the line is complete
				consumed += lineLen
			}
		}
		head, lineLen = head[:0], 0
		if err != nil {
			if !errors.Is(err, io.EOF) {
				return "", n, false, consumed, err
			}
			if consumed == 0 && total >= maxWaitScanBytes {
				consumed = total // one line fills the whole read cap: skip it
			}
			return "", n, false, consumed, nil
		}
	}
}
