// Package gitreconcile provides distributed commit serialization, submodule merge
// coordination, and superproject gitlink reconciliation across fleet workspaces (STORY-GOV-3705).
package gitreconcile

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/viovy/iazio-harness/internal/procutil"
)

var (
	ErrLeaseAcquisitionTimeout = errors.New("timeout acquiring git merge lease")
	ErrLeaseHeld               = errors.New("repository merge lease held by another workspace")
	ErrRebaseConflict          = errors.New("submodule rebase encountered merge conflict")
)

// ConflictError captures merge conflict details for targeted agent resumption.
type ConflictError struct {
	SubmodulePath        string   `json:"submodule_path"`
	ConflictedFiles      []string `json:"conflicted_files"`
	Diff                 string   `json:"diff"`
	ActiveConversationID string   `json:"active_conversation_id"`
	ResolutionPrompt     string   `json:"resolution_prompt"`
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("rebase conflict in %s on files %v; resume agy conversation %s",
		e.SubmodulePath, e.ConflictedFiles, e.ActiveConversationID)
}

// LeaseInfo holds lease metadata returned by iazio-harness-api.
type LeaseInfo struct {
	Repo       string    `json:"repo"`
	Holder     string    `json:"holder"`
	LeaseID    string    `json:"lease_id"`
	TTLSeconds int       `json:"ttl_seconds"`
	AcquiredAt time.Time `json:"acquired_at"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// LeaseHandle holds an acquired merge lease and maintains its heartbeat.
type LeaseHandle struct {
	client     *LeaseClient
	info       LeaseInfo
	ctx        context.Context
	cancelFunc context.CancelFunc
	released   bool
	mu         sync.Mutex
}

// LeaseID returns the active lease ID.
func (h *LeaseHandle) LeaseID() string {
	return h.info.LeaseID
}

// Release stops the heartbeat and releases the lease in iazio-harness-api.
func (h *LeaseHandle) Release(ctx context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.released {
		return nil
	}
	h.released = true
	if h.cancelFunc != nil {
		h.cancelFunc()
	}
	return h.client.releaseLease(ctx, h.info.Repo, h.info.Holder, h.info.LeaseID)
}

// LeaseClient coordinates lease operations with iazio-harness-api.
type LeaseClient struct {
	BaseURL    string
	HTTPClient *http.Client
}

// NewLeaseClient creates a new LeaseClient.
func NewLeaseClient(baseURL string) *LeaseClient {
	return &LeaseClient{
		BaseURL:    strings.TrimSuffix(baseURL, "/"),
		HTTPClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// AcquireWithRetry attempts to acquire a lease, retrying with exponential backoff until success or ctx expires.
func (c *LeaseClient) AcquireWithRetry(ctx context.Context, repo, holder string, ttlSeconds int) (*LeaseHandle, error) {
	backoff := 500 * time.Millisecond
	maxBackoff := 5 * time.Second

	for {
		handle, acquired, err := c.acquireOnce(ctx, repo, holder, ttlSeconds)
		if err != nil {
			return nil, err
		}
		if acquired {
			return handle, nil
		}

		select {
		case <-ctx.Done():
			return nil, ErrLeaseAcquisitionTimeout
		case <-time.After(backoff):
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
}

func (c *LeaseClient) acquireOnce(ctx context.Context, repo, holder string, ttlSeconds int) (*LeaseHandle, bool, error) {
	reqBody, _ := json.Marshal(map[string]any{
		"repo":        repo,
		"holder":      holder,
		"ttl_seconds": ttlSeconds,
	})
	url := fmt.Sprintf("%s/api/v1/git-leases/acquire", c.BaseURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusConflict {
		return nil, false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("unexpected status %d from git-leases/acquire", resp.StatusCode)
	}

	var res struct {
		Acquired bool      `json:"acquired"`
		Lease    LeaseInfo `json:"lease"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, false, err
	}
	if !res.Acquired || res.Lease.LeaseID == "" {
		return nil, false, nil
	}

	hCtx, hCancel := context.WithCancel(context.Background())
	handle := &LeaseHandle{
		client:     c,
		info:       res.Lease,
		ctx:        hCtx,
		cancelFunc: hCancel,
	}

	// Start heartbeat routine
	hbInterval := time.Duration(ttlSeconds/3) * time.Second
	if hbInterval < 2*time.Second {
		hbInterval = 2 * time.Second
	}
	go handle.heartbeatLoop(hbInterval, ttlSeconds)

	return handle, true, nil
}

func (h *LeaseHandle) heartbeatLoop(interval time.Duration, ttlSeconds int) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-h.ctx.Done():
			return
		case <-ticker.C:
			_ = h.client.renewLease(h.ctx, h.info.Repo, h.info.Holder, h.info.LeaseID, ttlSeconds)
		}
	}
}

