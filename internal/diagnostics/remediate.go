package diagnostics

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/viovy/iazio-harness/internal/gitclean"
	"github.com/viovy/iazio-harness/internal/procutil"
	"github.com/viovy/iazio-harness/internal/repolock"
)

const (
	// DefaultGitUpdateInterval is the default periodic repository update interval (30m).
	DefaultGitUpdateInterval = 30 * time.Minute
	// MinGitUpdateInterval is the minimum permitted periodic repository update interval (15m).
	MinGitUpdateInterval = 15 * time.Minute
	// MaxGitUpdateInterval is the maximum permitted periodic repository update interval (1h).
	MaxGitUpdateInterval = 1 * time.Hour
	// DefaultAgyRemediationCooldown is the circuit-breaker cooldown (60m) per worktree dirty state fingerprint.
	DefaultAgyRemediationCooldown = 60 * time.Minute
	// AgyGitUpdateRemediationPrompt is the canonical prompt passed to iazio-harness (agy) during Stage-2 escalation.
	AgyGitUpdateRemediationPrompt = `execute shell prompt "autopilot git-update"`
)

// ClampGitUpdateInterval clamps a periodic git-update interval into [15m, 1h] (defaulting <=0 to 30m).
func ClampGitUpdateInterval(d time.Duration) time.Duration {
	if d <= 0 {
		return DefaultGitUpdateInterval
	}
	if d < MinGitUpdateInterval {
		return MinGitUpdateInterval
	}
	if d > MaxGitUpdateInterval {
		return MaxGitUpdateInterval
	}
	return d
}

// ParseGitUpdateInterval parses a duration string and clamps it into [15m, 1h] (defaulting empty/invalid to 30m).
func ParseGitUpdateInterval(raw string) time.Duration {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return DefaultGitUpdateInterval
	}
	d, err := time.ParseDuration(trimmed)
	if err != nil || d <= 0 {
		return DefaultGitUpdateInterval
	}
	return ClampGitUpdateInterval(d)
}

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
	// StrategyGitUpdate executes post-ASE "autopilot git-update" with autoremediation when an ASE
	// flow has completed and the last job is completed/idle, stashing any residual uncommitted files
	// in root/submodules, fast-forwarding main, and aligning submodules so the queue can resume cleanly.
	StrategyGitUpdate RemediateStrategy = "git-update"
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
	case "git-update", "git_update", "autopilot-git-update", "post-ase-git-update":
		return StrategyGitUpdate
	case "force-clean", "force", "force_clean", "tier3", "discard", "hard":
		return StrategyForceClean
	default:
		return StrategySafe
	}
}

