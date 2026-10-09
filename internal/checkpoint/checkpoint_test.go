package checkpoint

import (
	"path/filepath"
	"testing"
	"time"
)

func TestSaveAndReadCheckpoint(t *testing.T) {
	tempWorktree := t.TempDir()
	tempSpool := t.TempDir()

	state := State{
		JobID:          "job-test-123",
		ScheduleID:     "sch-456",
		Engine:         "agy",
		WorktreePath:   tempWorktree,
		ConversationID: "conv-abc-789",
		StartedAt:      time.Now().UTC().Format(time.RFC3339),
		Status:         "RUNNING",
		PID:            4242,
	}

	if err := Save(tempWorktree, tempSpool, state); err != nil {
		t.Fatalf("failed to save checkpoint: %v", err)
	}

	// Verify reading from worktree
	loaded, err := ReadWorktree(tempWorktree)
	if err != nil {
		t.Fatalf("failed to read worktree checkpoint: %v", err)
	}
	if loaded.JobID != state.JobID {
		t.Errorf("expected JobID %s, got %s", state.JobID, loaded.JobID)
	}
	if loaded.ConversationID != state.ConversationID {
		t.Errorf("expected ConversationID %s, got %s", state.ConversationID, loaded.ConversationID)
	}
	if loaded.PID != 4242 {
		t.Errorf("expected PID 4242, got %d", loaded.PID)
	}

	// Verify reading from spool
	spoolFile := filepath.Join(tempSpool, "job-test-123.checkpoint.json")
	spoolLoaded, err := ReadFile(spoolFile)
	if err != nil {
		t.Fatalf("failed to read spool checkpoint: %v", err)
	}
	if spoolLoaded.Engine != "agy" {
		t.Errorf("expected engine agy, got %s", spoolLoaded.Engine)
	}
}
