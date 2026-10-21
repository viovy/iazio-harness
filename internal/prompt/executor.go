// Package prompt coordinates prompt execution, submodule path resolution,
// exclusive workspace locking, and delegation to autopilot.
package prompt

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

// PromptPayload represents the structured JSON input received from iazio-agent.
type PromptPayload struct {
	PromptID       string `json:"prompt_id"`
	StoryID        string `json:"story_id,omitempty"`
	Repo           string `json:"repo"`
	Title          string `json:"title,omitempty"`
	Prompt         string `json:"prompt"`
	TargetAgent    string `json:"target_agent,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
}

// Executor coordinates submodule resolution, workspace file locking, and execution delegation.
type Executor struct {
	fleetRoot string
	runner    autopilot.PromptRunner
	lockDir   string
}

// NewExecutor constructs a new prompt Executor.
func NewExecutor(fleetRoot string, runner autopilot.PromptRunner) *Executor {
	if fleetRoot == "" {
		fleetRoot = ResolveFleetRoot()
	}
	if runner == nil {
		runner = autopilot.NewPromptRunner("")
	}
	return &Executor{
		fleetRoot: fleetRoot,
		runner:    runner,
	}
}

// SetLockDir overrides the lock directory (useful for hermetic unit testing).
func (e *Executor) SetLockDir(dir string) {
	e.lockDir = dir
}

// Execute validates payload, resolves target submodule directory, acquires exclusive lock,
// and invokes autopilot.PromptRunner.
func (e *Executor) Execute(ctx context.Context, payload PromptPayload) (*autopilot.PromptResult, error) {
	if strings.TrimSpace(payload.PromptID) == "" {
		if strings.TrimSpace(payload.StoryID) != "" {
			payload.PromptID = "prompt-" + strings.TrimSpace(payload.StoryID)
		} else {
			payload.PromptID = fmt.Sprintf("prompt-%d", time.Now().UnixNano())
		}
	}

	if err := validatePayload(payload); err != nil {
		return nil, fmt.Errorf("invalid prompt payload: %w", err)
	}

	targetDir, err := e.ResolveSubmodulePath(payload.Repo)
	if err != nil {
		return nil, fmt.Errorf("resolve submodule path: %w", err)
	}

	// 1. Resolve lock file path
	lockPath := e.resolveLockPath(targetDir, payload.Repo)

	timeout := time.Duration(payload.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 900 * time.Second
	}

	lockCtx, cancelLock := context.WithTimeout(ctx, timeout)
	defer cancelLock()

	// 2. Acquire exclusive per-submodule lock (serializes concurrent runs on same submodule)
	lock, err := engines.HoldPath(lockCtx, lockPath)
	if err != nil {
		return nil, fmt.Errorf("failed to acquire submodule lock for %s: %w", payload.Repo, err)
	}
	defer func() {
		_ = lock.Release()
	}()

	// 3. Delegate execution to autopilot
	opts := autopilot.RunPromptOptions{
		PromptID:    payload.PromptID,
		StoryID:     payload.StoryID,
		Repo:        payload.Repo,
		Title:       payload.Title,
		Prompt:      payload.Prompt,
		TargetAgent: payload.TargetAgent,
		WorkingDir:  targetDir,
		Timeout:     timeout,
	}

	result, err := e.runner.RunPrompt(ctx, opts)
	if err != nil {
		return result, err
	}

	return result, nil
}

// ResolveSubmodulePath resolves the submodule directory, validating against .gitmodules or superproject root.
func (e *Executor) ResolveSubmodulePath(repo string) (string, error) {
	return codebase.ResolveSubmodulePath(e.fleetRoot, repo)
}

func (e *Executor) resolveLockPath(targetDir, repo string) string {
	if e.lockDir != "" {
		return filepath.Join(e.lockDir, sanitizeRepoName(repo)+"-prompt.lock")
	}

	gitDir := ResolveGitDir(targetDir)
	return filepath.Join(gitDir, "iazio-harness-prompt.lock")
}

// ResolveGitDir locates the .git directory for a checkout, following submodule .git pointers.
func ResolveGitDir(checkout string) string {
	gitPath := filepath.Join(checkout, ".git")
	fi, err := os.Stat(gitPath)
	if err != nil {
		return checkout
	}
	if fi.IsDir() {
		return gitPath
	}

	// Submodules have a .git file: "gitdir: ../.git/modules/<submodule>"
	data, err := os.ReadFile(gitPath)
	if err != nil {
		return checkout
	}

	line := strings.TrimSpace(string(data))
	if strings.HasPrefix(line, "gitdir:") {
		rel := strings.TrimSpace(strings.TrimPrefix(line, "gitdir:"))
		if filepath.IsAbs(rel) {
			return rel
		}
		resolved := filepath.Clean(filepath.Join(checkout, rel))
		if stat, err := os.Stat(resolved); err == nil && stat.IsDir() {
			return resolved
		}
	}

	return checkout
}

// ResolveFleetRoot resolves the fleet superproject root from env, current working dir, or parent dirs.
func ResolveFleetRoot() string {
	return codebase.ResolveFleetRoot()
}

func validatePayload(p PromptPayload) error {
	promptID := strings.TrimSpace(p.PromptID)
	if promptID == "" && strings.TrimSpace(p.StoryID) != "" {
		promptID = strings.TrimSpace(p.StoryID)
	}
	if promptID == "" {
		promptID = fmt.Sprintf("prompt-%d", time.Now().UnixNano())
	}
	if strings.ContainsAny(promptID, `/\`) || strings.Contains(promptID, "..") {
		return errors.New("invalid prompt_id: path traversal characters not permitted")
	}
	if strings.TrimSpace(p.Repo) == "" {
		return errors.New("repo is required")
	}
	if strings.TrimSpace(p.Prompt) == "" {
		return errors.New("prompt is required")
	}
	return nil
}

func validateRelativeRepoPath(p string) error {
	clean := strings.TrimSpace(p)
	if clean == "" {
		return errors.New("repo must not be empty")
	}
	if strings.Contains(clean, "..") {
		return fmt.Errorf("path traversal not permitted in repo: %q", p)
	}
	if strings.HasPrefix(clean, "/") || strings.HasPrefix(clean, "\\") {
		return fmt.Errorf("absolute paths not permitted in repo: %q", p)
	}
	if len(clean) >= 2 && clean[1] == ':' && ((clean[0] >= 'a' && clean[0] <= 'z') || (clean[0] >= 'A' && clean[0] <= 'Z')) {
		return fmt.Errorf("volume prefixes not permitted in repo: %q", p)
	}
	return nil
}

func isSuperprojectRoot(repo string) bool {
	return codebase.IsSuperprojectRoot(repo)
}

func sanitizeRepoName(repo string) string {
	if isSuperprojectRoot(repo) {
		return "root"
	}
	r := strings.ReplaceAll(repo, "/", "-")
	r = strings.ReplaceAll(r, "\\", "-")
	return r
}

func isSubmoduleRegistered(gitmodulesPath, submodule string) (bool, error) {
	return codebase.IsSubmoduleRegistered(gitmodulesPath, submodule)
}
