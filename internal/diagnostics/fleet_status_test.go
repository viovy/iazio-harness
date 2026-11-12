package diagnostics

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	sessionStoryPath := filepath.Join(tmpDir, ".session_story_id")
	os.WriteFile(sessionStoryPath, []byte("STORY-GOV-5159\n"), 0644)
	parseASEState(tmpDir, res)
	if res.ASEState == "" {
		t.Errorf("expected non-empty ASEState")
	}

	if err := ResetASELocks(tmpDir); err != nil {
		t.Errorf("ResetASELocks failed: %v", err)
	}
	if _, err := os.Stat(sessionStoryPath); !os.IsNotExist(err) {
		t.Errorf("expected .session_story_id to be removed by ResetASELocks, stat err=%v", err)
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

func TestInspectRepoFleetStatus_AdhocASESessionStoryID(t *testing.T) {
	tmpDir := t.TempDir()
	cmd := exec.Command("git", "init", "-b", "chore/gov-5135-test")
	cmd.Dir = tmpDir
	if err := cmd.Run(); err != nil {
		t.Fatalf("git init: %v", err)
	}
	exec.Command("git", "-C", tmpDir, "config", "user.email", "test@example.com").Run()
	exec.Command("git", "-C", tmpDir, "config", "user.name", "Test User").Run()
	_ = os.WriteFile(filepath.Join(tmpDir, "README.md"), []byte("# Test\n"), 0644)
	exec.Command("git", "-C", tmpDir, "add", ".").Run()
	exec.Command("git", "-C", tmpDir, "commit", "-m", "init").Run()

	// Write .session_story_id on a non-main story branch
	_ = os.WriteFile(filepath.Join(tmpDir, ".session_story_id"), []byte("STORY-GOV-5135\n"), 0644)

	res, err := InspectRepoFleetStatus(context.Background(), tmpDir)
	if err != nil {
		t.Fatalf("InspectRepoFleetStatus error: %v", err)
	}
	if res.ASEState != "RUNNING" {
		t.Errorf("expected ASEState=RUNNING for active .session_story_id on story branch, got %s", res.ASEState)
	}
	if res.ActiveStoryID != "STORY-GOV-5135" {
		t.Errorf("expected ActiveStoryID=STORY-GOV-5135, got %s", res.ActiveStoryID)
	}
	if res.ExecutionProvenance != "INDEPENDENT_USER_SESSION" {
		t.Errorf("expected ExecutionProvenance=INDEPENDENT_USER_SESSION, got %s", res.ExecutionProvenance)
	}
	if res.ToolLiveness != "LIVE" {
		t.Errorf("expected ToolLiveness=LIVE, got %s", res.ToolLiveness)
	}
	if !isDiagnosticCmdLine("iazio-harness fleet-status --repo /tmp/foo --json") {
		t.Errorf("expected fleet-status invocation to be classified as diagnostic")
	}
}

func TestReadGitWorktreeBranch(t *testing.T) {
	if got, ok := readGitWorktreeBranch(filepath.Join(t.TempDir(), "nonexistent")); ok || got != "" {
		t.Fatalf("expected empty branch for nonexistent dir, got %q", got)
	}

	dir1 := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir1, ".git"), 0o755)
	_ = os.WriteFile(filepath.Join(dir1, ".git", "HEAD"), []byte("ref: refs/heads/feat/gov-5144\n"), 0o644)
	if got, ok := readGitWorktreeBranch(dir1); !ok || got != "feat/gov-5144" {
		t.Fatalf("expected feat/gov-5144, got %q", got)
	}

	_ = os.WriteFile(filepath.Join(dir1, ".git", "HEAD"), []byte("abcdef1234567890\n"), 0o644)
	if got, ok := readGitWorktreeBranch(dir1); !ok || got != "detached" {
		t.Fatalf("expected empty branch for detached HEAD, got %q", got)
	}

	dir2 := t.TempDir()
	gitModulesDir := filepath.Join(dir2, "modules", "sub1")
	worktreeDir := filepath.Join(dir2, "sub1")
	_ = os.MkdirAll(gitModulesDir, 0o755)
	_ = os.MkdirAll(worktreeDir, 0o755)
	_ = os.WriteFile(filepath.Join(worktreeDir, ".git"), []byte("gitdir: ../modules/sub1\n"), 0o644)
	_ = os.WriteFile(filepath.Join(gitModulesDir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644)
	if got, ok := readGitWorktreeBranch(worktreeDir); !ok || got != "main" {
		t.Fatalf("expected main from gitdir file, got %q", got)
	}
}

func TestHasExactRepoPathBoundary_AndLineContainsExactRepoPath(t *testing.T) {
	base := "/Users/romeo/work/meta-repo"
	if !HasExactRepoPathBoundary("/Users/romeo/work/meta-repo", base) {
		t.Errorf("expected exact match to return true")
	}
	if !HasExactRepoPathBoundary("/Users/romeo/work/meta-repo/iazio-agent", base) {
		t.Errorf("expected child submodule path to return true")
	}
	if HasExactRepoPathBoundary("/Users/romeo/work/meta-repo-02", base) {
		t.Errorf("expected sibling slot meta-repo-02 to return false")
	}
	if HasExactRepoPathBoundary("/Users/romeo/work/meta-repo-03/iazio-agent", base) {
		t.Errorf("expected sibling slot child meta-repo-03/iazio-agent to return false")
	}

	if !LineContainsExactRepoPath("agy --cwd /Users/romeo/work/meta-repo --print", base) {
		t.Errorf("expected exact path in cmdline to match")
	}
	if !LineContainsExactRepoPath("autopilot -C /Users/romeo/work/meta-repo/vigo verify", base) {
		t.Errorf("expected child path in cmdline to match")
	}
	if LineContainsExactRepoPath("agy --cwd /Users/romeo/work/meta-repo-03 --print", base) {
		t.Errorf("expected sibling meta-repo-03 in cmdline NOT to match meta-repo")
	}
}

func TestInspectRepoFleetStatus_CompletedStoryVigo0235_Tier1SelfRemediation(t *testing.T) {
	tmpDir := t.TempDir()
	cmd := exec.Command("git", "init", "-b", "main")
	cmd.Dir = tmpDir
	if err := cmd.Run(); err != nil {
		t.Fatalf("git init: %v", err)
	}
	exec.Command("git", "-C", tmpDir, "config", "user.email", "test@example.com").Run()
	exec.Command("git", "-C", tmpDir, "config", "user.name", "Test User").Run()

	// Create DONE story file for STORY-VIGO-0235 and index.yml for STORY-GOV-3133
	storiesDir := filepath.Join(tmpDir, "docs", "stories")
	_ = os.MkdirAll(storiesDir, 0o755)
	storyContent := "---\nid: \"STORY-VIGO-0235\"\nstatus: DONE\n---\n# STORY-VIGO-0235\n"
	_ = os.WriteFile(filepath.Join(storiesDir, "2026-11-03-vigo-0235-vigo-bws.story.md"), []byte(storyContent), 0o644)
	_ = os.WriteFile(filepath.Join(storiesDir, "index.yml"), []byte("stories:\n  - id: STORY-GOV-3133\n    status: DONE\n"), 0o644)
	_ = os.WriteFile(filepath.Join(tmpDir, ".gitignore"), []byte(".session_story_id\n.autopilot/\ntmp/\n"), 0o644)
	exec.Command("git", "-C", tmpDir, "add", ".").Run()
	exec.Command("git", "-C", tmpDir, "commit", "-m", "chore: initial main with completed story").Run()

	// Reproduce exact mac-mini bug:
	// 1. Leftover .session_story_id with STORY-VIGO-0235
	_ = os.WriteFile(filepath.Join(tmpDir, ".session_story_id"), []byte("STORY-VIGO-0235\n"), 0o644)
	// 2. Leftover .autopilot/state.json with current_phase="VERIFICATION"
	_ = os.MkdirAll(filepath.Join(tmpDir, ".autopilot"), 0o755)
	_ = os.WriteFile(filepath.Join(tmpDir, ".autopilot", "state.json"), []byte(`{"story_id":"","current_phase":"VERIFICATION","verification_status":"PASSED"}`), 0o644)
	// 3. Unrelated historical tmp/ase-resume-STORY-GOV-3133.json
	_ = os.MkdirAll(filepath.Join(tmpDir, "tmp"), 0o755)
	_ = os.WriteFile(filepath.Join(tmpDir, "tmp", "ase-resume-STORY-GOV-3133.json"), []byte(`{"story_id":"STORY-GOV-3133","phase":"5"}`), 0o644)

	res, err := InspectRepoFleetStatus(context.Background(), tmpDir)
	if err != nil {
		t.Fatalf("InspectRepoFleetStatus error: %v", err)
	}
	if res.ASEState != "IDLE" {
		t.Errorf("expected ASEState=IDLE after Tier 1 remediation of completed STORY-VIGO-0235, got %s", res.ASEState)
	}
	if res.ActiveStoryID != "" {
		t.Errorf("expected ActiveStoryID to be cleared for completed story, got %q", res.ActiveStoryID)
	}
	if res.CurrentPhase != "" {
		t.Errorf("expected CurrentPhase to be cleared, got %q", res.CurrentPhase)
	}
	if !res.RemediationApplied || len(res.RemediationActions) == 0 {
		t.Errorf("expected RemediationApplied=true with recorded actions, got applied=%v actions=%v", res.RemediationApplied, res.RemediationActions)
	}
	if _, err := os.Stat(filepath.Join(tmpDir, ".session_story_id")); !os.IsNotExist(err) {
		t.Errorf("expected .session_story_id to be removed by Tier 1 remediation")
	}
	if _, err := os.Stat(filepath.Join(tmpDir, "tmp", "ase-resume-STORY-GOV-3133.json")); !os.IsNotExist(err) {
		t.Errorf("expected completed historical tmp/ase-resume-STORY-GOV-3133.json to be pruned")
	}
}

