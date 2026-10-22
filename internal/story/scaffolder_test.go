package story

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/viovy/iazio-harness/internal/autopilot"
	"github.com/viovy/iazio-harness/internal/engines"
)

type mockAutopilot struct {
	createStoryFunc func(ctx context.Context, opts autopilot.CreateStoryOptions) (*autopilot.StoryResult, error)
}

func (m *mockAutopilot) CreateStory(ctx context.Context, opts autopilot.CreateStoryOptions) (*autopilot.StoryResult, error) {
	if m.createStoryFunc != nil {
		return m.createStoryFunc(ctx, opts)
	}
	return &autopilot.StoryResult{
		StoryID:      "STORY-MOCK-001",
		Submodule:    opts.Submodule,
		StoryPath:    "docs/stories/2026-10-19-story.md",
		IndexUpdated: true,
	}, nil
}

func (m *mockAutopilot) ValidateIndex(ctx context.Context, workingDir string) error {
	return nil
}

func setupTestFleet(t *testing.T) (fleetRoot, submoduleDir string) {
	t.Helper()
	tmpDir := t.TempDir()

	subDir := filepath.Join(tmpDir, "iazio-mcp")
	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatalf("failed to create subDir: %v", err)
	}

	gitmodules := `[submodule "iazio-mcp"]
	path = iazio-mcp
	url = git@github.com:viovy/iazio-mcp.git
`
	if err := os.WriteFile(filepath.Join(tmpDir, ".gitmodules"), []byte(gitmodules), 0644); err != nil {
		t.Fatalf("failed to write .gitmodules: %v", err)
	}

	return tmpDir, subDir
}

func TestScaffolder_SubmoduleResolution(t *testing.T) {
	fleetRoot, _ := setupTestFleet(t)
	scaffolder := NewScaffolder(fleetRoot, &mockAutopilot{})

	// 1. Valid registered submodule
	path, err := scaffolder.ResolveSubmodulePath("iazio-mcp")
	if err != nil {
		t.Fatalf("unexpected error resolving registered submodule: %v", err)
	}
	if !strings.HasSuffix(path, "iazio-mcp") {
		t.Errorf("expected path to end in iazio-mcp, got %s", path)
	}

	// 2. Superproject root "."
	pathRoot, err := scaffolder.ResolveSubmodulePath(".")
	if err != nil {
		t.Fatalf("unexpected error resolving root '.': %v", err)
	}
	if pathRoot != fleetRoot {
		t.Errorf("expected root %s, got %s", fleetRoot, pathRoot)
	}

	// 3. Path traversal rejected
	_, err = scaffolder.ResolveSubmodulePath("../etc/passwd")
	if err == nil {
		t.Fatal("expected path traversal to be rejected, got nil")
	}

	// 4. Unregistered submodule rejected
	unregisteredDir := filepath.Join(fleetRoot, "unregistered")
	_ = os.MkdirAll(unregisteredDir, 0755)
	_, err = scaffolder.ResolveSubmodulePath("unregistered")
	if err == nil {
		t.Fatal("expected unregistered submodule to fail validation, got nil")
	}
}

func TestScaffolder_ScaffoldStory_Success(t *testing.T) {
	fleetRoot, subDir := setupTestFleet(t)
	var capturedOpts autopilot.CreateStoryOptions

	mockAP := &mockAutopilot{
		createStoryFunc: func(ctx context.Context, opts autopilot.CreateStoryOptions) (*autopilot.StoryResult, error) {
			capturedOpts = opts
			return &autopilot.StoryResult{
				StoryID:      "STORY-MOCK-001",
				Submodule:    opts.Submodule,
				StoryPath:    "docs/stories/story.md",
				IndexUpdated: true,
			}, nil
		},
	}

	scaffolder := NewScaffolder(fleetRoot, mockAP)
	scaffolder.SetLockDir(filepath.Join(fleetRoot, ".locks"))

	payload := StoryPayload{
		Submodule:          "iazio-mcp",
		Title:              "Add Fleet RAG Discovery",
		Priority:           "P1",
		Problem:            "Problem context",
		AcceptanceCriteria: []string{"Criteria 1"},
		TargetFiles:        []string{"internal/test.go"},
	}

	res, err := scaffolder.ScaffoldStory(context.Background(), payload)
	if err != nil {
		t.Fatalf("unexpected ScaffoldStory error: %v", err)
	}

	if res.StoryID != "STORY-MOCK-001" {
		t.Errorf("expected story ID STORY-MOCK-001, got %s", res.StoryID)
	}
	if capturedOpts.WorkingDir != subDir {
		t.Errorf("expected working dir %s, got %s", subDir, capturedOpts.WorkingDir)
	}
}

