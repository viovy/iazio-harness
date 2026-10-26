package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMockAgy_ScenariosUnit(t *testing.T) {
	t.Setenv("MOCK_AGY_FAST", "1")

	t.Run("Scenario1_OrdinaryPromptWithMCPDaemons", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		args := []string{"--model=flash", "--agent=general", "--print=hello world"}
		code := run(args, "ordinary_prompt_with_mcp_daemons", &stdout, &stderr)
		if code != 0 {
			t.Fatalf("expected exit 0, got %d", code)
		}
		if !strings.Contains(stdout.String(), "Executing ordinary prompt") {
			t.Fatalf("unexpected stdout: %s", stdout.String())
		}
	})

	t.Run("Scenario2_AgentRestartAndUpgradeMidRun", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		args := []string{"--conversation=test-conv-001"}
		code := run(args, "agent_restart_and_upgrade_mid_run", &stdout, &stderr)
		if code != 0 {
			t.Fatalf("expected exit 0, got %d", code)
		}
		if !strings.Contains(stdout.String(), "Active conversation: test-conv-001") {
			t.Fatalf("unexpected stdout: %s", stdout.String())
		}
	})

	t.Run("Scenario3_ASEPrintTimeoutInJobContinuation", func(t *testing.T) {
		tmpDir := t.TempDir()
		origWd, _ := os.Getwd()
		_ = os.Chdir(tmpDir)
		defer func() { _ = os.Chdir(origWd) }()

		// Turn 1
		var stdout1, stderr1 bytes.Buffer
		code1 := run([]string{"--print=initial run"}, "ase_print_timeout_in_job_continuation", &stdout1, &stderr1)
		if code1 != 0 {
			t.Fatalf("turn 1 expected exit 0, got %d", code1)
		}
		if !strings.Contains(stdout1.String(), "print timeout") {
			t.Fatalf("expected print timeout in turn 1: %s", stdout1.String())
		}

		// Turn 2
		var stdout2, stderr2 bytes.Buffer
		code2 := run([]string{"--conversation=conv-ase-01", "resume"}, "ase_print_timeout_in_job_continuation", &stdout2, &stderr2)
		if code2 != 0 {
			t.Fatalf("turn 2 expected exit 0, got %d", code2)
		}
		if !strings.Contains(stdout2.String(), "resumed in-place") {
			t.Fatalf("expected resumed in turn 2: %s", stdout2.String())
		}
	})

	t.Run("Scenario4_BackgroundTaskHangRecovery", func(t *testing.T) {
		stateMarker := filepath.Join(os.TempDir(), "mock_agy_bg_hang.tmp")
		_ = os.Remove(stateMarker)

		// Initial hang
		var stdout1, stderr1 bytes.Buffer
		code1 := run([]string{"--print=run background"}, "background_task_hang_recovery", &stdout1, &stderr1)
		if code1 != 143 {
			t.Fatalf("expected exit 143, got %d", code1)
		}

		// Resumed
		var stdout2, stderr2 bytes.Buffer
		code2 := run([]string{"--conversation=conv-bg-01", "resume"}, "background_task_hang_recovery", &stdout2, &stderr2)
		if code2 != 0 {
			t.Fatalf("expected exit 0 on resume, got %d", code2)
		}
	})

	t.Run("Scenario5_MultiAttemptLogPreservation", func(t *testing.T) {
		marker := filepath.Join(os.TempDir(), "mock_agy_attempt_marker.tmp")
		_ = os.Remove(marker)

		// Attempt 1 fails
		var stdout1, stderr1 bytes.Buffer
		code1 := run([]string{"--print=attempt 1"}, "multi_attempt_log_preservation_and_schedule_sync", &stdout1, &stderr1)
		if code1 != 1 {
			t.Fatalf("expected exit 1, got %d", code1)
		}

		// Attempt 2 succeeds
		var stdout2, stderr2 bytes.Buffer
		code2 := run([]string{"--print=attempt 2"}, "multi_attempt_log_preservation_and_schedule_sync", &stdout2, &stderr2)
		if code2 != 0 {
			t.Fatalf("expected exit 0, got %d", code2)
		}
	})

	t.Run("Scenario6_DirtyWorktreeResumeAndQueueUnblock", func(t *testing.T) {
		tmpDir := t.TempDir()
		origWd, _ := os.Getwd()
		_ = os.Chdir(tmpDir)
		defer func() { _ = os.Chdir(origWd) }()

		stateMarker := filepath.Join(os.TempDir(), "mock_agy_dirty_state.tmp")
		_ = os.Remove(stateMarker)

		// Dirty exit
		var stdout1, stderr1 bytes.Buffer
		code1 := run([]string{"dirty work"}, "dirty_worktree_resume_and_queue_unblock", &stdout1, &stderr1)
		if code1 != 10 {
			t.Fatalf("expected exit 10, got %d", code1)
		}

		// Resume clean
		var stdout2, stderr2 bytes.Buffer
		code2 := run([]string{"resume"}, "dirty_worktree_resume_and_queue_unblock", &stdout2, &stderr2)
		if code2 != 0 {
			t.Fatalf("expected exit 0, got %d", code2)
		}
	})

	t.Run("DefaultScenario", func(t *testing.T) {
		logFile := filepath.Join(t.TempDir(), "out.log")
		var stdout, stderr bytes.Buffer
		args := []string{
			"--conversation", "default-conv",
			"--model", "gpt-4",
			"--agent", "coder",
			"--log-file=" + logFile,
			"--print-timeout", "60",
			"--dangerously-skip-permissions",
			"--disable-slash-commands",
			"positional", "prompt", "text",
		}
		code := run(args, "", &stdout, &stderr)
		if code != 0 {
			t.Fatalf("expected exit 0, got %d", code)
		}
		if !strings.Contains(stdout.String(), "Resumed conversation default-conv") {
			t.Fatalf("unexpected stdout: %s", stdout.String())
		}
		if _, err := os.Stat(logFile); err != nil {
			t.Fatalf("expected log file to exist: %v", err)
		}
	})
}
