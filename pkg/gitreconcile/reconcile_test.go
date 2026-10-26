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
	handle, err := client.AcquireWithRetry(ctx, "viovy/meta-repo", "workspace-a", 10)
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
	handleB, err := client.AcquireWithRetry(ctx, "viovy/meta-repo", "workspace-b", 10)
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

	err := rec.ReconcileSuperprojectGitlinks(ctx, "/work/meta-repo", modified)
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
