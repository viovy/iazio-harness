package diagnostics

import (
	"os"
	"strings"
	"testing"
)

func TestDiagnose_CleanSuccess(t *testing.T) {
	tmpDir := t.TempDir()
	gitRunner := func(args ...string) (string, error) {
		if len(args) > 0 && args[0] == "rev-parse" {
			return "main", nil
		}
		if len(args) > 0 && args[0] == "status" {
			return "", nil
		}
		return "", nil
	}

	report := Diagnose(DiagnosticInput{
		JobID:        "job-100",
		StoryID:      "STORY-1",
		WorktreePath: tmpDir,
		ExitCode:     0,
		OutputText:   "Execution finished successfully.",
		GitRunner:    gitRunner,
	})

	if !report.Healthy {
		t.Fatalf("expected healthy report, got unhealthy: %+v", report)
	}
	if len(report.Findings) != 0 {
		t.Fatalf("expected 0 findings, got %d", len(report.Findings))
	}
	if report.SIR != nil {
		t.Fatalf("expected nil SIR, got %+v", report.SIR)
	}
}

func TestDiagnose_AlreadyDoneOnMain(t *testing.T) {
	tmpDir := t.TempDir()
	gitRunner := func(args ...string) (string, error) {
		if len(args) > 0 && args[0] == "rev-parse" {
			return "chore/iazio-0098-feature", nil
		}
		if len(args) > 0 && args[0] == "status" {
			return "?? temp.txt", nil
		}
		return "", nil
	}

	report := Diagnose(DiagnosticInput{
		JobID:        "job-1051",
		StoryID:      "STORY-APP-0098",
		WorktreePath: tmpDir,
		ExitCode:     0,
		OutputText:   "After reviewing git log, STORY-APP-0098 is already landed on origin/main via PR #3045.",
		GitRunner:    gitRunner,
	})

	if report.Healthy {
		t.Fatalf("expected unhealthy report due to superseded branch on duplicate feature branch")
	}
	if report.SIR == nil {
		t.Fatalf("expected SIR to be emitted, got nil")
	}
	if !report.SIR.AutoHealable {
		t.Fatalf("expected AutoHealable=true for superseded branch")
	}
	if report.SIR.Category != "SUPERSEDED_BRANCH" {
		t.Fatalf("expected category SUPERSEDED_BRANCH, got %s", report.SIR.Category)
	}
	if report.SIRPath == "" {
		t.Fatalf("expected non-empty SIRPath")
	}
	if _, err := os.Stat(report.SIRPath); err != nil {
		t.Fatalf("expected SIR file to exist at %s: %v", report.SIRPath, err)
	}
}

func TestDiagnose_DirtyWorktreePostExit(t *testing.T) {
	tmpDir := t.TempDir()
	gitRunner := func(args ...string) (string, error) {
		if len(args) > 0 && args[0] == "rev-parse" {
			return "main", nil
		}
		if len(args) > 0 && args[0] == "status" {
			return " M file.go\n?? untracked.go", nil
		}
		return "", nil
	}

	report := Diagnose(DiagnosticInput{
		JobID:        "job-200",
		StoryID:      "STORY-APP-0100",
		WorktreePath: tmpDir,
		ExitCode:     0,
		OutputText:   "I have edited the files for you.",
		GitRunner:    gitRunner,
	})

	if report.Healthy {
		t.Fatalf("expected unhealthy report when worktree is dirty post-exit")
	}
	foundDirty := false
	for _, f := range report.Findings {
		if f.Code == CodeDirtyWorktreePostExit {
			foundDirty = true
			break
		}
	}
	if !foundDirty {
		t.Fatalf("expected DIRTY_WORKTREE_POST_EXIT finding in report: %+v", report)
	}
	if report.SIR == nil {
		t.Fatalf("expected SIR emitted for dirty worktree")
	}
}

