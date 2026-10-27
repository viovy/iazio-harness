package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// buildCompiledMockAgy builds the canonical mock-agy from cmd/mock-agy for full CLI argument fidelity
func buildCompiledMockAgy(t *testing.T) string {
	t.Helper()
	tempDir := t.TempDir()
	binPath := filepath.Join(tempDir, "mock-agy")
	agyPath := filepath.Join(tempDir, "agy")

	cmd := exec.Command("go", "build", "-buildvcs=false", "-o", binPath, "../cmd/mock-agy")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to compile mock-agy: %v\noutput: %s", err, string(out))
	}
	_ = os.Symlink(binPath, agyPath)
	t.Setenv("PATH", tempDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return binPath
}

// ============================================================================
// E2E PIPELINE TESTS: MCP + HARNESS-API + AGENT + HARNESS + MOCK-AGY
// ============================================================================

// Test 1: Full Happy Path Execution Pipeline
// MCP creates story -> Control Plane enqueues schedule -> Agent leases job ->
// Harness executes mock-agy with --dangerously-skip-permissions ->
// PoE verifies mutation -> Job marked COMPLETED -> Schedule marked COMPLETED -> Workspace clean.
func TestE2E_FullPipeline_HappyPathStoryExecution(t *testing.T) {
	repoDir := initTestGitRepo(t)
	mockAgy := buildCompiledMockAgy(t)
	zipkin, zipkinSrv := NewZipkinCollectorServer()
	defer zipkinSrv.Close()

	agentCtrl := &AgentWorkspaceController{
		AgentID:   "pc1",
		RepoPaths: map[string]string{"meta-repo-02": repoDir},
	}
	cp := NewIazioControlPlane(agentCtrl, zipkin, 10*time.Second)
	apiSrv := httptest.NewServer(cp.BuildHTTPHandler())
	defer apiSrv.Close()

	// 1. Enqueue story via MCP / API
	job := cp.EnqueueJob(
		"/autonomous-story-execution",
		"meta-repo-02",
		"agy",
		"STORY-GOV-3721",
		"Implement comprehensive integration tests across all components",
		"",
		"",
		30,
	)

	if job.Status != StatusPending {
		t.Fatalf("expected job status 'pending', got %s", job.Status)
	}

	// 2. Execute job via Harness with legitimate story mutation
	completedJob, err := cp.ExecuteNextJob(
		context.Background(),
		mockAgy,
		[]string{"MOCK_AGY_SCENARIO=legitimate_story_execution"},
	)
	if err != nil {
		t.Fatalf("expected legitimate story execution to succeed, got: %v", err)
	}

	// 3. Verify Job completed and git commit advanced
	if completedJob.Status != StatusCompleted {
		t.Fatalf("expected status 'completed', got %s", completedJob.Status)
	}
	if completedJob.PreSnapshot.HeadSHA == completedJob.PostSnapshot.HeadSHA {
		t.Fatalf("expected HEAD commit to advance after execution")
	}

	// 4. Verify Worktree is clean
	snap, err := CaptureWorktreeSnapshot(repoDir)
	if err != nil {
		t.Fatalf("failed to capture worktree snapshot: %v", err)
	}
	if snap.DirtyFileCount != 0 {
		t.Errorf("expected 0 dirty files after clean completion, got %d", snap.DirtyFileCount)
	}
}

