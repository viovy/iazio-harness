// Package diagnostics analyzes IDE child execution traces and worktree git status,
// detects failure signatures, and emits Structured Intervention Requests (SIR).
package diagnostics

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Anomaly codes.
const (
	CodeAlreadyDoneOnMain    = "ALREADY_DONE_ON_MAIN"
	CodeDirtyWorktreePostExit = "DIRTY_WORKTREE_POST_EXIT"
	CodeBranchSuperseded     = "BRANCH_SUPERSEDED"
	CodeSlashCommandIgnored  = "SLASH_COMMAND_IGNORED"
	CodePermissionDenied     = "PERMISSION_DENIED"
	CodeMergeConflict        = "MERGE_CONFLICT"
	CodeNonZeroExit          = "NON_ZERO_EXIT"
)

// Finding represents one detected issue or anomaly.
type Finding struct {
	Code        string `json:"code"`
	Severity    string `json:"severity"` // "ERROR", "WARNING", "INFO"
	Message     string `json:"message"`
	Remediation string `json:"remediation"`
}

// SIRRecord is a Structured Intervention Request emitted for human or autonomous remediation.
type SIRRecord struct {
	SchemaVersion int       `json:"schema_version"`
	ID            string    `json:"id"`
	JobID         string    `json:"job_id"`
	StoryID       string    `json:"story_id,omitempty"`
	WorktreePath  string    `json:"worktree_path"`
	Severity      string    `json:"severity"`
	Category      string    `json:"category"`
	Summary       string    `json:"summary"`
	Findings      []Finding `json:"findings"`
	AutoHealable  bool      `json:"auto_healable"`
	Remediation   string    `json:"remediation"`
	CreatedAt     string    `json:"created_at"`
}

// DiagnosticReport is the overall result of diagnosing an execution.
type DiagnosticReport struct {
	Healthy      bool       `json:"healthy"`
	ExitCode     int        `json:"exit_code"`
	Findings     []Finding  `json:"findings"`
	StoryID      string     `json:"story_id,omitempty"`
	WorktreePath string     `json:"worktree_path"`
	Branch       string     `json:"branch,omitempty"`
	Porcelain    string     `json:"porcelain,omitempty"`
	SIR          *SIRRecord `json:"sir,omitempty"`
	SIRPath      string     `json:"sir_path,omitempty"`
	GeneratedAt  string     `json:"generated_at"`
}

// DiagnosticInput provides context for execution diagnosis.
type DiagnosticInput struct {
	JobID        string
	StoryID      string
	WorktreePath string
	ExitCode     int
	OutputText   string
	GitRunner    func(args ...string) (string, error)
}

var alreadyDoneRegexes = []*regexp.Regexp{
	regexp.MustCompile(`(?i)already\s+(completed|merged|landed)\s+(on|to|into)\s+(origin/)?main`),
	regexp.MustCompile(`(?i)already\s+landed\b`),
	regexp.MustCompile(`(?i)already\s+marked\s+DONE\b`),
	regexp.MustCompile(`(?i)already\s+closed\s+out\b`),
	regexp.MustCompile(`(?i)PR\s+#\d+\s+is\s+already\s+merged`),
}

var slashCommandDisabledRegexes = []*regexp.Regexp{
	regexp.MustCompile(`(?i)slash\s+commands?\s+are\s+disabled`),
	regexp.MustCompile(`(?i)slash\s+commands?\s+is\s+disabled`),
	regexp.MustCompile(`(?i)unknown\s+slash\s+command`),
	regexp.MustCompile(`(?i)unrecognized\s+slash\s+command`),
}

var permissionDeniedRegexes = []*regexp.Regexp{
	regexp.MustCompile(`(?i)permission\s+denied`),
	regexp.MustCompile(`(?i)401\s+Unauthorized`),
	regexp.MustCompile(`(?i)authentication\s+(failed|required)`),
	regexp.MustCompile(`(?i)invalid\s+token`),
	regexp.MustCompile(`(?i)fatal:\s+could\s+not\s+read\s+Username`),
}

