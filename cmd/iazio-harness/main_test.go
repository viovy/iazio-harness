package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

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

func TestRunVerifyReleaseCommand(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/agents/debian/tools", func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"host":                   "debian",
			"supported_capabilities": []string{"user_service_watchdog_v2", "deterministic_resume_4tuple_v1", "agy_conversation_pinning_v1"},
			"tools": []map[string]any{
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
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("GET /v1/jobs/job-1311", func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"id":                      "job-1311",
			"initial_conversation_id": "aad902c8-bda6-44c7-8a68-c468a1562399",
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("GET /v1/jobs/job-1313", func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"id":                      "job-1313",
			"parent_job_id":           "job-1311",
			"initial_conversation_id": "aad902c8-bda6-44c7-8a68-c468a1562399",
			"conversation_lineage": []map[string]any{
				{
					"conversation_id": "aad902c8-bda6-44c7-8a68-c468a1562399",
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
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	if err := runVerifyReleaseCommand([]string{"--api-url", server.URL, "--token", "test-token"}); err != nil {
		t.Fatalf("unexpected error running verify-release command: %v", err)
	}

	if err := run([]string{"verify-release", "--api-url", server.URL, "--token", "test-token"}); err != nil {
		t.Fatalf("unexpected error running verify-release via run: %v", err)
	}

	if err := run([]string{"verify-live-release", "--api-url", server.URL, "--token", "test-token"}); err != nil {
		t.Fatalf("unexpected error running verify-live-release via run: %v", err)
	}

	// Failure case
	if err := runVerifyReleaseCommand([]string{"--api-url", "http://127.0.0.1:1"}); err == nil {
		t.Fatal("expected error connecting to non-existent server, got nil")
	}
}

func TestCleanStaleGitLocks(t *testing.T) {
	tempWorkspace := t.TempDir()
	gitDir := filepath.Join(tempWorkspace, ".git")
	if err := os.MkdirAll(filepath.Join(gitDir, "modules", "sub1"), 0755); err != nil {
		t.Fatalf("failed to create git dir: %v", err)
	}

	rootLock := filepath.Join(gitDir, "index.lock")
	subLock := filepath.Join(gitDir, "modules", "sub1", "index.lock")
	if err := os.WriteFile(rootLock, []byte("root-lock"), 0644); err != nil {
		t.Fatalf("failed to write root lock: %v", err)
	}
	if err := os.WriteFile(subLock, []byte("sub-lock"), 0644); err != nil {
		t.Fatalf("failed to write sub lock: %v", err)
	}

	cleaned := cleanStaleGitLocks(tempWorkspace)
	if len(cleaned) != 2 {
		t.Fatalf("expected 2 cleaned locks, got: %v", cleaned)
	}

	if _, err := os.Stat(rootLock); !os.IsNotExist(err) {
		t.Errorf("expected root lock to be removed")
	}
	if _, err := os.Stat(subLock); !os.IsNotExist(err) {
		t.Errorf("expected submodule lock to be removed")
	}
}

func TestCleanStaleGitLocks_PreservesHeldLock(t *testing.T) {
	tempWorkspace := t.TempDir()
	gitDir := filepath.Join(tempWorkspace, ".git")
	if err := os.MkdirAll(gitDir, 0755); err != nil {
		t.Fatalf("failed to create git dir: %v", err)
	}

	rootLock := filepath.Join(gitDir, "index.lock")
	f, err := os.OpenFile(rootLock, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		t.Fatalf("failed to create root lock: %v", err)
	}
	defer f.Close()

	if !isLockFileHeld(rootLock) {
		t.Skip("environment cannot detect open file descriptor via /proc or lsof")
	}

	if !hasRemainingGitLocks(tempWorkspace) {
		t.Fatalf("expected hasRemainingGitLocks to be true when root lock exists")
	}

	cleaned := cleanStaleGitLocks(tempWorkspace)
	if len(cleaned) != 0 {
		t.Fatalf("expected 0 cleaned locks for held lock, got: %v", cleaned)
	}

	if _, err := os.Stat(rootLock); err != nil {
		t.Fatalf("expected held root lock to be preserved, got: %v", err)
	}
}

func TestSanitizeAlphanumeric_And_SanitizeStep(t *testing.T) {
	if got := sanitizeAlphanumeric("hello_WORLD-123.test !@#$%^&*()"); got != "hello_WORLD-123.test" {
		t.Fatalf("expected hello_WORLD-123.test, got: %s", got)
	}
	if got := sanitizeStep(nil); got != "0" {
		t.Fatalf("expected 0 for nil, got: %s", got)
	}
	if got := sanitizeStep("abc"); got != "0" {
		t.Fatalf("expected 0 for abc, got: %s", got)
	}
	if got := sanitizeStep("Phase 2: Step 4"); got != "24" {
		t.Fatalf("expected 24 for digits, got: %s", got)
	}
	if got := sanitizeStep(42); got != "42" {
		t.Fatalf("expected 42, got: %s", got)
	}
}

func TestCollectWorkspaceDirtyFiles(t *testing.T) {
	if got := collectWorkspaceDirtyFiles(""); got != nil {
		t.Fatalf("expected nil for empty worktree, got: %v", got)
	}
	if got := collectWorkspaceDirtyFiles("/non-existent-directory-xyz"); got != nil {
		t.Fatalf("expected nil for non-existent dir, got: %v", got)
	}

	tempDir := t.TempDir()
	cmd := exec.Command("git", "init", tempDir)
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to git init: %v", err)
	}

	untracked := filepath.Join(tempDir, "dirty.txt")
	if err := os.WriteFile(untracked, []byte("dirty content"), 0644); err != nil {
		t.Fatalf("failed to write dirty file: %v", err)
	}

	files := collectWorkspaceDirtyFiles(tempDir)
	if len(files) == 0 {
		t.Fatalf("expected dirty files, got empty list")
	}
}

func TestResolveGitDir_And_SubmoduleGitDir(t *testing.T) {
	tempDir := t.TempDir()
	gitDirFile := filepath.Join(tempDir, ".git")
	modulesDir := filepath.Join(tempDir, "sub_git_dir")
	if err := os.MkdirAll(modulesDir, 0755); err != nil {
		t.Fatalf("failed to create dir: %v", err)
	}

	if err := os.WriteFile(gitDirFile, []byte("gitdir: sub_git_dir\n"), 0644); err != nil {
		t.Fatalf("failed to write gitdir file: %v", err)
	}

	resolved, err := resolveGitDir(tempDir)
	if err != nil {
		t.Fatalf("resolveGitDir failed: %v", err)
	}
	if resolved != modulesDir {
		t.Fatalf("expected %s, got: %s", modulesDir, resolved)
	}

	// Test with remaining locks
	configLock := filepath.Join(modulesDir, "config.lock")
	if err := os.WriteFile(configLock, []byte("lock"), 0644); err != nil {
		t.Fatalf("failed to write lock: %v", err)
	}
	if !hasRemainingGitLocks(tempDir) {
		t.Fatalf("expected hasRemainingGitLocks to be true with config.lock")
	}
}

func TestHasRunningGitProcess(t *testing.T) {
	tempDir := t.TempDir()
	if hasRunningGitProcess(tempDir) {
		t.Fatalf("expected false for tempDir without git processes")
	}
}

func TestRunServiceCommand(t *testing.T) {
	// Status
	if err := runServiceCommand([]string{"status"}); err != nil {
		t.Fatalf("unexpected error running service status: %v", err)
	}

	// Dry run install
	_ = runServiceCommand([]string{"install", "--dry-run"})

	// Unknown action
	if err := runServiceCommand([]string{"unknown-action-xyz"}); err == nil {
		t.Fatalf("expected error for unknown action, got nil")
	}
}

func TestExecuteJob_AutoResume_HungBackgroundTask(t *testing.T) {
	mockDir := t.TempDir()
	countFile := filepath.Join(mockDir, "invocations.txt")
	argsFile := filepath.Join(mockDir, "args.txt")
	mockAgy := filepath.Join(mockDir, "agy")

	// Script simulates turn 1 hanging/reaped (outputs [background_task.auto_reaped] and exits 143),
	// and turn 2 resuming cleanly with exit 0.
	script := fmt.Sprintf(`#!/bin/sh
echo "run" >> "%s"
echo "$@" >> "%s"
RUNS=$(wc -l < "%s")
if [ "$RUNS" -eq 1 ]; then
  echo "Conversation ID: c0ffee00-1234-5678-9abc-def012345678"
  echo "[ASE-SUPERVISOR] [background_task.auto_reaped] Inactivity limit (60s) exceeded while root agent is idle. Terminating 1 background task(s) to unblock resume cycle..."
  exit 143
else
  echo "Resumed successfully and completed story flow."
  exit 0
fi
`, countFile, argsFile, countFile)

	_ = os.WriteFile(mockAgy, []byte(script), 0755)
	t.Setenv("PATH", mockDir+":"+os.Getenv("PATH"))

	workDir := t.TempDir()
	var postedExitCode int
	var postedChunks []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/chunks") {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if txt, ok := body["text"].(string); ok {
				postedChunks = append(postedChunks, txt)
			}
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/exit") {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if ec, ok := body["exit_code"].(float64); ok {
				postedExitCode = int(ec)
			}
			w.WriteHeader(http.StatusOK)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/conversations") {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.URL.Path == "/v1/jobs/job-reap-autoresume" {
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(controlplane.JobDetail{
				ID:           "job-reap-autoresume",
				Kind:         "ordinary",
				Engine:       "agy",
				Prompt:       "execute story flow",
				StoryID:      "STORY-REAP-001",
				WorktreePath: workDir,
			})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	err := executeJob(context.Background(), srv.URL, "job-reap-autoresume", workDir, "")
	if err != nil {
		t.Fatalf("executeJob failed: %v", err)
	}

	data, err := os.ReadFile(countFile)
	if err != nil {
		t.Fatalf("expected count file: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected exactly 2 executions (1 failed/reaped + 1 auto-resume), got %d executions", len(lines))
	}

	argsData, _ := os.ReadFile(argsFile)
	if !strings.Contains(string(argsData), "resume") {
		t.Fatalf("expected resume prompt in second invocation, got args:\n%s", string(argsData))
	}
	if postedExitCode != 0 {
		t.Fatalf("expected final posted exit code 0, got %d", postedExitCode)
	}
}

func TestExecuteJob_AttachRunningTool(t *testing.T) {
	workDir := t.TempDir()
	jobID := "job-attach-test"

	var postedChunks []string
	var postedExitCode int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/chunks") {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if txt, ok := body["text"].(string); ok {
				postedChunks = append(postedChunks, txt)
			}
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/ticks") {
			w.WriteHeader(http.StatusOK)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/exit") {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if ec, ok := body["exit_code"].(float64); ok {
				postedExitCode = int(ec)
			}
			w.WriteHeader(http.StatusOK)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/conversations") {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.URL.Path == "/v1/jobs/"+jobID {
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(controlplane.JobDetail{
				ID:           jobID,
				Kind:         "ordinary",
				Engine:       "agy",
				Prompt:       "test prompt",
				StoryID:      "STORY-ATTACH-001",
				WorktreePath: workDir,
			})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	mockDir := t.TempDir()
	mockAgy := filepath.Join(mockDir, "agy")
	_ = os.WriteFile(mockAgy, []byte("#!/bin/sh\nexit 0\n"), 0755)
	t.Setenv("PATH", mockDir+":"+os.Getenv("PATH"))

	cmd := exec.Command("sleep", "0.4")
	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start mock tool process: %v", err)
	}
	defer func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	}()
	go func() {
		_ = cmd.Wait()
	}()

	engineLogPath := filepath.Join(spool.DefaultDir(), jobID+".engine.log")
	_ = os.WriteFile(engineLogPath, []byte("Attached engine output line\n"), 0644)
	defer os.Remove(engineLogPath)

	err := executeJob(context.Background(), srv.URL, jobID, workDir, "", "ordinary", "c0ffee-attach", strconv.Itoa(cmd.Process.Pid))
	if err != nil {
		t.Fatalf("executeJob failed: %v", err)
	}

	var foundAttachMsg bool
	for _, chunk := range postedChunks {
		if strings.Contains(chunk, "[attach_mode]") {
			foundAttachMsg = true
			break
		}
	}
	if !foundAttachMsg {
		t.Fatalf("expected [attach_mode] chunk in postedChunks, got: %v", postedChunks)
	}
	_ = postedExitCode
}