// IsEphemeralCachePath returns true for known background IDE/daemon cache, OS metadata, or
// idle session/closeout/coverage files (.session_story_id, TestResults/Diagnostics/pr-closeout-proof-*.json,
// cov_auth) that safely regenerate and should not block a repository on main.
func IsEphemeralCachePath(relPath string) bool {
	slash := filepath.ToSlash(strings.TrimSpace(relPath))
	slash = strings.TrimPrefix(slash, "./")
	if slash == "" {
		return false
	}
	base := filepath.Base(slash)
	if base == ".DS_Store" || base == "Thumbs.db" || base == ".session_story_id" || base == "cov_auth" || strings.HasPrefix(base, "cov_") {
		return true
	}
	if strings.HasPrefix(base, "pr-closeout-proof-") && strings.HasSuffix(base, ".json") {
		return true
	}
	if strings.Contains(slash, "/.gradle/") || strings.HasPrefix(slash, ".gradle/") ||
		strings.Contains(slash, "/build/tmp/") || strings.HasPrefix(slash, "build/tmp/") ||
		strings.Contains(slash, "/.dart_tool/") || strings.HasPrefix(slash, ".dart_tool/") {
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

	// Tier 2 / Post-ASE Git-Update: Verify no active execution holds the repository
	if lockInfo, lockErr := repolock.Read(cleanPath); lockErr == nil && lockInfo.PID > 0 && isPIDAlive(lockInfo.PID) {
		res.Skipped = true
		res.SkipReason = "active_repolock"
		res.FleetStatus = st
		res.DurationMs = time.Since(start).Milliseconds()
		res.Message = "skipped remediation: active repolock held"
		return res, nil
	}
	if st.ASEState == "RUNNING" || st.ASEState == "IN_PROGRESS" || st.ExecutionProvenance == "COORDINATED_HARNESS" || st.LockDetails != "" || st.HasActiveGitLock {
		res.Skipped = true
		res.SkipReason = "active_execution_or_lock"
		res.FleetStatus = st
		res.DurationMs = time.Since(start).Milliseconds()
		res.Message = "skipped remediation: active execution or lock detected"
		return res, nil
	}
	if strategy == StrategyGitUpdate {
		return executePostASEGitUpdateRemediation(ctx, start, cleanPath, st, res)
	}
	if st.Branch != "main" && st.Branch != "master" {
		res.Skipped = true
		res.SkipReason = "non_main_branch"
		res.FleetStatus = st
		res.DurationMs = time.Since(start).Milliseconds()
		res.Message = fmt.Sprintf("skipped remediation: repository is on non-main branch %q", st.Branch)
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
				if !isGitTrackedFile(cleanPath, ".session_story_id") {
					_ = os.Remove(filepath.Join(cleanPath, ".session_story_id"))
				}
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
			AutoRemediateTier1: true,
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

func executePostASEGitUpdateRemediation(ctx context.Context, start time.Time, cleanPath string, st *RepoFleetStatusResult, res *RemediateResult) (*RemediateResult, error) {
	_ = ResetASELocks(cleanPath)

	// 1. Clean or safely stash dirty/non-main submodules left behind by a completed ASE flow
	subSet := make(map[string]bool)
	for _, entry := range st.DirtySubmodules {
		subRel := strings.TrimSpace(strings.Split(entry, " ")[0])
		if subRel != "" {
			subSet[subRel] = true
		}
	}
	for _, entry := range st.NonMainSubmodules {
		subRel := strings.TrimSpace(strings.Split(entry, " ")[0])
		if subRel != "" {
			subSet[subRel] = true
		}
	}
	for subRel := range subSet {
		subFull := filepath.Join(cleanPath, subRel)
		if cleanSubmoduleEphemeralNoise(ctx, subFull) {
			res.Actions = append(res.Actions, fmt.Sprintf("cleaned ephemeral cache noise in submodule %s", subRel))
			continue
		}
		if out, err := procutil.CommandContext(ctx, "git", "-C", subFull, "status", "--porcelain").Output(); err == nil && strings.TrimSpace(string(out)) != "" {
			stashCmd := procutil.CommandContext(ctx, "git", "-C", subFull, "stash", "push", "-u", "-m", "iazio-harness: auto-remediate completed ASE dirty submodule")
			if stashCmd.Run() == nil {
				res.Actions = append(res.Actions, fmt.Sprintf("stashed uncommitted post-ASE changes in submodule %s", subRel))
			} else {
				_ = procutil.CommandContext(ctx, "git", "-C", subFull, "checkout", "--", ".").Run()
				_ = procutil.CommandContext(ctx, "git", "-C", subFull, "clean", "-fd").Run()
				res.Actions = append(res.Actions, fmt.Sprintf("discarded residual post-ASE changes in submodule %s", subRel))
			}
		}
	}

	// 2. Clean or safely stash residual dirty files in root worktree
	for _, m := range st.ModifiedFiles {
		if IsEphemeralCachePath(m) {
			_ = procutil.CommandContext(ctx, "git", "-C", cleanPath, "checkout", "--", m).Run()
		}
	}
	for _, u := range st.UntrackedFiles {
		if IsEphemeralCachePath(u) {
			_ = os.RemoveAll(filepath.Join(cleanPath, u))
		}
	}
	if out, err := procutil.CommandContext(ctx, "git", "-C", cleanPath, "status", "--porcelain").Output(); err == nil && strings.TrimSpace(string(out)) != "" {
		stashCmd := procutil.CommandContext(ctx, "git", "-C", cleanPath, "stash", "push", "-u", "-m", "iazio-harness: auto-remediate completed ASE dirty root")
		if stashCmd.Run() == nil {
			res.Actions = append(res.Actions, "stashed uncommitted post-ASE changes in root worktree")
		} else {
			_ = procutil.CommandContext(ctx, "git", "-C", cleanPath, "checkout", "--", ".").Run()
			_ = procutil.CommandContext(ctx, "git", "-C", cleanPath, "clean", "-fd").Run()
			res.Actions = append(res.Actions, "cleaned residual post-ASE changes in root worktree")
		}
	}

	// 3. Execute "autopilot git-update" with autoremediation (plus direct git pull + submodule alignment)
	if autopilotBin := resolveAutopilotBinary(); autopilotBin != "" {
		upCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		upCmd := procutil.CommandContext(upCtx, autopilotBin, "git-update", "--repo-root", cleanPath, "--dry-run", "--json")
		upCmd.Dir = cleanPath
		upCmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		if upCmd.Run() == nil {
			res.Actions = append(res.Actions, "executed autopilot git-update with autoremediation")
		}
		cancel()
	}

	// Ensure root is on main/master, fast-forwarded/aligned to origin, and submodules are aligned to superproject pins
	targetBranch := st.Branch
	if targetBranch != "main" && targetBranch != "master" {
		targetBranch = "main"
	}
	_ = procutil.CommandContext(ctx, "git", "-C", cleanPath, "checkout", "-f", targetBranch).Run()

	pullCtx, cancelPull := context.WithTimeout(ctx, 25*time.Second)
	pullCmd := procutil.CommandContext(pullCtx, "git", "-C", cleanPath, "pull", "--ff-only", "--no-recurse-submodules", "origin", targetBranch)
	pullCmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if pullCmd.Run() == nil {
		res.Actions = append(res.Actions, fmt.Sprintf("fast-forwarded %s to origin/%s", targetBranch, targetBranch))
	} else if procutil.CommandContext(ctx, "git", "-C", cleanPath, "rev-parse", "--verify", "origin/"+targetBranch).Run() == nil {
		if procutil.CommandContext(ctx, "git", "-C", cleanPath, "reset", "--hard", "origin/"+targetBranch).Run() == nil {
			res.Actions = append(res.Actions, fmt.Sprintf("aligned %s to origin/%s", targetBranch, targetBranch))
		}
	}
	cancelPull()

	if !isGitTrackedFile(cleanPath, ".session_story_id") {
		_ = os.Remove(filepath.Join(cleanPath, ".session_story_id"))
	}
	if pOut, pErr := procutil.CommandContext(ctx, "git", "-C", cleanPath, "status", "--porcelain", "--", "tmp", ".autopilot").Output(); pErr == nil && strings.TrimSpace(string(pOut)) != "" {
		for _, line := range strings.Split(strings.TrimSpace(string(pOut)), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "?? ") {
				rel := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "?? "))
				_ = os.RemoveAll(filepath.Join(cleanPath, rel))
			}
		}
	}

	if _, statErr := os.Stat(filepath.Join(cleanPath, ".gitmodules")); statErr == nil {
		subCtx, cancelSub := context.WithTimeout(ctx, 30*time.Second)
		subCmd := procutil.CommandContext(subCtx, "git", "-C", cleanPath, "submodule", "update", "--init", "--recursive", "--force")
		subCmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		if subCmd.Run() == nil {
			res.Actions = append(res.Actions, "aligned submodule gitlinks to superproject pins")
		}
		cancelSub()

		// Reattach any detached submodule HEADs to main so SubmodulesClean remains true
		if subOut, sErr := procutil.CommandContext(ctx, "git", "-C", cleanPath, "submodule--helper", "list").Output(); sErr == nil {
			for _, line := range strings.Split(strings.TrimSpace(string(subOut)), "\n") {
				fields := strings.Fields(line)
				if len(fields) >= 4 {
					subRel := fields[len(fields)-1]
					subFull := filepath.Join(cleanPath, subRel)
					if br, ok := readGitWorktreeBranch(subFull); ok && br == "detached" {
						_ = procutil.CommandContext(ctx, "git", "-C", subFull, "checkout", "-B", "main", "HEAD").Run()
					}
				}
			}
		}
	}

	if refreshed, rErr := InspectRepoFleetStatusWithOptions(ctx, cleanPath, InspectOptions{
		AutoRemediateTier1: true,
		AutoRemediateSafe:  false,
	}); rErr == nil && refreshed != nil {
		refreshed.RemediationApplied = true
		refreshed.RemediationActions = append([]string{}, res.Actions...)
		if refreshed.IsClean && refreshed.SubmodulesClean {
			refreshed.ASEState = "IDLE"
			refreshed.ExecutionProvenance = "NONE"
			refreshed.ActiveStoryID = ""
			refreshed.CurrentPhase = ""
		}
		st = refreshed
	}

	res.FleetStatus = st
	res.DurationMs = time.Since(start).Milliseconds()
	res.Message = fmt.Sprintf("completed post-ASE autopilot git-update remediation on %s (%d actions)", cleanPath, len(res.Actions))
	return res, nil
}