var mergeConflictRegexes = []*regexp.Regexp{
	regexp.MustCompile(`(?i)CONFLICT\s+\(content\)`),
	regexp.MustCompile(`(?i)Automatic\s+merge\s+failed;\s+fix\s+conflicts`),
}

// Diagnose evaluates execution logs and worktree state to detect anomalies.
func Diagnose(input DiagnosticInput) DiagnosticReport {
	report := DiagnosticReport{
		Healthy:      true,
		ExitCode:     input.ExitCode,
		StoryID:      input.StoryID,
		WorktreePath: input.WorktreePath,
		GeneratedAt:  time.Now().UTC().Format(time.RFC3339),
	}

	git := input.GitRunner
	if git == nil {
		git = func(args ...string) (string, error) {
			cmd := exec.Command("git", append([]string{"-C", input.WorktreePath}, args...)...)
			out, err := cmd.CombinedOutput()
			return strings.TrimSpace(string(out)), err
		}
	}

	// 1. Check current git branch
	if input.WorktreePath != "" {
		if branch, err := git("rev-parse", "--abbrev-ref", "HEAD"); err == nil && branch != "" {
			report.Branch = branch
		}
	}

	// 2. Check worktree porcelain status
	if input.WorktreePath != "" {
		if porc, err := git("status", "--porcelain"); err == nil {
			report.Porcelain = porc
		}
	}

	// 3. Scan OutputText for signatures
	hasAlreadyDone := false
	for _, re := range alreadyDoneRegexes {
		if re.MatchString(input.OutputText) {
			hasAlreadyDone = true
			break
		}
	}

	hasSlashDisabled := false
	for _, re := range slashCommandDisabledRegexes {
		if re.MatchString(input.OutputText) {
			hasSlashDisabled = true
			break
		}
	}

	hasPermissionDenied := false
	for _, re := range permissionDeniedRegexes {
		if re.MatchString(input.OutputText) {
			hasPermissionDenied = true
			break
		}
	}

	hasMergeConflict := false
	for _, re := range mergeConflictRegexes {
		if re.MatchString(input.OutputText) {
			hasMergeConflict = true
			break
		}
	}

	// 4. Compile findings
	if hasAlreadyDone {
		if report.Branch != "" && report.Branch != "main" && report.Branch != "master" {
			report.Findings = append(report.Findings, Finding{
				Code:        CodeAlreadyDoneOnMain,
				Severity:    "ERROR",
				Message:     "Model execution detected that the requested story is already completed or merged on origin/main, but worktree remains on an unmerged feature branch.",
				Remediation: "Auto-heal worktree: checkout origin/main, pull latest, and clean up duplicate local branch.",
			})
		} else {
			report.Findings = append(report.Findings, Finding{
				Code:        CodeAlreadyDoneOnMain,
				Severity:    "INFO",
				Message:     "Requested story is already completed or merged on origin/main.",
				Remediation: "Close out story or pull latest origin/main.",
			})
		}
	}

	if hasSlashDisabled {
		report.Findings = append(report.Findings, Finding{
			Code:        CodeSlashCommandIgnored,
			Severity:    "ERROR",
			Message:     "Slash command was disabled or unrecognized by the execution engine.",
			Remediation: "Ensure DisableSlashCommands is set to false when prompt contains slash commands.",
		})
	}

	if hasPermissionDenied {
		report.Findings = append(report.Findings, Finding{
			Code:        CodePermissionDenied,
			Severity:    "ERROR",
			Message:     "Execution encountered permission or authentication denial.",
			Remediation: "Verify secret vault token, GitHub auth credentials, or environment permissions.",
		})
	}

	if hasMergeConflict {
		report.Findings = append(report.Findings, Finding{
			Code:        CodeMergeConflict,
			Severity:    "ERROR",
			Message:     "Git merge conflict occurred during execution.",
			Remediation: "Resolve conflicting files or rebase on latest origin/main.",
		})
	}

	// 5. Worktree cleanliness check after exit
	if input.ExitCode == 0 && report.Porcelain != "" {
		lines := strings.Split(strings.TrimSpace(report.Porcelain), "\n")
		count := len(lines)
		report.Findings = append(report.Findings, Finding{
			Code:        CodeDirtyWorktreePostExit,
			Severity:    "ERROR",
			Message:     fmt.Sprintf("Child engine exited with code 0 but left %d modified/untracked files in worktree without completing story.", count),
			Remediation: "Review uncommitted changes, complete ASE Phase 4/5, or discard untracked artifacts.",
		})
	}

	if input.ExitCode != 0 {
		report.Findings = append(report.Findings, Finding{
			Code:        CodeNonZeroExit,
			Severity:    "ERROR",
			Message:     fmt.Sprintf("Child engine process terminated with exit code %d.", input.ExitCode),
			Remediation: "Inspect execution spool log for errors, fix root cause, and re-execute.",
		})
	}

	// Determine healthy status and whether SIR is required
	hasError := false
	for _, f := range report.Findings {
		if f.Severity == "ERROR" {
			hasError = true
			break
		}
	}

	if hasError || input.ExitCode != 0 {
		report.Healthy = false
	}

	if hasError && input.WorktreePath != "" {
		sir := createSIR(input, report)
		report.SIR = sir
		if sirPath, err := writeSIR(input.WorktreePath, input.JobID, input.StoryID, sir); err == nil {
			report.SIRPath = sirPath
		}
	}

	return report
}

