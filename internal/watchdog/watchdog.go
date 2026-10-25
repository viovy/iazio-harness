// Package watchdog monitors in-flight harness executions, reconciles orphaned jobs, and manages daemon liveness.
package watchdog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/viovy/iazio-harness/internal/repolock"
)

const (
	StatusRunning                = "RUNNING"
	StatusCompleted              = "COMPLETED"
	StatusFailed                 = "FAILED"
	StatusTerminatedUnexpectedly = "TERMINATED_UNEXPECTEDLY"

	DefaultInterval = 5 * time.Second
)

// TrackedJob records state of an in-flight or completed harness execution.
type TrackedJob struct {
	JobID          string    `json:"job_id"`
	ScheduleID     string    `json:"schedule_id,omitempty"`
	WorktreePath   string    `json:"worktree_path"`
	DocsHubPath    string    `json:"docs_hub_path,omitempty"`
	APIURL         string    `json:"api_url"`
	Engine         string    `json:"engine"`
	Prompt         string    `json:"prompt"`
	StoryID        string    `json:"story_id,omitempty"`
	ConversationID string    `json:"conversation_id,omitempty"`
	PID            int       `json:"pid"`
	HarnessPID     int       `json:"harness_pid,omitempty"`
	ToolPID        int       `json:"tool_pid,omitempty"`
	StartedAt      time.Time `json:"started_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	Status         string    `json:"status"`
	ExitCode       int       `json:"exit_code"`
	LastError      string    `json:"last_error,omitempty"`
}

// DaemonStatus records the heartbeat and health of the iazio-harness daemon.
type DaemonStatus struct {
	PID           int       `json:"pid"`
	Status        string    `json:"status"`
	UptimeSeconds int64     `json:"uptime_seconds"`
	ActiveJobs    int       `json:"active_jobs"`
	StartedAt     time.Time `json:"started_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// Options configures a Watchdog instance.
type Options struct {
	StateFile  string
	StatusFile string
	IsAlive    func(pid int) bool
	Now        func() time.Time
}

// Watchdog tracks running harness jobs and autonomously sweeps dead processes.
type Watchdog struct {
	mu         sync.Mutex
	stateFile  string
	statusFile string
	jobs       map[string]*TrackedJob
	isAlive    func(pid int) bool
	now        func() time.Time
	startedAt  time.Time
}

// New creates a Watchdog.
func New(opts Options) *Watchdog {
	if opts.StateFile == "" {
		home, _ := os.UserHomeDir()
		if home != "" {
			opts.StateFile = filepath.Join(home, ".iazio", "harness-active-jobs.json")
		} else {
			opts.StateFile = filepath.Join(os.TempDir(), "harness-active-jobs.json")
		}
	}
	if opts.StatusFile == "" {
		home, _ := os.UserHomeDir()
		if home != "" {
			opts.StatusFile = filepath.Join(home, ".iazio", "harness-daemon.status")
		} else {
			opts.StatusFile = filepath.Join(os.TempDir(), "harness-daemon.status")
		}
	}
	if opts.IsAlive == nil {
		opts.IsAlive = defaultIsProcessAlive
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}

	w := &Watchdog{
		stateFile:  opts.StateFile,
		statusFile: opts.StatusFile,
		jobs:       make(map[string]*TrackedJob),
		isAlive:    opts.IsAlive,
		now:        opts.Now,
		startedAt:  opts.Now().UTC(),
	}
	w.loadState()
	return w
}

// IsProcessAlive tests whether a process with the given PID is actively running.
func IsProcessAlive(pid int) bool {
	return defaultIsProcessAlive(pid)
}

func (w *Watchdog) loadState() {
	if w.stateFile == "" {
		return
	}
	data, err := os.ReadFile(w.stateFile)
	if err != nil {
		return
	}
	var list []*TrackedJob
	if err := json.Unmarshal(data, &list); err == nil {
		for _, j := range list {
			if j != nil && j.JobID != "" {
				w.jobs[j.JobID] = j
			}
		}
	}
}

func (w *Watchdog) persistStateLocked() {
	if w.stateFile == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(w.stateFile), 0o755)
	list := make([]*TrackedJob, 0, len(w.jobs))
	for _, j := range w.jobs {
		list = append(list, j)
	}
	data, err := json.MarshalIndent(list, "", "  ")
	if err == nil {
		_ = os.WriteFile(w.stateFile, data, 0o644)
	}
}

// Track registers an active job with the watchdog.
func (w *Watchdog) Track(job TrackedJob) {
	w.mu.Lock()
	defer w.mu.Unlock()

	now := w.now().UTC()
	if job.StartedAt.IsZero() {
		job.StartedAt = now
	}
	job.UpdatedAt = now
	if job.Status == "" {
		job.Status = StatusRunning
	}

	jobCopy := job
	w.jobs[job.JobID] = &jobCopy
	w.persistStateLocked()
}

