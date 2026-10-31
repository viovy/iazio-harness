package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/viovy/iazio-harness/internal/controlplane"
	"github.com/viovy/iazio-harness/internal/engines"
)

// buildMockAgy compiles cmd/mock-agy into a temporary directory and sets PATH.
func buildMockAgy(t *testing.T) string {
	t.Helper()
	tempDir := t.TempDir()
	binPath := filepath.Join(tempDir, "mock-agy")
	agyPath := filepath.Join(tempDir, "agy")

	// Preserve Go build caches before isolating HOME
	origGopath := os.Getenv("GOPATH")
	if origGopath == "" {
		if u, err := os.UserHomeDir(); err == nil {
			origGopath = filepath.Join(u, "go")
		}
	}
	origGocache := os.Getenv("GOCACHE")

	// Isolate HOME so local sessionstore / conversation history does not pollute tests
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	if origGopath != "" {
		t.Setenv("GOPATH", origGopath)
	}
	if origGocache != "" {
		t.Setenv("GOCACHE", origGocache)
	}

	// Resolve local source package for mock-agy
	mockAgyPkg := "../mock-agy"
	if _, err := os.Stat(mockAgyPkg); err != nil {
		mockAgyPkg = "github.com/viovy/iazio-harness/cmd/mock-agy"
	}

	cmd := exec.Command("go", "build", "-buildvcs=false", "-o", binPath, mockAgyPkg)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to compile mock-agy: %v\noutput: %s", err, string(out))
	}

	// Create symlink or copy as agy so both 'mock-agy' and 'agy' CLI resolutions invoke this binary
	if err := os.Symlink(binPath, agyPath); err != nil {
		// Fallback to copy if symlink fails
		data, rErr := os.ReadFile(binPath)
		if rErr != nil {
			t.Fatalf("failed to read compiled mock-agy: %v", rErr)
		}
		if wErr := os.WriteFile(agyPath, data, 0o755); wErr != nil {
			t.Fatalf("failed to write agy copy: %v", wErr)
		}
	}

	t.Setenv("PATH", tempDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return binPath
}

func initTestGitWorktree(t *testing.T) string {
	t.Helper()
	worktree := t.TempDir()
	_ = exec.Command("git", "init", worktree).Run()
	_ = exec.Command("git", "-C", worktree, "config", "user.name", "Test User").Run()
	_ = exec.Command("git", "-C", worktree, "config", "user.email", "test@test.local").Run()
	return worktree
}

