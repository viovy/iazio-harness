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

	"github.com/viovy/iazio-core/pkg/procutil"
	"github.com/viovy/iazio-harness/internal/agyhooks"
	asegate "github.com/viovy/iazio-harness/internal/ase/gate"
	"github.com/viovy/iazio-harness/internal/auth"
	"github.com/viovy/iazio-harness/internal/controlplane"
	"github.com/viovy/iazio-harness/internal/engines"
	"github.com/viovy/iazio-harness/internal/redact"
	"github.com/viovy/iazio-harness/internal/sessionstore"
	"github.com/viovy/iazio-harness/internal/watchdog"
)

var (
	storyIDRegex            = regexp.MustCompile(`\b(STORY-[A-Za-z0-9_-]+)\b`)
	canonicalStoryIDRegex   = regexp.MustCompile(`(?i)\b(STORY-(?:GOV-POINTERS-[0-9-]+|[A-Z0-9]+(?:-[A-Z0-9]+)*-[0-9]+))\b`)
	reviewArtifactPathRegex = regexp.MustCompile(`(?i)\S*docs/reviews/\S*`)
	convIDRegex             = regexp.MustCompile(`(?i)(?:created|resumed|resuming|loaded|active|using|streaming)\s+conversation\s+([A-Za-z0-9_-]{12,64})`)
	bgDrainIdleRegex        = regexp.MustCompile(`(?i)root agent idle;\s*waiting up to\s*[0-9hms.]+\s*for\s*(\d+)\s*background task`)
	printTimeoutRegex       = regexp.MustCompile(`(?i)print timeout after\s*[0-9hms.]+\s*with turn in progress`)
	aseCompletionRegex      = regexp.MustCompile(`(?i)(?:autonomous story execution is complete|\[story_completed\]|story[_\s-]+completed\s+successfully|all acceptance criteria[^\n]*(?:met|verified|complete)|"ase_complete"\s*:\s*true)`)
	hex16Regex              = regexp.MustCompile(`^[0-9a-f]{16}$`)
	hex32Regex              = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

func isASEExecutionPrompt(prompt string) bool {
	trimmed := strings.TrimSpace(prompt)
	return strings.HasPrefix(trimmed, "/autonomous-story-execution") ||
		strings.HasPrefix(trimmed, "autopilot ase") ||
		strings.HasPrefix(trimmed, "story.execute") ||
		strings.HasPrefix(trimmed, "story.create_and_execute") ||
		strings.HasPrefix(trimmed, "story_execute") ||
		strings.HasPrefix(trimmed, "story_create_and_execute") ||
		strings.HasPrefix(trimmed, "execute_story")
}

func isPointerStoryID(sid string) bool {
	return strings.HasPrefix(strings.ToUpper(strings.TrimSpace(sid)), "STORY-GOV-POINTERS-")
}

func extractTargetStoryIDFromText(text string) string {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return ""
	}
	cleaned := reviewArtifactPathRegex.ReplaceAllString(trimmed, " ")
	lines := strings.Split(cleaned, "\n")
	if len(lines) > 1 {
		hasTitleHeaderWithoutID := false
		for i, rawLine := range lines {
			line := strings.TrimSpace(rawLine)
			if line == "" {
				continue
			}
			lower := strings.ToLower(line)
			if strings.HasPrefix(lower, "story id:") || strings.HasPrefix(lower, "story_id:") || strings.HasPrefix(lower, "story:") || strings.HasPrefix(lower, "id:") {
				if m := canonicalStoryIDRegex.FindStringSubmatch(line); len(m) > 1 {
					return strings.ToUpper(strings.TrimSpace(m[1]))
				}
			}
			if i == 0 || strings.HasPrefix(lower, "title:") {
				if m := canonicalStoryIDRegex.FindStringSubmatch(line); len(m) > 1 {
					return strings.ToUpper(strings.TrimSpace(m[1]))
				}
				if strings.HasPrefix(lower, "title:") {
					hasTitleHeaderWithoutID = true
				}
			}
			if strings.HasPrefix(lower, "context & problem statement") ||
				strings.HasPrefix(lower, "## context") ||
				strings.HasPrefix(lower, "upstream dependency context") ||
				strings.HasPrefix(lower, "problem statement:") {
				if hasTitleHeaderWithoutID {
					return ""
				}
				break
			}
		}
		if hasTitleHeaderWithoutID {
			return ""
		}
	}
	if m := canonicalStoryIDRegex.FindStringSubmatch(cleaned); len(m) > 1 {
		return strings.ToUpper(strings.TrimSpace(m[1]))
	}
	return ""
}

func isNonMainWorktreeBranch(workingDir, gitBin string) (bool, string) {
	if strings.TrimSpace(workingDir) == "" {
		return false, ""
	}
	if gitBin == "" {
		gitBin = "git"
	}
	cmd := procutil.Command(gitBin, "rev-parse", "--abbrev-ref", "HEAD")
	cmd.Dir = workingDir
	out, err := cmd.Output()
	if err != nil {
		return false, ""
	}
	br := strings.TrimSpace(string(out))
	if br == "" || br == "main" || br == "master" || br == "HEAD" {
		return false, br
	}
	return true, br
}

const (
	// DefaultPromptTimeout is the default execution window for long-running agentic prompts (7 days).
	DefaultPromptTimeout            = 7 * 24 * time.Hour
	defaultPromptTimeout            = DefaultPromptTimeout
	defaultGracePeriod              = 5 * time.Second
	defaultBackgroundDrainGrace     = 60 * time.Second
	defaultMaxAutoResumeAttempts    = 3
	defaultASEMaxAutoResumeAttempts = 16
	geminiDirName                   = ".gemini"
	agyCliDirName                   = "antigravity-cli"
)

func isPromptRunnerTestBinary() bool {
	for _, arg := range os.Args {
		if strings.HasPrefix(arg, "-test.") {
			return true
		}
	}
	if len(os.Args) > 0 {
		base := strings.ToLower(filepath.Base(os.Args[0]))
		if strings.HasSuffix(base, ".test") || strings.HasSuffix(base, ".test.exe") {
			return true
		}
	}
	return false
}

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
	PromptID                   string        `json:"prompt_id"`
	JobID                      string        `json:"job_id,omitempty"`
	APIURL                     string        `json:"api_url,omitempty"`
	Token                      string        `json:"token,omitempty"`
	LeaseToken                 string        `json:"lease_token,omitempty"`
	FencingToken               int64         `json:"fencing_token,omitempty"`
	ResumeConversationID       string        `json:"resume_conversation_id,omitempty"`
	StoryID                    string        `json:"story_id,omitempty"`
	Repo                       string        `json:"repo"`
	Branch                     string        `json:"branch,omitempty"`
	Title                      string        `json:"title,omitempty"`
	Prompt                     string        `json:"prompt"`
	TargetAgent                string        `json:"target_agent,omitempty"`
	Model                      string        `json:"model,omitempty"`
	Effort                     string        `json:"effort,omitempty"`
	WorkingDir                 string        `json:"working_dir"`
	TraceID                    string        `json:"trace_id,omitempty"`
	Timeout                    time.Duration `json:"timeout,omitempty"`
	GracePeriod                time.Duration `json:"grace_period,omitempty"`
	BackgroundDrainGracePeriod time.Duration `json:"background_drain_grace_period,omitempty"`
	MaxAutoResumeAttempts      *int          `json:"max_auto_resume_attempts,omitempty"`
}

func resolvePromptAuthToken(ctx context.Context, explicit string) string {
	if tok := strings.TrimSpace(explicit); tok != "" {
		return tok
	}
	for _, k := range []string{"IAZIO_HARNESS_TOKEN", "IAZIO_AGENT_TOKEN", "IAZIO_AUTH_TOKEN"} {
		if tok := strings.TrimSpace(os.Getenv(k)); tok != "" {
			return tok
		}
	}
	if tok, err := auth.Bearer(ctx); err == nil && strings.TrimSpace(tok) != "" {
		return strings.TrimSpace(tok)
	}
	return ""
}

func hasHeadlessPermissionDenial(text string) bool {
	if strings.TrimSpace(text) == "" {
		return false
	}
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, "[agy step #") {
			continue
		}
		lower := strings.ToLower(line)
		if strings.Contains(lower, "jetski: no output produced") ||
			strings.Contains(lower, "headless mode cannot prompt for") ||
			strings.Contains(lower, "so it was auto-denied") {
			return true
		}
	}
	return false
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
	HarnessPID           int      `json:"harness_pid,omitempty"`
	ToolPID              int      `json:"tool_pid,omitempty"`
	ElapsedSeconds       int      `json:"elapsed_seconds,omitempty"`
	FailureCategory      string   `json:"failure_category,omitempty"`
	FailureStage         string   `json:"failure_stage,omitempty"`
	FailureDirection     string   `json:"failure_direction,omitempty"`
	FailureService       string   `json:"failure_service,omitempty"`
	ResumeAttempts       int      `json:"resume_attempts,omitempty"`
	Status               string   `json:"status"`
	ExitCode             int      `json:"exit_code"`
	PreGitHeadSHA        string   `json:"pre_git_head_sha,omitempty"`
	PreGitTreeSHA        string   `json:"pre_git_tree_sha,omitempty"`
	GitHeadSHA           string          `json:"git_head_sha"`
	GitTreeSHA           string          `json:"git_tree_sha"`
	ModifiedFiles        []string        `json:"modified_files"`
	Summary              string          `json:"summary"`
	Output               string          `json:"output,omitempty"`
	DurationMs           int64           `json:"duration_ms"`
	ASEComplete          bool            `json:"ase_complete"`
	ASEGateStatus        map[string]bool `json:"ase_gate_status,omitempty"`
}

// MaxPromptSummaryChars is the maximum length of PromptResult.Summary (AC3: <= 500 chars).
const MaxPromptSummaryChars = 500

var agyStepOutputPrefixRegex = regexp.MustCompile(`^(?:\[[^\]]+\]\s*)?\[agy step #\d+\]\s*output:\s*(.+)$`)

// extractCraftOrTerminalHeadline extracts a CRAFT closeout headline or terminal status line
// from raw output or transcript lines if present.
func extractCraftOrTerminalHeadline(sources ...string) string {
	var headline string
	for _, src := range sources {
		lines := strings.Split(src, "\n")
		for i, rawLine := range lines {
			trimmed := strings.TrimSpace(rawLine)
			if m := agyStepOutputPrefixRegex.FindStringSubmatch(trimmed); len(m) > 1 {
				trimmed = strings.TrimSpace(m[1])
			}
			if trimmed == "" || strings.Contains(trimmed, "[agy step #") || strings.Contains(trimmed, "[ASE-SUPERVISOR]") {
				continue
			}
			lower := strings.ToLower(trimmed)
			if strings.HasPrefix(lower, "## craft closeout") || strings.HasPrefix(lower, "# craft closeout") {
				// Look ahead for the first substantive non-heading line (e.g. Result / Context bullet)
				detail := ""
				for j := i + 1; j < len(lines) && j <= i+12; j++ {
					cand := strings.TrimSpace(lines[j])
					if m := agyStepOutputPrefixRegex.FindStringSubmatch(cand); len(m) > 1 {
						cand = strings.TrimSpace(m[1])
					}
					if cand == "" || strings.HasPrefix(cand, "#") || strings.Contains(cand, "[agy step #") || strings.Contains(cand, "[ASE-SUPERVISOR]") {
						continue
					}
					detail = cand
					break
				}
				if detail != "" {
					headline = trimmed + " — " + detail
				} else {
					headline = trimmed
				}
			} else if strings.Contains(lower, "autonomous story execution is complete") || strings.Contains(lower, "craft closeout") {
				headline = trimmed
			}
		}
	}
	if len(headline) > 280 {
		headline = strings.TrimSpace(headline[:277]) + "..."
	}
	return headline
}

// extractLastCraftCloseoutBlock extracts only the terminal CRAFT Closeout markdown section
// (when followed by substantive non-empty lines) so intermediate multi-turn "waiting for..."
// progress messages are never stitched into the executive summary.
func extractLastCraftCloseoutBlock(sources ...string) string {
	var best string
	for _, src := range sources {
		if strings.TrimSpace(src) == "" {
			continue
		}
		lines := strings.Split(src, "\n")
		lastIdx := -1
		for i, rawLine := range lines {
			trimmed := strings.TrimSpace(strings.TrimRight(rawLine, "\r"))
			if m := agyStepOutputPrefixRegex.FindStringSubmatch(trimmed); len(m) > 1 {
				trimmed = strings.TrimSpace(m[1])
			}
			lower := strings.ToLower(trimmed)
			if strings.HasPrefix(lower, "## craft closeout") || strings.HasPrefix(lower, "# craft closeout") {
				lastIdx = i
			}
		}
		if lastIdx < 0 {
			continue
		}
		var block []string
		hasBody := false
		for i := lastIdx; i < len(lines) && len(block) < 12; i++ {
			trimmed := strings.TrimRight(lines[i], "\r")
			if m := agyStepOutputPrefixRegex.FindStringSubmatch(strings.TrimSpace(trimmed)); len(m) > 1 {
				trimmed = strings.TrimSpace(m[1])
			}
			t := strings.TrimSpace(trimmed)
			if t == "" || strings.Contains(t, "[agy step #") || strings.Contains(t, "[ASE-SUPERVISOR]") {
				continue
			}
			if i > lastIdx {
				hasBody = true
			}
			block = append(block, t)
		}
		if hasBody && len(block) > 1 {
			best = strings.Join(block, "\n")
		}
	}
	return best
}

// BuildConcisePromptSummary builds a concise executive summary (<=500 chars)
// stripped of [agy step #...] trace lines and [ASE-SUPERVISOR] log lines,
// prioritizing the terminal CRAFT closeout block rather than stitching earlier multi-turn waits.
func BuildConcisePromptSummary(rawOut, rawTranscript, rawErr string, waitErr error) string {
	filterCleanLines := func(s string) string {
		if strings.TrimSpace(s) == "" {
			return ""
		}
		var kept []string
		for _, line := range strings.Split(s, "\n") {
			trimmed := strings.TrimRight(line, "\r")
			if strings.TrimSpace(trimmed) == "" {
				continue
			}
			if strings.Contains(trimmed, "[agy step #") || strings.Contains(trimmed, "[ASE-SUPERVISOR]") {
				continue
			}
			kept = append(kept, trimmed)
		}
		return strings.TrimSpace(strings.Join(kept, "\n"))
	}

	if terminalCraft := extractLastCraftCloseoutBlock(rawOut, rawTranscript); terminalCraft != "" {
		summary := redact.Egress(strings.TrimSpace(terminalCraft))
		if len(summary) > MaxPromptSummaryChars {
			summary = strings.TrimSpace(summary[:MaxPromptSummaryChars-3]) + "..."
		}
		return summary
	}

	var lastStepOutput string
	for _, src := range []string{rawTranscript, rawOut} {
		for _, line := range strings.Split(src, "\n") {
			trimmed := strings.TrimSpace(line)
			if m := agyStepOutputPrefixRegex.FindStringSubmatch(trimmed); len(m) > 1 {
				cand := strings.TrimSpace(m[1])
				if cand != "" && !strings.Contains(cand, "[agy step #") && !strings.Contains(cand, "[ASE-SUPERVISOR]") {
					lastStepOutput = cand
				}
			}
		}
	}
	craftHeadline := extractCraftOrTerminalHeadline(rawTranscript, rawOut)
	if craftHeadline != "" && lastStepOutput == "" {
		lastStepOutput = craftHeadline
	}
	if len(lastStepOutput) > 320 {
		lastStepOutput = strings.TrimSpace(lastStepOutput[:317]) + "..."
	}

	summary := filterCleanLines(rawOut)
	if craftHeadline != "" && !strings.Contains(summary, craftHeadline) {
		if summary == "" {
			summary = craftHeadline
		} else {
			summary = craftHeadline + "\n" + summary
		}
	}
	if lastStepOutput != "" && !strings.Contains(summary, lastStepOutput) {
		if summary == "" {
			summary = lastStepOutput
		} else {
			summary = summary + "\n" + lastStepOutput
		}
	}
	if summary == "" {
		summary = filterCleanLines(rawErr)
	}
	if summary == "" && waitErr != nil {
		summary = filterCleanLines(waitErr.Error())
	}
	summary = redact.Egress(strings.TrimSpace(summary))
	if len(summary) > MaxPromptSummaryChars {
		if lastStepOutput != "" && len(lastStepOutput) < MaxPromptSummaryChars-10 && !strings.Contains(summary[:MaxPromptSummaryChars-3], lastStepOutput) {
			headBudget := MaxPromptSummaryChars - len(lastStepOutput) - 5
			if headBudget > 0 {
				summary = strings.TrimSpace(summary[:headBudget]) + "\n...\n" + lastStepOutput
			} else {
				summary = lastStepOutput
			}
		}
		if len(summary) > MaxPromptSummaryChars {
			summary = strings.TrimSpace(summary[:MaxPromptSummaryChars-3]) + "..."
		}
	}
	return summary
}

