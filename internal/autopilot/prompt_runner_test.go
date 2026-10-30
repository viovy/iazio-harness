package autopilot

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/viovy/iazio-harness/internal/engines"
)

// TestAutopilotHelperProcess acts as a mock autopilot subprocess for unit tests.
func TestAutopilotHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_AUTOPILOT_HELPER") != "1" {
		return
	}
	defer os.Exit(0)

	args := os.Args
	for len(args) > 0 {
		if args[0] == "--" {
			args = args[1:]
			break
		}
		args = args[1:]
	}

	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "no arguments passed to helper")
		os.Exit(2)
	}

	cmd := args[0]
	if (cmd == "prompt" || cmd == "story") && len(args) >= 2 && args[1] == "run" {
		var promptFile string
		var storyArg string
		for i := 2; i < len(args); i++ {
			if args[i] == "--file" && i+1 < len(args) {
				promptFile = args[i+1]
			}
			if args[i] == "--story" && i+1 < len(args) {
				storyArg = args[i+1]
			}
		}

		// Verify prompt content from temp file or stdin
		var promptContent string
		if promptFile != "" {
			if b, err := os.ReadFile(promptFile); err == nil {
				promptContent = string(b)
			}
		}
		if promptContent == "" {
			b, _ := io.ReadAll(os.Stdin)
			promptContent = string(b)
		}

		if strings.Contains(promptContent, "SLEEP_TRIGGER_TIMEOUT") {
			time.Sleep(2 * time.Second)
			os.Exit(0)
		}

		if strings.Contains(promptContent, "SIMULATE_FAILURE") {
			fmt.Fprintln(os.Stderr, "execution failed: syntax error in generated code")
			os.Exit(1)
		}

		// Test output with secret token to test redaction
		pwd, _ := os.Getwd()
		branchEnv := os.Getenv("AUTOPILOT_TARGET_BRANCH")
		fmt.Printf("Executed in %s: ghp_SECRET_GITHUB_TOKEN_1234567890abcdef OK story=%s branch=%s\n", filepath.Base(pwd), storyArg, branchEnv)
		return
	}

	fmt.Fprintf(os.Stderr, "unknown command: %v\n", args)
	os.Exit(2)
}

func makeFakeAutopilot(t *testing.T) string {
	t.Helper()
	tmpDir := t.TempDir()
	if runtime.GOOS == "windows" {
		fakeBin := filepath.Join(tmpDir, "fake-autopilot.bat")
		script := fmt.Sprintf("@echo off\r\nset GO_WANT_AUTOPILOT_HELPER=1\r\n\"%s\" -test.run=TestAutopilotHelperProcess -- %%*\r\n", os.Args[0])
		if err := os.WriteFile(fakeBin, []byte(script), 0755); err != nil {
			t.Fatalf("failed to write fake autopilot bat: %v", err)
		}
		return fakeBin
	}
	fakeBin := filepath.Join(tmpDir, "fake-autopilot")
	script := fmt.Sprintf("#!/bin/sh\nexport GO_WANT_AUTOPILOT_HELPER=1\nexec %s -test.run=TestAutopilotHelperProcess -- \"$@\"\n", os.Args[0])
	if err := os.WriteFile(fakeBin, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write fake autopilot binary: %v", err)
	}
	return fakeBin
}

func initTestGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v (%s)", args, err, string(out))
		}
	}

	run("init")
	run("config", "user.name", "Test User")
	run("config", "user.email", "test@example.com")

	testFile := filepath.Join(dir, "README.md")
	if err := os.WriteFile(testFile, []byte("# Test Repo\n"), 0644); err != nil {
		t.Fatal(err)
	}
	run("add", "README.md")
	run("commit", "-m", "Initial commit")

	return dir
}

