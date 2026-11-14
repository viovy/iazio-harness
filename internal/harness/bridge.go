// Package harness implements the iazio-harness SIR checkpoint remediation bridge,
// bounded self-correction loop, and HITL circuit breaker (ADR-0118).
package harness

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Bridge orchestrates the bounded self-correction loop between autopilot and coding executors.
type Bridge struct {
	Autopilot       AutopilotClient
	Executor        Executor
	ModelEscalator  func(stage string, retryCount int, sirCode string) string
	MaxStageRetries int
	OnHITLSuspend   func(ctx context.Context, suspend HITLSuspend) error
	OnEvent         func(ev HarnessEvent)
}

// DefaultStageModelEscalation selects the model tier for a stage based on retry count.
func DefaultStageModelEscalation(stage string, retryCount int, sirCode string) string {
	switch {
	case retryCount >= 2:
		return "gemini-4-hydrogen-high"
	case retryCount == 1:
		return "gemini-4-carbon-high"
	default:
		return "gemini-3.8-flash-high"
	}
}

func (b *Bridge) resolveStageModel(stage string, retryCount int, sirCode string) string {
	escalator := b.ModelEscalator
	if escalator == nil {
		escalator = DefaultStageModelEscalation
	}
	return strings.TrimSpace(escalator(stage, retryCount, sirCode))
}

func (b *Bridge) executeForStage(ctx context.Context, worktreePath, prompt, stage string, retryCount int, sirCode string) (string, error) {
	model := b.resolveStageModel(stage, retryCount, sirCode)
	if me, ok := b.Executor.(ModelExecutor); ok && model != "" {
		return model, me.ExecuteWithModel(ctx, worktreePath, prompt, model)
	}
	return model, b.Executor.Execute(ctx, worktreePath, prompt)
}

// NewBridge creates a new SIR Checkpoint Bridge.
func NewBridge(ap AutopilotClient, exec Executor) *Bridge {
	return &Bridge{
		Autopilot:       ap,
		Executor:        exec,
		MaxStageRetries: DefaultMaxStageRetries,
	}
}