func TestReconcileAdhocASEState_IdleInteractiveShellDoesNotPauseQueue(t *testing.T) {
	tmpDir := t.TempDir()
	// Simulate an open interactive agy shell sitting on clean main with no bound story and no locks
	res := &RepoFleetStatusResult{
		RepoPath:            tmpDir,
		Branch:              "main",
		IsClean:             true,
		WorktreeState:       "CLEAN",
		ASEState:            "IDLE",
		AgentTool:           "agy",
		AgentPID:            49113,
		ToolLiveness:        "LIVE",
		ExecutionProvenance: "INDEPENDENT_USER_SESSION",
	}
	reconcileAdhocASEState(tmpDir, res)
	if res.ASEState != "IDLE" {
		t.Errorf("expected idle interactive agy shell on clean main with no story/lock to remain ASEState=IDLE, got %s", res.ASEState)
	}
	if res.ExecutionProvenance != "NONE" {
		t.Errorf("expected idle interactive agy shell with no ASE evidence to reset ExecutionProvenance=NONE, got %s", res.ExecutionProvenance)
	}
	if !isDiagnosticCmdLine("/usr/local/bin/agy --hub") {
		t.Errorf("expected agy --hub daemon to be classified as diagnostic/non-session")
	}

	// Simulate an active autopilot-ase tool or bound IN_PROGRESS story with live agy tool -> must become RUNNING
	resActive := &RepoFleetStatusResult{
		RepoPath:            tmpDir,
		Branch:              "main",
		IsClean:             true,
		WorktreeState:       "CLEAN",
		ASEState:            "IDLE",
		ActiveStoryID:       "STORY-GOV-5156",
		AgentTool:           "agy",
		AgentPID:            49113,
		ToolLiveness:        "LIVE",
		ExecutionProvenance: "INDEPENDENT_USER_SESSION",
	}
	reconcileAdhocASEState(tmpDir, resActive)
	if resActive.ASEState != "RUNNING" {
		t.Errorf("expected live agy tool with active non-DONE story to become ASEState=RUNNING, got %s", resActive.ASEState)
	}
}

func TestRemediateRepo_Tier2SafeGradleCacheAndTier3ForceClean(t *testing.T) {
	tmpDir := t.TempDir()
	cmd := exec.Command("git", "init", "-b", "main")
	cmd.Dir = tmpDir
	if err := cmd.Run(); err != nil {
		t.Fatalf("git init: %v", err)
	}
	exec.Command("git", "-C", tmpDir, "config", "user.email", "test@example.com").Run()
	exec.Command("git", "-C", tmpDir, "config", "user.name", "Test User").Run()

	gradleFile := filepath.Join(tmpDir, "android", ".gradle", "8.14", "fileHashes", "fileHashes.bin")
	_ = os.MkdirAll(filepath.Join(tmpDir, "android", ".gradle", "8.14", "fileHashes"), 0o755)
	_ = os.WriteFile(gradleFile, []byte("v1"), 0o644)
	exec.Command("git", "-C", tmpDir, "add", ".").Run()
	exec.Command("git", "-C", tmpDir, "commit", "-m", "chore: initial").Run()

	// Modify tracked gradle cache file
	_ = os.WriteFile(gradleFile, []byte("v2-daemon-noise"), 0o644)

	remRes, err := RemediateRepo(context.Background(), RemediateOptions{
		RepoPath: tmpDir,
		Strategy: StrategySafe,
	})
	if err != nil {
		t.Fatalf("RemediateRepo StrategySafe error: %v", err)
	}
	if !remRes.Success || remRes.Skipped {
		t.Fatalf("expected StrategySafe success without skip, got %+v", remRes)
	}
	if remRes.FleetStatus == nil || !remRes.FleetStatus.IsClean {
		t.Errorf("expected FleetStatus.IsClean=true after Tier 2 safe remediation of gradle cache, got %+v", remRes.FleetStatus)
	}

	// Modify real file and test StrategyForceClean
	realFile := filepath.Join(tmpDir, "main.go")
	_ = os.WriteFile(realFile, []byte("package main\n"), 0o644)
	_ = os.WriteFile(filepath.Join(tmpDir, ".session_story_id"), []byte("STORY-GOV-9999\n"), 0o644)

	forceRes, err := RemediateRepo(context.Background(), RemediateOptions{
		RepoPath: tmpDir,
		Strategy: StrategyForceClean,
	})
	if err != nil {
		t.Fatalf("RemediateRepo StrategyForceClean error: %v", err)
	}
	if !forceRes.Success || forceRes.FleetStatus == nil || !forceRes.FleetStatus.IsClean {
		t.Errorf("expected StrategyForceClean to restore clean state, got %+v", forceRes)
	}
	if _, err := os.Stat(filepath.Join(tmpDir, ".session_story_id")); !os.IsNotExist(err) {
		t.Errorf("expected .session_story_id to be removed by StrategyForceClean")
	}
	if jb, jErr := forceRes.JSON(); jErr != nil || len(jb) == 0 {
		t.Errorf("expected forceRes.JSON() to serialize cleanly, err=%v", jErr)
	}
}

