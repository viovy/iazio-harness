// Package controlplane posts harness output to the control plane.
package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

var httpClient = &http.Client{Timeout: 30 * time.Second}

// Default transient retry parameters for rolling restart / update resilience.
const (
	DefaultMaxRetries     = 3
	DefaultInitialBackoff = 100 * time.Millisecond
)

// IsTransientStatus reports whether a status code represents a temporary gateway or overload condition.
func IsTransientStatus(status int) bool {
	return status == http.StatusBadGateway ||
		status == http.StatusServiceUnavailable ||
		status == http.StatusGatewayTimeout ||
		status == http.StatusTooManyRequests
}

// IsTransientError reports whether an error represents a temporary network error or connection refused/reset.
func IsTransientError(err error) bool {
	if err == nil {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "eof")
}

func doPostWithRetry(ctx context.Context, url, token string, raw []byte, opName string) error {
	backoff := DefaultInitialBackoff
	var lastErr error
	for attempt := 0; attempt <= DefaultMaxRetries; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			lastErr = err
			if !IsTransientError(err) || attempt == DefaultMaxRetries {
				return lastErr
			}
		} else {
			_ = resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return nil
			}
			lastErr = fmt.Errorf("%s: %s", opName, resp.Status)
			if !IsTransientStatus(resp.StatusCode) || attempt == DefaultMaxRetries {
				return lastErr
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
	}
	return lastErr
}

// PostChunk stores one stripped chunk. The raw spool stays on the runner.
func PostChunk(ctx context.Context, baseURL, token, jobID, stream, text string) error {
	return PostChunkWithTelemetry(ctx, baseURL, token, jobID, stream, text, "")
}

