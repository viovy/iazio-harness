package e2e_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/viovy/iazio-harness/internal/gitclean"
)

// ============================================================================
// 1. ZIPKIN V2 TELEMETRY COLLECTOR & AUTOMATED TROUBLESHOOTING INSIGHTS
// ============================================================================

type ZipkinSpan struct {
	TraceID   string            `json:"traceId"`
	ID        string            `json:"id"`
	ParentID  string            `json:"parentId,omitempty"`
	Name      string            `json:"name"`
	Timestamp int64             `json:"timestamp"`
	Duration  int64             `json:"duration"`
	Tags      map[string]string `json:"tags"`
}

type TroubleshootingInsight struct {
	TraceID           string       `json:"trace_id"`
	ZipkinUIURL       string       `json:"zipkin_ui_url"`
	SpanCount         int          `json:"span_count"`
	RootCauseCategory string       `json:"root_cause_category"`
	Summary           string       `json:"summary"`
	PinpointedSpan    string       `json:"pinpointed_span"`
	RemediationAction string       `json:"remediation_action"`
	RecordedSpans     []ZipkinSpan `json:"recorded_spans"`
}

type ZipkinCollector struct {
	mu     sync.Mutex
	traces map[string][]ZipkinSpan
	URL    string
}

func NewZipkinCollectorServer() (*ZipkinCollector, *httptest.Server) {
	c := &ZipkinCollector{traces: make(map[string][]ZipkinSpan)}
	mux := http.NewServeMux()

	// Standard Zipkin V2 POST /api/v2/spans ingestion
	mux.HandleFunc("POST /api/v2/spans", func(w http.ResponseWriter, r *http.Request) {
		var spans []ZipkinSpan
		if err := json.NewDecoder(r.Body).Decode(&spans); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		c.mu.Lock()
		for _, s := range spans {
			c.traces[s.TraceID] = append(c.traces[s.TraceID], s)
		}
		c.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	})

	// Standard Zipkin V2 GET /api/v2/traces/{traceId} query
	mux.HandleFunc("GET /api/v2/traces/{traceId}", func(w http.ResponseWriter, r *http.Request) {
		traceID := r.PathValue("traceId")
		c.mu.Lock()
		spans := c.traces[traceID]
		c.mu.Unlock()
		if len(spans) == 0 {
			http.Error(w, "trace not found", http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(spans)
	})

	srv := httptest.NewServer(mux)
	c.URL = srv.URL
	return c, srv
}

func (c *ZipkinCollector) EmitSpan(traceID, name string, duration time.Duration, tags map[string]string) {
	idBytes := make([]byte, 8)
	_, _ = rand.Read(idBytes)
	span := ZipkinSpan{
		TraceID:   traceID,
		ID:        hex.EncodeToString(idBytes),
		Name:      name,
		Timestamp: time.Now().UnixMicro(),
		Duration:  duration.Microseconds(),
		Tags:      tags,
	}
	body, _ := json.Marshal([]ZipkinSpan{span})
	resp, err := http.Post(c.URL+"/api/v2/spans", "application/json", bytes.NewReader(body))
	if err == nil {
		_ = resp.Body.Close()
	}
}

func (c *ZipkinCollector) AnalyzeTrace(traceID string) TroubleshootingInsight {
	c.mu.Lock()
	spans := append([]ZipkinSpan(nil), c.traces[traceID]...)
	c.mu.Unlock()

	insight := TroubleshootingInsight{
		TraceID:           traceID,
		ZipkinUIURL:       fmt.Sprintf("%s/zipkin/traces/%s", c.URL, traceID),
		SpanCount:         len(spans),
		RootCauseCategory: "HEALTHY_EXECUTION",
		Summary:           "All execution spans completed with verified repository mutations.",
		RecordedSpans:     spans,
	}

	for _, s := range spans {
		if s.Tags["poe.passed"] == "false" {
			insight.RootCauseCategory = s.Tags["poe.error_code"]
			insight.PinpointedSpan = s.Name
			insight.Summary = fmt.Sprintf("Span %q rejected job completion: %s", s.Name, s.Tags["poe.reason"])
			insight.RemediationAction = "Inspect agent CLI arguments and ensure real non-spec file mutations occur during execution."
			return insight
		}
		if s.Tags["schedule.stuck_reason"] != "" {
			insight.RootCauseCategory = "STUCK_SCHEDULE_RECONCILED"
			insight.PinpointedSpan = s.Name
			insight.Summary = s.Tags["schedule.stuck_reason"]
			insight.RemediationAction = "Click 'Force Unstick Schedule' or 'Discard Changes' on target agent workspace."
			return insight
		}
	}
	return insight
}

// ============================================================================
// 2. STRICT WORKTREE FINGERPRINTING & ANTI-BYPASS PROOF OF EXECUTION (PoE)
// ============================================================================

var ErrBypassOrNoOpDetected = errors.New("BYPASS_OR_NOOP_DETECTED")

type WorktreeSnapshot struct {
	HeadSHA              string   `json:"head_sha"`
	NonSpecMutationHash  string   `json:"non_spec_mutation_hash"`
	NonSpecChangedFiles  []string `json:"non_spec_changed_files"`
	StorySpecContentHash string   `json:"story_spec_content_hash"`
	HasIndexLock         bool     `json:"has_index_lock"`
	DirtyFileCount       int      `json:"dirty_file_count"`
}

// CaptureWorktreeSnapshot computes an exact cryptographic snapshot of the repo
// BEFORE and AFTER agy runs, explicitly separating pre-scaffolded story markdown
// files (docs/stories/*) from actual code/repo deliverables!
func CaptureWorktreeSnapshot(repoDir string) (WorktreeSnapshot, error) {
	var snap WorktreeSnapshot

	if _, err := os.Stat(filepath.Join(repoDir, ".git", "index.lock")); err == nil {
		snap.HasIndexLock = true
	}

	headOut, err := exec.Command("git", "-C", repoDir, "rev-parse", "HEAD").Output()
	if err != nil {
		return snap, fmt.Errorf("git rev-parse HEAD failed: %w", err)
	}
	snap.HeadSHA = strings.TrimSpace(string(headOut))

	statusOut, err := exec.Command("git", "-C", repoDir, "status", "--porcelain", "-uall").Output()
	if err != nil {
		return snap, fmt.Errorf("git status failed: %w", err)
	}

	lines := strings.Split(strings.TrimSpace(string(statusOut)), "\n")
	nonSpecHasher := sha256.New()
	specHasher := sha256.New()

	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		snap.DirtyFileCount++
		relPath := strings.TrimSpace(line[2:])
		fullPath := filepath.Join(repoDir, relPath)
		content, _ := os.ReadFile(fullPath)

		if strings.HasPrefix(relPath, "docs/stories/") || relPath == "docs/stories" {
			specHasher.Write([]byte(relPath))
			specHasher.Write(content)
		} else {
			snap.NonSpecChangedFiles = append(snap.NonSpecChangedFiles, relPath)
			nonSpecHasher.Write([]byte(line))
			nonSpecHasher.Write(content)
		}
	}

	snap.NonSpecMutationHash = hex.EncodeToString(nonSpecHasher.Sum(nil))
	snap.StorySpecContentHash = hex.EncodeToString(specHasher.Sum(nil))
	return snap, nil
}

// VerifyStrictStoryExecution prevents the job_0828bc2214e083b5 bypass:
// Even if StoryID ("STORY-ROOT-245") was pre-allocated AND docs/stories/story-root-245.story.md
// was pre-scaffolded in the dirty worktree before enqueue, the job FAILS unless actual
// non-spec repository mutations (commits or file changes) occurred DURING the execution window!
func VerifyStrictStoryExecution(action string, pre, post WorktreeSnapshot, stdout string) (string, error) {
	trimmed := strings.TrimSpace(stdout)
	if trimmed == "" || strings.HasPrefix(trimmed, "[+] prompt executed via") {
		return "SYNTHETIC_OR_EMPTY_STDOUT", fmt.Errorf("%w: synthetic or empty stdout (%q)", ErrBypassOrNoOpDetected, trimmed)
	}

	if action == "/autonomous-story-execution" {
		headCommitted := pre.HeadSHA != post.HeadSHA
		nonSpecWorktreeMutated := pre.NonSpecMutationHash != post.NonSpecMutationHash

		if !headCommitted && !nonSpecWorktreeMutated {
			return "PRE_SCAFFOLD_NOOP_BYPASS", fmt.Errorf(
				"%w: zero code/repo deliverables mutated during execution (pre_head=%s post_head=%s, pre_non_spec_hash=%s post_non_spec_hash=%s). Pre-scaffolding docs/stories/*.md without executing the story is rejected",
				ErrBypassOrNoOpDetected, pre.HeadSHA, post.HeadSHA, pre.NonSpecMutationHash[:12], post.NonSpecMutationHash[:12],
			)
		}
	}
	return "VERIFIED", nil
}

// ============================================================================
// 3. PLATFORM CONTROL PLANE (Jobs, History, Schedules, Agent Repo Actions)
// ============================================================================

type JobStatus string

const (
	StatusPending   JobStatus = "pending"
	StatusRunning   JobStatus = "running"
	StatusCompleted JobStatus = "completed"
	StatusFailed    JobStatus = "failed"
	StatusCancelled JobStatus = "cancelled"
)

type Job struct {
	ID             string           `json:"id"`
	RetryOfJobID   string           `json:"retry_of_job_id,omitempty"`
	ScheduleID     string           `json:"schedule_id,omitempty"`
	TraceID        string           `json:"trace_id"`
	Action         string           `json:"action"`
	TargetRepo     string           `json:"target_repo"`
	TargetAgent    string           `json:"target_agent"`
	StoryID        string           `json:"story_id"`
	StoryTitle     string           `json:"story_title"`
	Status         JobStatus        `json:"status"`
	FencingToken   uint64           `json:"fencing_token"`
	PreSnapshot    WorktreeSnapshot `json:"pre_snapshot"`
	PostSnapshot   WorktreeSnapshot `json:"post_snapshot"`
	OutputLog      string           `json:"output_log"`
	ErrorMsg       string           `json:"error_msg,omitempty"`
	MaxDurationSec int              `json:"max_duration_sec"`
	LeaseExpiresAt time.Time        `json:"lease_expires_at"`
	UpdatedAt      time.Time        `json:"updated_at"`
}

type Schedule struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	TargetAgent    string    `json:"target_agent"`
	TargetRepo     string    `json:"target_repo"`
	Action         string    `json:"action"`
	Enabled        bool      `json:"enabled"`
	Status         string    `json:"status"` // "idle", "running", "paused"
	ActiveJobID    string    `json:"active_job_id,omitempty"`
	LastTraceID    string    `json:"last_trace_id,omitempty"`
	CompletedRuns  int       `json:"completed_runs"`
	MaxDurationSec int       `json:"max_duration_sec"`
	LastReconciled time.Time `json:"last_reconciled"`
}

