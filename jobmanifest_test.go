package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/chuanqq/remote-cli/internal/control"
)

func testManifestConfig(t *testing.T) *Config {
	t.Helper()
	return &Config{
		DefaultShell:   "bash",
		JobDir:         t.TempDir(),
		StateDir:       t.TempDir(),
		Mode:           modeSlaver,
		ResolvedNodeID: "node-1",
	}
}

func TestJobManifestPersistedAndRunningJobRecovered(t *testing.T) {
	skipOnWindows(t)
	cfg := testManifestConfig(t)
	jm := NewJobManager(cfg)

	job, err := jm.Spawn(SpawnRequest{Command: "sleep 30"})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cfg.StateDir, "slaver", "jobs", job.ID+".json")
	var m jobManifest
	deadline := time.Now().Add(2 * time.Second)
	for {
		found, err := control.ReadJSONFile(path, &m)
		if err == nil && found && m.State == string(JobRunning) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("manifest not persisted as running: found=%t err=%v state=%q", found, err, m.State)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if m.NodeID != "node-1" {
		t.Errorf("manifest node_id = %q, want node-1", m.NodeID)
	}

	// A "restart" rebuilds the job from the manifest while the process lives.
	jm2 := NewJobManager(cfg)
	recovered := jm2.Get(job.ID)
	if recovered == nil {
		t.Fatal("running job was not recovered")
	}
	info := recovered.info()
	if info.State != JobRunning || !info.Recovered {
		t.Fatalf("recovered job = %+v", info)
	}
	if _, err := os.Stat(job.LogPath); err != nil {
		t.Fatalf("job log must not be removed: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("manifest must not be removed: %v", err)
	}

	// Stop the recovered job and let both managers observe the exit before the
	// test temp directories are removed.
	t.Cleanup(func() {
		_, _ = jm2.Kill(job.ID, "KILL")
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			cur := jm2.Get(job.ID)
			if cur == nil || cur.info().State != JobRunning {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		// Final manifest writes happen just after the state flip.
		time.Sleep(200 * time.Millisecond)
	})
}

func TestJobManifestFinishedJobRestored(t *testing.T) {
	skipOnWindows(t)
	cfg := testManifestConfig(t)
	jm := NewJobManager(cfg)
	job, err := jm.Spawn(SpawnRequest{Command: "echo hi"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-job.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("job did not finish")
	}
	path := filepath.Join(cfg.StateDir, "slaver", "jobs", job.ID+".json")
	deadline := time.Now().Add(2 * time.Second)
	for {
		var m jobManifest
		found, err := control.ReadJSONFile(path, &m)
		if err == nil && found && m.State != string(JobRunning) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("finished manifest not persisted: found=%t err=%v state=%q", found, err, m.State)
		}
		time.Sleep(20 * time.Millisecond)
	}

	jm2 := NewJobManager(cfg)
	recovered := jm2.Get(job.ID)
	if recovered == nil {
		t.Fatal("finished job was not restored")
	}
	info := recovered.info()
	if info.State != JobExited || !info.Recovered {
		t.Fatalf("restored finished job = %+v", info)
	}
	if info.ExitCode == nil || *info.ExitCode != 0 {
		t.Fatalf("restored exit code = %v, want 0", info.ExitCode)
	}
}

func TestJobManifestOrphanMarkedExited(t *testing.T) {
	skipOnWindows(t)
	cfg := testManifestConfig(t)
	dir := filepath.Join(cfg.StateDir, "slaver", "jobs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "orphan.log")
	if err := os.WriteFile(logPath, []byte("old output\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := &jobManifest{
		SchemaVersion: 1,
		JobID:         "job-orphan",
		Command:       "sleep 999",
		PID:           999999,
		LogPath:       logPath,
		StartedAt:     time.Now().Add(-time.Hour),
		State:         string(JobRunning),
	}
	if err := control.WriteJSONAtomic(filepath.Join(dir, "job-orphan.json"), manifest); err != nil {
		t.Fatal(err)
	}

	jm := NewJobManager(cfg)
	job := jm.Get("job-orphan")
	if job == nil {
		t.Fatal("orphan manifest was not restored")
	}
	info := job.info()
	if info.State != JobExited || !info.Recovered {
		t.Fatalf("orphan job = %+v, want exited+recovered", info)
	}
	if info.ExitCode != nil {
		t.Fatalf("orphan job exit code = %v, want unknown", info.ExitCode)
	}
	if _, err := os.Stat(logPath); err != nil {
		t.Fatalf("orphan log must not be deleted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "job-orphan.json")); err != nil {
		t.Fatalf("manifest must remain: %v", err)
	}
}
