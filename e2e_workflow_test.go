package e2e_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// ============================================================================
// 1. DOMAIN, PROOF-OF-EXECUTION CONTRACT & QUEUE STORE
// ============================================================================

var (
	ErrJobNotFound          = errors.New("job not found")
	ErrStaleFencingToken    = errors.New("409 conflict: stale fencing token")
	ErrNoPendingJob         = errors.New("no pending job available")
	ErrBypassOrNoOpDetected = errors.New("BYPASS_OR_NOOP_DETECTED: job claimed completion without verifiable execution proof")
)

type JobStatus string

const (
	StatusPending   JobStatus = "pending"
	StatusRunning   JobStatus = "running"
	StatusCompleted JobStatus = "completed"
	StatusFailed    JobStatus = "failed"
)

type ExecutionProof struct {
	PreGitHeadSHA    string `json:"pre_git_head_sha"`
	PostGitHeadSHA   string `json:"post_git_head_sha"`
	PreGitTreeSHA    string `json:"pre_git_tree_sha"`
	PostGitTreeSHA   string `json:"post_git_tree_sha"`
	AllocatedStoryID string `json:"allocated_story_id"`
	StdoutBytes      int    `json:"stdout_bytes"`
	StdoutLineCount  int    `json:"stdout_line_count"`
	RawTranscript    string `json:"raw_transcript"`
	AgentPID         int    `json:"agent_pid"`
	DurationMs       int64  `json:"duration_ms"`
}

// VerifyExecutionProof enforces hard anti-lying and anti-noop invariants before
// any job is allowed to transition to StatusCompleted.
func VerifyExecutionProof(action string, proof ExecutionProof) error {
	// 1. Reject canned/synthetic wrapper summaries masquerading as output
	trimmed := strings.TrimSpace(proof.RawTranscript)
	if trimmed == "" || strings.HasPrefix(trimmed, "[+] prompt executed via") {
		return fmt.Errorf("%w: synthetic or empty transcript detected (%q)", ErrBypassOrNoOpDetected, trimmed)
	}

	// 2. Require minimum real telemetry volume from the underlying agent CLI
	if proof.StdoutBytes < 256 || proof.StdoutLineCount < 5 {
		return fmt.Errorf("%w: insufficient agent stdout telemetry (bytes=%d, lines=%d)",
			ErrBypassOrNoOpDetected, proof.StdoutBytes, proof.StdoutLineCount)
	}

	// 3. For mutating workflows (/autonomous-story-execution), enforce Git mutation & Story allocation
	if action == "/autonomous-story-execution" {
		if proof.PreGitHeadSHA == "" || proof.PostGitHeadSHA == "" ||
			proof.PreGitTreeSHA == "" || proof.PostGitTreeSHA == "" {
			return fmt.Errorf("%w: missing pre/post Git SHA telemetry", ErrBypassOrNoOpDetected)
		}

		// If neither HEAD commit nor working tree SHA changed, zero work was done!
		if proof.PreGitHeadSHA == proof.PostGitHeadSHA && proof.PreGitTreeSHA == proof.PostGitTreeSHA {
			return fmt.Errorf("%w: zero repository mutation (HEAD=%s and Tree=%s unchanged)",
				ErrBypassOrNoOpDetected, proof.PostGitHeadSHA, proof.PostGitTreeSHA)
		}

		// Must have allocated a real Story ID (cannot remain empty or 'pending-allocation')
		if proof.AllocatedStoryID == "" || proof.AllocatedStoryID == "pending-allocation" {
			return fmt.Errorf("%w: story_id was never allocated (got %q)",
				ErrBypassOrNoOpDetected, proof.AllocatedStoryID)
		}
	}

	return nil
}

type Job struct {
	ID             string          `json:"id"`
	Action         string          `json:"action"`
	TargetRepo     string          `json:"target_repo"`
	TargetAgent    string          `json:"target_agent"`
	Story          string          `json:"story"`
	StoryID        string          `json:"story_id"`
	Status         JobStatus       `json:"status"`
	AgentID        string          `json:"agent_id"`
	FencingToken   uint64          `json:"fencing_token"`
	LeaseExpiresAt time.Time       `json:"lease_expires_at"`
	OutputLog      string          `json:"output_log"`
	ErrorMsg       string          `json:"error_msg"`
	ExecutionProof *ExecutionProof `json:"execution_proof,omitempty"`
	UpdatedAt      time.Time       `json:"updated_at"`
}

type QueueStore struct {
	mu       sync.Mutex
	jobs     map[string]*Job
	order    []string
	leaseTTL time.Duration
}

func NewQueueStore(leaseTTL time.Duration) *QueueStore {
	return &QueueStore{
		jobs:     make(map[string]*Job),
		leaseTTL: leaseTTL,
	}
}

func generateJobID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "job_" + hex.EncodeToString(b)
}

