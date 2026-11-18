package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/chuanqq/remote-cli/internal/control"
)

// Job manifest persistence (D5): only remote_spawn metadata is durable.
// Execution / scan / wait handles stay in process memory by design.
//
// Recovery must never kill or delete anything: upgrade.sh starts the new
// server through remote_spawn, so cleaning "orphan jobs" at startup would kill
// the upgrade itself.

const jobManifestSchemaVersion = 1

type jobManifestOwner struct {
	PrincipalID string `json:"principal_id,omitempty"`
	OperationID string `json:"operation_id,omitempty"`
}

type jobManifest struct {
	SchemaVersion    int              `json:"schema_version"`
	JobID            string           `json:"job_id"`
	NodeID           string           `json:"node_id,omitempty"`
	Command          string           `json:"command"`
	WorkingDirectory string           `json:"working_directory,omitempty"`
	PID              int              `json:"pid"`
	LogPath          string           `json:"log_path"`
	StartedAt        time.Time        `json:"started_at"`
	TimeoutSeconds   int              `json:"timeout_seconds,omitempty"`
	State            string           `json:"state"`
	ExitCode         *int             `json:"exit_code"`
	EndedAt          *time.Time       `json:"ended_at"`
	Owner            jobManifestOwner `json:"owner,omitempty"`
	Recovered        bool             `json:"recovered,omitempty"`
	RecoveryNote     string           `json:"recovery_note,omitempty"`
}

func (jm *JobManager) persistJob(j *Job) {
	if jm.manifestDir == "" || j == nil {
		return
	}
	info := j.info()
	m := jobManifest{
		SchemaVersion:    jobManifestSchemaVersion,
		JobID:            j.ID,
		NodeID:           jm.cfg.ResolvedNodeID,
		Command:          j.Command,
		WorkingDirectory: j.WorkingDirectory,
		PID:              j.PID,
		LogPath:          j.LogPath,
		StartedAt:        j.StartedAt,
		TimeoutSeconds:   j.TimeoutSeconds,
		State:            string(info.State),
		ExitCode:         info.ExitCode,
		Recovered:        j.recovered,
		RecoveryNote:     j.recoveryNote,
	}
	if info.EndedAt != "" {
		if t, err := time.Parse(time.RFC3339Nano, info.EndedAt); err == nil {
			m.EndedAt = &t
		}
	}
	path := filepath.Join(jm.manifestDir, j.ID+".json")
	j.manifestPath = path
	if err := control.WriteJSONAtomic(path, &m); err != nil {
		slog.Default().Warn("persist job manifest failed", "job_id", j.ID, "path", path, "err", err.Error())
	}
}

func (jm *JobManager) recover() {
	if jm.manifestDir == "" {
		return
	}
	entries, err := os.ReadDir(jm.manifestDir)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Default().Warn("read job manifest dir failed", "dir", jm.manifestDir, "err", err.Error())
		}
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(jm.manifestDir, e.Name())
		var m jobManifest
		found, err := control.ReadJSONFile(path, &m)
		if err != nil || !found {
			slog.Default().Warn("read job manifest failed", "path", path, "err", errString(err))
			continue
		}
		if m.SchemaVersion > jobManifestSchemaVersion {
			slog.Default().Warn("job manifest has a newer schema; ignored",
				"path", path, "schema", m.SchemaVersion)
			continue
		}
		if m.JobID == "" {
			m.JobID = strings.TrimSuffix(e.Name(), ".json")
		}
		jm.recoverOne(path, &m)
	}
}

func (jm *JobManager) recoverOne(path string, m *jobManifest) {
	now := time.Now()
	job := &Job{
		ID:               m.JobID,
		Command:          m.Command,
		WorkingDirectory: m.WorkingDirectory,
		PID:              m.PID,
		LogPath:          m.LogPath,
		StartedAt:        m.StartedAt,
		TimeoutSeconds:   m.TimeoutSeconds,
		manifestPath:     path,
		recovered:        true,
		done:             make(chan struct{}),
	}

	switch JobState(m.State) {
	case JobRunning:
		if m.PID > 0 && processAlive(m.PID) && commandMatches(m.PID, m.Command) {
			job.state = JobRunning
			job.recoveryNote = "recovered from manifest after restart; the process is still running"
			jm.addRecovered(job)
			go jm.monitorRecovered(job)
			slog.Default().Info("recovered running job", "job_id", job.ID, "pid", job.PID)
			return
		}
		// The process is gone (or the PID now belongs to something else).
		job.state = JobExited
		job.endedAt = now
		job.recoveryNote = "process was not found after restart; marked exited (exit code unavailable)"
		close(job.done)
		jm.addRecovered(job)
		jm.persistJob(job)
	case JobExited, JobKilled, JobTimeout:
		job.state = JobState(m.State)
		if m.ExitCode != nil {
			job.exitCode = *m.ExitCode
			job.exitKnown = true
		}
		job.endedAt = now
		if m.EndedAt != nil {
			job.endedAt = *m.EndedAt
		}
		job.recoveryNote = "restored from manifest"
		close(job.done)
		jm.addRecovered(job)
	default:
		slog.Default().Warn("job manifest has an unknown state; ignored", "job_id", m.JobID, "state", m.State)
	}
}

func (jm *JobManager) addRecovered(job *Job) {
	jm.mu.Lock()
	jm.jobs[job.ID] = job
	jm.mu.Unlock()
}

// monitorRecovered polls a job that survived the restart until its process
// disappears. The exit code is unknowable (the process is not our child).
func (jm *JobManager) monitorRecovered(job *Job) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for range ticker.C {
		if processAlive(job.PID) && commandMatches(job.PID, job.Command) {
			continue
		}
		job.mu.Lock()
		job.state = JobExited
		job.endedAt = time.Now()
		job.exitKnown = false
		job.recoveryNote = "recovered job ended after restart; exit code unavailable"
		job.mu.Unlock()
		close(job.done)
		jm.persistJob(job)
		return
	}
}

// commandMatches guards against PID reuse: the running process must look like
// our command. When the platform cannot report the command line it stays
// conservative and keeps the job (a false "exited" would hide a live job).
func commandMatches(pid int, command string) bool {
	line := processCommandLine(pid)
	if line == "" {
		return true
	}
	if command == "" {
		return true
	}
	if strings.Contains(line, command) {
		return true
	}
	// Shell wrappers can alter quoting; compare a distinctive prefix.
	probe := command
	if len(probe) > 32 {
		probe = probe[:32]
	}
	return strings.Contains(line, probe)
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
