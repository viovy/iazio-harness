package idempotency

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// StepRequest carries the turn step data to register with iazio-api.
type StepRequest struct {
	ConversationID string         `json:"conversation_id"`
	SessionID      string         `json:"session_id,omitempty"`
	StepIndex      int            `json:"step_index"`
	Command        string         `json:"command"`
	Checksum       string         `json:"checksum,omitempty"`
	Status         string         `json:"status,omitempty"`
	Metadata       map[string]any `json:"metadata,omitempty"`
}

// StepResponse represents the result of the idempotency check/record.
type StepResponse struct {
	Recorded        bool           `json:"recorded"`
	AlreadyExecuted bool           `json:"already_executed"`
	Step            map[string]any `json:"step,omitempty"`
}

// ComputeChecksum calculates a deterministic SHA-256 hash for a step command and payload.
func ComputeChecksum(conversationID string, stepIndex int, command, payload string) string {
	h := sha256.New()
	_, _ = h.Write([]byte(fmt.Sprintf("%s:%d:%s:%s", conversationID, stepIndex, command, payload)))
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// Client interacts with the iazio-api step checksum endpoints.
type Client struct {
	BaseURL    string
	Token      string
	HTTPClient *http.Client
}

// NewClient returns a new idempotency client.
func NewClient(baseURL, token string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
		HTTPClient: &http.Client{
			Timeout: 15 * time.Second,
		},
	}
}

// CheckOrRecordStep sends a step to iazio-api. If the step was already executed,
// AlreadyExecuted will be true, indicating the caller should skip re-running side effects.
func (c *Client) CheckOrRecordStep(ctx context.Context, req StepRequest) (StepResponse, error) {
	if c == nil || c.BaseURL == "" {
		return StepResponse{Recorded: true, AlreadyExecuted: false}, nil
	}

	if req.Checksum == "" {
		req.Checksum = ComputeChecksum(req.ConversationID, req.StepIndex, req.Command, "")
	}
	if req.Status == "" {
		req.Status = "completed"
	}

	body, err := json.Marshal(req)
	if err != nil {
		return StepResponse{}, fmt.Errorf("marshal step request: %w", err)
	}

	endpoint := c.BaseURL + "/v1/steps/checksum"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return StepResponse{}, fmt.Errorf("create http request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.Token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.Token)
	}

	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return StepResponse{}, fmt.Errorf("post step checksum: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return StepResponse{}, fmt.Errorf("step checksum api error: status %d", resp.StatusCode)
	}

	var stepResp StepResponse
	if err := json.NewDecoder(resp.Body).Decode(&stepResp); err != nil {
		return StepResponse{}, fmt.Errorf("decode step response: %w", err)
	}

	return stepResp, nil
}
