package checkpoint

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSaveAndReadCheckpoint(t *testing.T) {
	tempWorktree := t.TempDir()
	tempSpool := t.TempDir()

	state := State{
		JobID:                    "job-test-123",
		ScheduleID:               "sch-456",
		Engine:                   "agy",
		WorktreePath:             tempWorktree,
		ConversationID:           "conv-abc-789",
		StoryID:                  "STORY-TEST-001",
		StepIndex:                2,
		StartedAt:                time.Now().UTC().Format(time.RFC3339),
		Status:                   "RUNNING",
		PID:                      4242,
		TerminationReasonCode:    "BACKGROUND_TASK_HANG",
		TerminationReasonMessage: "hung",
		Resumable:                true,
		WorkspaceDirtyFiles:      []string{"file1.go"},
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
	if loaded.StoryID != state.StoryID {
		t.Errorf("expected StoryID %s, got %s", state.StoryID, loaded.StoryID)
	}
	if loaded.StepIndex != state.StepIndex {
		t.Errorf("expected StepIndex %d, got %d", state.StepIndex, loaded.StepIndex)
	}
	if loaded.PID != 4242 {
		t.Errorf("expected PID 4242, got %d", loaded.PID)
	}
	if !loaded.Resumable {
		t.Errorf("expected resumable true")
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

	// Verify defense-in-depth .gitignore in worktree .iazio directory
	giPath := filepath.Join(tempWorktree, ".iazio", ".gitignore")
	rawGi, err := os.ReadFile(giPath)
	if err != nil {
		t.Fatalf("expected .iazio/.gitignore to exist: %v", err)
	}
	if string(rawGi) != "*\n" {
		t.Errorf("expected .iazio/.gitignore to contain '*\\n', got %q", string(rawGi))
	}
}

func TestReadFile_Errors(t *testing.T) {
	_, err := ReadFile("/nonexistent/path/ckpt.json")
	if err == nil {
		t.Errorf("expected error for nonexistent file")
	}

	badFile := filepath.Join(t.TempDir(), "bad.json")
	_ = os.WriteFile(badFile, []byte("invalid json"), 0o644)
	_, err = ReadFile(badFile)
	if err == nil {
		t.Errorf("expected error for invalid json")
	}
}

func TestSave_EmptyTargets(t *testing.T) {
	state := State{JobID: "j-1"}
	if err := Save("", "", state); err != nil {
		t.Errorf("expected nil error for empty targets, got %v", err)
	}
}
