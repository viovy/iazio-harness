package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/viovy/iazio-harness/internal/checkpoint"
	"github.com/viovy/iazio-harness/internal/controlplane"
	"github.com/viovy/iazio-harness/internal/spool"
)

func TestRunJobDefaults(t *testing.T) {
	// Missing job-id must error
	err := runJob([]string{})
	if err == nil || !strings.Contains(err.Error(), "job-id is required") {
		t.Fatalf("expected job-id required error, got %v", err)
	}

	// Environment variable fallback
	os.Setenv("IAZIO_HARNESS_API_URL", "http://custom-url")
	defer os.Unsetenv("IAZIO_HARNESS_API_URL")

	err = runJob([]string{})
	if err == nil || !strings.Contains(err.Error(), "job-id is required") {
		t.Fatalf("expected job-id required error with env set, got %v", err)
	}

	// Test ~/.iazio/api_url file resolution
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("IAZIO_HARNESS_API_URL", "")
	_ = os.MkdirAll(filepath.Join(tmpHome, ".iazio"), 0o755)
	_ = os.WriteFile(filepath.Join(tmpHome, ".iazio", "api_url"), []byte("http://home-resolved:8090"), 0o644)
	err = runJob([]string{})
	if err == nil || !strings.Contains(err.Error(), "job-id is required") {
		t.Fatalf("expected job-id required error with home set, got %v", err)
	}

	// Test --chunk flag execution
	chunkSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	defer chunkSrv.Close()
	err = runJob([]string{"--api-url", chunkSrv.URL, "--job-id", "j123", "--chunk", "sample log output"})
	if err != nil {
		t.Fatalf("expected successful chunk post via runJob, got %v", err)
	}
}

func TestMainVersionInvocation(t *testing.T) {
	origArgs := os.Args
	defer func() { os.Args = origArgs }()

	os.Args = []string{"iazio-harness", "version"}
	main()
}

func TestFormatVersion(t *testing.T) {
	v := formatVersion()
	if !strings.Contains(v, "iazio-harness") {
		t.Fatalf("expected iazio-harness version, got %s", v)
	}

	origCommit := commit
	defer func() { commit = origCommit }()
	commit = "1234567890abcdef1234"
	vWithCommit := formatVersion()
	if !strings.Contains(vWithCommit, "+1234567890ab") {
		t.Fatalf("expected truncated commit in formatVersion, got %s", vWithCommit)
	}
}

func TestResolveExecutionOptions(t *testing.T) {
	// 1. Defaults for normal prompt
	m, a, disableSlash := resolveExecutionOptions(controlplane.JobDetail{}, "Refactor the authentication module")
	if m != "gemini-3.8-flash-high" {
		t.Fatalf("expected default model gemini-3.8-flash-high, got %s", m)
	}
	if a != "" {
		t.Fatalf("expected empty default agent, got %s", a)
	}
	if !disableSlash {
		t.Fatalf("expected disableSlash=true for normal prompt")
	}

	// 2. Slash command prompt adaptively enables slash commands
	_, _, disableSlash = resolveExecutionOptions(controlplane.JobDetail{}, "/autonomous-story-execution on mac-mini")
	if disableSlash {
		t.Fatalf("expected disableSlash=false for slash command prompt")
	}

	// 3. Explicit Model and Agent in job detail
	f := false
	m, a, disableSlash = resolveExecutionOptions(controlplane.JobDetail{
		Model:                "claude-opus-4.6",
		Agent:                "reviewer",
		DisableSlashCommands: &f,
	}, "Refactor code")
	if m != "claude-opus-4.6" || a != "reviewer" || disableSlash {
		t.Fatalf("expected explicit options applied, got m=%s, a=%s, disableSlash=%v", m, a, disableSlash)
	}

	// 4. Fallback to EnvVars
	m, a, disableSlash = resolveExecutionOptions(controlplane.JobDetail{
		EnvVars: map[string]string{
			"MODEL":                 "custom-model",
			"AGENT":                 "custom-agent",
			"ENABLE_SLASH_COMMANDS": "true",
		},
	}, "Do something")
	if m != "custom-model" || a != "custom-agent" || disableSlash {
		t.Fatalf("expected env vars applied, got m=%s, a=%s, disableSlash=%v", m, a, disableSlash)
	}
}

func TestRunStoryCommand_Validations(t *testing.T) {
	// Missing args
	err := runStoryCommand([]string{})
	if err == nil || !strings.Contains(err.Error(), "usage") {
		t.Fatalf("expected usage error, got %v", err)
	}

	// Unknown subcommand
	err = runStoryCommand([]string{"destroy"})
	if err == nil || !strings.Contains(err.Error(), "unknown story subcommand") {
		t.Fatalf("expected unknown subcommand error, got %v", err)
	}

	// Missing --json flag
	err = runStoryCreate([]string{})
	if err == nil || !strings.Contains(err.Error(), "requires --json") {
		t.Fatalf("expected requires --json error, got %v", err)
	}
}

func TestRunPromptCommand_Validations(t *testing.T) {
	// Missing args
	err := runPromptCommand([]string{})
	if err == nil || !strings.Contains(err.Error(), "usage") {
		t.Fatalf("expected usage error, got %v", err)
	}

	// Unknown subcommand
	err = runPromptCommand([]string{"destroy"})
	if err == nil || !strings.Contains(err.Error(), "unknown prompt subcommand") {
		t.Fatalf("expected unknown subcommand error, got %v", err)
	}

	// Missing --json flag
	err = runPromptRun([]string{})
	if err == nil || !strings.Contains(err.Error(), "requires --json") {
		t.Fatalf("expected requires --json error, got %v", err)
	}
}

func TestRunCodebaseCommand_Validations(t *testing.T) {
	// Missing args
	err := runCodebaseCommand([]string{})
	if err == nil || !strings.Contains(err.Error(), "usage") {
		t.Fatalf("expected usage error, got %v", err)
	}

	// Unknown subcommand
	err = runCodebaseCommand([]string{"destroy"})
	if err == nil || !strings.Contains(err.Error(), "unknown codebase subcommand") {
		t.Fatalf("expected unknown subcommand error, got %v", err)
	}

	// Missing --json flag on repos
	err = runCodebaseRepos([]string{})
	if err == nil || !strings.Contains(err.Error(), "requires --json") {
		t.Fatalf("expected requires --json error on repos, got %v", err)
	}

	// Missing --json flag on export
	err = runCodebaseExport([]string{})
	if err == nil || !strings.Contains(err.Error(), "requires --json") {
		t.Fatalf("expected requires --json error on export, got %v", err)
	}
}

