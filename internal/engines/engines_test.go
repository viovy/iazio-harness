package engines

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/viovy/iazio-harness/internal/idempotency"
)

func TestArgvOrder(t *testing.T) {
	tests := []struct {
		name string
		req  ExecutionRequest
		want []string
		cwd  string
	}{
		{
			name: "agent",
			req: ExecutionRequest{
				Engine: "agent", Prompt: "do thing", Kind: "execute",
				WorktreePath: "/repos/leaf-01", DocsHubPath: "/repos/docs-hub",
			},
			want: []string{"agent", "--print", "--output-format", "text", "--trust", "--force", "--model", "auto", "--", "do thing"},
			cwd:  "/repos/leaf-01",
		},
		{
			name: "agent prompt dash",
			req: ExecutionRequest{
				Engine: "agent", Prompt: "-n", Kind: "execute",
				WorktreePath: "/repos/leaf-01", DocsHubPath: "/repos/docs-hub",
			},
			want: []string{"agent", "--print", "--output-format", "text", "--trust", "--force", "--model", "auto", "--", "-n"},
			cwd:  "/repos/leaf-01",
		},
		{
			name: "agent refinement cwd",
			req: ExecutionRequest{
				Engine: "agent", Prompt: "p", Kind: KindStoryRefinement,
				WorktreePath: "/repos/leaf-01", DocsHubPath: "/repos/docs-hub",
			},
			want: []string{"agent", "--print", "--output-format", "text", "--trust", "--force", "--model", "auto", "--", "p"},
			cwd:  "/repos/docs-hub",
		},
		{
			name: "agy full",
			req: ExecutionRequest{
				Engine: "agy", Prompt: "p", PrintTimeout: "30s", Model: "m1", Agent: "coder",
				DisableSlashCommands: true, DangerouslySkipPermissions: true, LogFile: "/tmp/agy.log",
				Kind: "execute", WorktreePath: "/repos/leaf-01", DocsHubPath: "/repos/docs-hub",
			},
			want: []string{"agy", "--print", "p", "--print-timeout", "30s", "--model", "m1", "--agent", "coder", "--disable-slash-commands", "--dangerously-skip-permissions", "--log-file", "/tmp/agy.log"},
			cwd:  "/repos/leaf-01",
		},
		{
			name: "agy minimal refinement",
			req: ExecutionRequest{
				Engine: "agy", Prompt: "p", PrintTimeout: "10s", Model: "m", Kind: KindStoryRefinement,
				WorktreePath: "/repos/leaf-01", DocsHubPath: "/repos/docs-hub",
			},
			want: []string{"agy", "--print", "p", "--print-timeout", "10s", "--model", "m", "--dangerously-skip-permissions"},
			cwd:  "/repos/docs-hub",
		},
		{
			name: "agy defaults",
			req: ExecutionRequest{
				Engine: "agy", Prompt: "hello", Kind: "execute",
				WorktreePath: "/repos/leaf-01", DocsHubPath: "/repos/docs-hub",
			},
			want: []string{"agy", "--print", "hello", "--print-timeout", "0", "--model", "gemini-3.8-flash-high", "--dangerously-skip-permissions"},
			cwd:  "/repos/leaf-01",
		},
		{
			name: "empty engine defaults to agy",
			req: ExecutionRequest{
				Engine: "", Prompt: "hello", Kind: "execute",
				WorktreePath: "/repos/leaf-01", DocsHubPath: "/repos/docs-hub",
			},
			want: []string{"agy", "--print", "hello", "--print-timeout", "0", "--model", "gemini-3.8-flash-high", "--dangerously-skip-permissions"},
			cwd:  "/repos/leaf-01",
		},
		{
			name: "agent with resume",
			req: ExecutionRequest{
				Engine: "agent", Prompt: "continue work", Kind: "execute",
				WorktreePath: "/repos/leaf-01", DocsHubPath: "/repos/docs-hub",
				ResumeConversationID: "cursor-uuid-123",
			},
			want: []string{"agent", "--print", "--output-format", "text", "--trust", "--force", "--model", "auto", "--resume", "cursor-uuid-123", "--", "continue work"},
			cwd:  "/repos/leaf-01",
		},
		{
			name: "agy with resume",
			req: ExecutionRequest{
				Engine: "agy", Prompt: "continue work", Kind: "execute",
				WorktreePath: "/repos/leaf-01", DocsHubPath: "/repos/docs-hub",
				ResumeConversationID: "conv-uuid-456",
			},
			want: []string{"agy", "--print", "continue work", "--print-timeout", "0", "--model", "gemini-3.8-flash-high", "--conversation=conv-uuid-456", "--dangerously-skip-permissions"},
			cwd:  "/repos/leaf-01",
		},
		{
			name: "agy autonomous resume prompt",
			req: ExecutionRequest{
				Engine: "agy", Prompt: "", Kind: "resume",
				WorktreePath: "/repos/leaf-01", DocsHubPath: "/repos/docs-hub",
				ResumeConversationID: "conv-uuid-789",
			},
			want: []string{"agy", "--print", "resume", "--print-timeout", "0", "--model", "gemini-3.8-flash-high", "--conversation=conv-uuid-789", "--dangerously-skip-permissions"},
			cwd:  "/repos/leaf-01",
		},
		{
			name: "agent explicit model",
			req: ExecutionRequest{
				Engine: "agent", Prompt: "build feature", Kind: "execute",
				Model:        "gpt-5",
				WorktreePath: "/repos/leaf-01", DocsHubPath: "/repos/docs-hub",
			},
			want: []string{"agent", "--print", "--output-format", "text", "--trust", "--force", "--model", "gpt-5", "--", "build feature"},
			cwd:  "/repos/leaf-01",
		},
		{
			name: "opencode",
			req: ExecutionRequest{
				Engine: "opencode", Prompt: "p", Kind: "execute",
				WorktreePath: "/repos/leaf-01", DocsHubPath: "/repos/docs-hub",
			},
			want: []string{"opencode", "run", "--auto", "p"},
			cwd:  "/repos/leaf-01",
		},
		{
			name: "copilot",
			req: ExecutionRequest{
				Engine: "copilot", Prompt: "p", Kind: "execute",
				WorktreePath: "/repos/leaf-01", DocsHubPath: "/repos/docs-hub",
			},
			want: []string{"copilot", "--prompt", "p"},
			cwd:  "/repos/leaf-01",
		},
		{
			name: "autopilot-prompt-run",
			req: ExecutionRequest{
				Engine: "autopilot", Prompt: "STORY-GOV-3576", Kind: "execute",
				StoryID:      "STORY-GOV-3576",
				WorktreePath: "/repos/leaf-01", DocsHubPath: "/repos/docs-hub",
			},
			want: []string{"autopilot", "prompt", "run", "--story", "STORY-GOV-3576", "STORY-GOV-3576"},
			cwd:  "/repos/leaf-01",
		},
		{
			name: "autopilot-story-execution",
			req: ExecutionRequest{
				Engine: "autopilot", Kind: "story_execution",
				StoryID:      "STORY-GOV-3576",
				WorktreePath: "/repos/leaf-01", DocsHubPath: "/repos/docs-hub",
			},
			want: []string{"autopilot", "story", "run", "--story", "STORY-GOV-3576"},
			cwd:  "/repos/leaf-01",
		},
		{
			name: "autopilot-story-begin",
			req: ExecutionRequest{
				Engine: "autopilot", Kind: "story_begin",
				StoryID:      "STORY-GOV-3576",
				WorktreePath: "/repos/leaf-01", DocsHubPath: "/repos/docs-hub",
			},
			want: []string{"autopilot", "story", "begin", "--story", "STORY-GOV-3576"},
			cwd:  "/repos/leaf-01",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := BuildArgv(tt.req)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Join(got, "\x00") != strings.Join(tt.want, "\x00") {
				t.Fatalf("argv %q want %q", got, tt.want)
			}
			if cwd := WorkingDirectory(tt.req.Kind, tt.req.WorktreePath, tt.req.DocsHubPath); cwd != tt.cwd {
				t.Fatalf("cwd %s want %s", cwd, tt.cwd)
			}
		})
	}
}

