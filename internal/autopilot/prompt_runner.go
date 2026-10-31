// Package autopilot provides integration with the autopilot CLI, story governance, and prompt execution.
package autopilot

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/viovy/iazio-harness/internal/controlplane"
	"github.com/viovy/iazio-harness/internal/engines"
	"github.com/viovy/iazio-harness/internal/redact"
	"github.com/viovy/iazio-harness/internal/sessionstore"
)

var (
	storyIDRegex = regexp.MustCompile(`\b(STORY-[A-Za-z0-9_-]+)\b`)
	convIDRegex  = regexp.MustCompile(`(?i)(?:created|resumed|resuming|loaded|active|using|streaming)\s+conversation\s+([A-Za-z0-9_-]{12,64})`)
	hex16Regex   = regexp.MustCompile(`^[0-9a-f]{16}$`)
	hex32Regex   = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

const (
	// DefaultPromptTimeout is the default execution window for long-running agentic prompts (7 days).
	DefaultPromptTimeout = 7 * 24 * time.Hour
	defaultPromptTimeout = DefaultPromptTimeout
	defaultGracePeriod   = 5 * time.Second
	geminiDirName        = ".gemini"
	agyCliDirName        = "antigravity-cli"
)

// ResolveDefaultPromptTimeout returns the default prompt timeout, honoring IAZIO_PROMPT_TIMEOUT_SECONDS when set to a positive integer.
func ResolveDefaultPromptTimeout() time.Duration {
	if raw := strings.TrimSpace(os.Getenv("IAZIO_PROMPT_TIMEOUT_SECONDS")); raw != "" {
		if sec, err := strconv.Atoi(raw); err == nil && sec > 0 {
			return time.Duration(sec) * time.Second
		}
	}
	return DefaultPromptTimeout
}

func formatW3CTraceParent(rawTraceID, rawSpanID string) (string, string) {
	traceLower := strings.ToLower(strings.TrimSpace(rawTraceID))
	if !hex32Regex.MatchString(traceLower) || traceLower == "00000000000000000000000000000000" {
		sum := sha256.Sum256([]byte(rawTraceID))
		traceLower = hex.EncodeToString(sum[:16])
	}
	spanLower := strings.ToLower(strings.TrimSpace(rawSpanID))
	if !hex16Regex.MatchString(spanLower) || spanLower == "0000000000000000" {
		seed := rawSpanID
		if strings.TrimSpace(seed) == "" {
			seed = fmt.Sprintf("span-%d", time.Now().UnixNano())
		}
		sum := sha256.Sum256([]byte(seed))
		spanLower = hex.EncodeToString(sum[:8])
	}
	return traceLower, fmt.Sprintf("00-%s-%s-01", traceLower, spanLower)
}

// RunPromptOptions specifies parameters for executing a prompt via autopilot.
type RunPromptOptions struct {
	PromptID     string        `json:"prompt_id"`
	JobID        string        `json:"job_id,omitempty"`
	APIURL       string        `json:"api_url,omitempty"`
	Token        string        `json:"token,omitempty"`
	FencingToken int64         `json:"fencing_token,omitempty"`
	StoryID      string        `json:"story_id,omitempty"`
	Repo        string        `json:"repo"`
	Branch      string        `json:"branch,omitempty"`
	Title       string        `json:"title,omitempty"`
	Prompt      string        `json:"prompt"`
	TargetAgent string        `json:"target_agent,omitempty"`
	WorkingDir  string        `json:"working_dir"`
	TraceID     string        `json:"trace_id,omitempty"`
	Timeout     time.Duration `json:"timeout,omitempty"`
	GracePeriod time.Duration `json:"grace_period,omitempty"`
}

func hasHeadlessPermissionDenial(text string) bool {
	lower := strings.ToLower(text)
	return strings.Contains(lower, "jetski: no output produced") ||
		strings.Contains(lower, "headless mode cannot prompt for") ||
		strings.Contains(lower, "so it was auto-denied")
}

// PromptResult represents the structured telemetry returned after prompt execution.
type PromptResult struct {
	PromptID             string   `json:"prompt_id"`
	Repo                 string   `json:"repo"`
	WorktreePath         string   `json:"worktree_path,omitempty"`
	Branch               string   `json:"branch,omitempty"`
	TargetAgent          string   `json:"target_agent"`
	StoryID              string   `json:"story_id,omitempty"`
	StoryTitle           string   `json:"story_title,omitempty"`
	StoryPath            string   `json:"story_path,omitempty"`
	ConversationID       string   `json:"conversation_id,omitempty"`
	ActiveConversationID string   `json:"active_conversation_id,omitempty"`
	ConversationIDs      []string `json:"conversation_ids,omitempty"`
	Status               string   `json:"status"`
	ExitCode             int      `json:"exit_code"`
	PreGitHeadSHA        string   `json:"pre_git_head_sha,omitempty"`
	PreGitTreeSHA        string   `json:"pre_git_tree_sha,omitempty"`
	GitHeadSHA           string   `json:"git_head_sha"`
	GitTreeSHA           string   `json:"git_tree_sha"`
	ModifiedFiles        []string `json:"modified_files"`
	Summary              string   `json:"summary"`
	DurationMs           int64    `json:"duration_ms"`
}

type chunkStreamingWriter struct {
	mu           *sync.Mutex
	buf          *bytes.Buffer
	lastOutAt    *time.Time
	streamedAny  *bool
	convDetected *string
	ctx          context.Context
	apiURL       string
	token        string
	jobID        string
	stream       string
}

func (w *chunkStreamingWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	w.mu.Lock()
	_, _ = w.buf.Write(p)
	*w.lastOutAt = time.Now()
	text := redact.Egress(string(p))
	var newConv string
	if *w.convDetected == "" {
		if m := convIDRegex.FindStringSubmatch(text); len(m) > 1 && sessionstore.IsValidConversationID(m[1]) {
			*w.convDetected = m[1]
			newConv = m[1]
		}
	}
	canPost := w.jobID != "" && w.apiURL != "" && strings.TrimSpace(text) != ""
	if canPost {
		*w.streamedAny = true
	}
	w.mu.Unlock()

	if newConv != "" && w.jobID != "" && w.apiURL != "" {
		_ = controlplane.PostConversation(w.ctx, w.apiURL, w.token, w.jobID, newConv)
	}
	if canPost {
		_ = controlplane.PostChunk(w.ctx, w.apiURL, w.token, w.jobID, w.stream, text)
	}
	return len(p), nil
}

func discoverLogFileConversation(logPath string) string {
	if strings.TrimSpace(logPath) == "" {
		return ""
	}
	b, err := os.ReadFile(logPath)
	if err != nil || len(b) == 0 {
		return ""
	}
	if m := convIDRegex.FindStringSubmatch(string(b)); len(m) > 1 && sessionstore.IsValidConversationID(m[1]) {
		return m[1]
	}
	return ""
}

func discoverStoryFromASEState(workingDir string, after time.Time) (storyID, storyTitle, storyPath string) {
	if strings.TrimSpace(workingDir) == "" {
		return "", "", ""
	}
	checkSessionFile := func(p string) string {
		fi, err := os.Stat(p)
		if err != nil || fi.IsDir() || fi.ModTime().Before(after) {
			return ""
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return ""
		}
		if m := storyIDRegex.FindStringSubmatch(string(b)); len(m) > 1 {
			return m[1]
		}
		return ""
	}

	candidates := []string{
		filepath.Join(workingDir, ".session_story_id"),
		filepath.Join(workingDir, ".autopilot", "state.json"),
		filepath.Join(workingDir, ".autopilot", "ase-state.json"),
	}
	if subMatches, err := filepath.Glob(filepath.Join(workingDir, "*", ".session_story_id")); err == nil {
		candidates = append(candidates, subMatches...)
	}
	for _, p := range candidates {
		if sid := checkSessionFile(p); sid != "" {
			storyID = sid
			break
		}
	}

	storyGlobs := []string{
		filepath.Join(workingDir, "docs", "stories", "*.story.md"),
		filepath.Join(workingDir, "*", "docs", "stories", "*.story.md"),
	}
	for _, pattern := range storyGlobs {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			continue
		}
		for _, mPath := range matches {
			fi, errS := os.Stat(mPath)
			if errS != nil || fi.IsDir() || fi.ModTime().Before(after) {
				continue
			}
			b, errR := os.ReadFile(mPath)
			if errR != nil {
				continue
			}
			content := string(b)
			var fileSID, fileTitle string
			for _, line := range strings.Split(content, "\n") {
				trimmed := strings.TrimSpace(line)
				if strings.HasPrefix(trimmed, "id:") && fileSID == "" {
					if m := storyIDRegex.FindStringSubmatch(trimmed); len(m) > 1 {
						fileSID = m[1]
					}
				} else if strings.HasPrefix(trimmed, "title:") && fileTitle == "" {
					fileTitle = strings.Trim(strings.TrimSpace(strings.TrimPrefix(trimmed, "title:")), `"'`)
				}
			}
			if fileSID == "" {
				if m := storyIDRegex.FindStringSubmatch(filepath.Base(mPath)); len(m) > 1 {
					fileSID = m[1]
				}
			}
			if storyID == "" || fileSID == storyID {
				if storyID == "" {
					storyID = fileSID
				}
				if fileTitle != "" {
					storyTitle = fileTitle
				}
				if rel, errRel := filepath.Rel(workingDir, mPath); errRel == nil {
					storyPath = filepath.ToSlash(rel)
				} else {
					storyPath = filepath.ToSlash(mPath)
				}
				break
			}
		}
	}
	return storyID, storyTitle, storyPath
}