type chunkStreamingWriter struct {
	mu             *sync.Mutex
	buf            *bytes.Buffer
	lastOutAt      *time.Time
	streamedAny    *bool
	convDetected   *string
	onConvDetected func(string)
	ctx            context.Context
	apiURL         string
	token          string
	jobID          string
	stream         string
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
	onConv := w.onConvDetected
	w.mu.Unlock()

	if newConv != "" {
		_, _ = fmt.Fprintf(os.Stderr, "CONVERSATION_ID: %s\nACTIVE_CONVERSATION_ID: %s\n", newConv, newConv)
		if onConv != nil {
			onConv(newConv)
		}
		if w.jobID != "" && w.apiURL != "" {
			_ = controlplane.PostConversation(w.ctx, w.apiURL, w.token, w.jobID, newConv)
		}
	}
	if canPost {
		_ = controlplane.PostChunk(w.ctx, w.apiURL, w.token, w.jobID, w.stream, text)
	}
	return len(p), nil
}

func discoverRepoLockConversation(workingDir string, after time.Time) string {
	if strings.TrimSpace(workingDir) == "" {
		return ""
	}
	lockPath := filepath.Join(workingDir, ".iazio", "repo.lock")
	fi, err := os.Stat(lockPath)
	if err != nil || fi.IsDir() || fi.ModTime().Before(after) {
		return ""
	}
	b, err := os.ReadFile(lockPath)
	if err != nil || len(b) == 0 {
		return ""
	}
	var payload struct {
		ConversationID string `json:"conversation_id"`
	}
	if err := json.Unmarshal(b, &payload); err == nil {
		cid := strings.TrimSpace(payload.ConversationID)
		if sessionstore.IsValidConversationID(cid) {
			return cid
		}
	}
	if m := convIDRegex.FindStringSubmatch(string(b)); len(m) > 1 && sessionstore.IsValidConversationID(m[1]) {
		return m[1]
	}
	return ""
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
	sid, stitle, spath, _ := discoverStoryFromASEStateWithSource(workingDir, after, true)
	return sid, stitle, spath
}

func discoverStoryFromASEStateWithSource(workingDir string, after time.Time, allowDoneFallback bool) (storyID, storyTitle, storyPath string, fromSessionFile bool) {
	if strings.TrimSpace(workingDir) == "" {
		return "", "", "", false
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
	var fallbackPointerID string
	for _, p := range candidates {
		if sid := checkSessionFile(p); sid != "" {
			if isPointerStoryID(sid) {
				if fallbackPointerID == "" {
					fallbackPointerID = sid
				}
				continue
			}
			storyID = sid
			fromSessionFile = true
			break
		}
	}

	storyGlobs := []string{
		filepath.Join(workingDir, "docs", "stories", "*.story.md"),
		filepath.Join(workingDir, "*", "docs", "stories", "*.story.md"),
	}
	var fallbackPointerTitle, fallbackPointerPath string
	var bestActiveSID, bestActiveTitle, bestActivePath string
	var bestActiveMod time.Time
	var bestDoneSID, bestDoneTitle, bestDonePath string
	var bestDoneMod time.Time

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
			var fileSID, fileTitle, fileStatus string
			for _, line := range strings.Split(content, "\n") {
				trimmed := strings.TrimSpace(line)
				if strings.HasPrefix(trimmed, "id:") && fileSID == "" {
					if m := storyIDRegex.FindStringSubmatch(trimmed); len(m) > 1 {
						fileSID = m[1]
					}
				} else if strings.HasPrefix(trimmed, "title:") && fileTitle == "" {
					fileTitle = strings.Trim(strings.TrimSpace(strings.TrimPrefix(trimmed, "title:")), `"'`)
				} else if strings.HasPrefix(trimmed, "status:") && fileStatus == "" {
					fileStatus = strings.ToUpper(strings.Trim(strings.TrimSpace(strings.TrimPrefix(trimmed, "status:")), `"'`))
				}
			}
			if fileSID == "" {
				if m := storyIDRegex.FindStringSubmatch(filepath.Base(mPath)); len(m) > 1 {
					fileSID = m[1]
				}
			}
			var relPath string
			if rel, errRel := filepath.Rel(workingDir, mPath); errRel == nil {
				relPath = filepath.ToSlash(rel)
			} else {
				relPath = filepath.ToSlash(mPath)
			}
			if isPointerStoryID(fileSID) {
				if fallbackPointerID == "" || fallbackPointerID == fileSID {
					fallbackPointerID = fileSID
					fallbackPointerTitle = fileTitle
					fallbackPointerPath = relPath
				}
				continue
			}
			if storyID != "" {
				if fileSID == storyID {
					if fileTitle != "" {
						storyTitle = fileTitle
					}
					storyPath = relPath
					break
				}
				continue
			}
			isDone := fileStatus == "DONE" || fileStatus == "COMPLETED"
			if isDone {
				if !allowDoneFallback {
					continue
				}
				if bestDoneSID == "" || fi.ModTime().After(bestDoneMod) || (fi.ModTime().Equal(bestDoneMod) && relPath > bestDonePath) {
					bestDoneSID = fileSID
					bestDoneTitle = fileTitle
					bestDonePath = relPath
					bestDoneMod = fi.ModTime()
				}
			} else {
				if bestActiveSID == "" || fi.ModTime().After(bestActiveMod) || (fi.ModTime().Equal(bestActiveMod) && relPath > bestActivePath) {
					bestActiveSID = fileSID
					bestActiveTitle = fileTitle
					bestActivePath = relPath
					bestActiveMod = fi.ModTime()
				}
			}
		}
	}
	if storyID == "" {
		if bestActiveSID != "" {
			storyID = bestActiveSID
			storyTitle = bestActiveTitle
			storyPath = bestActivePath
			fromSessionFile = true
		} else if bestDoneSID != "" {
			storyID = bestDoneSID
			storyTitle = bestDoneTitle
			storyPath = bestDonePath
		} else if fallbackPointerID != "" {
			storyID = fallbackPointerID
			storyTitle = fallbackPointerTitle
			storyPath = fallbackPointerPath
		}
	}
	return storyID, storyTitle, storyPath, fromSessionFile
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

func summarizeStepOutput(s string, maxLen int) string {
	first := ""
	craftHeading := ""
	craftDetail := ""
	lines := strings.Split(s, "\n")
	for i, raw := range lines {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			continue
		}
		if first == "" {
			first = trimmed
		}
		lower := strings.ToLower(trimmed)
		if strings.HasPrefix(lower, "## craft closeout") || strings.HasPrefix(lower, "# craft closeout") {
			craftHeading = trimmed
			for j := i + 1; j < len(lines); j++ {
				cand := strings.TrimSpace(lines[j])
				if cand != "" && !strings.HasPrefix(cand, "#") {
					craftDetail = cand
					break
				}
			}
		}
	}
	out := first
	if craftHeading != "" {
		craftSummary := craftHeading
		if craftDetail != "" {
			craftSummary += " — " + craftDetail
		}
		if first != "" && first != craftHeading {
			out = first + " | " + craftSummary
		} else {
			out = craftSummary
		}
	}
	if len(out) > maxLen && maxLen > 3 {
		return out[:maxLen-3] + "..."
	}
	return out
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
		if contentLine := summarizeStepOutput(step.Content, 360); contentLine != "" {
			sb.WriteString(fmt.Sprintf("[%s] [agy step #%d] output: %s\n", ts, step.StepIndex, redact.Egress(contentLine)))
		}
	}
	return sb.String()
}

// SupervisorHeartbeatPrefix is the shared prefix for periodic supervisor liveness chunks.
const SupervisorHeartbeatPrefix = "[ASE-SUPERVISOR] Execution active ("

var supervisorHeartbeatInterval = 20 * time.Second

var (
	transcriptTerminalCacheMu sync.Mutex
	transcriptTerminalCache   = make(map[string]struct {
		size     int64
		mtimeNS  int64
		terminal bool
	})
)

func isTranscriptAtTerminalPlannerResponse(convID string) bool {
	if strings.TrimSpace(convID) == "" || !sessionstore.IsValidConversationID(convID) {
		return false
	}
	brainDir := resolveBrainDir()
	if brainDir == "" {
		return false
	}
	transcriptPath := filepath.Join(brainDir, convID, ".system_generated", "logs", "transcript.jsonl")
	fi, err := os.Stat(transcriptPath)
	if err != nil || fi.Size() == 0 {
		return false
	}
	size := fi.Size()
	mtimeNS := fi.ModTime().UnixNano()

	transcriptTerminalCacheMu.Lock()
	if cached, ok := transcriptTerminalCache[transcriptPath]; ok && cached.size == size && cached.mtimeNS == mtimeNS {
		res := cached.terminal
		transcriptTerminalCacheMu.Unlock()
		return res
	}
	transcriptTerminalCacheMu.Unlock()

	f, err := os.Open(transcriptPath)
	if err != nil {
		return false
	}
	defer f.Close()

	const tailWindow = 256 * 1024
	discardFirstLine := false
	if size > tailWindow {
		if _, sErr := f.Seek(size-tailWindow, io.SeekStart); sErr == nil {
			discardFirstLine = true
		}
	}

	reader := bufio.NewReaderSize(f, 64*1024)
	if discardFirstLine {
		_, _ = reader.ReadString('\n')
	}
	var lastStep transcriptStep
	for {
		line, readErr := reader.ReadString('\n')
		trimmed := strings.TrimSpace(line)
		if trimmed != "" {
			var step transcriptStep
			if err := json.Unmarshal([]byte(trimmed), &step); err == nil && step.StepIndex > 0 {
				lastStep = step
			}
		}
		if readErr != nil {
			if readErr != io.EOF {
				return false
			}
			break
		}
	}
	terminal := lastStep.StepIndex > 0 &&
		lastStep.Type == "PLANNER_RESPONSE" &&
		strings.EqualFold(lastStep.Status, "DONE") &&
		len(lastStep.ToolCalls) == 0

	transcriptTerminalCacheMu.Lock()
	transcriptTerminalCache[transcriptPath] = struct {
		size     int64
		mtimeNS  int64
		terminal bool
	}{size: size, mtimeNS: mtimeNS, terminal: terminal}
	transcriptTerminalCacheMu.Unlock()
	return terminal
}

func hasBackgroundDrainLog(logPath, stdoutText, stderrText string) (bool, int) {
	check := func(s string) (bool, int) {
		trimmed := strings.TrimSpace(s)
		if trimmed == "" {
			return false, 0
		}
		lines := strings.Split(trimmed, "\n")
		for i := len(lines) - 1; i >= 0; i-- {
			line := strings.TrimSpace(lines[i])
			if line == "" || strings.Contains(line, "[ASE-SUPERVISOR]") {
				continue
			}
			if m := bgDrainIdleRegex.FindStringSubmatch(line); len(m) > 0 {
				cnt := 1
				if len(m) > 1 {
					if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
						cnt = n
					}
				}
				return true, cnt
			}
			if strings.Contains(line, "WaitForConversationFullyIdle") {
				return true, 1
			}
			// A newer non-drain log line appeared after any earlier drain message.
			return false, 0
		}
		return false, 0
	}
	if ok, cnt := check(stdoutText); ok {
		return true, cnt
	}
	if ok, cnt := check(stderrText); ok {
		return true, cnt
	}
	if strings.TrimSpace(logPath) != "" {
		if b, err := os.ReadFile(logPath); err == nil && len(b) > 0 {
			if ok, cnt := check(string(b)); ok {
				return true, cnt
			}
		}
	}
	return false, 0
}

func hasPrintTimeoutLog(logPath, stdoutText, stderrText string) bool {
	if printTimeoutRegex.MatchString(stdoutText) || printTimeoutRegex.MatchString(stderrText) {
		return true
	}
	if strings.TrimSpace(logPath) != "" {
		if b, err := os.ReadFile(logPath); err == nil && len(b) > 0 {
			if printTimeoutRegex.MatchString(string(b)) {
				return true
			}
		}
	}
	return false
}

func inspectStoryHonestySplit(workingDir, spRoot, storyID string) (wtRep asegate.StoryHonestyReport, externalFiles []asegate.StoryMarkdownMetadata, externalAllHonest bool, externalIncomplete bool) {
	wtRep = asegate.InspectStoryHonesty([]string{workingDir}, storyID)
	spClean := strings.TrimSpace(spRoot)
	if spClean == "" {
		return wtRep, nil, false, false
	}
	wtStoriesDir := filepath.Clean(filepath.Join(workingDir, "docs", "stories"))
	if abs, err := filepath.Abs(wtStoriesDir); err == nil {
		wtStoriesDir = abs
	}
	if eval, err := filepath.EvalSymlinks(wtStoriesDir); err == nil {
		wtStoriesDir = eval
	}
	spRep := asegate.InspectStoryHonesty([]string{spClean}, storyID)
	externalAllHonest = true
	for _, f := range spRep.Files {
		fDir := filepath.Clean(filepath.Dir(f.FilePath))
		if fDir == wtStoriesDir {
			continue
		}
		externalFiles = append(externalFiles, f)
		if !f.HonestDone {
			externalAllHonest = false
			externalIncomplete = true
		}
	}
	if len(externalFiles) == 0 {
		externalAllHonest = false
	}
	return wtRep, externalFiles, externalAllHonest, externalIncomplete
}

func storyFrontmatterStatus(workingDir, storyID string) (found bool, status string) {
	rep := asegate.InspectStoryHonesty([]string{workingDir}, storyID)
	if !rep.Found || len(rep.Files) == 0 {
		return false, ""
	}
	return true, rep.Files[0].Status
}

func isStoryIncompleteInWorktree(workingDir, storyID string) bool {
	rep := asegate.InspectStoryHonesty([]string{workingDir}, storyID)
	return rep.Found && !rep.AllHonestDone
}

func isStoryDoneInWorktree(workingDir, storyID string) bool {
	rep := asegate.InspectStoryHonesty([]string{workingDir}, storyID)
	return rep.Found && rep.AllHonestDone
}

func resolveSuperprojectRoot(dir, gitBin string) string {
	return asegate.ResolveSuperprojectRoot(dir, gitBin)
}

func countUnpushedCommits(dir, gitBin string) (int, error) {
	return asegate.CountUnpushedCommits(dir, gitBin)
}

