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

// DefaultMaterializeClient optionally overrides the autopilot.Client used during lazy story materialization.
var DefaultMaterializeClient autopilot.Client

// MaterializeStoryFile delegates story scaffolding and index registration to the canonical autopilot CLI via ScaffoldStory.
func (s *Scaffolder) MaterializeStoryFile(ctx context.Context, payload StoryPayload) (*autopilot.StoryResult, error) {
	return s.ScaffoldStory(ctx, payload)
}

// MaterializeStoryFile writes a pre-rendered canonical story file to docs/stories/<file> when content is provided,
// without synthesizing story frontmatter or allocating story IDs inside iazio-harness (ADR-006).
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

// ErrStoryNotFound is returned when a requested story does not exist in the backlog or control plane.
var ErrStoryNotFound = engines.ErrStoryNotFound

const storyFileExt = ".story.md"

type remoteStoryDetail struct {
	ID                   string   `json:"id"`
	Title                string   `json:"title"`
	Status               string   `json:"status"`
	Priority             string   `json:"priority"`
	Path                 string   `json:"path"`
	Content              string   `json:"content"`
	Submodules           []string `json:"submodules,omitempty"`
	Problem              string   `json:"problem,omitempty"`
	AcceptanceCriteria   []string `json:"acceptance_criteria,omitempty"`
	TargetFiles          []string `json:"target_files,omitempty"`
	DependsOn            []string `json:"depends_on,omitempty"`
	VerificationCommands []string `json:"verification_commands,omitempty"`
}

// StoryExistsOnDisk reports whether the specified story file exists in the repoRoot or fleetRoot backlog.
func StoryExistsOnDisk(repoRoot, storyID string) bool {
	return storyExistsOnDisk(repoRoot, storyID)
}

func storyMatchesID(name, storyID string) bool {
	nameUpper := strings.ToUpper(name)
	idUpper := strings.ToUpper(storyID)
	if strings.Contains(nameUpper, idUpper) {
		return true
	}
	cleanID := strings.TrimPrefix(idUpper, "STORY-")
	if cleanID != "" && strings.Contains(nameUpper, cleanID) {
		return true
	}
	return false
}

func checkIndexForStory(indexPath, storyID, baseDir string) bool {
	data, err := os.ReadFile(indexPath)
	if err != nil {
		return false
	}
	content := string(data)
	idUpper := strings.ToUpper(storyID)
	cleanUpper := strings.TrimPrefix(idUpper, "STORY-")
	contentUpper := strings.ToUpper(content)
	if !strings.Contains(contentUpper, idUpper) && (cleanUpper == "" || !strings.Contains(contentUpper, cleanUpper)) {
		return false
	}
	lines := strings.Split(content, "\n")
	currentMatch := false
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "- id:") {
			val := strings.TrimSpace(strings.TrimPrefix(line, "- id:"))
			currentMatch = strings.EqualFold(val, storyID) || strings.EqualFold(strings.TrimPrefix(val, "STORY-"), cleanUpper)
		} else if currentMatch && strings.HasPrefix(line, "path:") {
			relPath := strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "path:")), `"`)
			if relPath != "" {
				fullPath := filepath.Join(baseDir, relPath)
				if fi, err := os.Stat(fullPath); err == nil && !fi.IsDir() && fi.Size() > 0 {
					return true
				}
			}
			return false
		}
	}
	return false
}

