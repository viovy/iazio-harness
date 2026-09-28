// Command iazio-harness runs one stored prompt and exits.
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/viovy/iazio-harness/internal/auth"
	"github.com/viovy/iazio-harness/internal/controlplane"
	"github.com/viovy/iazio-harness/internal/engines"
	"github.com/viovy/iazio-harness/internal/prompt"
	"github.com/viovy/iazio-harness/internal/spool"
)

var (
	version = "0.1.0-dev"
	commit  = "unknown"
	branch  = "unknown"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: iazio-harness version|auth|run")
	}
	switch args[0] {
	case "version":
		fmt.Println(formatVersion())
		return nil
	case "auth":
		if len(args) > 1 && args[1] == "login" {
			return auth.Login(context.Background())
		}
		st, err := auth.Status()
		if err != nil {
			return err
		}
		fmt.Println(st.String())
		return nil
	case "run":
		return runJob(args[1:])
	default:
		return fmt.Errorf("unknown command")
	}
}

func runJob(args []string) error {
	var api, jobID, worktree, docsHub, chunk string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--api-url":
			i++
			if i < len(args) {
				api = args[i]
			}
		case "--job-id":
			i++
			if i < len(args) {
				jobID = args[i]
			}
		case "--worktree-path":
			i++
			if i < len(args) {
				worktree = args[i]
			}
		case "--docs-hub-path":
			i++
			if i < len(args) {
				docsHub = args[i]
			}
		case "--chunk":
			i++
			if i < len(args) {
				chunk = args[i]
			}
		}
	}
	if api == "" {
		api = os.Getenv("IAZIO_HARNESS_API_URL")
	}
	if api == "" {
		api = "https://tian.go.ro/iazio-harness-api"
	}
	if jobID == "" {
		return fmt.Errorf("job-id is required")
	}
	if chunk != "" {
		return controlplane.PostChunk(context.Background(), api, "", jobID, "stdout", chunk)
	}
	return executeJob(context.Background(), api, jobID, worktree, docsHub)
}

func executeJob(ctx context.Context, api, jobID, worktree, docsHub string) error {
	token, _ := auth.Bearer(ctx)
	job, err := controlplane.GetJob(ctx, api, token, jobID)
	if err != nil {
		return fmt.Errorf("get job: %w", err)
	}
	if worktree == "" {
		worktree = job.WorktreePath
	}
	if docsHub == "" {
		docsHub = job.DocsHubPath
	}
	targetDir, err := engines.CheckoutDir(job.Kind, worktree, docsHub)
	if err != nil {
		return err
	}
	lock, err := engines.Hold(targetDir, "")
	if err != nil {
		return fmt.Errorf("checkout lock: %w", err)
	}
	defer lock.Release()

	prepRes, err := prompt.Prepare(prompt.Input{
		Kind:       job.Kind,
		JobID:      job.ID,
		Body:       job.Prompt,
		Transcript: job.Transcript,
		SpoolDir:   spool.DefaultDir(),
		Values: prompt.Values{
			StoryID:        job.StoryID,
			TargetLeafName: filepath.Base(worktree),
			WorktreePath:   worktree,
			DocsHubPath:    docsHub,
		},
	})
	if err != nil {
		return fmt.Errorf("prepare prompt: %w", err)
	}

	sp, err := spool.Open(spool.DefaultDir(), jobID)
	if err != nil {
		return fmt.Errorf("open spool: %w", err)
	}
	defer sp.Close()

	flushCtx, stopFlush := context.WithCancel(ctx)
	defer stopFlush()
	go func() {
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-flushCtx.Done():
				return
			case <-ticker.C:
				events := sp.Flush()
				for _, ev := range events {
					if ev.Type == spool.EventOutputChunk {
						_ = controlplane.PostChunk(context.Background(), api, token, jobID, ev.Stream, ev.Text)
					}
				}
			}
		}
	}()

	runner := engines.CLI{Engine: job.Engine}
	execReq := engines.ExecutionRequest{
		Engine:       job.Engine,
		Kind:         job.Kind,
		Prompt:       prepRes.Prompt,
		WorktreePath: worktree,
		DocsHubPath:  docsHub,
		ScheduleEnv:  job.EnvVars,
		LogFile:      filepath.Join(spool.DefaultDir(), jobID+".engine.log"),
	}
	execRes, execErr := runner.Execute(ctx, execReq, sp)
	stopFlush()

	// Final spool flush
	for _, ev := range sp.Flush() {
		if ev.Type == spool.EventOutputChunk {
			_ = controlplane.PostChunk(context.Background(), api, token, jobID, ev.Stream, ev.Text)
		}
	}

	_ = controlplane.PostExit(context.Background(), api, token, jobID, execRes.ExitCode)

	if execErr != nil {
		return execErr
	}
	if execRes.ExitCode != 0 {
		return fmt.Errorf("engine %s exited with code %d", job.Engine, execRes.ExitCode)
	}
	return nil
}

func formatVersion() string {
	if commit == "" || commit == "unknown" {
		return "iazio-harness " + version
	}
	sha := commit
	if len(sha) > 12 {
		sha = sha[:12]
	}
	return "iazio-harness " + version + "+" + sha
}
