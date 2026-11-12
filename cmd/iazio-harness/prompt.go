package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/viovy/iazio-harness/internal/autopilot"
	"github.com/viovy/iazio-harness/internal/prompt"
)

// runPromptCommand processes `iazio-harness prompt ...` commands.
func runPromptCommand(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: iazio-harness prompt run --json")
	}

	sub := args[0]
	switch sub {
	case "run":
		return runPromptRun(args[1:])
	default:
		return fmt.Errorf("unknown prompt subcommand %q", sub)
	}
}

func runPromptRun(args []string) error {
	jsonMode := false
	var promptFlag string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--json" || arg == "-json" {
			jsonMode = true
		} else if (arg == "--prompt" || arg == "-prompt") && i+1 < len(args) {
			promptFlag = args[i+1]
			i++
		} else if strings.HasPrefix(arg, "--prompt=") {
			promptFlag = strings.TrimPrefix(arg, "--prompt=")
		}
	}

	if !jsonMode {
		return errors.New("prompt run requires --json flag")
	}

	// Read stdin with 10MB limit to safely support multi-kilobyte Markdown prompts
	stdinData, err := io.ReadAll(io.LimitReader(os.Stdin, 10<<20))
	if err != nil {
		return writePromptError(fmt.Errorf("failed to read stdin: %w", err))
	}
	if len(strings.TrimSpace(string(stdinData))) == 0 {
		fallbackPrompt := strings.TrimSpace(promptFlag)
		if fallbackPrompt == "" {
			fallbackPrompt = strings.TrimSpace(os.Getenv("IAZIO_PROMPT"))
		}
		fallbackStoryID := strings.TrimSpace(os.Getenv("IAZIO_STORY_ID"))
		if fallbackPrompt == "" && fallbackStoryID != "" {
			fallbackPrompt = "/autonomous-story-execution " + fallbackStoryID
		}
		if fallbackPrompt == "" {
			return writePromptError(errors.New("empty stdin JSON payload"))
		}
		synth := map[string]any{
			"prompt": fallbackPrompt,
		}
		if jid := strings.TrimSpace(os.Getenv("IAZIO_JOB_ID")); jid != "" {
			synth["prompt_id"] = "prompt-" + jid
		} else if fallbackStoryID != "" {
			synth["prompt_id"] = "prompt-" + fallbackStoryID
		}
		if fallbackStoryID != "" {
			synth["story_id"] = fallbackStoryID
		}
		if repoEnv := strings.TrimSpace(os.Getenv("IAZIO_REPO")); repoEnv != "" {
			synth["repo"] = repoEnv
		}
		if agentEnv := strings.TrimSpace(os.Getenv("IAZIO_TARGET_AGENT")); agentEnv != "" {
			synth["target_agent"] = agentEnv
		}
		stdinData, _ = json.Marshal(synth)
	}

	var payload prompt.PromptPayload
	if err := json.Unmarshal(stdinData, &payload); err != nil {
		return writePromptError(fmt.Errorf("invalid prompt JSON payload: %w", err))
	}

	// Graceful termination context listening for SIGINT and SIGTERM
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	executor := prompt.NewExecutor("", autopilot.NewPromptRunner(""))
	result, execErr := executor.Execute(ctx, payload)

	if result != nil {
		_ = persistPromptResultEnvelope(result, payload)
		if encErr := encodePromptResultToWriter(os.Stdout, result); encErr != nil {
			return writePromptError(fmt.Errorf("failed to encode result: %w", encErr))
		}
		if execErr != nil {
			return execErr
		}
		if result.ExitCode != 0 {
			return fmt.Errorf("prompt execution exited with code %d", result.ExitCode)
		}
		return nil
	}

	return writePromptError(execErr)
}

func isBrokenPipeOrClosedPipeError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.EPIPE) || errors.Is(err, os.ErrClosed) {
		return true
	}
	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "broken pipe") ||
		strings.Contains(lower, "pipe is being closed") ||
		strings.Contains(lower, "file already closed")
}

func persistPromptResultEnvelope(result *autopilot.PromptResult, payload prompt.PromptPayload) string {
	if result == nil {
		return ""
	}
	resultFile := strings.TrimSpace(os.Getenv("IAZIO_RESULT_FILE"))
	if resultFile == "" {
		jobID := strings.TrimSpace(os.Getenv("IAZIO_JOB_ID"))
		if jobID == "" {
			jobID = strings.TrimSpace(payload.PromptID)
		}
		if jobID != "" && !strings.ContainsAny(jobID, "/\\") {
			if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
				resultFile = filepath.Join(home, ".iazio", "runs", jobID, "result.json")
			}
		}
	}
	if resultFile == "" {
		return ""
	}
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return ""
	}
	if err := os.MkdirAll(filepath.Dir(resultFile), 0o700); err != nil {
		return ""
	}
	tmpFile := resultFile + ".tmp"
	if err := os.WriteFile(tmpFile, append(data, '\n'), 0o600); err != nil {
		return ""
	}
	if err := os.Rename(tmpFile, resultFile); err != nil {
		_ = os.Remove(tmpFile)
		return ""
	}
	return resultFile
}

func encodePromptResultToWriter(w io.Writer, result *autopilot.PromptResult) error {
	if result == nil {
		return nil
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if encErr := enc.Encode(result); encErr != nil {
		if isBrokenPipeOrClosedPipeError(encErr) {
			return nil
		}
		return encErr
	}
	return nil
}

func writePromptError(err error) error {
	if err == nil {
		return nil
	}
	errPayload := map[string]string{
		"status": "failed",
		"error":  err.Error(),
	}
	errBytes, _ := json.Marshal(errPayload)
	fmt.Fprintln(os.Stderr, string(errBytes))
	return err
}
