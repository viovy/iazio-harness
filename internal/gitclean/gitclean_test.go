package gitclean

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func initGitRepo(t *testing.T, dir string) {
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
			t.Fatalf("failed command %v in %s: %v (%s)", c, dir, err, string(out))
		}
	}
}

func commitFile(t *testing.T, dir, filename, content string) {
	t.Helper()
	filePath := filepath.Join(dir, filename)
	if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
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
			t.Fatalf("failed git command %v: %v (%s)", c, err, string(out))
		}
	}
}

func TestDiscardWorkspace_CleanDirtyFilesAndLocks(t *testing.T) {
	tmpDir := t.TempDir()
	initGitRepo(t, tmpDir)

	// Create and commit a tracked file
	commitFile(t, tmpDir, "README.md", "# Original Content\n")

	// 1. Modify the tracked file
	if err := os.WriteFile(filepath.Join(tmpDir, "README.md"), []byte("# Modified Uncommitted Content\n"), 0644); err != nil {
		t.Fatalf("failed modifying README.md: %v", err)
	}

	// 2. Add an untracked new file
	if err := os.WriteFile(filepath.Join(tmpDir, "new_untracked.txt"), []byte("temporary untracked file\n"), 0644); err != nil {
		t.Fatalf("failed writing new_untracked.txt: %v", err)
	}

	// 3. Add an untracked directory with files
	subDir := filepath.Join(tmpDir, "untracked_dir", "nested")
	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatalf("failed creating untracked_dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(subDir, "nested_file.txt"), []byte("nested content\n"), 0644); err != nil {
		t.Fatalf("failed writing nested_file.txt: %v", err)
	}

	// 4. Create an ignored file via .gitignore
	if err := os.WriteFile(filepath.Join(tmpDir, ".gitignore"), []byte("*.ignored\n"), 0644); err != nil {
		t.Fatalf("failed writing .gitignore: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "temp.ignored"), []byte("ignored artifact\n"), 0644); err != nil {
		t.Fatalf("failed writing temp.ignored: %v", err)
	}

	// 5. Create a stale .git/index.lock file
	lockPath := filepath.Join(tmpDir, ".git", "index.lock")
	if err := os.WriteFile(lockPath, []byte("stale lock\n"), 0644); err != nil {
		t.Fatalf("failed writing index.lock: %v", err)
	}

	// Verify before discard: repository is dirty and lock exists
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("expected index.lock to exist before discard")
	}

	var progressEvents []string
	opts := DiscardOptions{
		WorktreePath: tmpDir,
		Force:        true,
		Submodules:   true,
		RemoveLocks:  true,
		OnProgress: func(stage, detail string) {
			progressEvents = append(progressEvents, stage+": "+detail)
		},
	}

	res, err := DiscardWorkspace(context.Background(), opts)
	if err != nil {
		t.Fatalf("DiscardWorkspace failed: %v", err)
	}

	if !res.Success {
		t.Fatalf("expected res.Success == true, got false. Message: %s", res.Message)
	}
	if !res.Clean {
		t.Fatalf("expected res.Clean == true, got false. Remaining porcelain: %s", res.RemainingPorcelain)
	}
	if len(res.LocksRemoved) == 0 {
		t.Errorf("expected index.lock to be reported as removed")
	}

	// Verify tracked file is reset to original content
	readmeBytes, err := os.ReadFile(filepath.Join(tmpDir, "README.md"))
	if err != nil {
		t.Fatalf("failed reading README.md after discard: %v", err)
	}
	if string(readmeBytes) != "# Original Content\n" {
		t.Errorf("expected README.md to be reset, got: %q", string(readmeBytes))
	}

	// Verify untracked file is removed
	if _, err := os.Stat(filepath.Join(tmpDir, "new_untracked.txt")); !os.IsNotExist(err) {
		t.Errorf("expected new_untracked.txt to be removed")
	}

	// Verify untracked directory is removed
	if _, err := os.Stat(filepath.Join(tmpDir, "untracked_dir")); !os.IsNotExist(err) {
		t.Errorf("expected untracked_dir to be removed")
	}

	// Verify ignored file is also removed (-x flag in -ffdx)
	if _, err := os.Stat(filepath.Join(tmpDir, "temp.ignored")); !os.IsNotExist(err) {
		t.Errorf("expected temp.ignored to be removed by clean -ffdx")
	}

	// Verify lock is removed
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Errorf("expected index.lock to be removed")
	}

	// Verify progress events recorded
	if len(progressEvents) == 0 {
		t.Errorf("expected progress events to be emitted")
	}
}

