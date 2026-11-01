// Package gitclean provides deterministic workspace reset and untracked file cleanup
// for pure code actions without AI agent (agy/agent/opencode) execution (STORY-GOV-3745).
package gitclean

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/viovy/iazio-harness/internal/procutil"
	"github.com/viovy/iazio-harness/internal/repolock"
)

// DiscardOptions configures the workspace discard/clean execution.
type DiscardOptions struct {
	WorktreePath string
	Force        bool
	Submodules   bool
	RemoveLocks  bool
	OnProgress   func(stage string, detail string)
}

// DiscardResult contains the outcome of the discard and clean operation.
type DiscardResult struct {
	WorktreePath       string        `json:"worktree_path"`
	Success            bool          `json:"success"`
	Clean              bool          `json:"clean"`
	FilesReset         int           `json:"files_reset"`
	LocksRemoved       []string      `json:"locks_removed"`
	OperationsAborted  []string      `json:"operations_aborted"`
	SubmodulesCleaned  int           `json:"submodules_cleaned"`
	RemainingPorcelain string        `json:"remaining_porcelain,omitempty"`
	Duration           time.Duration `json:"duration_ns"`
	DurationStr        string        `json:"duration"`
	Message            string        `json:"message"`
}

// String returns a human-readable summary of the discard result.
func (r *DiscardResult) String() string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("[iazio-harness] Workspace Discard Report: %s\n", r.WorktreePath))
	sb.WriteString(fmt.Sprintf("  - Success: %t\n", r.Success))
	sb.WriteString(fmt.Sprintf("  - Worktree Clean: %t\n", r.Clean))
	sb.WriteString(fmt.Sprintf("  - Duration: %s\n", r.DurationStr))
	if len(r.LocksRemoved) > 0 {
		sb.WriteString(fmt.Sprintf("  - Stale Locks Removed (%d): %s\n", len(r.LocksRemoved), strings.Join(r.LocksRemoved, ", ")))
	}
	if len(r.OperationsAborted) > 0 {
		sb.WriteString(fmt.Sprintf("  - In-Progress Operations Aborted: %s\n", strings.Join(r.OperationsAborted, ", ")))
	}
	if r.SubmodulesCleaned > 0 {
		sb.WriteString(fmt.Sprintf("  - Submodules Recursively Cleaned: %d\n", r.SubmodulesCleaned))
	}
	if r.RemainingPorcelain != "" {
		sb.WriteString(fmt.Sprintf("  - Remaining Dirty Files:\n%s\n", r.RemainingPorcelain))
	} else {
		sb.WriteString("  - Status: git status is 100% clean\n")
	}
	return sb.String()
}

// JSON returns a formatted JSON representation of the discard result.
func (r *DiscardResult) JSON() ([]byte, error) {
	return json.MarshalIndent(r, "", "  ")
}

// IsPureCodeAction checks whether a given job kind or action represents a pure code action
// like workspace discard or git reset that does not require AI agent execution or conversation checkpoints.
func IsPureCodeAction(kind string) bool {
	k := strings.ToLower(strings.TrimSpace(kind))
	switch k {
	case "discard", "git_clean", "clean", "git_reset", "reset_workspace", "heal_git", "force_clean_git":
		return true
	default:
		return false
	}
}

