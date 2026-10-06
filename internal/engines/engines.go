// Package engines builds IDE CLI argv, the child environment, and the checkout lock.
package engines

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// KindStoryRefinement selects the docs hub as the child working directory and lock target.
const KindStoryRefinement = "story_refinement"

// ToolRunner is the seam that starts one IDE CLI and streams its output.
type ToolRunner interface {
	// Name returns the engine name.
	Name() string
	// Execute starts the child and returns when it has exited.
	// On context cancel the child process group is stopped before Execute returns.
	Execute(ctx context.Context, req ExecutionRequest, stream EventStream) (ExecutionResult, error)
	// HealthCheck reports whether the engine binary can be found.
	HealthCheck() error
}

// EventStream receives unmodified child bytes, including ANSI sequences.
// Implementations must not copy schedule environment into emitted events.
type EventStream interface {
	AppendRaw(stream string, p []byte) error
}

// ExecutionRequest is one IDE CLI invocation.
type ExecutionRequest struct {
	Engine                     string
	Kind                       string
	Prompt                     string
	WorktreePath               string
	DocsHubPath                string
	PrintTimeout               string
	Model                      string
	Agent                      string
	DisableSlashCommands       bool
	DangerouslySkipPermissions bool
	LogFile                    string
	ResumeConversationID       string
	OnConversationID           func(string)
	// Argv, when non-empty, replaces BuildArgv. Tests use it to run a local program.
	Argv []string
	// BaseEnv, when non-nil, replaces the inherited environment before schedule and forced keys.
	BaseEnv     []string
	ScheduleEnv map[string]string
}

// ExecutionResult is the child exit state.
type ExecutionResult struct {
	ExitCode int
}

// CLI runs agent, agy, or opencode.
type CLI struct {
	Engine   string
	LookPath func(string) (string, error)
}

// Name returns the configured engine name.
func (c CLI) Name() string {
	if c.Engine == "" {
		return "agy"
	}
	return c.Engine
}

// HealthCheck reports whether Name is on PATH.
func (c CLI) HealthCheck() error {
	look := c.LookPath
	if look == nil {
		look = exec.LookPath
	}
	_, err := look(c.Name())
	if err == nil {
		return nil
	}
	if c.LookPath == nil {
		if home, errH := os.UserHomeDir(); errH == nil && home != "" {
			candidates := []string{
				filepath.Join(home, ".local", "bin", c.Name()),
				filepath.Join(home, ".iazio", "bin", c.Name()),
			}
			for _, cand := range candidates {
				if fi, errS := os.Stat(cand); errS == nil && !fi.IsDir() {
					return nil
				}
			}
		}
	}
	return err
}

// Execute starts the child with stdin on os.DevNull.
// A cancelled context stops the child process group before Execute returns.
func (c CLI) Execute(ctx context.Context, req ExecutionRequest, stream EventStream) (ExecutionResult, error) {
	if err := ctx.Err(); err != nil {
		return ExecutionResult{}, err
	}
	argv := req.Argv
	if len(argv) == 0 {
		built, err := BuildArgv(req)
		if err != nil {
			return ExecutionResult{}, err
		}
		argv = built
	}
	if len(argv) == 0 || argv[0] == "" {
		return ExecutionResult{}, errors.New("empty argv")
	}

	if req.ResumeConversationID != "" && req.OnConversationID != nil {
		req.OnConversationID(req.ResumeConversationID)
	} else if req.Engine == "agent" && req.OnConversationID != nil && len(req.Argv) == 0 {
		look := c.LookPath
		if look == nil {
			look = exec.LookPath
		}
		agentBin := "agent"
		if resolved, err := look(agentBin); err == nil {
			agentBin = resolved
		}
		chatCmd := exec.CommandContext(ctx, agentBin, "create-chat")
		if out, err := chatCmd.Output(); err == nil {
			createdID := strings.TrimSpace(string(out))
			if createdID != "" {
				req.ResumeConversationID = createdID
				req.OnConversationID(createdID)
				if rebuilt, errB := BuildArgv(req); errB == nil {
					argv = rebuilt
				}
			}
		}
	}

	bin := argv[0]
	if resolved, err := exec.LookPath(bin); err == nil {
		bin = resolved
	} else if home, err := os.UserHomeDir(); err == nil && home != "" {
		candidates := []string{
			filepath.Join(home, ".local", "bin", bin),
			filepath.Join(home, ".iazio", "bin", bin),
		}
		for _, cand := range candidates {
			if fi, err := os.Stat(cand); err == nil && !fi.IsDir() {
				bin = cand
				break
			}
		}
	}
	cmd := exec.Command(bin, argv[1:]...)
	cmd.Dir = WorkingDirectory(req.Kind, req.WorktreePath, req.DocsHubPath)
	base := req.BaseEnv
	if req.BaseEnv == nil {
		base = os.Environ()
	}
	cmd.Env = ChildEnv(base, req.ScheduleEnv, req.DocsHubPath)
	setProcGroup(cmd)
	stdin, err := os.Open(os.DevNull)
	if err != nil {
		return ExecutionResult{}, err
	}
	defer stdin.Close()
	cmd.Stdin = stdin
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return ExecutionResult{}, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return ExecutionResult{}, err
	}
	if err := cmd.Start(); err != nil {
		return ExecutionResult{}, err
	}
	done := make(chan struct{})
	go watchCancel(ctx, cmd, done)

	var scanned bool
	var convMu sync.Mutex
	stdoutWriter := rawWriter{stream: "stdout", dst: stream, onConv: req.OnConversationID, scanned: &scanned, mu: &convMu}
	stderrWriter := rawWriter{stream: "stderr", dst: stream, onConv: req.OnConversationID, scanned: &scanned, mu: &convMu}

	if req.LogFile != "" && req.OnConversationID != nil {
		go func() {
			for i := 0; i < 20; i++ {
				select {
				case <-done:
					return
				case <-time.After(250 * time.Millisecond):
					convMu.Lock()
					isScanned := scanned
					convMu.Unlock()
					if isScanned {
						return
					}
					if data, err := os.ReadFile(req.LogFile); err == nil && len(data) > 0 {
						sData := string(data)
						const marker = "Created conversation "
						if idx := strings.Index(sData, marker); idx != -1 {
							rest := sData[idx+len(marker):]
							fields := strings.Fields(rest)
							if len(fields) > 0 && len(fields[0]) >= 32 {
								convID := strings.TrimSpace(fields[0])
								convMu.Lock()
								if !scanned {
									scanned = true
									go req.OnConversationID(convID)
								}
								convMu.Unlock()
								return
							}
						}
					}
				}
			}
		}()
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(stdoutWriter, stdout)
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(stderrWriter, stderr)
	}()
	waitErr := cmd.Wait()
	wg.Wait()
	close(done)
	code := exitCode(waitErr)
	if ctx.Err() != nil {
		return ExecutionResult{ExitCode: code}, ctx.Err()
	}
	if waitErr != nil {
		var ee *exec.ExitError
		if errors.As(waitErr, &ee) {
			return ExecutionResult{ExitCode: code}, nil
		}
		return ExecutionResult{ExitCode: code}, waitErr
	}
	return ExecutionResult{ExitCode: code}, nil
}