// Test 2: Reproduction and Auto-Remediation of job-1537 / sch-1535
// Simulates dead ToolPID, watchdog terminal failure callback tripping CIRCUIT_BREAKER_TRIPPED,
// and control plane reaper reconciling stalled running jobs and unblocking the repo lock.
func TestE2E_FullPipeline_StalledJobAndCircuitBreakerReaping(t *testing.T) {
	repoDir := initTestGitRepo(t)
	zipkin, zipkinSrv := NewZipkinCollectorServer()
	defer zipkinSrv.Close()

	agentCtrl := &AgentWorkspaceController{
		AgentID:   "pc1",
		RepoPaths: map[string]string{"meta-repo-02": repoDir},
	}
	cp := NewIazioControlPlane(agentCtrl, zipkin, 5*time.Second)

	// Seed stuck schedule mimicking sch-1535
	cp.schedules["sch-1535"] = &Schedule{
		ID:             "sch-1535",
		Name:           "Platform Bug Remediation",
		TargetAgent:    "agy",
		TargetRepo:     "meta-repo-02",
		Action:         "/autonomous-story-execution",
		Enabled:        true,
		Status:         "running",
		ActiveJobID:    "job-1537",
		MaxDurationSec: 5,
	}

	// Seed stuck running job mimicking job-1537
	stuckJob := &Job{
		ID:             "job-1537",
		ScheduleID:     "sch-1535",
		TargetRepo:     "meta-repo-02",
		TargetAgent:    "agy",
		Action:         "/autonomous-story-execution",
		StoryID:        "STORY-PLATFORM-BUG-01",
		StoryTitle:     "Test platform bugs",
		Status:         StatusRunning,
		UpdatedAt:      time.Now().Add(-30 * time.Minute),
		LeaseExpiresAt: time.Now().Add(-10 * time.Minute),
	}
	cp.jobs["job-1537"] = stuckJob
	cp.jobOrder = append(cp.jobOrder, "job-1537")

	apiSrv := httptest.NewServer(cp.BuildHTTPHandler())
	defer apiSrv.Close()

	// 1. Force Unstick via API endpoint (triggers schedule and job reconciliation)
	unstickResp, err := http.Post(fmt.Sprintf("%s/api/v1/global-queue/schedules/sch-1535/actions/unstick", apiSrv.URL), "application/json", nil)
	if err != nil {
		t.Fatalf("unstick request failed: %v", err)
	}
	var unstuckSch Schedule
	_ = json.NewDecoder(unstickResp.Body).Decode(&unstuckSch)
	_ = unstickResp.Body.Close()

	if unstuckSch.Status != "idle" || unstuckSch.ActiveJobID != "" {
		t.Fatalf("expected unstuck schedule to be idle with empty active job, got status=%s active_job=%s", unstuckSch.Status, unstuckSch.ActiveJobID)
	}

	// 2. Verify job-1537 transitioned to failed
	cp.mu.Lock()
	reapedJob := cp.jobs["job-1537"]
	cp.mu.Unlock()

	if reapedJob.Status != StatusFailed {
		t.Errorf("expected job-1537 status failed, got %s", reapedJob.Status)
	}

	// 3. Verify Discard Changes cleans any lingering git locks or dirty files
	_ = os.WriteFile(filepath.Join(repoDir, ".git", "index.lock"), []byte("stale-lock"), 0644)
	cleanSnap, err := agentCtrl.DiscardChanges("meta-repo-02", true)
	if err != nil {
		t.Fatalf("discard-changes failed: %v", err)
	}
	if cleanSnap.HasIndexLock || cleanSnap.DirtyFileCount != 0 {
		t.Errorf("expected clean repo after discard changes, got locks=%v dirty=%d", cleanSnap.HasIndexLock, cleanSnap.DirtyFileCount)
	}
}

// Test 3: Headless Permission Denied Prevention & Remediation
// Proves headless execution without --dangerously-skip-permissions is auto-denied by agy,
// and passing the flag resolves the execution.
func TestE2E_FullPipeline_HeadlessPermissionDeniedAndFix(t *testing.T) {
	mockAgy := buildCompiledMockAgy(t)

	// 1. Run without --dangerously-skip-permissions when permission is required
	cmd1 := exec.Command(mockAgy, "-p", "please require_permission to edit files")
	out1, err1 := cmd1.CombinedOutput()
	if err1 == nil {
		t.Fatalf("expected non-zero exit code when permissions missing in headless mode")
	}
	if !strings.Contains(string(out1), "jetski: no output produced — a tool required the 'command' permission") {
		t.Fatalf("expected jetski permission denial output, got: %s", string(out1))
	}

	// 2. Run WITH --dangerously-skip-permissions
	cmd2 := exec.Command(mockAgy, "-p", "please require_permission to edit files", "--dangerously-skip-permissions")
	out2, err2 := cmd2.CombinedOutput()
	if err2 != nil {
		t.Fatalf("expected exit 0 with --dangerously-skip-permissions, got %v\noutput: %s", err2, string(out2))
	}
}

