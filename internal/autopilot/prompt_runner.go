// Package autopilot provides integration with the autopilot CLI, story governance, and prompt execution.
package autopilot

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/viovy/iazio-harness/internal/engines"
	"github.com/viovy/iazio-harness/internal/redact"
)

var storyIDRegex = regexp.MustCompile(`\b(STORY-[A-Za-z0-9_-]+)\b`)

const (
	defaultPromptTimeout = 900 * time.Second
	defaultGracePeriod   = 5 * time.Second
)

// RunPromptOptions specifies parameters for executing a prompt via autopilot.
type RunPromptOptions struct {
	PromptID    string        `json:"prompt_id"`
	StoryID     string        `json:"story_id,omitempty"`
	Repo        string        `json:"repo"`
	Title       string        `json:"title,omitempty"`
	Prompt      string        `json:"prompt"`
	TargetAgent string        `json:"target_agent,omitempty"`
	WorkingDir  string        `json:"working_dir"`
	TraceID     string        `json:"trace_id,omitempty"`
	Timeout     time.Duration `json:"timeout,omitempty"`
	GracePeriod time.Duration `json:"grace_period,omitempty"`
}

// PromptResult represents the structured telemetry returned after prompt execution.
type PromptResult struct {
	PromptID      string   `json:"prompt_id"`
	Repo          string   `json:"repo"`
	TargetAgent   string   `json:"target_agent"`
	StoryID       string   `json:"story_id,omitempty"`
	StoryTitle    string   `json:"story_title,omitempty"`
	StoryPath     string   `json:"story_path,omitempty"`
	Status        string   `json:"status"`
	ExitCode      int      `json:"exit_code"`
	GitHeadSHA    string   `json:"git_head_sha"`
	GitTreeSHA    string   `json:"git_tree_sha"`
	ModifiedFiles []string `json:"modified_files"`
	Summary       string   `json:"summary"`
	DurationMs    int64    `json:"duration_ms"`
}

// PromptRunner defines the interface for executing prompts in a submodule directory.
type PromptRunner interface {
	RunPrompt(ctx context.Context, opts RunPromptOptions) (*PromptResult, error)
}

// CLIPromptRunner delegates prompt execution to the autopilot binary on the host.
type CLIPromptRunner struct {
	AutopilotBinary string
	GitBinary       string
	GracePeriod     time.Duration
}

// NewPromptRunner creates a CLIPromptRunner.
func NewPromptRunner(autopilotBin string) *CLIPromptRunner {
	if autopilotBin == "" {
		if env := os.Getenv("AUTOPILOT_BIN"); env != "" {
			autopilotBin = env
		} else {
			autopilotBin = "autopilot"
		}
	}
	return &CLIPromptRunner{
		AutopilotBinary: autopilotBin,
		GitBinary:       "git",
		GracePeriod:     defaultGracePeriod,
	}
}