// ExecuteStory runs the complete lifecycle:
// 1. autopilot story begin
// 2. Initial coding executor pass
// 3. autopilot run
// 4. SIR interception -> JIT remediation -> autopilot run --resume <ckpt_id>
// 5. HITL circuit breaker when retries exceed threshold.
func (b *Bridge) ExecuteStory(ctx context.Context, jobID, storyID, initialPrompt string) error {
	if b.Autopilot == nil {
		return fmt.Errorf("autopilot client is required")
	}
	if b.Executor == nil {
		return fmt.Errorf("executor is required")
	}
	maxRetries := b.MaxStageRetries
	if maxRetries <= 0 {
		maxRetries = DefaultMaxStageRetries
	}

	// 1. Bootstrap: provision lease, worktree, and coding guardrails (<= 200 tokens)
	boot, err := b.Autopilot.StoryBegin(ctx, storyID)
	if err != nil {
		return fmt.Errorf("autopilot story begin failed: %w", err)
	}
	b.emitEvent(HarnessEvent{
		Type:      "stage_transition",
		JobID:     jobID,
		Stage:     "story_begin",
		Timestamp: time.Now().UTC(),
		Payload: map[string]string{
			"story_id":      boot.StoryID,
			"worktree_path": boot.WorktreePath,
		},
	})

	// 2. Initial Coding Pass
	fullPrompt := initialPrompt
	if boot.CodingGuardrails != "" {
		fullPrompt = fmt.Sprintf("%s\n\n### Coding Guardrails\n%s", initialPrompt, boot.CodingGuardrails)
	}
	activeModel, err := b.executeForStage(ctx, boot.WorktreePath, fullPrompt, "initial_coding_pass", 0, "")
	if err != nil {
		return fmt.Errorf("initial coding executor failed: %w", err)
	}

	var initialPayload any
	if activeModel != "" {
		initialPayload = map[string]any{"model": activeModel}
	}
	b.emitEvent(HarnessEvent{
		Type:      "stage_transition",
		JobID:     jobID,
		Stage:     "initial_coding_pass",
		Payload:   initialPayload,
		Timestamp: time.Now().UTC(),
	})

	// Run deterministic SDLC verification
	runRes, err := b.Autopilot.Run(ctx, boot.WorktreePath)
	if err != nil && runRes == nil {
		return fmt.Errorf("autopilot run failed: %w", err)
	}

	// 3. SIR Interception & Bounded Checkpoint Remediation Loop
	stageHaltCounts := make(map[string]int)

	for runRes != nil && runRes.SIREnv != nil && runRes.SIREnv.Status == "SIR_BOUNDARY_HALT" {
		sir := runRes.SIREnv
		haltedStage := sir.Checkpoint.HaltedStage
		ckptID := sir.Checkpoint.ID

		stageHaltCounts[haltedStage]++
		curRetries := stageHaltCounts[haltedStage]
		remediationModel := b.resolveStageModel(haltedStage, curRetries, sir.Incident.SIRCode)

		sirPayload := map[string]any{
			"sir_code":    sir.Incident.SIRCode,
			"summary":     sir.Incident.Summary,
			"retry_count": curRetries,
			"resume_cmd":  sir.Checkpoint.ResumeCmd,
		}
		if remediationModel != "" {
			sirPayload["model"] = remediationModel
		}
		b.emitEvent(HarnessEvent{
			Type:       "sir_halt",
			JobID:      jobID,
			Stage:      haltedStage,
			Checkpoint: ckptID,
			Timestamp:  time.Now().UTC(),
			Payload:    sirPayload,
		})

		// 5. HITL Circuit Breaker: suspend if halted stage retries exceed threshold
		if curRetries > maxRetries {
			suspendRecord := HITLSuspend{
				InterventionID: fmt.Sprintf("hitl-%s-%s-%d", storyID, haltedStage, time.Now().Unix()),
				ResumeToken:    generateResumeToken(),
				Slug:           storyID,
				SIRType:        sir.Incident.SIRCode,
				Status:         "suspended",
				SuspendedAt:    time.Now().UTC().Format(time.RFC3339),
				Reason:         fmt.Sprintf("Stage %q halted %d times exceeding max threshold %d: %s", haltedStage, curRetries, maxRetries, sir.Incident.Summary),
				ResumeCommand:  sir.Checkpoint.ResumeCmd,
				SIRPath:        sir.Incident.RawLogPath,
			}

			b.emitEvent(HarnessEvent{
				Type:       "hitl_suspend",
				JobID:      jobID,
				Stage:      haltedStage,
				Checkpoint: ckptID,
				Timestamp:  time.Now().UTC(),
				Payload:    suspendRecord,
			})

			if b.OnHITLSuspend != nil {
				_ = b.OnHITLSuspend(ctx, suspendRecord)
			}

			return fmt.Errorf("%w: stage %q reached %d halts", ErrHITLSuspended, haltedStage, curRetries)
		}

		// Enforce context budget constraint (<= 800 tokens)
		if sir.HydratedCanonicalContext.TokensInjected > MaxHydratedContextTokens {
			return fmt.Errorf("%w: tokens %d exceeds max %d",
				ErrContextBudgetExceeded, sir.HydratedCanonicalContext.TokensInjected, MaxHydratedContextTokens)
		}

		// Build isolated JIT remediation prompt — strictly passing only summary, locations, and hydrated context
		remediationPrompt := BuildRemediationPrompt(sir)

		// Dispatch coding executor with hydrated SIR context only
		usedModel, err := b.executeForStage(ctx, boot.WorktreePath, remediationPrompt, haltedStage, curRetries, sir.Incident.SIRCode)
		if err != nil {
			return fmt.Errorf("executor remediation pass failed: %w", err)
		}
		if usedModel != "" {
			activeModel = usedModel
		}

		// 4. Checkpoint Resume
		resumeCmd := sir.Checkpoint.ResumeCmd
		if resumeCmd == "" {
			resumeCmd = fmt.Sprintf("autopilot run --resume %s", ckptID)
		}

		runRes, err = b.Autopilot.Resume(ctx, boot.WorktreePath, resumeCmd)
		if err != nil && runRes == nil {
			return fmt.Errorf("autopilot resume failed: %w", err)
		}
	}

	var completedPayload any
	if activeModel != "" {
		completedPayload = map[string]any{"model": activeModel}
	}
	b.emitEvent(HarnessEvent{
		Type:      "stage_transition",
		JobID:     jobID,
		Stage:     "completed",
		Payload:   completedPayload,
		Timestamp: time.Now().UTC(),
	})

	return nil
}

