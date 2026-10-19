package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/viovy/iazio-harness/internal/codebase"
)

// runCodebaseCommand processes `iazio-harness codebase ...` commands.
func runCodebaseCommand(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: iazio-harness codebase repos|export --json")
	}

	sub := args[0]
	switch sub {
	case "repos":
		return runCodebaseRepos(args[1:])
	case "export":
		return runCodebaseExport(args[1:])
	default:
		return fmt.Errorf("unknown codebase subcommand %q", sub)
	}
}

func runCodebaseRepos(args []string) error {
	jsonMode := false
	for _, arg := range args {
		if arg == "--json" || arg == "-json" {
			jsonMode = true
		}
	}

	if !jsonMode {
		return writeCodebaseError(errors.New("codebase repos requires --json flag"))
	}

	fleetRoot := codebase.ResolveFleetRoot()
	result, err := codebase.DiscoverRepos(context.Background(), fleetRoot)
	if err != nil {
		return writeCodebaseError(err)
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(result); err != nil {
		return writeCodebaseError(fmt.Errorf("failed to encode repos result: %w", err))
	}

	return nil
}

func runCodebaseExport(args []string) error {
	jsonMode := false
	for _, arg := range args {
		if arg == "--json" || arg == "-json" {
			jsonMode = true
		}
	}

	if !jsonMode {
		return writeCodebaseError(errors.New("codebase export requires --json flag"))
	}

	// Read stdin with 10MB limit
	stdinData, err := io.ReadAll(io.LimitReader(os.Stdin, 10<<20))
	if err != nil {
		return writeCodebaseError(fmt.Errorf("failed to read stdin: %w", err))
	}
	if len(stdinData) == 0 {
		return writeCodebaseError(errors.New("empty stdin JSON payload"))
	}

	var req codebase.ExportRequest
	if err := json.Unmarshal(stdinData, &req); err != nil {
		return writeCodebaseError(fmt.Errorf("invalid export JSON payload: %w", err))
	}

	fleetRoot := codebase.ResolveFleetRoot()
	result, err := codebase.Export(context.Background(), fleetRoot, req)
	if err != nil {
		return writeCodebaseError(err)
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(result); err != nil {
		return writeCodebaseError(fmt.Errorf("failed to encode export result: %w", err))
	}

	return nil
}

func writeCodebaseError(err error) error {
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
