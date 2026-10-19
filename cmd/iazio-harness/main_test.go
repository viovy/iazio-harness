package main

import (
	"os"
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
}

func TestFormatVersion(t *testing.T) {
	v := formatVersion()
	if !strings.Contains(v, "iazio-harness") {
		t.Fatalf("expected iazio-harness version, got %s", v)
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
