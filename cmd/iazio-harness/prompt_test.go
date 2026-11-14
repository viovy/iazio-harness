package main

import (
	"testing"

	"github.com/viovy/iazio-harness/internal/prompt"
)

func TestPopulatePromptPayloadModelFromEnv(t *testing.T) {
	t.Setenv("IAZIO_MODEL", "gemini-4-carbon-high")
	t.Setenv("IAZIO_EFFORT", "high")

	// Empty payload model/effort falls back to env vars.
	p := prompt.PromptPayload{PromptID: "p1", Prompt: "hello"}
	populatePromptPayloadModelFromEnv(&p)
	if p.Model != "gemini-4-carbon-high" || p.Effort != "high" {
		t.Fatalf("expected env fallback (gemini-4-carbon-high, high), got (%q, %q)", p.Model, p.Effort)
	}

	// Explicit payload model/effort is preserved over env vars.
	explicit := prompt.PromptPayload{
		PromptID: "p2",
		Prompt:   "hello",
		Model:    "gemini-4-hydrogen-high",
		Effort:   "xhigh",
	}
	populatePromptPayloadModelFromEnv(&explicit)
	if explicit.Model != "gemini-4-hydrogen-high" || explicit.Effort != "xhigh" {
		t.Fatalf("expected explicit model/effort preserved, got (%q, %q)", explicit.Model, explicit.Effort)
	}
}
