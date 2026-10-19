package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
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
	for _, arg := range args {
		if arg == "--json" || arg == "-json" {
			jsonMode = true
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
	if len(stdinData) == 0 {
		return writePromptError(errors.New("empty stdin JSON payload"))
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
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if encErr := enc.Encode(result); encErr != nil {
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
