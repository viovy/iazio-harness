// Package diagnostics provides deep inspection of fleet git status, ASE state, and agent provenance.
package diagnostics

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/viovy/iazio-core/pkg/procutil"
	"github.com/viovy/iazio-harness/internal/agyhooks"
	"github.com/viovy/iazio-harness/internal/repolock"
)

var (
	branchExplicitStoryRe  = regexp.MustCompile(`(?i)\b(STORY-[A-Z0-9]+-[0-9]+)\b`)
	branchSlugStoryRe      = regexp.MustCompile(`(?i)^(?:chore|feat|fix|refactor|docs|test|ci|perf|build)/(?:story-)?([a-z]{2,10})-([0-9]{2,6})(?:[-/]|$)`)
	transcriptBoundStoryRe = regexp.MustCompile(`(?i)(?:--story(?:-id)?[=\s\\"']+|story\s+begin\s+|/autonomous-story-execution[^\n]{0,120}?|(?:^|[^a-z0-9_])story_id[\\"\s:=]+|session_story_id[^\n]{0,80}?)(STORY-[A-Z0-9]+-[0-9]+)`)
)

// InferStoryIDFromBranch extracts a canonical STORY-<CAT>-<NUM> identifier from a story branch name
// (e.g. "chore/gov-5186-increase-jenkins-zfs-volume-25gb" -> "STORY-GOV-5186") or submodule status entry.
func InferStoryIDFromBranch(branch string) string {
	b := strings.TrimSpace(branch)
	if b == "" || b == "main" || b == "master" || b == "HEAD" || b == "detached" {
		return ""
	}
	if idx := strings.Index(b, "(on "); idx >= 0 {
		b = strings.TrimSuffix(strings.TrimSpace(b[idx+4:]), ")")
	}
	if m := branchExplicitStoryRe.FindStringSubmatch(b); len(m) == 2 {
		return strings.ToUpper(m[1])
	}
	if m := branchSlugStoryRe.FindStringSubmatch(b); len(m) == 3 {
		return fmt.Sprintf("STORY-%s-%s", strings.ToUpper(m[1]), m[2])
	}
	return ""
}

// InspectOptions configures repository evaluation and inline self-remediation behavior.
type InspectOptions struct {
	AutoRemediateTier1 bool `json:"auto_remediate_tier1"`
	AutoRemediateSafe  bool `json:"auto_remediate_safe"`
}

// RepoFleetStatusResult contains the complete diagnostic posture of a repository and active agent tooling.
type RepoFleetStatusResult struct {
	RepoPath             string   `json:"repo_path"`
	Branch               string   `json:"branch"`
	HeadSHA              string   `json:"head_sha"`
	IsClean              bool     `json:"is_clean"`
	Ahead                int      `json:"ahead"`
	Behind               int      `json:"behind"`
	DirtyFilesCount      int      `json:"dirty_files_count"`
	UntrackedFilesCount  int      `json:"untracked_files_count"`
	HasActiveGitLock     bool     `json:"has_active_git_lock"`
	ModifiedFiles        []string `json:"modified_files"`
	UntrackedFiles       []string `json:"untracked_files"`
	SubmodulesClean      bool     `json:"submodules_clean"`
	DirtySubmodules      []string `json:"dirty_submodules"`
	NonMainSubmodules    []string `json:"non_main_submodules,omitempty"`
	WorktreeState        string   `json:"worktree_state"` // CLEAN, CLEAN_SUBMODULE_DRIFT, DIRTY_UNTRACKED_ONLY, DIRTY_MODIFIED, BRANCH_NOT_MAIN, SUBMODULES_NOT_MAIN
	SubmoduleDrift       []string `json:"submodule_drift,omitempty"`
	OrphanedProcesses    []string `json:"orphaned_processes,omitempty"`
	ASEState             string   `json:"ase_state"` // IDLE, RUNNING, SUSPENDED_HITL, FAILED_DIRTY, ORPHANED
	ActiveStoryID        string   `json:"active_story_id,omitempty"`
	CurrentPhase         string   `json:"current_phase,omitempty"`
	LockDetails          string   `json:"lock_details,omitempty"`
	AgentTool            string   `json:"agent_tool,omitempty"`
	AgentPID             int      `json:"agent_pid,omitempty"`
	ToolLiveness           string   `json:"tool_liveness"`        // LIVE, TERMINATED, KILLED, NONE
	ExecutionProvenance    string   `json:"execution_provenance"` // INDEPENDENT_USER_SESSION, COORDINATED_HARNESS, COORDINATED_HARNESS_FAILED, NONE
	ActiveConversationID   string   `json:"active_conversation_id,omitempty"`
	HasActiveAgentASETurn  bool     `json:"has_active_agent_ase_turn,omitempty"`
	MetadataYmlRaw         string   `json:"metadata_yml_raw,omitempty"`
	MetadataYmlSHA       string   `json:"metadata_yml_sha,omitempty"`
	MetadataYmlModTime   string   `json:"metadata_yml_mod_time,omitempty"`
	GitVersion           string   `json:"git_version,omitempty"`
	RemediationApplied   bool     `json:"remediation_applied,omitempty"`
	RemediationActions   []string `json:"remediation_actions,omitempty"`
	DiagnosticSummary    string   `json:"diagnostic_summary"`
	CheckedAt            string   `json:"checked_at"`
}

// InspectRepoFleetStatus performs deep forensic inspection of a git repo worktree,
// submodules, ASE lifecycle state, and running agent processes with Tier 1 inline self-remediation enabled.
func InspectRepoFleetStatus(ctx context.Context, repoPath string) (*RepoFleetStatusResult, error) {
	return InspectRepoFleetStatusWithOptions(ctx, repoPath, InspectOptions{
		AutoRemediateTier1: true,
		AutoRemediateSafe:  false,
	})
}

// InspectRepoFleetStatusWithOptions performs deep forensic inspection of a git repo worktree
// with configurable Tier 1 (inline completed-ASE state cleanup) and Tier 2 (safe idle-main) remediation.
func InspectRepoFleetStatusWithOptions(ctx context.Context, repoPath string, opts InspectOptions) (*RepoFleetStatusResult, error) {
	cleanPath := filepath.Clean(repoPath)
	if r, err := filepath.EvalSymlinks(cleanPath); err == nil {
		cleanPath = r
	}

	res := &RepoFleetStatusResult{
		RepoPath:            cleanPath,
		IsClean:             true,
		SubmodulesClean:     true,
		WorktreeState:       "CLEAN",
		ASEState:            "IDLE",
		ToolLiveness:        "NONE",
		ExecutionProvenance: "NONE",
		CheckedAt:           time.Now().UTC().Format(time.RFC3339),
	}

	// 0a. Normalize cross-OS WSL/Windows git object alternates to portable relative paths before git status runs
	normalizeGitAlternatesPaths(cleanPath, opts.AutoRemediateTier1 || opts.AutoRemediateSafe, res)

	// 0b. Check git lock sentinels (prune abandoned >5m .lock files and abort stranded maintenance/completed rebases)
	inspectGitLockSentinels(cleanPath, opts.AutoRemediateTier1 || opts.AutoRemediateSafe, res)

	// 0c. Sanitize user-level ~/.gemini/config/plugins when --auto-remediate is enabled outside go test.
	if opts.AutoRemediateSafe && !agyhooks.IsTestProcess() {
		if sanRes, sErr := agyhooks.SanitizeHome(""); sErr == nil && sanRes.Changed {
			res.RemediationApplied = true
			res.RemediationActions = append(res.RemediationActions,
				fmt.Sprintf("sanitized ~/.gemini/config/plugins (removed=%v, disabled=%v)", sanRes.RemovedPluginDirs, sanRes.DisabledPlugins))
		}
	}

	// 1. Git Status Porcelain v2 & Branch
	cmd := procutil.CommandContext(ctx, "git", "-C", cleanPath, "status", "--porcelain=v2", "--branch")
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
	out, err := cmd.CombinedOutput()
	if err == nil {
		parseGitPorcelainV2(string(out), res)
	} else {
		res.IsClean = false
		res.DiagnosticSummary += fmt.Sprintf("git status failed: %v\n", err)
	}

	// 1b. Check if branch is ahead of origin/main when upstream is unconfigured
	if res.Ahead == 0 && res.Branch != "" && res.Branch != "main" && res.Branch != "master" {
		revCmd := procutil.CommandContext(ctx, "git", "-C", cleanPath, "rev-list", "--count", "origin/main..HEAD")
		if revOut, err := revCmd.Output(); err == nil {
			if count, err := strconv.Atoi(strings.TrimSpace(string(revOut))); err == nil && count > 0 {
				res.Ahead = count
			}
		}
	}

	// 2. Git Submodules (use native C builtin submodule--helper status without --recursive so legacy nested gitlinks do not abort status)
	subCmd := procutil.CommandContext(ctx, "git", "-C", cleanPath, "submodule--helper", "status")
	subOut, err := subCmd.CombinedOutput()
	if err != nil {
		subCmd = procutil.CommandContext(ctx, "git", "-C", cleanPath, "submodule", "status")
		subOut, err = subCmd.CombinedOutput()
	}
	if err == nil || len(subOut) > 0 {
		parseSubmoduleStatus(ctx, cleanPath, string(subOut), res)
	}

	res.DirtyFilesCount = len(res.ModifiedFiles) + len(res.DirtySubmodules)
	res.UntrackedFilesCount = len(res.UntrackedFiles)

	// Classify overall cleanliness and worktree state
	hasModifiedFiles := len(res.ModifiedFiles) > 0
	hasUntrackedFiles := len(res.UntrackedFiles) > 0
	hasDirtySubmodules := len(res.DirtySubmodules) > 0
	hasSubmoduleDrift := len(res.SubmoduleDrift) > 0
	hasNonMainSubmodules := len(res.NonMainSubmodules) > 0
	isMainBranch := res.Branch == "main" || res.Branch == "master"

	if res.HasActiveGitLock || hasModifiedFiles || hasDirtySubmodules {
		res.WorktreeState = "DIRTY_MODIFIED"
		res.IsClean = false
	} else if hasUntrackedFiles {
		res.WorktreeState = "DIRTY_UNTRACKED_ONLY"
		res.IsClean = false
	} else if !isMainBranch {
		res.WorktreeState = "BRANCH_NOT_MAIN"
		res.IsClean = false
	} else if hasNonMainSubmodules {
		res.WorktreeState = "SUBMODULES_NOT_MAIN"
		res.IsClean = false
	} else if hasSubmoduleDrift {
		res.WorktreeState = "CLEAN_SUBMODULE_DRIFT"
		res.IsClean = true
	} else {
		res.WorktreeState = "CLEAN"
		res.IsClean = true
	}
	res.SubmodulesClean = (len(res.DirtySubmodules) == 0 && len(res.NonMainSubmodules) == 0)

	// 3. Process Discovery & Provenance (run before ASE state pruning so active brain ASE turns are never pruned mid-turn)
	discoverAgentProcesses(cleanPath, res)
	inferredBrainStoryID := res.ActiveStoryID
	// Clear transcript-prose-inferred story ID before authoritative on-disk/branch checks so
	// .autopilot/state.json, .session_story_id, and current branch always take precedence.
	res.ActiveStoryID = ""

	// 4. ASE State & Autopilot locks (with Tier 1 completed-ASE artifact cleanup when no active ASE turn is in flight)
	parseASEStateWithOpts(cleanPath, opts.AutoRemediateTier1, res)
	if res.ActiveStoryID == "" && inferredBrainStoryID != "" {
		if canBindTranscriptStoryToRepo(cleanPath, inferredBrainStoryID) &&
			(res.HasActiveAgentASETurn || IsStoryExplicitlyInProgressOnDisk(cleanPath, inferredBrainStoryID) || !isMainBranch || len(res.NonMainSubmodules) > 0) {
			res.ActiveStoryID = inferredBrainStoryID
		}
	}
	applyNoAgentFallbackState(res)

	// 4b. Reconcile ad-hoc ASE session state and provenance (strict evidence gate)
	reconcileAdhocASEStateWithOpts(cleanPath, opts.AutoRemediateTier1, res)

	// 4b2. Auto-remediate completed story branches or automated governance-pointers branches back to main when idle
	if (opts.AutoRemediateTier1 || opts.AutoRemediateSafe) && !isMainBranch {
		if autoRemediateCompletedOrMaintenanceBranch(ctx, cleanPath, res) {
			isMainBranch = res.Branch == "main" || res.Branch == "master"
		}
	}

	// 4c. Optional Tier 2 safe remediation when idle on main/master
	if opts.AutoRemediateSafe && (res.ASEState == "IDLE" || res.ASEState == "FAILED_DIRTY") && isMainBranch && res.LockDetails == "" && !res.HasActiveGitLock {
		needsSafeRemediation := (!res.IsClean && hasOnlyRemediableMainNoise(res)) || (res.IsClean && res.Behind > 0) || res.WorktreeState == "CLEAN_SUBMODULE_DRIFT"
		if needsSafeRemediation {
			if remRes, remErr := RemediateRepo(ctx, RemediateOptions{
				RepoPath: cleanPath,
				Strategy: StrategySafe,
			}); remErr == nil && remRes != nil && remRes.FleetStatus != nil && !remRes.Skipped {
				res = remRes.FleetStatus
			}
		}
	}

	// 5. Enrich metadata.yml snapshot and git describe version
	populateMetadataAndGitVersion(ctx, cleanPath, res)

	// 6. Finalize Diagnostic Summary
	buildDiagnosticSummary(res)

	return res, nil
}

func hasOnlyRemediableMainNoise(res *RepoFleetStatusResult) bool {
	if len(res.NonMainSubmodules) > 0 {
		return false
	}
	for _, m := range res.ModifiedFiles {
		if !IsEphemeralCachePath(m) {
			return false
		}
	}
	for _, u := range res.UntrackedFiles {
		if !IsEphemeralCachePath(u) {
			return false
		}
	}
	return true
}

func resolveRepoGitDir(repoPath string) string {
	gitDir := filepath.Join(repoPath, ".git")
	fi, err := os.Stat(gitDir)
	if err != nil {
		return ""
	}
	if !fi.IsDir() {
		if b, rErr := os.ReadFile(gitDir); rErr == nil {
			content := strings.TrimSpace(string(b))
			if strings.HasPrefix(content, "gitdir:") {
				target := strings.TrimSpace(strings.TrimPrefix(content, "gitdir:"))
				if !filepath.IsAbs(target) {
					target = filepath.Join(repoPath, target)
				}
				gitDir = target
			}
		}
	}
	return filepath.Clean(gitDir)
}