func TestScaffolder_FlockConcurrency(t *testing.T) {
	fleetRoot, subDir := setupTestFleet(t)
	lockDir := filepath.Join(fleetRoot, ".locks")

	// Manually hold lock on subDir
	heldLock, err := engines.Hold(subDir, lockDir)
	if err != nil {
		t.Fatalf("failed to hold initial lock: %v", err)
	}
	defer func() { _ = heldLock.Release() }()

	scaffolder := NewScaffolder(fleetRoot, &mockAutopilot{})
	scaffolder.SetLockDir(lockDir)

	payload := StoryPayload{
		Submodule: "iazio-mcp",
		Title:     "Concurrent test",
	}

	// Attempting to scaffold should fail because the lock is already held
	_, err = scaffolder.ScaffoldStory(context.Background(), payload)
	if err == nil {
		t.Fatal("expected error due to held lock, got nil")
	}
	if !strings.Contains(err.Error(), "exclusive checkout lock") {
		t.Errorf("expected lock error, got %v", err)
	}
}

func TestScaffolder_ConcurrentAccessSerializes(t *testing.T) {
	fleetRoot, _ := setupTestFleet(t)
	lockDir := filepath.Join(fleetRoot, ".locks")

	var callOrder []int
	var mu sync.Mutex

	mockAP := &mockAutopilot{
		createStoryFunc: func(ctx context.Context, opts autopilot.CreateStoryOptions) (*autopilot.StoryResult, error) {
			mu.Lock()
			callOrder = append(callOrder, 1)
			mu.Unlock()
			time.Sleep(20 * time.Millisecond)
			return &autopilot.StoryResult{
				StoryID:      "STORY-1",
				Submodule:    opts.Submodule,
				StoryPath:    "docs/stories/1.md",
				IndexUpdated: true,
			}, nil
		},
	}

	scaffolder := NewScaffolder(fleetRoot, mockAP)
	scaffolder.SetLockDir(lockDir)

	// With HoldWithTimeout (2s retry window), two concurrent calls taking 20ms each will serialize and both succeed
	var wg sync.WaitGroup
	results := make([]error, 2)
	for i := 0; i < 2; i++ {
		idx := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := scaffolder.ScaffoldStory(context.Background(), StoryPayload{
				Submodule: "iazio-mcp",
				Title:     "Concurrent",
			})
			results[idx] = err
		}()
	}
	wg.Wait()

	for i, r := range results {
		if r != nil {
			t.Errorf("expected call %d to succeed sequentially, got: %v", i, r)
		}
	}
	if len(callOrder) != 2 {
		t.Errorf("expected 2 calls, got %d", len(callOrder))
	}

	// Now test that a context that cancels before the lock is released returns WORKSPACE_LOCKED
	targetDir, _ := scaffolder.ResolveSubmodulePath("iazio-mcp")
	lk, err := engines.Hold(targetDir, lockDir)
	if err != nil {
		t.Fatalf("failed to hold test lock: %v", err)
	}
	defer func() { _ = lk.Release() }()

	shortCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, errTimeout := scaffolder.ScaffoldStory(shortCtx, StoryPayload{
		Submodule: "iazio-mcp",
		Title:     "Blocked",
	})
	if errTimeout == nil || !strings.Contains(errTimeout.Error(), "WORKSPACE_LOCKED") {
		t.Errorf("expected WORKSPACE_LOCKED on timeout, got %v", errTimeout)
	}
}

func TestScaffolder_CentralizedFleetBacklog(t *testing.T) {
	fleetRoot, _ := setupTestFleet(t)
	// Create docs/stories in fleetRoot, but leave iazio-mcp without docs/stories
	fleetStories := filepath.Join(fleetRoot, "docs", "stories")
	if err := os.MkdirAll(fleetStories, 0755); err != nil {
		t.Fatalf("failed to create fleetStories: %v", err)
	}

	var capturedOpts autopilot.CreateStoryOptions
	mockAP := &mockAutopilot{
		createStoryFunc: func(ctx context.Context, opts autopilot.CreateStoryOptions) (*autopilot.StoryResult, error) {
			capturedOpts = opts
			return &autopilot.StoryResult{
				StoryID:      "STORY-CENTRAL-001",
				Submodule:    opts.Submodule,
				StoryPath:    "docs/stories/central.md",
				IndexUpdated: true,
			}, nil
		},
	}

	scaffolder := NewScaffolder(fleetRoot, mockAP)
	res, err := scaffolder.ScaffoldStory(context.Background(), StoryPayload{
		Submodule: "iazio-mcp",
		Title:     "Centralized Story",
	})
	if err != nil {
		t.Fatalf("ScaffoldStory failed: %v", err)
	}
	if res.StoryID != "STORY-CENTRAL-001" {
		t.Errorf("expected STORY-CENTRAL-001, got %s", res.StoryID)
	}
	if capturedOpts.WorkingDir != fleetRoot {
		t.Errorf("expected WorkingDir to redirect to fleetRoot (%s), got %s", fleetRoot, capturedOpts.WorkingDir)
	}
	if capturedOpts.StoriesDir != fleetStories {
		t.Errorf("expected StoriesDir to be %s, got %s", fleetStories, capturedOpts.StoriesDir)
	}
}

