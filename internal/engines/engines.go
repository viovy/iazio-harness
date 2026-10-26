// Package engines builds IDE CLI argv, the child environment, and the checkout lock.
package engines

import (
	"bytes"
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
	"github.com/viovy/iazio-harness/internal/sessionstore"
)

// KindStoryRefinement selects the docs hub as the child working directory and lock target.
const (
	KindStoryRefinement = "story_refinement"
	flagStory           = "--story"
)

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

// PreFlightHook is executed prior to child execution if set.
var PreFlightHook func(ctx context.Context, workDir, storyID string) error

// ExecutionRequest is one IDE CLI invocation.
type ExecutionRequest struct {
	Engine                     string
	Kind                       string
	StoryID                    string
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
	OnStoryAllocated           func(string)
	OnPID                      func(int)
	// Argv, when non-empty, replaces BuildArgv. Tests use it to run a local program.
	Argv []string
	// BaseEnv, when non-nil, replaces the inherited environment before schedule and forced keys.
	BaseEnv     []string
	ScheduleEnv map[string]string
	// Sandbox executes child in an isolated ephemeral worktree snapshot.
	Sandbox           bool
	OnSandboxDiff     func(string)
	IdempotencyClient *idempotency.Client
	StepIndex         int
	JobID             string
}

// ExecutionResult is the child exit state.
type ExecutionResult struct {
	ExitCode        int
	BackgroundTasks []BackgroundTaskInfo
	WasAutoReaped   bool
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
	resolved, err := look(c.Name())
	if err == nil {
		if lfi, errL := os.Lstat(resolved); errL == nil && (lfi.Mode()&os.ModeSymlink != 0) {
			if _, errS := filepath.EvalSymlinks(resolved); errS != nil {
				return fmt.Errorf("engine %s is a broken symlink: %w", c.Name(), errS)
			}
		}
		return nil
	}
	if c.LookPath == nil {
		if home, errH := os.UserHomeDir(); errH == nil && home != "" {
			candidates := []string{
				filepath.Join(home, ".local", "bin", c.Name()),
				filepath.Join(home, ".iazio", "bin", c.Name()),
				filepath.Join(home, ".gemini", "antigravity-cli", "bin", c.Name()),
			}
			for _, cand := range candidates {
				if lfi, errL := os.Lstat(cand); errL == nil && !lfi.IsDir() {
					if lfi.Mode()&os.ModeSymlink != 0 {
						if _, errS := filepath.EvalSymlinks(cand); errS != nil {
							continue
						}
					}
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
	// If engine is set to iazio-harness or contains harness, auto-correct engine to autopilot or agy
	if strings.Contains(strings.ToLower(req.Engine), "harness") || strings.Contains(strings.ToLower(c.Engine), "harness") {
		targetEngine := "agy"
		if req.StoryID != "" && req.ResumeConversationID == "" && req.Kind == KindStoryRefinement {
			targetEngine = "autopilot"
		}
		req.Engine = targetEngine
		c.Engine = targetEngine
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

	if req.Engine == "agent" && req.ResumeConversationID == "" && req.OnConversationID != nil && len(req.Argv) == 0 {
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
		if lfi, errL := os.Lstat(resolved); errL == nil && (lfi.Mode()&os.ModeSymlink != 0) {
			if realPath, errS := filepath.EvalSymlinks(resolved); errS == nil {
				bin = realPath
			} else {
				return ExecutionResult{}, fmt.Errorf("engine binary %s is a broken symlink: %w", resolved, errS)
			}
		} else {
			bin = resolved
		}
	} else if home, err := os.UserHomeDir(); err == nil && home != "" {
		candidates := []string{
			filepath.Join(home, ".local", "bin", bin),
			filepath.Join(home, ".iazio", "bin", bin),
			filepath.Join(home, ".gemini", "antigravity-cli", "bin", bin),
		}
		for _, cand := range candidates {
			if lfi, errL := os.Lstat(cand); errL == nil && !lfi.IsDir() {
				if lfi.Mode()&os.ModeSymlink != 0 {
					if realPath, errS := filepath.EvalSymlinks(cand); errS == nil {
						bin = realPath
						break
					}
					continue
				}
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
	if PreFlightHook != nil && req.StoryID != "" && workDir != "" {
		if pfErr := PreFlightHook(ctx, workDir, req.StoryID); pfErr != nil {
			if stream != nil {
				_ = stream.AppendRaw("stderr", []byte(fmt.Sprintf("[iazio-harness] Warning: pre-flight story materialization failed: %v\n", pfErr)))
			}
		}
	}
	base := req.BaseEnv
	if req.BaseEnv == nil {
		base = os.Environ()
	}
	cmd.Env = ChildEnv(base, req.ScheduleEnv, req.DocsHubPath)
	cmd.Env = append(cmd.Env,
		EnvManagedBy+"=harness",
	)
	if req.JobID != "" {
		cmd.Env = append(cmd.Env, EnvJobID+"="+req.JobID)
	}
	if req.StoryID != "" {
		cmd.Env = append(cmd.Env, EnvStoryID+"="+req.StoryID)
	}
	setProcGroup(cmd)
	stdin, err := os.Open(os.DevNull)
	if err != nil {
		return ExecutionResult{}, err
	}
	defer stdin.Close()
	cmd.Stdin = stdin

	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		return ExecutionResult{}, err
	}
	defer stdoutR.Close()
	cmd.Stdout = stdoutW

	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		_ = stdoutW.Close()
		return ExecutionResult{}, err
	}
	defer stderrR.Close()
	cmd.Stderr = stderrW

	if err := cmd.Start(); err != nil {
		_ = stdoutW.Close()
		_ = stderrW.Close()
		if stream != nil {
			_ = stream.AppendRaw("stderr", []byte(fmt.Sprintf("[iazio-harness] FATAL: failed to spawn %s: %v\n", bin, err)))
		}
		return ExecutionResult{}, err
	}
	_ = stdoutW.Close()
	_ = stderrW.Close()
	postStartProcessGroup(cmd)
	if cmd.Process != nil {
		if stream != nil {
			_ = stream.AppendRaw("stdout", []byte(fmt.Sprintf("[iazio-harness] Spawned engine process %s with PID %d\n", filepath.Base(bin), cmd.Process.Pid)))
		}
		if req.OnPID != nil {
			req.OnPID(cmd.Process.Pid)
		}
	}
	done := make(chan struct{})
	var doneOnce sync.Once
	safeCloseDone := func() {
		doneOnce.Do(func() {
			close(done)
		})
	}
	defer safeCloseDone()

	var scanned bool
	var convMu sync.Mutex
	var recentOut, recentErr string
	var stderrLogFile *os.File
	if req.LogFile != "" {
		stderrLogPath := strings.TrimSuffix(req.LogFile, ".log") + ".stderr.log"
		_ = os.MkdirAll(filepath.Dir(stderrLogPath), 0755)
		if f, err := os.OpenFile(stderrLogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644); err == nil {
			stderrLogFile = f
			defer stderrLogFile.Close()
		}
	}

	var supervisor *TaskSupervisor
	if cmd.Process != nil && cmd.Process.Pid > 0 {
		supervisor = NewTaskSupervisor(cmd.Process.Pid, stream)
		defer supervisor.Stop()
	}

	stdoutWriter := rawWriter{stream: "stdout", dst: stream, onConv: req.OnConversationID, onStoryAllocated: req.OnStoryAllocated, scanned: &scanned, recent: &recentOut, mu: &convMu, resumeConvID: req.ResumeConversationID, logFile: nil, supervisor: supervisor}
	stderrWriter := rawWriter{stream: "stderr", dst: stream, onConv: req.OnConversationID, onStoryAllocated: req.OnStoryAllocated, scanned: &scanned, recent: &recentErr, mu: &convMu, resumeConvID: req.ResumeConversationID, logFile: stderrLogFile, supervisor: supervisor}

	startTime := time.Now().Add(-2 * time.Second)
	var logOffset int64
	scanLogFile := func() {
		convMu.Lock()
		defer convMu.Unlock()
		if scanned {
			return
		}
		if req.LogFile != "" {
			f, err := os.Open(req.LogFile)
			if err == nil {
				defer f.Close()
				fi, errS := f.Stat()
				if errS == nil && fi.Size() > 0 {
					if fi.Size() < logOffset {
						logOffset = 0
					}
					readFrom := logOffset
					if readFrom > 256 {
						readFrom -= 256
					} else {
						readFrom = 0
					}
					if _, errSeek := f.Seek(readFrom, io.SeekStart); errSeek == nil {
						data, errR := io.ReadAll(f)
						if errR == nil && len(data) > 0 {
							logOffset = fi.Size()
							if cid := extractConversationIDFromText(string(data), ""); cid != "" {
								scanned = true
								req.OnConversationID(cid)
								return
							}
						}
					}
				}
			}
		}

		// When ResumeConversationID is set, restrict discovery strictly to verifying the requested conversation:
		// Only check if <req.ResumeConversationID>.db was updated after startTime.
		// Never scan shared ~/.gemini/antigravity-cli/cli.log or arbitrary foreign .db files (Finding H1).
		if req.ResumeConversationID != "" {
			if home, errH := os.UserHomeDir(); errH == nil && home != "" {
				targetDb := filepath.Join(home, ".gemini", "antigravity-cli", "conversations", req.ResumeConversationID+".db")
				if fi, errStat := os.Stat(targetDb); errStat == nil && fi.ModTime().After(startTime) {
					scanned = true
					req.OnConversationID(req.ResumeConversationID)
					return
				}
			}
			return
		}

		// When per-job LogFile is specified or for managed harness jobs, restrict discovery strictly to req.LogFile.
		// Shared cli.log has no per-job isolation and causes cross-job session hijacking under concurrency (Finding H1).
		if req.LogFile != "" || req.JobID != "" {
			return
		}

		// Fallback for unmanaged/standalone CLI invocations without a JobID:
		if home, errH := os.UserHomeDir(); errH == nil && home != "" {
			cliLog := filepath.Join(home, ".gemini", "antigravity-cli", "cli.log")
			if fi, errStat := os.Stat(cliLog); errStat == nil && fi.Size() > 0 && fi.ModTime().After(startTime) {
				if fCli, errO := os.Open(cliLog); errO == nil {
					defer fCli.Close()
					var seekPos int64
					if fi.Size() > 16384 {
						seekPos = fi.Size() - 16384
					}
					_, _ = fCli.Seek(seekPos, io.SeekStart)
					dataCli, _ := io.ReadAll(fCli)
					if len(dataCli) > 0 {
						if cid := extractConversationIDFromText(string(dataCli), ""); cid != "" {
							scanned = true
							req.OnConversationID(cid)
							return
						}
					}
				}
			}

			// Also inspect newly created conversation DBs in ~/.gemini/antigravity-cli/conversations/
			convDir := filepath.Join(home, ".gemini", "antigravity-cli", "conversations")
			if entries, errD := os.ReadDir(convDir); errD == nil {
				for _, entry := range entries {
					if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".db") {
						continue
					}
					info, errI := entry.Info()
					if errI == nil && info.ModTime().After(startTime) {
						cid := strings.TrimSuffix(entry.Name(), ".db")
						if len(cid) >= 32 && sessionstore.IsValidConversationID(cid) {
							scanned = true
							req.OnConversationID(cid)
							return
						}
					}
				}
			}
		}
	}

	if (req.LogFile != "" || req.Engine == "agy") && req.OnConversationID != nil {
		go func() {
			ticker := time.NewTicker(50 * time.Millisecond)
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
		_, _ = io.Copy(stdoutWriter, stdoutR)
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(stderrWriter, stderrR)
	}()

	waitCh := make(chan error, 1)
	go func() {
		// Monitor the primary engine process (tool_pid) directly rather than blocking on
		// inherited pipe descriptors held open by persistent grandchild MCP daemons.
		if cmd.Process != nil {
			state, err := cmd.Process.Wait()
			if err != nil {
				waitCh <- err
			} else if !state.Success() {
				waitCh <- &exec.ExitError{ProcessState: state}
			} else {
				waitCh <- nil
			}
		} else {
			waitCh <- cmd.Wait()
		}
	}()

	pipesDone := make(chan struct{})
	go func() {
		wg.Wait()
		close(pipesDone)
	}()

	var waitErr error
	select {
	case waitErr = <-waitCh:
		// Primary engine process exited. Allow a bounded window for stdout/stderr to drain.
		select {
		case <-pipesDone:
		case <-time.After(2 * time.Second):
			// Force close pipes to unblock any io.Copy held open by orphaned background grandchildren
			_ = stdoutR.Close()
			_ = stderrR.Close()
			select {
			case <-pipesDone:
			case <-time.After(500 * time.Millisecond):
			}
		}
		// Reap any lingering grandchild MCP server daemons in the job's process group
		killProcessGroup(cmd)
	case <-ctx.Done():
		stopProcessGroup(cmd)
		select {
		case waitErr = <-waitCh:
		case <-time.After(3 * time.Second):
			killProcessGroup(cmd)
			waitErr = <-waitCh
		}
		_ = stdoutR.Close()
		_ = stderrR.Close()
		select {
		case <-pipesDone:
		case <-time.After(500 * time.Millisecond):
		}
	}

	safeCloseDone()
	// Ensure no background child or grandchild MCP servers outlive the parent process group
	killProcessGroup(cmd)
	if req.LogFile != "" && req.OnConversationID != nil {
		scanLogFile()
	}
	var bgTasks []BackgroundTaskInfo
	var wasReaped bool
	if supervisor != nil {
		bgTasks = supervisor.ActiveTasks()
		if len(bgTasks) == 0 {
			bgTasks = supervisor.ReapedTasks()
		}
		wasReaped = supervisor.WasAutoReaped()
	}

	code := exitCode(waitErr)
	if ctx.Err() != nil {
		return ExecutionResult{ExitCode: code, BackgroundTasks: bgTasks, WasAutoReaped: wasReaped}, ctx.Err()
	}
	if waitErr != nil {
		var ee *exec.ExitError
		if errors.As(waitErr, &ee) {
			return ExecutionResult{ExitCode: code, BackgroundTasks: bgTasks, WasAutoReaped: wasReaped}, nil
		}
		return ExecutionResult{ExitCode: code, BackgroundTasks: bgTasks, WasAutoReaped: wasReaped}, waitErr
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
	return ExecutionResult{ExitCode: code, BackgroundTasks: bgTasks, WasAutoReaped: wasReaped}, nil
}

func watchCancel(ctx context.Context, cmd *exec.Cmd, done <-chan struct{}) {
	select {
	case <-ctx.Done():
		stopProcessGroup(cmd)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
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
	stream           string
	dst              EventStream
	onConv           func(string)
	onStoryAllocated func(string)
	scanned          *bool
	recent           *string
	mu               *sync.Mutex
	resumeConvID     string
	logFile          *os.File
	supervisor       *TaskSupervisor
}

func cleanRootAgentIdle(p []byte) []byte {
	if !bytes.Contains(p, []byte("root agent idle; waiting up to")) {
		return p
	}
	lines := bytes.Split(p, []byte("\n"))
	var filtered [][]byte
	for _, l := range lines {
		if !bytes.Contains(l, []byte("root agent idle; waiting up to")) {
			filtered = append(filtered, l)
		}
	}
	if len(filtered) == 0 || (len(filtered) == 1 && len(filtered[0]) == 0) {
		return nil
	}
	return bytes.Join(filtered, []byte("\n"))
}

func (w rawWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	pClean := cleanRootAgentIdle(p)
	if w.supervisor != nil && len(pClean) > 0 {
		w.supervisor.OnOutput(pClean)
	}
	if w.logFile != nil && len(pClean) > 0 {
		_, _ = w.logFile.Write(pClean)
	}
	if w.onStoryAllocated != nil && len(pClean) > 0 {
		str := string(pClean)
		for _, prefix := range []string{"ALLOCATED_STORY_ID:", "STORY_ID:", "Bound active story ID:", "Bound dynamically allocated story ID:"} {
			if idx := strings.Index(str, prefix); idx != -1 {
				rem := strings.TrimLeft(str[idx+len(prefix):], " \t")
				if endIdx := strings.IndexAny(rem, "\r\n ,;()"); endIdx != -1 {
					rem = rem[:endIdx]
				}
				storyID := strings.TrimSpace(rem)
				if storyID != "" && storyID != "pending-allocation" && storyID != "pending_harness_creation" {
					w.onStoryAllocated(storyID)
					break
				}
			}
		}
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
			cid := extractConversationID(combined, w.resumeConvID)
			if cid == "" {
				cid = extractConversationIDFromText(combined, w.resumeConvID)
			}
			if cid != "" {
				if w.resumeConvID != "" && cid != w.resumeConvID {
					// Ignore foreign conversation ID emitted on stdout/stderr during a resume run (Finding M1)
				} else {
					*w.scanned = true
					w.onConv(cid)
				}
			}
		}
		w.mu.Unlock()
	}
	if w.dst != nil && len(pClean) > 0 {
		buf := append([]byte(nil), pClean...)
		if err := w.dst.AppendRaw(w.stream, buf); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

func extractConversationIDFromText(sData string, resumeConvIDs ...string) string {
	var resumeConvID string
	if len(resumeConvIDs) > 0 {
		resumeConvID = resumeConvIDs[0]
	}
	markers := []string{
		"Created conversation ",
		"created conversation ",
		"Resumed conversation ",
		"resumed conversation ",
		"Resuming conversation ",
		"resuming conversation ",
		"Loaded conversation ",
		"loaded conversation ",
		"Active conversation ",
		"active conversation ",
		"Using conversation ",
		"using conversation ",
		"Print mode: conversation=",
		"Conversation ID: ",
		"conversation ID: ",
		"Conversation ID=",
		"conversation ID=",
		"Streaming conversation ",
		"streaming conversation ",
		"GetConversationDetail: found conversation ",
	}
	var firstCandidate string
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
				candidate = strings.TrimPrefix(candidate, "--conversation=")
				if len(candidate) >= 32 && sessionstore.IsValidConversationID(candidate) {
					if resumeConvID != "" && candidate == resumeConvID {
						return candidate
					}
					if firstCandidate == "" {
						firstCandidate = candidate
					}
				}
			}
			searchData = rest
		}
	}
	if resumeConvID == "" {
		return firstCandidate
	}
	return ""
}

func extractConversationID(s string, resumeConvIDs ...string) string {
	var resumeConvID string
	if len(resumeConvIDs) > 0 {
		resumeConvID = resumeConvIDs[0]
	}
	keys := []string{`"conversation_id":"`, `"session_id":"`, `"conversationId":"`, `"sessionId":"`}
	var firstCandidate string
	for _, k := range keys {
		search := s
		for {
			idx := strings.Index(search, k)
			if idx == -1 {
				break
			}
			start := idx + len(k)
			end := strings.IndexByte(search[start:], '"')
			if end != -1 {
				val := strings.TrimSpace(search[start : start+end])
				if len(val) > 0 && sessionstore.IsValidConversationID(val) {
					if resumeConvID != "" && val == resumeConvID {
						return val
					}
					if firstCandidate == "" {
						firstCandidate = val
					}
				}
				search = search[start+end:]
			} else {
				break
			}
		}
	}
	if cid := extractConversationIDFromText(s, resumeConvID); cid != "" {
		return cid
	}
	if resumeConvID == "" {
		return firstCandidate
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
				filepath.Join(home, ".gemini", "antigravity-cli", "bin", tool),
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
	if strings.Contains(engine, "harness") {
		if req.StoryID != "" && req.ResumeConversationID == "" && req.Kind == KindStoryRefinement {
			engine = "autopilot"
		} else {
			engine = "agy"
		}
	}

	trimmedPrompt := strings.TrimSpace(req.Prompt)
	isEmptyOrBareResume := trimmedPrompt == "" ||
		strings.EqualFold(trimmedPrompt, "resume") ||
		strings.EqualFold(trimmedPrompt, "/autonomous-story-execution resume")

	if req.ResumeConversationID == "" {
		if strings.EqualFold(trimmedPrompt, "resume") ||
			strings.EqualFold(trimmedPrompt, "/autonomous-story-execution resume") ||
			(req.Kind == "resume" && trimmedPrompt == "") {
			return nil, fmt.Errorf("cannot execute engine %q with prompt %q when ResumeConversationID is empty", engine, req.Prompt)
		}
	} else if isEmptyOrBareResume || req.Kind == "resume" {
		req.Prompt = "resume"
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
	case "agy", "mock-agy":
		timeout := req.PrintTimeout
		if timeout == "" || timeout == "0" || timeout == "0s" || timeout == "none" || timeout == "unbounded" {
			timeout = "0"
		}
		model := req.Model
		if model == "" {
			model = "gemini-3.8-flash-high"
		}
		prompt := req.Prompt
		if prompt == "" {
			prompt = "resume"
		}
		bin := engine
		if envBin := os.Getenv("IAZIO_AGY_BIN"); envBin != "" {
			bin = envBin
		} else if bin == "agy" {
			if _, err := exec.LookPath(bin); err != nil {
				if _, errG := exec.LookPath("gemini"); errG == nil {
					bin = "gemini"
				}
			}
		}
		argv := []string{bin, "--print", prompt, "--print-timeout", timeout, "--model", model}
		if req.ResumeConversationID != "" {
			argv = append(argv, "--conversation="+req.ResumeConversationID)
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
		if req.Kind == "story_begin" {
			argv := []string{"autopilot", "story", "begin"}
			if req.StoryID != "" {
				argv = append(argv, flagStory, req.StoryID)
			}
			return argv, nil
		}
		if req.Kind == "story_execution" || req.Kind == "story_run" || (req.Kind == "" && req.StoryID != "") {
			argv := []string{"autopilot", "story", "run"}
			if req.StoryID != "" {
				argv = append(argv, flagStory, req.StoryID)
			}
			return argv, nil
		}
		argv := []string{"autopilot", "prompt", "run"}
		if req.StoryID != "" {
			argv = append(argv, flagStory, req.StoryID)
		}
		if req.Prompt != "" {
			argv = append(argv, req.Prompt)
		}
		return argv, nil
	default:
		return nil, errors.New("unknown engine")
	}
}