// NormalizeGitAlternatesFile rewrites cross-OS WSL (/mnt/<drive>/...) or non-portable absolute paths
// in a .git/objects/info/alternates file into portable slash-separated relative paths when the target
// objects directory exists on disk.
func NormalizeGitAlternatesFile(repoPath, altFile string) (bool, error) {
	raw, err := os.ReadFile(altFile)
	if err != nil {
		return false, err
	}
	objectsDir := filepath.Clean(filepath.Dir(filepath.Dir(altFile)))
	lines := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
	changed := false
	outLines := make([]string, 0, len(lines))

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			outLines = append(outLines, trimmed)
			continue
		}
		// Already a portable relative path that resolves on disk?
		if !strings.HasPrefix(trimmed, "/") && !strings.HasPrefix(trimmed, `\`) && !(len(trimmed) >= 2 && trimmed[1] == ':') {
			cand := filepath.Clean(filepath.Join(objectsDir, filepath.FromSlash(trimmed)))
			if fi, stErr := os.Stat(cand); stErr == nil && fi.IsDir() {
				normRel := filepath.ToSlash(trimmed)
				if normRel != trimmed {
					changed = true
				}
				outLines = append(outLines, normRel)
				continue
			}
		}

		targetDir := resolveAlternateObjectsTarget(repoPath, objectsDir, trimmed)
		if targetDir != "" {
			if rel, relErr := filepath.Rel(objectsDir, targetDir); relErr == nil && rel != "" && !strings.HasPrefix(rel, "/") {
				relSlash := filepath.ToSlash(rel)
				if relSlash != trimmed {
					changed = true
				}
				outLines = append(outLines, relSlash)
				continue
			}
		}
		outLines = append(outLines, trimmed)
	}

	if !changed {
		return false, nil
	}
	newContent := strings.TrimRight(strings.Join(outLines, "\n"), "\n") + "\n"
	if err := os.WriteFile(altFile, []byte(newContent), 0o644); err != nil {
		return false, err
	}
	return true, nil
}

func resolveAlternateObjectsTarget(repoPath, objectsDir, rawAlt string) string {
	slash := strings.ReplaceAll(strings.TrimSpace(rawAlt), `\`, `/`)
	var candidates []string

	// 1. Direct path as written
	candidates = append(candidates, filepath.FromSlash(slash))

	// 2. WSL /mnt/<drive>/<rest> <-> Windows <Drive>:\<rest>
	if strings.HasPrefix(strings.ToLower(slash), "/mnt/") && len(slash) > 6 && slash[6] == '/' {
		drive := strings.ToUpper(slash[5:6])
		rest := slash[7:]
		if runtime.GOOS == "windows" {
			candidates = append([]string{drive + `:\` + strings.ReplaceAll(rest, "/", `\`)}, candidates...)
		} else {
			candidates = append(candidates, drive+`:/`+rest)
		}
	} else if len(slash) >= 3 && slash[1] == ':' && slash[2] == '/' {
		drive := strings.ToLower(slash[0:1])
		rest := slash[3:]
		candidates = append([]string{"/mnt/" + drive + "/" + rest}, candidates...)
	}

	// 3. Sibling repository resolution relative to filepath.Dir(repoPath) (checked first so co-located reference repos win):
	// e.g. /mnt/c/work/meta-repo/.git/objects or /mnt/c/work/meta-repo/.git/modules/foo/objects
	// when repoPath is <parent>/meta-repo-04 and <parent>/meta-repo/.git/... exists.
	if repoPath != "" {
		parentDir := filepath.Dir(filepath.Clean(repoPath))
		if idx := strings.Index(slash, "/.git/"); idx > 0 {
			prefix := slash[:idx]
			gitSuffix := slash[idx+1:] // ".git/objects" or ".git/modules/..."
			siblingName := filepath.Base(filepath.FromSlash(prefix))
			if siblingName != "" && siblingName != "." && siblingName != "/" {
				candidates = append([]string{filepath.Join(parentDir, siblingName, filepath.FromSlash(gitSuffix))}, candidates...)
			}
		}
	}

	for _, cand := range candidates {
		cleanCand := filepath.Clean(cand)
		if fi, err := os.Stat(cleanCand); err == nil && fi.IsDir() {
			return cleanCand
		}
	}
	return ""
}

func normalizeGitAlternatesPaths(repoPath string, autoRemediate bool, res *RepoFleetStatusResult) {
	if !autoRemediate || strings.TrimSpace(repoPath) == "" {
		return
	}
	gitDir := resolveRepoGitDir(repoPath)
	if gitDir == "" {
		return
	}
	var altFiles []string
	rootAlt := filepath.Join(gitDir, "objects", "info", "alternates")
	if _, err := os.Stat(rootAlt); err == nil {
		altFiles = append(altFiles, rootAlt)
	}
	for _, pattern := range []string{
		filepath.Join(gitDir, "modules", "*", "objects", "info", "alternates"),
		filepath.Join(gitDir, "modules", "*", "*", "objects", "info", "alternates"),
	} {
		if matches, err := filepath.Glob(pattern); err == nil {
			altFiles = append(altFiles, matches...)
		}
	}
	normalizedCount := 0
	for _, f := range altFiles {
		if ok, err := NormalizeGitAlternatesFile(repoPath, f); err == nil && ok {
			normalizedCount++
		}
	}
	if normalizedCount > 0 && res != nil {
		res.RemediationApplied = true
		res.RemediationActions = append(res.RemediationActions, fmt.Sprintf("normalized %d git object alternates file(s) to portable relative paths", normalizedCount))
	}
}

func readRebaseHeadBranch(sentinelPath string) (branch string, latestMod time.Time) {
	fi, err := os.Stat(sentinelPath)
	if err != nil {
		return "", time.Time{}
	}
	latestMod = fi.ModTime()
	for _, name := range []string{"head-name", "done", "git-rebase-todo", "stopped-sha", "orig-head"} {
		p := filepath.Join(sentinelPath, name)
		if sfi, sErr := os.Stat(p); sErr == nil && sfi.ModTime().After(latestMod) {
			latestMod = sfi.ModTime()
		}
	}
	if b, rErr := os.ReadFile(filepath.Join(sentinelPath, "head-name")); rErr == nil {
		raw := strings.TrimSpace(string(b))
		raw = strings.TrimPrefix(raw, "refs/heads/")
		branch = raw
	}
	return branch, latestMod
}

func shouldAutoAbortStrandedRebase(repoPath, sentinelPath string) (string, bool) {
	if lockInfo, err := repolock.Read(repoPath); err == nil && lockInfo.PID > 0 && isPIDAlive(lockInfo.PID) {
		return "", false
	}
	if _, err := os.Stat(filepath.Join(repoPath, ".autopilot", "lock")); err == nil {
		return "", false
	}
	if _, err := os.Stat(filepath.Join(repoPath, ".ase", "active.lock")); err == nil {
		return "", false
	}
	branch, modTime := readRebaseHeadBranch(sentinelPath)
	if branch == "" {
		return "", false
	}
	age := time.Since(modTime)
	_, hasIndexLock := os.Stat(filepath.Join(filepath.Dir(sentinelPath), "index.lock"))
	_, hasStoppedSHA := os.Stat(filepath.Join(sentinelPath, "stopped-sha"))
	conflictHalted := hasStoppedSHA == nil && os.IsNotExist(hasIndexLock)
	if isAutomatedMaintenanceBranch(branch) && (age > 2*time.Minute || conflictHalted) {
		return branch, true
	}
	if inferred := InferStoryIDFromBranch(branch); inferred != "" && IsStoryCompletedOnDisk(repoPath, inferred) && (age > 5*time.Minute || conflictHalted) {
		return branch, true
	}
	return "", false
}

func abortStrandedRebaseAndCheckoutMain(repoPath, sentinelPath, rebaseBranch string, res *RepoFleetStatusResult) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	abortCmd := procutil.CommandContext(ctx, "git", "-C", repoPath, "rebase", "--abort")
	if abortCmd.Run() != nil {
		_ = os.RemoveAll(sentinelPath)
		_ = procutil.CommandContext(ctx, "git", "-C", repoPath, "reset", "--hard", "HEAD").Run()
	}
	_ = procutil.CommandContext(ctx, "git", "-C", repoPath, "checkout", "-f", "main").Run()

	pullCmd := procutil.CommandContext(ctx, "git", "-C", repoPath, "pull", "--ff-only", "--no-recurse-submodules", "origin", "main")
	pullCmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if pullCmd.Run() != nil && isAutomatedMaintenanceBranch(rebaseBranch) {
		fetchCmd := procutil.CommandContext(ctx, "git", "-C", repoPath, "fetch", "origin", "main")
		fetchCmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		if fetchCmd.Run() == nil {
			_ = procutil.CommandContext(ctx, "git", "-C", repoPath, "reset", "--hard", "origin/main").Run()
		}
	}
	if isAutomatedMaintenanceBranch(rebaseBranch) {
		_ = procutil.CommandContext(ctx, "git", "-C", repoPath, "branch", "-D", rebaseBranch).Run()
		if sirs, gErr := filepath.Glob(filepath.Join(repoPath, "tmp", "reports", "intervention-git-update-sir-*.json")); gErr == nil {
			for _, sir := range sirs {
				_ = os.Remove(sir)
			}
		}
	}
	if _, err := os.Stat(sentinelPath); os.IsNotExist(err) {
		if res != nil {
			res.RemediationApplied = true
			res.RemediationActions = append(res.RemediationActions, fmt.Sprintf("aborted stranded rebase on %s and checked out main", rebaseBranch))
		}
		return true
	}
	return false
}

func inspectGitLockSentinels(repoPath string, autoPruneStale bool, res *RepoFleetStatusResult) {
	gitDir := resolveRepoGitDir(repoPath)
	if gitDir == "" {
		return
	}
	sentinels := []string{
		filepath.Join(gitDir, "index.lock"),
		filepath.Join(gitDir, "HEAD.lock"),
		filepath.Join(gitDir, "shallow.lock"),
		filepath.Join(gitDir, "MERGE_HEAD"),
		filepath.Join(gitDir, "rebase-merge"),
		filepath.Join(gitDir, "rebase-apply"),
		filepath.Join(gitDir, "CHERRY_PICK_HEAD"),
		filepath.Join(gitDir, "REVERT_HEAD"),
	}
	for _, s := range sentinels {
		if sfi, sErr := os.Stat(s); sErr == nil {
			base := filepath.Base(s)
			if autoPruneStale && strings.HasSuffix(base, ".lock") && time.Since(sfi.ModTime()) > 5*time.Minute {
				if os.Remove(s) == nil {
					res.RemediationApplied = true
					res.RemediationActions = append(res.RemediationActions, fmt.Sprintf("removed stale git lock %s", base))
					continue
				}
			}
			if autoPruneStale && (base == "rebase-merge" || base == "rebase-apply") {
				if rebaseBranch, ok := shouldAutoAbortStrandedRebase(repoPath, s); ok {
					if abortStrandedRebaseAndCheckoutMain(repoPath, s, rebaseBranch, res) {
						continue
					}
				}
			}
			res.HasActiveGitLock = true
			return
		}
	}
}

func populateMetadataAndGitVersion(ctx context.Context, cleanPath string, res *RepoFleetStatusResult) {
	metaPath := filepath.Join(cleanPath, "metadata.yml")
	if fi, err := os.Stat(metaPath); err == nil && !fi.IsDir() && fi.Size() < 2<<20 {
		if metaBytes, err := os.ReadFile(metaPath); err == nil {
			res.MetadataYmlRaw = string(metaBytes)
			hash := sha256.Sum256(metaBytes)
			res.MetadataYmlSHA = fmt.Sprintf("%x", hash)
			res.MetadataYmlModTime = fi.ModTime().UTC().Format(time.RFC3339)
		}
	}
	verCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	gitVerCmd := procutil.CommandContext(verCtx, "git", "-C", cleanPath, "describe", "--tags", "--always")
	if out, err := gitVerCmd.Output(); err == nil {
		res.GitVersion = strings.TrimSpace(string(out))
	}
}

// HasExactRepoPathBoundary returns true if candidatePath equals repoPath or is a child path inside repoPath.
// It strictly prevents "/Users/romeo/work/meta-repo" from matching "/Users/romeo/work/meta-repo-02".
func HasExactRepoPathBoundary(candidatePath, repoPath string) bool {
	if strings.TrimSpace(candidatePath) == "" || strings.TrimSpace(repoPath) == "" {
		return false
	}
	cleanCand := filepath.Clean(strings.TrimSpace(candidatePath))
	cleanRepo := filepath.Clean(strings.TrimSpace(repoPath))
	if cleanCand == cleanRepo {
		return true
	}
	sep := string(filepath.Separator)
	if strings.HasPrefix(cleanCand, cleanRepo+sep) {
		return true
	}
	if runtime.GOOS == "windows" {
		candLower := strings.ToLower(cleanCand)
		repoLower := strings.ToLower(cleanRepo)
		if candLower == repoLower || strings.HasPrefix(candLower, repoLower+sep) || strings.HasPrefix(candLower, repoLower+"/") {
			return true
		}
	}
	return false
}

// LineContainsExactRepoPath returns true if cmdLine contains repoPath with a valid path boundary
// (end of string, whitespace, '/', '\', '"', '\'', ':', ';') and NOT a sibling suffix like "-02".
func LineContainsExactRepoPath(cmdLine, repoPath string) bool {
	cleanRepo := filepath.Clean(strings.TrimSpace(repoPath))
	if cleanRepo == "" || cleanRepo == "." {
		return false
	}
	search := cmdLine
	for {
		idx := strings.Index(search, cleanRepo)
		if idx < 0 {
			return false
		}
		afterIdx := idx + len(cleanRepo)
		if afterIdx >= len(search) {
			return true
		}
		nextCh := search[afterIdx]
		if nextCh == ' ' || nextCh == '\t' || nextCh == '\n' || nextCh == '\r' ||
			nextCh == '/' || nextCh == '\\' || nextCh == '"' || nextCh == '\'' ||
			nextCh == ':' || nextCh == ';' {
			return true
		}
		search = search[afterIdx:]
	}
}

func storyCandidateDirs(repoPath, sid string) []string {
	candidateDirs := []string{
		filepath.Join(repoPath, "docs", "stories"),
		filepath.Join(filepath.Dir(repoPath), "docs", "stories"),
	}
	parts := strings.Split(sid, "-")
	if len(parts) >= 3 {
		cat := strings.ToLower(parts[1])
		switch cat {
		case "vigo":
			candidateDirs = append(candidateDirs,
				filepath.Join(repoPath, "vigo", "docs", "stories"),
				filepath.Join(repoPath, "vigo-api", "docs", "stories"),
			)
		case "iazio":
			candidateDirs = append(candidateDirs,
				filepath.Join(repoPath, "iazio-infra", "docs", "stories"),
				filepath.Join(repoPath, "iazio", "docs", "stories"),
			)
		case "gov", "auto", "ap":
			candidateDirs = append(candidateDirs,
				filepath.Join(repoPath, "autopilot-umbrella", "docs", "stories"),
				filepath.Join(repoPath, "iazio-infra", "docs", "stories"),
			)
		case "eye":
			candidateDirs = append(candidateDirs,
				filepath.Join(repoPath, "eye-habit", "docs", "stories"),
			)
		}
	}
	return candidateDirs
}

func extractMarkdownSection(content string, sectionHeaders ...string) string {
	lines := strings.Split(content, "\n")
	inSection := false
	var sb strings.Builder
	for _, rawLine := range lines {
		trimmed := strings.TrimSpace(rawLine)
		if strings.HasPrefix(trimmed, "## ") {
			if inSection {
				break
			}
			lowerHeader := strings.ToLower(trimmed)
			for _, h := range sectionHeaders {
				if strings.HasPrefix(lowerHeader, strings.ToLower(h)) {
					inSection = true
					break
				}
			}
			continue
		}
		if inSection {
			sb.WriteString(trimmed)
			sb.WriteByte('\n')
		}
	}
	return sb.String()
}

func hasUnresolvedStorySIROnDisk(repoPath, sid string) bool {
	if repoPath == "" || sid == "" {
		return false
	}
	lowerSID := strings.ToLower(sid)
	shortSlug := strings.TrimPrefix(lowerSID, "story-")
	reportsDir := filepath.Join(repoPath, "tmp", "reports")
	entries, err := os.ReadDir(reportsDir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		nameLower := strings.ToLower(e.Name())
		if !strings.HasPrefix(nameLower, "intervention-") || !strings.HasSuffix(nameLower, ".json") {
			continue
		}
		if !strings.Contains(nameLower, lowerSID) && !strings.Contains(nameLower, shortSlug) {
			continue
		}
		b, rErr := os.ReadFile(filepath.Join(reportsDir, e.Name()))
		if rErr != nil {
			continue
		}
		var payload struct {
			Status string `json:"status"`
		}
		if json.Unmarshal(b, &payload) == nil {
			st := strings.ToUpper(strings.TrimSpace(payload.Status))
			if st == "RESOLVED" || st == "CLOSED" || st == "DONE" {
				continue
			}
		}
		return true
	}
	return false
}

func isStoryMarkdownIncomplete(repoPath, sid, content string) bool {
	if hasUnresolvedStorySIROnDisk(repoPath, sid) {
		return true
	}
	reviewsSec := extractMarkdownSection(content, "## 7. reviews", "## reviews")
	if reviewsSec != "" && strings.Contains(strings.ToLower(reviewsSec), "_pending_") {
		return true
	}
	dodSec := extractMarkdownSection(content, "## 5. definition of done", "## definition of done")
	if dodSec != "" && strings.Contains(dodSec, "- [ ]") {
		return true
	}
	return false
}

// IsStoryCompletedOnDisk returns true if storyID is marked DONE in .autopilot/ase-state.json,
// docs/stories/*.story.md, child family docs/stories/*.story.md, or docs/stories/index.yml,
// and does not have pending reviews, unchecked DoD items, or unresolved story SIRs.
func IsStoryCompletedOnDisk(repoPath, storyID string) bool {
	sid := strings.ToUpper(strings.TrimSpace(storyID))
	if repoPath == "" || !strings.HasPrefix(sid, "STORY-") {
		return false
	}
	if hasUnresolvedStorySIROnDisk(repoPath, sid) {
		return false
	}

	// 1. Check .autopilot/ase-state.json
	if b, err := os.ReadFile(filepath.Join(repoPath, ".autopilot", "ase-state.json")); err == nil {
		var aseSt struct {
			StoryID string `json:"story_id"`
			Status  string `json:"status"`
			Phase   string `json:"phase"`
		}
		if json.Unmarshal(b, &aseSt) == nil && strings.EqualFold(strings.TrimSpace(aseSt.StoryID), sid) {
			if strings.EqualFold(aseSt.Status, "DONE") || strings.EqualFold(aseSt.Phase, "completed") {
				return true
			}
		}
	}

	// 2. Check candidate docs/stories directories (root, parent, and category-inferred child hubs)
	candidateDirs := storyCandidateDirs(repoPath, sid)

	idLower := strings.ToLower(sid)
	shortSlug := strings.TrimPrefix(idLower, "story-")

	for _, dir := range candidateDirs {
		entries, err := os.ReadDir(dir)
		if err == nil {
			for _, e := range entries {
				if e.IsDir() || !strings.HasSuffix(e.Name(), ".story.md") {
					continue
				}
				nameLower := strings.ToLower(e.Name())
				if !strings.Contains(nameLower, idLower) && !strings.Contains(nameLower, shortSlug) {
					continue
				}
				data, rErr := os.ReadFile(filepath.Join(dir, e.Name()))
				if rErr != nil {
					continue
				}
				content := string(data)
				if strings.Contains(strings.ToUpper(content), sid) {
					if isStoryMarkdownIncomplete(repoPath, sid, content) {
						return false
					}
					if strings.Contains(content, "\nstatus: DONE\n") ||
						strings.Contains(content, "\nstatus: DONE\r\n") ||
						strings.Contains(content, "\nstatus: \"DONE\"\n") ||
						strings.Contains(content, "\nstatus: 'DONE'\n") {
						return true
					}
				}
			}
		}

		// Also check index.yml in dir
		if indexBytes, idxErr := os.ReadFile(filepath.Join(dir, "index.yml")); idxErr == nil {
			lines := strings.Split(string(indexBytes), "\n")
			for i, l := range lines {
				if strings.Contains(strings.ToUpper(l), sid) {
					end := i + 8
					if end > len(lines) {
						end = len(lines)
					}
					for j := i; j < end; j++ {
						trimmed := strings.TrimSpace(lines[j])
						if j > i && strings.HasPrefix(trimmed, "- id:") {
							break
						}
						if strings.HasPrefix(strings.ToLower(trimmed), "status:") {
							val := strings.Trim(strings.TrimSpace(trimmed[len("status:"):]), "\"'")
							if strings.EqualFold(val, "DONE") {
								return true
							}
							break
						}
					}
				}
			}
		}
	}
	return false
}

// IsStoryExplicitlyInProgressOnDisk returns true if storyID is marked IN_PROGRESS or READY in
// docs/stories/*.story.md, child family docs/stories/*.story.md, or docs/stories/index.yml,
// or if its story markdown has incomplete DoD/reviews or open SIRs.
func IsStoryExplicitlyInProgressOnDisk(repoPath, storyID string) bool {
	sid := strings.ToUpper(strings.TrimSpace(storyID))
	if repoPath == "" || !strings.HasPrefix(sid, "STORY-") {
		return false
	}

	candidateDirs := storyCandidateDirs(repoPath, sid)
	idLower := strings.ToLower(sid)
	shortSlug := strings.TrimPrefix(idLower, "story-")

	for _, dir := range candidateDirs {
		entries, err := os.ReadDir(dir)
		if err == nil {
			for _, e := range entries {
				if e.IsDir() || !strings.HasSuffix(e.Name(), ".story.md") {
					continue
				}
				nameLower := strings.ToLower(e.Name())
				if !strings.Contains(nameLower, idLower) && !strings.Contains(nameLower, shortSlug) {
					continue
				}
				data, rErr := os.ReadFile(filepath.Join(dir, e.Name()))
				if rErr != nil {
					continue
				}
				content := string(data)
				if strings.Contains(strings.ToUpper(content), sid) {
					if strings.Contains(content, "\nstatus: IN_PROGRESS") ||
						strings.Contains(content, "\nstatus: \"IN_PROGRESS\"") ||
						strings.Contains(content, "\nstatus: 'IN_PROGRESS'") ||
						strings.Contains(content, "\nstatus: READY") ||
						strings.Contains(content, "\nstatus: \"READY\"") ||
						strings.Contains(content, "\nstatus: 'READY'") ||
						isStoryMarkdownIncomplete(repoPath, sid, content) {
						return true
					}
				}
			}
		}
	}
	return false
}

func hasActiveScopedResumeFile(repoPath, storyID string) bool {
	sid := strings.TrimSpace(storyID)
	if repoPath == "" || sid == "" {
		return false
	}
	resumePath := filepath.Join(repoPath, "tmp", fmt.Sprintf("ase-resume-%s.json", sid))
	b, err := os.ReadFile(resumePath)
	if err != nil {
		return false
	}
	var payload struct {
		Status string `json:"status"`
		Phase  string `json:"phase"`
	}
	if json.Unmarshal(b, &payload) == nil {
		if strings.EqualFold(payload.Status, "DONE") || strings.EqualFold(payload.Phase, "completed") {
			return false
		}
	}
	return true
}

func parseGitPorcelainV2(out string, res *RepoFleetStatusResult) {
	lines := strings.Split(out, "\n")
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "fatal:") || strings.HasPrefix(l, "error:") {
			continue
		}
		if strings.HasPrefix(l, "# branch.head ") {
			res.Branch = strings.TrimPrefix(l, "# branch.head ")
		} else if strings.HasPrefix(l, "# branch.oid ") {
			res.HeadSHA = strings.TrimPrefix(l, "# branch.oid ")
		} else if strings.HasPrefix(l, "# branch.ab ") {
			// # branch.ab +ahead -behind
			parts := strings.Fields(l)
			if len(parts) >= 4 {
				if a, err := strconv.Atoi(strings.TrimPrefix(parts[2], "+")); err == nil {
					res.Ahead = a
				}
				if b, err := strconv.Atoi(strings.TrimPrefix(parts[3], "-")); err == nil {
					res.Behind = b
				}
			}
		} else if strings.HasPrefix(l, "1 ") || strings.HasPrefix(l, "2 ") || strings.HasPrefix(l, "u ") {
			parts := strings.Fields(l)
			// Check if this line is a submodule gitlink:
			// Porcelain v2 format for changes: 1 <XY> <sub> <mH> <mI> <mW> <hH> <hI> <path>
			if len(parts) >= 9 && strings.HasPrefix(parts[2], "S") {
				subCode := parts[2]
				subPath := parts[len(parts)-1]
				hasModified := len(subCode) > 2 && subCode[2] == 'M'
				hasUntracked := len(subCode) > 3 && subCode[3] == 'U'
				hasCommitDrift := len(subCode) > 1 && subCode[1] == 'C'
				if hasModified || hasUntracked {
					res.DirtySubmodules = append(res.DirtySubmodules, subPath+" (uncommitted changes)")
				} else if hasCommitDrift {
					res.SubmoduleDrift = append(res.SubmoduleDrift, subPath)
				}
				continue
			}
			res.IsClean = false
			if len(parts) > 8 {
				res.ModifiedFiles = append(res.ModifiedFiles, parts[len(parts)-1])
			} else if len(parts) > 1 {
				res.ModifiedFiles = append(res.ModifiedFiles, parts[len(parts)-1])
			}
		} else if strings.HasPrefix(l, "? ") {
			res.IsClean = false
			parts := strings.Fields(l)
			if len(parts) >= 2 {
				res.UntrackedFiles = append(res.UntrackedFiles, parts[1])
			}
		}
	}
}

func parseSubmoduleStatus(ctx context.Context, cleanPath string, out string, res *RepoFleetStatusResult) {
	lines := strings.Split(out, "\n")
	seenDirty := make(map[string]bool)
	seenDrift := make(map[string]bool)
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "fatal:") || strings.HasPrefix(l, "error:") {
			continue
		}
		parts := strings.Fields(l)
		if len(parts) >= 2 {
			subPath := parts[1]
			fullSubPath := filepath.Join(cleanPath, subPath)
			hasUncommitted := false

			// Deep check: inspect the submodule worktree itself for uncommitted files and current branch
			if stat, err := os.Stat(fullSubPath); err == nil && stat.IsDir() {
				if res.Branch == "" {
					subStatusCmd := procutil.CommandContext(ctx, "git", "-C", fullSubPath, "status", "--porcelain")
					if subOut, err := subStatusCmd.Output(); err == nil && len(strings.TrimSpace(string(subOut))) > 0 {
						hasUncommitted = true
					}
				}
				if currBranch, ok := readGitWorktreeBranch(fullSubPath); ok {
					if currBranch != "main" && currBranch != "master" && currBranch != "detached" {
						res.NonMainSubmodules = append(res.NonMainSubmodules, fmt.Sprintf("%s (on %s)", subPath, currBranch))
					}
				} else {
					branchCmd := procutil.CommandContext(ctx, "git", "-C", fullSubPath, "branch", "--show-current")
					if bOut, err := branchCmd.Output(); err == nil {
						currBranch := strings.TrimSpace(string(bOut))
						if currBranch == "" {
							currBranch = "detached"
						}
						if currBranch != "main" && currBranch != "master" && currBranch != "detached" {
							res.NonMainSubmodules = append(res.NonMainSubmodules, fmt.Sprintf("%s (on %s)", subPath, currBranch))
						}
					}
				}
			}

			if hasUncommitted {
				if !seenDirty[subPath] {
					res.DirtySubmodules = append(res.DirtySubmodules, subPath+" (uncommitted changes)")
					seenDirty[subPath] = true
				}
			} else if strings.HasPrefix(l, "+") {
				if !seenDrift[subPath] {
					res.SubmoduleDrift = append(res.SubmoduleDrift, subPath)
					seenDrift[subPath] = true
				}
			} else if strings.HasPrefix(l, "-") || strings.HasPrefix(l, "U") {
				if !seenDirty[subPath] {
					res.DirtySubmodules = append(res.DirtySubmodules, subPath)
					seenDirty[subPath] = true
				}
			}
		}
	}
}

func parseASEState(repoPath string, res *RepoFleetStatusResult) {
	parseASEStateWithOpts(repoPath, true, res)
}

func parseASEStateWithOpts(repoPath string, autoRemediateTier1 bool, res *RepoFleetStatusResult) {
	// 1. Check locks first
	var locks []string
	if _, err := os.Stat(filepath.Join(repoPath, ".autopilot", "lock")); err == nil {
		locks = append(locks, ".autopilot/lock")
	}
	if _, err := os.Stat(filepath.Join(repoPath, ".ase", "active.lock")); err == nil {
		locks = append(locks, ".ase/active.lock")
	}
	if _, err := os.Stat(filepath.Join(repoPath, ".index.yml.lock")); err == nil {
		locks = append(locks, ".index.yml.lock")
	}

	isMainBranch := res.Branch == "" || res.Branch == "main" || res.Branch == "master"
	canPruneCompletedStory := isMainBranch && len(res.NonMainSubmodules) == 0 && !res.HasActiveAgentASETurn

	// 2. Inspect .autopilot/state.json (and auto-remediate stale state when story is DONE and repo is back on main)
	stateFile := filepath.Join(repoPath, ".autopilot", "state.json")
	if b, err := os.ReadFile(stateFile); err == nil {
		var s struct {
			State        string `json:"state"`
			StoryID      string `json:"story_id"`
			CurrentPhase string `json:"current_phase"`
			UpdatedAt    string `json:"updated_at"`
		}
		if json.Unmarshal(b, &s) == nil {
			storyDone := canPruneCompletedStory && s.StoryID != "" && IsStoryCompletedOnDisk(repoPath, s.StoryID)
			staleIdlePhase := s.StoryID == "" && s.CurrentPhase != "" && (s.State == "" || s.State == "IDLE") && len(locks) == 0 && isMainBranch
			if autoRemediateTier1 && (storyDone || staleIdlePhase) {
				resetAutopilotStateFileToIdle(stateFile)
				res.RemediationApplied = true
				if storyDone {
					res.RemediationActions = append(res.RemediationActions, fmt.Sprintf("reset completed story %s in .autopilot/state.json", s.StoryID))
				} else {
					res.RemediationActions = append(res.RemediationActions, fmt.Sprintf("cleared stale phase %s in .autopilot/state.json", s.CurrentPhase))
				}
			} else if !storyDone {
				if s.State != "" {
					res.ASEState = s.State
				}
				if s.StoryID != "" {
					res.ActiveStoryID = s.StoryID
				}
				if !staleIdlePhase && s.CurrentPhase != "" {
					res.CurrentPhase = s.CurrentPhase
				}
			}
		}
	}

	if len(locks) > 0 {
		res.LockDetails = strings.Join(locks, ", ")
		if res.ASEState == "IDLE" {
			res.ASEState = "RUNNING"
		}
	}

	// 3. Prune completed tmp/ase-resume-STORY-*.json checkpoints when unlocked and back on main
	if autoRemediateTier1 && len(locks) == 0 && canPruneCompletedStory {
		if matches, err := filepath.Glob(filepath.Join(repoPath, "tmp", "ase-resume-STORY-*.json")); err == nil {
			for _, m := range matches {
				base := filepath.Base(m)
				candID := strings.TrimSuffix(strings.TrimPrefix(base, "ase-resume-"), ".json")
				if IsStoryCompletedOnDisk(repoPath, candID) {
					if os.Remove(m) == nil {
						res.RemediationApplied = true
						res.RemediationActions = append(res.RemediationActions, fmt.Sprintf("removed completed checkpoint tmp/%s", base))
					}
				}
			}
		}
	}

	// 4. Check .session_story_id and scoped tmp/ase-resume-<storyID>.json for interactive / ad-hoc ASE sessions
	sessionPath := filepath.Join(repoPath, ".session_story_id")
	if autoRemediateTier1 && len(locks) == 0 && canPruneCompletedStory {
		pruneCompletedSubmoduleSessionStoryFiles(repoPath, res)
	}
	if autoRemediateTier1 && len(locks) == 0 && isMainBranch {
		if _, statErr := os.Stat(sessionPath); os.IsNotExist(statErr) && isGitTrackedFile(repoPath, ".session_story_id") {
			if procutil.Command("git", "-C", repoPath, "checkout", "HEAD", "--", ".session_story_id").Run() == nil {
				res.RemediationApplied = true
				res.RemediationActions = append(res.RemediationActions, "restored deleted tracked .session_story_id to HEAD")
				removeModifiedFileFromStatus(res, ".session_story_id")
			}
		}
	}
	if b, err := os.ReadFile(sessionPath); err == nil {
		storyID := strings.TrimSpace(string(b))
		if strings.HasPrefix(strings.ToUpper(storyID), "STORY-") {
			if canPruneCompletedStory && IsStoryCompletedOnDisk(repoPath, storyID) {
				if autoRemediateTier1 {
					if changed, action := cleanOrRestoreSessionStoryFile(repoPath, storyID); changed {
						res.RemediationApplied = true
						res.RemediationActions = append(res.RemediationActions, action)
						removeModifiedFileFromStatus(res, ".session_story_id")
					}
					_ = os.Remove(filepath.Join(repoPath, "tmp", fmt.Sprintf("ase-resume-%s.json", storyID)))
				}
			} else {
				if res.ActiveStoryID == "" {
					res.ActiveStoryID = storyID
				}
				nonMainBranch := res.Branch != "" && res.Branch != "main" && res.Branch != "master" && res.Branch != "HEAD"
				hasResumeFile := hasActiveScopedResumeFile(repoPath, storyID)
				if nonMainBranch || len(res.NonMainSubmodules) > 0 || hasResumeFile || IsStoryExplicitlyInProgressOnDisk(repoPath, storyID) || res.HasActiveAgentASETurn {
					if res.ASEState == "IDLE" || res.ASEState == "FAILED_DIRTY" {
						res.ASEState = "RUNNING"
					}
					if res.CurrentPhase == "" {
						res.CurrentPhase = "ADHOC_ASE"
					}
				}
			}
		}
	}

	// 5. Infer ActiveStoryID from non-main story branch or non-main submodule branches when not bound in files
	if res.ActiveStoryID == "" {
		inferred := InferStoryIDFromBranch(res.Branch)
		if inferred == "" {
			for _, subEntry := range res.NonMainSubmodules {
				if cand := InferStoryIDFromBranch(subEntry); cand != "" {
					inferred = cand
					break
				}
			}
		}
		if inferred != "" {
			res.ActiveStoryID = inferred
			if res.ASEState == "IDLE" || res.ASEState == "FAILED_DIRTY" {
				res.ASEState = "RUNNING"
			}
			if res.CurrentPhase == "" {
				res.CurrentPhase = "ADHOC_ASE"
			}
		}
	}

	if len(locks) == 0 && res.WorktreeState == "DIRTY_MODIFIED" && res.ASEState == "IDLE" {
		res.ASEState = "FAILED_DIRTY"
	}
}

func isGitTrackedFile(repoPath, relFile string) bool {
	if strings.TrimSpace(repoPath) == "" || strings.TrimSpace(relFile) == "" {
		return false
	}
	cmd := procutil.Command("git", "-C", repoPath, "ls-files", "--error-unmatch", relFile)
	return cmd.Run() == nil
}

func cleanOrRestoreSessionStoryFile(repoPath, storyID string) (bool, string) {
	sessionPath := filepath.Join(repoPath, ".session_story_id")
	if isGitTrackedFile(repoPath, ".session_story_id") {
		out, err := procutil.Command("git", "-C", repoPath, "status", "--porcelain", "--", ".session_story_id").Output()
		if err == nil && strings.TrimSpace(string(out)) != "" {
			if procutil.Command("git", "-C", repoPath, "checkout", "HEAD", "--", ".session_story_id").Run() == nil {
				return true, fmt.Sprintf("restored tracked .session_story_id (%s) to HEAD", storyID)
			}
		}
		return false, ""
	}
	if os.Remove(sessionPath) == nil {
		return true, fmt.Sprintf("removed completed .session_story_id (%s)", storyID)
	}
	return false, ""
}

func hasSessionStoryFile(repoPath string) bool {
	if strings.TrimSpace(repoPath) == "" {
		return false
	}
	b, err := os.ReadFile(filepath.Join(repoPath, ".session_story_id"))
	if err != nil {
		return false
	}
	return strings.HasPrefix(strings.ToUpper(strings.TrimSpace(string(b))), "STORY-")
}

func hasDeadRunningRepoLock(repoPath string) bool {
	if strings.TrimSpace(repoPath) == "" {
		return false
	}
	lockInfo, err := repolock.Read(repoPath)
	if err != nil {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(lockInfo.Status), "RUNNING") &&
		lockInfo.PID <= 0 && lockInfo.HarnessPID <= 0 && lockInfo.ToolPID <= 0 {
		return false
	}
	if (lockInfo.PID > 0 && isPIDAlive(lockInfo.PID)) ||
		(lockInfo.HarnessPID > 0 && isPIDAlive(lockInfo.HarnessPID)) ||
		(lockInfo.ToolPID > 0 && isPIDAlive(lockInfo.ToolPID)) {
		return false
	}
	return true
}

func isSessionStoryFileStale(repoPath string, now time.Time) bool {
	if strings.TrimSpace(repoPath) == "" {
		return false
	}
	fi, err := os.Stat(filepath.Join(repoPath, ".session_story_id"))
	if err != nil || fi.IsDir() {
		return false
	}
	if now.IsZero() {
		now = time.Now()
	}
	return !fi.ModTime().IsZero() && now.Sub(fi.ModTime()) > 15*time.Minute
}

func pruneCompletedSubmoduleSessionStoryFiles(repoPath string, res *RepoFleetStatusResult) {
	if strings.TrimSpace(repoPath) == "" {
		return
	}
	matches, err := filepath.Glob(filepath.Join(repoPath, "*", ".session_story_id"))
	if err != nil {
		return
	}
	for _, m := range matches {
		b, errR := os.ReadFile(m)
		if errR != nil {
			continue
		}
		subSID := strings.TrimSpace(string(b))
		if !strings.HasPrefix(strings.ToUpper(subSID), "STORY-") {
			continue
		}
		subDir := filepath.Dir(m)
		if IsStoryCompletedOnDisk(repoPath, subSID) || IsStoryCompletedOnDisk(subDir, subSID) {
			if changed, action := cleanOrRestoreSessionStoryFile(subDir, subSID); changed && res != nil {
				res.RemediationApplied = true
				res.RemediationActions = append(res.RemediationActions, fmt.Sprintf("%s in %s", action, filepath.Base(subDir)))
			}
		}
	}
}

func removeModifiedFileFromStatus(res *RepoFleetStatusResult, relFile string) {
	if res == nil {
		return
	}
	filtered := make([]string, 0, len(res.ModifiedFiles))
	for _, f := range res.ModifiedFiles {
		if f != relFile {
			filtered = append(filtered, f)
		}
	}
	if len(filtered) == len(res.ModifiedFiles) {
		return
	}
	res.ModifiedFiles = filtered
	res.DirtyFilesCount = len(res.ModifiedFiles) + len(res.DirtySubmodules)
	hasModifiedFiles := len(res.ModifiedFiles) > 0
	hasUntrackedFiles := len(res.UntrackedFiles) > 0
	hasDirtySubmodules := len(res.DirtySubmodules) > 0
	hasSubmoduleDrift := len(res.SubmoduleDrift) > 0
	hasNonMainSubmodules := len(res.NonMainSubmodules) > 0
	isMainBranch := res.Branch == "main" || res.Branch == "master"
	if res.HasActiveGitLock || hasModifiedFiles || hasDirtySubmodules {
		res.WorktreeState = "DIRTY_MODIFIED"
		res.IsClean = false
	} else if hasUntrackedFiles {
		res.WorktreeState = "DIRTY_UNTRACKED_ONLY"
		res.IsClean = false
	} else if !isMainBranch {
		res.WorktreeState = "BRANCH_NOT_MAIN"
		res.IsClean = false
	} else if hasNonMainSubmodules {
		res.WorktreeState = "SUBMODULES_NOT_MAIN"
		res.IsClean = false
	} else if hasSubmoduleDrift {
		res.WorktreeState = "CLEAN_SUBMODULE_DRIFT"
		res.IsClean = true
	} else {
		res.WorktreeState = "CLEAN"
		res.IsClean = true
	}
}

func resetAutopilotStateFileToIdle(stateFile string) {
	b, err := os.ReadFile(stateFile)
	if err != nil {
		return
	}
	var s map[string]any
	if json.Unmarshal(b, &s) != nil {
		return
	}
	s["state"] = "IDLE"
	s["story_id"] = ""
	s["current_phase"] = ""
	delete(s, "lock")
	delete(s, "active_job_id")
	s["updated_at"] = time.Now().UTC().Format(time.RFC3339)
	if updated, errM := json.MarshalIndent(s, "", "  "); errM == nil {
		_ = os.WriteFile(stateFile, append(updated, '\n'), 0o644)
	}
}

func isDiagnosticCmdLine(cmdLine string) bool {
	lower := strings.ToLower(cmdLine)
	return strings.Contains(lower, "fleet-status") ||
		strings.Contains(lower, "repo evaluate") ||
		strings.Contains(lower, "repo remediate") ||
		strings.Contains(lower, "prompt run") ||
		strings.Contains(lower, "execute shell prompt") ||
		strings.Contains(lower, "iazio-agy-log-git-update") ||
		strings.Contains(lower, "status --json") ||
		strings.Contains(lower, "auth status") ||
		strings.Contains(lower, "autopilot-mcp") ||
		strings.Contains(lower, "autopilot-fleet") ||
		strings.Contains(lower, "autopilot-observe") ||
		strings.Contains(lower, "iazio-mcp") ||
		strings.Contains(lower, "mcp serve") ||
		strings.Contains(lower, "mcp-server") ||
		strings.Contains(lower, "mcp_proxy_bundle") ||
		strings.Contains(lower, "chrome-devtools-mcp") ||
		strings.Contains(lower, "git-update") ||
		strings.Contains(lower, "iazio-harness daemon") ||
		strings.Contains(lower, "iazio-agent") ||
		strings.Contains(lower, "iazio-harvester") ||
		strings.Contains(lower, "antigravity ide helper") ||
		strings.Contains(lower, "gradle-server") ||
		strings.Contains(lower, "org.eclipse.equinox") ||
		strings.Contains(lower, " --hub") ||
		strings.HasSuffix(strings.TrimSpace(lower), "--hub")
}

func isAutomatedMaintenanceRepoLock(repoPath string) bool {
	if strings.TrimSpace(repoPath) == "" {
		return false
	}
	lockInfo, err := repolock.Read(repoPath)
	if err != nil {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(lockInfo.Status), "RUNNING") {
		return false
	}
	alive := (lockInfo.PID > 0 && isPIDAlive(lockInfo.PID)) ||
		(lockInfo.HarnessPID > 0 && isPIDAlive(lockInfo.HarnessPID)) ||
		(lockInfo.ToolPID > 0 && isPIDAlive(lockInfo.ToolPID))
	if !alive {
		return false
	}
	jobLower := strings.ToLower(strings.TrimSpace(lockInfo.JobID))
	schedLower := strings.ToLower(strings.TrimSpace(lockInfo.ScheduleID))
	promptLower := strings.ToLower(strings.TrimSpace(lockInfo.Prompt))
	return strings.HasPrefix(jobLower, "autopilot-git-update") ||
		strings.HasPrefix(jobLower, "git-update-") ||
		strings.HasPrefix(schedLower, "autopilot-git-update") ||
		strings.Contains(promptLower, "autopilot git-update")
}

func isAutomatedMaintenanceBranch(branch string) bool {
	b := strings.ToLower(strings.TrimSpace(branch))
	return strings.HasPrefix(b, "chore/governance-pointers-") ||
		strings.HasPrefix(b, "chore/submodule-pointers-") ||
		strings.HasPrefix(b, "chore/auto-sync-")
}

func hasExplicitIdleStateFile(repoPath string) bool {
	if strings.TrimSpace(repoPath) == "" {
		return false
	}
	if b, err := os.ReadFile(filepath.Join(repoPath, ".autopilot", "ase-state.json")); err == nil {
		var as struct {
			Status string `json:"status"`
			Phase  string `json:"phase"`
		}
		if json.Unmarshal(b, &as) == nil {
			if strings.EqualFold(strings.TrimSpace(as.Status), "DONE") ||
				strings.EqualFold(strings.TrimSpace(as.Status), "COMPLETED") ||
				strings.EqualFold(strings.TrimSpace(as.Phase), "completed") {
				return true
			}
		}
	}
	if b, err := os.ReadFile(filepath.Join(repoPath, ".autopilot", "state.json")); err == nil {
		var s struct {
			State   string `json:"state"`
			StoryID string `json:"story_id"`
		}
		if json.Unmarshal(b, &s) == nil {
			if strings.EqualFold(strings.TrimSpace(s.State), "IDLE") && strings.TrimSpace(s.StoryID) == "" {
				return true
			}
		}
	}
	return false
}

func autoRemediateCompletedOrMaintenanceBranch(ctx context.Context, cleanPath string, res *RepoFleetStatusResult) bool {
	hasActiveSession := res != nil && (res.ASEState == "RUNNING" || res.HasActiveAgentASETurn ||
		(res.ToolLiveness == "LIVE" && (res.ExecutionProvenance == "INDEPENDENT_USER_SESSION" || res.ExecutionProvenance == "COORDINATED_HARNESS")))
	if res == nil || res.HasActiveGitLock || res.LockDetails != "" || hasActiveSession {
		return false
	}
	isMaint := isAutomatedMaintenanceBranch(res.Branch)
	if !isMaint && (len(res.ModifiedFiles) > 0 || len(res.DirtySubmodules) > 0 || len(res.NonMainSubmodules) > 0 || len(res.SubmoduleDrift) > 0) {
		return false
	}
	inferredStory := InferStoryIDFromBranch(res.Branch)
	eligible := isMaint ||
		(inferredStory != "" && IsStoryCompletedOnDisk(cleanPath, inferredStory) && hasExplicitIdleStateFile(cleanPath))
	if !eligible {
		return false
	}
	if lockInfo, err := repolock.Read(cleanPath); err == nil && lockInfo.PID > 0 && isPIDAlive(lockInfo.PID) {
		return false
	}
	prevBranch := res.Branch
	checkoutCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	checkoutArgs := []string{"-C", cleanPath, "checkout", "main"}
	if isMaint {
		checkoutArgs = []string{"-C", cleanPath, "checkout", "-f", "main"}
	}
	checkoutErr := procutil.CommandContext(checkoutCtx, "git", checkoutArgs...).Run()
	cancel()
	if checkoutErr != nil {
		return false
	}
	pullCtx, cancelPull := context.WithTimeout(ctx, 15*time.Second)
	pullCmd := procutil.CommandContext(pullCtx, "git", "-C", cleanPath, "pull", "--ff-only", "--no-recurse-submodules", "origin", "main")
	pullCmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if pullErr := pullCmd.Run(); pullErr != nil && isAutomatedMaintenanceBranch(prevBranch) {
		fetchCmd := procutil.CommandContext(pullCtx, "git", "-C", cleanPath, "fetch", "origin", "main")
		fetchCmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		if fetchCmd.Run() == nil {
			_ = procutil.CommandContext(pullCtx, "git", "-C", cleanPath, "reset", "--hard", "origin/main").Run()
		}
	}
	cancelPull()

	if isAutomatedMaintenanceBranch(prevBranch) {
		_ = procutil.CommandContext(ctx, "git", "-C", cleanPath, "branch", "-D", prevBranch).Run()
		if sirs, gErr := filepath.Glob(filepath.Join(cleanPath, "tmp", "reports", "intervention-git-update-sir-*.json")); gErr == nil {
			for _, sir := range sirs {
				_ = os.Remove(sir)
			}
		}
	}

	res.Branch = "main"
	res.Ahead = 0
	res.Behind = 0
	if out, err := procutil.CommandContext(ctx, "git", "-C", cleanPath, "status", "--porcelain=v2", "--branch").CombinedOutput(); err == nil {
		res.ModifiedFiles = nil
		res.UntrackedFiles = nil
		res.DirtySubmodules = nil
		res.SubmoduleDrift = nil
		res.IsClean = true
		parseGitPorcelainV2(string(out), res)
	}
	res.DirtyFilesCount = len(res.ModifiedFiles) + len(res.DirtySubmodules)
	res.UntrackedFilesCount = len(res.UntrackedFiles)
	if len(res.ModifiedFiles) > 0 || len(res.DirtySubmodules) > 0 {
		res.WorktreeState = "DIRTY_MODIFIED"
		res.IsClean = false
	} else if len(res.UntrackedFiles) > 0 {
		res.WorktreeState = "DIRTY_UNTRACKED_ONLY"
		res.IsClean = false
	} else if len(res.NonMainSubmodules) > 0 {
		res.WorktreeState = "SUBMODULES_NOT_MAIN"
		res.IsClean = false
	} else if len(res.SubmoduleDrift) > 0 {
		res.WorktreeState = "CLEAN_SUBMODULE_DRIFT"
		res.IsClean = true
	} else {
		res.WorktreeState = "CLEAN"
		res.IsClean = true
	}
	res.RemediationApplied = true
	res.RemediationActions = append(res.RemediationActions, fmt.Sprintf("checked out main from completed/maintenance branch %s", prevBranch))
	return true
}

func reconcileAdhocASEState(repoPath string, res *RepoFleetStatusResult) {
	reconcileAdhocASEStateWithOpts(repoPath, true, res)
}

func reconcileAdhocASEStateWithOpts(repoPath string, autoRemediateTier1 bool, res *RepoFleetStatusResult) {
	nonMainBranch := res.Branch != "" && res.Branch != "main" && res.Branch != "master" && res.Branch != "HEAD"
	hasScopedResume := res.ActiveStoryID != "" && hasActiveScopedResumeFile(repoPath, res.ActiveStoryID)
	isAutopilotProcess := strings.HasPrefix(strings.ToLower(res.AgentTool), "autopilot")

	// If ActiveStoryID was inferred or read on a completed story, .autopilot/state.json is explicitly IDLE,
	// and no active agent process, lock, or resume file exists, clear the completed story so post-ASE branches
	// do not stay locked in ADHOC_ASE after the agent exits.
	// Conversely, if the story is still incomplete on disk (IN_PROGRESS) on a branch/submodule with no live
	// agent process, no lock, and either an explicit IDLE/DONE state file or no root .session_story_id,
	// classify it as FAILED_DIRTY / TERMINATED (preserving ActiveStoryID for in-flight detection) rather than ADHOC_ASE.
	if res.ActiveStoryID != "" && res.ToolLiveness != "LIVE" && !res.HasActiveAgentASETurn && res.LockDetails == "" && !hasScopedResume {
		storyCompleted := IsStoryCompletedOnDisk(repoPath, res.ActiveStoryID)
		explicitIdle := hasExplicitIdleStateFile(repoPath)
		hasRootSessionFile := hasSessionStoryFile(repoPath)
		deadRepoLock := hasDeadRunningRepoLock(repoPath)
		staleSessionFile := isSessionStoryFileStale(repoPath, time.Now())
		if storyCompleted && (explicitIdle || deadRepoLock || staleSessionFile) {
			if autoRemediateTier1 {
				if deadRepoLock {
					_ = os.Remove(filepath.Join(repoPath, ".iazio", "repo.lock"))
					res.RemediationApplied = true
					res.RemediationActions = append(res.RemediationActions, "released stale .iazio/repo.lock with dead PID")
				}
				if changed, action := cleanOrRestoreSessionStoryFile(repoPath, res.ActiveStoryID); changed {
					res.RemediationApplied = true
					res.RemediationActions = append(res.RemediationActions, action)
					removeModifiedFileFromStatus(res, ".session_story_id")
				}
			}
			res.ActiveStoryID = ""
			if res.CurrentPhase == "ADHOC_ASE" {
				res.CurrentPhase = ""
			}
			if res.ASEState == "RUNNING" {
				if res.WorktreeState == "DIRTY_MODIFIED" {
					res.ASEState = "FAILED_DIRTY"
				} else {
					res.ASEState = "IDLE"
				}
			}
			res.ExecutionProvenance = "NONE"
		} else if !storyCompleted && (explicitIdle || deadRepoLock || staleSessionFile || (!hasRootSessionFile && (res.CurrentPhase == "ADHOC_ASE" || nonMainBranch || len(res.NonMainSubmodules) > 0))) {
			if autoRemediateTier1 && deadRepoLock {
				_ = os.Remove(filepath.Join(repoPath, ".iazio", "repo.lock"))
				res.RemediationApplied = true
				res.RemediationActions = append(res.RemediationActions, "released stale .iazio/repo.lock with dead PID")
			}
			if res.CurrentPhase == "ADHOC_ASE" {
				res.CurrentPhase = ""
			}
			res.ASEState = "FAILED_DIRTY"
			res.ToolLiveness = "TERMINATED"
			res.ExecutionProvenance = "NONE"
			res.AgentTool = ""
		}
	}

	// Strict Ad-Hoc ASE Evidence Gate:
	// An interactive agent shell (agy/cursor/opencode/claude) sitting on clean main or an automated
	// maintenance branch (chore/governance-pointers-*) with no bound story, no locks, no active ASE brain turn,
	// and no autopilot* process is an idle shell — NOT an active ad-hoc ASE execution.
	hasAffirmativeASEEvidence := res.LockDetails != "" ||
		isAutopilotProcess ||
		res.HasActiveAgentASETurn ||
		(res.ToolLiveness == "LIVE" && (nonMainBranch || len(res.NonMainSubmodules) > 0) && !isAutomatedMaintenanceBranch(res.Branch)) ||
		(res.ActiveStoryID != "" && (nonMainBranch || len(res.NonMainSubmodules) > 0 || hasScopedResume || res.ToolLiveness == "LIVE"))

	if res.ToolLiveness == "LIVE" && res.ExecutionProvenance == "INDEPENDENT_USER_SESSION" && hasAffirmativeASEEvidence {
		if res.ASEState == "IDLE" || res.ASEState == "FAILED_DIRTY" {
			res.ASEState = "RUNNING"
		}
		if res.CurrentPhase == "" {
			res.CurrentPhase = "ADHOC_ASE"
		}
	}

	// If no affirmative ad-hoc ASE evidence exists and ASE is not RUNNING/IN_PROGRESS,
	// clear INDEPENDENT_USER_SESSION provenance so idle shells on clean/dirty repos do not pause queues.
	if !hasAffirmativeASEEvidence && res.ASEState != "RUNNING" && res.ASEState != "IN_PROGRESS" && res.ExecutionProvenance == "INDEPENDENT_USER_SESSION" {
		res.ExecutionProvenance = "NONE"
	}

	// If .session_story_id set ActiveStoryID on main, but there is no non-main branch, no locks,
	// no scoped resume file, and no live agent tool in this repo, the session story file is stale.
	if res.ActiveStoryID != "" && res.ASEState != "RUNNING" && res.ASEState != "IN_PROGRESS" && !nonMainBranch &&
		len(res.NonMainSubmodules) == 0 && res.LockDetails == "" && !hasScopedResume && res.ToolLiveness != "LIVE" && !res.HasActiveAgentASETurn {
		if autoRemediateTier1 {
			if changed, action := cleanOrRestoreSessionStoryFile(repoPath, res.ActiveStoryID); changed {
				res.RemediationApplied = true
				res.RemediationActions = append(res.RemediationActions, action)
				removeModifiedFileFromStatus(res, ".session_story_id")
			}
		}
		res.ActiveStoryID = ""
		if res.CurrentPhase == "ADHOC_ASE" {
			res.CurrentPhase = ""
		}
	}

	if res.ASEState == "RUNNING" &&
		res.ExecutionProvenance != "COORDINATED_HARNESS" &&
		res.ExecutionProvenance != "COORDINATED_HARNESS_FAILED" {
		res.ExecutionProvenance = "INDEPENDENT_USER_SESSION"
		if res.ToolLiveness == "NONE" || res.ToolLiveness == "TERMINATED" || res.ToolLiveness == "" {
			res.ToolLiveness = "LIVE"
		}
		if res.AgentTool == "" {
			res.AgentTool = "autopilot-ase"
		}
	}
}

var knownAgentBinaries = []string{
	"agy", "antigravity", "opencode", "cursor", "claude", "autopilot", "iazio-harness",
}

var auxiliaryChildBinaries = []string{
	"iazio-mcp", "autopilot-mcp", "node", "gopls", "rust-analyzer", "tsserver", "pyright", "python",
}

func isAgentTool(cmd string) (string, bool) {
	lower := strings.ToLower(filepath.Base(cmd))
	if strings.Contains(lower, "-mcp") || strings.Contains(lower, "mcp-") || strings.HasSuffix(lower, "mcp") {
		return "", false
	}
	for _, tool := range knownAgentBinaries {
		if strings.Contains(lower, tool) {
			return tool, true
		}
	}
	return "", false
}

func isAuxiliaryTool(cmd string) (string, bool) {
	lower := strings.ToLower(filepath.Base(cmd))
	for _, tool := range auxiliaryChildBinaries {
		if strings.Contains(lower, tool) {
			return tool, true
		}
	}
	return "", false
}

func isPIDAlive(pid int) bool {
	if pid <= 1 {
		return false
	}
	if runtime.GOOS == "linux" {
		if _, err := os.Stat(fmt.Sprintf("/proc/%d", pid)); err == nil {
			return true
		}
	}
	return procutil.IsPIDAlive(pid)
}

func discoverAgentProcesses(repoPath string, res *RepoFleetStatusResult) {
	if isAutomatedMaintenanceRepoLock(repoPath) {
		return
	}
	if runtime.GOOS != "linux" {
		// Non-linux fallback using ps and lsof CWD inspection
		discoverAgentProcessesFallback(repoPath, res)
		return
	}

	procDir, err := os.Open("/proc")
	if err != nil {
		discoverAgentProcessesFallback(repoPath, res)
		return
	}
	defer procDir.Close()

	entries, err := procDir.Readdirnames(-1)
	if err != nil {
		return
	}

	selfPID := os.Getpid()
	nonMainBranch := res.Branch != "" && res.Branch != "main" && res.Branch != "master" && res.Branch != "HEAD" && !isAutomatedMaintenanceBranch(res.Branch)

	for _, name := range entries {
		pid, err := strconv.Atoi(name)
		if err != nil || pid <= 1 || pid == selfPID {
			continue
		}

		// Read cwd symlink
		cwdPath := filepath.Join("/proc", name, "cwd")
		targetCwd, err := os.Readlink(cwdPath)
		if err != nil {
			continue
		}

		if !HasExactRepoPathBoundary(targetCwd, repoPath) {
			continue
		}

		// Read cmdline
		cmdlineBytes, _ := os.ReadFile(filepath.Join("/proc", name, "cmdline"))
		cmdArgs := strings.Split(string(cmdlineBytes), "\x00")
		if len(cmdArgs) == 0 || cmdArgs[0] == "" {
			continue
		}
		if isDiagnosticCmdLine(strings.Join(cmdArgs, " ")) {
			continue
		}

		// Primary agent tool detection
		toolName, match := isAgentTool(cmdArgs[0])
		if !match && len(cmdArgs) > 1 {
			toolName, match = isAgentTool(cmdArgs[1])
		}
		if match {
			environBytes, _ := os.ReadFile(filepath.Join("/proc", name, "environ"))
			provenance, isMaintenance := classifyProcessEnvProvenance(string(environBytes))
			if isMaintenance {
				continue
			}

			fdPaths := readProcOpenFDLinks(pid)
			fdConvID, _, fdActiveASE, fdStoryID := inspectOpenAgentPathsForBrainAndASE(fdPaths, time.Now())
			argConvID := extractConversationIDFromArgs(cmdArgs)
			if argConvID == "" {
				argConvID = fdConvID
			}
			canBindFDStory := fdStoryID != "" && canBindTranscriptStoryToRepo(repoPath, fdStoryID) &&
				(fdActiveASE || IsStoryExplicitlyInProgressOnDisk(repoPath, fdStoryID) || nonMainBranch || len(res.NonMainSubmodules) > 0)

			isBetterCandidate := res.AgentTool == "" ||
				(!res.HasActiveAgentASETurn && (fdActiveASE || strings.HasPrefix(strings.ToLower(toolName), "autopilot")))

			if isBetterCandidate {
				res.AgentTool = toolName
				res.AgentPID = pid
				res.ToolLiveness = "LIVE"
				if argConvID != "" {
					res.ActiveConversationID = argConvID
				}
				if fdActiveASE {
					res.HasActiveAgentASETurn = true
				}
				if canBindFDStory && res.ActiveStoryID == "" {
					res.ActiveStoryID = fdStoryID
				}
				res.ExecutionProvenance = provenance
			} else if fdActiveASE || canBindFDStory {
				if fdActiveASE {
					res.HasActiveAgentASETurn = true
				}
				if argConvID != "" && res.ActiveConversationID == "" {
					res.ActiveConversationID = argConvID
				}
				if canBindFDStory && res.ActiveStoryID == "" {
					res.ActiveStoryID = fdStoryID
				}
			}
		}

		// Detect orphaned auxiliary processes (e.g. iazio-mcp stdio helper whose parent exited)
		if auxName, isAux := isAuxiliaryTool(cmdArgs[0]); isAux {
			statBytes, _ := os.ReadFile(filepath.Join("/proc", name, "stat"))
			statFields := strings.Fields(string(statBytes))
			ppid := 0
			if len(statFields) > 3 {
				ppid, _ = strconv.Atoi(statFields[3])
			}
			if ppid == 1 || !isPIDAlive(ppid) {
				res.OrphanedProcesses = append(res.OrphanedProcesses, fmt.Sprintf("%s (PID %d, PPID %d)", auxName, pid, ppid))
			}
		}
	}

	if !res.HasActiveAgentASETurn {
		discoverActiveBrainTranscriptForRepo(repoPath, time.Now(), res)
	}
}

func applyNoAgentFallbackState(res *RepoFleetStatusResult) {
	if res == nil || res.AgentTool != "" {
		return
	}
	if res.LockDetails != "" {
		res.ToolLiveness = "KILLED"
		res.ASEState = "ORPHANED"
		res.ExecutionProvenance = "COORDINATED_HARNESS_FAILED"
	} else if res.WorktreeState == "DIRTY_MODIFIED" && res.ASEState == "IDLE" {
		res.ASEState = "FAILED_DIRTY"
		res.ToolLiveness = "TERMINATED"
	} else if !res.IsClean && res.ASEState != "RUNNING" {
		res.ToolLiveness = "TERMINATED"
	}
}

func readProcOpenFDLinks(pid int) []string {
	if pid <= 1 {
		return nil
	}
	fdDir := fmt.Sprintf("/proc/%d/fd", pid)
	entries, err := os.ReadDir(fdDir)
	if err != nil {
		return nil
	}
	paths := make([]string, 0, len(entries))
	for _, e := range entries {
		if target, err := os.Readlink(filepath.Join(fdDir, e.Name())); err == nil && target != "" {
			paths = append(paths, target)
		}
	}
	return paths
}

func extractBrainCandidateFromOpenPath(rawPath string) (appDataDir string, convID string, ok bool) {
	p := filepath.ToSlash(strings.TrimSpace(rawPath))
	p = strings.TrimSuffix(p, " (deleted)")
	if p == "" {
		return "", "", false
	}

	if idx := strings.Index(p, "/brain/"); idx >= 0 {
		rest := p[idx+len("/brain/"):]
		candID := rest
		if slashIdx := strings.IndexByte(rest, '/'); slashIdx >= 0 {
			candID = rest[:slashIdx]
		}
		if len(candID) >= 8 && !strings.HasPrefix(candID, ".") {
			return filepath.FromSlash(p[:idx]), candID, true
		}
	}
	if idx := strings.Index(p, "/conversations/"); idx >= 0 {
		rest := p[idx+len("/conversations/"):]
		base := rest
		if slashIdx := strings.IndexByte(rest, '/'); slashIdx >= 0 {
			base = rest[:slashIdx]
		}
		candID := strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(base, "-wal"), "-shm"), ".db")
		if len(candID) >= 8 && !strings.HasPrefix(candID, ".") {
			return filepath.FromSlash(p[:idx]), candID, true
		}
	}
	if idx := strings.Index(p, "/presence/"); idx >= 0 {
		rest := p[idx+len("/presence/"):]
		base := rest
		if slashIdx := strings.IndexByte(rest, '/'); slashIdx >= 0 {
			base = rest[:slashIdx]
		}
		candID := strings.TrimSuffix(base, ".lock")
		if len(candID) >= 8 && !strings.HasPrefix(candID, ".") {
			return filepath.FromSlash(p[:idx]), candID, true
		}
	}
	return "", "", false
}

func stripTranscriptToolResponseLines(s string) string {
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	filtered := make([]string, 0, len(lines))
	for _, rawLine := range lines {
		lowerLine := strings.ToLower(rawLine)
		if strings.Contains(lowerLine, `"type":"tool_response"`) || strings.Contains(lowerLine, `"type": "tool_response"`) {
			continue
		}
		filtered = append(filtered, rawLine)
	}
	return strings.Join(filtered, "\n")
}

func isAutomatedMaintenanceTranscript(s string) bool {
	lower := strings.ToLower(stripTranscriptToolResponseLines(s))
	return strings.Contains(lower, `execute shell prompt "autopilot git-update"`) ||
		strings.Contains(lower, `execute shell prompt \"autopilot git-update\"`) ||
		strings.Contains(lower, "diagnostic failure summary from shell autopilot git-update") ||
		strings.Contains(lower, "iazio-agy-log-git-update-agy") ||
		strings.Contains(lower, `"job_id":"autopilot-git-update`) ||
		strings.Contains(lower, `"schedule_id":"autopilot-git-update`)
}

