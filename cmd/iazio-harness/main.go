// Command iazio-harness runs one stored prompt and exits.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/viovy/iazio-harness/internal/auth"
	"github.com/viovy/iazio-harness/internal/checkpoint"
	"github.com/viovy/iazio-harness/internal/controlplane"
	"github.com/viovy/iazio-harness/internal/diagnostics"
	"github.com/viovy/iazio-harness/internal/engines"
	"github.com/viovy/iazio-harness/internal/prompt"
	"github.com/viovy/iazio-harness/internal/repolock"
	"github.com/viovy/iazio-harness/internal/service"
	"github.com/viovy/iazio-harness/internal/sessionstore"
	"github.com/viovy/iazio-harness/internal/spool"
	"github.com/viovy/iazio-harness/internal/telemetry"
	"github.com/viovy/iazio-harness/internal/watchdog"
)

const fallbackResumeStoryID = "STORY-RESUME-INPLACE"

var (
	version = "0.1.0-dev"
	commit  = "unknown"
	branch  = "unknown"
)

func main() {
	flushTracer := telemetry.InitGlobalTracer("iazio-harness", "")
	defer flushTracer()

	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: iazio-harness version|auth|run|story|prompt|codebase|status|heal-ase|service|daemon")
	}
	var spanOpts = []telemetry.SpanOption{
		telemetry.WithKind("INTERNAL"),
		telemetry.WithTag("cli.command", args[0]),
	}
	if tp := os.Getenv("TRACEPARENT"); tp != "" {
		parts := strings.Split(tp, "-")
		if len(parts) >= 3 && len(parts[1]) == 32 && len(parts[2]) == 16 {
			spanOpts = append(spanOpts, telemetry.WithParentTrace(parts[1], parts[2]))
		}
	}
	ctx, span := telemetry.StartSpan(context.Background(), "harness."+args[0], spanOpts...)
	defer span.End()
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
		return runJobWithContext(ctx, args[1:])
	case "story":
		return runStoryCommand(args[1:])
	case "prompt":
		return runPromptCommand(args[1:])
	case "codebase":
		return runCodebaseCommand(args[1:])
	case "status", "fleet-status", "inspect":
		return runFleetStatusCommand(args[1:])
	case "heal-ase":
		return runHealASECommand(args[1:])
	case "service":
		return runServiceCommand(args[1:])
	case "daemon":
		return runDaemonCommand(args[1:])
	default:
		return fmt.Errorf("unknown command")
	}
}

func runJob(args []string) error {
	return runJobWithContext(context.Background(), args)
}

func runJobWithContext(ctx context.Context, args []string) error {
	var api, jobID, worktree, docsHub, chunk, kind, resumeConv string
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
		case "--kind":
			i++
			if i < len(args) {
				kind = args[i]
			}
		case "--resume-conversation", "--conversation":
			i++
			if i < len(args) {
				resumeConv = args[i]
			}
		}
	}
	if api == "" {
		api = os.Getenv("IAZIO_HARNESS_API_URL")
	}
	if api == "" {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			if b, err := os.ReadFile(filepath.Join(home, ".iazio", "api_url")); err == nil {
				api = strings.TrimSpace(string(b))
			}
		}
	}
	if api == "" {
		api = "http://localhost:8090"
	}
	if jobID == "" {
		return fmt.Errorf("job-id is required")
	}
	if chunk != "" {
		return controlplane.PostChunk(ctx, api, "", jobID, "stdout", chunk)
	}
	return executeJob(ctx, api, jobID, worktree, docsHub, kind, resumeConv)
}

