package diagnostics

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/viovy/iazio-harness/internal/gitclean"
	"github.com/viovy/iazio-harness/internal/procutil"
	"github.com/viovy/iazio-harness/internal/repolock"
)

// RemediateStrategy defines the self-remediation tier to execute on a repository.
type RemediateStrategy string

const (
	// StrategyTier1 performs non-destructive cleanup of completed/stale ASE session artifacts,
	// expired git locks (>5m), and orphaned auxiliary helper processes.
	StrategyTier1 RemediateStrategy = "tier1"
	// StrategySafe performs Tier 1 plus safe idle-main remediation: discards known ephemeral
	// IDE/daemon cache noise (e.g. .gradle/*/fileHashes/*, .DS_Store, phantom CRLF drift),
	// fast-forwards clean main/master when behind origin, and aligns submodule gitlinks.
	StrategySafe RemediateStrategy = "safe"
	// StrategyForceClean performs Tier 3 full deterministic workspace reset across root and
	// all submodules (locks, processes, git reset --hard, git clean -ffdx, submodule reset/init).
	StrategyForceClean RemediateStrategy = "force-clean"
)

// RemediateOptions configures a repository remediation execution.
type RemediateOptions struct {
	RepoPath string            `json:"repo_path"`
	Strategy RemediateStrategy `json:"strategy"`
}

// RemediateResult reports the actions taken and resulting repository posture.
type RemediateResult struct {
	RepoPath    string                 `json:"repo_path"`
	Strategy    RemediateStrategy      `json:"strategy"`
	Success     bool                   `json:"success"`
	Skipped     bool                   `json:"skipped,omitempty"`
	SkipReason  string                 `json:"skip_reason,omitempty"`
	Actions     []string               `json:"actions,omitempty"`
	FleetStatus *RepoFleetStatusResult `json:"fleet_status,omitempty"`
	DurationMs  int64                  `json:"duration_ms"`
	Message     string                 `json:"message"`
}

// NormalizeStrategy maps CLI strategy strings to canonical RemediateStrategy values.
func NormalizeStrategy(raw string) RemediateStrategy {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "tier1", "ephemeral", "ase":
		return StrategyTier1
	case "", "safe", "auto", "tier2", "safe-closeout":
		return StrategySafe
	case "force-clean", "force", "force_clean", "tier3", "discard", "hard":
		return StrategyForceClean
	default:
		return StrategySafe
	}
}

// IsEphemeralCachePath returns true for known background IDE/daemon cache or OS metadata files
// that safely regenerate and should not block a repository on main.
func IsEphemeralCachePath(relPath string) bool {
	slash := filepath.ToSlash(strings.TrimSpace(relPath))
	base := filepath.Base(slash)
	if base == ".DS_Store" || base == "Thumbs.db" || base == ".session_story_id" {
		return true
	}
	if strings.Contains(slash, "/.gradle/") || strings.HasPrefix(slash, ".gradle/") ||
		strings.Contains(slash, "/build/tmp/") || strings.Contains(slash, "/.dart_tool/") {
		return true
	}
	return false
}