func TestResolveFleetRoot_And_Validations(t *testing.T) {
	// 1. ResolveFleetRoot with env
	t.Setenv("IAZIO_FLEET_ROOT", "/custom/fleet")
	if ResolveFleetRoot() != "/custom/fleet" {
		t.Errorf("expected /custom/fleet, got %s", ResolveFleetRoot())
	}

	// 2. ResolveFleetRoot without env
	t.Setenv("IAZIO_FLEET_ROOT", "")
	root := ResolveFleetRoot()
	if root == "" {
		t.Error("expected non-empty fleet root")
	}

	// 3. validateRelativePath
	if err := validateRelativePath(""); err == nil {
		t.Error("expected error for empty path")
	}
	if err := validateRelativePath("../escape"); err == nil {
		t.Error("expected error for traversal")
	}
	if err := validateRelativePath("/absolute/path"); err == nil {
		t.Error("expected error for / prefix")
	}
	if err := validateRelativePath("\\windows\\path"); err == nil {
		t.Error("expected error for \\ prefix")
	}
	if err := validateRelativePath("C:drive"); err == nil {
		t.Error("expected error for volume prefix")
	}
	if err := validateRelativePath("sub/dir/file.txt"); err != nil {
		t.Errorf("expected valid relative path, got %v", err)
	}

	// 4. NewScaffolder without client
	sc := NewScaffolder("", nil)
	if sc == nil {
		t.Error("expected non-nil scaffolder")
	}

	// 5. EnsureStoriesIndexBootstrap
	testStoriesDir := filepath.Join(t.TempDir(), "docs", "stories")
	if err := EnsureStoriesIndexBootstrap(testStoriesDir); err != nil {
		t.Fatalf("EnsureStoriesIndexBootstrap failed: %v", err)
	}
	idxBytes, err := os.ReadFile(filepath.Join(testStoriesDir, "index.yml"))
	if err != nil || !strings.Contains(string(idxBytes), "stories: []") {
		t.Fatalf("expected valid bootstrapped index.yml, got %s (err: %v)", string(idxBytes), err)
	}
	// Calling again should be idempotent
	if err := EnsureStoriesIndexBootstrap(testStoriesDir); err != nil {
		t.Fatalf("idempotent EnsureStoriesIndexBootstrap failed: %v", err)
	}
}

func TestMaterializePendingStories(t *testing.T) {
	tmpDir := t.TempDir()

	// 1. Empty story ID is a no-op
	if err := MaterializePendingStories(context.Background(), tmpDir, ""); err != nil {
		t.Fatalf("expected nil for empty storyID, got %v", err)
	}

	// 2. Mock control plane HTTP server
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/stories/STORY-REMOTE-001" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{
				"id": "STORY-REMOTE-001",
				"title": "Remote Materialized Story",
				"status": "pending",
				"path": "docs/stories/story-remote-001.story.md",
				"content": "# STORY-REMOTE-001: Remote Materialized Story\n\nContent materialized from API."
			}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer mockServer.Close()

	t.Setenv("IAZIO_HARNESS_API_URL", mockServer.URL)

	// Pre-condition: story does not exist on disk
	storyFile := filepath.Join(tmpDir, "docs", "stories", "story-remote-001.story.md")
	if _, err := os.Stat(storyFile); err == nil {
		t.Fatalf("expected %s not to exist yet", storyFile)
	}

	// Materialize
	if err := MaterializePendingStories(context.Background(), tmpDir, "STORY-REMOTE-001"); err != nil {
		t.Fatalf("MaterializePendingStories failed: %v", err)
	}

	// Verify file was written
	content, err := os.ReadFile(storyFile)
	if err != nil {
		t.Fatalf("failed to read materialized story file: %v", err)
	}
	if !strings.Contains(string(content), "Content materialized from API") {
		t.Errorf("expected story content from API, got %s", string(content))
	}

	// Verify index.yml was updated
	indexFile := filepath.Join(tmpDir, "docs", "stories", "index.yml")
	idxBytes, err := os.ReadFile(indexFile)
	if err != nil {
		t.Fatalf("failed to read index.yml: %v", err)
	}
	if !strings.Contains(string(idxBytes), "STORY-REMOTE-001") {
		t.Errorf("expected index.yml to contain STORY-REMOTE-001, got: %s", string(idxBytes))
	}
	if !strings.Contains(string(idxBytes), "path: docs/stories/story-remote-001.story.md") {
		t.Errorf("expected index.yml to contain path: docs/stories/..., got: %s", string(idxBytes))
	}

	// Calling again should detect existing file and not fail even if API server is closed
	mockServer.Close()
	if err := MaterializePendingStories(context.Background(), tmpDir, "STORY-REMOTE-001"); err != nil {
		t.Fatalf("idempotent MaterializePendingStories failed: %v", err)
	}

	// Verify engines.PreFlightHook is wired
	if engines.PreFlightHook == nil {
		t.Error("expected engines.PreFlightHook to be wired")
	}
}

func TestMaterializeLiveStory(t *testing.T) {
	if os.Getenv("TEST_LIVE_MATERIALIZE") != "1" {
		t.Skip("skipping live materialize test")
	}
	repoRoot := "/home/romeo/work/meta-repo"
	err := MaterializePendingStories(context.Background(), repoRoot, "STORY-IAZIO_MCP-84089")
	if err != nil {
		t.Fatalf("MaterializePendingStories live failed: %v", err)
	}
}

