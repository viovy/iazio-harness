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
	raw, err := json.Marshal(map[string]string{
		"type": "OUTPUT_CHUNK", "stream": stream, "text": text,
	})
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
func PostConversation(ctx context.Context, baseURL, token, jobID, conversationID string) error {
	raw, err := json.Marshal(map[string]string{
		"conversation_id": conversationID,
	})
	if err != nil {
		return err
	}
	return doPostWithRetry(ctx, strings.TrimRight(baseURL, "/")+"/v1/jobs/"+jobID+"/conversations", token, raw, "conversation post")
}

// JobDetail carries the execution specification from iazio-harness-api.
type JobDetail struct {
	ID                          string            `json:"id"`
	ScheduleID                  string            `json:"schedule_id"`
	Kind                        string            `json:"kind"`
	Status                      string            `json:"status"`
	Engine                      string            `json:"engine"`
	Prompt                      string            `json:"prompt"`
	StoryID                     string            `json:"story_id"`
	SourceIdeaID                string            `json:"source_idea_id"`
	Transcript                  string            `json:"transcript"`
	WorktreePath                string            `json:"worktree_path"`
	DocsHubPath                 string            `json:"docs_hub_path"`
	EnvVars                     map[string]string `json:"env_vars"`
	MaxExecutionDurationSeconds int               `json:"max_execution_duration_seconds"`
	ResumeConversationID        string            `json:"resume_conversation_id,omitempty"`
	Model                       string            `json:"model,omitempty"`
	Agent                       string            `json:"agent,omitempty"`
	DisableSlashCommands        *bool             `json:"disable_slash_commands,omitempty"`
	HostID                      string            `json:"host_id,omitempty"`
	TargetHost                  string            `json:"target_host,omitempty"`
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
	ExitCode       int    `json:"exit_code"`
	Reason         string `json:"reason,omitempty"`
	ErrorMessage   string `json:"error_message,omitempty"`
	DiagnosticCode string `json:"diagnostic_code,omitempty"`
	Engine         string `json:"engine,omitempty"`
	Model          string `json:"model,omitempty"`
	Remediation    string `json:"remediation,omitempty"`
	ASEComplete    bool   `json:"ase_complete,omitempty"`
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

