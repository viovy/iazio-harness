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
				ID:          "job-1506",
				Prompt:      "update tools and check MCP health",
				Kind:        "ordinary",
				Engine:      "mock-agy",
				Agent:       "mock-agy",
				Status:      "RUNNING",
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

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
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
				ID:          "job-1500",
				Prompt:      "run resilience test during agent upgrade",
				Kind:        "ordinary",
				Engine:      "mock-agy",
				Agent:       "mock-agy",
				Status:      "RUNNING",
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

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
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
				ID:          "job-1372",
				StoryID:     "STORY-TEST-1372",
				Prompt:      "/autonomous-story-execution execute story",
				Kind:        "autonomous_story_execution",
				Engine:      "mock-agy",
				Agent:       "mock-agy",
				Status:      "RUNNING",
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

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
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
				ID:          "job-1331",
				StoryID:     "STORY-TEST-1331",
				Prompt:      "/autonomous-story-execution execute story",
				Kind:        "autonomous_story_execution",
				Engine:      "mock-agy",
				Agent:       "mock-agy",
				Status:      "RUNNING",
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

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
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
	ctx1, cancel1 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel1()
	_ = executeJob(ctx1, srv.URL, "job-1466", worktree, "", "ordinary", "", "")

	// Advance to Attempt 2
	mu.Lock()
	currentAttempt = 2
	mu.Unlock()

	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
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
				ID:          "job-1324",
				Prompt:      "execute step 1 and leave dirty worktree",
				Kind:        "ordinary",
				Engine:      "mock-agy",
				Agent:       "mock-agy",
				Status:      "RUNNING",
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
	ctx1, cancel1 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel1()
	_ = executeJob(ctx1, srv.URL, "job-1324", worktree, "", "ordinary", "", "")

	dirtyFile := filepath.Join(worktree, "dirty_file_1324.tmp")
	if _, err := os.Stat(dirtyFile); os.IsNotExist(err) {
		t.Fatalf("expected dirty file to exist after run 1")
	}

	// Run 2: Resume on the same worktree with resume prompt
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
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