func extractContextSummaryUserRequests(lower string) string {
	const openTag = "<context_summary>"
	const closeTag = "</context_summary>"
	const reqHeader = "# user requests"
	var collected []string
	search := lower
	for {
		start := strings.Index(search, openTag)
		if start < 0 {
			break
		}
		block := search[start+len(openTag):]
		advance := start + len(openTag)
		if end := strings.Index(block, closeTag); end >= 0 {
			advance += end + len(closeTag)
			block = block[:end]
		} else {
			advance = len(search)
		}
		if reqIdx := strings.Index(block, reqHeader); reqIdx >= 0 {
			reqSec := block[reqIdx:]
			cut := len(reqSec)
			for _, sep := range []string{"\n# ", `\n# `, "\n</context_summary>", `\n</context_summary>`} {
				if idx := strings.Index(reqSec[len(reqHeader):], sep); idx >= 0 {
					cand := len(reqHeader) + idx
					if cand < cut {
						cut = cand
					}
				}
			}
			collected = append(collected, reqSec[:cut])
		}
		search = search[advance:]
	}
	return strings.Join(collected, "\n")
}

func stripSystemTagRange(lower string, openTag, closeTag string) string {
	for {
		start := strings.Index(lower, openTag)
		if start < 0 {
			break
		}
		rest := lower[start+len(openTag):]
		endRel := strings.Index(rest, closeTag)
		if endRel >= 0 {
			end := start + len(openTag) + endRel + len(closeTag)
			if closeTag == "</rule[" {
				if gt := strings.IndexByte(lower[end:], '>'); gt >= 0 && gt < 256 {
					end += gt + 1
				}
			}
			lower = lower[:start] + " " + lower[end:]
			continue
		}
		// If the closing tag was truncated by the head/tail byte window, strip up to <user_request> or newline
		reqIdx := strings.Index(rest, "<user_request>")
		nlIdx := strings.IndexByte(rest, '\n')
		cut := len(lower)
		if reqIdx >= 0 && (nlIdx < 0 || reqIdx < nlIdx) {
			cut = start + len(openTag) + reqIdx
		} else if nlIdx >= 0 {
			cut = start + len(openTag) + nlIdx
		}
		lower = lower[:start] + " " + lower[cut:]
	}
	return lower
}