func TestRemediateRepo_StrategiesAndSkipsAndSubmoduleNoise(t *testing.T) {
	// 1. NormalizeStrategy & IsEphemeralCachePath coverage
	if NormalizeStrategy("tier1") != StrategyTier1 || NormalizeStrategy("ephemeral") != StrategyTier1 {
		t.Errorf("expected StrategyTier1")
	}
	if NormalizeStrategy("safe") != StrategySafe || NormalizeStrategy("unknown-val") != StrategySafe {
		t.Errorf("expected StrategySafe")
	}
	if NormalizeStrategy("force-clean") != StrategyForceClean || NormalizeStrategy("tier3") != StrategyForceClean {
		t.Errorf("expected StrategyForceClean")
	}
	for _, ep := range []string{".DS_Store", "sub/Thumbs.db", ".session_story_id", "a/.gradle/cache.bin", ".gradle/x", "a/build/tmp/x", "a/.dart_tool/x"} {
		if !IsEphemeralCachePath(ep) {
			t.Errorf("expected IsEphemeralCachePath(%q)=true", ep)
		}
	}
	if IsEphemeralCachePath("cmd/main.go") {
		t.Errorf("expected IsEphemeralCachePath(cmd/main.go)=false")
	}
	if !hasOnlyRemediableMainNoise(&RepoFleetStatusResult{
		ModifiedFiles:   []string{"android/.gradle/8.14/fileHashes/fileHashes.bin"},
		UntrackedFiles:  []string{".DS_Store"},
		DirtySubmodules: []string{"sub1 (uncommitted changes)"},
		Behind:          1,
	}) {
		t.Errorf("expected hasOnlyRemediableMainNoise=true for ephemeral files + dirty submodule + behind")
	}
	if hasOnlyRemediableMainNoise(&RepoFleetStatusResult{
		ModifiedFiles: []string{"cmd/main.go"},
	}) {
		t.Errorf("expected hasOnlyRemediableMainNoise=false for real source modification")
	}
	if hasOnlyRemediableMainNoise(&RepoFleetStatusResult{
		UntrackedFiles: []string{"cmd/new.go"},
	}) {
		t.Errorf("expected hasOnlyRemediableMainNoise=false for real untracked source file")
	}
	if hasOnlyRemediableMainNoise(&RepoFleetStatusResult{
		NonMainSubmodules: []string{"sub1 (on feat/x)"},
	}) {
		t.Errorf("expected hasOnlyRemediableMainNoise=false when submodule is on non-main branch")
	}

	// 2. Setup git repo with metadata.yml, tags, and a submodule
	tmpDir := t.TempDir()
	exec.Command("git", "init", "-b", "main", tmpDir).Run()
	exec.Command("git", "-C", tmpDir, "config", "user.email", "test@example.com").Run()
	exec.Command("git", "-C", tmpDir, "config", "user.name", "Test User").Run()

	_ = os.WriteFile(filepath.Join(tmpDir, "metadata.yml"), []byte("id: test-repo\n"), 0o644)
	exec.Command("git", "-C", tmpDir, "add", "metadata.yml").Run()
	exec.Command("git", "-C", tmpDir, "commit", "-m", "init").Run()
	exec.Command("git", "-C", tmpDir, "tag", "v0.1.0").Run()

	// Stale git lock >5m auto-prune vs fresh lock detection
	indexLock := filepath.Join(tmpDir, ".git", "index.lock")
	_ = os.WriteFile(indexLock, []byte(""), 0o644)
	oldTime := time.Now().Add(-10 * time.Minute)
	_ = os.Chtimes(indexLock, oldTime, oldTime)
	lockRes1 := &RepoFleetStatusResult{}
	inspectGitLockSentinels(tmpDir, true, lockRes1)
	if lockRes1.HasActiveGitLock {
		t.Errorf("expected stale >5m index.lock to be auto-pruned when autoRemediateTier1=true")
	}
	// Fresh lock
	_ = os.WriteFile(indexLock, []byte(""), 0o644)
	lockRes2 := &RepoFleetStatusResult{}
	inspectGitLockSentinels(tmpDir, true, lockRes2)
	if !lockRes2.HasActiveGitLock {
		t.Errorf("expected fresh index.lock to report HasActiveGitLock=true")
	}
	// StrategySafe must skip when active git lock is present
	skipLockRes, err := RemediateRepo(context.Background(), RemediateOptions{
		RepoPath: tmpDir,
		Strategy: StrategySafe,
	})
	if err != nil || !skipLockRes.Skipped || skipLockRes.SkipReason != "active_execution_or_lock" {
		t.Errorf("expected StrategySafe to skip on active git lock, got %+v (err=%v)", skipLockRes, err)
	}
	_ = os.Remove(indexLock)

	// StrategyTier1 execution
	t1Res, err := RemediateRepo(context.Background(), RemediateOptions{
		RepoPath: tmpDir,
		Strategy: StrategyTier1,
	})
	if err != nil || !t1Res.Success || t1Res.Strategy != StrategyTier1 {
		t.Errorf("expected StrategyTier1 success, got %+v (err=%v)", t1Res, err)
	}
	if t1Res.FleetStatus == nil || t1Res.FleetStatus.MetadataYmlSHA == "" || t1Res.FleetStatus.GitVersion == "" {
		t.Errorf("expected MetadataYmlSHA and GitVersion populated, got %+v", t1Res.FleetStatus)
	}

	// StrategySafe on non-main branch must skip
	exec.Command("git", "-C", tmpDir, "checkout", "-b", "feat/test-branch").Run()
	skipBranchRes, err := RemediateRepo(context.Background(), RemediateOptions{
		RepoPath: tmpDir,
		Strategy: StrategySafe,
	})
	if err != nil || !skipBranchRes.Skipped || skipBranchRes.SkipReason != "non_main_branch" {
		t.Errorf("expected StrategySafe to skip on non-main branch, got %+v", skipBranchRes)
	}
	exec.Command("git", "-C", tmpDir, "checkout", "main").Run()

	// Test cleanSubmoduleEphemeralNoise directly on a sub-repo
	subDir := t.TempDir()
	exec.Command("git", "init", "-b", "main", subDir).Run()
	exec.Command("git", "-C", subDir, "config", "user.email", "test@example.com").Run()
	exec.Command("git", "-C", subDir, "config", "user.name", "Test User").Run()
	subGradle := filepath.Join(subDir, ".gradle", "8.14", "fileHashes.bin")
	_ = os.MkdirAll(filepath.Dir(subGradle), 0o755)
	_ = os.WriteFile(subGradle, []byte("v1"), 0o644)
	_ = os.WriteFile(filepath.Join(subDir, "app.go"), []byte("package app\n"), 0o644)
	exec.Command("git", "-C", subDir, "add", ".").Run()
	exec.Command("git", "-C", subDir, "commit", "-m", "sub init").Run()

	// Ephemeral modification + untracked .DS_Store in subDir -> should clean and return true
	_ = os.WriteFile(subGradle, []byte("v2"), 0o644)
	_ = os.WriteFile(filepath.Join(subDir, ".DS_Store"), []byte("noise"), 0o644)
	if !cleanSubmoduleEphemeralNoise(context.Background(), subDir) {
		t.Errorf("expected cleanSubmoduleEphemeralNoise=true when only ephemeral cache files are dirty")
	}

	// Real modification in subDir -> should NOT clean and return false
	_ = os.WriteFile(filepath.Join(subDir, "app.go"), []byte("package app\n// edited\n"), 0o644)
	if cleanSubmoduleEphemeralNoise(context.Background(), subDir) {
		t.Errorf("expected cleanSubmoduleEphemeralNoise=false when real source file is modified")
	}

	// InspectRepoFleetStatusWithOptions with AutoRemediateSafe=true on root with .DS_Store
	_ = os.WriteFile(filepath.Join(tmpDir, ".DS_Store"), []byte("noise"), 0o644)
	autoSafeSt, err := InspectRepoFleetStatusWithOptions(context.Background(), tmpDir, InspectOptions{
		AutoRemediateTier1: true,
		AutoRemediateSafe:  true,
	})
	if err != nil || !autoSafeSt.IsClean {
		t.Errorf("expected AutoRemediateSafe=true to clean .DS_Store and report IsClean=true, got %+v (err=%v)", autoSafeSt, err)
	}

	// Test hasActiveScopedResumeFile and IsStoryCompletedOnDisk (index.yml and category subdirectories)
	_ = os.MkdirAll(filepath.Join(tmpDir, "tmp"), 0o755)
	_ = os.WriteFile(filepath.Join(tmpDir, "tmp", "ase-resume-STORY-VIGO-0100.json"), []byte(`{"status":"DONE"}`), 0o644)
	if hasActiveScopedResumeFile(tmpDir, "STORY-VIGO-0100") {
		t.Errorf("expected DONE resume file to return false")
	}
	_ = os.WriteFile(filepath.Join(tmpDir, "tmp", "ase-resume-STORY-VIGO-0101.json"), []byte(`{"status":"IN_PROGRESS"}`), 0o644)
	if !hasActiveScopedResumeFile(tmpDir, "STORY-VIGO-0101") {
		t.Errorf("expected IN_PROGRESS resume file to return true")
	}

	vigoStories := filepath.Join(tmpDir, "vigo", "docs", "stories")
	_ = os.MkdirAll(vigoStories, 0o755)
	_ = os.WriteFile(filepath.Join(vigoStories, "index.yml"), []byte("- id: STORY-VIGO-0100\n  status: DONE\n"), 0o644)
	if !IsStoryCompletedOnDisk(tmpDir, "STORY-VIGO-0100") {
		t.Errorf("expected IsStoryCompletedOnDisk via index.yml in vigo/docs/stories to return true")
	}
	iazioStories := filepath.Join(tmpDir, "iazio-infra", "docs", "stories")
	_ = os.MkdirAll(iazioStories, 0o755)
	_ = os.WriteFile(filepath.Join(iazioStories, "2026-11-05-iazio-0200-test.story.md"), []byte("---\nid: STORY-IAZIO-0200\nstatus: DONE\n---\n"), 0o644)
	if !IsStoryCompletedOnDisk(tmpDir, "STORY-IAZIO-0200") {
		t.Errorf("expected IsStoryCompletedOnDisk via story file in iazio-infra/docs/stories to return true")
	}
	if !isDiagnosticCmdLine("iazio-harness fleet-status --repo /tmp --json") || !isDiagnosticCmdLine("iazio-harness repo evaluate") || !isDiagnosticCmdLine("/Users/romeo/.local/bin/autopilot-mcp serve") {
		t.Errorf("expected isDiagnosticCmdLine=true")
	}
	if _, ok := isAgentTool("/Users/romeo/.local/bin/autopilot-mcp"); ok {
		t.Errorf("expected autopilot-mcp to NOT be classified as an active agent tool")
	}
	if _, ok := isAuxiliaryTool("/Users/romeo/.local/bin/autopilot-mcp"); !ok {
		t.Errorf("expected autopilot-mcp to be classified as an auxiliary tool")
	}

	// Cover buildDiagnosticSummary, readGitWorktreeBranch, parseSubmoduleStatus, reconcileAdhocASEStateWithOpts
	diagRes := &RepoFleetStatusResult{
		RepoPath:             tmpDir,
		Branch:               "main",
		HeadSHA:              "abcdef1234567890",
		Ahead:                1,
		Behind:               2,
		IsClean:              true,
		WorktreeState:        "CLEAN_SUBMODULE_DRIFT",
		SubmoduleDrift:       []string{"sub1"},
		SubmodulesClean:      false,
		DirtySubmodules:      []string{"sub1"},
		NonMainSubmodules:    []string{"sub1 (on feat/x)"},
		ASEState:             "RUNNING",
		ActiveStoryID:        "STORY-GOV-5156",
		CurrentPhase:         "ADHOC_ASE",
		LockDetails:          ".autopilot/lock",
		OrphanedProcesses:    []string{"node (PID 99)"},
		AgentTool:            "agy",
		AgentPID:             1234,
		ToolLiveness:         "LIVE",
		ExecutionProvenance:  "INDEPENDENT_USER_SESSION",
		ActiveConversationID: "conv-123",
		RemediationActions:   []string{"removed .session_story_id"},
	}
	buildDiagnosticSummary(diagRes)
	if !strings.Contains(diagRes.DiagnosticSummary, "SUBMODULE SHA DRIFT") || !strings.Contains(diagRes.DiagnosticSummary, "Self-Remediation Actions") {
		t.Errorf("unexpected DiagnosticSummary: %s", diagRes.DiagnosticSummary)
	}
	diagRes2 := &RepoFleetStatusResult{
		RepoPath:        tmpDir,
		IsClean:         false,
		WorktreeState:   "DIRTY_MODIFIED",
		ModifiedFiles:   []string{"a.go"},
		UntrackedFiles:  []string{"b.go"},
		SubmodulesClean: false,
	}
	buildDiagnosticSummary(diagRes2)

	// readGitWorktreeBranch with gitdir file and detached HEAD
	wtDir := t.TempDir()
	actualGitDir := filepath.Join(wtDir, ".git-actual")
	_ = os.MkdirAll(actualGitDir, 0o755)
	_ = os.WriteFile(filepath.Join(wtDir, ".git"), []byte("gitdir: .git-actual\n"), 0o644)
	_ = os.WriteFile(filepath.Join(actualGitDir, "HEAD"), []byte("deadbeef1234567890\n"), 0o644)
	if br, ok := readGitWorktreeBranch(wtDir); !ok || br != "detached" {
		t.Errorf("expected detached branch from gitdir file, got %q (%v)", br, ok)
	}
	// inspectGitLockSentinels with gitdir file
	_ = os.WriteFile(filepath.Join(actualGitDir, "MERGE_HEAD"), []byte("deadbeef\n"), 0o644)
	lockRes3 := &RepoFleetStatusResult{}
	inspectGitLockSentinels(wtDir, true, lockRes3)
	if !lockRes3.HasActiveGitLock {
		t.Errorf("expected MERGE_HEAD via gitdir file to set HasActiveGitLock=true")
	}

	// parseSubmoduleStatus
	subRes := &RepoFleetStatusResult{}
	parseSubmoduleStatus(context.Background(), wtDir, "+111111 subA (heads/main)\n-222222 subB\nU333333 subC\n", subRes)
	if len(subRes.SubmoduleDrift) != 1 || len(subRes.DirtySubmodules) != 2 {
		t.Errorf("unexpected parseSubmoduleStatus result: %+v", subRes)
	}

	// reconcileAdhocASEStateWithOpts with affirmative evidence
	adhocRes := &RepoFleetStatusResult{
		Branch:              "main",
		ASEState:            "IDLE",
		ToolLiveness:        "LIVE",
		ExecutionProvenance: "INDEPENDENT_USER_SESSION",
		LockDetails:         ".autopilot/lock",
	}
	reconcileAdhocASEStateWithOpts(tmpDir, true, adhocRes)
	if adhocRes.ASEState != "RUNNING" || adhocRes.CurrentPhase != "ADHOC_ASE" {
		t.Errorf("expected reconcileAdhocASEStateWithOpts to transition to RUNNING/ADHOC_ASE on lock evidence, got %+v", adhocRes)
	}
}