func TestCLIPromptRunner_Success(t *testing.T) {
	fakeBin := makeFakeAutopilot(t)
	repoDir := initTestGitRepo(t)

	// Make an uncommitted file to test git snapshot modified_files
	newFile := filepath.Join(repoDir, "new_code.go")
	if err := os.WriteFile(newFile, []byte("package main\n"), 0644); err != nil {
		t.Fatal(err)
	}

	runner := NewPromptRunner(fakeBin)
	runner.GracePeriod = 200 * time.Millisecond

	opts := RunPromptOptions{
		PromptID:    "test-prompt-001",
		Repo:        "test-submodule",
		Title:       "Test prompt execution",
		Prompt:      "Refactor code and add unit tests",
		TargetAgent: "autopilot",
		WorkingDir:  repoDir,
		Timeout:     5 * time.Second,
	}

	res, err := runner.RunPrompt(context.Background(), opts)
	if err != nil {
		t.Fatalf("unexpected RunPrompt error: %v", err)
	}

	if res.Status != "completed" {
		t.Errorf("expected status completed, got %s", res.Status)
	}
	if res.ExitCode != 0 {
		t.Errorf("expected exit code 0, got %d", res.ExitCode)
	}
	if res.GitHeadSHA == "" {
		t.Error("expected non-empty GitHeadSHA")
	}
	if res.GitTreeSHA == "" {
		t.Error("expected non-empty GitTreeSHA")
	}
	if len(res.ModifiedFiles) == 0 {
		t.Error("expected modified_files to contain new_code.go")
	} else if res.ModifiedFiles[0] != "new_code.go" {
		t.Errorf("expected modified_files [new_code.go], got %v", res.ModifiedFiles)
	}
	// Verify secret redaction in summary
	if strings.Contains(res.Summary, "ghp_SECRET_GITHUB_TOKEN") {
		t.Errorf("summary contained unredacted secret token: %s", res.Summary)
	}
	if !strings.Contains(res.Summary, "[REDACTED_GITHUB_TOKEN]") {
		t.Errorf("expected summary to contain [REDACTED_GITHUB_TOKEN], got: %s", res.Summary)
	}
}

func TestCLIPromptRunner_Failure(t *testing.T) {
	fakeBin := makeFakeAutopilot(t)
	repoDir := initTestGitRepo(t)

	runner := NewPromptRunner(fakeBin)
	runner.GracePeriod = 200 * time.Millisecond

	opts := RunPromptOptions{
		PromptID:    "test-prompt-fail",
		Repo:        "test-submodule",
		Prompt:      "SIMULATE_FAILURE",
		TargetAgent: "autopilot",
		WorkingDir:  repoDir,
		Timeout:     5 * time.Second,
	}

	res, err := runner.RunPrompt(context.Background(), opts)
	if err == nil {
		t.Fatal("expected RunPrompt error, got nil")
	}
	if res == nil {
		t.Fatal("expected non-nil PromptResult on failure")
	}
	if res.Status != "failed" {
		t.Errorf("expected status failed, got %s", res.Status)
	}
	if res.ExitCode != 1 {
		t.Errorf("expected exit code 1, got %d", res.ExitCode)
	}
}

func TestCLIPromptRunner_Timeout(t *testing.T) {
	fakeBin := makeFakeAutopilot(t)
	repoDir := initTestGitRepo(t)

	runner := NewPromptRunner(fakeBin)
	runner.GracePeriod = 100 * time.Millisecond

	opts := RunPromptOptions{
		PromptID:    "test-prompt-timeout",
		Repo:        "test-submodule",
		Prompt:      "SLEEP_TRIGGER_TIMEOUT",
		TargetAgent: "autopilot",
		WorkingDir:  repoDir,
		Timeout:     100 * time.Millisecond,
		GracePeriod: 100 * time.Millisecond,
	}

	res, err := runner.RunPrompt(context.Background(), opts)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if res == nil {
		t.Fatal("expected non-nil PromptResult on timeout")
	}
	if res.Status != "timed_out" {
		t.Errorf("expected status timed_out, got %s", res.Status)
	}
}

func TestNewPromptRunnerDefaults(t *testing.T) {
	t.Setenv("AUTOPILOT_BIN", "/custom/runner/bin")
	r1 := NewPromptRunner("")
	if r1.AutopilotBinary != "/custom/runner/bin" {
		t.Errorf("expected /custom/runner/bin, got %s", r1.AutopilotBinary)
	}

	t.Setenv("AUTOPILOT_BIN", "")
	r2 := NewPromptRunner("")
	if r2.AutopilotBinary != "autopilot" {
		t.Errorf("expected autopilot, got %s", r2.AutopilotBinary)
	}
}