// BuildRemediationPrompt constructs the token-budgeted prompt for the coding executor.
// It explicitly excludes raw log dumps from tmp/logs/.
func BuildRemediationPrompt(sir *SIREnvelope) string {
	var sb strings.Builder
	sb.WriteString("### SIR BOUNDARY HALT REMEDIATION\n")
	sb.WriteString(fmt.Sprintf("**Halted Stage:** %s\n", sir.Checkpoint.HaltedStage))
	sb.WriteString(fmt.Sprintf("**Checkpoint ID:** %s\n", sir.Checkpoint.ID))
	sb.WriteString(fmt.Sprintf("**SIR Code:** %s\n\n", sir.Incident.SIRCode))

	sb.WriteString("#### Incident Summary\n")
	sb.WriteString(sir.Incident.Summary)
	sb.WriteString("\n\n")

	if len(sir.Incident.Locations) > 0 {
		sb.WriteString("#### Problem Locations\n")
		for _, loc := range sir.Incident.Locations {
			sb.WriteString(fmt.Sprintf("- %s\n", loc))
		}
		sb.WriteString("\n")
	}

	if len(sir.HydratedCanonicalContext.Rules) > 0 {
		sb.WriteString("#### Canonical Rules\n")
		for _, r := range sir.HydratedCanonicalContext.Rules {
			sb.WriteString(fmt.Sprintf("- **%s** (%s): %s -> Remediation: %s\n", r.ID, r.Source, r.Directive, r.Remediation))
		}
		sb.WriteString("\n")
	}

	if len(sir.HydratedCanonicalContext.Instincts) > 0 {
		sb.WriteString("#### Verified Learned Instincts\n")
		for _, inst := range sir.HydratedCanonicalContext.Instincts {
			sb.WriteString(fmt.Sprintf("- **%s** (`%s`): %s\n", inst.ID, inst.MatchRegex, inst.Remediation))
		}
		sb.WriteString("\n")
	}

	if sir.HydratedCanonicalContext.IazioMCPEscalationHint != "" {
		sb.WriteString(fmt.Sprintf("#### Memory Escalation Hint: %s\n\n", sir.HydratedCanonicalContext.IazioMCPEscalationHint))
	}

	sb.WriteString("Please fix the root causes in the above locations adhering strictly to the canonical rules, and prepare for checkpoint resumption.\n")
	return sb.String()
}

func (b *Bridge) emitEvent(ev HarnessEvent) {
	if b.OnEvent != nil {
		b.OnEvent(ev)
	}
}

func generateResumeToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ParseSIREnvelope attempts to unmarshal a raw stdout string into a SIREnvelope.
func ParseSIREnvelope(stdout string) (*SIREnvelope, error) {
	trimmed := strings.TrimSpace(stdout)
	if !strings.Contains(trimmed, "SIR_BOUNDARY_HALT") {
		return nil, nil
	}
	// Locate JSON block if wrapped in diagnostics
	start := strings.Index(trimmed, "{")
	end := strings.LastIndex(trimmed, "}")
	if start >= 0 && end > start {
		trimmed = trimmed[start : end+1]
	}
	var env SIREnvelope
	if err := json.Unmarshal([]byte(trimmed), &env); err != nil {
		return nil, err
	}
	if env.Status != "SIR_BOUNDARY_HALT" {
		return nil, nil
	}
	return &env, nil
}