func (c *LeaseClient) renewLease(ctx context.Context, repo, holder, leaseID string, ttlSeconds int) error {
	reqBody, _ := json.Marshal(map[string]any{
		"repo":        repo,
		"holder":      holder,
		"lease_id":    leaseID,
		"ttl_seconds": ttlSeconds,
	})
	url := fmt.Sprintf("%s/api/v1/git-leases/renew", c.BaseURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

func (c *LeaseClient) releaseLease(ctx context.Context, repo, holder, leaseID string) error {
	reqBody, _ := json.Marshal(map[string]any{
		"repo":     repo,
		"holder":   holder,
		"lease_id": leaseID,
	})
	url := fmt.Sprintf("%s/api/v1/git-leases/release", c.BaseURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

// GitRunner abstracts command execution for git operations.
type GitRunner func(dir string, args ...string) (string, error)

// DefaultGitRunner executes real git commands via os/exec.
func DefaultGitRunner(dir string, args ...string) (string, error) {
	cmd := procutil.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// Reconciler coordinates submodule rebases and superproject gitlink merges.
type Reconciler struct {
	LeaseClient *LeaseClient
	RunGit      GitRunner
}

// NewReconciler constructs a new Reconciler.
func NewReconciler(client *LeaseClient) *Reconciler {
	return &Reconciler{
		LeaseClient: client,
		RunGit:      DefaultGitRunner,
	}
}

// RebaseAndVerifySubmodule rebases the submodule on origin/main, verifies semantic correctness,
// and returns a ConflictError with active conversation resumption instructions if conflicts occur.
func (r *Reconciler) RebaseAndVerifySubmodule(ctx context.Context, subDir, activeConvID, verifyCmd string) error {
	// 1. Fetch origin/main
	if _, err := r.RunGit(subDir, "fetch", "origin", "--prune"); err != nil {
		return fmt.Errorf("git fetch failed in %s: %w", subDir, err)
	}

	// 2. Check if rebase is needed
	out, err := r.RunGit(subDir, "rev-list", "HEAD..origin/main", "--count")
	if err == nil && strings.TrimSpace(out) == "0" {
		// Local is already ahead or up-to-date with origin/main
		return nil
	}

	// 3. Attempt rebase
	rebaseOut, rebaseErr := r.RunGit(subDir, "rebase", "origin/main")
	if rebaseErr != nil {
		// Conflict detected!
		diffOut, _ := r.RunGit(subDir, "diff")
		statusOut, _ := r.RunGit(subDir, "diff", "--name-only", "--diff-filter=U")
		var confFiles []string
		for _, line := range strings.Split(statusOut, "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed != "" {
				confFiles = append(confFiles, trimmed)
			}
		}

		// Abort rebase to leave clean state
		_, _ = r.RunGit(subDir, "rebase", "--abort")

		prompt := fmt.Sprintf(
			"Submodule %s encountered a git rebase conflict against origin/main. Conflicted files: %v.\nDiff summary:\n%s\nPlease resolve conflict, run '%s', and commit.",
			filepath.Base(subDir), confFiles, diffOut, verifyCmd,
		)

		return &ConflictError{
			SubmodulePath:        subDir,
			ConflictedFiles:      confFiles,
			Diff:                 diffOut,
			ActiveConversationID: activeConvID,
			ResolutionPrompt:     prompt,
		}
	}

	// 4. Rebase succeeded cleanly. Run verification gate if provided.
	if verifyCmd != "" {
		parts := strings.Fields(verifyCmd)
		if len(parts) > 0 {
			cmd := procutil.CommandContext(ctx, parts[0], parts[1:]...)
			cmd.Dir = subDir
			if out, err := cmd.CombinedOutput(); err != nil {
				return fmt.Errorf("incremental verification failed after rebase in %s (%s): %v, output: %s",
					subDir, verifyCmd, err, string(out))
			}
		}
	}

	_ = rebaseOut
	return nil
}

// ReconcileSuperprojectGitlinks updates modified submodule gitlink pointers (160000)
// on top of freshly fetched origin/main in the superproject.
func (r *Reconciler) ReconcileSuperprojectGitlinks(ctx context.Context, superDir string, modifiedSubmodules map[string]string) error {
	// 1. Fetch origin/main of superproject
	if _, err := r.RunGit(superDir, "fetch", "origin", "main"); err != nil {
		return fmt.Errorf("superproject fetch origin main failed: %w", err)
	}

	// 2. Re-apply 160000 gitlink cache entries for modified submodules
	for subPath, newSHA := range modifiedSubmodules {
		cacheInfo := fmt.Sprintf("160000,%s,%s", newSHA, subPath)
		if _, err := r.RunGit(superDir, "update-index", "--cacheinfo", cacheInfo); err != nil {
			return fmt.Errorf("failed updating gitlink for %s to %s: %w", subPath, newSHA, err)
		}
	}

	return nil
}
