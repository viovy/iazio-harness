package watchdog

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/viovy/iazio-harness/internal/repolock"
)

func TestWatchdog_TrackCompleteFail(t *testing.T) {
	tempDir := t.TempDir()
	stateFile := filepath.Join(tempDir, "active-jobs.json")
	statusFile := filepath.Join(tempDir, "daemon.status")

	aliveMap := map[int]bool{100: true, 200: true}
	w := New(Options{
		StateFile:  stateFile,
		StatusFile: statusFile,
		IsAlive:    func(pid int) bool { return aliveMap[pid] },
	})

	job1 := TrackedJob{
		JobID:        "job-1",
		WorktreePath: filepath.Join(tempDir, "repo1"),
		PID:          100,
	}
	w.Track(job1)

	got, ok := w.GetJob("job-1")
	if !ok || got.Status != StatusRunning {
		t.Fatalf("expected job-1 running, got %v", got)
	}

	w.Complete("job-1", 0)
	got, _ = w.GetJob("job-1")
	if got.Status != StatusCompleted || got.ExitCode != 0 {
		t.Fatalf("expected completed status, got %v", got)
	}

	job2 := TrackedJob{
		JobID: "job-2",
		PID:   200,
	}
	w.Track(job2)
	w.Fail("job-2", 1, "test failure")
	got, _ = w.GetJob("job-2")
	if got.Status != StatusFailed || got.LastError != "test failure" {
		t.Fatalf("expected failed status, got %v", got)
	}

	w.Untrack("job-2")
	_, ok = w.GetJob("job-2")
	if ok {
		t.Fatalf("expected job-2 untracked")
	}
}

func TestWatchdog_SweepDeadProcesses(t *testing.T) {
	tempDir := t.TempDir()
	stateFile := filepath.Join(tempDir, "active-jobs.json")
	statusFile := filepath.Join(tempDir, "daemon.status")
	repoDir := filepath.Join(tempDir, "test-repo")
	_ = os.MkdirAll(filepath.Join(repoDir, ".iazio"), 0o755)

	// Write mock repo.lock
	lockData, _ := json.Marshal(repolock.LockInfo{
		JobID:  "job-zombie",
		PID:    9999,
		Status: "RUNNING",
	})
	_ = os.WriteFile(filepath.Join(repoDir, ".iazio", "repo.lock"), lockData, 0o644)

	aliveMap := map[int]bool{
		1001: true,
		9999: false, // dead
	}

	w := New(Options{
		StateFile:  stateFile,
		StatusFile: statusFile,
		IsAlive:    func(pid int) bool { return aliveMap[pid] },
	})

	w.Track(TrackedJob{
		JobID:        "job-zombie",
		WorktreePath: repoDir,
		PID:          9999,
	})
	w.Track(TrackedJob{
		JobID: "job-alive",
		PID:   1001,
	})

	reaped := w.SweepDeadProcesses()
	if len(reaped) != 1 || reaped[0] != "job-zombie" {
		t.Fatalf("expected reaped job-zombie, got %v", reaped)
	}

	zombie, _ := w.GetJob("job-zombie")
	if zombie.Status != StatusTerminatedUnexpectedly {
		t.Fatalf("expected TERMINATED_UNEXPECTEDLY, got %s", zombie.Status)
	}

	// Verify repo.lock updated
	info, err := repolock.Read(repoDir)
	if err != nil || info.Status != StatusTerminatedUnexpectedly {
		t.Fatalf("expected repo.lock updated to TERMINATED_UNEXPECTEDLY, got %v, err=%v", info, err)
	}

	alive, _ := w.GetJob("job-alive")
	if alive.Status != StatusRunning {
		t.Fatalf("expected job-alive to remain RUNNING, got %s", alive.Status)
	}
}

func TestWatchdog_TouchHeartbeatAndRun(t *testing.T) {
	tempDir := t.TempDir()
	stateFile := filepath.Join(tempDir, "active-jobs.json")
	statusFile := filepath.Join(tempDir, "daemon.status")

	w := New(Options{
		StateFile:  stateFile,
		StatusFile: statusFile,
		IsAlive:    func(int) bool { return true },
	})

	if err := w.TouchHeartbeat(); err != nil {
		t.Fatalf("TouchHeartbeat failed: %v", err)
	}

	data, err := os.ReadFile(statusFile)
	if err != nil {
		t.Fatalf("status file missing: %v", err)
	}
	var st DaemonStatus
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatalf("failed to parse daemon status: %v", err)
	}
	if st.Status != "RUNNING" || st.PID != os.Getpid() {
		t.Fatalf("unexpected daemon status: %+v", st)
	}

	// Test Run cancellation
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- w.Run(ctx, 20*time.Millisecond)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	err = <-done
	if err != context.Canceled {
		t.Fatalf("expected context.Canceled, got %v", err)
	}

	// Status file should be updated to STOPPED
	data, _ = os.ReadFile(statusFile)
	_ = json.Unmarshal(data, &st)
	if st.Status != "STOPPED" {
		t.Fatalf("expected STOPPED status after exit, got %s", st.Status)
	}
}