func TestExecuteJob_AttachRunningTool_ContextCanceled(t *testing.T) {
	workDir := t.TempDir()
	jobID := "job-attach-cancel"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/chunks") || strings.HasSuffix(r.URL.Path, "/ticks") || strings.HasSuffix(r.URL.Path, "/conversations") {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.URL.Path == "/v1/jobs/"+jobID {
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(controlplane.JobDetail{
				ID:           jobID,
				Kind:         "ordinary",
				Engine:       "agy",
				Prompt:       "test prompt",
				StoryID:      "STORY-ATTACH-002",
				WorktreePath: workDir,
			})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cmd := exec.Command("sleep", "2")
	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start mock tool process: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(150*time.Millisecond, cancel)

	_ = executeJob(ctx, srv.URL, jobID, workDir, "", "ordinary", "c0ffee-cancel", strconv.Itoa(cmd.Process.Pid))
}

func TestRunJobWithContext_AttachToolPIDFlag(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	args := []string{"--api-url", "http://127.0.0.1:9999", "--job-id", "job-flag-test", "--attach-tool-pid", "12345"}
	_ = runJobWithContext(ctx, args)
}

func TestRunJobWithContext_Chunk(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	args := []string{"--api-url", srv.URL, "--job-id", "job-chunk", "--chunk", "hello world"}
	if err := runJobWithContext(context.Background(), args); err != nil {
		t.Fatalf("expected nil err, got %v", err)
	}
}

