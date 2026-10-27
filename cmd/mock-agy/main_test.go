package main

import (
	"bytes"
	"os"
	"os/exec"
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

	t.Run("Subcommands", func(t *testing.T) {
		var stdout bytes.Buffer
		code := run([]string{"version", "--json"}, "", &stdout, os.Stderr)
		if code != 0 || !strings.Contains(stdout.String(), "1.15.0-mock") {
			t.Fatalf("version failed: code %d, out: %s", code, stdout.String())
		}

		stdout.Reset()
		code = run([]string{"models", "--json"}, "", &stdout, os.Stderr)
		if code != 0 || !strings.Contains(stdout.String(), "gemini-2.5-pro") {
			t.Fatalf("models failed: code %d, out: %s", code, stdout.String())
		}

		stdout.Reset()
		code = run([]string{"agents"}, "", &stdout, os.Stderr)
		if code != 0 || !strings.Contains(stdout.String(), "autonomous") {
			t.Fatalf("agents failed: code %d, out: %s", code, stdout.String())
		}

		stdout.Reset()
		code = run([]string{"help"}, "", &stdout, os.Stderr)
		if code != 0 || !strings.Contains(stdout.String(), "Usage:") {
			t.Fatalf("help failed: code %d, out: %s", code, stdout.String())
		}
	})

	t.Run("HeadlessPermissionDenied", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		// Missing --dangerously-skip-permissions with permission-requiring prompt
		code := run([]string{"-p", "please require_permission to edit"}, "", &stdout, &stderr)
		if code != 1 {
			t.Fatalf("expected exit 1 on missing permission flag, got %d", code)
		}
		if !strings.Contains(stderr.String(), "jetski: no output produced — a tool required the 'command' permission") {
			t.Fatalf("unexpected stderr: %s", stderr.String())
		}

		// With --dangerously-skip-permissions, should succeed
		stdout.Reset()
		stderr.Reset()
		code = run([]string{"-p", "please require_permission to edit", "--dangerously-skip-permissions"}, "", &stdout, &stderr)
		if code != 0 {
			t.Fatalf("expected exit 0 with flag, got %d", code)
		}

		// Explicit permission_denied scenario
		stdout.Reset()
		stderr.Reset()
		code = run([]string{"-p", "ordinary"}, "permission_denied", &stdout, &stderr)
		if code != 1 || !strings.Contains(stderr.String(), "jetski: no output produced") {
			t.Fatalf("expected permission_denied exit 1, got code %d, err: %s", code, stderr.String())
		}
	})

	t.Run("FailureScenarios", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := run([]string{"-p", "test"}, "session_corrupted", &stdout, &stderr)
		if code != 1 || !strings.Contains(stderr.String(), "malformed") {
			t.Fatalf("expected session_corrupted failure, got %d: %s", code, stderr.String())
		}

		stdout.Reset()
		stderr.Reset()
		code = run([]string{"-p", "test"}, "quota_exhausted", &stdout, &stderr)
		if code != 1 || !strings.Contains(stderr.String(), "429") {
			t.Fatalf("expected quota_exhausted failure, got %d: %s", code, stderr.String())
		}
	})

	t.Run("AdditionalFlagsAndArgs", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		args := []string{
			"--output-format=stream-json",
			"--output-format", "json",
			"--input-format=text",
			"--input-format", "text",
			"--mode=chat",
			"--mode", "chat",
			"--effort=high",
			"--effort", "high",
			"--add-dir=/tmp",
			"--add-dir", "/tmp",
			"--continue",
			"-c",
			"--print-timeout=120",
			"--print-timeout", "120",
			"--print=flag testing",
		}
		code := run(args, "", &stdout, &stderr)
		if code != 0 {
			t.Fatalf("expected exit 0, got %d", code)
		}
	})

	t.Run("Scenario7_NoOpBypassEmptyStdout", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := run([]string{"--print=noop test"}, "noop_bypass_empty_stdout", &stdout, &stderr)
		if code != 0 {
			t.Fatalf("expected exit 0, got %d", code)
		}
		if !strings.Contains(stdout.String(), "prompt executed via agy") {
			t.Fatalf("unexpected stdout: %s", stdout.String())
		}
	})

	t.Run("Scenario7b_NoOpBypassZeroMutation", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := run([]string{"--print=noop zero mutation"}, "noop_bypass_zero_mutation", &stdout, &stderr)
		if code != 0 {
			t.Fatalf("expected exit 0, got %d", code)
		}
		if !strings.Contains(stdout.String(), "without git changes") {
			t.Fatalf("unexpected stdout: %s", stdout.String())
		}
	})

	t.Run("Scenario8_LegitimateStoryExecution", func(t *testing.T) {
		tmpDir := t.TempDir()
		origWd, _ := os.Getwd()
		_ = os.Chdir(tmpDir)
		defer func() { _ = os.Chdir(origWd) }()

		_ = exec.Command("git", "init").Run()
		_ = exec.Command("git", "config", "user.email", "test@test.local").Run()
		_ = exec.Command("git", "config", "user.name", "Tester").Run()

		var stdout, stderr bytes.Buffer
		code := run([]string{"--print=execute STORY-GOV-3715"}, "legitimate_story_execution", &stdout, &stderr)
		if code != 0 {
			t.Fatalf("expected exit 0, got %d", code)
		}
		if !strings.Contains(stdout.String(), "cryptographic proof of mutation") {
			t.Fatalf("unexpected stdout: %s", stdout.String())
		}
	})

	t.Run("Scenario10_InPlaceHarnessAutoResume", func(t *testing.T) {
		tmpDir := t.TempDir()
		origWd, _ := os.Getwd()
		_ = os.Chdir(tmpDir)
		defer func() { _ = os.Chdir(origWd) }()

		_ = exec.Command("git", "init").Run()
		_ = exec.Command("git", "config", "user.email", "test@test.local").Run()
		_ = exec.Command("git", "config", "user.name", "Tester").Run()

		marker := filepath.Join(os.TempDir(), "mock_agy_inplace_resume.tmp")
		_ = os.Remove(marker)

		// Turn 1 fails transiently
		var stdout1, stderr1 bytes.Buffer
		code1 := run([]string{"--print=start story"}, "in_place_harness_auto_resume", &stdout1, &stderr1)
		if code1 != 1 {
			t.Fatalf("turn 1 expected exit 1, got %d", code1)
		}

		// Turn 2 succeeds
		var stdout2, stderr2 bytes.Buffer
		code2 := run([]string{"--conversation=mock-conv-1111-2222-3333-444455556666", "resume"}, "in_place_harness_auto_resume", &stdout2, &stderr2)
		if code2 != 0 {
			t.Fatalf("turn 2 expected exit 0, got %d", code2)
		}
		if !strings.Contains(stdout2.String(), "In-place auto-resume attempt 2 succeeded cleanly") {
			t.Fatalf("unexpected stdout2: %s", stdout2.String())
		}
	})

	t.Run("Scenario11_EmptyConversationDirtyWorktreeResume", func(t *testing.T) {
		tmpDir := t.TempDir()
		origWd, _ := os.Getwd()
		_ = os.Chdir(tmpDir)
		defer func() { _ = os.Chdir(origWd) }()

		_ = exec.Command("git", "init").Run()
		_ = exec.Command("git", "config", "user.email", "test@test.local").Run()
		_ = exec.Command("git", "config", "user.name", "Tester").Run()
		_ = os.WriteFile("dirty_worktree_file.tmp", []byte("initial dirty"), 0644)

		var stdout, stderr bytes.Buffer
		code := run([]string{"resume and complete current /autonomous-story-execution flow"}, "empty_conversation_dirty_worktree_resume", &stdout, &stderr)
		if code != 0 {
			t.Fatalf("expected exit 0, got %d: %s", code, stderr.String())
		}
		if !strings.Contains(stdout.String(), "Unanchored dirty worktree flow completed successfully") {
			t.Fatalf("unexpected stdout: %s", stdout.String())
		}
	})

	t.Run("Scenario12_EmptyConversationCleanWorktreeStoryRestart", func(t *testing.T) {
		tmpDir := t.TempDir()
		origWd, _ := os.Getwd()
		_ = os.Chdir(tmpDir)
		defer func() { _ = os.Chdir(origWd) }()

		_ = exec.Command("git", "init").Run()
		_ = exec.Command("git", "config", "user.email", "test@test.local").Run()
		_ = exec.Command("git", "config", "user.name", "Tester").Run()

		var stdout, stderr bytes.Buffer
		code := run([]string{"Execute story STORY-GOV-9999 end-to-end using /autonomous-story-execution."}, "empty_conversation_clean_worktree_story_restart", &stdout, &stderr)
		if code != 0 {
			t.Fatalf("expected exit 0, got %d: %s", code, stderr.String())
		}
		if !strings.Contains(stdout.String(), "Story restarted from scratch on clean worktree successfully") {
			t.Fatalf("unexpected stdout: %s", stdout.String())
		}
	})
}
