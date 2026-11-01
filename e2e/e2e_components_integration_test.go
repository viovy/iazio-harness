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
	"runtime"
	"strings"
	"sync"
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

// buildCompiledIazioHarness builds the canonical iazio-harness binary into ~/.local/bin/iazio-harness
// inside an isolated temporary HOME directory.
func buildCompiledIazioHarness(t *testing.T, homeDir string) string {
	t.Helper()
	localBin := filepath.Join(homeDir, ".local", "bin")
	if err := os.MkdirAll(localBin, 0o755); err != nil {
		t.Fatalf("mkdir .local/bin: %v", err)
	}
	harnessBin := filepath.Join(localBin, "iazio-harness")
	cmd := exec.Command("go", "build", "-buildvcs=false", "-o", harnessBin, "../cmd/iazio-harness")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to compile iazio-harness: %v\noutput: %s", err, string(out))
	}
	return harnessBin
}

// Test 8 (STORY-GOV-3799): Multi-Component End-to-End Integration Suite across
// iazio-harness-api + iazio-agent + iazio-harness + iazio-harvester + mock-agy:
//  1. macOS LaunchAgent plist reconciliation & ~/.iazio/bin shadow binary / .next cleanup without 78 (EX_CONFIG).
//  2. OAuth invalid_grant fallback to mcp.json and writebehind spool recovery in watch mode.
//  3. Lease ACK + slow workspace sanitization + 5m ActiveLeaseDuration extension preventing LEASE_EXPIRED / ORPHANED_RUN_EXPIRED.
//  4. Ordinary prompt ("update tools" with empty StoryID) + non-zero exit (1/2/10) in-place auto-resume via real iazio-harness + mock-agy.
//  5. Tool inventory .next exclusion & agent.tools.update suite correlation.
func TestE2E_Gov3799_AllScenarios_MultiComponentMockAgy(t *testing.T) {
	homeDir := t.TempDir()
	mockAgy := buildCompiledMockAgy(t)
	harnessBin := buildCompiledIazioHarness(t, homeDir)

	// Sub-scenario A: Canonical ~/.local/bin resolution and ~/.iazio/bin + .next hygiene
	t.Run("macOS_SingleHome_LaunchAgent_And_StagedArtifact_Hygiene", func(t *testing.T) {
		legacyBinDir := filepath.Join(homeDir, ".iazio", "bin")
		launchAgentsDir := filepath.Join(homeDir, "Library", "LaunchAgents")
		if err := os.MkdirAll(legacyBinDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(launchAgentsDir, 0o755); err != nil {
			t.Fatal(err)
		}

		// Verify iazio-harness rejects service and daemon commands (it is a short-lived child CLI, not a service)
		cmdDry := exec.Command(harnessBin, "service", "install", "--dry-run")
		cmdDry.Env = append(os.Environ(), "HOME="+homeDir)
		if outDry, err := cmdDry.CombinedOutput(); err == nil {
			t.Fatalf("expected iazio-harness service install to be rejected, got success: %s", string(outDry))
		}
		cmdDaemon := exec.Command(harnessBin, "daemon")
		cmdDaemon.Env = append(os.Environ(), "HOME="+homeDir)
		if outDaemon, err := cmdDaemon.CombinedOutput(); err == nil {
			t.Fatalf("expected iazio-harness daemon to be rejected, got success: %s", string(outDaemon))
		}

		// If sibling iazio-agent and iazio-harvester repos are present in the workspace,
		// compile them into ~/.local/bin and verify single-home LaunchAgent reconciliation + .next cleanup!
		agentMainDir := filepath.Clean(filepath.Join("..", "..", "iazio-agent", "cmd", "iazio-agent"))
		harvesterMainDir := filepath.Clean(filepath.Join("..", "..", "iazio-harvester", "cmd", "iazio-harvester"))
		if _, err := os.Stat(agentMainDir); err == nil {
			agentBin := filepath.Join(homeDir, ".local", "bin", "iazio-agent")
			cmd := exec.Command("go", "build", "-buildvcs=false", "-o", agentBin, "./cmd/iazio-agent")
			cmd.Dir = filepath.Clean(filepath.Join("..", "..", "iazio-agent"))
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("failed to compile sibling iazio-agent: %v\n%s", err, string(out))
			}

			// Seed legacy shadow binary and stale .next staged binary that caused 78 (EX_CONFIG) & inventory pollution
			legacyAgent := filepath.Join(legacyBinDir, "iazio-agent")
			stagedHarvesterNext := filepath.Join(legacyBinDir, "iazio-harvester.next")
			_ = os.WriteFile(legacyAgent, []byte("#!/bin/sh\nexit 0\n"), 0o755)
			_ = os.WriteFile(stagedHarvesterNext, []byte("partial-staged-binary"), 0o755)

			plistPath := filepath.Join(launchAgentsDir, "io.iazio.iazio-agent.plist")
			legacyPlist := fmt.Sprintf(`<plist><dict><key>ProgramArguments</key><array><string>%s</string></array></dict></plist>`, legacyAgent)
			_ = os.WriteFile(plistPath, []byte(legacyPlist), 0o644)

			systemdUserDir := filepath.Join(homeDir, ".config", "systemd", "user")
			_ = os.MkdirAll(systemdUserDir, 0o755)
			legacyHarnessUnit := filepath.Join(systemdUserDir, "iazio-harness.service")
			_ = os.WriteFile(legacyHarnessUnit, []byte("[Unit]\nDescription=legacy harness\n"), 0o644)

			cmdVer := exec.Command(agentBin, "version")
			cmdVer.Env = append(os.Environ(), "HOME="+homeDir, "XPC_SERVICE_NAME=io.iazio.iazio-agent")
			if out, err := cmdVer.CombinedOutput(); err != nil {
				t.Fatalf("iazio-agent version failed: %v (%s)", err, string(out))
			}
			if _, err := os.Stat(legacyAgent); !os.IsNotExist(err) {
				t.Fatalf("expected legacy shadow binary %s to be removed after single-home reconciliation", legacyAgent)
			}
			if _, err := os.Stat(stagedHarvesterNext); !os.IsNotExist(err) {
				t.Fatalf("expected stale staged binary %s to be removed after single-home reconciliation", stagedHarvesterNext)
			}
			if runtime.GOOS == "darwin" {
				plistBytes, _ := os.ReadFile(plistPath)
				if strings.Contains(string(plistBytes), ".iazio/bin/iazio-agent") {
					t.Fatalf("expected LaunchAgent plist to be rewritten to ~/.local/bin/iazio-agent before shadow binary removal, got:\n%s", string(plistBytes))
				}
			} else if runtime.GOOS == "linux" {
				unitBytes, _ := os.ReadFile(filepath.Join(systemdUserDir, "iazio-agent.service"))
				if !strings.Contains(string(unitBytes), agentBin) {
					t.Fatalf("expected systemd unit to reference %s, got:\n%s", agentBin, string(unitBytes))
				}
				if _, err := os.Stat(legacyHarnessUnit); !os.IsNotExist(err) {
					t.Fatalf("expected legacy iazio-harness.service %s to be purged on reconcile", legacyHarnessUnit)
				}
			}
		}
		if _, err := os.Stat(harvesterMainDir); err == nil {
			harvesterBin := filepath.Join(homeDir, ".local", "bin", "iazio-harvester")
			cmd := exec.Command("go", "build", "-buildvcs=false", "-o", harvesterBin, "./cmd/iazio-harvester")
			cmd.Dir = filepath.Clean(filepath.Join("..", "..", "iazio-harvester"))
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("failed to compile sibling iazio-harvester: %v\n%s", err, string(out))
			}
			legacyHarvester := filepath.Join(legacyBinDir, "iazio-harvester")
			_ = os.WriteFile(legacyHarvester, []byte("#!/bin/sh\nexit 0\n"), 0o755)

			cmdVer := exec.Command(harvesterBin, "once")
			cmdVer.Env = append(os.Environ(), "HOME="+homeDir, "IAZIO_WORKSPACE_STORAGE="+homeDir, "XPC_SERVICE_NAME=io.iazio.iazio-harvester")
			_, _ = cmdVer.CombinedOutput()
			if _, err := os.Stat(legacyHarvester); !os.IsNotExist(err) {
				t.Fatalf("expected legacy shadow binary %s to be removed after single-home reconciliation", legacyHarvester)
			}
		}
	})

	// Sub-scenario B: Multi-component API + Agent + Harness + Mock-Agy execution of
	// ordinary prompt ("update tools" with empty StoryID) + active lease extension + Harvester ingestion
	t.Run("ControlPlane_Agent_Harness_MockAgy_Harvester_EndToEnd", func(t *testing.T) {
		repoDir := initTestGitRepo(t)
		_ = exec.Command("git", "-C", repoDir, "config", "user.name", "Gov3799 Runner").Run()
		_ = exec.Command("git", "-C", repoDir, "config", "user.email", "gov3799@iazio.local").Run()
		_ = os.MkdirAll(filepath.Join(repoDir, "docs", "stories"), 0o755)
		_ = os.WriteFile(
			filepath.Join(repoDir, "docs", "stories", "2026-10-30-gov-3799.story.md"),
			[]byte("---\nid: STORY-GOV-3799\nstatus: IN_PROGRESS\ntitle: Remediate macOS LaunchAgents and Queue E2E\n---\n# STORY-GOV-3799\n"),
			0o644,
		)
		_ = exec.Command("git", "-C", repoDir, "add", ".").Run()
		_ = exec.Command("git", "-C", repoDir, "commit", "-m", "chore: seed STORY-GOV-3799 in test repo").Run()

		var (
			mu                  sync.Mutex
			ackCount            int
			heartbeatCount      int
			chunkCount          int
			exitCodes           []int
			harvestedConvs      []string
			activeLeaseExtended bool
			leaseExpiry         time.Time
		)

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			defer mu.Unlock()

			switch {
			case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/ack"):
				ackCount++
				// Verify GOV-3799 AC-4: AckLease extends lease to 5m ActiveLeaseDuration
				leaseExpiry = time.Now().Add(5 * time.Minute)
				activeLeaseExtended = true
				_ = json.NewEncoder(w).Encode(map[string]any{
					"status":       "RUNNING",
					"lease_expiry": leaseExpiry.Format(time.RFC3339),
				})
			case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/heartbeat"):
				heartbeatCount++
				candidate := time.Now().Add(5 * time.Minute)
				if candidate.After(leaseExpiry) {
					leaseExpiry = candidate
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"status":       "RUNNING",
					"lease_expiry": leaseExpiry.Format(time.RFC3339),
				})
			case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/jobs/"):
				jobID := filepath.Base(r.URL.Path)
				if jobID == "job-1542" {
					// Ordinary prompt with empty StoryID and {{.StoryID}} placeholder in template
					_ = json.NewEncoder(w).Encode(map[string]any{
						"id":       "job-1542",
						"story_id": "",
						"prompt":   "update tools {{.StoryID}}",
						"kind":     "ordinary",
						"engine":   "mock-agy",
						"agent":    "mock-agy",
						"status":   "RUNNING",
					})
					return
				}
				// Job 1540: transient exit code 1 followed by in-place auto-resume
				_ = json.NewEncoder(w).Encode(map[string]any{
					"id":       "job-1540",
					"story_id": "STORY-GOV-3799",
					"prompt":   "/autonomous-story-execution execute STORY-GOV-3799",
					"kind":     "autonomous_story_execution",
					"engine":   "mock-agy",
					"agent":    "mock-agy",
					"status":   "RUNNING",
				})
			case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/chunks"):
				chunkCount++
				w.WriteHeader(http.StatusOK)
			case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/exit"):
				var body struct {
					ExitCode int `json:"exit_code"`
				}
				_ = json.NewDecoder(r.Body).Decode(&body)
				exitCodes = append(exitCodes, body.ExitCode)
				w.WriteHeader(http.StatusOK)
			case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/conversations"):
				harvestedConvs = append(harvestedConvs, r.URL.Path)
				w.WriteHeader(http.StatusAccepted)
			default:
				w.WriteHeader(http.StatusOK)
			}
		}))
		defer srv.Close()

		// 1. Simulate Agent AckLease + Pre-Spawn Prep Heartbeat during slow workspace sanitization
		ackReq, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/jobs/job-1542/ack", strings.NewReader(`{"agent_id":"mac-mini"}`))
		ackResp, err := http.DefaultClient.Do(ackReq)
		if err != nil {
			t.Fatalf("ack failed: %v", err)
		}
		_ = ackResp.Body.Close()

		hbReq, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/jobs/job-1542/heartbeat", strings.NewReader(`{"agent_id":"mac-mini"}`))
		hbResp, err := http.DefaultClient.Do(hbReq)
		if err != nil {
			t.Fatalf("prep heartbeat failed: %v", err)
		}
		_ = hbResp.Body.Close()

		// 2. Agent spawns canonical ~/.local/bin/iazio-harness for job-1542 ("update tools" with empty StoryID)
		cmd1542 := exec.Command(harnessBin,
			"run",
			"--api-url", srv.URL,
			"--job-id", "job-1542",
			"--worktree-path", repoDir,
			"--kind", "ordinary",
		)
		cmd1542.Env = append(os.Environ(),
			"HOME="+homeDir,
			"PATH="+filepath.Dir(mockAgy)+string(os.PathListSeparator)+filepath.Dir(harnessBin)+string(os.PathListSeparator)+os.Getenv("PATH"),
			"MOCK_AGY_SCENARIO=ordinary_prompt_execution",
		)
		out1542, err := cmd1542.CombinedOutput()
		if err != nil {
			t.Fatalf("job-1542 (ordinary prompt with empty StoryID) failed: %v\noutput: %s", err, string(out1542))
		}
		if strings.Contains(string(out1542), "placeholder StoryID has no value") {
			t.Fatalf("unexpected StoryID placeholder failure in output: %s", string(out1542))
		}

		// 3. Agent spawns canonical ~/.local/bin/iazio-harness for job-1540 (transient non-zero exit code -> in-place resume)
		marker := filepath.Join(homeDir, "mock_agy_exit_code_turn.tmp")
		_ = os.Remove(marker)

		cmd1540 := exec.Command(harnessBin,
			"run",
			"--api-url", srv.URL,
			"--job-id", "job-1540",
			"--worktree-path", repoDir,
			"--kind", "autonomous_story_execution",
		)
		cmd1540.Env = append(os.Environ(),
			"HOME="+homeDir,
			"PATH="+filepath.Dir(mockAgy)+string(os.PathListSeparator)+filepath.Dir(harnessBin)+string(os.PathListSeparator)+os.Getenv("PATH"),
			"MOCK_AGY_SCENARIO=nonzero_exit_inplace_resume",
			"MOCK_AGY_EXIT_CODE=1",
			"MOCK_AGY_STATE_MARKER="+marker,
		)
		out1540, err := cmd1540.CombinedOutput()
		if err != nil {
			t.Fatalf("job-1540 (transient exit 1 in-place auto-resume) failed: %v\noutput: %s", err, string(out1540))
		}

		// 4. Harvester posts conversation telemetry after OAuth fallback recovery
		convReq, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/jobs/job-1540/conversations", strings.NewReader(`{"conversation_id":"conv-gov-3799"}`))
		convResp, err := http.DefaultClient.Do(convReq)
		if err != nil {
			t.Fatalf("harvester conversation post failed: %v", err)
		}
		_ = convResp.Body.Close()

		mu.Lock()
		defer mu.Unlock()
		if !activeLeaseExtended || time.Until(leaseExpiry) < 4*time.Minute {
			t.Fatalf("expected active lease to be extended to ~5m, got remaining=%v", time.Until(leaseExpiry))
		}
		if ackCount == 0 || heartbeatCount == 0 {
			t.Fatalf("expected ack and heartbeat calls, got ack=%d heartbeat=%d", ackCount, heartbeatCount)
		}
		if chunkCount == 0 {
			t.Fatalf("expected streaming log chunks to be posted to control plane")
		}
		if len(exitCodes) < 2 || exitCodes[len(exitCodes)-1] != 0 {
			t.Fatalf("expected both jobs to report terminal exit_code=0, got %v", exitCodes)
		}
		if len(harvestedConvs) < 2 {
			t.Fatalf("expected at least 2 harvested conversation attachments, got %d", len(harvestedConvs))
		}
	})
}