type HistoryEntry struct {
	EventID   string    `json:"event_id"`
	JobID     string    `json:"job_id,omitempty"`
	SchedID   string    `json:"schedule_id,omitempty"`
	TraceID   string    `json:"trace_id"`
	EventType string    `json:"event_type"`
	Status    string    `json:"status"`
	Details   string    `json:"details"`
	Timestamp time.Time `json:"timestamp"`
}

type AgentWorkspaceController struct {
	AgentID   string
	RepoPaths map[string]string
	mu        sync.Mutex
	activeCmd map[string]*exec.Cmd
}

func (ac *AgentWorkspaceController) RegisterCancelCmd(repo string, cmd *exec.Cmd) {
	ac.mu.Lock()
	defer ac.mu.Unlock()
	if ac.activeCmd == nil {
		ac.activeCmd = make(map[string]*exec.Cmd)
	}
	ac.activeCmd[repo] = cmd
}

func (ac *AgentWorkspaceController) AbortActiveProcess(repo string) bool {
	ac.mu.Lock()
	defer ac.mu.Unlock()
	if cmd, ok := ac.activeCmd[repo]; ok && cmd != nil && cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		delete(ac.activeCmd, repo)
		return true
	}
	return false
}

// DiscardChanges implements the exact backend + agent logic for the
// "Discard Changes" button on /agents/mac-mini/iazio-test-repo/queue,
// delegating to the production gitclean.DiscardWorkspace pure code action routine.
func (ac *AgentWorkspaceController) DiscardChanges(repoName string, forceKillActive bool) (WorktreeSnapshot, error) {
	repoDir, ok := ac.RepoPaths[repoName]
	if !ok {
		return WorktreeSnapshot{}, fmt.Errorf("unknown repo %q on agent %s", repoName, ac.AgentID)
	}

	if forceKillActive {
		ac.AbortActiveProcess(repoName)
	}

	_, err := gitclean.DiscardWorkspace(context.Background(), gitclean.DiscardOptions{
		WorktreePath: repoDir,
		Force:        forceKillActive,
		Submodules:   true,
		RemoveLocks:  true,
	})
	if err != nil {
		return WorktreeSnapshot{}, err
	}

	return CaptureWorktreeSnapshot(repoDir)
}

// DiscardAll executes DiscardChanges across all registered repositories for this agent.
func (ac *AgentWorkspaceController) DiscardAll(forceKillActive bool) ([]string, error) {
	ac.mu.Lock()
	var repos []string
	for r := range ac.RepoPaths {
		repos = append(repos, r)
	}
	ac.mu.Unlock()

	sort.Strings(repos)
	for _, r := range repos {
		if _, err := ac.DiscardChanges(r, forceKillActive); err != nil {
			return nil, err
		}
	}
	return repos, nil
}

// ============================================================================
// 4. IAZIO CONTROL PLANE SERVER (Jobs, Schedules, History, Buttons, Zipkin)
// ============================================================================

type IazioControlPlane struct {
	mu        sync.Mutex
	jobs      map[string]*Job
	jobOrder  []string
	schedules map[string]*Schedule
	history   []HistoryEntry
	agentCtrl *AgentWorkspaceController
	zipkin    *ZipkinCollector
	leaseTTL  time.Duration
}