func stripTranscriptBoilerplateAndToolOutputs(s string) string {
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	filtered := make([]string, 0, len(lines))
	for _, rawLine := range lines {
		lowerLine := strings.ToLower(rawLine)
		if strings.Contains(lowerLine, `"type":"tool_response"`) || strings.Contains(lowerLine, `"type": "tool_response"`) {
			continue
		}
		filtered = append(filtered, lowerLine)
	}
	cleaned := strings.Join(filtered, "\n")
	preservedRequests := extractContextSummaryUserRequests(cleaned)
	for _, pair := range [][2]string{
		{"<user_rules>", "</user_rules>"},
		{"<rule[", "</rule["},
		{"<skills>", "</skills>"},
		{"<subagents>", "</subagents>"},
		{"<slash_commands>", "</slash_commands>"},
		{"<mcp_servers>", "</mcp_servers>"},
		{"<context_summary>", "</context_summary>"},
	} {
		cleaned = stripSystemTagRange(cleaned, pair[0], pair[1])
	}
	if preservedRequests != "" {
		cleaned += "\n" + preservedRequests
	}
	return cleaned
}

func containsASEWorkflowMarker(s string) bool {
	if isAutomatedMaintenanceTranscript(s) {
		return false
	}
	cleaned := stripTranscriptBoilerplateAndToolOutputs(s)
	return strings.Contains(cleaned, "/autonomous-story-execution") ||
		strings.Contains(cleaned, "skills/autonomous-story-execution") ||
		strings.Contains(cleaned, "commands/autonomous-story-execution.md") ||
		strings.Contains(cleaned, "autopilot-story") ||
		strings.Contains(cleaned, "autopilot-git-flow") ||
		strings.Contains(cleaned, "autopilot-review-scheme") ||
		strings.Contains(cleaned, "autopilot review-scheme") ||
		strings.Contains(cleaned, "autopilot conformance") ||
		strings.Contains(cleaned, "autopilot story begin") ||
		strings.Contains(cleaned, "autopilot ase") ||
		strings.Contains(cleaned, "git-flow ase-start") ||
		strings.Contains(cleaned, "git-flow ase-ship") ||
		strings.Contains(cleaned, ".session_story_id")
}