// RemediateRepo executes the requested self-remediation tier on repoPath and returns the updated posture.
func RemediateRepo(ctx context.Context, opts RemediateOptions) (*RemediateResult, error) {
	start := time.Now()
	cleanPath := filepath.Clean(strings.TrimSpace(opts.RepoPath))
	if cleanPath == "" || cleanPath == "." {
		if wd, err := os.Getwd(); err == nil {
			cleanPath = wd
		}
	}
	if r, err := filepath.EvalSymlinks(cleanPath); err == nil {
		cleanPath = r
	}

	strategy := NormalizeStrategy(string(opts.Strategy))
	res := &RemediateResult{
		RepoPath: cleanPath,
		Strategy: strategy,
		Success:  true,
	}

	// Tier 3: Force Clean
	if strategy == StrategyForceClean {
		_ = ReapOrphanedProcesses(ctx, cleanPath)
		res.Actions = append(res.Actions, "reaped orphaned helper processes")
		_ = ResetASELocks(cleanPath)
		res.Actions = append(res.Actions, "reset ASE locks and session artifacts")

		discRes, err := gitclean.DiscardWorkspace(ctx, gitclean.DiscardOptions{
			WorktreePath: cleanPath,
			Force:        true,
			Submodules:   true,
			RemoveLocks:  true,
		})
		if discRes != nil && len(discRes.LocksRemoved) > 0 {
			res.Actions = append(res.Actions, fmt.Sprintf("removed locks: %s", strings.Join(discRes.LocksRemoved, ", ")))
		}
		if err != nil {
			res.Success = false
			res.Message = fmt.Sprintf("force-clean failed: %v", err)
			res.DurationMs = time.Since(start).Milliseconds()
			return res, err
		}
		res.Actions = append(res.Actions, "discarded uncommitted/untracked changes across root and submodules")

		st, _ := InspectRepoFleetStatusWithOptions(ctx, cleanPath, InspectOptions{
			AutoRemediateTier1: true,
			AutoRemediateSafe:  false,
		})
		res.FleetStatus = st
		res.DurationMs = time.Since(start).Milliseconds()
		res.Message = fmt.Sprintf("completed Tier 3 force-clean on %s", cleanPath)
		return res, nil
	}

	// Tier 1: Reap orphaned processes and inspect with inline Tier 1 ASE artifact cleanup
	_ = ReapOrphanedProcesses(ctx, cleanPath)
	st, err := InspectRepoFleetStatusWithOptions(ctx, cleanPath, InspectOptions{
		AutoRemediateTier1: true,
		AutoRemediateSafe:  false,
	})
	if err != nil {
		res.Success = false
		res.Message = fmt.Sprintf("fleet-status evaluation failed: %v", err)
		res.DurationMs = time.Since(start).Milliseconds()
		return res, err
	}
	if len(st.RemediationActions) > 0 {
		res.Actions = append(res.Actions, st.RemediationActions...)
	}

	if strategy == StrategyTier1 {
		res.FleetStatus = st
		res.DurationMs = time.Since(start).Milliseconds()
		res.Message = fmt.Sprintf("completed Tier 1 ASE/ephemeral remediation on %s (%d actions)", cleanPath, len(res.Actions))
		return res, nil
	}

	// Tier 2 (StrategySafe): Verify no active execution holds the repository
	if lockInfo, lockErr := repolock.Read(cleanPath); lockErr == nil && lockInfo.PID > 0 && isPIDAlive(lockInfo.PID) {
		res.Skipped = true
		res.SkipReason = "active_repolock"
		res.FleetStatus = st
		res.DurationMs = time.Since(start).Milliseconds()
		res.Message = "skipped Tier 2 safe remediation: active repolock held"
		return res, nil
	}
	if st.ASEState == "RUNNING" || st.ExecutionProvenance == "COORDINATED_HARNESS" || st.LockDetails != "" || st.HasActiveGitLock {
		res.Skipped = true
		res.SkipReason = "active_execution_or_lock"
		res.FleetStatus = st
		res.DurationMs = time.Since(start).Milliseconds()
		res.Message = "skipped Tier 2 safe remediation: active execution or lock detected"
		return res, nil
	}
	if st.Branch != "main" && st.Branch != "master" {
		res.Skipped = true
		res.SkipReason = "non_main_branch"
		res.FleetStatus = st
		res.DurationMs = time.Since(start).Milliseconds()
		res.Message = fmt.Sprintf("skipped Tier 2 safe remediation: repository is on non-main branch %q", st.Branch)
		return res, nil
	}

	tier2Changed := false

	// 2a. Clean ephemeral cache files in root if all dirty/untracked files are ephemeral cache paths
	if len(st.ModifiedFiles) > 0 || len(st.UntrackedFiles) > 0 {
		allEphemeral := true
		for _, m := range st.ModifiedFiles {
			if !IsEphemeralCachePath(m) {
				allEphemeral = false
				break
			}
		}
		for _, u := range st.UntrackedFiles {
			if !IsEphemeralCachePath(u) {
				allEphemeral = false
				break
			}
		}
		if allEphemeral {
			for _, m := range st.ModifiedFiles {
				_ = procutil.CommandContext(ctx, "git", "-C", cleanPath, "checkout", "--", m).Run()
			}
			for _, u := range st.UntrackedFiles {
				_ = os.RemoveAll(filepath.Join(cleanPath, u))
			}
			res.Actions = append(res.Actions, "cleaned ephemeral cache files in root worktree")
			tier2Changed = true
		} else if len(st.UntrackedFiles) == 0 {
			// Check for phantom CRLF stat drift
			diffCmd := procutil.CommandContext(ctx, "git", "-C", cleanPath, "diff", "--ignore-cr-at-eol", "--quiet", "HEAD")
			if diffCmd.Run() == nil {
				_ = procutil.CommandContext(ctx, "git", "-C", cleanPath, "reset", "--hard", "HEAD").Run()
				res.Actions = append(res.Actions, "normalized phantom CRLF stat drift in root worktree")
				tier2Changed = true
			}
		}
	}

	// 2b. Clean submodules whose only modifications are ephemeral IDE/Gradle cache files or phantom CRLF drift
	for _, dirtySub := range st.DirtySubmodules {
		subRel := strings.TrimSpace(strings.Split(dirtySub, " ")[0])
		if subRel == "" {
			continue
		}
		subFull := filepath.Join(cleanPath, subRel)
		if cleanedSub := cleanSubmoduleEphemeralNoise(ctx, subFull); cleanedSub {
			res.Actions = append(res.Actions, fmt.Sprintf("cleaned ephemeral cache/stat noise in submodule %s", subRel))
			tier2Changed = true
		}
	}

	// 2c. Re-verify root cleanliness; if clean on main/master and behind origin, fast-forward pull and align submodules
	rootPorcelainCmd := procutil.CommandContext(ctx, "git", "-C", cleanPath, "status", "--porcelain")
	if pOut, pErr := rootPorcelainCmd.Output(); pErr == nil && strings.TrimSpace(string(pOut)) == "" {
		if st.Behind > 0 && st.Ahead == 0 {
			pullCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
			pullCmd := procutil.CommandContext(pullCtx, "git", "-C", cleanPath, "pull", "--ff-only", "--no-recurse-submodules", "origin", st.Branch)
			pullCmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
			if pullCmd.Run() == nil {
				res.Actions = append(res.Actions, fmt.Sprintf("fast-forwarded %s to origin/%s", st.Branch, st.Branch))
				tier2Changed = true
			}
			cancel()
		}
		if len(st.SubmoduleDrift) > 0 || tier2Changed {
			if _, statErr := os.Stat(filepath.Join(cleanPath, ".gitmodules")); statErr == nil {
				subCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
				subCmd := procutil.CommandContext(subCtx, "git", "-C", cleanPath, "submodule", "update", "--init")
				subCmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
				if subCmd.Run() == nil {
					res.Actions = append(res.Actions, "aligned submodule gitlinks to superproject pins")
					tier2Changed = true
				}
				cancel()
			}
		}
	}

	if tier2Changed {
		if refreshed, rErr := InspectRepoFleetStatusWithOptions(ctx, cleanPath, InspectOptions{
			AutoRemediateTier1: false,
			AutoRemediateSafe:  false,
		}); rErr == nil && refreshed != nil {
			refreshed.RemediationApplied = true
			refreshed.RemediationActions = append([]string{}, res.Actions...)
			st = refreshed
		}
	}

	res.FleetStatus = st
	res.DurationMs = time.Since(start).Milliseconds()
	res.Message = fmt.Sprintf("completed Tier 2 safe remediation on %s (%d actions)", cleanPath, len(res.Actions))
	return res, nil
}