func TestRunDispatch(t *testing.T) {
	// Empty args
	if err := run([]string{}); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Errorf("expected usage error on empty args, got %v", err)
	}

	// Version
	if err := run([]string{"version"}); err != nil {
		t.Errorf("unexpected error on version: %v", err)
	}

	// Auth
	cfgPath := filepath.Join(t.TempDir(), "harness.json")
	t.Setenv("IAZIO_HARNESS_CONFIG", cfgPath)
	if err := run([]string{"auth"}); err != nil {
		t.Errorf("unexpected error on auth: %v", err)
	}

	// Auth login noninteractive
	t.Setenv("IAZIO_HARNESS_NONINTERACTIVE", "1")
	if err := run([]string{"auth", "login"}); err == nil {
		t.Errorf("expected error on noninteractive auth login without token")
	}

	// Subcommand dispatches
	if err := run([]string{"story"}); err == nil {
		t.Errorf("expected story usage error")
	}
	if err := run([]string{"prompt"}); err == nil {
		t.Errorf("expected prompt usage error")
	}
	if err := run([]string{"codebase"}); err == nil {
		t.Errorf("expected codebase usage error")
	}
	if err := run([]string{"run"}); err == nil {
		t.Errorf("expected run usage error")
	}
	if err := run([]string{"nonexistent-command"}); err == nil {
		t.Errorf("expected unknown command error")
	}
}

func TestRunJobChunkAndExecution(t *testing.T) {
	var chunkReceived string
	var exitCodeReceived int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/chunks") {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if txt, ok := body["text"].(string); ok {
				chunkReceived = txt
			}
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/exit") {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if ec, ok := body["exit_code"].(float64); ok {
				exitCodeReceived = int(ec)
			}
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.URL.Path == "/v1/jobs/job-err" {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "job not found"})
			return
		}
		if r.URL.Path == "/v1/jobs/job-no-worktree" {
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(controlplane.JobDetail{
				ID:           "job-no-worktree",
				Kind:         "story_refinement",
				DocsHubPath:  "",
				WorktreePath: "",
			})
			return
		}
		if r.URL.Path == "/v1/jobs/job-exec" {
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(controlplane.JobDetail{
				ID:           "job-exec",
				Kind:         "ordinary",
				Engine:       "nonexistent-engine",
				Prompt:       "do task",
				WorktreePath: t.TempDir(),
			})
			return
		}
		if r.URL.Path == "/v1/jobs/job-resume" {
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(controlplane.JobDetail{
				ID:                          "job-resume",
				Kind:                        "resume",
				StoryID:                     "STORY-RESUME-01",
				Engine:                      "nonexistent-engine",
				Prompt:                      "continue story",
				WorktreePath:                t.TempDir(),
				MaxExecutionDurationSeconds: 120,
				EnvVars: map[string]string{
					"RESUME_CONVERSATION_ID": "conv-prior-123",
				},
			})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// 1. Post chunk via runJob
	err := runJob([]string{"--api-url", srv.URL, "--job-id", "job-1", "--chunk", "sample log message"})
	if err != nil {
		t.Fatalf("runJob chunk failed: %v", err)
	}
	if chunkReceived != "sample log message" {
		t.Errorf("expected chunk 'sample log message', got %q", chunkReceived)
	}

	// 2. Pre-execution error handling in executeJob (job not found)
	err = executeJob(context.Background(), srv.URL, "job-err", "", "")
	if err == nil {
		t.Fatalf("expected executeJob to fail when job not found")
	}
	if exitCodeReceived != 1 {
		t.Errorf("expected exit code 1 posted on pre-execution error, got %d", exitCodeReceived)
	}

	// 3. Checkout dir error handling
	err = executeJob(context.Background(), srv.URL, "job-no-worktree", "", "")
	if err == nil {
		t.Fatalf("expected executeJob to fail on checkout dir")
	}

	// 4. Execution flow with unknown engine
	err = executeJob(context.Background(), srv.URL, "job-exec", "", "")
	if err == nil {
		t.Fatalf("expected executeJob to fail with unknown engine")
	}

	// 5. Execution flow with resume kind
	_ = executeJob(context.Background(), srv.URL, "job-resume", "", "")
}

