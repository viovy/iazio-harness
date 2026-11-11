package autopilot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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

		if strings.Contains(promptContent, "SIMULATE_JETSKI_PERMISSION_DENIED") {
			fmt.Println(`jetski: no output produced — a tool required the "command" permission that headless mode cannot prompt for, so it was auto-denied. Add an allow-rule under permissions.allow in settings.json (e.g. command(<target>)). Alternatively, re-run with --dangerously-skip-permissions to auto-approve all tools.`)
			os.Exit(0)
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
	t.Setenv("IAZIO_TEST_SKIP_REMOTE", "1")
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

func TestCLIPromptRunner_DefaultTimeoutAndEnvOverride(t *testing.T) {
	if DefaultPromptTimeout != 7*24*time.Hour {
		t.Fatalf("expected DefaultPromptTimeout=168h, got %v", DefaultPromptTimeout)
	}
	t.Setenv("IAZIO_PROMPT_TIMEOUT_SECONDS", "")
	if got := ResolveDefaultPromptTimeout(); got != 7*24*time.Hour {
		t.Fatalf("expected 168h default, got %v", got)
	}
	t.Setenv("IAZIO_PROMPT_TIMEOUT_SECONDS", "1800")
	if got := ResolveDefaultPromptTimeout(); got != 30*time.Minute {
		t.Fatalf("expected 30m env override, got %v", got)
	}
}

func TestCLIPromptRunner_PreFlightHookFailsOnStoryNotFound(t *testing.T) {
	fakeBin := makeFakeAutopilot(t)
	repoDir := initTestGitRepo(t)

	runner := NewPromptRunner(fakeBin)
	runner.PreFlightHook = func(ctx context.Context, workingDir, storyID string) error {
		return fmt.Errorf("%w: %s", engines.ErrStoryNotFound, storyID)
	}

	_, err := runner.RunPrompt(context.Background(), RunPromptOptions{
		PromptID:   "prompt-STORY-MISSING-404",
		StoryID:    "STORY-MISSING-404",
		Repo:       "test-submodule",
		Prompt:     "Execute missing story",
		WorkingDir: repoDir,
		Timeout:    5 * time.Second,
	})
	if err == nil {
		t.Fatal("expected error when PreFlightHook returns ErrStoryNotFound, got nil")
	}
	if !strings.Contains(err.Error(), "pre-flight story materialization failed for STORY-MISSING-404") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestCLIPromptRunner_TraceParentW3CSanitization(t *testing.T) {
	w3cRe := strings.ToLower
	_ = w3cRe
	traceHex, tp := formatW3CTraceParent("tr-non-hex-trace-id", "prompt-STORY-GOV-3806")
	if len(traceHex) != 32 {
		t.Fatalf("expected 32-char hex traceID, got %q", traceHex)
	}
	parts := strings.Split(tp, "-")
	if len(parts) != 4 || parts[0] != "00" || len(parts[1]) != 32 || len(parts[2]) != 16 || parts[3] != "01" {
		t.Fatalf("expected valid W3C traceparent 00-<32hex>-<16hex>-01, got %q", tp)
	}
	if !hex32Regex.MatchString(parts[1]) || !hex16Regex.MatchString(parts[2]) {
		t.Fatalf("expected lowercase hex traceId and spanId in %q", tp)
	}
}

func TestCLIPromptRunner_FailsOnJetskiHeadlessPermissionDenial(t *testing.T) {
	fakeBin := makeFakeAutopilot(t)
	repoDir := initTestGitRepo(t)

	runner := NewPromptRunner(fakeBin)
	opts := RunPromptOptions{
		PromptID:    "job_54dcf0141811092f",
		Repo:        "iazio-web",
		Title:       "Sort prompts alphabetically in iazio-web /prompts view",
		Prompt:      "SIMULATE_JETSKI_PERMISSION_DENIED",
		TargetAgent: "agy",
		WorkingDir:  repoDir,
		Timeout:     5 * time.Second,
	}

	res, err := runner.RunPrompt(context.Background(), opts)
	if err == nil {
		t.Fatal("expected error when agy outputs jetski headless permission denial with exit code 0, got nil")
	}
	if res == nil {
		t.Fatal("expected non-nil PromptResult")
	}
	if res.Status != "failed" {
		t.Fatalf("expected status 'failed', got %q", res.Status)
	}
	if res.ExitCode == 0 {
		t.Fatalf("expected non-zero exit code, got %d", res.ExitCode)
	}
	if !strings.Contains(err.Error(), "PERMISSION_DENIED") {
		t.Fatalf("expected PERMISSION_DENIED error, got: %v", err)
	}
}

func TestDiscoverLogFileConversation_And_DiscoverStoryFromASEState(t *testing.T) {
	if got := discoverLogFileConversation(""); got != "" {
		t.Fatalf("expected empty conv ID for blank log path, got %q", got)
	}
	if got := discoverLogFileConversation("/nonexistent/path/agy.log"); got != "" {
		t.Fatalf("expected empty conv ID for missing log path, got %q", got)
	}

	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "agy-run.log")
	if err := os.WriteFile(logPath, []byte("2026-10-31 INFO Created conversation conv-logfile-998877665544\n"), 0o600); err != nil {
		t.Fatalf("write log file: %v", err)
	}
	if got := discoverLogFileConversation(logPath); got != "conv-logfile-998877665544" {
		t.Fatalf("expected conv-logfile-998877665544, got %q", got)
	}

	if sid, stitle, spath := discoverStoryFromASEState("", time.Now()); sid != "" || stitle != "" || spath != "" {
		t.Fatalf("expected empty story result for blank workingDir, got %q %q %q", sid, stitle, spath)
	}

	after := time.Now().Add(-5 * time.Second)
	wsDir := filepath.Join(tmpDir, "ws")
	if err := os.MkdirAll(filepath.Join(wsDir, ".autopilot"), 0o755); err != nil {
		t.Fatalf("mkdir .autopilot: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(wsDir, "docs", "stories"), 0o755); err != nil {
		t.Fatalf("mkdir docs/stories: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wsDir, ".session_story_id"), []byte("STORY-GOV-3839\n"), 0o600); err != nil {
		t.Fatalf("write .session_story_id: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wsDir, ".autopilot", "ase-state.json"), []byte(`{"story_id":"STORY-GOV-3839"}`), 0o600); err != nil {
		t.Fatalf("write ase-state.json: %v", err)
	}
	storyFile := filepath.Join(wsDir, "docs", "stories", "2026-10-31-gov-3839.story.md")
	storyBody := "---\nid: STORY-GOV-3839\ntitle: \"ASE State Discovery Test\"\nstatus: IN_PROGRESS\n---\n"
	if err := os.WriteFile(storyFile, []byte(storyBody), 0o600); err != nil {
		t.Fatalf("write story file: %v", err)
	}

	sid, stitle, spath := discoverStoryFromASEState(wsDir, after)
	if sid != "STORY-GOV-3839" {
		t.Fatalf("expected STORY-GOV-3839, got %q", sid)
	}
	if stitle != "ASE State Discovery Test" {
		t.Fatalf("expected ASE State Discovery Test, got %q", stitle)
	}
	if spath != "docs/stories/2026-10-31-gov-3839.story.md" {
		t.Fatalf("expected docs/stories/2026-10-31-gov-3839.story.md, got %q", spath)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	var mu sync.Mutex
	var buf bytes.Buffer
	lastOut := time.Now()
	streamed := false
	conv := ""
	csw := &chunkStreamingWriter{
		mu: &mu, buf: &buf, lastOutAt: &lastOut, streamedAny: &streamed,
		convDetected: &conv, ctx: context.Background(), apiURL: srv.URL, token: "tok", jobID: "job_1", stream: "stdout",
	}
	if n, err := csw.Write(nil); err != nil || n != 0 {
		t.Fatalf("expected 0, nil for empty write, got %d %v", n, err)
	}
	msg := []byte("Created conversation conv-stream-112233445566\n")
	if n, err := csw.Write(msg); err != nil || n != len(msg) {
		t.Fatalf("write failed: %d %v", n, err)
	}
	if conv != "conv-stream-112233445566" || !streamed {
		t.Fatalf("expected conv-stream-112233445566 and streamed=true, got %q %v", conv, streamed)
	}

	wrapper := makeFakeAutopilot(t)
	runner := NewPromptRunner(wrapper)
	res, err := runner.RunPrompt(context.Background(), RunPromptOptions{
		PromptID:     "prompt_stream_cov",
		JobID:        "job_stream_cov",
		APIURL:       srv.URL,
		Token:        "tok",
		FencingToken: 7,
		Repo:         "iazio-web",
		Title:        "ASE State Discovery Test",
		Prompt:       "normal execution prompt",
		TargetAgent:  "autopilot",
		WorkingDir:   wsDir,
		Timeout:      10 * time.Second,
	})
	if err != nil {
		t.Fatalf("RunPrompt with streaming failed: %v", err)
	}
	if res.StoryID != "STORY-GOV-3839" {
		t.Fatalf("expected StoryID STORY-GOV-3839 from ASE state, got %q", res.StoryID)
	}
}

func TestTailBrainTranscriptSteps_And_StartupBanner(t *testing.T) {
	brainDir := t.TempDir()
	t.Setenv("IAZIO_BRAIN_DIR", brainDir)

	convID := "ba042127-c63f-4b1e-a42e-95b307e61bbc"
	logsDir := filepath.Join(brainDir, convID, ".system_generated", "logs")
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		t.Fatalf("mkdir logsDir: %v", err)
	}
	transcriptFile := filepath.Join(logsDir, "transcript.jsonl")
	lines := strings.Join([]string{
		`{"step_index":1,"type":"USER_INPUT","status":"DONE","created_at":"2026-10-31T14:00:00Z","content":"/autonomous-story-execution"}`,
		`{"step_index":2,"type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-10-31T14:00:05Z","thinking":"Inspecting live job state","content":"Running git status check.","tool_calls":[{"name":"run_command","arguments":"{\"CommandLine\":\"git status -sb\",\"toolSummary\":\"Check git status\"}"}]}`,
		`{"step_index":3,"type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-10-31T14:00:10Z","thinking":"Reading prompt runner","tool_calls":[{"name":"read_file","arguments":{"Path":"/home/romeo/work/meta-repo/iazio-harness/internal/autopilot/prompt_runner.go"}}]}`,
	}, "\n") + "\n"
	if err := os.WriteFile(transcriptFile, []byte(lines), 0o600); err != nil {
		t.Fatalf("write transcript.jsonl: %v", err)
	}

	seen := make(map[int]bool)
	out1 := tailBrainTranscriptSteps(convID, seen)
	if !strings.Contains(out1, "[agy step #2] thinking: Inspecting live job state") {
		t.Fatalf("expected thinking in out1, got:\n%s", out1)
	}
	if !strings.Contains(out1, "[agy step #2] tool run_command: Check git status — `git status -sb`") {
		t.Fatalf("expected tool_call summary in out1, got:\n%s", out1)
	}
	if !strings.Contains(out1, "[agy step #2] output: Running git status check.") {
		t.Fatalf("expected content in out1, got:\n%s", out1)
	}
	if !strings.Contains(out1, "[agy step #3] tool read_file: /home/romeo/work/meta-repo/iazio-harness/internal/autopilot/prompt_runner.go") {
		t.Fatalf("expected step #3 read_file path in out1, got:\n%s", out1)
	}

	// Second call with no new steps must return empty string
	if out2 := tailBrainTranscriptSteps(convID, seen); out2 != "" {
		t.Fatalf("expected empty out2 when no new steps added, got %q", out2)
	}

	var chunkMu sync.Mutex
	var postedChunks []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/chunks") {
			b, _ := os.ReadFile("/dev/null")
			_ = b
			var payload map[string]any
			_ = json.NewDecoder(r.Body).Decode(&payload)
			if payload["type"] == "OUTPUT_CHUNK" {
				if txt, ok := payload["text"].(string); ok {
					chunkMu.Lock()
					postedChunks = append(postedChunks, txt)
					chunkMu.Unlock()
				}
			}
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	wrapper := makeFakeAutopilot(t)
	repoDir := initTestGitRepo(t)
	storyDir := filepath.Join(repoDir, "docs", "stories")
	_ = os.MkdirAll(storyDir, 0o755)
	_ = os.WriteFile(filepath.Join(storyDir, "2026-11-09-gov-3850.story.md"), []byte("---\nid: STORY-GOV-3850\nstatus: DONE\n---\n"), 0o644)
	_ = exec.Command("git", "-C", repoDir, "add", ".").Run()
	_ = exec.Command("git", "-C", repoDir, "commit", "-m", "chore: add completed story").Run()
	runner := NewPromptRunner(wrapper)
	_, err := runner.RunPrompt(context.Background(), RunPromptOptions{
		PromptID:    "prompt_banner_test",
		JobID:       "job_3ffa5d0be1649f0e",
		APIURL:      srv.URL,
		Token:       "tok",
		Repo:        "meta-repo-02",
		Title:       "Banner and Transcript Test",
		Prompt:      "/autonomous-story-execution STORY-GOV-3850",
		TargetAgent: "autopilot",
		WorkingDir:  repoDir,
		Timeout:     5 * time.Second,
	})
	if err != nil {
		t.Fatalf("RunPrompt failed: %v", err)
	}

	chunkMu.Lock()
	joinedChunks := strings.Join(postedChunks, "\n")
	chunkMu.Unlock()
	if !strings.Contains(joinedChunks, "[ASE-SUPERVISOR] Starting prompt execution for Job job_3ffa5d0be1649f0e") {
		t.Fatalf("expected startup banner chunk in postedChunks, got:\n%s", joinedChunks)
	}
}

func TestBackgroundDrainPIDLivenessAndAutoResume_Unit(t *testing.T) {
	// 1. Unit test helper functions directly
	logFile := filepath.Join(t.TempDir(), "agy.log")
	_ = os.WriteFile(logFile, []byte("2026/10/31 16:04:01 WaitForConversationFullyIdle: root agent idle; waiting up to 1h0m0s for 2 background task(s)\n"), 0o600)

	if ok, cnt := hasBackgroundDrainLog("", "some line\nWaitForConversationFullyIdle: root agent idle; waiting up to 1h0m0s for 3 background task(s)\n", ""); !ok || cnt != 3 {
		t.Fatalf("expected drain=true cnt=3 from stdout, got %v %d", ok, cnt)
	}
	if ok, cnt := hasBackgroundDrainLog("", "", "WaitForConversationFullyIdle: custom format\n"); !ok || cnt != 1 {
		t.Fatalf("expected drain=true cnt=1 from stderr fallback, got %v %d", ok, cnt)
	}
	if ok, cnt := hasBackgroundDrainLog("", "WaitForConversationFullyIdle: root agent idle; waiting up to 1h0m0s for 1 background task(s)\n[agy step #5] output: resumed active work\n", ""); ok || cnt != 0 {
		t.Fatalf("expected drain=false when newer non-drain line follows, got %v %d", ok, cnt)
	}
	if ok, cnt := hasBackgroundDrainLog(logFile, "", ""); !ok || cnt != 2 {
		t.Fatalf("expected drain=true cnt=2 from logPath, got %v %d", ok, cnt)
	}

	ptLogFile := filepath.Join(t.TempDir(), "pt.log")
	_ = os.WriteFile(ptLogFile, []byte("print timeout after 15m0s with turn in progress\n"), 0o600)
	if !hasPrintTimeoutLog("", "Error: print timeout after 15m0s with turn in progress", "") {
		t.Fatalf("expected printTimeout=true from stdout")
	}
	if !hasPrintTimeoutLog(ptLogFile, "", "") {
		t.Fatalf("expected printTimeout=true from logPath")
	}
	if hasPrintTimeoutLog("", "normal output", "") {
		t.Fatalf("expected printTimeout=false on normal output")
	}

	wsDir := t.TempDir()
	storiesDir := filepath.Join(wsDir, "docs", "stories")
	_ = os.MkdirAll(storiesDir, 0o755)
	storyPath := filepath.Join(storiesDir, "2026-10-31-gov-3850-test.story.md")
	_ = os.WriteFile(storyPath, []byte("---\nid: STORY-GOV-3850\nstatus: IN_PROGRESS\n---\n# Story\n"), 0o600)
	if !isStoryIncompleteInWorktree(wsDir, "STORY-GOV-3850") {
		t.Fatalf("expected story IN_PROGRESS to be incomplete")
	}
	if isStoryIncompleteInWorktree("", "STORY-GOV-3850") || isStoryIncompleteInWorktree(wsDir, "") {
		t.Fatalf("expected empty args to return false")
	}
	_ = os.WriteFile(storyPath, []byte("---\nid: STORY-GOV-3850\nstatus: DONE\n---\n# Story\n"), 0o600)
	if isStoryIncompleteInWorktree(wsDir, "STORY-GOV-3850") {
		t.Fatalf("expected story DONE to not be incomplete")
	}

	brainDir := t.TempDir()
	t.Setenv("IAZIO_BRAIN_DIR", brainDir)
	convID := "ba042127-c63f-4b1e-a42e-95b307e61bbc"
	tpDir := filepath.Join(brainDir, convID, ".system_generated", "logs")
	_ = os.MkdirAll(tpDir, 0o755)
	_ = os.WriteFile(filepath.Join(tpDir, "transcript.jsonl"), []byte(`{"step_index":1}`+"\n"), 0o600)

	if chatty, _ := hasActiveConversationActivity(convID, os.Getpid(), time.Now(), 0); !chatty {
		t.Fatalf("expected chatty=true with recent lastTranscriptOrOutputAt")
	}
	if chatty, _ := hasActiveConversationActivity(convID, 0, time.Time{}, 10*time.Second); !chatty {
		t.Fatalf("expected chatty=true from fresh transcript.jsonl modtime")
	}

	r := NewPromptRunner("autopilot")
	if d := r.resolveBackgroundDrainGracePeriod(RunPromptOptions{BackgroundDrainGracePeriod: 250 * time.Millisecond}); d != 250*time.Millisecond {
		t.Fatalf("unexpected opts grace: %v", d)
	}
	r.BackgroundDrainGracePeriod = 350 * time.Millisecond
	if d := r.resolveBackgroundDrainGracePeriod(RunPromptOptions{}); d != 350*time.Millisecond {
		t.Fatalf("unexpected runner grace: %v", d)
	}
	r.BackgroundDrainGracePeriod = 0
	t.Setenv("IAZIO_BG_DRAIN_GRACE_MS", "120")
	if d := r.resolveBackgroundDrainGracePeriod(RunPromptOptions{}); d != 120*time.Millisecond {
		t.Fatalf("unexpected env ms grace: %v", d)
	}
	t.Setenv("IAZIO_BG_DRAIN_GRACE_MS", "")
	t.Setenv("IAZIO_BG_DRAIN_GRACE_SECONDS", "2")
	if d := r.resolveBackgroundDrainGracePeriod(RunPromptOptions{}); d != 2*time.Second {
		t.Fatalf("unexpected env sec grace: %v", d)
	}
	t.Setenv("IAZIO_BG_DRAIN_GRACE_SECONDS", "")
	t.Setenv("MOCK_AGY_FAST", "1")
	t.Setenv("IAZIO_TEST_SKIP_REMOTE", "1")
	if d := r.resolveBackgroundDrainGracePeriod(RunPromptOptions{}); d != 150*time.Millisecond {
		t.Fatalf("unexpected MOCK_AGY_FAST grace: %v", d)
	}

	maxOpt := 4
	if m := r.resolveMaxAutoResumeAttempts(RunPromptOptions{MaxAutoResumeAttempts: &maxOpt}); m != 4 {
		t.Fatalf("unexpected opts maxAutoResume: %d", m)
	}
	maxRunner := 3
	r.MaxAutoResumeAttempts = &maxRunner
	if m := r.resolveMaxAutoResumeAttempts(RunPromptOptions{}); m != 3 {
		t.Fatalf("unexpected runner maxAutoResume: %d", m)
	}
	r.MaxAutoResumeAttempts = nil
	t.Setenv("IAZIO_MAX_AUTO_RESUME_ATTEMPTS", "5")
	if m := r.resolveMaxAutoResumeAttempts(RunPromptOptions{}); m != 5 {
		t.Fatalf("unexpected env maxAutoResume: %d", m)
	}
	t.Setenv("IAZIO_MAX_AUTO_RESUME_ATTEMPTS", "")

	// 2. Test RunPrompt end-to-end with a mock-agy script that triggers timeout deferral, background drain reap, and auto-resume
	binDir := t.TempDir()
	mockAgyBin := filepath.Join(binDir, "mock-agy")
	script := `#!/bin/sh
LOG_FILE=""
CONV_ARG=""
while [ $# -gt 0 ]; do
  case "$1" in
    --log-file=*) LOG_FILE="${1#*=}" ;;
    --log-file) shift; LOG_FILE="$1" ;;
    --conversation=*) CONV_ARG="${1#*=}" ;;
    --conversation) shift; CONV_ARG="$1" ;;
  esac
  shift
done

if [ -z "$CONV_ARG" ]; then
  mkdir -p .autopilot docs/stories
  echo '{"story_id":"STORY-GOV-3850"}' > .autopilot/ase-state.json
  printf -- "---\nid: STORY-GOV-3850\nstatus: IN_PROGRESS\n---\n# Story\n" > docs/stories/2026-10-31-gov-3850.story.md
  echo "Created conversation ba042127-c63f-4b1e-a42e-95b307e61bbc"
  if [ -n "$LOG_FILE" ]; then
    echo "Created conversation ba042127-c63f-4b1e-a42e-95b307e61bbc" >> "$LOG_FILE"
  fi
  sleep 0.12
  echo "Still active on turn 1"
  sleep 0.12
  echo "WaitForConversationFullyIdle: root agent idle; waiting up to 1h0m0s for 1 background task(s)"
  if [ -n "$LOG_FILE" ]; then
    echo "WaitForConversationFullyIdle: root agent idle; waiting up to 1h0m0s for 1 background task(s)" >> "$LOG_FILE"
  fi
  sleep 5
  exit 0
else
  printf -- "---\nid: STORY-GOV-3850\nstatus: DONE\n---\n# Story\n" > docs/stories/2026-10-31-gov-3850.story.md
  git add docs/stories/2026-10-31-gov-3850.story.md >/dev/null 2>&1 || true
  git commit -m "feat: complete STORY-GOV-3850" >/dev/null 2>&1 || true
  echo "Resumed conversation $CONV_ARG and completed STORY-GOV-3850"
  exit 0
fi
`
	if err := os.WriteFile(mockAgyBin, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock-agy script: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	repoDir := initTestGitRepo(t)
	runner := NewPromptRunner("autopilot")
	runner.GracePeriod = 50 * time.Millisecond
	res, err := runner.RunPrompt(context.Background(), RunPromptOptions{
		PromptID:                   "prompt_bg_drain_unit",
		JobID:                      "job_3ffa5d0be1649f0e",
		APIURL:                     srv.URL,
		Token:                      "tok",
		Repo:                       "meta-repo-02",
		Prompt:                     "/autonomous-story-execution STORY-GOV-3850",
		TargetAgent:                mockAgyBin,
		WorkingDir:                 repoDir,
		Timeout:                    180 * time.Millisecond,
		BackgroundDrainGracePeriod: 120 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("expected auto-resumed RunPrompt to succeed, got err=%v", err)
	}
	if res.Status != "completed" || res.ResumeAttempts < 1 || res.ActiveConversationID != convID {
		t.Fatalf("unexpected result: %+v", res)
	}

	// 3. Test fail-closed BACKGROUND_DRAIN_TIMEOUT when MaxAutoResumeAttempts = 0
	repoDir2 := initTestGitRepo(t)
	zeroResumes := 0
	res2, err2 := runner.RunPrompt(context.Background(), RunPromptOptions{
		PromptID:                   "prompt_bg_drain_failclosed",
		JobID:                      "job_3ffa5d0be1649f0e_fc",
		APIURL:                     srv.URL,
		Token:                      "tok",
		Repo:                       "meta-repo-02",
		Prompt:                     "/autonomous-story-execution STORY-GOV-3850",
		TargetAgent:                mockAgyBin,
		WorkingDir:                 repoDir2,
		Timeout:                    180 * time.Millisecond,
		BackgroundDrainGracePeriod: 120 * time.Millisecond,
		MaxAutoResumeAttempts:      &zeroResumes,
	})
	if err2 == nil {
		t.Fatalf("expected error when auto-resume is disabled on background drain")
	}
	if res2 == nil || res2.Status != "timed_out" || res2.FailureCategory != "BACKGROUND_DRAIN_TIMEOUT" || res2.FailureStage != "BACKGROUND_DRAIN" {
		t.Fatalf("expected fail-closed BACKGROUND_DRAIN_TIMEOUT, got %+v", res2)
	}
}

func TestJob110f47f09551baba_ASECompletionDuringBackgroundDrainAndServiceFaultClassification(t *testing.T) {
	brainDir := t.TempDir()
	t.Setenv("IAZIO_BRAIN_DIR", brainDir)

	repoDir := initTestGitRepo(t)
	storiesDir := filepath.Join(repoDir, "docs", "stories")
	if err := os.MkdirAll(storiesDir, 0o755); err != nil {
		t.Fatalf("mkdir stories: %v", err)
	}
	primaryStoryPath := filepath.Join(storiesDir, "2026-11-01-gov-3873-prompts-alphabetical-sorting-and-ui-verification.story.md")
	pointerStoryPath := filepath.Join(storiesDir, "2026-11-01-gov-pointers-20261101-044815.story.md")

	if err := os.WriteFile(primaryStoryPath, []byte("---\nid: STORY-GOV-3873\ntitle: Prompts Alphabetical Sorting and UI Verification\nstatus: IN_PROGRESS\n---\n"), 0o600); err != nil {
		t.Fatalf("write primary story: %v", err)
	}
	if err := os.WriteFile(pointerStoryPath, []byte("---\nid: STORY-GOV-POINTERS-20261101-044815\ntitle: Reconcile Submodule Pointers\nstatus: DONE\n---\n"), 0o600); err != nil {
		t.Fatalf("write pointer story: %v", err)
	}

	step321Out := "[2026-11-01T05:39:45Z] [agy step #321] output: Autonomous story execution is complete for both [`STORY-GOV-3873`](file:///home/romeo/work/meta-repo/docs/stories/2026-11-01-gov-3873-prompts-alphabetical-sorting-and-ui-verification.story.md) and [`STORY-GOV-POINTERS-20261101-044815`](file:///home/romeo/work/meta-repo/docs/stories/2026-11-01-gov-pointers-20261101-044815.story.md)."

	// Must be false while STORY-GOV-3873 is still IN_PROGRESS or uncommitted
	if isASEFlowCompletedInWorktree(repoDir, "STORY-GOV-3873", step321Out, "git") {
		t.Fatalf("expected isASEFlowCompletedInWorktree=false while STORY-GOV-3873 is IN_PROGRESS")
	}

	// Mark STORY-GOV-3873 DONE and commit cleanly
	if err := os.WriteFile(primaryStoryPath, []byte("---\nid: STORY-GOV-3873\ntitle: Prompts Alphabetical Sorting and UI Verification\nstatus: DONE\n---\n"), 0o600); err != nil {
		t.Fatalf("write primary story DONE: %v", err)
	}
	for _, args := range [][]string{
		{"add", "docs"},
		{"commit", "-m", "chore: complete STORY-GOV-3873 and pointer story"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, string(out))
		}
	}

	// Write ephemeral files (.session_story_id with pointer story, .autopilot/ase-state.json with primary story)
	_ = os.WriteFile(filepath.Join(repoDir, ".session_story_id"), []byte("STORY-GOV-POINTERS-20261101-044815\n"), 0o600)
	_ = os.MkdirAll(filepath.Join(repoDir, ".autopilot"), 0o755)
	_ = os.WriteFile(filepath.Join(repoDir, ".autopilot", "ase-state.json"), []byte(`{"story_id":"STORY-GOV-3873","title":"Prompts Alphabetical Sorting and UI Verification"}`), 0o600)

	// Verify pointer story deprioritization in discoverStoryFromASEState
	discID, _, _ := discoverStoryFromASEState(repoDir, time.Now().Add(-10*time.Second))
	if discID != "STORY-GOV-3873" {
		t.Fatalf("expected discoverStoryFromASEState to prefer non-pointer STORY-GOV-3873 over STORY-GOV-POINTERS-*, got %q", discID)
	}

	// Verify isASEFlowCompletedInWorktree is true with clean non-ephemeral worktree + DONE stories
	if !isASEFlowCompletedInWorktree(repoDir, "STORY-GOV-3873", step321Out, "git") {
		t.Fatalf("expected isASEFlowCompletedInWorktree=true when stories are DONE and worktree has only ephemeral files")
	}

	// End-to-end RunPrompt test reproducing job_110f47f09551baba
	convID := "18f40eb1-d86f-41bf-a69f-7d9de6536cb6"
	binDir := t.TempDir()
	mockAgyBin := filepath.Join(binDir, "mock-agy")
	script := fmt.Sprintf(`#!/bin/sh
LOG_FILE=""
while [ $# -gt 0 ]; do
  case "$1" in
    --log-file=*) LOG_FILE="${1#*=}" ;;
    --log-file) shift; LOG_FILE="$1" ;;
  esac
  shift
done

if [ -n "$LOG_FILE" ]; then
  echo "Created conversation %s" >> "$LOG_FILE"
fi
mkdir -p "%s/%s/.system_generated/logs"
cat << 'EOF' > "%s/%s/.system_generated/logs/transcript.jsonl"
{"step_index":1,"type":"USER_INPUT","content":"/autonomous-story-execution STORY-GOV-3873"}
{"step_index":321,"type":"PLANNER_RESPONSE","content":"Autonomous story execution is complete for both [STORY-GOV-3873](file:///home/romeo/work/meta-repo/docs/stories/2026-11-01-gov-3873-prompts-alphabetical-sorting-and-ui-verification.story.md) and [STORY-GOV-POINTERS-20261101-044815](file:///home/romeo/work/meta-repo/docs/stories/2026-11-01-gov-pointers-20261101-044815.story.md)."}
EOF
if [ -n "$LOG_FILE" ]; then
  echo "2026/11/01 05:39:45 WaitForConversationFullyIdle: root agent idle; waiting up to 1h0m0s for 1 background task(s)" >> "$LOG_FILE"
fi
sleep 5
exit 0
`, convID, brainDir, convID, brainDir, convID)

	if err := os.WriteFile(mockAgyBin, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock-agy script: %v", err)
	}

	// Simulate transient 503 on first chunk post to verify control-plane release/restart resilience
	var postCount int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		postCount++
		c := postCount
		mu.Unlock()
		if c == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	runner := NewPromptRunner("autopilot")
	runner.GracePeriod = 50 * time.Millisecond
	zeroResumes := 0
	res, err := runner.RunPrompt(context.Background(), RunPromptOptions{
		PromptID:                   "prompt_job_110f47f09551baba",
		JobID:                      "job_110f47f09551baba",
		APIURL:                     srv.URL,
		Token:                      "tok",
		FencingToken:               1,
		Repo:                       "meta-repo",
		Prompt:                     "/autonomous-story-execution STORY-GOV-3873",
		TargetAgent:                mockAgyBin,
		WorkingDir:                 repoDir,
		Timeout:                    2 * time.Second,
		BackgroundDrainGracePeriod: 100 * time.Millisecond,
		MaxAutoResumeAttempts:      &zeroResumes,
	})
	if err != nil {
		t.Fatalf("expected job_110f47f09551baba to complete cleanly during background drain, got err=%v", err)
	}
	if res.Status != "completed" || res.ExitCode != 0 {
		t.Fatalf("expected Status=completed ExitCode=0, got Status=%q ExitCode=%d", res.Status, res.ExitCode)
	}
	if res.FailureCategory != "" || res.FailureStage != "" || res.FailureDirection != "NONE" {
		t.Fatalf("expected clean failure classification on completed job, got category=%q stage=%q direction=%q", res.FailureCategory, res.FailureStage, res.FailureDirection)
	}
	if res.StoryID != "STORY-GOV-3873" {
		t.Fatalf("expected primary StoryID STORY-GOV-3873 (not pointer story), got %q", res.StoryID)
	}
	if res.ResumeAttempts != 0 {
		t.Fatalf("expected 0 auto-resume attempts when ASE flow already completed in Turn 1, got %d", res.ResumeAttempts)
	}
	if !strings.Contains(res.Summary, "Autonomous story execution is complete for both") {
		t.Fatalf("expected transcript step #321 in Summary, got %q", res.Summary)
	}

	// Verify Upstream vs Downstream vs Local Execution fault classification
	cat, stg, dir, svc := ClassifyPromptFailure("failed", "", "", "ResourceExhausted: 429 quota exceeded", nil)
	if cat != "UPSTREAM_MODEL_UNAVAILABLE" || stg != "LLM_INFERENCE" || dir != "UPSTREAM" || svc != "antigravity-llm-provider" {
		t.Fatalf("unexpected LLM upstream classification: %q %q %q %q", cat, stg, dir, svc)
	}
	cat, stg, dir, svc = ClassifyPromptFailure("failed", "", "", "gh pr merge failed: api.github.com 502 Bad Gateway", nil)
	if cat != "UPSTREAM_SCM_UNAVAILABLE" || stg != "GIT_DELIVERY" || dir != "UPSTREAM" || svc != "github-api" {
		t.Fatalf("unexpected GitHub upstream classification: %q %q %q %q", cat, stg, dir, svc)
	}
	cat, stg, dir, svc = ClassifyPromptFailure("failed", "", "", "jenkins build check timed out: 503 Service Unavailable", nil)
	if cat != "UPSTREAM_CI_UNAVAILABLE" || stg != "CI_VERIFICATION" || dir != "UPSTREAM" || svc != "jenkins-ci" {
		t.Fatalf("unexpected Jenkins upstream classification: %q %q %q %q", cat, stg, dir, svc)
	}
	cat, stg, dir, svc = ClassifyPromptFailure("failed", "", "", "controlplane post failed: iazio-harness-api connection refused", nil)
	if cat != "DOWNSTREAM_CONTROL_PLANE_UNAVAILABLE" || stg != "CONTROL_PLANE_SYNC" || dir != "DOWNSTREAM" || svc != "iazio-harness-api" {
		t.Fatalf("unexpected downstream control-plane classification: %q %q %q %q", cat, stg, dir, svc)
	}
	cat, stg, dir, svc = ClassifyPromptFailure("timed_out", "BACKGROUND_DRAIN_TIMEOUT", "BACKGROUND_DRAIN", "", nil)
	if cat != "BACKGROUND_DRAIN_TIMEOUT" || stg != "BACKGROUND_DRAIN" || dir != "LOCAL_EXECUTION" || svc != "agy-cli" {
		t.Fatalf("unexpected background drain local classification: %q %q %q %q", cat, stg, dir, svc)
	}
}

func TestJob9a39902fd8c9aabf_InitialResumeConversationHeartbeatAndSubmoduleResidualGate(t *testing.T) {
	brainDir := t.TempDir()
	t.Setenv("IAZIO_BRAIN_DIR", brainDir)

	repoDir := initTestGitRepo(t)
	subDir := filepath.Join(repoDir, "iazio-agent")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatalf("mkdir submodule: %v", err)
	}
	for _, args := range [][]string{
		{"init"},
		{"config", "user.name", "Test User"},
		{"config", "user.email", "test@example.com"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = subDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("sub git %v: %v (%s)", args, err, string(out))
		}
	}
	subMain := filepath.Join(subDir, "main.go")
	if err := os.WriteFile(subMain, []byte("package main\n"), 0o600); err != nil {
		t.Fatalf("write sub main.go: %v", err)
	}
	for _, args := range [][]string{
		{"add", "main.go"},
		{"commit", "-m", "initial submodule commit"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = subDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("sub git %v: %v (%s)", args, err, string(out))
		}
	}
	for _, args := range [][]string{
		{"add", "iazio-agent"},
		{"commit", "-m", "chore: track iazio-agent submodule gitlink"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("super git %v: %v (%s)", args, err, string(out))
		}
	}

	pinnedConv := "29c18bab-aa8a-4d78-8426-4808d832582a"
	binDir := t.TempDir()
	mockAgyBin := filepath.Join(binDir, "mock-agy")
	stateMarker := filepath.Join(binDir, "turn_count")
	script := fmt.Sprintf(`#!/bin/sh
CONV_ARG=""
while [ $# -gt 0 ]; do
  case "$1" in
    --conversation=*) CONV_ARG="${1#*=}" ;;
    --conversation) shift; CONV_ARG="$1" ;;
  esac
  shift
done

if [ "$CONV_ARG" != "%s" ]; then
  echo "ERROR: expected --conversation=%s, got $CONV_ARG" >&2
  exit 2
fi

if [ ! -f "%s" ]; then
  echo "1" > "%s"
  # Simulate leaving dirty modified file inside submodule without completing ASE
  echo "package main // dirty residual" > iazio-agent/main.go
  echo "Resumed conversation $CONV_ARG turn 1 (left dirty submodule)"
  exit 0
else
  # Turn 2 (auto-resumed): commit submodule cleanly, sync gitlink, and mark story DONE
  git -C iazio-agent add main.go >/dev/null 2>&1 || true
  git -C iazio-agent commit -m "fix: commit submodule residual" >/dev/null 2>&1 || true
  mkdir -p docs/stories
  printf -- "---\nid: STORY-IAZIO-0183\nstatus: DONE\n---\n# Story\n" > docs/stories/2026-11-04-iazio-0183.story.md
  git add iazio-agent docs/stories/2026-11-04-iazio-0183.story.md >/dev/null 2>&1 || true
  git commit -m "chore: complete STORY-IAZIO-0183" >/dev/null 2>&1 || true
  echo "Autonomous story execution is complete for STORY-IAZIO-0183 on $CONV_ARG"
  exit 0
fi
`, pinnedConv, pinnedConv, stateMarker, stateMarker)
	if err := os.WriteFile(mockAgyBin, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock-agy: %v", err)
	}

	var hbMu sync.Mutex
	var hbReqs []map[string]any
	var completeReqs []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/heartbeat") {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			hbMu.Lock()
			hbReqs = append(hbReqs, body)
			hbMu.Unlock()
		} else if strings.HasSuffix(r.URL.Path, "/complete") {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			hbMu.Lock()
			completeReqs = append(completeReqs, body)
			hbMu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	wdFile := filepath.Join(t.TempDir(), "harness-watchdog.json")
	liveDir := filepath.Join(t.TempDir(), "liveness")
	t.Setenv("IAZIO_HARNESS_WATCHDOG_FILE", wdFile)
	t.Setenv("IAZIO_LIVENESS_DIR", liveDir)
	t.Setenv("IAZIO_FORCE_DIRECT_COMPLETE", "1")

	runner := NewPromptRunner("autopilot")
	runner.GracePeriod = 50 * time.Millisecond
	res, err := runner.RunPrompt(context.Background(), RunPromptOptions{
		PromptID:             "prompt_job_9a39902fd8c9aabf",
		JobID:                "job_9a39902fd8c9aabf",
		APIURL:               srv.URL,
		Token:                "tok-9a3990",
		LeaseToken:           "lease-9a3990",
		FencingToken:         12,
		ResumeConversationID: pinnedConv,
		Repo:                 "meta-repo",
		Prompt:               "/autonomous-story-execution implement STORY-IAZIO-0183",
		TargetAgent:          mockAgyBin,
		WorkingDir:           repoDir,
		Timeout:              5 * time.Second,
	})
	if err != nil {
		t.Fatalf("expected RunPrompt to auto-resume and complete cleanly, got err=%v", err)
	}
	if res.ActiveConversationID != pinnedConv {
		t.Fatalf("expected ActiveConversationID=%q, got %q", pinnedConv, res.ActiveConversationID)
	}
	if res.ResumeAttempts != 1 {
		t.Fatalf("expected 1 auto-resume attempt triggered by dirty submodule file, got %d", res.ResumeAttempts)
	}
	hbMu.Lock()
	hbCount := len(hbReqs)
	compCount := len(completeReqs)
	hbMu.Unlock()
	if hbCount == 0 {
		t.Fatalf("expected built-in PostJobHeartbeat requests to /api/v1/agent/jobs/{id}/heartbeat, got 0")
	}
	if compCount == 0 {
		t.Fatalf("expected orphan fallback PostAgentJobComplete request to /api/v1/agent/jobs/{id}/complete, got 0")
	}
	if _, err := os.Stat(filepath.Join(liveDir, pinnedConv+".json")); err != nil {
		t.Fatalf("expected conversation liveness record at %s: %v", filepath.Join(liveDir, pinnedConv+".json"), err)
	}
	if _, err := os.Stat(wdFile); err != nil {
		t.Fatalf("expected local watchdog state file at %s: %s", wdFile, err)
	}
}

func TestJobCbbb1363993215f9_PulledDoneStoryNeverShadowsNewlyAllocatedSessionStory(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("MOCK_AGY_SCENARIO", "job_cbbb1363993215f9_wave4_pulled_done_story_override")
	t.Setenv("MOCK_AGY_UNIT", "1")
	t.Setenv("IAZIO_TEST_SKIP_REMOTE", "1")

	repoDir := t.TempDir()
	_ = exec.Command("git", "-C", repoDir, "init").Run()
	_ = exec.Command("git", "-C", repoDir, "config", "user.email", "test@test.local").Run()
	_ = exec.Command("git", "-C", repoDir, "config", "user.name", "Tester").Run()

	mockAgyBin := filepath.Join(t.TempDir(), "mock-agy")
	buildCmd := exec.Command("go", "build", "-buildvcs=false", "-o", mockAgyBin, "../../cmd/mock-agy")
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("build mock-agy failed: %v (%s)", err, string(out))
	}

	var mu sync.Mutex
	var boundStoryIDs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/story-binding") {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if sid, ok := body["story_id"].(string); ok {
				mu.Lock()
				boundStoryIDs = append(boundStoryIDs, sid)
				mu.Unlock()
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	runner := NewPromptRunner("autopilot")
	res, err := runner.RunPrompt(context.Background(), RunPromptOptions{
		PromptID:     "prompt_job_cbbb1363993215f9",
		JobID:        "job_cbbb1363993215f9",
		APIURL:       srv.URL,
		Token:        "tok-cbbb1363",
		FencingToken: 1,
		Repo:         "meta-repo",
		StoryID:      "", // story.create_and_execute has empty initial StoryID
		Prompt:       "/autonomous-story-execution Wave 4 canonical AI hygiene",
		TargetAgent:  mockAgyBin,
		WorkingDir:   repoDir,
		Timeout:      10 * time.Second,
	})
	if err != nil {
		t.Fatalf("expected RunPrompt to succeed, got err=%v", err)
	}
	if res.StoryID != "STORY-GOV-5154" {
		t.Fatalf("expected res.StoryID='STORY-GOV-5154' (NOT pulled DONE STORY-GOV-5144), got %q", res.StoryID)
	}
	if res.StoryTitle != "Wave 4 Canonical AI Hygiene and Skills" {
		t.Fatalf("expected res.StoryTitle='Wave 4 Canonical AI Hygiene and Skills', got %q", res.StoryTitle)
	}
	mu.Lock()
	bindings := append([]string(nil), boundStoryIDs...)
	mu.Unlock()
	for _, b := range bindings {
		if b == "STORY-GOV-5144" {
			t.Fatalf("expected live polling to ignore pulled DONE story STORY-GOV-5144, got bindings=%v", bindings)
		}
	}
	if len(bindings) == 0 || bindings[len(bindings)-1] != "STORY-GOV-5154" {
		t.Fatalf("expected final story binding to be STORY-GOV-5154, got %v", bindings)
	}
}

func TestJob38eeeb536a5a7130_AgyCrashAutoResumeAndNewConversationFallback(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("MOCK_AGY_SCENARIO", "job_38eeeb536a5a7130_crash_resume_and_new_conv_fallback")
	t.Setenv("MOCK_AGY_STATE_MARKER", filepath.Join(homeDir, "mock_agy_38eeeb53_turn.tmp"))
	t.Setenv("IAZIO_TEST_SKIP_REMOTE", "1")

	repoDir := t.TempDir()
	_ = exec.Command("git", "-C", repoDir, "init").Run()
	_ = exec.Command("git", "-C", repoDir, "config", "user.email", "test@test.local").Run()
	_ = exec.Command("git", "-C", repoDir, "config", "user.name", "Tester").Run()

	mockAgyBin := filepath.Join(t.TempDir(), "mock-agy")
	buildCmd := exec.Command("go", "build", "-buildvcs=false", "-o", mockAgyBin, "../../cmd/mock-agy")
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("build mock-agy failed: %v (%s)", err, string(out))
	}

	runner := NewPromptRunner("autopilot")
	res, err := runner.RunPrompt(context.Background(), RunPromptOptions{
		PromptID:     "prompt_job_38eeeb536a5a7130",
		JobID:        "job_38eeeb536a5a7130",
		Repo:         "meta-repo",
		StoryID:      "STORY-IAZIO-0185",
		Prompt:       "/autonomous-story-execution STORY-IAZIO-0185",
		TargetAgent:  mockAgyBin,
		WorkingDir:   repoDir,
		Timeout:      15 * time.Second,
	})
	if err != nil {
		t.Fatalf("expected RunPrompt to recover from agy crash and corrupted conversation via new conversation fallback, got err=%v", err)
	}
	if res.Status != "completed" || res.ExitCode != 0 {
		t.Fatalf("expected status=completed exit=0, got status=%s exit=%d", res.Status, res.ExitCode)
	}
	if res.StoryID != "STORY-IAZIO-0185" {
		t.Fatalf("expected StoryID=STORY-IAZIO-0185, got %q", res.StoryID)
	}
	if res.ActiveConversationID != "2bff5c4b-a111-4c71-a7b5-999988887777" {
		t.Fatalf("expected ActiveConversationID to switch to fallback conversation 2bff5c4b-a111-4c71-a7b5-999988887777, got %q", res.ActiveConversationID)
	}
	if !strings.Contains(res.Output, "auto_resume.new_conversation_fallback") {
		t.Fatalf("expected supervisor log in Output to record auto_resume.new_conversation_fallback, got:\n%s", res.Output)
	}
	if strings.Contains(res.Summary, "[ASE-SUPERVISOR]") || strings.Contains(res.Summary, "[agy step #") {
		t.Fatalf("expected Summary to strip [ASE-SUPERVISOR] and [agy step #...] lines, got:\n%s", res.Summary)
	}
}

func TestStory0190_RealTimeConversationDiscoveryAndHeartbeat(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("MOCK_AGY_FAST", "1")
	t.Setenv("IAZIO_TEST_SKIP_REMOTE", "1")

	repoDir := t.TempDir()
	_ = exec.Command("git", "-C", repoDir, "init").Run()
	_ = exec.Command("git", "-C", repoDir, "config", "user.email", "test@test.local").Run()
	_ = exec.Command("git", "-C", repoDir, "config", "user.name", "Tester").Run()

	convID := "aff9bb70-6c84-4121-ba1e-68745328700a"
	mockAgyBin := filepath.Join(t.TempDir(), "mock-agy-rt")
	script := fmt.Sprintf(`#!/usr/bin/env bash
set -e
mkdir -p .iazio
printf '{"conversation_id":"%s","story_id":"STORY-IAZIO-0190"}\n' > .iazio/repo.lock
echo "CONVERSATION_ID: %s"
echo "ALLOCATED_STORY_ID: STORY-IAZIO-0190"
sleep 0.2
rm -rf .iazio
mkdir -p docs/stories
printf -- "---\nid: STORY-IAZIO-0190\nstatus: DONE\n---\n# Story\n" > docs/stories/2026-11-06-iazio-0190.story.md
git add docs/stories/2026-11-06-iazio-0190.story.md >/dev/null 2>&1 || true
git commit -m "feat(STORY-IAZIO-0190): complete story" >/dev/null 2>&1 || true
echo "Autonomous story execution is complete for STORY-IAZIO-0190"
exit 0
`, convID, convID)
	if err := os.WriteFile(mockAgyBin, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock-agy-rt: %v", err)
	}

	var mu sync.Mutex
	var heartbeatConvs []string
	var postedConvs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		if strings.HasSuffix(r.URL.Path, "/heartbeat") {
			if c, ok := body["active_conversation_id"].(string); ok && c != "" {
				heartbeatConvs = append(heartbeatConvs, c)
			}
		} else if c, ok := body["conversation_id"].(string); ok && c != "" {
			postedConvs = append(postedConvs, c)
		}
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	runner := NewPromptRunner("autopilot")
	res, err := runner.RunPrompt(context.Background(), RunPromptOptions{
		PromptID:    "prompt_story_0190",
		JobID:       "job_story_0190",
		APIURL:      srv.URL,
		Token:       "tok-0190",
		Repo:        "iazio-mcp",
		Prompt:      "/autonomous-story-execution implement STORY-IAZIO-0190",
		TargetAgent: mockAgyBin,
		WorkingDir:  repoDir,
		Timeout:     5 * time.Second,
	})
	if err != nil {
		t.Fatalf("RunPrompt failed: %v", err)
	}
	if res.ConversationID != convID || res.ActiveConversationID != convID {
		t.Fatalf("expected ConversationID=%s, got conv=%q active=%q", convID, res.ConversationID, res.ActiveConversationID)
	}
	mu.Lock()
	hbList := append([]string(nil), heartbeatConvs...)
	postList := append([]string(nil), postedConvs...)
	mu.Unlock()
	if len(hbList) == 0 || hbList[0] != convID {
		t.Fatalf("expected real-time heartbeat with active_conversation_id=%s while job is running, got %v", convID, hbList)
	}
	if len(postList) == 0 || postList[0] != convID {
		t.Fatalf("expected real-time PostConversation with conversation_id=%s, got %v", convID, postList)
	}

	// Verify discoverRepoLockConversation helper directly
	lockDir := filepath.Join(repoDir, ".iazio")
	_ = os.MkdirAll(lockDir, 0o755)
	_ = os.WriteFile(filepath.Join(lockDir, "repo.lock"), []byte(fmt.Sprintf(`{"conversation_id":%q}`, convID)), 0o644)
	if got := discoverRepoLockConversation(repoDir, time.Now().Add(-5*time.Second)); got != convID {
		t.Fatalf("expected discoverRepoLockConversation=%s, got %q", convID, got)
	}
}

func TestJobBbd74e1ad21fbc40_TerminalTranscriptCompletionAndAmbientEnvIsolation(t *testing.T) {
	brainDir := t.TempDir()
	t.Setenv("IAZIO_BRAIN_DIR", brainDir)

	// 1. Verify isTranscriptAtTerminalPlannerResponse
	convID := "0092d1cb-cc35-41ce-9143-a480dca5e283"
	tpDir := filepath.Join(brainDir, convID, ".system_generated", "logs")
	if err := os.MkdirAll(tpDir, 0o755); err != nil {
		t.Fatalf("mkdir transcript dir: %v", err)
	}
	tpFile := filepath.Join(tpDir, "transcript.jsonl")
	_ = os.WriteFile(tpFile, []byte(`{"step_index":368,"type":"PLANNER_RESPONSE","status":"DONE","tool_calls":[{"name":"run_command"}]}`+"\n"), 0o600)
	if isTranscriptAtTerminalPlannerResponse(convID) {
		t.Fatalf("expected false when last PLANNER_RESPONSE has tool_calls")
	}
	_ = os.WriteFile(tpFile, []byte(
		`{"step_index":368,"type":"PLANNER_RESPONSE","status":"DONE","tool_calls":[{"name":"run_command"}]}`+"\n"+
			`{"step_index":477,"type":"PLANNER_RESPONSE","status":"DONE","content":"## CRAFT Closeout\nAll 10 findings remediated for STORY-GOV-5176.","tool_calls":[]}`+"\n",
	), 0o600)
	if !isTranscriptAtTerminalPlannerResponse(convID) {
		t.Fatalf("expected true when last step is terminal PLANNER_RESPONSE with status=DONE and empty tool_calls")
	}

	// 2. Verify isASEFlowCompletedInWorktree ignores unrelated historical IN_PROGRESS stories when primaryStoryID is found and DONE
	repoDir := initTestGitRepo(t)
	storiesDir := filepath.Join(repoDir, "docs", "stories")
	if err := os.MkdirAll(storiesDir, 0o755); err != nil {
		t.Fatalf("mkdir stories: %v", err)
	}
	primaryPath := filepath.Join(storiesDir, "2026-11-07-gov-5176-canonical-ai-autopilot-iazio-deep-review-remediation.story.md")
	unrelatedOldPath := filepath.Join(storiesDir, "2026-10-01-gov-1001-old-in-progress.story.md")
	_ = os.WriteFile(primaryPath, []byte("---\nid: STORY-GOV-5176\ntitle: Remediate All 10 Findings\nstatus: DONE\n---\n"), 0o600)
	_ = os.WriteFile(unrelatedOldPath, []byte("---\nid: STORY-GOV-1001\ntitle: Historical Unrelated Story\nstatus: IN_PROGRESS\n---\n"), 0o600)
	for _, args := range [][]string{
		{"add", "docs"},
		{"commit", "-m", "feat(STORY-GOV-5176): complete story"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, string(out))
		}
	}

	combinedWithUnrelatedMention := "Completed STORY-GOV-5176 (see historical note STORY-GOV-1001).\n## CRAFT Closeout"
	if !isASEFlowCompletedInWorktree(repoDir, "STORY-GOV-5176", combinedWithUnrelatedMention, "git") {
		t.Fatalf("expected isASEFlowCompletedInWorktree=true for DONE primaryStoryID STORY-GOV-5176 even when combinedOutput mentions unrelated STORY-GOV-1001")
	}

	// 3. Verify ambient IAZIO_JOB_ID / IAZIO_HARNESS_API_URL are ignored inside test binaries when opts.JobID == ""
	var ambientCalls int
	var mu sync.Mutex
	ambientSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ambientCalls++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer ambientSrv.Close()

	t.Setenv("IAZIO_JOB_ID", "job_bbd74e1ad21fbc40")
	t.Setenv("IAZIO_HARNESS_API_URL", ambientSrv.URL)

	binDir := t.TempDir()
	mockEcho := filepath.Join(binDir, "mock-echo")
	_ = os.WriteFile(mockEcho, []byte("#!/bin/sh\necho 'done STORY-EXPLICIT-111'\nexit 0\n"), 0o755)

	runner := NewPromptRunner(mockEcho)
	if _, err := runner.RunPrompt(context.Background(), RunPromptOptions{
		PromptID:   "unit_test_isolation",
		StoryID:    "STORY-EXPLICIT-111",
		Prompt:     "Execute story STORY-EXPLICIT-111",
		WorkingDir: repoDir,
	}); err != nil {
		t.Fatalf("RunPrompt isolated test failed: %v", err)
	}
	mu.Lock()
	gotAmbientCalls := ambientCalls
	mu.Unlock()
	if gotAmbientCalls != 0 {
		t.Fatalf("expected 0 control-plane calls to ambient IAZIO_HARNESS_API_URL when opts.JobID/APIURL are empty in test binary, got %d", gotAmbientCalls)
	}

	// 4. Verify end-to-end RunPrompt settles when transcript reaches terminal PLANNER_RESPONSE and primary story is DONE even if agy --print hangs without WaitForConversationFullyIdle log
	mockAgyHang := filepath.Join(binDir, "mock-agy-hang")
	hangScript := fmt.Sprintf(`#!/bin/sh
echo "CONVERSATION_ID: %s"
echo "ALLOCATED_STORY_ID: STORY-GOV-5176"
sleep 5
exit 0
`, convID)
	if err := os.WriteFile(mockAgyHang, []byte(hangScript), 0o755); err != nil {
		t.Fatalf("write mock-agy-hang: %v", err)
	}

	runner2 := NewPromptRunner("autopilot")
	runner2.GracePeriod = 50 * time.Millisecond
	res, err := runner2.RunPrompt(context.Background(), RunPromptOptions{
		PromptID:                   "prompt_job_bbd74e1ad21fbc40",
		JobID:                      "job_bbd74e1ad21fbc40",
		APIURL:                     ambientSrv.URL,
		Token:                      "tok",
		StoryID:                    "STORY-GOV-5176",
		Prompt:                     "/autonomous-story-execution STORY-GOV-5176",
		TargetAgent:                mockAgyHang,
		WorkingDir:                 repoDir,
		Timeout:                    2 * time.Second,
		BackgroundDrainGracePeriod: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("expected hung agy after terminal PLANNER_RESPONSE + DONE story to settle cleanly, got err=%v", err)
	}
	if res.Status != "completed" || res.ExitCode != 0 {
		t.Fatalf("expected completed ExitCode=0, got Status=%q ExitCode=%d", res.Status, res.ExitCode)
	}
}

func TestTurnLoop_SupervisorHeartbeatDoesNotResetSilentMs(t *testing.T) {
	origInterval := supervisorHeartbeatInterval
	supervisorHeartbeatInterval = 150 * time.Millisecond
	t.Cleanup(func() { supervisorHeartbeatInterval = origInterval })

	var (
		mu              sync.Mutex
		heartbeatChunks int
		tickSilentMs    []int64
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/chunks") {
			var body struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				SilentForMs int64  `json:"silent_for_ms"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			if body.Type == "OUTPUT_CHUNK" && strings.Contains(body.Text, SupervisorHeartbeatPrefix) {
				heartbeatChunks++
			}
			if body.Type == "OUTPUT_TICK" {
				tickSilentMs = append(tickSilentMs, body.SilentForMs)
			}
			mu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	binDir := t.TempDir()
	mockSilent := filepath.Join(binDir, "mock-agy-silent")
	script := "#!/bin/sh\necho 'initial output'\nsleep 5\nexit 0\n"
	if err := os.WriteFile(mockSilent, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock-silent: %v", err)
	}

	runner := NewPromptRunner("autopilot")
	runner.GracePeriod = 50 * time.Millisecond
	_, _ = runner.RunPrompt(context.Background(), RunPromptOptions{
		PromptID:    "test_silent_ms",
		JobID:       "job_silent_ms",
		APIURL:      srv.URL,
		Token:       "tok",
		Prompt:      "non-ase prompt",
		TargetAgent: mockSilent,
		WorkingDir:  t.TempDir(),
		Timeout:     4500 * time.Millisecond,
	})

	mu.Lock()
	defer mu.Unlock()
	if heartbeatChunks < 2 {
		t.Fatalf("expected >= 2 supervisor heartbeat chunks, got %d", heartbeatChunks)
	}
	if len(tickSilentMs) < 2 {
		t.Fatalf("expected >= 2 OUTPUT_TICK payloads, got %v", tickSilentMs)
	}
	if tickSilentMs[len(tickSilentMs)-1] <= tickSilentMs[0] || tickSilentMs[len(tickSilentMs)-1] < 2000 {
		t.Fatalf("expected silent_ms to increase monotonically across supervisor heartbeats without resetting to 0, got %v", tickSilentMs)
	}
}

func TestTurnLoop_TerminalPlannerResponseWithInProgressStoryDoesNotPrematurelySettle(t *testing.T) {
	brainDir := t.TempDir()
	t.Setenv("IAZIO_BRAIN_DIR", brainDir)

	convID := "1192d1cb-cc35-41ce-9143-a480dca5e283"
	logDir := filepath.Join(brainDir, convID, ".system_generated", "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatalf("mkdir transcript dir: %v", err)
	}
	transcriptPath := filepath.Join(logDir, "transcript.jsonl")
	content := `{"step_index":1,"type":"USER_INPUT","status":"DONE","content":"Execute story STORY-GOV-5199"}` + "\n" +
		`{"step_index":2,"type":"PLANNER_RESPONSE","status":"DONE","content":"Waiting for subagent...","tool_calls":[]}` + "\n"
	if err := os.WriteFile(transcriptPath, []byte(content), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	if !isTranscriptAtTerminalPlannerResponse(convID) {
		t.Fatalf("expected isTranscriptAtTerminalPlannerResponse=true")
	}

	repoDir := t.TempDir()
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test User"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, string(out))
		}
	}
	storiesDir := filepath.Join(repoDir, "docs", "stories")
	_ = os.MkdirAll(storiesDir, 0o755)
	_ = os.WriteFile(filepath.Join(storiesDir, "2026-11-07-gov-5199-in-progress.story.md"), []byte("---\nid: STORY-GOV-5199\ntitle: In Progress Story\nstatus: IN_PROGRESS\n---\n"), 0o600)
	for _, args := range [][]string{
		{"add", "docs"},
		{"commit", "-m", "chore: add IN_PROGRESS story"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, string(out))
		}
	}

	if isASEFlowCompletedInWorktree(repoDir, "STORY-GOV-5199", "Waiting for subagent...", "git") {
		t.Fatalf("expected isASEFlowCompletedInWorktree=false while STORY-GOV-5199 is IN_PROGRESS")
	}

	// Drive RunPrompt end-to-end (R-04): terminal transcript + IN_PROGRESS story must NOT trigger
	// premature turnBgDrainReaped or ase_completion.detected_during_bg_drain while child is alive.
	mockInProgress := filepath.Join(t.TempDir(), "mock-agy-inprogress")
	script := "#!/bin/sh\n" +
		"echo \"Conversation ID: " + convID + "\"\n" +
		"echo \"bound story STORY-GOV-5199\"\n" +
		"sleep 3\n" +
		"exit 0\n"
	if err := os.WriteFile(mockInProgress, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock script: %v", err)
	}
	res, err := NewPromptRunner("autopilot").RunPrompt(context.Background(), RunPromptOptions{
		PromptID:                   "test_in_progress_no_reap",
		StoryID:                    "STORY-GOV-5199",
		Prompt:                     "non-ase prompt STORY-GOV-5199",
		TargetAgent:                mockInProgress,
		WorkingDir:                 repoDir,
		Timeout:                    6 * time.Second,
		BackgroundDrainGracePeriod: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("unexpected error running mock-inprogress-agy: %v", err)
	}
	if strings.Contains(res.Summary, "ase_completion.detected_during_bg_drain") || strings.Contains(res.Summary, "background_task.auto_reaped") {
		t.Fatalf("expected no premature drain completion or auto-reap while STORY-GOV-5199 is IN_PROGRESS, got summary:\n%s", res.Summary)
	}
	if res.DurationMs < 2500 {
		t.Fatalf("expected mock process to run for full 3s without premature reap (BackgroundDrainGracePeriod=100ms), got duration=%dms", res.DurationMs)
	}
}

func TestHasActiveSDLCChildProcess_IgnoresMCPServerDescendants(t *testing.T) {
	origFind := findChildProcessesFunc
	t.Cleanup(func() { findChildProcessesFunc = origFind })

	// 1. MCP-server descendants of agy must be ignored by hasActiveSDLCChildProcess and hasActiveConversationActivity
	findChildProcessesFunc = func(rootPID int) []engines.BackgroundTaskInfo {
		return []engines.BackgroundTaskInfo{
			{PID: 101, Command: "/Users/romeo/.local/bin/autopilot-mcp serve"},
			{PID: 102, Command: "npm exec chrome-devtools-mcp@latest"},
			{PID: 103, Command: "dart mcp-server"},
		}
	}
	if active, cmd := hasActiveSDLCChildProcess(100); active {
		t.Fatalf("expected MCP server descendants to be ignored, got active=true cmd=%q", cmd)
	}
	_, activeChildren := hasActiveConversationActivity("", 100, time.Time{}, 30*time.Second)
	if activeChildren != 0 {
		t.Fatalf("expected MCP server descendants to be excluded from activeChildren count, got %d", activeChildren)
	}

	// 2. Real SDLC commands ('autopilot git-flow finish', 'go test ./...', 'make validate-local') must be detected
	for _, sdlcCmd := range []string{
		"autopilot git-flow finish",
		"go test ./...",
		"make validate-local",
	} {
		findChildProcessesFunc = func(rootPID int) []engines.BackgroundTaskInfo {
			return []engines.BackgroundTaskInfo{
				{PID: 101, Command: "/Users/romeo/.local/bin/autopilot-mcp serve"},
				{PID: 201, Command: sdlcCmd},
			}
		}
		active, matched := hasActiveSDLCChildProcess(100)
		if !active || matched != sdlcCmd {
			t.Fatalf("expected SDLC command %q to be detected, got active=%v matched=%q", sdlcCmd, active, matched)
		}
	}
}

func TestIsSDLCChildCommandLine_DetectsSDLCInMCPNamedPaths(t *testing.T) {
	origFind := findChildProcessesFunc
	t.Cleanup(func() { findChildProcessesFunc = origFind })

	// 1. Real SDLC commands in MCP-named repos/packages must be detected as SDLC and NOT classified as daemons
	sdlcInMCPPaths := []string{
		"go test -race ./pkg/mcp/...",
		"/tmp/go-build123/b001/mcp.test -test.v",
		"go build ./cmd/autopilot-mcp",
		"make -C iazio-mcp test",
		`git commit -m "fix(mcp): harden ticket lock"`,
	}
	for _, cmd := range sdlcInMCPPaths {
		if !isSDLCChildCommandLine(cmd) {
			t.Fatalf("expected %q to be detected as SDLC child command line", cmd)
		}
		if isMCPOrDaemonChildProcess(cmd) {
			t.Fatalf("expected %q NOT to be classified as MCP/daemon child process", cmd)
		}
		findChildProcessesFunc = func(rootPID int) []engines.BackgroundTaskInfo {
			return []engines.BackgroundTaskInfo{{PID: 301, Command: cmd}}
		}
		if active, matched := hasActiveSDLCChildProcess(100); !active || matched != cmd {
			t.Fatalf("expected hasActiveSDLCChildProcess to detect %q, got active=%v matched=%q", cmd, active, matched)
		}
		_, activeChildren := hasActiveConversationActivity("", 100, time.Time{}, 30*time.Second)
		if activeChildren != 1 {
			t.Fatalf("expected hasActiveConversationActivity to count %q in activeChildren, got %d", cmd, activeChildren)
		}
	}

	// 2. Genuine MCP/LSP daemons must NOT be detected as SDLC and MUST be classified as daemons
	mcpDaemons := []string{
		"/Users/romeo/.local/bin/autopilot-mcp serve",
		"npm exec chrome-devtools-mcp@latest",
		"dart mcp-server",
		"gopls serve",
	}
	for _, daemonCmd := range mcpDaemons {
		if isSDLCChildCommandLine(daemonCmd) {
			t.Fatalf("expected MCP/LSP daemon %q NOT to be detected as SDLC child command line", daemonCmd)
		}
		if !isMCPOrDaemonChildProcess(daemonCmd) {
			t.Fatalf("expected MCP/LSP daemon %q to be classified as MCP/daemon child process", daemonCmd)
		}
		findChildProcessesFunc = func(rootPID int) []engines.BackgroundTaskInfo {
			return []engines.BackgroundTaskInfo{{PID: 401, Command: daemonCmd}}
		}
		if active, matched := hasActiveSDLCChildProcess(100); active {
			t.Fatalf("expected hasActiveSDLCChildProcess to ignore %q, got active=true matched=%q", daemonCmd, matched)
		}
		_, activeChildren := hasActiveConversationActivity("", 100, time.Time{}, 30*time.Second)
		if activeChildren != 0 {
			t.Fatalf("expected hasActiveConversationActivity to exclude %q from activeChildren, got %d", daemonCmd, activeChildren)
		}
	}

	// 3. Verify IAZIO_PARENT_HARNESS_PID nested-harness ambient-env gate (R2-05)
	var parentHarnessHit int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parentHarnessHit++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	t.Setenv("IAZIO_TEST_ALLOW_AMBIENT_ENV", "1")
	t.Setenv("IAZIO_PARENT_HARNESS_PID", "999999")
	t.Setenv("IAZIO_JOB_ID", "job_nested_child_leak")
	t.Setenv("IAZIO_HARNESS_API_URL", srv.URL)

	fakeBin := makeFakeAutopilot(t)
	repoDir := initTestGitRepo(t)
	_, err := NewPromptRunner(fakeBin).RunPrompt(context.Background(), RunPromptOptions{
		PromptID:   "test_nested_parent_pid",
		StoryID:    "STORY-NESTED-999",
		Repo:       "test-submodule",
		Prompt:     "Execute task",
		WorkingDir: repoDir,
		Timeout:    5 * time.Second,
	})
	if err != nil {
		t.Fatalf("unexpected RunPrompt error under IAZIO_PARENT_HARNESS_PID: %v", err)
	}
	if parentHarnessHit != 0 {
		t.Fatalf("expected nested harness child (IAZIO_PARENT_HARNESS_PID=999999) to suppress ambient IAZIO_JOB_ID/IAZIO_HARNESS_API_URL, got %d hits", parentHarnessHit)
	}
}

func TestStoryGov5223_ReviewPathStoryIDExclusionAndCommittedFeatureBranchAutoResume(t *testing.T) {
	// 1. Verify extractTargetStoryIDFromText ignores review artifact filenames and non-canonical underscored tokens
	promptWithReviewPath := "/autonomous-story-execution clean up dummy FSM-bypass review artifacts (model: dummy in vigo/docs/reviews/2026-08-03-review-V2-STORY-AUTO-GOV_2251_HOOK_MINING_AUDIT_STORIES.md)"
	if got := extractTargetStoryIDFromText(promptWithReviewPath); got != "" {
		t.Fatalf("expected empty target story ID when prompt only references a review artifact path, got %q", got)
	}
	if got := extractTargetStoryIDFromText("/autonomous-story-execution implement STORY-VIGO-0289 and check docs/reviews/2026-08-03-review-STORY-OLD-1111.md"); got != "STORY-VIGO-0289" {
		t.Fatalf("expected STORY-VIGO-0289, got %q", got)
	}

	// 2. Verify RunPrompt auto-resumes when Turn 1 commits an IN_PROGRESS story to a feature branch and exits 0 (e.g. after invoke_subagent)
	repoDir := initTestGitRepo(t)
	if onNonMain, br := isNonMainWorktreeBranch(repoDir, "git"); onNonMain {
		t.Fatalf("expected initial repo branch %q to be main/master", br)
	}

	convID := "8e0e4af9-2263-4afb-9794-3863bf9b97b9"
	binDir := t.TempDir()
	stateMarker := filepath.Join(binDir, "turn_marker")
	mockAgyBin := filepath.Join(binDir, "mock-agy")
	script := fmt.Sprintf(`#!/bin/sh
CONV_ARG=""
while [ $# -gt 0 ]; do
  case "$1" in
    --conversation=*) CONV_ARG="${1#*=}" ;;
    --conversation) shift; CONV_ARG="$1" ;;
  esac
  shift
done

if [ ! -f "%s" ]; then
  echo "1" > "%s"
  echo "Created conversation %s"
  git checkout -b feat/vigo-0289-security-transport-hygiene-remediation >/dev/null 2>&1 || true
  mkdir -p docs/stories
  printf -- "---\nid: STORY-VIGO-0289\ntitle: Security Transport Hygiene\nstatus: IN_PROGRESS\n---\n# Story\n" > docs/stories/2026-11-09-vigo-0289.story.md
  git add docs/stories/2026-11-09-vigo-0289.story.md >/dev/null 2>&1 || true
  git commit -m "feat(vigo-0289): commit phase 3 and invoke review subagent" >/dev/null 2>&1 || true
  echo "Launched review subagent; ending turn."
  exit 0
else
  printf -- "---\nid: STORY-VIGO-0289\ntitle: Security Transport Hygiene\nstatus: DONE\n---\n# Story\n" > docs/stories/2026-11-09-vigo-0289.story.md
  git add docs/stories/2026-11-09-vigo-0289.story.md >/dev/null 2>&1 || true
  git commit -m "chore(vigo-0289): complete story" >/dev/null 2>&1 || true
  git checkout main >/dev/null 2>&1 || git checkout master >/dev/null 2>&1 || true
  git merge --ff-only feat/vigo-0289-security-transport-hygiene-remediation >/dev/null 2>&1 || true
  echo "Autonomous story execution is complete for STORY-VIGO-0289 on $CONV_ARG"
  exit 0
fi
`, stateMarker, stateMarker, convID)
	if err := os.WriteFile(mockAgyBin, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock-agy: %v", err)
	}

	runner := NewPromptRunner("autopilot")
	runner.GracePeriod = 50 * time.Millisecond
	res, err := runner.RunPrompt(context.Background(), RunPromptOptions{
		PromptID:    "prompt_gov_5223",
		JobID:       "job_ed03faa8f7094f29",
		Repo:        "vigo",
		Prompt:      promptWithReviewPath,
		TargetAgent: mockAgyBin,
		WorkingDir:  repoDir,
		Timeout:     5 * time.Second,
	})
	if err != nil {
		t.Fatalf("expected RunPrompt to auto-resume and complete STORY-VIGO-0289, got err=%v", err)
	}
	if res.StoryID != "STORY-VIGO-0289" {
		t.Fatalf("expected StoryID=STORY-VIGO-0289 (not review path artifact), got %q", res.StoryID)
	}
	if res.ResumeAttempts != 1 {
		t.Fatalf("expected 1 auto-resume attempt when Turn 1 exited on committed IN_PROGRESS feature branch, got %d", res.ResumeAttempts)
	}
	if res.Status != "completed" || res.ExitCode != 0 {
		t.Fatalf("expected completed ExitCode=0, got status=%s exit=%d", res.Status, res.ExitCode)
	}
}

func TestASECompletionGate_HardenedFailureModes_StoryGov5232(t *testing.T) {
	tmpDir := t.TempDir()
	repoDir := filepath.Join(tmpDir, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	execGit := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, string(out))
		}
	}
	execGit("init", "-b", "main")
	execGit("config", "user.email", "test@viovy.test")
	execGit("config", "user.name", "Test Runner")
	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("# Test Repo\n"), 0o644); err != nil {
		t.Fatalf("write readme: %v", err)
	}
	execGit("add", "README.md")
	execGit("commit", "-m", "initial commit")

	// 1. Verify resolveSuperprojectRoot returns parent dir when .git is a gitlink file and .gitmodules exists
	subDir := filepath.Join(repoDir, "submodule-a")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatalf("mkdir subDir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, ".gitmodules"), []byte("[submodule \"submodule-a\"]\n\tpath = submodule-a\n"), 0o644); err != nil {
		t.Fatalf("write gitmodules: %v", err)
	}
	_ = exec.Command("git", "-C", repoDir, "add", ".gitmodules").Run()
	_ = exec.Command("git", "-C", repoDir, "commit", "-m", "chore: add gitmodules").Run()
	// Mock gitlink in submodule-a
	if err := os.WriteFile(filepath.Join(subDir, ".git"), []byte("gitdir: ../.git/modules/submodule-a\n"), 0o644); err != nil {
		t.Fatalf("write submodule gitlink: %v", err)
	}
	if sp := resolveSuperprojectRoot(subDir, "git"); sp != repoDir {
		t.Fatalf("expected resolveSuperprojectRoot(%q)=%q, got %q", subDir, repoDir, sp)
	}
	// Verify standalone clone under tmp/ (with .git as a directory) does NOT resolve superproject
	standaloneDir := filepath.Join(repoDir, "tmp", "standalone")
	if err := os.MkdirAll(filepath.Join(standaloneDir, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir standalone .git: %v", err)
	}
	if sp := resolveSuperprojectRoot(standaloneDir, "git"); sp != "" {
		t.Fatalf("expected resolveSuperprojectRoot for standalone clone to be empty, got %q", sp)
	}

	// 2. Scenario A: Direct commit on main with NO story binding during /autonomous-story-execution
	// Must fail closed with ASE_GATE_INCOMPLETE: missing_story_binding
	mockAgyBin := filepath.Join(tmpDir, "mock-agy-unbound")
	scriptUnbound := `#!/bin/sh
echo "Created conversation conv-unbound-123"
echo "modified content" >> README.md
git add README.md >/dev/null 2>&1
git commit -m "feat: direct commit on main" >/dev/null 2>&1
echo "### Implementation Summary"
exit 0
`
	if err := os.WriteFile(mockAgyBin, []byte(scriptUnbound), 0o755); err != nil {
		t.Fatalf("write mock-agy: %v", err)
	}

	runner := NewPromptRunner("autopilot")
	runner.GracePeriod = 50 * time.Millisecond
	resA, errA := runner.RunPrompt(context.Background(), RunPromptOptions{
		PromptID:    "prompt_unbound_ase",
		JobID:       "job_unbound",
		Repo:        "repo",
		Prompt:      "/autonomous-story-execution Title: Do some work without story",
		TargetAgent: mockAgyBin,
		WorkingDir:  repoDir,
		Timeout:     5 * time.Second,
	})
	if errA == nil {
		t.Fatalf("expected error for unbounded story execution, got nil")
	}
	if resA.Status != "failed" || resA.FailureCategory != "ASE_GATE_INCOMPLETE" {
		t.Fatalf("expected status=failed failureCategory=ASE_GATE_INCOMPLETE, got status=%s cat=%s", resA.Status, resA.FailureCategory)
	}
	if !strings.Contains(errA.Error(), "missing_story_binding") {
		t.Fatalf("expected missing_story_binding error, got: %v", errA)
	}

	// 3. Scenario B: Story bound but left IN_PROGRESS and exited
	// Must fail closed with ASE_GATE_INCOMPLETE: incomplete_ase_flow
	mockAgyInProgress := filepath.Join(tmpDir, "mock-agy-inprogress")
	scriptInProgress := `#!/bin/sh
echo "Created conversation conv-inprogress-123"
mkdir -p docs/stories
printf -- "---\nid: STORY-TEST-9999\ntitle: Test Incomplete\nstatus: IN_PROGRESS\n---\n# Story\n" > docs/stories/2026-11-09-test.story.md
git add docs/stories/2026-11-09-test.story.md >/dev/null 2>&1
git commit -m "feat(test-9999): work in progress" >/dev/null 2>&1
echo "Autonomous story execution is complete for STORY-TEST-9999"
exit 0
`
	if err := os.WriteFile(mockAgyInProgress, []byte(scriptInProgress), 0o755); err != nil {
		t.Fatalf("write mock-agy: %v", err)
	}

	resB, errB := runner.RunPrompt(context.Background(), RunPromptOptions{
		PromptID:    "prompt_inprogress_ase",
		JobID:       "job_inprogress",
		Repo:        "repo",
		Prompt:      "/autonomous-story-execution Title: Test Incomplete Story",
		TargetAgent: mockAgyInProgress,
		WorkingDir:  repoDir,
		Timeout:     5 * time.Second,
	})
	if errB == nil {
		t.Fatalf("expected error for IN_PROGRESS story completion attempt, got nil")
	}
	if resB.Status != "failed" || resB.FailureCategory != "ASE_GATE_INCOMPLETE" {
		t.Fatalf("expected status=failed failureCategory=ASE_GATE_INCOMPLETE, got status=%s cat=%s", resB.Status, resB.FailureCategory)
	}
	if !strings.Contains(errB.Error(), "incomplete_ase_flow") {
		t.Fatalf("expected incomplete_ase_flow error, got: %v", errB)
	}

	// 4. Scenario C: Story DONE on main but NO remote upstream exists
	// Must fail closed with ASE_GATE_INCOMPLETE: unpushed_probe_failed
	t.Setenv("IAZIO_TEST_SKIP_REMOTE", "")
	_ = exec.Command("git", "-C", repoDir, "rm", "-f", "docs/stories/2026-11-09-test.story.md").Run()
	_ = exec.Command("git", "-C", repoDir, "commit", "-m", "chore: remove previous story").Run()
	mockAgyNoRemote := filepath.Join(tmpDir, "mock-agy-noremote")
	scriptNoRemote := `#!/bin/sh
echo "Created conversation conv-noremote-123"
mkdir -p docs/stories
printf -- "---\nid: STORY-TEST-8888\ntitle: Test No Remote\nstatus: DONE\n---\n# Story\n" > docs/stories/2026-11-09-noremote.story.md
git add docs/stories/2026-11-09-noremote.story.md >/dev/null 2>&1
git commit -m "feat(test-8888): complete story without remote" >/dev/null 2>&1
echo "Autonomous story execution is complete for STORY-TEST-8888"
exit 0
`
	if err := os.WriteFile(mockAgyNoRemote, []byte(scriptNoRemote), 0o755); err != nil {
		t.Fatalf("write mock-agy: %v", err)
	}
	resC, errC := runner.RunPrompt(context.Background(), RunPromptOptions{
		PromptID:    "prompt_noremote_ase",
		JobID:       "job_noremote",
		Repo:        "repo",
		Prompt:      "/autonomous-story-execution Title: Test No Remote",
		TargetAgent: mockAgyNoRemote,
		WorkingDir:  repoDir,
		Timeout:     5 * time.Second,
	})
	if errC == nil {
		t.Fatalf("expected error for no-remote story execution, got nil")
	}
	if resC.Status != "failed" || resC.FailureCategory != "ASE_GATE_INCOMPLETE" {
		t.Fatalf("expected status=failed failureCategory=ASE_GATE_INCOMPLETE, got status=%s cat=%s", resC.Status, resC.FailureCategory)
	}
	if !strings.Contains(errC.Error(), "unpushed_probe_failed") {
		t.Fatalf("expected unpushed_probe_failed error, got: %v", errC)
	}

	// 5. Scenario D: Worktree left on unmerged feature branch
	// Must fail closed with ASE_GATE_INCOMPLETE: unmerged_feature_branch
	_ = exec.Command("git", "-C", repoDir, "rm", "-f", "docs/stories/2026-11-09-noremote.story.md").Run()
	_ = exec.Command("git", "-C", repoDir, "commit", "-m", "chore: remove previous story").Run()
	_ = exec.Command("git", "-C", repoDir, "checkout", "-b", "feat/STORY-TEST-7777-unmerged").Run()
	mockAgyUnmerged := filepath.Join(tmpDir, "mock-agy-unmerged")
	scriptUnmerged := `#!/bin/sh
echo "Created conversation conv-unmerged-123"
mkdir -p docs/stories
printf -- "---\nid: STORY-TEST-7777\ntitle: Test Unmerged Branch\nstatus: DONE\n---\n# Story\n" > docs/stories/2026-11-09-unmerged.story.md
git add docs/stories/2026-11-09-unmerged.story.md >/dev/null 2>&1
git commit -m "feat(test-7777): done on feature branch" >/dev/null 2>&1
echo "Autonomous story execution is complete for STORY-TEST-7777"
exit 0
`
	if err := os.WriteFile(mockAgyUnmerged, []byte(scriptUnmerged), 0o755); err != nil {
		t.Fatalf("write mock-agy: %v", err)
	}
	resD, errD := runner.RunPrompt(context.Background(), RunPromptOptions{
		PromptID:    "prompt_unmerged_branch_ase",
		JobID:       "job_unmerged",
		Repo:        "repo",
		Prompt:      "/autonomous-story-execution Title: Test Unmerged Branch",
		TargetAgent: mockAgyUnmerged,
		WorkingDir:  repoDir,
		Timeout:     5 * time.Second,
	})
	if errD == nil {
		t.Fatalf("expected error for unmerged branch story execution, got nil")
	}
	if resD.Status != "failed" || resD.FailureCategory != "ASE_GATE_INCOMPLETE" {
		t.Fatalf("expected status=failed failureCategory=ASE_GATE_INCOMPLETE, got status=%s cat=%s", resD.Status, resD.FailureCategory)
	}
	if !strings.Contains(errD.Error(), "unmerged_feature_branch") {
		t.Fatalf("expected unmerged_feature_branch error, got: %v", errD)
	}
	_ = exec.Command("git", "-C", repoDir, "checkout", "main").Run()

	// 6. Scenario E: Bare remote configured, local commits unpushed
	// Must fail closed with ASE_GATE_INCOMPLETE: unpushed_commits
	bareRemoteDir := filepath.Join(tmpDir, "bare-origin.git")
	_ = exec.Command("git", "init", "--bare", bareRemoteDir).Run()
	_ = exec.Command("git", "-C", repoDir, "remote", "add", "origin", bareRemoteDir).Run()
	_ = exec.Command("git", "-C", repoDir, "push", "-u", "origin", "main").Run()

	mockAgyUnpushed := filepath.Join(tmpDir, "mock-agy-unpushed")
	scriptUnpushed := `#!/bin/sh
echo "Created conversation conv-unpushed-123"
mkdir -p docs/stories
printf -- "---\nid: STORY-TEST-6666\ntitle: Test Unpushed Commits\nstatus: DONE\n---\n# Story\n" > docs/stories/2026-11-09-unpushed.story.md
git add docs/stories/2026-11-09-unpushed.story.md >/dev/null 2>&1
git commit -m "feat(test-6666): completed but not pushed" >/dev/null 2>&1
echo "Autonomous story execution is complete for STORY-TEST-6666"
exit 0
`
	if err := os.WriteFile(mockAgyUnpushed, []byte(scriptUnpushed), 0o755); err != nil {
		t.Fatalf("write mock-agy: %v", err)
	}
	resE, errE := runner.RunPrompt(context.Background(), RunPromptOptions{
		PromptID:    "prompt_unpushed_ase",
		JobID:       "job_unpushed",
		Repo:        "repo",
		Prompt:      "/autonomous-story-execution Title: Test Unpushed Commits",
		TargetAgent: mockAgyUnpushed,
		WorkingDir:  repoDir,
		Timeout:     5 * time.Second,
	})
	if errE == nil {
		t.Fatalf("expected error for unpushed commits story execution, got nil")
	}
	if resE.Status != "failed" || resE.FailureCategory != "ASE_GATE_INCOMPLETE" {
		t.Fatalf("expected status=failed failureCategory=ASE_GATE_INCOMPLETE, got status=%s cat=%s", resE.Status, resE.FailureCategory)
	}
	if !strings.Contains(errE.Error(), "unpushed_commits") {
		t.Fatalf("expected unpushed_commits error, got: %v", errE)
	}

	// 7. Scenario F: Submodule --add-dir argument injection verification (AC2)
	argsOutFile := filepath.Join(tmpDir, "args.out")
	mockAgyArgsCheck := filepath.Join(tmpDir, "mock-agy-args")
	scriptArgsCheck := fmt.Sprintf(`#!/bin/sh
echo "$@" > %s
exit 0
`, argsOutFile)
	if err := os.WriteFile(mockAgyArgsCheck, []byte(scriptArgsCheck), 0o755); err != nil {
		t.Fatalf("write mock-agy: %v", err)
	}
	_, errF := runner.RunPrompt(context.Background(), RunPromptOptions{
		PromptID:    "prompt_submodule_args",
		JobID:       "job_submodule_args",
		Repo:        "submodule-a",
		Prompt:      "simple test prompt",
		TargetAgent: mockAgyArgsCheck,
		WorkingDir:  subDir,
		Timeout:     5 * time.Second,
	})
	if errF != nil {
		t.Fatalf("RunPrompt failed: %v", errF)
	}
	argsData, _ := os.ReadFile(argsOutFile)
	if !strings.Contains(string(argsData), "--add-dir "+repoDir) {
		t.Fatalf("expected mock agent to receive '--add-dir %s' in args, got output: %s", repoDir, string(argsData))
	}
}

func TestJob7a422318d6294d43_Regressions(t *testing.T) {
	// 1. RC-1: Multi-line prompt with Title: without story ID must not scrape incidental STORY-* from Context & Problem Statement
	promptWithContextMention := `/autonomous-story-execution
Title: Wave 3 — ViGo Forensic Product Audit Remediation: CLI SQLite Parity

Submodule: vigo-cli
Backlog: vigo

Context & Problem Statement:
AC3 of STORY-VIGO-0288 claimed to replace the mock fallback in status.go.
`
	if got := extractTargetStoryIDFromText(promptWithContextMention); got != "" {
		t.Fatalf("expected empty story ID for prompt with Title header and incidental context mention, got %q", got)
	}
	if got := extractTargetStoryIDFromText("/autonomous-story-execution\nTitle: Implement STORY-VIGO-0310\nContext: refs STORY-VIGO-0288"); got != "STORY-VIGO-0310" {
		t.Fatalf("expected STORY-VIGO-0310 from Title header, got %q", got)
	}
	if got := extractTargetStoryIDFromText("/autonomous-story-execution\nTitle: Wave 3\nStory ID: STORY-VIGO-0310\nContext: refs STORY-VIGO-0288"); got != "STORY-VIGO-0310" {
		t.Fatalf("expected STORY-VIGO-0310 from Story ID header, got %q", got)
	}

	// 2. RC-4: hasHeadlessPermissionDenial must ignore [agy step #N] transcript lines
	transcriptLine := `[2026-11-11T04:55:21Z] [agy step #691] thinking: Wait: why did it auto-resume? Because !hasHeadlessPermissionDenial(rawCombined) checks "headless mode cannot prompt for" or "so it was auto-denied" or "jetski: no output produced"`
	if hasHeadlessPermissionDenial(transcriptLine) {
		t.Fatalf("expected hasHeadlessPermissionDenial to ignore [agy step #N] transcript lines")
	}
	realStderrLine := `ERROR: Headless mode cannot prompt for tool approval, so it was auto-denied.`
	if !hasHeadlessPermissionDenial(realStderrLine) {
		t.Fatalf("expected hasHeadlessPermissionDenial to detect real stderr headless denial")
	}

	// 3. RC-3: isStoryIncompleteAnywhere respects authoritative superproject DONE when submodule is on main
	tmpDir := t.TempDir()
	spRoot := filepath.Join(tmpDir, "meta-repo")
	subDir := filepath.Join(spRoot, "vigo-cli")
	_ = os.MkdirAll(filepath.Join(spRoot, "docs", "stories"), 0o755)
	_ = os.MkdirAll(filepath.Join(subDir, "docs", "stories"), 0o755)
	_ = exec.Command("git", "init", "-b", "main", spRoot).Run()
	_ = exec.Command("git", "init", "-b", "main", subDir).Run()
	_ = os.WriteFile(filepath.Join(spRoot, "docs", "stories", "2026-11-11-vigo-0310.story.md"), []byte("---\nid: STORY-VIGO-0310\nstatus: DONE\n---\n"), 0o644)
	_ = os.WriteFile(filepath.Join(subDir, "docs", "stories", "2026-11-11-vigo-0310.story.md"), []byte("---\nid: STORY-VIGO-0310\nstatus: IN_PROGRESS\n---\n"), 0o644)
	if isStoryIncompleteAnywhere(subDir, spRoot, "STORY-VIGO-0310") {
		t.Fatalf("expected isStoryIncompleteAnywhere to return false when superproject has STORY-VIGO-0310 DONE and submodule is on main")
	}
}

func TestStoryGov5253_ConcisePromptSummaryStripsTranscriptAndSupervisorLines(t *testing.T) {
	var transcriptBuilder strings.Builder
	for i := 1; i <= 1132; i++ {
		transcriptBuilder.WriteString(fmt.Sprintf("[2026-11-11T18:00:00Z] [agy step #%d] thinking: Executing step %d with verbose trace payload %s\n", i, i, strings.Repeat("x", 120)))
		transcriptBuilder.WriteString(fmt.Sprintf("[2026-11-11T18:00:01Z] [agy step #%d] tool run_command: Verification step %d\n", i, i))
	}
	finalOutcome := "Autonomous story execution is complete for STORY-GOV-5253 — all acceptance criteria verified and landed."
	transcriptBuilder.WriteString(fmt.Sprintf("[2026-11-11T18:30:00Z] [agy step #1133] output: %s\n", finalOutcome))

	rawOut := strings.Join([]string{
		"Created conversation 208632c0-2776-40a2-80e5-c905cecb280c",
		"[2026-11-11T18:10:00Z] [ASE-SUPERVISOR] Execution active (elapsed=600s, story=STORY-GOV-5253)",
		"[2026-11-11T18:20:00Z] [ASE-SUPERVISOR] [auto_resume.triggered] Resuming conversation",
		strings.Repeat("build progress line with verbose output ", 40),
	}, "\n")

	summary := BuildConcisePromptSummary(rawOut, transcriptBuilder.String(), "", nil)
	if len(summary) > MaxPromptSummaryChars {
		t.Fatalf("expected summary length <= %d, got %d", MaxPromptSummaryChars, len(summary))
	}
	if strings.Contains(summary, "[agy step #") {
		t.Fatalf("expected [agy step #...] lines to be stripped from summary, got:\n%s", summary)
	}
	if strings.Contains(summary, "[ASE-SUPERVISOR]") {
		t.Fatalf("expected [ASE-SUPERVISOR] lines to be stripped from summary, got:\n%s", summary)
	}
	if !strings.Contains(summary, finalOutcome) {
		t.Fatalf("expected final executive outcome %q to be preserved in summary, got:\n%s", finalOutcome, summary)
	}
}

func TestStoryGov5256_ClassifyPromptFailure_ASEGateNotOverriddenByTranscriptNoise(t *testing.T) {
	transcriptWith429 := strings.Join([]string{
		"[agy step #92] thinking: Earlier call returned 429 ResourceExhausted quota limit on helper subagent",
		"[agy step #950] tool run_command: autopilot-git-flow land --story STORY-GOV-5251",
	}, "\n")
	waitErr := fmt.Errorf("ASE_GATE_INCOMPLETE: unmerged_feature_branch (worktree remains on branch feat/gov-5251-k8s-clusters-jenkins-unification; git-flow land not completed)")

	cat, stage, dir, svc := ClassifyPromptFailure("failed", "ASE_GATE_INCOMPLETE", "COMPLETION_GATE", transcriptWith429, waitErr)
	if cat != "ASE_GATE_INCOMPLETE" || stage != "COMPLETION_GATE" || dir != "LOCAL_EXECUTION" || svc != "autopilot/ase-gate" {
		t.Fatalf("expected ASE_GATE_INCOMPLETE classification to take precedence over transcript 429 and branch name 'jenkins', got category=%q stage=%q direction=%q service=%q", cat, stage, dir, svc)
	}

	// Even if failureCategory is initially empty and only waitErr carries the unmerged_feature_branch gate error
	cat2, stage2, dir2, svc2 := ClassifyPromptFailure("failed", "", "", transcriptWith429, waitErr)
	if cat2 != "ASE_GATE_INCOMPLETE" || stage2 != "COMPLETION_GATE" || dir2 != "LOCAL_EXECUTION" || svc2 != "autopilot/ase-gate" {
		t.Fatalf("expected waitErr unmerged_feature_branch to classify as ASE_GATE_INCOMPLETE, got category=%q stage=%q direction=%q service=%q", cat2, stage2, dir2, svc2)
	}
}

func TestStoryGov5256_AutoResumeWhenStoryDoneOnUnmergedFeatureBranch(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping shell mock script test on windows")
	}
	repoDir := t.TempDir()
	binDir := t.TempDir()

	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test User"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v (%s)", args, err, string(out))
		}
	}

	storyDir := filepath.Join(repoDir, "docs", "stories")
	if err := os.MkdirAll(storyDir, 0o755); err != nil {
		t.Fatalf("mkdir stories: %v", err)
	}
	storyPath := filepath.Join(storyDir, "2026-11-11-gov-5256.story.md")
	if err := os.WriteFile(storyPath, []byte("---\nid: STORY-GOV-5256\nstatus: DONE\n---\n# Story\n"), 0o644); err != nil {
		t.Fatalf("write story: %v", err)
	}
	for _, args := range [][]string{
		{"add", "."},
		{"commit", "-m", "initial main commit"},
		{"checkout", "-b", "feat/gov-5256-unmerged"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v (%s)", args, err, string(out))
		}
	}
	if err := os.WriteFile(filepath.Join(repoDir, "feature.txt"), []byte("landed via auto-resume\n"), 0o644); err != nil {
		t.Fatalf("write feature.txt: %v", err)
	}
	for _, args := range [][]string{
		{"add", "feature.txt"},
		{"commit", "-m", "feat(STORY-GOV-5256): feature commit on branch"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v (%s)", args, err, string(out))
		}
	}

	convID := "ebd2122a-fe9f-4472-b880-b33e99199901"
	mockBin := filepath.Join(binDir, "mock-agy-resume-land")
	script := fmt.Sprintf(`#!/bin/sh
echo "Conversation ID: %s"
echo "[agy step #92] thinking: earlier step saw 429 ResourceExhausted on optional lookup"
case "$*" in
  *"--resume"*|*"land"*)
    git checkout main >/dev/null 2>&1
    git merge --no-ff feat/gov-5256-unmerged -m "Merge feat/gov-5256-unmerged" >/dev/null 2>&1
    echo "Autonomous story execution is complete for STORY-GOV-5256."
    exit 0
    ;;
  *)
    echo "Turn 1 exited while still on feat/gov-5256-unmerged before landing."
    exit 0
    ;;
esac
`, convID)
	if err := os.WriteFile(mockBin, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock script: %v", err)
	}

	t.Setenv("IAZIO_TEST_SKIP_REMOTE", "1")
	runner := NewPromptRunner(binDir)
	res, err := runner.RunPrompt(context.Background(), RunPromptOptions{
		PromptID:    "prompt_gov_5256",
		JobID:       "job_7d5cad6fd223f5a0",
		Repo:        "meta-repo",
		Prompt:      "/autonomous-story-execution STORY-GOV-5256",
		StoryID:     "STORY-GOV-5256",
		TargetAgent: mockBin,
		WorkingDir:  repoDir,
		Timeout:     15 * time.Second,
	})
	if err != nil {
		t.Fatalf("expected auto-resume to land feature branch and complete STORY-GOV-5256 without error, got err=%v (status=%s category=%s)", err, res.Status, res.FailureCategory)
	}
	if res.Status != "completed" {
		t.Fatalf("expected status=completed after auto-resume landed branch, got %s (category=%s)", res.Status, res.FailureCategory)
	}
	if res.ResumeAttempts < 1 {
		t.Fatalf("expected ResumeAttempts >= 1 when Turn 1 exited on unmerged feature branch with story status: DONE, got %d", res.ResumeAttempts)
	}
}