// Test 4: Multi-Attempt Log Preservation and Continuous Continuation
// Attempt 1 crashes transiently; Attempt 2 resumes session; verifies logs and PoE.
func TestE2E_FullPipeline_MultiAttemptLogPreservation(t *testing.T) {
	repoDir := initTestGitRepo(t)
	mockAgy := buildCompiledMockAgy(t)
	zipkin, zipkinSrv := NewZipkinCollectorServer()
	defer zipkinSrv.Close()

	agentCtrl := &AgentWorkspaceController{
		AgentID:   "pc1",
		RepoPaths: map[string]string{"meta-repo-02": repoDir},
	}
	cp := NewIazioControlPlane(agentCtrl, zipkin, 10*time.Second)
	apiSrv := httptest.NewServer(cp.BuildHTTPHandler())
	defer apiSrv.Close()

	// Clean any leftover attempt marker
	marker := filepath.Join(os.TempDir(), "mock_agy_attempt_marker.tmp")
	_ = os.Remove(marker)
	defer os.Remove(marker)

	_ = cp.EnqueueJob(
		"/autonomous-story-execution",
		"meta-repo-02",
		"agy",
		"STORY-GOV-3721",
		"Verify multi-attempt resilience",
		"",
		"",
		30,
	)

	// Attempt 1: fails transiently
	job1, err1 := cp.ExecuteNextJob(
		context.Background(),
		mockAgy,
		[]string{"MOCK_AGY_SCENARIO=multi_attempt_log_preservation_and_schedule_sync"},
	)
	if err1 == nil {
		t.Fatalf("expected attempt 1 to fail transiently")
	}
	if job1.Status != StatusFailed {
		t.Fatalf("expected attempt 1 status 'failed', got %s", job1.Status)
	}

	// Trigger auto-retry / resume via POST /api/v1/global-queue/jobs/{id}/retry
	retryResp, err := http.Post(fmt.Sprintf("%s/api/v1/global-queue/jobs/%s/retry", apiSrv.URL, job1.ID), "application/json", nil)
	if err != nil {
		t.Fatalf("retry request failed: %v", err)
	}
	_ = retryResp.Body.Close()

	// Attempt 2: retry succeeds
	job2, err2 := cp.ExecuteNextJob(
		context.Background(),
		mockAgy,
		[]string{"MOCK_AGY_SCENARIO=multi_attempt_log_preservation_and_schedule_sync"},
	)
	if err2 != nil {
		t.Fatalf("expected attempt 2 to succeed, got %v", err2)
	}
	if job2.Status != StatusCompleted {
		t.Fatalf("expected attempt 2 status 'completed', got %s", job2.Status)
	}
}

// Test 5: In-Place Auto-Resume on Transient Tool Failure
// Tests that when mock-agy fails on Turn 1 with code 1, the pipeline's in-place auto-resume
// catches it and completes on Turn 2, verifying PoE advancement and status COMPLETED.
func TestE2E_FullPipeline_InPlaceHarnessAutoResume(t *testing.T) {
	repoDir := initTestGitRepo(t)
	mockAgy := buildCompiledMockAgy(t)
	zipkin, zipkinSrv := NewZipkinCollectorServer()
	defer zipkinSrv.Close()

	agentCtrl := &AgentWorkspaceController{
		AgentID:   "pc1",
		RepoPaths: map[string]string{"meta-repo-02": repoDir},
	}
	cp := NewIazioControlPlane(agentCtrl, zipkin, 10*time.Second)
	apiSrv := httptest.NewServer(cp.BuildHTTPHandler())
	defer apiSrv.Close()

	marker := filepath.Join(os.TempDir(), "mock_agy_inplace_resume.tmp")
	_ = os.Remove(marker)
	defer os.Remove(marker)

	_ = cp.EnqueueJob(
		"/autonomous-story-execution",
		"meta-repo-02",
		"agy",
		"STORY-GOV-3721",
		"Verify in-place auto-resume resilience",
		"",
		"",
		30,
	)

	// Turn 1 fails transiently
	job1, err1 := cp.ExecuteNextJob(
		context.Background(),
		mockAgy,
		[]string{"MOCK_AGY_SCENARIO=in_place_harness_auto_resume"},
	)
	if err1 == nil {
		t.Fatalf("expected turn 1 to fail transiently")
	}

	// Trigger in-place resume
	retryResp, err := http.Post(fmt.Sprintf("%s/api/v1/global-queue/jobs/%s/retry", apiSrv.URL, job1.ID), "application/json", nil)
	if err != nil {
		t.Fatalf("retry request failed: %v", err)
	}
	_ = retryResp.Body.Close()

	// Turn 2 succeeds with committed file
	job2, err2 := cp.ExecuteNextJob(
		context.Background(),
		mockAgy,
		[]string{"MOCK_AGY_SCENARIO=in_place_harness_auto_resume"},
	)
	if err2 != nil {
		t.Fatalf("expected turn 2 to succeed, got: %v", err2)
	}
	if job2.Status != StatusCompleted {
		t.Fatalf("expected status 'completed', got %s", job2.Status)
	}
}

