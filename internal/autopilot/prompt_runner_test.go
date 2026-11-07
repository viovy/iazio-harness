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
	if !strings.Contains(res.Summary, "auto_resume.new_conversation_fallback") {
		t.Fatalf("expected supervisor log to record auto_resume.new_conversation_fallback, got:\n%s", res.Summary)
	}
}

func TestStory0190_RealTimeConversationDiscoveryAndHeartbeat(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("MOCK_AGY_FAST", "1")

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

func TestStory0193_PostResponseIdleDrainSettlementAndTestBinaryIsolation(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("MOCK_AGY_FAST", "1")
	t.Setenv("IAZIO_BG_DRAIN_GRACE_SECONDS", "1")

	// 1. Verify hasBackgroundDrainLog skips trailing benign idle noise (fetchAvailableModels, loadCodeAssist, token refreshed)
	noiseLogPath := filepath.Join(t.TempDir(), "agy-drain-noise.log")
	noiseLog := strings.Join([]string{
		`I1106 23:17:07.123456 1527564 log.go:349] WaitForConversationFullyIdle: waiting for 1 background task(s) to finish`,
		`E1106 23:23:07.123456 1527564 http_helpers.go:108] request failed (status 503): service unavailable`,
		`W1106 23:23:07.123457 1527564 log.go:353] fetchAvailableModels failed: status 503`,
		`W1106 23:29:07.123457 1527564 log.go:353] loadCodeAssist failed: status 502`,
		`W1107 00:00:07.123457 1527564 browser.go:707] token refreshed`,
	}, "\n") + "\n"
	if err := os.WriteFile(noiseLogPath, []byte(noiseLog), 0o644); err != nil {
		t.Fatalf("write noiseLogPath: %v", err)
	}
	if inDrain, tasks := hasBackgroundDrainLog(noiseLogPath, "", ""); !inDrain || tasks != 1 {
		t.Fatalf("expected hasBackgroundDrainLog=true tasks=1 despite trailing idle noise, got inDrain=%v tasks=%d", inDrain, tasks)
	}

	// 2. Verify storyFrontmatterStatus fails closed as IN_PROGRESS on conflicting duplicates (one IN_PROGRESS, one DONE),
	// returns DONE when duplicate files are both DONE, and isASEFlowCompletedInWorktree ignores unrelated IN_PROGRESS stories in combinedOutput.
	repoDir := t.TempDir()
	_ = exec.Command("git", "-C", repoDir, "init").Run()
	_ = exec.Command("git", "-C", repoDir, "config", "user.email", "test@test.local").Run()
	_ = exec.Command("git", "-C", repoDir, "config", "user.name", "Tester").Run()

	storiesDir := filepath.Join(repoDir, "docs", "stories")
	if err := os.MkdirAll(storiesDir, 0o755); err != nil {
		t.Fatalf("mkdir stories: %v", err)
	}
	dupOldPath := filepath.Join(storiesDir, "2026-11-01-dup-old.story.md")
	dupDonePath := filepath.Join(storiesDir, "2026-11-02-dup-done.story.md")
	_ = os.WriteFile(dupOldPath, []byte("---\nid: STORY-DUP-001\nstatus: IN_PROGRESS\n---\n"), 0o644)
	_ = os.WriteFile(dupDonePath, []byte("---\nid: STORY-DUP-001\nstatus: DONE\n---\n"), 0o644)
	if found, st := storyFrontmatterStatus(repoDir, "STORY-DUP-001"); !found || st != "IN_PROGRESS" {
		t.Fatalf("expected conflicting duplicate story files to fail closed as IN_PROGRESS, got st=%q found=%v", st, found)
	}
	_ = os.WriteFile(dupOldPath, []byte("---\nid: STORY-DUP-001\nstatus: DONE\n---\n"), 0o644)
	if found, st := storyFrontmatterStatus(repoDir, "STORY-DUP-001"); !found || st != "DONE" {
		t.Fatalf("expected storyFrontmatterStatus(STORY-DUP-001)=DONE when duplicate files agree on DONE, got st=%q found=%v", st, found)
	}

	// Unrelated historical story that happens to be IN_PROGRESS in worktree, mentioned in combinedOutput
	_ = os.WriteFile(filepath.Join(storiesDir, "2026-11-01-other.story.md"), []byte("---\nid: STORY-OTHER-999\nstatus: IN_PROGRESS\n---\n"), 0o644)
	_ = os.WriteFile(filepath.Join(storiesDir, "2026-11-07-gov-5176.story.md"), []byte("---\nid: STORY-GOV-5176\nstatus: DONE\n---\n"), 0o644)
	_ = os.WriteFile(filepath.Join(repoDir, ".session_story_id"), []byte("STORY-GOV-5176\n"), 0o644)
	_ = exec.Command("git", "-C", repoDir, "add", ".").Run()
	_ = exec.Command("git", "-C", repoDir, "commit", "-m", "feat(STORY-GOV-5176): complete").Run()
	if !isASEFlowCompletedInWorktree(repoDir, "STORY-GOV-5176", "inspected STORY-OTHER-999 during audit\n## CRAFT Closeout\nAll done", "git") {
		t.Fatalf("expected isASEFlowCompletedInWorktree=true when primaryStoryID STORY-GOV-5176 is DONE even if combinedOutput mentions STORY-OTHER-999")
	}

	// 3. Verify strict loopback URL parsing and resolveControlPlaneEnv test-binary isolation across all 5 env vars
	if !isLoopbackControlPlaneURL("http://127.0.0.1:8080") || !isLoopbackControlPlaneURL("http://localhost:8080") || !isLoopbackControlPlaneURL("http://[::1]:8080") {
		t.Fatal("expected valid loopback URLs to be accepted")
	}
	if isLoopbackControlPlaneURL("http://localhost.evil.tld:8080") || isLoopbackControlPlaneURL("http://127.0.0.1.nip.io:8080") || isLoopbackControlPlaneURL("https://tian.go.ro/iazio-harness-api") {
		t.Fatal("expected spoofed or external URLs to be rejected by isLoopbackControlPlaneURL")
	}

	t.Setenv("IAZIO_HARNESS_API_URL", "https://production.invalid.example/iazio-harness-api")
	t.Setenv("IAZIO_JOB_ID", "job_bbd74e1ad21fbc40")
	t.Setenv("IAZIO_FENCING_TOKEN", "77")
	t.Setenv("IAZIO_LEASE_TOKEN", "lease-prod-77")
	t.Setenv("IAZIO_RESUME_CONVERSATION_ID", "11111111-2222-3333-4444-555555555555")
	t.Setenv("IAZIO_CONVERSATION_ID", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	gotJob, gotAPI, gotLease, gotResume, gotEnvConv, gotFT := resolveControlPlaneEnv(RunPromptOptions{})
	if gotJob != "" || gotAPI != "" || gotLease != "" || gotResume != "" || gotEnvConv != "" || gotFT != 0 {
		t.Fatalf("expected resolveControlPlaneEnv to clear all inherited non-loopback env vars in test binary, got job=%q api=%q lease=%q resume=%q conv=%q ft=%d",
			gotJob, gotAPI, gotLease, gotResume, gotEnvConv, gotFT)
	}

	// 4. Verify isTranscriptAtTerminalCompletedResponse step baseline check and RunPrompt post-turn-idle settlement
	// with empty opts.StoryID (auto-discovered from worktree) and growing PostTick silent_ms.
	convID := "0092d1cb-cc35-41ce-9143-a480dca5e283"
	logDir := filepath.Join(homeDir, ".gemini", "antigravity-cli", "brain", convID, ".system_generated", "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatalf("mkdir logDir: %v", err)
	}
	transcriptPath := filepath.Join(logDir, "transcript.jsonl")
	terminalLine := `{"step_index":477,"type":"PLANNER_RESPONSE","status":"DONE","content":"## CRAFT Closeout\nCompleted STORY-GOV-5176."}` + "\n"
	if err := os.WriteFile(transcriptPath, []byte(terminalLine), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	if !isTranscriptAtTerminalCompletedResponse(convID, 0) {
		t.Fatalf("expected isTranscriptAtTerminalCompletedResponse(%s, 0)=true", convID)
	}
	if isTranscriptAtTerminalCompletedResponse(convID, 477) {
		t.Fatalf("expected isTranscriptAtTerminalCompletedResponse(%s, 477)=false when step_index <= minStepIndex", convID)
	}

	var tickMu sync.Mutex
	var maxSilentMs int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/chunks") {
			var body struct {
				Type        string `json:"type"`
				SilentForMs int64  `json:"silent_for_ms"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Type == "OUTPUT_TICK" {
				tickMu.Lock()
				if body.SilentForMs > maxSilentMs {
					maxSilentMs = body.SilentForMs
				}
				tickMu.Unlock()
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	mockAgyBin := filepath.Join(t.TempDir(), "mock-agy-post-turn-hang")
	script := fmt.Sprintf(`#!/usr/bin/env bash
echo "CONVERSATION_ID: %s"
echo "ALLOCATED_STORY_ID: STORY-GOV-5176"
echo "## CRAFT Closeout"
echo "## Fleet Git Status Summary"
sleep 30
`, convID)
	if err := os.WriteFile(mockAgyBin, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock-agy-post-turn-hang: %v", err)
	}

	runner := NewPromptRunner("autopilot")
	res, err := runner.RunPrompt(context.Background(), RunPromptOptions{
		PromptID:    "prompt_ase_0193",
		JobID:       "job_local_0193",
		APIURL:      srv.URL,
		Repo:        "meta-repo",
		StoryID:     "", // Empty StoryID: exercises auto-discovery without resetting lastOutAt on every tick (F-01)
		Prompt:      "/autonomous-story-execution",
		TargetAgent: mockAgyBin,
		WorkingDir:  repoDir,
		Timeout:     10 * time.Second,
	})
	if err != nil {
		t.Fatalf("expected RunPrompt to settle post-turn idle completion without timing out, got err=%v", err)
	}
	if res.Status != "completed" || res.ExitCode != 0 {
		t.Fatalf("expected status=completed exit=0, got status=%s exit=%d", res.Status, res.ExitCode)
	}
	if !strings.Contains(res.Summary, "ase_completion.detected_post_turn_idle") {
		t.Fatalf("expected summary to contain ase_completion.detected_post_turn_idle, got:\n%s", res.Summary)
	}
	tickMu.Lock()
	gotMaxSilent := maxSilentMs
	tickMu.Unlock()
	if gotMaxSilent <= 0 {
		t.Fatalf("expected PostTick silent_ms to grow > 0 while tool is silent, got maxSilentMs=%d", gotMaxSilent)
	}
}



