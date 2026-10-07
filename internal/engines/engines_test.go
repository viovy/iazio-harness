package engines

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
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
			want: []string{"agent", "--print", "--output-format", "text", "--trust", "--force", "--", "do thing"},
			cwd:  "/repos/leaf-01",
		},
		{
			name: "agent prompt dash",
			req: ExecutionRequest{
				Engine: "agent", Prompt: "-n", Kind: "execute",
				WorktreePath: "/repos/leaf-01", DocsHubPath: "/repos/docs-hub",
			},
			want: []string{"agent", "--print", "--output-format", "text", "--trust", "--force", "--", "-n"},
			cwd:  "/repos/leaf-01",
		},
		{
			name: "agent refinement cwd",
			req: ExecutionRequest{
				Engine: "agent", Prompt: "p", Kind: KindStoryRefinement,
				WorktreePath: "/repos/leaf-01", DocsHubPath: "/repos/docs-hub",
			},
			want: []string{"agent", "--print", "--output-format", "text", "--trust", "--force", "--", "p"},
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
			want: []string{"agy", "--print", "p", "--print-timeout", "10s", "--model", "m"},
			cwd:  "/repos/docs-hub",
		},
		{
			name: "agy defaults",
			req: ExecutionRequest{
				Engine: "agy", Prompt: "hello", Kind: "execute",
				WorktreePath: "/repos/leaf-01", DocsHubPath: "/repos/docs-hub",
			},
			want: []string{"agy", "--print", "hello", "--print-timeout", "10m", "--model", "gemini-3.8-flash-high"},
			cwd:  "/repos/leaf-01",
		},
		{
			name: "empty engine defaults to agy",
			req: ExecutionRequest{
				Engine: "", Prompt: "hello", Kind: "execute",
				WorktreePath: "/repos/leaf-01", DocsHubPath: "/repos/docs-hub",
			},
			want: []string{"agy", "--print", "hello", "--print-timeout", "10m", "--model", "gemini-3.8-flash-high"},
			cwd:  "/repos/leaf-01",
		},
		{
			name: "agent with resume",
			req: ExecutionRequest{
				Engine: "agent", Prompt: "continue work", Kind: "execute",
				WorktreePath: "/repos/leaf-01", DocsHubPath: "/repos/docs-hub",
				ResumeConversationID: "chat-uuid-123",
			},
			want: []string{"agent", "--print", "--output-format", "text", "--trust", "--force", "--resume", "chat-uuid-123", "--", "continue work"},
			cwd:  "/repos/leaf-01",
		},
		{
			name: "agy with resume",
			req: ExecutionRequest{
				Engine: "agy", Prompt: "continue work", Kind: "execute",
				WorktreePath: "/repos/leaf-01", DocsHubPath: "/repos/docs-hub",
				ResumeConversationID: "conv-uuid-456",
			},
			want: []string{"agy", "--print", "continue work", "--print-timeout", "10m", "--model", "gemini-3.8-flash-high", "--conversation", "conv-uuid-456"},
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

func TestChildStdinIsDevNull(t *testing.T) {
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
		Argv:         []string{"/bin/sh", script},
		BaseEnv:      []string{"OUT=" + out, "PATH=/bin"},
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
		Argv:         []string{"/bin/sh", script},
		BaseEnv:      []string{"OUT=" + out, "PATH=/bin"},
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
	want := "0\nyes\n\n/repos/docs-hub\n"
	if string(got) != want {
		t.Fatalf("got %q want %q", got, want)
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
	events []string
}

func (s *testCaptureStream) AppendRaw(stream string, p []byte) error {
	s.events = append(s.events, string(p))
	return nil
}

func TestEarlyConversationIDStreaming(t *testing.T) {
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
		Argv:         []string{"/bin/sh", script},
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
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for OnConversationID callback")
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
	logLine3 := "Finished task in conversation=" + uuid1 + "."
	if got := extractConversationIDFromText(logLine3); got != uuid1 {
		t.Fatalf("expected %s, got %s", uuid1, got)
	}

	// Earlier false-positive marker occurrence should not block later valid UUID
	logLine4 := "Flags: conversation=none\nServer: conversation=" + uuid2
	if got := extractConversationIDFromText(logLine4); got != uuid2 {
		t.Fatalf("expected %s, got %s", uuid2, got)
	}

	stdoutText := "Session initialized: Created conversation " + uuid1 + "\n"
	if got := extractConversationID(stdoutText); got != uuid1 {
		t.Fatalf("expected %s from extractConversationID, got %s", uuid1, got)
	}
}

func TestLogFileConversationIDPostExit(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "engine.log")
	uuid := "e1234567-89ab-cdef-0123-456789abcdef"

	script := filepath.Join(dir, "run_log.sh")
	// Writes to log file right before exiting
	body := "#!/bin/sh\nsleep 0.05\necho 'Created conversation " + uuid + "' > \"" + logFile + "\"\n"
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
		Argv:         []string{"/bin/sh", script},
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
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for log file OnConversationID callback")
	}

	mu.Lock()
	defer mu.Unlock()
	if capturedID != uuid {
		t.Fatalf("expected %q, got %q", uuid, capturedID)
	}
}

