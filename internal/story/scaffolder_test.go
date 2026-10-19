package story

import (
	"context"
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

	// Since Hold is non-blocking, two simultaneous calls will have one succeed and one fail with lock held
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

	successCount := 0
	lockErrCount := 0
	for _, r := range results {
		if r == nil {
			successCount++
		} else if strings.Contains(r.Error(), "exclusive checkout lock") {
			lockErrCount++
		}
	}

	if successCount != 1 || lockErrCount != 1 {
		t.Errorf("expected 1 success and 1 lock contention error, got %d successes and %d lock errors (%v)", successCount, lockErrCount, results)
	}
}
