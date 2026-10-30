package harness

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

type mockAutopilot struct {
	mu           sync.Mutex
	storyBeginFn func(ctx context.Context, storyID string) (*BootstrapResult, error)
	runFn        func(ctx context.Context, worktreePath string) (*RunResult, error)
	resumeFn     func(ctx context.Context, worktreePath string, resumeCmd string) (*RunResult, error)
	resumeCalls  []string
}

func (m *mockAutopilot) StoryBegin(ctx context.Context, storyID string) (*BootstrapResult, error) {
	if m.storyBeginFn != nil {
		return m.storyBeginFn(ctx, storyID)
	}
	return &BootstrapResult{
		StoryID:          storyID,
		WorktreePath:     "/tmp/worktrees/" + storyID,
		CodingGuardrails: "Rule: Follow standard formatting.",
	}, nil
}

func (m *mockAutopilot) Run(ctx context.Context, worktreePath string) (*RunResult, error) {
	if m.runFn != nil {
		return m.runFn(ctx, worktreePath)
	}
	return &RunResult{ExitCode: 0}, nil
}

func (m *mockAutopilot) Resume(ctx context.Context, worktreePath string, resumeCmd string) (*RunResult, error) {
	m.mu.Lock()
	m.resumeCalls = append(m.resumeCalls, resumeCmd)
	m.mu.Unlock()
	if m.resumeFn != nil {
		return m.resumeFn(ctx, worktreePath, resumeCmd)
	}
	return &RunResult{ExitCode: 0}, nil
}

type mockExecutor struct {
	mu              sync.Mutex
	executeCalls    []string
	receivedPrompts []string
}

func (m *mockExecutor) Execute(ctx context.Context, worktreePath string, prompt string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.executeCalls = append(m.executeCalls, worktreePath)
	m.receivedPrompts = append(m.receivedPrompts, prompt)
	return nil
}

func sampleSIREnvelope(stage, ckptID string, tokens int) *SIREnvelope {
	return &SIREnvelope{
		SchemaVersion: 1,
		Status:        "SIR_BOUNDARY_HALT",
		StoryID:       "STORY-TEST-0148",
		Checkpoint: SIRCheckpoint{
			ID:              ckptID,
			CompletedStages: []string{"lint", "build"},
			HaltedStage:     stage,
			GitTreeSHA:      "abcdef1234567890abcdef1234567890abcdef12",
			ResumeCmd:       "autopilot run --resume " + ckptID,
		},
		Incident: SIRIncident{
			SIRCode:    "SIR_UNIT_TEST_FAILED",
			Summary:    "TestUserAuth failed with nil pointer dereference",
			Locations:  []string{"pkg/auth/auth.go:42", "pkg/auth/auth_test.go:88"},
			RawLogPath: "tmp/logs/build-failed-verbose-dump.log",
		},
		HydratedCanonicalContext: HydratedCanonicalContext{
			TokensInjected: tokens,
			Rules: []SIRRule{
				{
					ID:          "RULE-AUTH-01",
					Source:      "ai/rules/auth.md",
					Directive:   "Always check nil pointers before claims lookup",
					Remediation: "Add if user == nil check",
					Tokens:      45,
				},
			},
			Instincts: []SIRInst{
				{
					ID:          "INST-102",
					MatchRegex:  `nil pointer dereference.*auth\.go`,
					Remediation: "Guard auth session claims extraction",
					Tokens:      30,
				},
			},
			IazioMCPEscalationHint: "Query iazio-mcp retrieve_error_remediation with sir_code=SIR_UNIT_TEST_FAILED",
		},
	}
}