func NewIazioControlPlane(agentCtrl *AgentWorkspaceController, zipkin *ZipkinCollector, leaseTTL time.Duration) *IazioControlPlane {
	return &IazioControlPlane{
		jobs:      make(map[string]*Job),
		schedules: make(map[string]*Schedule),
		agentCtrl: agentCtrl,
		zipkin:    zipkin,
		leaseTTL:  leaseTTL,
	}
}

func newHexID(prefix string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

func (cp *IazioControlPlane) recordHistory(jobID, schedID, traceID, eventType, status, details string) {
	cp.history = append(cp.history, HistoryEntry{
		EventID:   newHexID("evt_"),
		JobID:     jobID,
		SchedID:   schedID,
		TraceID:   traceID,
		EventType: eventType,
		Status:    status,
		Details:   details,
		Timestamp: time.Now(),
	})
}

// ReconcileSchedules fixes stuck schedules like sch-1532 by checking if their
// ActiveJobID has finished, failed, cancelled, or exceeded MaxDurationSec.
func (cp *IazioControlPlane) ReconcileSchedules(forceUnstickSchedID string) {
	cp.mu.Lock()
	defer cp.mu.Unlock()

	for _, sch := range cp.schedules {
		if sch.ActiveJobID == "" {
			continue
		}
		job, exists := cp.jobs[sch.ActiveJobID]
		if !exists {
			sch.ActiveJobID = ""
			sch.Status = "idle"
			continue
		}

		isForceTarget := (forceUnstickSchedID != "" && sch.ID == forceUnstickSchedID)
		isTimedOut := sch.MaxDurationSec > 0 && time.Since(job.UpdatedAt) > time.Duration(sch.MaxDurationSec)*time.Second

		if job.Status == StatusRunning && (isForceTarget || isTimedOut) {
			cp.agentCtrl.AbortActiveProcess(sch.TargetRepo)
			_, _ = cp.agentCtrl.DiscardChanges(sch.TargetRepo, true)
			job.Status = StatusFailed
			job.ErrorMsg = "SCHEDULE_WATCHDOG_ABORT: job exceeded schedule timeout or was force-unstuck"
			job.FencingToken++
			cp.zipkin.EmitSpan(job.TraceID, "schedule.reconcile.abort_stuck_job", 5*time.Millisecond, map[string]string{
				"iazio.schedule.id":     sch.ID,
				"iazio.job.id":          job.ID,
				"schedule.stuck_reason": job.ErrorMsg,
			})
			cp.recordHistory(job.ID, sch.ID, job.TraceID, "SCHEDULE_FORCE_UNSTUCK", string(StatusFailed), job.ErrorMsg)
		}

		if job.Status == StatusCompleted || job.Status == StatusFailed || job.Status == StatusCancelled {
			if job.Status == StatusCompleted {
				sch.CompletedRuns++
			}
			sch.ActiveJobID = ""
			if sch.Enabled {
				sch.Status = "idle"
			} else {
				sch.Status = "paused"
			}
			sch.LastReconciled = time.Now()
		}
	}
}

func (cp *IazioControlPlane) EnqueueJob(action, targetRepo, targetAgent, storyID, storyTitle, schedID, retryOf string, maxDurSec int) *Job {
	cp.mu.Lock()
	defer cp.mu.Unlock()

	traceID := newHexID("") + newHexID("")
	jobID := newHexID("job_")
	j := &Job{
		ID:             jobID,
		RetryOfJobID:   retryOf,
		ScheduleID:     schedID,
		TraceID:        traceID,
		Action:         action,
		TargetRepo:     targetRepo,
		TargetAgent:    targetAgent,
		StoryID:        storyID,
		StoryTitle:     storyTitle,
		Status:         StatusPending,
		FencingToken:   0,
		MaxDurationSec: maxDurSec,
		UpdatedAt:      time.Now(),
	}
	cp.jobs[jobID] = j
	cp.jobOrder = append(cp.jobOrder, jobID)

	cp.zipkin.EmitSpan(traceID, "api.queue.enqueue", 2*time.Millisecond, map[string]string{
		"iazio.job.id":      jobID,
		"iazio.story.id":    storyID,
		"iazio.schedule.id": schedID,
		"iazio.action":      action,
	})
	cp.recordHistory(jobID, schedID, traceID, "JOB_ENQUEUED", string(StatusPending), storyTitle)
	copyJob := *j
	return &copyJob
}

// ExecuteNextJob runs the full Agent + Harness + Mock-Agy + Strict PoE lifecycle
func (cp *IazioControlPlane) ExecuteNextJob(ctx context.Context, agyBin string, extraEnv []string) (*Job, error) {
	cp.mu.Lock()
	var claimed *Job
	for _, id := range cp.jobOrder {
		j := cp.jobs[id]
		if j.Status == StatusPending {
			j.Status = StatusRunning
			j.FencingToken++
			j.LeaseExpiresAt = time.Now().Add(cp.leaseTTL)
			j.UpdatedAt = time.Now()
			copyJob := *j
			claimed = &copyJob
			break
		}
	}
	cp.mu.Unlock()

	if claimed == nil {
		return nil, errors.New("no pending jobs")
	}

	repoDir := cp.agentCtrl.RepoPaths[claimed.TargetRepo]

	// If this is a scheduled repo cleanup (e.g., sch-1532), clear stale .git/index.lock before running
	if claimed.Action == "/repo-cleanup" {
		_ = os.Remove(filepath.Join(repoDir, ".git", "index.lock"))
	}

	// 1. Capture Pre-Execution Worktree Snapshot
	preSnap, err := CaptureWorktreeSnapshot(repoDir)
	if err != nil {
		return nil, err
	}
	cp.zipkin.EmitSpan(claimed.TraceID, "harness.git.pre_snapshot", 3*time.Millisecond, map[string]string{
		"git.pre_head":          preSnap.HeadSHA,
		"git.pre_non_spec_hash": preSnap.NonSpecMutationHash[:12],
		"git.dirty_files":       fmt.Sprintf("%d", preSnap.DirtyFileCount),
	})

	// 2. Execute agy subprocess with non-interactive Stdin (/dev/null) and PGID isolation
	maxDur := 10 * time.Second
	if claimed.MaxDurationSec > 0 {
		maxDur = time.Duration(claimed.MaxDurationSec) * time.Second
	}
	execCtx, cancel := context.WithTimeout(ctx, maxDur)
	defer cancel()

	start := time.Now()
	cmd := exec.CommandContext(execCtx, agyBin,
		"--job-id", claimed.ID,
		"--action", claimed.Action,
		"--story-id", claimed.StoryID,
	)
	cmd.Dir = repoDir
	cmd.Stdin = nil // Guarantees /dev/null: prevents interactive stdin hangs on sch-1532!
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	cmd.Env = append(cmd.Env, extraEnv...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}

	var outBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &outBuf

	if err := cmd.Start(); err != nil {
		return nil, err
	}
	cp.agentCtrl.RegisterCancelCmd(claimed.TargetRepo, cmd)
	waitErr := cmd.Wait()
	cp.agentCtrl.RegisterCancelCmd(claimed.TargetRepo, nil)
	execDuration := time.Since(start)

	// 3. Capture Post-Execution Worktree Snapshot
	postSnap, _ := CaptureWorktreeSnapshot(repoDir)

	cp.zipkin.EmitSpan(claimed.TraceID, "harness.agy.exec", execDuration, map[string]string{
		"iazio.job.id":      claimed.ID,
		"proc.stdout_bytes": fmt.Sprintf("%d", outBuf.Len()),
		"git.post_head":     postSnap.HeadSHA,
	})

	// 4. Enforce Strict Proof-of-Execution (catches job_0828bc2214e083b5 pre-scaffold bypass!)
	cp.mu.Lock()
	defer cp.mu.Unlock()
	stored := cp.jobs[claimed.ID]
	if stored.FencingToken != claimed.FencingToken || stored.Status != StatusRunning {
		return stored, errors.New("409 conflict: job was cancelled or preempted")
	}

	stored.PreSnapshot = preSnap
	stored.PostSnapshot = postSnap
	stored.OutputLog = outBuf.String()
	stored.UpdatedAt = time.Now()

	if waitErr != nil {
		stored.Status = StatusFailed
		stored.ErrorMsg = fmt.Sprintf("subprocess failed: %v", waitErr)
		cp.recordHistory(stored.ID, stored.ScheduleID, stored.TraceID, "JOB_FAILED", string(StatusFailed), stored.ErrorMsg)
		copyJob := *stored
		return &copyJob, waitErr
	}

	errCode, poeErr := VerifyStrictStoryExecution(stored.Action, preSnap, postSnap, outBuf.String())
	if poeErr != nil {
		stored.Status = StatusFailed
		stored.ErrorMsg = poeErr.Error()
		cp.zipkin.EmitSpan(stored.TraceID, "api.poe.verify", 1*time.Millisecond, map[string]string{
			"poe.passed":     "false",
			"poe.error_code": errCode,
			"poe.reason":     poeErr.Error(),
		})
		cp.recordHistory(stored.ID, stored.ScheduleID, stored.TraceID, "JOB_REJECTED_BY_POE", string(StatusFailed), poeErr.Error())
		copyJob := *stored
		return &copyJob, poeErr
	}

	stored.Status = StatusCompleted
	cp.zipkin.EmitSpan(stored.TraceID, "api.poe.verify", 1*time.Millisecond, map[string]string{
		"poe.passed": "true",
	})
	cp.recordHistory(stored.ID, stored.ScheduleID, stored.TraceID, "JOB_COMPLETED", string(StatusCompleted), "Verified execution proof")
	copyJob := *stored
	return &copyJob, nil
}

// BuildHTTPHandler exposes all endpoints backing /global-queue/jobs, /history, /schedules,
// /observability/traces/{traceId}/insights, and /agents/{agent}/{repo}/queue button actions.
func (cp *IazioControlPlane) BuildHTTPHandler() http.Handler {
	mux := http.NewServeMux()

	// GET /api/v1/global-queue/jobs
	mux.HandleFunc("GET /api/v1/global-queue/jobs", func(w http.ResponseWriter, r *http.Request) {
		cp.mu.Lock()
		defer cp.mu.Unlock()
		var list []*Job
		for _, id := range cp.jobOrder {
			list = append(list, cp.jobs[id])
		}
		_ = json.NewEncoder(w).Encode(list)
	})

	// GET /api/v1/global-queue/history
	mux.HandleFunc("GET /api/v1/global-queue/history", func(w http.ResponseWriter, r *http.Request) {
		cp.mu.Lock()
		defer cp.mu.Unlock()
		_ = json.NewEncoder(w).Encode(cp.history)
	})

	// GET /api/v1/global-queue/schedules
	mux.HandleFunc("GET /api/v1/global-queue/schedules", func(w http.ResponseWriter, r *http.Request) {
		cp.ReconcileSchedules("")
		cp.mu.Lock()
		defer cp.mu.Unlock()
		var list []*Schedule
		for _, s := range cp.schedules {
			list = append(list, s)
		}
		_ = json.NewEncoder(w).Encode(list)
	})

	// POST /api/v1/agents/{agent}/{repo}/actions/discard-changes
	mux.HandleFunc("POST /api/v1/agents/{agent}/{repo}/actions/discard-changes", func(w http.ResponseWriter, r *http.Request) {
		repo := r.PathValue("repo")
		traceID := newHexID("") + newHexID("")
		snap, err := cp.agentCtrl.DiscardChanges(repo, true)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		cp.zipkin.EmitSpan(traceID, "agent.control.discard_changes", 4*time.Millisecond, map[string]string{
			"iazio.agent.id":  r.PathValue("agent"),
			"iazio.repo":      repo,
			"git.dirty_files": fmt.Sprintf("%d", snap.DirtyFileCount),
		})
		cp.mu.Lock()
		cp.recordHistory("", "", traceID, "WORKSPACE_DISCARD_CHANGES", "clean", fmt.Sprintf("Repo %s reset to clean HEAD %s", repo, snap.HeadSHA))
		cp.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"clean":            snap.DirtyFileCount == 0 && !snap.HasIndexLock,
			"dirty_file_count": snap.DirtyFileCount,
			"head_sha":         snap.HeadSHA,
			"trace_id":         traceID,
		})
	})

	// POST /v1/hosts/{agent}/repos/discard-all and POST /api/v1/agents/{agent}/repos/discard-all
	discardAllHandler := func(w http.ResponseWriter, r *http.Request) {
		agent := r.PathValue("agent")
		force := r.URL.Query().Get("force") == "true"
		traceID := newHexID("") + newHexID("")

		// Abort any running jobs for this host/agent if force is set
		if force {
			cp.mu.Lock()
			for _, j := range cp.jobs {
				if j.Status == StatusRunning {
					j.Status = StatusCancelled
					j.ErrorMsg = "ABORTED_BY_DISCARD_ALL"
				}
			}
			cp.mu.Unlock()
		}

		repos, err := cp.agentCtrl.DiscardAll(force)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		cp.zipkin.EmitSpan(traceID, "agent.control.discard_all_repos", 10*time.Millisecond, map[string]string{
			"iazio.agent.id": agent,
			"repos.count":    fmt.Sprintf("%d", len(repos)),
		})

		cp.mu.Lock()
		cp.recordHistory("", "", traceID, "WORKSPACE_DISCARD_ALL_REPOS", "clean", fmt.Sprintf("All %d repos reset to clean on agent %s", len(repos), agent))
		cp.mu.Unlock()

		_ = json.NewEncoder(w).Encode(map[string]any{
			"count": len(repos),
			"repos": repos,
		})
	}
	mux.HandleFunc("POST /v1/hosts/{agent}/repos/discard-all", discardAllHandler)
	mux.HandleFunc("POST /api/v1/agents/{agent}/repos/discard-all", discardAllHandler)

	// POST /api/v1/global-queue/jobs/{id}/cancel
	mux.HandleFunc("POST /api/v1/global-queue/jobs/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		cp.mu.Lock()
		j, ok := cp.jobs[id]
		if !ok {
			cp.mu.Unlock()
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		j.Status = StatusCancelled
		j.FencingToken++
		cp.agentCtrl.AbortActiveProcess(j.TargetRepo)
		cp.recordHistory(j.ID, j.ScheduleID, j.TraceID, "JOB_CANCELLED_BY_USER", string(StatusCancelled), "Cancelled via UI button")
		cp.mu.Unlock()
		_ = json.NewEncoder(w).Encode(j)
	})

	// POST /api/v1/global-queue/jobs/{id}/retry
	mux.HandleFunc("POST /api/v1/global-queue/jobs/{id}/retry", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		cp.mu.Lock()
		orig, ok := cp.jobs[id]
		cp.mu.Unlock()
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		retried := cp.EnqueueJob(orig.Action, orig.TargetRepo, orig.TargetAgent, orig.StoryID, orig.StoryTitle, orig.ScheduleID, orig.ID, orig.MaxDurationSec)
		_ = json.NewEncoder(w).Encode(retried)
	})

	// POST /api/v1/global-queue/schedules/{id}/actions/{action} (unstick | trigger | pause | resume)
	mux.HandleFunc("POST /api/v1/global-queue/schedules/{id}/actions/{action}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		act := r.PathValue("action")

		if act == "unstick" {
			cp.ReconcileSchedules(id)
		}

		cp.mu.Lock()
		sch, ok := cp.schedules[id]
		if !ok {
			cp.mu.Unlock()
			http.Error(w, "schedule not found", http.StatusNotFound)
			return
		}

		switch act {
		case "pause":
			sch.Enabled = false
			sch.Status = "paused"
		case "resume":
			sch.Enabled = true
			if sch.ActiveJobID == "" {
				sch.Status = "idle"
			}
		case "trigger":
			cp.mu.Unlock()
			cp.ReconcileSchedules(id)
			job := cp.EnqueueJob(sch.Action, sch.TargetRepo, sch.TargetAgent, "SCHEDULED-CLEANUP", sch.Name, sch.ID, "", sch.MaxDurationSec)
			cp.mu.Lock()
			sch.ActiveJobID = job.ID
			sch.LastTraceID = job.TraceID
			sch.Status = "running"
		}
		copySch := *sch
		cp.mu.Unlock()
		_ = json.NewEncoder(w).Encode(copySch)
	})

	// GET /api/v1/observability/traces/{traceId}/insights
	mux.HandleFunc("GET /api/v1/observability/traces/{traceId}/insights", func(w http.ResponseWriter, r *http.Request) {
		insight := cp.zipkin.AnalyzeTrace(r.PathValue("traceId"))
		_ = json.NewEncoder(w).Encode(insight)
	})

	return mux
}