// Untrack removes a job from tracking.
func (w *Watchdog) Untrack(jobID string) {
	w.mu.Lock()
	defer w.mu.Unlock()

	delete(w.jobs, jobID)
	w.persistStateLocked()
}

// Complete marks a job as completed.
func (w *Watchdog) Complete(jobID string, exitCode int) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if job, ok := w.jobs[jobID]; ok {
		job.Status = StatusCompleted
		job.ExitCode = exitCode
		job.UpdatedAt = w.now().UTC()
		w.persistStateLocked()
	}
}

// Fail marks a job as failed.
func (w *Watchdog) Fail(jobID string, exitCode int, errText string) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if job, ok := w.jobs[jobID]; ok {
		job.Status = StatusFailed
		job.ExitCode = exitCode
		job.LastError = errText
		job.UpdatedAt = w.now().UTC()
		w.persistStateLocked()
	}
}

// GetJob returns a copy of a tracked job.
func (w *Watchdog) GetJob(jobID string) (TrackedJob, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()

	j, ok := w.jobs[jobID]
	if !ok || j == nil {
		return TrackedJob{}, false
	}
	return *j, true
}

// ListJobs returns copies of all tracked jobs.
func (w *Watchdog) ListJobs() []TrackedJob {
	w.mu.Lock()
	defer w.mu.Unlock()

	res := make([]TrackedJob, 0, len(w.jobs))
	for _, j := range w.jobs {
		if j != nil {
			res = append(res, *j)
		}
	}
	return res
}

// SweepDeadProcesses inspects running jobs. If the PID is dead, it transitions the job to TERMINATED_UNEXPECTEDLY.
func (w *Watchdog) SweepDeadProcesses() []string {
	w.mu.Lock()
	defer w.mu.Unlock()

	var reaped []string
	now := w.now().UTC()

	for jobID, job := range w.jobs {
		if job.Status != StatusRunning {
			continue
		}
		if job.PID <= 0 || !w.isAlive(job.PID) {
			job.Status = StatusTerminatedUnexpectedly
			job.UpdatedAt = now
			job.LastError = fmt.Sprintf("process PID %d terminated unexpectedly without clean exit", job.PID)
			reaped = append(reaped, jobID)

			// Clean up stale repo lock if worktree path is present
			if job.WorktreePath != "" {
				lockInfo, err := repolock.Read(job.WorktreePath)
				if err == nil && lockInfo.JobID == job.JobID {
					lockInfo.Status = StatusTerminatedUnexpectedly
					lockInfo.UpdatedAt = now
					lockPath := filepath.Join(job.WorktreePath, ".iazio", "repo.lock")
					if data, err := json.MarshalIndent(lockInfo, "", "  "); err == nil {
						_ = os.WriteFile(lockPath, data, 0o644)
					}
				}
			}
		}
	}

	if len(reaped) > 0 {
		w.persistStateLocked()
	}
	return reaped
}

// TouchHeartbeat writes the current daemon status to the status file.
func (w *Watchdog) TouchHeartbeat() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.statusFile == "" {
		return errors.New("empty status file path")
	}
	_ = os.MkdirAll(filepath.Dir(w.statusFile), 0o755)

	activeCount := 0
	for _, j := range w.jobs {
		if j.Status == StatusRunning {
			activeCount++
		}
	}

	now := w.now().UTC()
	uptime := int64(now.Sub(w.startedAt).Seconds())
	if uptime < 0 {
		uptime = 0
	}

	status := DaemonStatus{
		PID:           os.Getpid(),
		Status:        "RUNNING",
		UptimeSeconds: uptime,
		ActiveJobs:    activeCount,
		StartedAt:     w.startedAt,
		UpdatedAt:     now,
	}

	data, err := json.MarshalIndent(status, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(w.statusFile, data, 0o644)
}

func (w *Watchdog) markDaemonStopped() {
	if w.statusFile == "" {
		return
	}
	status := DaemonStatus{
		PID:       os.Getpid(),
		Status:    "STOPPED",
		StartedAt: w.startedAt,
		UpdatedAt: w.now().UTC(),
	}
	data, err := json.MarshalIndent(status, "", "  ")
	if err == nil {
		_ = os.WriteFile(w.statusFile, data, 0o644)
	}
}

func (w *Watchdog) isMaintenanceActive() bool {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return false
	}
	lockPath := filepath.Join(home, ".iazio", "maintenance.lock")
	_, err = os.Stat(lockPath)
	return err == nil
}

// Run executes the periodic watchdog monitoring loop until the context is canceled.
func (w *Watchdog) Run(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		interval = DefaultInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Initial sweep and heartbeat
	w.SweepDeadProcesses()
	_ = w.TouchHeartbeat()

	for {
		select {
		case <-ctx.Done():
			w.markDaemonStopped()
			return ctx.Err()
		case <-ticker.C:
			if w.isMaintenanceActive() {
				continue
			}
			w.SweepDeadProcesses()
			_ = w.TouchHeartbeat()
		}
	}
}
