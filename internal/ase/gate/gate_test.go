package gate

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestVerify_AllClean(t *testing.T) {
	tmpDir := t.TempDir()

	mockGit := func(args ...string) (string, error) {
		cmd := args[0]
		switch cmd {
		case "status":
			return "", nil
		case "rev-parse":
			return "main", nil
		case "submodule":
			return " abcdef0 submod (heads/main)", nil
		case "log":
			return "commit12345", nil
		default:
			return "", nil
		}
	}

	opts := GateOptions{
		WorktreePath: tmpDir,
		StoryID:      "STORY-GOV-3684",
		GitRunner:    mockGit,
	}

	res := Verify(opts)
	if !res.Passed {
		t.Fatalf("expected gate to pass, got failed: reason=%s, unmet=%v", res.Reason, res.UnmetGates)
	}
	if !res.GitClean || !res.BranchClean || !res.SubmodulesClean || !res.StoryDone {
		t.Errorf("expected all sub-gates true, got %+v", res)
	}
}

func TestVerify_EmptyWorktree(t *testing.T) {
	res := Verify(GateOptions{StoryID: "STORY-1"})
	if res.Passed {
		t.Errorf("expected false for empty worktree with story ID")
	}

	res2 := Verify(GateOptions{})
	if !res2.Passed {
		t.Errorf("expected true for empty worktree without story ID")
	}
}

func TestVerify_DetachedBranchAndSubmodules(t *testing.T) {
	tmpDir := t.TempDir()

	mockGit := func(args ...string) (string, error) {
		cmd := args[0]
		switch cmd {
		case "status":
			return "", nil
		case "rev-parse":
			return "HEAD", nil
		case "submodule":
			return "+abcdef0 submod1\nU123456 submod2", nil
		default:
			return "", nil
		}
	}

	res := Verify(GateOptions{
		WorktreePath: tmpDir,
		GitRunner:    mockGit,
	})
	if res.BranchClean {
		t.Errorf("expected BranchClean=false for detached HEAD")
	}
	if res.SubmodulesClean {
		t.Errorf("expected SubmodulesClean=false for dirty submodules")
	}
}

func TestVerify_RequiredStamps(t *testing.T) {
	tmpDir := t.TempDir()
	reportsDir := filepath.Join(tmpDir, "tmp", "reports")
	_ = os.MkdirAll(reportsDir, 0755)

	mockGit := func(args ...string) (string, error) {
		return "", nil
	}

	// Stamps missing
	res := Verify(GateOptions{
		WorktreePath:           tmpDir,
		RequireTestsPassed:     true,
		RequireReleaseVerified: true,
		GitRunner:              mockGit,
	})
	if res.TestsPassed || res.ReleaseVerified {
		t.Errorf("expected false for missing stamps")
	}

	// Stamps present
	_ = os.WriteFile(filepath.Join(reportsDir, "validate-local-ok.stamp"), []byte("ok"), 0644)
	_ = os.WriteFile(filepath.Join(reportsDir, "verify-live-release-ok.stamp"), []byte("ok"), 0644)

	res2 := Verify(GateOptions{
		WorktreePath:           tmpDir,
		RequireTestsPassed:     true,
		RequireReleaseVerified: true,
		GitRunner:              mockGit,
	})
	if !res2.TestsPassed || !res2.ReleaseVerified {
		t.Errorf("expected true for present stamps")
	}
}

func TestVerify_DirtyTree(t *testing.T) {
	tmpDir := t.TempDir()

	mockGit := func(args ...string) (string, error) {
		cmd := args[0]
		switch cmd {
		case "status":
			return " M file.go\n?? untracked.txt", nil
		case "rev-parse":
			return "main", nil
		case "submodule":
			return "", nil
		case "log":
			return "commit12345", nil
		default:
			return "", nil
		}
	}

	opts := GateOptions{
		WorktreePath: tmpDir,
		StoryID:      "STORY-GOV-3684",
		GitRunner:    mockGit,
	}

	res := Verify(opts)
	if res.Passed {
		t.Fatalf("expected gate to fail on dirty tree, got passed")
	}
	if res.Reason != "incomplete_ase_flow" {
		t.Errorf("expected reason incomplete_ase_flow, got %s", res.Reason)
	}
	if res.GitClean {
		t.Errorf("expected GitClean=false")
	}
}

func TestVerify_StoryNotDone(t *testing.T) {
	tmpDir := t.TempDir()
	storiesDir := filepath.Join(tmpDir, "docs", "stories")
	_ = os.MkdirAll(storiesDir, 0755)

	storyFile := filepath.Join(storiesDir, "2026-10-25-gov-3684-test.story.md")
	_ = os.WriteFile(storyFile, []byte("---\nid: STORY-GOV-3684\nstatus: IN_PROGRESS\n---\n"), 0644)

	mockGit := func(args ...string) (string, error) {
		cmd := args[0]
		switch cmd {
		case "status":
			return "", nil
		case "rev-parse":
			return "main", nil
		case "submodule":
			return "", nil
		case "log":
			return "", fmt.Errorf("no commit found")
		default:
			return "", nil
		}
	}

	opts := GateOptions{
		WorktreePath: tmpDir,
		StoryID:      "STORY-GOV-3684",
		GitRunner:    mockGit,
	}

	res := Verify(opts)
	if res.Passed {
		t.Fatalf("expected gate to fail when story not DONE, got passed")
	}
	if res.StoryDone {
		t.Errorf("expected StoryDone=false")
	}
}

func TestVerify_StoryNotDone_EvenWithGitCommit(t *testing.T) {
	tmpDir := t.TempDir()
	storiesDir := filepath.Join(tmpDir, "docs", "stories")
	_ = os.MkdirAll(storiesDir, 0755)

	storyFile := filepath.Join(storiesDir, "2026-10-25-gov-3684-test.story.md")
	_ = os.WriteFile(storyFile, []byte("---\nid: STORY-GOV-3684\nstatus: IN_PROGRESS\n---\n"), 0644)

	mockGit := func(args ...string) (string, error) {
		cmd := args[0]
		switch cmd {
		case "status":
			return "", nil
		case "rev-parse":
			return "main", nil
		case "submodule":
			return "", nil
		case "log":
			return "commit-initial-bind-3684", nil
		default:
			return "", nil
		}
	}

	opts := GateOptions{
		WorktreePath: tmpDir,
		StoryID:      "STORY-GOV-3684",
		GitRunner:    mockGit,
	}

	res := Verify(opts)
	if res.Passed {
		t.Fatalf("expected gate to fail when story file is IN_PROGRESS despite git commits on branch")
	}
	if res.StoryDone {
		t.Errorf("expected StoryDone=false when story markdown status is IN_PROGRESS")
	}
}

func TestDefaultGitRunner(t *testing.T) {
	runner := DefaultGitRunner(t.TempDir())
	_, _ = runner("version")
}
