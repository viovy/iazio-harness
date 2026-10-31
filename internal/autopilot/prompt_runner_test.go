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
