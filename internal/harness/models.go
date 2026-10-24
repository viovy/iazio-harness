// Package harness implements the iazio-harness SIR checkpoint remediation bridge,
// bounded self-correction loop, and HITL circuit breaker (ADR-0118).
package harness

import (
	"context"
	"errors"
	"time"
)

var (
	// ErrHITLSuspended is returned when stage retries exceed threshold and HITL intervention is required.
	ErrHITLSuspended = errors.New("execution suspended for human-in-the-loop (HITL) intervention")
	// ErrContextBudgetExceeded is returned when hydrated context exceeds the allowed token ceiling.
	ErrContextBudgetExceeded = errors.New("hydrated canonical context exceeds token budget ceiling")
)

// Constants for ADR-0118 token limits and retry thresholds.
const (
	MaxCodingGuardrailsTokens = 200
	MaxHydratedContextTokens  = 800
	DefaultMaxStageRetries    = 3
)

// SIREnvelope models the typed wire payload defined in ai/contracts/sir-envelope.schema.json.
type SIREnvelope struct {
	SchemaVersion            int                      `json:"schema_version"`
	Status                   string                   `json:"status"` // "SIR_BOUNDARY_HALT"
	StoryID                  string                   `json:"story_id"`
	Checkpoint               SIRCheckpoint            `json:"checkpoint"`
	Incident                 SIRIncident              `json:"incident"`
	HydratedCanonicalContext HydratedCanonicalContext `json:"hydrated_canonical_context"`
}

// SIRCheckpoint contains checkpoint resume information.
type SIRCheckpoint struct {
	ID              string   `json:"id"`
	CompletedStages []string `json:"completed_stages"`
	HaltedStage     string   `json:"halted_stage"`
	GitTreeSHA      string   `json:"git_tree_sha"`
	ResumeCmd       string   `json:"resume_cmd"`
}

// SIRIncident details the halt cause.
type SIRIncident struct {
	SIRCode    string   `json:"sir_code"`
	Summary    string   `json:"summary"`
	Locations  []string `json:"locations"`
	RawLogPath string   `json:"raw_log_path"`
}

// HydratedCanonicalContext contains token-budgeted rules and instincts for remediation.
type HydratedCanonicalContext struct {
	TokensInjected         int       `json:"tokens_injected"`
	Rules                  []SIRRule `json:"rules"`
	Instincts              []SIRInst `json:"instincts"`
	IazioMCPEscalationHint string    `json:"iazio_mcp_escalation_hint,omitempty"`
}

// SIRRule defines one canonical rule injected for remediation.
type SIRRule struct {
	ID          string `json:"id"`
	Source      string `json:"source"`
	Directive   string `json:"directive"`
	Remediation string `json:"remediation"`
	Tokens      int    `json:"tokens"`
}

// SIRInst defines one verified instinct injected for remediation.
type SIRInst struct {
	ID          string `json:"id"`
	MatchRegex  string `json:"match_regex"`
	Remediation string `json:"remediation"`
	Tokens      int    `json:"tokens"`
}

// HITLSuspend models the record defined in ai/contracts/hitl-suspend.schema.json.
type HITLSuspend struct {
	InterventionID string  `json:"intervention_id"`
	ResumeToken    string  `json:"resume_token"`
	Slug           string  `json:"slug,omitempty"`
	SIRType        string  `json:"sir_type,omitempty"`
	Status         string  `json:"status"` // "suspended"
	SuspendedAt    string  `json:"suspended_at"`
	ResumedAt      *string `json:"resumed_at,omitempty"`
	Reason         string  `json:"reason,omitempty"`
	ResumeCommand  string  `json:"resume_command,omitempty"`
	SIRPath        string  `json:"sir_path,omitempty"`
}

// BootstrapResult contains workspace provisioning details from autopilot story begin.
type BootstrapResult struct {
	StoryID          string `json:"story_id"`
	WorktreePath     string `json:"worktree_path"`
	CodingGuardrails string `json:"coding_guardrails"`
}

// RunResult represents the execution outcome of autopilot run or resume.
type RunResult struct {
	ExitCode int          `json:"exit_code"`
	Stdout   string       `json:"stdout"`
	Stderr   string       `json:"stderr"`
	SIREnv   *SIREnvelope `json:"sir_envelope,omitempty"`
}

// Executor drives the target coding LLM (cursor, opencode, antigravity) in a worktree.
type Executor interface {
	Execute(ctx context.Context, worktreePath string, prompt string) error
}

// AutopilotClient abstracts the deterministic autopilot SDLC toolchain.
type AutopilotClient interface {
	StoryBegin(ctx context.Context, storyID string) (*BootstrapResult, error)
	Run(ctx context.Context, worktreePath string) (*RunResult, error)
	Resume(ctx context.Context, worktreePath string, resumeCmd string) (*RunResult, error)
}

// HarnessEvent records progress transitions for telemetry and SSE streaming.
type HarnessEvent struct {
	Type       string    `json:"type"` // "stage_transition", "sir_halt", "token_telemetry", "hitl_suspend"
	JobID      string    `json:"job_id"`
	Stage      string    `json:"stage,omitempty"`
	Checkpoint string    `json:"checkpoint,omitempty"`
	Tokens     int       `json:"tokens,omitempty"`
	Duration   string    `json:"duration,omitempty"`
	Payload    any       `json:"payload,omitempty"`
	Timestamp  time.Time `json:"timestamp"`
}