func resolveAutopilotBinary() string {
	if p, err := exec.LookPath("autopilot"); err == nil && p != "" {
		return p
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		cand := filepath.Join(home, ".local", "bin", "autopilot")
		if st, err := os.Stat(cand); err == nil && !st.IsDir() {
			return cand
		}
	}
	for _, cand := range []string{"/opt/homebrew/bin/autopilot", "/usr/local/bin/autopilot"} {
		if st, err := os.Stat(cand); err == nil && !st.IsDir() {
			return cand
		}
	}
	return ""
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

// IsWorktreeIdleForGitUpdate returns true when repolock is idle and there is no active IN_PROGRESS ASE story
// in .autopilot/ase-state.json, git HEAD story branch, or .session_story_id (or the referenced story is already DONE).
func IsWorktreeIdleForGitUpdate(repoPath string, pidAliveFn func(int) bool) bool {
	cleanPath := filepath.Clean(strings.TrimSpace(repoPath))
	if cleanPath == "" {
		return false
	}
	if pidAliveFn == nil {
		pidAliveFn = isPIDAlive
	}
	if lockInfo, err := repolock.Read(cleanPath); err == nil {
		lockStatus := strings.ToUpper(strings.TrimSpace(lockInfo.Status))
		if lockStatus != "COMPLETED" && lockStatus != "FAILED" && lockStatus != "IDLE" {
			if (lockInfo.PID > 0 && pidAliveFn(lockInfo.PID)) ||
				(lockInfo.HarnessPID > 0 && pidAliveFn(lockInfo.HarnessPID)) ||
				(lockInfo.ToolPID > 0 && pidAliveFn(lockInfo.ToolPID)) {
				return false
			}
		}
	}
	if _, err := os.Stat(filepath.Join(cleanPath, ".autopilot", "lock")); err == nil {
		return false
	}
	if _, err := os.Stat(filepath.Join(cleanPath, ".ase", "active.lock")); err == nil {
		return false
	}

	// 1. Check in-flight branch directly from git HEAD or submodules:
	// Any non-main, non-maintenance feature/fix/chore branch is considered in-flight unless its inferred story is completely DONE.
	if brOut, brErr := procutil.Command("git", "-C", cleanPath, "symbolic-ref", "--short", "-q", "HEAD").Output(); brErr == nil {
		br := strings.TrimSpace(string(brOut))
		if br != "" && br != "main" && br != "master" && br != "HEAD" && !isAutomatedMaintenanceBranch(br) {
			inferred := InferStoryIDFromBranch(br)
			if inferred == "" || !IsStoryCompletedOnDisk(cleanPath, inferred) {
				return false
			}
		}
	}
	if hasAnyNonMainSubmodule(cleanPath) || hasActiveNonMainSubmoduleStory(cleanPath) {
		return false
	}

	// 2. Check active ASE state from .autopilot/ase-state.json
	aseStatePath := filepath.Join(cleanPath, ".autopilot", "ase-state.json")
	if b, err := os.ReadFile(aseStatePath); err == nil {
		var as struct {
			StoryID string `json:"story_id"`
			Status  string `json:"status"`
			Phase   string `json:"phase"`
		}
		if json.Unmarshal(b, &as) == nil {
			status := strings.ToUpper(strings.TrimSpace(as.Status))
			if status == "IN_PROGRESS" || status == "RUNNING" {
				if as.StoryID == "" || !IsStoryCompletedOnDisk(cleanPath, as.StoryID) {
					return false
				}
			}
		}
	}

	// 3. Check active state from .autopilot/state.json
	statePath := filepath.Join(cleanPath, ".autopilot", "state.json")
	if b, err := os.ReadFile(statePath); err == nil {
		var s struct {
			State   string `json:"state"`
			StoryID string `json:"story_id"`
		}
		if json.Unmarshal(b, &s) == nil {
			st := strings.ToUpper(strings.TrimSpace(s.State))
			if st == "IN_PROGRESS" || st == "RUNNING" {
				if s.StoryID == "" || !IsStoryCompletedOnDisk(cleanPath, s.StoryID) {
					return false
				}
			}
		}
	}

	// 4. Check active session story from .session_story_id
	sessionPath := filepath.Join(cleanPath, ".session_story_id")
	if b, err := os.ReadFile(sessionPath); err == nil {
		storyID := strings.TrimSpace(string(b))
		if storyID != "" && !IsStoryCompletedOnDisk(cleanPath, storyID) {
			if IsStoryExplicitlyInProgressOnDisk(cleanPath, storyID) || hasUnresolvedStorySIROnDisk(cleanPath, storyID) {
				return false
			}
			if _, rErr := os.Stat(filepath.Join(cleanPath, "tmp", "ase-resume-"+storyID+".json")); rErr == nil {
				return false
			}
			if brOut, brErr := procutil.Command("git", "-C", cleanPath, "symbolic-ref", "--short", "-q", "HEAD").Output(); brErr == nil {
				br := strings.TrimSpace(string(brOut))
				if br != "" && br != "main" && br != "master" && br != "HEAD" {
					return false
				}
			}
			if hasAnyNonMainSubmodule(cleanPath) {
				return false
			}
		}
	}

	// 5. Check live agent processes bound to this worktree
	var probeRes RepoFleetStatusResult
	discoverAgentProcesses(cleanPath, &probeRes)
	if probeRes.ToolLiveness == "LIVE" &&
		(probeRes.HasActiveAgentASETurn || probeRes.ActiveStoryID != "" || strings.HasPrefix(strings.ToLower(probeRes.AgentTool), "autopilot")) {
		return false
	}

	return true
}

func discoverGitmodulePaths(repoPath string) []string {
	b, err := os.ReadFile(filepath.Join(repoPath, ".gitmodules"))
	if err != nil {
		return nil
	}
	var subs []string
	for _, line := range strings.Split(string(b), "\n") {
		l := strings.TrimSpace(line)
		if strings.HasPrefix(l, "path") && strings.Contains(l, "=") {
			parts := strings.SplitN(l, "=", 2)
			if len(parts) == 2 {
				sub := strings.TrimSpace(parts[1])
				if sub != "" {
					subs = append(subs, sub)
				}
			}
		}
	}
	return subs
}

func hasAnyNonMainSubmodule(repoPath string) bool {
	for _, sub := range discoverGitmodulePaths(repoPath) {
		subDir := filepath.Join(repoPath, sub)
		if br, ok := readGitWorktreeBranch(subDir); ok && br != "main" && br != "master" && br != "detached" {
			return true
		}
	}
	return false
}

func hasActiveNonMainSubmoduleStory(repoPath string) bool {
	for _, sub := range discoverGitmodulePaths(repoPath) {
		subDir := filepath.Join(repoPath, sub)
		if br, ok := readGitWorktreeBranch(subDir); ok && br != "main" && br != "master" && br != "detached" {
			if inferred := InferStoryIDFromBranch(br); inferred != "" && !IsStoryCompletedOnDisk(repoPath, inferred) {
				return true
			}
		}
	}
	return false
}

func healStrandedMaintenanceBranchBeforeGitUpdate(ctx context.Context, cleanPath string) {
	brOut, brErr := procutil.CommandContext(ctx, "git", "-C", cleanPath, "symbolic-ref", "--short", "-q", "HEAD").Output()
	if brErr != nil {
		return
	}
	br := strings.TrimSpace(string(brOut))
	if !isAutomatedMaintenanceBranch(br) {
		return
	}
	_ = procutil.CommandContext(ctx, "git", "-C", cleanPath, "checkout", "main").Run()
	fetchCmd := procutil.CommandContext(ctx, "git", "-C", cleanPath, "fetch", "origin", "main")
	fetchCmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if fetchCmd.Run() == nil {
		_ = procutil.CommandContext(ctx, "git", "-C", cleanPath, "reset", "--hard", "origin/main").Run()
	}
	_ = procutil.CommandContext(ctx, "git", "-C", cleanPath, "branch", "-D", br).Run()
	if sirs, gErr := filepath.Glob(filepath.Join(cleanPath, "tmp", "reports", "intervention-git-update-sir-*.json")); gErr == nil {
		for _, sir := range sirs {
			_ = os.Remove(sir)
		}
	}
}

func cleanOrRestoreArtifactInDir(ctx context.Context, dir, relPath string) bool {
	relSlash := filepath.ToSlash(strings.TrimSpace(relPath))
	if relSlash == "" {
		return false
	}
	fullPath := filepath.Join(dir, filepath.FromSlash(relSlash))
	if isGitTrackedFile(dir, relSlash) {
		out, err := procutil.CommandContext(ctx, "git", "-C", dir, "status", "--porcelain", "--", relSlash).Output()
		if err == nil && strings.TrimSpace(string(out)) != "" {
			if procutil.CommandContext(ctx, "git", "-C", dir, "checkout", "HEAD", "--", relSlash).Run() == nil ||
				procutil.CommandContext(ctx, "git", "-C", dir, "checkout", "--", relSlash).Run() == nil {
				return true
			}
		}
		return false
	}
	if _, err := os.Stat(fullPath); err == nil {
		if os.RemoveAll(fullPath) == nil {
			return true
		}
	}
	return false
}

// SanitizeIdleEphemeralArtifacts cleans or restores ephemeral artifacts (.session_story_id,
// TestResults/Diagnostics/pr-closeout-proof-*.json, cov_auth) when repolock is idle and no IN_PROGRESS ASE story is active.
func SanitizeIdleEphemeralArtifacts(ctx context.Context, repoPath string, pidAliveFn func(int) bool) ([]string, bool) {
	if !IsWorktreeIdleForGitUpdate(repoPath, pidAliveFn) {
		return nil, false
	}
	cleanPath := filepath.Clean(strings.TrimSpace(repoPath))
	var cleaned []string
	seen := make(map[string]bool)
	record := func(rel string) {
		if !seen[rel] {
			seen[rel] = true
			cleaned = append(cleaned, rel)
		}
	}

	for _, rel := range []string{".session_story_id", "cov_auth"} {
		if cleanOrRestoreArtifactInDir(ctx, cleanPath, rel) {
			record(rel)
		}
	}
	if matches, err := filepath.Glob(filepath.Join(cleanPath, "TestResults", "Diagnostics", "pr-closeout-proof-*.json")); err == nil {
		for _, m := range matches {
			if rel, rErr := filepath.Rel(cleanPath, m); rErr == nil {
				relSlash := filepath.ToSlash(rel)
				if cleanOrRestoreArtifactInDir(ctx, cleanPath, relSlash) {
					record(relSlash)
				}
			}
		}
	}
	if matches, err := filepath.Glob(filepath.Join(cleanPath, "cov_*")); err == nil {
		for _, m := range matches {
			if rel, rErr := filepath.Rel(cleanPath, m); rErr == nil {
				relSlash := filepath.ToSlash(rel)
				if cleanOrRestoreArtifactInDir(ctx, cleanPath, relSlash) {
					record(relSlash)
				}
			}
		}
	}
	if out, err := procutil.CommandContext(ctx, "git", "-C", cleanPath, "status", "--porcelain").Output(); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			if len(line) < 4 {
				continue
			}
			rel := strings.TrimSpace(line[3:])
			if IsEphemeralCachePath(rel) {
				if cleanOrRestoreArtifactInDir(ctx, cleanPath, rel) {
					record(filepath.ToSlash(rel))
				}
			}
		}
	}
	return cleaned, true
}