func TestRunServiceCommand_DryRun(t *testing.T) {
	_ = runServiceCommand([]string{"install", "--dry-run"})
	_ = runServiceCommand([]string{"dev-install", "--dry-run"})
	_ = runServiceCommand([]string{"pause", "--dry-run"})
	_ = runServiceCommand([]string{"resume", "--dry-run"})
	_ = runServiceCommand([]string{"revive", "--dry-run"})
	_ = runServiceCommand([]string{"uninstall", "--dry-run"})
	_ = runServiceCommand([]string{"unknown-action"})
}

func TestResolveLiveStoryID_Patterns(t *testing.T) {
	tempDir := t.TempDir()

	// 1. Output pattern STORY_ID=...
	out1 := "running task\nSTORY_ID=STORY-GOV-3691\nvalidating"
	got1 := resolveLiveStoryID(tempDir, out1, "pending_harness_creation")
	if got1 != "STORY-GOV-3691" {
		t.Errorf("expected STORY-GOV-3691 from explicit output, got %q", got1)
	}

	// 2. Output pattern docs/stories/...
	out2 := "created file docs/stories/2026-10-25-gov-3691-cleanup-legacy.story.md"
	got2 := resolveLiveStoryID(tempDir, out2, "pending_harness_creation")
	if got2 != "STORY-GOV-3691" {
		t.Errorf("expected STORY-GOV-3691 from story path, got %q", got2)
	}

	// 3. .autopilot/active_story.json
	apDir := filepath.Join(tempDir, ".autopilot")
	_ = os.MkdirAll(apDir, 0755)
	_ = os.WriteFile(filepath.Join(apDir, "active_story.json"), []byte(`{"story_id":"STORY-GOV-3691"}`), 0644)
	got3 := resolveLiveStoryID(tempDir, "", "pending_harness_creation")
	if got3 != "STORY-GOV-3691" {
		t.Errorf("expected STORY-GOV-3691 from .autopilot metadata, got %q", got3)
	}

	// 4. Git branch resolution
	gitDir := t.TempDir()
	cmdInit := exec.Command("git", "init", gitDir)
	_ = cmdInit.Run()
	cmdBranch := exec.Command("git", "-C", gitDir, "checkout", "-b", "chore/gov-3691-root-hygiene-wsl-decom")
	_ = cmdBranch.Run()
	got4 := resolveLiveStoryID(gitDir, "", "pending_harness_creation")
	if got4 != "STORY-GOV-3691" {
		t.Errorf("expected STORY-GOV-3691 from git branch, got %q", got4)
	}
}

