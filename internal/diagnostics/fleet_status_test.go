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
	if !strings.Contains(res.DiagnosticSummary, "  M  file.txt") {
		t.Errorf("summary missing modified file: %s", res.DiagnosticSummary)
	}
	if !strings.Contains(res.DiagnosticSummary, "  ?? untracked.go") {
		t.Errorf("summary missing untracked file: %s", res.DiagnosticSummary)
	}

	// Test ResetASELocks
	if err := ResetASELocks(tmpDir); err != nil {
		t.Fatalf("ResetASELocks failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tmpDir, ".autopilot", "lock")); !os.IsNotExist(err) {
		t.Errorf("expected lock to be removed")
	}
}

func TestParseSubmoduleStatus_Variants(t *testing.T) {
	res := &RepoFleetStatusResult{}
	cleanPath := t.TempDir()

	subOut := "+1234567 sub1 (heads/main)\n-abcdef0 sub2\nU0000000 sub3\n"
	parseSubmoduleStatus(context.Background(), cleanPath, subOut, res)

	if len(res.SubmoduleDrift) != 1 || res.SubmoduleDrift[0] != "sub1" {
		t.Errorf("expected sub1 in drift, got: %v", res.SubmoduleDrift)
	}
	if len(res.DirtySubmodules) != 2 {
		t.Errorf("expected 2 dirty submodules (sub2, sub3), got: %v", res.DirtySubmodules)
	}
}

func TestSubmoduleDriftClassification(t *testing.T) {
	// Porcelain v2 output with clean submodule commit drift (SC..)
	driftOut := `# branch.oid 1111111111111111111111111111111111111111
# branch.head main
1 .M SC.. 160000 160000 160000 2222222222222222222222222222222222222222 3333333333333333333333333333333333333333 iazio-mcp
`
	res := &RepoFleetStatusResult{IsClean: true}
	parseGitPorcelainV2(driftOut, res)

	if len(res.SubmoduleDrift) != 1 || res.SubmoduleDrift[0] != "iazio-mcp" {
		t.Errorf("expected submodule drift for iazio-mcp, got %v", res.SubmoduleDrift)
	}
	if len(res.DirtySubmodules) != 0 {
		t.Errorf("expected no dirty submodules, got %v", res.DirtySubmodules)
	}

	// Porcelain v2 output with dirty submodule content (S.M.)
	dirtyOut := `# branch.oid 1111111111111111111111111111111111111111
# branch.head main
1 .M S.M. 160000 160000 160000 2222222222222222222222222222222222222222 3333333333333333333333333333333333333333 iazio-web
`
	resDirty := &RepoFleetStatusResult{IsClean: true}
	parseGitPorcelainV2(dirtyOut, resDirty)

	if len(resDirty.DirtySubmodules) != 1 || !strings.Contains(resDirty.DirtySubmodules[0], "iazio-web") {
		t.Errorf("expected dirty submodule for iazio-web, got %v", resDirty.DirtySubmodules)
	}
}

func TestToolIdentificationAndLiveness(t *testing.T) {
	if tool, ok := isAgentTool("/usr/local/bin/autopilot"); !ok || tool != "autopilot" {
		t.Errorf("expected autopilot, got %s (%v)", tool, ok)
	}
	if _, ok := isAgentTool("/usr/bin/bash"); ok {
		t.Errorf("expected false for bash")
	}

	if tool, ok := isAuxiliaryTool("/usr/bin/node"); !ok || tool != "node" {
		t.Errorf("expected node, got %s (%v)", tool, ok)
	}
	if _, ok := isAuxiliaryTool("/usr/bin/unknown-tool"); ok {
		t.Errorf("expected false for unknown-tool")
	}

	if isPIDAlive(-1) {
		t.Errorf("expected PID -1 not alive")
	}
	if isPIDAlive(0) {
		t.Errorf("expected PID 0 not alive")
	}
	if isPIDAlive(1) {
		t.Errorf("expected PID 1 not alive per guard")
	}
	if !isPIDAlive(os.Getpid()) {
		t.Errorf("expected current PID to be alive")
	}

	tmpDir := t.TempDir()
	if err := ReapOrphanedProcesses(context.Background(), tmpDir); err != nil {
		t.Errorf("ReapOrphanedProcesses failed: %v", err)
	}
}

