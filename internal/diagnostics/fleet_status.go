// Package diagnostics provides deep inspection of fleet git status, ASE state, and agent provenance.
package diagnostics

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// RepoFleetStatusResult contains the complete diagnostic posture of a repository and active agent tooling.
type RepoFleetStatusResult struct {
	RepoPath             string   `json:"repo_path"`
	Branch               string   `json:"branch"`
	HeadSHA              string   `json:"head_sha"`
	IsClean              bool     `json:"is_clean"`
	Ahead                int      `json:"ahead"`
	Behind               int      `json:"behind"`
	ModifiedFiles        []string `json:"modified_files"`
	UntrackedFiles       []string `json:"untracked_files"`
	SubmodulesClean      bool     `json:"submodules_clean"`
	DirtySubmodules      []string `json:"dirty_submodules"`
	WorktreeState        string   `json:"worktree_state"` // CLEAN, CLEAN_SUBMODULE_DRIFT, DIRTY_UNTRACKED_ONLY, DIRTY_MODIFIED
	SubmoduleDrift       []string `json:"submodule_drift,omitempty"`
	OrphanedProcesses    []string `json:"orphaned_processes,omitempty"`
	ASEState             string   `json:"ase_state"` // IDLE, RUNNING, SUSPENDED_HITL, FAILED_DIRTY, ORPHANED
	ActiveStoryID        string   `json:"active_story_id,omitempty"`
	CurrentPhase         string   `json:"current_phase,omitempty"`
	LockDetails          string   `json:"lock_details,omitempty"`
	AgentTool            string   `json:"agent_tool,omitempty"`
	AgentPID             int      `json:"agent_pid,omitempty"`
	ToolLiveness         string   `json:"tool_liveness"` // LIVE, TERMINATED, KILLED, NONE
	ExecutionProvenance  string   `json:"execution_provenance"` // INDEPENDENT_USER_SESSION, COORDINATED_HARNESS, COORDINATED_HARNESS_FAILED, NONE
	ActiveConversationID string   `json:"active_conversation_id,omitempty"`
	DiagnosticSummary    string   `json:"diagnostic_summary"`
	CheckedAt            string   `json:"checked_at"`
}

// InspectRepoFleetStatus performs deep forensic inspection of a git repo worktree,
// submodules, ASE lifecycle state, and running agent processes.
func InspectRepoFleetStatus(ctx context.Context, repoPath string) (*RepoFleetStatusResult, error) {
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

	// 1. Git Status Porcelain v2 & Branch
	cmd := exec.CommandContext(ctx, "git", "-C", cleanPath, "status", "--porcelain=v2", "--branch")
	out, err := cmd.CombinedOutput()
	if err == nil {
		parseGitPorcelainV2(string(out), res)
	} else {
		res.IsClean = false
		res.DiagnosticSummary += fmt.Sprintf("git status failed: %v\n", err)
	}

	// 1b. Check if branch is ahead of origin/main when upstream is unconfigured
	if res.Ahead == 0 && res.Branch != "" && res.Branch != "main" && res.Branch != "master" {
		revCmd := exec.CommandContext(ctx, "git", "-C", cleanPath, "rev-list", "--count", "origin/main..HEAD")
		if revOut, err := revCmd.Output(); err == nil {
			if count, err := strconv.Atoi(strings.TrimSpace(string(revOut))); err == nil && count > 0 {
				res.Ahead = count
			}
		}
	}

	// 2. Git Submodules
	subCmd := exec.CommandContext(ctx, "git", "-C", cleanPath, "submodule", "status", "--recursive")
	if subOut, err := subCmd.CombinedOutput(); err == nil {
		parseSubmoduleStatus(ctx, cleanPath, string(subOut), res)
	}

	// Classify overall cleanliness and worktree state
	hasModifiedFiles := len(res.ModifiedFiles) > 0
	hasUntrackedFiles := len(res.UntrackedFiles) > 0
	hasDirtySubmodules := len(res.DirtySubmodules) > 0
	hasSubmoduleDrift := len(res.SubmoduleDrift) > 0

	if hasModifiedFiles || hasDirtySubmodules {
		res.WorktreeState = "DIRTY_MODIFIED"
		res.IsClean = false
	} else if hasUntrackedFiles {
		res.WorktreeState = "DIRTY_UNTRACKED_ONLY"
		res.IsClean = false
	} else if hasSubmoduleDrift {
		res.WorktreeState = "CLEAN_SUBMODULE_DRIFT"
		res.IsClean = true
	} else {
		res.WorktreeState = "CLEAN"
		res.IsClean = true
	}
	res.SubmodulesClean = (len(res.DirtySubmodules) == 0)

	// 3. ASE State & Autopilot locks
	parseASEState(cleanPath, res)

	// 4. Process Discovery & Provenance
	discoverAgentProcesses(cleanPath, res)

	// 5. Finalize Diagnostic Summary
	buildDiagnosticSummary(res)

	return res, nil
}