func extractBoundStoryMatchesFromTranscript(headStr, tailStr string) [][]string {
	cleanHead := stripTranscriptBoilerplateAndToolOutputs(headStr)
	cleanTail := stripTranscriptBoilerplateAndToolOutputs(tailStr)
	return append(
		transcriptBoundStoryRe.FindAllStringSubmatch(cleanHead, -1),
		transcriptBoundStoryRe.FindAllStringSubmatch(cleanTail, -1)...,
	)
}

func hasNonEmptyJSONArray(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" || trimmed == "[]" {
		return false
	}
	var arr []any
	if err := json.Unmarshal(raw, &arr); err == nil {
		return len(arr) > 0
	}
	return true
}

var bgTaskStartRe = regexp.MustCompile(`Tool is running as a background task with task id:\s*([a-zA-Z0-9_-]+/task-[0-9]+)`)

func hasPendingBackgroundTask(tail string) bool {
	matches := bgTaskStartRe.FindAllStringSubmatchIndex(tail, -1)
	for _, m := range matches {
		if len(m) < 4 {
			continue
		}
		taskID := tail[m[2]:m[3]]
		rest := tail[m[1]:]
		if !strings.Contains(rest, taskID+`" finished with result:`) &&
			!strings.Contains(rest, taskID+`\" finished with result:`) {
			return true
		}
	}
	return false
}

