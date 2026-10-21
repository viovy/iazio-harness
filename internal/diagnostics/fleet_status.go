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

	// 2. Git Submodules
	subCmd := exec.CommandContext(ctx, "git", "-C", cleanPath, "submodule", "status", "--recursive")
	if subOut, err := subCmd.CombinedOutput(); err == nil {
		parseSubmoduleStatus(string(subOut), res)
	}

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
			res.IsClean = false
			parts := strings.Fields(l)
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

func parseSubmoduleStatus(out string, res *RepoFleetStatusResult) {
	lines := strings.Split(out, "\n")
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		// Prefix '+' indicates commit does not match index; '-' not initialized; 'U' merge conflict
		if strings.HasPrefix(l, "+") || strings.HasPrefix(l, "-") || strings.HasPrefix(l, "U") {
			res.SubmodulesClean = false
			parts := strings.Fields(l)
			if len(parts) >= 2 {
				res.DirtySubmodules = append(res.DirtySubmodules, parts[1])
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
	} else if !res.IsClean && res.ASEState == "IDLE" {
		res.ASEState = "FAILED_DIRTY"
	}
}

var knownAgentBinaries = []string{
	"agy", "antigravity", "opencode", "cursor", "claude", "autopilot", "iazio-harness",
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

		toolName, match := isAgentTool(cmdArgs[0])
		if !match && len(cmdArgs) > 1 {
			toolName, match = isAgentTool(cmdArgs[1])
		}
		if !match {
			continue
		}

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
		if strings.Contains(environStr, "IAZIO_HARNESS_JOB_ID") ||
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
		return
	}

	// No running agent PID found
	if res.LockDetails != "" {
		res.ToolLiveness = "KILLED"
		res.ASEState = "ORPHANED"
		res.ExecutionProvenance = "COORDINATED_HARNESS_FAILED"
	} else if res.ASEState == "FAILED_DIRTY" || !res.IsClean {
		res.ToolLiveness = "TERMINATED"
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

func buildDiagnosticSummary(res *RepoFleetStatusResult) {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Repository: %s\n", res.RepoPath))
	sb.WriteString(fmt.Sprintf("Branch: %s (head: %.8s)", res.Branch, res.HeadSHA))
	if res.Ahead > 0 || res.Behind > 0 {
		sb.WriteString(fmt.Sprintf(" [ahead +%d, behind -%d]", res.Ahead, res.Behind))
	}
	sb.WriteString("\n")

	if res.IsClean {
		sb.WriteString("Working Tree: CLEAN\n")
	} else {
		sb.WriteString(fmt.Sprintf("Working Tree: DIRTY (%d modified, %d untracked)\n",
			len(res.ModifiedFiles), len(res.UntrackedFiles)))
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