// DiscardWorkspace performs an aggressive, deterministic git reset and clean on the target workspace.
// It removes stale locks, aborts conflicts, resets modified files, removes all untracked files and dirs (-ffdx),
// recurses into all submodules, and verifies clean status.
func DiscardWorkspace(ctx context.Context, opts DiscardOptions) (*DiscardResult, error) {
	start := time.Now()
	res := &DiscardResult{
		WorktreePath: opts.WorktreePath,
		Success:      false,
		Clean:        false,
	}

	reportProgress := func(stage, detail string) {
		if opts.OnProgress != nil {
			opts.OnProgress(stage, detail)
		}
	}

	targetDir := strings.TrimSpace(opts.WorktreePath)
	if targetDir == "" {
		targetDir = "."
	}
	absTarget, err := filepath.Abs(targetDir)
	if err != nil {
		absTarget = targetDir
	}
	res.WorktreePath = absTarget

	reportProgress("START", fmt.Sprintf("Beginning workspace discard on %s", absTarget))

	// 1. Resolve git directory
	gitDir := filepath.Join(absTarget, ".git")
	revCmd := procutil.CommandContext(ctx, "git", "-C", absTarget, "rev-parse", "--git-dir")
	revCmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if out, err := revCmd.Output(); err == nil {
		clean := strings.TrimSpace(string(out))
		if clean != "" {
			if filepath.IsAbs(clean) {
				gitDir = clean
			} else {
				gitDir = filepath.Join(absTarget, clean)
			}
		}
	}

	// 2. Remove stale git locks and harness/autopilot locks
	lockCandidates := []string{
		filepath.Join(gitDir, "index.lock"),
		filepath.Join(gitDir, "HEAD.lock"),
		filepath.Join(gitDir, "config.lock"),
		filepath.Join(gitDir, "FETCH_HEAD.lock"),
		filepath.Join(gitDir, "ORIG_HEAD.lock"),
		filepath.Join(gitDir, "MERGE_HEAD"),
		filepath.Join(gitDir, "CHERRY_PICK_HEAD"),
		filepath.Join(gitDir, "REVERT_HEAD"),
		filepath.Join(absTarget, ".ase", "active.lock"),
		filepath.Join(absTarget, ".autopilot", "lock"),
	}
	for _, lp := range lockCandidates {
		if fi, err := os.Stat(lp); err == nil && !fi.IsDir() {
			if rmErr := os.Remove(lp); rmErr == nil {
				res.LocksRemoved = append(res.LocksRemoved, filepath.Base(lp))
			}
		}
	}
	_ = repolock.ForceRelease(absTarget)
	if len(res.LocksRemoved) > 0 {
		reportProgress("LOCKS_PURGED", fmt.Sprintf("Purged %d stale locks (%s)", len(res.LocksRemoved), strings.Join(res.LocksRemoved, ", ")))
	}

	// 3. Abort in-progress git operations
	ops := []string{"merge", "rebase", "cherry-pick", "revert"}
	for _, op := range ops {
		abortCmd := procutil.CommandContext(ctx, "git", "-C", absTarget, op, "--abort")
		abortCmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		if err := abortCmd.Run(); err == nil {
			res.OperationsAborted = append(res.OperationsAborted, op)
		}
	}
	if len(res.OperationsAborted) > 0 {
		reportProgress("OPERATIONS_ABORTED", fmt.Sprintf("Aborted in-progress: %s", strings.Join(res.OperationsAborted, ", ")))
	}

	// 4. Hard reset of tracked files
	reportProgress("RESET_HEAD", "Executing git reset --hard HEAD")
	resetCmd := procutil.CommandContext(ctx, "git", "-C", absTarget, "reset", "--hard", "HEAD")
	resetCmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if out, err := resetCmd.CombinedOutput(); err != nil {
		res.Duration = time.Since(start)
		res.DurationStr = res.Duration.Truncate(time.Millisecond).String()
		res.Message = fmt.Sprintf("git reset --hard HEAD failed: %v (%s)", err, strings.TrimSpace(string(out)))
		return res, fmt.Errorf("git reset: %w: %s", err, string(out))
	}

	// 5. Force-clean all untracked files and directories (-ffdx)
	reportProgress("CLEAN_UNTRACKED", "Executing git clean -ffdx to remove all untracked and new files")
	cleanCmd := procutil.CommandContext(ctx, "git", "-C", absTarget, "clean", "-ffdx")
	cleanCmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if out, err := cleanCmd.CombinedOutput(); err != nil {
		res.Duration = time.Since(start)
		res.DurationStr = res.Duration.Truncate(time.Millisecond).String()
		res.Message = fmt.Sprintf("git clean -ffdx failed: %v (%s)", err, strings.TrimSpace(string(out)))
		return res, fmt.Errorf("git clean: %w: %s", err, string(out))
	}

	// 6. Submodule reset and clean (if enabled, default true)
	if opts.Submodules {
		reportProgress("SUBMODULES", "Recursively resetting and cleaning all submodules")
		// Submodule reset
		subReset := procutil.CommandContext(ctx, "git", "-C", absTarget, "submodule", "foreach", "--recursive", "git reset --hard HEAD || true")
		subReset.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		_ = subReset.Run()

		// Submodule clean
		subClean := procutil.CommandContext(ctx, "git", "-C", absTarget, "submodule", "foreach", "--recursive", "git clean -ffdx || true")
		subClean.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		_ = subClean.Run()

		// Submodule update to pin
		subUpdate := procutil.CommandContext(ctx, "git", "-C", absTarget, "submodule", "update", "--init", "--recursive", "--force")
		subUpdate.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		_ = subUpdate.Run()

		// Count submodules
		subCountCmd := procutil.CommandContext(ctx, "git", "-C", absTarget, "submodule", "status")
		if subOut, err := subCountCmd.Output(); err == nil {
			lines := strings.Split(strings.TrimSpace(string(subOut)), "\n")
			count := 0
			for _, l := range lines {
				if strings.TrimSpace(l) != "" {
					count++
				}
			}
			res.SubmodulesCleaned = count
		}
	}

	// 7. Post-condition verification
	statusCmd := procutil.CommandContext(ctx, "git", "-C", absTarget, "status", "--porcelain")
	statusCmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	statusOut, _ := statusCmd.Output()
	porcelain := strings.TrimSpace(string(statusOut))

	res.Duration = time.Since(start)
	res.DurationStr = res.Duration.Truncate(time.Millisecond).String()
	res.RemainingPorcelain = porcelain

	if porcelain == "" {
		res.Clean = true
		res.Success = true
		res.Message = "Workspace successfully reset to pristine git state (clean)"
		reportProgress("COMPLETE", res.Message)
	} else {
		res.Clean = false
		res.Success = false
		res.Message = fmt.Sprintf("Workspace has remaining uncleaned porcelain entries: %s", porcelain)
		reportProgress("DIRTY_REMAINS", res.Message)
	}

	return res, nil
}