func resolveBrainDir() string {
	if override := strings.TrimSpace(os.Getenv("IAZIO_BRAIN_DIR")); override != "" {
		return override
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, geminiDirName, agyCliDirName, "brain")
}

func discoverNewBrainConversation(after time.Time, preExisting map[string]bool) string {
	brainDir := resolveBrainDir()
	if brainDir != "" {
		if entries, err := os.ReadDir(brainDir); err == nil {
			for _, e := range entries {
				if !e.IsDir() || preExisting[e.Name()] {
					continue
				}
				if !sessionstore.IsValidConversationID(e.Name()) {
					continue
				}
				if info, errI := e.Info(); errI == nil && info.ModTime().After(after) {
					return e.Name()
				}
			}
		}
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	convDir := filepath.Join(home, geminiDirName, agyCliDirName, "conversations")
	if entries, err := os.ReadDir(convDir); err == nil {
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".db") {
				continue
			}
			cid := strings.TrimSuffix(e.Name(), ".db")
			if preExisting[cid] || !sessionstore.IsValidConversationID(cid) {
				continue
			}
			if info, errI := e.Info(); errI == nil && info.ModTime().After(after) {
				return cid
			}
		}
	}
	return ""
}

type transcriptToolCall struct {
	Name      string `json:"name"`
	Arguments any    `json:"arguments"`
	Args      any    `json:"args"`
}

