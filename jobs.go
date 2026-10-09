package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Background jobs (remote_spawn + remote_job_*)
//
// remote_execute is synchronous: a command meant to keep running after the
// call returns (`nohup x &`) either blocked the call until its timeout or was
// SIGKILLed with the shell's process group. A job instead is:
//
//   - started in its own session (setsid), so it is not in the caller's
//     process group and survives the request, and even a server restart;
//   - writing stdout+stderr straight into a server-managed log file (a real
//     file, not a pipe, so nothing in this process waits on it);
//   - addressable by a job id that the caller gets back immediately, for
//     status, log reading (with follow) and kill.
//
// Job metadata is in memory only. Finished jobs are pruned after jobRetention,
// together with their log files.
// ---------------------------------------------------------------------------

const (
	jobRetention     = 24 * time.Hour
	maxFinishedJobs  = 500
	jobSweepInterval = 10 * time.Minute
)

type JobState string

const (
	JobRunning JobState = "running"
	JobExited  JobState = "exited"
	JobKilled  JobState = "killed"
	JobTimeout JobState = "timed_out"
)

type Job struct {
	ID               string
	Command          string
	WorkingDirectory string
	PID              int
	LogPath          string
	StartedAt        time.Time

	mu       sync.Mutex
	state    JobState
	exitCode int
	endedAt  time.Time
	killed   bool
	timedOut bool
	done     chan struct{}
}

// JobInfo is the JSON view of a job.
type JobInfo struct {
	JobID            string   `json:"job_id"`
	PID              int      `json:"pid"`
	State            JobState `json:"state"`
	Command          string   `json:"command"`
	WorkingDirectory string   `json:"working_directory,omitempty"`
	ExitCode         *int     `json:"exit_code,omitempty"`
	StartedAt        string   `json:"started_at"`
	EndedAt          string   `json:"ended_at,omitempty"`
	ElapsedMs        int64    `json:"elapsed_ms"`
	LogPath          string   `json:"log_path"`
	LogSize          int64    `json:"log_size"`
}

func (j *Job) info() JobInfo {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := JobInfo{
		JobID:            j.ID,
		PID:              j.PID,
		State:            j.state,
		Command:          j.Command,
		WorkingDirectory: j.WorkingDirectory,
		StartedAt:        j.StartedAt.Format(time.RFC3339Nano),
		LogPath:          j.LogPath,
	}
	end := time.Now()
	if j.state != JobRunning {
		code := j.exitCode
		out.ExitCode = &code
		out.EndedAt = j.endedAt.Format(time.RFC3339Nano)
		end = j.endedAt
	}
	out.ElapsedMs = end.Sub(j.StartedAt).Milliseconds()
	if fi, err := os.Stat(j.LogPath); err == nil {
		out.LogSize = fi.Size()
	}
	return out
}

func (j *Job) running() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.state == JobRunning
}

// Done is closed when the job's process has exited.
func (j *Job) Done() <-chan struct{} { return j.done }

type SpawnRequest struct {
	Command          string
	WorkingDirectory string
	Environment      map[string]string
	Shell            string
	TimeoutSeconds   int // 0 = no limit
}

type JobManager struct {
	cfg  *Config
	mu   sync.Mutex
	jobs map[string]*Job
}

func NewJobManager(cfg *Config) *JobManager {
	jm := &JobManager{cfg: cfg, jobs: make(map[string]*Job)}
	go jm.sweepLoop()
	return jm
}