func TestExecuteJob_AutoResume_EnginePrintTimeout(t *testing.T) {
	mockDir := t.TempDir()
	countFile := filepath.Join(mockDir, "invocations.txt")
	argsFile := filepath.Join(mockDir, "args.txt")
	mockAgy := filepath.Join(mockDir, "agy")

	// Script simulates turn 1 exiting code 0 with agy print timeout and terminated background task
	// turn 2 resuming cleanly with exit 0, creating a git commit and outputting valid telemetry
	script := fmt.Sprintf(`#!/bin/sh
echo "run" >> "%s"
echo "$@" >> "%s"
RUNS=$(wc -l < "%s")
if [ "$RUNS" -eq 1 ]; then
  echo "Conversation ID: c0ffee00-1234-5678-9abc-def012345678"
  echo "[agy] print timeout after 1h0m0s with turn in progress; returning partial output"
  echo "terminating 1 background task(s) on exit"
  exit 0
else
  echo "Resumed turn 2 cleanly"
  echo "line 2: executing story flow for STORY-GOV-3691"
  echo "line 3: verifying gates"
  echo "line 4: running checks"
  echo "line 5: story complete"
  echo "padding: $(head -c 300 /dev/zero | tr '\0' 'A')"
  echo "mutation" > mutation.txt
  git add mutation.txt
  git commit -m "feat: complete story mutation"
  exit 0
fi
`, countFile, argsFile, countFile)

	_ = os.WriteFile(mockAgy, []byte(script), 0755)
	t.Setenv("PATH", mockDir+":"+os.Getenv("PATH"))

	workDir := t.TempDir()
	_ = exec.Command("git", "init", workDir).Run()
	_ = exec.Command("git", "-C", workDir, "config", "user.name", "Test").Run()
	_ = exec.Command("git", "-C", workDir, "config", "user.email", "test@example.com").Run()
	_ = os.WriteFile(filepath.Join(workDir, "init.txt"), []byte("init"), 0644)
	_ = exec.Command("git", "-C", workDir, "add", ".").Run()
	_ = exec.Command("git", "-C", workDir, "commit", "-m", "init").Run()
	var postedExitCode int
	var postedChunks []string
	var postedBindings []map[string]any
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/chunks") {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if txt, ok := body["text"].(string); ok {
				mu.Lock()
				postedChunks = append(postedChunks, txt)
				mu.Unlock()
			}
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/story-binding") {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			postedBindings = append(postedBindings, body)
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/exit") {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if ec, ok := body["exit_code"].(float64); ok {
				postedExitCode = int(ec)
			}
			w.WriteHeader(http.StatusOK)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/conversations") {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.URL.Path == "/v1/jobs/job-print-timeout" {
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(controlplane.JobDetail{
				ID:           "job-print-timeout",
				Kind:         "story",
				Engine:       "agy",
				Prompt:       "/autonomous-story-execution implement STORY-GOV-3691",
				StoryID:      "STORY-GOV-3691",
				WorktreePath: workDir,
			})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	err := executeJob(context.Background(), srv.URL, "job-print-timeout", workDir, "")
	if err != nil {
		t.Fatalf("executeJob failed: %v", err)
	}

	data, err := os.ReadFile(countFile)
	if err != nil {
		t.Fatalf("expected count file: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected exactly 2 executions (turn 1 print timeout + turn 2 auto-continuation), got %d executions", len(lines))
	}

	argsData, _ := os.ReadFile(argsFile)
	if !strings.Contains(string(argsData), "resume") {
		t.Fatalf("expected resume prompt in continuation turn, got args:\n%s", string(argsData))
	}

	// Verify that turn continuation banner was logged
	foundContinuationBanner := false
	mu.Lock()
	for _, chunk := range postedChunks {
		if strings.Contains(chunk, "Initiating in-job continuation turn") {
			foundContinuationBanner = true
			break
		}
	}
	mu.Unlock()
	if !foundContinuationBanner {
		t.Fatalf("expected in-job continuation banner in posted chunks, got: %+v", postedChunks)
	}
	if postedExitCode != 0 {
		t.Fatalf("expected final exit code 0, got %d", postedExitCode)
	}
}