func TestEnvOverride(t *testing.T) {
	tests := []struct {
		name     string
		base     []string
		schedule map[string]string
		docs     string
		key      string
		want     string
	}{
		{
			name:     "git prompt forced",
			base:     []string{"GIT_TERMINAL_PROMPT=1", "PATH=/bin"},
			schedule: map[string]string{"GIT_TERMINAL_PROMPT": "1"},
			docs:     "/repos/docs-hub",
			key:      "GIT_TERMINAL_PROMPT",
			want:     "0",
		},
		{
			name:     "ssh batch forced",
			schedule: map[string]string{"SSH_BATCHMODE": "no"},
			docs:     "/repos/docs-hub",
			key:      "SSH_BATCHMODE",
			want:     "yes",
		},
		{
			name:     "askpass cleared",
			schedule: map[string]string{"SSH_ASKPASS": "/tmp/ask"},
			docs:     "/repos/docs-hub",
			key:      "SSH_ASKPASS",
			want:     "",
		},
		{
			name:     "docs hub path forced",
			schedule: map[string]string{"IAZIO_DOCS_HUB_PATH": "/wrong"},
			docs:     "/repos/docs-hub",
			key:      "IAZIO_DOCS_HUB_PATH",
			want:     "/repos/docs-hub",
		},
		{
			name:     "schedule value kept",
			base:     []string{"FOO=old"},
			schedule: map[string]string{"FOO": "bar"},
			docs:     "/repos/docs-hub",
			key:      "FOO",
			want:     "bar",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := ChildEnv(tt.base, tt.schedule, tt.docs)
			got, ok := EnvLookup(env, tt.key)
			if !ok {
				t.Fatalf("missing %s in %v", tt.key, env)
			}
			if got != tt.want {
				t.Fatalf("%s=%q want %q", tt.key, got, tt.want)
			}
			joined := strings.Join(env, "\n")
			if strings.Contains(joined, "GIT_TERMINAL_PROMPT=1") || strings.Contains(joined, "SSH_BATCHMODE=no") {
				t.Fatal(joined)
			}
		})
	}
}

func TestChildEnvPATHAugmentation(t *testing.T) {
	env := ChildEnv([]string{"PATH=/usr/bin"}, nil, "/repos/docs-hub")
	path, ok := EnvLookup(env, "PATH")
	if !ok {
		t.Fatal("missing PATH in ChildEnv")
	}
	if !strings.Contains(path, "/usr/bin") {
		t.Fatalf("expected /usr/bin in PATH, got: %s", path)
	}
	home, _ := os.UserHomeDir()
	if home != "" {
		localBin := filepath.Join(home, ".local", "bin")
		if fi, err := os.Stat(localBin); err == nil && fi.IsDir() {
			if !strings.Contains(path, localBin) {
				t.Fatalf("expected %s in PATH, got: %s", localBin, path)
			}
		}
	}
}

