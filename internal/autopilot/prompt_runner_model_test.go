package autopilot

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResolvePromptExecutionModel_TaskClassAndResumeLadders(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name          string
		opts          RunPromptOptions
		resumeAttempt int
		wantModel     string
		wantEffort    string
	}{
		{
			name: "single leaf initial run picks flash-high",
			opts: RunPromptOptions{
				Repo:   "iazio-harness",
				Prompt: "fix unit test",
			},
			resumeAttempt: 0,
			wantModel:     "gemini-3.8-flash-high",
			wantEffort:    "high",
		},
		{
			name: "meta-repo initial run picks carbon-high",
			opts: RunPromptOptions{
				Repo:   "meta-repo",
				Prompt: "update governance docs",
			},
			resumeAttempt: 0,
			wantModel:     "gemini-4-carbon-high",
			wantEffort:    "high",
		},
		{
			name: "dot repo initial run picks carbon-high",
			opts: RunPromptOptions{
				Repo:   ".",
				Prompt: "root task",
			},
			resumeAttempt: 0,
			wantModel:     "gemini-4-carbon-high",
			wantEffort:    "high",
		},
		{
			name: "empty repo initial run picks carbon-high",
			opts: RunPromptOptions{
				Repo:   "",
				Prompt: "root task",
			},
			resumeAttempt: 0,
			wantModel:     "gemini-4-carbon-high",
			wantEffort:    "high",
		},
		{
			name: "multi-repo submodules prompt picks carbon-high",
			opts: RunPromptOptions{
				Repo:   "iazio-harness",
				Prompt: "coordinate across submodules for release",
			},
			resumeAttempt: 0,
			wantModel:     "gemini-4-carbon-high",
			wantEffort:    "high",
		},
		{
			name: "resume attempt 1 picks carbon-high",
			opts: RunPromptOptions{
				Repo:   "iazio-harness",
				Prompt: "fix unit test",
			},
			resumeAttempt: 1,
			wantModel:     "gemini-4-carbon-high",
			wantEffort:    "high",
		},
		{
			name: "resume attempt 2 picks hydrogen-high with xhigh effort",
			opts: RunPromptOptions{
				Repo:   "iazio-harness",
				Prompt: "fix unit test",
			},
			resumeAttempt: 2,
			wantModel:     "gemini-4-hydrogen-high",
			wantEffort:    "xhigh",
		},
		{
			name: "auto explicit model follows task-class ladder",
			opts: RunPromptOptions{
				Repo:   "iazio-harness",
				Prompt: "fix unit test",
				Model:  "auto",
			},
			resumeAttempt: 0,
			wantModel:     "gemini-3.8-flash-high",
			wantEffort:    "high",
		},
		{
			name: "explicit model and effort honored when untripped",
			opts: RunPromptOptions{
				Repo:   "iazio-harness",
				Prompt: "fix unit test",
				Model:  "gemini-4-argon-high",
				Effort: "MAX",
			},
			resumeAttempt: 0,
			wantModel:     "gemini-4-argon-high",
			wantEffort:    "max",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotModel, gotEffort := resolvePromptExecutionModelFromCache(tc.opts, tc.resumeAttempt, nil, now)
			if gotModel != tc.wantModel || gotEffort != tc.wantEffort {
				t.Fatalf("got (%q, %q), want (%q, %q)", gotModel, gotEffort, tc.wantModel, tc.wantEffort)
			}
		})
	}
}

