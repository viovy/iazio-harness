// Package engines builds IDE CLI argv, the child environment, and the checkout lock.
package engines

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
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
		return "cli"
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
	cmd := exec.Command(argv[0], argv[1:]...)
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
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(rawWriter{stream: "stdout", dst: stream}, stdout)
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(rawWriter{stream: "stderr", dst: stream}, stderr)
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
	stream string
	dst    EventStream
}

func (w rawWriter) Write(p []byte) (int, error) {
	if w.dst == nil || len(p) == 0 {
		return len(p), nil
	}
	buf := append([]byte(nil), p...)
	if err := w.dst.AppendRaw(w.stream, buf); err != nil {
		return 0, err
	}
	return len(p), nil
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
		return []string{"agent", "--print", "--output-format", "text", "--trust", "--force", "--", req.Prompt}, nil
	case "agy":
		if req.PrintTimeout == "" || req.Model == "" {
			return nil, errors.New("agy requires print timeout and model")
		}
		argv := []string{"agy", "--print", req.Prompt, "--print-timeout", req.PrintTimeout, "--model", req.Model}
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