func isStoryDoneAnywhere(workingDir, superprojectRoot, storyID string) bool {
	wtRep, extFiles, extAllHonest, extIncomplete := inspectStoryHonestySplit(workingDir, superprojectRoot, storyID)
	if extIncomplete {
		return false
	}
	if !wtRep.Found && len(extFiles) == 0 {
		return false
	}
	// Dishonest DONE copy in workingDir always blocks completion even if superproject looks clean.
	for _, f := range wtRep.Files {
		if len(f.DishonestReasons) > 0 {
			return false
		}
	}
	if len(extFiles) > 0 && extAllHonest {
		onNonMain, _ := asegate.IsNonMainBranch(workingDir, "git")
		if !onNonMain {
			return true
		}
	}
	return wtRep.Found && wtRep.AllHonestDone
}

func isStoryIncompleteAnywhere(workingDir, superprojectRoot, storyID string) bool {
	wtRep, extFiles, extAllHonest, extIncomplete := inspectStoryHonestySplit(workingDir, superprojectRoot, storyID)
	if extIncomplete {
		return true
	}
	for _, f := range wtRep.Files {
		if len(f.DishonestReasons) > 0 {
			return true
		}
	}
	// Preserve authoritative superproject DONE when a standalone leaf submodule copy on main has stale IN_PROGRESS.
	if len(extFiles) > 0 && extAllHonest {
		onNonMain, _ := asegate.IsNonMainBranch(workingDir, "git")
		if !onNonMain {
			return false
		}
	}
	return wtRep.Found && !wtRep.AllHonestDone
}

func isEphemeralHarnessFile(relPath string) bool {
	p := strings.TrimSpace(filepath.ToSlash(relPath))
	if p == "" {
		return true
	}
	base := filepath.Base(p)
	if base == ".session_story_id" {
		return true
	}
	if p == ".autopilot" || strings.HasPrefix(p, ".autopilot/") || strings.Contains(p, "/.autopilot/") {
		return true
	}
	return false
}

func filterNonEphemeralModified(modified []string) []string {
	var out []string
	for _, m := range modified {
		if !isEphemeralHarnessFile(m) {
			out = append(out, m)
		}
	}
	return out
}

type aseMultiRepoReport struct {
	Complete              bool
	HasCompletionEvidence bool
	StoryDone             bool
	StoryIncomplete       bool
	GitClean              bool
	BranchClean           bool
	SubmodulesClean       bool
	AnyNonMainBranch      bool
	FirstNonMainBranch    string
	BranchSummary         string
	DirtyFiles            []string
	UnpushedTotal         int
	UnpushedProbeErr      error
	SubmoduleReason       string
	UnmetReasons          []string
	StoryReport           asegate.StoryHonestyReport
	RepoDirs              []string
}

func evaluateMultiRepoASEGates(workingDir, primaryStoryID, combinedOutput, gitBin string) aseMultiRepoReport {
	rep := aseMultiRepoReport{
		GitClean:        true,
		BranchClean:     true,
		SubmodulesClean: true,
	}
	if strings.TrimSpace(workingDir) == "" {
		return rep
	}
	if strings.TrimSpace(gitBin) == "" {
		gitBin = "git"
	}
	spRoot := asegate.ResolveSuperprojectRoot(workingDir, gitBin)

	var candidateStories []string
	seenStory := make(map[string]bool)
	addStory := func(sid string) {
		clean := strings.ToUpper(strings.TrimSpace(sid))
		if clean != "" && !seenStory[clean] {
			seenStory[clean] = true
			candidateStories = append(candidateStories, clean)
		}
	}
	addStory(primaryStoryID)

	var inspectDirs []string
	inspectDirs = append(inspectDirs, workingDir)
	if spRoot != "" {
		inspectDirs = append(inspectDirs, spRoot)
	}
	if strings.TrimSpace(primaryStoryID) != "" {
		rep.StoryReport = asegate.InspectStoryHonesty(inspectDirs, primaryStoryID)
	}
	if !rep.StoryReport.Found {
		for _, m := range storyIDRegex.FindAllStringSubmatch(combinedOutput, -1) {
			if len(m) > 1 {
				addStory(m[1])
			}
		}
	}

	seenSub := make(map[string]bool)
	var scopedSubmodules []string
	for _, s := range rep.StoryReport.Submodules {
		if !seenSub[s] {
			seenSub[s] = true
			scopedSubmodules = append(scopedSubmodules, s)
		}
	}
	for _, sid := range candidateStories {
		if isStoryIncompleteAnywhere(workingDir, spRoot, sid) {
			rep.StoryIncomplete = true
		}
		if sid != strings.ToUpper(strings.TrimSpace(primaryStoryID)) {
			cRep := asegate.InspectStoryHonesty(inspectDirs, sid)
			for _, s := range cRep.Submodules {
				if !seenSub[s] {
					seenSub[s] = true
					scopedSubmodules = append(scopedSubmodules, s)
				}
			}
			for _, r := range cRep.Reasons {
				rep.UnmetReasons = append(rep.UnmetReasons, r)
			}
		}
	}
	rep.UnmetReasons = append(rep.UnmetReasons, rep.StoryReport.Reasons...)

	if !rep.StoryIncomplete {
		if strings.TrimSpace(primaryStoryID) != "" && isStoryDoneAnywhere(workingDir, spRoot, primaryStoryID) {
			rep.StoryDone = true
			rep.HasCompletionEvidence = true
		} else {
			for _, sid := range candidateStories {
				if isStoryDoneAnywhere(workingDir, spRoot, sid) {
					rep.StoryDone = true
					rep.HasCompletionEvidence = true
					break
				}
			}
			if !rep.HasCompletionEvidence && aseCompletionRegex.MatchString(combinedOutput) {
				if len(candidateStories) == 0 || !rep.StoryReport.Found {
					allMissing := true
					for _, sid := range candidateStories {
						if r := asegate.InspectStoryHonesty(inspectDirs, sid); r.Found {
							allMissing = false
							break
						}
					}
					if allMissing {
						rep.HasCompletionEvidence = true
					}
				}
			}
		}
	}

	rep.RepoDirs = asegate.ResolveScopedRepoDirs(workingDir, spRoot, scopedSubmodules)
	if len(rep.RepoDirs) == 0 {
		rep.RepoDirs = []string{workingDir}
	}

	var branchNotes []string
	for _, repoDir := range rep.RepoDirs {
		repoLabel := filepath.Base(repoDir)
		_, _, rawModified := captureGitSnapshot(repoDir, gitBin)
		dirty := filterNonEphemeralModified(rawModified)
		if len(dirty) > 0 {
			rep.GitClean = false
			for _, f := range dirty {
				if filepath.Clean(repoDir) == filepath.Clean(workingDir) {
					rep.DirtyFiles = append(rep.DirtyFiles, f)
				} else {
					rep.DirtyFiles = append(rep.DirtyFiles, repoLabel+"/"+f)
				}
			}
			rep.UnmetReasons = append(rep.UnmetReasons, fmt.Sprintf("dirty_worktree(%s:%d)", repoLabel, len(dirty)))
		}
		if onNonMain, br := asegate.IsNonMainBranch(repoDir, gitBin); onNonMain {
			rep.BranchClean = false
			rep.AnyNonMainBranch = true
			label := fmt.Sprintf("%s@%s", repoLabel, br)
			branchNotes = append(branchNotes, label)
			if rep.FirstNonMainBranch == "" {
				rep.FirstNonMainBranch = label
			}
			rep.UnmetReasons = append(rep.UnmetReasons, fmt.Sprintf("unmerged_branch(%s:%s)", repoLabel, br))
		} else if br != "" {
			branchNotes = append(branchNotes, fmt.Sprintf("%s@%s", repoLabel, br))
		}
		unpushed, uErr := asegate.CountUnpushedCommits(repoDir, gitBin)
		if uErr != nil {
			rep.BranchClean = false
			if rep.UnpushedProbeErr == nil {
				rep.UnpushedProbeErr = fmt.Errorf("%s: %w", repoLabel, uErr)
			}
			rep.UnmetReasons = append(rep.UnmetReasons, fmt.Sprintf("unpushed_probe_failed(%s)", repoLabel))
		} else if unpushed > 0 {
			rep.BranchClean = false
			rep.UnpushedTotal += unpushed
			rep.UnmetReasons = append(rep.UnmetReasons, fmt.Sprintf("unpushed_commits(%s:%d)", repoLabel, unpushed))
		}
		if subOK, subReason := asegate.CheckSubmodulesClean(repoDir, gitBin, scopedSubmodules); !subOK {
			rep.SubmodulesClean = false
			if rep.SubmoduleReason == "" {
				rep.SubmoduleReason = subReason
			}
			rep.UnmetReasons = append(rep.UnmetReasons, subReason)
		}
	}
	rep.BranchSummary = strings.Join(branchNotes, ", ")
	rep.Complete = !rep.StoryIncomplete && rep.HasCompletionEvidence && rep.GitClean && rep.BranchClean && rep.SubmodulesClean
	return rep
}

func isASEFlowCompletedInWorktree(workingDir, primaryStoryID, combinedOutput, gitBin string) bool {
	return evaluateMultiRepoASEGates(workingDir, primaryStoryID, combinedOutput, gitBin).Complete
}

var sdlcChildTokens = []string{
	"autopilot",
	"git-flow",
	"git ",
	"gh ",
	"make",
	"go test",
	"go build",
	"flutter",
	"dart",
	"playwright",
	"npx",
	"npm",
	"pytest",
	"cargo",
	"dotnet",
}

var findChildProcessesFunc = engines.FindChildProcesses

func commandExecutionFields(cmdLower string) []string {
	fields := strings.Fields(strings.TrimSpace(cmdLower))
	for len(fields) > 0 {
		tok := strings.Trim(fields[0], "\"'")
		if strings.Contains(tok, "=") && !strings.HasPrefix(tok, "/") && !strings.HasPrefix(tok, ".") {
			fields = fields[1:]
			continue
		}
		break
	}
	return fields
}

func commandBinaryBasename(execFields []string) string {
	if len(execFields) == 0 {
		return ""
	}
	rawBin := strings.Trim(execFields[0], "\"'")
	base := filepath.Base(strings.ReplaceAll(rawBin, "\\", "/"))
	return strings.TrimSuffix(base, ".exe")
}

func isStrongSDLCCommandLine(cmdLower string) bool {
	execFields := commandExecutionFields(cmdLower)
	if len(execFields) == 0 {
		return false
	}
	bin := commandBinaryBasename(execFields)
	if bin == "" {
		return false
	}
	if strings.HasSuffix(bin, ".test") {
		return true
	}
	switch bin {
	case "make", "gmake", "git", "gh", "git-flow", "pytest", "cargo", "dotnet", "playwright", "flutter":
		return true
	case "autopilot":
		return len(execFields) >= 2
	case "go":
		for _, arg := range execFields[1:] {
			switch arg {
			case "test", "build", "vet", "run", "generate", "install", "mod", "tool":
				return true
			}
		}
	case "dart":
		if len(execFields) >= 2 && execFields[1] != "mcp-server" && execFields[1] != "language-server" {
			return true
		}
	case "npm", "npx", "pnpm", "yarn":
		if len(execFields) >= 2 {
			switch execFields[1] {
			case "test", "build", "lint", "check", "ci":
				return true
			case "run":
				if len(execFields) >= 3 && !strings.Contains(execFields[2], "mcp") {
					return true
				}
			}
		}
	}
	return false
}

func isMCPOrDaemonChildProcess(cmdline string) bool {
	lower := strings.ToLower(strings.TrimSpace(cmdline))
	if lower == "" {
		return true
	}
	if isStrongSDLCCommandLine(lower) {
		return false
	}
	execFields := commandExecutionFields(lower)
	bin := commandBinaryBasename(execFields)
	if bin == "gopls" ||
		bin == "mcp-server" ||
		bin == "mcp_server" ||
		strings.HasSuffix(bin, "-mcp") ||
		strings.HasPrefix(bin, "mcp-") ||
		strings.Contains(bin, "language-server") ||
		strings.HasSuffix(bin, "-lsp") ||
		bin == "lsp-server" {
		return true
	}
	if bin == "dart" && len(execFields) >= 2 && (execFields[1] == "mcp-server" || execFields[1] == "language-server") {
		return true
	}
	if bin == "npm" || bin == "npx" || bin == "pnpm" || bin == "node" || bin == "python" || bin == "python3" || bin == "uv" || bin == "uvx" {
		for _, arg := range execFields[1:] {
			if strings.Contains(arg, "mcp") || strings.Contains(arg, "language-server") {
				return true
			}
		}
	}
	if extra := strings.TrimSpace(os.Getenv("IAZIO_IGNORE_CHILD_PATTERNS")); extra != "" {
		for _, pat := range strings.Split(extra, ",") {
			p := strings.ToLower(strings.TrimSpace(pat))
			if p != "" && strings.Contains(lower, p) {
				return true
			}
		}
	}
	return false
}

func isSDLCChildCommandLine(cmdline string) bool {
	cmdLower := strings.ToLower(strings.TrimSpace(cmdline))
	if cmdLower == "" || strings.HasPrefix(cmdLower, "sleep ") || cmdLower == "sleep" {
		return false
	}
	if isStrongSDLCCommandLine(cmdLower) {
		return true
	}
	if isMCPOrDaemonChildProcess(cmdLower) {
		return false
	}
	execFields := commandExecutionFields(cmdLower)
	bin := commandBinaryBasename(execFields)
	for _, tok := range sdlcChildTokens {
		if tok == "autopilot" {
			if bin == "autopilot" {
				return true
			}
			continue
		}
		if strings.Contains(cmdLower, tok) {
			return true
		}
	}
	return false
}

func hasActiveSDLCChildProcess(toolPID int) (bool, string) {
	if toolPID <= 0 {
		return false, ""
	}
	children := findChildProcessesFunc(toolPID)
	for _, ch := range children {
		if isSDLCChildCommandLine(ch.Command) {
			return true, ch.Command
		}
	}
	return false, ""
}

