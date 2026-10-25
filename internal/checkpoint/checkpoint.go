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
	JobID                    string   `json:"job_id"`
	ScheduleID               string   `json:"schedule_id"`
	Engine                   string   `json:"engine"`
	WorktreePath             string   `json:"worktree_path"`
	ConversationID           string   `json:"conversation_id,omitempty"`
	StoryID                  string   `json:"story_id,omitempty"`
	StepIndex                int      `json:"step_index,omitempty"`
	StartedAt                string   `json:"started_at"`
	UpdatedAt                string   `json:"updated_at"`
	Status                   string   `json:"status"` // "RUNNING", "COMPLETED", "FAILED"
	ExitCode                 int      `json:"exit_code"`
	PID                      int      `json:"pid,omitempty"`
	ASEComplete              bool     `json:"ase_complete,omitempty"`
	TerminationReasonCode    string   `json:"termination_reason_code,omitempty"`
	TerminationReasonMessage string   `json:"termination_reason_message,omitempty"`
	Resumable                bool     `json:"resumable"`
	NonResumableReason       string   `json:"non_resumable_reason,omitempty"`
	WorkspaceDirtyFiles      []string `json:"workspace_dirty_files,omitempty"`
	BackgroundTasks          any      `json:"background_tasks,omitempty"`
}

// CheckpointFileName is the file name stored under .iazio/ in the worktree.
const CheckpointFileName = "execution-checkpoint.json"

func atomicWriteFile(target string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(target)
	tmpFile, err := os.CreateTemp(dir, ".tmp-ckpt-*")
	if err != nil {
		return os.WriteFile(target, data, perm)
	}
	tmpName := tmpFile.Name()
	defer func() {
		_ = os.Remove(tmpName)
	}()
	if _, err := tmpFile.Write(data); err != nil {
		_ = tmpFile.Close()
		return err
	}
	if err := tmpFile.Sync(); err != nil {
		_ = tmpFile.Close()
		return err
	}
	if err := tmpFile.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return err
	}
	return os.Rename(tmpName, target)
}

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
			// Defense-in-depth: ensure .iazio/.gitignore exists so worktree is never dirtied
			giPath := filepath.Join(iazioDir, ".gitignore")
			if _, statErr := os.Stat(giPath); os.IsNotExist(statErr) {
				_ = os.WriteFile(giPath, []byte("*\n"), 0o644)
			}
			target := filepath.Join(iazioDir, CheckpointFileName)
			_ = atomicWriteFile(target, data, 0o644)
		}
	}

	// 2. Save in spool directory
	if spoolDir != "" && state.JobID != "" {
		if err := os.MkdirAll(spoolDir, 0o755); err == nil {
			target := filepath.Join(spoolDir, state.JobID+".checkpoint.json")
			_ = atomicWriteFile(target, data, 0o644)
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