func TestLockRefusal(t *testing.T) {
	tests := []struct {
		name    string
		kind    string
		hold    string
		wantErr bool
	}{
		{name: "worktree held", kind: "execute", hold: "worktree", wantErr: true},
		{name: "docs hub does not block worktree", kind: "execute", hold: "docs", wantErr: false},
		{name: "refinement docs held", kind: KindStoryRefinement, hold: "docs", wantErr: true},
		{name: "refinement ignores worktree lock", kind: KindStoryRefinement, hold: "worktree", wantErr: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			work := t.TempDir()
			docs := t.TempDir()
			locks := t.TempDir()
			heldPath := work
			if tt.hold == "docs" {
				heldPath = docs
			}
			held, err := Hold(heldPath, locks)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = held.Release() })
			target, err := CheckoutDir(tt.kind, work, docs)
			if err != nil {
				t.Fatal(err)
			}
			lk, err := Hold(target, locks)
			if tt.wantErr {
				if !errors.Is(err, ErrLockHeld) {
					t.Fatalf("got %v", err)
				}
				if lk != nil {
					t.Fatal("lock acquired; exec would proceed")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := lk.Release(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCancelStopsChild(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep not on PATH")
	}
	ctx, cancel := context.WithCancel(context.Background())
	dir := t.TempDir()
	eng := CLI{}
	req := ExecutionRequest{
		Kind:         "execute",
		WorktreePath: dir,
		DocsHubPath:  dir,
		Argv:         []string{"sleep", "30"},
		BaseEnv:      []string{"PATH=" + os.Getenv("PATH")},
	}
	errCh := make(chan error, 1)
	start := time.Now()
	go func() {
		_, err := eng.Execute(ctx, req, nil)
		errCh <- err
	}()
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		if time.Since(start) > 8*time.Second {
			t.Fatal("child was not stopped")
		}
	case <-time.After(8 * time.Second):
		t.Fatal("child still running")
	}
}

func testShCommand(t *testing.T) (string, string) {
	t.Helper()
	path, err := exec.LookPath("sh")
	if err != nil {
		path, err = exec.LookPath("bash")
	}
	if err != nil {
		if _, err := os.Stat("/bin/sh"); err == nil {
			path = "/bin/sh"
		}
	}
	if path == "" {
		t.Skip("sh/bash not found on PATH")
	}
	return path, os.Getenv("PATH")
}

func TestChildStdinIsDevNull(t *testing.T) {
	shPath, sysPath := testShCommand(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "stdin.txt")
	script := filepath.Join(dir, "read.sh")
	body := "#!/bin/sh\nif read -r line; then printf '%s' \"$line\" > \"$OUT\"; else printf EOF > \"$OUT\"; fi\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	eng := CLI{}
	_, err := eng.Execute(context.Background(), ExecutionRequest{
		Kind:         "execute",
		WorktreePath: dir,
		DocsHubPath:  dir,
		Argv:         []string{shPath, script},
		BaseEnv:      []string{"OUT=" + out, "PATH=" + sysPath},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "EOF" {
		t.Fatalf("stdin yielded %q", got)
	}
}

func TestChildEnvForcedOnProcess(t *testing.T) {
	shPath, sysPath := testShCommand(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "env.txt")
	script := filepath.Join(dir, "env.sh")
	body := "#!/bin/sh\nprintf '%s\\n' \"$GIT_TERMINAL_PROMPT\" \"$SSH_BATCHMODE\" \"$SSH_ASKPASS\" \"$IAZIO_DOCS_HUB_PATH\" > \"$OUT\"\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	eng := CLI{}
	_, err := eng.Execute(context.Background(), ExecutionRequest{
		Kind:         "execute",
		WorktreePath: dir,
		DocsHubPath:  "/repos/docs-hub",
		Argv:         []string{shPath, script},
		BaseEnv:      []string{"OUT=" + out, "PATH=" + sysPath},
		ScheduleEnv: map[string]string{
			"GIT_TERMINAL_PROMPT": "1",
			"SSH_BATCHMODE":       "no",
			"SSH_ASKPASS":         "/tmp/ask",
			"IAZIO_DOCS_HUB_PATH": "/wrong",
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	gotNorm := strings.ReplaceAll(string(got), "\r\n", "\n")
	want := "0\nyes\n\n/repos/docs-hub\n"
	if gotNorm != want {
		t.Fatalf("got %q want %q", gotNorm, want)
	}
}

func TestExtractConversationID(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{
			input: `{"event":"init","conversation_id":"e5bb80ff-9747-4c25-8ab1-706ecdf3679b","init":{}}`,
			want:  "e5bb80ff-9747-4c25-8ab1-706ecdf3679b",
		},
		{
			input: `{"type":"system","subtype":"init","session_id":"sess-1234-uuid"}`,
			want:  "sess-1234-uuid",
		},
		{
			input: `normal output without any session identifier`,
			want:  "",
		},
	}
	for _, tt := range tests {
		got := extractConversationID(tt.input)
		if got != tt.want {
			t.Errorf("extractConversationID(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

type testCaptureStream struct {
	mu     sync.Mutex
	events []string
}

func (s *testCaptureStream) AppendRaw(stream string, p []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, string(p))
	return nil
}

func TestEarlyConversationIDStreaming(t *testing.T) {
	shPath, sysPath := testShCommand(t)
	dir := t.TempDir()
	script := filepath.Join(dir, "emit.sh")
	body := "#!/bin/sh\necho '{\"event\":\"init\",\"conversation_id\":\"streamed-conv-123\"}'\necho 'second line'\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	var capturedID string
	var mu sync.Mutex
	doneCapture := make(chan struct{})

	stream := &testCaptureStream{}
	eng := CLI{}
	res, err := eng.Execute(context.Background(), ExecutionRequest{
		Kind:         "execute",
		WorktreePath: dir,
		DocsHubPath:  dir,
		Argv:         []string{shPath, script},
		BaseEnv:      []string{"PATH=" + sysPath},
		OnConversationID: func(cid string) {
			mu.Lock()
			capturedID = cid
			mu.Unlock()
			select {
			case <-doneCapture:
			default:
				close(doneCapture)
			}
		},
	}, stream)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit code %d", res.ExitCode)
	}

	select {
	case <-doneCapture:
	case <-time.After(10 * time.Second):
		stream.mu.Lock()
		events := append([]string(nil), stream.events...)
		stream.mu.Unlock()
		t.Fatalf("timed out waiting for OnConversationID callback, stream events: %v", events)
	}

	mu.Lock()
	defer mu.Unlock()
	if capturedID != "streamed-conv-123" {
		t.Fatalf("expected 'streamed-conv-123', got %q", capturedID)
	}
}

func TestExtractConversationIDFromText(t *testing.T) {
	uuid1 := "d9d4953a-1427-4274-8010-7270aa743bb3"
	logLine1 := "I1006 19:58:28.686365 1 server.go:1263] Created conversation " + uuid1
	if got := extractConversationIDFromText(logLine1); got != uuid1 {
		t.Fatalf("expected %s, got %s", uuid1, got)
	}

	uuid2 := "a45a2580-7964-4dcd-8a79-f45696a9add0"
	logLine2 := "I1006 19:58:28.693980 1 session.go:192] Print mode: conversation=" + uuid2 + ", sending message"
	if got := extractConversationIDFromText(logLine2); got != uuid2 {
		t.Fatalf("expected %s, got %s", uuid2, got)
	}

	// Trailing punctuation should be trimmed
	logLine3 := "Resumed conversation " + uuid1 + "."
	if got := extractConversationIDFromText(logLine3); got != uuid1 {
		t.Fatalf("expected %s, got %s", uuid1, got)
	}

	// Earlier false-positive marker occurrence should not block later valid UUID
	logLine4 := "Flags: Resumed conversation none\nServer: Active conversation " + uuid2
	if got := extractConversationIDFromText(logLine4); got != uuid2 {
		t.Fatalf("expected %s, got %s", uuid2, got)
	}

	stdoutText := "Session initialized: Created conversation " + uuid1 + "\n"
	if got := extractConversationID(stdoutText); got != uuid1 {
		t.Fatalf("expected %s from extractConversationID, got %s", uuid1, got)
	}

	// Explicit engine markers test
	uuid3 := "b56c3691-8075-4ede-9b80-056707b0bee1"
	if got := extractConversationIDFromText("Active conversation " + uuid3); got != uuid3 {
		t.Fatalf("expected %s, got %s", uuid3, got)
	}
	if got := extractConversationIDFromText("Agent active. Conversation ID: " + uuid3); got != uuid3 {
		t.Fatalf("expected %s, got %s", uuid3, got)
	}
	if got := extractConversationIDFromText("Engine status: Conversation ID=" + uuid3); got != uuid3 {
		t.Fatalf("expected %s, got %s", uuid3, got)
	}

	// Bare markers in narrative / CLI flags / LLM output must be ignored (Finding H1)
	foreignID := "1d446877-5bf9-44eb-9db5-e1c007a9fe31"
	if got := extractConversationIDFromText("User asked: agy --conversation=" + foreignID); got != "" {
		t.Fatalf("expected bare conversation= to be ignored, got %q", got)
	}
	if got := extractConversationIDFromText("echo conversation=" + foreignID); got != "" {
		t.Fatalf("expected bare conversation= to be ignored, got %q", got)
	}
	if got := extractConversationIDFromText("Logged: conversation_id=" + foreignID); got != "" {
		t.Fatalf("expected bare conversation_id= to be ignored, got %q", got)
	}
	if got := extractConversationIDFromText("Starting prompt: conversationID=" + foreignID); got != "" {
		t.Fatalf("expected bare conversationID= to be ignored, got %q", got)
	}
	if got := extractConversationIDFromText("conversation: " + foreignID); got != "" {
		t.Fatalf("expected bare conversation: to be ignored, got %q", got)
	}
}

type mockRawStream struct{}

func (m *mockRawStream) AppendRaw(stream string, p []byte) error {
	return nil
}

func TestRawWriter_ResumeConversationIsolation(t *testing.T) {
	// Verify that when ResumeConversationID is set, rawWriter ignores LLM stdout containing foreign conversation IDs (Finding H1, M1)
	expectedID := "aad902c8-bda6-44c7-8a68-c468a1562399"
	foreignID := "1d446877-5bf9-44eb-9db5-e1c007a9fe31"

	var detectedID string
	var scanned bool
	var mu sync.Mutex
	var recent string

	writer := rawWriter{
		stream:       "stdout",
		dst:          &mockRawStream{},
		onConv:       func(cid string) { detectedID = cid },
		scanned:      &scanned,
		recent:       &recent,
		mu:           &mu,
		resumeConvID: expectedID,
	}

	// 1. LLM echoes foreign conversation ID in its response text
	llmOutput := []byte("I noticed prior job ran with agy --conversation=" + foreignID + " which failed.\n")
	_, err := writer.Write(llmOutput)
	if err != nil {
		t.Fatalf("write error: %v", err)
	}
	if detectedID != "" {
		t.Fatalf("expected foreign ID to be ignored, but detected: %s", detectedID)
	}

	// 2. LLM echoes 'Created conversation <foreign>' in its markdown
	llmCreated := []byte("The prior output was: Created conversation " + foreignID + "\n")
	_, err = writer.Write(llmCreated)
	if err != nil {
		t.Fatalf("write error: %v", err)
	}
	if detectedID != "" {
		t.Fatalf("expected foreign created ID to be ignored during resume, but detected: %s", detectedID)
	}

	// 3. LLM echoes JSON '{"conversation_id":"<foreign>"}'
	llmJSON := []byte(`{"event":"chunk","conversation_id":"` + foreignID + `"}` + "\n")
	_, err = writer.Write(llmJSON)
	if err != nil {
		t.Fatalf("write error: %v", err)
	}
	if detectedID != "" {
		t.Fatalf("expected foreign JSON conversation_id to be ignored during resume, but detected: %s", detectedID)
	}

	// 4. Real engine marker with expected ID is received
	engineMarker := []byte("Resumed conversation " + expectedID + "\n")
	_, err = writer.Write(engineMarker)
	if err != nil {
		t.Fatalf("write error: %v", err)
	}
	if detectedID != expectedID {
		t.Fatalf("expected %s, got %s", expectedID, detectedID)
	}
}

func TestLogFileConversationIDPostExit(t *testing.T) {
	shPath, sysPath := testShCommand(t)
	dir := t.TempDir()
	logFile := filepath.Join(dir, "engine.log")
	uuid := "e1234567-89ab-cdef-0123-456789abcdef"

	script := filepath.Join(dir, "run_log.sh")
	// Writes to log file right before exiting
	body := "#!/bin/sh\nsleep 0.05\necho 'Created conversation " + uuid + "' > \"" + filepath.ToSlash(logFile) + "\"\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	var capturedID string
	var mu sync.Mutex
	doneCapture := make(chan struct{})

	stream := &testCaptureStream{}
	eng := CLI{}
	res, err := eng.Execute(context.Background(), ExecutionRequest{
		Kind:         "execute",
		WorktreePath: dir,
		DocsHubPath:  dir,
		LogFile:      logFile,
		Argv:         []string{shPath, script},
		BaseEnv:      []string{"PATH=" + sysPath},
		OnConversationID: func(cid string) {
			mu.Lock()
			capturedID = cid
			mu.Unlock()
			select {
			case <-doneCapture:
			default:
				close(doneCapture)
			}
		},
	}, stream)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit code %d", res.ExitCode)
	}

	select {
	case <-doneCapture:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for log file OnConversationID callback")
	}

	mu.Lock()
	defer mu.Unlock()
	if capturedID != uuid {
		t.Fatalf("expected %q, got %q", uuid, capturedID)
	}
}

func TestCliLogFallback_And_ConversationDBSensing(t *testing.T) {
	shPath, sysPath := testShCommand(t)
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	agyLogDir := filepath.Join(dir, ".gemini", "antigravity-cli")
	_ = os.MkdirAll(agyLogDir, 0o755)
	cliLogFile := filepath.Join(agyLogDir, "cli.log")
	uuid := "a9876543-21fe-dcba-9876-543210fedcba"

	script := filepath.Join(dir, "run_agy_log.sh")
	body := "#!/bin/sh\nsleep 0.05\necho 'Created conversation " + uuid + "' > \"" + filepath.ToSlash(cliLogFile) + "\"\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	var capturedID string
	var mu sync.Mutex
	doneCapture := make(chan struct{})

	stream := &testCaptureStream{}
	eng := CLI{}
	res, err := eng.Execute(context.Background(), ExecutionRequest{
		Kind:         "execute",
		WorktreePath: dir,
		DocsHubPath:  dir,
		Engine:       "agy",
		Argv:         []string{shPath, script},
		BaseEnv:      []string{"PATH=" + sysPath, "HOME=" + dir},
		OnConversationID: func(cid string) {
			mu.Lock()
			capturedID = cid
			mu.Unlock()
			select {
			case <-doneCapture:
			default:
				close(doneCapture)
			}
		},
	}, stream)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit code %d", res.ExitCode)
	}

	select {
	case <-doneCapture:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for cli.log OnConversationID callback")
	}

	mu.Lock()
	defer mu.Unlock()
	if capturedID != uuid {
		t.Fatalf("expected %q, got %q", uuid, capturedID)
	}
}

func TestHold_FallbackToGitDir(t *testing.T) {
	work := t.TempDir()
	gitDir := filepath.Join(work, ".git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Use an invalid unwritable path for lockDir (pointing to an existing non-directory file)
	dummyFile := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(dummyFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	invalidLockDir := filepath.Join(dummyFile, "cannot-create-dir")

	lk, err := Hold(work, invalidLockDir)
	if err != nil {
		t.Fatalf("expected fallback to succeed, got error: %v", err)
	}
	defer func() { _ = lk.Release() }()

	expectedFallback := filepath.Join(gitDir, "execution.lock")
	if lk.Path != expectedFallback {
		t.Fatalf("expected fallback path %s, got %s", expectedFallback, lk.Path)
	}

	// Second hold on same worktree with invalidLockDir should fail with ErrLockHeld
	_, err2 := Hold(work, invalidLockDir)
	if !errors.Is(err2, ErrLockHeld) {
		t.Fatalf("expected ErrLockHeld on fallback lock, got %v", err2)
	}
}

func TestEnginesExtraCoverage(t *testing.T) {
	// 1. CLI Name and HealthCheck
	cDefault := CLI{}
	if cDefault.Name() != "agy" {
		t.Errorf("expected agy, got %s", cDefault.Name())
	}
	cCustom := CLI{
		Engine: "echo",
		LookPath: func(s string) (string, error) {
			return "/bin/echo", nil
		},
	}
	if err := cCustom.HealthCheck(); err != nil {
		t.Errorf("expected health check success, got %v", err)
	}

	cFail := CLI{
		Engine: "missing-engine",
		LookPath: func(s string) (string, error) {
			return "", errors.New("not found")
		},
	}
	if err := cFail.HealthCheck(); err == nil {
		t.Errorf("expected health check failure for missing engine")
	}

	// 2. isAntigravityConversation
	if isAntigravityConversation("") {
		t.Errorf("expected false for empty cid")
	}
	if isAntigravityConversation("definitely-not-an-agy-conversation-uuid") {
		t.Errorf("expected false for nonexistent cid")
	}

	// 3. DefaultLockDir
	t.Setenv("IAZIO_HARNESS_LOCK_DIR", "/custom/locks")
	if DefaultLockDir() != "/custom/locks" {
		t.Errorf("expected /custom/locks, got %s", DefaultLockDir())
	}
	t.Setenv("IAZIO_HARNESS_LOCK_DIR", "")
	if DefaultLockDir() == "" {
		t.Errorf("expected non-empty default lock dir")
	}

	// 4. NewLock and HoldPath
	lockFile := filepath.Join(t.TempDir(), "test.lock")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	lk, err := HoldPath(ctx, lockFile)
	if err != nil {
		t.Fatalf("HoldPath failed: %v", err)
	}
	_ = lk.Release()

	// Canceled HoldPath
	ctxCanceled, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	// Hold the lock with another handle first
	f, _ := os.OpenFile(lockFile, os.O_CREATE|os.O_RDWR, 0o644)
	_ = FileLock(f)
	defer func() { _ = FileUnlock(f); _ = f.Close() }()
	_, err = HoldPath(ctxCanceled, lockFile)
	if err == nil {
		t.Errorf("expected error on canceled context in HoldPath")
	}

	// Test HoldWithTimeout success
	checkoutDir := t.TempDir()
	lkTimeout, err := HoldWithTimeout(context.Background(), checkoutDir, "", 100*time.Millisecond)
	if err != nil {
		t.Fatalf("HoldWithTimeout failed: %v", err)
	}
	// While held, second hold should time out with WORKSPACE_LOCKED
	_, errLocked := HoldWithTimeout(context.Background(), checkoutDir, "", 50*time.Millisecond)
	if errLocked == nil || !strings.Contains(errLocked.Error(), "WORKSPACE_LOCKED") {
		t.Fatalf("expected WORKSPACE_LOCKED error, got %v", errLocked)
	}
	_ = lkTimeout.Release()

	// 5. Process group hooks
	PostStartProcessGroup(nil)
	StopProcessGroup(nil)
	KillProcessGroup(nil)
	postStartProcessGroup(nil)
	stopProcessGroup(nil)
	killProcessGroup(nil)

	cmd := exec.Command("sleep", "10")
	SetProcGroup(cmd)
	setProcGroup(cmd)
	if err := cmd.Start(); err == nil {
		PostStartProcessGroup(cmd)
		postStartProcessGroup(cmd)
		stopProcessGroup(cmd)
		killProcessGroup(cmd)
		_ = cmd.Wait()
	}

	// 6. Lock path helpers
	nl := NewLock(nil, "/path/to/lock")
	if nl.Path != "/path/to/lock" {
		t.Errorf("expected path /path/to/lock, got %s", nl.Path)
	}
	p := PrimaryLockPath("/my/worktree", "")
	if p == "" {
		t.Errorf("expected non-empty primary lock path")
	}
	fb := FallbackLockPath("/my/worktree")
	if fb == "" {
		t.Errorf("expected non-empty fallback lock path")
	}

	// 7. HealthCheck candidate paths
	candDir := t.TempDir()
	t.Setenv("HOME", candDir)
	localBin := filepath.Join(candDir, ".local", "bin")
	_ = os.MkdirAll(localBin, 0o755)
	_ = os.WriteFile(filepath.Join(localBin, "myengine"), []byte("#!/bin/sh\n"), 0o755)
	candEngine := CLI{
		Engine: "myengine",
		LookPath: func(file string) (string, error) {
			return "", os.ErrNotExist
		},
	}
	// With nil LookPath, it searches HOME/.local/bin
	candEngineNilLook := CLI{Engine: "myengine"}
	if err := candEngineNilLook.HealthCheck(); err != nil {
		t.Errorf("expected HealthCheck candidate lookup to succeed, got %v", err)
	}
	_ = candEngine.HealthCheck()

	// 8. exitCode edge cases
	if ec := exitCode(nil); ec != 0 {
		t.Errorf("expected 0, got %d", ec)
	}
	if ec := exitCode(errors.New("generic error")); ec != 1 {
		t.Errorf("expected 1, got %d", ec)
	}

	// 9. lock.go edge cases
	t.Setenv("IAZIO_HARNESS_LOCK_DIR", "/custom/locks")
	if d := DefaultLockDir(); d != "/custom/locks" {
		t.Errorf("expected /custom/locks, got %s", d)
	}
	if _, err := CheckoutDir("ordinary", "", ""); err == nil {
		t.Errorf("expected error for empty checkout path")
	}
	if _, err := Hold("", ""); err == nil {
		t.Errorf("expected error for empty checkout in Hold")
	}
	submodDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(submodDir, ".git"), []byte("gitdir: ../.git/modules/sub"), 0o644)
	if fb := FallbackLockPath(submodDir); !strings.HasSuffix(fb, ".git-execution.lock") {
		t.Errorf("expected submodule fallback lock path, got %s", fb)
	}

	// 10. env.go edge cases
	if _, ok := EnvLookup([]string{"FOO=BAR", "MALFORMED_NO_EQUALS"}, "NOT_FOUND"); ok {
		t.Errorf("expected false for missing key")
	}
	if val, ok := EnvLookup([]string{"FOO=BAR"}, "FOO"); !ok || val != "BAR" {
		t.Errorf("expected BAR, got %s, %v", val, ok)
	}
}

func TestExecute_Sandbox_Idempotency_AutoCorrect(t *testing.T) {
	// 1. Idempotency skip
	srvIdemp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(idempotency.StepResponse{
			AlreadyExecuted: true,
		})
	}))
	defer srvIdemp.Close()

	stream := &testCaptureStream{}
	idempClient := idempotency.NewClient(srvIdemp.URL, "tok")
	cli := CLI{Engine: "agy"}
	res, err := cli.Execute(context.Background(), ExecutionRequest{
		Engine:               "agy",
		ResumeConversationID: "conv-idemp-123",
		StepIndex:            1,
		IdempotencyClient:    idempClient,
		Argv:                 []string{"echo", "hello"},
	}, stream)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("expected idempotency skip to succeed with 0, got res=%+v, err=%v", res, err)
	}

	// 2. Sandbox execution and OnSandboxDiff
	workDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(workDir, "existing.txt"), []byte("base"), 0o644)
	mockSh := filepath.Join(t.TempDir(), "run.sh")
	_ = os.WriteFile(mockSh, []byte("#!/bin/sh\necho 'added' > newly_created.txt\nexit 0\n"), 0o755)

	var capturedDiff string
	res, err = cli.Execute(context.Background(), ExecutionRequest{
		Engine:       "agy",
		WorktreePath: workDir,
		Sandbox:      true,
		Argv:         []string{mockSh},
		OnSandboxDiff: func(d string) {
			capturedDiff = d
		},
	}, stream)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("expected sandbox execution to succeed, got %+v, %v", res, err)
	}
	_ = capturedDiff
	// Verify workspace on disk was not polluted because it was executed in a sandbox
	if _, err := os.Stat(filepath.Join(workDir, "newly_created.txt")); err == nil {
		t.Errorf("newly_created.txt should not exist in original worktree when sandboxed")
	}

	// 3. Auto-correct engine from agent to agy for Antigravity conversation ID
	mockAgy := filepath.Join(t.TempDir(), "mock_agy.sh")
	_ = os.WriteFile(mockAgy, []byte("#!/bin/sh\necho 'auto-corrected agy success'\nexit 0\n"), 0o755)

	res, err = cli.Execute(context.Background(), ExecutionRequest{
		Engine:               "agent",
		ResumeConversationID: "conv-antigravity-9999",
		Argv:                 []string{mockAgy},
	}, stream)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("expected auto-correct execution to succeed, got %+v, %v", res, err)
	}

	// 4. Auto-correct engine from "iazio-harness" to agy
	res, err = cli.Execute(context.Background(), ExecutionRequest{
		Engine: "iazio-harness",
		Prompt: "echo test",
		Argv:   []string{mockAgy},
	}, stream)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("expected harness engine remap execution to succeed, got %+v, %v", res, err)
	}

	builtArgv, err := BuildArgv(ExecutionRequest{
		Engine: "iazio-harness",
		Prompt: "run prompt",
	})
	if err != nil || len(builtArgv) == 0 || (builtArgv[0] != "agy" && builtArgv[0] != "gemini") {
		t.Fatalf("expected BuildArgv to remap iazio-harness to agy/gemini, got %+v, %v", builtArgv, err)
	}
}