func TestDiscardWorkspace_Submodules(t *testing.T) {
	// Create main repo and child submodule
	parentDir := t.TempDir()
	submoduleDir := t.TempDir()

	initGitRepo(t, parentDir)
	commitFile(t, parentDir, "main.txt", "main content")

	initGitRepo(t, submoduleDir)
	commitFile(t, submoduleDir, "sub.txt", "sub content")

	// Add submodule to parent
	addSubCmd := exec.Command("git", "-c", "protocol.file.allow=always", "submodule", "add", submoduleDir, "submod")
	addSubCmd.Dir = parentDir
	addSubCmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ALLOW_PROTOCOL=file")
	if out, err := addSubCmd.CombinedOutput(); err != nil {
		t.Fatalf("failed git submodule add: %v (%s)", err, string(out))
	}
	// Commit submodule addition in parent
	commitCmd := exec.Command("git", "commit", "-am", "add submodule submod")
	commitCmd.Dir = parentDir
	commitCmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if out, err := commitCmd.CombinedOutput(); err != nil {
		t.Fatalf("failed committing submodule: %v (%s)", err, string(out))
	}

	// Now dirty the submodule
	submodSubPath := filepath.Join(parentDir, "submod")
	if err := os.WriteFile(filepath.Join(submodSubPath, "sub.txt"), []byte("dirtied sub file"), 0644); err != nil {
		t.Fatalf("failed dirtying submodule: %v", err)
	}
	if err := os.WriteFile(filepath.Join(submodSubPath, "untracked_in_sub.txt"), []byte("new file in sub"), 0644); err != nil {
		t.Fatalf("failed creating untracked file in submodule: %v", err)
	}

	// Also dirty the parent
	if err := os.WriteFile(filepath.Join(parentDir, "new_parent.txt"), []byte("new file in parent"), 0644); err != nil {
		t.Fatalf("failed creating untracked file in parent: %v", err)
	}

	opts := DiscardOptions{
		WorktreePath: parentDir,
		Force:        true,
		Submodules:   true,
		RemoveLocks:  true,
	}

	res, err := DiscardWorkspace(context.Background(), opts)
	if err != nil {
		t.Fatalf("DiscardWorkspace failed with submodules: %v", err)
	}
	if !res.Clean {
		t.Fatalf("expected clean worktree after submodule discard, got porcelain: %s", res.RemainingPorcelain)
	}

	// Verify submodule file restored
	subBytes, err := os.ReadFile(filepath.Join(submodSubPath, "sub.txt"))
	if err != nil {
		t.Fatalf("failed reading sub.txt: %v", err)
	}
	if string(subBytes) != "sub content" {
		t.Errorf("expected sub.txt to be restored to 'sub content', got %q", string(subBytes))
	}

	// Verify untracked in submodule was removed
	if _, err := os.Stat(filepath.Join(submodSubPath, "untracked_in_sub.txt")); !os.IsNotExist(err) {
		t.Errorf("expected untracked_in_sub.txt to be removed")
	}
}

func TestDiscardResult_GoldenOutput(t *testing.T) {
	res := &DiscardResult{
		WorktreePath:      "/mock/repo",
		Success:           true,
		Clean:             true,
		Duration:          12 * time.Millisecond,
		DurationStr:       "12ms",
		LocksRemoved:      []string{"index.lock"},
		OperationsAborted: []string{"merge"},
		SubmodulesCleaned: 2,
		Message:           "Workspace successfully reset to pristine git state (clean)",
	}

	goldenTxtPath := filepath.Join("testdata", "discard_report.golden.txt")
	expectedTxtBytes, err := os.ReadFile(goldenTxtPath)
	if err != nil {
		t.Fatalf("failed reading golden txt: %v", err)
	}
	actualTxt := res.String()
	if strings.TrimSpace(actualTxt) != strings.TrimSpace(string(expectedTxtBytes)) {
		t.Errorf("golden text mismatch:\nGOT:\n%s\nWANT:\n%s", actualTxt, string(expectedTxtBytes))
	}

	goldenJSONPath := filepath.Join("testdata", "discard_report.golden.json")
	expectedJSONBytes, err := os.ReadFile(goldenJSONPath)
	if err != nil {
		t.Fatalf("failed reading golden json: %v", err)
	}
	actualJSON, err := res.JSON()
	if err != nil {
		t.Fatalf("failed generating json: %v", err)
	}
	if strings.TrimSpace(string(actualJSON)) != strings.TrimSpace(string(expectedJSONBytes)) {
		t.Errorf("golden json mismatch:\nGOT:\n%s\nWANT:\n%s", string(actualJSON), string(expectedJSONBytes))
	}
}

func TestIsPureCodeAction(t *testing.T) {
	cases := []struct {
		kind string
		want bool
	}{
		{"discard", true},
		{"DISCARD", true},
		{"git_clean", true},
		{"clean", true},
		{"git_reset", true},
		{"reset_workspace", true},
		{"heal_git", true},
		{"force_clean_git", true},
		{"ordinary", false},
		{"story_execution", false},
		{"story_refinement", false},
		{"intervention", false},
		{"resume", false},
		{"", false},
	}

	for _, tc := range cases {
		got := IsPureCodeAction(tc.kind)
		if got != tc.want {
			t.Errorf("IsPureCodeAction(%q) = %v, want %v", tc.kind, got, tc.want)
		}
	}
}