func isTranscriptTurnInFlight(tail []byte, modTime time.Time, now time.Time) bool {
	if modTime.IsZero() || now.Sub(modTime) > 15*time.Minute {
		return false
	}
	tailStr := string(tail)
	hasBg := hasPendingBackgroundTask(tailStr)
	if !hasBg && now.Sub(modTime) > 3*time.Minute {
		return false
	}
	lines := strings.Split(tailStr, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" || !strings.HasPrefix(line, "{") || !strings.HasSuffix(line, "}") {
			continue
		}
		var step struct {
			Type            string          `json:"type"`
			Status          string          `json:"status"`
			ToolCalls       json.RawMessage `json:"tool_calls"`
			TruncatedFields []string        `json:"truncated_fields"`
		}
		if err := json.Unmarshal([]byte(line), &step); err != nil {
			continue
		}
		hasTools := hasNonEmptyJSONArray(step.ToolCalls)
		if !hasTools {
			for _, tf := range step.TruncatedFields {
				if strings.EqualFold(strings.TrimSpace(tf), "tool_calls") {
					hasTools = true
					break
				}
			}
		}
		if strings.EqualFold(step.Type, "PLANNER_RESPONSE") &&
			strings.EqualFold(step.Status, "DONE") &&
			!hasTools {
			if now.Sub(modTime) <= 15*time.Minute && hasBg {
				return true
			}
			return false
		}
		return true
	}
	return false
}

func readHeadAndTailBytes(path string, headLimit, tailLimit int64) (head []byte, tail []byte, modTime time.Time, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, time.Time{}, err
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return nil, nil, time.Time{}, err
	}
	modTime = fi.ModTime()
	size := fi.Size()
	if size <= 0 {
		return nil, nil, modTime, nil
	}
	if size <= headLimit+tailLimit {
		all := make([]byte, size)
		n, _ := f.Read(all)
		all = all[:n]
		return all, all, modTime, nil
	}
	head = make([]byte, headLimit)
	hn, _ := f.ReadAt(head, 0)
	head = head[:hn]

	tail = make([]byte, tailLimit)
	tn, _ := f.ReadAt(tail, size-tailLimit)
	tail = tail[:tn]
	return head, tail, modTime, nil
}

func readMiddleTranscriptUserInputs(path string, headLimit, tailLimit int64) string {
	const maxScanFileSize = 32 * 1024 * 1024
	const maxCollectedBytes = 256 * 1024
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil || fi.Size() <= headLimit+tailLimit || fi.Size() > maxScanFileSize {
		return ""
	}
	startOffset := headLimit - 4096
	if startOffset < 0 {
		startOffset = 0
	}
	endOffset := fi.Size() - tailLimit + 4096
	if endOffset > fi.Size() {
		endOffset = fi.Size()
	}
	if startOffset > 0 {
		if _, err := f.Seek(startOffset, io.SeekStart); err != nil {
			return ""
		}
	}
	reader := bufio.NewReaderSize(f, 64*1024)
	offset := startOffset
	if startOffset > 0 {
		firstFrag, rErr := reader.ReadString('\n')
		offset += int64(len(firstFrag))
		if rErr != nil {
			return ""
		}
	}
	var b strings.Builder
	for offset < endOffset {
		line, rErr := reader.ReadString('\n')
		offset += int64(len(line))
		if len(line) > 0 {
			lower := strings.ToLower(line)
			if strings.Contains(lower, `"type":"user_input"`) || strings.Contains(lower, `"type": "user_input"`) {
				if b.Len()+len(line) > maxCollectedBytes {
					remaining := maxCollectedBytes - b.Len()
					if remaining > 0 {
						b.WriteString(line[:remaining])
						b.WriteByte('\n')
					}
					break
				}
				b.WriteString(line)
			}
		}
		if rErr != nil {
			break
		}
	}
	return b.String()
}

func inspectBrainTranscriptDir(brainDir string, now time.Time) (newestMod time.Time, hasActiveASETurn bool, inferredStoryID string) {
	for _, rel := range []string{
		filepath.Join(".system_generated", "logs", "transcript.jsonl"),
		filepath.Join(".system_generated", "logs", "transcript_full.jsonl"),
	} {
		full := filepath.Join(brainDir, rel)
		head, tail, modTime, err := readHeadAndTailBytes(full, 64*1024, 128*1024)
		if err != nil {
			continue
		}
		if modTime.After(newestMod) {
			newestMod = modTime
		}
		headStr := string(head)
		tailStr := string(tail)
		midUserInputs := readMiddleTranscriptUserInputs(full, 64*1024, 128*1024)
		if isAutomatedMaintenanceTranscript(headStr) || isAutomatedMaintenanceTranscript(midUserInputs) || isAutomatedMaintenanceTranscript(tailStr) {
			continue
		}
		if containsASEWorkflowMarker(headStr) || containsASEWorkflowMarker(midUserInputs) || containsASEWorkflowMarker(tailStr) {
			if inferredStoryID == "" && !modTime.IsZero() && now.Sub(modTime) <= 2*time.Hour {
				combinedHead := headStr
				if midUserInputs != "" {
					combinedHead += "\n" + midUserInputs
				}
				matches := extractBoundStoryMatchesFromTranscript(combinedHead, tailStr)
				if len(matches) > 0 {
					inferredStoryID = strings.ToUpper(matches[len(matches)-1][1])
				}
			}
			if !hasActiveASETurn && isTranscriptTurnInFlight(tail, modTime, now) {
				hasActiveASETurn = true
			}
		}
	}
	return newestMod, hasActiveASETurn, inferredStoryID
}

func inspectOpenAgentPathsForBrainAndASE(openPaths []string, now time.Time) (convID string, newestTranscript time.Time, hasActiveASETurn bool, inferredStoryID string) {
	seenBrains := make(map[string]bool)
	var fallbackConvID string
	for _, raw := range openPaths {
		appDataDir, candID, ok := extractBrainCandidateFromOpenPath(raw)
		if !ok || seenBrains[candID] {
			continue
		}
		seenBrains[candID] = true
		if fallbackConvID == "" {
			fallbackConvID = candID
		}
		brainDir := filepath.Join(appDataDir, "brain", candID)
		modTime, activeASE, storyID := inspectBrainTranscriptDir(brainDir, now)
		if storyID != "" && inferredStoryID == "" {
			inferredStoryID = storyID
		}
		if activeASE {
			hasActiveASETurn = true
			if storyID != "" {
				inferredStoryID = storyID
			}
		}
		if !modTime.IsZero() && modTime.After(newestTranscript) {
			newestTranscript = modTime
			convID = candID
		}
	}
	if convID == "" {
		convID = fallbackConvID
	}
	return convID, newestTranscript, hasActiveASETurn, inferredStoryID
}

func extractConversationIDFromArgs(cmdArgs []string) string {
	for i := 0; i < len(cmdArgs); i++ {
		if strings.HasPrefix(cmdArgs[i], "--conversation=") {
			return strings.TrimPrefix(cmdArgs[i], "--conversation=")
		}
		if (cmdArgs[i] == "--conversation" || cmdArgs[i] == "-c") && i+1 < len(cmdArgs) {
			return cmdArgs[i+1]
		}
	}
	return ""
}

// parsePsElapsedDuration parses ps etime format ([[dd-]hh:]mm:ss) into time.Duration.
func parsePsElapsedDuration(etime string) time.Duration {
	raw := strings.TrimSpace(etime)
	if raw == "" {
		return 0
	}
	days := 0
	if dashIdx := strings.Index(raw, "-"); dashIdx > 0 {
		if d, err := strconv.Atoi(raw[:dashIdx]); err == nil {
			days = d
		}
		raw = raw[dashIdx+1:]
	}
	parts := strings.Split(raw, ":")
	var hours, mins, secs int
	switch len(parts) {
	case 3:
		hours, _ = strconv.Atoi(parts[0])
		mins, _ = strconv.Atoi(parts[1])
		secs, _ = strconv.Atoi(parts[2])
	case 2:
		mins, _ = strconv.Atoi(parts[0])
		secs, _ = strconv.Atoi(parts[1])
	default:
		return 0
	}
	return time.Duration(days)*24*time.Hour +
		time.Duration(hours)*time.Hour +
		time.Duration(mins)*time.Minute +
		time.Duration(secs)*time.Second
}

// parseDarwinLsofBrainTranscripts extracts the most recently modified brain conversation ID and transcript timestamp
// from lsof -F n output.
func parseDarwinLsofBrainTranscripts(lsofOut string) (convID string, newestTranscript time.Time) {
	convID, newestTranscript, _, _ = parseDarwinLsofBrainAndASE(lsofOut, time.Now())
	return convID, newestTranscript
}

func parseDarwinLsofBrainAndASE(lsofOut string, now time.Time) (convID string, newestTranscript time.Time, hasActiveASETurn bool, inferredStoryID string) {
	var paths []string
	for _, line := range strings.Split(lsofOut, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "n") {
			paths = append(paths, strings.TrimPrefix(line, "n"))
		}
	}
	return inspectOpenAgentPathsForBrainAndASE(paths, now)
}

// inspectDarwinAgentProcess determines provenance, active conversation ID, and whether an interactive
// CLI process has been sitting idle at a prompt (>15m elapsed with no recent brain transcript activity).
func inspectDarwinAgentProcess(pid int, etime string, cmdLine string, hasRepoLock bool) (provenance string, convID string, isIdle bool) {
	provenance, convID, isIdle, _, _ = inspectDarwinAgentProcessDetailed(pid, etime, cmdLine, hasRepoLock)
	return provenance, convID, isIdle
}

func envHasNonEmptyVar(fields []string, key string) bool {
	prefix := key + "="
	for _, f := range fields {
		if strings.HasPrefix(f, prefix) && strings.TrimSpace(strings.TrimPrefix(f, prefix)) != "" {
			return true
		}
	}
	return false
}

func envContainsKV(fields []string, kv string) bool {
	for _, f := range fields {
		if f == kv {
			return true
		}
	}
	return false
}

func isAutomatedMaintenanceEnv(envStr string) bool {
	fields := strings.Fields(strings.ReplaceAll(envStr, "\x00", " "))
	if envContainsKV(fields, "IAZIO_MAINTENANCE_RUN=1") {
		return true
	}
	if envContainsKV(fields, "IAZIO_AGENT_MANAGED=1") && !envHasNonEmptyVar(fields, "IAZIO_HARNESS_JOB_ID") {
		return true
	}
	return false
}

func classifyProcessEnvProvenance(envStr string) (string, bool) {
	if isAutomatedMaintenanceEnv(envStr) {
		return "NONE", true
	}
	fields := strings.Fields(strings.ReplaceAll(envStr, "\x00", " "))
	if envHasNonEmptyVar(fields, "IAZIO_HARNESS_JOB_ID") || envContainsKV(fields, "IAZIO_MANAGED_BY=harness") {
		return "COORDINATED_HARNESS", false
	}
	return "INDEPENDENT_USER_SESSION", false
}

