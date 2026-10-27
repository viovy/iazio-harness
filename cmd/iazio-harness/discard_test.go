package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"github.com/viovy/iazio-harness/internal/controlplane"
	"github.com/viovy/iazio-harness/internal/spool"
	"github.com/viovy/iazio-harness/internal/watchdog"
)

func initTestGitRepo(t *testing.T, dir string) {
	t.Helper()
	cmds := [][]string{
		{"git", "init"},
		{"git", "config", "user.name", "Test User"},
		{"git", "config", "user.email", "test@example.com"},
	}
	for _, c := range cmds {
		cmd := exec.Command(c[0], c[1:]...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("command %v failed in %s: %v (%s)", c, dir, err, string(out))
		}
	}
}

func commitTestFile(t *testing.T, dir, filename, content string) {
	t.Helper()
	p := filepath.Join(dir, filename)
	if err := os.WriteFile(p, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write %s: %v", filename, err)
	}
	cmds := [][]string{
		{"git", "add", filename},
		{"git", "commit", "-m", "commit " + filename},
	}
	for _, c := range cmds {
		cmd := exec.Command(c[0], c[1:]...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("command %v failed: %v (%s)", c, err, string(out))
		}
	}
}

func TestRunDiscardCommand_TextAndJSON(t *testing.T) {
	tmpDir := t.TempDir()
	initTestGitRepo(t, tmpDir)
	commitTestFile(t, tmpDir, "base.txt", "original content")

	// 1. Modify tracked file
	if err := os.WriteFile(filepath.Join(tmpDir, "base.txt"), []byte("modified dirty content"), 0644); err != nil {
		t.Fatalf("failed modifying base.txt: %v", err)
	}
	// 2. Add untracked file
	if err := os.WriteFile(filepath.Join(tmpDir, "new_file.txt"), []byte("untracked file"), 0644); err != nil {
		t.Fatalf("failed writing new_file.txt: %v", err)
	}
	// 3. Add stale lock
	lockPath := filepath.Join(tmpDir, ".git", "index.lock")
	_ = os.WriteFile(lockPath, []byte("stale lock"), 0644)

	// Run discard command with text output
	err := runDiscardCommand([]string{"--worktree-path=" + tmpDir, "--force"})
	if err != nil {
		t.Fatalf("runDiscardCommand failed: %v", err)
	}

	// Verify base.txt restored
	b, err := os.ReadFile(filepath.Join(tmpDir, "base.txt"))
	if err != nil || string(b) != "original content" {
		t.Errorf("base.txt not restored: %s (err: %v)", string(b), err)
	}

	// Verify new_file.txt deleted
	if _, err := os.Stat(filepath.Join(tmpDir, "new_file.txt")); !os.IsNotExist(err) {
		t.Errorf("expected new_file.txt to be removed")
	}

	// Verify lock deleted
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Errorf("expected index.lock to be removed")
	}

	// Dirty again and run with --json
	if err := os.WriteFile(filepath.Join(tmpDir, "base.txt"), []byte("modified dirty 2"), 0644); err != nil {
		t.Fatalf("failed dirtying base.txt: %v", err)
	}
	err = runDiscardCommand([]string{"--worktree-path=" + tmpDir, "--json"})
	if err != nil {
		t.Fatalf("runDiscardCommand --json failed: %v", err)
	}
}

func TestExecutePureCodeAction_EndToEnd(t *testing.T) {
	tmpDir := t.TempDir()
	initTestGitRepo(t, tmpDir)
	commitTestFile(t, tmpDir, "file.txt", "v1")

	// Create dirty state
	_ = os.WriteFile(filepath.Join(tmpDir, "file.txt"), []byte("v2 dirty"), 0644)
	_ = os.WriteFile(filepath.Join(tmpDir, "new.txt"), []byte("untracked"), 0644)

	var mu sync.Mutex
	var postedChunks []string
	var postedExit controlplane.ExitOptions
	exitPosted := false

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		if r.URL.Path == "/v1/jobs/job-discard-1/chunks" {
			var body struct {
				Stream string `json:"stream"`
				Chunk  string `json:"chunk"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			postedChunks = append(postedChunks, body.Chunk)
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.URL.Path == "/v1/jobs/job-discard-1/exit" {
			_ = json.NewDecoder(r.Body).Decode(&postedExit)
			exitPosted = true
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sp, err := spool.Open(spool.DefaultDir(), "job-discard-1")
	if err != nil {
		t.Fatalf("failed to open spool: %v", err)
	}
	defer sp.Close()

	wd := watchdog.New(watchdog.Options{})
	job := controlplane.JobDetail{
		ID:           "job-discard-1",
		ScheduleID:   "sch-discard-1",
		Kind:         "discard",
		WorktreePath: tmpDir,
		Status:       "RUNNING",
	}

	err = executePureCodeAction(context.Background(), srv.URL, "", job, tmpDir, "", sp, wd)
	if err != nil {
		t.Fatalf("executePureCodeAction failed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if !exitPosted {
		t.Fatalf("expected exit to be posted to control plane")
	}
	if postedExit.ExitCode != 0 {
		t.Errorf("expected exit code 0, got %d", postedExit.ExitCode)
	}
	if postedExit.DiagnosticCode != "SUCCESS" {
		t.Errorf("expected DiagnosticCode SUCCESS, got %s", postedExit.DiagnosticCode)
	}
	if postedExit.TerminationReasonCode != "CLEAN_SUCCESS" {
		t.Errorf("expected TerminationReasonCode CLEAN_SUCCESS, got %s", postedExit.TerminationReasonCode)
	}

	// Verify worktree is pristine
	b, _ := os.ReadFile(filepath.Join(tmpDir, "file.txt"))
	if string(b) != "v1" {
		t.Errorf("expected file.txt to be restored to v1, got %s", string(b))
	}
	if _, err := os.Stat(filepath.Join(tmpDir, "new.txt")); !os.IsNotExist(err) {
		t.Errorf("expected new.txt to be deleted")
	}
}