func TestExecuteJob_AutoResume_ExitCodeZeroWithUnmetGates(t *testing.T) {
	mockHome := t.TempDir()
	t.Setenv("HOME", mockHome)

	mockDir := t.TempDir()
	countFile := filepath.Join(mockDir, "invocations.txt")
	argsFile := filepath.Join(mockDir, "args.txt")
	mockAgy := filepath.Join(mockDir, "agy")

	workDir := t.TempDir()
	_ = exec.Command("git", "init", workDir).Run()
	_ = exec.Command("git", "-C", workDir, "config", "user.email", "test@test.local").Run()
	_ = exec.Command("git", "-C", workDir, "config", "user.name", "Test").Run()
	_ = os.WriteFile(filepath.Join(workDir, ".gitignore"), []byte("tmp/\n.autopilot/\n.iazio/\ninvocations.txt\nargs.txt\n"), 0644)
	_ = os.WriteFile(filepath.Join(workDir, "init.txt"), []byte("init"), 0644)
	storyDir := filepath.Join(workDir, "docs", "stories")
	_ = os.MkdirAll(storyDir, 0755)
	storyFile := filepath.Join(storyDir, "2026-10-25-gov-99999-test.story.md")
	_ = os.WriteFile(storyFile, []byte("---\nid: STORY-GOV-99999\nstatus: IN_PROGRESS\n---\n"), 0644)
	_ = exec.Command("git", "-C", workDir, "add", ".gitignore", "init.txt", "docs/").Run()
	_ = exec.Command("git", "-C", workDir, "commit", "-m", "initial commit").Run()

	dirtyFile := filepath.Join(workDir, "uncommitted.txt")

	script := fmt.Sprintf(`#!/bin/sh
echo "run" >> "%s"
echo "$@" >> "%s"
RUNS=$(wc -l < "%s")
if [ "$RUNS" -eq 1 ]; then
  echo "Conversation ID: 11112222-3333-4444-5555-666677778888"
  echo "modified file" >> "%s"
  exit 0
else
  rm -f "%s"
  printf -- "---\nid: STORY-GOV-99999\nstatus: DONE\n---\n" > "%s"
  git -C "%s" commit -am "mark story done" >/dev/null 2>&1
  echo "Turn 2 completed cleanly"
  exit 0
fi
`, countFile, argsFile, countFile, dirtyFile, dirtyFile, storyFile, workDir)

	_ = os.WriteFile(mockAgy, []byte(script), 0755)
	t.Setenv("PATH", mockDir+":"+os.Getenv("PATH"))

	var postedExitCode int
	var postedChunks []string
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/chunks") {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if txt, ok := body["text"].(string); ok {
				mu.Lock()
				postedChunks = append(postedChunks, txt)
				mu.Unlock()
			}
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/story-binding") {
			w.WriteHeader(http.StatusOK)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/exit") {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if ec, ok := body["exit_code"].(float64); ok {
				postedExitCode = int(ec)
			}
			w.WriteHeader(http.StatusOK)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/conversations") {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.URL.Path == "/v1/jobs/job-unmet-gates" {
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(controlplane.JobDetail{
				ID:           "job-unmet-gates",
				Kind:         "story",
				Engine:       "agy",
				Prompt:       "/autonomous-story-execution implement STORY-GOV-99999",
				StoryID:      "STORY-GOV-99999",
				WorktreePath: workDir,
			})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	err := executeJob(context.Background(), srv.URL, "job-unmet-gates", workDir, "")
	if err != nil {
		t.Fatalf("executeJob failed: %v", err)
	}

	data, err := os.ReadFile(countFile)
	if err != nil {
		t.Fatalf("reading count file: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 invocations (1 initial + 1 continuation), got %d: %v", len(lines), lines)
	}

	argsData, _ := os.ReadFile(argsFile)
	if !strings.Contains(string(argsData), "--conversation=11112222-3333-4444-5555-666677778888") {
		t.Fatalf("expected continuation invocation to bind conversation, got: %s", string(argsData))
	}
	if !strings.Contains(string(argsData), "resume — continue autonomous story execution") {
		t.Fatalf("expected continuation prompt in args, got: %s", string(argsData))
	}
	if postedExitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", postedExitCode)
	}
}

