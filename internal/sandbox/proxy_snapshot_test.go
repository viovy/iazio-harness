package sandbox

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRecordReplayProxy(t *testing.T) {
	dir := t.TempDir()
	cassettePath := filepath.Join(dir, "test_cassette.json")

	// Upstream test server
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Custom", "upstream-val")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"result":"live upstream response"}`))
	}))
	defer upstream.Close()

	// 1. Record Mode
	recordProxy, err := NewRecordReplayProxy(ProxyModeRecord, cassettePath, upstream.URL)
	if err != nil {
		t.Fatalf("NewRecordReplayProxy record: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/test", nil)
	rec := httptest.NewRecorder()
	recordProxy.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("record status: %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "live upstream response") {
		t.Fatalf("unexpected record body: %s", rec.Body.String())
	}

	// Verify cassette was saved to disk
	if _, err := os.Stat(cassettePath); err != nil {
		t.Fatalf("cassette not saved: %v", err)
	}

	// 2. Shut down upstream to prove Replay works offline
	upstream.Close()

	replayProxy, err := NewRecordReplayProxy(ProxyModeReplay, cassettePath, "")
	if err != nil {
		t.Fatalf("NewRecordReplayProxy replay: %v", err)
	}

	// Match existing request
	reqReplay := httptest.NewRequest(http.MethodGet, "/v1/test", nil)
	recReplay := httptest.NewRecorder()
	replayProxy.ServeHTTP(recReplay, reqReplay)

	if recReplay.Code != http.StatusOK {
		t.Fatalf("replay status: %d, body: %s", recReplay.Code, recReplay.Body.String())
	}
	if !strings.Contains(recReplay.Body.String(), "live upstream response") {
		t.Fatalf("unexpected replay body: %s", recReplay.Body.String())
	}
	if recReplay.Header().Get("X-Custom") != "upstream-val" {
		t.Fatalf("missing replay header: %v", recReplay.Header())
	}

	// Non-matching request should fail hermetically
	reqUnknown := httptest.NewRequest(http.MethodGet, "/v1/unknown", nil)
	recUnknown := httptest.NewRecorder()
	replayProxy.ServeHTTP(recUnknown, reqUnknown)

	if recUnknown.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 for unmatched replay call, got %d", recUnknown.Code)
	}
}

