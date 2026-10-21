package diagnostics

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestInspectRepoFleetStatus_CleanRepo(t *testing.T) {
	tmpDir := t.TempDir()

	// Initialize git repo
	cmd := exec.Command("git", "init", "-b", "main")
	cmd.Dir = tmpDir
	if err := cmd.Run(); err != nil {
		t.Fatalf("git init: %v", err)
	}

	exec.Command("git", "-C", tmpDir, "config", "user.email", "test@example.com").Run()
	exec.Command("git", "-C", tmpDir, "config", "user.name", "Test User").Run()

	testFile := filepath.Join(tmpDir, "README.md")
	os.WriteFile(testFile, []byte("# Test Repo\n"), 0644)

	exec.Command("git", "-C", tmpDir, "add", ".").Run()
	exec.Command("git", "-C", tmpDir, "commit", "-m", "init").Run()

	res, err := InspectRepoFleetStatus(context.Background(), tmpDir)
	if err != nil {
		t.Fatalf("InspectRepoFleetStatus error: %v", err)
	}

	if !res.IsClean {
		t.Errorf("expected clean repo, got modified=%v untracked=%v", res.ModifiedFiles, res.UntrackedFiles)
	}
	if res.Branch != "main" {
		t.Errorf("expected branch main, got %s", res.Branch)
	}
	if res.ASEState != "IDLE" {
		t.Errorf("expected ASEState IDLE, got %s", res.ASEState)
	}
	if !strings.Contains(res.DiagnosticSummary, "Working Tree: CLEAN") {
		t.Errorf("summary missing CLEAN: %s", res.DiagnosticSummary)
	}
}

func TestInspectRepoFleetStatus_DirtyAndLocked(t *testing.T) {
	tmpDir := t.TempDir()

	cmd := exec.Command("git", "init", "-b", "feature/story-1")
	cmd.Dir = tmpDir
	if err := cmd.Run(); err != nil {
		t.Fatalf("git init: %v", err)
	}

	exec.Command("git", "-C", tmpDir, "config", "user.email", "test@example.com").Run()
	exec.Command("git", "-C", tmpDir, "config", "user.name", "Test User").Run()

	testFile := filepath.Join(tmpDir, "file.txt")
	os.WriteFile(testFile, []byte("initial"), 0644)
	exec.Command("git", "-C", tmpDir, "add", ".").Run()
	exec.Command("git", "-C", tmpDir, "commit", "-m", "initial").Run()

	// Make dirty
	os.WriteFile(testFile, []byte("modified"), 0644)
	os.WriteFile(filepath.Join(tmpDir, "untracked.go"), []byte("package main"), 0644)

	// Add autopilot lock
	os.MkdirAll(filepath.Join(tmpDir, ".autopilot"), 0755)
	os.WriteFile(filepath.Join(tmpDir, ".autopilot", "lock"), []byte("running"), 0644)

	res, err := InspectRepoFleetStatus(context.Background(), tmpDir)
	if err != nil {
		t.Fatalf("InspectRepoFleetStatus error: %v", err)
	}

	if res.IsClean {
		t.Errorf("expected dirty repo, got clean")
	}
	if len(res.ModifiedFiles) == 0 {
		t.Errorf("expected modified files, got none")
	}
	if len(res.UntrackedFiles) == 0 {
		t.Errorf("expected untracked files, got none")
	}
	if res.ASEState != "ORPHANED" && res.ASEState != "RUNNING" {
		t.Errorf("expected RUNNING or ORPHANED due to lockfile, got %s", res.ASEState)
	}
	if res.LockDetails == "" {
		t.Errorf("expected lock details, got empty")
	}
}