func stripTranscriptStepLinesForClassification(s string) string {
	if s == "" || !strings.Contains(s, "[agy step #") {
		return s
	}
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[agy step #") {
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// ClassifyPromptFailure classifies upstream, downstream, and local execution failures.
func isStaleHookFailureText(text string) bool {
	matched, _ := agyhooks.DetectStalePluginHookFailure(text)
	return matched
}

func ClassifyPromptFailure(execStatus, failureCategory, failureStage, combinedOutput string, waitErr error) (category, stage, direction, service string) {
	category = strings.TrimSpace(failureCategory)
	stage = strings.TrimSpace(failureStage)
	errText := stripTranscriptStepLinesForClassification(combinedOutput)
	if waitErr != nil {
		errText += " " + waitErr.Error()
	}
	lower := strings.ToLower(execStatus + " " + category + " " + stage + " " + errText)

	switch {
	case category == "PERMISSION_DENIED" || strings.Contains(lower, "permission_denied") || strings.Contains(lower, "auto-denied"):
		return "PERMISSION_DENIED", "TOOL_EXECUTION", "LOCAL_EXECUTION", "iazio-harness/sandbox"
	case category == "STALE_PLUGIN_HOOK_DIR" || strings.Contains(lower, "stale_plugin_hook_dir") || isStaleHookFailureText(errText):
		return "STALE_PLUGIN_HOOK_DIR", "TOOL_EXECUTION", "LOCAL_EXECUTION", "agy-cli/plugin-hooks"
	case category == "BACKGROUND_DRAIN_TIMEOUT" || strings.Contains(lower, "background_drain_timeout"):
		return "BACKGROUND_DRAIN_TIMEOUT", "BACKGROUND_DRAIN", "LOCAL_EXECUTION", "agy-cli"
	case category == "ASE_GATE_INCOMPLETE" || strings.Contains(lower, "ase_gate_incomplete") || strings.Contains(lower, "dirty_worktree_post_exit") || strings.Contains(lower, "incomplete_ase_flow") || strings.Contains(lower, "unmerged_feature_branch") || strings.Contains(lower, "unpushed_commits") || strings.Contains(lower, "missing_story_binding"):
		return "ASE_GATE_INCOMPLETE", "COMPLETION_GATE", "LOCAL_EXECUTION", "autopilot/ase-gate"
	case category == "UPSTREAM_MODEL_UNAVAILABLE" || category == "TOOL_QUOTA_OR_AUTH" || strings.Contains(lower, "429") || strings.Contains(lower, "resourceexhausted") || strings.Contains(lower, "resource_exhausted") || strings.Contains(lower, "quota") || strings.Contains(lower, "insufficient_quota"):
		return "UPSTREAM_MODEL_UNAVAILABLE", "LLM_INFERENCE", "UPSTREAM", "antigravity-llm-provider"
	case category == "UPSTREAM_AUTH_PROVIDER_OUTAGE" || strings.Contains(lower, "hydra") || strings.Contains(lower, "oauth2") || strings.Contains(lower, "bws ") || strings.Contains(lower, "bitwarden"):
		return "UPSTREAM_AUTH_PROVIDER_OUTAGE", "PREFLIGHT", "UPSTREAM", "ory-hydra-bws"
	case category == "UPSTREAM_SCM_UNAVAILABLE" || strings.Contains(lower, "api.github.com") || (strings.Contains(lower, "github.com") && (strings.Contains(lower, "502") || strings.Contains(lower, "503") || strings.Contains(lower, "504") || strings.Contains(lower, "connection refused"))):
		return "UPSTREAM_SCM_UNAVAILABLE", "GIT_DELIVERY", "UPSTREAM", "github-api"
	case category == "UPSTREAM_CI_UNAVAILABLE" || strings.Contains(lower, "jenkins") || strings.Contains(lower, "sonarqube"):
		return "UPSTREAM_CI_UNAVAILABLE", "CI_VERIFICATION", "UPSTREAM", "jenkins-ci"
	case category == "UPSTREAM_LLM_UNAVAILABLE" || (strings.Contains(lower, "llm") || strings.Contains(lower, "gemini") || strings.Contains(lower, "anthropic") || strings.Contains(lower, "openai") || strings.Contains(lower, "model provider")) && (strings.Contains(lower, "502") || strings.Contains(lower, "503") || strings.Contains(lower, "504") || strings.Contains(lower, "unavailable")):
		return "UPSTREAM_MODEL_UNAVAILABLE", "LLM_INFERENCE", "UPSTREAM", "antigravity-llm-provider"
	case category == "DOWNSTREAM_CONTROL_PLANE_UNAVAILABLE" || category == "DOWNSTREAM_CONTROL_PLANE_OUTAGE" || strings.Contains(lower, "iazio-harness-api") || strings.Contains(lower, "controlplane") || strings.Contains(lower, "control_plane_unavailable"):
		return "DOWNSTREAM_CONTROL_PLANE_UNAVAILABLE", "CONTROL_PLANE_SYNC", "DOWNSTREAM", "iazio-harness-api"
	case category == "DOWNSTREAM_DATABASE_UNAVAILABLE" || strings.Contains(lower, "postgres") || strings.Contains(lower, "pgvector") || strings.Contains(lower, "sqlstate"):
		return "DOWNSTREAM_DATABASE_UNAVAILABLE", "QUEUE_DISPATCH", "DOWNSTREAM", "postgresql"
	case category == "DOWNSTREAM_INGEST_OR_HARVESTER_OUTAGE" || strings.Contains(lower, "iazio-api") || strings.Contains(lower, "iazio-harvester") || strings.Contains(lower, "/v1/ingest/batches"):
		return "DOWNSTREAM_INGEST_OR_HARVESTER_OUTAGE", "TELEMETRY_HARVEST", "DOWNSTREAM", "iazio-api/iazio-harvester"
	case category == "DOWNSTREAM_RAG_OR_MCP_UNAVAILABLE" || strings.Contains(lower, "iazio-rag-api") || strings.Contains(lower, "iazio-mcp"):
		return "DOWNSTREAM_RAG_OR_MCP_UNAVAILABLE", "TOOL_EXECUTION", "DOWNSTREAM", "iazio-rag-api/iazio-mcp"
	case category == "STORY_NOT_FOUND" || strings.Contains(lower, "story not found") || strings.Contains(lower, "story_not_found"):
		return "STORY_NOT_FOUND", "STORY_VALIDATION", "LOCAL_EXECUTION", "backlog"
	case category == "WORKSPACE_PREP_ERROR" || strings.Contains(lower, "workspace_prep") || strings.Contains(lower, "stale git lock"):
		return "WORKSPACE_PREP_ERROR", "WORKSPACE_PREP", "LOCAL_EXECUTION", "iazio-agent/workspace"
	default:
		if category == "" {
			if execStatus == "timed_out" {
				category = "EXECUTION_TIMEOUT"
			} else {
				category = "TOOL_CRASH"
			}
		}
		if stage == "" {
			stage = "TOOL_EXECUTION"
		}
		return category, stage, "LOCAL_EXECUTION", "iazio-harness/engine"
	}
}

func hasActiveConversationActivity(convID string, toolPID int, lastTranscriptOrOutputAt time.Time, activeWindow time.Duration) (chatty bool, activeChildren int) {
	if activeWindow <= 0 {
		activeWindow = 30 * time.Second
	}
	if !lastTranscriptOrOutputAt.IsZero() && time.Since(lastTranscriptOrOutputAt) < activeWindow {
		chatty = true
	}
	if !chatty && strings.TrimSpace(convID) != "" && sessionstore.IsValidConversationID(convID) {
		if brainDir := resolveBrainDir(); brainDir != "" {
			tp := filepath.Join(brainDir, convID, ".system_generated", "logs", "transcript.jsonl")
			if fi, err := os.Stat(tp); err == nil && time.Since(fi.ModTime()) < activeWindow {
				chatty = true
			}
		}
	}
	if toolPID > 0 {
		children := findChildProcessesFunc(toolPID)
		for _, ch := range children {
			cmdLower := strings.ToLower(strings.TrimSpace(ch.Command))
			if !isMCPOrDaemonChildProcess(cmdLower) && !strings.HasPrefix(cmdLower, "sleep") {
				activeChildren++
			}
		}
	}
	return chatty, activeChildren
}

// PromptRunner defines the interface for executing prompts in a submodule directory.
type PromptRunner interface {
	RunPrompt(ctx context.Context, opts RunPromptOptions) (*PromptResult, error)
}

// CLIPromptRunner delegates prompt execution to the autopilot binary on the host.
type CLIPromptRunner struct {
	AutopilotBinary            string
	GitBinary                  string
	GracePeriod                time.Duration
	BackgroundDrainGracePeriod time.Duration
	MaxAutoResumeAttempts      *int
	PreFlightHook              func(ctx context.Context, workingDir, storyID string) error
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

func (r *CLIPromptRunner) resolveBackgroundDrainGracePeriod(opts RunPromptOptions) time.Duration {
	if opts.BackgroundDrainGracePeriod > 0 {
		return opts.BackgroundDrainGracePeriod
	}
	if r.BackgroundDrainGracePeriod > 0 {
		return r.BackgroundDrainGracePeriod
	}
	if rawMs := strings.TrimSpace(os.Getenv("IAZIO_BG_DRAIN_GRACE_MS")); rawMs != "" {
		if ms, err := strconv.Atoi(rawMs); err == nil && ms > 0 {
			return time.Duration(ms) * time.Millisecond
		}
	}
	if rawSec := strings.TrimSpace(os.Getenv("IAZIO_BG_DRAIN_GRACE_SECONDS")); rawSec != "" {
		if sec, err := strconv.Atoi(rawSec); err == nil && sec > 0 {
			return time.Duration(sec) * time.Second
		}
	}
	if os.Getenv("MOCK_AGY_FAST") == "1" {
		return 150 * time.Millisecond
	}
	return defaultBackgroundDrainGrace
}

func (r *CLIPromptRunner) resolveMaxAutoResumeAttempts(opts RunPromptOptions) int {
	if opts.MaxAutoResumeAttempts != nil {
		return *opts.MaxAutoResumeAttempts
	}
	if r.MaxAutoResumeAttempts != nil {
		return *r.MaxAutoResumeAttempts
	}
	if raw := strings.TrimSpace(os.Getenv("IAZIO_MAX_AUTO_RESUME_ATTEMPTS")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n >= 0 {
			return n
		}
	}
	if strings.Contains(opts.Prompt, "/autonomous-story-execution") || strings.TrimSpace(opts.StoryID) != "" {
		return defaultASEMaxAutoResumeAttempts
	}
	return defaultMaxAutoResumeAttempts
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

	bgDrainGrace := r.resolveBackgroundDrainGracePeriod(opts)
	maxAutoResumes := r.resolveMaxAutoResumeAttempts(opts)

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
		storyID = extractTargetStoryIDFromText(opts.Title)
	}
	if storyID == "" {
		storyID = extractTargetStoryIDFromText(opts.Prompt)
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
	cmdBin := r.AutopilotBinary
	effectiveAgent := targetAgent
	if effectiveAgent == "" || effectiveAgent == "auto" || (storyID == "" && effectiveAgent == "autopilot") {
		effectiveAgent = "agy"
	}
	baseEffectiveAgent := strings.ToLower(filepath.Base(effectiveAgent))
	isMockAgy := strings.HasPrefix(baseEffectiveAgent, "mock-agy")
	isDirectAgy := r.AutopilotBinary == "autopilot" && (baseEffectiveAgent == "agy" || baseEffectiveAgent == "agy.exe")

	parentHarnessPID := strings.TrimSpace(os.Getenv("IAZIO_PARENT_HARNESS_PID"))
	isNestedHarnessChild := parentHarnessPID != "" && parentHarnessPID != strconv.Itoa(os.Getpid())
	allowAmbientEnv := (!isPromptRunnerTestBinary() || os.Getenv("IAZIO_TEST_ALLOW_AMBIENT_ENV") == "1") && !isNestedHarnessChild
	jobID := strings.TrimSpace(opts.JobID)
	if jobID == "" && allowAmbientEnv {
		jobID = strings.TrimSpace(os.Getenv("IAZIO_JOB_ID"))
	}
	apiURL := strings.TrimSpace(opts.APIURL)
	if apiURL == "" && allowAmbientEnv {
		apiURL = strings.TrimSpace(os.Getenv("IAZIO_HARNESS_API_URL"))
	}
	resolveToken := func() string {
		return resolvePromptAuthToken(ctx, opts.Token)
	}
	token := resolveToken()
	fencingToken := opts.FencingToken
	if fencingToken == 0 && allowAmbientEnv {
		if rawFT := strings.TrimSpace(os.Getenv("IAZIO_FENCING_TOKEN")); rawFT != "" {
			if parsedFT, errFT := strconv.ParseInt(rawFT, 10, 64); errFT == nil {
				fencingToken = parsedFT
			}
		}
	}
	leaseToken := strings.TrimSpace(opts.LeaseToken)
	if leaseToken == "" && allowAmbientEnv {
		leaseToken = strings.TrimSpace(os.Getenv("IAZIO_LEASE_TOKEN"))
	}
	initialResumeConv := strings.TrimSpace(opts.ResumeConversationID)
	if initialResumeConv == "" && allowAmbientEnv {
		initialResumeConv = strings.TrimSpace(os.Getenv("IAZIO_RESUME_CONVERSATION_ID"))
	}
	targetBranch := strings.TrimSpace(opts.Branch)
	traceID := opts.TraceID
	if traceID == "" {
		traceID = os.Getenv("TRACE_ID")
	}
	if traceID == "" {
		traceID = os.Getenv("ZIPKIN_TRACE_ID")
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
	var stdoutBuf, stderrBuf, transcriptBuf bytes.Buffer
	lastOutAt := startTime
	streamedAny := false
	detectedConvID := initialResumeConv
	if detectedConvID == "" && allowAmbientEnv {
		if envConv := strings.TrimSpace(os.Getenv("IAZIO_CONVERSATION_ID")); sessionstore.IsValidConversationID(envConv) {
			detectedConvID = envConv
		}
	}
	boundStoryID := storyID
	var storyTitle, storyPath string
	postedStoryID := ""
	seenTranscriptSteps := make(map[int]bool)
	superprojectRoot := resolveSuperprojectRoot(opts.WorkingDir, gitBin)

	if detectedConvID != "" {
		_, _ = fmt.Fprintf(os.Stderr, "CONVERSATION_ID: %s\nACTIVE_CONVERSATION_ID: %s\n", detectedConvID, detectedConvID)
	}
	if boundStoryID != "" {
		_, _ = fmt.Fprintf(os.Stderr, "ALLOCATED_STORY_ID: %s\n", boundStoryID)
	}

	var stdoutW io.Writer
	var stderrW io.Writer

	emitSupervisorNote := func(msg string) {
		streamMu.Lock()
		_, _ = stdoutBuf.WriteString(msg)
		lastOutAt = time.Now()
		if jobID != "" && apiURL != "" {
			streamedAny = true
		}
		streamMu.Unlock()
		if jobID != "" && apiURL != "" {
			_ = controlplane.PostChunk(ctx, apiURL, resolveToken(), jobID, "stdout", msg)
		}
	}

	initialParentPID := os.Getppid()
	var localWatchdog *watchdog.Watchdog
	wdFile := strings.TrimSpace(os.Getenv("IAZIO_HARNESS_WATCHDOG_FILE"))
	if jobID != "" && (!isPromptRunnerTestBinary() || wdFile != "") {
		statusFile := ""
		if wdFile != "" {
			statusFile = wdFile + ".status"
		}
		localWatchdog = watchdog.New(watchdog.Options{
			StateFile:  wdFile,
			StatusFile: statusFile,
		})
	}

	syncLocalWatchdogAndLiveness := func(toolPID int, activeConv, activeStory, status string, exitCode int, errMsg string) {
		if jobID == "" {
			return
		}
		if localWatchdog != nil {
			switch status {
			case watchdog.StatusCompleted:
				localWatchdog.Complete(jobID, exitCode)
			case watchdog.StatusFailed:
				localWatchdog.Fail(jobID, exitCode, errMsg)
			default:
				localWatchdog.Track(watchdog.TrackedJob{
					JobID:          jobID,
					WorktreePath:   opts.WorkingDir,
					APIURL:         apiURL,
					Engine:         effectiveAgent,
					Prompt:         opts.Prompt,
					StoryID:        activeStory,
					ConversationID: activeConv,
					PID:            os.Getpid(),
					HarnessPID:     os.Getpid(),
					ToolPID:        toolPID,
					StartedAt:      startTime.UTC(),
					Status:         watchdog.StatusRunning,
				})
			}
		}

		liveDir := strings.TrimSpace(os.Getenv("IAZIO_LIVENESS_DIR"))
		if liveDir == "" && !isPromptRunnerTestBinary() {
			if home, errH := os.UserHomeDir(); errH == nil && home != "" {
				liveDir = filepath.Join(home, ".iazio", "liveness")
			}
		}
		if liveDir == "" {
			return
		}
		streamMu.Lock()
		lastOut := lastOutAt
		streamMu.Unlock()
		nowUTC := time.Now().UTC()
		lastEvent := lastOut.UTC()
		if status != watchdog.StatusRunning || time.Since(lastOut) < 30*time.Second || (toolPID > 0 && len(engines.FindChildProcesses(toolPID)) > 0) {
			lastEvent = nowUTC
		}
		if errM := os.MkdirAll(liveDir, 0o755); errM != nil {
			return
		}
		rec := map[string]any{
			"job_id":          jobID,
			"conversation_id": strings.TrimSpace(activeConv),
			"story_id":        strings.TrimSpace(activeStory),
			"harness_pid":     os.Getpid(),
			"tool_pid":        toolPID,
			"status":          status,
			"exit_code":       exitCode,
			"last_event_time": lastEvent,
			"updated_at":      nowUTC,
		}
		if strings.TrimSpace(errMsg) != "" {
			rec["last_error"] = strings.TrimSpace(errMsg)
		}
		if b, errJ := json.Marshal(rec); errJ == nil {
			_ = os.WriteFile(filepath.Join(liveDir, jobID+".json"), b, 0o644)
			cleanConv := strings.TrimPrefix(strings.TrimSpace(activeConv), "agy-")
			if cleanConv != "" {
				_ = os.WriteFile(filepath.Join(liveDir, cleanConv+".json"), b, 0o644)
			}
		}
	}

	emitJobHeartbeat := func(toolPID int, activeConv, activeStory string) {
		syncLocalWatchdogAndLiveness(toolPID, activeConv, activeStory, watchdog.StatusRunning, 0, "")
		if jobID == "" || apiURL == "" {
			return
		}
		_ = controlplane.PostJobHeartbeat(ctx, apiURL, resolveToken(), jobID, controlplane.JobHeartbeatOptions{
			FencingToken:         fencingToken,
			LeaseToken:           leaseToken,
			StoryID:              activeStory,
			WorktreePath:         opts.WorkingDir,
			Repo:                 opts.Repo,
			AgentPID:             os.Getpid(),
			ToolPID:              toolPID,
			ActiveConversationID: activeConv,
			ElapsedSeconds:       int64(time.Since(startTime).Seconds()),
		})
	}

	// Capture pre-execution Git state
	preHeadSHA, preTreeSHA, _ := captureGitSnapshot(opts.WorkingDir, gitBin)

	var lastToolPID int
	var resumeAttempts int
	var failureCategory string
	var failureStage string

	onStreamConvDetected := func(newConv string) {
		streamMu.Lock()
		curStory := boundStoryID
		pid := lastToolPID
		streamMu.Unlock()
		emitJobHeartbeat(pid, newConv, curStory)
	}
	stdoutW = &chunkStreamingWriter{
		mu: &streamMu, buf: &stdoutBuf, lastOutAt: &lastOutAt, streamedAny: &streamedAny,
		convDetected: &detectedConvID, onConvDetected: onStreamConvDetected,
		ctx: ctx, apiURL: apiURL, token: token, jobID: jobID, stream: "stdout",
	}
	stderrW = &chunkStreamingWriter{
		mu: &streamMu, buf: &stderrBuf, lastOutAt: &lastOutAt, streamedAny: &streamedAny,
		convDetected: &detectedConvID, onConvDetected: onStreamConvDetected,
		ctx: ctx, apiURL: apiURL, token: token, jobID: jobID, stream: "stderr",
	}

	var lastTurnStaleHook bool
	var lastTurnStalePlugin string

	runSingleTurn := func(turnPrompt string, resumeConvID string, isResumeTurn bool) (turnStatus string, turnExit int, turnErr error, turnBgDrainReaped bool, turnPrintTimeout bool) {
		streamMu.Lock()
		turnStartOutLen := stdoutBuf.Len()
		turnStartErrLen := stderrBuf.Len()
		streamMu.Unlock()

		var turnStartTranscriptOffset int64
		if resumeConvID != "" && sessionstore.IsValidConversationID(resumeConvID) {
			if brainDir := resolveBrainDir(); brainDir != "" {
				tp := filepath.Join(brainDir, resumeConvID, ".system_generated", "logs", "transcript.jsonl")
				if fi, errStat := os.Stat(tp); errStat == nil {
					turnStartTranscriptOffset = fi.Size()
				}
			}
		}
		lastTurnStaleHook = false
		lastTurnStalePlugin = ""

		var tmpLogPath string
		var args []string
		var resolvedModel, resolvedEffort string
		if isMockAgy || isDirectAgy || strings.Contains(strings.ToLower(filepath.Base(cmdBin)), "autopilot") || targetAgent == "" || targetAgent == "auto" || strings.EqualFold(targetAgent, "autopilot") {
			resolvedModel, resolvedEffort = ResolvePromptExecutionModel(opts, resumeAttempts)
		}
		if isMockAgy || isDirectAgy {
			_, _ = agyhooks.SanitizeEnv(os.Environ())
			if logFile, errL := os.CreateTemp("", fmt.Sprintf("iazio-agy-log-%s-*.log", opts.PromptID)); errL == nil {
				tmpLogPath = logFile.Name()
				_ = logFile.Close()
				defer os.Remove(tmpLogPath)
			}
			cmdBin = effectiveAgent
			args = []string{"--print", turnPrompt, "--dangerously-skip-permissions"}
			if (isDirectAgy || (isMockAgy && strings.TrimSpace(opts.Model) != "")) && resolvedModel != "" {
				args = append(args, "--model", resolvedModel)
				if resolvedEffort != "" {
					args = append(args, "--effort", resolvedEffort)
				}
			}
			if resumeConvID != "" {
				args = append(args, "--conversation="+resumeConvID)
			}
			if tmpLogPath != "" {
				args = append(args, "--log-file", tmpLogPath)
			}
			if superprojectRoot != "" && superprojectRoot != opts.WorkingDir {
				args = append(args, "--add-dir", superprojectRoot)
			}
		} else if storyID != "" && targetAgent == "autopilot" && strings.TrimSpace(turnPrompt) == "" {
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

		cmd := procutil.Command(cmdBin, args...)
		cmd.Dir = opts.WorkingDir
		cmd.Env = append(os.Environ(), fmt.Sprintf("IAZIO_PARENT_HARNESS_PID=%d", os.Getpid()))
		if resolvedModel != "" {
			cmd.Env = append(cmd.Env, "AGY_MODEL="+resolvedModel)
			if resolvedEffort != "" {
				cmd.Env = append(cmd.Env, "AGY_EFFORT="+resolvedEffort)
			}
		}
		if targetBranch != "" {
			cmd.Env = append(cmd.Env, "AUTOPILOT_TARGET_BRANCH="+targetBranch)
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
		cmd.Stdin = bytes.NewReader([]byte(turnPrompt))
		cmd.Stdout = stdoutW
		cmd.Stderr = stderrW

		engines.SetProcGroup(cmd)
		if err := cmd.Start(); err != nil {
			return "failed", 1, fmt.Errorf("spawn autopilot: %w", err), false, false
		}
		engines.PostStartProcessGroup(cmd)

		toolPID := 0
		if cmd.Process != nil {
			toolPID = cmd.Process.Pid
			streamMu.Lock()
			lastToolPID = toolPID
			streamMu.Unlock()
		}

		if !isResumeTurn && jobID != "" && apiURL != "" {
			tsStr := time.Now().UTC().Format(time.RFC3339)
			promptPreview := strings.TrimSpace(strings.ReplaceAll(turnPrompt, "\n", " "))
			if len(promptPreview) > 160 {
				promptPreview = promptPreview[:157] + "..."
			}
			streamMu.Lock()
			storyLabel := boundStoryID
			curBound := boundStoryID
			activeConv := detectedConvID
			streamMu.Unlock()
			if storyLabel == "" {
				storyLabel = "pending-allocation"
			}
			banner := fmt.Sprintf(
				"[%s] [ASE-SUPERVISOR] Starting prompt execution for Job %s (HarnessPID=%d, ToolPID=%d, Agent=%s, Story=%s, Worktree=%s)\n[%s] [ASE-SUPERVISOR] Prompt: %s\n",
				tsStr, jobID, os.Getpid(), toolPID, effectiveAgent, storyLabel, opts.WorkingDir, tsStr, redact.Egress(promptPreview),
			)
			tok := resolveToken()
			if activeConv != "" {
				_ = controlplane.PostConversation(ctx, apiURL, tok, jobID, activeConv)
			}
			_ = controlplane.PostChunk(ctx, apiURL, tok, jobID, "stdout", banner)
			_ = controlplane.PostTick(ctx, apiURL, tok, jobID, 0, os.Getpid(), toolPID, int(time.Since(startTime).Seconds()))
			emitJobHeartbeat(toolPID, activeConv, curBound)
		} else if isResumeTurn && jobID != "" && apiURL != "" {
			streamMu.Lock()
			curBound := boundStoryID
			streamMu.Unlock()
			tok := resolveToken()
			if resumeConvID != "" {
				_ = controlplane.PostConversation(ctx, apiURL, tok, jobID, resumeConvID)
			}
			_ = controlplane.PostTick(ctx, apiURL, tok, jobID, 0, os.Getpid(), toolPID, int(time.Since(startTime).Seconds()))
			emitJobHeartbeat(toolPID, resumeConvID, curBound)
		}

		waitDone := make(chan error, 1)
		go func() {
			waitDone <- cmd.Wait()
		}()

		pollInterval := 2 * time.Second
		if os.Getenv("MOCK_AGY_FAST") == "1" || timeout < 5*time.Second || bgDrainGrace < 2*time.Second {
			pollInterval = 50 * time.Millisecond
		}
		ticker := time.NewTicker(pollInterval)
		defer ticker.Stop()

		turnTimeout := timeout
		if isResumeTurn && turnTimeout < 5*time.Second {
			turnTimeout = 5 * time.Second
		}
		deadline := time.Now().Add(turnTimeout)
		lastHeartbeatChunkAt := time.Now()
		lastJobHeartbeatAt := time.Now()
		var bgDrainSince time.Time
		var timedOut bool
		var canceled bool

		syncTurnTelemetry := func() (curConv, curBoundStory string) {
			streamMu.Lock()
			curConv = detectedConvID
			curBoundStory = boundStoryID
			streamMu.Unlock()

			if curConv == "" {
				cid := discoverLogFileConversation(tmpLogPath)
				if cid == "" {
					cid = discoverRepoLockConversation(opts.WorkingDir, startTime.Add(-2*time.Second))
				}
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
					_, _ = fmt.Fprintf(os.Stderr, "CONVERSATION_ID: %s\nACTIVE_CONVERSATION_ID: %s\n", cid, cid)
					emitJobHeartbeat(toolPID, cid, curBoundStory)
					lastJobHeartbeatAt = time.Now()
					if jobID != "" && apiURL != "" {
						tok := resolveToken()
						_ = controlplane.PostConversation(ctx, apiURL, tok, jobID, cid)
						convMsg := fmt.Sprintf("[%s] [ASE-SUPERVISOR] Attached Antigravity conversation: %s\n", time.Now().UTC().Format(time.RFC3339), cid)
						_ = controlplane.PostChunk(ctx, apiURL, tok, jobID, "stdout", convMsg)
						lastHeartbeatChunkAt = time.Now()
					}
				}
			}

			streamMu.Lock()
			alreadyPostedStory := postedStoryID
			streamMu.Unlock()
			if curBoundStory == "" || alreadyPostedStory == "" || isPointerStoryID(curBoundStory) || strings.TrimSpace(opts.StoryID) == "" {
				sid, stitle, spath, fromSession := discoverStoryFromASEStateWithSource(opts.WorkingDir, startTime.Add(-2*time.Second), false)
				if sid == "" || (curBoundStory != "" && !isPointerStoryID(curBoundStory) && isPointerStoryID(sid)) {
					sid = curBoundStory
				}
				if sid != "" {
					streamMu.Lock()
					canOverride := boundStoryID == "" ||
						(isPointerStoryID(boundStoryID) && !isPointerStoryID(sid)) ||
						(strings.TrimSpace(opts.StoryID) == "" && fromSession && !isPointerStoryID(sid) && sid != boundStoryID)
					if canOverride {
						boundStoryID = sid
						if stitle != "" {
							storyTitle = stitle
						}
						if spath != "" {
							storyPath = spath
						}
					}
					curBoundStory = boundStoryID
					shouldPost := postedStoryID == "" ||
						(isPointerStoryID(postedStoryID) && !isPointerStoryID(curBoundStory)) ||
						(strings.TrimSpace(opts.StoryID) == "" && fromSession && !isPointerStoryID(curBoundStory) && curBoundStory != postedStoryID)
					if shouldPost {
						postedStoryID = curBoundStory
					}
					lastOutAt = time.Now()
					streamMu.Unlock()
					if stitle == "" {
						stitle = opts.Title
					}
					if shouldPost {
						_, _ = fmt.Fprintf(os.Stderr, "ALLOCATED_STORY_ID: %s\n", curBoundStory)
						emitJobHeartbeat(toolPID, curConv, curBoundStory)
						lastJobHeartbeatAt = time.Now()
						if jobID != "" && apiURL != "" {
							tok := resolveToken()
							_ = controlplane.PostStoryBinding(ctx, apiURL, tok, jobID, curBoundStory, stitle, spath, fencingToken)
							storyMsg := fmt.Sprintf("[%s] [ASE-SUPERVISOR] Bound governing story: %s (%s)\n", time.Now().UTC().Format(time.RFC3339), curBoundStory, stitle)
							_ = controlplane.PostChunk(ctx, apiURL, tok, jobID, "stdout", storyMsg)
							lastHeartbeatChunkAt = time.Now()
						}
					}
				}
			}

			if curConv != "" {
				streamMu.Lock()
				stepChunk := tailBrainTranscriptSteps(curConv, seenTranscriptSteps)
				if stepChunk != "" {
					_, _ = transcriptBuf.WriteString(stepChunk)
					lastOutAt = time.Now()
					bgDrainSince = time.Time{}
				}
				streamMu.Unlock()
				if stepChunk != "" && jobID != "" && apiURL != "" {
					_ = controlplane.PostChunk(ctx, apiURL, resolveToken(), jobID, "stdout", stepChunk)
					lastHeartbeatChunkAt = time.Now()
				}
			}
			return curConv, curBoundStory
		}

		checkTurnStaleHook := func(convID, outSlice, errSlice string) (bool, string) {
			if !(isMockAgy || isDirectAgy) {
				return false, ""
			}
			var logChunk string
			if tmpLogPath != "" {
				if lb, lErr := os.ReadFile(tmpLogPath); lErr == nil {
					logChunk = string(lb)
				}
			}
			if matched, plugin := agyhooks.DetectStalePluginHookFailure(outSlice, errSlice, logChunk); matched {
				return true, plugin
			}
			if convID != "" && sessionstore.IsValidConversationID(convID) {
				if brainDir := resolveBrainDir(); brainDir != "" {
					tp := filepath.Join(brainDir, convID, ".system_generated", "logs", "transcript.jsonl")
					if matched, plugin := agyhooks.ScanTranscriptSliceForStaleHook(tp, turnStartTranscriptOffset); matched {
						return true, plugin
					}
				}
			}
			return false, ""
		}

		// Multi-day (>24h) ASE resilience: allow up to 336 active turn timeout deferrals (7+ days)
		// and up to 720 background drain deferrals (12 hours) while active SDLC children are running under toolPID.
		const maxTimeoutDeferrals = 336
		const maxBgDrainDeferrals = 720
		timeoutDeferrals := 0
		bgDrainDeferrals := 0
		var turnCompletedDuringDrain bool
		var lastWorktreeCheckAt time.Time

	turnLoop:
		for {
			select {
			case <-ctx.Done():
				canceled = true
				engines.StopProcessGroup(cmd)
				select {
				case <-waitDone:
				case <-time.After(gracePeriod):
					engines.KillProcessGroup(cmd)
					<-waitDone
				}
				turnErr = ctx.Err()
				break turnLoop

			case wErr := <-waitDone:
				turnErr = wErr
				break turnLoop

			case <-ticker.C:
				curConv, curBoundStory := syncTurnTelemetry()
				toolAlive := toolPID > 0 && watchdog.IsProcessAlive(toolPID)

				streamMu.Lock()
				fullOut := stdoutBuf.String()
				fullErr := stderrBuf.String()
				fullTranscript := transcriptBuf.String()
				outSnapshot := fullOut
				if turnStartOutLen <= len(fullOut) {
					outSnapshot = fullOut[turnStartOutLen:]
				}
				errSnapshot := fullErr
				if turnStartErrLen <= len(fullErr) {
					errSnapshot = fullErr[turnStartErrLen:]
				}
				lastActivity := lastOutAt
				streamMu.Unlock()

				inBgDrain, bgTasks := hasBackgroundDrainLog(tmpLogPath, outSnapshot, errSnapshot)
				terminalPlannerTurn := isTranscriptAtTerminalPlannerResponse(curConv)
				if inBgDrain || (terminalPlannerTurn && time.Since(lastWorktreeCheckAt) >= 10*time.Second) {
					lastWorktreeCheckAt = time.Now()
					turnStaleNow, turnStalePluginNow := checkTurnStaleHook(curConv, outSnapshot, errSnapshot)
					if turnStaleNow {
						lastTurnStaleHook = true
						lastTurnStalePlugin = turnStalePluginNow
					}
					// If ASE/story execution is already completed in the worktree/transcript and no SDLC child is running, settle immediately as completed!
					sdlcActiveNow, _ := hasActiveSDLCChildProcess(toolPID)
					if !sdlcActiveNow && !turnStaleNow && isASEFlowCompletedInWorktree(opts.WorkingDir, curBoundStory, fullOut+"\n"+fullTranscript+"\n"+fullErr, gitBin) {
						turnCompletedDuringDrain = true
						turnBgDrainReaped = false
						drainStateDesc := "while root agent entered background drain"
						eventTag := "[ase_completion.detected_during_bg_drain]"
						if !inBgDrain && terminalPlannerTurn {
							drainStateDesc = "at terminal PLANNER_RESPONSE"
							eventTag = "[ase_completion.detected_at_terminal_turn]"
						}
						compNote := fmt.Sprintf(
							"[%s] [ASE-SUPERVISOR] %s ASE flow and story %s verified DONE in worktree %s (ToolPID=%d, Conv=%s, Tasks=%d); settling turn as completed.\n",
							time.Now().UTC().Format(time.RFC3339), eventTag, curBoundStory, drainStateDesc, toolPID, curConv, bgTasks,
						)
						emitSupervisorNote(compNote)
						engines.StopProcessGroup(cmd)
						select {
						case <-waitDone:
						case <-time.After(gracePeriod):
							engines.KillProcessGroup(cmd)
							<-waitDone
						}
						turnErr = nil
						break turnLoop
					}
				}
				if inBgDrain {
					if bgDrainSince.IsZero() {
						bgDrainSince = time.Now()
					} else if time.Since(bgDrainSince) >= bgDrainGrace {
						if sdlcActive, sdlcCmd := hasActiveSDLCChildProcess(toolPID); sdlcActive && bgDrainDeferrals < maxBgDrainDeferrals {
							bgDrainDeferrals++
							bgDrainSince = time.Now()
							deferDrainNote := fmt.Sprintf(
								"[%s] [ASE-SUPERVISOR] [background_task.sdlc_active] Deferring background drain reap (deferral=%d/%d) while active SDLC child process is running under ToolPID=%d: %s\n",
								time.Now().UTC().Format(time.RFC3339), bgDrainDeferrals, maxBgDrainDeferrals, toolPID, redact.Egress(sdlcCmd),
							)
							emitSupervisorNote(deferDrainNote)
							continue
						}
						turnBgDrainReaped = true
						reapNote := fmt.Sprintf(
							"[%s] [ASE-SUPERVISOR] [background_task.auto_reaped] Root agent idle in background drain (ToolPID=%d, Conv=%s, Tasks=%d); reaping idle background task(s) after %s to unblock auto-resume.\n",
							time.Now().UTC().Format(time.RFC3339), toolPID, curConv, bgTasks, bgDrainGrace,
						)
						emitSupervisorNote(reapNote)
						engines.StopProcessGroup(cmd)
						select {
						case wErr := <-waitDone:
							turnErr = wErr
						case <-time.After(gracePeriod):
							engines.KillProcessGroup(cmd)
							turnErr = <-waitDone
						}
						break turnLoop
					}
					if jobID != "" && apiURL != "" {
						elapsedSec := int(time.Since(startTime).Seconds())
						_ = controlplane.PostTick(ctx, apiURL, resolveToken(), jobID, time.Since(lastActivity).Milliseconds(), os.Getpid(), toolPID, elapsedSec)
						if toolAlive && time.Since(lastJobHeartbeatAt) >= 10*time.Second {
							emitJobHeartbeat(toolPID, curConv, curBoundStory)
							lastJobHeartbeatAt = time.Now()
						}
					}
					continue
				} else {
					bgDrainSince = time.Time{}
				}

				if time.Now().After(deadline) {
					if toolAlive && curConv != "" && !inBgDrain && timeoutDeferrals < maxTimeoutDeferrals {
						chatty, activeChildren := hasActiveConversationActivity(curConv, toolPID, lastActivity, turnTimeout)
						if chatty || activeChildren > 0 {
							timeoutDeferrals++
							deadline = time.Now().Add(turnTimeout)
							deferNote := fmt.Sprintf(
								"[%s] [ASE-SUPERVISOR] [pid_liveness.timeout_deferred] Agent tool PID=%d is still alive on conversation %s (chatty=%v, active_children=%d, deferral=%d/%d); deferring timeout while conversation is active.\n",
								time.Now().UTC().Format(time.RFC3339), toolPID, curConv, chatty, activeChildren, timeoutDeferrals, maxTimeoutDeferrals,
							)
							emitSupervisorNote(deferNote)
							if jobID != "" && apiURL != "" {
								emitJobHeartbeat(toolPID, curConv, curBoundStory)
								lastJobHeartbeatAt = time.Now()
							}
							continue
						}
					}
					timedOut = true
					if inBgDrain {
						turnBgDrainReaped = true
					}
					engines.StopProcessGroup(cmd)
					select {
					case <-waitDone:
					case <-time.After(gracePeriod):
						engines.KillProcessGroup(cmd)
						<-waitDone
					}
					turnErr = context.DeadlineExceeded
					break turnLoop
				}

				if jobID != "" && apiURL != "" {
					streamMu.Lock()
					silentMs := time.Since(lastOutAt).Milliseconds()
					streamMu.Unlock()
					elapsedSec := int(time.Since(startTime).Seconds())
					tok := resolveToken()

					if time.Since(lastHeartbeatChunkAt) >= supervisorHeartbeatInterval {
						hbStory := curBoundStory
						if hbStory == "" {
							hbStory = "pending"
						}
						hbConv := curConv
						if hbConv == "" {
							hbConv = "pending"
						}
						hbMsg := fmt.Sprintf(
							"[%s] %selapsed: %ds, HarnessPID=%d, ToolPID=%d, Story=%s, Conv=%s)\n",
							time.Now().UTC().Format(time.RFC3339), SupervisorHeartbeatPrefix, elapsedSec, os.Getpid(), toolPID, hbStory, hbConv,
						)
						_ = controlplane.PostChunk(ctx, apiURL, tok, jobID, "stdout", hbMsg)
						lastHeartbeatChunkAt = time.Now()
					}
					_ = controlplane.PostTick(ctx, apiURL, tok, jobID, silentMs, os.Getpid(), toolPID, elapsedSec)
					if toolAlive && time.Since(lastJobHeartbeatAt) >= 10*time.Second {
						emitJobHeartbeat(toolPID, curConv, curBoundStory)
						lastJobHeartbeatAt = time.Now()
					}
				}
			}
		}

		// Final sync of conversation and transcript steps from this turn's log file / brain dir
		_, _ = syncTurnTelemetry()
		streamMu.Lock()
		fullOutFinal := stdoutBuf.String()
		fullErrFinal := stderrBuf.String()
		outFinal := fullOutFinal
		if turnStartOutLen <= len(fullOutFinal) {
			outFinal = fullOutFinal[turnStartOutLen:]
		}
		errFinal := fullErrFinal
		if turnStartErrLen <= len(fullErrFinal) {
			errFinal = fullErrFinal[turnStartErrLen:]
		}
		curConvFinal := detectedConvID
		streamMu.Unlock()

		if curConvFinal == "" {
			if cid := discoverLogFileConversation(tmpLogPath); cid != "" {
				curConvFinal = cid
			} else if cid := discoverRepoLockConversation(opts.WorkingDir, startTime.Add(-2*time.Second)); cid != "" {
				curConvFinal = cid
			} else if m := convIDRegex.FindStringSubmatch(outFinal + "\n" + errFinal); len(m) > 1 && sessionstore.IsValidConversationID(m[1]) {
				curConvFinal = m[1]
			} else if cid := discoverNewBrainConversation(startTime.Add(-2*time.Second), preExistingConvs); cid != "" {
				curConvFinal = cid
			}
			if curConvFinal != "" {
				streamMu.Lock()
				detectedConvID = curConvFinal
				streamMu.Unlock()
				_, _ = fmt.Fprintf(os.Stderr, "CONVERSATION_ID: %s\nACTIVE_CONVERSATION_ID: %s\n", curConvFinal, curConvFinal)
				emitJobHeartbeat(toolPID, curConvFinal, boundStoryID)
				if jobID != "" && apiURL != "" {
					_ = controlplane.PostConversation(ctx, apiURL, resolveToken(), jobID, curConvFinal)
				}
			}
		}
		if curConvFinal != "" {
			streamMu.Lock()
			finalSteps := tailBrainTranscriptSteps(curConvFinal, seenTranscriptSteps)
			if finalSteps != "" {
				transcriptBuf.WriteString(finalSteps)
			}
			streamMu.Unlock()
			if finalSteps != "" && jobID != "" && apiURL != "" {
				_ = controlplane.PostChunk(ctx, apiURL, resolveToken(), jobID, "stdout", finalSteps)
			}
		}

		streamMu.Lock()
		curTurnStory := boundStoryID
		fullTranscriptFinal := transcriptBuf.String()
		streamMu.Unlock()
		if curTurnStory == "" || isPointerStoryID(curTurnStory) {
			if sid, stitle, spath := discoverStoryFromASEState(opts.WorkingDir, startTime.Add(-2*time.Second)); sid != "" {
				if curTurnStory == "" || (isPointerStoryID(curTurnStory) && !isPointerStoryID(sid)) {
					curTurnStory = sid
					streamMu.Lock()
					boundStoryID = sid
					if storyTitle == "" && stitle != "" {
						storyTitle = stitle
					}
					if storyPath == "" && spath != "" {
						storyPath = spath
					}
					streamMu.Unlock()
				}
			}
		}

		if matched, plugin := checkTurnStaleHook(curConvFinal, outFinal, errFinal); matched {
			lastTurnStaleHook = true
			lastTurnStalePlugin = plugin
		}

		if !canceled && !lastTurnStaleHook && !hasHeadlessPermissionDenial(fullOutFinal+"\n"+fullErrFinal) &&
			(turnCompletedDuringDrain || isASEFlowCompletedInWorktree(opts.WorkingDir, curTurnStory, fullOutFinal+"\n"+fullTranscriptFinal+"\n"+fullErrFinal, gitBin)) {
			return "completed", 0, nil, false, false
		}

		if !turnBgDrainReaped {
			if inBg, _ := hasBackgroundDrainLog(tmpLogPath, outFinal, errFinal); inBg && (turnErr != nil || timedOut) {
				turnBgDrainReaped = true
			}
		}
		turnPrintTimeout = hasPrintTimeoutLog(tmpLogPath, outFinal, errFinal)

		switch {
		case canceled:
			return "canceled", 1, turnErr, turnBgDrainReaped, turnPrintTimeout
		case timedOut:
			return "timed_out", 124, turnErr, turnBgDrainReaped, turnPrintTimeout
		case turnErr != nil:
			code := 1
			var exitErr *exec.ExitError
			if errors.As(turnErr, &exitErr) {
				code = exitErr.ExitCode()
			}
			return "failed", code, turnErr, turnBgDrainReaped, turnPrintTimeout
		default:
			return "completed", 0, nil, turnBgDrainReaped, turnPrintTimeout
		}
	}

	firstTurnIsResume := initialResumeConv != ""
	execStatus, exitCode, waitErr, bgDrainReaped, printTimedOut := runSingleTurn(opts.Prompt, initialResumeConv, firstTurnIsResume)

	isASEFlow := strings.Contains(opts.Prompt, "/autonomous-story-execution") || strings.TrimSpace(opts.StoryID) != "" || boundStoryID != ""

	// Check if in-place auto-resume should be triggered when conversation ID is resolved or when agy crashes during an ASE flow
	if (isMockAgy || isDirectAgy) && ctx.Err() == nil {
		failedConvAttempts := make(map[string]int)
		staleHookConvCount := make(map[string]int)
		for resumeAttempts < maxAutoResumes {
			streamMu.Lock()
			curConv := detectedConvID
			curStory := boundStoryID
			rawStdStreams := stdoutBuf.String() + "\n" + stderrBuf.String()
			rawCombined := stdoutBuf.String() + "\n" + transcriptBuf.String() + "\n" + stderrBuf.String()
			streamMu.Unlock()

			spRoot := resolveSuperprojectRoot(opts.WorkingDir, gitBin)
			if curStory == "" || isPointerStoryID(curStory) || (!isStoryIncompleteAnywhere(opts.WorkingDir, spRoot, curStory) && !isStoryDoneAnywhere(opts.WorkingDir, spRoot, curStory)) {
				if sid, _, _ := discoverStoryFromASEState(opts.WorkingDir, startTime.Add(-2*time.Second)); sid != "" {
					if curStory == "" || (isPointerStoryID(curStory) && !isPointerStoryID(sid)) || isStoryIncompleteAnywhere(opts.WorkingDir, spRoot, sid) {
						curStory = sid
						streamMu.Lock()
						boundStoryID = sid
						streamMu.Unlock()
					}
				}
			}
			if curStory != "" {
				isASEFlow = true
				if opts.MaxAutoResumeAttempts == nil && r.MaxAutoResumeAttempts == nil && strings.TrimSpace(os.Getenv("IAZIO_MAX_AUTO_RESUME_ATTEMPTS")) == "" && maxAutoResumes < defaultASEMaxAutoResumeAttempts {
					maxAutoResumes = defaultASEMaxAutoResumeAttempts
				}
			}
			loopGateRep := evaluateMultiRepoASEGates(opts.WorkingDir, curStory, rawCombined, gitBin)
			if !lastTurnStaleHook && !hasHeadlessPermissionDenial(rawStdStreams) && loopGateRep.Complete {
				execStatus = "completed"
				exitCode = 0
				waitErr = nil
				bgDrainReaped = false
				printTimedOut = false
				break
			}
			nonEphemeralModified := loopGateRep.DirtyFiles
			hasDirtyWorktree := len(nonEphemeralModified) > 0
			storyIncomplete := isStoryIncompleteAnywhere(opts.WorkingDir, spRoot, curStory) || loopGateRep.StoryIncomplete
			isExplicitASEPrompt := isASEExecutionPrompt(opts.Prompt)
			onNonMainBranch := loopGateRep.AnyNonMainBranch
			nonMainBranchName := loopGateRep.FirstNonMainBranch
			if nonMainBranchName == "" {
				nonMainBranchName = loopGateRep.BranchSummary
			}
			unpushedCnt := loopGateRep.UnpushedTotal
			aseBranchIncomplete := isASEFlow && ((storyIncomplete && (isExplicitASEPrompt || onNonMainBranch)) || (isExplicitASEPrompt && (onNonMainBranch || unpushedCnt > 0 || !loopGateRep.SubmodulesClean || !loopGateRep.BranchClean || !loopGateRep.StoryDone)))
			aseDirtyIncomplete := isASEFlow && hasDirtyWorktree

			lowerCombined := strings.ToLower(rawCombined)
			lowerStd := strings.ToLower(stripTranscriptStepLinesForClassification(rawStdStreams))
			isFatalQuota := strings.Contains(lowerStd, "resourceexhausted") || strings.Contains(lowerStd, "429 you have exhausted")
			isCorruptedSession := strings.Contains(lowerCombined, "conversation session corrupted") || strings.Contains(lowerCombined, "database disk image is malformed")

			var staleSanRes agyhooks.SanitizeResult
			if lastTurnStaleHook {
				staleSanRes, _ = agyhooks.SanitizeEnv(os.Environ())
				if curConv != "" {
					staleHookConvCount[curConv]++
				}
			}

			if (execStatus != "completed" || exitCode != 0) && curConv != "" {
				if !lastTurnStaleHook || staleHookConvCount[curConv] > 1 {
					failedConvAttempts[curConv]++
				}
			}
			aseCrashOrIncomplete := isASEFlow && (execStatus != "completed" || exitCode != 0) && !isFatalQuota
			needsResume := !hasHeadlessPermissionDenial(rawStdStreams) && !isFatalQuota &&
				(lastTurnStaleHook ||
					(curConv != "" && (bgDrainReaped || printTimedOut || aseDirtyIncomplete || aseBranchIncomplete || (storyIncomplete && (execStatus != "completed" || hasDirtyWorktree || bgDrainIdleRegex.MatchString(rawCombined))))) ||
					aseCrashOrIncomplete)

			if !needsResume {
				break
			}

			resumeAttempts++
			storyTarget := curStory
			if storyTarget == "" {
				storyTarget = "active story"
			}

			fallbackToNewConv := curConv == "" || isCorruptedSession || (curConv != "" && failedConvAttempts[curConv] >= 2)
			if lastTurnStaleHook {
				if fallbackToNewConv {
					prevConv := curConv
					curConv = ""
					streamMu.Lock()
					detectedConvID = ""
					streamMu.Unlock()
					hookConvLabel := prevConv
					if hookConvLabel == "" {
						hookConvLabel = "<new>"
					}
					resumeNote := fmt.Sprintf(
						"[%s] [ASE-SUPERVISOR] [plugin_hook.sanitized_auto_resume] Detected stale Antigravity plugin hook (%s); sanitized ~/.gemini/config/plugins (removed=%v, disabled=%v) and auto-resuming conversation %s (attempt %d/%d)...\n",
						time.Now().UTC().Format(time.RFC3339), lastTurnStalePlugin, staleSanRes.RemovedPluginDirs, staleSanRes.DisabledPlugins, hookConvLabel, resumeAttempts, maxAutoResumes,
					)
					emitSupervisorNote(resumeNote)
					resumePrompt := "Stale ~/.gemini/config/plugins PreToolUse hook directory has been sanitized by iazio-harness and tool execution is restored. Continue and complete the original request."
					if isASEFlow {
						resumePrompt = fmt.Sprintf(
							"Stale ~/.gemini/config/plugins PreToolUse hook directory has been sanitized by iazio-harness and tool execution is restored. Continue and complete /autonomous-story-execution for %s in a new conversation: inspect existing worktree state (branch=%s, dirty_files=%d, unpushed_commits=%d), finish remaining tasks, commit all changes cleanly, mark story status: DONE, run autopilot-git-flow land --story %s --status-done --record-reviews, and settle the worktree on main.",
							storyTarget, nonMainBranchName, len(nonEphemeralModified), unpushedCnt, storyTarget,
						)
					}
					execStatus, exitCode, waitErr, bgDrainReaped, printTimedOut = runSingleTurn(resumePrompt, "", false)
				} else {
					resumeNote := fmt.Sprintf(
						"[%s] [ASE-SUPERVISOR] [plugin_hook.sanitized_auto_resume] Detected stale Antigravity plugin hook (%s); sanitized ~/.gemini/config/plugins (removed=%v, disabled=%v) and auto-resuming conversation %s (attempt %d/%d)...\n",
						time.Now().UTC().Format(time.RFC3339), lastTurnStalePlugin, staleSanRes.RemovedPluginDirs, staleSanRes.DisabledPlugins, curConv, resumeAttempts, maxAutoResumes,
					)
					emitSupervisorNote(resumeNote)
					resumePrompt := "Stale ~/.gemini/config/plugins PreToolUse hook directory has been sanitized by iazio-harness and tool execution is restored. Continue and complete the original request."
					if isASEFlow {
						resumePrompt = fmt.Sprintf(
							"Stale ~/.gemini/config/plugins PreToolUse hook directory has been sanitized by iazio-harness and tool execution is restored. Continue and complete /autonomous-story-execution for %s on conversation %s (branch=%s, dirty_files=%d, unpushed_commits=%d): finish remaining tasks, commit all worktree changes cleanly, mark story status: DONE, run autopilot-git-flow land --story %s --status-done --record-reviews, and settle the worktree on main.",
							storyTarget, curConv, nonMainBranchName, len(nonEphemeralModified), unpushedCnt, storyTarget,
						)
					}
					execStatus, exitCode, waitErr, bgDrainReaped, printTimedOut = runSingleTurn(resumePrompt, curConv, true)
				}
				continue
			}
			if fallbackToNewConv {
				prevConv := curConv
				curConv = ""
				streamMu.Lock()
				detectedConvID = ""
				streamMu.Unlock()
				resumeNote := fmt.Sprintf(
					"[%s] [ASE-SUPERVISOR] [auto_resume.new_conversation_fallback] Previous conversation %q failed or was unresumable; starting new conversation (attempt %d/%d, Story=%s, branch=%s, dirty_files=%d, unpushed=%d) to complete ASE flow...\n",
					time.Now().UTC().Format(time.RFC3339), prevConv, resumeAttempts, maxAutoResumes, storyTarget, nonMainBranchName, len(nonEphemeralModified), unpushedCnt,
				)
				emitSupervisorNote(resumeNote)
				resumePrompt := fmt.Sprintf(
					"Resume and complete /autonomous-story-execution for %s in a new conversation: inspect existing worktree state (branch=%s, dirty_files=%d, unpushed_commits=%d), finish remaining tasks, commit all changes cleanly, mark story status: DONE, run autopilot-git-flow land --story %s --status-done --record-reviews, and settle the worktree on main.",
					storyTarget, nonMainBranchName, len(nonEphemeralModified), unpushedCnt, storyTarget,
				)
				execStatus, exitCode, waitErr, bgDrainReaped, printTimedOut = runSingleTurn(resumePrompt, "", false)
			} else {
				resumeNote := fmt.Sprintf(
					"[%s] [ASE-SUPERVISOR] [auto_resume.triggered] Auto-resuming conversation %s (attempt %d/%d, Story=%s, branch=%s, dirty_files=%d, unpushed=%d) to complete job after background drain / partial turn...\n",
					time.Now().UTC().Format(time.RFC3339), curConv, resumeAttempts, maxAutoResumes, storyTarget, nonMainBranchName, len(nonEphemeralModified), unpushedCnt,
				)
				emitSupervisorNote(resumeNote)

				resumePrompt := fmt.Sprintf(
					"Resume and complete /autonomous-story-execution for %s on conversation %s: worktree is not yet settled on main (branch=%s, dirty_files=%d, unpushed_commits=%d). Finish remaining tasks, commit all worktree changes cleanly, mark story status: DONE, run autopilot-git-flow land --story %s --status-done --record-reviews, and settle the worktree on main.",
					storyTarget, curConv, nonMainBranchName, len(nonEphemeralModified), unpushedCnt, storyTarget,
				)
				execStatus, exitCode, waitErr, bgDrainReaped, printTimedOut = runSingleTurn(resumePrompt, curConv, true)
			}
		}
	}

	durationMs := time.Since(startTime).Milliseconds()
	elapsedSeconds := int(time.Since(startTime).Seconds())

	// 5. Post-Execution Git Snapshot
	headSHA, treeSHA, modified := captureGitSnapshot(opts.WorkingDir, gitBin)

	// 6. Output Summary & Redaction
	streamMu.Lock()
	rawOut := stdoutBuf.String()
	rawTranscript := transcriptBuf.String()
	rawErr := stderrBuf.String()
	alreadyStreamed := streamedAny
	curConv := detectedConvID
	curBoundStory := boundStoryID
	curBoundTitle := storyTitle
	curBoundPath := storyPath
	streamMu.Unlock()

	combinedOutput := rawOut + "\n" + rawTranscript + "\n" + rawErr
	fullOutput := rawOut
	if trimmedTranscript := strings.TrimSpace(rawTranscript); trimmedTranscript != "" && !strings.Contains(fullOutput, trimmedTranscript) {
		if strings.TrimSpace(fullOutput) == "" {
			fullOutput = rawTranscript
		} else {
			fullOutput = strings.TrimRight(fullOutput, "\n") + "\n" + rawTranscript
		}
	}
	if strings.TrimSpace(fullOutput) == "" {
		if len(rawErr) > 0 {
			fullOutput = rawErr
		} else if waitErr != nil {
			fullOutput = waitErr.Error()
		}
	}
	fullOutput = redact.Egress(strings.TrimSpace(fullOutput))
	summary := BuildConcisePromptSummary(rawOut, rawTranscript, rawErr, waitErr)

	if hasHeadlessPermissionDenial(rawOut + "\n" + rawErr) {
		execStatus = "failed"
		if exitCode == 0 {
			exitCode = 1
		}
		failureCategory = "PERMISSION_DENIED"
		failureStage = "TOOL_EXECUTION"
		waitErr = fmt.Errorf("PERMISSION_DENIED: headless agent tool permission was auto-denied: %s", summary)
	} else if lastTurnStaleHook {
		execStatus = "failed"
		if exitCode == 0 {
			exitCode = 1
		}
		failureCategory = "STALE_PLUGIN_HOOK_DIR"
		failureStage = "TOOL_EXECUTION"
		waitErr = fmt.Errorf("STALE_PLUGIN_HOOK_DIR: Antigravity PreToolUse plugin hook (%s) failed with missing plugin directory", lastTurnStalePlugin)
	}

	detectedStoryID := curBoundStory
	if detectedStoryID == "" {
		detectedStoryID = storyID
	}
	detectedStoryPath := curBoundPath
	detectedStoryTitle := curBoundTitle

	if aseSID, aseTitle, asePath, fromSession := discoverStoryFromASEStateWithSource(opts.WorkingDir, startTime.Add(-2*time.Second), true); aseSID != "" {
		if detectedStoryID == "" ||
			(isPointerStoryID(detectedStoryID) && !isPointerStoryID(aseSID)) ||
			(strings.TrimSpace(opts.StoryID) == "" && fromSession && !isPointerStoryID(aseSID) && aseSID != detectedStoryID) {
			detectedStoryID = aseSID
			if aseTitle != "" {
				detectedStoryTitle = aseTitle
			}
			if asePath != "" {
				detectedStoryPath = asePath
			}
		} else {
			if detectedStoryTitle == "" && aseTitle != "" {
				detectedStoryTitle = aseTitle
			}
			if detectedStoryPath == "" && asePath != "" {
				detectedStoryPath = asePath
			}
		}
	}

	for _, f := range modified {
		if (strings.HasPrefix(f, "docs/stories/") || strings.Contains(f, "/docs/stories/")) && strings.HasSuffix(f, ".story.md") {
			if m := storyIDRegex.FindStringSubmatch(f); len(m) > 1 {
				candID := strings.ToUpper(m[1])
				if detectedStoryID == "" || (isPointerStoryID(detectedStoryID) && !isPointerStoryID(candID)) {
					detectedStoryID = candID
					detectedStoryPath = f
					break
				}
			}
			if detectedStoryPath == "" {
				detectedStoryPath = f
			}
		}
	}

	if detectedStoryID == "" || isPointerStoryID(detectedStoryID) {
		for _, m := range storyIDRegex.FindAllStringSubmatch(combinedOutput, -1) {
			if len(m) > 1 {
				candID := strings.ToUpper(m[1])
				if !isPointerStoryID(candID) {
					detectedStoryID = candID
					break
				}
				if detectedStoryID == "" {
					detectedStoryID = candID
				}
			}
		}
	}
	if detectedStoryTitle == "" && opts.Title != "" {
		detectedStoryTitle = opts.Title
	}
	streamMu.Lock()
	alreadyPostedStory := postedStoryID
	streamMu.Unlock()
	if detectedStoryID != "" && detectedStoryID != alreadyPostedStory && jobID != "" && apiURL != "" {
		_ = controlplane.PostStoryBinding(ctx, apiURL, resolveToken(), jobID, detectedStoryID, detectedStoryTitle, detectedStoryPath, fencingToken)
	}

	finalExplicitASE := isASEExecutionPrompt(opts.Prompt)
	var finalGateRep aseMultiRepoReport
	if isASEFlow || finalExplicitASE || strings.TrimSpace(detectedStoryID) != "" {
		finalGateRep = evaluateMultiRepoASEGates(opts.WorkingDir, detectedStoryID, combinedOutput, gitBin)
	}

	if !lastTurnStaleHook && !hasHeadlessPermissionDenial(combinedOutput) && finalGateRep.Complete {
		execStatus = "completed"
		exitCode = 0
		waitErr = nil
		bgDrainReaped = false
		failureCategory = ""
		failureStage = ""
	}

	// Enforce fail-closed BACKGROUND_DRAIN_TIMEOUT if background drain was not resolved by auto-resume or completion verification
	if bgDrainReaped && (execStatus != "completed" || isStoryIncompleteAnywhere(opts.WorkingDir, superprojectRoot, detectedStoryID)) {
		execStatus = "timed_out"
		if exitCode == 0 {
			exitCode = 124
		}
		failureCategory = "BACKGROUND_DRAIN_TIMEOUT"
		failureStage = "BACKGROUND_DRAIN"
		if waitErr == nil {
			waitErr = fmt.Errorf("BACKGROUND_DRAIN_TIMEOUT: conversation %s timed out waiting on background drain before completing story %s", curConv, detectedStoryID)
		}
	}

	nonEphemeralFinal := filterNonEphemeralModified(modified)
	dirtyAll := finalGateRep.DirtyFiles
	if len(dirtyAll) == 0 && len(nonEphemeralFinal) > 0 {
		dirtyAll = nonEphemeralFinal
	}
	finalStoryIncomplete := isStoryIncompleteAnywhere(opts.WorkingDir, superprojectRoot, detectedStoryID) || finalGateRep.StoryIncomplete
	finalStoryDone := finalGateRep.StoryDone
	finalOnNonMainBranch, finalBranchName := isNonMainWorktreeBranch(opts.WorkingDir, gitBin)
	if finalGateRep.AnyNonMainBranch {
		finalOnNonMainBranch = true
		if finalGateRep.FirstNonMainBranch != "" {
			finalBranchName = finalGateRep.FirstNonMainBranch
		}
	}
	if (isASEFlow || finalExplicitASE) && execStatus == "completed" {
		markGateIncomplete := func(errMsg string) {
			execStatus = "failed"
			if exitCode == 0 {
				exitCode = 1
			}
			failureCategory = "ASE_GATE_INCOMPLETE"
			failureStage = "COMPLETION_GATE"
			if waitErr == nil {
				waitErr = fmt.Errorf("%s", errMsg)
			}
		}
		if len(dirtyAll) > 0 {
			markGateIncomplete(fmt.Sprintf("ASE_GATE_INCOMPLETE: dirty_worktree_post_exit (%d uncommitted file(s) remaining across scoped repos: %s)", len(dirtyAll), strings.Join(dirtyAll, ", ")))
		} else if !finalGateRep.SubmodulesClean {
			markGateIncomplete(fmt.Sprintf("ASE_GATE_INCOMPLETE: %s", finalGateRep.SubmoduleReason))
		} else if finalExplicitASE && strings.TrimSpace(detectedStoryID) == "" {
			markGateIncomplete("ASE_GATE_INCOMPLETE: missing_story_binding (no canonical STORY_ID bound or created during ASE run)")
		} else if finalExplicitASE && !finalStoryDone {
			reasonSuffix := strings.Join(finalGateRep.StoryReport.Reasons, ", ")
			if reasonSuffix == "" {
				reasonSuffix = fmt.Sprintf("story %s is not marked DONE in docs/stories/ on branch %s", detectedStoryID, finalBranchName)
			}
			markGateIncomplete(fmt.Sprintf("ASE_GATE_INCOMPLETE: incomplete_ase_flow (%s)", reasonSuffix))
		} else if finalStoryIncomplete && finalOnNonMainBranch {
			markGateIncomplete(fmt.Sprintf("ASE_GATE_INCOMPLETE: incomplete_ase_flow (story %s remains incomplete on branch %s)", detectedStoryID, finalBranchName))
		} else if finalExplicitASE && finalOnNonMainBranch {
			markGateIncomplete(fmt.Sprintf("ASE_GATE_INCOMPLETE: unmerged_feature_branch (repo worktree remains on branch %s; git-flow land not completed)", finalBranchName))
		} else if finalExplicitASE && finalGateRep.UnpushedProbeErr != nil {
			markGateIncomplete(fmt.Sprintf("ASE_GATE_INCOMPLETE: unpushed_probe_failed (%v)", finalGateRep.UnpushedProbeErr))
		} else if finalExplicitASE && finalGateRep.UnpushedTotal > 0 {
			markGateIncomplete(fmt.Sprintf("ASE_GATE_INCOMPLETE: unpushed_commits (%d unpushed commit(s) across scoped repos; git-flow land not completed)", finalGateRep.UnpushedTotal))
		}
	}

	var finalASEComplete bool
	var finalASEGateStatus map[string]bool
	if isASEFlow || finalExplicitASE || finalGateRep.StoryReport.Found {
		testsPassedGate := execStatus == "completed"
		gitCleanGate := finalGateRep.GitClean && len(dirtyAll) == 0
		branchCleanGate := finalGateRep.BranchClean && !finalOnNonMainBranch && finalGateRep.UnpushedProbeErr == nil && finalGateRep.UnpushedTotal == 0
		submodulesCleanGate := finalGateRep.SubmodulesClean
		storyDoneGate := finalGateRep.StoryDone
		if !finalExplicitASE && !finalGateRep.StoryReport.Found {
			storyDoneGate = finalGateRep.HasCompletionEvidence && !finalStoryIncomplete
		}
		releaseVerifiedGate := testsPassedGate && gitCleanGate && branchCleanGate && submodulesCleanGate && storyDoneGate
		finalASEComplete = releaseVerifiedGate
		finalASEGateStatus = map[string]bool{
			"git_clean":        gitCleanGate,
			"branch_clean":     branchCleanGate,
			"submodules_clean": submodulesCleanGate,
			"tests_passed":     testsPassedGate,
			"release_verified": releaseVerifiedGate,
			"story_done":       storyDoneGate,
		}
	}

	failureDirection := "NONE"
	failureService := ""
	if execStatus != "completed" {
		failureCategory, failureStage, failureDirection, failureService = ClassifyPromptFailure(execStatus, failureCategory, failureStage, combinedOutput, waitErr)
	}

	if summary == "" && waitErr != nil {
		summary = BuildConcisePromptSummary(rawOut, rawTranscript, rawErr, waitErr)
	}
	if fullOutput == "" && waitErr != nil {
		fullOutput = redact.Egress(strings.TrimSpace(waitErr.Error()))
	}

	if jobID != "" && apiURL != "" && summary != "" && !alreadyStreamed {
		_ = controlplane.PostChunk(ctx, apiURL, resolveToken(), jobID, "stdout", summary+"\n")
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
		HarnessPID:           os.Getpid(),
		ToolPID:              lastToolPID,
		ElapsedSeconds:       elapsedSeconds,
		FailureCategory:      failureCategory,
		FailureStage:         failureStage,
		FailureDirection:     failureDirection,
		FailureService:       failureService,
		ResumeAttempts:       resumeAttempts,
		Status:               execStatus,
		ExitCode:             exitCode,
		ASEComplete:          finalASEComplete,
		ASEGateStatus:        finalASEGateStatus,
		PreGitHeadSHA:        preHeadSHA,
		PreGitTreeSHA:        preTreeSHA,
		GitHeadSHA:           headSHA,
		GitTreeSHA:           treeSHA,
		ModifiedFiles:        modified,
		Summary:              summary,
		Output:               fullOutput,
		DurationMs:           durationMs,
	}

	completeErrMsg := ""
	if execStatus != "completed" {
		if waitErr != nil {
			completeErrMsg = fmt.Sprintf("autopilot execution %s: %v", execStatus, waitErr)
		} else {
			completeErrMsg = fmt.Sprintf("autopilot execution %s (exit=%d)", execStatus, exitCode)
		}
		syncLocalWatchdogAndLiveness(lastToolPID, curConv, detectedStoryID, watchdog.StatusFailed, exitCode, completeErrMsg)
	} else {
		syncLocalWatchdogAndLiveness(lastToolPID, curConv, detectedStoryID, watchdog.StatusCompleted, 0, "")
	}

	// Multi-day (>24h) orphan completion fallback: if parent iazio-agent upgraded and self-restarted mid-run,
	// settle the job directly on iazio-harness-api so completion is never lost when stdout pipe is closed.
	parentOrphaned := os.Getenv("IAZIO_FORCE_DIRECT_COMPLETE") == "1" ||
		(!isPromptRunnerTestBinary() && (initialParentPID <= 1 || os.Getppid() != initialParentPID || !watchdog.IsProcessAlive(initialParentPID)))
	if jobID != "" && apiURL != "" && parentOrphaned {
		_ = controlplane.PostAgentJobComplete(ctx, apiURL, resolveToken(), jobID, fencingToken, result, completeErrMsg)
	}

	if waitErr != nil && execStatus != "completed" {
		return result, fmt.Errorf("autopilot execution %s: %w", execStatus, waitErr)
	}

	return result, nil
}

func parseGitPorcelainPaths(raw string) []string {
	var paths []string
	trimmed := strings.TrimRight(raw, "\r\n")
	if trimmed == "" {
		return paths
	}
	for _, line := range strings.Split(trimmed, "\n") {
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
			paths = append(paths, pathPart)
		}
	}
	return paths
}

func captureGitSnapshot(dir, gitBin string) (headSHA, treeSHA string, modified []string) {
	modified = make([]string, 0)
	if dir == "" {
		return "", "", modified
	}

	// 1. git rev-parse HEAD
	cmdHead := procutil.Command(gitBin, "rev-parse", "HEAD")
	cmdHead.Dir = dir
	if out, err := cmdHead.Output(); err == nil {
		headSHA = strings.TrimSpace(string(out))
	}

	// 2. git rev-parse HEAD^{tree}
	cmdTree := procutil.Command(gitBin, "rev-parse", "HEAD^{tree}")
	cmdTree.Dir = dir
	if out, err := cmdTree.Output(); err == nil {
		treeSHA = strings.TrimSpace(string(out))
	}

	// 3. git status --porcelain --ignore-submodules=none (plus nested submodule dirty file expansion)
	cmdStatus := procutil.Command(gitBin, "status", "--porcelain", "--ignore-submodules=none")
	cmdStatus.Dir = dir
	out, err := cmdStatus.Output()
	if err != nil {
		cmdFallback := procutil.Command(gitBin, "status", "--porcelain")
		cmdFallback.Dir = dir
		out, err = cmdFallback.Output()
	}
	if err == nil {
		seen := make(map[string]bool)
		addMod := func(p string) {
			if p != "" && !seen[p] {
				seen[p] = true
				modified = append(modified, p)
			}
		}
		for _, pathPart := range parseGitPorcelainPaths(string(out)) {
			subDir := filepath.Join(dir, filepath.FromSlash(pathPart))
			if _, statErr := os.Stat(filepath.Join(subDir, ".git")); statErr == nil {
				subStatus := procutil.Command(gitBin, "status", "--porcelain")
				subStatus.Dir = subDir
				if subOut, subErr := subStatus.Output(); subErr == nil {
					subPaths := parseGitPorcelainPaths(string(subOut))
					if len(subPaths) > 0 {
						for _, sp := range subPaths {
							addMod(pathPart + "/" + sp)
						}
						continue
					}
				}
			}
			addMod(pathPart)
		}
	}

	return headSHA, treeSHA, modified
}

// ResolvePromptExecutionModel selects the agy model and effort for a prompt turn based on
// task scope, resume attempt tier, explicit overrides, and local quota circuit caches.
func ResolvePromptExecutionModel(opts RunPromptOptions, resumeAttempt int) (model, effort string) {
	return resolvePromptExecutionModelFromCache(opts, resumeAttempt, defaultQuotaCachePaths(), time.Now().UTC())
}

func defaultQuotaCachePaths() []string {
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return nil
	}
	return []string{
		filepath.Join(home, ".autopilot", "state", "model-quota-circuit.json"),
		filepath.Join(home, ".autopilot", "review-model-cache.json"),
	}
}

func resolvePromptExecutionModelFromCache(opts RunPromptOptions, resumeAttempt int, cachePaths []string, now time.Time) (model, effort string) {
	tripped := loadTrippedAgyModels(cachePaths, now)
	ladder := agyCandidateLadder(opts, resumeAttempt)

	chosenModel := ""
	explicitModel := strings.TrimSpace(opts.Model)
	if explicitModel != "" && !strings.EqualFold(explicitModel, "auto") {
		if !tripped[strings.ToLower(explicitModel)] {
			chosenModel = explicitModel
		}
	}
	if chosenModel == "" {
		for _, candidate := range ladder {
			if !tripped[strings.ToLower(candidate)] {
				chosenModel = candidate
				break
			}
		}
	}
	if chosenModel == "" && len(ladder) > 0 {
		chosenModel = ladder[0]
	}

	chosenEffort := strings.ToLower(strings.TrimSpace(opts.Effort))
	if chosenEffort == "" {
		chosenEffort = defaultEffortForModel(chosenModel)
	}
	return chosenModel, chosenEffort
}

func agyCandidateLadder(opts RunPromptOptions, resumeAttempt int) []string {
	repo := strings.TrimSpace(opts.Repo)
	isMultiOrRoot := repo == "" || repo == "." || strings.EqualFold(repo, "meta-repo") || strings.Contains(opts.Prompt, "submodules")
	switch {
	case resumeAttempt >= 2:
		return []string{"gemini-4-hydrogen-high", "gemini-4-argon-high", "gemini-4-carbon-high", "gemini-3.8-flash-high"}
	case resumeAttempt == 1 || isMultiOrRoot:
		return []string{"gemini-4-carbon-high", "gemini-4-argon-high", "gemini-3.8-flash-high"}
	default:
		return []string{"gemini-3.8-flash-high", "gemini-4-carbon-medium", "gemini-4-carbon-high", "gemini-4-argon-high"}
	}
}

func defaultEffortForModel(model string) string {
	lower := strings.ToLower(strings.TrimSpace(model))
	switch {
	case lower == "gemini-4-hydrogen-high" || strings.HasSuffix(lower, "-xhigh"):
		return "xhigh"
	case strings.HasSuffix(lower, "-medium"):
		return "medium"
	case strings.HasSuffix(lower, "-low"):
		return "low"
	case strings.HasSuffix(lower, "-max"):
		return "max"
	case lower == "":
		return ""
	default:
		return "high"
	}
}

func loadTrippedAgyModels(cachePaths []string, now time.Time) map[string]bool {
	tripped := make(map[string]bool)
	for _, path := range cachePaths {
		if strings.TrimSpace(path) == "" {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil || len(bytes.TrimSpace(data)) == 0 {
			continue
		}
		var raw any
		if err := json.Unmarshal(data, &raw); err != nil {
			continue
		}
		collectTrippedAgyModels(raw, "", now, tripped)
	}
	return tripped
}

func collectTrippedAgyModels(node any, keyHint string, now time.Time, tripped map[string]bool) {
	switch typed := node.(type) {
	case []any:
		for _, item := range typed {
			collectTrippedAgyModels(item, "", now, tripped)
		}
	case map[string]any:
		if isQuotaEntryMap(typed) {
			if model, ok := extractActiveTrippedAgyModel(typed, keyHint, now); ok {
				tripped[strings.ToLower(model)] = true
			}
			return
		}
		for k, v := range typed {
			nextHint := k
			switch strings.ToLower(k) {
			case "entries", "models", "circuits", "cache", "items":
				nextHint = ""
			}
			collectTrippedAgyModels(v, nextHint, now, tripped)
		}
	}
}

func isQuotaEntryMap(m map[string]any) bool {
	for _, field := range []string{"status", "disabled", "expires_at", "expiresAt"} {
		if _, exists := m[field]; exists {
			return true
		}
	}
	return false
}

func extractActiveTrippedAgyModel(m map[string]any, keyHint string, now time.Time) (string, bool) {
	hintTool, hintModel := splitToolModelKey(keyHint)
	tool := firstNonEmptyString(m["tool"], hintTool)
	model := firstNonEmptyString(m["model"], hintModel)
	if model == "" {
		return "", false
	}
	if tool != "" && !strings.EqualFold(tool, "agy") {
		return "", false
	}
	if tool == "" && strings.Contains(keyHint, ":") {
		return "", false
	}

	disabled, _ := m["disabled"].(bool)
	status := strings.ToLower(strings.TrimSpace(firstNonEmptyString(m["status"])))
	isTrippedStatus := disabled || status == "quota-exceeded" || status == "quota_exceeded" || status == "disabled"
	if !isTrippedStatus {
		return "", false
	}

	expiresAt, hasExpiry := parseOptionalExpiry(m["expires_at"], m["expiresAt"])
	if hasExpiry && !expiresAt.After(now) {
		return "", false
	}
	return model, true
}

func splitToolModelKey(key string) (tool, model string) {
	trimmed := strings.TrimSpace(key)
	if idx := strings.Index(trimmed, ":"); idx > 0 {
		return strings.TrimSpace(trimmed[:idx]), strings.TrimSpace(trimmed[idx+1:])
	}
	return "", trimmed
}

func firstNonEmptyString(vals ...any) string {
	for _, v := range vals {
		if s, ok := v.(string); ok {
			if trimmed := strings.TrimSpace(s); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}

func parseOptionalExpiry(vals ...any) (time.Time, bool) {
	for _, v := range vals {
		if v == nil {
			continue
		}
		switch tv := v.(type) {
		case string:
			s := strings.TrimSpace(tv)
			if s == "" {
				continue
			}
			if parsed, err := time.Parse(time.RFC3339Nano, s); err == nil {
				return parsed, true
			}
			if parsed, err := time.Parse(time.RFC3339, s); err == nil {
				return parsed, true
			}
		case float64:
			if tv > 0 {
				return time.Unix(int64(tv), 0).UTC(), true
			}
		}
	}
	return time.Time{}, false
}