func TestResolveDynamicTool(t *testing.T) {
	// Explicit engine returns as-is
	if tool := ResolveDynamicTool("agent", nil); tool != "agent" {
		t.Fatalf("expected agent, got %s", tool)
	}
	if tool := ResolveDynamicTool("copilot", []string{"agy"}); tool != "copilot" {
		t.Fatalf("expected copilot, got %s", tool)
	}

	// Dynamic fallback with custom priority list
	tool := ResolveDynamicTool("auto", []string{"nonexistent-tool-xyz", "sh"})
	if tool != "sh" {
		t.Fatalf("expected sh from PATH, got %s", tool)
	}

	// Default priority list fallback
	toolDefault := ResolveDynamicTool("", nil)
	if toolDefault == "" {
		t.Fatalf("expected non-empty default resolved tool")
	}
}

func TestExecute_GrandchildPipeLeakResilience(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping bash pipe test on Windows")
	}

	stream := &testCaptureStream{}
	cli := CLI{Engine: "agy"}

	// Script outputs a line, forks a background sleep that inherits stdout/stderr, then exits immediately.
	// Previously this caused wg.Wait() to deadlock waiting indefinitely for io.Copy EOF.
	mockScript := filepath.Join(t.TempDir(), "leak_pipes.sh")
	scriptContent := "#!/bin/bash\necho 'engine-work-complete'\n(sleep 30 >&1) &\nexit 0\n"
	if err := os.WriteFile(mockScript, []byte(scriptContent), 0o755); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res, err := cli.Execute(ctx, ExecutionRequest{
		Engine: "agy",
		Argv:   []string{mockScript},
	}, stream)

	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("expected execute to succeed despite leaked pipe, got err: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", res.ExitCode)
	}
	if elapsed > 4*time.Second {
		t.Fatalf("execute took too long (%v), expected prompt return under 4s", elapsed)
	}

	stream.mu.Lock()
	foundOutput := false
	for _, chunk := range stream.events {
		if strings.Contains(chunk, "engine-work-complete") {
			foundOutput = true
			break
		}
	}
	stream.mu.Unlock()

	if !foundOutput {
		t.Fatalf("expected to capture 'engine-work-complete' in stream events, got: %v", stream.events)
	}
}

