package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// execWaitDelay bounds how long Wait lingers after the shell itself has exited
// while some descendant still holds the stdout/stderr pipes open — the classic
// `nohup x > log 2>&1 & sleep 3; ps ...` pattern, where a forked subshell keeps
// the pipe alive. Without it the call blocked until the timeout and the whole
// process group (including the job meant to stay in the background) was
// SIGKILLed; production logs showed this as timed_out=true with exit_code=0.
const execWaitDelay = 2 * time.Second

type Executor struct {
	config  *Config
	running sync.Map // map[string]context.CancelFunc
}

func NewExecutor(cfg *Config) *Executor {
	return &Executor{config: cfg}
}

type ExecResult struct {
	ID               string
	Command          string
	ExitCode         int
	Stdout           string
	Stderr           string
	DurationMs       int64
	StartedAt        time.Time
	CompletedAt      time.Time
	WorkingDirectory string
	TimedOut         bool
	StdoutTruncated  bool
	StderrTruncated  bool
	StdoutTotalBytes int64
	StderrTotalBytes int64
	OutputEncoding   string
	// Error explains why the command did not run or did not finish normally
	// (denied, bad working directory, start failure, cancelled).
	Error string
	// NotStarted is true when no process was spawned at all (refused by
	// policy / read-only mode, bad working directory, start failure).
	NotStarted bool
}

// checkCommand applies the operator deny policy (SHELL_API_DENY_COMMANDS /
// SHELL_API_BLOCK_JUMP_HOST) to a shell command line.
func (c *Config) checkCommand(command string) error {
	if c == nil || c.DenyCommand == nil {
		return nil
	}
	loc := c.DenyCommand.FindStringIndex(command)
	if loc == nil {
		return nil
	}
	m := strings.TrimSpace(strings.TrimLeft(command[loc[0]:loc[1]], ";&|()`$\n "))
	msg := fmt.Sprintf("command denied by server policy (matched %q)", m)
	if strings.Contains(m, "ssh") || strings.Contains(m, "scp") || strings.Contains(m, "sftp") {
		msg += ": hopping from this server to other hosts is blocked because commands run there " +
			"escape this server's audit log. Use the target host's own remote shell server, " +
			"or report the exact command for a human to run"
	}
	return errors.New(msg)
}

// checkWorkingDir verifies dir exists on THIS host before spawning, so the
// caller gets a precise error instead of a bare exit_code=-1. Agents regularly
// sent their local (client-side) paths here.
func checkWorkingDir(dir string) error {
	if dir == "" {
		return nil
	}
	fi, err := os.Stat(dir)
	if err != nil {
		host, _ := os.Hostname()
		return fmt.Errorf("working_directory %q does not exist on remote host %s "+
			"(paths are resolved on the server, not on the client machine): %v", dir, host, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("working_directory %q is not a directory", dir)
	}
	return nil
}

func (e *Executor) resolveShell(shell string) string {
	if shell == "" {
		return e.config.DefaultShell
	}
	return shell
}

func (e *Executor) resolveTimeout(timeoutMs int) time.Duration {
	if timeoutMs <= 0 {
		timeoutMs = 30000
	}
	if maxMs := e.config.MaxTimeout * 1000; timeoutMs > maxMs {
		timeoutMs = maxMs
	}
	return time.Duration(timeoutMs) * time.Millisecond
}

// newCommand builds a shell command bound to ctx. When ctx ends (timeout,
// remote_cancel, client disconnect, shutdown) the whole process group is
// killed; WaitDelay keeps Wait from hanging on pipes held by stray children.
func newCommand(ctx context.Context, shell, command, dir string, env map[string]string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, shell, "-c", command)
	setProcAttr(cmd)
	cmd.Cancel = func() error {
		killProcessGroup(cmd.Process.Pid)
		return nil
	}
	cmd.WaitDelay = execWaitDelay
	if dir != "" {
		cmd.Dir = dir
	}
	if env != nil {
		extra := make([]string, 0, len(env))
		for k, v := range env {
			extra = append(extra, fmt.Sprintf("%s=%s", k, v))
		}
		cmd.Env = append(cmd.Environ(), extra...)
	}
	return cmd
}

