// Package controlplane posts harness output to the control plane.
package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

var httpClient = &http.Client{Timeout: 30 * time.Second}

// PostChunk stores one stripped chunk. The raw spool stays on the runner.
func PostChunk(ctx context.Context, baseURL, token, jobID, stream, text string) error {
	raw, err := json.Marshal(map[string]string{
		"type": "OUTPUT_CHUNK", "stream": stream, "text": text,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(baseURL, "/")+"/v1/jobs/"+jobID+"/chunks", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("chunk post: %s", resp.Status)
	}
	return nil
}

// PostTick pulses an execution keepalive tick when the engine is silent.
func PostTick(ctx context.Context, baseURL, token, jobID string, silentForMs int64) error {
	raw, err := json.Marshal(map[string]any{
		"type":          "OUTPUT_TICK",
		"silent_for_ms": silentForMs,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(baseURL, "/")+"/v1/jobs/"+jobID+"/chunks", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("tick post: %s", resp.Status)
	}
	return nil
}

// PostConversation registers an early or streamed conversation ID with the control plane.
func PostConversation(ctx context.Context, baseURL, token, jobID, conversationID string) error {
	raw, err := json.Marshal(map[string]string{
		"conversation_id": conversationID,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(baseURL, "/")+"/v1/jobs/"+jobID+"/conversations", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("conversation post: %s", resp.Status)
	}
	return nil
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
}

// GetJob fetches the job definition from the control plane.
func GetJob(ctx context.Context, baseURL, token, jobID string) (JobDetail, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/v1/jobs/"+jobID, nil)
	if err != nil {
		return JobDetail{}, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return JobDetail{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return JobDetail{}, fmt.Errorf("get job: status %s", resp.Status)
	}
	var job JobDetail
	if err := json.NewDecoder(resp.Body).Decode(&job); err != nil {
		return JobDetail{}, fmt.Errorf("decode job: %w", err)
	}
	return job, nil
}

// PostExit reports the process exit code to the control plane.
func PostExit(ctx context.Context, baseURL, token, jobID string, exitCode int) error {
	raw, err := json.Marshal(map[string]int{"exit_code": exitCode})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(baseURL, "/")+"/v1/jobs/"+jobID+"/exit", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("exit post: %s", resp.Status)
	}
	return nil
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
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(baseURL, "/")+"/v1/jobs/"+jobID+"/story-draft", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("story draft post: %s", resp.Status)
	}
	return nil
}