func (s *QueueStore) Enqueue(action, targetRepo, targetAgent, story, storyID string) *Job {
	s.mu.Lock()
	defer s.mu.Unlock()

	id := generateJobID()
	j := &Job{
		ID:           id,
		Action:       action,
		TargetRepo:   targetRepo,
		TargetAgent:  targetAgent,
		Story:        story,
		StoryID:      storyID,
		Status:       StatusPending,
		FencingToken: 0,
		UpdatedAt:    time.Now(),
	}
	s.jobs[id] = j
	s.order = append(s.order, id)
	copyJob := *j
	return &copyJob
}

func (s *QueueStore) Get(id string) (*Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	if !ok {
		return nil, ErrJobNotFound
	}
	copyJob := *j
	return &copyJob, nil
}

// Claim atomically locks the next pending job for targetAgent and increments FencingToken.
func (s *QueueStore) Claim(agentID, targetAgent string) (*Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	for _, id := range s.order {
		j := s.jobs[id]
		if j.Status == StatusPending && j.TargetAgent == targetAgent {
			j.Status = StatusRunning
			j.AgentID = agentID
			j.FencingToken++ // MONOTONIC FENCING TOKEN INCREMENT
			j.LeaseExpiresAt = now.Add(s.leaseTTL)
			j.UpdatedAt = now
			copyJob := *j
			return &copyJob, nil
		}
	}
	return nil, ErrNoPendingJob
}

// Heartbeat enforces strict CAS on FencingToken and extends LeaseExpiresAt.
func (s *QueueStore) Heartbeat(jobID string, fencingToken uint64, storyID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	j, ok := s.jobs[jobID]
	if !ok {
		return ErrJobNotFound
	}
	if j.FencingToken != fencingToken || j.Status != StatusRunning {
		return fmt.Errorf("%w: job=%s expected_token=%d got_token=%d status=%s",
			ErrStaleFencingToken, jobID, j.FencingToken, fencingToken, j.Status)
	}
	j.LeaseExpiresAt = time.Now().Add(s.leaseTTL)
	if storyID != "" && storyID != "pending-allocation" {
		j.StoryID = storyID
	}
	j.UpdatedAt = time.Now()
	return nil
}

// AppendLogs appends a real-time output chunk to the job's log.
func (s *QueueStore) AppendLogs(jobID string, fencingToken uint64, chunk string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	j, ok := s.jobs[jobID]
	if !ok {
		return ErrJobNotFound
	}
	if j.FencingToken != fencingToken || j.Status != StatusRunning {
		return fmt.Errorf("%w: append logs rejected for job=%s expected_token=%d got_token=%d status=%s",
			ErrStaleFencingToken, jobID, j.FencingToken, fencingToken, j.Status)
	}
	j.OutputLog += chunk
	j.UpdatedAt = time.Now()
	return nil
}

// Closeout enforces strict CAS on FencingToken and validates Proof-of-Execution before accepting completion.
func (s *QueueStore) Closeout(jobID string, fencingToken uint64, status JobStatus, storyID, outputLog, errMsg string, proof ExecutionProof) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	j, ok := s.jobs[jobID]
	if !ok {
		return ErrJobNotFound
	}
	if j.FencingToken != fencingToken || j.Status != StatusRunning {
		return fmt.Errorf("%w: closeout rejected for job=%s expected_token=%d got_token=%d status=%s",
			ErrStaleFencingToken, jobID, j.FencingToken, fencingToken, j.Status)
	}

	j.ExecutionProof = &proof
	if storyID != "" {
		j.StoryID = storyID
	}
	if outputLog != "" {
		j.OutputLog = outputLog
	}
	j.UpdatedAt = time.Now()

	// If claiming completion, enforce Proof-of-Execution anti-bypass verification
	if status == StatusCompleted {
		if err := VerifyExecutionProof(j.Action, proof); err != nil {
			j.Status = StatusFailed
			j.ErrorMsg = err.Error()
			return err
		}
	}

	j.Status = status
	j.ErrorMsg = errMsg
	return nil
}

// ReapExpiredLeases moves running jobs whose lease expired back to pending.
func (s *QueueStore) ReapExpiredLeases() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	reaped := 0
	now := time.Now()
	for _, j := range s.jobs {
		if j.Status == StatusRunning && now.After(j.LeaseExpiresAt) {
			j.Status = StatusPending
			j.AgentID = ""
			j.UpdatedAt = now
			reaped++
		}
	}
	return reaped
}

// ============================================================================
// 2. HTTP SERVER (iazio-api / global queue)
// ============================================================================