func TestResolvePromptExecutionModel_QuotaCircuitCache(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	circuitPath := filepath.Join(dir, "model-quota-circuit.json")
	reviewCachePath := filepath.Join(dir, "review-model-cache.json")

	circuitJSON := `{
		"entries": {
			"agy:gemini-3.8-flash-high": {
				"tool": "agy",
				"model": "gemini-3.8-flash-high",
				"status": "quota-exceeded",
				"expires_at": "2026-11-14T13:00:00Z"
			},
			"cursor:gemini-4-carbon-medium": {
				"tool": "cursor",
				"model": "gemini-4-carbon-medium",
				"status": "disabled",
				"expires_at": null
			},
			"agy:gemini-4-carbon-high": {
				"tool": "agy",
				"model": "gemini-4-carbon-high",
				"status": "quota-exceeded",
				"expires_at": "2026-11-14T11:59:00Z"
			}
		}
	}`
	if err := os.WriteFile(circuitPath, []byte(circuitJSON), 0o600); err != nil {
		t.Fatalf("write circuit cache: %v", err)
	}

	reviewJSON := `{
		"models": {
			"agy:gemini-4-hydrogen-high": {
				"status": "disabled"
			}
		}
	}`
	if err := os.WriteFile(reviewCachePath, []byte(reviewJSON), 0o600); err != nil {
		t.Fatalf("write review cache: %v", err)
	}

	caches := []string{circuitPath, reviewCachePath}

	// Flash-high is tripped for agy, so single-leaf falls back to carbon-medium (cursor entry must not block agy).
	m, e := resolvePromptExecutionModelFromCache(RunPromptOptions{Repo: "iazio-harness", Prompt: "leaf task"}, 0, caches, now)
	if m != "gemini-4-carbon-medium" || e != "medium" {
		t.Fatalf("leaf fallback got (%q, %q), want (gemini-4-carbon-medium, medium)", m, e)
	}

	// Explicit model that is tripped must fall back down the ladder.
	m, e = resolvePromptExecutionModelFromCache(RunPromptOptions{
		Repo:   "iazio-harness",
		Prompt: "leaf task",
		Model:  "gemini-3.8-flash-high",
	}, 0, caches, now)
	if m != "gemini-4-carbon-medium" || e != "medium" {
		t.Fatalf("tripped explicit fallback got (%q, %q), want (gemini-4-carbon-medium, medium)", m, e)
	}

	// Expired carbon-high entry must NOT trip carbon-high.
	m, e = resolvePromptExecutionModelFromCache(RunPromptOptions{Repo: "iazio-harness", Prompt: "leaf task"}, 1, caches, now)
	if m != "gemini-4-carbon-high" || e != "high" {
		t.Fatalf("expired entry check got (%q, %q), want (gemini-4-carbon-high, high)", m, e)
	}

	// Hydrogen-high disabled in review-model-cache -> attempt >= 2 falls back to argon-high.
	m, e = resolvePromptExecutionModelFromCache(RunPromptOptions{Repo: "iazio-harness", Prompt: "leaf task"}, 2, caches, now)
	if m != "gemini-4-argon-high" || e != "high" {
		t.Fatalf("attempt 2 fallback got (%q, %q), want (gemini-4-argon-high, high)", m, e)
	}
}

func TestRunPrompt_MockAgyModelFlagsAndEnvPropagation(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	binDir := t.TempDir()
	mockBin := filepath.Join(binDir, "mock-agy")
	captureFile := filepath.Join(t.TempDir(), "captured.txt")

	script := `#!/bin/sh
echo "ARGV=$*" > "` + captureFile + `"
echo "AGY_MODEL=$AGY_MODEL" >> "` + captureFile + `"
echo "AGY_EFFORT=$AGY_EFFORT" >> "` + captureFile + `"
echo "Autonomous story execution is complete."
exit 0
`
	if err := os.WriteFile(mockBin, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock-agy: %v", err)
	}

	repoDir := initTestGitRepo(t)
	runner := NewPromptRunner("autopilot")

	// When opts.Model is empty on mock-agy: sets AGY_MODEL/AGY_EFFORT env, does not alter minimal mock-agy argv.
	_, err := runner.RunPrompt(context.Background(), RunPromptOptions{
		PromptID:    "prompt_model_default",
		Repo:        "iazio-harness",
		Prompt:      "run single leaf check",
		TargetAgent: mockBin,
		WorkingDir:  repoDir,
		Timeout:     5 * time.Second,
	})
	if err != nil {
		t.Fatalf("RunPrompt default failed: %v", err)
	}
	raw, err := os.ReadFile(captureFile)
	if err != nil {
		t.Fatalf("read captured default output: %v", err)
	}
	out := string(raw)
	if strings.Contains(out, "--model ") {
		t.Fatalf("expected minimal argv without --model when opts.Model is empty on mock-agy, got:\n%s", out)
	}
	if !strings.Contains(out, "AGY_MODEL=gemini-3.8-flash-high") || !strings.Contains(out, "AGY_EFFORT=high") {
		t.Fatalf("expected AGY_MODEL/AGY_EFFORT env vars, got:\n%s", out)
	}

	// When opts.Model is explicitly set on mock-agy: passes --model and --effort flags too.
	_, err = runner.RunPrompt(context.Background(), RunPromptOptions{
		PromptID:    "prompt_model_explicit",
		Repo:        "iazio-harness",
		Prompt:      "run explicit model check",
		TargetAgent: mockBin,
		Model:       "gemini-4-hydrogen-high",
		WorkingDir:  repoDir,
		Timeout:     5 * time.Second,
	})
	if err != nil {
		t.Fatalf("RunPrompt explicit failed: %v", err)
	}
	raw, err = os.ReadFile(captureFile)
	if err != nil {
		t.Fatalf("read captured explicit output: %v", err)
	}
	out = string(raw)
	if !strings.Contains(out, "--model gemini-4-hydrogen-high") || !strings.Contains(out, "--effort xhigh") {
		t.Fatalf("expected --model and --effort flags on explicit mock-agy run, got:\n%s", out)
	}
	if !strings.Contains(out, "AGY_MODEL=gemini-4-hydrogen-high") || !strings.Contains(out, "AGY_EFFORT=xhigh") {
		t.Fatalf("expected AGY_MODEL=gemini-4-hydrogen-high and AGY_EFFORT=xhigh, got:\n%s", out)
	}
}
