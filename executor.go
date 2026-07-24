package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

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
}

func (e *Executor) Execute(req ExecuteRequest) *ExecResult {
	id := uuid.New().String()

	shell := req.Shell
	if shell == "" {
		shell = e.config.DefaultShell
	}

	timeoutMs := req.TimeoutMs
	if timeoutMs <= 0 {
		timeoutMs = 30000
	}
	maxTimeoutMs := e.config.MaxTimeout * 1000
	if timeoutMs > maxTimeoutMs {
		timeoutMs = maxTimeoutMs
	}

	maxOutput := req.MaxOutputBytes
	if maxOutput <= 0 {
		maxOutput = e.config.MaxOutput
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutMs)*time.Millisecond)
	defer cancel()

	e.running.Store(id, cancel)
	defer e.running.Delete(id)

	cmd := exec.Command(shell, "-c", req.Command)
	setProcAttr(cmd)
	if req.WorkingDirectory != "" {
		cmd.Dir = req.WorkingDirectory
	}
	if req.Environment != nil {
		env := make([]string, 0, len(req.Environment))
		for k, v := range req.Environment {
			env = append(env, fmt.Sprintf("%s=%s", k, v))
		}
		cmd.Env = append(cmd.Environ(), env...)
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	startErr := cmd.Start()
	if startErr != nil {
		return &ExecResult{
			ID:               id,
			Command:          req.Command,
			ExitCode:         -1,
			DurationMs:       time.Since(start).Milliseconds(),
			StartedAt:        start,
			CompletedAt:      time.Now(),
			WorkingDirectory: req.WorkingDirectory,
		}
	}

	waitCh := make(chan error, 1)
	go func() {
		waitCh <- cmd.Wait()
	}()

	var err error
	timedOut := false
	select {
	case err = <-waitCh:
	case <-ctx.Done():
		timedOut = ctx.Err() == context.DeadlineExceeded
		killProcessGroup(cmd.Process.Pid)
		err = <-waitCh
	}

	duration := time.Since(start)

	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = -1
		}
	}

	keepTail := strings.EqualFold(req.TruncateMode, "tail")
	stdoutStr := stdout.String()
	stderrStr := stderr.String()
	stdoutTruncated := false
	stderrTruncated := false

	if len(stdoutStr) > maxOutput {
		stdoutStr = truncateToLimit(stdoutStr, maxOutput, keepTail)
		stdoutTruncated = true
	}
	if len(stderrStr) > maxOutput {
		stderrStr = truncateToLimit(stderrStr, maxOutput, keepTail)
		stderrTruncated = true
	}

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
		StdoutTruncated:  stdoutTruncated,
		StderrTruncated:  stderrTruncated,
	}
}

type StreamCallback func(event StreamEvent)

func (e *Executor) ExecuteStream(req ExecuteRequest, callback StreamCallback) {
	id := uuid.New().String()

	shell := req.Shell
	if shell == "" {
		shell = e.config.DefaultShell
	}

	timeoutMs := req.TimeoutMs
	if timeoutMs <= 0 {
		timeoutMs = 30000
	}
	maxTimeoutMs := e.config.MaxTimeout * 1000
	if timeoutMs > maxTimeoutMs {
		timeoutMs = maxTimeoutMs
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutMs)*time.Millisecond)
	defer cancel()

	e.running.Store(id, cancel)
	defer e.running.Delete(id)

	cmd := exec.Command(shell, "-c", req.Command)
	setProcAttr(cmd)
	if req.WorkingDirectory != "" {
		cmd.Dir = req.WorkingDirectory
	}
	if req.Environment != nil {
		env := make([]string, 0, len(req.Environment))
		for k, v := range req.Environment {
			env = append(env, fmt.Sprintf("%s=%s", k, v))
		}
		cmd.Env = append(cmd.Environ(), env...)
	}

	stdoutPipe, _ := cmd.StdoutPipe()
	stderrPipe, _ := cmd.StderrPipe()

	start := time.Now()
	if err := cmd.Start(); err != nil {
		callback(StreamEvent{
			Type:      "error",
			Line:      err.Error(),
			Timestamp: time.Now().Format(time.RFC3339Nano),
		})
		return
	}

	go func() {
		<-ctx.Done()
		killProcessGroup(cmd.Process.Pid)
	}()

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

	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = -1
		}
	}

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

// truncateToLimit keeps the first (head) or last (tail) max bytes of s.
// Cuts are nudged to a rune boundary so UTF-8 output stays printable.
func truncateToLimit(s string, max int, tail bool) string {
	if len(s) <= max {
		return s
	}
	if !tail {
		out := s[:max]
		// Do not end mid-rune: back off to the last full rune.
		for len(out) > 0 && !utf8.ValidString(out) {
			out = out[:len(out)-1]
		}
		return out
	}
	out := s[len(s)-max:]
	// Do not start mid-rune: drop the leading partial bytes.
	for len(out) > 0 && !utf8.ValidString(out) {
		out = out[1:]
	}
	return out
}

func (e *Executor) ExecuteInDir(shell, command, dir string, env []string, timeoutMs int) *ExecResult {
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
	return e.Execute(req)
}