func resolveStoryFile(repoRoot, storyID string) (string, error) {
	cleanID := strings.TrimSpace(storyID)
	if cleanID == "" {
		return "", errors.New("storyID cannot be empty")
	}
	if repoRoot == "" {
		repoRoot = ResolveFleetRoot()
	}
	storiesDir := filepath.Join(repoRoot, "docs", "stories")
	candidates := []string{
		filepath.Join(storiesDir, cleanID+storyFileExt),
		filepath.Join(storiesDir, cleanID+".md"),
		filepath.Join(storiesDir, strings.ToLower(cleanID)+storyFileExt),
		filepath.Join(storiesDir, strings.ToLower(cleanID)+".md"),
	}
	fleetRoot := ResolveFleetRoot()
	if fleetRoot != "" && fleetRoot != repoRoot {
		fleetStories := filepath.Join(fleetRoot, "docs", "stories")
		candidates = append(candidates,
			filepath.Join(fleetStories, cleanID+storyFileExt),
			filepath.Join(fleetStories, cleanID+".md"),
			filepath.Join(fleetStories, strings.ToLower(cleanID)+storyFileExt),
			filepath.Join(fleetStories, strings.ToLower(cleanID)+".md"),
		)
	}
	for _, c := range candidates {
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() && fi.Size() > 0 {
			return c, nil
		}
	}
	scanMatch := func(dir string) string {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return ""
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name := e.Name()
			if (strings.HasSuffix(name, storyFileExt) || strings.HasSuffix(name, ".md")) && storyMatchesID(name, cleanID) {
				full := filepath.Join(dir, name)
				if fi, err := os.Stat(full); err == nil && !fi.IsDir() && fi.Size() > 0 {
					return full
				}
			}
		}
		return ""
	}
	if p := scanMatch(storiesDir); p != "" {
		return p, nil
	}
	if fleetRoot != "" && fleetRoot != repoRoot {
		if p := scanMatch(filepath.Join(fleetRoot, "docs", "stories")); p != "" {
			return p, nil
		}
	}
	return "", fmt.Errorf("%w: %s", ErrStoryNotFound, cleanID)
}

// MaterializePendingStories ensures the story markdown file exists on disk for the Scaffolder's repository root.
func (s *Scaffolder) MaterializePendingStories(ctx context.Context, repoRoot, storyID string) (string, error) {
	if repoRoot == "" && s != nil {
		repoRoot = s.fleetRoot
	}
	if err := MaterializePendingStories(ctx, repoRoot, storyID); err != nil {
		return "", err
	}
	p, _ := resolveStoryFile(repoRoot, storyID)
	return p, nil
}

// ExecuteStory materializes any pending story definition before resolving the story file on disk.
func (s *Scaffolder) ExecuteStory(ctx context.Context, repoRoot, storyID string) (string, error) {
	if repoRoot == "" && s != nil {
		repoRoot = s.fleetRoot
	}
	_, _ = s.MaterializePendingStories(ctx, repoRoot, storyID)
	return resolveStoryFile(repoRoot, storyID)
}

func scanStoriesDir(storiesDir, storyID string) bool {
	entries, err := os.ReadDir(storiesDir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, storyFileExt) || strings.HasSuffix(name, ".md") {
			if storyMatchesID(name, storyID) {
				return true
			}
		}
	}
	return false
}

func storyExistsOnDisk(repoRoot, storyID string) bool {
	cleanID := strings.TrimSpace(storyID)
	if cleanID == "" {
		return false
	}
	storiesDir := filepath.Join(repoRoot, "docs", "stories")
	candidates := []string{
		filepath.Join(storiesDir, cleanID+storyFileExt),
		filepath.Join(storiesDir, cleanID+".md"),
		filepath.Join(storiesDir, strings.ToLower(cleanID)+storyFileExt),
		filepath.Join(storiesDir, strings.ToLower(cleanID)+".md"),
	}
	fleetRoot := ResolveFleetRoot()
	if fleetRoot != "" && fleetRoot != repoRoot {
		fleetStories := filepath.Join(fleetRoot, "docs", "stories")
		candidates = append(candidates,
			filepath.Join(fleetStories, cleanID+storyFileExt),
			filepath.Join(fleetStories, cleanID+".md"),
			filepath.Join(fleetStories, strings.ToLower(cleanID)+storyFileExt),
			filepath.Join(fleetStories, strings.ToLower(cleanID)+".md"),
		)
	}
	for _, c := range candidates {
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() && fi.Size() > 0 {
			return true
		}
	}
	if scanStoriesDir(storiesDir, cleanID) {
		return true
	}
	if fleetRoot != "" && fleetRoot != repoRoot {
		if scanStoriesDir(filepath.Join(fleetRoot, "docs", "stories"), cleanID) {
			return true
		}
	}
	if checkIndexForStory(filepath.Join(storiesDir, "index.yml"), cleanID, repoRoot) {
		return true
	}
	if fleetRoot != "" && fleetRoot != repoRoot {
		if checkIndexForStory(filepath.Join(fleetRoot, "docs", "stories", "index.yml"), cleanID, fleetRoot) {
			return true
		}
	}
	return false
}