func TestExecuteJob_SuccessfulRunWithMockEngine(t *testing.T) {
	mockDir := t.TempDir()
	mockAgy := filepath.Join(mockDir, "agy")
	_ = os.WriteFile(mockAgy, []byte("#!/bin/sh\necho 'mock agy output'\nexit 0\n"), 0755)

	t.Setenv("PATH", mockDir+":"+os.Getenv("PATH"))

	workDir := t.TempDir()
	var exitCodePosted int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/chunks") {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/exit") {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if ec, ok := body["exit_code"].(float64); ok {
				exitCodePosted = int(ec)
			}
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.URL.Path == "/v1/jobs/job-mock-agy" {
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(controlplane.JobDetail{
				ID:           "job-mock-agy",
				Kind:         "ordinary",
				Engine:       "agy",
				Prompt:       "test task",
				WorktreePath: workDir,
			})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	err := executeJob(context.Background(), srv.URL, "job-mock-agy", workDir, "")
	if err != nil {
		t.Fatalf("executeJob failed: %v", err)
	}
	if exitCodePosted != 0 {
		t.Errorf("expected exit code 0 posted, got %d", exitCodePosted)
	}
}

func TestExecuteJob_AutoFallbackOnQuota(t *testing.T) {
	mockHome := t.TempDir()
	t.Setenv("HOME", mockHome)
	mockDir := t.TempDir()
	mockAgent := filepath.Join(mockDir, "agent")
	_ = os.WriteFile(mockAgent, []byte("#!/bin/sh\nif [ \"$1\" = \"create-chat\" ]; then echo 'cursor-chat-123'; exit 0; fi\necho \"ActionRequiredError: You're out of usage.\"\nexit 1\n"), 0755)
	mockAgy := filepath.Join(mockDir, "agy")
	agyConvID := "1d446877-5bf9-44eb-9db5-e1c007a9fe31"
	_ = os.WriteFile(mockAgy, []byte(fmt.Sprintf(`#!/bin/sh
while [ $# -gt 0 ]; do
  case "$1" in
    --log-file)
      shift
      echo "I1024 12:00:00.000000 1 server.go:1274] Created conversation %s" > "$1"
      ;;
  esac
  shift
done
echo 'fallback agy execution success'
exit 0
`, agyConvID)), 0755)

	t.Setenv("PATH", mockDir+":"+os.Getenv("PATH"))

	workDir := t.TempDir()
	var postedConvs []map[string]any
	var exitPosted map[string]any
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/chunks") {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/conversations") {
			var m map[string]any
			_ = json.NewDecoder(r.Body).Decode(&m)
			mu.Lock()
			postedConvs = append(postedConvs, m)
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/exit") {
			var m map[string]any
			_ = json.NewDecoder(r.Body).Decode(&m)
			mu.Lock()
			exitPosted = m
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.URL.Path == "/v1/jobs/job-quota-fallback" {
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(controlplane.JobDetail{
				ID:           "job-quota-fallback",
				Kind:         "ordinary",
				Engine:       "agent",
				Prompt:       "task needing quota",
				WorktreePath: workDir,
			})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	err := executeJob(context.Background(), srv.URL, "job-quota-fallback", workDir, "")
	if err != nil && !strings.Contains(err.Error(), "engine agy") {
		t.Fatalf("expected fallback to agy to execute, got %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	// Finding C2 Verification: the agy conversation MUST be registered as fallback_primary, NOT dropped_unpinned!
	foundAgyFallback := false
	for _, pc := range postedConvs {
		if pc["conversation_id"] == agyConvID {
			if pc["role"] == "dropped_unpinned" {
				t.Fatalf("finding C2 regression: agy conversation %s was mislabeled as dropped_unpinned", agyConvID)
			}
			if pc["role"] == "fallback_primary" && pc["fallback"] == true {
				foundAgyFallback = true
			}
		}
	}
	if !foundAgyFallback {
		t.Fatalf("expected agy conversation %s to be registered as fallback_primary with fallback:true, got: %+v", agyConvID, postedConvs)
	}
	if exitPosted == nil {
		t.Fatal("expected exit to be posted")
	}
	if exitPosted["engine"] != "agy" {
		t.Fatalf("expected exit engine to be agy, got %v", exitPosted["engine"])
	}

	ck, err := checkpoint.ReadWorktree(workDir)
	if err != nil {
		t.Fatalf("failed to read checkpoint: %v", err)
	}
	if ck.ConversationID != agyConvID {
		t.Fatalf("expected checkpoint conversation ID to be %s, got %s", agyConvID, ck.ConversationID)
	}
	if ck.Engine != "agy" {
		t.Fatalf("expected checkpoint engine to be agy, got %s", ck.Engine)
	}
}

func TestExecuteJob_DeterministicResume_ControlPlaneFallbackMode_StartsClean(t *testing.T) {
	mockDir := t.TempDir()
	mockAgy := filepath.Join(mockDir, "agy")
	cleanConvID := "1d446877-5bf9-44eb-9db5-e1c007a9fe31"
	_ = os.WriteFile(mockAgy, []byte(fmt.Sprintf(`#!/bin/sh
for arg in "$@"; do
  case "$arg" in
    --conversation=*)
      echo "FATAL: received unexpected --conversation flag on clean fallback: $arg" >&2
      exit 2
      ;;
  esac
done
while [ $# -gt 0 ]; do
  case "$1" in
    --log-file)
      shift
      echo "I1024 12:00:00.000000 1 server.go:1274] Created conversation %s" > "$1"
      ;;
  esac
  shift
done
echo "clean fallback execution success"
exit 0
`, cleanConvID)), 0755)

	t.Setenv("PATH", mockDir+":"+os.Getenv("PATH"))

	workDir := t.TempDir()
	var postedConvs []map[string]any
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/chunks") {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/conversations") {
			var m map[string]any
			_ = json.NewDecoder(r.Body).Decode(&m)
			mu.Lock()
			postedConvs = append(postedConvs, m)
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/exit") {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.URL.Path == "/v1/jobs/job-cp-fallback" {
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(controlplane.JobDetail{
				ID:                    "job-cp-fallback",
				Kind:                  "resume",
				Engine:                "agy",
				Prompt:                "resume",
				WorktreePath:          workDir,
				InitialConversationID: "aad902c8-bda6-44c7-8a68-c468a1562399",
				ActiveConversationID:  "", // empty -> must start clean
				ResumeMode:            "fallback_new_conversation",
				ResumeFallbackReason:  "cross-engine fallback ordered by CP",
			})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	err := executeJob(context.Background(), srv.URL, "job-cp-fallback", workDir, "")
	if err != nil {
		t.Fatalf("expected successful execution of clean fallback, got: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	foundClean := false
	for _, pc := range postedConvs {
		if pc["conversation_id"] == cleanConvID {
			foundClean = true
			if pc["fallback"] != true {
				t.Fatalf("expected fallback: true, got %v", pc["fallback"])
			}
			if pc["role"] != "fallback_primary" {
				t.Fatalf("expected role: fallback_primary, got %v", pc["role"])
			}
		}
	}
	if !foundClean {
		t.Fatalf("expected clean fallback conversation %s, got: %+v", cleanConvID, postedConvs)
	}
}

func TestExecuteJob_DeterministicResumeConversationPinning(t *testing.T) {
	mockHome := t.TempDir()
	targetConvID := "aad902c8-bda6-44c7-8a68-c468a1562399"
	_ = os.MkdirAll(filepath.Join(mockHome, ".gemini", "antigravity-cli", "brain", targetConvID), 0755)
	t.Setenv("HOME", mockHome)

	mockDir := t.TempDir()
	mockAgy := filepath.Join(mockDir, "agy")
	_ = os.WriteFile(mockAgy, []byte(fmt.Sprintf(`#!/bin/sh
for arg in "$@"; do
  if [ "$arg" = "--conversation=%s" ]; then
    echo "successfully resumed conversation $arg"
    exit 0
  fi
done
echo "error: missing expected --conversation=%s"
exit 1
`, targetConvID, targetConvID)), 0755)

	t.Setenv("PATH", mockDir+":"+os.Getenv("PATH"))

	workDir := t.TempDir()
	var postedConvs []map[string]any
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/chunks") {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/conversations") {
			var m map[string]any
			_ = json.NewDecoder(r.Body).Decode(&m)
			mu.Lock()
			postedConvs = append(postedConvs, m)
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/exit") {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.URL.Path == "/v1/jobs/job-1313-resume" {
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(controlplane.JobDetail{
				ID:                    "job-1313-resume",
				Kind:                  "resume",
				Engine:                "agy",
				Prompt:                "resume",
				WorktreePath:          workDir,
				InitialConversationID: targetConvID,
				ResumeConversationID:  targetConvID,
			})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	err := executeJob(context.Background(), srv.URL, "job-1313-resume", workDir, "")
	if err != nil {
		t.Fatalf("expected successful resume job execution, got: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(postedConvs) == 0 {
		t.Fatal("expected at least one posted conversation")
	}
	for _, pc := range postedConvs {
		if pc["conversation_id"] != targetConvID {
			t.Fatalf("unexpected posted conversation ID: %v (want %s)", pc["conversation_id"], targetConvID)
		}
	}
}

func TestExecuteJob_DeterministicResumeConversationPinning_FallbackNewConversation(t *testing.T) {
	mockHome := t.TempDir()
	targetConvID := "aad902c8-bda6-44c7-8a68-c468a1562399"
	// Do NOT create targetConvID on disk, to trigger Stage 2 fallback
	t.Setenv("HOME", mockHome)

	fallbackConvID := "1d446877-5bf9-44eb-9db5-e1c007a9fe31"
	mockDir := t.TempDir()
	mockAgy := filepath.Join(mockDir, "agy")
	_ = os.WriteFile(mockAgy, []byte(fmt.Sprintf(`#!/bin/sh
while [ $# -gt 0 ]; do
  case "$1" in
    --log-file)
      shift
      echo "I1024 12:00:00.000000 1 server.go:1274] Created conversation %s" > "$1"
      ;;
  esac
  shift
done
echo "fallback execution completed successfully"
exit 0
`, fallbackConvID)), 0755)

	t.Setenv("PATH", mockDir+":"+os.Getenv("PATH"))

	workDir := t.TempDir()
	var postedConvs []map[string]any
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/chunks") {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/conversations") {
			var m map[string]any
			_ = json.NewDecoder(r.Body).Decode(&m)
			mu.Lock()
			postedConvs = append(postedConvs, m)
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/exit") {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.URL.Path == "/v1/jobs/job-1313-fallback" {
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(controlplane.JobDetail{
				ID:                    "job-1313-fallback",
				Kind:                  "resume",
				Engine:                "agy",
				Prompt:                "resume",
				WorktreePath:          workDir,
				InitialConversationID: targetConvID,
				ResumeConversationID:  targetConvID,
				InterruptedASEState: map[string]any{
					"phase": "PHASE_4_METADATA_REVIEW",
					"step":  3,
				},
			})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	err := executeJob(context.Background(), srv.URL, "job-1313-fallback", workDir, "")
	if err != nil {
		t.Fatalf("expected successful fallback resume execution, got: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(postedConvs) == 0 {
		t.Fatal("expected at least one posted conversation on fallback")
	}
	foundFallback := false
	for _, pc := range postedConvs {
		if pc["conversation_id"] == fallbackConvID {
			foundFallback = true
			if pc["fallback"] != true {
				t.Fatalf("expected fallback: true, got %v", pc["fallback"])
			}
			if pc["role"] != "fallback_primary" {
				t.Fatalf("expected role: fallback_primary, got %v", pc["role"])
			}
		}
	}
	if !foundFallback {
		t.Fatalf("expected posted conversation %s, got: %+v", fallbackConvID, postedConvs)
	}
}

func TestExecuteJob_DeterministicResume_EngineAllocatesNewConversation_HandledAsFallback(t *testing.T) {
	mockHome := t.TempDir()
	targetConvID := "aad902c8-bda6-44c7-8a68-c468a1562399"
	_ = os.MkdirAll(filepath.Join(mockHome, ".gemini", "antigravity-cli", "brain", targetConvID), 0755)
	t.Setenv("HOME", mockHome)

	fallbackConvID := "1d446877-5bf9-44eb-9db5-e1c007a9fe31"
	mockDir := t.TempDir()
	mockAgy := filepath.Join(mockDir, "agy")
	_ = os.WriteFile(mockAgy, []byte(fmt.Sprintf(`#!/bin/sh
while [ $# -gt 0 ]; do
  case "$1" in
    --log-file)
      shift
      echo "I1024 12:00:00.000000 1 server.go:1274] Created conversation %s" > "$1"
      ;;
  esac
  shift
done
echo "Created conversation %s"
exit 0
`, fallbackConvID, fallbackConvID)), 0755)

	t.Setenv("PATH", mockDir+":"+os.Getenv("PATH"))

	workDir := t.TempDir()
	var postedConvs []map[string]any
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/chunks") {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/conversations") {
			var m map[string]any
			_ = json.NewDecoder(r.Body).Decode(&m)
			mu.Lock()
			postedConvs = append(postedConvs, m)
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/exit") {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.URL.Path == "/v1/jobs/job-1313-fork" {
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(controlplane.JobDetail{
				ID:                    "job-1313-fork",
				Kind:                  "resume",
				Engine:                "agy",
				Prompt:                "resume",
				WorktreePath:          workDir,
				InitialConversationID: targetConvID,
				ResumeConversationID:  targetConvID,
			})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	err := executeJob(context.Background(), srv.URL, "job-1313-fork", workDir, "")
	if err != nil {
		t.Fatalf("expected successful execution, got: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(postedConvs) == 0 {
		t.Fatal("expected at least one posted conversation")
	}
	foundFallback := false
	for _, pc := range postedConvs {
		if pc["conversation_id"] == fallbackConvID {
			foundFallback = true
			if pc["fallback"] != true {
				t.Fatalf("expected fallback: true, got %v", pc["fallback"])
			}
			if pc["role"] != "fallback_primary" {
				t.Fatalf("expected role: fallback_primary, got %v", pc["role"])
			}
		}
	}
	if !foundFallback {
		t.Fatalf("expected fallback conversation %s to be posted, got: %+v", fallbackConvID, postedConvs)
	}
}

func TestExecuteJob_DeterministicResume_TaskFailure_NoSecondExecution(t *testing.T) {
	mockHome := t.TempDir()
	targetConvID := "aad902c8-bda6-44c7-8a68-c468a1562399"
	_ = os.MkdirAll(filepath.Join(mockHome, ".gemini", "antigravity-cli", "brain", targetConvID), 0755)
	t.Setenv("HOME", mockHome)

	mockDir := t.TempDir()
	countFile := filepath.Join(mockDir, "invocations.txt")
	mockAgy := filepath.Join(mockDir, "agy")
	// The mock engine is called with --conversation=targetConvID, prints NO session markers at all, and exits 1 due to task error
	_ = os.WriteFile(mockAgy, []byte(fmt.Sprintf(`#!/bin/sh
echo "run" >> "%s"
echo "compiling tests..."
echo "running suite..."
echo "FAIL: task test assertion error" >&2
exit 1
`, countFile)), 0755)

	t.Setenv("PATH", mockDir+":"+os.Getenv("PATH"))

	workDir := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/chunks") || strings.HasSuffix(r.URL.Path, "/conversations") || strings.HasSuffix(r.URL.Path, "/exit") {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.URL.Path == "/v1/jobs/job-1313-taskfail" {
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(controlplane.JobDetail{
				ID:                    "job-1313-taskfail",
				Kind:                  "resume",
				Engine:                "agy",
				Prompt:                "resume",
				WorktreePath:          workDir,
				InitialConversationID: targetConvID,
				ResumeConversationID:  targetConvID,
			})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	_ = executeJob(context.Background(), srv.URL, "job-1313-taskfail", workDir, "")

	data, err := os.ReadFile(countFile)
	if err != nil {
		t.Fatalf("expected count file: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected exactly 1 execution (no duplicate Stage 2 fallback on task error), got %d executions", len(lines))
	}
}

func TestExecuteJob_DeterministicResume_ToolOutputNotAttachError(t *testing.T) {
	mockHome := t.TempDir()
	targetConvID := "aad902c8-bda6-44c7-8a68-c468a1562399"
	_ = os.MkdirAll(filepath.Join(mockHome, ".gemini", "antigravity-cli", "brain", targetConvID), 0755)
	t.Setenv("HOME", mockHome)

	mockDir := t.TempDir()
	countFile := filepath.Join(mockDir, "invocations.txt")
	mockAgy := filepath.Join(mockDir, "agy")
	// The mock tool outputs a line containing "conversation not found" in regular command stdout, but NOT an engine error line
	_ = os.WriteFile(mockAgy, []byte(fmt.Sprintf(`#!/bin/sh
echo "run" >> "%s"
echo "grep: checking source files..."
echo "file scanner: conversation not found in local index"
exit 1
`, countFile)), 0755)

	t.Setenv("PATH", mockDir+":"+os.Getenv("PATH"))

	workDir := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/chunks") || strings.HasSuffix(r.URL.Path, "/conversations") || strings.HasSuffix(r.URL.Path, "/exit") {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.URL.Path == "/v1/jobs/job-1313-tool-output-no-fallback" {
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(controlplane.JobDetail{
				ID:                    "job-1313-tool-output-no-fallback",
				Kind:                  "resume",
				Engine:                "agy",
				Prompt:                "resume",
				WorktreePath:          workDir,
				InitialConversationID: targetConvID,
				ResumeConversationID:  targetConvID,
			})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	_ = executeJob(context.Background(), srv.URL, "job-1313-tool-output-no-fallback", workDir, "")

	data, err := os.ReadFile(countFile)
	if err != nil {
		t.Fatalf("expected count file: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected exactly 1 execution (no false-positive Stage 2 fallback from tool output), got %d executions", len(lines))
	}
}

func TestExecuteJob_DeterministicResume_PostRunAttachError_SingleFallback(t *testing.T) {
	mockHome := t.TempDir()
	targetConvID := "aad902c8-bda6-44c7-8a68-c468a1562399"
	fallbackConvID := "99998888-7777-6666-5555-444433332222"
	// Session exists on disk so Stage 1 pre-flight succeeds
	_ = os.MkdirAll(filepath.Join(mockHome, ".gemini", "antigravity-cli", "brain", targetConvID), 0755)
	t.Setenv("HOME", mockHome)

	mockDir := t.TempDir()
	countFile := filepath.Join(mockDir, "invocations.txt")
	argsFile := filepath.Join(mockDir, "args.txt")
	mockAgy := filepath.Join(mockDir, "agy")
	_ = os.WriteFile(mockAgy, []byte(fmt.Sprintf(`#!/bin/sh
echo "$@" >> "%s"
if ! grep -q "run1" "%s" 2>/dev/null; then
    echo "run1" >> "%s"
    PREV=""
    for arg in "$@"; do
        if [ "$PREV" = "--log-file" ]; then
            echo "Error: failed to attach to conversation: corrupted session database" > "$arg"
        fi
        PREV="$arg"
    done
    echo "Error: failed to attach to conversation: corrupted session database" >&2
    exit 1
else
    echo "run2" >> "%s"
    echo "Conversation ID: %s"
    exit 0
fi
`, argsFile, countFile, countFile, countFile, fallbackConvID)), 0755)

	t.Setenv("PATH", mockDir+":"+os.Getenv("PATH"))

	workDir := t.TempDir()
	var postMu sync.Mutex
	postedConvs := []map[string]any{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/chunks") || strings.HasSuffix(r.URL.Path, "/exit") {
			w.WriteHeader(http.StatusOK)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/conversations") {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			postMu.Lock()
			postedConvs = append(postedConvs, body)
			postMu.Unlock()
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.URL.Path == "/v1/jobs/job-1313-postrun-attach-fail" {
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(controlplane.JobDetail{
				ID:                    "job-1313-postrun-attach-fail",
				Kind:                  "resume",
				Engine:                "agy",
				Prompt:                "resume",
				WorktreePath:          workDir,
				InitialConversationID: targetConvID,
				ResumeConversationID:  targetConvID,
			})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	_ = executeJob(context.Background(), srv.URL, "job-1313-postrun-attach-fail", workDir, "")

	data, err := os.ReadFile(countFile)
	if err != nil {
		t.Fatalf("expected count file: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected exactly 2 executions (run1 attach fail, run2 fallback), got %d executions: %v", len(lines), lines)
	}

	argsData, _ := os.ReadFile(argsFile)
	argLines := strings.Split(strings.TrimSpace(string(argsData)), "\n")
	if len(argLines) < 2 {
		t.Fatalf("expected at least 2 arg lines, got: %v", argLines)
	}
	if !strings.Contains(argLines[0], "--conversation="+targetConvID) {
		t.Fatalf("expected first run to have --conversation=%s, got: %s", targetConvID, argLines[0])
	}
	if strings.Contains(argLines[1], "--conversation="+targetConvID) {
		t.Fatalf("expected second run to NOT have --conversation=%s, got: %s", targetConvID, argLines[1])
	}

	postMu.Lock()
	defer postMu.Unlock()
	foundFallback := false
	for _, pc := range postedConvs {
		if pc["conversation_id"] == fallbackConvID {
			foundFallback = true
			if pc["fallback"] != true {
				t.Fatalf("expected fallback: true, got %v", pc["fallback"])
			}
			if pc["role"] != "fallback_primary" {
				t.Fatalf("expected role: fallback_primary, got %v", pc["role"])
			}
		}
	}
	if !foundFallback {
		t.Fatalf("expected fallback conversation %s to be posted, got: %+v", fallbackConvID, postedConvs)
	}
}

func TestExecuteJob_DeterministicResume_Run1AlreadyFellBack_NoStage2DoubleAllocation(t *testing.T) {
	mockHome := t.TempDir()
	targetConvID := "aad902c8-bda6-44c7-8a68-c468a1562399"
	fallbackConvX := "11112222-3333-4444-5555-666677778888"
	_ = os.MkdirAll(filepath.Join(mockHome, ".gemini", "antigravity-cli", "brain", targetConvID), 0755)
	t.Setenv("HOME", mockHome)

	mockDir := t.TempDir()
	countFile := filepath.Join(mockDir, "invocations.txt")
	mockAgy := filepath.Join(mockDir, "agy")
	// Run 1 emits "Conversation ID: fallbackConvX" during execution, and then exits with attach error.
	// Since run 1 already fell back to fallbackConvX, Stage 2 must NOT execute again and allocate a second ID (Finding F1).
	_ = os.WriteFile(mockAgy, []byte(fmt.Sprintf(`#!/bin/sh
echo "run" >> "%s"
PREV=""
for arg in "$@"; do
    if [ "$PREV" = "--log-file" ]; then
        echo "Conversation ID: %s" > "$arg"
        echo "fatal: session not found on host" >> "$arg"
    fi
    PREV="$arg"
done
echo "Conversation ID: %s"
exit 1
`, countFile, fallbackConvX, fallbackConvX)), 0755)

	t.Setenv("PATH", mockDir+":"+os.Getenv("PATH"))

	workDir := t.TempDir()
	var postMu sync.Mutex
	postedConvs := []map[string]any{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/chunks") || strings.HasSuffix(r.URL.Path, "/exit") {
			w.WriteHeader(http.StatusOK)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/conversations") {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			postMu.Lock()
			postedConvs = append(postedConvs, body)
			postMu.Unlock()
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.URL.Path == "/v1/jobs/job-f1-check" {
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(controlplane.JobDetail{
				ID:                    "job-f1-check",
				Kind:                  "resume",
				Engine:                "agy",
				Prompt:                "resume",
				WorktreePath:          workDir,
				InitialConversationID: targetConvID,
				ResumeConversationID:  targetConvID,
			})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	_ = executeJob(context.Background(), srv.URL, "job-f1-check", workDir, "")

	data, err := os.ReadFile(countFile)
	if err != nil {
		t.Fatalf("expected count file: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected exactly 1 execution (no secondary Stage 2 allocation when run 1 already fell back), got %d executions: %v", len(lines), lines)
	}

	postMu.Lock()
	defer postMu.Unlock()
	if len(postedConvs) != 1 {
		t.Fatalf("expected exactly 1 posted conversation, got %d: %+v", len(postedConvs), postedConvs)
	}
	if postedConvs[0]["conversation_id"] != fallbackConvX {
		t.Fatalf("expected conversation_id %s, got %v", fallbackConvX, postedConvs[0]["conversation_id"])
	}
	if postedConvs[0]["role"] != "fallback_primary" {
		t.Fatalf("expected role fallback_primary, got %v", postedConvs[0]["role"])
	}
}

func TestExecuteJob_DeterministicResume_SpoolDatabaseKeyword_NoFallback(t *testing.T) {
	mockHome := t.TempDir()
	targetConvID := "aad902c8-bda6-44c7-8a68-c468a1562399"
	_ = os.MkdirAll(filepath.Join(mockHome, ".gemini", "antigravity-cli", "brain", targetConvID), 0755)
	t.Setenv("HOME", mockHome)

	mockDir := t.TempDir()
	countFile := filepath.Join(mockDir, "invocations.txt")
	mockAgy := filepath.Join(mockDir, "agy")
	// The tool outputs "Error: database migration failed" in tool stdout, but NOT in engine log
	_ = os.WriteFile(mockAgy, []byte(fmt.Sprintf(`#!/bin/sh
echo "run" >> "%s"
echo "Error: database migration failed"
exit 1
`, countFile)), 0755)

	t.Setenv("PATH", mockDir+":"+os.Getenv("PATH"))

	workDir := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/chunks") || strings.HasSuffix(r.URL.Path, "/conversations") || strings.HasSuffix(r.URL.Path, "/exit") {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.URL.Path == "/v1/jobs/job-db-keyword-test" {
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(controlplane.JobDetail{
				ID:                    "job-db-keyword-test",
				Kind:                  "resume",
				Engine:                "agy",
				Prompt:                "resume",
				WorktreePath:          workDir,
				InitialConversationID: targetConvID,
				ResumeConversationID:  targetConvID,
			})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	_ = executeJob(context.Background(), srv.URL, "job-db-keyword-test", workDir, "")

	data, err := os.ReadFile(countFile)
	if err != nil {
		t.Fatalf("expected count file: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected exactly 1 execution (no false-positive Stage 2 from database keyword), got %d executions", len(lines))
	}
}

func TestExecuteJob_DeterministicResume_ControlPlaneResume_NoHeuristicDiscovery(t *testing.T) {
	mockHome := t.TempDir()
	t.Setenv("HOME", mockHome)

	mockDir := t.TempDir()
	countFile := filepath.Join(mockDir, "invocations.txt")
	argsFile := filepath.Join(mockDir, "args.txt")
	mockAgy := filepath.Join(mockDir, "agy")
	_ = os.WriteFile(mockAgy, []byte(fmt.Sprintf(`#!/bin/sh
echo "run" >> "%s"
echo "$@" >> "%s"
echo "Conversation ID: new-allocated-conv-12345"
exit 0
`, countFile, argsFile)), 0755)

	t.Setenv("PATH", mockDir+":"+os.Getenv("PATH"))

	workDir := t.TempDir()
	// Create dirty context that would normally trigger heuristic discovery
	_ = os.MkdirAll(filepath.Join(workDir, "docs", "reviews"), 0755)
	_ = os.WriteFile(filepath.Join(workDir, "docs", "reviews", "2026-10-24-review-test.md"), []byte(`---
conversation_id: "heuristic-guessed-conv-id"
---
`), 0644)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/chunks") || strings.HasSuffix(r.URL.Path, "/conversations") || strings.HasSuffix(r.URL.Path, "/exit") {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.URL.Path == "/v1/jobs/job-cp-no-heuristic" {
			w.WriteHeader(http.StatusOK)
			// Control-plane resume with empty ResumeConversationID (fallback or fresh resume requested)
			_ = json.NewEncoder(w).Encode(controlplane.JobDetail{
				ID:           "job-cp-no-heuristic",
				ParentJobID:  "job-parent-123",
				RootJobID:    "job-root-100",
				Kind:         "resume",
				Engine:       "agy",
				Prompt:       "resume",
				WorktreePath: workDir,
			})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	_ = executeJob(context.Background(), srv.URL, "job-cp-no-heuristic", workDir, "")

	argsData, _ := os.ReadFile(argsFile)
	if strings.Contains(string(argsData), "heuristic-guessed-conv-id") {
		t.Fatalf("control-plane resume must NOT heuristically discover or bind unpinned conversation from dirty context, got args: %s", string(argsData))
	}
}

func TestHasEngineQuotaExhaustedError(t *testing.T) {
	// 1. Legitimate engine error lines with quota/usage exhaustion
	legitCases := [][]byte{
		[]byte("ActionRequiredError: You're out of usage.\n"),
		[]byte("[error] rate limit exceeded for model\n"),
		[]byte("error: quota exceeded on project\n"),
		[]byte("[fatal] RESOURCE_EXHAUSTED: quota limit reached\n"),
		[]byte("FATAL: Switch to Auto, or ask your admin to increase your limit\n"),
	}
	for i, c := range legitCases {
		if !hasEngineQuotaExhaustedError(c) {
			t.Fatalf("case %d: expected quota error detected for %q", i, string(c))
		}
	}

	// 2. Spool / LLM output / normal prose mentioning quota keywords must NOT trigger (Finding M3)
	falseCases := [][]byte{
		[]byte("The user asked about ActionRequiredError and how to resolve it.\n"),
		[]byte("I checked if the system is out of usage in previous logs.\n"),
		[]byte("docs/runbooks: when quota exceeded occurs, switch engine.\n"),
		[]byte("print('ActionRequiredError: not real error')\n"),
		[]byte(""),
	}
	for i, c := range falseCases {
		if hasEngineQuotaExhaustedError(c) {
			t.Fatalf("case %d: expected no quota error for narrative text %q", i, string(c))
		}
	}
}

func TestExecuteJob_DeterministicResume_SpoolQuotaKeyword_DoesNotSuppressStage2(t *testing.T) {
	mockHome := t.TempDir()
	t.Setenv("HOME", mockHome)
	_ = os.MkdirAll(filepath.Join(mockHome, ".gemini", "antigravity-cli", "conversations"), 0755)

	binDir := t.TempDir()
	agyBin := filepath.Join(binDir, "agy")
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

	runCountFile := filepath.Join(binDir, "run_count.txt")
	_ = os.WriteFile(runCountFile, []byte("0"), 0644)

	script := fmt.Sprintf(`#!/bin/sh
cnt=$(cat %s)
cnt=$((cnt + 1))
echo "$cnt" > %s

if [ "$cnt" -eq 1 ]; then
    PREV=""
    for arg in "$@"; do
        if [ "$PREV" = "--log-file" ]; then
            echo "[error] failed to attach to conversation: session corrupted" > "$arg"
        fi
        PREV="$arg"
    done
    echo "[error] failed to attach to conversation: session corrupted" >&2
    exit 1
fi

# Run 2 (Stage 2 Fallback): starts clean new conversation
PREV=""
for arg in "$@"; do
    if [ "$PREV" = "--log-file" ]; then
        echo "Created conversation 88888888-8888-8888-8888-888888888888" > "$arg"
    fi
    PREV="$arg"
done
echo "Created conversation 88888888-8888-8888-8888-888888888888"
exit 0
`, runCountFile, runCountFile)

	_ = os.WriteFile(agyBin, []byte(script), 0755)

	initialConvID := "11111111-1111-1111-1111-111111111111"
	workDir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(workDir, ".git"), 0755)

	// Pre-seed spool log with text containing "ActionRequiredError: You're out of usage."
	// In M3, this used to trick combinedOutput and suppress Stage 2 fallback!
	spoolLogPath := filepath.Join(spool.DefaultDir(), "job-spool-quota-test.log")
	_ = os.MkdirAll(spool.DefaultDir(), 0755)
	_ = os.WriteFile(spoolLogPath, []byte("Prompt text discussing ActionRequiredError: You're out of usage. How do I fix this?\n"), 0644)
	defer os.Remove(spoolLogPath)

	var recordedMode, recordedFallbackReason string
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/chunks") || strings.HasSuffix(r.URL.Path, "/exit") {
			w.WriteHeader(http.StatusOK)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/conversations") {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			if r, ok := body["role"].(string); ok && r == "fallback_primary" {
				recordedMode = "fallback_new_conversation"
				recordedFallbackReason, _ = body["fallback_reason"].(string)
			}
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.URL.Path == "/v1/jobs/job-spool-quota-test" {
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(controlplane.JobDetail{
				ID:                    "job-spool-quota-test",
				ParentJobID:           "job-parent-001",
				InitialConversationID: initialConvID,
				ResumeConversationID:  initialConvID,
				ActiveConversationID:  initialConvID,
				Kind:                  "resume",
				Engine:                "agy",
				Prompt:                "test prompt",
				WorktreePath:          workDir,
			})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	_ = executeJob(context.Background(), srv.URL, "job-spool-quota-test", workDir, "")

	finalCountBytes, _ := os.ReadFile(runCountFile)
	finalCount := strings.TrimSpace(string(finalCountBytes))
	if finalCount != "2" {
		t.Fatalf("expected Stage 2 fallback to run (run count 2), but got run count %s (spool keyword falsely suppressed Stage 2)", finalCount)
	}

	mu.Lock()
	mode := recordedMode
	reason := recordedFallbackReason
	mu.Unlock()

	if mode != "fallback_new_conversation" {
		t.Fatalf("expected fallback_new_conversation, got %q", mode)
	}
	if !strings.Contains(reason, "failed to attach") && !strings.Contains(reason, "failed to resume") {
		t.Fatalf("expected fallback reason mentioning failure to attach or resume, got %q", reason)
	}
}

func TestExecuteJob_DeterministicResume_ControlPlaneResume_ArgvOverrideIgnoredWhenEmptyParent(t *testing.T) {
	binDir := t.TempDir()
	agyBin := filepath.Join(binDir, "agy")
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

	argsFile := filepath.Join(binDir, "received_args.txt")
	script := fmt.Sprintf(`#!/bin/sh
echo "$@" > %s
exit 0
`, argsFile)
	_ = os.WriteFile(agyBin, []byte(script), 0755)

	workDir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(workDir, ".git"), 0755)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/chunks") || strings.HasSuffix(r.URL.Path, "/conversations") || strings.HasSuffix(r.URL.Path, "/exit") {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.URL.Path == "/v1/jobs/job-cp-override-test" {
			w.WriteHeader(http.StatusOK)
			// Parent has NO conversation ID registered
			_ = json.NewEncoder(w).Encode(controlplane.JobDetail{
				ID:           "job-cp-override-test",
				ParentJobID:  "job-parent-999",
				Kind:         "resume",
				Engine:       "agy",
				Prompt:       "resume prompt",
				WorktreePath: workDir,
			})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// Pass arbitrary foreign ID as argv override
	foreignLockID := "lock-foreign-conv-1234-uuid"
	_ = executeJob(context.Background(), srv.URL, "job-cp-override-test", workDir, "", "resume", foreignLockID)

	argsData, _ := os.ReadFile(argsFile)
	if strings.Contains(string(argsData), foreignLockID) {
		t.Fatalf("CP resume with empty parent conversation must NOT adopt argv override %s, got args: %s", foreignLockID, string(argsData))
	}
}

func TestPipedStdinHandlers(t *testing.T) {
	origStdin := os.Stdin
	defer func() { os.Stdin = origStdin }()

	// 0. Successful codebase repos discovery
	tmpRoot := t.TempDir()
	_ = os.WriteFile(filepath.Join(tmpRoot, ".gitmodules"), []byte("[submodule \"test-sub\"]\n\tpath = test-sub\n"), 0644)
	_ = os.MkdirAll(filepath.Join(tmpRoot, "test-sub"), 0755)
	t.Setenv("IAZIO_FLEET_ROOT", tmpRoot)
	if err := runCodebaseRepos([]string{"--json"}); err != nil {
		t.Errorf("unexpected error on runCodebaseRepos: %v", err)
	}

	// 1. Empty stdin for story create
	rEmpty, wEmpty, _ := os.Pipe()
	_ = wEmpty.Close()
	os.Stdin = rEmpty
	err := runStoryCreate([]string{"--json"})
	if err == nil || !strings.Contains(err.Error(), "empty stdin") {
		t.Errorf("expected empty stdin error, got %v", err)
	}

	// 2. Invalid json for story create
	rBad, wBad, _ := os.Pipe()
	_, _ = wBad.Write([]byte("{invalid-json"))
	_ = wBad.Close()
	os.Stdin = rBad
	err = runStoryCreate([]string{"--json"})
	if err == nil || !strings.Contains(err.Error(), "invalid story JSON") {
		t.Errorf("expected invalid story JSON error, got %v", err)
	}

	// 3. Empty stdin for prompt run
	rPEmpty, wPEmpty, _ := os.Pipe()
	_ = wPEmpty.Close()
	os.Stdin = rPEmpty
	err = runPromptRun([]string{"--json"})
	if err == nil || !strings.Contains(err.Error(), "empty stdin") {
		t.Errorf("expected empty stdin error for prompt, got %v", err)
	}

	// 4. Invalid json for prompt run
	rPBad, wPBad, _ := os.Pipe()
	_, _ = wPBad.Write([]byte("{invalid-json"))
	_ = wPBad.Close()
	os.Stdin = rPBad
	err = runPromptRun([]string{"--json"})
	if err == nil || !strings.Contains(err.Error(), "invalid prompt JSON") {
		t.Errorf("expected invalid prompt JSON error, got %v", err)
	}

	// 5. Empty stdin for codebase export
	rCEmpty, wCEmpty, _ := os.Pipe()
	_ = wCEmpty.Close()
	os.Stdin = rCEmpty
	err = runCodebaseExport([]string{"--json"})
	if err == nil || !strings.Contains(err.Error(), "empty stdin") {
		t.Errorf("expected empty stdin error for codebase export, got %v", err)
	}

	// 6. Invalid json for codebase export
	rCBad, wCBad, _ := os.Pipe()
	_, _ = wCBad.Write([]byte("{invalid-json"))
	_ = wCBad.Close()
	os.Stdin = rCBad
	err = runCodebaseExport([]string{"--json"})
	if err == nil || !strings.Contains(err.Error(), "invalid export JSON") {
		t.Errorf("expected invalid export JSON error, got %v", err)
	}

	// 7. Valid json for codebase export
	rCGood, wCGood, _ := os.Pipe()
	expData, _ := json.Marshal(map[string]any{
		"repo_name": "iazio-harness",
		"paths":     []string{"cmd/iazio-harness/main.go"},
	})
	_, _ = wCGood.Write(expData)
	_ = wCGood.Close()
	os.Stdin = rCGood
	_ = runCodebaseExport([]string{"--json"})

	// 8. Story create with valid payload (mock / non-empty)
	rSGood, wSGood, _ := os.Pipe()
	storyData, _ := json.Marshal(map[string]any{
		"story_id":  "STORY-GOV-9999",
		"title":     "Test Auto Story",
		"submodule": "test-sub",
	})
	_, _ = wSGood.Write(storyData)
	_ = wSGood.Close()
	os.Stdin = rSGood
	_ = runStoryCreate([]string{"--json"})

	// 9. Prompt run with valid payload
	rPGood, wPGood, _ := os.Pipe()
	promptData, _ := json.Marshal(map[string]any{
		"prompt":        "Do auto testing",
		"worktree_path": t.TempDir(),
	})
	_, _ = wPGood.Write(promptData)
	_ = wPGood.Close()
	os.Stdin = rPGood
	_ = runPromptRun([]string{"--json"})
}

func TestWriteErrorHelpers(t *testing.T) {
	if err := writeStoryError(nil); err != nil {
		t.Errorf("expected nil for nil error")
	}
	if err := writeStoryError(errors.New("test-story-err")); err == nil {
		t.Errorf("expected non-nil error")
	}

	if err := writePromptError(nil); err != nil {
		t.Errorf("expected nil for nil error")
	}
	if err := writePromptError(errors.New("test-prompt-err")); err == nil {
		t.Errorf("expected non-nil error")
	}

	if err := writeCodebaseError(nil); err != nil {
		t.Errorf("expected nil for nil error")
	}
	if err := writeCodebaseError(errors.New("test-codebase-err")); err == nil {
		t.Errorf("expected non-nil error")
	}

	// Subcommand usage and routing
	if err := runStoryCommand([]string{}); err == nil {
		t.Error("expected error for empty args")
	}
	if err := runStoryCommand([]string{"invalid"}); err == nil {
		t.Error("expected error for invalid story command")
	}
	if err := runStoryCreate([]string{}); err == nil {
		t.Error("expected error when --json missing")
	}

	if err := runPromptCommand([]string{}); err == nil {
		t.Error("expected error for empty args")
	}
	if err := runPromptCommand([]string{"invalid"}); err == nil {
		t.Error("expected error for invalid prompt command")
	}
	if err := runPromptRun([]string{}); err == nil {
		t.Error("expected error when --json missing")
	}

	if err := runCodebaseCommand([]string{}); err == nil {
		t.Error("expected error for empty args")
	}
	if err := runCodebaseCommand([]string{"invalid"}); err == nil {
		t.Error("expected error for invalid codebase command")
	}
	if err := runCodebaseRepos([]string{}); err == nil {
		t.Error("expected error when --json missing")
	}
	if err := runCodebaseExport([]string{}); err == nil {
		t.Error("expected error when --json missing")
	}

	// Top level run() usage and invalid
	if err := run([]string{}); err == nil {
		t.Error("expected error on empty run args")
	}
	if err := run([]string{"unknown-cmd"}); err == nil {
		t.Error("expected error on unknown command")
	}
	if err := run([]string{"version"}); err != nil {
		t.Errorf("unexpected error on version: %v", err)
	}
	_ = run([]string{"story"})
	_ = run([]string{"prompt"})
	_ = run([]string{"codebase"})

	tmpFleet := t.TempDir()
	_ = run([]string{"fleet-status", "--repo=" + tmpFleet, "--json"})
	_ = run([]string{"fleet-status", "--worktree-path", tmpFleet})
	_ = run([]string{"heal-ase", "--repo=" + tmpFleet})
	_ = run([]string{"heal-ase", "--worktree-path", tmpFleet})

	// Service commands dry-run and actions
	if err := run([]string{"service", "install", "--dry-run"}); err != nil {
		t.Errorf("unexpected error on service install --dry-run: %v", err)
	}
	if err := run([]string{"service", "dev-install", "--dry-run"}); err != nil {
		t.Errorf("unexpected error on service dev-install --dry-run: %v", err)
	}
	if err := run([]string{"service", "pause", "--dry-run"}); err != nil {
		t.Errorf("unexpected error on service pause --dry-run: %v", err)
	}
	if err := run([]string{"service", "resume", "--dry-run"}); err != nil {
		t.Errorf("unexpected error on service resume --dry-run: %v", err)
	}
	if err := run([]string{"service", "revive-check", "--dry-run"}); err != nil {
		t.Errorf("unexpected error on service revive-check --dry-run: %v", err)
	}
	if err := run([]string{"service", "status"}); err != nil {
		t.Errorf("unexpected error on service status: %v", err)
	}
	if err := run([]string{"service", "invalid-action"}); err == nil {
		t.Errorf("expected error on unknown service action")
	}
}