func executeJob(ctx context.Context, api, jobID, worktree, docsHub string, overrides ...string) error {
	var spanOpts = []telemetry.SpanOption{
		telemetry.WithKind("INTERNAL"),
		telemetry.WithTag("cli.command", "run"),
		telemetry.WithTag("iazio.job_id", jobID),
	}
	ctx, span := telemetry.StartSpan(ctx, "harness.execute_job", spanOpts...)
	defer span.End()

	var kindOverride, resumeConvOverride string
	if len(overrides) > 0 {
		kindOverride = overrides[0]
	}
	if len(overrides) > 1 {
		resumeConvOverride = overrides[1]
	}
	token, _ := auth.Bearer(ctx)
	job, err := controlplane.GetJob(ctx, api, token, jobID)
	if err != nil {
		_ = controlplane.PostChunk(context.Background(), api, token, jobID, "stderr", fmt.Sprintf("[iazio-harness] Pre-execution failure: get job %s: %v\n", jobID, err))
		_ = controlplane.PostExit(context.Background(), api, token, jobID, 1)
		return fmt.Errorf("get job: %w", err)
	}
	if kindOverride != "" {
		job.Kind = kindOverride
	}
	if resumeConvOverride != "" {
		job.ResumeConversationID = resumeConvOverride
	}
	if worktree == "" {
		worktree = job.WorktreePath
	}
	if docsHub == "" {
		docsHub = job.DocsHubPath
	}
	span.SetTag("iazio.story_id", job.StoryID)
	span.SetTag("iazio.kind", job.Kind)
	span.SetTag("iazio.worktree", worktree)
	span.SetTag("iazio.engine", job.Engine)
	span.SetTag("iazio.host_id", job.HostID)
	span.SetTag("iazio.schedule_id", job.ScheduleID)
	targetDir, err := engines.CheckoutDir(job.Kind, worktree, docsHub)
	if err != nil {
		_ = controlplane.PostChunk(context.Background(), api, token, jobID, "stderr", fmt.Sprintf("[iazio-harness] Pre-execution failure: checkout dir %s: %v\n", targetDir, err))
		_ = controlplane.PostExit(context.Background(), api, token, jobID, 1)
		return err
	}

	wd := watchdog.New(watchdog.Options{})
	wd.Track(watchdog.TrackedJob{
		JobID:        job.ID,
		ScheduleID:   job.ScheduleID,
		WorktreePath: targetDir,
		DocsHubPath:  docsHub,
		APIURL:       api,
		Engine:       job.Engine,
		StoryID:      job.StoryID,
		PID:          os.Getpid(),
		Status:       watchdog.StatusRunning,
	})

	lock, err := engines.Hold(targetDir, "")
	if err != nil {
		_ = controlplane.PostChunk(context.Background(), api, token, jobID, "stderr", fmt.Sprintf("[iazio-harness] Pre-execution failure: checkout lock on %s: %v\n", targetDir, err))
		_ = controlplane.PostExit(context.Background(), api, token, jobID, 1)
		return fmt.Errorf("checkout lock: %w", err)
	}
	defer lock.Release()

	storyID := job.StoryID
	if strings.TrimSpace(storyID) == "" || strings.EqualFold(storyID, fallbackResumeStoryID) || strings.EqualFold(storyID, "resume complete current ase flow") {
		dCtx := sessionstore.DefaultResolver().DetectDirtyWorktreeContext(worktree)
		if dCtx.StoryID != "" {
			storyID = dCtx.StoryID
		}
	}
	if strings.TrimSpace(storyID) == "" {
		storyID = fallbackResumeStoryID
	}

	prepRes, err := prompt.Prepare(prompt.Input{
		Kind:       job.Kind,
		JobID:      job.ID,
		Body:       job.Prompt,
		Transcript: job.Transcript,
		SpoolDir:   spool.DefaultDir(),
		Values: prompt.Values{
			StoryID:        storyID,
			TargetLeafName: filepath.Base(worktree),
			WorktreePath:   worktree,
			DocsHubPath:    docsHub,
		},
	})
	if err != nil {
		_ = controlplane.PostChunk(context.Background(), api, token, jobID, "stderr", fmt.Sprintf("[iazio-harness] Pre-execution failure: prepare prompt for job %s: %v\n", jobID, err))
		_ = controlplane.PostExit(context.Background(), api, token, jobID, 1)
		return fmt.Errorf("prepare prompt: %w", err)
	}

	sp, err := spool.Open(spool.DefaultDir(), jobID)
	if err != nil {
		_ = controlplane.PostChunk(context.Background(), api, token, jobID, "stderr", fmt.Sprintf("[iazio-harness] Pre-execution failure: open spool for job %s: %v\n", jobID, err))
		_ = controlplane.PostExit(context.Background(), api, token, jobID, 1)
		return fmt.Errorf("open spool: %w", err)
	}
	defer sp.Close()

	engineName := job.Engine
	if engineName == "" {
		engineName = "agy"
	}

	resumeConvID := job.ActiveConversationID
	if resumeConvID == "" {
		resumeConvID = job.ResumeConversationID
	}
	if resumeConvID == "" {
		resumeConvID = job.InitialConversationID
	}
	if resumeConvID == "" && job.EnvVars != nil {
		resumeConvID = job.EnvVars["RESUME_CONVERSATION_ID"]
	}
	if resumeConvID == "" && len(job.ConversationIDs) > 0 {
		resumeConvID = job.ConversationIDs[len(job.ConversationIDs)-1]
	}
	if job.Kind == "resume" {
		resolver := sessionstore.DefaultResolver()
		dCtx := resolver.DetectDirtyWorktreeContext(worktree)
		if (job.StoryID == "" || strings.EqualFold(job.StoryID, fallbackResumeStoryID) || strings.EqualFold(job.StoryID, "resume complete current ase flow")) && dCtx.StoryID != "" {
			job.StoryID = dCtx.StoryID
		}
		if resumeConvID == "" {
			if dCtx.ConversationID != "" {
				resumeConvID = dCtx.ConversationID
				log.Printf("[harness] Discovered prior conversation %s from dirty context (review: %s, verdict: %s)", resumeConvID, dCtx.ReviewFile, dCtx.Verdict)
			} else if job.StoryID != "" {
				if sess, err := resolver.ResolveForStory(worktree, job.StoryID); err == nil && sess != nil {
					resumeConvID = sess.ConversationID
				}
			} else {
				if sess, err := resolver.ResolveLatest(worktree); err == nil && sess != nil {
					resumeConvID = sess.ConversationID
				}
			}
		}
		if resumeConvID != "" {
			lowConv := strings.ToLower(resumeConvID)
			if !strings.HasPrefix(lowConv, "cursor-") && engineName == "agent" {
				engineName = "agy"
				log.Printf("[harness] Auto-corrected engine from agent to agy for session %s", resumeConvID)
			}
			log.Printf("[harness] Auto-discovered prior conversation %s (story: %s, engine: %s) for worktree %s", resumeConvID, job.StoryID, engineName, worktree)
		}
	}
	lockHandle, lockErr := repolock.Acquire(worktree, repolock.LockInfo{
		JobID:          job.ID,
		ScheduleID:     job.ScheduleID,
		PID:            os.Getpid(),
		Engine:         engineName,
		ConversationID: resumeConvID,
		StoryID:        job.StoryID,
		Prompt:         job.Prompt,
		Status:         "RUNNING",
	}, func(pid int) bool {
		if pid <= 0 {
			return false
		}
		p, err := os.FindProcess(pid)
		if err != nil {
			return false
		}
		return p.Signal(syscall.Signal(0)) == nil
	})
	if lockErr != nil {
		_ = sp.AppendRaw("stderr", []byte(fmt.Sprintf("[iazio-harness] Repository lock failure: %v\n", lockErr)))
		_ = controlplane.PostExit(context.Background(), api, token, jobID, 1)
		return fmt.Errorf("repolock: %w", lockErr)
	}
	defer lockHandle.Release()

	if resumeConvID == "" && lockHandle.Info().ConversationID != "" && (job.Kind == "resume" || (job.StoryID != "" && lockHandle.Info().StoryID == job.StoryID && lockHandle.Info().Status != "COMPLETED")) {
		resumeConvID = lockHandle.Info().ConversationID
		log.Printf("[harness] Adopted prior conversation %s from repo lock for %s", resumeConvID, worktree)
	}

	isPreflightFallback := false
	preflightFallbackReason := ""
	attemptedResumeConv := resumeConvID
	if resumeConvID != "" {
		if !sessionstore.IsValidConversationID(resumeConvID) {
			isPreflightFallback = true
			preflightFallbackReason = fmt.Sprintf("invalid initial conversation id format: %s", resumeConvID)
			log.Printf("[harness] [job.resume.conversation_fallback] initial_conversation_id=%s reason=%q", resumeConvID, preflightFallbackReason)
			resumeConvID = ""
		} else if engineName == "agy" && !sessionstore.DefaultResolver().SessionExists(resumeConvID) {
			isPreflightFallback = true
			preflightFallbackReason = fmt.Sprintf("initial conversation %s does not exist on disk on host for engine agy", resumeConvID)
			log.Printf("[harness] [job.resume.conversation_fallback] initial_conversation_id=%s reason=%q", resumeConvID, preflightFallbackReason)
			resumeConvID = ""
		}
	}

	if isPreflightFallback && job.InterruptedASEState != nil && len(job.InterruptedASEState) > 0 {
		resumeDirective := fmt.Sprintf("\n[ASE RESUME DIRECTIVE: Prior execution on conversation %s interrupted. Fallback to new conversation. Interrupted State: phase=%v, step=%v, story_id=%v, gate=%v. Resume execution from this checkpoint.]\n\n",
			attemptedResumeConv,
			job.InterruptedASEState["phase"],
			job.InterruptedASEState["step"],
			job.InterruptedASEState["story_id"],
			job.InterruptedASEState["last_completed_gate"],
		)
		prepRes.Prompt = resumeDirective + prepRes.Prompt
	}

	var activePID int
	activeConvID := resumeConvID

	// Immediately post startup diagnostic chunk so UI never sits on blank "Waiting for output..."
	_ = sp.AppendRaw("stdout", []byte(fmt.Sprintf("[iazio-harness] Initializing execution for job %s on %s (engine: %s, kind: %s)\n", jobID, worktree, engineName, job.Kind)))
	if resumeConvID != "" {
		_ = sp.AppendRaw("stdout", []byte(fmt.Sprintf("[iazio-harness] Resuming execution: conversation %s (story: %s, engine: %s)\n", resumeConvID, job.StoryID, engineName)))
	}
	_ = checkpoint.Save(worktree, spool.DefaultDir(), checkpoint.State{
		JobID:          job.ID,
		ScheduleID:     job.ScheduleID,
		Engine:         engineName,
		WorktreePath:   worktree,
		ConversationID: activeConvID,
		StoryID:        job.StoryID,
		StepIndex:      0,
		StartedAt:      time.Now().UTC().Format(time.RFC3339),
		Status:         "RUNNING",
	})

	startTime := time.Now()
	flushCtx, stopFlush := context.WithCancel(ctx)
	defer stopFlush()
	go func() {
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()
		var lastTickSent time.Time
		var lastSilenceWarning time.Time
		for {
			select {
			case <-flushCtx.Done():
				return
			case <-ticker.C:
				events := sp.Flush()
				now := time.Now()
				for _, ev := range events {
					if ev.Type == spool.EventOutputChunk {
						if err := controlplane.PostChunk(context.Background(), api, token, jobID, ev.Stream, ev.Text); err != nil {
							log.Printf("[harness] post chunk failed for %s: %v", jobID, err)
						} else {
							lastTickSent = now
						}
					} else if ev.Type == spool.EventOutputTick {
						if now.Sub(lastTickSent) >= 5*time.Second {
							elapsedSec := int(time.Since(startTime).Seconds())
							if err := controlplane.PostTick(context.Background(), api, token, jobID, ev.SilentForMs, os.Getpid(), activePID, elapsedSec); err != nil {
								log.Printf("[harness] post tick failed for %s: %v", jobID, err)
							} else {
								lastTickSent = now
							}
						}
						// If the engine has been silent for >= 30s, emit an explicit diagnostic chunk
						// so the user in iazio-web can see the process is alive and waiting for model response
						if ev.SilentForMs >= 30000 && now.Sub(lastSilenceWarning) >= 30*time.Second {
							lastSilenceWarning = now
							harnessPID := os.Getpid()
							elapsedStr := time.Since(startTime).Truncate(time.Second).String()
							diag := fmt.Sprintf("[iazio-harness] Execution active (job %s, engine: %s, harness_pid: %d, tool_pid: %d, elapsed: %s); silent for %ds (waiting for model output/response)...\n",
								jobID, engineName, harnessPID, activePID, elapsedStr, ev.SilentForMs/1000)
							_ = controlplane.PostChunk(context.Background(), api, token, jobID, "stdout", diag)
						}
					}
				}
			}
		}
	}()

	printTimeout := "0"
	if job.MaxExecutionDurationSeconds > 0 {
		printTimeout = fmt.Sprintf("%ds", job.MaxExecutionDurationSeconds)
	}
	modelName, agentName, disableSlashCommands := resolveExecutionOptions(job, prepRes.Prompt)
	runner := engines.CLI{Engine: engineName}

	var convWg sync.WaitGroup
	var convMu sync.Mutex
	var confirmedConvID string
	var execReq engines.ExecutionRequest

	execReq = engines.ExecutionRequest{
		Engine:                     engineName,
		Kind:                       job.Kind,
		StoryID:                    sessionstore.ExtractStoryID(storyID),
		Prompt:                     prepRes.Prompt,
		WorktreePath:               worktree,
		DocsHubPath:                docsHub,
		PrintTimeout:               printTimeout,
		Model:                      modelName,
		Agent:                      agentName,
		DisableSlashCommands:       disableSlashCommands,
		DangerouslySkipPermissions: true,
		ScheduleEnv:                job.EnvVars,
		LogFile:                    filepath.Join(spool.DefaultDir(), jobID+".engine.log"),
		ResumeConversationID:       resumeConvID,
		OnPID: func(pid int) {
			activePID = pid
			span.AddAnnotation(fmt.Sprintf("engine_process_started:pid=%d", pid))
			wd.Track(watchdog.TrackedJob{
				JobID:          job.ID,
				ScheduleID:     job.ScheduleID,
				WorktreePath:   targetDir,
				DocsHubPath:    docsHub,
				APIURL:         api,
				Engine:         engineName,
				StoryID:        job.StoryID,
				ConversationID: activeConvID,
				PID:            pid,
				Status:         watchdog.StatusRunning,
			})
			_ = lockHandle.Update(repolock.LockInfo{
				JobID:          job.ID,
				ScheduleID:     job.ScheduleID,
				PID:            os.Getpid(),
				HarnessPID:     pid,
				Engine:         engineName,
				ConversationID: activeConvID,
				StoryID:        job.StoryID,
				Prompt:         job.Prompt,
				Status:         "RUNNING",
			})
			_ = checkpoint.Save(worktree, spool.DefaultDir(), checkpoint.State{
				JobID:          job.ID,
				ScheduleID:     job.ScheduleID,
				Engine:         engineName,
				WorktreePath:   worktree,
				ConversationID: activeConvID,
				StoryID:        job.StoryID,
				StepIndex:      1,
				PID:            pid,
				StartedAt:      time.Now().UTC().Format(time.RFC3339),
				Status:         "RUNNING",
			})
		},
		OnConversationID: func(cid string) {
			convMu.Lock()
			defer convMu.Unlock()

			if cid == "" || !sessionstore.IsValidConversationID(cid) {
				return
			}

			// Strict pinning: once a conversation ID has been confirmed and pinned for this job execution,
			// any differing subsequent conversation ID is an unpinned tertiary leak and must be rejected!
			if confirmedConvID != "" {
				if cid != confirmedConvID {
					log.Printf("[harness] ignoring unpinned tertiary conversation %s; keeping active conversation %s pinned for job %s", cid, confirmedConvID, jobID)
					go func(droppedID, pinnedID string) {
						_ = controlplane.PostConversationWithOptions(context.Background(), api, token, jobID, droppedID, controlplane.PostConversationOptions{
							Role:           "dropped_unpinned",
							FallbackReason: fmt.Sprintf("rejected tertiary conversation %s; active conversation %s is already pinned", droppedID, pinnedID),
						})
					}(cid, confirmedConvID)
				}
				return
			}

			isFallback := false
			fallbackReason := ""
			role := "initial_active"
			if resumeConvID != "" {
				if cid == resumeConvID {
					role = "initial_resumed"
				} else {
					isFallback = true
					role = "fallback_primary"
					fallbackReason = fmt.Sprintf("failed to attach to initial conversation %s; started new conversation %s", resumeConvID, cid)
					log.Printf("[harness] [job.resume.conversation_fallback] initial_conversation_id=%s active_conversation_id=%s reason=%q", resumeConvID, cid, fallbackReason)
				}
			} else if isPreflightFallback {
				isFallback = true
				role = "fallback_primary"
				fallbackReason = preflightFallbackReason
				log.Printf("[harness] [job.resume.conversation_fallback] initial_conversation_id=%s active_conversation_id=%s reason=%q", job.InitialConversationID, cid, fallbackReason)
			}

			confirmedConvID = cid
			activeConvID = cid

			span.AddAnnotation(fmt.Sprintf("conversation_detected:%s:fallback=%v:role=%s", cid, isFallback, role))
			_ = lockHandle.Update(repolock.LockInfo{
				JobID:          job.ID,
				ScheduleID:     job.ScheduleID,
				PID:            os.Getpid(),
				HarnessPID:     activePID,
				Engine:         engineName,
				ConversationID: cid,
				StoryID:        job.StoryID,
				Prompt:         job.Prompt,
				Status:         "RUNNING",
			})
			_ = checkpoint.Save(worktree, spool.DefaultDir(), checkpoint.State{
				JobID:          job.ID,
				ScheduleID:     job.ScheduleID,
				Engine:         engineName,
				WorktreePath:   worktree,
				ConversationID: cid,
				StoryID:        job.StoryID,
				StepIndex:      2,
				PID:            activePID,
				StartedAt:      time.Now().UTC().Format(time.RFC3339),
				Status:         "RUNNING",
			})
			convWg.Add(1)
			asePhase := ""
			if job.InterruptedASEState != nil {
				if p, ok := job.InterruptedASEState["phase"].(string); ok {
					asePhase = p
				}
			}
			go func(c string, fb bool, fbReason, r, phase string) {
				defer convWg.Done()
				var err error
				opt := controlplane.PostConversationOptions{
					Fallback:       fb,
					FallbackReason: fbReason,
					Role:           r,
					CLIArgs:        fmt.Sprintf("--conversation=%s", c),
					ASEPhase:       phase,
				}
				for attempt := 1; attempt <= 10; attempt++ {
					err = controlplane.PostConversationWithOptions(context.Background(), api, token, jobID, c, opt)
					if err == nil {
						return
					}
					time.Sleep(time.Duration(attempt*100) * time.Millisecond)
				}
				if err != nil {
					log.Printf("warning: failed to post conversation ID %s for job %s: %v", c, jobID, err)
				}
			}(cid, isFallback, fallbackReason, role, asePhase)
		},
	}
	execRes, execErr := runner.Execute(ctx, execReq, sp)
	span.AddAnnotation(fmt.Sprintf("engine_process_exited:code=%d", execRes.ExitCode))
	stopFlush()

	// Wait for any in-flight conversation ID registrations with a bounded timeout
	convDone := make(chan struct{})
	go func() {
		convWg.Wait()
		close(convDone)
	}()
	select {
	case <-convDone:
	case <-time.After(3 * time.Second):
	}

	// Final spool flush
	for _, ev := range sp.Flush() {
		if ev.Type == spool.EventOutputChunk {
			_ = controlplane.PostChunk(context.Background(), api, token, jobID, ev.Stream, ev.Text)
		}
	}

	// Execution Diagnosis & Anomaly Detection
	_ = sp.Sync()
	engineLogData, _ := os.ReadFile(execReq.LogFile)
	spoolLogData, _ := os.ReadFile(filepath.Join(spool.DefaultDir(), job.ID+".log"))
	combinedOutput := string(engineLogData) + "\n" + string(spoolLogData)

	diagReport := diagnostics.Diagnose(diagnostics.DiagnosticInput{
		JobID:        job.ID,
		StoryID:      job.StoryID,
		WorktreePath: worktree,
		ExitCode:     execRes.ExitCode,
		OutputText:   combinedOutput,
	})

	convMu.Lock()
	sessionConfirmed := (confirmedConvID == resumeConvID)
	convMu.Unlock()

	// Inspect engine execution log/stderr specifically (never LLM agent output or transcript) for session attach errors
	engineLogStr := strings.ToLower(string(engineLogData))
	hasEngineSessionError := strings.Contains(engineLogStr, "conversation not found") ||
		strings.Contains(engineLogStr, "session not found") ||
		strings.Contains(engineLogStr, "failed to open session") ||
		strings.Contains(engineLogStr, "failed to attach to conversation") ||
		strings.Contains(engineLogStr, "failed to attach to session") ||
		strings.Contains(engineLogStr, "corrupted session database") ||
		strings.Contains(engineLogStr, "no session with id") ||
		strings.Contains(engineLogStr, "unknown conversation") ||
		strings.Contains(engineLogStr, "invalid session id") ||
		strings.Contains(engineLogStr, "cannot find conversation") ||
		strings.Contains(engineLogStr, "cannot find session") ||
		strings.Contains(engineLogStr, "cannot load session") ||
		strings.Contains(engineLogStr, "session file missing") ||
		strings.Contains(engineLogStr, "no conversation found") ||
		strings.Contains(engineLogStr, "conversation file does not exist") ||
		strings.Contains(engineLogStr, "session does not exist") ||
		strings.Contains(engineLogStr, "conversation does not exist")

	isQuotaExhausted := false
	for _, f := range diagReport.Findings {
		if f.Code == diagnostics.CodeQuotaExhausted {
			isQuotaExhausted = true
			break
		}
	}
	combinedLower := strings.ToLower(combinedOutput)
	if !isQuotaExhausted && (strings.Contains(engineLogStr, "actionrequirederror") || strings.Contains(engineLogStr, "out of usage") || strings.Contains(combinedLower, "actionrequirederror") || strings.Contains(combinedLower, "out of usage")) {
		isQuotaExhausted = true
	}

	// Stage 2 fallback ONLY triggers if the engine actively failed to attach to resumeConvID.
	// Normal task failures (lint, tests, build) inside the session must NEVER trigger Stage 2 fallback (F1, F2).
	isSessionAttachFailure := !sessionConfirmed && hasEngineSessionError && !isQuotaExhausted

	// Check for conversation resume failure auto-fallback (Stage 2 Fallback)
	// ONLY trigger if session could not be attached or unrecoverable session error occurred (F2)
	if resumeConvID != "" && execRes.ExitCode != 0 && ctx.Err() == nil && isSessionAttachFailure {
		fallbackReason := fmt.Sprintf("tool execution failed to resume on conversation %s (exit code %d)", resumeConvID, execRes.ExitCode)
		fallbackMsg := fmt.Sprintf("[iazio-harness] [job.resume.conversation_fallback] %s; activating Stage 2 fallback: starting new conversation...\n", fallbackReason)
		_ = sp.AppendRaw("stdout", []byte(fallbackMsg))
		_ = controlplane.PostChunk(context.Background(), api, token, jobID, "stdout", fallbackMsg)

		fallbackReq := execReq
		fallbackReq.ResumeConversationID = "" // clear to start a new conversation
		fallbackReq.Argv = nil

		// Reset confirmed conversation so the new fallback conversation can be pinned
		convMu.Lock()
		confirmedConvID = ""
		isPreflightFallback = true
		preflightFallbackReason = fallbackReason
		convMu.Unlock()

		if job.InterruptedASEState != nil && len(job.InterruptedASEState) > 0 {
			resumeDirective := fmt.Sprintf("\n[ASE RESUME DIRECTIVE: Prior execution on conversation %s failed with exit code %d. Fallback to new conversation. Interrupted State: phase=%v, step=%v, story_id=%v, gate=%v. Resume execution from this checkpoint.]\n\n",
				resumeConvID,
				execRes.ExitCode,
				job.InterruptedASEState["phase"],
				job.InterruptedASEState["step"],
				job.InterruptedASEState["story_id"],
				job.InterruptedASEState["last_completed_gate"],
			)
			fallbackReq.Prompt = resumeDirective + fallbackReq.Prompt
		}

		execRes, execErr = runner.Execute(ctx, fallbackReq, sp)

		// Re-flush spool after fallback
		for _, ev := range sp.Flush() {
			if ev.Type == spool.EventOutputChunk {
				_ = controlplane.PostChunk(context.Background(), api, token, jobID, ev.Stream, ev.Text)
			}
		}

		// Wait for any in-flight conversation registration from the fallback run
		fallbackConvDone := make(chan struct{})
		go func() {
			convWg.Wait()
			close(fallbackConvDone)
		}()
		select {
		case <-fallbackConvDone:
		case <-time.After(3 * time.Second):
		}

		// Re-diagnose after fallback run
		_ = sp.Sync()
		engineLogData, _ = os.ReadFile(fallbackReq.LogFile)
		spoolLogData, _ = os.ReadFile(filepath.Join(spool.DefaultDir(), job.ID+".log"))
		combinedOutput = string(engineLogData) + "\n" + string(spoolLogData)
		diagReport = diagnostics.Diagnose(diagnostics.DiagnosticInput{
			JobID:        job.ID,
			StoryID:      job.StoryID,
			WorktreePath: worktree,
			ExitCode:     execRes.ExitCode,
			OutputText:   combinedOutput,
		})
	}

	// Check for quota exhaustion auto-fallback
	isQuotaExhausted = false
	for _, f := range diagReport.Findings {
		if f.Code == diagnostics.CodeQuotaExhausted {
			isQuotaExhausted = true
			break
		}
	}
	if !isQuotaExhausted && (strings.Contains(combinedOutput, "ActionRequiredError") || strings.Contains(combinedOutput, "out of usage")) {
		isQuotaExhausted = true
	}

	if isQuotaExhausted && engineName == "agent" && ctx.Err() == nil {
		fallbackEngine := "agy"
		fallbackModel := "gemini-3.8-flash-high"
		fallbackMsg := fmt.Sprintf("[iazio-harness] Quota/usage limit exhausted on engine 'agent' (Cursor CLI): ActionRequiredError: You're out of usage.\n[iazio-harness] Activating auto-engine fallback: switching to '%s' (model: %s) and re-executing prompt in-place...\n", fallbackEngine, fallbackModel)
		_ = sp.AppendRaw("stdout", []byte(fallbackMsg))
		_ = controlplane.PostChunk(context.Background(), api, token, jobID, "stdout", fallbackMsg)

		fallbackReq := execReq
		fallbackReq.Engine = fallbackEngine
		fallbackReq.Model = fallbackModel
		fallbackReq.Argv = nil
		if fallbackReq.ResumeConversationID != "" && strings.HasPrefix(strings.ToLower(fallbackReq.ResumeConversationID), "cursor-") {
			fallbackReq.ResumeConversationID = ""
		}
		fallbackRunner := engines.CLI{Engine: fallbackEngine}
		if checkErr := fallbackRunner.HealthCheck(); checkErr == nil {
			log.Printf("[harness] Auto-engine fallback: switching from agent to agy for job %s", jobID)
			engineName = fallbackEngine
			modelName = fallbackModel
			execRes, execErr = fallbackRunner.Execute(ctx, fallbackReq, sp)

			// Re-flush spool after fallback
			for _, ev := range sp.Flush() {
				if ev.Type == spool.EventOutputChunk {
					_ = controlplane.PostChunk(context.Background(), api, token, jobID, ev.Stream, ev.Text)
				}
			}

			// Re-diagnose after fallback run
			_ = sp.Sync()
			engineLogData, _ = os.ReadFile(fallbackReq.LogFile)
			spoolLogData, _ = os.ReadFile(filepath.Join(spool.DefaultDir(), job.ID+".log"))
			combinedOutput = string(engineLogData) + "\n" + string(spoolLogData)
			diagReport = diagnostics.Diagnose(diagnostics.DiagnosticInput{
				JobID:        job.ID,
				StoryID:      job.StoryID,
				WorktreePath: worktree,
				ExitCode:     execRes.ExitCode,
				OutputText:   combinedOutput,
			})
		} else {
			warnMsg := fmt.Sprintf("[iazio-harness] Auto-fallback engine '%s' is not installed on host PATH: %v\n", fallbackEngine, checkErr)
			_ = sp.AppendRaw("stdout", []byte(warnMsg))
			_ = controlplane.PostChunk(context.Background(), api, token, jobID, "stdout", warnMsg)
		}
	}

	if len(diagReport.Findings) > 0 {
		for _, f := range diagReport.Findings {
			diagMsg := fmt.Sprintf("[iazio-harness] Anomaly detected: [%s] (%s): %s Remediation: %s\n", f.Code, f.Severity, f.Message, f.Remediation)
			_ = sp.AppendRaw("stdout", []byte(diagMsg))
			_ = controlplane.PostChunk(context.Background(), api, token, jobID, "stdout", diagMsg)
		}
		if diagReport.SIR != nil {
			sirMsg := fmt.Sprintf("[iazio-harness] Emitted Structured Intervention Request (SIR): %s (category: %s, auto-healable: %v)\n", diagReport.SIR.ID, diagReport.SIR.Category, diagReport.SIR.AutoHealable)
			_ = sp.AppendRaw("stdout", []byte(sirMsg))
			_ = controlplane.PostChunk(context.Background(), api, token, jobID, "stdout", sirMsg)
			if diagReport.SIRPath != "" {
				persMsg := fmt.Sprintf("[iazio-harness] SIR report persisted to: %s\n", diagReport.SIRPath)
				_ = sp.AppendRaw("stdout", []byte(persMsg))
				_ = controlplane.PostChunk(context.Background(), api, token, jobID, "stdout", persMsg)
			}
		}
		summaryText := "\n" + diagReport.TroubleshootingSummary() + "\n"
		_ = sp.AppendRaw("stdout", []byte(summaryText))
		_ = controlplane.PostChunk(context.Background(), api, token, jobID, "stdout", summaryText)
	}
	if diagReport.ASEComplete && diagReport.Healthy && execRes.ExitCode != 0 {
		overMsg := fmt.Sprintf("[iazio-harness] ASE verified complete on origin/main with clean worktree. Reconciling exit code %d to 0 (COMPLETED).\n", execRes.ExitCode)
		_ = sp.AppendRaw("stdout", []byte(overMsg))
		_ = controlplane.PostChunk(context.Background(), api, token, jobID, "stdout", overMsg)
		execRes.ExitCode = 0
		execErr = nil
	} else if !diagReport.Healthy && execRes.ExitCode == 0 {
		execRes.ExitCode = 1
	}

	// Save terminal execution checkpoint
	finalStatus := "COMPLETED"
	if execErr != nil || execRes.ExitCode != 0 {
		finalStatus = "FAILED"
	}
	_ = checkpoint.Save(worktree, spool.DefaultDir(), checkpoint.State{
		JobID:          job.ID,
		ScheduleID:     job.ScheduleID,
		Engine:         engineName,
		WorktreePath:   worktree,
		ConversationID: activeConvID,
		StoryID:        job.StoryID,
		StepIndex:      3,
		StartedAt:      time.Now().UTC().Format(time.RFC3339),
		Status:         finalStatus,
		ExitCode:       execRes.ExitCode,
		ASEComplete:    diagReport.ASEComplete,
	})

	// Explicitly release checkout file lock before posting exit to control plane,
	// ensuring subsequent jobs can immediately acquire the lock without race or contention.
	_ = lock.Release()

	exitOpts := controlplane.ExitOptions{
		ExitCode:    execRes.ExitCode,
		Engine:      engineName,
		Model:       modelName,
		ASEComplete: diagReport.ASEComplete,
	}
	if execRes.ExitCode != 0 || !diagReport.Healthy {
		if len(diagReport.Findings) > 0 {
			f := diagReport.Findings[0]
			exitOpts.DiagnosticCode = string(f.Code)
			exitOpts.Reason = string(f.Code)
			exitOpts.ErrorMessage = f.Message
			exitOpts.Remediation = f.Remediation
		}
		for _, line := range strings.Split(combinedOutput, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.Contains(trimmed, "ActionRequiredError") || strings.Contains(trimmed, "out of usage") {
				exitOpts.ErrorMessage = trimmed
				exitOpts.DiagnosticCode = string(diagnostics.CodeQuotaExhausted)
				exitOpts.Reason = string(diagnostics.CodeQuotaExhausted)
				exitOpts.Remediation = "Switch execution engine (e.g. to agy with Gemini Flash / Auto model) or increase model usage limit."
				break
			}
		}
		if exitOpts.Reason == "" {
			exitOpts.Reason = "NON_ZERO_EXIT"
		}
	}

	span.SetTag("iazio.status", finalStatus)
	span.SetTag("iazio.exit_code", fmt.Sprintf("%d", execRes.ExitCode))
	if exitOpts.DiagnosticCode != "" {
		span.SetTag("iazio.diagnostic_code", exitOpts.DiagnosticCode)
	}
	if exitOpts.Reason != "" {
		span.SetTag("iazio.reason", exitOpts.Reason)
	}
	if execErr != nil {
		span.SetError(execErr)
	}

	var postExitErr error
	for attempt := 1; attempt <= 3; attempt++ {
		postExitErr = controlplane.PostExitWithOptions(context.Background(), api, token, jobID, exitOpts)
		if postExitErr == nil {
			break
		}
		time.Sleep(time.Duration(attempt*250) * time.Millisecond)
	}
	if postExitErr != nil {
		fmt.Fprintf(os.Stderr, "warning: post exit failed for job %s: %v\n", jobID, postExitErr)
	} else {
		span.AddAnnotation("post_exit_complete")
	}

	if finalStatus == "COMPLETED" {
		wd.Complete(job.ID, execRes.ExitCode)
	} else {
		errMsg := ""
		if execErr != nil {
			errMsg = execErr.Error()
		} else if exitOpts.ErrorMessage != "" {
			errMsg = exitOpts.ErrorMessage
		}
		wd.Fail(job.ID, execRes.ExitCode, errMsg)
	}

	if execErr != nil {
		return execErr
	}
	if execRes.ExitCode != 0 {
		return fmt.Errorf("engine %s exited with code %d", engineName, execRes.ExitCode)
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

func resolveExecutionOptions(job controlplane.JobDetail, promptBody string) (model string, agent string, disableSlash bool) {
	model = job.Model
	if model == "" && job.EnvVars != nil {
		model = job.EnvVars["MODEL"]
	}
	if model == "" {
		model = "gemini-3.8-flash-high"
	}

	agent = job.Agent
	if agent == "" && job.EnvVars != nil {
		agent = job.EnvVars["AGENT"]
	}

	disableSlash = true
	trimmedPrompt := strings.TrimSpace(promptBody)
	if job.DisableSlashCommands != nil {
		disableSlash = *job.DisableSlashCommands
	} else if strings.HasPrefix(trimmedPrompt, "/") {
		disableSlash = false
	} else if job.EnvVars != nil {
		if strings.ToLower(job.EnvVars["ENABLE_SLASH_COMMANDS"]) == "true" ||
			strings.ToLower(job.EnvVars["DISABLE_SLASH_COMMANDS"]) == "false" {
			disableSlash = false
		}
	}
	return
}

func runFleetStatusCommand(args []string) error {
	repoPath := "."
	var jsonOutput bool
	for i := 0; i < len(args); i++ {
		switch {
		case (args[i] == "--repo" || args[i] == "--worktree-path") && i+1 < len(args):
			i++
			repoPath = args[i]
		case strings.HasPrefix(args[i], "--repo="):
			repoPath = strings.TrimPrefix(args[i], "--repo=")
		case strings.HasPrefix(args[i], "--worktree-path="):
			repoPath = strings.TrimPrefix(args[i], "--worktree-path=")
		case args[i] == "--json":
			jsonOutput = true
		}
	}
	res, err := diagnostics.InspectRepoFleetStatus(context.Background(), repoPath)
	if err != nil {
		return err
	}
	if jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(res)
	}
	fmt.Print(res.DiagnosticSummary)
	return nil
}

func runHealASECommand(args []string) error {
	repoPath := "."
	for i := 0; i < len(args); i++ {
		switch {
		case (args[i] == "--repo" || args[i] == "--worktree-path") && i+1 < len(args):
			i++
			repoPath = args[i]
		case strings.HasPrefix(args[i], "--repo="):
			repoPath = strings.TrimPrefix(args[i], "--repo=")
		case strings.HasPrefix(args[i], "--worktree-path="):
			repoPath = strings.TrimPrefix(args[i], "--worktree-path=")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := diagnostics.ReapOrphanedProcesses(ctx, repoPath); err != nil {
		fmt.Fprintf(os.Stderr, "warning: reap orphaned processes: %v\n", err)
	}
	if err := diagnostics.ResetASELocks(repoPath); err != nil {
		return fmt.Errorf("reset ase locks: %w", err)
	}
	fmt.Printf("Successfully healed ASE state and reaped orphaned processes for %s\n", repoPath)
	return nil
}

func runServiceCommand(args []string) error {
	action := "status"
	force := false
	dry := false
	var extraArgs []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--dry-run":
			dry = true
		case "--force", "-f":
			force = true
		default:
			if !strings.HasPrefix(args[i], "-") && action == "status" {
				action = args[i]
			} else {
				extraArgs = append(extraArgs, args[i])
			}
		}
	}

	bin, err := os.Executable()
	if err != nil {
		bin = "iazio-harness"
	} else if resolved, err := filepath.EvalSymlinks(bin); err == nil {
		bin = resolved
	}

	home, _ := os.UserHomeDir()
	goos := runtime.GOOS

	switch action {
	case "install":
		if dry {
			fmt.Println(service.ActionText(goos, action, bin, force))
			return nil
		}
		switch goos {
		case "darwin":
			return service.InstallDarwin(bin)
		case "windows":
			return service.InstallWindows(bin)
		default:
			if service.IsSystemdAvailable() {
				return service.InstallLinux(bin)
			}
			return service.InstallUserDaemon(bin)
		}
	case "dev-install":
		src := bin
		if len(extraArgs) > 0 {
			src = extraArgs[0]
		}
		if dry {
			fmt.Printf("hot-swap %s and restart supervisor\n", src)
			return nil
		}
		if err := service.DevInstall(src, home, nil); err != nil {
			return err
		}
		fmt.Println("iazio-harness dev version installed and supervisor active")
		return nil
	case "pause":
		if dry {
			fmt.Println("pause supervisor and place maintenance lock")
			return nil
		}
		if err := service.PauseService(home, nil); err != nil {
			return err
		}
		fmt.Println("iazio-harness service paused (maintenance mode active)")
		return nil
	case "resume":
		if dry {
			fmt.Println("resume supervisor and remove maintenance lock")
			return nil
		}
		if err := service.ResumeService(home, nil); err != nil {
			return err
		}
		fmt.Println("iazio-harness service resumed (maintenance mode cleared)")
		return nil
	case "revive-check":
		if dry {
			fmt.Println("check supervisor health and auto-revive if inactive")
			return nil
		}
		return service.ReviveCheck(home, nil)
	case "start", "stop", "restart", "uninstall":
		if dry {
			fmt.Println(service.ActionText(goos, action, bin, force))
			return nil
		}
		if goos == "linux" && !service.IsSystemdAvailable() {
			switch action {
			case "start":
				return service.StartUserDaemonAt(home, bin, nil)
			case "stop":
				return service.StopUserDaemonAt(home)
			case "restart":
				_ = service.StopUserDaemonAt(home)
				return service.StartUserDaemonAt(home, bin, nil)
			case "uninstall":
				return service.UninstallUserDaemonAt(home)
			}
		}
		_, err := service.Apply(goos, action, bin, false, force, nil)
		return err
	case "status":
		if goos == "linux" && !service.IsSystemdAvailable() {
			running, pid := service.StatusUserDaemonAt(home)
			if running {
				fmt.Printf("running (pid: %d)\n", pid)
				return nil
			}
			fmt.Println("installed but not running")
			return nil
		}
		txt := service.ActionText(goos, action, bin, force)
		fmt.Println(txt)
		return nil
	default:
		return fmt.Errorf("unknown service action %s", action)
	}
}

func runDaemonCommand(args []string) error {
	interval := watchdog.DefaultInterval
	for i := 0; i < len(args); i++ {
		if (args[i] == "--interval" || args[i] == "-i") && i+1 < len(args) {
			i++
			if d, err := time.ParseDuration(args[i]); err == nil {
				interval = d
			}
		}
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	log.Printf("[iazio-harness] Starting execution supervisor daemon (pid: %d, interval: %v)...", os.Getpid(), interval)
	wd := watchdog.New(watchdog.Options{})
	return wd.Run(ctx, interval)
}