func cleanSubmoduleEphemeralNoise(ctx context.Context, subFullPath string) bool {
	cmd := procutil.CommandContext(ctx, "git", "-C", subFullPath, "status", "--porcelain")
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	raw := strings.TrimRight(string(out), "\r\n")
	if strings.TrimSpace(raw) == "" {
		return false
	}
	lines := strings.Split(raw, "\n")
	var modified, untracked []string
	for _, line := range lines {
		if len(line) < 3 {
			continue
		}
		status := line[:2]
		file := strings.TrimSpace(line[3:])
		if !IsEphemeralCachePath(file) {
			// Check if phantom CRLF drift explains non-ephemeral tracked files
			diffCmd := procutil.CommandContext(ctx, "git", "-C", subFullPath, "diff", "--ignore-cr-at-eol", "--quiet", "HEAD", "--", file)
			if diffCmd.Run() != nil {
				return false
			}
		}
		if status == "??" {
			untracked = append(untracked, file)
		} else {
			modified = append(modified, file)
		}
	}
	for _, m := range modified {
		_ = procutil.CommandContext(ctx, "git", "-C", subFullPath, "checkout", "--", m).Run()
	}
	for _, u := range untracked {
		_ = os.RemoveAll(filepath.Join(subFullPath, u))
	}
	verifyCmd := procutil.CommandContext(ctx, "git", "-C", subFullPath, "status", "--porcelain")
	if vOut, vErr := verifyCmd.Output(); vErr == nil && strings.TrimSpace(string(vOut)) == "" {
		return true
	}
	return false
}

// JSON serializes RemediateResult as indented JSON.
func (r *RemediateResult) JSON() ([]byte, error) {
	return json.MarshalIndent(r, "", "  ")
}