func watchCancel(ctx context.Context, cmd *exec.Cmd, done <-chan struct{}) {
	select {
	case <-ctx.Done():
		stopProcessGroup(cmd)
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			killProcessGroup(cmd)
		}
	case <-done:
	}
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if status, ok := ee.Sys().(syscall.WaitStatus); ok {
			if status.Signaled() {
				return 128 + int(status.Signal())
			}
			return status.ExitStatus()
		}
		return ee.ExitCode()
	}
	return 1
}

type rawWriter struct {
	stream  string
	dst     EventStream
	onConv  func(string)
	scanned *bool
	mu      *sync.Mutex
}

func (w rawWriter) Write(p []byte) (int, error) {
	if w.dst == nil || len(p) == 0 {
		return len(p), nil
	}
	if w.onConv != nil && w.scanned != nil && w.mu != nil {
		w.mu.Lock()
		if !*w.scanned {
			if cid := extractConversationID(string(p)); cid != "" {
				*w.scanned = true
				go w.onConv(cid)
			}
		}
		w.mu.Unlock()
	}
	buf := append([]byte(nil), p...)
	if err := w.dst.AppendRaw(w.stream, buf); err != nil {
		return 0, err
	}
	return len(p), nil
}

func extractConversationID(s string) string {
	keys := []string{`"conversation_id":"`, `"session_id":"`, `"conversationId":"`, `"sessionId":"`}
	for _, k := range keys {
		idx := strings.Index(s, k)
		if idx != -1 {
			start := idx + len(k)
			end := strings.IndexByte(s[start:], '"')
			if end != -1 {
				val := strings.TrimSpace(s[start : start+end])
				if val != "" {
					return val
				}
			}
		}
	}
	return ""
}

// WorkingDirectory returns the docs hub for story_refinement and the worktree otherwise.
func WorkingDirectory(kind, worktree, docsHub string) string {
	if kind == KindStoryRefinement {
		return docsHub
	}
	return worktree
}

// BuildArgv returns the IDE CLI arguments for the engine.
// The prompt is a single argv element.
func BuildArgv(req ExecutionRequest) ([]string, error) {
	switch req.Engine {
	case "agent":
		argv := []string{"agent", "--print", "--output-format", "text", "--trust", "--force"}
		if req.ResumeConversationID != "" {
			argv = append(argv, "--resume", req.ResumeConversationID)
		}
		argv = append(argv, "--", req.Prompt)
		return argv, nil
	case "agy", "":
		timeout := req.PrintTimeout
		if timeout == "" {
			timeout = "10m"
		}
		model := req.Model
		if model == "" {
			model = "gemini-3.8-flash-high"
		}
		argv := []string{"agy", "--print", req.Prompt, "--print-timeout", timeout, "--model", model}
		if req.ResumeConversationID != "" {
			argv = append(argv, "--conversation", req.ResumeConversationID)
		}
		if req.Agent != "" {
			argv = append(argv, "--agent", req.Agent)
		}
		if req.DisableSlashCommands {
			argv = append(argv, "--disable-slash-commands")
		}
		if req.DangerouslySkipPermissions {
			argv = append(argv, "--dangerously-skip-permissions")
		}
		if req.LogFile != "" {
			argv = append(argv, "--log-file", req.LogFile)
		}
		return argv, nil
	case "opencode":
		return []string{"opencode", "run", "--auto", req.Prompt}, nil
	default:
		return nil, errors.New("unknown engine")
	}
}