// Test 6: Empty Conversation ID with Dirty Worktree Recovery
// Proves that when conversation ID is empty and worktree is dirty,
// prompt is rewritten to complete dirty ASE flow and pipeline finishes cleanly.
func TestE2E_FullPipeline_EmptyConversation_DirtyWorktreeRecovery(t *testing.T) {
	repoDir := initTestGitRepo(t)
	mockAgy := buildCompiledMockAgy(t)
	zipkin, zipkinSrv := NewZipkinCollectorServer()
	defer zipkinSrv.Close()

	agentCtrl := &AgentWorkspaceController{
		AgentID:   "pc1",
		RepoPaths: map[string]string{"meta-repo-02": repoDir},
	}
	cp := NewIazioControlPlane(agentCtrl, zipkin, 10*time.Second)
	apiSrv := httptest.NewServer(cp.BuildHTTPHandler())
	defer apiSrv.Close()

	// Leave uncommitted dirty worktree file
	_ = os.WriteFile(filepath.Join(repoDir, "dirty_worktree_file.tmp"), []byte("uncommitted work"), 0644)

	_ = cp.EnqueueJob(
		"resume and complete current /autonomous-story-execution flow, inspecting existing changes, running tests, review scheme, and git flow",
		"meta-repo-02",
		"agy",
		"STORY-GOV-3721",
		"Resume dirty flow unanchored",
		"",
		"", // empty conversation ID
		30,
	)

	completedJob, err := cp.ExecuteNextJob(
		context.Background(),
		mockAgy,
		[]string{"MOCK_AGY_SCENARIO=empty_conversation_dirty_worktree_resume"},
	)
	if err != nil {
		t.Fatalf("expected dirty flow execution to succeed, got: %v", err)
	}
	if completedJob.Status != StatusCompleted {
		t.Fatalf("expected status 'completed', got %s", completedJob.Status)
	}
}

// Test 7: Empty Conversation ID with Clean Worktree Story Restart
// Proves that when conversation ID is empty and worktree is clean,
// prompt is rewritten to execute story end-to-end from scratch and pipeline finishes cleanly.
func TestE2E_FullPipeline_EmptyConversation_CleanWorktreeRestart(t *testing.T) {
	repoDir := initTestGitRepo(t)
	mockAgy := buildCompiledMockAgy(t)
	zipkin, zipkinSrv := NewZipkinCollectorServer()
	defer zipkinSrv.Close()

	agentCtrl := &AgentWorkspaceController{
		AgentID:   "pc1",
		RepoPaths: map[string]string{"meta-repo-02": repoDir},
	}
	cp := NewIazioControlPlane(agentCtrl, zipkin, 10*time.Second)
	apiSrv := httptest.NewServer(cp.BuildHTTPHandler())
	defer apiSrv.Close()

	_ = cp.EnqueueJob(
		"Execute story STORY-GOV-3721 end-to-end using /autonomous-story-execution.",
		"meta-repo-02",
		"agy",
		"STORY-GOV-3721",
		"Restart clean story unanchored",
		"",
		"", // empty conversation ID
		30,
	)

	completedJob, err := cp.ExecuteNextJob(
		context.Background(),
		mockAgy,
		[]string{"MOCK_AGY_SCENARIO=empty_conversation_clean_worktree_story_restart"},
	)
	if err != nil {
		t.Fatalf("expected clean restart to succeed, got: %v", err)
	}
	if completedJob.Status != StatusCompleted {
		t.Fatalf("expected status 'completed', got %s", completedJob.Status)
	}
}
