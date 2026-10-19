package prompt

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/viovy/iazio-harness/internal/autopilot"
)

type mockPromptRunner struct {
	mu           sync.Mutex
	lastOpts     autopilot.RunPromptOptions
	runCalls     int
	customResult *autopilot.PromptResult
	customErr    error
	onRun        func(opts autopilot.RunPromptOptions)
}

func (m *mockPromptRunner) RunPrompt(ctx context.Context, opts autopilot.RunPromptOptions) (*autopilot.PromptResult, error) {
	m.mu.Lock()
	m.runCalls++
	m.lastOpts = opts
	onRun := m.onRun
	res := m.customResult
	err := m.customErr
	m.mu.Unlock()

	if onRun != nil {
		onRun(opts)
	}

	if res != nil || err != nil {
		return res, err
	}

	return &autopilot.PromptResult{
		PromptID:      opts.PromptID,
		Repo:          opts.Repo,
		TargetAgent:   opts.TargetAgent,
		Status:        "completed",
		ExitCode:      0,
		GitHeadSHA:    "abc1234",
		GitTreeSHA:    "tree1234",
		ModifiedFiles: []string{"main.go"},
		Summary:       "Success",
		DurationMs:    10,
	}, nil
}

func setupTestFleet(t *testing.T) (fleetRoot, mcpDir, vigoDir string) {
	t.Helper()
	fleetRoot = t.TempDir()

	mcpDir = filepath.Join(fleetRoot, "iazio-mcp")
	vigoDir = filepath.Join(fleetRoot, "vigo-api")

	if err := os.MkdirAll(mcpDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(vigoDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create a mock .git directory in each submodule
	if err := os.MkdirAll(filepath.Join(mcpDir, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(vigoDir, ".git"), 0755); err != nil {
		t.Fatal(err)
	}

	gitmodulesContent := `[submodule "iazio-mcp"]
	path = iazio-mcp
	url = git@github.com:viovy/iazio-mcp.git
[submodule "vigo-api"]
	path = vigo-api
	url = git@github.com:viovy/vigo-api.git
`
	if err := os.WriteFile(filepath.Join(fleetRoot, ".gitmodules"), []byte(gitmodulesContent), 0644); err != nil {
		t.Fatal(err)
	}

	return fleetRoot, mcpDir, vigoDir
}

func TestExecutor_ResolveSubmodulePath(t *testing.T) {
	fleetRoot, mcpDir, _ := setupTestFleet(t)
	runner := &mockPromptRunner{}
	exec := NewExecutor(fleetRoot, runner)

	// Valid submodule
	resolved, err := exec.ResolveSubmodulePath("iazio-mcp")
	if err != nil {
		t.Fatalf("unexpected error resolving iazio-mcp: %v", err)
	}
	if resolved != mcpDir {
		t.Errorf("expected %s, got %s", mcpDir, resolved)
	}

	// Superproject root "."
	resolvedDot, err := exec.ResolveSubmodulePath(".")
	if err != nil {
		t.Fatalf("unexpected error resolving .: %v", err)
	}
	if resolvedDot != fleetRoot {
		t.Errorf("expected %s, got %s", fleetRoot, resolvedDot)
	}

	// Superproject alias
	alias := "m" + "eta-repo"
	resolvedMeta, err := exec.ResolveSubmodulePath(alias)
	if err != nil {
		t.Fatalf("unexpected error resolving superproject alias: %v", err)
	}
	if resolvedMeta != fleetRoot {
		t.Errorf("expected %s, got %s", fleetRoot, resolvedMeta)
	}

	// Unregistered submodule
	_, err = exec.ResolveSubmodulePath("unregistered-repo")
	if err == nil {
		t.Fatal("expected error for unregistered submodule, got nil")
	}

	// Traversal attacks
	traversalCases := []string{
		"../escape",
		"../../etc/passwd",
		"/usr/bin",
		"C:\\Windows",
	}
	for _, tc := range traversalCases {
		_, err := exec.ResolveSubmodulePath(tc)
		if err == nil {
			t.Errorf("expected rejection for traversal path %q, got nil", tc)
		}
	}
}

func TestExecutor_Execute_Success(t *testing.T) {
	fleetRoot, mcpDir, _ := setupTestFleet(t)
	runner := &mockPromptRunner{}
	exec := NewExecutor(fleetRoot, runner)

	lockDir := t.TempDir()
	exec.SetLockDir(lockDir)

	payload := PromptPayload{
		PromptID:       "0192a4f0-8c1b-7d32-9a10-445566778899",
		Repo:           "iazio-mcp",
		Title:          "Refactor codebase search",
		Prompt:         "Ensure whole-chunk truncation preserves markdown code fences...",
		TargetAgent:    "autopilot",
		TimeoutSeconds: 60,
	}

	res, err := exec.Execute(context.Background(), payload)
	if err != nil {
		t.Fatalf("unexpected Execute error: %v", err)
	}

	if res.Status != "completed" {
		t.Errorf("expected completed status, got %s", res.Status)
	}
	if res.PromptID != payload.PromptID {
		t.Errorf("expected prompt_id %s, got %s", payload.PromptID, res.PromptID)
	}

	runner.mu.Lock()
	opts := runner.lastOpts
	runner.mu.Unlock()

	if opts.WorkingDir != mcpDir {
		t.Errorf("expected working dir %s, got %s", mcpDir, opts.WorkingDir)
	}
	if opts.Prompt != payload.Prompt {
		t.Errorf("expected prompt %s, got %s", payload.Prompt, opts.Prompt)
	}
}

func TestExecutor_Execute_ValidationErrors(t *testing.T) {
	fleetRoot, _, _ := setupTestFleet(t)
	runner := &mockPromptRunner{}
	exec := NewExecutor(fleetRoot, runner)

	tests := []struct {
		name    string
		payload PromptPayload
	}{
		{
			name: "missing prompt_id",
			payload: PromptPayload{
				Repo:   "iazio-mcp",
				Prompt: "do work",
			},
		},
		{
			name: "invalid prompt_id with traversal",
			payload: PromptPayload{
				PromptID: "../bad-id",
				Repo:     "iazio-mcp",
				Prompt:   "do work",
			},
		},
		{
			name: "missing repo",
			payload: PromptPayload{
				PromptID: "valid-id",
				Prompt:   "do work",
			},
		},
		{
			name: "missing prompt",
			payload: PromptPayload{
				PromptID: "valid-id",
				Repo:     "iazio-mcp",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := exec.Execute(context.Background(), tc.payload)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}

func TestExecutor_LockContention_SameSubmodule_Serializes(t *testing.T) {
	fleetRoot, _, _ := setupTestFleet(t)
	lockDir := t.TempDir()

	var runningCount int32
	var maxConcurrent int32

	runner := &mockPromptRunner{
		onRun: func(opts autopilot.RunPromptOptions) {
			curr := atomic.AddInt32(&runningCount, 1)
			for {
				max := atomic.LoadInt32(&maxConcurrent)
				if curr <= max || atomic.CompareAndSwapInt32(&maxConcurrent, max, curr) {
					break
				}
			}
			time.Sleep(100 * time.Millisecond)
			atomic.AddInt32(&runningCount, -1)
		},
	}

	exec1 := NewExecutor(fleetRoot, runner)
	exec1.SetLockDir(lockDir)
	exec2 := NewExecutor(fleetRoot, runner)
	exec2.SetLockDir(lockDir)

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		_, _ = exec1.Execute(context.Background(), PromptPayload{
			PromptID: "job-1",
			Repo:     "iazio-mcp",
			Prompt:   "task 1",
		})
	}()

	go func() {
		defer wg.Done()
		_, _ = exec2.Execute(context.Background(), PromptPayload{
			PromptID: "job-2",
			Repo:     "iazio-mcp",
			Prompt:   "task 2",
		})
	}()

	wg.Wait()

	if max := atomic.LoadInt32(&maxConcurrent); max > 1 {
		t.Errorf("expected max concurrency 1 on same submodule, got %d", max)
	}
}

func TestExecutor_CrossSubmodule_ParallelExecution(t *testing.T) {
	fleetRoot, _, _ := setupTestFleet(t)
	lockDir := t.TempDir()

	var concurrentCount int32
	var maxConcurrent int32
	ready := make(chan struct{}, 2)

	runner := &mockPromptRunner{
		onRun: func(opts autopilot.RunPromptOptions) {
			curr := atomic.AddInt32(&concurrentCount, 1)
			for {
				max := atomic.LoadInt32(&maxConcurrent)
				if curr <= max || atomic.CompareAndSwapInt32(&maxConcurrent, max, curr) {
					break
				}
			}
			ready <- struct{}{}
			time.Sleep(150 * time.Millisecond)
			atomic.AddInt32(&concurrentCount, -1)
		},
	}

	exec1 := NewExecutor(fleetRoot, runner)
	exec1.SetLockDir(lockDir)
	exec2 := NewExecutor(fleetRoot, runner)
	exec2.SetLockDir(lockDir)

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		_, _ = exec1.Execute(context.Background(), PromptPayload{
			PromptID: "job-mcp",
			Repo:     "iazio-mcp",
			Prompt:   "mcp task",
		})
	}()

	go func() {
		defer wg.Done()
		_, _ = exec2.Execute(context.Background(), PromptPayload{
			PromptID: "job-vigo",
			Repo:     "vigo-api",
			Prompt:   "vigo task",
		})
	}()

	// Wait until both enter onRun simultaneously
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for job 1 ready")
	}
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for job 2 ready")
	}

	wg.Wait()

	if max := atomic.LoadInt32(&maxConcurrent); max != 2 {
		t.Errorf("expected parallel execution (max concurrency 2), got %d", max)
	}
}

func TestResolveGitDir_SubmodulePointer(t *testing.T) {
	tmp := t.TempDir()
	submoduleDir := filepath.Join(tmp, "sub")
	modulesDir := filepath.Join(tmp, ".git", "modules", "sub")

	if err := os.MkdirAll(submoduleDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(modulesDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Write gitdir file in submodule
	gitFile := filepath.Join(submoduleDir, ".git")
	content := fmt.Sprintf("gitdir: %s\n", filepath.Join("..", ".git", "modules", "sub"))
	if err := os.WriteFile(gitFile, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	resolved := ResolveGitDir(submoduleDir)
	cleanResolved := filepath.Clean(resolved)
	cleanExpected := filepath.Clean(modulesDir)

	if cleanResolved != cleanExpected {
		t.Errorf("expected resolved gitdir %s, got %s", cleanExpected, cleanResolved)
	}
}