// ComputeWorktreeDirtyFingerprint computes a deterministic fingerprint combining git_head_sha,
// git_tree_sha, and the current dirty status porcelain for Stage-2 agy cooldown tracking.
func ComputeWorktreeDirtyFingerprint(ctx context.Context, repoPath string) (headSHA, treeSHA, fingerprint string) {
	if out, err := procutil.CommandContext(ctx, "git", "-C", repoPath, "rev-parse", "HEAD").Output(); err == nil {
		headSHA = strings.TrimSpace(string(out))
	}
	if out, err := procutil.CommandContext(ctx, "git", "-C", repoPath, "rev-parse", "HEAD^{tree}").Output(); err == nil {
		treeSHA = strings.TrimSpace(string(out))
	}
	porcelain := ""
	if out, err := procutil.CommandContext(ctx, "git", "-C", repoPath, "status", "--porcelain=v2", "--branch").Output(); err == nil {
		porcelain = strings.TrimSpace(string(out))
	}
	sum := sha256.Sum256([]byte(headSHA + ":" + treeSHA + ":" + porcelain))
	fingerprint = fmt.Sprintf("%s:%s:%x", headSHA, treeSHA, sum[:8])
	return headSHA, treeSHA, fingerprint
}

// TwoStageGitUpdateResult captures the outcome of a 2-stage worktree git-update and remediation pass in iazio-harness.
type TwoStageGitUpdateResult struct {
	RepoPath              string   `json:"repo_path"`
	Skipped               bool     `json:"skipped,omitempty"`
	SkipReason            string   `json:"skip_reason,omitempty"`
	SanitizedArtifacts    []string `json:"sanitized_artifacts,omitempty"`
	Stage1Executed        bool     `json:"stage1_executed"`
	Stage1Success         bool     `json:"stage1_success"`
	Stage1Output          string   `json:"stage1_output,omitempty"`
	Stage2Escalated       bool     `json:"stage2_escalated"`
	Stage2CooldownSkipped bool     `json:"stage2_cooldown_skipped,omitempty"`
	Stage2Success         bool     `json:"stage2_success"`
	Stage2Output          string   `json:"stage2_output,omitempty"`
	DirtyFingerprint      string   `json:"dirty_fingerprint,omitempty"`
	QueueResumed          bool     `json:"queue_resumed"`
	IsClean               bool     `json:"is_clean"`
}