// PostChunkWithTelemetry stores one stripped chunk with optional telemetry such as active child tasks.
func PostChunkWithTelemetry(ctx context.Context, baseURL, token, jobID, stream, text, activeChildTasks string) error {
	payload := map[string]string{
		"type": "OUTPUT_CHUNK", "stream": stream, "text": text,
	}
	if activeChildTasks != "" {
		payload["active_child_tasks"] = activeChildTasks
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return doPostWithRetry(ctx, strings.TrimRight(baseURL, "/")+"/v1/jobs/"+jobID+"/chunks", token, raw, "chunk post")
}

// PostTick pulses an execution keepalive tick when the engine is silent, optionally transmitting telemetry PIDs and elapsed time.
func PostTick(ctx context.Context, baseURL, token, jobID string, silentForMs int64, telemetryOpts ...int) error {
	payload := map[string]any{
		"type":          "OUTPUT_TICK",
		"silent_for_ms": silentForMs,
	}
	if len(telemetryOpts) > 0 && telemetryOpts[0] > 0 {
		payload["harness_pid"] = telemetryOpts[0]
	}
	if len(telemetryOpts) > 1 && telemetryOpts[1] > 0 {
		payload["tool_pid"] = telemetryOpts[1]
	}
	if len(telemetryOpts) > 2 {
		payload["elapsed_seconds"] = telemetryOpts[2]
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return doPostWithRetry(ctx, strings.TrimRight(baseURL, "/")+"/v1/jobs/"+jobID+"/chunks", token, raw, "tick post")
}

// PostConversation registers an early or streamed conversation ID with the control plane.
// PostConversationOptions specifies fallback and lineage flags when attaching a conversation.
type PostConversationOptions struct {
	Fallback          bool           `json:"fallback,omitempty"`
	FallbackReason    string         `json:"fallback_reason,omitempty"`
	Role              string         `json:"role,omitempty"`
	CLIArgs           string         `json:"cli_args,omitempty"`
	ASEPhase          string         `json:"ase_phase,omitempty"`
	RehydrationStatus map[string]any `json:"rehydration_status,omitempty"`
}

func PostConversation(ctx context.Context, baseURL, token, jobID, conversationID string) error {
	return PostConversationWithOptions(ctx, baseURL, token, jobID, conversationID, PostConversationOptions{})
}

// PostConversationWithOptions attaches a conversation with fallback and lineage options.
func PostConversationWithOptions(ctx context.Context, baseURL, token, jobID, conversationID string, opt PostConversationOptions) error {
	payload := map[string]any{
		"conversation_id": conversationID,
	}
	if opt.Fallback {
		payload["fallback"] = true
	}
	if opt.FallbackReason != "" {
		payload["fallback_reason"] = opt.FallbackReason
	}
	if opt.Role != "" {
		payload["role"] = opt.Role
	}
	if opt.CLIArgs != "" {
		payload["cli_args"] = opt.CLIArgs
	}
	if opt.ASEPhase != "" {
		payload["ase_phase"] = opt.ASEPhase
	}
	if opt.RehydrationStatus != nil {
		payload["rehydration_status"] = opt.RehydrationStatus
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return doPostWithRetry(ctx, strings.TrimRight(baseURL, "/")+"/v1/jobs/"+jobID+"/conversations", token, raw, "conversation post")
}

// PostJobReadiness reports workspace readiness, reap results, and rehydration status to iazio-harness-api.
func PostJobReadiness(ctx context.Context, baseURL, token, jobID string, reapResult, manifest, rehydrationStatus map[string]any) error {
	payload := map[string]any{}
	if reapResult != nil {
		payload["reap_result"] = reapResult
	}
	if manifest != nil {
		payload["workspace_state_manifest"] = manifest
	}
	if rehydrationStatus != nil {
		payload["rehydration_status"] = rehydrationStatus
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return doPostWithRetry(ctx, strings.TrimRight(baseURL, "/")+"/v1/jobs/"+jobID+"/readiness", token, raw, "readiness post")
}

// JobDetail carries the execution specification from iazio-harness-api.
type JobDetail struct {
	ID                          string                 `json:"id"`
	ScheduleID                  string                 `json:"schedule_id"`
	Kind                        string                 `json:"kind"`
	Status                      string                 `json:"status"`
	Engine                      string                 `json:"engine"`
	Prompt                      string                 `json:"prompt"`
	StoryID                     string                 `json:"story_id"`
	SourceIdeaID                string                 `json:"source_idea_id"`
	Transcript                  string                 `json:"transcript"`
	WorktreePath                string                 `json:"worktree_path"`
	DocsHubPath                 string                 `json:"docs_hub_path"`
	EnvVars                     map[string]string      `json:"env_vars"`
	MaxExecutionDurationSeconds int                    `json:"max_execution_duration_seconds"`
	ResumeConversationID        string                 `json:"resume_conversation_id,omitempty"`
	Model                       string                 `json:"model,omitempty"`
	Agent                       string                 `json:"agent,omitempty"`
	DisableSlashCommands        *bool                  `json:"disable_slash_commands,omitempty"`
	HostID                      string                 `json:"host_id,omitempty"`
	TargetHost                  string                 `json:"target_host,omitempty"`
	ParentJobID                 string                 `json:"parent_job_id,omitempty"`
	RootJobID                   string                 `json:"root_job_id,omitempty"`
	ExecutionAttempt            int                    `json:"execution_attempt,omitempty"`
	ExecutionChain              []string               `json:"execution_chain,omitempty"`
	InitialConversationID       string                 `json:"initial_conversation_id,omitempty"`
	ActiveConversationID        string                 `json:"active_conversation_id,omitempty"`
	ResumeMode                  string                 `json:"resume_mode,omitempty"`
	ResumeFallbackReason        string                 `json:"resume_fallback_reason,omitempty"`
	InterruptedASEState         map[string]any         `json:"interrupted_ase_state,omitempty"`
	ConversationIDs             []string               `json:"conversation_ids,omitempty"`
	Reason                      string                 `json:"reason,omitempty"`
	ExitCode                    int                    `json:"exit_code,omitempty"`
	TerminationReasonMessage    string                 `json:"termination_reason_message,omitempty"`
	FailureReasonSummary        string                 `json:"failure_reason_summary,omitempty"`
	FencingToken                int64                  `json:"fencing_token,omitempty"`
	AttemptHistory              []AttemptFailureRecord `json:"attempt_history,omitempty"`
}

// AttemptFailureRecord documents a single recorded attempt failure in history.
type AttemptFailureRecord struct {
	AttemptNumber   int       `json:"attempt_number"`
	Tool            string    `json:"tool,omitempty"`
	ExitCode        int       `json:"exit_code"`
	FailureCategory string    `json:"failure_category,omitempty"`
	Reason          string    `json:"reason,omitempty"`
	ErrorMessage    string    `json:"error_message,omitempty"`
	Timestamp       time.Time `json:"timestamp,omitempty"`
}

// GetJob fetches the job definition from the control plane.
func GetJob(ctx context.Context, baseURL, token, jobID string) (JobDetail, error) {
	url := strings.TrimRight(baseURL, "/") + "/v1/jobs/" + jobID
	backoff := DefaultInitialBackoff
	var lastErr error
	for attempt := 0; attempt <= DefaultMaxRetries; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return JobDetail{}, err
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			lastErr = err
			if !IsTransientError(err) || attempt == DefaultMaxRetries {
				return JobDetail{}, lastErr
			}
		} else {
			if resp.StatusCode == http.StatusOK {
				var job JobDetail
				decErr := json.NewDecoder(resp.Body).Decode(&job)
				_ = resp.Body.Close()
				if decErr != nil {
					return JobDetail{}, fmt.Errorf("decode job: %w", decErr)
				}
				return job, nil
			}
			_ = resp.Body.Close()
			lastErr = fmt.Errorf("get job: status %s", resp.Status)
			if !IsTransientStatus(resp.StatusCode) || attempt == DefaultMaxRetries {
				return JobDetail{}, lastErr
			}
		}
		select {
		case <-ctx.Done():
			return JobDetail{}, ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
	}
	return JobDetail{}, lastErr
}

// ExitOptions provides structured completion and failure metadata.
type ExitOptions struct {
	ExitCode                 int      `json:"exit_code"`
	Reason                   string   `json:"reason,omitempty"`
	ErrorMessage             string   `json:"error_message,omitempty"`
	DiagnosticCode           string   `json:"diagnostic_code,omitempty"`
	Engine                   string   `json:"engine,omitempty"`
	Model                    string   `json:"model,omitempty"`
	Remediation              string   `json:"remediation,omitempty"`
	ASEComplete              bool     `json:"ase_complete,omitempty"`
	TerminationReasonCode    string   `json:"termination_reason_code,omitempty"`
	TerminationReasonMessage string   `json:"termination_reason_message,omitempty"`
	Resumable                bool     `json:"resumable"`
	NonResumableReason       string   `json:"non_resumable_reason,omitempty"`
	WorkspaceDirtyFiles      []string `json:"workspace_dirty_files,omitempty"`
	BackgroundTasks          any      `json:"background_tasks,omitempty"`
}

// PostExit reports the process exit code to the control plane.
func PostExit(ctx context.Context, baseURL, token, jobID string, exitCode int) error {
	return PostExitWithOptions(ctx, baseURL, token, jobID, ExitOptions{ExitCode: exitCode})
}

// PostExitWithOptions reports detailed process exit status, error messages, and diagnostic codes.
func PostExitWithOptions(ctx context.Context, baseURL, token, jobID string, opts ExitOptions) error {
	raw, err := json.Marshal(opts)
	if err != nil {
		return err
	}
	return doPostWithRetry(ctx, strings.TrimRight(baseURL, "/")+"/v1/jobs/"+jobID+"/exit", token, raw, "exit post")
}

// PostStoryDraft posts the refined story draft to the control plane.
func PostStoryDraft(ctx context.Context, baseURL, token, jobID, storyID, body string) error {
	raw, err := json.Marshal(map[string]string{
		"story_id": storyID,
		"body":     body,
	})
	if err != nil {
		return err
	}
	return doPostWithRetry(ctx, strings.TrimRight(baseURL, "/")+"/v1/jobs/"+jobID+"/story-draft", token, raw, "story draft post")
}

// ToolActionSummary captures a structured action/outcome from the prior conversation.
type ToolActionSummary struct {
	Step    int    `json:"step"`
	Tool    string `json:"tool"`
	Action  string `json:"action"`
	Summary string `json:"summary"`
	Status  string `json:"status"`
}

// RehydrationBundle contains the synthesized context necessary to bootstrap a fallback conversation.
type RehydrationBundle struct {
	ConversationID         string              `json:"conversation_id"`
	JobID                  string              `json:"job_id,omitempty"`
	StoryID                string              `json:"story_id,omitempty"`
	StorySpecification     string              `json:"story_specification,omitempty"`
	AcceptanceCriteria     []string            `json:"acceptance_criteria,omitempty"`
	CompletedPhases        []string            `json:"completed_phases,omitempty"`
	InterruptedPhase       string              `json:"interrupted_phase,omitempty"`
	InterruptedStep        string              `json:"interrupted_step,omitempty"`
	RemainingChecklist     []string            `json:"remaining_checklist,omitempty"`
	LastToolActions        []ToolActionSummary `json:"last_tool_actions,omitempty"`
	WorkspaceStateManifest map[string]any      `json:"workspace_state_manifest,omitempty"`
	BootstrapPrompt        string              `json:"bootstrap_prompt"`
	GeneratedAt            time.Time           `json:"generated_at"`
}

// GetRehydrationBundle fetches the canonical transcript recovery bundle for a conversation from the control plane.
func GetRehydrationBundle(ctx context.Context, baseURL, token, conversationID string) (RehydrationBundle, error) {
	var bundle RehydrationBundle
	conversationID = strings.TrimSpace(conversationID)
	if conversationID == "" {
		return bundle, fmt.Errorf("conversationID is required")
	}
	url := strings.TrimRight(baseURL, "/") + "/api/v1/conversations/" + conversationID + "/rehydration-bundle"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return bundle, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return bundle, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return bundle, fmt.Errorf("get rehydration bundle status: %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&bundle); err != nil {
		return bundle, fmt.Errorf("decode rehydration bundle: %w", err)
	}
	return bundle, nil
}

// PostStoryBinding dynamically binds an allocated story ID and metadata to a running or historical job.
func PostStoryBinding(ctx context.Context, baseURL, token, jobID, storyID, storyTitle, storyPath string, fencingToken ...int64) error {
	storyID = strings.TrimSpace(storyID)
	if storyID == "" {
		return errors.New("story_id is required")
	}
	payload := map[string]any{
		"story_id":    storyID,
		"story_title": storyTitle,
		"story_path":  storyPath,
	}
	if len(fencingToken) > 0 && fencingToken[0] > 0 {
		payload["fencing_token"] = fencingToken[0]
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return doPostWithRetry(ctx, strings.TrimRight(baseURL, "/")+"/v1/jobs/"+jobID+"/story-binding", token, raw, "story binding post")
}

// PostTurnContinued emits a structured turn auto-continuation event to the control plane.
func PostTurnContinued(ctx context.Context, baseURL, token, jobID string, turn int, conversationID string, unmetGates []string, activeChildSummary string) error {
	payload := map[string]any{
		"type":               "TURN_AUTO_CONTINUED",
		"continuation_turn":  turn,
		"conversation_id":    conversationID,
		"unmet_gates":        unmetGates,
		"active_child_tasks": activeChildSummary,
		"continued_at":       time.Now().UTC(),
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return doPostWithRetry(ctx, strings.TrimRight(baseURL, "/")+"/v1/jobs/"+jobID+"/chunks", token, raw, "turn continued post")
}