// TestSIRCheckpointResume verifies AC-2:
// On SIR_BOUNDARY_HALT at ckpt_01, iazio-harness passes only the hydrated SIR envelope
// (<= 800 tokens) to the coding executor and resumes via autopilot run --resume ckpt_01.
func TestSIRCheckpointResume(t *testing.T) {
	sirEnv := sampleSIREnvelope("unit_test", "ckpt_01", 150)

	var runCalled bool
	ap := &mockAutopilot{
		runFn: func(ctx context.Context, worktreePath string) (*RunResult, error) {
			if !runCalled {
				runCalled = true
				return &RunResult{
					ExitCode: 1,
					SIREnv:   sirEnv,
				}, nil
			}
			return &RunResult{ExitCode: 0}, nil
		},
		resumeFn: func(ctx context.Context, worktreePath string, resumeCmd string) (*RunResult, error) {
			// Resume succeeds with clean exit
			return &RunResult{ExitCode: 0}, nil
		},
	}

	exec := &mockExecutor{}
	var events []HarnessEvent
	var evMu sync.Mutex

	bridge := NewBridge(ap, exec)
	bridge.OnEvent = func(ev HarnessEvent) {
		evMu.Lock()
		events = append(events, ev)
		evMu.Unlock()
	}

	ctx := context.Background()
	err := bridge.ExecuteStory(ctx, "job-101", "STORY-TEST-0148", "Implement auth resilience")
	if err != nil {
		t.Fatalf("unexpected ExecuteStory error: %v", err)
	}

	// 1. Verify executor passes: initial pass + 1 remediation pass
	exec.mu.Lock()
	defer exec.mu.Unlock()
	if len(exec.executeCalls) != 2 {
		t.Fatalf("expected 2 executor calls (initial + remediation), got %d", len(exec.executeCalls))
	}

	remediationPrompt := exec.receivedPrompts[1]
	// 2. Verify remediation prompt includes incident summary, locations, rules, instincts
	if !strings.Contains(remediationPrompt, "TestUserAuth failed with nil pointer dereference") {
		t.Fatalf("remediation prompt missing incident summary: %s", remediationPrompt)
	}
	if !strings.Contains(remediationPrompt, "pkg/auth/auth.go:42") {
		t.Fatalf("remediation prompt missing location: %s", remediationPrompt)
	}
	if !strings.Contains(remediationPrompt, "RULE-AUTH-01") {
		t.Fatalf("remediation prompt missing rule: %s", remediationPrompt)
	}
	if !strings.Contains(remediationPrompt, "INST-102") {
		t.Fatalf("remediation prompt missing instinct: %s", remediationPrompt)
	}
	// 3. Verify remediation prompt NEVER includes raw log path dump
	if strings.Contains(remediationPrompt, "tmp/logs/build-failed-verbose-dump.log") {
		t.Fatalf("remediation prompt improperly leaked raw log path: %s", remediationPrompt)
	}

	// 4. Verify resume command was executed with ckpt_01
	ap.mu.Lock()
	defer ap.mu.Unlock()
	if len(ap.resumeCalls) != 1 || ap.resumeCalls[0] != "autopilot run --resume ckpt_01" {
		t.Fatalf("expected resume call 'autopilot run --resume ckpt_01', got: %v", ap.resumeCalls)
	}

	// 5. Verify events recorded
	evMu.Lock()
	defer evMu.Unlock()
	hasSIRHalt := false
	hasCompleted := false
	for _, ev := range events {
		if ev.Type == "sir_halt" && ev.Checkpoint == "ckpt_01" {
			hasSIRHalt = true
		}
		if ev.Type == "stage_transition" && ev.Stage == "completed" {
			hasCompleted = true
		}
	}
	if !hasSIRHalt {
		t.Fatalf("expected sir_halt event for ckpt_01")
	}
	if !hasCompleted {
		t.Fatalf("expected completed stage_transition event")
	}
}

