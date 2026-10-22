package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/viovy/iazio-harness/internal/controlplane"
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
	mockDir := t.TempDir()
	mockAgent := filepath.Join(mockDir, "agent")
	_ = os.WriteFile(mockAgent, []byte("#!/bin/sh\nif [ \"$1\" = \"create-chat\" ]; then echo 'cursor-chat-123'; exit 0; fi\necho \"ActionRequiredError: You're out of usage.\"\nexit 1\n"), 0755)
	mockAgy := filepath.Join(mockDir, "agy")
	_ = os.WriteFile(mockAgy, []byte("#!/bin/sh\necho 'fallback agy execution success'\nexit 0\n"), 0755)

	t.Setenv("PATH", mockDir+":"+os.Getenv("PATH"))

	workDir := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/chunks") {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/conversations") {
			w.WriteHeader(http.StatusOK)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/exit") {
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
	// Fallback was activated: initial engine 'agent' fell back to 'agy'.
	// Because initial log contains ActionRequiredError, diagnostics flags the finding resulting in exit code 1.
	if err != nil && !strings.Contains(err.Error(), "engine agy") {
		t.Fatalf("expected fallback to agy to execute, got %v", err)
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
}
