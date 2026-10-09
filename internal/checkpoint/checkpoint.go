// Package checkpoint persists and recovers prompt execution states across infrastructure issues.
package checkpoint

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// State represents the saved execution state of a prompt run.
type State struct {
	JobID          string `json:"job_id"`
	ScheduleID     string `json:"schedule_id"`
	Engine         string `json:"engine"`
	WorktreePath   string `json:"worktree_path"`
	ConversationID string `json:"conversation_id,omitempty"`
	StartedAt      string `json:"started_at"`
	UpdatedAt      string `json:"updated_at"`
	Status         string `json:"status"` // "RUNNING", "COMPLETED", "FAILED"
	ExitCode       int    `json:"exit_code"`
	PID            int    `json:"pid,omitempty"`
}

// CheckpointFileName is the file name stored under .iazio/ in the worktree.
const CheckpointFileName = "execution-checkpoint.json"

// Save writes the execution state to both the worktree's .iazio directory and the spool directory.
func Save(worktree, spoolDir string, state State) error {
	if state.UpdatedAt == "" {
		state.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}

	// 1. Save in worktree .iazio directory
	if worktree != "" {
		iazioDir := filepath.Join(worktree, ".iazio")
		if err := os.MkdirAll(iazioDir, 0o755); err == nil {
			target := filepath.Join(iazioDir, CheckpointFileName)
			_ = os.WriteFile(target, data, 0o644)
		}
	}

	// 2. Save in spool directory
	if spoolDir != "" && state.JobID != "" {
		if err := os.MkdirAll(spoolDir, 0o755); err == nil {
			target := filepath.Join(spoolDir, state.JobID+".checkpoint.json")
			_ = os.WriteFile(target, data, 0o644)
		}
	}

	return nil
}

// ReadWorktree reads the checkpoint from the worktree's .iazio/ directory.
func ReadWorktree(worktree string) (State, error) {
	path := filepath.Join(worktree, ".iazio", CheckpointFileName)
	return ReadFile(path)
}

// ReadFile parses a checkpoint state from a given path.
func ReadFile(path string) (State, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return State{}, err
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return State{}, err
	}
	return state, nil
}