// RunPrompt executes the prompt inside opts.WorkingDir using autopilot.
func (r *CLIPromptRunner) RunPrompt(ctx context.Context, opts RunPromptOptions) (*PromptResult, error) {
	if strings.TrimSpace(opts.Prompt) == "" {
		return nil, errors.New("prompt is required")
	}
	if strings.TrimSpace(opts.WorkingDir) == "" {
		return nil, errors.New("working directory is required")
	}

	targetAgent := opts.TargetAgent
	if targetAgent == "" {
		targetAgent = "autopilot"
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultPromptTimeout
	}

	gracePeriod := opts.GracePeriod
	if gracePeriod <= 0 {
		gracePeriod = r.GracePeriod
	}
	if gracePeriod <= 0 {
		gracePeriod = defaultGracePeriod
	}

	gitBin := r.GitBinary
	if gitBin == "" {
		gitBin = "git"
	}

	startTime := time.Now()

	// 1. Create temporary prompt file to prevent shell-escaping issues and OS argv limits
	tmpFile, err := os.CreateTemp("", fmt.Sprintf("iazio-prompt-%s-*.md", opts.PromptID))
	if err != nil {
		return nil, fmt.Errorf("create temp prompt file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	if _, err := tmpFile.WriteString(opts.Prompt); err != nil {
		_ = tmpFile.Close()
		return nil, fmt.Errorf("write prompt to temp file: %w", err)
	}
	_ = tmpFile.Close()

	storyID := strings.TrimSpace(opts.StoryID)
	if storyID == "" {
		if m := storyIDRegex.FindStringSubmatch(opts.Title); len(m) > 1 {
			storyID = m[1]
		}
	}
	if storyID == "" {
		if m := storyIDRegex.FindStringSubmatch(opts.Prompt); len(m) > 1 {
			storyID = m[1]
		}
	}

	if storyID != "" && engines.PreFlightHook != nil {
		if pfErr := engines.PreFlightHook(ctx, opts.WorkingDir, storyID); pfErr != nil {
			log.Printf("[prompt_runner] warning: PreFlightHook materialization error for story %s: %v", storyID, pfErr)
		}
	}

	// 2. Prepare autopilot command invocation:
	// If the prompt targets story execution via autopilot, route directly to the standard
	// built-in `autopilot story run --story <ID>`, eliminating exit 99 across older host runtimes.
	var args []string
	if storyID != "" && targetAgent == "autopilot" && strings.TrimSpace(opts.Prompt) == "" {
		args = []string{"story", "run", "--story", storyID}
	} else {
		args = []string{"prompt", "run", "--file", tmpPath}
		if targetAgent != "" && targetAgent != "auto" {
			args = append(args, "--target-agent", targetAgent)
		} else {
			args = append(args, "--target-agent", "agy")
		}
		if opts.Title != "" {
			args = append(args, "--title", opts.Title)
		}
		if storyID != "" {
			args = append(args, "--story", storyID)
		}
	}

	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.Command(r.AutopilotBinary, args...)
	cmd.Dir = opts.WorkingDir

	// Propagate Zipkin/W3C trace continuity into autopilot process
	cmd.Env = os.Environ()
	traceID := opts.TraceID
	if traceID == "" {
		traceID = os.Getenv("TRACE_ID")
	}
	if traceID == "" {
		traceID = os.Getenv("ZIPKIN_TRACE_ID")
	}
	if traceID != "" {
		cmd.Env = append(cmd.Env,
			"ZIPKIN_TRACE_ID="+traceID,
			"TRACE_ID="+traceID,
			fmt.Sprintf("TRACEPARENT=00-%s-%s-01", traceID, opts.PromptID),
		)
	} else if tp := os.Getenv("TRACEPARENT"); tp != "" {
		cmd.Env = append(cmd.Env, "TRACEPARENT="+tp)
	}

	// Dual delivery: also pipe prompt bytes to Stdin for mock or stdin-reading executors
	cmd.Stdin = bytes.NewReader([]byte(opts.Prompt))

	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	// 3. Configure dedicated process group
	engines.SetProcGroup(cmd)

	if err := cmd.Start(); err != nil {
		durationMs := time.Since(startTime).Milliseconds()
		headSHA, treeSHA, modified := captureGitSnapshot(opts.WorkingDir, gitBin)
		return &PromptResult{
			PromptID:      opts.PromptID,
			Repo:          opts.Repo,
			TargetAgent:   targetAgent,
			Status:        "failed",
			ExitCode:      1,
			GitHeadSHA:    headSHA,
			GitTreeSHA:    treeSHA,
			ModifiedFiles: modified,
			Summary:       redact.Egress(fmt.Sprintf("Failed to spawn autopilot: %v", err)),
			DurationMs:    durationMs,
		}, fmt.Errorf("spawn autopilot: %w", err)
	}

	engines.PostStartProcessGroup(cmd)

	// 4. Wait for execution or cancellation / timeout
	waitDone := make(chan error, 1)
	go func() {
		waitDone <- cmd.Wait()
	}()

	var waitErr error
	var execStatus string = "completed"
	var exitCode int = 0

	select {
	case <-execCtx.Done():
		// Context timed out or canceled
		if errors.Is(execCtx.Err(), context.DeadlineExceeded) {
			execStatus = "timed_out"
		} else {
			execStatus = "canceled"
		}
		exitCode = 1

		// Graceful SIGTERM to process group
		engines.StopProcessGroup(cmd)

		select {
		case <-waitDone:
			// Terminated within grace period
		case <-time.After(gracePeriod):
			// Escalation to SIGKILL
			engines.KillProcessGroup(cmd)
			<-waitDone
		}
		waitErr = execCtx.Err()

	case waitErr = <-waitDone:
		if waitErr != nil {
			execStatus = "failed"
			var exitErr *exec.ExitError
			if errors.As(waitErr, &exitErr) {
				exitCode = exitErr.ExitCode()
			} else {
				exitCode = 1
			}
		}
	}

	durationMs := time.Since(startTime).Milliseconds()

	// 5. Post-Execution Git Snapshot
	headSHA, treeSHA, modified := captureGitSnapshot(opts.WorkingDir, gitBin)

	// 6. Output Summary & Redaction
	summary := stdoutBuf.String()
	if strings.TrimSpace(summary) == "" {
		if stderrBuf.Len() > 0 {
			summary = stderrBuf.String()
		} else if waitErr != nil {
			summary = waitErr.Error()
		} else {
			summary = fmt.Sprintf("Prompt executed successfully in %s via %s", opts.Repo, targetAgent)
		}
	}
	summary = redact.Egress(strings.TrimSpace(summary))

	detectedStoryID := storyID
	var detectedStoryPath string
	var detectedStoryTitle string

	for _, f := range modified {
		if strings.HasPrefix(f, "docs/stories/") && strings.HasSuffix(f, ".story.md") {
			detectedStoryPath = f
			if detectedStoryID == "" {
				if m := storyIDRegex.FindStringSubmatch(f); len(m) > 1 {
					detectedStoryID = m[1]
				}
			}
			break
		}
	}

	if detectedStoryID == "" {
		if m := storyIDRegex.FindStringSubmatch(summary); len(m) > 1 {
			detectedStoryID = m[1]
		}
	}
	if opts.Title != "" {
		detectedStoryTitle = opts.Title
	}

	result := &PromptResult{
		PromptID:      opts.PromptID,
		Repo:          opts.Repo,
		TargetAgent:   targetAgent,
		StoryID:       detectedStoryID,
		StoryTitle:    detectedStoryTitle,
		StoryPath:     detectedStoryPath,
		Status:        execStatus,
		ExitCode:      exitCode,
		GitHeadSHA:    headSHA,
		GitTreeSHA:    treeSHA,
		ModifiedFiles: modified,
		Summary:       summary,
		DurationMs:    durationMs,
	}

	if waitErr != nil && execStatus != "completed" {
		return result, fmt.Errorf("autopilot execution %s: %w", execStatus, waitErr)
	}

	return result, nil
}

func captureGitSnapshot(dir, gitBin string) (headSHA, treeSHA string, modified []string) {
	modified = make([]string, 0)
	if dir == "" {
		return "", "", modified
	}

	// 1. git rev-parse HEAD
	cmdHead := exec.Command(gitBin, "rev-parse", "HEAD")
	cmdHead.Dir = dir
	if out, err := cmdHead.Output(); err == nil {
		headSHA = strings.TrimSpace(string(out))
	}

	// 2. git rev-parse HEAD^{tree}
	cmdTree := exec.Command(gitBin, "rev-parse", "HEAD^{tree}")
	cmdTree.Dir = dir
	if out, err := cmdTree.Output(); err == nil {
		treeSHA = strings.TrimSpace(string(out))
	}

	// 3. git status --porcelain
	cmdStatus := exec.Command(gitBin, "status", "--porcelain")
	cmdStatus.Dir = dir
	if out, err := cmdStatus.Output(); err == nil {
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		for _, line := range lines {
			line = strings.TrimRight(line, "\r")
			if len(line) < 3 {
				continue
			}
			pathPart := strings.TrimSpace(line[2:])
			if idx := strings.Index(pathPart, " -> "); idx != -1 {
				pathPart = strings.TrimSpace(pathPart[idx+4:])
			}
			pathPart = strings.Trim(pathPart, `"`)
			pathPart = filepath.ToSlash(pathPart)
			if pathPart != "" {
				modified = append(modified, pathPart)
			}
		}
	}

	return headSHA, treeSHA, modified
}