func sanitizeSlug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	reg := regexp.MustCompile(`[^a-z0-9_-]+`)
	s = reg.ReplaceAllString(s, "-")
	return strings.Trim(s, "-")
}

func createSIR(input DiagnosticInput, report DiagnosticReport) *SIRRecord {
	category := "EXECUTION_FAILURE"
	autoHeal := false
	remediation := "Inspect diagnostic findings and remediate manually."
	summary := fmt.Sprintf("Job %s encountered execution errors.", input.JobID)

	for _, f := range report.Findings {
		if f.Code == CodeAlreadyDoneOnMain {
			category = "SUPERSEDED_BRANCH"
			autoHeal = true
			remediation = f.Remediation
			summary = f.Message
			break
		} else if f.Code == CodeDirtyWorktreePostExit {
			category = "INCOMPLETE_DIRTY_WORKTREE"
			remediation = f.Remediation
			summary = f.Message
		} else if f.Code == CodeSlashCommandIgnored {
			category = "HARNESS_CONFIGURATION"
			remediation = f.Remediation
			summary = f.Message
		} else if f.Code == CodePermissionDenied {
			category = "AUTH_PERMISSION"
			remediation = f.Remediation
			summary = f.Message
		}
	}

	id := fmt.Sprintf("SIR-%d", time.Now().UnixNano()/1e6)
	if input.JobID != "" {
		id = fmt.Sprintf("SIR-%s-%d", input.JobID, time.Now().UnixNano()/1e6)
	}

	return &SIRRecord{
		SchemaVersion: 1,
		ID:            id,
		JobID:         input.JobID,
		StoryID:       input.StoryID,
		WorktreePath:  input.WorktreePath,
		Severity:      "HIGH",
		Category:      category,
		Summary:       summary,
		Findings:      report.Findings,
		AutoHealable:  autoHeal,
		Remediation:   remediation,
		CreatedAt:     time.Now().UTC().Format(time.RFC3339),
	}
}

func writeSIR(worktree, jobID, storyID string, sir *SIRRecord) (string, error) {
	reportsDir := filepath.Join(worktree, "tmp", "reports")
	if err := os.MkdirAll(reportsDir, 0755); err != nil {
		return "", err
	}

	slug := sanitizeSlug(storyID)
	if slug == "" {
		slug = "job"
	}
	filename := fmt.Sprintf("intervention-%s-%s.json", slug, sanitizeSlug(jobID))
	fullPath := filepath.Join(reportsDir, filename)

	data, err := json.MarshalIndent(sir, "", "  ")
	if err != nil {
		return "", err
	}

	if err := os.WriteFile(fullPath, data, 0644); err != nil {
		return "", err
	}
	return fullPath, nil
}