func parseGitPorcelainV2(out string, res *RepoFleetStatusResult) {
	lines := strings.Split(out, "\n")
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" {
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
		if l == "" {
			continue
		}
		parts := strings.Fields(l)
		if len(parts) >= 2 {
			subPath := parts[1]
			fullSubPath := filepath.Join(cleanPath, subPath)
			hasUncommitted := false

			// Deep check: inspect the submodule worktree itself for uncommitted files
			if stat, err := os.Stat(fullSubPath); err == nil && stat.IsDir() {
				subStatusCmd := exec.CommandContext(ctx, "git", "-C", fullSubPath, "status", "--porcelain")
				if subOut, err := subStatusCmd.Output(); err == nil && len(strings.TrimSpace(string(subOut))) > 0 {
					hasUncommitted = true
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
	stateFile := filepath.Join(repoPath, ".autopilot", "state.json")
	if b, err := os.ReadFile(stateFile); err == nil {
		var s struct {
			State        string `json:"state"`
			StoryID      string `json:"story_id"`
			CurrentPhase string `json:"current_phase"`
			UpdatedAt    string `json:"updated_at"`
		}
		if json.Unmarshal(b, &s) == nil {
			if s.State != "" {
				res.ASEState = s.State
			}
			res.ActiveStoryID = s.StoryID
			res.CurrentPhase = s.CurrentPhase
		}
	}

	// Check locks
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

	if len(locks) > 0 {
		res.LockDetails = strings.Join(locks, ", ")
		if res.ASEState == "IDLE" {
			res.ASEState = "RUNNING"
		}
	} else if res.WorktreeState == "DIRTY_MODIFIED" && res.ASEState == "IDLE" {
		res.ASEState = "FAILED_DIRTY"
	}
}

var knownAgentBinaries = []string{
	"agy", "antigravity", "opencode", "cursor", "claude", "autopilot", "iazio-harness",
}

var auxiliaryChildBinaries = []string{
	"iazio-mcp", "node", "gopls", "rust-analyzer", "tsserver", "pyright", "python",
}

func isAgentTool(cmd string) (string, bool) {
	lower := strings.ToLower(filepath.Base(cmd))
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
	_, err := os.Stat(fmt.Sprintf("/proc/%d", pid))
	return err == nil
}

func discoverAgentProcesses(repoPath string, res *RepoFleetStatusResult) {
	if runtime.GOOS != "linux" {
		// Non-linux fallback using ps if available
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

	for _, name := range entries {
		pid, err := strconv.Atoi(name)
		if err != nil || pid <= 1 {
			continue
		}

		// Read cwd symlink
		cwdPath := filepath.Join("/proc", name, "cwd")
		targetCwd, err := os.Readlink(cwdPath)
		if err != nil {
			continue
		}

		cleanTarget := filepath.Clean(targetCwd)
		if !strings.HasPrefix(cleanTarget, repoPath) {
			continue
		}

		// Read cmdline
		cmdlineBytes, _ := os.ReadFile(filepath.Join("/proc", name, "cmdline"))
		cmdArgs := strings.Split(string(cmdlineBytes), "\x00")
		if len(cmdArgs) == 0 || cmdArgs[0] == "" {
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
			} else if strings.Contains(environStr, "SSH_CLIENT") ||
				strings.Contains(environStr, "SSH_TTY") ||
				strings.Contains(environStr, "TMUX") ||
				strings.Contains(environStr, "TERM_PROGRAM") {
				res.ExecutionProvenance = "INDEPENDENT_USER_SESSION"
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
		} else if !res.IsClean {
			res.ToolLiveness = "TERMINATED"
		}
	}
}

func discoverAgentProcessesFallback(repoPath string, res *RepoFleetStatusResult) {
	cmd := exec.Command("ps", "-ef")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return
	}
	lines := strings.Split(string(out), "\n")
	for _, l := range lines {
		if !strings.Contains(l, repoPath) {
			continue
		}
		for _, tool := range knownAgentBinaries {
			if strings.Contains(l, tool) {
				fields := strings.Fields(l)
				if len(fields) > 1 {
					if pid, err := strconv.Atoi(fields[1]); err == nil {
						res.AgentPID = pid
					}
				}
				res.AgentTool = tool
				res.ToolLiveness = "LIVE"
				res.ExecutionProvenance = "INDEPENDENT_USER_SESSION"
				return
			}
		}
	}
	if res.LockDetails != "" {
		res.ToolLiveness = "KILLED"
		res.ASEState = "ORPHANED"
		res.ExecutionProvenance = "COORDINATED_HARNESS_FAILED"
	} else if res.ASEState == "FAILED_DIRTY" || !res.IsClean {
		res.ToolLiveness = "TERMINATED"
	}
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
		if !strings.HasPrefix(filepath.Clean(targetCwd), cleanPath) {
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
			_ = syscall.Kill(pid, syscall.SIGTERM)
			time.Sleep(50 * time.Millisecond)
			if isPIDAlive(pid) {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	}
	return nil
}

// ResetASELocks removes stale lock files and resets .autopilot/state.json to IDLE.
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

	stateFile := filepath.Join(cleanPath, ".autopilot", "state.json")
	if b, err := os.ReadFile(stateFile); err == nil {
		var s map[string]any
		if json.Unmarshal(b, &s) == nil {
			s["state"] = "IDLE"
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
	}

	if len(res.SubmoduleDrift) > 0 {
		sb.WriteString(fmt.Sprintf("Submodule Drift: %s\n", strings.Join(res.SubmoduleDrift, ", ")))
	}

	if res.SubmodulesClean {
		sb.WriteString("Submodules: ALL CLEAN\n")
	} else {
		sb.WriteString(fmt.Sprintf("Submodules: DIRTY / DRIFT DETECTED (%s)\n",
			strings.Join(res.DirtySubmodules, ", ")))
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

	res.DiagnosticSummary = sb.String()
}