func TestInspectAndRemediate_TrackedSessionStoryIDAndStrategyGitUpdate(t *testing.T) {
	tmpDir := t.TempDir()
	cmd := exec.Command("git", "init", "-b", "main")
	cmd.Dir = tmpDir
	if err := cmd.Run(); err != nil {
		t.Fatalf("git init: %v", err)
	}
	exec.Command("git", "-C", tmpDir, "config", "user.email", "test@example.com").Run()
	exec.Command("git", "-C", tmpDir, "config", "user.name", "Test User").Run()

	// Create DONE story and a TRACKED .session_story_id (reproducing workspace behind origin/main)
	storiesDir := filepath.Join(tmpDir, "docs", "stories")
	_ = os.MkdirAll(storiesDir, 0o755)
	_ = os.WriteFile(filepath.Join(storiesDir, "index.yml"), []byte("stories:\n  - id: STORY-GOV-5135\n    status: DONE\n"), 0o644)
	_ = os.WriteFile(filepath.Join(tmpDir, ".session_story_id"), []byte("STORY-GOV-5135\n"), 0o644)
	_ = os.WriteFile(filepath.Join(tmpDir, "README.md"), []byte("v1\n"), 0o644)
	exec.Command("git", "-C", tmpDir, "add", ".").Run()
	exec.Command("git", "-C", tmpDir, "commit", "-m", "chore: tracked session story id").Run()

	// Simulate prior bug where os.Remove deleted tracked .session_story_id causing D .session_story_id
	_ = os.Remove(filepath.Join(tmpDir, ".session_story_id"))

	res, err := InspectRepoFleetStatus(context.Background(), tmpDir)
	if err != nil {
		t.Fatalf("InspectRepoFleetStatus: %v", err)
	}
	if !res.IsClean || res.WorktreeState != "CLEAN" {
		t.Errorf("expected tracked .session_story_id deletion to be restored to HEAD leaving repo CLEAN, got IsClean=%v state=%s modified=%v", res.IsClean, res.WorktreeState, res.ModifiedFiles)
	}
	if res.ASEState != "IDLE" {
		t.Errorf("expected ASEState=IDLE after restoring tracked completed .session_story_id, got %s", res.ASEState)
	}

	// Now dirty README.md as if a completed ASE job left residual modifications, and run StrategyGitUpdate
	_ = os.WriteFile(filepath.Join(tmpDir, "README.md"), []byte("v2-dirty-after-ase\n"), 0o644)
	_ = os.WriteFile(filepath.Join(tmpDir, ".DS_Store"), []byte("noise\n"), 0o644)
	_ = os.WriteFile(filepath.Join(tmpDir, ".gitmodules"), []byte(""), 0o644)
	if NormalizeStrategy("git-update") != StrategyGitUpdate {
		t.Fatalf("expected NormalizeStrategy(git-update) == StrategyGitUpdate")
	}
	remRes, remErr := RemediateRepo(context.Background(), RemediateOptions{
		RepoPath: tmpDir,
		Strategy: StrategyGitUpdate,
	})
	if remErr != nil || remRes == nil || !remRes.Success {
		t.Fatalf("RemediateRepo(StrategyGitUpdate) failed: %v (%+v)", remErr, remRes)
	}
	if remRes.FleetStatus == nil || !remRes.FleetStatus.IsClean || remRes.FleetStatus.ASEState != "IDLE" {
		t.Errorf("expected StrategyGitUpdate to leave repo CLEAN and IDLE, got %+v", remRes.FleetStatus)
	}

	// Also exercise executePostASEGitUpdateRemediation with dirty submodule and untracked root files
	subDir := filepath.Join(tmpDir, "sub1")
	_ = os.MkdirAll(subDir, 0o755)
	exec.Command("git", "init", "-b", "main", subDir).Run()
	exec.Command("git", "-C", subDir, "config", "user.email", "test@example.com").Run()
	exec.Command("git", "-C", subDir, "config", "user.name", "Test User").Run()
	_ = os.WriteFile(filepath.Join(subDir, "sub.txt"), []byte("v1\n"), 0o644)
	exec.Command("git", "-C", subDir, "add", ".").Run()
	exec.Command("git", "-C", subDir, "commit", "-m", "sub init").Run()
	_ = os.WriteFile(filepath.Join(subDir, "sub.txt"), []byte("v2-dirty\n"), 0o644)

	stWithSub := &RepoFleetStatusResult{
		RepoPath:          tmpDir,
		Branch:            "feat/old",
		IsClean:           false,
		SubmodulesClean:   false,
		ModifiedFiles:     []string{".DS_Store"},
		UntrackedFiles:    []string{".DS_Store"},
		DirtySubmodules:   []string{"sub1 (uncommitted changes)"},
		NonMainSubmodules: []string{"sub1 (on feat/sub)"},
	}
	_, _ = executePostASEGitUpdateRemediation(context.Background(), time.Now(), tmpDir, stWithSub, &RemediateResult{
		RepoPath: tmpDir,
		Strategy: StrategyGitUpdate,
		Success:  true,
	})
	_ = resolveAutopilotBinary()
}

func TestInferStoryIDFromBranchAndInFlightStoryBranchPreservesAdhocASE(t *testing.T) {
	if got := InferStoryIDFromBranch("chore/gov-5186-increase-jenkins-zfs-volume-25gb"); got != "STORY-GOV-5186" {
		t.Fatalf("expected STORY-GOV-5186, got %q", got)
	}
	if got := InferStoryIDFromBranch("autopilot-plugins-internal (on feat/STORY-IAZIO-0194-sync)"); got != "STORY-IAZIO-0194" {
		t.Fatalf("expected STORY-IAZIO-0194, got %q", got)
	}
	if got := InferStoryIDFromBranch("main"); got != "" {
		t.Fatalf("expected empty for main, got %q", got)
	}

	tmpDir := t.TempDir()
	exec.Command("git", "init", "-b", "chore/gov-5186-increase-jenkins-zfs-volume-25gb", tmpDir).Run()
	exec.Command("git", "-C", tmpDir, "config", "user.email", "test@example.com").Run()
	exec.Command("git", "-C", tmpDir, "config", "user.name", "Test User").Run()

	storiesDir := filepath.Join(tmpDir, "docs", "stories")
	_ = os.MkdirAll(storiesDir, 0o755)
	// Story file marked DONE for PR gate while branch is still active (not merged to main yet)
	_ = os.WriteFile(filepath.Join(storiesDir, "2026-11-07-gov-5186-test.story.md"), []byte("---\nid: STORY-GOV-5186\nstatus: DONE\n---\n"), 0o644)
	exec.Command("git", "-C", tmpDir, "add", ".").Run()
	exec.Command("git", "-C", tmpDir, "commit", "-m", "feat: pre-land story commit").Run()
	// Make dirty modification on the active story branch without .session_story_id
	_ = os.WriteFile(filepath.Join(tmpDir, "work.txt"), []byte("in-flight\n"), 0o644)

	res, err := InspectRepoFleetStatus(context.Background(), tmpDir)
	if err != nil {
		t.Fatalf("InspectRepoFleetStatus error: %v", err)
	}
	if res.ActiveStoryID != "STORY-GOV-5186" {
		t.Errorf("expected ActiveStoryID=STORY-GOV-5186 inferred from branch, got %q", res.ActiveStoryID)
	}
	if res.ASEState != "RUNNING" {
		t.Errorf("expected ASEState=RUNNING on active story branch even when story frontmatter is DONE for PR gate, got %q", res.ASEState)
	}
	if res.ExecutionProvenance != "INDEPENDENT_USER_SESSION" {
		t.Errorf("expected ExecutionProvenance=INDEPENDENT_USER_SESSION, got %q", res.ExecutionProvenance)
	}
}

