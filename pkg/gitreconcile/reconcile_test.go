package gitreconcile

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLeaseClient_AcquireRenewRelease(t *testing.T) {
	var mu sync.Mutex
	activeLeases := make(map[string]LeaseInfo)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/git-leases/acquire":
			var req struct {
				Repo       string `json:"repo"`
				Holder     string `json:"holder"`
				TTLSeconds int    `json:"ttl_seconds"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			mu.Lock()
			defer mu.Unlock()
			if cur, exists := activeLeases[req.Repo]; exists && cur.Holder != req.Holder {
				w.WriteHeader(http.StatusConflict)
				_ = json.NewEncoder(w).Encode(map[string]any{"acquired": false, "current_holder": cur.Holder})
				return
			}
			lease := LeaseInfo{
				Repo:       req.Repo,
				Holder:     req.Holder,
				LeaseID:    "lease-123",
				TTLSeconds: req.TTLSeconds,
				AcquiredAt: time.Now(),
				ExpiresAt:  time.Now().Add(time.Duration(req.TTLSeconds) * time.Second),
			}
			activeLeases[req.Repo] = lease
			_ = json.NewEncoder(w).Encode(map[string]any{"acquired": true, "lease": lease})

		case "/api/v1/git-leases/renew":
			var req struct {
				Repo    string `json:"repo"`
				Holder  string `json:"holder"`
				LeaseID string `json:"lease_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			mu.Lock()
			defer mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"renewed": true})

		case "/api/v1/git-leases/release":
			var req struct {
				Repo    string `json:"repo"`
				Holder  string `json:"holder"`
				LeaseID string `json:"lease_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			mu.Lock()
			delete(activeLeases, req.Repo)
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"released": true})
		}
	}))
	defer ts.Close()

	client := NewLeaseClient(ts.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// 1. Acquire
	handle, err := client.AcquireWithRetry(ctx, "viovy/superproject-root", "workspace-a", 10)
	if err != nil {
		t.Fatalf("failed acquiring lease: %v", err)
	}
	if handle.LeaseID() != "lease-123" {
		t.Errorf("expected lease-123, got %s", handle.LeaseID())
	}

	// 2. Release
	if err := handle.Release(ctx); err != nil {
		t.Fatalf("failed releasing lease: %v", err)
	}

	// 3. Acquire by workspace-b
	handleB, err := client.AcquireWithRetry(ctx, "viovy/superproject-root", "workspace-b", 10)
	if err != nil {
		t.Fatalf("failed acquiring lease by workspace-b: %v", err)
	}
	if err := handleB.Release(ctx); err != nil {
		t.Fatalf("failed releasing lease B: %v", err)
	}
}

func TestReconciler_RebaseAndVerifySubmodule_ConflictHandling(t *testing.T) {
	rec := NewReconciler(nil)

	// Mock git runner that simulates a rebase conflict
	rec.RunGit = func(dir string, args ...string) (string, error) {
		cmdStr := strings.Join(args, " ")
		switch {
		case strings.HasPrefix(cmdStr, "fetch"):
			return "", nil
		case strings.HasPrefix(cmdStr, "rev-list"):
			return "2", nil // 2 commits ahead on remote
		case strings.HasPrefix(cmdStr, "rebase origin/main"):
			return "CONFLICT (content): Merge conflict in internal/api/routes.go", &execError{msg: "exit 1"}
		case strings.HasPrefix(cmdStr, "diff --name-only"):
			return "internal/api/routes.go\n", nil
		case strings.HasPrefix(cmdStr, "diff"):
			return "+<<<<<<< HEAD\n+// local\n+=======\n+// remote\n+>>>>>>> origin/main\n", nil
		case strings.HasPrefix(cmdStr, "rebase --abort"):
			return "", nil
		default:
			return "", nil
		}
	}

	ctx := context.Background()
	err := rec.RebaseAndVerifySubmodule(ctx, "/path/to/iazio-harness-api", "conv-active-789", "go test ./...")
	if err == nil {
		t.Fatalf("expected conflict error, got nil")
	}

	confErr, ok := err.(*ConflictError)
	if !ok {
		t.Fatalf("expected *ConflictError, got %T: %v", err, err)
	}

	if confErr.ActiveConversationID != "conv-active-789" {
		t.Errorf("expected active conversation conv-active-789, got %s", confErr.ActiveConversationID)
	}
	if len(confErr.ConflictedFiles) != 1 || confErr.ConflictedFiles[0] != "internal/api/routes.go" {
		t.Errorf("expected internal/api/routes.go in conflicted files, got %v", confErr.ConflictedFiles)
	}
	if !strings.Contains(confErr.ResolutionPrompt, "Please resolve conflict") {
		t.Errorf("resolution prompt missing resolution instruction: %s", confErr.ResolutionPrompt)
	}
}

func TestReconciler_ReconcileSuperprojectGitlinks(t *testing.T) {
	rec := NewReconciler(nil)
	var executedCommands []string

	rec.RunGit = func(dir string, args ...string) (string, error) {
		executedCommands = append(executedCommands, strings.Join(args, " "))
		return "", nil
	}

	ctx := context.Background()
	modified := map[string]string{
		"iazio-agent": "0123456789abcdef0123456789abcdef01234567",
		"iazio-web":   "fedcba9876543210fedcba9876543210fedcba98",
	}

	err := rec.ReconcileSuperprojectGitlinks(ctx, "/work/superproject-root", modified)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var hasFetch, hasAgentLink, hasWebLink bool
	for _, cmd := range executedCommands {
		if strings.Contains(cmd, "fetch origin main") {
			hasFetch = true
		}
		if strings.Contains(cmd, "update-index --cacheinfo 160000,0123456789abcdef0123456789abcdef01234567,iazio-agent") {
			hasAgentLink = true
		}
		if strings.Contains(cmd, "update-index --cacheinfo 160000,fedcba9876543210fedcba9876543210fedcba98,iazio-web") {
			hasWebLink = true
		}
	}

	if !hasFetch {
		t.Errorf("missing superproject fetch: %v", executedCommands)
	}
	if !hasAgentLink {
		t.Errorf("missing iazio-agent gitlink update: %v", executedCommands)
	}
	if !hasWebLink {
		t.Errorf("missing iazio-web gitlink update: %v", executedCommands)
	}
}

type execError struct {
	msg string
}

func (e *execError) Error() string {
	return e.msg
}

func TestReconciler_AndLeaseClient_EdgeBranches(t *testing.T) {
	// DefaultGitRunner smoke check against git --version
	if out, err := DefaultGitRunner(t.TempDir(), "--version"); err != nil || !strings.Contains(out, "git") {
		t.Fatalf("expected DefaultGitRunner --version success, got out=%q err=%v", out, err)
	}

	// Up-to-date submodule returns nil without rebasing
	recUpToDate := NewReconciler(nil)
	recUpToDate.RunGit = func(dir string, args ...string) (string, error) {
		if len(args) > 0 && args[0] == "rev-list" {
			return "0\n", nil
		}
		return "", nil
	}
	if err := recUpToDate.RebaseAndVerifySubmodule(context.Background(), t.TempDir(), "cid-1", ""); err != nil {
		t.Fatalf("expected up-to-date submodule nil, got %v", err)
	}

	// Clean rebase + passing verifyCmd and failing verifyCmd
	recVerify := NewReconciler(nil)
	recVerify.RunGit = func(dir string, args ...string) (string, error) {
		if len(args) > 0 && args[0] == "rev-list" {
			return "1\n", nil
		}
		return "", nil
	}
	if err := recVerify.RebaseAndVerifySubmodule(context.Background(), t.TempDir(), "cid-2", "git --version"); err != nil {
		t.Fatalf("expected clean rebase + git --version verify success, got %v", err)
	}
	if err := recVerify.RebaseAndVerifySubmodule(context.Background(), t.TempDir(), "cid-3", "git invalid-subcommand-xyz"); err == nil {
		t.Fatal("expected verifyCmd failure error")
	}

	// Fetch failure in RebaseAndVerifySubmodule & ReconcileSuperprojectGitlinks
	recErr := NewReconciler(nil)
	recErr.RunGit = func(dir string, args ...string) (string, error) {
		return "", &execError{msg: "git failed"}
	}
	if err := recErr.RebaseAndVerifySubmodule(context.Background(), t.TempDir(), "", ""); err == nil {
		t.Fatal("expected fetch failure in RebaseAndVerifySubmodule")
	}
	if err := recErr.ReconcileSuperprojectGitlinks(context.Background(), t.TempDir(), map[string]string{"sub": "abc"}); err == nil {
		t.Fatal("expected fetch failure in ReconcileSuperprojectGitlinks")
	}
	recUpdateErr := NewReconciler(nil)
	recUpdateErr.RunGit = func(dir string, args ...string) (string, error) {
		if len(args) > 0 && args[0] == "update-index" {
			return "", &execError{msg: "update-index fail"}
		}
		return "", nil
	}
	if err := recUpdateErr.ReconcileSuperprojectGitlinks(context.Background(), t.TempDir(), map[string]string{"sub": "abc"}); err == nil {
		t.Fatal("expected update-index error")
	}

	// ConflictError.Error formatting
	ce := &ConflictError{SubmodulePath: "/tmp/sub", ConflictedFiles: []string{"a.go"}, ActiveConversationID: "cid-x"}
	if !strings.Contains(ce.Error(), "/tmp/sub") || !strings.Contains(ce.Error(), "cid-x") {
		t.Fatalf("unexpected ConflictError.Error(): %q", ce.Error())
	}

	// LeaseClient renewLease, idempotent Release, conflict timeout, and non-200 handling
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/git-leases/acquire":
			if r.Header.Get("X-Mode") == "" && strings.Contains(r.URL.RawQuery, "") {
				w.WriteHeader(http.StatusConflict)
				return
			}
		case "/api/v1/git-leases/renew":
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer ts.Close()

	client := NewLeaseClient(ts.URL)
	if err := client.renewLease(context.Background(), "repo", "holder", "lease-1", 10); err != nil {
		t.Fatalf("renewLease failed: %v", err)
	}
	// Double release returns nil idempotently
	h := &LeaseHandle{client: client, info: LeaseInfo{Repo: "repo", Holder: "h", LeaseID: "l"}}
	if err := h.Release(context.Background()); err != nil {
		t.Fatalf("first Release error: %v", err)
	}
	if err := h.Release(context.Background()); err != nil {
		t.Fatalf("second idempotent Release error: %v", err)
	}

	ctxTimeout, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := client.AcquireWithRetry(ctxTimeout, "repo", "holder", 10); err != ErrLeaseAcquisitionTimeout {
		t.Fatalf("expected ErrLeaseAcquisitionTimeout, got %v", err)
	}

	ts500 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts500.Close()
	if _, _, err := NewLeaseClient(ts500.URL).acquireOnce(context.Background(), "repo", "holder", 10); err == nil {
		t.Fatal("expected error on 500 status")
	}
}