// ============================================================================
// 5. TEST HELPERS: GIT REPO INITIALIZER & MULTI-MODE MOCK AGY BINARY
// ============================================================================

func initTestGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGit := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, string(out))
		}
	}
	runGit("init")
	runGit("config", "user.email", "ci@iazio.local")
	runGit("config", "user.name", "Iazio CI")

	// Seed initial tracked files simulating WSL residual artifacts & .autopilot python script
	_ = os.MkdirAll(filepath.Join(dir, ".autopilot"), 0755)
	_ = os.WriteFile(filepath.Join(dir, "wsl_artifact.Zone.Identifier"), []byte("zone-transfer"), 0644)
	_ = os.WriteFile(filepath.Join(dir, ".autopilot", "legacy_runner.py"), []byte("print('legacy')\n"), 0644)
	_ = os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Iazio Repo\n"), 0644)

	runGit("add", ".")
	runGit("commit", "-m", "initial commit with legacy artifacts")
	return dir
}

func buildMultiModeMockAgy(t *testing.T) string {
	t.Helper()
	tmpDir := t.TempDir()
	srcPath := filepath.Join(tmpDir, "main.go")
	binPath := filepath.Join(tmpDir, "mock-agy")

	src := `package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

func main() {
	jobID := flag.String("job-id", "", "Job ID")
	action := flag.String("action", "", "Action")
	storyID := flag.String("story-id", "", "Story ID")
	flag.Parse()

	mode := os.Getenv("MOCK_AGY_MODE")
	switch mode {
	case "BYPASS_NOOP_PRE_SCAFFOLD":
		// Simulates job_0828bc2214e083b5: prints realistic-looking output and exits 0
		// WITHOUT mutating any repo files or committing!
		fmt.Printf("[mock-agy] Loaded pre-scaffolded story %s for job %s\n", *storyID, *jobID)
		fmt.Println("[mock-agy] Exiting 0 without touching repo files...")
		os.Exit(0)

	case "STUCK_INFINITE_HANG":
		// Simulates stuck schedule sch-1532 hanging forever
		fmt.Println("[mock-agy] Hanging on locked resource...")
		time.Sleep(30 * time.Second)

	case "CLEANUP_SUCCESS":
		// Simulates sch-1532 executing actual repo cleanup
		_ = exec.Command("git", "clean", "-ffdx").Run()
		_ = os.WriteFile("CLEANUP_AUDIT.log", []byte("cleaned by "+*jobID+"\n"), 0644)
		fmt.Printf("[mock-agy] Repo cleanup completed for job %s (action=%s)\n", *jobID, *action)
		os.Exit(0)

	case "REAL_STORY_ROOT_245_EXECUTION":
		// Actually executes STORY-ROOT-245: removes WSL artifact and .autopilot script and commits!
		_ = os.Remove("wsl_artifact.Zone.Identifier")
		_ = os.Remove(filepath.Join(".autopilot", "legacy_runner.py"))
		_ = exec.Command("git", "add", "-A").Run()
		_ = exec.Command("git", "commit", "-m", "feat("+*storyID+"): eradicate WSL artifacts and .autopilot scripts").Run()
		fmt.Printf("[mock-agy] Executed %s and committed changes for job %s\n", *storyID, *jobID)
		os.Exit(0)
	}
}
`
	if err := os.WriteFile(srcPath, []byte(src), 0600); err != nil {
		t.Fatalf("write mock-agy source: %v", err)
	}
	if out, err := exec.Command("go", "build", "-o", binPath, srcPath).CombinedOutput(); err != nil {
		t.Fatalf("build mock-agy: %v\n%s", err, string(out))
	}
	return binPath
}