func inspectDarwinAgentProcessDetailed(pid int, etime string, cmdLine string, hasRepoLock bool) (provenance string, convID string, isIdle bool, hasActiveASETurn bool, inferredStoryID string) {
	provenance = "INDEPENDENT_USER_SESSION"
	convID = extractConversationIDFromArgs(strings.Fields(cmdLine))

	envCtx, cancelEnv := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	envOut, envErr := procutil.CommandContext(envCtx, "ps", "eww", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	cancelEnv()
	if envErr == nil {
		var isMaint bool
		provenance, isMaint = classifyProcessEnvProvenance(string(envOut))
		if isMaint {
			return "NONE", "", true, false, ""
		}
	}
	if provenance == "COORDINATED_HARNESS" {
		return provenance, convID, false, false, ""
	}

	elapsed := parsePsElapsedDuration(etime)
	lsofCtx, cancelLsof := context.WithTimeout(context.Background(), 8*time.Second)
	lsofOut, lsofErr := procutil.CommandContext(lsofCtx, "lsof", "-n", "-P", "-p", strconv.Itoa(pid), "-F", "n").Output()
	cancelLsof()
	var newestTranscript time.Time
	if lsofErr == nil && len(lsofOut) > 0 {
		lsofConv, ts, activeASE, storyID := parseDarwinLsofBrainAndASE(string(lsofOut), time.Now())
		newestTranscript = ts
		hasActiveASETurn = activeASE
		inferredStoryID = storyID
		if convID == "" && lsofConv != "" {
			convID = lsofConv
		}
	}
	if newestTranscript.IsZero() && convID != "" {
		for _, brainRoot := range candidateBrainRootDirs() {
			modTime, activeASE, storyID := inspectBrainTranscriptDir(filepath.Join(brainRoot, convID), time.Now())
			if !modTime.IsZero() && modTime.After(newestTranscript) {
				newestTranscript = modTime
			}
			if activeASE {
				hasActiveASETurn = true
			}
			if inferredStoryID == "" && storyID != "" {
				inferredStoryID = storyID
			}
		}
	}

	if !hasRepoLock && !hasActiveASETurn && elapsed > 15*time.Minute {
		if newestTranscript.IsZero() || time.Since(newestTranscript) > 15*time.Minute {
			return provenance, convID, true, false, ""
		}
	}
	return provenance, convID, false, hasActiveASETurn, inferredStoryID
}

func canonicalizeTranscriptPathText(s string) string {
	lower := strings.ToLower(s)
	norm := strings.ReplaceAll(lower, `\\`, `/`)
	if !strings.Contains(norm, `:\`) {
		norm = strings.ReplaceAll(norm, `\r\n`, "\n")
		norm = strings.ReplaceAll(norm, `\n`, "\n")
		norm = strings.ReplaceAll(norm, `\t`, " ")
	}
	norm = strings.ReplaceAll(norm, `\"`, `"`)
	norm = strings.ReplaceAll(norm, `\`, `/`)
	for strings.Contains(norm, "//") {
		norm = strings.ReplaceAll(norm, "//", "/")
	}
	norm = strings.ReplaceAll(norm, "/private/var/folders/", "/var/folders/")
	norm = strings.ReplaceAll(norm, "/private/tmp/", "/tmp/")
	// Normalize WSL /mnt/<drive>/ to <drive>:/ so Windows C:\... and WSL /mnt/c/... match identically.
	for i := 0; i < 26; i++ {
		ch := string(rune('a' + i))
		wslPrefix := "/mnt/" + ch + "/"
		winPrefix := ch + ":/"
		if strings.Contains(norm, wslPrefix) {
			norm = strings.ReplaceAll(norm, wslPrefix, winPrefix)
		}
	}
	return norm
}

func stripTrailingWorktreeNumber(cleanPath string) string {
	idx := strings.LastIndexByte(cleanPath, '-')
	slashIdx := strings.LastIndexByte(cleanPath, '/')
	if idx <= slashIdx+1 || idx >= len(cleanPath)-1 {
		return cleanPath
	}
	for i := idx + 1; i < len(cleanPath); i++ {
		if cleanPath[i] < '0' || cleanPath[i] > '9' {
			return cleanPath
		}
	}
	return cleanPath[:idx]
}

func isTranscriptPathBoundaryChar(ch byte) bool {
	return ch == '/' || ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' ||
		ch == '"' || ch == '\'' || ch == ':' || ch == ';' ||
		ch == ',' || ch == ')' || ch == ']' || ch == '}' || ch == '>'
}

func countWorktreeFamilyOccurrences(searchAll, cleanRepo string) (selfCount int, maxOtherSiblingCount int) {
	familyPrefix := stripTrailingWorktreeNumber(cleanRepo)
	counts := make(map[string]int)
	search := searchAll
	for {
		idx := strings.Index(search, familyPrefix)
		if idx < 0 {
			break
		}
		afterPrefix := idx + len(familyPrefix)
		validBefore := true
		if idx > 0 && !strings.HasPrefix(familyPrefix, "/") {
			prev := search[idx-1]
			if (prev >= 'a' && prev <= 'z') || (prev >= '0' && prev <= '9') || prev == '_' || prev == '-' {
				validBefore = false
			}
		}
		advance := afterPrefix
		if validBefore {
			candPath := familyPrefix
			afterCand := afterPrefix
			if afterPrefix < len(search) && search[afterPrefix] == '-' {
				j := afterPrefix + 1
				for j < len(search) && search[j] >= '0' && search[j] <= '9' {
					j++
				}
				if j > afterPrefix+1 {
					candPath = search[idx:j]
					afterCand = j
					advance = j
				}
			}
			if afterCand >= len(search) {
				counts[candPath]++
			} else {
				rest := search[afterCand:]
				nextCh := search[afterCand]
				if nextCh != '-' && !strings.HasPrefix(rest, "/.git/objects") && !strings.HasPrefix(rest, "/.git/modules") {
					if isTranscriptPathBoundaryChar(nextCh) {
						counts[candPath]++
					}
				}
			}
		}
		if advance <= 0 || advance > len(search) {
			break
		}
		search = search[advance:]
	}
	selfCount = counts[cleanRepo]
	for k, v := range counts {
		if k != cleanRepo && v > maxOtherSiblingCount {
			maxOtherSiblingCount = v
		}
	}
	return selfCount, maxOtherSiblingCount
}

// IsStoryOwnedBySiblingWorktree returns true when storyID is not explicitly active in repoPath
// (neither in repoPath/.session_story_id, repoPath's git branch, nor IN_PROGRESS on disk in repoPath),
// and a sibling worktree in the same parent directory explicitly owns storyID.
func IsStoryOwnedBySiblingWorktree(repoPath, storyID string) bool {
	sid := strings.ToUpper(strings.TrimSpace(storyID))
	cleanRepo := filepath.Clean(strings.TrimSpace(repoPath))
	if cleanRepo == "" || cleanRepo == "." || !strings.HasPrefix(sid, "STORY-") {
		return false
	}
	if b, err := os.ReadFile(filepath.Join(cleanRepo, ".session_story_id")); err == nil &&
		strings.EqualFold(strings.TrimSpace(string(b)), sid) {
		return false
	}
	if br, ok := readGitWorktreeBranch(cleanRepo); ok && strings.EqualFold(InferStoryIDFromBranch(br), sid) {
		return false
	}
	if IsStoryExplicitlyInProgressOnDisk(cleanRepo, sid) {
		return false
	}

	parentDir := filepath.Dir(cleanRepo)
	familyBase := stripTrailingWorktreeNumber(strings.ToLower(filepath.Base(cleanRepo)))
	entries, err := os.ReadDir(parentDir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		candNameLower := strings.ToLower(e.Name())
		if stripTrailingWorktreeNumber(candNameLower) != familyBase {
			continue
		}
		sibPath := filepath.Clean(filepath.Join(parentDir, e.Name()))
		if sibPath == cleanRepo {
			continue
		}
		if b, sErr := os.ReadFile(filepath.Join(sibPath, ".session_story_id")); sErr == nil &&
			strings.EqualFold(strings.TrimSpace(string(b)), sid) {
			return true
		}
		if lockInfo, lErr := repolock.Read(sibPath); lErr == nil && strings.EqualFold(strings.TrimSpace(lockInfo.StoryID), sid) {
			return true
		}
		if br, ok := readGitWorktreeBranch(sibPath); ok && strings.EqualFold(InferStoryIDFromBranch(br), sid) {
			return true
		}
		if IsStoryExplicitlyInProgressOnDisk(sibPath, sid) {
			return true
		}
	}
	return false
}

func canBindTranscriptStoryToRepo(repoPath, storyID string) bool {
	sid := strings.ToUpper(strings.TrimSpace(storyID))
	if !strings.HasPrefix(sid, "STORY-") || strings.HasPrefix(sid, "STORY-GOV-POINTERS-") {
		return false
	}
	if IsStoryCompletedOnDisk(repoPath, sid) || IsStoryOwnedBySiblingWorktree(repoPath, sid) {
		return false
	}
	if strings.TrimSpace(repoPath) != "" && hasExplicitIdleStateFile(repoPath) && !hasSessionStoryFile(repoPath) {
		if br, ok := readGitWorktreeBranch(repoPath); !ok || br == "main" || br == "master" {
			return IsStoryExplicitlyInProgressOnDisk(repoPath, sid)
		}
	}
	return true
}

func isConversationClaimedByOtherWorktree(repoPath, candID string) bool {
	cleanRepo := filepath.Clean(strings.TrimSpace(repoPath))
	if cleanRepo == "" || cleanRepo == "." {
		return false
	}
	cleanRepoSlash := strings.ToLower(filepath.ToSlash(cleanRepo))

	home, _ := os.UserHomeDir()
	if home != "" {
		for _, stateFile := range []string{
			filepath.Join(home, ".iazio", "watchdog-active-jobs.json"),
			filepath.Join(home, ".iazio", "harness-active-jobs.json"),
		} {
			if data, err := os.ReadFile(stateFile); err == nil {
				var jobs []struct {
					WorktreePath   string `json:"worktree_path"`
					ConversationID string `json:"conversation_id"`
				}
				if json.Unmarshal(data, &jobs) == nil {
					for _, j := range jobs {
						if strings.EqualFold(strings.TrimSpace(j.ConversationID), candID) {
							wtSlash := strings.ToLower(filepath.ToSlash(filepath.Clean(j.WorktreePath)))
							if wtSlash != "" && wtSlash != cleanRepoSlash {
								return true
							}
						}
					}
				}
			}
		}
	}

	parentDir := filepath.Dir(cleanRepo)
	if entries, err := os.ReadDir(parentDir); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			sibPath := filepath.Clean(filepath.Join(parentDir, e.Name()))
			if strings.EqualFold(filepath.ToSlash(sibPath), cleanRepoSlash) {
				continue
			}
			if lockInfo, lErr := repolock.Read(sibPath); lErr == nil {
				if strings.EqualFold(strings.TrimSpace(lockInfo.ConversationID), candID) {
					return true
				}
			}
		}
	}

	if home != "" {
		dbPath := filepath.Join(home, ".gemini", "antigravity-cli", "conversations", candID+".db")
		if raw, err := os.ReadFile(dbPath); err == nil {
			if isDBWorkspaceClaimedBySiblingWorktree(raw, cleanRepoSlash) {
				return true
			}
		}
	}
	return false
}

func splitSlashDirBase(p string) (string, string) {
	p = strings.TrimRight(p, "/")
	idx := strings.LastIndexByte(p, '/')
	if idx < 0 {
		return "", p
	}
	if idx == 0 {
		return "/", p[1:]
	}
	return p[:idx], p[idx+1:]
}

func isDBWorkspaceClaimedBySiblingWorktree(raw []byte, cleanRepoSlash string) bool {
	if len(raw) == 0 || strings.TrimSpace(cleanRepoSlash) == "" {
		return false
	}
	cleanRepoSlash = strings.TrimRight(strings.ToLower(filepath.ToSlash(cleanRepoSlash)), "/")
	parentSlash, baseName := splitSlashDirBase(cleanRepoSlash)
	familyBase := stripTrailingWorktreeNumber(baseName)
	if parentSlash == "" || familyBase == "" {
		return false
	}

	selfRootCount := 0
	siblingCounts := make(map[string]int)
	maxSiblingRootCount := 0

	prefix := []byte("file://")
	idx := 0
	for idx < len(raw) {
		pos := bytes.Index(raw[idx:], prefix)
		if pos < 0 {
			break
		}
		start := idx + pos + len(prefix)
		end := start
		for end < len(raw) {
			b := raw[end]
			if b < 32 || b > 126 ||
				b == '"' || b == '\'' || b == '\r' || b == '\n' ||
				b == '}' || b == ']' || b == ')' || b == ' ' || b == '\t' ||
				b == '<' || b == '>' || b == '#' || b == '?' ||
				b == ',' || b == ';' || b == '`' {
				break
			}
			end++
		}
		if end > start {
			token := strings.TrimSpace(string(raw[start:end]))
			declaredSlash := strings.TrimRight(strings.ToLower(filepath.ToSlash(filepath.Clean(token))), "/")
			if strings.HasPrefix(declaredSlash, "/") {
				dirSlash, decBase := splitSlashDirBase(declaredSlash)
				if dirSlash == parentSlash && stripTrailingWorktreeNumber(decBase) == familyBase {
					if declaredSlash == cleanRepoSlash {
						selfRootCount++
					} else {
						siblingCounts[declaredSlash]++
						if siblingCounts[declaredSlash] > maxSiblingRootCount {
							maxSiblingRootCount = siblingCounts[declaredSlash]
						}
					}
				}
			}
		}
		idx = start
	}

	return maxSiblingRootCount > 0 && (selfRootCount == 0 || maxSiblingRootCount > selfRootCount*2)
}

// TranscriptReferencesRepoPath returns true if transcript text references repoPath (across Windows C:\...,
// JSON-escaped C:\\..., C:/..., or WSL /mnt/c/... representations) with a strict path boundary (never matching
// a sibling suffix like "-04" when repoPath is "meta-repo", ignoring .git/objects or .git/modules alternate references,
// and rejecting incidental prose mentions when a numbered sibling worktree has dominant occurrences in the transcript).
func TranscriptReferencesRepoPath(text, repoPath string) bool {
	cleanRepo := strings.TrimRight(canonicalizeTranscriptPathText(strings.TrimSpace(repoPath)), "/")
	if cleanRepo == "" || cleanRepo == "." {
		return false
	}
	searchAll := canonicalizeTranscriptPathText(text)
	if cwdMarker := "command working directory: "; strings.Contains(searchAll, cwdMarker) {
		searchCWD := searchAll
		for {
			idx := strings.Index(searchCWD, cwdMarker)
			if idx < 0 {
				break
			}
			rest := searchCWD[idx+len(cwdMarker):]
			if nl := strings.IndexAny(rest, "\r\n\"'"); nl >= 0 {
				rest = rest[:nl]
			}
			if cutIdx := strings.Index(rest, "/napp data directory:"); cutIdx >= 0 {
				rest = rest[:cutIdx]
			}
			declaredCWD := strings.TrimRight(strings.TrimSpace(rest), "/")
			if strings.HasPrefix(declaredCWD, "/") || (len(declaredCWD) >= 3 && declaredCWD[1] == ':' && declaredCWD[2] == '/') {
				return declaredCWD == cleanRepo
			}
			searchCWD = searchCWD[idx+len(cwdMarker):]
		}
	}

	selfCount, maxOtherSiblingCount := countWorktreeFamilyOccurrences(searchAll, cleanRepo)
	if selfCount == 0 || maxOtherSiblingCount >= selfCount {
		return false
	}
	return true
}

func isTempTestHomeDir(p string) bool {
	slash := strings.ToLower(filepath.ToSlash(strings.TrimSpace(p)))
	if slash == "" {
		return false
	}
	return strings.Contains(slash, "/tmp/") ||
		strings.Contains(slash, "/var/folders/") ||
		strings.Contains(slash, "/appdata/local/temp/") ||
		strings.Contains(slash, "teste2e_") ||
		strings.Contains(slash, "go-build")
}

func candidateBrainRootDirs() []string {
	if custom := strings.TrimSpace(os.Getenv("IAZIO_BRAIN_DIR")); custom != "" {
		dirs := []string{filepath.Clean(custom)}
		if filepath.Base(filepath.Clean(custom)) != "brain" {
			sub := filepath.Join(filepath.Clean(custom), "brain")
			if fi, err := os.Stat(sub); err == nil && fi.IsDir() {
				dirs = append([]string{sub}, dirs...)
			}
		}
		return dirs
	}

	seen := make(map[string]bool)
	var dirs []string
	addDir := func(d string) {
		clean := filepath.Clean(strings.TrimSpace(d))
		if clean == "" || seen[clean] {
			return
		}
		if fi, err := os.Stat(clean); err == nil && fi.IsDir() {
			seen[clean] = true
			dirs = append(dirs, clean)
		}
	}

	home, _ := os.UserHomeDir()
	if home != "" {
		addDir(filepath.Join(home, ".gemini", "antigravity-cli", "brain"))
		if isTempTestHomeDir(home) {
			return dirs
		}
	}
	if up := strings.TrimSpace(os.Getenv("USERPROFILE")); up != "" {
		addDir(filepath.Join(up, ".gemini", "antigravity-cli", "brain"))
		if isTempTestHomeDir(up) {
			return dirs
		}
	}
	if runtime.GOOS == "linux" {
		if matches, err := filepath.Glob("/mnt/c/Users/*/.gemini/antigravity-cli/brain"); err == nil {
			for _, m := range matches {
				addDir(m)
			}
		}
	}
	return dirs
}

// InspectActiveBrainASETurnForRepo scans candidate Antigravity brain directories for an in-flight
// /autonomous-story-execution turn targeting repoPath (modified within the last 15 minutes).
func InspectActiveBrainASETurnForRepo(repoPath string, now time.Time) (active bool, convID string, storyID string, newestMod time.Time) {
	if strings.TrimSpace(repoPath) == "" || isAutomatedMaintenanceRepoLock(repoPath) {
		return false, "", "", time.Time{}
	}
	if now.IsZero() {
		now = time.Now()
	}
	for _, brainRoot := range candidateBrainRootDirs() {
		entries, err := os.ReadDir(brainRoot)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") || len(e.Name()) < 8 {
				continue
			}
			candID := e.Name()
			if isConversationClaimedByOtherWorktree(repoPath, candID) {
				continue
			}
			convBrainDir := filepath.Join(brainRoot, candID)
			for _, rel := range []string{
				filepath.Join(".system_generated", "logs", "transcript.jsonl"),
				filepath.Join(".system_generated", "logs", "transcript_full.jsonl"),
			} {
				full := filepath.Join(convBrainDir, rel)
				fi, stErr := os.Stat(full)
				if stErr != nil || fi.IsDir() {
					continue
				}
				modTime := fi.ModTime()
				if modTime.IsZero() || now.Sub(modTime) > 15*time.Minute {
					continue
				}
				head, tail, _, rErr := readHeadAndTailBytes(full, 96*1024, 128*1024)
				if rErr != nil {
					continue
				}
				headStr := string(head)
				tailStr := string(tail)
				midUserInputs := readMiddleTranscriptUserInputs(full, 96*1024, 128*1024)
				if isAutomatedMaintenanceTranscript(headStr) || isAutomatedMaintenanceTranscript(midUserInputs) || isAutomatedMaintenanceTranscript(tailStr) {
					continue
				}
				combinedForRef := headStr + "\n" + tailStr
				if midUserInputs != "" {
					combinedForRef = headStr + "\n" + midUserInputs + "\n" + tailStr
				}
				if !TranscriptReferencesRepoPath(combinedForRef, repoPath) {
					continue
				}
				if !containsASEWorkflowMarker(headStr) && !containsASEWorkflowMarker(midUserInputs) && !containsASEWorkflowMarker(tailStr) {
					continue
				}
				if !isTranscriptTurnInFlight(tail, modTime, now) {
					continue
				}
				var candStory string
				combinedHead := headStr
				if midUserInputs != "" {
					combinedHead += "\n" + midUserInputs
				}
				allMatches := extractBoundStoryMatchesFromTranscript(combinedHead, tailStr)
				for i := len(allMatches) - 1; i >= 0; i-- {
					sid := strings.ToUpper(allMatches[i][1])
					if canBindTranscriptStoryToRepo(repoPath, sid) {
						candStory = sid
						break
					}
				}
				if modTime.After(newestMod) {
					active = true
					convID = candID
					storyID = candStory
					newestMod = modTime
				}
			}
		}
	}
	return active, convID, storyID, newestMod
}