type transcriptStep struct {
	StepIndex int                  `json:"step_index"`
	Type      string               `json:"type"`
	Status    string               `json:"status"`
	CreatedAt string               `json:"created_at"`
	Thinking  string               `json:"thinking"`
	Content   string               `json:"content"`
	ToolCalls []transcriptToolCall `json:"tool_calls"`
}

func summarizeToolCallArg(args any) string {
	var m map[string]any
	switch v := args.(type) {
	case string:
		_ = json.Unmarshal([]byte(v), &m)
	case map[string]any:
		m = v
	}
	if m == nil {
		return ""
	}
	var parts []string
	if ts, ok := m["toolSummary"].(string); ok && strings.TrimSpace(ts) != "" {
		parts = append(parts, strings.TrimSpace(ts))
	}
	if cmdLine, ok := m["CommandLine"].(string); ok && strings.TrimSpace(cmdLine) != "" {
		cl := strings.TrimSpace(strings.ReplaceAll(cmdLine, "\n", " "))
		if len(cl) > 120 {
			cl = cl[:117] + "..."
		}
		parts = append(parts, "`"+cl+"`")
	} else if pathVal, ok := m["Path"].(string); ok && strings.TrimSpace(pathVal) != "" {
		parts = append(parts, strings.TrimSpace(pathVal))
	}
	return strings.Join(parts, " — ")
}

func firstNonEmptyLine(s string, maxLen int) string {
	for _, line := range strings.Split(s, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" {
			if len(trimmed) > maxLen {
				return trimmed[:maxLen-3] + "..."
			}
			return trimmed
		}
	}
	return ""
}

func tailBrainTranscriptSteps(convID string, seenSteps map[int]bool) string {
	if strings.TrimSpace(convID) == "" || !sessionstore.IsValidConversationID(convID) {
		return ""
	}
	brainDir := resolveBrainDir()
	if brainDir == "" {
		return ""
	}
	transcriptPath := filepath.Join(brainDir, convID, ".system_generated", "logs", "transcript.jsonl")
	f, err := os.Open(transcriptPath)
	if err != nil {
		return ""
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var sb strings.Builder
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var step transcriptStep
		if err := json.Unmarshal([]byte(line), &step); err != nil {
			continue
		}
		if step.StepIndex <= 0 || seenSteps[step.StepIndex] {
			continue
		}
		if step.Type != "PLANNER_RESPONSE" {
			seenSteps[step.StepIndex] = true
			continue
		}
		if step.Status != "" && step.Status != "DONE" && len(step.ToolCalls) == 0 && strings.TrimSpace(step.Content) == "" && strings.TrimSpace(step.Thinking) == "" {
			continue
		}
		seenSteps[step.StepIndex] = true
		ts := step.CreatedAt
		if ts == "" {
			ts = time.Now().UTC().Format(time.RFC3339)
		}
		if thinkLine := firstNonEmptyLine(step.Thinking, 180); thinkLine != "" {
			sb.WriteString(fmt.Sprintf("[%s] [agy step #%d] thinking: %s\n", ts, step.StepIndex, redact.Egress(thinkLine)))
		}
		for _, tc := range step.ToolCalls {
			rawArgs := tc.Arguments
			if rawArgs == nil {
				rawArgs = tc.Args
			}
			argSummary := summarizeToolCallArg(rawArgs)
			if argSummary != "" {
				sb.WriteString(fmt.Sprintf("[%s] [agy step #%d] tool %s: %s\n", ts, step.StepIndex, tc.Name, redact.Egress(argSummary)))
			} else {
				sb.WriteString(fmt.Sprintf("[%s] [agy step #%d] tool %s\n", ts, step.StepIndex, tc.Name))
			}
		}
		if contentLine := firstNonEmptyLine(step.Content, 240); contentLine != "" {
			sb.WriteString(fmt.Sprintf("[%s] [agy step #%d] output: %s\n", ts, step.StepIndex, redact.Egress(contentLine)))
		}
	}
	return sb.String()
}