// ============================================================================
// 6. E2E & INTEGRATION TESTS COVERING ALL REPORTED SCENARIOS
// ============================================================================

// Test 1: Reproduces job_0828bc2214e083b5 (STORY-ROOT-245 pre-scaffolded before enqueue)
// and proves the Strict Worktree Delta PoE catches the fake completion, records a Zipkin
// root-cause insight, and succeeds only when retry actually removes the files & commits.
func TestE2E_Job0828bc2214e083b5_PreScaffoldedBypassCaughtAndRetried(t *testing.T) {
	repoDir := initTestGitRepo(t)
	mockAgy := buildMultiModeMockAgy(t)
	zipkin, zipkinSrv := NewZipkinCollectorServer()
	defer zipkinSrv.Close()

	agentCtrl := &AgentWorkspaceController{
		AgentID:   "mac-mini",
		RepoPaths: map[string]string{".": repoDir},
	}
	cp := NewIazioControlPlane(agentCtrl, zipkin, 5*time.Second)
	apiSrv := httptest.NewServer(cp.BuildHTTPHandler())
	defer apiSrv.Close()

	// Simulate caller pre-scaffolding docs/stories/story-root-245.story.md BEFORE enqueueing
	storiesDir := filepath.Join(repoDir, "docs", "stories")
	_ = os.MkdirAll(storiesDir, 0755)
	_ = os.WriteFile(filepath.Join(storiesDir, "story-root-245.story.md"),
		[]byte("# STORY-ROOT-245: Eradicate Root WSL Artifacts\nStatus: In Progress\n"), 0644)

	// Enqueue with pre-allocated STORY-ROOT-245 (exact signature of job_0828bc2214e083b5)
	job := cp.EnqueueJob(
		"/autonomous-story-execution",
		".",
		"agy",
		"STORY-ROOT-245",
		"Eradicate Root WSL Artifacts, Untrack Binary RAG DB, and Decommission Residual .autopilot Python Scripts",
		"",
		"",
		30,
	)
	_ = job

	// Run 1: mock-agy exits 0 without modifying non-spec repo files
	executedJob, err := cp.ExecuteNextJob(context.Background(), mockAgy, []string{"MOCK_AGY_MODE=BYPASS_NOOP_PRE_SCAFFOLD"})
	if !errors.Is(err, ErrBypassOrNoOpDetected) {
		t.Fatalf("expected ErrBypassOrNoOpDetected for pre-scaffolded no-op run, got: %v", err)
	}
	if executedJob.Status != StatusFailed {
		t.Fatalf("expected status 'failed', got %s", executedJob.Status)
	}

	// Query Zipkin Troubleshooting Insights for the failed job's trace_id
	resp, err := http.Get(fmt.Sprintf("%s/api/v1/observability/traces/%s/insights", apiSrv.URL, executedJob.TraceID))
	if err != nil {
		t.Fatalf("failed to query trace insights: %v", err)
	}
	var insight TroubleshootingInsight
	_ = json.NewDecoder(resp.Body).Decode(&insight)
	_ = resp.Body.Close()

	if insight.RootCauseCategory != "PRE_SCAFFOLD_NOOP_BYPASS" {
		t.Fatalf("expected Zipkin insight RootCauseCategory='PRE_SCAFFOLD_NOOP_BYPASS', got %q", insight.RootCauseCategory)
	}

	// Trigger Retry via UI Button Endpoint: POST /api/v1/global-queue/jobs/{id}/retry
	retryResp, err := http.Post(fmt.Sprintf("%s/api/v1/global-queue/jobs/%s/retry", apiSrv.URL, executedJob.ID), "application/json", nil)
	if err != nil {
		t.Fatalf("retry request failed: %v", err)
	}
	var retriedJob Job
	_ = json.NewDecoder(retryResp.Body).Decode(&retriedJob)
	_ = retryResp.Body.Close()

	if retriedJob.ID == executedJob.ID || retriedJob.RetryOfJobID != executedJob.ID {
		t.Fatalf("expected brand-new job ID linked via RetryOfJobID, got id=%s retry_of=%s", retriedJob.ID, retriedJob.RetryOfJobID)
	}

	// Run 2: mock-agy executes REAL_STORY_ROOT_245_EXECUTION (deletes WSL + .autopilot files & commits)
	completedJob, err := cp.ExecuteNextJob(context.Background(), mockAgy, []string{"MOCK_AGY_MODE=REAL_STORY_ROOT_245_EXECUTION"})
	if err != nil {
		t.Fatalf("expected real execution to pass PoE, got error: %v", err)
	}
	if completedJob.Status != StatusCompleted {
		t.Fatalf("expected status 'completed', got %s", completedJob.Status)
	}
	if completedJob.PreSnapshot.HeadSHA == completedJob.PostSnapshot.HeadSHA {
		t.Fatalf("expected HEAD commit SHA to advance after real execution")
	}
}

