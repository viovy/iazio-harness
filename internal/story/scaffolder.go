// Package story coordinates workspace submodule resolution, file locking,
// and delegation of story creation to the autopilot governance CLI.
package story

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/viovy/iazio-harness/internal/autopilot"
	"github.com/viovy/iazio-harness/internal/engines"
)

// StoryPayload represents the semantic JSON payload accepted by iazio-harness story create.
type StoryPayload struct {
	Submodule            string   `json:"submodule"`
	Title                string   `json:"title"`
	Priority             string   `json:"priority,omitempty"`
	Problem              string   `json:"problem"`
	AcceptanceCriteria   []string `json:"acceptance_criteria"`
	TargetFiles          []string `json:"target_files"`
	DependsOn            []string `json:"depends_on,omitempty"`
	VerificationCommands []string `json:"verification_commands,omitempty"`
}

// Scaffolder manages submodule resolution, exclusive locking, and autopilot execution.
type Scaffolder struct {
	fleetRoot string
	autopilot autopilot.Client
	lockDir   string
}

// NewScaffolder constructs a new Scaffolder. If fleetRoot is empty, it attempts auto-discovery.
func NewScaffolder(fleetRoot string, ap autopilot.Client) *Scaffolder {
	if fleetRoot == "" {
		fleetRoot = ResolveFleetRoot()
	}
	if ap == nil {
		ap = autopilot.NewCLIClient("")
	}
	return &Scaffolder{
		fleetRoot: fleetRoot,
		autopilot: ap,
	}
}

// SetLockDir allows overriding the lock directory (useful for hermetic unit testing).
func (s *Scaffolder) SetLockDir(dir string) {
	s.lockDir = dir
}

// ScaffoldStory resolves the target submodule worktree, acquires an exclusive file lock,
// and executes autopilot to scaffold the story and update index.yml.
func (s *Scaffolder) ScaffoldStory(ctx context.Context, payload StoryPayload) (*autopilot.StoryResult, error) {
	submodule := strings.TrimSpace(payload.Submodule)
	if submodule == "" {
		return nil, errors.New("submodule is required")
	}

	targetDir, err := s.ResolveSubmodulePath(submodule)
	if err != nil {
		return nil, fmt.Errorf("resolve submodule path: %w", err)
	}

	// Acquire exclusive flock for the checkout so parallel runs never race on autopilot next-id or index.yml
	lock, err := engines.Hold(targetDir, s.lockDir)
	if err != nil {
		return nil, fmt.Errorf("failed to acquire exclusive checkout lock for %s: %w", targetDir, err)
	}
	defer func() { _ = lock.Release() }()

	storiesDir := filepath.Join(targetDir, "docs", "stories")
	opts := autopilot.CreateStoryOptions{
		Submodule:            submodule,
		Title:                payload.Title,
		Priority:             payload.Priority,
		Problem:              payload.Problem,
		AcceptanceCriteria:   payload.AcceptanceCriteria,
		TargetFiles:          payload.TargetFiles,
		DependsOn:            payload.DependsOn,
		VerificationCommands: payload.VerificationCommands,
		WorkingDir:           targetDir,
		StoriesDir:           storiesDir,
	}

	result, err := s.autopilot.CreateStory(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("autopilot execution error: %w", err)
	}

	return result, nil
}

// ResolveSubmodulePath resolves the directory on disk for the given submodule name,
// validating that it matches a registered submodule in .gitmodules or is the superproject root ".".
func (s *Scaffolder) ResolveSubmodulePath(submodule string) (string, error) {
	if err := validateRelativePath(submodule); err != nil && submodule != "." {
		return "", err
	}

	fleetRoot := s.fleetRoot
	if fleetRoot == "" {
		fleetRoot = "."
	}

	if submodule == "." {
		return filepath.Abs(fleetRoot)
	}

	targetDir := filepath.Join(fleetRoot, submodule)
	absTarget, err := filepath.Abs(targetDir)
	if err != nil {
		return "", err
	}

	// Validate target directory exists
	info, err := os.Stat(absTarget)
	if err != nil {
		return "", fmt.Errorf("submodule directory does not exist: %s (%w)", absTarget, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("submodule path is not a directory: %s", absTarget)
	}

	// Validate in .gitmodules if present
	gitmodulesPath := filepath.Join(fleetRoot, ".gitmodules")
	if _, err := os.Stat(gitmodulesPath); err == nil {
		registered, err := isSubmoduleInGitmodules(gitmodulesPath, submodule)
		if err != nil {
			return "", fmt.Errorf("failed to inspect .gitmodules: %w", err)
		}
		if !registered {
			return "", fmt.Errorf("submodule %q is not registered in .gitmodules", submodule)
		}
	}

	return absTarget, nil
}

// ResolveFleetRoot resolves the fleet superproject root from env, current working dir, or parent dirs.
func ResolveFleetRoot() string {
	if env := os.Getenv("IAZIO_FLEET_ROOT"); env != "" {
		return env
	}

	dir, err := os.Getwd()
	if err != nil {
		return "."
	}

	curr := dir
	for {
		if _, err := os.Stat(filepath.Join(curr, ".gitmodules")); err == nil {
			return curr
		}
		parent := filepath.Dir(curr)
		if parent == curr {
			break
		}
		curr = parent
	}
	return dir
}

func isSubmoduleInGitmodules(gitmodulesPath, submodule string) (bool, error) {
	f, err := os.Open(gitmodulesPath)
	if err != nil {
		return false, err
	}
	defer f.Close()

	normSub := strings.TrimSpace(filepath.ToSlash(submodule))
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "path =") || strings.HasPrefix(line, "path=") {
			parts := strings.SplitN(line, "=", 2)
			if len(parts) == 2 {
				val := strings.TrimSpace(parts[1])
				if filepath.ToSlash(val) == normSub {
					return true, nil
				}
			}
		}
	}
	return false, scanner.Err()
}

func validateRelativePath(p string) error {
	clean := strings.TrimSpace(p)
	if clean == "" {
		return errors.New("path must not be empty")
	}
	if strings.Contains(clean, "..") {
		return fmt.Errorf("path traversal not permitted: %q", p)
	}
	if strings.HasPrefix(clean, "/") || strings.HasPrefix(clean, "\\") {
		return fmt.Errorf("absolute paths not permitted: %q", p)
	}
	if len(clean) >= 2 && clean[1] == ':' && ((clean[0] >= 'a' && clean[0] <= 'z') || (clean[0] >= 'A' && clean[0] <= 'Z')) {
		return fmt.Errorf("volume prefixes not permitted: %q", p)
	}
	return nil
}