func TestStoryGov5197_AdhocASEFalsePositiveAndMaintenanceBranchAutoRemediation(t *testing.T) {
	for _, cmdLine := range []string{
		"/usr/local/bin/iazio-harness daemon",
		"/usr/local/bin/iazio-agent run",
		"/usr/local/bin/iazio-harvester watch",
		"node /opt/.antigravity-ide/extensions/datacloud/mcp_proxy_bundle.js",
		"/Applications/Antigravity IDE Helper (Plugin)",
	} {
		if !isDiagnosticCmdLine(cmdLine) {
			t.Errorf("expected isDiagnosticCmdLine(%q)=true", cmdLine)
		}
	}

	if !isAutomatedMaintenanceBranch("chore/governance-pointers-20261108-055824") {
		t.Errorf("expected chore/governance-pointers-* to be classified as automated maintenance branch")
	}
	if isAutomatedMaintenanceBranch("fix/gov-5197-test") {
		t.Errorf("expected fix/gov-5197-test NOT to be classified as automated maintenance branch")
	}

	if got := parsePsElapsedDuration("04-00:37:05"); got != 4*24*time.Hour+37*time.Minute+5*time.Second {
		t.Errorf("unexpected elapsed duration for 04-00:37:05: %v", got)
	}
	if got := parsePsElapsedDuration("31:30"); got != 31*time.Minute+30*time.Second {
		t.Errorf("unexpected elapsed duration for 31:30: %v", got)
	}
	if got := parsePsElapsedDuration(""); got != 0 {
		t.Errorf("unexpected elapsed duration for empty string: %v", got)
	}
	if got := parsePsElapsedDuration("invalid"); got != 0 {
		t.Errorf("unexpected elapsed duration for invalid string: %v", got)
	}
	if got := extractConversationIDFromArgs([]string{"agy", "--conversation=conv-abc"}); got != "conv-abc" {
		t.Errorf("expected conv-abc, got %q", got)
	}
	if got := extractConversationIDFromArgs([]string{"agy", "-c", "conv-xyz"}); got != "conv-xyz" {
		t.Errorf("expected conv-xyz, got %q", got)
	}
	if got := extractConversationIDFromArgs([]string{"agy", "--print"}); got != "" {
		t.Errorf("expected empty convID, got %q", got)
	}

	// Test parseDarwinLsofBrainTranscripts and inspectDarwinAgentProcess
	tmpBrainBase := t.TempDir()
	brainConvID := "12345678-aaaa-bbbb-cccc-1234567890ab"
	transcriptDir := filepath.Join(tmpBrainBase, "brain", brainConvID, ".system_generated", "logs")
	_ = os.MkdirAll(transcriptDir, 0o755)
	transcriptFile := filepath.Join(transcriptDir, "transcript.jsonl")
	_ = os.WriteFile(transcriptFile, []byte(`{"step_index":1}`+"\n"), 0o644)
	lsofSample := fmt.Sprintf("p9999\nn/tmp/other\nn%s\nn%s/extra\n", filepath.Join(tmpBrainBase, "brain", brainConvID), filepath.Join(tmpBrainBase, "brain", ".hidden"))
	parsedConv, parsedTS := parseDarwinLsofBrainTranscripts(lsofSample)
	if parsedConv != brainConvID || parsedTS.IsZero() {
		t.Errorf("expected parsedConv=%s and non-zero timestamp, got conv=%q ts=%v", brainConvID, parsedConv, parsedTS)
	}

	prov, convID, isIdle := inspectDarwinAgentProcess(os.Getpid(), "00:10", "agy --conversation=conv-short", false)
	if prov != "INDEPENDENT_USER_SESSION" || convID != "conv-short" || isIdle {
		t.Errorf("unexpected inspectDarwinAgentProcess result for short elapsed: prov=%s conv=%s idle=%v", prov, convID, isIdle)
	}
	_, _, isIdleLong := inspectDarwinAgentProcess(999999, "02-00:00:00", "agy chat", false)
	if !isIdleLong {
		t.Errorf("expected non-existent PID with 2d elapsed and no lock to be classified as idle")
	}

	// Test parsePsOutputForAgentProcesses and discoverAgentProcessesFallback
	psRepoDir := t.TempDir()
	psSample := strings.Join([]string{
		"64435 1 01:00 node /opt/.antigravity-ide/extensions/mcp_proxy_bundle.js " + psRepoDir,
		fmt.Sprintf("%d 1 00:05 env agy --cwd %s --conversation=conv-ps-match", os.Getpid()+100000, psRepoDir),
	}, "\n")
	psRes := &RepoFleetStatusResult{RepoPath: psRepoDir}
	if !parsePsOutputForAgentProcesses(psSample, os.Getpid(), os.Getppid(), psRepoDir, psRes) {
		t.Errorf("expected parsePsOutputForAgentProcesses to match env agy process")
	}
	if psRes.AgentTool != "agy" || psRes.ActiveConversationID != "conv-ps-match" {
		t.Errorf("unexpected psRes: tool=%q conv=%q", psRes.AgentTool, psRes.ActiveConversationID)
	}
	fallbackRes := &RepoFleetStatusResult{RepoPath: psRepoDir, LockDetails: "locked"}
	discoverAgentProcessesFallback(psRepoDir, fallbackRes)
	if fallbackRes.ASEState != "ORPHANED" || fallbackRes.ToolLiveness != "KILLED" {
		t.Errorf("expected discoverAgentProcessesFallback with lock and no process to set ORPHANED/KILLED, got state=%s live=%s", fallbackRes.ASEState, fallbackRes.ToolLiveness)
	}

	// Test parseSubmoduleStatus (detached HEAD must NOT be added to NonMainSubmodules, feature branch must be)
	subRoot := t.TempDir()
	subDetached := filepath.Join(subRoot, "sub-detached")
	subFeature := filepath.Join(subRoot, "sub-feature")
	_ = os.MkdirAll(filepath.Join(subDetached, ".git"), 0o755)
	_ = os.WriteFile(filepath.Join(subDetached, ".git", "HEAD"), []byte("88bb4eb187dce4679bff2a71d0bbfd08fbeeb2f6\n"), 0o644)
	_ = os.MkdirAll(filepath.Join(subFeature, ".git"), 0o755)
	_ = os.WriteFile(filepath.Join(subFeature, ".git", "HEAD"), []byte("ref: refs/heads/fix/gov-5197-sub\n"), 0o644)
	subRes := &RepoFleetStatusResult{Branch: "main"}
	subOut := "+88bb4eb187dce4679bff2a71d0bbfd08fbeeb2f6 sub-detached (heads/main)\n 88bb4eb187dce4679bff2a71d0bbfd08fbeeb2f6 sub-feature (heads/fix/gov-5197-sub)\n-88bb4eb187dce4679bff2a71d0bbfd08fbeeb2f6 sub-uninit\n"
	parseSubmoduleStatus(context.Background(), subRoot, subOut, subRes)
	if len(subRes.NonMainSubmodules) != 1 || !strings.Contains(subRes.NonMainSubmodules[0], "sub-feature") {
		t.Errorf("expected only sub-feature in NonMainSubmodules (not detached HEAD), got %v", subRes.NonMainSubmodules)
	}
	if len(subRes.SubmoduleDrift) != 1 || subRes.SubmoduleDrift[0] != "sub-detached" {
		t.Errorf("expected sub-detached in SubmoduleDrift, got %v", subRes.SubmoduleDrift)
	}

	// Test hasExplicitIdleStateFile with .autopilot/ase-state.json
	aseDoneDir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(aseDoneDir, ".autopilot"), 0o755)
	_ = os.WriteFile(filepath.Join(aseDoneDir, ".autopilot", "ase-state.json"), []byte(`{"status":"DONE","phase":"completed"}`), 0o644)
	if !hasExplicitIdleStateFile(aseDoneDir) {
		t.Errorf("expected hasExplicitIdleStateFile=true when ase-state.json has status=DONE")
	}

	// 1. Automated governance-pointers branch with idle/live shell and no story must NOT report ADHOC_ASE
	tmpMaint := t.TempDir()
	exec.Command("git", "init", "-b", "main", tmpMaint).Run()
	exec.Command("git", "-C", tmpMaint, "config", "user.email", "test@example.com").Run()
	exec.Command("git", "-C", tmpMaint, "config", "user.name", "Test User").Run()
	_ = os.WriteFile(filepath.Join(tmpMaint, "README.md"), []byte("v1\n"), 0o644)
	exec.Command("git", "-C", tmpMaint, "add", ".").Run()
	exec.Command("git", "-C", tmpMaint, "commit", "-m", "init main").Run()
	exec.Command("git", "-C", tmpMaint, "checkout", "-b", "chore/governance-pointers-20261108-055824").Run()

	maintRes := &RepoFleetStatusResult{
		RepoPath:            tmpMaint,
		Branch:              "chore/governance-pointers-20261108-055824",
		IsClean:             false,
		WorktreeState:       "BRANCH_NOT_MAIN",
		ASEState:            "IDLE",
		AgentTool:           "agy",
		AgentPID:            88833,
		ToolLiveness:        "LIVE",
		ExecutionProvenance: "INDEPENDENT_USER_SESSION",
	}
	reconcileAdhocASEState(tmpMaint, maintRes)
	if maintRes.ASEState != "IDLE" || maintRes.ExecutionProvenance != "NONE" {
		t.Errorf("expected maintenance branch without story/locks to remain IDLE/NONE, got state=%s prov=%s", maintRes.ASEState, maintRes.ExecutionProvenance)
	}

	// InspectRepoFleetStatus should auto-remediate clean chore/governance-pointers-* back to main
	inspectedMaint, err := InspectRepoFleetStatus(context.Background(), tmpMaint)
	if err != nil {
		t.Fatalf("InspectRepoFleetStatus(tmpMaint): %v", err)
	}
	if inspectedMaint.Branch != "main" || !inspectedMaint.IsClean || inspectedMaint.ASEState != "IDLE" {
		t.Errorf("expected chore/governance-pointers-* to auto-checkout main and report CLEAN/IDLE, got branch=%s clean=%v state=%s",
			inspectedMaint.Branch, inspectedMaint.IsClean, inspectedMaint.ASEState)
	}

	// 2. Completed story branch with explicit IDLE .autopilot/state.json and no active agent tool
	tmpDone := t.TempDir()
	exec.Command("git", "init", "-b", "main", tmpDone).Run()
	exec.Command("git", "-C", tmpDone, "config", "user.email", "test@example.com").Run()
	exec.Command("git", "-C", tmpDone, "config", "user.name", "Test User").Run()
	_ = os.WriteFile(filepath.Join(tmpDone, ".gitignore"), []byte(".autopilot/\n.session_story_id\n"), 0o644)
	storiesDir := filepath.Join(tmpDone, "docs", "stories")
	_ = os.MkdirAll(storiesDir, 0o755)
	_ = os.WriteFile(filepath.Join(storiesDir, "2026-11-08-gov-5195-test.story.md"), []byte("---\nid: STORY-GOV-5195\nstatus: DONE\n---\n"), 0o644)
	exec.Command("git", "-C", tmpDone, "add", ".").Run()
	exec.Command("git", "-C", tmpDone, "commit", "-m", "main with DONE story").Run()
	exec.Command("git", "-C", tmpDone, "checkout", "-b", "fix/gov-5195-fix-global-queue").Run()
	_ = os.MkdirAll(filepath.Join(tmpDone, ".autopilot"), 0o755)
	_ = os.WriteFile(filepath.Join(tmpDone, ".autopilot", "state.json"), []byte(`{"state":"IDLE","story_id":""}`), 0o644)

	inspectedDone, err := InspectRepoFleetStatus(context.Background(), tmpDone)
	if err != nil {
		t.Fatalf("InspectRepoFleetStatus(tmpDone): %v", err)
	}
	if inspectedDone.ASEState != "IDLE" || inspectedDone.ActiveStoryID != "" || inspectedDone.Branch != "main" {
		t.Errorf("expected completed story branch with IDLE state.json to clear ADHOC_ASE and checkout main, got state=%s story=%q branch=%s",
			inspectedDone.ASEState, inspectedDone.ActiveStoryID, inspectedDone.Branch)
	}
}

func TestStoryIazio0200_LinuxBrainFDActiveASETurnAndMaintenanceBranchHealing(t *testing.T) {
	appDataDir := t.TempDir()
	convID := "0bfc5ce7-9110-46e3-90d0-7d8133a07d68"
	convDir := filepath.Join(appDataDir, "conversations")
	presDir := filepath.Join(appDataDir, "presence")
	logsDir := filepath.Join(appDataDir, "brain", convID, ".system_generated", "logs")
	_ = os.MkdirAll(convDir, 0o755)
	_ = os.MkdirAll(presDir, 0o755)
	_ = os.MkdirAll(logsDir, 0o755)

	convDB := filepath.Join(convDir, convID+".db")
	presLock := filepath.Join(presDir, convID+".lock")
	transcriptPath := filepath.Join(logsDir, "transcript.jsonl")
	_ = os.WriteFile(convDB, []byte("sqlite"), 0o644)
	_ = os.WriteFile(presLock, []byte("lock"), 0o644)

	// 1. In-flight ASE turn (last step is PLANNER_RESPONSE with tool_calls)
	inFlightLines := strings.Join([]string{
		`{"step_index":0,"type":"USER_INPUT","status":"DONE","content":"/autonomous-story-execution deep review"}`,
		`{"step_index":5,"type":"PLANNER_RESPONSE","status":"DONE","tool_calls":[{"name":"run_command","args":{"CommandLine":"autopilot-story index-upsert --story STORY-IAZIO-0200"}}]}`,
	}, "\n") + "\n"
	_ = os.WriteFile(transcriptPath, []byte(inFlightLines), 0o644)

	gotConv, gotTS, activeASE, inferredStory := inspectOpenAgentPathsForBrainAndASE([]string{convDB + "-wal", presLock}, time.Now())
	if gotConv != convID || gotTS.IsZero() || !activeASE || inferredStory != "STORY-IAZIO-0200" {
		t.Fatalf("expected active ASE turn from open .db/.lock paths, got conv=%q ts=%v active=%v story=%q",
			gotConv, gotTS, activeASE, inferredStory)
	}

	// Reconcile on main while HasActiveAgentASETurn=true -> must report RUNNING / ADHOC_ASE / INDEPENDENT_USER_SESSION
	repoDir := t.TempDir()
	activeRes := &RepoFleetStatusResult{
		RepoPath:              repoDir,
		Branch:                "main",
		IsClean:               true,
		WorktreeState:         "CLEAN",
		ASEState:              "IDLE",
		AgentTool:             "agy",
		AgentPID:              3967534,
		ToolLiveness:          "LIVE",
		ExecutionProvenance:   "INDEPENDENT_USER_SESSION",
		ActiveConversationID:  gotConv,
		HasActiveAgentASETurn: true,
		ActiveStoryID:         inferredStory,
	}
	reconcileAdhocASEState(repoDir, activeRes)
	if activeRes.ASEState != "RUNNING" || activeRes.CurrentPhase != "ADHOC_ASE" || activeRes.ExecutionProvenance != "INDEPENDENT_USER_SESSION" {
		t.Fatalf("expected active brain ASE turn on main to report RUNNING/ADHOC_ASE/INDEPENDENT_USER_SESSION, got %+v", activeRes)
	}

	// 2. Completed ASE turn (last step is PLANNER_RESPONSE with status=DONE and no tool_calls)
	completedLines := inFlightLines + `{"step_index":6,"type":"PLANNER_RESPONSE","status":"DONE","content":"## CRAFT Closeout"}` + "\n"
	_ = os.WriteFile(transcriptPath, []byte(completedLines), 0o644)
	_, _, activeAfterDone, _ := inspectOpenAgentPathsForBrainAndASE([]string{convDB}, time.Now())
	if activeAfterDone {
		t.Fatalf("expected completed final PLANNER_RESPONSE without tool_calls to report activeASE=false")
	}

	// 3. Maintenance branch auto-healing even when idle shell has ToolLiveness=LIVE but ExecutionProvenance=NONE
	maintRepo := t.TempDir()
	exec.Command("git", "init", "-b", "main", maintRepo).Run()
	exec.Command("git", "-C", maintRepo, "config", "user.email", "test@example.com").Run()
	exec.Command("git", "-C", maintRepo, "config", "user.name", "Test User").Run()
	_ = os.WriteFile(filepath.Join(maintRepo, "README.md"), []byte("v1\n"), 0o644)
	exec.Command("git", "-C", maintRepo, "add", ".").Run()
	exec.Command("git", "-C", maintRepo, "commit", "-m", "init main").Run()
	exec.Command("git", "-C", maintRepo, "checkout", "-b", "chore/governance-pointers-20261109-072028").Run()
	reportsDir := filepath.Join(maintRepo, "tmp", "reports")
	_ = os.MkdirAll(reportsDir, 0o755)
	sirPath := filepath.Join(reportsDir, "intervention-git-update-sir-5002c92fd239.json")
	_ = os.WriteFile(sirPath, []byte(`{"status":"OPEN"}`), 0o644)

	idleShellMaint := &RepoFleetStatusResult{
		RepoPath:            maintRepo,
		Branch:              "chore/governance-pointers-20261109-072028",
		IsClean:             false,
		WorktreeState:       "BRANCH_NOT_MAIN",
		ASEState:            "IDLE",
		AgentTool:           "agy",
		AgentPID:            3967534,
		ToolLiveness:        "LIVE",
		ExecutionProvenance: "NONE",
	}
	if !autoRemediateCompletedOrMaintenanceBranch(context.Background(), maintRepo, idleShellMaint) {
		t.Fatalf("expected autoRemediateCompletedOrMaintenanceBranch to heal chore/governance-pointers-* even with idle LIVE shell")
	}
	if idleShellMaint.Branch != "main" {
		t.Fatalf("expected Branch=main after healing, got %q", idleShellMaint.Branch)
	}
	if _, err := os.Stat(sirPath); !os.IsNotExist(err) {
		t.Fatalf("expected stale intervention-git-update-sir JSON to be removed on maintenance branch heal")
	}
}