// Test 2: Reproduces stuck schedule sch-1532 (/global-queue/schedules/sch-1532)
// and proves Force-Unstick / Watchdog Reconciliation + Trigger Now completes cleanly.
func TestE2E_ScheduleSch1532_StuckRecoveryAndCleanupCompletion(t *testing.T) {
	repoDir := initTestGitRepo(t)
	mockAgy := buildMultiModeMockAgy(t)
	zipkin, zipkinSrv := NewZipkinCollectorServer()
	defer zipkinSrv.Close()

	agentCtrl := &AgentWorkspaceController{
		AgentID:   "mac-mini",
		RepoPaths: map[string]string{"iazio-test-repo": repoDir},
	}
	cp := NewIazioControlPlane(agentCtrl, zipkin, 5*time.Second)
	cp.schedules["sch-1532"] = &Schedule{
		ID:             "sch-1532",
		Name:           "Repository Code & Worktree Cleanup",
		TargetAgent:    "agy",
		TargetRepo:     "iazio-test-repo",
		Action:         "/repo-cleanup",
		Enabled:        true,
		Status:         "idle",
		MaxDurationSec: 1, // 1s ceiling for test speed
	}
	apiSrv := httptest.NewServer(cp.BuildHTTPHandler())
	defer apiSrv.Close()

	// 1. Trigger sch-1532 when a stale .git/index.lock exists and mock-agy hangs
	_ = os.WriteFile(filepath.Join(repoDir, ".git", "index.lock"), []byte("stale-lock"), 0644)
	trigResp, _ := http.Post(apiSrv.URL+"/api/v1/global-queue/schedules/sch-1532/actions/trigger", "application/json", nil)
	_ = trigResp.Body.Close()

	// Execute with STUCK_INFINITE_HANG — bounded by sch-1532's MaxDurationSec (1s)
	_, err := cp.ExecuteNextJob(context.Background(), mockAgy, []string{"MOCK_AGY_MODE=STUCK_INFINITE_HANG"})
	if err == nil {
		t.Fatalf("expected hung schedule job to be killed by schedule timeout")
	}

	// 2. Call Force Unstick / Reconcile button endpoint on sch-1532
	unstickResp, _ := http.Post(apiSrv.URL+"/api/v1/global-queue/schedules/sch-1532/actions/unstick", "application/json", nil)
	var unstuckSch Schedule
	_ = json.NewDecoder(unstickResp.Body).Decode(&unstuckSch)
	_ = unstickResp.Body.Close()

	if unstuckSch.Status != "idle" || unstuckSch.ActiveJobID != "" {
		t.Fatalf("expected sch-1532 to be reconciled to 'idle' with empty ActiveJobID, got status=%s active=%s",
			unstuckSch.Status, unstuckSch.ActiveJobID)
	}

	// 3. Trigger sch-1532 again with healthy CLEANUP_SUCCESS mode
	cp.schedules["sch-1532"].MaxDurationSec = 10
	trigResp2, _ := http.Post(apiSrv.URL+"/api/v1/global-queue/schedules/sch-1532/actions/trigger", "application/json", nil)
	_ = trigResp2.Body.Close()

	cleanJob, err := cp.ExecuteNextJob(context.Background(), mockAgy, []string{"MOCK_AGY_MODE=CLEANUP_SUCCESS"})
	if err != nil || cleanJob.Status != StatusCompleted {
		t.Fatalf("expected sch-1532 cleanup run to complete, got status=%v err=%v", cleanJob.Status, err)
	}

	// 4. Verify GET /api/v1/global-queue/schedules reconciles sch-1532 CompletedRuns = 1 and Status = idle
	schedListResp, _ := http.Get(apiSrv.URL + "/api/v1/global-queue/schedules")
	var schedules []Schedule
	_ = json.NewDecoder(schedListResp.Body).Decode(&schedules)
	_ = schedListResp.Body.Close()

	if len(schedules) != 1 || schedules[0].CompletedRuns != 1 || schedules[0].Status != "idle" {
		t.Fatalf("expected sch-1532 CompletedRuns=1 and Status='idle', got %+v", schedules)
	}
}