// TestHITLSuspendOnMaxRetries verifies AC-2:
// After 3 consecutive halts at the same stage, iazio-harness suspends the job with
// a valid hitl-suspend.schema.json state and triggers the circuit breaker.
func TestHITLSuspendOnMaxRetries(t *testing.T) {
	stage := "unit_test"
	sirEnv := sampleSIREnvelope(stage, "ckpt_loop", 100)

	ap := &mockAutopilot{
		runFn: func(ctx context.Context, worktreePath string) (*RunResult, error) {
			return &RunResult{ExitCode: 1, SIREnv: sirEnv}, nil
		},
		resumeFn: func(ctx context.Context, worktreePath string, resumeCmd string) (*RunResult, error) {
			// Always halt again at the same stage to simulate an unresolvable failure
			return &RunResult{ExitCode: 1, SIREnv: sirEnv}, nil
		},
	}

	exec := &mockExecutor{}
	var suspendedRecord *HITLSuspend
	var suspendMu sync.Mutex

	bridge := NewBridge(ap, exec)
	bridge.MaxStageRetries = 3
	bridge.OnHITLSuspend = func(ctx context.Context, suspend HITLSuspend) error {
		suspendMu.Lock()
		suspendedRecord = &suspend
		suspendMu.Unlock()
		return nil
	}

	ctx := context.Background()
	err := bridge.ExecuteStory(ctx, "job-loop-breaker", "STORY-TEST-0148", "Fix bug")
	if err == nil {
		t.Fatalf("expected ErrHITLSuspended, got nil")
	}
	if !errors.Is(err, ErrHITLSuspended) {
		t.Fatalf("expected error wrapping ErrHITLSuspended, got: %v", err)
	}

	suspendMu.Lock()
	defer suspendMu.Unlock()
	if suspendedRecord == nil {
		t.Fatalf("expected OnHITLSuspend callback to be invoked with valid HITLSuspend record")
	}

	// Verify HITL Suspend schema fields compliance
	if suspendedRecord.Status != "suspended" {
		t.Fatalf("expected status 'suspended', got: %s", suspendedRecord.Status)
	}
	if suspendedRecord.InterventionID == "" {
		t.Fatalf("expected non-empty InterventionID")
	}
	if suspendedRecord.ResumeToken == "" || len(suspendedRecord.ResumeToken) < 8 {
		t.Fatalf("expected resume token >= 8 chars, got: %s", suspendedRecord.ResumeToken)
	}
	if suspendedRecord.ResumeCommand != "autopilot run --resume ckpt_loop" {
		t.Fatalf("expected resume command 'autopilot run --resume ckpt_loop', got: %s", suspendedRecord.ResumeCommand)
	}
	if !strings.Contains(suspendedRecord.Reason, "halted 4 times exceeding max threshold 3") {
		t.Fatalf("expected reason detailing stage halt count, got: %s", suspendedRecord.Reason)
	}
}

func TestContextBudgetExceeded(t *testing.T) {
	// Over the 800 token ceiling
	sirEnv := sampleSIREnvelope("unit_test", "ckpt_overflow", 850)

	ap := &mockAutopilot{
		runFn: func(ctx context.Context, worktreePath string) (*RunResult, error) {
			return &RunResult{ExitCode: 1, SIREnv: sirEnv}, nil
		},
	}
	exec := &mockExecutor{}
	bridge := NewBridge(ap, exec)

	err := bridge.ExecuteStory(context.Background(), "job-overflow", "STORY-TEST-0148", "Test")
	if err == nil || !errors.Is(err, ErrContextBudgetExceeded) {
		t.Fatalf("expected ErrContextBudgetExceeded, got: %v", err)
	}
}

func TestParseSIREnvelope(t *testing.T) {
	if env, err := ParseSIREnvelope("normal output"); err != nil || env != nil {
		t.Fatalf("expected nil for normal output")
	}
	raw := `prefix {"status": "SIR_BOUNDARY_HALT", "story_id": "test"} suffix`
	env, err := ParseSIREnvelope(raw)
	if err != nil || env == nil || env.StoryID != "test" {
		t.Fatalf("expected parsed envelope, got: %+v, err: %v", env, err)
	}
	invalidJSON := `prefix {"status": "SIR_BOUNDARY_HALT", broken json`
	if _, err := ParseSIREnvelope(invalidJSON); err == nil {
		t.Fatalf("expected error for broken JSON")
	}
	notHalt := `{"status": "OK"}`
	if env, err := ParseSIREnvelope(notHalt); err != nil || env != nil {
		t.Fatalf("expected nil for status!=SIR_BOUNDARY_HALT")
	}
}
