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
	defaultWaitTimeoutSec = 60
	maxWaitTimeoutSec     = 300
	defaultWaitInterval   = time.Second
	minWaitInterval       = 200 * time.Millisecond
	maxWaitScanBytes      = 64 << 20 // file_contains scans at most this much
	maxWaitMatchLen       = 2000
	waitProbeTimeoutMs    = 10000 // per-probe timeout for command_exit0
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
}

type waitDeps struct {
	roots    []string
	executor *Executor
	jobs     *JobManager
}

// probe is one condition check: (satisfied, detail, error). A non-nil error
// is permanent (bad target) and aborts the wait.
type probe func(ctx context.Context) (bool, string, error)

func WaitFor(ctx context.Context, req WaitForRequest, deps waitDeps) (*WaitForResult, error) {
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
	if strings.TrimSpace(req.Target) == "" {
		return nil, errors.New("target is required")
	}

	res := &WaitForResult{Condition: req.Condition, Target: req.Target}
	check, err := buildProbe(req, deps, res)
	if err != nil {
		return nil, err
	}

	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		ok, detail, err := check(ctx)
		res.Checks++
		res.Detail = detail
		if err != nil {
			return nil, err
		}
		if ok {
			res.Satisfied = true
			res.WaitedMs = time.Since(start).Milliseconds()
			return res, nil
		}
		select {
		case <-ctx.Done():
			res.WaitedMs = time.Since(start).Milliseconds()
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				res.TimedOut = true
				return res, nil
			}
			return res, errors.New("wait cancelled")
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
		return func(context.Context) (bool, string, error) {
			line, n, found, err := scanFileForMatch(abs, 0, re, req.Encoding)
			if err != nil {
				return false, err.Error(), nil
			}
			if found {
				return true, fmt.Sprintf("line %d: %s", n, line), nil
			}
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
		// Only content appended after the call started counts.
		var offset int64
		if fi, err := os.Stat(abs); err == nil {
			offset = fi.Size()
		}
		return func(context.Context) (bool, string, error) {
			fi, err := os.Stat(abs)
			if err != nil {
				return false, "log not present", nil
			}
			if fi.Size() < offset {
				offset = 0 // rotated / truncated: start over
			}
			line, _, found, err := scanFileForMatch(abs, offset, re, req.Encoding)
			if err != nil {
				return false, err.Error(), nil
			}
			if found {
				return true, line, nil
			}
			return false, fmt.Sprintf("no new matching line (scanned from offset %d)", offset), nil
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

// scanFileForMatch scans path from offset line by line and returns the first
// line matching re, its 1-based line number relative to offset, and whether
// one was found. At most maxWaitScanBytes are read.
func scanFileForMatch(path string, offset int64, re *regexp.Regexp, encoding string) (string, int, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, false, err
	}
	defer f.Close()
	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return "", 0, false, err
		}
	}
	sc := bufio.NewScanner(io.LimitReader(f, maxWaitScanBytes))
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	n := 0
	for sc.Scan() {
		n++
		line := sc.Text()
		if encoding != "" {
			if dec, err := decodeToUTF8([]byte(line), encoding); err == nil {
				line = dec
			}
		}
		if re.MatchString(line) {
			return truncateString(line, maxWaitMatchLen), n, true, nil
		}
	}
	return "", n, false, sc.Err()
}
