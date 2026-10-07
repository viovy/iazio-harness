package controlplane

import (
	"context"
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


