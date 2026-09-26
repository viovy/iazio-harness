// Package controlplane posts harness output to the control plane.
package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("chunk post: %s", resp.Status)
	}
	return nil
}