// Test 3: Tests the "Discard Changes" button on /agents/mac-mini/iazio-test-repo/queue
// plus Pause/Resume Schedule, Cancel Job, and /global-queue/history & /jobs views.
func TestE2E_AgentRepoQueue_DiscardChangesButton_AndAllQueueButtons(t *testing.T) {
	repoDir := initTestGitRepo(t)
	zipkin, zipkinSrv := NewZipkinCollectorServer()
	defer zipkinSrv.Close()

	agentCtrl := &AgentWorkspaceController{
		AgentID:   "mac-mini",
		RepoPaths: map[string]string{"iazio-test-repo": repoDir},
	}
	cp := NewIazioControlPlane(agentCtrl, zipkin, 5*time.Second)
	cp.schedules["sch-1532"] = &Schedule{
		ID:          "sch-1532",
		Name:        "Repo Cleanup",
		TargetAgent: "agy",
		TargetRepo:  "iazio-test-repo",
		Action:      "/repo-cleanup",
		Enabled:     true,
		Status:      "idle",
	}
	apiSrv := httptest.NewServer(cp.BuildHTTPHandler())
	defer apiSrv.Close()

	// Dirty the repo with modified tracked files, untracked files, AND a stale .git/index.lock
	_ = os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("CORRUPTED DIRTY CONTENT\n"), 0644)
	_ = os.WriteFile(filepath.Join(repoDir, "untracked_temp.tmp"), []byte("garbage\n"), 0644)
	_ = os.WriteFile(filepath.Join(repoDir, ".git", "index.lock"), []byte("locked"), 0644)

	dirtySnap, _ := CaptureWorktreeSnapshot(repoDir)
	if dirtySnap.DirtyFileCount == 0 || !dirtySnap.HasIndexLock {
		t.Fatalf("expected dirty repo with index.lock before clicking Discard Changes")
	}

	// Click "Discard Changes" button: POST /api/v1/agents/mac-mini/iazio-test-repo/actions/discard-changes
	discardResp, err := http.Post(apiSrv.URL+"/api/v1/agents/mac-mini/iazio-test-repo/actions/discard-changes", "application/json", nil)
	if err != nil {
		t.Fatalf("discard-changes button call failed: %v", err)
	}
	var discardResult struct {
		Clean          bool   `json:"clean"`
		DirtyFileCount int    `json:"dirty_file_count"`
		HeadSHA        string `json:"head_sha"`
		TraceID        string `json:"trace_id"`
	}
	_ = json.NewDecoder(discardResp.Body).Decode(&discardResult)
	_ = discardResp.Body.Close()

	if !discardResult.Clean || discardResult.DirtyFileCount != 0 {
		t.Fatalf("expected Discard Changes button to leave clean worktree, got %+v", discardResult)
	}

	// Verify README.md was restored and untracked_temp.tmp + .git/index.lock are gone
	readmeBytes, _ := os.ReadFile(filepath.Join(repoDir, "README.md"))
	if string(readmeBytes) != "# Iazio Repo\n" {
		t.Fatalf("README.md was not reset by Discard Changes: %q", string(readmeBytes))
	}
	if _, err := os.Stat(filepath.Join(repoDir, "untracked_temp.tmp")); !os.IsNotExist(err) {
		t.Fatalf("expected untracked_temp.tmp to be deleted by Discard Changes")
	}

	// Test Cancel Job button on /global-queue/jobs
	pendingJob := cp.EnqueueJob("/autonomous-story-execution", "iazio-test-repo", "agy", "STORY-999", "Test Cancel Button", "", "", 60)
	cancelResp, _ := http.Post(fmt.Sprintf("%s/api/v1/global-queue/jobs/%s/cancel", apiSrv.URL, pendingJob.ID), "application/json", nil)
	var cancelledJob Job
	_ = json.NewDecoder(cancelResp.Body).Decode(&cancelledJob)
	_ = cancelResp.Body.Close()
	if cancelledJob.Status != StatusCancelled {
		t.Fatalf("expected Cancelled status after clicking Cancel button, got %s", cancelledJob.Status)
	}

	// Test Pause & Resume Schedule buttons on /global-queue/schedules/sch-1532
	pauseResp, _ := http.Post(apiSrv.URL+"/api/v1/global-queue/schedules/sch-1532/actions/pause", "application/json", nil)
	var pausedSch Schedule
	_ = json.NewDecoder(pauseResp.Body).Decode(&pausedSch)
	_ = pauseResp.Body.Close()
	if pausedSch.Status != "paused" || pausedSch.Enabled {
		t.Fatalf("expected paused schedule, got %+v", pausedSch)
	}

	resumeResp, _ := http.Post(apiSrv.URL+"/api/v1/global-queue/schedules/sch-1532/actions/resume", "application/json", nil)
	var resumedSch Schedule
	_ = json.NewDecoder(resumeResp.Body).Decode(&resumedSch)
	_ = resumeResp.Body.Close()
	if resumedSch.Status != "idle" || !resumedSch.Enabled {
		t.Fatalf("expected idle/enabled schedule after resume, got %+v", resumedSch)
	}

	// Verify /global-queue/history recorded all actions with Zipkin trace IDs
	histResp, _ := http.Get(apiSrv.URL + "/api/v1/global-queue/history")
	var history []HistoryEntry
	_ = json.NewDecoder(histResp.Body).Decode(&history)
	_ = histResp.Body.Close()
	if len(history) < 3 {
		t.Fatalf("expected audit events in /global-queue/history, got %d", len(history))
	}
}

