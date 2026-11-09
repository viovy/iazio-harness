package diagnostics

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func initTestDiagnosticGitRepo(t *testing.T, dir string) {
	t.Helper()
	runGit := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test",
			"GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=Test",
			"GIT_COMMITTER_EMAIL=test@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v (%s)", args, err, string(out))
		}
	}
	runGit("init", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Test\n"), 0644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	runGit("add", "README.md")
	runGit("commit", "-m", "initial commit")
}

func TestGitUpdateInterval_ClampAndParse(t *testing.T) {
	if got := ParseGitUpdateInterval(""); got != 30*time.Minute {
		t.Fatalf("expected default 30m, got %v", got)
	}
	if got := ParseGitUpdateInterval("10m"); got != 15*time.Minute {
		t.Fatalf("expected 10m clamped to 15m, got %v", got)
	}
	if got := ParseGitUpdateInterval("40m"); got != 40*time.Minute {
		t.Fatalf("expected 40m preserved, got %v", got)
	}
	if got := ParseGitUpdateInterval("2h"); got != 1*time.Hour {
		t.Fatalf("expected 2h clamped to 1h, got %v", got)
	}
}

func TestTwoStageGitUpdateRemediator_Step0Step1Step2CooldownAndResume(t *testing.T) {
	repoDir := t.TempDir()
	initTestDiagnosticGitRepo(t, repoDir)
	ctx := context.Background()

	// 1. When IN_PROGRESS ASE story is active, execution must skip
	_ = os.MkdirAll(filepath.Join(repoDir, ".autopilot"), 0755)
	_ = os.WriteFile(filepath.Join(repoDir, ".autopilot", "ase-state.json"), []byte(`{"story_id":"STORY-IAZIO-9999","status":"IN_PROGRESS","phase":"implementation"}`), 0644)
	_ = os.WriteFile(filepath.Join(repoDir, ".session_story_id"), []byte("STORY-IAZIO-9999\n"), 0644)

	rem := NewTwoStageGitUpdateRemediator(20 * time.Minute)
	skipRes := rem.Execute(ctx, repoDir)
	if !skipRes.Skipped {
		t.Fatalf("expected skip when IN_PROGRESS ASE story is active, got %+v", skipRes)
	}
	if _, err := os.Stat(filepath.Join(repoDir, ".session_story_id")); err != nil {
		t.Fatalf("expected .session_story_id to be untouched while IN_PROGRESS")
	}

	// 2. Mark story DONE and add ephemeral artifacts + dirty file; test Step 0 + Step 1 clean shell git-update
	_ = os.WriteFile(filepath.Join(repoDir, ".autopilot", "ase-state.json"), []byte(`{"story_id":"STORY-IAZIO-9999","status":"DONE","phase":"completed"}`), 0644)
	_ = os.WriteFile(filepath.Join(repoDir, "cov_auth"), []byte("mode: set\n"), 0644)
	_ = os.MkdirAll(filepath.Join(repoDir, "TestResults", "Diagnostics"), 0755)
	proofPath := filepath.Join(repoDir, "TestResults", "Diagnostics", "pr-closeout-proof-pr-7.json")
	_ = os.WriteFile(proofPath, []byte(`{"ok":true}`), 0644)
	dirtyFile := filepath.Join(repoDir, "uncommitted.txt")
	_ = os.WriteFile(dirtyFile, []byte("dirty"), 0644)

	var resumed int
	rem.ShellUpdate = func(ctx context.Context, path string) (string, error) {
		_ = os.Remove(dirtyFile)
		return "autopilot git-update ok", nil
	}
	rem.ResumeQueue = func(ctx context.Context, path string) error {
		resumed++
		return nil
	}

	s1Res := rem.Execute(ctx, repoDir)
	if !s1Res.Stage1Executed || !s1Res.Stage1Success || !s1Res.IsClean || !s1Res.QueueResumed || resumed != 1 {
		t.Fatalf("expected Step 1 clean completion and queue resume, got %+v", s1Res)
	}
	for _, p := range []string{
		filepath.Join(repoDir, ".session_story_id"),
		filepath.Join(repoDir, "cov_auth"),
		proofPath,
	} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("expected Step 0 to remove %s, stat err=%v", p, err)
		}
	}

	// 3. Step 1 fails -> Step 2 escalates to agy with 60m cooldown per dirty fingerprint
	_ = os.WriteFile(dirtyFile, []byte("diverged"), 0644)
	nowTime := time.Date(2026, 11, 7, 13, 0, 0, 0, time.UTC)
	rem.Now = func() time.Time { return nowTime }
	rem.ShellUpdate = func(ctx context.Context, path string) (string, error) {
		return "fatal: Not possible to fast-forward, aborting.", errors.New("exit status 1")
	}

	var agyCalls int
	var agyShouldFix bool
	rem.AgyEscalate = func(ctx context.Context, path, prompt, diag string) (string, error) {
		agyCalls++
		if prompt != `execute shell prompt "autopilot git-update"` {
			t.Fatalf("unexpected prompt %q", prompt)
		}
		if !strings.Contains(diag, "Not possible to fast-forward") {
			t.Fatalf("expected Step 1 diagnostic output in agy escalation, got %q", diag)
		}
		if agyShouldFix {
			_ = os.Remove(dirtyFile)
			return "agy fixed worktree", nil
		}
		return "agy failed", errors.New("exit status 1")
	}

	// First failure escalates to agy
	r1 := rem.Execute(ctx, repoDir)
	if !r1.Stage2Escalated || r1.Stage2Success || agyCalls != 1 {
		t.Fatalf("expected Stage 2 escalation on first failure, got %+v (agyCalls=%d)", r1, agyCalls)
	}

	// Within 60m cooldown for identical dirty fingerprint -> skipped
	nowTime = nowTime.Add(30 * time.Minute)
	r2 := rem.Execute(ctx, repoDir)
	if !r2.Stage2CooldownSkipped || agyCalls != 1 {
		t.Fatalf("expected Stage 2 cooldown skip within 60m, got %+v (agyCalls=%d)", r2, agyCalls)
	}

	// After 60m cooldown expires -> agy runs again, fixes worktree, and auto-resumes queue
	nowTime = nowTime.Add(31 * time.Minute)
	agyShouldFix = true
	r3 := rem.Execute(ctx, repoDir)
	if !r3.Stage2Escalated || !r3.Stage2Success || !r3.QueueResumed || agyCalls != 2 || resumed != 2 {
		t.Fatalf("expected Stage 2 to succeed and resume queue after cooldown, got %+v (agyCalls=%d resumed=%d)", r3, agyCalls, resumed)
	}
}