func TestCLIPromptRunner_StoryDetection(t *testing.T) {
	fakeBin := makeFakeAutopilot(t)
	repoDir := initTestGitRepo(t)

	runner := NewPromptRunner(fakeBin)

	// Case 1: StoryID explicitly in opts
	opts1 := RunPromptOptions{
		PromptID:   "test-prompt-explicit",
		StoryID:    "STORY-EXPLICIT-111",
		Repo:       "test-submodule",
		Prompt:     "Execute task",
		WorkingDir: repoDir,
		Timeout:    5 * time.Second,
	}
	res1, err := runner.RunPrompt(context.Background(), opts1)
	if err != nil {
		t.Fatalf("unexpected RunPrompt error: %v", err)
	}
	if !strings.Contains(res1.Summary, "story=STORY-EXPLICIT-111") {
		t.Fatalf("expected story=STORY-EXPLICIT-111 in summary, got: %s", res1.Summary)
	}

	// Case 2: StoryID detected in prompt content
	opts2 := RunPromptOptions{
		PromptID:   "test-prompt-detected",
		Repo:       "test-submodule",
		Prompt:     "# Execute Story STORY-DETECTED-222\nPlease implement ACs",
		WorkingDir: repoDir,
		Timeout:    5 * time.Second,
	}
	res2, err := runner.RunPrompt(context.Background(), opts2)
	if err != nil {
		t.Fatalf("unexpected RunPrompt error: %v", err)
	}
	if !strings.Contains(res2.Summary, "story=STORY-DETECTED-222") {
		t.Fatalf("expected story=STORY-DETECTED-222 in summary, got: %s", res2.Summary)
	}
}

func TestCLIPromptRunner_PreFlightHookMaterialization(t *testing.T) {
	fakeBin := makeFakeAutopilot(t)
	repoDir := initTestGitRepo(t)

	var hookInvoked bool
	var hookStoryID string
	var hookWorkDir string
	origHook := engines.PreFlightHook
	defer func() { engines.PreFlightHook = origHook }()

	engines.PreFlightHook = func(ctx context.Context, workDir, storyID string) error {
		hookInvoked = true
		hookStoryID = storyID
		hookWorkDir = workDir
		return nil
	}

	runner := NewPromptRunner(fakeBin)
	opts := RunPromptOptions{
		PromptID:   "test-prompt-hook",
		StoryID:    "STORY-HOOK-333",
		Repo:       "test-submodule",
		Prompt:     "Execute task for STORY-HOOK-333",
		WorkingDir: repoDir,
		Timeout:    5 * time.Second,
	}
	res, err := runner.RunPrompt(context.Background(), opts)
	if err != nil {
		t.Fatalf("unexpected RunPrompt error: %v", err)
	}
	if !hookInvoked {
		t.Fatal("expected PreFlightHook to be invoked for story execution")
	}
	if hookStoryID != "STORY-HOOK-333" {
		t.Fatalf("expected hookStoryID STORY-HOOK-333, got %s", hookStoryID)
	}
	if hookWorkDir != repoDir {
		t.Fatalf("expected hookWorkDir %s, got %s", repoDir, hookWorkDir)
	}
	if res.Status != "completed" {
		t.Fatalf("expected completed status, got %s", res.Status)
	}
}

func TestPromptRunner_RunPrompt_DetectsStoryID(t *testing.T) {
	fakeBin := makeFakeAutopilot(t)
	repoDir := initTestGitRepo(t)

	runner := NewPromptRunner(fakeBin)
	opts := RunPromptOptions{
		PromptID:   "test-prompt-detect-story",
		Repo:       "test-submodule",
		Prompt:     "Please implement STORY-GOV-3688 now",
		WorkingDir: repoDir,
		Timeout:    5 * time.Second,
	}

	res, err := runner.RunPrompt(context.Background(), opts)
	if err != nil {
		t.Fatalf("unexpected RunPrompt error: %v", err)
	}
	if res.StoryID != "STORY-GOV-3688" {
		t.Errorf("expected detected StoryID STORY-GOV-3688, got %s", res.StoryID)
	}
}

func TestCLIPromptRunner_BranchPropagation(t *testing.T) {
	fakeBin := makeFakeAutopilot(t)
	repoDir := initTestGitRepo(t)

	runner := NewPromptRunner(fakeBin)
	opts := RunPromptOptions{
		PromptID:   "test-prompt-branch",
		Repo:       "test-submodule",
		Branch:     "feat/forensic-v2",
		Prompt:     "Execute prompt on target branch",
		WorkingDir: repoDir,
		Timeout:    5 * time.Second,
	}

	res, err := runner.RunPrompt(context.Background(), opts)
	if err != nil {
		t.Fatalf("unexpected RunPrompt error: %v", err)
	}
	if res.Branch != "feat/forensic-v2" {
		t.Fatalf("expected PromptResult.Branch 'feat/forensic-v2', got %q", res.Branch)
	}
	if !strings.Contains(res.Summary, "branch=feat/forensic-v2") {
		t.Fatalf("expected AUTOPILOT_TARGET_BRANCH=feat/forensic-v2 in subprocess env, summary=%s", res.Summary)
	}
}