// Test 4: Tests the "Discard All" functionality across all repositories on an agent (STORY-GOV-3729).
// Verifies that when an in-progress job is running and multiple repositories are dirty or locked,
// calling POST /v1/hosts/{agent}/repos/discard-all with force=true aborts in-progress jobs,
// removes index locks, restores all tracked files, removes all untracked files, and leaves all repos clean.
func TestE2E_AgentRepo_DiscardAllRepos_AndInProgressJobCancellation(t *testing.T) {
	repoDirA := initTestGitRepo(t)
	repoDirB := initTestGitRepo(t)
	zipkin, zipkinSrv := NewZipkinCollectorServer()
	defer zipkinSrv.Close()

	agentCtrl := &AgentWorkspaceController{
		AgentID: "mac-mini",
		RepoPaths: map[string]string{
			"repo-alpha": repoDirA,
			"repo-beta":  repoDirB,
		},
	}
	cp := NewIazioControlPlane(agentCtrl, zipkin, 5*time.Second)
	apiSrv := httptest.NewServer(cp.BuildHTTPHandler())
	defer apiSrv.Close()

	// 1. Enqueue a job on repo-alpha and mark it StatusRunning
	runningJob := cp.EnqueueJob("/autonomous-story-execution", "repo-alpha", "agy", "STORY-GOV-3729", "In-flight task", "", "", 120)
	cp.mu.Lock()
	cp.jobs[runningJob.ID].Status = StatusRunning
	cp.mu.Unlock()

	// 2. Dirty repo-alpha
	_ = os.WriteFile(filepath.Join(repoDirA, "README.md"), []byte("DIRTY ALPHA CONTENT\n"), 0644)
	_ = os.WriteFile(filepath.Join(repoDirA, "untracked_alpha.tmp"), []byte("junk A\n"), 0644)

	// 3. Dirty repo-beta with untracked files and a stale .git/index.lock
	_ = os.WriteFile(filepath.Join(repoDirB, "README.md"), []byte("DIRTY BETA CONTENT\n"), 0644)
	_ = os.WriteFile(filepath.Join(repoDirB, "untracked_beta.tmp"), []byte("junk B\n"), 0644)
	_ = os.WriteFile(filepath.Join(repoDirB, ".git", "index.lock"), []byte("locked"), 0644)

	snapA, _ := CaptureWorktreeSnapshot(repoDirA)
	snapB, _ := CaptureWorktreeSnapshot(repoDirB)
	if snapA.DirtyFileCount == 0 || snapB.DirtyFileCount == 0 || !snapB.HasIndexLock {
		t.Fatalf("expected both repos to be dirty before Discard All")
	}

	// 4. Trigger Discard All via POST /v1/hosts/mac-mini/repos/discard-all?force=true
	discardResp, err := http.Post(apiSrv.URL+"/v1/hosts/mac-mini/repos/discard-all?force=true", "application/json", nil)
	if err != nil {
		t.Fatalf("discard-all call failed: %v", err)
	}
	defer discardResp.Body.Close()

	if discardResp.StatusCode != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d", discardResp.StatusCode)
	}

	var res struct {
		Count int      `json:"count"`
		Repos []string `json:"repos"`
	}
	if err := json.NewDecoder(discardResp.Body).Decode(&res); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if res.Count != 2 || len(res.Repos) != 2 {
		t.Fatalf("expected count 2 and 2 repos, got %+v", res)
	}

	// 5. Verify the in-progress job was cancelled / aborted
	cp.mu.Lock()
	j := cp.jobs[runningJob.ID]
	cp.mu.Unlock()
	if j.Status != StatusCancelled {
		t.Fatalf("expected in-progress job to be cancelled, got %s", j.Status)
	}

	// 6. Verify worktrees are clean for both repos
	snapCleanA, err := CaptureWorktreeSnapshot(repoDirA)
	if err != nil {
		t.Fatalf("failed to capture snapA: %v", err)
	}
	if snapCleanA.DirtyFileCount != 0 || snapCleanA.HasIndexLock {
		t.Fatalf("expected repoA to be completely clean, got %+v", snapCleanA)
	}

	snapCleanB, err := CaptureWorktreeSnapshot(repoDirB)
	if err != nil {
		t.Fatalf("failed to capture snapB: %v", err)
	}
	if snapCleanB.DirtyFileCount != 0 || snapCleanB.HasIndexLock {
		t.Fatalf("expected repoB to be completely clean, got %+v", snapCleanB)
	}

	// 7. Verify file contents restored and untracked files deleted
	readmeA, _ := os.ReadFile(filepath.Join(repoDirA, "README.md"))
	if string(readmeA) != "# Iazio Repo\n" {
		t.Fatalf("repoA README.md not restored: %q", string(readmeA))
	}
	if _, err := os.Stat(filepath.Join(repoDirA, "untracked_alpha.tmp")); !os.IsNotExist(err) {
		t.Fatalf("expected untracked_alpha.tmp to be deleted")
	}

	readmeB, _ := os.ReadFile(filepath.Join(repoDirB, "README.md"))
	if string(readmeB) != "# Iazio Repo\n" {
		t.Fatalf("repoB README.md not restored: %q", string(readmeB))
	}
	if _, err := os.Stat(filepath.Join(repoDirB, "untracked_beta.tmp")); !os.IsNotExist(err) {
		t.Fatalf("expected untracked_beta.tmp to be deleted")
	}
}

// Test 5: Pure Code Action execution: Discard and Clean Git without AI model invocation
func TestE2E_IazioHarness_PureCodeAction_DiscardAndClean(t *testing.T) {
	repoDir := initTestGitRepo(t)

	// Dirty the repo with modified tracked file, untracked file, untracked folder, and stale lock
	_ = os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("# Overwritten uncommitted\n"), 0644)
	_ = os.WriteFile(filepath.Join(repoDir, "new_untracked_file.go"), []byte("package main\n"), 0644)
	subDir := filepath.Join(repoDir, "untracked_dir", "nested")
	_ = os.MkdirAll(subDir, 0755)
	_ = os.WriteFile(filepath.Join(subDir, "untracked_leaf.txt"), []byte("data"), 0644)
	_ = os.WriteFile(filepath.Join(repoDir, ".git", "index.lock"), []byte("lock content"), 0644)

	snapDirty, _ := CaptureWorktreeSnapshot(repoDir)
	if snapDirty.DirtyFileCount == 0 {
		t.Fatalf("expected dirty repo before discard, got %+v", snapDirty)
	}

	var progressStages []string
	res, err := gitclean.DiscardWorkspace(context.Background(), gitclean.DiscardOptions{
		WorktreePath: repoDir,
		Force:        true,
		Submodules:   true,
		RemoveLocks:  true,
		OnProgress: func(stage, detail string) {
			progressStages = append(progressStages, stage)
		},
	})
	if err != nil {
		t.Fatalf("DiscardWorkspace failed: %v", err)
	}

	if !res.Success || !res.Clean {
		t.Fatalf("expected res.Success and res.Clean to be true, got %+v", res)
	}
	if res.RemainingPorcelain != "" {
		t.Fatalf("expected empty remaining porcelain, got: %s", res.RemainingPorcelain)
	}

	// Verify post-condition: 100% clean
	snapClean, _ := CaptureWorktreeSnapshot(repoDir)
	if snapClean.DirtyFileCount != 0 || snapClean.HasIndexLock {
		t.Fatalf("expected clean worktree after discard, got %+v", snapClean)
	}

	// Verify file content restored
	readmeBytes, _ := os.ReadFile(filepath.Join(repoDir, "README.md"))
	if string(readmeBytes) != "# Iazio Repo\n" {
		t.Fatalf("README.md was not reset: %s", string(readmeBytes))
	}

	// Verify untracked items removed
	if _, err := os.Stat(filepath.Join(repoDir, "new_untracked_file.go")); !os.IsNotExist(err) {
		t.Fatalf("expected new_untracked_file.go to be deleted")
	}
	if _, err := os.Stat(filepath.Join(repoDir, "untracked_dir")); !os.IsNotExist(err) {
		t.Fatalf("expected untracked_dir to be deleted")
	}
}
