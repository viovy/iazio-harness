package gate

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

func TestInspectStoryHonesty_RejectsUncheckedDoDPendingAndDuplicateFullReviews(t *testing.T) {
	rootDir := t.TempDir()
	storiesDir := filepath.Join(rootDir, "docs", "stories")
	hubStoriesDir := filepath.Join(rootDir, "iazio-infra", "docs", "stories")
	if err := os.MkdirAll(storiesDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(hubStoriesDir, 0755); err != nil {
		t.Fatal(err)
	}
	rootStory := filepath.Join(storiesDir, "2026-11-14-gov-5282-test.story.md")
	hubStory := filepath.Join(hubStoriesDir, "2026-11-14-gov-5282-test.story.md")

	// 1. status: DONE with unchecked Definition of Done & _pending_ reviews
	badDoD := "---\nid: STORY-GOV-5282\nstatus: DONE\nreview_scheme_scope: full\n---\n\n## Tasks\n- [x] Done task\n\n## Definition of Done\n- [ ] Land PR\n\n## 7. Reviews\n| 1 | opencode | codex | _pending_ | _pending_ |\n"
	_ = os.WriteFile(rootStory, []byte(badDoD), 0644)
	_ = os.WriteFile(hubStory, []byte(badDoD), 0644)
	rep := InspectStoryHonesty([]string{rootDir}, "STORY-GOV-5282")
	if !rep.Found || rep.AllHonestDone || !rep.Incomplete {
		t.Fatalf("expected unchecked DoD & _pending_ reviews to fail honesty, got %+v", rep)
	}
	joined := strings.Join(rep.Reasons, ",")
	if !strings.Contains(joined, "unchecked_dod") || !strings.Contains(joined, "pending_reviews_table") {
		t.Fatalf("expected unchecked_dod & pending_reviews_table reasons, got %v", rep.Reasons)
	}

	// 2. Duplicate single-file review reuse under review_scheme_scope: full
	dupRev := "---\nid: STORY-GOV-5282\nstatus: DONE\nreview_scheme_scope: full\n---\n\n## Tasks\n- [x] T1\n\n## Definition of Done\n- [x] D1\n\n## 7. Reviews\n| # | Tool | Outcome | Artifact |\n|---|---|---|---|\n| 1 | opencode | CLEAN | docs/reviews/2026-11-14-gov-5282-shared.md |\n| 2 | agy | CLEAN | docs/reviews/2026-11-14-gov-5282-shared.md |\n"
	_ = os.WriteFile(rootStory, []byte(dupRev), 0644)
	_ = os.WriteFile(hubStory, []byte(dupRev), 0644)
	repDup := InspectStoryHonesty([]string{rootDir}, "STORY-GOV-5282")
	if repDup.AllHonestDone || !strings.Contains(strings.Join(repDup.Reasons, ","), "duplicate_review_file_reuse") {
		t.Fatalf("expected duplicate_review_file_reuse to fail full-scope review honesty, got %+v", repDup)
	}

	// 3. One copy clean, second copy in hub still has unchecked task -> rejected
	cleanCopy := "---\nid: STORY-GOV-5282\nstatus: DONE\nreview_scheme_scope: full\n---\n\n## Tasks\n- [x] T1\n\n## Definition of Done\n- [x] D1\n\n## 7. Reviews\n| # | Tool | Outcome | Artifact |\n|---|---|---|---|\n| 1 | opencode | CLEAN | docs/reviews/2026-11-14-gov-5282-opencode.md |\n| 2 | agy | CLEAN | docs/reviews/2026-11-14-gov-5282-agy.md |\n"
	staleHub := "---\nid: STORY-GOV-5282\nstatus: DONE\nreview_scheme_scope: full\n---\n\n## Tasks\n- [ ] Missed task\n\n## Definition of Done\n- [x] D1\n"
	_ = os.WriteFile(rootStory, []byte(cleanCopy), 0644)
	_ = os.WriteFile(hubStory, []byte(staleHub), 0644)
	repDrift := InspectStoryHonesty([]string{rootDir}, "STORY-GOV-5282")
	if repDrift.AllHonestDone || !strings.Contains(strings.Join(repDrift.Reasons, ","), "unchecked_tasks") {
		t.Fatalf("expected second incomplete hub copy to fail honesty, got %+v", repDrift)
	}

	// 4. Both copies clean & distinct review files -> honest pass
	_ = os.WriteFile(hubStory, []byte(cleanCopy), 0644)
	repOK := InspectStoryHonesty([]string{rootDir}, "STORY-GOV-5282")
	if !repOK.Found || !repOK.AllHonestDone || repOK.Incomplete {
		t.Fatalf("expected honest DONE copies to pass, got %+v", repOK)
	}
}

func TestVerify_MultiRepoSuperprojectAndSiblingSubmodule(t *testing.T) {
	base := t.TempDir()
	runGit := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v in %s failed: %v: %s", args, dir, err, string(out))
		}
	}
	initPushRepo := func(name string) (string, string) {
		bare := filepath.Join(base, name+".git")
		if out, err := exec.Command("git", "init", "--bare", "-b", "main", bare).CombinedOutput(); err != nil {
			t.Fatalf("git init bare %s: %v: %s", name, err, string(out))
		}
		dir := filepath.Join(base, name)
		_ = os.MkdirAll(dir, 0755)
		runGit(dir, "init", "-b", "main")
		runGit(dir, "config", "user.email", "test@example.com")
		runGit(dir, "config", "user.name", "Test")
		runGit(dir, "remote", "add", "origin", bare)
		_ = os.WriteFile(filepath.Join(dir, "README.md"), []byte(name+"\n"), 0644)
		runGit(dir, "add", ".")
		runGit(dir, "commit", "-m", "init "+name)
		runGit(dir, "push", "-u", "origin", "main")
		return dir, bare
	}

	spDir, _ := initPushRepo("meta-repo")
	_, leafBare := initPushRepo("iazio-mcp")
	_, hubBare := initPushRepo("iazio-infra")
	cmdAdd := exec.Command("git", "-c", "protocol.file.allow=always", "-C", spDir, "submodule", "add", leafBare, "iazio-mcp")
	if out, err := cmdAdd.CombinedOutput(); err != nil {
		t.Fatalf("submodule add iazio-mcp: %v: %s", err, string(out))
	}
	cmdAddHub := exec.Command("git", "-c", "protocol.file.allow=always", "-C", spDir, "submodule", "add", hubBare, "iazio-infra")
	if out, err := cmdAddHub.CombinedOutput(); err != nil {
		t.Fatalf("submodule add iazio-infra: %v: %s", err, string(out))
	}

	leafDir := filepath.Join(spDir, "iazio-mcp")
	hubDir := filepath.Join(spDir, "iazio-infra")
	runGit(leafDir, "checkout", "main")
	runGit(hubDir, "checkout", "main")

	cleanStory := "---\nid: STORY-GOV-5282\nstatus: DONE\nreview_scheme_scope: full\nsubmodules:\n  - iazio-mcp\n  - iazio-infra\n---\n\n## Tasks\n- [x] Done\n\n## Definition of Done\n- [x] Landed\n\n## 7. Reviews\n| # | Tool | Outcome | Artifact |\n|---|---|---|---|\n| 1 | opencode | CLEAN | docs/reviews/2026-11-14-gov-5282-opencode.md |\n| 2 | agy | CLEAN | docs/reviews/2026-11-14-gov-5282-agy.md |\n"
	_ = os.MkdirAll(filepath.Join(spDir, "docs", "stories"), 0755)
	_ = os.WriteFile(filepath.Join(spDir, "docs", "stories", "2026-11-14-gov-5282.story.md"), []byte(cleanStory), 0644)
	_ = os.MkdirAll(filepath.Join(hubDir, "docs", "stories"), 0755)
	_ = os.WriteFile(filepath.Join(hubDir, "docs", "stories", "2026-11-14-gov-5282.story.md"), []byte(cleanStory), 0644)
	runGit(hubDir, "add", ".")
	runGit(hubDir, "commit", "-m", "story done")
	runGit(hubDir, "push", "origin", "main")

	// Superproject left on unmerged feature branch while leaf iazio-mcp is clean on main!
	runGit(spDir, "checkout", "-b", "feat/gov-5282")
	runGit(spDir, "add", ".")
	runGit(spDir, "commit", "-m", "chore: superproject branch WIP")
	runGit(spDir, "push", "-u", "origin", "feat/gov-5282")

	resBranchFail := Verify(GateOptions{
		WorktreePath:           leafDir,
		StoryID:                "STORY-GOV-5282",
		RequireGitClean:        true,
		RequireSubmodulesClean: true,
	})
	if resBranchFail.Passed || resBranchFail.BranchClean {
		t.Fatalf("expected Verify from clean leaf to FAIL while superproject is on feat/gov-5282, got %+v", resBranchFail)
	}

	// Merge superproject back to main + push, but leave sibling iazio-infra on feature branch
	runGit(spDir, "checkout", "main")
	runGit(spDir, "merge", "--no-ff", "feat/gov-5282", "-m", "merge")
	runGit(spDir, "push", "origin", "main")
	runGit(hubDir, "checkout", "-b", "fix/hub-unmerged")
	resHubFail := Verify(GateOptions{
		WorktreePath:           leafDir,
		StoryID:                "STORY-GOV-5282",
		RequireGitClean:        true,
		RequireSubmodulesClean: true,
	})
	if resHubFail.Passed || resHubFail.BranchClean {
		t.Fatalf("expected Verify to FAIL while sibling story submodule iazio-infra is on feature branch, got %+v", resHubFail)
	}

	// Settle sibling hub back on main + re-sync gitlink cleanly -> must pass
	runGit(hubDir, "checkout", "main")
	resOK := Verify(GateOptions{
		WorktreePath:           leafDir,
		StoryID:                "STORY-GOV-5282",
		RequireGitClean:        true,
		RequireSubmodulesClean: true,
	})
	if !resOK.Passed {
		t.Fatalf("expected Verify to PASS once leaf, sibling hub, and superproject are all clean on main with honest story, got %+v", resOK)
	}
}