type agyCooldownState struct {
	Fingerprint string
	HeadSHA     string
	TreeSHA     string
	AttemptedAt time.Time
}

// TwoStageGitUpdateRemediator coordinates Step 0 (ephemeral artifact cleanup), Step 1 (shell autopilot git-update),
// and Step 2 (agy escalation with 60m fingerprint cooldown and queue auto-resume).
type TwoStageGitUpdateRemediator struct {
	mu          sync.Mutex
	Interval    time.Duration
	CooldownDur time.Duration
	ShellUpdate func(ctx context.Context, repoPath string) (string, error)
	AgyEscalate func(ctx context.Context, repoPath, prompt, diagnosticOutput string) (string, error)
	ResumeQueue func(ctx context.Context, repoPath string) error
	IsPIDAlive  func(int) bool
	Now         func() time.Time
	cooldowns   map[string]agyCooldownState
}

// NewTwoStageGitUpdateRemediator creates a 2-stage git-update remediator with clamped interval [15m, 1h] and 60m cooldown.
func NewTwoStageGitUpdateRemediator(interval time.Duration) *TwoStageGitUpdateRemediator {
	return &TwoStageGitUpdateRemediator{
		Interval:    ClampGitUpdateInterval(interval),
		CooldownDur: DefaultAgyRemediationCooldown,
		IsPIDAlive:  isPIDAlive,
		Now:         time.Now,
		cooldowns:   make(map[string]agyCooldownState),
	}
}