// PromptRunner defines the interface for executing prompts in a submodule directory.
type PromptRunner interface {
	RunPrompt(ctx context.Context, opts RunPromptOptions) (*PromptResult, error)
}

// CLIPromptRunner delegates prompt execution to the autopilot binary on the host.
type CLIPromptRunner struct {
	AutopilotBinary string
	GitBinary       string
	GracePeriod     time.Duration
	PreFlightHook   func(ctx context.Context, workingDir, storyID string) error
}

// NewPromptRunner creates a CLIPromptRunner.
func NewPromptRunner(autopilotBin string) *CLIPromptRunner {
	if autopilotBin == "" {
		if env := os.Getenv("AUTOPILOT_BIN"); env != "" {
			autopilotBin = env
		} else {
			autopilotBin = "autopilot"
		}
	}
	return &CLIPromptRunner{
		AutopilotBinary: autopilotBin,
		GitBinary:       "git",
		GracePeriod:     defaultGracePeriod,
	}
}

// RunPrompt executes the prompt inside opts.WorkingDir using autopilot.
func (r *CLIPromptRunner) RunPrompt(ctx context.Context, opts RunPromptOptions) (*PromptResult, error) {
	if strings.TrimSpace(opts.Prompt) == "" {
		return nil, errors.New("prompt is required")
	}
	if strings.TrimSpace(opts.WorkingDir) == "" {
		return nil, errors.New("working directory is required")
	}

	targetAgent := opts.TargetAgent
	if targetAgent == "" {
		targetAgent = "autopilot"
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = ResolveDefaultPromptTimeout()
	}

	gracePeriod := opts.GracePeriod
	if gracePeriod <= 0 {
		gracePeriod = r.GracePeriod
	}
	if gracePeriod <= 0 {
		gracePeriod = defaultGracePeriod
	}

	gitBin := r.GitBinary
	if gitBin == "" {
		gitBin = "git"
	}

	startTime := time.Now()

	// 1. Create temporary prompt file to prevent shell-escaping issues and OS argv limits
	tmpFile, err := os.CreateTemp("", fmt.Sprintf("iazio-prompt-%s-*.md", opts.PromptID))
	if err != nil {
		return nil, fmt.Errorf("create temp prompt file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	if _, err := tmpFile.WriteString(opts.Prompt); err != nil {
		_ = tmpFile.Close()
		return nil, fmt.Errorf("write prompt to temp file: %w", err)
	}
	_ = tmpFile.Close()

	storyID := strings.TrimSpace(opts.StoryID)
	if storyID == "" {
		if m := storyIDRegex.FindStringSubmatch(opts.Title); len(m) > 1 {
			storyID = m[1]
		}
	}
	if storyID == "" {
		if m := storyIDRegex.FindStringSubmatch(opts.Prompt); len(m) > 1 {
			storyID = m[1]
		}
	}

	pfHook := r.PreFlightHook
	if pfHook == nil {
		pfHook = engines.PreFlightHook
	}
	if storyID != "" && pfHook != nil {
		if pfErr := pfHook(ctx, opts.WorkingDir, storyID); pfErr != nil {
			if errors.Is(pfErr, engines.ErrStoryNotFound) || strings.Contains(strings.ToLower(pfErr.Error()), "story not found") {
				return nil, fmt.Errorf("pre-flight story materialization failed for %s: %w", storyID, pfErr)
			}
			log.Printf("[prompt_runner] warning: PreFlightHook materialization error for story %s: %v", storyID, pfErr)
		}
	}

	// 2. Prepare autopilot or direct headless agent command invocation:
	// If the prompt targets story execution via autopilot, route directly to the standard
	// built-in `autopilot story run --story <ID>`, eliminating exit 99 across older host runtimes.
	cmdBin := r.AutopilotBinary
	var args []string
	effectiveAgent := targetAgent
	if effectiveAgent == "" || effectiveAgent == "auto" || (storyID == "" && effectiveAgent == "autopilot") {
		effectiveAgent = "agy"
	}
	baseEffectiveAgent := strings.ToLower(filepath.Base(effectiveAgent))
	isMockAgy := strings.HasPrefix(baseEffectiveAgent, "mock-agy")
	isDirectAgy := r.AutopilotBinary == "autopilot" && (baseEffectiveAgent == "agy" || baseEffectiveAgent == "agy.exe")

	var tmpLogPath string
	if isMockAgy || isDirectAgy {
		if logFile, errL := os.CreateTemp("", fmt.Sprintf("iazio-agy-log-%s-*.log", opts.PromptID)); errL == nil {
			tmpLogPath = logFile.Name()
			_ = logFile.Close()
			defer os.Remove(tmpLogPath)
		}
		cmdBin = effectiveAgent
		args = []string{"--print", opts.Prompt, "--dangerously-skip-permissions"}
		if tmpLogPath != "" {
			args = append(args, "--log-file", tmpLogPath)
		}
	} else if storyID != "" && targetAgent == "autopilot" && strings.TrimSpace(opts.Prompt) == "" {
		args = []string{"story", "run", "--story", storyID}
	} else {
		args = []string{"prompt", "run", "--file", tmpPath}
		args = append(args, "--target-agent", effectiveAgent)
		if opts.Title != "" {
			args = append(args, "--title", opts.Title)
		}
		if storyID != "" {
			args = append(args, "--story", storyID)
		}
	}

	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.Command(cmdBin, args...)
	cmd.Dir = opts.WorkingDir

	// Propagate Zipkin/W3C trace continuity and target branch into autopilot process
	cmd.Env = os.Environ()
	targetBranch := strings.TrimSpace(opts.Branch)
	if targetBranch != "" {
		cmd.Env = append(cmd.Env, "AUTOPILOT_TARGET_BRANCH="+targetBranch)
	}
	traceID := opts.TraceID
	if traceID == "" {
		traceID = os.Getenv("TRACE_ID")
	}
	if traceID == "" {
		traceID = os.Getenv("ZIPKIN_TRACE_ID")
	}
	if traceID != "" {
		_, traceparent := formatW3CTraceParent(traceID, opts.PromptID)
		cmd.Env = append(cmd.Env,
			"ZIPKIN_TRACE_ID="+traceID,
			"TRACE_ID="+traceID,
			"TRACEPARENT="+traceparent,
		)
	} else if tp := os.Getenv("TRACEPARENT"); tp != "" {
		cmd.Env = append(cmd.Env, "TRACEPARENT="+tp)
	}

	// Dual delivery: also pipe prompt bytes to Stdin for mock or stdin-reading executors
	cmd.Stdin = bytes.NewReader([]byte(opts.Prompt))

	jobID := strings.TrimSpace(opts.JobID)
	if jobID == "" {
		jobID = strings.TrimSpace(os.Getenv("IAZIO_JOB_ID"))
	}
	apiURL := strings.TrimSpace(opts.APIURL)
	if apiURL == "" {
		apiURL = strings.TrimSpace(os.Getenv("IAZIO_HARNESS_API_URL"))
	}
	token := strings.TrimSpace(opts.Token)
	if token == "" {
		token = strings.TrimSpace(os.Getenv("IAZIO_HARNESS_TOKEN"))
	}

	preExistingConvs := map[string]bool{}
	if brainDir := resolveBrainDir(); brainDir != "" {
		if entries, errD := os.ReadDir(brainDir); errD == nil {
			for _, e := range entries {
				preExistingConvs[e.Name()] = true
			}
		}
	}
	if home, errH := os.UserHomeDir(); errH == nil && home != "" {
		if entries, errD := os.ReadDir(filepath.Join(home, geminiDirName, agyCliDirName, "conversations")); errD == nil {
			for _, e := range entries {
				preExistingConvs[strings.TrimSuffix(e.Name(), ".db")] = true
			}
		}
	}

	var streamMu sync.Mutex
	var stdoutBuf, stderrBuf bytes.Buffer
	lastOutAt := startTime
	streamedAny := false
	detectedConvID := ""
	seenTranscriptSteps := make(map[int]bool)

	var stdoutW io.Writer = &stdoutBuf
	var stderrW io.Writer = &stderrBuf
	if jobID != "" && apiURL != "" {
		stdoutW = &chunkStreamingWriter{
			mu: &streamMu, buf: &stdoutBuf, lastOutAt: &lastOutAt, streamedAny: &streamedAny,
			convDetected: &detectedConvID, ctx: ctx, apiURL: apiURL, token: token, jobID: jobID, stream: "stdout",
		}
		stderrW = &chunkStreamingWriter{
			mu: &streamMu, buf: &stderrBuf, lastOutAt: &lastOutAt, streamedAny: &streamedAny,
			convDetected: &detectedConvID, ctx: ctx, apiURL: apiURL, token: token, jobID: jobID, stream: "stderr",
		}
	}
	cmd.Stdout = stdoutW
	cmd.Stderr = stderrW

	// Capture pre-execution Git state
	preHeadSHA, preTreeSHA, _ := captureGitSnapshot(opts.WorkingDir, gitBin)

	// 3. Configure dedicated process group
	engines.SetProcGroup(cmd)

	if err := cmd.Start(); err != nil {
		durationMs := time.Since(startTime).Milliseconds()
		headSHA, treeSHA, modified := captureGitSnapshot(opts.WorkingDir, gitBin)
		return &PromptResult{
			PromptID:      opts.PromptID,
			Repo:          opts.Repo,
			WorktreePath:  opts.WorkingDir,
			TargetAgent:   targetAgent,
			Status:        "failed",
			ExitCode:      1,
			PreGitHeadSHA: preHeadSHA,
			PreGitTreeSHA: preTreeSHA,
			GitHeadSHA:    headSHA,
			GitTreeSHA:    treeSHA,
			ModifiedFiles: modified,
			Summary:       redact.Egress(fmt.Sprintf("Failed to spawn autopilot: %v", err)),
			DurationMs:    durationMs,
		}, fmt.Errorf("spawn autopilot: %w", err)
	}

	engines.PostStartProcessGroup(cmd)

	toolPID := 0
	if cmd.Process != nil {
		toolPID = cmd.Process.Pid
	}
	boundStoryID := storyID
	tickDone := make(chan struct{})
	if jobID != "" && apiURL != "" {
		tsStr := time.Now().UTC().Format(time.RFC3339)
		promptPreview := strings.TrimSpace(strings.ReplaceAll(opts.Prompt, "\n", " "))
		if len(promptPreview) > 160 {
			promptPreview = promptPreview[:157] + "..."
		}
		storyLabel := boundStoryID
		if storyLabel == "" {
			storyLabel = "pending-allocation"
		}
		banner := fmt.Sprintf(
			"[%s] [ASE-SUPERVISOR] Starting prompt execution for Job %s (HarnessPID=%d, ToolPID=%d, Agent=%s, Story=%s, Worktree=%s)\n[%s] [ASE-SUPERVISOR] Prompt: %s\n",
			tsStr, jobID, os.Getpid(), toolPID, effectiveAgent, storyLabel, opts.WorkingDir, tsStr, redact.Egress(promptPreview),
		)
		_ = controlplane.PostChunk(ctx, apiURL, token, jobID, "stdout", banner)
		_ = controlplane.PostTick(ctx, apiURL, token, jobID, 0, os.Getpid(), toolPID, 0)
		go func() {
			ticker := time.NewTicker(2 * time.Second)
			defer ticker.Stop()
			lastHeartbeatChunkAt := startTime
			for {
				select {
				case <-tickDone:
					return
				case <-execCtx.Done():
					return
				case <-ticker.C:
					streamMu.Lock()
					curConv := detectedConvID
					curBoundStory := boundStoryID
					streamMu.Unlock()

					if curConv == "" {
						cid := discoverLogFileConversation(tmpLogPath)
						if cid == "" {
							cid = discoverNewBrainConversation(startTime.Add(-2*time.Second), preExistingConvs)
						}
						if cid != "" {
							streamMu.Lock()
							if detectedConvID == "" {
								detectedConvID = cid
							}
							curConv = detectedConvID
							lastOutAt = time.Now()
							streamMu.Unlock()
							_ = controlplane.PostConversation(ctx, apiURL, token, jobID, cid)
							convMsg := fmt.Sprintf("[%s] [ASE-SUPERVISOR] Attached Antigravity conversation: %s\n", time.Now().UTC().Format(time.RFC3339), cid)
							_ = controlplane.PostChunk(ctx, apiURL, token, jobID, "stdout", convMsg)
							lastHeartbeatChunkAt = time.Now()
						}
					}

					if curBoundStory == "" {
						if sid, stitle, spath := discoverStoryFromASEState(opts.WorkingDir, startTime.Add(-2*time.Second)); sid != "" {
							streamMu.Lock()
							if boundStoryID == "" {
								boundStoryID = sid
							}
							curBoundStory = boundStoryID
							lastOutAt = time.Now()
							streamMu.Unlock()
							if stitle == "" {
								stitle = opts.Title
							}
							_ = controlplane.PostStoryBinding(ctx, apiURL, token, jobID, sid, stitle, spath, opts.FencingToken)
							storyMsg := fmt.Sprintf("[%s] [ASE-SUPERVISOR] Bound governing story: %s (%s)\n", time.Now().UTC().Format(time.RFC3339), sid, stitle)
							_ = controlplane.PostChunk(ctx, apiURL, token, jobID, "stdout", storyMsg)
							lastHeartbeatChunkAt = time.Now()
						}
					}

					if curConv != "" {
						streamMu.Lock()
						stepChunk := tailBrainTranscriptSteps(curConv, seenTranscriptSteps)
						if stepChunk != "" {
							lastOutAt = time.Now()
						}
						streamMu.Unlock()
						if stepChunk != "" {
							_ = controlplane.PostChunk(ctx, apiURL, token, jobID, "stdout", stepChunk)
							lastHeartbeatChunkAt = time.Now()
						}
					}

					streamMu.Lock()
					silentMs := time.Since(lastOutAt).Milliseconds()
					streamMu.Unlock()
					elapsedSec := int(time.Since(startTime).Seconds())

					if time.Since(lastHeartbeatChunkAt) >= 20*time.Second {
						hbStory := curBoundStory
						if hbStory == "" {
							hbStory = "pending"
						}
						hbConv := curConv
						if hbConv == "" {
							hbConv = "pending"
						}
						hbMsg := fmt.Sprintf(
							"[%s] [ASE-SUPERVISOR] Execution active (elapsed: %ds, HarnessPID=%d, ToolPID=%d, Story=%s, Conv=%s)\n",
							time.Now().UTC().Format(time.RFC3339), elapsedSec, os.Getpid(), toolPID, hbStory, hbConv,
						)
						_ = controlplane.PostChunk(ctx, apiURL, token, jobID, "stdout", hbMsg)
						lastHeartbeatChunkAt = time.Now()
						streamMu.Lock()
						lastOutAt = time.Now()
						silentMs = 0
						streamMu.Unlock()
					}

					_ = controlplane.PostTick(ctx, apiURL, token, jobID, silentMs, os.Getpid(), toolPID, elapsedSec)
				}
			}
		}()
	}

	// 4. Wait for execution or cancellation / timeout
	waitDone := make(chan error, 1)
	go func() {
		waitDone <- cmd.Wait()
	}()

	var waitErr error
	var execStatus string = "completed"
	var exitCode int = 0

	select {
	case <-execCtx.Done():
		// Context timed out or canceled
		if errors.Is(execCtx.Err(), context.DeadlineExceeded) {
			execStatus = "timed_out"
		} else {
			execStatus = "canceled"
		}
		exitCode = 1

		// Graceful SIGTERM to process group
		engines.StopProcessGroup(cmd)

		select {
		case <-waitDone:
			// Terminated within grace period
		case <-time.After(gracePeriod):
			// Escalation to SIGKILL
			engines.KillProcessGroup(cmd)
			<-waitDone
		}
		waitErr = execCtx.Err()

	case waitErr = <-waitDone:
		if waitErr != nil {
			execStatus = "failed"
			var exitErr *exec.ExitError
			if errors.As(waitErr, &exitErr) {
				exitCode = exitErr.ExitCode()
			} else {
				exitCode = 1
			}
		}
	}
	close(tickDone)

	durationMs := time.Since(startTime).Milliseconds()

	// 5. Post-Execution Git Snapshot
	headSHA, treeSHA, modified := captureGitSnapshot(opts.WorkingDir, gitBin)

	// 6. Output Summary & Redaction
	streamMu.Lock()
	rawOut := stdoutBuf.String()
	rawErr := stderrBuf.String()
	alreadyStreamed := streamedAny
	curConv := detectedConvID
	streamMu.Unlock()

	combinedOutput := rawOut + "\n" + rawErr
	summary := rawOut
	if strings.TrimSpace(summary) == "" {
		if len(rawErr) > 0 {
			summary = rawErr
		} else if waitErr != nil {
			summary = waitErr.Error()
		}
	}
	summary = redact.Egress(strings.TrimSpace(summary))

	if curConv == "" {
		if cid := discoverLogFileConversation(tmpLogPath); cid != "" {
			curConv = cid
		} else if m := convIDRegex.FindStringSubmatch(combinedOutput); len(m) > 1 && sessionstore.IsValidConversationID(m[1]) {
			curConv = m[1]
		} else if cid := discoverNewBrainConversation(startTime.Add(-2*time.Second), preExistingConvs); cid != "" {
			curConv = cid
		}
		if curConv != "" && jobID != "" && apiURL != "" {
			_ = controlplane.PostConversation(ctx, apiURL, token, jobID, curConv)
		}
	}

	if curConv != "" && jobID != "" && apiURL != "" {
		streamMu.Lock()
		finalSteps := tailBrainTranscriptSteps(curConv, seenTranscriptSteps)
		streamMu.Unlock()
		if finalSteps != "" {
			_ = controlplane.PostChunk(ctx, apiURL, token, jobID, "stdout", finalSteps)
		}
	}

	if hasHeadlessPermissionDenial(combinedOutput) {
		execStatus = "failed"
		if exitCode == 0 {
			exitCode = 1
		}
		waitErr = fmt.Errorf("PERMISSION_DENIED: headless agent tool permission was auto-denied: %s", summary)
	}

	if jobID != "" && apiURL != "" && summary != "" && !alreadyStreamed {
		_ = controlplane.PostChunk(ctx, apiURL, token, jobID, "stdout", summary+"\n")
	}

	detectedStoryID := storyID
	var detectedStoryPath string
	var detectedStoryTitle string

	if aseSID, aseTitle, asePath := discoverStoryFromASEState(opts.WorkingDir, startTime.Add(-2*time.Second)); aseSID != "" {
		if detectedStoryID == "" {
			detectedStoryID = aseSID
		}
		if aseTitle != "" {
			detectedStoryTitle = aseTitle
		}
		if asePath != "" {
			detectedStoryPath = asePath
		}
	}

	for _, f := range modified {
		if (strings.HasPrefix(f, "docs/stories/") || strings.Contains(f, "/docs/stories/")) && strings.HasSuffix(f, ".story.md") {
			if detectedStoryPath == "" {
				detectedStoryPath = f
			}
			if detectedStoryID == "" {
				if m := storyIDRegex.FindStringSubmatch(f); len(m) > 1 {
					detectedStoryID = m[1]
				}
			}
			break
		}
	}

	if detectedStoryID == "" {
		if m := storyIDRegex.FindStringSubmatch(summary); len(m) > 1 {
			detectedStoryID = m[1]
		}
	}
	if detectedStoryTitle == "" && opts.Title != "" {
		detectedStoryTitle = opts.Title
	}
	streamMu.Lock()
	alreadyBoundStory := boundStoryID
	streamMu.Unlock()
	if detectedStoryID != "" && detectedStoryID != alreadyBoundStory && jobID != "" && apiURL != "" {
		_ = controlplane.PostStoryBinding(ctx, apiURL, token, jobID, detectedStoryID, detectedStoryTitle, detectedStoryPath, opts.FencingToken)
	}

	var convIDs []string
	if curConv != "" {
		convIDs = []string{curConv}
	}
	result := &PromptResult{
		PromptID:             opts.PromptID,
		Repo:                 opts.Repo,
		WorktreePath:         opts.WorkingDir,
		Branch:               targetBranch,
		TargetAgent:          targetAgent,
		StoryID:              detectedStoryID,
		StoryTitle:           detectedStoryTitle,
		StoryPath:            detectedStoryPath,
		ConversationID:       curConv,
		ActiveConversationID: curConv,
		ConversationIDs:      convIDs,
		Status:               execStatus,
		ExitCode:             exitCode,
		PreGitHeadSHA:        preHeadSHA,
		PreGitTreeSHA:        preTreeSHA,
		GitHeadSHA:           headSHA,
		GitTreeSHA:           treeSHA,
		ModifiedFiles:        modified,
		Summary:              summary,
		DurationMs:           durationMs,
	}

	if waitErr != nil && execStatus != "completed" {
		return result, fmt.Errorf("autopilot execution %s: %w", execStatus, waitErr)
	}

	return result, nil
}

func captureGitSnapshot(dir, gitBin string) (headSHA, treeSHA string, modified []string) {
	modified = make([]string, 0)
	if dir == "" {
		return "", "", modified
	}

	// 1. git rev-parse HEAD
	cmdHead := exec.Command(gitBin, "rev-parse", "HEAD")
	cmdHead.Dir = dir
	if out, err := cmdHead.Output(); err == nil {
		headSHA = strings.TrimSpace(string(out))
	}

	// 2. git rev-parse HEAD^{tree}
	cmdTree := exec.Command(gitBin, "rev-parse", "HEAD^{tree}")
	cmdTree.Dir = dir
	if out, err := cmdTree.Output(); err == nil {
		treeSHA = strings.TrimSpace(string(out))
	}

	// 3. git status --porcelain
	cmdStatus := exec.Command(gitBin, "status", "--porcelain")
	cmdStatus.Dir = dir
	if out, err := cmdStatus.Output(); err == nil {
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		for _, line := range lines {
			line = strings.TrimRight(line, "\r")
			if len(line) < 3 {
				continue
			}
			pathPart := strings.TrimSpace(line[2:])
			if idx := strings.Index(pathPart, " -> "); idx != -1 {
				pathPart = strings.TrimSpace(pathPart[idx+4:])
			}
			pathPart = strings.Trim(pathPart, `"`)
			pathPart = filepath.ToSlash(pathPart)
			if pathPart != "" {
				modified = append(modified, pathPart)
			}
		}
	}

	return headSHA, treeSHA, modified
}
