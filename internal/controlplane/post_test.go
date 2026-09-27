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