func NewIazioAPIServer(store *QueueStore) *httptest.Server {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /api/v1/queue/jobs", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID          string `json:"id"`
			Action      string `json:"action"`
			TargetRepo  string `json:"target_repo"`
			TargetAgent string `json:"target_agent"`
			Story       string `json:"story"`
			StoryID     string `json:"story_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		// Requirement 1: Server-side ID generation strictly enforced.
		// Reject any client-supplied ID with 400 Bad Request to prevent injection/backfill bypasses.
		if strings.TrimSpace(req.ID) != "" {
			http.Error(w, "client-supplied job ID is forbidden; server generates IDs", http.StatusBadRequest)
			return
		}

		job := store.Enqueue(req.Action, req.TargetRepo, req.TargetAgent, req.Story, req.StoryID)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(job)
	})

	mux.HandleFunc("GET /api/v1/queue/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		job, err := store.Get(r.PathValue("id"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(job)
	})

	mux.HandleFunc("POST /api/v1/queue/claim", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			AgentID     string `json:"agent_id"`
			TargetAgent string `json:"target_agent"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		job, err := store.Claim(req.AgentID, req.TargetAgent)
		if errors.Is(err, ErrNoPendingJob) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_ = json.NewEncoder(w).Encode(job)
	})

	mux.HandleFunc("POST /api/v1/queue/jobs/{id}/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			FencingToken uint64 `json:"fencing_token"`
			StoryID      string `json:"story_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		err := store.Heartbeat(r.PathValue("id"), req.FencingToken, req.StoryID)
		if errors.Is(err, ErrStaleFencingToken) {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	// Requirement 2: Real-time stdout/stderr log chunk streaming endpoint
	mux.HandleFunc("POST /api/v1/queue/jobs/{id}/logs", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			FencingToken uint64 `json:"fencing_token"`
			Chunk        string `json:"chunk"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		err := store.AppendLogs(r.PathValue("id"), req.FencingToken, req.Chunk)
		if errors.Is(err, ErrStaleFencingToken) {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		if errors.Is(err, ErrJobNotFound) {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	// Requirement 3: Cryptographic Proof-of-Execution Gate on Closeout
	mux.HandleFunc("POST /api/v1/queue/jobs/{id}/closeout", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			FencingToken   uint64         `json:"fencing_token"`
			Status         JobStatus      `json:"status"`
			StoryID        string         `json:"story_id"`
			OutputLog      string         `json:"output_log"`
			ErrorMsg       string         `json:"error_msg"`
			ExecutionProof ExecutionProof `json:"execution_proof"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		err := store.Closeout(r.PathValue("id"), req.FencingToken, req.Status, req.StoryID, req.OutputLog, req.ErrorMsg, req.ExecutionProof)
		if errors.Is(err, ErrStaleFencingToken) {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		if errors.Is(err, ErrJobNotFound) {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		if errors.Is(err, ErrBypassOrNoOpDetected) {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	return httptest.NewServer(mux)
}

// ============================================================================
// 3. MCP SERVER HANDLER (iazio-mcp with Read-After-Write Verification)
// ============================================================================

type IazioMCPClient struct {
	apiBaseURL string
	httpClient *http.Client
}

func NewIazioMCPClient(apiBaseURL string) *IazioMCPClient {
	return &IazioMCPClient{
		apiBaseURL: apiBaseURL,
		httpClient: &http.Client{Timeout: 5 * time.Second},
	}
}

func (m *IazioMCPClient) QueueAutonomousStoryExecution(ctx context.Context, targetRepo, targetAgent, story string) (*Job, error) {
	payload := map[string]string{
		"action":       "/autonomous-story-execution",
		"target_repo":  targetRepo,
		"target_agent": targetAgent,
		"story":        story,
		"story_id":     "pending-allocation",
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.apiBaseURL+"/api/v1/queue/jobs", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	resp, err := m.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("iazio-mcp failed to reach iazio-api: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("iazio-api rejected enqueue with status %d", resp.StatusCode)
	}

	var created Job
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		return nil, err
	}

	// Read-After-Write verification: guarantee job actually exists in Global Queue before returning!
	verifyReq, _ := http.NewRequestWithContext(ctx, http.MethodGet, m.apiBaseURL+"/api/v1/queue/jobs/"+created.ID, nil)
	verifyResp, err := m.httpClient.Do(verifyReq)
	if err != nil {
		return nil, fmt.Errorf("read-after-write verification failed: %w", err)
	}
	defer verifyResp.Body.Close()
	if verifyResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("hallucination guard triggered: job %s not found after enqueue", created.ID)
	}

	var verified Job
	if err := json.NewDecoder(verifyResp.Body).Decode(&verified); err != nil {
		return nil, err
	}
	return &verified, nil
}

// ============================================================================
// 4. AGENT & HARNESS (iazio-agent + iazio-harness executing agy)
// ============================================================================

type IazioHarness struct {
	AgyBinaryPath string
	WorkspaceDir  string
	APIBaseURL    string
}

func (h *IazioHarness) captureGitSHAs(ctx context.Context) (headSHA string, treeSHA string, err error) {
	headOut, err := exec.CommandContext(ctx, "git", "-C", h.WorkspaceDir, "rev-parse", "HEAD").Output()
	if err != nil {
		return "", "", fmt.Errorf("git rev-parse HEAD failed: %w", err)
	}
	treeOut, err := exec.CommandContext(ctx, "git", "-C", h.WorkspaceDir, "rev-parse", "HEAD^{tree}").Output()
	if err != nil {
		return "", "", fmt.Errorf("git rev-parse HEAD^{tree} failed: %w", err)
	}
	return strings.TrimSpace(string(headOut)), strings.TrimSpace(string(treeOut)), nil
}

func (h *IazioHarness) streamLogChunk(ctx context.Context, jobID string, fencingToken uint64, chunk string) {
	if h.APIBaseURL == "" {
		return
	}
	body, _ := json.Marshal(map[string]any{
		"fencing_token": fencingToken,
		"chunk":         chunk,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/api/v1/queue/jobs/%s/logs", h.APIBaseURL, jobID),
		bytes.NewReader(body))
	if err == nil {
		client := &http.Client{Timeout: 2 * time.Second}
		resp, doErr := client.Do(req)
		if doErr == nil {
			_ = resp.Body.Close()
		}
	}
}

func (h *IazioHarness) Execute(
	ctx context.Context,
	job *Job,
	onStoryAllocated func(string),
) (allocatedStoryID string, output string, proof ExecutionProof, err error) {
	startTime := time.Now()

	// Capture Pre-execution Git state
	preHeadSHA, preTreeSHA, _ := h.captureGitSHAs(ctx)

	cmd := exec.CommandContext(ctx, h.AgyBinaryPath,
		"--job-id", job.ID,
		"--fencing-token", fmt.Sprintf("%d", job.FencingToken),
		"--story-id", job.StoryID,
		"--story", job.Story,
	)
	cmd.Dir = h.WorkspaceDir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", "", proof, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return "", "", proof, err
	}

	agentPID := 0
	if cmd.Process != nil {
		agentPID = cmd.Process.Pid
	}

	allocatedStoryID = job.StoryID
	var outBuf bytes.Buffer
	var stdoutLineCount int

	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		line := scanner.Text()
		stdoutLineCount++
		chunk := line + "\n"
		outBuf.WriteString(chunk)

		// Real-time log streaming to iazio-api
		h.streamLogChunk(ctx, job.ID, job.FencingToken, chunk)

		if strings.HasPrefix(line, "ALLOCATED_STORY_ID:") {
			id := strings.TrimSpace(strings.TrimPrefix(line, "ALLOCATED_STORY_ID:"))
			allocatedStoryID = id
			if onStoryAllocated != nil {
				onStoryAllocated(id)
			}
		}
	}

	waitErr := cmd.Wait()
	durationMs := time.Since(startTime).Milliseconds()

	// Capture Post-execution Git state
	postHeadSHA, postTreeSHA, _ := h.captureGitSHAs(ctx)

	rawTranscript := outBuf.String()
	totalBytes := len(rawTranscript)

	proof = ExecutionProof{
		PreGitHeadSHA:    preHeadSHA,
		PostGitHeadSHA:   postHeadSHA,
		PreGitTreeSHA:    preTreeSHA,
		PostGitTreeSHA:   postTreeSHA,
		AllocatedStoryID: allocatedStoryID,
		StdoutBytes:      totalBytes,
		StdoutLineCount:  stdoutLineCount,
		RawTranscript:    rawTranscript,
		AgentPID:         agentPID,
		DurationMs:       durationMs,
	}

	combinedOutput := rawTranscript + stderr.String()

	// If child process exited 0, enforce Proof-of-Execution invariants
	if waitErr == nil {
		if poeErr := VerifyExecutionProof(job.Action, proof); poeErr != nil {
			return allocatedStoryID, combinedOutput, proof, poeErr
		}
	}

	return allocatedStoryID, combinedOutput, proof, waitErr
}

type IazioAgent struct {
	AgentID           string
	TargetAgent       string
	APIBaseURL        string
	Harness           *IazioHarness
	HeartbeatInterval time.Duration
	MaxExecutionTime  time.Duration
	PauseHeartbeats   bool // Used in tests to simulate network partition / hung agent
	mu                sync.Mutex
}

func (a *IazioAgent) SetPauseHeartbeats(pause bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.PauseHeartbeats = pause
}

func (a *IazioAgent) isPaused() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.PauseHeartbeats
}

func (a *IazioAgent) ClaimAndExecuteOnce(ctx context.Context) (*Job, error) {
	httpClient := &http.Client{Timeout: 5 * time.Second}

	// 1. Claim job from Global Queue
	claimBody, _ := json.Marshal(map[string]string{
		"agent_id":     a.AgentID,
		"target_agent": a.TargetAgent,
	})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, a.APIBaseURL+"/api/v1/queue/claim", bytes.NewReader(claimBody))
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return nil, ErrNoPendingJob
	}
	var job Job
	if err := json.NewDecoder(resp.Body).Decode(&job); err != nil {
		return nil, err
	}

	// 2. Start decoupled background heartbeat loop
	execCtx, execCancel := context.WithTimeout(ctx, a.MaxExecutionTime)
	defer execCancel()

	var storyMu sync.Mutex
	currentStoryID := job.StoryID

	sendHeartbeat := func(storyID string) error {
		hbBody, _ := json.Marshal(map[string]any{
			"fencing_token": job.FencingToken,
			"story_id":      storyID,
		})
		hbReq, _ := http.NewRequestWithContext(ctx, http.MethodPost,
			fmt.Sprintf("%s/api/v1/queue/jobs/%s/heartbeat", a.APIBaseURL, job.ID),
			bytes.NewReader(hbBody))
		hbResp, err := httpClient.Do(hbReq)
		if err != nil {
			return err
		}
		defer hbResp.Body.Close()
		if hbResp.StatusCode == http.StatusConflict {
			return ErrStaleFencingToken
		}
		return nil
	}

	hbErrCh := make(chan error, 1)
	doneCh := make(chan struct{})

	go func() {
		ticker := time.NewTicker(a.HeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-doneCh:
				return
			case <-execCtx.Done():
				return
			case <-ticker.C:
				if a.isPaused() {
					continue
				}
				storyMu.Lock()
				sid := currentStoryID
				storyMu.Unlock()

				if err := sendHeartbeat(sid); errors.Is(err, ErrStaleFencingToken) {
					hbErrCh <- err
					execCancel() // Kill zombie harness & agy immediately!
					return
				}
			}
		}
	}()

	// 3. Execute prompt via iazio-harness -> agy
	allocatedID, output, proof, execErr := a.Harness.Execute(execCtx, &job, func(newStoryID string) {
		storyMu.Lock()
		currentStoryID = newStoryID
		storyMu.Unlock()
		if !a.isPaused() {
			_ = sendHeartbeat(newStoryID)
		}
	})
	close(doneCh)

	select {
	case hbErr := <-hbErrCh:
		return &job, hbErr
	default:
	}

	// 4. Closeout job with FencingToken CAS and Proof of Execution
	finalStatus := StatusCompleted
	errMsg := ""
	if execErr != nil {
		finalStatus = StatusFailed
		errMsg = execErr.Error()
	}

	closeBody, _ := json.Marshal(map[string]any{
		"fencing_token":   job.FencingToken,
		"status":          finalStatus,
		"story_id":        allocatedID,
		"output_log":      output,
		"error_msg":       errMsg,
		"execution_proof": proof,
	})
	closeReq, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/api/v1/queue/jobs/%s/closeout", a.APIBaseURL, job.ID),
		bytes.NewReader(closeBody))
	closeResp, err := httpClient.Do(closeReq)
	if err != nil {
		return &job, err
	}
	defer closeResp.Body.Close()
	if closeResp.StatusCode == http.StatusConflict {
		return &job, ErrStaleFencingToken
	}

	return &job, execErr
}

// ============================================================================
// 5. MOCK AGY CLI BUILDER & INTEGRATION TESTS
// ============================================================================

func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	runCmd := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, string(out))
		}
	}
	runCmd("init")
	runCmd("config", "user.name", "test-agent")
	runCmd("config", "user.email", "test@example.com")
	initFile := filepath.Join(dir, "README.md")
	if err := os.WriteFile(initFile, []byte("# Initial Workspace Repo\n"), 0600); err != nil {
		t.Fatalf("failed to write initial file: %v", err)
	}
	runCmd("add", ".")
	runCmd("commit", "-m", "initial commit")
}

// buildMockAgyCLI compiles a real executable binary that acts as the `agy` CLI tool.
func buildMockAgyCLI(t *testing.T) string {
	t.Helper()
	tmpDir := t.TempDir()
	srcPath := filepath.Join(tmpDir, "main.go")
	binPath := filepath.Join(tmpDir, "mock-agy")

	mockSrc := `package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

func main() {
	jobID := flag.String("job-id", "", "Job ID")
	fencingToken := flag.Uint64("fencing-token", 0, "Fencing Token")
	storyID := flag.String("story-id", "", "Story ID")
	story := flag.String("story", "", "Story Title")
	flag.Parse()

	scenario := os.Getenv("MOCK_AGY_SCENARIO")
	if scenario == "noop_bypass" {
		// Simulate fake/noop run: sleep and exit 0 without mutating git or emitting valid telemetry
		time.Sleep(50 * time.Millisecond)
		if os.Getenv("MOCK_AGY_SYNTHETIC_OUTPUT") == "1" {
			fmt.Printf("[+] prompt executed via agy (exit 0)\n")
		}
		os.Exit(0)
	}

	allocatedID := "STORY-GOV-3715"
	if *storyID != "" && *storyID != "pending-allocation" {
		allocatedID = *storyID
	}
	fmt.Printf("ALLOCATED_STORY_ID: %s\n", allocatedID)

	sleepDuration := 250 * time.Millisecond
	if envSleep := os.Getenv("MOCK_AGY_SLEEP_MS"); envSleep != "" {
		if d, err := time.ParseDuration(envSleep + "ms"); err == nil {
			sleepDuration = d
		}
	}

	// Mutate Git working tree and commit
	workDir, _ := os.Getwd()
	storyFile := filepath.Join(workDir, "story_artifact.txt")
	_ = os.WriteFile(storyFile, []byte(fmt.Sprintf("Story execution artifact for %s\n", allocatedID)), 0644)
	_ = exec.Command("git", "add", ".").Run()
	_ = exec.Command("git", "commit", "-m", fmt.Sprintf("feat: implement story %s", allocatedID)).Run()

	// Simulate deep work across multiple lease intervals
	time.Sleep(sleepDuration)

	// Stream detailed real telemetry (> 256 bytes, >= 5 lines)
	fmt.Printf("[agy-telemetry] Initializing autonomous story execution engine for job=%s\n", *jobID)
	fmt.Printf("[agy-telemetry] Fencing token bound: token=%d story=%q\n", *fencingToken, *story)
	fmt.Printf("[agy-telemetry] Repository mutations committed to git tree successfully\n")
	fmt.Printf("[agy-telemetry] Running verification gates: validate-local, lint, and unit tests\n")
	fmt.Printf("[agy-telemetry] All verification checks green: proof of execution ready\n")
	fmt.Printf("EXECUTED_ON_AGY job=%s token=%d story=%q\n", *jobID, *fencingToken, *story)
}
`
	if err := os.WriteFile(srcPath, []byte(mockSrc), 0600); err != nil {
		t.Fatalf("failed to write mock-agy source: %v", err)
	}
	cmd := exec.Command("go", "build", "-o", binPath, srcPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to compile mock-agy binary: %v\n%s", err, string(out))
	}
	return binPath
}

// Test 1: Proves full E2E flow (MCP -> API -> Agent -> Harness -> mock-agy)
// and proves a silent long-running job (300ms > 100ms LeaseTTL) DOES NOT TIMEOUT
// because decoupled heartbeats renew the lease every 25ms, and satisfies PoE.
func TestE2E_MCP_Queue_Agent_Harness_MockAgy_NoPrematureTimeout(t *testing.T) {
	mockAgyBin := buildMockAgyCLI(t)
	t.Setenv("MOCK_AGY_SLEEP_MS", "300") // 3x longer than the 100ms lease TTL!
	t.Setenv("MOCK_AGY_SCENARIO", "legitimate")

	leaseTTL := 100 * time.Millisecond
	store := NewQueueStore(leaseTTL)
	server := NewIazioAPIServer(store)
	defer server.Close()

	wsDir := t.TempDir()
	initGitRepo(t, wsDir)

	// Background Reaper running every 30ms
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		ticker := time.NewTicker(30 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				store.ReapExpiredLeases()
			}
		}
	}()

	// Step 1: Enqueue via iazio-mcp (with Read-After-Write check)
	mcpClient := NewIazioMCPClient(server.URL)
	enqueuedJob, err := mcpClient.QueueAutonomousStoryExecution(
		ctx,
		".",
		"agy",
		"Enforce Monotonic Fencing Token CAS Across Iazio Queue, Agent Heartbeats, and Story Closeout",
	)
	if err != nil {
		t.Fatalf("MCP enqueue failed: %v", err)
	}
	if enqueuedJob.Status != StatusPending {
		t.Fatalf("expected pending status, got %s", enqueuedJob.Status)
	}

	// Step 2: iazio-agent claims & executes via iazio-harness -> mock-agy
	agent := &IazioAgent{
		AgentID:           "agent-mac-mini-01",
		TargetAgent:       "agy",
		APIBaseURL:        server.URL,
		HeartbeatInterval: 25 * time.Millisecond,
		MaxExecutionTime:  10 * time.Second,
		Harness: &IazioHarness{
			AgyBinaryPath: mockAgyBin,
			WorkspaceDir:  wsDir,
			APIBaseURL:    server.URL,
		},
	}

	claimedJob, err := agent.ClaimAndExecuteOnce(ctx)
	if err != nil {
		t.Fatalf("Agent execution failed (premature timeout or error): %v", err)
	}
	if claimedJob.FencingToken != 1 {
		t.Fatalf("expected fencing_token=1, got %d", claimedJob.FencingToken)
	}

	// Step 3: Verify final state in iazio-api Global Queue
	finalJob, err := store.Get(enqueuedJob.ID)
	if err != nil {
		t.Fatalf("failed to fetch final job: %v", err)
	}
	if finalJob.Status != StatusCompleted {
		t.Fatalf("expected job status 'completed', got %q (error: %s)", finalJob.Status, finalJob.ErrorMsg)
	}
	if finalJob.StoryID != "STORY-GOV-3715" {
		t.Fatalf("expected dynamic story ID 'STORY-GOV-3715', got %q", finalJob.StoryID)
	}
	if !strings.Contains(finalJob.OutputLog, "EXECUTED_ON_AGY") {
		t.Fatalf("expected mock-agy output in job log, got: %s", finalJob.OutputLog)
	}
	if finalJob.ExecutionProof == nil {
		t.Fatalf("expected execution proof recorded on job")
	}
	if finalJob.ExecutionProof.PreGitHeadSHA == finalJob.ExecutionProof.PostGitHeadSHA {
		t.Fatalf("expected Git HEAD mutation proof, got unchanged SHA")
	}
}

// Test 2: Proves Monotonic Fencing Token CAS prevents split-brain overwrites.
// Agent 1 claims (token=1) -> network freezes -> Reaper re-queues -> Agent 2 claims (token=2)
// -> Agent 1 unfreezes -> API rejects Agent 1 with 409 Conflict -> Agent 2 completes cleanly.
func TestE2E_FencingTokenCAS_PreventsSplitBrainZombieOverwrite(t *testing.T) {
	mockAgyBin := buildMockAgyCLI(t)
	t.Setenv("MOCK_AGY_SLEEP_MS", "400")
	t.Setenv("MOCK_AGY_SCENARIO", "legitimate")

	leaseTTL := 80 * time.Millisecond
	store := NewQueueStore(leaseTTL)
	server := NewIazioAPIServer(store)
	defer server.Close()

	wsDir1 := t.TempDir()
	initGitRepo(t, wsDir1)
	wsDir2 := t.TempDir()
	initGitRepo(t, wsDir2)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mcpClient := NewIazioMCPClient(server.URL)
	job, err := mcpClient.QueueAutonomousStoryExecution(ctx, ".", "agy", "Test Split-Brain Fencing CAS")
	if err != nil {
		t.Fatalf("enqueue failed: %v", err)
	}

	// Agent 1 has paused heartbeats (simulating a hung/partitioned node)
	agent1 := &IazioAgent{
		AgentID:           "zombie-agent-01",
		TargetAgent:       "agy",
		APIBaseURL:        server.URL,
		HeartbeatInterval: 25 * time.Millisecond,
		MaxExecutionTime:  10 * time.Second,
		PauseHeartbeats:   true,
		Harness: &IazioHarness{
			AgyBinaryPath: mockAgyBin,
			WorkspaceDir:  wsDir1,
			APIBaseURL:    server.URL,
		},
	}

	agent1ErrCh := make(chan error, 1)
	go func() {
		_, err := agent1.ClaimAndExecuteOnce(ctx)
		agent1ErrCh <- err
	}()

	// Wait for Agent 1's 80ms lease to expire, then reap it
	time.Sleep(130 * time.Millisecond)
	if reaped := store.ReapExpiredLeases(); reaped != 1 {
		t.Fatalf("expected reaper to reap 1 expired job, got %d", reaped)
	}

	// Agent 2 claims the re-queued job (receives monotonic fencing_token = 2)
	t.Setenv("MOCK_AGY_SLEEP_MS", "100")
	agent2 := &IazioAgent{
		AgentID:           "healthy-agent-02",
		TargetAgent:       "agy",
		APIBaseURL:        server.URL,
		HeartbeatInterval: 20 * time.Millisecond,
		MaxExecutionTime:  10 * time.Second,
		Harness: &IazioHarness{
			AgyBinaryPath: mockAgyBin,
			WorkspaceDir:  wsDir2,
			APIBaseURL:    server.URL,
		},
	}

	// Unpause Agent 1 so it tries to send a stale heartbeat/closeout with token=1
	agent1.SetPauseHeartbeats(false)

	claimedBy2, err := agent2.ClaimAndExecuteOnce(ctx)
	if err != nil {
		t.Fatalf("healthy Agent 2 failed to execute re-queued job: %v", err)
	}
	if claimedBy2.FencingToken != 2 {
		t.Fatalf("expected Agent 2 to receive fencing_token=2, got %d", claimedBy2.FencingToken)
	}

	// Agent 1 must have been rejected with ErrStaleFencingToken (409 Conflict)
	err1 := <-agent1ErrCh
	if !errors.Is(err1, ErrStaleFencingToken) {
		t.Fatalf("expected zombie Agent 1 to be rejected with ErrStaleFencingToken, got: %v", err1)
	}

	// Verify final job state belongs to Agent 2 (token=2, completed)
	finalJob, _ := store.Get(job.ID)
	if finalJob.Status != StatusCompleted || finalJob.FencingToken != 2 || finalJob.AgentID != "healthy-agent-02" {
		t.Fatalf("corrupted final state: status=%s token=%d agent=%s",
			finalJob.Status, finalJob.FencingToken, finalJob.AgentID)
	}
}

// Test 3: Read-After-Write Enqueue Verification prevents hallucinated/uncommitted IDs.
func TestE2E_MCP_ReadAfterWriteVerification(t *testing.T) {
	mux := http.NewServeMux()
	// Server returns 201 with a fake job ID, but GET returns 404
	mux.HandleFunc("POST /api/v1/queue/jobs", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(Job{ID: "job_ghost_fake_1234", Status: StatusPending})
	})
	mux.HandleFunc("GET /api/v1/queue/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "job not found", http.StatusNotFound)
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	client := NewIazioMCPClient(server.URL)
	_, err := client.QueueAutonomousStoryExecution(context.Background(), ".", "agy", "Verify Read After Write")
	if err == nil {
		t.Fatalf("expected read-after-write verification to fail-closed on 404 job")
	}
	if !strings.Contains(err.Error(), "hallucination guard triggered") {
		t.Fatalf("expected error mentioning hallucination guard, got: %v", err)
	}
}

// Test 4: Proof-of-Execution Gate catches no-op bypass and marks job failed.
func TestE2E_MockAgy_NoOpBypass_DetectedAndFailed(t *testing.T) {
	mockAgyBin := buildMockAgyCLI(t)
	t.Setenv("MOCK_AGY_SCENARIO", "noop_bypass")
	t.Setenv("MOCK_AGY_SYNTHETIC_OUTPUT", "1")

	leaseTTL := 2 * time.Second
	store := NewQueueStore(leaseTTL)
	server := NewIazioAPIServer(store)
	defer server.Close()

	wsDir := t.TempDir()
	initGitRepo(t, wsDir)

	mcpClient := NewIazioMCPClient(server.URL)
	enqueuedJob, err := mcpClient.QueueAutonomousStoryExecution(
		context.Background(),
		".",
		"agy",
		"Attempt No-Op Bypass Execution",
	)
	if err != nil {
		t.Fatalf("MCP enqueue failed: %v", err)
	}

	agent := &IazioAgent{
		AgentID:           "test-agent-noop",
		TargetAgent:       "agy",
		APIBaseURL:        server.URL,
		HeartbeatInterval: 50 * time.Millisecond,
		MaxExecutionTime:  5 * time.Second,
		Harness: &IazioHarness{
			AgyBinaryPath: mockAgyBin,
			WorkspaceDir:  wsDir,
			APIBaseURL:    server.URL,
		},
	}

	_, execErr := agent.ClaimAndExecuteOnce(context.Background())
	if execErr == nil {
		t.Fatalf("expected ClaimAndExecuteOnce to fail on no-op bypass run")
	}
	if !errors.Is(execErr, ErrBypassOrNoOpDetected) && !strings.Contains(execErr.Error(), "BYPASS_OR_NOOP_DETECTED") {
		t.Fatalf("expected error mentioning BYPASS_OR_NOOP_DETECTED, got: %v", execErr)
	}

	// Verify job in database is marked failed with BYPASS_OR_NOOP_DETECTED
	finalJob, err := store.Get(enqueuedJob.ID)
	if err != nil {
		t.Fatalf("failed to fetch final job: %v", err)
	}
	if finalJob.Status != StatusFailed {
		t.Fatalf("expected final job status 'failed', got: %s", finalJob.Status)
	}
	if !strings.Contains(finalJob.ErrorMsg, "BYPASS_OR_NOOP_DETECTED") {
		t.Fatalf("expected final job error_msg to contain BYPASS_OR_NOOP_DETECTED, got: %q", finalJob.ErrorMsg)
	}
}

// Test 5: POST /api/v1/queue/jobs strictly rejects client-supplied IDs.
func TestE2E_API_RejectsClientSuppliedJobID(t *testing.T) {
	store := NewQueueStore(10 * time.Second)
	server := NewIazioAPIServer(store)
	defer server.Close()

	payload := map[string]string{
		"id":           "job_3060a0cb6d5e7c71",
		"action":       "/autonomous-story-execution",
		"target_repo":  ".",
		"target_agent": "agy",
		"story":        "Inject custom job ID",
		"story_id":     "STORY-GOV-9999",
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost, server.URL+"/api/v1/queue/jobs", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request when client supplies job id, got %d", resp.StatusCode)
	}
}

// Test 6: Real-time stdout/stderr log chunk streaming via POST /api/v1/queue/jobs/{id}/logs
func TestE2E_LiveLogStreaming(t *testing.T) {
	mockAgyBin := buildMockAgyCLI(t)
	t.Setenv("MOCK_AGY_SCENARIO", "legitimate")
	t.Setenv("MOCK_AGY_SLEEP_MS", "50")

	leaseTTL := 2 * time.Second
	store := NewQueueStore(leaseTTL)
	server := NewIazioAPIServer(store)
	defer server.Close()

	wsDir := t.TempDir()
	initGitRepo(t, wsDir)

	mcpClient := NewIazioMCPClient(server.URL)
	job, err := mcpClient.QueueAutonomousStoryExecution(context.Background(), ".", "agy", "Live Log Stream Test")
	if err != nil {
		t.Fatalf("enqueue failed: %v", err)
	}

	agent := &IazioAgent{
		AgentID:           "test-agent-streaming",
		TargetAgent:       "agy",
		APIBaseURL:        server.URL,
		HeartbeatInterval: 50 * time.Millisecond,
		MaxExecutionTime:  5 * time.Second,
		Harness: &IazioHarness{
			AgyBinaryPath: mockAgyBin,
			WorkspaceDir:  wsDir,
			APIBaseURL:    server.URL,
		},
	}

	_, err = agent.ClaimAndExecuteOnce(context.Background())
	if err != nil {
		t.Fatalf("execution failed: %v", err)
	}

	finalJob, err := store.Get(job.ID)
	if err != nil {
		t.Fatalf("failed to get job: %v", err)
	}
	if !strings.Contains(finalJob.OutputLog, "[agy-telemetry]") {
		t.Fatalf("expected OutputLog to contain streamed [agy-telemetry] chunks, got: %s", finalJob.OutputLog)
	}
	if !strings.Contains(finalJob.OutputLog, "EXECUTED_ON_AGY") {
		t.Fatalf("expected OutputLog to contain EXECUTED_ON_AGY, got: %s", finalJob.OutputLog)
	}
}