func TestWorkspaceSnapshotAndDiff(t *testing.T) {
	dir := t.TempDir()
	sourceDir := filepath.Join(dir, "workspace")
	if err := os.MkdirAll(sourceDir, 0755); err != nil {
		t.Fatal(err)
	}

	f1 := filepath.Join(sourceDir, "file1.txt")
	f2 := filepath.Join(sourceDir, "file2.txt")
	_ = os.WriteFile(f1, []byte("original content 1"), 0644)
	_ = os.WriteFile(f2, []byte("original content 2"), 0644)

	// Create snapshot
	snap, err := CreateSnapshot(sourceDir, "snap-v1")
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	defer snap.Cleanup()

	if len(snap.FileHashes) != 2 {
		t.Fatalf("expected 2 files in snapshot, got %d", len(snap.FileHashes))
	}

	// Mutate workspace: modify file1, delete file2, add file3
	_ = os.WriteFile(f1, []byte("modified content 1"), 0644)
	_ = os.Remove(f2)
	f3 := filepath.Join(sourceDir, "file3.txt")
	_ = os.WriteFile(f3, []byte("new file 3"), 0644)

	added, modified, deleted, err := ComputeDiff(snap, sourceDir)
	if err != nil {
		t.Fatalf("ComputeDiff: %v", err)
	}

	if len(added) != 1 || added[0] != "file3.txt" {
		t.Errorf("expected added [file3.txt], got %v", added)
	}
	if len(modified) != 1 || modified[0] != "file1.txt" {
		t.Errorf("expected modified [file1.txt], got %v", modified)
	}
	if len(deleted) != 1 || deleted[0] != "file2.txt" {
		t.Errorf("expected deleted [file2.txt], got %v", deleted)
	}

	// Restore snapshot
	if err := RestoreSnapshot(snap, sourceDir); err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}

	restoredContent, _ := os.ReadFile(f1)
	if string(restoredContent) != "original content 1" {
		t.Errorf("expected restored file1 content, got %s", string(restoredContent))
	}

	restoredF2, _ := os.ReadFile(f2)
	if string(restoredF2) != "original content 2" {
		t.Errorf("expected restored file2 content, got %s", string(restoredF2))
	}

	// Cleanup snapshot
	if err := snap.Cleanup(); err != nil {
		t.Errorf("snap.Cleanup: %v", err)
	}

	// Cleanup nil and empty checks
	var nilSnap *WorkspaceSnapshot
	if err := nilSnap.Cleanup(); err != nil {
		t.Errorf("expected nil from nilSnap.Cleanup, got %v", err)
	}
	emptySnap := &WorkspaceSnapshot{}
	if err := emptySnap.Cleanup(); err != nil {
		t.Errorf("expected nil from emptySnap.Cleanup, got %v", err)
	}

	var nilBox *Sandbox
	if err := nilBox.Cleanup(); err != nil {
		t.Errorf("expected nil from nilBox.Cleanup, got %v", err)
	}
	if _, err := nilBox.Diff(); err == nil {
		t.Errorf("expected error from nilBox.Diff")
	}

	emptyBox := &Sandbox{}
	if err := emptyBox.Cleanup(); err != nil {
		t.Errorf("expected nil from emptyBox.Cleanup, got %v", err)
	}
	if _, err := emptyBox.Diff(); err == nil {
		t.Errorf("expected error from emptyBox.Diff")
	}

	// Real sandbox lifecycle
	sbDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(sbDir, "f.txt"), []byte("hello"), 0o644)
	realBox, err := Create(sbDir, Options{})
	if err != nil {
		t.Fatalf("Create sandbox failed: %v", err)
	}
	diffOut, err := realBox.Diff()
	if err != nil {
		t.Fatalf("realBox.Diff failed: %v", err)
	}
	if diffOut != "" {
		t.Errorf("expected empty diff on fresh clone, got %s", diffOut)
	}
	if err := realBox.Cleanup(); err != nil {
		t.Errorf("realBox.Cleanup failed: %v", err)
	}

	// Proxy empty upstream and cassette paths
	noUpstreamProxy := &RecordReplayProxy{}
	if _, err := noUpstreamProxy.forwardToUpstream(httptest.NewRequest(http.MethodGet, "/x", nil), nil); err == nil {
		t.Errorf("expected error when forwardToUpstream with empty upstreamURL")
	}
	if err := noUpstreamProxy.LoadCassette(); err != nil {
		t.Errorf("expected nil for LoadCassette with empty cassettePath, got %v", err)
	}
	if err := noUpstreamProxy.saveCassetteLocked(); err != nil {
		t.Errorf("expected nil for saveCassetteLocked with empty cassettePath, got %v", err)
	}

	// AddInteraction
	noUpstreamProxy.AddInteraction(RecordedInteraction{
		Method:       "GET",
		URL:          "http://test.com",
		StatusCode:   200,
		ResponseBody: "ok",
	})
	if len(noUpstreamProxy.cassette.Interactions) != 1 {
		t.Errorf("expected 1 interaction, got %d", len(noUpstreamProxy.cassette.Interactions))
	}

	// CreateSnapshot edge cases
	if _, err := CreateSnapshot("", "snap-id"); err == nil {
		t.Errorf("expected error for empty sourceDir in CreateSnapshot")
	}
	snapAutoID, err := CreateSnapshot(sbDir, "")
	if err != nil {
		t.Fatalf("CreateSnapshot with empty snapshotID failed: %v", err)
	}
	_ = snapAutoID.Cleanup()
}