func getHarnessAPIURL() string {
	apiURL := os.Getenv("IAZIO_HARNESS_API_URL")
	if apiURL == "" {
		apiURL = os.Getenv("IAZIO_HARNESS_URL")
	}
	if apiURL == "" {
		apiURL = os.Getenv("IAZIO_API_URL")
	}
	if apiURL == "" {
		apiURL = "http://127.0.0.1:8090"
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
	return detail, nil
}

// MaterializePendingStories verifies whether the story markdown file exists on disk.
// If missing, it fetches the story detail from the control plane API (lazy materialization)
// and delegates scaffolding to the canonical autopilot CLI when DefaultMaterializeClient is configured or content is empty.
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
		if strings.Contains(err.Error(), "404") || strings.Contains(strings.ToLower(err.Error()), "not found") {
			return fmt.Errorf("%w: story %q: %v", ErrStoryNotFound, storyID, err)
		}
		return err
	}

	var sub string
	if len(detail.Submodules) > 0 {
		sub = detail.Submodules[0]
	}

	if DefaultMaterializeClient != nil || strings.TrimSpace(detail.Content) == "" {
		targetSub := sub
		if targetSub == "" {
			targetSub = "."
		}
		scaffolder := NewScaffolder(repoRoot, DefaultMaterializeClient)
		_, err := scaffolder.MaterializeStoryFile(ctx, StoryPayload{
			Submodule:            targetSub,
			Title:                detail.Title,
			Priority:             detail.Priority,
			Problem:              detail.Problem,
			AcceptanceCriteria:   detail.AcceptanceCriteria,
			TargetFiles:          detail.TargetFiles,
			DependsOn:            detail.DependsOn,
			VerificationCommands: detail.VerificationCommands,
		})
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
	ensureStoryInIndexWithPriority(storiesDir, storyID, detail.Title, filepath.Join("docs", "stories", targetName), detail.Status, detail.Priority, sub)
	return nil
}

func ensureStoryInIndex(storiesDir, storyID, title, relativePath string, submodules ...string) {
	ensureStoryInIndexWithPriority(storiesDir, storyID, title, relativePath, "READY", "P1", submodules...)
}

func ensureStoryInIndexWithPriority(storiesDir, storyID, title, relativePath, status, priority string, submodules ...string) {
	indexPath := filepath.Join(storiesDir, "index.yml")
	content, err := os.ReadFile(indexPath)
	if err != nil {
		return
	}
	if strings.Contains(string(content), storyID) {
		return
	}
	normStatus := strings.ToUpper(strings.TrimSpace(status))
	if normStatus == "" || normStatus == "PENDING" || normStatus == "PENDING_INDEX" || normStatus == "BACKLOG" {
		normStatus = "READY"
	}
	normPriority := strings.ToUpper(strings.TrimSpace(priority))
	if normPriority == "" {
		normPriority = "P1"
	}
	subStr := ""
	if len(submodules) > 0 && submodules[0] != "" {
		subStr = fmt.Sprintf("      submodules:\n        - %s\n", submodules[0])
	}
	entry := fmt.Sprintf("\n    - id: %s\n      title: %q\n      status: %s\n      priority: %s\n      path: %s\n%s      created_at: %q\n      updated_at: %q\n",
		storyID, title, normStatus, normPriority, filepath.ToSlash(relativePath), subStr, time.Now().UTC().Format(time.RFC3339), time.Now().UTC().Format(time.RFC3339))
	_ = os.WriteFile(indexPath, append(content, []byte(entry)...), 0o644)
}

func init() {
	engines.PreFlightHook = MaterializePendingStories
}