func TestRawWriter_FiltersRootAgentIdleMessage(t *testing.T) {
	stream := &testCaptureStream{}
	w := rawWriter{
		stream: "stdout",
		dst:    stream,
	}

	input := "line 1\nroot agent idle; waiting up to 1h0m0s for 1 background task(s)\nline 2\n"
	n, err := w.Write([]byte(input))
	if err != nil {
		t.Fatalf("unexpected write error: %v", err)
	}
	if n != len(input) {
		t.Fatalf("expected n=%d, got %d", len(input), n)
	}

	stream.mu.Lock()
	defer stream.mu.Unlock()
	combined := strings.Join(stream.events, "")
	if strings.Contains(combined, "root agent idle; waiting up to") {
		t.Errorf("expected 'root agent idle; waiting up to' to be filtered out, got:\n%s", combined)
	}
	if !strings.Contains(combined, "line 1") || !strings.Contains(combined, "line 2") {
		t.Errorf("expected remaining lines to be preserved, got:\n%s", combined)
	}
}

func TestProcessGroupHelpers(t *testing.T) {
	cmd := exec.Command("sleep", "5")
	SetProcGroup(cmd)
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Errorf("expected Setpgid to be true")
	}
	PostStartProcessGroup(cmd)
	setProcGroup(cmd)
	postStartProcessGroup(cmd)

	StopProcessGroup(nil)
	KillProcessGroup(nil)
	stopProcessGroup(nil)
	killProcessGroup(nil)

	cmdNil := &exec.Cmd{}
	StopProcessGroup(cmdNil)
	KillProcessGroup(cmdNil)

	if err := cmd.Start(); err == nil {
		StopProcessGroup(cmd)
		KillProcessGroup(cmd)
		_ = cmd.Wait()
	}
}

