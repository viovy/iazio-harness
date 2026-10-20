// Package engines builds IDE CLI argv, the child environment, and the checkout lock.
package engines

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/viovy/iazio-harness/internal/idempotency"
	"github.com/viovy/iazio-harness/internal/sandbox"
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
	// Sandbox executes child in an isolated ephemeral worktree snapshot.
	Sandbox       bool
	OnSandboxDiff func(string)
	IdempotencyClient *idempotency.Client
	StepIndex         int
	JobID             string
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
	if req.Engine != "" && c.Engine != req.Engine {
		c.Engine = req.Engine
	}
	// If attempting to run agent with an Antigravity conversation ID, auto-correct engine to agy
	if req.Engine == "agent" && req.ResumeConversationID != "" && (isAntigravityConversation(req.ResumeConversationID) || !strings.HasPrefix(strings.ToLower(req.ResumeConversationID), "cursor-")) {
		req.Engine = "agy"
		c.Engine = "agy"
		if req.Model == "" || req.Model == "auto" {
			req.Model = "gemini-3.8-flash-high"
		}
		if len(req.Argv) == 0 {
			req.Argv = nil
		}
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

	if req.IdempotencyClient != nil && req.ResumeConversationID != "" {
		commandStr := strings.Join(argv, " ")
		stepResp, err := req.IdempotencyClient.CheckOrRecordStep(ctx, idempotency.StepRequest{
			ConversationID: req.ResumeConversationID,
			SessionID:      req.JobID,
			StepIndex:      req.StepIndex,
			Command:        commandStr,
		})
		if err == nil && stepResp.AlreadyExecuted {
			if stream != nil {
				_ = stream.AppendRaw("stdout", []byte(fmt.Sprintf("[iazio-harness] Step %d already executed in conversation %s; skipping to preserve resume idempotency.\n", req.StepIndex, req.ResumeConversationID)))
			}
			return ExecutionResult{ExitCode: 0}, nil
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
	workDir := WorkingDirectory(req.Kind, req.WorktreePath, req.DocsHubPath)
	var sb *sandbox.Sandbox
	if req.Sandbox && workDir != "" {
		s, errS := sandbox.Create(workDir, sandbox.Options{})
		if errS != nil {
			return ExecutionResult{}, fmt.Errorf("create sandbox: %w", errS)
		}
		sb = s
		defer func() {
			if req.OnSandboxDiff != nil {
				if diff, errD := sb.Diff(); errD == nil {
					req.OnSandboxDiff(diff)
				}
			}
			_ = sb.Cleanup()
		}()
		workDir = sb.SandboxPath
	}
	cmd.Dir = workDir
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
		if stream != nil {
			_ = stream.AppendRaw("stderr", []byte(fmt.Sprintf("[iazio-harness] FATAL: failed to spawn %s: %v\n", bin, err)))
		}
		return ExecutionResult{}, err
	}
	postStartProcessGroup(cmd)
	if cmd.Process != nil && stream != nil {
		_ = stream.AppendRaw("stdout", []byte(fmt.Sprintf("[iazio-harness] Spawned engine process %s with PID %d\n", filepath.Base(bin), cmd.Process.Pid)))
	}
	done := make(chan struct{})
	go watchCancel(ctx, cmd, done)

	var scanned bool
	var convMu sync.Mutex
	var recentOut, recentErr string
	stdoutWriter := rawWriter{stream: "stdout", dst: stream, onConv: req.OnConversationID, scanned: &scanned, recent: &recentOut, mu: &convMu}
	stderrWriter := rawWriter{stream: "stderr", dst: stream, onConv: req.OnConversationID, scanned: &scanned, recent: &recentErr, mu: &convMu}

	var logOffset int64
	scanLogFile := func() {
		convMu.Lock()
		defer convMu.Unlock()
		if scanned {
			return
		}
		f, err := os.Open(req.LogFile)
		if err != nil {
			return
		}
		defer f.Close()
		fi, err := f.Stat()
		if err != nil || fi.Size() == 0 {
			return
		}
		if fi.Size() < logOffset {
			logOffset = 0
		}
		readFrom := logOffset
		if readFrom > 256 {
			readFrom -= 256
		} else {
			readFrom = 0
		}
		if _, err := f.Seek(readFrom, io.SeekStart); err != nil {
			return
		}
		data, err := io.ReadAll(f)
		if err == nil && len(data) > 0 {
			logOffset = fi.Size()
			if cid := extractConversationIDFromText(string(data)); cid != "" {
				scanned = true
				req.OnConversationID(cid)
			}
		}
	}

	if req.LogFile != "" && req.OnConversationID != nil {
		go func() {
			ticker := time.NewTicker(250 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-done:
					scanLogFile()
					return
				case <-ticker.C:
					scanLogFile()
					convMu.Lock()
					isScanned := scanned
					convMu.Unlock()
					if isScanned {
						return
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
	wg.Wait()
	waitErr := cmd.Wait()
	close(done)
	if req.LogFile != "" && req.OnConversationID != nil {
		scanLogFile()
	}
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
	if code == 0 && req.IdempotencyClient != nil && req.ResumeConversationID != "" {
		commandStr := strings.Join(argv, " ")
		_, _ = req.IdempotencyClient.CheckOrRecordStep(context.Background(), idempotency.StepRequest{
			ConversationID: req.ResumeConversationID,
			SessionID:      req.JobID,
			StepIndex:      req.StepIndex,
			Command:        commandStr,
			Status:         "completed",
		})
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
	recent  *string
	mu      *sync.Mutex
}

func (w rawWriter) Write(p []byte) (int, error) {
	if w.dst == nil || len(p) == 0 {
		return len(p), nil
	}
	if w.onConv != nil && w.scanned != nil && w.mu != nil {
		w.mu.Lock()
		if !*w.scanned {
			var combined string
			if w.recent != nil {
				combined = *w.recent + string(p)
				if len(combined) > 4096 {
					combined = combined[len(combined)-4096:]
				}
				*w.recent = combined
			} else {
				combined = string(p)
			}
			if cid := extractConversationID(combined); cid != "" {
				*w.scanned = true
				w.onConv(cid)
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

func extractConversationIDFromText(sData string) string {
	markers := []string{
		"Created conversation ",
		"Print mode: conversation=",
		"conversation=",
		"conversationID=",
		"conversation_id=",
		"Conversation ID: ",
		"Conversation ID=",
		"conversation: ",
		"Streaming conversation ",
		"GetConversationDetail: found conversation ",
	}
	for _, marker := range markers {
		searchData := sData
		for {
			idx := strings.Index(searchData, marker)
			if idx == -1 {
				break
			}
			rest := searchData[idx+len(marker):]
			fields := strings.FieldsFunc(rest, func(r rune) bool {
				return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == ',' || r == '"' || r == '\'' || r == '(' || r == ')'
			})
			if len(fields) > 0 {
				candidate := strings.Trim(fields[0], " .,;:\"'")
				if len(candidate) >= 32 {
					return candidate
				}
			}
			searchData = rest
		}
	}
	return ""
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
	if cid := extractConversationIDFromText(s); cid != "" {
		return cid
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

// isAntigravityConversation reports whether a conversation ID resides in the Antigravity brain store.
func isAntigravityConversation(cid string) bool {
	if cid == "" {
		return false
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return false
	}
	brainPath := filepath.Join(home, ".gemini", "antigravity-cli", "brain", cid)
	fi, err := os.Stat(brainPath)
	return err == nil && fi.IsDir()
}

// ResolveDynamicTool resolves the preferred tool using priority list and PATH availability.
func ResolveDynamicTool(engine string, priorityList []string) string {
	engine = strings.TrimSpace(strings.ToLower(engine))
	if engine != "" && engine != "auto" {
		return engine
	}
	if len(priorityList) == 0 {
		priorityList = []string{"agy", "agent", "copilot", "opencode"}
	}
	for _, tool := range priorityList {
		tool = strings.TrimSpace(strings.ToLower(tool))
		if tool == "" {
			continue
		}
		if _, err := exec.LookPath(tool); err == nil {
			return tool
		}
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			candidates := []string{
				filepath.Join(home, ".local", "bin", tool),
				filepath.Join(home, ".iazio", "bin", tool),
			}
			for _, cand := range candidates {
				if fi, errS := os.Stat(cand); errS == nil && !fi.IsDir() {
					return tool
				}
			}
		}
	}
	return "agy"
}

// BuildArgv returns the IDE CLI arguments for the engine.
// The prompt is a single argv element.
func BuildArgv(req ExecutionRequest) ([]string, error) {
	engine := strings.TrimSpace(strings.ToLower(req.Engine))
	if engine == "" || engine == "auto" {
		engine = ResolveDynamicTool(engine, nil)
	}
	switch engine {
	case "agent":
		argv := []string{"agent", "--print", "--output-format", "text", "--trust", "--force"}
		model := req.Model
		if model == "" || model == "gemini-3.8-flash-high" {
			model = "auto"
		}
		argv = append(argv, "--model", model)
		if req.ResumeConversationID != "" {
			argv = append(argv, "--resume", req.ResumeConversationID)
		}
		argv = append(argv, "--", req.Prompt)
		return argv, nil
	case "agy":
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
		argv = append(argv, "--dangerously-skip-permissions")
		if req.LogFile != "" {
			argv = append(argv, "--log-file", req.LogFile)
		}
		return argv, nil
	case "copilot":
		argv := []string{"copilot", "--prompt", req.Prompt}
		if req.Model != "" && req.Model != "auto" {
			argv = append(argv, "--model", req.Model)
		}
		return argv, nil
	case "opencode":
		return []string{"opencode", "run", "--auto", req.Prompt}, nil
	case "autopilot":
		return []string{"autopilot", "story", "begin", req.Prompt}, nil
	default:
		return nil, errors.New("unknown engine")
	}
}
