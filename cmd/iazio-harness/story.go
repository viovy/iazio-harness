package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/viovy/iazio-harness/internal/autopilot"
	"github.com/viovy/iazio-harness/internal/story"
)

// runStoryCommand processes `iazio-harness story ...` commands.
func runStoryCommand(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: iazio-harness story create --json")
	}

	sub := args[0]
	switch sub {
	case "create":
		return runStoryCreate(args[1:])
	default:
		return fmt.Errorf("unknown story subcommand %q", sub)
	}
}

func runStoryCreate(args []string) error {
	jsonMode := false
	for _, arg := range args {
		if arg == "--json" || arg == "-json" {
			jsonMode = true
		}
	}

	if !jsonMode {
		return errors.New("story create requires --json flag")
	}

	stdinData, err := io.ReadAll(io.LimitReader(os.Stdin, 2<<20))
	if err != nil {
		return writeStoryError(fmt.Errorf("failed to read stdin: %w", err))
	}
	if len(stdinData) == 0 {
		return writeStoryError(errors.New("empty stdin JSON payload"))
	}

	var payload story.StoryPayload
	if err := json.Unmarshal(stdinData, &payload); err != nil {
		return writeStoryError(fmt.Errorf("invalid story JSON payload: %w", err))
	}

	scaffolder := story.NewScaffolder("", autopilot.NewCLIClient(""))
	result, err := scaffolder.ScaffoldStory(context.Background(), payload)
	if err != nil {
		return writeStoryError(err)
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(result); err != nil {
		return writeStoryError(fmt.Errorf("failed to encode result: %w", err))
	}

	return nil
}

func writeStoryError(err error) error {
	if err == nil {
		return nil
	}
	errPayload := map[string]string{
		"error": err.Error(),
	}
	errBytes, _ := json.Marshal(errPayload)
	fmt.Fprintln(os.Stderr, string(errBytes))
	return err
}