func TestBuildArgv_UnanchoredResumeRejection(t *testing.T) {
	engines := []string{"agy", "agent", "copilot", "opencode"}
	prompts := []string{"resume", "RESUME", " resume ", "/autonomous-story-execution resume"}

	for _, eng := range engines {
		for _, p := range prompts {
			req := ExecutionRequest{
				Engine: eng,
				Prompt: p,
			}
			_, err := BuildArgv(req)
			if err == nil {
				t.Fatalf("expected error for unanchored resume on engine %q prompt %q, got nil", eng, p)
			}
			if !strings.Contains(err.Error(), "when ResumeConversationID is empty") {
				t.Fatalf("expected ResumeConversationID is empty error, got %v", err)
			}
		}

		// Also verify Kind: resume with empty prompt
		reqKind := ExecutionRequest{
			Engine: eng,
			Kind:   "resume",
		}
		_, errKind := BuildArgv(reqKind)
		if errKind == nil {
			t.Fatalf("expected error for unanchored Kind=resume on engine %q, got nil", eng)
		}
	}
}

func TestBuildArgv_AnchoredResumePass(t *testing.T) {
	reqAgy := ExecutionRequest{
		Engine:               "agy",
		Prompt:               "resume",
		ResumeConversationID: "conv-agy-1",
	}
	argvAgy, err := BuildArgv(reqAgy)
	if err != nil {
		t.Fatalf("unexpected error for anchored agy resume: %v", err)
	}
	foundConv := false
	for _, a := range argvAgy {
		if strings.Contains(a, "--conversation=conv-agy-1") {
			foundConv = true
		}
	}
	if !foundConv {
		t.Fatalf("expected --conversation=conv-agy-1 in argv: %v", argvAgy)
	}

	reqAgent := ExecutionRequest{
		Engine:               "agent",
		Prompt:               "resume",
		ResumeConversationID: "conv-agent-1",
	}
	argvAgent, errAgent := BuildArgv(reqAgent)
	if errAgent != nil {
		t.Fatalf("unexpected error for anchored agent resume: %v", errAgent)
	}
	foundResume := false
	for i, a := range argvAgent {
		if a == "--resume" && i+1 < len(argvAgent) && argvAgent[i+1] == "conv-agent-1" {
			foundResume = true
		}
	}
	if !foundResume {
		t.Fatalf("expected --resume conv-agent-1 in argv: %v", argvAgent)
	}
}