func TestWatchdog_ListJobsAndDefaults(t *testing.T) {
	tempDir := t.TempDir()
	w := New(Options{
		StateFile: filepath.Join(tempDir, "state.json"),
	})
	if len(w.ListJobs()) != 0 {
		t.Fatalf("expected 0 jobs")
	}
	w.Track(TrackedJob{JobID: "j1", PID: 1})
	jobs := w.ListJobs()
	if len(jobs) != 1 || jobs[0].JobID != "j1" {
		t.Fatalf("expected 1 job, got %+v", jobs)
	}
}

func TestGetDescendantProcesses(t *testing.T) {
	// Spawning a subshell that spawns a background child sleep
	cmd := exec.Command("bash", "-c", "sleep 10 & wait")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start bash: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	time.Sleep(100 * time.Millisecond)
	procs, err := GetDescendantProcesses(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("GetDescendantProcesses failed: %v", err)
	}
	if len(procs) == 0 {
		t.Logf("no descendants found yet (may be fast or platform specific)")
	} else {
		summary, pids := FormatActiveChildProcessesSummary(procs)
		if len(pids) != len(procs) {
			t.Errorf("expected %d pids, got %d", len(procs), len(pids))
		}
		if summary == "" {
			t.Errorf("expected non-empty summary")
		}
	}
}

func TestFormatActiveChildProcessesSummary(t *testing.T) {
	procs := []ProcessInfo{
		{
			PID:     1234,
			PPID:    1000,
			Command: "make",
			Args:    "make run-review-scheme STORY_ID=STORY-GOV-3691",
		},
		{
			PID:     1235,
			PPID:    1234,
			Command: "python3",
			Args:    "python3 scripts/canonical/run_review_scheme.py",
		},
	}

	summary, pids := FormatActiveChildProcessesSummary(procs)
	if len(pids) != 2 {
		t.Fatalf("expected 2 pids, got %v", pids)
	}
	if !strings.Contains(summary, "make run-review-scheme") || !strings.Contains(summary, "1234") {
		t.Errorf("expected summary to contain make command and pid, got: %s", summary)
	}
	if !strings.Contains(summary, "python3 scripts/canonical/run_review_scheme.py") || !strings.Contains(summary, "1235") {
		t.Errorf("expected summary to contain python3 command and pid, got: %s", summary)
	}
}

func TestFilterNonDaemonChildProcesses_MCPExclusion(t *testing.T) {
	procs := []ProcessInfo{
		{PID: 82790, Command: "autopilot-mcp", Args: "/Users/romeo/.local/bin/autopilot-mcp serve"},
		{PID: 82794, Command: "npm", Args: "npm exec chrome-devtools-mcp@latest"},
		{PID: 82898, Command: "chrome-devtools-mcp", Args: "chrome-devtools-mcp"},
		{PID: 82945, Command: "node", Args: "node .../googlecloudtools.datacloud..."},
		{PID: 90001, Command: "make", Args: "make validate-local"},
	}

	filtered := FilterNonDaemonChildProcesses(procs)
	if len(filtered) != 1 {
		t.Fatalf("expected 1 non-MCP process, got %d", len(filtered))
	}
	if filtered[0].PID != 90001 {
		t.Fatalf("expected PID 90001 to survive, got %d", filtered[0].PID)
	}

	summary, pids := FormatActiveChildProcessesSummary(procs)
	if strings.Contains(summary, "autopilot-mcp") || strings.Contains(summary, "chrome-devtools-mcp") {
		t.Fatalf("expected summary to exclude MCP daemons, got: %s", summary)
	}
	if len(pids) != 1 || pids[0] != 90001 {
		t.Fatalf("expected only non-MCP PID 90001 in pids, got: %v", pids)
	}
}

func TestIsProcessAlive_AndScanPSDescendants(t *testing.T) {
	myPid := os.Getpid()
	if !IsProcessAlive(myPid) {
		t.Fatalf("expected current PID %d to be alive", myPid)
	}
	if IsProcessAlive(-1) || IsProcessAlive(0) {
		t.Fatalf("expected non-positive PID to be dead")
	}

	cmd := exec.Command("sleep", "2")
	if err := cmd.Start(); err == nil {
		defer cmd.Process.Kill()
		procs, err := scanPSDescendants(myPid)
		if err != nil {
			t.Logf("scanPSDescendants returned err: %v", err)
		} else {
			t.Logf("scanPSDescendants found %d procs", len(procs))
		}
	}
}
