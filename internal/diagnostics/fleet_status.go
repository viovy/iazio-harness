// Package diagnostics provides deep inspection of fleet git status, ASE state, and agent provenance.
package diagnostics

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/viovy/iazio-harness/internal/procutil"
	"github.com/viovy/iazio-harness/internal/repolock"
)

var (
	branchExplicitStoryRe = regexp.MustCompile(`(?i)\b(STORY-[A-Z0-9]+-[0-9]+)\b`)
	branchSlugStoryRe     = regexp.MustCompile(`(?i)^(?:chore|feat|fix|refactor|docs|test|ci|perf|build)/(?:story-)?([a-z]{2,10})-([0-9]{2,6})(?:[-/]|$)`)
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
	ToolLiveness         string   `json:"tool_liveness"`        // LIVE, TERMINATED, KILLED, NONE
	ExecutionProvenance  string   `json:"execution_provenance"` // INDEPENDENT_USER_SESSION, COORDINATED_HARNESS, COORDINATED_HARNESS_FAILED, NONE
	ActiveConversationID string   `json:"active_conversation_id,omitempty"`
	MetadataYmlRaw       string   `json:"metadata_yml_raw,omitempty"`
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

	// 0. Check git lock sentinels (and prune abandoned >5m .lock files)
	inspectGitLockSentinels(cleanPath, opts.AutoRemediateTier1, res)

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

	// 3. ASE State & Autopilot locks (with Tier 1 completed-ASE artifact cleanup)
	parseASEStateWithOpts(cleanPath, opts.AutoRemediateTier1, res)

	// 4. Process Discovery & Provenance
	discoverAgentProcesses(cleanPath, res)

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

func inspectGitLockSentinels(repoPath string, autoPruneStale bool, res *RepoFleetStatusResult) {
	gitDir := filepath.Join(repoPath, ".git")
	fi, err := os.Stat(gitDir)
	if err != nil {
		return
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

// IsStoryCompletedOnDisk returns true if storyID is marked DONE in .autopilot/ase-state.json,
// docs/stories/*.story.md, child family docs/stories/*.story.md, or docs/stories/index.yml.
func IsStoryCompletedOnDisk(repoPath, storyID string) bool {
	sid := strings.ToUpper(strings.TrimSpace(storyID))
	if repoPath == "" || !strings.HasPrefix(sid, "STORY-") {
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
		}
	}

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
				if strings.Contains(strings.ToUpper(content), sid) &&
					(strings.Contains(content, "\nstatus: DONE\n") ||
						strings.Contains(content, "\nstatus: DONE\r\n") ||
						strings.Contains(content, "\nstatus: \"DONE\"\n") ||
						strings.Contains(content, "\nstatus: 'DONE'\n")) {
					return true
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
	canPruneCompletedStory := isMainBranch && len(res.NonMainSubmodules) == 0

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
				res.ActiveStoryID = s.StoryID
				if !staleIdlePhase {
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
				if nonMainBranch || len(res.NonMainSubmodules) > 0 || hasResumeFile {
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
		strings.Contains(lower, "status --json") ||
		strings.Contains(lower, "auth status") ||
		strings.Contains(lower, "autopilot-mcp") ||
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
	if res == nil || res.HasActiveGitLock || res.LockDetails != "" || res.ToolLiveness == "LIVE" || res.ASEState == "RUNNING" {
		return false
	}
	if len(res.ModifiedFiles) > 0 || len(res.DirtySubmodules) > 0 {
		return false
	}
	inferredStory := InferStoryIDFromBranch(res.Branch)
	eligible := isAutomatedMaintenanceBranch(res.Branch) ||
		(inferredStory != "" && IsStoryCompletedOnDisk(cleanPath, inferredStory) && hasExplicitIdleStateFile(cleanPath))
	if !eligible {
		return false
	}
	if lockInfo, err := repolock.Read(cleanPath); err == nil && lockInfo.PID > 0 && isPIDAlive(lockInfo.PID) {
		return false
	}
	prevBranch := res.Branch
	checkoutCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	checkoutErr := procutil.CommandContext(checkoutCtx, "git", "-C", cleanPath, "checkout", "main").Run()
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
	if res.ActiveStoryID != "" && res.ToolLiveness != "LIVE" && res.LockDetails == "" && !hasScopedResume &&
		IsStoryCompletedOnDisk(repoPath, res.ActiveStoryID) && hasExplicitIdleStateFile(repoPath) {
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
		if res.ASEState == "RUNNING" {
			if res.WorktreeState == "DIRTY_MODIFIED" {
				res.ASEState = "FAILED_DIRTY"
			} else {
				res.ASEState = "IDLE"
			}
		}
		res.ExecutionProvenance = "NONE"
	}

	// Strict Ad-Hoc ASE Evidence Gate:
	// An interactive agent shell (agy/cursor/opencode/claude) sitting on clean main or an automated
	// maintenance branch (chore/governance-pointers-*) with no bound story, no locks, and no autopilot*
	// process is an idle shell — NOT an active ad-hoc ASE execution.
	hasAffirmativeASEEvidence := res.LockDetails != "" ||
		isAutopilotProcess ||
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
	if res.ActiveStoryID != "" && res.ASEState != "RUNNING" && !nonMainBranch &&
		len(res.NonMainSubmodules) == 0 && res.LockDetails == "" && !hasScopedResume && res.ToolLiveness != "LIVE" {
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
	parentPID := os.Getppid()

	for _, name := range entries {
		pid, err := strconv.Atoi(name)
		if err != nil || pid <= 1 || pid == selfPID || pid == parentPID {
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
		if match && res.AgentTool == "" {
			res.AgentTool = toolName
			res.AgentPID = pid
			res.ToolLiveness = "LIVE"

			// Extract conversation ID from cmdArgs if present
			for i := 0; i < len(cmdArgs)-1; i++ {
				if cmdArgs[i] == "--conversation" || cmdArgs[i] == "-c" || strings.HasPrefix(cmdArgs[i], "--conversation=") {
					if strings.HasPrefix(cmdArgs[i], "--conversation=") {
						res.ActiveConversationID = strings.TrimPrefix(cmdArgs[i], "--conversation=")
					} else {
						res.ActiveConversationID = cmdArgs[i+1]
					}
					break
				}
			}

			// Read environ for execution provenance
			environBytes, _ := os.ReadFile(filepath.Join("/proc", name, "environ"))
			environStr := string(environBytes)
			if strings.Contains(environStr, "IAZIO_MANAGED_BY=harness") ||
				strings.Contains(environStr, "IAZIO_HARNESS_JOB_ID") ||
				strings.Contains(environStr, "IAZIO_AGENT_MANAGED=1") {
				res.ExecutionProvenance = "COORDINATED_HARNESS"
			} else {
				res.ExecutionProvenance = "INDEPENDENT_USER_SESSION"
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

	// If no running agent tool PID was found
	if res.AgentTool == "" {
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
	seenBrains := make(map[string]bool)
	for _, line := range strings.Split(lsofOut, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "n") {
			continue
		}
		p := strings.TrimPrefix(line, "n")
		idx := strings.Index(p, "/brain/")
		if idx < 0 {
			continue
		}
		rest := p[idx+len("/brain/"):]
		slashIdx := strings.IndexByte(rest, '/')
		candID := rest
		if slashIdx >= 0 {
			candID = rest[:slashIdx]
		}
		if len(candID) < 8 || strings.HasPrefix(candID, ".") || seenBrains[candID] {
			continue
		}
		seenBrains[candID] = true
		brainDir := p[:idx+len("/brain/")+len(candID)]
		for _, rel := range []string{
			filepath.Join(".system_generated", "logs", "transcript.jsonl"),
			filepath.Join(".system_generated", "logs", "transcript_full.jsonl"),
		} {
			if fi, stErr := os.Stat(filepath.Join(brainDir, rel)); stErr == nil {
				if fi.ModTime().After(newestTranscript) {
					newestTranscript = fi.ModTime()
					if convID == "" {
						convID = candID
					}
				}
			}
		}
	}
	return convID, newestTranscript
}

// inspectDarwinAgentProcess determines provenance, active conversation ID, and whether an interactive
// CLI process has been sitting idle at a prompt (>15m elapsed with no recent brain transcript activity).
func inspectDarwinAgentProcess(pid int, etime string, cmdLine string, hasRepoLock bool) (provenance string, convID string, isIdle bool) {
	provenance = "INDEPENDENT_USER_SESSION"
	convID = extractConversationIDFromArgs(strings.Fields(cmdLine))

	envCtx, cancelEnv := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	envOut, envErr := procutil.CommandContext(envCtx, "ps", "eww", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	cancelEnv()
	if envErr == nil {
		envStr := string(envOut)
		if strings.Contains(envStr, "IAZIO_MANAGED_BY=harness") ||
			strings.Contains(envStr, "IAZIO_HARNESS_JOB_ID") ||
			strings.Contains(envStr, "IAZIO_AGENT_MANAGED=1") {
			provenance = "COORDINATED_HARNESS"
		}
	}
	if provenance == "COORDINATED_HARNESS" {
		return provenance, convID, false
	}

	elapsed := parsePsElapsedDuration(etime)
	lsofCtx, cancelLsof := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	lsofOut, lsofErr := procutil.CommandContext(lsofCtx, "lsof", "-p", strconv.Itoa(pid), "-F", "n").Output()
	cancelLsof()
	var newestTranscript time.Time
	if lsofErr == nil && len(lsofOut) > 0 {
		lsofConv, ts := parseDarwinLsofBrainTranscripts(string(lsofOut))
		newestTranscript = ts
		if convID == "" && lsofConv != "" {
			convID = lsofConv
		}
	}

	if !hasRepoLock && elapsed > 15*time.Minute {
		if newestTranscript.IsZero() || time.Since(newestTranscript) > 15*time.Minute {
			return provenance, convID, true
		}
	}
	return provenance, convID, false
}

func discoverAgentProcessesFallback(repoPath string, res *RepoFleetStatusResult) {
	if runtime.GOOS != "windows" {
		selfPID := os.Getpid()
		parentPID := os.Getppid()
		cmd := procutil.Command("ps", "-axo", "pid=,ppid=,etime=,args=")
		out, err := cmd.CombinedOutput()
		if err != nil {
			return
		}
		if parsePsOutputForAgentProcesses(string(out), selfPID, parentPID, repoPath, res) {
			return
		}
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
		if err != nil || pid <= 1 || pid == selfPID || pid == parentPID {
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
		if LineContainsExactRepoPath(cmdLine, repoPath) {
			prov, convID, isIdle := inspectDarwinAgentProcess(pid, etime, cmdLine, res.LockDetails != "")
			if isIdle {
				continue
			}
			res.AgentPID = pid
			res.AgentTool = matchedTool
			res.ToolLiveness = "LIVE"
			res.ExecutionProvenance = prov
			if convID != "" && res.ActiveConversationID == "" {
				res.ActiveConversationID = convID
			}
			return true
		}
		cwdCandidates = append(cwdCandidates, candidateProc{pid: pid, tool: matchedTool, etime: etime, line: cmdLine})
	}

	// 2. Check working directory of candidate agent processes via lsof -a -d cwd -p <pids> -F pn
	if len(cwdCandidates) > 0 {
		pidStrs := make([]string, 0, len(cwdCandidates))
		pidToCand := make(map[int]candidateProc, len(cwdCandidates))
		for _, c := range cwdCandidates {
			pidStrs = append(pidStrs, strconv.Itoa(c.pid))
			pidToCand[c.pid] = c
		}
		lsofCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		lsofCmd := procutil.CommandContext(lsofCtx, "lsof", "-a", "-d", "cwd", "-p", strings.Join(pidStrs, ","), "-F", "pn")
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
						prov, convID, isIdle := inspectDarwinAgentProcess(currPID, cand.etime, cand.line, res.LockDetails != "")
						if isIdle {
							continue
						}
						res.AgentPID = currPID
						res.AgentTool = cand.tool
						res.ToolLiveness = "LIVE"
						res.ExecutionProvenance = prov
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