func TestCollectWorkspaceDirtyFiles_AndVerifyRelease(t *testing.T) {
	if files := collectWorkspaceDirtyFiles(""); files != nil {
		t.Errorf("expected nil for empty worktree")
	}

	tmpDir := t.TempDir()
	_ = exec.Command("git", "init", tmpDir).Run()
	_ = os.WriteFile(filepath.Join(tmpDir, "file.txt"), []byte("data"), 0644)
	dirty := collectWorkspaceDirtyFiles(tmpDir)
	if len(dirty) == 0 {
		t.Errorf("expected dirty files, got none")
	}

	err := runVerifyReleaseCommand([]string{"--api-url", "http://127.0.0.1:54321", "--token", "tok"})
	if err == nil {
		t.Errorf("expected error connecting to non-existent server")
	}
}

func TestExecuteJob_DeterministicResume_ConversationNotFound_StartsNewConversationWithASEPrompt(t *testing.T) {
	mockHome := t.TempDir()
	t.Setenv("HOME", mockHome)

	mockDir := t.TempDir()
	countFile := filepath.Join(mockDir, "invocations.txt")
	mockAgy := filepath.Join(mockDir, "agy")
	targetConvID := "110d238e-1851-4036-a4f8-c4633e56ae27"
	newConvID := "220e349f-2962-5147-b5f9-d5744f67bf38"

	_ = os.WriteFile(mockAgy, []byte(fmt.Sprintf(`#!/bin/sh
run_num=$(wc -l < "%s" 2>/dev/null || echo 0)
echo "run" >> "%s"
echo "$*" > "%s/run_${run_num}.txt"

if [ "$run_num" -eq 0 ]; then
  # Run 1: Resuming target conversation fails because conversation is not found
  echo 'warning: conversation "%s" not found' >&2
  exit 1
else
  # Run 2: Clean fallback starts new conversation
  while [ $# -gt 0 ]; do
    case "$1" in
      --log-file)
        shift
        echo "I1026 04:00:00.000000 1 server.go:1274] Created conversation %s" > "$1"
        ;;
    esac
    shift
  done
  echo "autonomous execution converged to green"
  exit 0
fi
`, countFile, countFile, mockDir, targetConvID, newConvID)), 0755)

	t.Setenv("PATH", mockDir+":"+os.Getenv("PATH"))

	workDir := t.TempDir()
	_ = exec.Command("git", "init", workDir).Run()

	var postMu sync.Mutex
	var postedConvs []map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/chunks") || strings.HasSuffix(r.URL.Path, "/ticks") || strings.HasSuffix(r.URL.Path, "/exit") {
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
		if r.URL.Path == "/v1/jobs/job-1461-ase-resume" {
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(controlplane.JobDetail{
				ID:                    "job-1461-ase-resume",
				Kind:                  "resume",
				Engine:                "agy",
				Prompt:                "resume",
				WorktreePath:          workDir,
				InitialConversationID: targetConvID,
				ResumeConversationID:  targetConvID,
				ParentJobID:           "job-1459",
			})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	err := executeJob(context.Background(), srv.URL, "job-1461-ase-resume", workDir, "")
	if err != nil {
		t.Fatalf("executeJob failed: %v", err)
	}

	data, err := os.ReadFile(countFile)
	if err != nil {
		t.Fatalf("expected count file: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected exactly 2 executions (run1 resume fail, run2 clean fallback), got %d executions: %v", len(lines), lines)
	}

	run1Data, err := os.ReadFile(filepath.Join(mockDir, "run_0.txt"))
	if err != nil {
		t.Fatalf("expected run_0.txt: %v", err)
	}
	run1Str := string(run1Data)

	run2Data, err := os.ReadFile(filepath.Join(mockDir, "run_1.txt"))
	if err != nil {
		t.Fatalf("expected run_1.txt: %v", err)
	}
	run2Str := string(run2Data)

	// 1. Run 1: attempt previous conversation with --conversation and --dangerously-skip-permissions
	if !strings.Contains(run1Str, "--conversation="+targetConvID) {
		t.Fatalf("expected run 1 to attempt target conversation, got: %s", run1Str)
	}
	if !strings.Contains(run1Str, "--dangerously-skip-permissions") {
		t.Fatalf("expected run 1 to include --dangerously-skip-permissions, got: %s", run1Str)
	}

	// 2. Run 2: start new conversation without --conversation, with --dangerously-skip-permissions, and with canonical ASE prompt
	if strings.Contains(run2Str, "--conversation="+targetConvID) {
		t.Fatalf("expected run 2 NOT to have target conversation, got: %s", run2Str)
	}
	if !strings.Contains(run2Str, "--dangerously-skip-permissions") {
		t.Fatalf("expected run 2 to include --dangerously-skip-permissions, got: %s", run2Str)
	}
	if !strings.Contains(run2Str, "resume and complete current /autonomous-story-execution flow") {
		t.Fatalf("expected run 2 to include canonical ASE prompt 'resume and complete current /autonomous-story-execution flow', got: %s", run2Str)
	}
}

func TestExecuteJob_OrdinaryPrompt_CleanExecution_MCPDaemonsNoHang(t *testing.T) {
	var postChunkTexts []string
	var postExitCode int
	var mu sync.Mutex

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/jobs/"):
			json.NewEncoder(w).Encode(controlplane.JobDetail{
				ID:          "job-1506",
				Prompt:      "update tools",
				Kind:        "ordinary",
				Engine:      "agy",
				Agent:       "agy",
				Status:      "RUNNING",
			})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/chunks"):
			var body struct {
				Text string `json:"text"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			postChunkTexts = append(postChunkTexts, body.Text)
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/ticks"):
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/exit"):
			var body struct {
				ExitCode int `json:"exit_code"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			postExitCode = body.ExitCode
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer ts.Close()

	worktree := t.TempDir()
	initCmd := exec.Command("git", "init", worktree)
	_ = initCmd.Run()

	mockDir := t.TempDir()
	mockAgy := filepath.Join(mockDir, "agy")
	script := `#!/bin/sh
# Spawn long-lived mock MCP servers that hold open stdout/stderr
sleep 30 &
sleep 30 &
echo "### Summary of Actions Completed (job-1506)"
echo "zero open SIRs, clean state."
exit 0
`
	if err := os.WriteFile(mockAgy, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", mockDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := executeJob(ctx, ts.URL, "job-1506", worktree, "", "ordinary", "", "")
	if err != nil {
		t.Fatalf("expected executeJob to succeed, got error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if postExitCode != 0 {
		t.Fatalf("expected exit code 0 reported to control plane, got %d", postExitCode)
	}

	allOutput := strings.Join(postChunkTexts, "\n")
	if strings.Contains(allOutput, "harness-job:") {
		t.Fatalf("expected output to NOT contain harness-job prefix, got: %s", allOutput)
	}
	if strings.Contains(allOutput, "STORY-RESUME-INPLACE") {
		t.Fatalf("expected ordinary prompt to NOT inject STORY-RESUME-INPLACE, got: %s", allOutput)
	}

	// Verify no duplicate [ASE-SUPERVISOR] Starting execution for job
	bannerCount := strings.Count(allOutput, "Starting execution for job job-1506")
	if bannerCount != 1 {
		t.Fatalf("expected exactly 1 occurrence of startup banner, got %d in: %s", bannerCount, allOutput)
	}
}

func TestFormatVersionAndHelpers(t *testing.T) {
	buildCommitSHA = "abcdef1234567890"
	buildTimeUTC = "2026-10-27T10:00:00Z"
	buildDirty = "1"
	version = "1.0.0"

	commit := resolveCommit()
	if commit != "abcdef1234567890" {
		t.Fatalf("unexpected commit: %s", commit)
	}

	bt := resolveBuildTime()
	if bt != "2026-10-27T10:00:00Z" {
		t.Fatalf("unexpected build time: %s", bt)
	}

	dirty := resolveDirty()
	if !dirty {
		t.Fatalf("expected dirty true")
	}

	jsonStr := formatVersionJSON()
	if !strings.Contains(jsonStr, "iazio-harness") || !strings.Contains(jsonStr, "abcdef1234567890") {
		t.Fatalf("unexpected formatVersionJSON: %s", jsonStr)
	}

	txtStr := formatVersion()
	if !strings.Contains(txtStr, "iazio-harness 1.0.0+abcdef123456") {
		t.Fatalf("unexpected formatVersion: %s", txtStr)
	}

	// Test fallback paths
	buildCommitSHA = ""
	buildTimeUTC = ""
	buildDirty = "false"
	if resolveDirty() {
		t.Fatalf("expected dirty false")
	}
	if resolveBuildTime() == "" {
		t.Fatalf("expected non-empty default build time")
	}
}

func TestExecuteJob_MissingStoryValidation_NoGitPollution(t *testing.T) {
	workDir := t.TempDir()
	// Initialize a clean git repository in workDir
	cmdInit := exec.Command("git", "init")
	cmdInit.Dir = workDir
	if err := cmdInit.Run(); err != nil {
		t.Fatalf("failed to git init in workDir: %v", err)
	}

	var exitOptionsPosted controlplane.ExitOptions
	var exitPosted bool
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/jobs/job-missing-12288":
			_ = json.NewEncoder(w).Encode(controlplane.JobDetail{
				ID:           "job-missing-12288",
				Kind:         "ase",
				Engine:       "agy",
				Prompt:       "Execute autonomous story flow for STORY-ROOT-12288 in .",
				StoryID:      "STORY-ROOT-12288",
				WorktreePath: workDir,
			})
			return
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/stories/STORY-ROOT-12288":
			http.Error(w, `{"error":"story not found"}`, http.StatusNotFound)
			return
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/exit"):
			exitPosted = true
			_ = json.NewDecoder(r.Body).Decode(&exitOptionsPosted)
			w.WriteHeader(http.StatusOK)
			return
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/readiness"):
			w.WriteHeader(http.StatusOK)
			return
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/chunks"):
			w.WriteHeader(http.StatusOK)
			return
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/ticks"):
			w.WriteHeader(http.StatusOK)
			return
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	t.Setenv("IAZIO_HARNESS_API_URL", srv.URL)

	err := executeJob(context.Background(), srv.URL, "job-missing-12288", workDir, "")
	if err == nil {
		t.Fatalf("expected executeJob to fail when story is missing, got nil")
	}

	mu.Lock()
	defer mu.Unlock()

	if !exitPosted {
		t.Fatalf("expected exit options to be posted to control plane")
	}
	if exitOptionsPosted.ExitCode != 1 {
		t.Errorf("expected ExitCode 1, got %d", exitOptionsPosted.ExitCode)
	}
	if exitOptionsPosted.DiagnosticCode != "STORY_NOT_FOUND" {
		t.Errorf("expected DiagnosticCode STORY_NOT_FOUND, got %q", exitOptionsPosted.DiagnosticCode)
	}
	if exitOptionsPosted.Reason != "STORY_NOT_FOUND" {
		t.Errorf("expected Reason STORY_NOT_FOUND, got %q", exitOptionsPosted.Reason)
	}
	if exitOptionsPosted.TerminationReasonCode != "STORY_NOT_FOUND" {
		t.Errorf("expected TerminationReasonCode STORY_NOT_FOUND, got %q", exitOptionsPosted.TerminationReasonCode)
	}
	if exitOptionsPosted.Resumable {
		t.Errorf("expected Resumable to be false for nonexistent story")
	}
	if len(exitOptionsPosted.WorkspaceDirtyFiles) > 0 {
		t.Errorf("expected zero workspace dirty files, got: %v", exitOptionsPosted.WorkspaceDirtyFiles)
	}

	// Verify working tree on disk was not polluted with dirty files
	cmdStatus := exec.Command("git", "status", "--porcelain")
	cmdStatus.Dir = workDir
	out, err := cmdStatus.Output()
	if err != nil {
		t.Fatalf("git status failed: %v", err)
	}
	if strings.TrimSpace(string(out)) != "" {
		t.Fatalf("git context polluted with dirty files: %s", string(out))
	}
}