// Spawn starts req.Command as a detached background job and returns at once.
func (jm *JobManager) Spawn(req SpawnRequest) (*Job, error) {
	if err := guardReadOnly("spawn background job"); err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.Command) == "" {
		return nil, errors.New("command is required")
	}
	if err := jm.cfg.checkCommand(req.Command); err != nil {
		return nil, err
	}
	if err := checkWorkingDir(req.WorkingDirectory); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(jm.cfg.JobDir, 0o700); err != nil {
		return nil, fmt.Errorf("create job dir %s: %w", jm.cfg.JobDir, err)
	}

	id := "job-" + uuid.New().String()[:8]
	logPath := filepath.Join(jm.cfg.JobDir, id+".log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create job log: %w", err)
	}
	defer logFile.Close() // the child holds its own descriptor

	shell := req.Shell
	if shell == "" {
		shell = jm.cfg.DefaultShell
	}
	cmd := exec.Command(shell, "-c", req.Command)
	setDetachedAttr(cmd)
	cmd.Dir = req.WorkingDirectory
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	// Stdin stays nil: os/exec connects it to /dev/null.
	if req.Environment != nil {
		env := cmd.Environ()
		for k, v := range req.Environment {
			env = append(env, k+"="+v)
		}
		cmd.Env = env
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start job: %w", err)
	}

	job := &Job{
		ID:               id,
		Command:          req.Command,
		WorkingDirectory: req.WorkingDirectory,
		PID:              cmd.Process.Pid,
		LogPath:          logPath,
		StartedAt:        time.Now(),
		state:            JobRunning,
		done:             make(chan struct{}),
	}

	var timer *time.Timer
	if req.TimeoutSeconds > 0 {
		timer = time.AfterFunc(time.Duration(req.TimeoutSeconds)*time.Second, func() {
			job.mu.Lock()
			job.timedOut = true
			job.mu.Unlock()
			killProcessGroup(job.PID)
		})
	}

	go func() {
		err := cmd.Wait()
		if timer != nil {
			timer.Stop()
		}
		job.mu.Lock()
		job.endedAt = time.Now()
		job.exitCode = 0
		if err != nil {
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				job.exitCode = exitErr.ExitCode()
			} else {
				job.exitCode = -1
			}
		}
		switch {
		case job.timedOut:
			job.state = JobTimeout
		case job.killed:
			job.state = JobKilled
		default:
			job.state = JobExited
		}
		job.mu.Unlock()
		close(job.done)
	}()

	jm.mu.Lock()
	jm.jobs[id] = job
	jm.mu.Unlock()
	return job, nil
}

func (jm *JobManager) Get(id string) *Job {
	jm.mu.Lock()
	defer jm.mu.Unlock()
	return jm.jobs[id]
}

// List returns all known jobs, newest first.
func (jm *JobManager) List() []*Job {
	jm.mu.Lock()
	out := make([]*Job, 0, len(jm.jobs))
	for _, j := range jm.jobs {
		out = append(out, j)
	}
	jm.mu.Unlock()
	sort.Slice(out, func(a, b int) bool { return out[a].StartedAt.After(out[b].StartedAt) })
	return out
}

// Kill signals the job's whole process group. signal is "TERM" (default),
// "INT" or "KILL".
func (jm *JobManager) Kill(id, signal string) (*Job, error) {
	job := jm.Get(id)
	if job == nil {
		return nil, fmt.Errorf("job not found: %s", id)
	}
	if !job.running() {
		return job, fmt.Errorf("job %s is not running (state %s)", id, job.info().State)
	}
	var sig syscall.Signal
	switch strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(signal)), "SIG") {
	case "", "TERM":
		sig = syscall.SIGTERM
	case "INT":
		sig = syscall.SIGINT
	case "KILL":
		sig = syscall.SIGKILL
	default:
		return job, fmt.Errorf("unsupported signal %q (want TERM, INT or KILL)", signal)
	}
	job.mu.Lock()
	job.killed = true
	job.mu.Unlock()
	if err := signalProcessGroup(job.PID, sig); err != nil {
		return job, fmt.Errorf("signal job %s: %w", id, err)
	}
	return job, nil
}

// Running reports the number of jobs still running.
func (jm *JobManager) Running() int {
	n := 0
	for _, j := range jm.List() {
		if j.running() {
			n++
		}
	}
	return n
}

func (jm *JobManager) sweepLoop() {
	ticker := time.NewTicker(jobSweepInterval)
	defer ticker.Stop()
	for range ticker.C {
		jm.sweep(time.Now())
	}
}

// sweep forgets finished jobs older than jobRetention (and the oldest beyond
// maxFinishedJobs) and removes their log files.
func (jm *JobManager) sweep(now time.Time) {
	if err := guardReadOnly("prune job logs"); err != nil {
		return
	}
	var finished []*Job
	jm.mu.Lock()
	for _, j := range jm.jobs {
		if !j.running() {
			finished = append(finished, j)
		}
	}
	sort.Slice(finished, func(a, b int) bool { return finished[a].StartedAt.After(finished[b].StartedAt) })
	for i, j := range finished {
		j.mu.Lock()
		ended := j.endedAt
		j.mu.Unlock()
		if i >= maxFinishedJobs || now.Sub(ended) > jobRetention {
			delete(jm.jobs, j.ID)
			_ = os.Remove(j.LogPath)
		}
	}
	jm.mu.Unlock()
}