func TestBuildArgv_EmptyConversation_DirtyWorktree(t *testing.T) {
	dir := t.TempDir()
	// Initialize git repo in dir and create uncommitted file
	_ = exec.Command("git", "-C", dir, "init").Run()
	_ = os.WriteFile(filepath.Join(dir, "dirty.txt"), []byte("dirty content"), 0644)

	req := ExecutionRequest{
		Engine:       "agy",
		Prompt:       "resume",
		WorktreePath: dir,
		StoryID:      "STORY-GOV-9999",
	}
	argv, err := BuildArgv(req)
	if err != nil {
		t.Fatalf("expected dirty worktree to resolve prompt without error, got: %v", err)
	}
	// Verify --conversation is NOT passed when ResumeConversationID is empty
	for _, a := range argv {
		if strings.Contains(a, "--conversation") {
			t.Fatalf("did not expect --conversation in argv when ResumeConversationID is empty: %v", argv)
		}
	}
	// Verify prompt is resolved to complete current ASE flow
	foundPrompt := false
	for _, a := range argv {
		if strings.Contains(a, "resume and complete current /autonomous-story-execution flow") {
			foundPrompt = true
			break
		}
	}
	if !foundPrompt {
		t.Fatalf("expected dirty flow completion prompt in argv, got: %v", argv)
	}
}

