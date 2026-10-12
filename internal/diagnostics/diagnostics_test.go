package diagnostics

import (
	"os"
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