// Scenario 1: ordinary_prompt_with_mcp_daemons (job-1506 / job-1464)
// mock-agy prints normal output, spawns background child processes that keep stdout/stderr open,
// and exits 0. Harness detects main PID exit 0, drains output, reaps leftover children,
// preserves clean prompt text, and marks job completed without execution_timeout.
func TestMockAgy_Scenario1_OrdinaryPromptWithMCPDaemons(t *testing.T) {
	buildMockAgy(t)
	t.Setenv("MOCK_AGY_SCENARIO", "ordinary_prompt_with_mcp_daemons")

	var mu sync.Mutex
	var postedChunks []string
	var postedExitCode = -1

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/jobs/"):
			_ = json.NewEncoder(w).Encode(controlplane.JobDetail{
				ID:     "job-1506",
				Prompt: "update tools and check MCP health",
				Kind:   "ordinary",
				Engine: "mock-agy",
				Agent:  "mock-agy",
				Status: "RUNNING",
			})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/chunks"):
			var body struct {
				Text string `json:"text"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			postedChunks = append(postedChunks, body.Text)
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/ticks"):
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/exit"):
			var body struct {
				ExitCode int `json:"exit_code"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			postedExitCode = body.ExitCode
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	worktree := initTestGitWorktree(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err := executeJob(ctx, srv.URL, "job-1506", worktree, "", "ordinary", "", "")
	if err != nil {
		t.Fatalf("expected executeJob to succeed, got: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if postedExitCode != 0 {
		t.Fatalf("expected exit code 0 reported to control plane, got %d", postedExitCode)
	}

	allOutput := strings.Join(postedChunks, "\n")
	if strings.Contains(allOutput, "harness-job:") {
		t.Fatalf("expected output to NOT contain harness-job prefix, got: %s", allOutput)
	}
	if !strings.Contains(allOutput, "[mock-agy] Execution completed successfully") {
		t.Fatalf("expected output to contain completion notice, got: %s", allOutput)
	}
}

// Scenario 2: agent_restart_and_upgrade_mid_run (job-1500 / job-1509)
// mock-agy streams output over multiple seconds. Harness and controlplane monitor execution,
// ensure no premature reaper expiry occurs while running, and process completes cleanly with 0.
func TestMockAgy_Scenario2_AgentRestartAndUpgradeMidRun(t *testing.T) {
	buildMockAgy(t)
	t.Setenv("MOCK_AGY_SCENARIO", "agent_restart_and_upgrade_mid_run")

	var mu sync.Mutex
	var postedChunks []string
	var postedExitCode = -1
	var reportedReasons []string
	var tickCount int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/jobs/"):
			_ = json.NewEncoder(w).Encode(controlplane.JobDetail{
				ID:     "job-1500",
				Prompt: "run resilience test during agent upgrade",
				Kind:   "ordinary",
				Engine: "mock-agy",
				Agent:  "mock-agy",
				Status: "RUNNING",
			})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/chunks"):
			var body struct {
				Text string `json:"text"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			postedChunks = append(postedChunks, body.Text)
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/ticks"):
			tickCount++
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/exit"):
			var body struct {
				ExitCode int    `json:"exit_code"`
				Reason   string `json:"reason"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			postedExitCode = body.ExitCode
			if body.Reason != "" {
				reportedReasons = append(reportedReasons, body.Reason)
			}
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	worktree := initTestGitWorktree(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err := executeJob(ctx, srv.URL, "job-1500", worktree, "", "ordinary", "", "")
	if err != nil {
		t.Fatalf("expected executeJob to succeed, got: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if postedExitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", postedExitCode)
	}

	for _, reason := range reportedReasons {
		if reason == "ORPHANED_RUN_EXPIRED" || reason == "lease_expired" || reason == "process_died" {
			t.Fatalf("unexpected premature failure reported during active run: %s", reason)
		}
	}

	allOutput := strings.Join(postedChunks, "\n")
	if !strings.Contains(allOutput, "step 5/5: active and streaming") {
		t.Fatalf("expected output to contain final streaming step, got: %s", allOutput)
	}
	if !strings.Contains(allOutput, "All steps completed successfully") {
		t.Fatalf("expected output to contain completion message, got: %s", allOutput)
	}
}

// Scenario 3: ase_print_timeout_in_job_continuation (job-1372 / job-1379 / job-1447)
// Turn 1 prints print timeout and leaves modified files, exiting 0.
// Harness automatically triggers Turn 2 in-place using mock-agy with resume prompt,
// Turn 2 cleans the worktree and completes, with all logs preserved in order.
func TestMockAgy_Scenario3_ASEPrintTimeoutInJobContinuation(t *testing.T) {
	_ = os.Remove(filepath.Join(os.TempDir(), "mock_agy_ase_turn.tmp"))
	buildMockAgy(t)
	t.Setenv("MOCK_AGY_SCENARIO", "ase_print_timeout_in_job_continuation")

	var mu sync.Mutex
	var postedChunks []string
	var postedExitCode = -1

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/jobs/"):
			_ = json.NewEncoder(w).Encode(controlplane.JobDetail{
				ID:      "job-1372",
				StoryID: "STORY-TEST-1372",
				Prompt:  "/autonomous-story-execution execute story",
				Kind:    "autonomous_story_execution",
				Engine:  "mock-agy",
				Agent:   "mock-agy",
				Status:  "RUNNING",
			})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/chunks"):
			var body struct {
				Text string `json:"text"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			postedChunks = append(postedChunks, body.Text)
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/ticks"):
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/exit"):
			var body struct {
				ExitCode int `json:"exit_code"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			postedExitCode = body.ExitCode
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	worktree := initTestGitWorktree(t)
	_ = exec.Command("git", "-C", worktree, "config", "user.name", "Test Runner").Run()
	_ = exec.Command("git", "-C", worktree, "config", "user.email", "test@test.local").Run()
	_ = os.WriteFile(filepath.Join(worktree, "init.txt"), []byte("init"), 0o644)
	_ = exec.Command("git", "-C", worktree, "add", ".").Run()
	_ = exec.Command("git", "-C", worktree, "commit", "-m", "initial commit").Run()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err := executeJob(ctx, srv.URL, "job-1372", worktree, "", "autonomous_story_execution", "", "")
	if err != nil {
		t.Fatalf("expected executeJob to succeed with in-place continuation, got: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if postedExitCode != 0 {
		t.Fatalf("expected exit code 0 after Turn 2 resume, got %d", postedExitCode)
	}

	allOutput := strings.Join(postedChunks, "\n")
	if !strings.Contains(allOutput, "Turn 1 executing ASE story flow") {
		t.Fatalf("expected Turn 1 output preserved, got: %s", allOutput)
	}
	if !strings.Contains(allOutput, "print timeout after 1h0m0s with turn in progress") {
		t.Fatalf("expected print timeout notice preserved, got: %s", allOutput)
	}
	if !strings.Contains(allOutput, "Turn 2 resumed in-place") {
		t.Fatalf("expected Turn 2 resume execution output, got: %s", allOutput)
	}
}

// Scenario 4: background_task_hang_recovery (job-1331)
// mock-agy reports root agent idle waiting on background task.
// Supervisor reaps the hung background task, in-place resume re-runs mock-agy,
// and finishes the job with exit code 0.
func TestMockAgy_Scenario4_BackgroundTaskHangRecovery(t *testing.T) {
	_ = os.Remove(filepath.Join(os.TempDir(), "mock_agy_bg_hang.tmp"))
	buildMockAgy(t)
	t.Setenv("MOCK_AGY_SCENARIO", "background_task_hang_recovery")
	t.Setenv("IAZIO_ROOT_IDLE_TASK_TIMEOUT", "1s")

	var mu sync.Mutex
	var postedChunks []string
	var postedExitCode = -1

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/jobs/"):
			_ = json.NewEncoder(w).Encode(controlplane.JobDetail{
				ID:      "job-1331",
				StoryID: "STORY-TEST-1331",
				Prompt:  "/autonomous-story-execution execute story",
				Kind:    "autonomous_story_execution",
				Engine:  "mock-agy",
				Agent:   "mock-agy",
				Status:  "RUNNING",
			})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/chunks"):
			var body struct {
				Text string `json:"text"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			postedChunks = append(postedChunks, body.Text)
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/ticks"):
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/exit"):
			var body struct {
				ExitCode int `json:"exit_code"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			postedExitCode = body.ExitCode
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	worktree := initTestGitWorktree(t)
	_ = exec.Command("git", "-C", worktree, "config", "user.name", "Test Runner").Run()
	_ = exec.Command("git", "-C", worktree, "config", "user.email", "test@test.local").Run()
	_ = os.WriteFile(filepath.Join(worktree, "init.txt"), []byte("init"), 0o644)
	_ = exec.Command("git", "-C", worktree, "add", ".").Run()
	_ = exec.Command("git", "-C", worktree, "commit", "-m", "initial commit").Run()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err := executeJob(ctx, srv.URL, "job-1331", worktree, "", "autonomous_story_execution", "", "")
	if err != nil {
		t.Fatalf("expected executeJob to recover and complete, got: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if postedExitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", postedExitCode)
	}

	allOutput := strings.Join(postedChunks, "\n")
	if !strings.Contains(allOutput, "root agent idle") && !strings.Contains(allOutput, "root agent is idle") {
		t.Fatalf("expected root agent idle notice in output, got: %s", allOutput)
	}
	if !strings.Contains(allOutput, "Resumed after background task hang was reaped") {
		t.Fatalf("expected resume output after hang reaped, got: %s", allOutput)
	}
}

// Scenario 5: multi_attempt_log_preservation_and_schedule_sync (job-1466 / sch-1467)
// Attempt 1 fails with transient exit 1.
// Attempt 2 runs and succeeds with exit 0.
// Verifies all logs are preserved with attempt separation, and schedule status syncs.
func TestMockAgy_Scenario5_MultiAttemptLogPreservationAndScheduleSync(t *testing.T) {
	buildMockAgy(t)
	t.Setenv("MOCK_AGY_SCENARIO", "multi_attempt_log_preservation_and_schedule_sync")

	var mu sync.Mutex
	var attempt1Chunks []string
	var attempt2Chunks []string
	var currentAttempt = 1
	var scheduleStatus = "pending"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/jobs/"):
			_ = json.NewEncoder(w).Encode(controlplane.JobDetail{
				ID:               "job-1466",
				ScheduleID:       "sch-1467",
				ExecutionAttempt: currentAttempt,
				Prompt:           "build and test package",
				Kind:             "ordinary",
				Engine:           "mock-agy",
				Agent:            "mock-agy",
				Status:           "RUNNING",
			})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/schedules/"):
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"id":                "sch-1467",
				"status":            scheduleStatus,
				"execution_attempt": currentAttempt,
			})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/chunks"):
			var body struct {
				Text string `json:"text"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if currentAttempt == 1 {
				attempt1Chunks = append(attempt1Chunks, body.Text)
			} else {
				attempt2Chunks = append(attempt2Chunks, body.Text)
			}
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/ticks"):
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/exit"):
			var body struct {
				ExitCode int    `json:"exit_code"`
				Reason   string `json:"reason"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if currentAttempt == 2 && body.ExitCode == 0 {
				scheduleStatus = "completed"
			}
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	worktree := initTestGitWorktree(t)

	// Attempt 1 execution
	ctx1, cancel1 := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel1()
	_ = executeJob(ctx1, srv.URL, "job-1466", worktree, "", "ordinary", "", "")

	// Advance to Attempt 2
	mu.Lock()
	currentAttempt = 2
	mu.Unlock()

	ctx2, cancel2 := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel2()
	err := executeJob(ctx2, srv.URL, "job-1466", worktree, "", "ordinary", "", "")
	if err != nil {
		t.Fatalf("expected Attempt 2 to succeed, got: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	att1Out := strings.Join(attempt1Chunks, "\n")
	att2Out := strings.Join(attempt2Chunks, "\n")

	if !strings.Contains(att1Out, "Attempt 1 startup logs") {
		t.Fatalf("expected Attempt 1 logs to capture startup output, got: %s", att1Out)
	}
	if !strings.Contains(att2Out, "Attempt 2 startup logs") {
		t.Fatalf("expected Attempt 2 logs to capture resume output, got: %s", att2Out)
	}
	if !strings.Contains(att2Out, "Attempt 2 succeeded cleanly") {
		t.Fatalf("expected Attempt 2 completion, got: %s", att2Out)
	}
	if scheduleStatus != "completed" {
		t.Fatalf("expected linked schedule sch-1467 to transition to completed, got: %s", scheduleStatus)
	}
	if currentAttempt != 2 {
		t.Fatalf("expected execution_attempt == 2, got: %d", currentAttempt)
	}
}

// Scenario 6: dirty_worktree_resume_and_queue_unblock (job-1324)
// Run 1 leaves dirty uncommitted files and terminates with code 10.
// Run 2 resumes on existing worktree, cleans the dirty state, and exits 0.
func TestMockAgy_Scenario6_DirtyWorktreeResumeAndQueueUnblock(t *testing.T) {
	buildMockAgy(t)
	t.Setenv("MOCK_AGY_SCENARIO", "dirty_worktree_resume_and_queue_unblock")

	var mu sync.Mutex
	var postedExitCodes []int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/jobs/"):
			_ = json.NewEncoder(w).Encode(controlplane.JobDetail{
				ID:     "job-1324",
				Prompt: "execute step 1 and leave dirty worktree",
				Kind:   "ordinary",
				Engine: "mock-agy",
				Agent:  "mock-agy",
				Status: "RUNNING",
			})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/chunks"):
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/ticks"):
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/exit"):
			var body struct {
				ExitCode int `json:"exit_code"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			postedExitCodes = append(postedExitCodes, body.ExitCode)
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	worktree := initTestGitWorktree(t)

	// Run 1: Fails with code 10 and leaves dirty file
	ctx1, cancel1 := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel1()
	_ = executeJob(ctx1, srv.URL, "job-1324", worktree, "", "ordinary", "", "")

	dirtyFile := filepath.Join(worktree, "dirty_file_1324.tmp")
	if _, err := os.Stat(dirtyFile); os.IsNotExist(err) {
		t.Fatalf("expected dirty file to exist after run 1")
	}

	// Run 2: Resume on the same worktree with resume prompt
	ctx2, cancel2 := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel2()
	err := executeJob(ctx2, srv.URL, "job-1324", worktree, "", "resume", "conv-1324", "")
	if err != nil {
		t.Fatalf("expected run 2 resume to succeed, got: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if len(postedExitCodes) < 2 {
		t.Fatalf("expected 2 exit reports, got %d", len(postedExitCodes))
	}
	if postedExitCodes[0] != 10 {
		t.Fatalf("expected Run 1 exit code 10, got %d", postedExitCodes[0])
	}
	if postedExitCodes[1] != 0 {
		t.Fatalf("expected Run 2 exit code 0, got %d", postedExitCodes[1])
	}

	// Verify dirty file was cleaned
	if _, err := os.Stat(dirtyFile); !os.IsNotExist(err) {
		t.Fatalf("expected dirty file to be removed after resume")
	}
}

// Scenario 7: noop_bypass_empty_stdout (job_3060a0cb6d5e7c71 regression test)
// mock-agy exits 0 with a synthetic wrapper string and zero git repository mutations.
// Proof-of-Execution (PoE) gate catches the fake completion and transitions the job to failed (BYPASS_OR_NOOP_DETECTED).
func TestMockAgy_Scenario7_NoOpBypass_CaughtByPoE(t *testing.T) {
	buildMockAgy(t)
	t.Setenv("MOCK_AGY_SCENARIO", "noop_bypass_empty_stdout")

	var mu sync.Mutex
	var postedExitCode = -1
	var postedReason = ""

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/jobs/"):
			_ = json.NewEncoder(w).Encode(controlplane.JobDetail{
				ID:      "job-3060a0cb6d5e7c71",
				Prompt:  "/autonomous-story-execution STORY-GOV-3715",
				Kind:    "ase",
				Engine:  "mock-agy",
				Agent:   "mock-agy",
				Status:  "RUNNING",
				StoryID: "STORY-GOV-3715",
			})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/chunks"):
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/ticks"):
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/exit"):
			var body struct {
				ExitCode              int    `json:"exit_code"`
				Reason                string `json:"reason"`
				TerminationReasonCode string `json:"termination_reason_code"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			postedExitCode = body.ExitCode
			postedReason = body.Reason
			if postedReason == "" {
				postedReason = body.TerminationReasonCode
			}
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	worktree := initTestGitWorktree(t)

	// Make an initial commit so rev-parse HEAD works
	initFile := filepath.Join(worktree, "init.txt")
	_ = os.WriteFile(initFile, []byte("init"), 0644)
	_ = exec.Command("git", "-C", worktree, "add", ".").Run()
	_ = exec.Command("git", "-C", worktree, "commit", "-m", "init commit").Run()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_ = executeJob(ctx, srv.URL, "job-3060a0cb6d5e7c71", worktree, "", "ase", "", "")

	mu.Lock()
	defer mu.Unlock()

	if postedExitCode != 1 {
		t.Fatalf("expected no-op bypass to be failed with exit code 1, got: %d", postedExitCode)
	}
	if postedReason != "BYPASS_OR_NOOP_DETECTED" {
		t.Fatalf("expected reason BYPASS_OR_NOOP_DETECTED, got: %q", postedReason)
	}
}

// Scenario 8: legitimate_story_execution
// mock-agy performs actual repository file creation, commits changes, and emits rich streaming logs.
// Proof-of-Execution (PoE) gate verifies pre != post Git SHAs and transitions job to COMPLETED_SUCCESS.
func TestMockAgy_Scenario8_LegitimateStoryMutation_PoEVerified(t *testing.T) {
	buildMockAgy(t)
	t.Setenv("MOCK_AGY_SCENARIO", "legitimate_story_execution")

	var mu sync.Mutex
	var postedExitCode = -1
	var postedReason = ""

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/jobs/"):
			_ = json.NewEncoder(w).Encode(controlplane.JobDetail{
				ID:      "job-legit-3715",
				Prompt:  "/autonomous-story-execution STORY-GOV-3715",
				Kind:    "ase",
				Engine:  "mock-agy",
				Agent:   "mock-agy",
				Status:  "RUNNING",
				StoryID: "STORY-GOV-3715",
			})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/chunks"):
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/ticks"):
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/exit"):
			var body struct {
				ExitCode              int    `json:"exit_code"`
				Reason                string `json:"reason"`
				TerminationReasonCode string `json:"termination_reason_code"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			postedExitCode = body.ExitCode
			postedReason = body.Reason
			if postedReason == "" {
				postedReason = body.TerminationReasonCode
			}
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	worktree := initTestGitWorktree(t)

	// Initial commit
	initFile := filepath.Join(worktree, "init.txt")
	_ = os.WriteFile(initFile, []byte("init"), 0644)
	_ = exec.Command("git", "-C", worktree, "add", ".").Run()
	_ = exec.Command("git", "-C", worktree, "commit", "-m", "init commit").Run()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err := executeJob(ctx, srv.URL, "job-legit-3715", worktree, "", "ase", "", "")
	if err != nil {
		t.Fatalf("expected legitimate story execution to succeed, got: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if postedExitCode != 0 {
		t.Fatalf("expected exit code 0, got: %d", postedExitCode)
	}
	if postedReason != "COMPLETED_SUCCESS" {
		t.Fatalf("expected reason COMPLETED_SUCCESS, got: %q", postedReason)
	}
}

// Scenario 9: downstream release gate verification
// Verifies that post-release / downstream release verification gate rejects unproven / fake-completed jobs.
func TestMockAgy_Scenario9_DownstreamReleaseGate_RejectsUnprovenJobs(t *testing.T) {
	var returnFakeJob bool
	const canonicalInitialConv = "aad902c8-bda6-44c7-8a68-c468a1562399"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/tools"):
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"host": "debian",
				"supported_capabilities": []string{
					"user_service_watchdog_v2",
					"deterministic_resume_4tuple_v1",
				},
				"version":    "v1.0.0",
				"commit_sha": "abc1234",
				"tools": []map[string]interface{}{
					{
						"name":              "iazio-agent",
						"installed":         true,
						"installation_type": "systemd-user",
						"active_state":      "active",
						"watchdog_enabled":  true,
						"config_status":     "synced",
					},
					{
						"name":              "iazio-harvester",
						"installed":         true,
						"installation_type": "systemd-user",
						"active_state":      "active",
						"watchdog_enabled":  true,
						"config_status":     "synced",
					},
				},
			})
		case strings.HasSuffix(r.URL.Path, "/jobs/job-1311"):
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"id":                      "job-1311",
				"initial_conversation_id": canonicalInitialConv,
			})
		case strings.HasSuffix(r.URL.Path, "/jobs/job-1313"):
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"id":                      "job-1313",
				"parent_job_id":           "job-1311",
				"initial_conversation_id": canonicalInitialConv,
				"conversation_lineage": []map[string]interface{}{
					{
						"conversation_id": canonicalInitialConv,
						"role":            "root_initial",
					},
					{
						"conversation_id": "1d446877-5bf9-44eb-9db5-e1c007a9fe31",
						"role":            "legacy_unpinned_secondary",
					},
					{
						"conversation_id": "14de9a1f-5679-4f11-a08f-f32bc7e47663",
						"role":            "legacy_unpinned_secondary",
					},
				},
			})
		case strings.HasSuffix(r.URL.Path, "/api/v1/queue/jobs"):
			if returnFakeJob {
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"jobs": []map[string]interface{}{
						{
							"id":             "job_3060a0cb6d5e7c71",
							"action":         "/autonomous-story-execution",
							"status":         "completed",
							"output_summary": "[+] prompt executed via agy (exit 0)",
						},
					},
				})
			} else {
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"jobs": []map[string]interface{}{
						{
							"id":                 "job-legit-3715",
							"action":             "/autonomous-story-execution",
							"status":             "completed",
							"output_summary":     "Verified story execution",
							"allocated_story_id": "STORY-GOV-3715",
							"pre_git_head_sha":   "head1",
							"post_git_head_sha":  "head2",
							"pre_git_tree_sha":   "tree1",
							"post_git_tree_sha":  "tree2",
						},
					},
				})
			}
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	ctx := context.Background()

	// 1. When a fake-completed job exists, release gate must FAIL
	returnFakeJob = true
	_, err := controlplane.VerifyLiveRelease(ctx, srv.URL, "test-token")
	if err == nil {
		t.Fatalf("expected release gate to fail when fake completed job is present")
	}
	if !strings.Contains(err.Error(), "proof-of-execution release gate failed") {
		t.Fatalf("expected error to cite proof-of-execution release gate, got: %v", err)
	}

	// 2. When only validly proven jobs exist, release gate must PASS
	returnFakeJob = false
	res, err := controlplane.VerifyLiveRelease(ctx, srv.URL, "test-token")
	if err != nil {
		t.Fatalf("expected release gate to pass with proven jobs, got: %v", err)
	}
	if !res.PoEEnforcementPassed {
		t.Fatalf("expected PoEEnforcementPassed to be true")
	}
}

// Scenario 10: In-Place Harness Auto-Resume on Transient Tool Failure
// Turn 1 fails with code 1; Harness catches the failure in-place, emits continuation banners
// and attempt history, re-invokes mock-agy on Turn 2, and exits 0 cleanly.
func TestMockAgy_Scenario10_InPlaceHarnessAutoResume(t *testing.T) {
	buildMockAgy(t)
	t.Setenv("MOCK_AGY_SCENARIO", "in_place_harness_auto_resume")

	marker := filepath.Join(os.TempDir(), "mock_agy_inplace_resume.tmp")
	_ = os.Remove(marker)
	defer os.Remove(marker)

	var mu sync.Mutex
	var postedChunks []string
	var postedExitCode = -1

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/jobs/"):
			_ = json.NewEncoder(w).Encode(controlplane.JobDetail{
				ID:      "job-inplace-3721",
				Prompt:  "/autonomous-story-execution verify-in-place-resume",
				Kind:    "autonomous-story-execution",
				Engine:  "mock-agy",
				Agent:   "mock-agy",
				StoryID: "STORY-GOV-3721",
				Status:  "RUNNING",
			})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/chunks"):
			var body struct {
				Text string `json:"text"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			postedChunks = append(postedChunks, body.Text)
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/ticks"):
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/conversations"):
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/continued"):
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/exit"):
			var body struct {
				ExitCode int `json:"exit_code"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			postedExitCode = body.ExitCode
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	worktree := initTestGitWorktree(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err := executeJob(ctx, srv.URL, "job-inplace-3721", worktree, "", "autonomous-story-execution", "", "")
	if err != nil {
		t.Fatalf("expected executeJob with in-place auto-resume to succeed, got: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if postedExitCode != 0 {
		t.Fatalf("expected final exit code 0 reported to control plane, got %d", postedExitCode)
	}

	allOutput := strings.Join(postedChunks, "\n")
	if !strings.Contains(allOutput, "Starting Attempt #2") {
		t.Fatalf("expected output to contain 'Starting Attempt #2' banner, got:\n%s", allOutput)
	}
	if !strings.Contains(allOutput, "In-place auto-resume attempt 2 succeeded cleanly") {
		t.Fatalf("expected output to contain Turn 2 completion message, got:\n%s", allOutput)
	}
}

// Scenario 11: Empty Conversation ID with Dirty Worktree Resume Recovery
// Proves that when conversation ID is empty and worktree has uncommitted modifications,
// the harness uses the ASE flow completion prompt, omits --conversation, and finishes the story flow.
func TestMockAgy_Scenario11_EmptyConversation_DirtyWorktree_FlowCompletion(t *testing.T) {
	buildMockAgy(t)
	t.Setenv("MOCK_AGY_SCENARIO", "empty_conversation_dirty_worktree_resume")

	var mu sync.Mutex
	var postedChunks []string
	var postedExitCode = -1

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/jobs/"):
			_ = json.NewEncoder(w).Encode(controlplane.JobDetail{
				ID:      "job-dirty-unanchored",
				Prompt:  "resume",
				Kind:    "resume",
				Engine:  "mock-agy",
				Agent:   "mock-agy",
				StoryID: "STORY-GOV-3721",
				Status:  "RUNNING",
			})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/chunks"):
			var body struct {
				Text string `json:"text"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			postedChunks = append(postedChunks, body.Text)
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/ticks"):
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/conversations"):
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/exit"):
			var body struct {
				ExitCode int `json:"exit_code"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			postedExitCode = body.ExitCode
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	worktree := initTestGitWorktree(t)

	// Create dirty uncommitted file
	_ = os.WriteFile(filepath.Join(worktree, "dirty_worktree_file.tmp"), []byte("uncommitted work"), 0644)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Empty conversation ID override passed with kind "resume"
	err := executeJob(ctx, srv.URL, "job-dirty-unanchored", worktree, "", "resume", "", "")
	if err != nil {
		t.Fatalf("expected executeJob with unanchored dirty worktree to succeed, got: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if postedExitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", postedExitCode)
	}

	allOutput := strings.Join(postedChunks, "\n")
	if !strings.Contains(allOutput, "Unanchored dirty worktree flow completed successfully") {
		t.Fatalf("expected completion of dirty flow, got: %s", allOutput)
	}
}

// Scenario 12: Empty Conversation ID with Clean Worktree Story Restart
// Proves that when conversation ID is empty and worktree is clean,
// the harness uses the story execution start prompt, omits --conversation, and restarts from scratch.
func TestMockAgy_Scenario12_EmptyConversation_CleanWorktree_StoryRestart(t *testing.T) {
	buildMockAgy(t)
	t.Setenv("MOCK_AGY_SCENARIO", "empty_conversation_clean_worktree_story_restart")

	var mu sync.Mutex
	var postedChunks []string
	var postedExitCode = -1

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/jobs/"):
			_ = json.NewEncoder(w).Encode(controlplane.JobDetail{
				ID:      "job-clean-story-start",
				Prompt:  "resume",
				Kind:    "resume",
				Engine:  "mock-agy",
				Agent:   "mock-agy",
				StoryID: "STORY-GOV-3721",
				Status:  "RUNNING",
			})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/chunks"):
			var body struct {
				Text string `json:"text"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			postedChunks = append(postedChunks, body.Text)
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/ticks"):
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/conversations"):
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/exit"):
			var body struct {
				ExitCode int `json:"exit_code"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			postedExitCode = body.ExitCode
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	worktree := initTestGitWorktree(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err := executeJob(ctx, srv.URL, "job-clean-story-start", worktree, "", "resume", "", "")
	if err != nil {
		t.Fatalf("expected executeJob on clean worktree story restart to succeed, got: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if postedExitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", postedExitCode)
	}

	allOutput := strings.Join(postedChunks, "\n")
	if !strings.Contains(allOutput, "Story restarted from scratch on clean worktree successfully") {
		t.Fatalf("expected clean worktree story restart notice, got: %s", allOutput)
	}
}

// Scenario 13: Ordinary prompt execution ("update tools") with {{.StoryID}} placeholder when StoryID is empty
func TestMockAgy_Scenario13_OrdinaryPromptWithEmptyStoryIDPlaceholder(t *testing.T) {
	buildMockAgy(t)
	t.Setenv("MOCK_AGY_SCENARIO", "ordinary_prompt_with_mcp_daemons")

	var mu sync.Mutex
	var postedChunks []string
	var postedExitCode = -1

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/jobs/"):
			_ = json.NewEncoder(w).Encode(controlplane.JobDetail{
				ID:      "job-1560",
				Prompt:  "update tools {{.StoryID}}",
				Kind:    "ordinary",
				Engine:  "mock-agy",
				Agent:   "mock-agy",
				StoryID: "",
				Status:  "RUNNING",
			})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/chunks"):
			var body struct {
				Text string `json:"text"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			postedChunks = append(postedChunks, body.Text)
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/ticks"):
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/exit"):
			var body struct {
				ExitCode int `json:"exit_code"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			postedExitCode = body.ExitCode
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	worktree := initTestGitWorktree(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err := executeJob(ctx, srv.URL, "job-1560", worktree, "", "ordinary", "", "")
	if err != nil {
		t.Fatalf("expected executeJob to succeed when StoryID is empty with {{.StoryID}} placeholder, got: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if postedExitCode != 0 {
		t.Fatalf("expected exit code 0 reported to control plane, got %d", postedExitCode)
	}

	allOutput := strings.Join(postedChunks, "\n")
	if strings.Contains(allOutput, "STORY-RESUME-INPLACE") || strings.Contains(allOutput, "{{.StoryID}}") {
		t.Fatalf("expected clean prompt without unresolved placeholder or fake StoryID, got: %s", allOutput)
	}
	if !strings.Contains(allOutput, "[mock-agy] Prompt: update tools") {
		t.Fatalf("expected mock-agy to receive clean 'update tools' prompt, got: %s", allOutput)
	}
}

// Scenario 14: Child engine (mock-agy) exiting with non-zero code (exit 1, 2, 10) triggering in-place continuation / auto-resume to completion
func TestMockAgy_Scenario14_NonZeroExitCodes_InPlaceAutoResume(t *testing.T) {
	buildMockAgy(t)
	t.Setenv("MOCK_AGY_SCENARIO", "nonzero_exit_inplace_resume")

	for _, code := range []string{"1", "2", "10"} {
		t.Run("exit_"+code, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "mock_agy_inplace_resume_"+code+".tmp")
			t.Setenv("MOCK_AGY_STATE_MARKER", marker)
			t.Setenv("MOCK_AGY_EXIT_CODE", code)

			var mu sync.Mutex
			var postedChunks []string
			var postedExitCode = -1

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				switch {
				case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/jobs/"):
					_ = json.NewEncoder(w).Encode(controlplane.JobDetail{
						ID:      "job-nonzero-" + code,
						Prompt:  "/autonomous-story-execution verify-nonzero-resume",
						Kind:    "autonomous-story-execution",
						Engine:  "mock-agy",
						Agent:   "mock-agy",
						StoryID: "STORY-GOV-3799",
						Status:  "RUNNING",
					})
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/chunks"):
					var body struct {
						Text string `json:"text"`
					}
					_ = json.NewDecoder(r.Body).Decode(&body)
					postedChunks = append(postedChunks, body.Text)
					w.WriteHeader(http.StatusOK)
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/ticks"):
					w.WriteHeader(http.StatusOK)
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/conversations"):
					w.WriteHeader(http.StatusOK)
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/continued"):
					w.WriteHeader(http.StatusOK)
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/exit"):
					var body struct {
						ExitCode int `json:"exit_code"`
					}
					_ = json.NewDecoder(r.Body).Decode(&body)
					postedExitCode = body.ExitCode
					w.WriteHeader(http.StatusOK)
				default:
					w.WriteHeader(http.StatusOK)
				}
			}))
			defer srv.Close()

			worktree := initTestGitWorktree(t)

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			err := executeJob(ctx, srv.URL, "job-nonzero-"+code, worktree, "", "autonomous-story-execution", "", "")
			if err != nil {
				t.Fatalf("expected executeJob with exit %s to auto-resume and succeed, got: %v", code, err)
			}

			mu.Lock()
			defer mu.Unlock()

			if postedExitCode != 0 {
				t.Fatalf("expected final exit code 0 after auto-resuming from exit %s, got %d", code, postedExitCode)
			}

			allOutput := strings.Join(postedChunks, "\n")
			if !strings.Contains(allOutput, "trigger_reason: exit_code_"+code) {
				t.Fatalf("expected output to record trigger_reason: exit_code_%s, got:\n%s", code, allOutput)
			}
			if !strings.Contains(allOutput, "In-place auto-resume attempt 2 succeeded cleanly") {
				t.Fatalf("expected Turn 2 completion output for exit %s, got:\n%s", code, allOutput)
			}
		})
	}
}

// Scenario 15: Canonical ~/.local/bin/iazio-harness path resolution and rejection of daemon/service mode
func TestMockAgy_Scenario15_CanonicalLocalBinPathResolution(t *testing.T) {
	_ = buildMockAgy(t)
	tmpHome := os.Getenv("HOME")
	localBinDir := filepath.Join(tmpHome, ".local", "bin")
	if err := os.MkdirAll(localBinDir, 0o755); err != nil {
		t.Fatalf("failed to create ~/.local/bin: %v", err)
	}

	// 1. Verify service and daemon subcommands are rejected (iazio-harness is a short-lived CLI runner)
	if err := run([]string{"service", "install"}); err == nil {
		t.Fatalf("expected service install to be rejected on iazio-harness")
	}
	if err := run([]string{"daemon"}); err == nil {
		t.Fatalf("expected daemon subcommand to be rejected on iazio-harness")
	}

	// 2. Verify ChildEnv injects ~/.local/bin and not ~/.iazio/bin
	env := engines.ChildEnv(nil, nil, "")
	pathVal, ok := engines.EnvLookup(env, "PATH")
	if !ok || !strings.Contains(pathVal, localBinDir) {
		t.Fatalf("expected ChildEnv PATH to include %s, got %q", localBinDir, pathVal)
	}
	if strings.Contains(pathVal, filepath.Join(tmpHome, ".iazio", "bin")) {
		t.Fatalf("expected ChildEnv PATH not to include legacy .iazio/bin, got %q", pathVal)
	}
}

// Scenario 16: job-1350 (sch-1348, Prompt: "Execute story STORY-ROOT-245", Kind: "ordinary")
// Turn 1 exits 0 leaving 4 dirty files in worktree. Because isASEJob recognizes "Execute story STORY-ROOT-245",
// harness triggers Turn 2 in-place auto-continuation, cleans worktree, marks STORY-ROOT-245 DONE, and exits 0.
func TestMockAgy_Scenario16_Job1350_DirtyWorktreePostExit_InPlaceAutoResume(t *testing.T) {
	buildMockAgy(t)
	marker := filepath.Join(t.TempDir(), "mock_agy_job_1350_turn.tmp")
	t.Setenv("MOCK_AGY_STATE_MARKER", marker)
	t.Setenv("MOCK_AGY_SCENARIO", "job_1350_dirty_worktree_post_exit")

	var mu sync.Mutex
	var postedChunks []string
	var postedExitCode = -1

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/jobs/"):
			_ = json.NewEncoder(w).Encode(controlplane.JobDetail{
				ID:     "job-1350",
				Prompt: "Execute story STORY-ROOT-245",
				Kind:   "ordinary",
				Engine: "mock-agy",
				Agent:  "mock-agy",
				Status: "RUNNING",
			})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/chunks"):
			var body struct {
				Text string `json:"text"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			postedChunks = append(postedChunks, body.Text)
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/exit"):
			var body struct {
				ExitCode int `json:"exit_code"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			postedExitCode = body.ExitCode
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	worktree := initTestGitWorktree(t)
	_ = os.MkdirAll(filepath.Join(worktree, "docs", "stories"), 0o755)
	_ = os.WriteFile(filepath.Join(worktree, "docs", "stories", "story-root-245.story.md"), []byte("---\nid: STORY-ROOT-245\ntitle: Fix dirty worktree post exit\nstatus: IN_PROGRESS\n---\n"), 0o644)
	_ = exec.Command("git", "-C", worktree, "add", ".").Run()
	_ = exec.Command("git", "-C", worktree, "commit", "-m", "chore: initial story").Run()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err := executeJob(ctx, srv.URL, "job-1350", worktree, "", "ordinary", "", "")
	if err != nil {
		t.Fatalf("expected executeJob for job-1350 to auto-resume in-place and succeed, got: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if postedExitCode != 0 {
		t.Fatalf("expected final exit code 0 after in-place dirty worktree continuation, got %d", postedExitCode)
	}
	allOutput := strings.Join(postedChunks, "\n")
	if !strings.Contains(allOutput, "Turn 2 cleaned dirty worktree and completed STORY-ROOT-245") {
		t.Fatalf("expected Turn 2 continuation output in chunks, got:\n%s", allOutput)
	}
}
