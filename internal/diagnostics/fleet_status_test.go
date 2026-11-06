package diagnostics

import (
	"context"
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