// Execute runs req.Command and waits for it. ctx is the caller's request
// context: when the client goes away the command is killed instead of running
// on to its timeout unobserved.
func (e *Executor) Execute(ctx context.Context, req ExecuteRequest) *ExecResult {
	id := uuid.New().String()
	now := time.Now()
	fail := func(msg string) *ExecResult {
		return &ExecResult{
			ID:               id,
			Command:          req.Command,
			ExitCode:         -1,
			Stderr:           msg,
			Error:            msg,
			NotStarted:       true,
			StartedAt:        now,
			CompletedAt:      time.Now(),
			WorkingDirectory: req.WorkingDirectory,
		}
	}

	// Layer-4 guard: shell execution is a write capability (a command can do
	// anything), so it is refused outright under read-only mode.
	if err := guardReadOnly("shell execution"); err != nil {
		return fail(err.Error())
	}
	if err := e.config.checkCommand(req.Command); err != nil {
		return fail(err.Error())
	}
	if err := checkWorkingDir(req.WorkingDirectory); err != nil {
		return fail(err.Error())
	}
	encoding, err := normalizeOutputEncoding(req.OutputEncoding)
	if err != nil {
		return fail(err.Error())
	}
	if ctx == nil {
		ctx = context.Background()
	}

	maxOutput := req.MaxOutputBytes
	if maxOutput <= 0 {
		maxOutput = e.config.MaxOutput
	}
	if maxOutput <= 0 {
		maxOutput = 1
	}
	keepTail := strings.EqualFold(req.TruncateMode, "tail")

	ctx, cancel := context.WithTimeout(ctx, e.resolveTimeout(req.TimeoutMs))
	defer cancel()

	e.running.Store(id, cancel)
	defer e.running.Delete(id)

	cmd := newCommand(ctx, e.resolveShell(req.Shell), req.Command, req.WorkingDirectory, req.Environment)

	// Bounded capture: memory stays O(max_output) no matter how much the
	// command prints. A few spare bytes let truncateToLimit land on a rune
	// boundary.
	stdout := newCappedBuffer(maxOutput+utf8.UTFMax-1, keepTail)
	stderr := newCappedBuffer(maxOutput+utf8.UTFMax-1, keepTail)
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	start := time.Now()
	if err := cmd.Start(); err != nil {
		res := fail("failed to start command: " + err.Error())
		res.StartedAt = start
		res.DurationMs = time.Since(start).Milliseconds()
		return res
	}
	waitErr := cmd.Wait()
	duration := time.Since(start)

	exitCode, timedOut, errMsg := classifyWait(ctx, cmd, waitErr)

	stdoutStr, stdoutTrunc, enc := finishOutput(stdout, maxOutput, keepTail, encoding)
	stderrStr, stderrTrunc, _ := finishOutput(stderr, maxOutput, keepTail, encoding)

	return &ExecResult{
		ID:               id,
		Command:          req.Command,
		ExitCode:         exitCode,
		Stdout:           stdoutStr,
		Stderr:           stderrStr,
		DurationMs:       duration.Milliseconds(),
		StartedAt:        start,
		CompletedAt:      time.Now(),
		WorkingDirectory: req.WorkingDirectory,
		TimedOut:         timedOut,
		StdoutTruncated:  stdoutTrunc,
		StderrTruncated:  stderrTrunc,
		StdoutTotalBytes: stdout.total,
		StderrTotalBytes: stderr.total,
		OutputEncoding:   enc,
		Error:            errMsg,
	}
}

// classifyWait turns Wait's error into (exit code, timed out, error note).
func classifyWait(ctx context.Context, cmd *exec.Cmd, waitErr error) (int, bool, string) {
	exitCode := 0
	switch {
	case waitErr == nil:
	case errors.Is(waitErr, exec.ErrWaitDelay):
		// The shell exited successfully; only a background descendant kept
		// the output pipes open. That is not a failure of the command.
		if cmd.ProcessState != nil {
			exitCode = cmd.ProcessState.ExitCode()
		}
	default:
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = -1
		}
	}

	if waitErr == nil || errors.Is(waitErr, exec.ErrWaitDelay) {
		return exitCode, false, ""
	}
	switch ctx.Err() {
	case context.DeadlineExceeded:
		return exitCode, true, ""
	case context.Canceled:
		return exitCode, false, "execution cancelled (remote_cancel, client disconnect or server shutdown)"
	}
	return exitCode, false, ""
}

type StreamCallback func(event StreamEvent)