// HasActiveBrainASETurnForRepo returns true if an Antigravity brain transcript has an in-flight
// /autonomous-story-execution turn targeting repoPath.
func HasActiveBrainASETurnForRepo(repoPath string, now time.Time) bool {
	active, _, _, _ := InspectActiveBrainASETurnForRepo(repoPath, now)
	return active
}

func discoverActiveBrainTranscriptForRepo(repoPath string, now time.Time, res *RepoFleetStatusResult) bool {
	if res == nil || isAutomatedMaintenanceRepoLock(repoPath) {
		return false
	}
	active, convID, storyID, _ := InspectActiveBrainASETurnForRepo(repoPath, now)
	if !active {
		return false
	}
	res.HasActiveAgentASETurn = true
	if res.ActiveConversationID == "" && convID != "" {
		res.ActiveConversationID = convID
	}
	if res.ActiveStoryID == "" && storyID != "" && canBindTranscriptStoryToRepo(repoPath, storyID) {
		res.ActiveStoryID = storyID
	}
	if res.AgentTool == "" {
		res.AgentTool = "agy"
	}
	res.ToolLiveness = "LIVE"
	if res.ExecutionProvenance == "" || res.ExecutionProvenance == "NONE" {
		if lockInfo, err := repolock.Read(repoPath); err == nil &&
			strings.EqualFold(strings.TrimSpace(lockInfo.Status), "RUNNING") &&
			((lockInfo.PID > 0 && isPIDAlive(lockInfo.PID)) || (lockInfo.HarnessPID > 0 && isPIDAlive(lockInfo.HarnessPID))) {
			res.ExecutionProvenance = "COORDINATED_HARNESS"
		} else {
			res.ExecutionProvenance = "INDEPENDENT_USER_SESSION"
		}
	}
	return true
}

func discoverAgentProcessesFallback(repoPath string, res *RepoFleetStatusResult) {
	if isAutomatedMaintenanceRepoLock(repoPath) {
		return
	}
	if runtime.GOOS != "windows" {
		selfPID := os.Getpid()
		parentPID := os.Getppid()
		cmd := procutil.Command("ps", "-axo", "pid=,ppid=,etime=,args=")
		out, err := cmd.CombinedOutput()
		if err == nil {
			if parsePsOutputForAgentProcesses(string(out), selfPID, parentPID, repoPath, res) {
				if !res.HasActiveAgentASETurn {
					discoverActiveBrainTranscriptForRepo(repoPath, time.Now(), res)
				}
				return
			}
		}
	}
	if discoverActiveBrainTranscriptForRepo(repoPath, time.Now(), res) {
		return
	}
	if res.LockDetails != "" {
		res.ToolLiveness = "KILLED"
		res.ASEState = "ORPHANED"
		res.ExecutionProvenance = "COORDINATED_HARNESS_FAILED"
	} else if res.ASEState == "FAILED_DIRTY" || (!res.IsClean && res.ASEState != "RUNNING") {
		res.ToolLiveness = "TERMINATED"
	}
}

func parsePsOutputForAgentProcesses(psOut string, selfPID, parentPID int, repoPath string, res *RepoFleetStatusResult) bool {
	lines := strings.Split(psOut, "\n")
	type candidateProc struct {
		pid   int
		tool  string
		etime string
		line  string
	}
	var cwdCandidates []candidateProc

	for _, l := range lines {
		if isDiagnosticCmdLine(l) {
			continue
		}
		fields := strings.Fields(l)
		if len(fields) < 4 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil || pid <= 1 || pid == selfPID {
			continue
		}
		etime := fields[2]
		argv0 := fields[3]
		matchedTool, ok := isAgentTool(argv0)
		if !ok {
			base0 := strings.ToLower(filepath.Base(argv0))
			if (base0 == "env" || base0 == "sh" || base0 == "bash" || base0 == "zsh") && len(fields) > 4 {
				matchedTool, ok = isAgentTool(fields[4])
			}
		}
		if !ok || matchedTool == "" {
			continue
		}
		cmdLine := strings.Join(fields[3:], " ")

		// 1. Direct command-line path match with strict boundary verification
		nonMainBranch := res != nil && res.Branch != "" && res.Branch != "main" && res.Branch != "master" && res.Branch != "HEAD" && !isAutomatedMaintenanceBranch(res.Branch)
		if LineContainsExactRepoPath(cmdLine, repoPath) {
			prov, convID, isIdle, activeASE, storyID := inspectDarwinAgentProcessDetailed(pid, etime, cmdLine, res.LockDetails != "")
			if isIdle || prov == "NONE" {
				continue
			}
			res.AgentPID = pid
			res.AgentTool = matchedTool
			res.ToolLiveness = "LIVE"
			res.ExecutionProvenance = prov
			if activeASE {
				res.HasActiveAgentASETurn = true
			}
			if storyID != "" && res.ActiveStoryID == "" && canBindTranscriptStoryToRepo(repoPath, storyID) &&
				(activeASE || IsStoryExplicitlyInProgressOnDisk(repoPath, storyID) || nonMainBranch || len(res.NonMainSubmodules) > 0) {
				res.ActiveStoryID = storyID
			}
			if convID != "" && res.ActiveConversationID == "" {
				res.ActiveConversationID = convID
			}
			return true
		}
		cwdCandidates = append(cwdCandidates, candidateProc{pid: pid, tool: matchedTool, etime: etime, line: cmdLine})
	}

	// 2. Check working directory of candidate agent processes via lsof -a -d cwd -p <pids> -F pn
	if len(cwdCandidates) > 0 {
		nonMainBranch := res != nil && res.Branch != "" && res.Branch != "main" && res.Branch != "master" && res.Branch != "HEAD" && !isAutomatedMaintenanceBranch(res.Branch)
		pidStrs := make([]string, 0, len(cwdCandidates))
		pidToCand := make(map[int]candidateProc, len(cwdCandidates))
		for _, c := range cwdCandidates {
			pidStrs = append(pidStrs, strconv.Itoa(c.pid))
			pidToCand[c.pid] = c
		}
		lsofCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		lsofCmd := procutil.CommandContext(lsofCtx, "lsof", "-n", "-P", "-a", "-d", "cwd", "-p", strings.Join(pidStrs, ","), "-F", "pn")
		lsofOut, lsofErr := lsofCmd.Output()
		cancel()
		if lsofErr == nil && len(lsofOut) > 0 {
			currPID := 0
			for _, line := range strings.Split(string(lsofOut), "\n") {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "p") {
					currPID, _ = strconv.Atoi(strings.TrimPrefix(line, "p"))
				} else if strings.HasPrefix(line, "n") && currPID > 0 {
					cwd := strings.TrimPrefix(line, "n")
					if HasExactRepoPathBoundary(cwd, repoPath) {
						cand := pidToCand[currPID]
						prov, convID, isIdle, activeASE, storyID := inspectDarwinAgentProcessDetailed(currPID, cand.etime, cand.line, res.LockDetails != "")
						if isIdle || prov == "NONE" {
							continue
						}
						res.AgentPID = currPID
						res.AgentTool = cand.tool
						res.ToolLiveness = "LIVE"
						res.ExecutionProvenance = prov
						if activeASE {
							res.HasActiveAgentASETurn = true
						}
						if storyID != "" && res.ActiveStoryID == "" && canBindTranscriptStoryToRepo(repoPath, storyID) &&
							(activeASE || IsStoryExplicitlyInProgressOnDisk(repoPath, storyID) || nonMainBranch || len(res.NonMainSubmodules) > 0) {
							res.ActiveStoryID = storyID
						}
						if convID != "" && res.ActiveConversationID == "" {
							res.ActiveConversationID = convID
						}
						return true
					}
				}
			}
		}
	}
	return false
}

// ReapOrphanedProcesses terminates any orphaned child/helper processes whose cwd is inside repoPath.
func ReapOrphanedProcesses(ctx context.Context, repoPath string) error {
	if runtime.GOOS != "linux" {
		return nil
	}
	cleanPath := filepath.Clean(repoPath)
	procDir, err := os.Open("/proc")
	if err != nil {
		return err
	}
	defer procDir.Close()

	entries, err := procDir.Readdirnames(-1)
	if err != nil {
		return err
	}

	for _, name := range entries {
		pid, err := strconv.Atoi(name)
		if err != nil || pid <= 1 {
			continue
		}
		cwdPath := filepath.Join("/proc", name, "cwd")
		targetCwd, err := os.Readlink(cwdPath)
		if err != nil {
			continue
		}
		if !HasExactRepoPathBoundary(targetCwd, cleanPath) {
			continue
		}

		cmdlineBytes, _ := os.ReadFile(filepath.Join("/proc", name, "cmdline"))
		cmdArgs := strings.Split(string(cmdlineBytes), "\x00")
		if len(cmdArgs) == 0 || cmdArgs[0] == "" {
			continue
		}

		_, isAux := isAuxiliaryTool(cmdArgs[0])
		if !isAux {
			continue
		}

		statBytes, _ := os.ReadFile(filepath.Join("/proc", name, "stat"))
		statFields := strings.Fields(string(statBytes))
		ppid := 0
		if len(statFields) > 3 {
			ppid, _ = strconv.Atoi(statFields[3])
		}

		if ppid == 1 || !isPIDAlive(ppid) {
			if proc, err := os.FindProcess(pid); err == nil {
				_ = proc.Kill()
			}
		}
	}
	return nil
}

// ResetASELocks removes stale lock files, purges untracked .session_story_id (or restores tracked .session_story_id to HEAD)
// and tmp/ase-resume-*.json, and resets .autopilot/state.json to IDLE.
func ResetASELocks(repoPath string) error {
	cleanPath := filepath.Clean(repoPath)
	locks := []string{
		filepath.Join(cleanPath, ".autopilot", "lock"),
		filepath.Join(cleanPath, ".ase", "active.lock"),
		filepath.Join(cleanPath, ".index.yml.lock"),
	}
	for _, l := range locks {
		_ = os.Remove(l)
	}
	_, _ = cleanOrRestoreSessionStoryFile(cleanPath, "reset")
	_ = repolock.ForceRelease(cleanPath)

	if matches, err := filepath.Glob(filepath.Join(cleanPath, "tmp", "ase-resume-*.json")); err == nil {
		for _, m := range matches {
			_ = os.Remove(m)
		}
	}

	stateFile := filepath.Join(cleanPath, ".autopilot", "state.json")
	if b, err := os.ReadFile(stateFile); err == nil {
		var s map[string]any
		if json.Unmarshal(b, &s) == nil {
			s["state"] = "IDLE"
			s["story_id"] = ""
			s["current_phase"] = ""
			s["lock"] = nil
			s["active_job_id"] = ""
			s["updated_at"] = time.Now().UTC().Format(time.RFC3339)
			if updated, errM := json.MarshalIndent(s, "", "  "); errM == nil {
				_ = os.WriteFile(stateFile, updated, 0o644)
			}
		}
	}
	return nil
}

func buildDiagnosticSummary(res *RepoFleetStatusResult) {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Repository: %s\n", res.RepoPath))
	sb.WriteString(fmt.Sprintf("Branch: %s (head: %.8s)", res.Branch, res.HeadSHA))
	if res.Ahead > 0 || res.Behind > 0 {
		sb.WriteString(fmt.Sprintf(" [ahead +%d, behind -%d]", res.Ahead, res.Behind))
	}
	sb.WriteString("\n")

	if res.IsClean {
		if res.WorktreeState == "CLEAN_SUBMODULE_DRIFT" {
			sb.WriteString("Working Tree: CLEAN (SUBMODULE SHA DRIFT)\n")
		} else {
			sb.WriteString("Working Tree: CLEAN\n")
		}
	} else {
		sb.WriteString(fmt.Sprintf("Working Tree: %s (%d modified, %d untracked)\n",
			res.WorktreeState, len(res.ModifiedFiles), len(res.UntrackedFiles)))
		for _, f := range res.ModifiedFiles {
			sb.WriteString(fmt.Sprintf("  M  %s\n", f))
		}
		for _, f := range res.UntrackedFiles {
			sb.WriteString(fmt.Sprintf("  ?? %s\n", f))
		}
	}

	if len(res.SubmoduleDrift) > 0 {
		sb.WriteString(fmt.Sprintf("Submodule Drift: %s\n", strings.Join(res.SubmoduleDrift, ", ")))
	}

	if res.SubmodulesClean {
		sb.WriteString("Submodules: ALL CLEAN (ON PRIMARY MAIN)\n")
	} else {
		var details []string
		if len(res.DirtySubmodules) > 0 {
			details = append(details, fmt.Sprintf("dirty: %s", strings.Join(res.DirtySubmodules, ", ")))
		}
		if len(res.NonMainSubmodules) > 0 {
			details = append(details, fmt.Sprintf("non-main branches: %s", strings.Join(res.NonMainSubmodules, ", ")))
		}
		if len(details) == 0 {
			details = append(details, "submodules not aligned with primary main branch")
		}
		sb.WriteString(fmt.Sprintf("Submodules: INCOMPLETE / NON-MAIN / DIRTY DETECTED (%s)\n",
			strings.Join(details, "; ")))
	}

	sb.WriteString(fmt.Sprintf("ASE State: %s", res.ASEState))
	if res.ActiveStoryID != "" {
		sb.WriteString(fmt.Sprintf(" (Story: %s, Phase: %s)", res.ActiveStoryID, res.CurrentPhase))
	}
	sb.WriteString("\n")

	if res.LockDetails != "" {
		sb.WriteString(fmt.Sprintf("Active Locks: %s\n", res.LockDetails))
	}

	if len(res.OrphanedProcesses) > 0 {
		sb.WriteString(fmt.Sprintf("Orphaned Helper Processes: %s\n", strings.Join(res.OrphanedProcesses, ", ")))
	}

	if res.AgentTool != "" {
		sb.WriteString(fmt.Sprintf("Agent Tool: %s (PID: %d, Liveness: %s)\n",
			res.AgentTool, res.AgentPID, res.ToolLiveness))
	} else {
		sb.WriteString("Agent Tool: NONE ACTIVE\n")
	}

	sb.WriteString(fmt.Sprintf("Provenance: %s\n", res.ExecutionProvenance))
	if res.ActiveConversationID != "" {
		sb.WriteString(fmt.Sprintf("Conversation ID: %s\n", res.ActiveConversationID))
	}
	if len(res.RemediationActions) > 0 {
		sb.WriteString(fmt.Sprintf("Self-Remediation Actions: %s\n", strings.Join(res.RemediationActions, "; ")))
	}

	res.DiagnosticSummary = sb.String()
}

func readGitWorktreeBranch(worktreePath string) (string, bool) {
	dotGit := filepath.Join(worktreePath, ".git")
	fi, err := os.Stat(dotGit)
	if err != nil {
		return "", false
	}
	gitDir := dotGit
	if !fi.IsDir() {
		raw, err := os.ReadFile(dotGit)
		if err != nil {
			return "", false
		}
		line := strings.TrimSpace(string(raw))
		if !strings.HasPrefix(line, "gitdir:") {
			return "", false
		}
		rel := strings.TrimSpace(strings.TrimPrefix(line, "gitdir:"))
		if filepath.IsAbs(rel) {
			gitDir = rel
		} else {
			gitDir = filepath.Join(worktreePath, rel)
		}
	}
	headRaw, err := os.ReadFile(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		return "", false
	}
	head := strings.TrimSpace(string(headRaw))
	if strings.HasPrefix(head, "ref: refs/heads/") {
		return strings.TrimPrefix(head, "ref: refs/heads/"), true
	}
	if head != "" {
		return "detached", true
	}
	return "", false
}