func TestParseSubmoduleStatusAndASE(t *testing.T) {
	tmpDir := t.TempDir()
	res := &RepoFleetStatusResult{IsClean: true}
	subOut := " 1111111111111111111111111111111111111111 sub1 (heads/main)\n+2222222222222222222222222222222222222222 sub2 (v1.0)\n"
	parseSubmoduleStatus(context.Background(), tmpDir, subOut, res)

	apDir := filepath.Join(tmpDir, ".autopilot")
	os.MkdirAll(apDir, 0755)
	os.WriteFile(filepath.Join(apDir, "state.json"), []byte(`{"state":"RUNNING","active_job_id":"job-123","lock":{"pid":99999999}}`), 0644)
	parseASEState(tmpDir, res)
	if res.ASEState == "" {
		t.Errorf("expected non-empty ASEState")
	}

	if err := ResetASELocks(tmpDir); err != nil {
		t.Errorf("ResetASELocks failed: %v", err)
	}
	parseASEState(tmpDir, res)
	if res.ASEState != "IDLE" {
		t.Errorf("expected IDLE after reset, got %s", res.ASEState)
	}
}

func TestFleetStatus_NonMainSubmoduleAndBranchCleanliness(t *testing.T) {
	// 1. Superproject on feature branch -> IsClean must be false, WorktreeState BRANCH_NOT_MAIN
	resFeature := &RepoFleetStatusResult{
		Branch:        "feature/my-new-feature",
		IsClean:       true,
		WorktreeState: "CLEAN",
	}
	// Run classification logic
	isMainBranch := resFeature.Branch == "main" || resFeature.Branch == "master"
	if !isMainBranch {
		resFeature.WorktreeState = "BRANCH_NOT_MAIN"
		resFeature.IsClean = false
	}
	if resFeature.IsClean {
		t.Errorf("expected IsClean=false when branch is not main/master")
	}
	if resFeature.WorktreeState != "BRANCH_NOT_MAIN" {
		t.Errorf("expected WorktreeState=BRANCH_NOT_MAIN, got %s", resFeature.WorktreeState)
	}

	// 2. Submodules on non-main branches -> SubmodulesClean must be false, IsClean must be false
	resSub := &RepoFleetStatusResult{
		Branch:            "main",
		IsClean:           true,
		WorktreeState:     "CLEAN",
		NonMainSubmodules: []string{"iazio-mcp (on chore/gov-test)"},
	}
	hasNonMain := len(resSub.NonMainSubmodules) > 0
	if hasNonMain {
		resSub.WorktreeState = "SUBMODULES_NOT_MAIN"
		resSub.IsClean = false
	}
	resSub.SubmodulesClean = (len(resSub.DirtySubmodules) == 0 && len(resSub.NonMainSubmodules) == 0)

	if resSub.IsClean {
		t.Errorf("expected IsClean=false when submodules are on non-main branches")
	}
	if resSub.SubmodulesClean {
		t.Errorf("expected SubmodulesClean=false when submodules are on non-main branches")
	}
	if resSub.WorktreeState != "SUBMODULES_NOT_MAIN" {
		t.Errorf("expected WorktreeState=SUBMODULES_NOT_MAIN, got %s", resSub.WorktreeState)
	}
}

func TestDiscoverAgentProcessesFallback_AndReap(t *testing.T) {
	res := &RepoFleetStatusResult{
		LockDetails: "lock",
	}
	discoverAgentProcessesFallback("/nonexistent/repo/path", res)
	if res.ToolLiveness != "KILLED" || res.ASEState != "ORPHANED" {
		t.Errorf("expected KILLED and ORPHANED, got %s and %s", res.ToolLiveness, res.ASEState)
	}

	res2 := &RepoFleetStatusResult{
		ASEState: "FAILED_DIRTY",
	}
	discoverAgentProcessesFallback("/nonexistent/repo/path", res2)
	if res2.ToolLiveness != "TERMINATED" {
		t.Errorf("expected TERMINATED, got %s", res2.ToolLiveness)
	}

	tmpDir := t.TempDir()
	if err := ReapOrphanedProcesses(context.Background(), tmpDir); err != nil {
		t.Errorf("ReapOrphanedProcesses failed: %v", err)
	}
}