func TestBuildArgv_EmptyConversation_CleanWorktree_StoryID(t *testing.T) {
	dir := t.TempDir()
	// Initialize clean git repo
	_ = exec.Command("git", "-C", dir, "init").Run()

	req := ExecutionRequest{
		Engine:       "agy",
		Prompt:       "resume",
		WorktreePath: dir,
		StoryID:      "STORY-GOV-8888",
	}
	argv, err := BuildArgv(req)
	if err != nil {
		t.Fatalf("expected clean worktree with StoryID to resolve prompt without error, got: %v", err)
	}
	// Verify --conversation is NOT passed
	for _, a := range argv {
		if strings.Contains(a, "--conversation") {
			t.Fatalf("did not expect --conversation in argv: %v", argv)
		}
	}
	// Verify prompt executes the story
	foundPrompt := false
	for _, a := range argv {
		if strings.Contains(a, "Execute story STORY-GOV-8888 end-to-end") {
			foundPrompt = true
			break
		}
	}
	if !foundPrompt {
		t.Fatalf("expected story execution prompt in argv, got: %v", argv)
	}
}

func TestEngineExecutionSymlinkResolutionAndFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping symlink tests on windows")
	}
	dir := t.TempDir()
	realBin := filepath.Join(dir, "real-sh")
	if err := os.WriteFile(realBin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	validSymlink := filepath.Join(dir, "symlink-sh")
	if err := os.Symlink(realBin, validSymlink); err != nil {
		t.Fatal(err)
	}

	brokenSymlink := filepath.Join(dir, "broken-symlink")
	if err := os.Symlink(filepath.Join(dir, "nonexistent"), brokenSymlink); err != nil {
		t.Fatal(err)
	}

	cli := CLI{Engine: "test"}
	// Valid symlink execute
	res, err := cli.Execute(context.Background(), ExecutionRequest{
		Engine: "custom",
		Argv:   []string{validSymlink},
	}, nil)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("expected valid symlink execution to succeed, got %+v, %v", res, err)
	}

	// Broken symlink execute
	_, errBroken := cli.Execute(context.Background(), ExecutionRequest{
		Engine: "custom",
		Argv:   []string{brokenSymlink},
	}, nil)
	if errBroken == nil {
		t.Fatalf("expected broken symlink execution to fail")
	}

	// Valid symlink healthcheck
	cliValid := CLI{Engine: "test", LookPath: func(string) (string, error) { return validSymlink, nil }}
	if err := cliValid.HealthCheck(); err != nil {
		t.Fatalf("expected valid symlink healthcheck to succeed, got %v", err)
	}

	// Broken symlink healthcheck
	cliBroken := CLI{Engine: "test", LookPath: func(string) (string, error) { return brokenSymlink, nil }}
	if err := cliBroken.HealthCheck(); err == nil {
		t.Fatalf("expected broken symlink healthcheck to fail")
	}
}

func TestBuildArgv_IazioAgyBinOverride(t *testing.T) {
	customAgy := "/custom/opt/bin/mock-agy"
	t.Setenv("IAZIO_AGY_BIN", customAgy)

	argv, err := BuildArgv(ExecutionRequest{
		Engine: "agy",
		Prompt: "test",
	})
	if err != nil {
		t.Fatalf("BuildArgv failed: %v", err)
	}
	if len(argv) == 0 || argv[0] != customAgy {
		t.Fatalf("expected argv[0] to be %q, got %+v", customAgy, argv)
	}
}

func TestRawWriter_OnStoryAllocated(t *testing.T) {
	var allocated string
	var mu sync.Mutex
	w := rawWriter{
		onStoryAllocated: func(s string) {
			mu.Lock()
			allocated = s
			mu.Unlock()
		},
	}

	_, _ = w.Write([]byte("Some log line\nALLOCATED_STORY_ID: STORY-GOV-9999\nAnother line"))
	mu.Lock()
	got := allocated
	mu.Unlock()

	if got != "STORY-GOV-9999" {
		t.Fatalf("expected STORY-GOV-9999, got %q", got)
	}
}


