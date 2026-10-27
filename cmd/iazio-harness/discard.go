package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/viovy/iazio-harness/internal/checkpoint"
	"github.com/viovy/iazio-harness/internal/controlplane"
	"github.com/viovy/iazio-harness/internal/gitclean"
	"github.com/viovy/iazio-harness/internal/repolock"
	"github.com/viovy/iazio-harness/internal/spool"
	"github.com/viovy/iazio-harness/internal/watchdog"
)

// runDiscardCommand implements the CLI command `iazio-harness discard` (and clean-git, git-clean, reset-workspace).
func runDiscardCommand(args []string) error {
	var worktree string
	force := false
	jsonOutput := false
	submodules := true

	for i := 0; i < len(args); i++ {
		switch {
		case (args[i] == "--worktree-path" || args[i] == "--repo") && i+1 < len(args):
			i++
			worktree = args[i]
		case strings.HasPrefix(args[i], "--worktree-path="):
			worktree = strings.TrimPrefix(args[i], "--worktree-path=")
		case strings.HasPrefix(args[i], "--repo="):
			worktree = strings.TrimPrefix(args[i], "--repo=")
		case args[i] == "--force" || args[i] == "-f":
			force = true
		case args[i] == "--json" || args[i] == "-json":
			jsonOutput = true
		case args[i] == "--no-submodules":
			submodules = false
		}
	}

	if worktree == "" {
		worktree = "."
	}

	res, err := gitclean.DiscardWorkspace(context.Background(), gitclean.DiscardOptions{
		WorktreePath: worktree,
		Force:        force,
		Submodules:   submodules,
		RemoveLocks:  true,
		OnProgress: func(stage, detail string) {
			if !jsonOutput {
				fmt.Printf("[iazio-harness] [%s] %s\n", stage, detail)
			}
		},
	})

	if jsonOutput {
		data, _ := res.JSON()
		fmt.Println(string(data))
	} else {
		fmt.Print(res.String())
	}

	if err != nil {
		return err
	}
	if !res.Success || !res.Clean {
		return fmt.Errorf("workspace reset incomplete: remaining dirty porcelain: %s", res.RemainingPorcelain)
	}
	return nil
}

// executePureCodeAction handles deterministic pure code execution jobs (e.g. discard, clean)
// directly inside `executeJob` without calling an LLM engine runner (agy/agent/opencode).
func executePureCodeAction(ctx context.Context, api, token string, job controlplane.JobDetail, targetDir, docsHub string, sp *spool.Spool, wd *watchdog.Watchdog) error {
	start := time.Now()
	jobID := job.ID

	banner := fmt.Sprintf("[iazio-harness] Executing pure code action: %s (job: %s, worktree: %s)\n"+
		"[iazio-harness] Note: Pure code actions do not invoke AI agents (agy/agent/opencode) and require zero conversational checkpoints.\n",
		job.Kind, jobID, targetDir)
	_ = sp.AppendRaw("stdout", []byte(banner))
	_ = controlplane.PostChunk(ctx, api, token, jobID, "stdout", banner)

	lockHandle, lockErr := repolock.Acquire(targetDir, repolock.LockInfo{
		JobID:          job.ID,
		ScheduleID:     job.ScheduleID,
		PID:            os.Getpid(),
		HarnessPID:     os.Getpid(),
		Engine:         "iazio-harness",
		ConversationID: "",
		StoryID:        job.StoryID,
		Prompt:         job.Prompt,
		Status:         "RUNNING",
	}, func(pid int) bool {
		return watchdog.IsProcessAlive(pid)
	})
	if lockErr != nil {
		errMsg := fmt.Sprintf("[iazio-harness] Repository lock acquisition failed: %v\n", lockErr)
		_ = sp.AppendRaw("stderr", []byte(errMsg))
		_ = controlplane.PostChunk(context.Background(), api, token, jobID, "stderr", errMsg)
		_ = controlplane.PostExit(context.Background(), api, token, jobID, 1)
		wd.Fail(jobID, 1, errMsg)
		return lockErr
	}
	defer lockHandle.Release()

	res, err := gitclean.DiscardWorkspace(ctx, gitclean.DiscardOptions{
		WorktreePath: targetDir,
		Force:        true,
		Submodules:   true,
		RemoveLocks:  true,
		OnProgress: func(stage, detail string) {
			msg := fmt.Sprintf("[iazio-harness] [%s] %s\n", stage, detail)
			_ = sp.AppendRaw("stdout", []byte(msg))
			_ = controlplane.PostChunk(context.Background(), api, token, jobID, "stdout", msg)
		},
	})

	durationStr := time.Since(start).Truncate(time.Millisecond).String()

	if err != nil || !res.Clean {
		failMsg := fmt.Sprintf("[iazio-harness] Pure code action %s failed after %s: %v (porcelain: %s)\n", job.Kind, durationStr, err, res.RemainingPorcelain)
		_ = sp.AppendRaw("stderr", []byte(failMsg))
		_ = controlplane.PostChunk(context.Background(), api, token, jobID, "stderr", failMsg)

		exitOpts := controlplane.ExitOptions{
			ExitCode:                 1,
			DiagnosticCode:           "CLEAN_FAILED",
			Reason:                   "CLEAN_FAILED",
			TerminationReasonCode:    "CLEAN_FAILED",
			TerminationReasonMessage: failMsg,
			ErrorMessage:             failMsg,
			Remediation:              "Verify git repository permissions and file locks.",
			Resumable:                false,
			NonResumableReason:       "Pure code action failed due to file or git lock errors",
			WorkspaceDirtyFiles:      collectWorkspaceDirtyFiles(targetDir),
		}
		_ = controlplane.PostExitWithOptions(context.Background(), api, token, jobID, exitOpts)
		wd.Fail(jobID, 1, failMsg)
		return fmt.Errorf("pure code action failed: %w", err)
	}

	successMsg := fmt.Sprintf("[iazio-harness] Pure code action %s completed successfully in %s. Status: git clean.\n", job.Kind, durationStr)
	_ = sp.AppendRaw("stdout", []byte(successMsg))
	_ = controlplane.PostChunk(context.Background(), api, token, jobID, "stdout", successMsg)

	_ = checkpoint.Save(targetDir, spool.DefaultDir(), checkpoint.State{
		JobID:                    job.ID,
		ScheduleID:               job.ScheduleID,
		Engine:                   "iazio-harness",
		WorktreePath:             targetDir,
		ConversationID:           "",
		StoryID:                  job.StoryID,
		StepIndex:                1,
		StartedAt:                start.UTC().Format(time.RFC3339),
		Status:                   "COMPLETED",
		ExitCode:                 0,
		TerminationReasonCode:    "CLEAN_SUCCESS",
		TerminationReasonMessage: res.Message,
		Resumable:                false,
		NonResumableReason:       "",
		WorkspaceDirtyFiles:      nil,
	})

	exitOpts := controlplane.ExitOptions{
		ExitCode:                 0,
		DiagnosticCode:           "SUCCESS",
		Reason:                   "COMPLETED",
		TerminationReasonCode:    "CLEAN_SUCCESS",
		TerminationReasonMessage: res.Message,
		ErrorMessage:             "",
		Remediation:              "",
		Resumable:                false,
		NonResumableReason:       "",
		WorkspaceDirtyFiles:      nil,
	}
	_ = controlplane.PostExitWithOptions(context.Background(), api, token, jobID, exitOpts)
	wd.Complete(jobID, 0)
	return nil
}
