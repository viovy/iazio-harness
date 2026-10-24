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
	CodeAlreadyDoneOnMain     = "ALREADY_DONE_ON_MAIN"
	CodeDirtyWorktreePostExit = "DIRTY_WORKTREE_POST_EXIT"
	CodeBranchSuperseded      = "BRANCH_SUPERSEDED"
	CodeSlashCommandIgnored   = "SLASH_COMMAND_IGNORED"
	CodePermissionDenied      = "PERMISSION_DENIED"
	CodeMergeConflict         = "MERGE_CONFLICT"
	CodeQuotaExhausted        = "QUOTA_EXHAUSTED"
	CodeProcessKilled         = "PROCESS_KILLED"
	CodeBinaryNotFound        = "BINARY_NOT_FOUND"
	CodeContextExceeded       = "CONTEXT_EXCEEDED"
	CodeBuildFailed           = "BUILD_FAILED"
	CodeTestFailed            = "TEST_FAILED"
	CodeNonZeroExit           = "NON_ZERO_EXIT"
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
	ASEComplete  bool       `json:"ase_complete"`
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

var quotaExhaustedRegexes = []*regexp.Regexp{
	regexp.MustCompile(`(?i)ActionRequiredError`),
	regexp.MustCompile(`(?i)Increase\s+limits\s+for\s+faster\s+responses`),
	regexp.MustCompile(`(?i)You're\s+out\s+of\s+usage`),
	regexp.MustCompile(`(?i)out\s+of\s+usage`),
	regexp.MustCompile(`(?i)Switch\s+to\s+Auto,\s+or\s+ask\s+your\s+admin\s+to\s+increase\s+your\s+limit`),
	regexp.MustCompile(`(?i)rate\s+limit\s+exceeded`),
	regexp.MustCompile(`(?i)quota\s+exceeded`),
	regexp.MustCompile(`(?i)RESOURCE_EXHAUSTED`),
	regexp.MustCompile(`(?i)insufficient_quota`),
	regexp.MustCompile(`(?i)429\s+Too\s+Many\s+Requests`),
}

var processKilledRegexes = []*regexp.Regexp{
	regexp.MustCompile(`(?i)Killed:\s*9`),
	regexp.MustCompile(`(?i)signal:\s*(killed|terminated)`),
	regexp.MustCompile(`(?i)fatal\s+error:\s*out\s+of\s+memory`),
	regexp.MustCompile(`(?i)runtime:\s*out\s+of\s+memory`),
	regexp.MustCompile(`(?i)exit\s+status\s+(137|143)`),
	regexp.MustCompile(`(?i)segmentation\s+fault`),
	regexp.MustCompile(`(?i)process_disappeared_on_host`),
}

var binaryNotFoundRegexes = []*regexp.Regexp{
	regexp.MustCompile(`(?i)executable\s+file\s+not\s+found`),
	regexp.MustCompile(`(?i)command\s+not\s+found:\s*([a-zA-Z0-9_-]+)`),
	regexp.MustCompile(`(?i)no\s+such\s+file\s+or\s+directory.*(?:bin|agy|agent|cursor|autopilot|git)`),
}

var contextExceededRegexes = []*regexp.Regexp{
	regexp.MustCompile(`(?i)context\s+length\s+exceeded`),
	regexp.MustCompile(`(?i)maximum\s+context\s+length`),
	regexp.MustCompile(`(?i)token\s+limit\s+exceeded`),
}

var buildFailedRegexes = []*regexp.Regexp{
	regexp.MustCompile(`(?i)build\s+failed`),
	regexp.MustCompile(`(?i)compilation\s+error`),
	regexp.MustCompile(`(?i)syntax\s+error:`),
	regexp.MustCompile(`(?i)undefined:\s+[a-zA-Z0-9_]+`),
}

var testFailedRegexes = []*regexp.Regexp{
	regexp.MustCompile(`(?i)---\s+FAIL:`),
	regexp.MustCompile(`(?i)FAIL\s+github\.com/`),
	regexp.MustCompile(`(?i)FAILED\s+\(failures=\d+\)`),
}

// isStoryDone checks whether the story markdown file exists and has status: DONE locally or on origin/main.
func isStoryDone(worktreePath, storyID string, git func(args ...string) (string, error)) bool {
	if storyID == "" {
		return false
	}
	// Check locally in docs/stories/*.story.md
	pattern := filepath.Join(worktreePath, "docs", "stories", "*.story.md")
	matches, _ := filepath.Glob(pattern)
	for _, m := range matches {
		data, err := os.ReadFile(m)
		if err == nil {
			content := string(data)
			if strings.Contains(content, storyID) {
				if strings.Contains(content, "status: DONE") || strings.Contains(content, "status: \"DONE\"") || strings.Contains(content, "status: 'DONE'") {
					return true
				}
			}
		}
	}

	// Check on origin/main via git log
	if git != nil {
		if out, err := git("log", "origin/main", "-n", "10", "--grep="+storyID, "--format=%s %b"); err == nil && strings.TrimSpace(out) != "" {
			lower := strings.ToLower(out)
			if strings.Contains(lower, "closeout") || strings.Contains(lower, "done") || strings.Contains(lower, "closed") {
				return true
			}
		}
	}
	return false
}