func TestIsWorktreeIdleForGitUpdate_StoryScopingAndBranchDetection(t *testing.T) {
	tempDir := t.TempDir()
	initTestDiagnosticGitRepo(t, tempDir)

	// Case 1: On main, no stories -> idle
	if !IsWorktreeIdleForGitUpdate(tempDir, nil) {
		t.Fatalf("expected clean main to be idle")
	}

	// Case 2: .autopilot/ase-state.json has an old completed story STORY-GOV-5169,
	// but active branch is feat/gov-5209-... with IN_PROGRESS story.
	dotAutopilot := filepath.Join(tempDir, ".autopilot")
	if err := os.MkdirAll(dotAutopilot, 0755); err != nil {
		t.Fatalf("mkdir .autopilot: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dotAutopilot, "ase-state.json"), []byte(`{"phase":"completed","status":"DONE","story_id":"STORY-GOV-5169"}`), 0644); err != nil {
		t.Fatalf("write ase-state: %v", err)
	}

	// Create docs/stories directory with IN_PROGRESS story STORY-GOV-5209
	storiesDir := filepath.Join(tempDir, "docs", "stories")
	if err := os.MkdirAll(storiesDir, 0755); err != nil {
		t.Fatalf("mkdir docs/stories: %v", err)
	}
	storyContent := "---\nid: STORY-GOV-5209\nstatus: IN_PROGRESS\n---\n"
	if err := os.WriteFile(filepath.Join(storiesDir, "2026-11-09-gov-5209.story.md"), []byte(storyContent), 0644); err != nil {
		t.Fatalf("write story file: %v", err)
	}

	// Switch git HEAD to feat/gov-5209-pointer-pr-rebase-rollback-and-fleet-healing
	cmd := exec.Command("git", "-C", tempDir, "checkout", "-b", "feat/gov-5209-pointer-pr-rebase-rollback-and-fleet-healing")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git checkout -b failed: %v (%s)", err, string(out))
	}

	// Active story branch must prevent worktree from being idle
	if IsWorktreeIdleForGitUpdate(tempDir, nil) {
		t.Fatalf("expected active story branch with IN_PROGRESS story to NOT be idle, but got idle=true")
	}

	// With .session_story_id present, it must also NOT be idle
	if err := os.WriteFile(filepath.Join(tempDir, ".session_story_id"), []byte("STORY-GOV-5209\n"), 0644); err != nil {
		t.Fatalf("write .session_story_id: %v", err)
	}
	if IsWorktreeIdleForGitUpdate(tempDir, nil) {
		t.Fatalf("expected worktree with active .session_story_id to NOT be idle, but got idle=true")
	}

	// Switch back to main while .session_story_id has an IN_PROGRESS story in iazio-infra/docs/stories -> still NOT idle!
	if out, err := exec.Command("git", "-C", tempDir, "checkout", "main").CombinedOutput(); err != nil {
		t.Fatalf("git checkout main: %v (%s)", err, string(out))
	}
	iazioStoriesDir := filepath.Join(tempDir, "iazio-infra", "docs", "stories")
	_ = os.MkdirAll(iazioStoriesDir, 0755)
	_ = os.WriteFile(filepath.Join(iazioStoriesDir, "2026-11-09-iazio-0200-test.story.md"), []byte("---\nid: STORY-IAZIO-0200\nstatus: IN_PROGRESS\n---\n"), 0644)
	_ = os.WriteFile(filepath.Join(tempDir, ".session_story_id"), []byte("STORY-IAZIO-0200\n"), 0644)
	if IsWorktreeIdleForGitUpdate(tempDir, nil) {
		t.Fatalf("expected worktree on main with IN_PROGRESS story in iazio-infra/docs/stories and .session_story_id to NOT be idle")
	}
}