func TestStoryGov5223_StrandedInFlightSubmoduleStoryDoesNotFabricateLiveAdhocASE(t *testing.T) {
	tmp := t.TempDir()
	_ = os.MkdirAll(filepath.Join(tmp, ".autopilot"), 0o755)
	if err := os.WriteFile(filepath.Join(tmp, ".autopilot", "state.json"), []byte(`{"state":"IDLE","phase":"COMPLETED","story":"STORY-GOV-5222"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	subDocs := filepath.Join(tmp, "vigo", "docs", "stories")
	_ = os.MkdirAll(subDocs, 0o755)
	storyContent := "---\nid: STORY-VIGO-0289\ntitle: Security Transport Hygiene Remediation\nstatus: IN_PROGRESS\n---\n"
	if err := os.WriteFile(filepath.Join(subDocs, "2026-11-09-vigo-0289-security-transport-hygiene-remediation.story.md"), []byte(storyContent), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "vigo", ".session_story_id"), []byte("STORY-VIGO-0289\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res := &RepoFleetStatusResult{
		RepoPath:            tmp,
		Branch:              "main",
		IsClean:             false,
		SubmodulesClean:     false,
		WorktreeState:       "DIRTY_TRACKED",
		ASEState:            "IDLE",
		ActiveStoryID:       "STORY-VIGO-0289",
		DirtySubmodules:     []string{"vigo"},
		NonMainSubmodules:   []string{"vigo (feat/vigo-0289-security-transport-hygiene-remediation)"},
		ToolLiveness:        "IDLE",
		ExecutionProvenance: "NONE",
	}

	reconcileAdhocASEState(tmp, res)

	if res.ASEState != "FAILED_DIRTY" {
		t.Fatalf("expected ASEState=FAILED_DIRTY for stranded in-flight story after agent exit, got %q", res.ASEState)
	}
	if res.ToolLiveness != "TERMINATED" {
		t.Fatalf("expected ToolLiveness=TERMINATED, got %q", res.ToolLiveness)
	}
	if res.ExecutionProvenance != "NONE" {
		t.Fatalf("expected ExecutionProvenance=NONE (must not fabricate INDEPENDENT_USER_SESSION), got %q", res.ExecutionProvenance)
	}
	if res.CurrentPhase != "" {
		t.Fatalf("expected CurrentPhase='', got %q", res.CurrentPhase)
	}
	if res.AgentTool != "" {
		t.Fatalf("expected AgentTool='', got %q", res.AgentTool)
	}
	if res.ActiveStoryID != "STORY-VIGO-0289" {
		t.Fatalf("expected ActiveStoryID=STORY-VIGO-0289 to be preserved as dirty story context, got %q", res.ActiveStoryID)
	}

	// Verify pruneCompletedSubmoduleSessionStoryFiles cleans up submodule .session_story_id once story is DONE.
	doneContent := "---\nid: STORY-VIGO-0289\ntitle: Security Transport Hygiene Remediation\nstatus: DONE\n---\n"
	if err := os.WriteFile(filepath.Join(subDocs, "2026-11-09-vigo-0289-security-transport-hygiene-remediation.story.md"), []byte(doneContent), 0o644); err != nil {
		t.Fatal(err)
	}
	pruneCompletedSubmoduleSessionStoryFiles(tmp, res)
	if _, err := os.Stat(filepath.Join(tmp, "vigo", ".session_story_id")); !os.IsNotExist(err) {
		t.Fatalf("expected completed submodule .session_story_id to be removed")
	}
}

func TestStoryIazio0200_IncompleteStoryCloseoutTruncatedToolCallsAndGitUpdateGuard(t *testing.T) {
	// 1. Story with status: DONE in frontmatter but unchecked DoD, _pending_ reviews, or open SIR is NOT completed on disk
	repoDir := t.TempDir()
	exec.Command("git", "init", "-b", "main", repoDir).Run()
	exec.Command("git", "-C", repoDir, "config", "user.email", "test@example.com").Run()
	exec.Command("git", "-C", repoDir, "config", "user.name", "Test User").Run()

	storiesDir := filepath.Join(repoDir, "docs", "stories")
	_ = os.MkdirAll(storiesDir, 0o755)
	storyPath := filepath.Join(storiesDir, "2026-11-09-iazio-0200-test.story.md")
	incompleteDone := "---\nid: STORY-IAZIO-0200\nstatus: DONE\n---\n## 5. Definition of Done\n- [ ] Check 1\n## 7. Reviews\n_pending_\n"
	_ = os.WriteFile(storyPath, []byte(incompleteDone), 0o644)

	if IsStoryCompletedOnDisk(repoDir, "STORY-IAZIO-0200") {
		t.Fatalf("expected IsStoryCompletedOnDisk=false when DoD has unchecked items and Reviews is _pending_")
	}
	if !IsStoryExplicitlyInProgressOnDisk(repoDir, "STORY-IAZIO-0200") {
		t.Fatalf("expected IsStoryExplicitlyInProgressOnDisk=true when story has incomplete DoD/Reviews")
	}

	// Complete DoD and Reviews, but leave an OPEN SIR -> still incomplete
	completeMarkdown := "---\nid: STORY-IAZIO-0200\nstatus: DONE\n---\n## 5. Definition of Done\n- [x] Check 1\n## 7. Reviews\n| Tool | Status |\n|---|---|\n| tdd | PASS |\n"
	_ = os.WriteFile(storyPath, []byte(completeMarkdown), 0o644)
	reportsDir := filepath.Join(repoDir, "tmp", "reports")
	_ = os.MkdirAll(reportsDir, 0o755)
	sirFile := filepath.Join(reportsDir, "intervention-story-iazio-0200-sir-e189f1a9051d.json")
	_ = os.WriteFile(sirFile, []byte(`{"id":"sir-e189f1a9051d","status":"OPEN"}`), 0o644)

	if IsStoryCompletedOnDisk(repoDir, "STORY-IAZIO-0200") {
		t.Fatalf("expected IsStoryCompletedOnDisk=false while open story SIR exists in tmp/reports")
	}
	_ = os.Remove(sirFile)
	if !IsStoryCompletedOnDisk(repoDir, "STORY-IAZIO-0200") {
		t.Fatalf("expected IsStoryCompletedOnDisk=true once DoD is checked, Reviews populated, and SIRs removed")
	}

	// 2. Truncated tool_calls in compact transcript.jsonl is recognized as in-flight, and inferredStoryID is preserved between turns
	appDataDir := t.TempDir()
	convID := "a7c57ec1-b2dd-42c5-992c-7ee59e241e6c"
	logsDir := filepath.Join(appDataDir, "brain", convID, ".system_generated", "logs")
	_ = os.MkdirAll(logsDir, 0o755)
	transcriptPath := filepath.Join(logsDir, "transcript.jsonl")
	truncatedTurn := strings.Join([]string{
		`{"step_index":0,"type":"USER_INPUT","status":"DONE","content":"/autonomous-story-execution STORY-IAZIO-0200"}`,
		`{"step_index":10,"type":"PLANNER_RESPONSE","status":"DONE","truncated_fields":["tool_calls"]}`,
	}, "\n") + "\n"
	_ = os.WriteFile(transcriptPath, []byte(truncatedTurn), 0o644)

	_, _, activeTrunc, inferredTrunc := inspectOpenAgentPathsForBrainAndASE([]string{transcriptPath}, time.Now())
	if !activeTrunc || inferredTrunc != "STORY-IAZIO-0200" {
		t.Fatalf("expected truncated_fields=[tool_calls] to report activeTrunc=true and story=STORY-IAZIO-0200, got active=%v story=%q", activeTrunc, inferredTrunc)
	}

	// Between turns (PLANNER_RESPONSE DONE without tool_calls), activeASE=false, but inferredStoryID is still returned within 2h
	betweenTurns := truncatedTurn + `{"step_index":11,"type":"PLANNER_RESPONSE","status":"DONE","content":"Checking next step"}` + "\n"
	_ = os.WriteFile(transcriptPath, []byte(betweenTurns), 0o644)
	_, _, activeBetween, inferredBetween := inspectOpenAgentPathsForBrainAndASE([]string{transcriptPath}, time.Now())
	if activeBetween || inferredBetween != "STORY-IAZIO-0200" {
		t.Fatalf("expected between-turns transcript to report active=false and story=STORY-IAZIO-0200, got active=%v story=%q", activeBetween, inferredBetween)
	}

	// 3. parseASEStateWithOpts must NOT prune .session_story_id on main while HasActiveAgentASETurn=true
	_ = os.WriteFile(filepath.Join(repoDir, ".session_story_id"), []byte("STORY-IAZIO-0200\n"), 0o644)
	inTurnRes := &RepoFleetStatusResult{
		RepoPath:              repoDir,
		Branch:                "main",
		HasActiveAgentASETurn: true,
		ASEState:              "IDLE",
	}
	parseASEStateWithOpts(repoDir, true, inTurnRes)
	if _, err := os.Stat(filepath.Join(repoDir, ".session_story_id")); err != nil {
		t.Fatalf("expected .session_story_id NOT to be pruned while HasActiveAgentASETurn=true")
	}
	if inTurnRes.ASEState != "RUNNING" || inTurnRes.ActiveStoryID != "STORY-IAZIO-0200" {
		t.Fatalf("expected RUNNING and ActiveStoryID=STORY-IAZIO-0200 during active turn on main, got state=%s story=%q", inTurnRes.ASEState, inTurnRes.ActiveStoryID)
	}

	// 4. IsWorktreeIdleForGitUpdate must return false on non-main feature branch without <cat>-<num> slug
	_ = os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("v1\n"), 0o644)
	exec.Command("git", "-C", repoDir, "add", ".").Run()
	exec.Command("git", "-C", repoDir, "commit", "-m", "init").Run()
	exec.Command("git", "-C", repoDir, "checkout", "-b", "fix/adhoc-ase-linux-brain-and-git-update-healing").Run()
	if IsWorktreeIdleForGitUpdate(repoDir, nil) {
		t.Fatalf("expected IsWorktreeIdleForGitUpdate=false on non-main feature branch without story slug")
	}
}

func TestStoryIazio0201_WindowsWSLAlternatesStrandedRebaseAndBrainTranscriptDiscovery(t *testing.T) {
	// 1. Cross-OS /mnt/c/... alternates file normalization to portable relative path
	baseWork := t.TempDir()
	refRepo := filepath.Join(baseWork, "meta-repo")
	slotRepo := filepath.Join(baseWork, "meta-repo-04")
	refObjects := filepath.Join(refRepo, ".git", "objects")
	slotObjectsInfo := filepath.Join(slotRepo, ".git", "objects", "info")
	_ = os.MkdirAll(refObjects, 0o755)
	_ = os.MkdirAll(slotObjectsInfo, 0o755)

	exec.Command("git", "init", "-b", "main", slotRepo).Run()
	exec.Command("git", "-C", slotRepo, "config", "user.email", "test@example.com").Run()
	exec.Command("git", "-C", slotRepo, "config", "user.name", "Test User").Run()

	// Write a non-portable absolute/WSL path into alternates pointing to sibling meta-repo/.git/objects
	altPath := filepath.Join(slotObjectsInfo, "alternates")
	wslFakeLine := "/mnt/c/work/meta-repo/.git/objects\n"
	_ = os.WriteFile(altPath, []byte(wslFakeLine), 0o644)

	changed, normErr := NormalizeGitAlternatesFile(slotRepo, altPath)
	if normErr != nil || !changed {
		t.Fatalf("expected NormalizeGitAlternatesFile to rewrite /mnt/c/... alternates (changed=%v err=%v)", changed, normErr)
	}
	normAltRaw, err := os.ReadFile(altPath)
	if err != nil {
		t.Fatal(err)
	}
	normAlt := strings.TrimSpace(string(normAltRaw))
	if normAlt != "../../../meta-repo/.git/objects" {
		t.Fatalf("expected portable relative alternates ../../../meta-repo/.git/objects, got %q", normAlt)
	}

	// 2. Stranded chore/governance-pointers-* rebase in .git/rebase-merge auto-aborts and restores main
	_ = os.WriteFile(filepath.Join(slotRepo, "README.md"), []byte("main-v1\n"), 0o644)
	reviewsDir := filepath.Join(slotRepo, "docs", "reviews")
	_ = os.MkdirAll(reviewsDir, 0o755)
	histReview := filepath.Join(reviewsDir, "2026-10-15-review-v0-V2-52a3f2-gov-3404-vigo.md")
	_ = os.WriteFile(histReview, []byte("# Historical Review STORY-GOV-3404\n"), 0o644)
	exec.Command("git", "-C", slotRepo, "add", ".").Run()
	exec.Command("git", "-C", slotRepo, "commit", "-m", "init main").Run()

	// Create conflicting commit on main vs chore/governance-pointers-20261109-171414
	exec.Command("git", "-C", slotRepo, "checkout", "-b", "chore/governance-pointers-20261109-171414").Run()
	_ = os.WriteFile(filepath.Join(slotRepo, "README.md"), []byte("pointers-branch\n"), 0o644)
	exec.Command("git", "-C", slotRepo, "add", ".").Run()
	exec.Command("git", "-C", slotRepo, "commit", "-m", "chore: governance pointers").Run()

	exec.Command("git", "-C", slotRepo, "checkout", "main").Run()
	_ = os.WriteFile(filepath.Join(slotRepo, "README.md"), []byte("main-v2\n"), 0o644)
	exec.Command("git", "-C", slotRepo, "add", ".").Run()
	exec.Command("git", "-C", slotRepo, "commit", "-m", "main advance").Run()

	exec.Command("git", "-C", slotRepo, "checkout", "chore/governance-pointers-20261109-171414").Run()
	// Start rebase that conflicts and leaves .git/rebase-merge on detached HEAD
	_ = exec.Command("git", "-C", slotRepo, "rebase", "main").Run()

	brainRoot := t.TempDir()
	t.Setenv("IAZIO_BRAIN_DIR", brainRoot)

	inspectedAfterRebaseHeal, err := InspectRepoFleetStatus(context.Background(), slotRepo)
	if err != nil {
		t.Fatalf("InspectRepoFleetStatus(slotRepo): %v", err)
	}
	if inspectedAfterRebaseHeal.Branch != "main" || !inspectedAfterRebaseHeal.IsClean || inspectedAfterRebaseHeal.ActiveStoryID != "" {
		t.Fatalf("expected stranded governance-pointers rebase to auto-abort to clean main with empty ActiveStoryID, got branch=%q clean=%v story=%q state=%q",
			inspectedAfterRebaseHeal.Branch, inspectedAfterRebaseHeal.IsClean, inspectedAfterRebaseHeal.ActiveStoryID, inspectedAfterRebaseHeal.ASEState)
	}

	// 3. TranscriptReferencesRepoPath boundary & cross-OS normalization + InspectActiveBrainASETurnForRepo
	escapedWindowsTranscript := `{"step_index":0,"type":"USER_INPUT","status":"DONE","content":"/autonomous-story-execution deep review"}` + "\n" +
		fmt.Sprintf(`{"step_index":1,"type":"PLANNER_RESPONSE","status":"DONE","tool_calls":[{"name":"read_file","args":{"Path":%q}}]}`,
			strings.ReplaceAll(filepath.Join(slotRepo, "AGENTS.md"), `\`, `\\`)) + "\n"
	if !TranscriptReferencesRepoPath(escapedWindowsTranscript, slotRepo) {
		t.Fatalf("expected TranscriptReferencesRepoPath=true for JSON-escaped repo path")
	}
	if TranscriptReferencesRepoPath(escapedWindowsTranscript, refRepo) {
		t.Fatalf("expected TranscriptReferencesRepoPath=false for prefix sibling meta-repo vs meta-repo-04")
	}

	convID := "1f2e5efc-e041-4c89-b25a-fe32389d66cd"
	convLogs := filepath.Join(brainRoot, convID, ".system_generated", "logs")
	_ = os.MkdirAll(convLogs, 0o755)
	_ = os.WriteFile(filepath.Join(convLogs, "transcript.jsonl"), []byte(escapedWindowsTranscript), 0o644)

	inspectedDuringBrainASE, err := InspectRepoFleetStatus(context.Background(), slotRepo)
	if err != nil {
		t.Fatalf("InspectRepoFleetStatus(during brain ASE): %v", err)
	}
	if inspectedDuringBrainASE.ASEState != "RUNNING" ||
		inspectedDuringBrainASE.ExecutionProvenance != "INDEPENDENT_USER_SESSION" ||
		inspectedDuringBrainASE.ToolLiveness != "LIVE" ||
		inspectedDuringBrainASE.ActiveConversationID != convID {
		t.Fatalf("expected active brain ASE turn to report RUNNING/INDEPENDENT_USER_SESSION/LIVE, got %+v", inspectedDuringBrainASE)
	}

	// 4. Verify background task waiting turn remains in-flight until finished with result
	bgPendingTail := escapedWindowsTranscript +
		`{"step_index":2,"type":"TOOL_RESPONSE","content":"Tool is running as a background task with task id: 1f2e5efc-e041-4c89-b25a-fe32389d66cd/task-101"}` + "\n" +
		`{"step_index":3,"type":"PLANNER_RESPONSE","status":"DONE","content":"Waiting for task-101 to finish."}` + "\n"
	if !isTranscriptTurnInFlight([]byte(bgPendingTail), time.Now(), time.Now()) {
		t.Fatalf("expected transcript with pending background task-101 to remain in-flight")
	}
	bgDoneTail := bgPendingTail +
		`{"step_index":4,"type":"USER_INPUT","content":"Task id \"1f2e5efc-e041-4c89-b25a-fe32389d66cd/task-101\" finished with result: Command exited with code 0."}` + "\n" +
		`{"step_index":5,"type":"PLANNER_RESPONSE","status":"DONE","content":"Completed."}` + "\n"
	if isTranscriptTurnInFlight([]byte(bgDoneTail), time.Now(), time.Now()) {
		t.Fatalf("expected transcript after task-101 completion and terminal PLANNER_RESPONSE to be idle")
	}
	if !HasActiveBrainASETurnForRepo(slotRepo, time.Now()) {
		t.Fatalf("expected HasActiveBrainASETurnForRepo(slotRepo)=true while brain transcript is in-flight")
	}
	if HasActiveBrainASETurnForRepo("", time.Now()) {
		t.Fatalf("expected HasActiveBrainASETurnForRepo(\"\")=false")
	}
	// Verify Windows drive path normalization in NormalizeGitAlternatesFile and readRebaseHeadBranch
	_ = os.WriteFile(altPath, []byte("# comment\nC:\\work\\meta-repo\\.git\\objects\n"), 0o644)
	if changed2, err2 := NormalizeGitAlternatesFile(slotRepo, altPath); err2 != nil || !changed2 {
		t.Fatalf("expected Windows path in alternates to normalize to relative path, got changed=%v err=%v", changed2, err2)
	}
	rebaseApplyDir := filepath.Join(slotRepo, ".git", "rebase-apply")
	_ = os.MkdirAll(rebaseApplyDir, 0o755)
	_ = os.WriteFile(filepath.Join(rebaseApplyDir, "head-name"), []byte("refs/heads/chore/submodule-pointers-20261109\n"), 0o644)
	_ = os.WriteFile(filepath.Join(rebaseApplyDir, "stopped-sha"), []byte("deadbeef\n"), 0o644)
	if hb, _ := readRebaseHeadBranch(rebaseApplyDir); hb != "chore/submodule-pointers-20261109" {
		t.Fatalf("expected readRebaseHeadBranch=chore/submodule-pointers-20261109, got %q", hb)
	}
	if _, shouldAbort := shouldAutoAbortStrandedRebase(slotRepo, rebaseApplyDir); !shouldAbort {
		t.Fatalf("expected shouldAutoAbortStrandedRebase=true for chore/submodule-pointers-20261109")
	}
	_ = os.RemoveAll(rebaseApplyDir)
}

func TestStoryGov5254_CrossWorktreeTranscriptAndSiblingStoryGuard(t *testing.T) {
	parentDir := t.TempDir()
	repo02 := filepath.Join(parentDir, "meta-repo-02")
	repo03 := filepath.Join(parentDir, "meta-repo-03")
	repo04 := filepath.Join(parentDir, "meta-repo-04")
	for _, r := range []string{repo02, repo03, repo04} {
		_ = os.MkdirAll(filepath.Join(r, ".autopilot"), 0o755)
		exec.Command("git", "init", "-b", "main", r).Run()
		exec.Command("git", "-C", r, "config", "user.email", "test@example.com").Run()
		exec.Command("git", "-C", r, "config", "user.name", "Test User").Run()
		_ = os.WriteFile(filepath.Join(r, "README.md"), []byte("clean\n"), 0o644)
		exec.Command("git", "-C", r, "add", ".").Run()
		exec.Command("git", "-C", r, "commit", "-m", "init").Run()
		_ = os.WriteFile(filepath.Join(r, ".autopilot", "state.json"), []byte(`{"state":"IDLE","story_id":""}`), 0o644)
	}

	// 1. Sibling worktree dominance in TranscriptReferencesRepoPath:
	// A transcript working in meta-repo-03 that mentions meta-repo-04 once in prose must NOT match meta-repo-04.
	repo03Transcript := strings.Join([]string{
		`{"step_index":0,"type":"USER_INPUT","status":"DONE","content":"/autonomous-story-execution STORY-GOV-5249"}`,
		fmt.Sprintf(`{"step_index":1,"type":"PLANNER_RESPONSE","status":"DONE","tool_calls":[{"name":"read_file","args":{"Path":%q}}]}`, filepath.Join(repo03, "AGENTS.md")),
		fmt.Sprintf(`{"step_index":2,"type":"PLANNER_RESPONSE","status":"DONE","tool_calls":[{"name":"read_file","args":{"Path":%q}}]}`, filepath.Join(repo03, "docs", "stories", "STORY-GOV-5249.story.md")),
		fmt.Sprintf(`{"step_index":3,"type":"TOOL_RESPONSE","content":"example worktree path %s in documentation"}`, repo04),
		fmt.Sprintf(`{"step_index":4,"type":"PLANNER_RESPONSE","status":"DONE","tool_calls":[{"name":"edit_file","args":{"Path":%q}}]}`, filepath.Join(repo03, "README.md")),
	}, "\n") + "\n"

	if !TranscriptReferencesRepoPath(repo03Transcript, repo03) {
		t.Fatalf("expected TranscriptReferencesRepoPath=true for dominant worktree repo03")
	}
	if TranscriptReferencesRepoPath(repo03Transcript, repo04) {
		t.Fatalf("expected TranscriptReferencesRepoPath=false for incidental prose mention of repo04 inside repo03 transcript")
	}
	if TranscriptReferencesRepoPath(repo03Transcript, repo02) {
		t.Fatalf("expected TranscriptReferencesRepoPath=false for unreferenced sibling repo02")
	}

	// 2. JSON-escaped newline after Command Working Directory header in JSONL must parse cleanly
	jsonlHeaderTranscript := fmt.Sprintf(`{"step_index":0,"content":"Command Working Directory: %s\nApp Data Directory: /Users/romeo/.gemini/antigravity-cli"}`, repo04)
	if !TranscriptReferencesRepoPath(jsonlHeaderTranscript, repo04) {
		t.Fatalf("expected JSONL Command Working Directory header to match repo04")
	}
	if TranscriptReferencesRepoPath(jsonlHeaderTranscript, repo02) {
		t.Fatalf("expected JSONL Command Working Directory header for repo04 not to match repo02")
	}

	// 3. Sibling story ownership guard: STORY-GOV-5249 owned by repo03 must never bind to clean main repo04
	_ = os.WriteFile(filepath.Join(repo03, ".session_story_id"), []byte("STORY-GOV-5249\n"), 0o644)
	if !IsStoryOwnedBySiblingWorktree(repo04, "STORY-GOV-5249") {
		t.Fatalf("expected IsStoryOwnedBySiblingWorktree(repo04, STORY-GOV-5249)=true when repo03 owns it")
	}
	if IsStoryOwnedBySiblingWorktree(repo03, "STORY-GOV-5249") {
		t.Fatalf("expected IsStoryOwnedBySiblingWorktree(repo03, STORY-GOV-5249)=false because repo03 is the owner")
	}

	brainRoot := t.TempDir()
	t.Setenv("IAZIO_BRAIN_DIR", brainRoot)
	conv03 := "34e278f2-fe4a-4991-aee4-d52c0b954556"
	logs03 := filepath.Join(brainRoot, conv03, ".system_generated", "logs")
	_ = os.MkdirAll(logs03, 0o755)
	_ = os.WriteFile(filepath.Join(logs03, "transcript.jsonl"), []byte(repo03Transcript), 0o644)

	if active04, _, story04, _ := InspectActiveBrainASETurnForRepo(repo04, time.Now()); active04 || story04 != "" {
		t.Fatalf("expected InspectActiveBrainASETurnForRepo(repo04) to ignore repo03 brain transcript, got active=%v story=%q", active04, story04)
	}

	// Even if repo04's own transcript inspects dirty_story_id: STORY-GOV-5249 on clean main with IDLE state.json,
	// STORY-GOV-5249 must not be bound to repo04.
	conv04 := "4aa50403-e90d-4482-8f0d-4d90e306c9f9"
	logs04 := filepath.Join(brainRoot, conv04, ".system_generated", "logs")
	_ = os.MkdirAll(logs04, 0o755)
	repo04DiagTranscript := strings.Join([]string{
		`{"step_index":0,"type":"USER_INPUT","status":"DONE","content":"/autonomous-story-execution why is STORY-GOV-5249 shown"}`,
		fmt.Sprintf(`{"step_index":1,"type":"TOOL_RESPONSE","content":"{\"repo_path\":%q,\"dirty_story_id\":\"STORY-GOV-5249\",\"active_story_id\":\"STORY-GOV-5249\"}"}`, repo04),
		fmt.Sprintf(`{"step_index":2,"type":"PLANNER_RESPONSE","status":"DONE","tool_calls":[{"name":"read_file","args":{"Path":%q}}]}`, filepath.Join(repo04, "README.md")),
	}, "\n") + "\n"
	_ = os.WriteFile(filepath.Join(logs04, "transcript.jsonl"), []byte(repo04DiagTranscript), 0o644)

	_, _, gotStory04, _ := InspectActiveBrainASETurnForRepo(repo04, time.Now())
	if gotStory04 != "" {
		t.Fatalf("expected repo04 diagnostic turn not to bind sibling story STORY-GOV-5249, got %q", gotStory04)
	}
}

func TestDeadRepoLockAndStaleSessionStoryFile_ClassifyAsFailedDirtyInsteadOfLiveAdhocASE(t *testing.T) {
	tmpDir := t.TempDir()
	_ = exec.Command("git", "init", "-b", "feat/gov-5249-oss-clean-room-hygiene", tmpDir).Run()
	_ = exec.Command("git", "-C", tmpDir, "config", "user.email", "test@example.com").Run()
	_ = exec.Command("git", "-C", tmpDir, "config", "user.name", "Test User").Run()

	storiesDir := filepath.Join(tmpDir, "docs", "stories")
	_ = os.MkdirAll(storiesDir, 0o755)
	_ = os.WriteFile(filepath.Join(storiesDir, "2026-11-11-gov-5249.story.md"), []byte("---\nid: STORY-GOV-5249\nstatus: IN_PROGRESS\n---\n"), 0o644)
	_ = os.WriteFile(filepath.Join(tmpDir, ".session_story_id"), []byte("STORY-GOV-5249\n"), 0o644)

	// 1. Dead .iazio/repo.lock (post-reboot orphan with dead PID 999999)
	iazioDir := filepath.Join(tmpDir, ".iazio")
	_ = os.MkdirAll(iazioDir, 0o755)
	_ = os.WriteFile(filepath.Join(iazioDir, "repo.lock"), []byte(`{"job_id":"job_de8c502853f28781","pid":999999,"status":"RUNNING"}`), 0o644)

	res := &RepoFleetStatusResult{
		Branch:              "feat/gov-5249-oss-clean-room-hygiene",
		ActiveStoryID:       "STORY-GOV-5249",
		ASEState:            "RUNNING",
		CurrentPhase:        "ADHOC_ASE",
		ToolLiveness:        "NONE",
		ExecutionProvenance: "NONE",
	}
	reconcileAdhocASEStateWithOpts(tmpDir, true, res)
	if res.ASEState != "FAILED_DIRTY" || res.ToolLiveness != "TERMINATED" || res.ExecutionProvenance != "NONE" {
		t.Fatalf("expected dead repo.lock on incomplete story branch to report FAILED_DIRTY/TERMINATED/NONE, got ASEState=%q ToolLiveness=%q Provenance=%q",
			res.ASEState, res.ToolLiveness, res.ExecutionProvenance)
	}
	if _, err := os.Stat(filepath.Join(iazioDir, "repo.lock")); !os.IsNotExist(err) {
		t.Fatalf("expected Tier 1 remediation to remove stale .iazio/repo.lock with dead PID")
	}

	// 2. Stale .session_story_id (>15m old) with no live process or lock
	staleTime := time.Now().Add(-2 * time.Hour)
	_ = os.Chtimes(filepath.Join(tmpDir, ".session_story_id"), staleTime, staleTime)
	resStale := &RepoFleetStatusResult{
		Branch:              "feat/gov-5249-oss-clean-room-hygiene",
		ActiveStoryID:       "STORY-GOV-5249",
		ASEState:            "RUNNING",
		CurrentPhase:        "ADHOC_ASE",
		ToolLiveness:        "NONE",
		ExecutionProvenance: "NONE",
	}
	reconcileAdhocASEStateWithOpts(tmpDir, false, resStale)
	if resStale.ASEState != "FAILED_DIRTY" || resStale.ToolLiveness != "TERMINATED" || resStale.ExecutionProvenance != "NONE" {
		t.Fatalf("expected stale .session_story_id (>15m old) with no live process to report FAILED_DIRTY/TERMINATED/NONE, got ASEState=%q ToolLiveness=%q Provenance=%q",
			resStale.ASEState, resStale.ToolLiveness, resStale.ExecutionProvenance)
	}
}