func TestDiagnose_SlashCommandIgnored(t *testing.T) {
	tmpDir := t.TempDir()
	report := Diagnose(DiagnosticInput{
		JobID:        "job-300",
		WorktreePath: tmpDir,
		ExitCode:     0,
		OutputText:   "Error: slash commands are disabled in print mode",
		GitRunner: func(args ...string) (string, error) {
			return "", nil
		},
	})

	if report.Healthy {
		t.Fatalf("expected unhealthy report when slash commands are disabled")
	}
	found := false
	for _, f := range report.Findings {
		if f.Code == CodeSlashCommandIgnored {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected SLASH_COMMAND_IGNORED finding, got: %+v", report)
	}
}

func TestDiagnose_PermissionDenied(t *testing.T) {
	tmpDir := t.TempDir()
	report := Diagnose(DiagnosticInput{
		JobID:        "job-400",
		WorktreePath: tmpDir,
		ExitCode:     1,
		OutputText:   "HTTP 401 Unauthorized: invalid token",
		GitRunner: func(args ...string) (string, error) {
			return "", nil
		},
	})

	if report.Healthy {
		t.Fatalf("expected unhealthy report for permission denied")
	}
	found := false
	for _, f := range report.Findings {
		if f.Code == CodePermissionDenied {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected PERMISSION_DENIED finding, got: %+v", report)
	}
}

func TestDiagnose_MergeConflict(t *testing.T) {
	tmpDir := t.TempDir()
	report := Diagnose(DiagnosticInput{
		JobID:        "job-500",
		WorktreePath: tmpDir,
		ExitCode:     1,
		OutputText:   "CONFLICT (content): Merge conflict in main.go\nAutomatic merge failed; fix conflicts",
		GitRunner: func(args ...string) (string, error) {
			return "", nil
		},
	})

	if report.Healthy {
		t.Fatalf("expected unhealthy report for merge conflict")
	}
	found := false
	for _, f := range report.Findings {
		if f.Code == CodeMergeConflict {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected MERGE_CONFLICT finding, got: %+v", report)
	}
}

func TestDiagnose_QuotaExhausted(t *testing.T) {
	report := Diagnose(DiagnosticInput{
		JobID:        "job-1067",
		StoryID:      "STORY-APP-0098",
		WorktreePath: t.TempDir(),
		ExitCode:     1,
		OutputText:   "ActionRequiredError: Increase limits for faster responses You're out of usage. Switch to Auto, or ask your admin to increase your limit to continue.",
		GitRunner: func(args ...string) (string, error) {
			return "", nil
		},
	})

	if report.Healthy {
		t.Fatalf("expected unhealthy report for quota exhaustion")
	}
	found := false
	for _, f := range report.Findings {
		if f.Code == CodeQuotaExhausted {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected QUOTA_EXHAUSTED finding, got: %+v", report.Findings)
	}
	if report.SIR == nil || report.SIR.Category != "QUOTA_EXHAUSTED" {
		t.Fatalf("expected SIR with category QUOTA_EXHAUSTED, got: %+v", report.SIR)
	}
}

func TestDiagnose_ProcessKilled(t *testing.T) {
	report := Diagnose(DiagnosticInput{
		JobID:        "job-1068",
		StoryID:      "STORY-APP-0098",
		WorktreePath: t.TempDir(),
		ExitCode:     137,
		OutputText:   "fatal error: out of memory allocating 2147483648 bytes\nsignal: killed\nKilled: 9\nexit status 137",
		GitRunner: func(args ...string) (string, error) {
			return "", nil
		},
	})

	if report.Healthy {
		t.Fatalf("expected unhealthy report for killed/OOM process")
	}
	found := false
	for _, f := range report.Findings {
		if f.Code == CodeProcessKilled {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected PROCESS_KILLED finding, got: %+v", report.Findings)
	}
	if report.SIR == nil || report.SIR.Category != "PROCESS_KILLED" {
		t.Fatalf("expected SIR with category PROCESS_KILLED, got: %+v", report.SIR)
	}
	summary := report.TroubleshootingSummary()
	if !strings.Contains(summary, "PROCESS_KILLED") || !strings.Contains(summary, "Remediation Action") {
		t.Fatalf("unexpected troubleshooting summary: %s", summary)
	}
}

func TestDiagnose_BinaryNotFound(t *testing.T) {
	report := Diagnose(DiagnosticInput{
		JobID:        "job-1069",
		StoryID:      "STORY-APP-0099",
		WorktreePath: t.TempDir(),
		ExitCode:     127,
		OutputText:   `exec: "agy": executable file not found in $PATH` + "\n" + `bash: command not found: agy`,
		GitRunner: func(args ...string) (string, error) {
			return "", nil
		},
	})

	if report.Healthy {
		t.Fatalf("expected unhealthy report for binary not found")
	}
	found := false
	for _, f := range report.Findings {
		if f.Code == CodeBinaryNotFound {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected BINARY_NOT_FOUND finding, got: %+v", report.Findings)
	}
	if report.SIR == nil || report.SIR.Category != "TOOL_UNAVAILABLE" {
		t.Fatalf("expected SIR with category TOOL_UNAVAILABLE, got: %+v", report.SIR)
	}
}

func TestDiagnose_ContextExceeded(t *testing.T) {
	report := Diagnose(DiagnosticInput{
		JobID:        "job-1070",
		StoryID:      "STORY-APP-0100",
		WorktreePath: t.TempDir(),
		ExitCode:     1,
		OutputText:   "API Error: context length exceeded (requested 128500 tokens, maximum context length is 128000)\ntoken limit exceeded",
		GitRunner: func(args ...string) (string, error) {
			return "", nil
		},
	})

	if report.Healthy {
		t.Fatalf("expected unhealthy report for context exceeded")
	}
	found := false
	for _, f := range report.Findings {
		if f.Code == CodeContextExceeded {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected CONTEXT_EXCEEDED finding, got: %+v", report.Findings)
	}
	if report.SIR == nil || report.SIR.Category != "CONTEXT_EXCEEDED" {
		t.Fatalf("expected SIR with category CONTEXT_EXCEEDED, got: %+v", report.SIR)
	}
}

func TestDiagnose_BuildFailed(t *testing.T) {
	report := Diagnose(DiagnosticInput{
		JobID:        "job-1071",
		StoryID:      "STORY-APP-0101",
		WorktreePath: t.TempDir(),
		ExitCode:     2,
		OutputText:   "go: build failed: syntax error: unexpected semicolon, expecting comma or )\ninternal/engine.go:42: undefined: FooBar",
		GitRunner: func(args ...string) (string, error) {
			return "", nil
		},
	})

	if report.Healthy {
		t.Fatalf("expected unhealthy report for build failed")
	}
	found := false
	for _, f := range report.Findings {
		if f.Code == CodeBuildFailed {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected BUILD_FAILED finding, got: %+v", report.Findings)
	}
	if report.SIR == nil || report.SIR.Category != "BUILD_FAILURE" {
		t.Fatalf("expected SIR with category BUILD_FAILURE, got: %+v", report.SIR)
	}
}

func TestDiagnose_TestFailed(t *testing.T) {
	report := Diagnose(DiagnosticInput{
		JobID:        "job-1072",
		StoryID:      "STORY-APP-0102",
		WorktreePath: t.TempDir(),
		ExitCode:     1,
		OutputText:   "=== RUN TestSomething\n--- FAIL: TestSomething (0.01s)\nFAIL github.com/viovy/iazio-harness/internal/engine [test failed]\nFAILED (failures=1)",
		GitRunner: func(args ...string) (string, error) {
			return "", nil
		},
	})

	if report.Healthy {
		t.Fatalf("expected unhealthy report for test failed")
	}
	found := false
	for _, f := range report.Findings {
		if f.Code == CodeTestFailed {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected TEST_FAILED finding, got: %+v", report.Findings)
	}
	if report.SIR == nil || report.SIR.Category != "TEST_FAILURE" {
		t.Fatalf("expected SIR with category TEST_FAILURE, got: %+v", report.SIR)
	}
}

func TestDiagnose_AlreadyDoneOnMainBranchInfo(t *testing.T) {
	report := Diagnose(DiagnosticInput{
		JobID:        "job-1073",
		StoryID:      "STORY-APP-0103",
		WorktreePath: t.TempDir(),
		ExitCode:     0,
		OutputText:   "Story is already landed on main.",
		GitRunner: func(args ...string) (string, error) {
			if len(args) > 0 && args[0] == "rev-parse" {
				return "main", nil
			}
			return "", nil
		},
	})

	if !report.Healthy {
		t.Fatalf("expected healthy report when already done on main and worktree is clean")
	}
	if len(report.Findings) != 1 || report.Findings[0].Severity != "INFO" {
		t.Fatalf("expected 1 INFO finding, got: %+v", report.Findings)
	}
}

func TestDiagnose_DefaultGitRunner(t *testing.T) {
	dir := t.TempDir()
	report := Diagnose(DiagnosticInput{
		JobID:        "job-default-git",
		StoryID:      "STORY-DEFAULT-GIT",
		WorktreePath: dir,
		ExitCode:     0,
		OutputText:   "Clean output",
		GitRunner:    nil, // exercises default git invocation path
	})
	if !report.Healthy {
		t.Fatalf("expected healthy report, got: %+v", report)
	}
}

func TestDiagnose_WriteSIREmptyStoryID(t *testing.T) {
	dir := t.TempDir()
	report := Diagnose(DiagnosticInput{
		JobID:        "job-empty-story",
		StoryID:      "",
		WorktreePath: dir,
		ExitCode:     1,
		OutputText:   "fatal: something went wrong",
		GitRunner: func(args ...string) (string, error) {
			return "", nil
		},
	})
	if report.Healthy {
		t.Fatalf("expected unhealthy")
	}
	if report.SIRPath == "" {
		t.Fatalf("expected SIRPath to be generated even when story ID is empty")
	}
	if !strings.Contains(report.SIRPath, "intervention-job-job-empty-story.json") {
		t.Fatalf("expected default 'job' slug in filename, got: %s", report.SIRPath)
	}
}

func TestDiagnose_GoldenFiles(t *testing.T) {
	fixtures := []struct {
		file         string
		expectedCode string
	}{
		{"quota_exhausted.log", CodeQuotaExhausted},
		{"process_killed_oom.log", CodeProcessKilled},
		{"binary_not_found.log", CodeBinaryNotFound},
		{"context_exceeded.log", CodeContextExceeded},
		{"build_failed.log", CodeBuildFailed},
		{"test_failed.log", CodeTestFailed},
		{"clean_success.log", ""},
	}

	for _, tc := range fixtures {
		t.Run(tc.file, func(t *testing.T) {
			content, err := os.ReadFile("testdata/" + tc.file)
			if err != nil {
				t.Fatalf("failed to read fixture %s: %v", tc.file, err)
			}
			exitCode := 0
			if tc.expectedCode != "" {
				exitCode = 1
			}
			report := Diagnose(DiagnosticInput{
				JobID:        "job-fixture",
				StoryID:      "STORY-FIXTURE",
				WorktreePath: t.TempDir(),
				ExitCode:     exitCode,
				OutputText:   string(content),
				GitRunner: func(args ...string) (string, error) {
					return "", nil
				},
			})

			if tc.expectedCode == "" {
				if !report.Healthy {
					t.Fatalf("expected clean healthy report for %s", tc.file)
				}
				cleanSummary := report.TroubleshootingSummary()
				if !strings.Contains(cleanSummary, "No anomalies") {
					t.Fatalf("expected 'No anomalies' in clean summary: %s", cleanSummary)
				}
			} else {
				if report.Healthy {
					t.Fatalf("expected unhealthy report for %s", tc.file)
				}
				found := false
				for _, f := range report.Findings {
					if f.Code == tc.expectedCode {
						found = true
						break
					}
				}
				if !found {
					t.Fatalf("expected code %s in findings for fixture %s: %+v", tc.expectedCode, tc.file, report.Findings)
				}
			}
		})
	}
}

func TestTroubleshootingSummary_FullFields(t *testing.T) {
	report := DiagnosticReport{
		Healthy:      false,
		ExitCode:     1,
		StoryID:      "STORY-FULL-FIELDS",
		Branch:       "feat/test-branch",
		WorktreePath: "/tmp/worktree",
		SIRPath:      "/tmp/worktree/tmp/reports/intervention-test.json",
		Findings: []Finding{
			{
				Code:        CodeProcessKilled,
				Severity:    "ERROR",
				Message:     "Process was killed",
				Remediation: "Check host memory limits",
			},
		},
	}

	summary := report.TroubleshootingSummary()
	if !strings.Contains(summary, "STORY-FULL-FIELDS") ||
		!strings.Contains(summary, "feat/test-branch") ||
		!strings.Contains(summary, "/tmp/worktree") ||
		!strings.Contains(summary, "intervention-test.json") {
		t.Fatalf("missing expected fields in troubleshooting summary: %s", summary)
	}
}
