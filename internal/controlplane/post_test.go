package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPostChunk(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/jobs/job-1/chunks" {
			t.Fatalf("path %s", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	if err := PostChunk(context.Background(), srv.URL, "tok", "job-1", "stdout", "building"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "building") || strings.Contains(body, "ANSI") {
		t.Fatalf("body %s", body)
	}
}

func TestPostTick(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/jobs/job-1/chunks" {
			t.Fatalf("path %s", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	if err := PostTick(context.Background(), srv.URL, "tok", "job-1", 5000); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, `"type":"OUTPUT_TICK"`) || !strings.Contains(body, `"silent_for_ms":5000`) {
		t.Fatalf("unexpected body: %s", body)
	}
}

func TestGetJob(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/jobs/job-123" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-tok" {
			t.Fatalf("unexpected auth: %s", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"job-123","engine":"copilot","prompt":"run test","story_id":"STORY-1"}`))
	}))
	defer srv.Close()

	job, err := GetJob(context.Background(), srv.URL, "test-tok", "job-123")
	if err != nil {
		t.Fatalf("GetJob failed: %v", err)
	}
	if job.ID != "job-123" || job.Engine != "copilot" || job.StoryID != "STORY-1" {
		t.Fatalf("unexpected job result: %+v", job)
	}
}

func TestPostExit(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/jobs/job-123/exit" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := PostExit(context.Background(), srv.URL, "test-tok", "job-123", 0); err != nil {
		t.Fatalf("PostExit failed: %v", err)
	}
	if !strings.Contains(body, `"exit_code":0`) {
		t.Fatalf("unexpected body: %s", body)
	}
}

func TestPostStoryDraft(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/jobs/job-123/story-draft" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	if err := PostStoryDraft(context.Background(), srv.URL, "test-tok", "job-123", "STORY-1", "# Story Draft"); err != nil {
		t.Fatalf("PostStoryDraft failed: %v", err)
	}
	if !strings.Contains(body, "STORY-1") || !strings.Contains(body, "# Story Draft") {
		t.Fatalf("unexpected body: %s", body)
	}
}

func TestPostConversation(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/jobs/job-123/conversations" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-tok" {
			t.Fatalf("unexpected auth: %s", r.Header.Get("Authorization"))
		}
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := PostConversation(context.Background(), srv.URL, "test-tok", "job-123", "conv-uuid-789"); err != nil {
		t.Fatalf("PostConversation failed: %v", err)
	}
	if !strings.Contains(body, `"conversation_id":"conv-uuid-789"`) {
		t.Fatalf("unexpected body: %s", body)
	}
}

func TestTransientStatusAndError(t *testing.T) {
	if !IsTransientStatus(502) || !IsTransientStatus(503) || !IsTransientStatus(504) || !IsTransientStatus(429) {
		t.Fatal("expected transient statuses")
	}
	if IsTransientStatus(400) || IsTransientStatus(404) || IsTransientStatus(500) {
		t.Fatal("non-transient status considered transient")
	}
	if !IsTransientError(errors.New("dial tcp: connection refused")) {
		t.Fatal("connection refused should be transient error")
	}
}

func TestPostExitTransientRetrySuccess(t *testing.T) {
	var attempts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts < 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`updating`))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	err := PostExit(context.Background(), srv.URL, "tok", "job-1", 0)
	if err != nil {
		t.Fatalf("expected success on retry, got: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("expected 2 attempts, got %d", attempts)
	}
}

func TestGetJobTransientRetrySuccess(t *testing.T) {
	var attempts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts < 2 {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`gateway restart`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"job-1","engine":"agy"}`))
	}))
	defer srv.Close()

	job, err := GetJob(context.Background(), srv.URL, "tok", "job-1")
	if err != nil {
		t.Fatalf("expected success on retry, got: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("expected 2 attempts, got %d", attempts)
	}
	if job.ID != "job-1" {
		t.Fatalf("expected ID job-1, got %s", job.ID)
	}
}

func TestControlplaneExtraCoverage(t *testing.T) {
	// 1. PostExitWithOptions
	var receivedOpts ExitOptions
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/exit") {
			_ = json.NewDecoder(r.Body).Decode(&receivedOpts)
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.URL.Path == "/v1/jobs/bad-json" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("{invalid-json"))
			return
		}
		if r.URL.Path == "/v1/jobs/not-found" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/fail-post") {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	opts := ExitOptions{
		ExitCode:       2,
		Reason:         "QUOTA",
		DiagnosticCode: "QUOTA_EXHAUSTED",
		Engine:         "agy",
		Model:          "gemini-flash",
		Remediation:    "Switch model",
		ASEComplete:    false,
	}
	err := PostExitWithOptions(context.Background(), srv.URL, "tok", "j-opt", opts)
	if err != nil {
		t.Fatalf("PostExitWithOptions failed: %v", err)
	}
	if receivedOpts.Reason != "QUOTA" || receivedOpts.DiagnosticCode != "QUOTA_EXHAUSTED" {
		t.Errorf("unexpected options: %+v", receivedOpts)
	}

	// 2. GetJob errors
	_, err = GetJob(context.Background(), srv.URL, "tok", "bad-json")
	if err == nil || !strings.Contains(err.Error(), "decode job") {
		t.Errorf("expected decode job error, got %v", err)
	}

	_, err = GetJob(context.Background(), srv.URL, "tok", "not-found")
	if err == nil || !strings.Contains(err.Error(), "status 404") {
		t.Errorf("expected 404 status error, got %v", err)
	}

	// 3. doPostWithRetry non-transient error
	err = doPostWithRetry(context.Background(), srv.URL+"/v1/jobs/fail-post", "tok", []byte(`{}`), "test-op")
	if err == nil || !strings.Contains(err.Error(), "test-op: 400") {
		t.Errorf("expected 400 error from doPostWithRetry, got %v", err)
	}

	// 4. Transient error checks
	if !IsTransientError(errors.New("connection reset by peer")) {
		t.Error("expected connection reset to be transient")
	}
	if !IsTransientError(errors.New("broken pipe")) {
		t.Error("expected broken pipe to be transient")
	}
	if !IsTransientError(errors.New("unexpected EOF")) {
		t.Error("expected EOF to be transient")
	}
	if IsTransientError(nil) {
		t.Error("expected nil to not be transient")
	}
	if IsTransientError(errors.New("permission denied")) {
		t.Error("expected permission denied to not be transient")
	}

	// 5. Canceled context in doPostWithRetry and GetJob
	ctxCanceled, cancel := context.WithCancel(context.Background())
	cancel()
	_ = doPostWithRetry(ctxCanceled, srv.URL, "tok", []byte(`{}`), "canceled-op")
	_, _ = GetJob(ctxCanceled, srv.URL, "tok", "job-1")

	// 6. Non-retryable GetJob on non-transient status
	_, _ = GetJob(context.Background(), srv.URL, "tok", "fail-post")
}