func isWorktreeCleanOnMain(ctx context.Context, repoPath string) bool {
	out, err := procutil.CommandContext(ctx, "git", "-C", repoPath, "status", "--porcelain=v2", "--branch").Output()
	if err != nil {
		return false
	}
	branch := ""
	for _, line := range strings.Split(string(out), "\n") {
		l := strings.TrimSpace(line)
		if l == "" {
			continue
		}
		if strings.HasPrefix(l, "# branch.head ") {
			branch = strings.TrimPrefix(l, "# branch.head ")
		} else if strings.HasPrefix(l, "# branch.ab ") {
			parts := strings.Fields(strings.TrimPrefix(l, "# branch.ab "))
			if len(parts) >= 2 && (parts[0] != "+0" || parts[1] != "-0") {
				return false
			}
		} else if !strings.HasPrefix(l, "# ") {
			if strings.HasPrefix(l, "? ") {
				p := strings.TrimPrefix(filepath.ToSlash(strings.TrimSpace(l[2:])), "./")
				if p == ".iazio" || strings.HasPrefix(p, ".iazio/") || p == ".autopilot" || strings.HasPrefix(p, ".autopilot/") {
					continue
				}
			}
			return false
		}
	}
	return branch == "main" || branch == "master"
}

// Execute runs the 2-stage worktree git-update and remediation flow on repoPath.
func (r *TwoStageGitUpdateRemediator) Execute(ctx context.Context, repoPath string) TwoStageGitUpdateResult {
	cleanPath := filepath.Clean(strings.TrimSpace(repoPath))
	res := TwoStageGitUpdateResult{RepoPath: cleanPath}

	pidAlive := r.IsPIDAlive
	if pidAlive == nil {
		pidAlive = isPIDAlive
	}
	nowFn := r.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	cooldown := r.CooldownDur
	if cooldown <= 0 {
		cooldown = DefaultAgyRemediationCooldown
	}

	if !IsWorktreeIdleForGitUpdate(cleanPath, pidAlive) {
		res.Skipped = true
		res.SkipReason = "active_execution_or_in_progress_story"
		return res
	}

	sanitized, _ := SanitizeIdleEphemeralArtifacts(ctx, cleanPath, pidAlive)
	res.SanitizedArtifacts = sanitized

	lockHandle, lockErr := repolock.Acquire(cleanPath, repolock.LockInfo{
		JobID:  "autopilot-git-update",
		PID:    os.Getpid(),
		Status: "RUNNING",
	}, pidAlive)
	if lockErr != nil {
		res.Skipped = true
		res.SkipReason = "repolock_busy"
		return res
	}
	releaseLock := func() {
		if lockHandle != nil {
			_ = lockHandle.Release()
			_ = repolock.ForceRelease(cleanPath)
			lockHandle = nil
		}
	}
	defer releaseLock()

	healStrandedMaintenanceBranchBeforeGitUpdate(ctx, cleanPath)

	res.Stage1Executed = true
	var step1Out string
	var step1Err error
	if r.ShellUpdate != nil {
		step1Out, step1Err = r.ShellUpdate(ctx, cleanPath)
	} else {
		autopilotBin := resolveAutopilotBinary()
		if autopilotBin == "" {
			autopilotBin = "autopilot"
		}
		cmd := procutil.CommandContext(ctx, autopilotBin, "git-update")
		cmd.Dir = cleanPath
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		b, err := cmd.CombinedOutput()
		step1Out, step1Err = strings.TrimSpace(string(b)), err
	}
	res.Stage1Output = step1Out

	releaseLock()
	_, _ = SanitizeIdleEphemeralArtifacts(ctx, cleanPath, pidAlive)
	if step1Err == nil && isWorktreeCleanOnMain(ctx, cleanPath) {
		res.Stage1Success = true
		res.IsClean = true
		r.mu.Lock()
		if r.cooldowns != nil {
			delete(r.cooldowns, cleanPath)
		}
		r.mu.Unlock()
		if r.ResumeQueue != nil {
			if r.ResumeQueue(ctx, cleanPath) == nil {
				res.QueueResumed = true
			}
		}
		return res
	}

	if !IsWorktreeIdleForGitUpdate(cleanPath, pidAlive) {
		return res
	}

	headSHA, treeSHA, fp := ComputeWorktreeDirtyFingerprint(ctx, cleanPath)
	res.DirtyFingerprint = fp
	now := nowFn().UTC()

	r.mu.Lock()
	if r.cooldowns == nil {
		r.cooldowns = make(map[string]agyCooldownState)
	}
	if prev, ok := r.cooldowns[cleanPath]; ok && prev.Fingerprint == fp && now.Sub(prev.AttemptedAt) < cooldown {
		r.mu.Unlock()
		res.Stage2CooldownSkipped = true
		return res
	}
	r.cooldowns[cleanPath] = agyCooldownState{
		Fingerprint: fp,
		HeadSHA:     headSHA,
		TreeSHA:     treeSHA,
		AttemptedAt: now,
	}
	r.mu.Unlock()

	diagOut := step1Out
	if diagOut == "" && step1Err != nil {
		diagOut = step1Err.Error()
	}

	res.Stage2Escalated = true
	if r.AgyEscalate != nil {
		out2, err2 := r.AgyEscalate(ctx, cleanPath, AgyGitUpdateRemediationPrompt, diagOut)
		res.Stage2Output = out2
		_, _ = SanitizeIdleEphemeralArtifacts(ctx, cleanPath, pidAlive)
		if err2 == nil && isWorktreeCleanOnMain(ctx, cleanPath) {
			res.Stage2Success = true
			res.IsClean = true
			r.mu.Lock()
			delete(r.cooldowns, cleanPath)
			r.mu.Unlock()
			if r.ResumeQueue != nil {
				if r.ResumeQueue(ctx, cleanPath) == nil {
					res.QueueResumed = true
				}
			}
		}
	}

	return res
}