func (e *Executor) ExecuteStream(ctx context.Context, req ExecuteRequest, callback StreamCallback) {
	id := uuid.New().String()

	failEvents := func(msg string) {
		callback(StreamEvent{
			Type:      "error",
			Line:      msg,
			Timestamp: time.Now().Format(time.RFC3339Nano),
		})
		callback(StreamEvent{
			Type:      "exit",
			ExitCode:  -1,
			Timestamp: time.Now().Format(time.RFC3339Nano),
		})
	}

	if err := guardReadOnly("shell execution (stream)"); err != nil {
		failEvents(err.Error())
		return
	}
	if err := e.config.checkCommand(req.Command); err != nil {
		failEvents(err.Error())
		return
	}
	if err := checkWorkingDir(req.WorkingDirectory); err != nil {
		failEvents(err.Error())
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}

	ctx, cancel := context.WithTimeout(ctx, e.resolveTimeout(req.TimeoutMs))
	defer cancel()

	e.running.Store(id, cancel)
	defer e.running.Delete(id)

	cmd := newCommand(ctx, e.resolveShell(req.Shell), req.Command, req.WorkingDirectory, req.Environment)

	stdoutPipe, _ := cmd.StdoutPipe()
	stderrPipe, _ := cmd.StderrPipe()

	start := time.Now()
	if err := cmd.Start(); err != nil {
		failEvents("failed to start command: " + err.Error())
		return
	}

	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(stdoutPipe)
		for scanner.Scan() {
			callback(StreamEvent{
				Type:      "stdout",
				Line:      scanner.Text(),
				Timestamp: time.Now().Format(time.RFC3339Nano),
			})
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(stderrPipe)
		for scanner.Scan() {
			callback(StreamEvent{
				Type:      "stderr",
				Line:      scanner.Text(),
				Timestamp: time.Now().Format(time.RFC3339Nano),
			})
		}
	}()

	wg.Wait()
	err := cmd.Wait()
	exitCode, _, _ := classifyWait(ctx, cmd, err)

	callback(StreamEvent{
		Type:      "exit",
		ExitCode:  exitCode,
		Duration:  time.Since(start).Milliseconds(),
		Timestamp: time.Now().Format(time.RFC3339Nano),
	})
}

func (e *Executor) Cancel(id string) bool {
	if cancel, ok := e.running.Load(id); ok {
		cancel.(context.CancelFunc)()
		return true
	}
	return false
}

// CancelAll cancels every running execution (used by graceful shutdown) and
// returns how many were cancelled.
func (e *Executor) CancelAll() int {
	n := 0
	e.running.Range(func(_, v any) bool {
		v.(context.CancelFunc)()
		n++
		return true
	})
	return n
}

// Running reports the number of in-flight executions.
func (e *Executor) Running() int {
	n := 0
	e.running.Range(func(_, _ any) bool {
		n++
		return true
	})
	return n
}

// truncateToLimit keeps the first (head) or last (tail) max bytes of s,
// nudging the cut to a rune boundary. At most UTFMax-1 bytes are examined, so
// invalid (e.g. GBK) bytes elsewhere in s neither shrink nor slow the cut.
// (An earlier version re-validated the whole string byte by byte: O(n²), and a
// single GBK byte collapsed the output to the prefix before it — often "".)
func truncateToLimit(s string, max int, tail bool) string {
	if len(s) <= max {
		return s
	}
	if max <= 0 {
		return ""
	}
	if !tail {
		cut := max // s[cut] is the first dropped byte
		for i := 0; i < utf8.UTFMax-1 && cut > 0 && !utf8.RuneStart(s[cut]); i++ {
			cut--
		}
		return s[:cut]
	}
	start := len(s) - max
	for i := 0; i < utf8.UTFMax-1 && start < len(s) && !utf8.RuneStart(s[start]); i++ {
		start++
	}
	return s[start:]
}

func (e *Executor) ExecuteInDir(ctx context.Context, shell, command, dir string, env []string, timeoutMs int) *ExecResult {
	req := ExecuteRequest{
		Command:          command,
		Shell:            shell,
		WorkingDirectory: dir,
		TimeoutMs:        timeoutMs,
	}
	if len(env) > 0 {
		req.Environment = make(map[string]string)
		for _, e := range env {
			parts := strings.SplitN(e, "=", 2)
			if len(parts) == 2 {
				req.Environment[parts[0]] = parts[1]
			}
		}
	}
	return e.Execute(ctx, req)
}