// CheckASECompletion evaluates whether the story execution has completed on origin/main or locally.
func CheckASECompletion(worktreePath, storyID string, git func(args ...string) (string, error)) (complete bool, branchMerged bool, autoHealed bool) {
	if git == nil || worktreePath == "" {
		return false, false, false
	}
	porc, _ := git("status", "--porcelain")
	isClean := strings.TrimSpace(porc) == ""

	branch, _ := git("rev-parse", "--abbrev-ref", "HEAD")
	branch = strings.TrimSpace(branch)

	storyDone := isStoryDone(worktreePath, storyID, git)

	if branch != "" && branch != "main" && branch != "master" {
		if _, err := git("merge-base", "--is-ancestor", "HEAD", "origin/main"); err == nil {
			branchMerged = true
		} else if storyID != "" {
			// In squash merges, branch HEAD may not be an ancestor, but commit exists on origin/main
			if out, err := git("log", "origin/main", "-n", "1", "--grep="+storyID, "--format=%H"); err == nil && strings.TrimSpace(out) != "" {
				branchMerged = true
			}
		}
		if !branchMerged {
			if out, err := git("log", "origin/main", "-n", "1", "--grep="+branch, "--format=%H"); err == nil && strings.TrimSpace(out) != "" {
				branchMerged = true
			}
		}

		// If branch is merged and worktree is clean, auto-heal to main
		if branchMerged && isClean {
			if _, err := git("checkout", "main"); err == nil {
				_, _ = git("pull", "--ff-only")
				_, _ = git("branch", "-D", branch)
				autoHealed = true
				complete = true
				return complete, branchMerged, autoHealed
			}
			complete = true
			return complete, branchMerged, autoHealed
		}
	} else if storyID != "" {
		// Already on main/master: must verify story is actually merged/closed out on origin/main or marked DONE
		if out, err := git("log", "origin/main", "-n", "1", "--grep="+storyID, "--format=%H"); err == nil && strings.TrimSpace(out) != "" {
			if storyDone {
				branchMerged = true
				if isClean {
					complete = true
					return complete, branchMerged, autoHealed
				}
			}
		}
		if storyDone && isClean {
			complete = true
			return complete, branchMerged, autoHealed
		}
	}

	return false, branchMerged, autoHealed
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

	// 2b. Evaluate ASE completion and branch merge state
	aseComplete, branchMerged, autoHealed := CheckASECompletion(input.WorktreePath, input.StoryID, git)
	if autoHealed {
		report.Branch = "main"
		report.Porcelain = ""
	}
	report.ASEComplete = aseComplete

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

	hasQuotaExhausted := false
	for _, re := range quotaExhaustedRegexes {
		if re.MatchString(input.OutputText) {
			hasQuotaExhausted = true
			break
		}
	}

	hasProcessKilled := false
	for _, re := range processKilledRegexes {
		if re.MatchString(input.OutputText) {
			hasProcessKilled = true
			break
		}
	}

	hasBinaryNotFound := false
	for _, re := range binaryNotFoundRegexes {
		if re.MatchString(input.OutputText) {
			hasBinaryNotFound = true
			break
		}
	}

	hasContextExceeded := false
	for _, re := range contextExceededRegexes {
		if re.MatchString(input.OutputText) {
			hasContextExceeded = true
			break
		}
	}

	hasBuildFailed := false
	for _, re := range buildFailedRegexes {
		if re.MatchString(input.OutputText) {
			hasBuildFailed = true
			break
		}
	}

	hasTestFailed := false
	for _, re := range testFailedRegexes {
		if re.MatchString(input.OutputText) {
			hasTestFailed = true
			break
		}
	}

	// 4. Compile findings
	if hasAlreadyDone {
		if report.Branch != "" && report.Branch != "main" && report.Branch != "master" && !branchMerged {
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
			report.ASEComplete = true
		}
	}

	if hasQuotaExhausted {
		report.Findings = append(report.Findings, Finding{
			Code:        CodeQuotaExhausted,
			Severity:    "ERROR",
			Message:     "Execution engine encountered model usage/quota exhaustion or rate limit (ActionRequiredError/RESOURCE_EXHAUSTED).",
			Remediation: "Switch execution engine (e.g. to agy with Auto/alternate model tier) or increase model usage quota.",
		})
	}

	if hasProcessKilled {
		report.Findings = append(report.Findings, Finding{
			Code:        CodeProcessKilled,
			Severity:    "ERROR",
			Message:     "Child execution process was killed, crashed, or disappeared on host (OOM / SIGKILL / SIGSEGV).",
			Remediation: "Verify host resource limits (memory/swap), check system logs for OOM events, and resume job using a smaller model tier.",
		})
	}

	if hasBinaryNotFound {
		report.Findings = append(report.Findings, Finding{
			Code:        CodeBinaryNotFound,
			Severity:    "ERROR",
			Message:     "Required tool executable or binary was not found on host PATH.",
			Remediation: "Ensure tool binary is installed in ~/.local/bin or ~/.iazio/bin and present in the agent host PATH.",
		})
	}

	if hasContextExceeded {
		report.Findings = append(report.Findings, Finding{
			Code:        CodeContextExceeded,
			Severity:    "ERROR",
			Message:     "Model context length or token limit exceeded during prompt execution.",
			Remediation: "Compact conversation history, reduce prompt size, or switch to a high-capacity model tier.",
		})
	}

	if hasBuildFailed {
		report.Findings = append(report.Findings, Finding{
			Code:        CodeBuildFailed,
			Severity:    "ERROR",
			Message:     "Compilation, build, or syntax check failed during execution.",
			Remediation: "Inspect compiler output, correct syntax/type errors, and verify local build passes.",
		})
	}

	if hasTestFailed {
		report.Findings = append(report.Findings, Finding{
			Code:        CodeTestFailed,
			Severity:    "ERROR",
			Message:     "Automated unit or integration tests failed during execution.",
			Remediation: "Run test suite locally, fix failing assertions, and verify 100% test pass rate.",
		})
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
		sev := "ERROR"
		if report.ASEComplete {
			sev = "WARNING"
		}
		report.Findings = append(report.Findings, Finding{
			Code:        CodeNonZeroExit,
			Severity:    sev,
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

	if hasError || (input.ExitCode != 0 && !report.ASEComplete) {
		report.Healthy = false
	} else if report.ASEComplete {
		report.Healthy = true
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
			autoHeal = true
			remediation = "Autonomously resume execution in same conversation ID with allow_dirty to complete story flow."
			summary = f.Message
		} else if f.Code == CodeSlashCommandIgnored {
			category = "HARNESS_CONFIGURATION"
			remediation = f.Remediation
			summary = f.Message
		} else if f.Code == CodePermissionDenied {
			category = "AUTH_PERMISSION"
			remediation = f.Remediation
			summary = f.Message
		} else if f.Code == CodeQuotaExhausted {
			category = "QUOTA_EXHAUSTED"
			remediation = f.Remediation
			summary = f.Message
		} else if f.Code == CodeProcessKilled {
			category = "PROCESS_KILLED"
			remediation = f.Remediation
			summary = f.Message
		} else if f.Code == CodeBinaryNotFound {
			category = "TOOL_UNAVAILABLE"
			remediation = f.Remediation
			summary = f.Message
		} else if f.Code == CodeContextExceeded {
			category = "CONTEXT_EXCEEDED"
			remediation = f.Remediation
			summary = f.Message
		} else if f.Code == CodeBuildFailed {
			category = "BUILD_FAILURE"
			remediation = f.Remediation
			summary = f.Message
		} else if f.Code == CodeTestFailed {
			category = "TEST_FAILURE"
			remediation = f.Remediation
			summary = f.Message
		} else if f.Code == CodeMergeConflict {
			category = "MERGE_CONFLICT"
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

// TroubleshootingSummary formats a human-readable markdown troubleshooting report.
func (r DiagnosticReport) TroubleshootingSummary() string {
	var sb strings.Builder
	status := "HEALTHY"
	if !r.Healthy {
		status = "UNHEALTHY / DEGRADED"
	}
	sb.WriteString(fmt.Sprintf("### Diagnostics & Troubleshooting Report [%s]\n\n", status))
	sb.WriteString(fmt.Sprintf("- **Exit Code:** %d\n", r.ExitCode))
	if r.StoryID != "" {
		sb.WriteString(fmt.Sprintf("- **Story ID:** %s\n", r.StoryID))
	}
	if r.Branch != "" {
		sb.WriteString(fmt.Sprintf("- **Branch:** `%s`\n", r.Branch))
	}
	if r.WorktreePath != "" {
		sb.WriteString(fmt.Sprintf("- **Worktree:** `%s`\n", r.WorktreePath))
	}
	if r.SIRPath != "" {
		sb.WriteString(fmt.Sprintf("- **SIR Generated:** [`%s`](file://%s)\n", filepath.Base(r.SIRPath), r.SIRPath))
	}

	if len(r.Findings) == 0 {
		sb.WriteString("\n*No anomalies or failure signatures detected.*\n")
		return sb.String()
	}

	sb.WriteString("\n#### Detected Anomalies & Actionable Remedies:\n\n")
	for i, f := range r.Findings {
		sb.WriteString(fmt.Sprintf("%d. **[%s] %s**\n", i+1, f.Severity, f.Code))
		sb.WriteString(fmt.Sprintf("   - **Message:** %s\n", f.Message))
		sb.WriteString(fmt.Sprintf("   - **Remediation Action:** %s\n\n", f.Remediation))
	}

	return sb.String()
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
