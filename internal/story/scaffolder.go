// Package story coordinates workspace submodule resolution, file locking,
// and delegation of story creation to the autopilot governance CLI.
package story

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
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

	workingDir := targetDir
	storiesDir := filepath.Join(targetDir, "docs", "stories")

	// Intelligent story hub resolution: if submodule does not have its own docs/stories
	// and fleet root does have docs/stories, target the centralized fleet backlog
	if submodule != "." && submodule != "" {
		if fi, err := os.Stat(storiesDir); err != nil || !fi.IsDir() {
			fleetStories := filepath.Join(s.fleetRoot, "docs", "stories")
			if fFi, fErr := os.Stat(fleetStories); fErr == nil && fFi.IsDir() {
				workingDir = s.fleetRoot
				storiesDir = fleetStories
			}
		}
	}

	// Acquire exclusive flock with context-aware try-lock (2s timeout) on the active workingDir
	// so parallel operations targeting the central or submodule index serialize cleanly.
	lock, err := engines.HoldWithTimeout(ctx, workingDir, s.lockDir, 2*time.Second)
	if err != nil {
		return nil, fmt.Errorf("WORKSPACE_LOCKED: failed to acquire exclusive checkout lock for %s: %w", workingDir, err)
	}
	defer func() { _ = lock.Release() }()

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

// MaterializeStoryFile writes a story markdown file to docs/stories/<file> and ensures docs/stories/ exists.
func MaterializeStoryFile(repoRoot, storyID, filename, content string) error {
	if strings.TrimSpace(storyID) == "" {
		return errors.New("storyID cannot be empty")
	}
	storiesDir := filepath.Join(repoRoot, "docs", "stories")
	if err := EnsureStoriesIndexBootstrap(storiesDir); err != nil {
		return err
	}
	targetFile := filename
	if targetFile == "" {
		targetFile = storyID + ".md"
	}
	filePath := filepath.Join(storiesDir, targetFile)
	if strings.TrimSpace(content) != "" {
		tmpPath := filePath + fmt.Sprintf(".tmp-%d", time.Now().UnixNano())
		if err := os.WriteFile(tmpPath, []byte(content), 0o644); err != nil {
			return fmt.Errorf("write story temp file %s: %w", tmpPath, err)
		}
		if err := os.Rename(tmpPath, filePath); err != nil {
			_ = os.Remove(tmpPath)
			return fmt.Errorf("rename story file to %s: %w", filePath, err)
		}
	}
	return nil
}

const storyFileExt = ".story.md"

type remoteStoryDetail struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Status  string `json:"status"`
	Path    string `json:"path"`
	Content string `json:"content"`
}

func storyExistsOnDisk(repoRoot, storyID string) bool {
	storiesDir := filepath.Join(repoRoot, "docs", "stories")
	candidates := []string{
		filepath.Join(storiesDir, storyID+storyFileExt),
		filepath.Join(storiesDir, storyID+".md"),
		filepath.Join(storiesDir, strings.ToLower(storyID)+storyFileExt),
		filepath.Join(storiesDir, strings.ToLower(storyID)+".md"),
	}
	fleetRoot := ResolveFleetRoot()
	if fleetRoot != "" && fleetRoot != repoRoot {
		fleetStories := filepath.Join(fleetRoot, "docs", "stories")
		candidates = append(candidates,
			filepath.Join(fleetStories, storyID+storyFileExt),
			filepath.Join(fleetStories, storyID+".md"),
			filepath.Join(fleetStories, strings.ToLower(storyID)+storyFileExt),
			filepath.Join(fleetStories, strings.ToLower(storyID)+".md"),
		)
	}
	for _, c := range candidates {
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() && fi.Size() > 0 {
			return true
		}
	}
	return false
}

func getHarnessAPIURL() string {
	apiURL := os.Getenv("IAZIO_HARNESS_API_URL")
	if apiURL == "" {
		apiURL = os.Getenv("IAZIO_API_URL")
	}
	if apiURL == "" {
		apiURL = "http://127.0.0.1:8080"
	}
	return strings.TrimRight(apiURL, "/")
}

func fetchStoryDetail(ctx context.Context, storyID string) (remoteStoryDetail, error) {
	reqURL := fmt.Sprintf("%s/api/v1/stories/%s", getHarnessAPIURL(), url.PathEscape(storyID))
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return remoteStoryDetail{}, err
	}
	token := os.Getenv("IAZIO_HARNESS_TOKEN")
	if token == "" {
		token = os.Getenv("IAZIO_TOKEN")
	}
	if token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+token)
	}

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return remoteStoryDetail{}, fmt.Errorf("fetch story %s from %s: %w", storyID, reqURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return remoteStoryDetail{}, fmt.Errorf("fetch story %s: status %d", storyID, resp.StatusCode)
	}

	var detail remoteStoryDetail
	if err := json.NewDecoder(resp.Body).Decode(&detail); err != nil {
		return remoteStoryDetail{}, fmt.Errorf("decode story detail: %w", err)
	}
	if detail.Content == "" {
		detail.Content = fmt.Sprintf("# %s: %s\n\nStatus: %s\n", detail.ID, detail.Title, detail.Status)
	}
	return detail, nil
}

// MaterializePendingStories verifies whether the story markdown file exists on disk.
// If missing, it fetches the story detail from the control plane API (lazy materialization)
// and writes docs/stories/<file> and ensures docs/stories/index.yml contains the entry.
func MaterializePendingStories(ctx context.Context, repoRoot, storyID string) error {
	storyID = strings.TrimSpace(storyID)
	if storyID == "" {
		return nil
	}
	if repoRoot == "" {
		repoRoot = ResolveFleetRoot()
	}

	if storyExistsOnDisk(repoRoot, storyID) {
		return nil
	}

	detail, err := fetchStoryDetail(ctx, storyID)
	if err != nil {
		return err
	}

	targetName := filepath.Base(detail.Path)
	if targetName == "." || targetName == "" {
		targetName = fmt.Sprintf("%s%s", strings.ToLower(storyID), storyFileExt)
	}

	if err := MaterializeStoryFile(repoRoot, storyID, targetName, detail.Content); err != nil {
		return err
	}

	storiesDir := filepath.Join(repoRoot, "docs", "stories")
	ensureStoryInIndex(storiesDir, storyID, detail.Title, filepath.Join("docs", "stories", targetName))
	return nil
}

func ensureStoryInIndex(storiesDir, storyID, title, relativePath string) {
	indexPath := filepath.Join(storiesDir, "index.yml")
	content, err := os.ReadFile(indexPath)
	if err != nil {
		return
	}
	if strings.Contains(string(content), storyID) {
		return
	}
	entry := fmt.Sprintf("\n  - id: %s\n    title: %q\n    status: pending\n    file: %s\n", storyID, title, relativePath)
	_ = os.WriteFile(indexPath, append(content, []byte(entry)...), 0o644)
}

func init() {
	engines.PreFlightHook = MaterializePendingStories
}