func TestCountUnpushedCommitsAndIsNonMainBranch_FailClosed(t *testing.T) {
	t.Setenv("IAZIO_TEST_SKIP_REMOTE", "")

	// 1. Non-git directory returns (false, "") cleanly
	nonGit := t.TempDir()
	if nonMain, br := IsNonMainBranch(nonGit, "git"); nonMain || br != "" {
		t.Fatalf("expected non-git dir to return (false, ''), got (%v, %q)", nonMain, br)
	}

	// 2. Unborn main vs unborn feat/x via symbolic-ref fallback
	unbornMain := t.TempDir()
	if out, err := exec.Command("git", "init", "-b", "main", unbornMain).CombinedOutput(); err != nil {
		t.Fatalf("git init main: %v: %s", err, string(out))
	}
	if nonMain, br := IsNonMainBranch(unbornMain, "git"); nonMain || br != "main" {
		t.Fatalf("expected unborn main to return (false, 'main'), got (%v, %q)", nonMain, br)
	}
	if nonMain, br := IsNonMainBranch(unbornMain, "/nonexistent-git-bin-0206"); !nonMain || !strings.Contains(br, "probe_error") {
		t.Fatalf("expected unprobeable .git repo to fail closed with probe_error, got (%v, %q)", nonMain, br)
	}

	unbornFeat := t.TempDir()
	if out, err := exec.Command("git", "init", "-b", "feat/demo", unbornFeat).CombinedOutput(); err != nil {
		t.Fatalf("git init feat/demo: %v: %s", err, string(out))
	}
	if nonMain, br := IsNonMainBranch(unbornFeat, "git"); !nonMain || br != "feat/demo" {
		t.Fatalf("expected unborn feat/demo to return (true, 'feat/demo'), got (%v, %q)", nonMain, br)
	}

	// 3. Committed git repo with NO remote fails closed in CountUnpushedCommits and Verify
	noRemote := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", noRemote}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v: %s", args, err, string(out))
		}
	}
	runGit("init", "-b", "main")
	runGit("config", "user.email", "test@example.com")
	runGit("config", "user.name", "Test")
	_ = os.MkdirAll(filepath.Join(noRemote, "docs", "stories"), 0o755)
	_ = os.WriteFile(filepath.Join(noRemote, "docs", "stories", "2026-11-14-gov-5282.story.md"), []byte("---\nid: STORY-GOV-5282\nstatus: DONE\n---\n\n## Definition of Done\n- [x] Landed\n"), 0o644)
	runGit("add", ".")
	runGit("commit", "-m", "init no remote")

	if _, err := CountUnpushedCommits(noRemote, "git"); err == nil || !strings.Contains(err.Error(), "no git remotes configured") {
		t.Fatalf("expected empty-remote repo to fail closed, got err=%v", err)
	}
	res := Verify(GateOptions{
		WorktreePath:           noRemote,
		StoryID:                "STORY-GOV-5282",
		RequireGitClean:        true,
		RequireSubmodulesClean: true,
	})
	if res.Passed || res.BranchClean {
		t.Fatalf("expected Verify on empty-remote repo to fail closed with BranchClean=false, got %+v", res)
	}

	// 4. IAZIO_TEST_SKIP_REMOTE=1 bypasses missing remote in isolated unit test fixtures
	t.Setenv("IAZIO_TEST_SKIP_REMOTE", "1")
	cnt, err := CountUnpushedCommits(noRemote, "git")
	if err != nil || cnt != 0 {
		t.Fatalf("expected IAZIO_TEST_SKIP_REMOTE=1 to return (0, nil), got (%d, %v)", cnt, err)
	}
}
