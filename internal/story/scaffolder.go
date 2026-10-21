// Package story coordinates workspace submodule resolution, file locking,
// and delegation of story creation to the autopilot governance CLI.
package story

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/viovy/iazio-harness/internal/autopilot"
	"github.com/viovy/iazio-harness/internal/codebase"
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
		return nil, fmt.Errorf("WORKSPACE_LOCKED: failed to acquire exclusive checkout lock for %s: %w", targetDir, err)
	}
	defer func() { _ = lock.Release() }()

	workingDir := targetDir
	storiesDir := filepath.Join(targetDir, "docs", "stories")

	// Intelligent story hub resolution: if submodule does not have its own docs/stories
	// and fleet root does have docs/stories, target the centralized fleet backlog
	if submodule != "." && submodule != "meta-repo" {
		if fi, err := os.Stat(storiesDir); err != nil || !fi.IsDir() {
			fleetStories := filepath.Join(s.fleetRoot, "docs", "stories")
			if fFi, fErr := os.Stat(fleetStories); fErr == nil && fFi.IsDir() {
				workingDir = s.fleetRoot
				storiesDir = fleetStories
			}
		}
	}

	// Ensure docs/stories and index.yml bootstrap
	if err := EnsureStoriesIndexBootstrap(storiesDir); err != nil {
		return nil, fmt.Errorf("failed to bootstrap stories index: %w", err)
	}

	opts := autopilot.CreateStoryOptions{
		Submodule:            submodule,
		Title:                payload.Title,
		Priority:             payload.Priority,
		Problem:              payload.Problem,
		AcceptanceCriteria:   payload.AcceptanceCriteria,
		TargetFiles:          payload.TargetFiles,
		DependsOn:            payload.DependsOn,
		VerificationCommands: payload.VerificationCommands,
		WorkingDir:           workingDir,
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
	return codebase.ResolveSubmodulePath(s.fleetRoot, submodule)
}

// ResolveFleetRoot resolves the fleet superproject root from env, current working dir, or parent dirs.
func ResolveFleetRoot() string {
	return codebase.ResolveFleetRoot()
}

func isSubmoduleInGitmodules(gitmodulesPath, submodule string) (bool, error) {
	return codebase.IsSubmoduleRegistered(gitmodulesPath, submodule)
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

// EnsureStoriesIndexBootstrap guarantees docs/stories and index.yml exist atomically.
func EnsureStoriesIndexBootstrap(storiesDir string) error {
	if err := os.MkdirAll(storiesDir, 0o755); err != nil {
		return fmt.Errorf("create stories dir %s: %w", storiesDir, err)
	}
	indexPath := filepath.Join(storiesDir, "index.yml")
	if _, err := os.Stat(indexPath); err == nil {
		return nil
	}
	tmpPath := filepath.Join(storiesDir, fmt.Sprintf(".tmp-index-%d.yml", time.Now().UnixNano()))
	initialContent := []byte("version: \"1.0\"\nstories: []\n")
	if err := os.WriteFile(tmpPath, initialContent, 0o644); err != nil {
		return fmt.Errorf("write temp index %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, indexPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename temp index to %s: %w", indexPath, err)
	}
	return nil
}
