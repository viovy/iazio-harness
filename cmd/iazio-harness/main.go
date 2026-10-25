// Command iazio-harness runs one stored prompt and exits.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/viovy/iazio-harness/internal/ase/gate"
	"github.com/viovy/iazio-harness/internal/auth"
	"github.com/viovy/iazio-harness/internal/checkpoint"
	"github.com/viovy/iazio-harness/internal/controlplane"
	"github.com/viovy/iazio-harness/internal/diagnostics"
	"github.com/viovy/iazio-harness/internal/engines"
	"github.com/viovy/iazio-harness/internal/prompt"
	"github.com/viovy/iazio-harness/internal/repolock"
	"github.com/viovy/iazio-harness/internal/service"
	"github.com/viovy/iazio-harness/internal/sessionstore"
	"github.com/viovy/iazio-harness/internal/spool"
	"github.com/viovy/iazio-harness/internal/telemetry"
	"github.com/viovy/iazio-harness/internal/watchdog"
)

const fallbackResumeStoryID = "STORY-RESUME-INPLACE"

var (
	version = "0.1.0-dev"
	commit  = "unknown"
	branch  = "unknown"
)

func main() {
	flushTracer := telemetry.InitGlobalTracer("iazio-harness", "")
	defer flushTracer()

	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: iazio-harness version|auth|run|story|prompt|codebase|status|heal-ase|service|daemon")
	}
	var spanOpts = []telemetry.SpanOption{
		telemetry.WithKind("INTERNAL"),
		telemetry.WithTag("cli.command", args[0]),
	}
	if tp := os.Getenv("TRACEPARENT"); tp != "" {
		parts := strings.Split(tp, "-")
		if len(parts) >= 3 && len(parts[1]) == 32 && len(parts[2]) == 16 {
			spanOpts = append(spanOpts, telemetry.WithParentTrace(parts[1], parts[2]))
		}
	}
	ctx, span := telemetry.StartSpan(context.Background(), "harness."+args[0], spanOpts...)
	defer span.End()
	switch args[0] {
	case "version":
		fmt.Println(formatVersion())
		return nil
	case "auth":
		if len(args) > 1 && args[1] == "login" {
			return auth.Login(context.Background())
		}
		st, err := auth.Status()
		if err != nil {
			return err
		}
		fmt.Println(st.String())
		return nil
	case "run":
		return runJobWithContext(ctx, args[1:])
	case "story":
		return runStoryCommand(args[1:])
	case "prompt":
		return runPromptCommand(args[1:])
	case "codebase":
		return runCodebaseCommand(args[1:])
	case "status", "fleet-status", "inspect":
		return runFleetStatusCommand(args[1:])
	case "heal-ase":
		return runHealASECommand(args[1:])
	case "service":
		return runServiceCommand(args[1:])
	case "daemon":
		return runDaemonCommand(args[1:])
	case "verify-release", "verify-live-release":
		return runVerifyReleaseCommand(args[1:])
	default:
		return fmt.Errorf("unknown command")
	}
}

func runJob(args []string) error {
	return runJobWithContext(context.Background(), args)
}

func runJobWithContext(ctx context.Context, args []string) error {
	var api, jobID, worktree, docsHub, chunk, kind, resumeConv string
	var attachToolPID int
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--api-url":
			i++
			if i < len(args) {
				api = args[i]
			}
		case "--job-id":
			i++
			if i < len(args) {
				jobID = args[i]
			}
		case "--worktree-path":
			i++
			if i < len(args) {
				worktree = args[i]
			}
		case "--docs-hub-path":
			i++
			if i < len(args) {
				docsHub = args[i]
			}
		case "--chunk":
			i++
			if i < len(args) {
				chunk = args[i]
			}
		case "--kind":
			i++
			if i < len(args) {
				kind = args[i]
			}
		case "--resume-conversation", "--conversation":
			i++
			if i < len(args) {
				resumeConv = args[i]
			}
		case "--attach-tool-pid":
			i++
			if i < len(args) {
				attachToolPID, _ = strconv.Atoi(args[i])
			}
		}
	}
	if api == "" {
		api = os.Getenv("IAZIO_HARNESS_API_URL")
	}
	if api == "" {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			if b, err := os.ReadFile(filepath.Join(home, ".iazio", "api_url")); err == nil {
				api = strings.TrimSpace(string(b))
			}
		}
	}
	if api == "" {
		api = "http://localhost:8090"
	}
	if jobID == "" {
		return fmt.Errorf("job-id is required")
	}
	if chunk != "" {
		return controlplane.PostChunk(ctx, api, "", jobID, "stdout", chunk)
	}
	return executeJob(ctx, api, jobID, worktree, docsHub, kind, resumeConv, strconv.Itoa(attachToolPID))
}

// hasEngineSessionAttachError inspects engine execution log/stderr specifically for session attachment errors.
// It requires candidate lines to be anchored by an engine-emitted error prefix
// (e.g. "[error]", "error:", "fatal:", "failed to...", "cannot find...") to prevent false positives
// when task commands or tool output mention session/conversation keywords in normal output (Finding B6, F5).
func hasEngineSessionAttachError(logData []byte) bool {
	if len(logData) == 0 {
		return false
	}
	lines := strings.Split(string(logData), "\n")
	for _, raw := range lines {
		line := strings.TrimSpace(strings.ToLower(raw))
		if line == "" {
			continue
		}
		isErrorPrefix := strings.HasPrefix(line, "[error]") ||
			strings.HasPrefix(line, "error:") ||
			strings.HasPrefix(line, "fatal:") ||
			strings.HasPrefix(line, "[fatal]") ||
			strings.HasPrefix(line, "failed to attach") ||
			strings.HasPrefix(line, "failed to open session") ||
			strings.HasPrefix(line, "cannot find session") ||
			strings.HasPrefix(line, "cannot find conversation") ||
			strings.HasPrefix(line, "corrupted session") ||
			strings.HasPrefix(line, "conversation not found") ||
			strings.HasPrefix(line, "session not found") ||
			strings.HasPrefix(line, "invalid session")

		if !isErrorPrefix {
			continue
		}

		if strings.Contains(line, "conversation") ||
			strings.Contains(line, "session") {
			return true
		}
	}
	return false
}

// hasEngineQuotaExhaustedError scans engine execution logs (excluding LLM spool output)
// for explicit quota/usage exhaustion error messages (Finding M3).
func hasEngineQuotaExhaustedError(logData []byte) bool {
	if len(logData) == 0 {
		return false
	}
	lines := strings.Split(string(logData), "\n")
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		lineLower := strings.ToLower(line)
		isErrorLine := strings.HasPrefix(lineLower, "[error]") ||
			strings.HasPrefix(lineLower, "error:") ||
			strings.HasPrefix(lineLower, "fatal:") ||
			strings.HasPrefix(lineLower, "[fatal]") ||
			strings.HasPrefix(lineLower, "actionrequirederror")

		if !isErrorLine {
			continue
		}

		if strings.Contains(lineLower, "actionrequirederror") ||
			strings.Contains(lineLower, "out of usage") ||
			strings.Contains(lineLower, "quota exceeded") ||
			strings.Contains(lineLower, "rate limit exceeded") ||
			strings.Contains(lineLower, "resource_exhausted") ||
			strings.Contains(lineLower, "insufficient_quota") ||
			strings.Contains(lineLower, "switch to auto, or ask your admin to increase your limit") {
			return true
		}
	}
	return false
}

func sanitizeAlphanumeric(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func sanitizeStep(v any) string {
	if v == nil {
		return "0"
	}
	s := strings.TrimSpace(fmt.Sprintf("%v", v))
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	res := b.String()
	if res == "" {
		return "0"
	}
	return res
}

func hasRunningGitProcess(workspacePath string) bool {
	cleanWorkspace := filepath.Clean(workspacePath)

	// 1. Linux /proc scanning
	if procEntries, err := os.ReadDir("/proc"); err == nil {
		for _, entry := range procEntries {
			if !entry.IsDir() {
				continue
			}
			cmdlineBytes, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
			if err != nil || len(cmdlineBytes) == 0 {
				continue
			}
			cmdline := string(bytes.ReplaceAll(cmdlineBytes, []byte{0}, []byte(" ")))
			if strings.HasPrefix(cmdline, "git ") || strings.Contains(cmdline, "/git ") {
				if strings.Contains(cmdline, cleanWorkspace) {
					return true
				}
				if cwdLink, err := os.Readlink(filepath.Join("/proc", entry.Name(), "cwd")); err == nil {
					cleanCwd := filepath.Clean(cwdLink)
					if cleanCwd == cleanWorkspace || strings.HasPrefix(cleanCwd, cleanWorkspace+string(filepath.Separator)) {
						return true
					}
				}
			}
		}
		return false
	}

	// 2. lsof detection for macOS/Darwin (and BSD)
	if lsofBin, err := exec.LookPath("lsof"); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		out, _ := exec.CommandContext(ctx, lsofBin, "-a", "-c", "git", "+D", cleanWorkspace, "-t").Output()
		cancel()
		if len(strings.TrimSpace(string(out))) > 0 {
			return true
		}
	}

	// 3. Fallback using ps
	out, err := exec.Command("ps", "-eo", "command").Output()
	if err == nil {
		lines := strings.Split(string(out), "\n")
		for _, line := range lines {
			if strings.Contains(line, "git") && strings.Contains(line, cleanWorkspace) {
				return true
			}
		}
	}
	return false
}

func isLockFileHeld(lockPath string) bool {
	cleanPath, err := filepath.Abs(lockPath)
	if err != nil {
		cleanPath = filepath.Clean(lockPath)
	}

	// 1. Linux /proc/*/fd scanning
	procEntries, err := os.ReadDir("/proc")
	if err == nil {
		for _, entry := range procEntries {
			if !entry.IsDir() {
				continue
			}
			pidStr := entry.Name()
			if pidStr == "self" || pidStr == "thread-self" {
				continue
			}
			fdDir := filepath.Join("/proc", pidStr, "fd")
			fds, err := os.ReadDir(fdDir)
			if err != nil {
				continue
			}
			for _, fd := range fds {
				target, err := os.Readlink(filepath.Join(fdDir, fd.Name()))
				if err == nil && filepath.Clean(target) == cleanPath {
					return true
				}
			}
		}
		return false
	}

	// 2. lsof detection for macOS / Darwin / Unix
	if lsofBin, err := exec.LookPath("lsof"); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		out, _ := exec.CommandContext(ctx, lsofBin, cleanPath, "-t").Output()
		if len(strings.TrimSpace(string(out))) > 0 {
			return true
		}
		return false
	}

	// 3. Fallback: if fuser exists
	if fuserBin, err := exec.LookPath("fuser"); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		out, _ := exec.CommandContext(ctx, fuserBin, cleanPath).Output()
		if len(strings.TrimSpace(string(out))) > 0 {
			return true
		}
		return false
	}

	// Fail closed if inspection tooling is unavailable and the file exists
	if _, err := os.Stat(cleanPath); err == nil {
		return true
	}
	return false
}

func resolveGitDir(workspacePath string) (string, error) {
	gitDir := filepath.Join(workspacePath, ".git")
	fi, err := os.Stat(gitDir)
	if err != nil {
		return "", err
	}
	targetGitDir := gitDir
	if !fi.IsDir() {
		if data, err := os.ReadFile(gitDir); err == nil {
			content := strings.TrimSpace(string(data))
			if strings.HasPrefix(content, "gitdir:") {
				targetGitDir = strings.TrimSpace(strings.TrimPrefix(content, "gitdir:"))
				if !filepath.IsAbs(targetGitDir) {
					targetGitDir = filepath.Join(workspacePath, targetGitDir)
				}
			}
		}
	}
	return targetGitDir, nil
}

func hasRemainingGitLocks(workspacePath string) bool {
	targetGitDir, err := resolveGitDir(workspacePath)
	if err != nil {
		return false
	}

	rootLocks := []string{
		filepath.Join(targetGitDir, "index.lock"),
		filepath.Join(targetGitDir, "HEAD.lock"),
		filepath.Join(targetGitDir, "config.lock"),
	}
	for _, l := range rootLocks {
		if _, err := os.Stat(l); err == nil {
			return true
		}
	}

	modulesDir := filepath.Join(targetGitDir, "modules")
	var foundLock bool
	if _, err := os.Stat(modulesDir); err == nil {
		_ = filepath.Walk(modulesDir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info == nil || foundLock {
				return nil
			}
			if !info.IsDir() && strings.HasSuffix(info.Name(), ".lock") {
				foundLock = true
			}
			return nil
		})
	}
	return foundLock
}

func cleanStaleGitLocks(workspacePath string) []string {
	var removed []string
	if hasRunningGitProcess(workspacePath) {
		return removed
	}
	targetGitDir, err := resolveGitDir(workspacePath)
	if err != nil {
		return removed
	}

	rootLocks := []string{
		filepath.Join(targetGitDir, "index.lock"),
		filepath.Join(targetGitDir, "HEAD.lock"),
		filepath.Join(targetGitDir, "config.lock"),
	}
	for _, l := range rootLocks {
		if _, err := os.Stat(l); err == nil {
			if !isLockFileHeld(l) {
				if err := os.Remove(l); err == nil {
					removed = append(removed, l)
				}
			}
		}
	}

	modulesDir := filepath.Join(targetGitDir, "modules")
	if _, err := os.Stat(modulesDir); err == nil {
		_ = filepath.Walk(modulesDir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info == nil {
				return nil
			}
			if !info.IsDir() && strings.HasSuffix(info.Name(), ".lock") {
				if !isLockFileHeld(path) {
					if err := os.Remove(path); err == nil {
						removed = append(removed, path)
					}
				}
			}
			return nil
		})
	}
	return removed
}

// resolveConversationFallback discovers an active or recent conversation ID on disk or in the worktree.
func resolveConversationFallback(worktree, storyID, engineName string) string {
	if worktree == "" {
		return ""
	}
	resolver := sessionstore.DefaultResolver()
	dCtx := resolver.DetectDirtyWorktreeContext(worktree)
	if dCtx.ConversationID != "" && sessionstore.IsValidConversationID(dCtx.ConversationID) {
		return dCtx.ConversationID
	}
	if storyID != "" {
		if sess, err := resolver.ResolveForStory(worktree, storyID); err == nil && sess != nil && sessionstore.IsValidConversationID(sess.ConversationID) {
			return sess.ConversationID
		}
	}
	if sess, err := resolver.ResolveLatest(worktree); err == nil && sess != nil && sessionstore.IsValidConversationID(sess.ConversationID) {
		return sess.ConversationID
	}
	return ""
}

func resolveLiveGitBranch(workspacePath string) string {
	if workspacePath == "" {
		return ""
	}
	cmd := exec.Command("git", "-C", workspacePath, "branch", "--show-current")
	out, err := cmd.Output()
	if err == nil {
		return strings.TrimSpace(string(out))
	}
	return ""
}

func resolveGitHead(workspacePath string) string {
	if workspacePath == "" {
		return ""
	}
	cmd := exec.Command("git", "-C", workspacePath, "rev-parse", "HEAD")
	out, err := cmd.Output()
	if err == nil {
		return strings.TrimSpace(string(out))
	}
	return ""
}

func countReviewReports(workspacePath string) int {
	if workspacePath == "" {
		return 0
	}
	count := 0
	for _, dir := range []string{filepath.Join(workspacePath, "docs", "reviews"), filepath.Join(workspacePath, "tmp", "reports")} {
		entries, err := os.ReadDir(dir)
		if err == nil {
			for _, e := range entries {
				name := strings.ToLower(e.Name())
				if strings.Contains(name, "review") && (strings.HasSuffix(name, ".md") || strings.HasSuffix(name, ".json")) {
					count++
				}
			}
		}
	}
	return count
}

func resolveLiveStoryID(workspacePath string, outputText string, currentStoryID string) string {
	// 1. From output text explicit STORY_ID bindings (e.g. STORY_ID=STORY-GOV-3691 or STORY_ID: STORY-GOV-3691)
	storyRegex := regexp.MustCompile(`STORY_ID[=:\s]+(STORY-[A-Z]+-[0-9]+)`)
	if matches := storyRegex.FindAllStringSubmatch(outputText, -1); len(matches) > 0 {
		last := matches[len(matches)-1][1]
		if last != "" {
			return last
		}
	}

	// 2. From story file paths in output (e.g. docs/stories/2026-10-25-gov-3691-*.story.md)
	fileRegex := regexp.MustCompile(`docs/stories/[0-9-]+-([a-z]+)-([0-9]+)[^/\s]*\.story\.md`)
	if matches := fileRegex.FindAllStringSubmatch(outputText, -1); len(matches) > 0 {
		last := matches[len(matches)-1]
		cat := strings.ToUpper(last[1])
		num := last[2]
		return fmt.Sprintf("STORY-%s-%s", cat, num)
	}

	// 3. From .autopilot metadata on disk
	if workspacePath != "" {
		for _, rel := range []string{".autopilot/active_story.json", ".autopilot/story.json", ".autopilot/state.json"} {
			metaPath := filepath.Join(workspacePath, rel)
			if data, err := os.ReadFile(metaPath); err == nil {
				var parsed struct {
					StoryID string `json:"story_id"`
					ID      string `json:"id"`
				}
				if json.Unmarshal(data, &parsed) == nil {
					if parsed.StoryID != "" && strings.HasPrefix(parsed.StoryID, "STORY-") {
						return parsed.StoryID
					}
					if parsed.ID != "" && strings.HasPrefix(parsed.ID, "STORY-") {
						return parsed.ID
					}
				}
			}
		}
	}

	// 4. From current git branch
	branch := resolveLiveGitBranch(workspacePath)
	if branch != "" {
		branchRegex := regexp.MustCompile(`(?:chore|feat|fix|refactor)/(?:story-)?([a-z]+)-([0-9]+)`)
		if m := branchRegex.FindStringSubmatch(branch); len(m) == 3 {
			return fmt.Sprintf("STORY-%s-%s", strings.ToUpper(m[1]), m[2])
		}
		branchStoryRegex := regexp.MustCompile(`(STORY-[A-Z]+-[0-9]+)`)
		if m := branchStoryRegex.FindStringSubmatch(branch); len(m) == 2 {
			return m[1]
		}
	}

	// 5. From recently modified story file in docs/stories
	if workspacePath != "" {
		storiesDir := filepath.Join(workspacePath, "docs", "stories")
		if entries, err := os.ReadDir(storiesDir); err == nil {
			var latestStoryID string
			var latestModTime time.Time
			for _, e := range entries {
				if strings.HasSuffix(e.Name(), ".story.md") {
					info, err := e.Info()
					if err == nil && info.ModTime().After(latestModTime) {
						m := regexp.MustCompile(`[0-9-]+-([a-z]+)-([0-9]+)`).FindStringSubmatch(e.Name())
						if len(m) == 3 {
							latestModTime = info.ModTime()
							latestStoryID = fmt.Sprintf("STORY-%s-%s", strings.ToUpper(m[1]), m[2])
						}
					}
				}
			}
			if latestStoryID != "" && (currentStoryID == "" || currentStoryID == "pending_harness_creation") {
				return latestStoryID
			}
		}
	}

	if currentStoryID != "" && currentStoryID != "pending_harness_creation" {
		return currentStoryID
	}
	return ""
}


func attachRunningTool(
	ctx context.Context,
	api, token, jobID string,
	job controlplane.JobDetail,
	toolPID int,
	engineName, activeConvID, targetDir, docsHub string,
	sp *spool.Spool,
	lockHandle *repolock.LockHandle,
) (engines.ExecutionResult, error) {
	attachMsg := fmt.Sprintf("\n[ASE-SUPERVISOR] [attach_mode] iazio-harness attached to running AI engine process (ToolPID %d, engine: %s) for job %s (conversation: %s in %s)\n",
		toolPID, engineName, jobID, activeConvID, targetDir)
	_ = sp.AppendRaw("stdout", []byte(attachMsg))
	_ = controlplane.PostChunk(context.Background(), api, token, jobID, "stdout", attachMsg)
	_ = controlplane.PostTick(context.Background(), api, token, jobID, 0, os.Getpid(), toolPID)

	if lockHandle != nil {
		_ = lockHandle.Update(repolock.LockInfo{
			JobID:          job.ID,
			ScheduleID:     job.ScheduleID,
			PID:            os.Getpid(),
			HarnessPID:     os.Getpid(),
			ToolPID:        toolPID,
			Engine:         engineName,
			ConversationID: activeConvID,
			StoryID:        job.StoryID,
			Prompt:         job.Prompt,
			Status:         "RUNNING",
		})
	}

	engineLogPath := filepath.Join(spool.DefaultDir(), jobID+".engine.log")
	var logFile *os.File
	var offset int64
	if f, err := os.Open(engineLogPath); err == nil {
		logFile = f
		offset, _ = f.Seek(0, io.SeekEnd)
	}

	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	lastTickSent := time.Now()
	startTime := time.Now()
	buf := make([]byte, 32768)

	for {
		select {
		case <-ctx.Done():
			if logFile != nil {
				_ = logFile.Close()
			}
			return engines.ExecutionResult{ExitCode: 130}, ctx.Err()
		case <-ticker.C:
			if logFile == nil {
				if f, err := os.Open(engineLogPath); err == nil {
					logFile = f
					offset, _ = f.Seek(0, io.SeekEnd)
				}
			}
			if logFile != nil {
				fi, err := logFile.Stat()
				if err == nil && fi.Size() > offset {
					n, _ := logFile.ReadAt(buf, offset)
					if n > 0 {
						chunk := buf[:n]
						offset += int64(n)
						_ = sp.AppendRaw("stdout", chunk)
						_ = controlplane.PostChunk(context.Background(), api, token, jobID, "stdout", string(chunk))
						lastTickSent = time.Now()
					}
				}
			}

			now := time.Now()
			if now.Sub(lastTickSent) >= 5*time.Second {
				elapsedSec := int(time.Since(startTime).Seconds())
				_ = controlplane.PostTick(context.Background(), api, token, jobID, 0, os.Getpid(), toolPID, elapsedSec)
				lastTickSent = now
			}

			if !watchdog.IsProcessAlive(toolPID) {
				if logFile != nil {
					if fi, err := logFile.Stat(); err == nil && fi.Size() > offset {
						n, _ := logFile.ReadAt(buf, offset)
						if n > 0 {
							chunk := buf[:n]
							_ = sp.AppendRaw("stdout", chunk)
							_ = controlplane.PostChunk(context.Background(), api, token, jobID, "stdout", string(chunk))
						}
					}
					_ = logFile.Close()
				}

				exitMsg := fmt.Sprintf("\n[ASE-SUPERVISOR] [attach_mode] AI tool process (ToolPID %d) terminated. Evaluating completion gates...\n", toolPID)
				_ = sp.AppendRaw("stdout", []byte(exitMsg))
				_ = controlplane.PostChunk(context.Background(), api, token, jobID, "stdout", exitMsg)

				gateRes := gate.Verify(gate.GateOptions{
					WorktreePath: targetDir,
					StoryID:      job.StoryID,
				})
				if gateRes.Passed {
					compMsg := fmt.Sprintf("[ASE-SUPERVISOR] ASE story %s verified completed via attach supervisor!\n", job.StoryID)
					_ = sp.AppendRaw("stdout", []byte(compMsg))
					_ = controlplane.PostChunk(context.Background(), api, token, jobID, "stdout", compMsg)
					return engines.ExecutionResult{ExitCode: 0}, nil
				}
				return engines.ExecutionResult{ExitCode: 1}, fmt.Errorf("tool process %d exited with incomplete gates: %v", toolPID, gateRes.UnmetGates)
			}
		}
	}
}

func executeJob(ctx context.Context, api, jobID, worktree, docsHub string, overrides ...string) error {
	var spanOpts = []telemetry.SpanOption{
		telemetry.WithKind("INTERNAL"),
		telemetry.WithTag("cli.command", "run"),
		telemetry.WithTag("iazio.job_id", jobID),
	}
	ctx, span := telemetry.StartSpan(ctx, "harness.execute_job", spanOpts...)
	defer span.End()

	var kindOverride, resumeConvOverride string
	var attachToolPID int
	if len(overrides) > 0 {
		kindOverride = overrides[0]
	}
	if len(overrides) > 1 {
		resumeConvOverride = overrides[1]
	}
	if len(overrides) > 2 && overrides[2] != "" {
		attachToolPID, _ = strconv.Atoi(overrides[2])
	}
	token, _ := auth.Bearer(ctx)
	job, err := controlplane.GetJob(ctx, api, token, jobID)
	if err != nil {
		_ = controlplane.PostChunk(context.Background(), api, token, jobID, "stderr", fmt.Sprintf("[iazio-harness] Pre-execution failure: get job %s: %v\n", jobID, err))
		_ = controlplane.PostExit(context.Background(), api, token, jobID, 1)
		return fmt.Errorf("get job: %w", err)
	}
	isControlPlaneResume := (job.ParentJobID != "" || job.RootJobID != "")
	if kindOverride != "" {
		job.Kind = kindOverride
	}
	if resumeConvOverride != "" {
		if isControlPlaneResume {
			// For Control Plane resumes, the control plane job record is authoritative (Finding L1)
			if (job.ResumeConversationID != "" && resumeConvOverride != job.ResumeConversationID) ||
				(job.ActiveConversationID != "" && resumeConvOverride != job.ActiveConversationID) {
				log.Printf("[harness] [job.resume.identity_mismatch] ignoring argv override %s, adhering to control plane resume identity %s", resumeConvOverride, job.ResumeConversationID)
			} else if job.ResumeConversationID == "" && job.ActiveConversationID == "" {
				log.Printf("[harness] ignoring --resume-conversation=%s override for control plane resume with empty parent conversation", resumeConvOverride)
			} else {
				job.ResumeConversationID = resumeConvOverride
			}
		} else {
			job.ResumeConversationID = resumeConvOverride
		}
	}
	if worktree == "" {
		worktree = job.WorktreePath
	}
	if docsHub == "" {
		docsHub = job.DocsHubPath
	}
	span.SetTag("iazio.story_id", job.StoryID)
	span.SetTag("iazio.kind", job.Kind)
	span.SetTag("iazio.worktree", worktree)
	span.SetTag("iazio.engine", job.Engine)
	span.SetTag("iazio.host_id", job.HostID)
	span.SetTag("iazio.schedule_id", job.ScheduleID)
	targetDir, err := engines.CheckoutDir(job.Kind, worktree, docsHub)
	if err != nil {
		_ = controlplane.PostChunk(context.Background(), api, token, jobID, "stderr", fmt.Sprintf("[iazio-harness] Pre-execution failure: checkout dir %s: %v\n", targetDir, err))
		_ = controlplane.PostExit(context.Background(), api, token, jobID, 1)
		return err
	}

	wd := watchdog.New(watchdog.Options{})
	wd.Track(watchdog.TrackedJob{
		JobID:        job.ID,
		ScheduleID:   job.ScheduleID,
		WorktreePath: targetDir,
		DocsHubPath:  docsHub,
		APIURL:       api,
		Engine:       job.Engine,
		StoryID:      job.StoryID,
		PID:          os.Getpid(),
		Status:       watchdog.StatusRunning,
	})

	cleanedLocks := cleanStaleGitLocks(targetDir)
	cleanLocksStatus := !hasRunningGitProcess(targetDir) && !hasRemainingGitLocks(targetDir)
	manifest := map[string]any{
		"clean_locks":         cleanLocksStatus,
		"stale_locks_removed": len(cleanedLocks),
	}
	_ = controlplane.PostJobReadiness(context.Background(), api, token, jobID, nil, manifest, nil)
	if len(cleanedLocks) > 0 {
		log.Printf("[harness] Pre-execution workspace check: removed %d stale git lock files in %s", len(cleanedLocks), targetDir)
	}

	lock, err := engines.Hold(targetDir, "")
	if err != nil {
		_ = controlplane.PostChunk(context.Background(), api, token, jobID, "stderr", fmt.Sprintf("[iazio-harness] Pre-execution failure: checkout lock on %s: %v\n", targetDir, err))
		_ = controlplane.PostExit(context.Background(), api, token, jobID, 1)
		return fmt.Errorf("checkout lock: %w", err)
	}
	defer lock.Release()

	storyID := job.StoryID
	if strings.TrimSpace(storyID) == "" || strings.EqualFold(storyID, fallbackResumeStoryID) || strings.EqualFold(storyID, "resume complete current ase flow") {
		dCtx := sessionstore.DefaultResolver().DetectDirtyWorktreeContext(worktree)
		if dCtx.StoryID != "" {
			storyID = dCtx.StoryID
		}
	}
	if strings.TrimSpace(storyID) == "" {
		storyID = fallbackResumeStoryID
	}

	prepRes, err := prompt.Prepare(prompt.Input{
		Kind:       job.Kind,
		JobID:      job.ID,
		Body:       job.Prompt,
		Transcript: job.Transcript,
		SpoolDir:   spool.DefaultDir(),
		Values: prompt.Values{
			StoryID:        storyID,
			TargetLeafName: filepath.Base(worktree),
			WorktreePath:   worktree,
			DocsHubPath:    docsHub,
		},
	})
	if err != nil {
		_ = controlplane.PostChunk(context.Background(), api, token, jobID, "stderr", fmt.Sprintf("[iazio-harness] Pre-execution failure: prepare prompt for job %s: %v\n", jobID, err))
		_ = controlplane.PostExit(context.Background(), api, token, jobID, 1)
		return fmt.Errorf("prepare prompt: %w", err)
	}

	sp, err := spool.Open(spool.DefaultDir(), jobID)
	if err != nil {
		_ = controlplane.PostChunk(context.Background(), api, token, jobID, "stderr", fmt.Sprintf("[iazio-harness] Pre-execution failure: open spool for job %s: %v\n", jobID, err))
		_ = controlplane.PostExit(context.Background(), api, token, jobID, 1)
		return fmt.Errorf("open spool: %w", err)
	}
	defer sp.Close()

	engineName := job.Engine
	if engineName == "" {
		engineName = "agy"
	}

	isPreflightFallback := false
	preflightFallbackReason := ""

	resumeConvID := job.ActiveConversationID
	if resumeConvID == "" && job.ResumeMode != "fallback_new_conversation" {
		resumeConvID = job.ResumeConversationID
	}
	if resumeConvID == "" && job.ResumeMode != "fallback_new_conversation" {
		resumeConvID = job.InitialConversationID
	}
	if resumeConvID == "" && job.ResumeMode != "fallback_new_conversation" && job.EnvVars != nil {
		resumeConvID = job.EnvVars["RESUME_CONVERSATION_ID"]
	}
	if resumeConvID == "" && job.ResumeMode != "fallback_new_conversation" && len(job.ConversationIDs) > 0 {
		resumeConvID = job.ConversationIDs[0]
	}
	if job.ResumeMode == "fallback_new_conversation" && job.ActiveConversationID == "" {
		isPreflightFallback = true
		preflightFallbackReason = job.ResumeFallbackReason
		if preflightFallbackReason == "" {
			preflightFallbackReason = "control plane initiated fallback to new conversation"
		}
		log.Printf("[harness] [job.resume.conversation_fallback] resume_mode=fallback_new_conversation reason=%q", preflightFallbackReason)
	}
	if resumeConvID == "" && !isControlPlaneResume {
		resumeConvID = resolveConversationFallback(worktree, job.StoryID, engineName)
		if resumeConvID != "" {
			log.Printf("[harness] [ASE-SUPERVISOR] Discovered prior conversation %s (story: %s, engine: %s) for worktree %s", resumeConvID, job.StoryID, engineName, worktree)
		}
	}
	lockHandle, lockErr := repolock.Acquire(worktree, repolock.LockInfo{
		JobID:          job.ID,
		ScheduleID:     job.ScheduleID,
		PID:            os.Getpid(),
		HarnessPID:     os.Getpid(),
		ToolPID:        attachToolPID,
		Engine:         engineName,
		ConversationID: resumeConvID,
		StoryID:        job.StoryID,
		Prompt:         job.Prompt,
		Status:         "RUNNING",
	}, func(pid int) bool {
		return watchdog.IsProcessAlive(pid)
	})
	if lockErr != nil {
		_ = sp.AppendRaw("stderr", []byte(fmt.Sprintf("[iazio-harness] Repository lock failure: %v\n", lockErr)))
		_ = controlplane.PostExit(context.Background(), api, token, jobID, 1)
		return fmt.Errorf("repolock: %w", lockErr)
	}
	defer lockHandle.Release()

	if attachToolPID <= 0 && lockHandle.Info().ToolPID > 0 && watchdog.IsProcessAlive(lockHandle.Info().ToolPID) {
		attachToolPID = lockHandle.Info().ToolPID
		log.Printf("[harness] [ASE-SUPERVISOR] Discovered active tool process (ToolPID: %d) from existing lock on %s", attachToolPID, worktree)
	}

	if !isControlPlaneResume && resumeConvID == "" && lockHandle.Info().ConversationID != "" && (job.Kind == "resume" || (job.StoryID != "" && lockHandle.Info().StoryID == job.StoryID && lockHandle.Info().Status != "COMPLETED")) {
		resumeConvID = lockHandle.Info().ConversationID
		log.Printf("[harness] Adopted prior conversation %s from repo lock for %s", resumeConvID, worktree)
	}

	attemptedResumeConv := resumeConvID
	if resumeConvID != "" {
		if !sessionstore.IsValidConversationID(resumeConvID) {
			isPreflightFallback = true
			preflightFallbackReason = fmt.Sprintf("invalid initial conversation id format: %s", resumeConvID)
			log.Printf("[harness] [job.resume.conversation_fallback] initial_conversation_id=%s reason=%q", resumeConvID, preflightFallbackReason)
			resumeConvID = ""
		} else if engineName == "agy" && !sessionstore.DefaultResolver().SessionExists(resumeConvID) {
			// C6: SessionExists is advisory; do not abort attach since agy state may reside in non-standard/relocated data dirs.
			// Stage 2 fallback handles real attachment failures safely.
			log.Printf("[harness] advisory: initial conversation %s not found in standard brain directories; attempting attach with engine %s anyway (Stage 2 fallback armed)", resumeConvID, engineName)
		}
	}

	var preflightRehydrationStatus map[string]any
	if isPreflightFallback && attemptedResumeConv != "" {
		bundle, bundleErr := controlplane.GetRehydrationBundle(ctx, api, token, attemptedResumeConv)
		if bundleErr != nil {
			warnMsg := fmt.Sprintf("[iazio-harness] Warning: Failed to fetch canonical rehydration bundle for %s: %v\n", attemptedResumeConv, bundleErr)
			_ = sp.AppendRaw("stderr", []byte(warnMsg))
			_ = controlplane.PostChunk(context.Background(), api, token, jobID, "stderr", warnMsg)
			preflightRehydrationStatus = map[string]any{
				"hydrated":               false,
				"source_conversation_id": attemptedResumeConv,
				"error":                  bundleErr.Error(),
			}
		} else if bundle.BootstrapPrompt != "" {
			prepRes.Prompt = bundle.BootstrapPrompt + "\n\n" + prepRes.Prompt
			diagMsg := fmt.Sprintf("[iazio-harness] Injected canonical rehydration bundle from %s (interrupted phase: %s)\n",
				attemptedResumeConv, bundle.InterruptedPhase)
			_ = sp.AppendRaw("stdout", []byte(diagMsg))
			preflightRehydrationStatus = map[string]any{
				"hydrated":                true,
				"source_conversation_id":  attemptedResumeConv,
				"turns_count":             len(bundle.LastToolActions),
				"last_tool_actions_count": len(bundle.LastToolActions),
				"ase_phase":               bundle.InterruptedPhase,
			}
		} else {
			preflightRehydrationStatus = map[string]any{
				"hydrated":               false,
				"source_conversation_id": attemptedResumeConv,
				"error":                  "empty rehydration bundle",
			}
		}
		if preflightRehydrationStatus != nil {
			_ = controlplane.PostJobReadiness(context.Background(), api, token, jobID, nil, nil, preflightRehydrationStatus)
		}
	}

	if isPreflightFallback && job.InterruptedASEState != nil && len(job.InterruptedASEState) > 0 {
		phase, _ := job.InterruptedASEState["phase"].(string)
		phaseSource, _ := job.InterruptedASEState["phase_source"].(string)
		if phase != "" && phase != "UNKNOWN" && phaseSource != "default" {
			phaseText := sanitizeAlphanumeric(phase)
			if phaseSource == "repo_halted_dirty" {
				phaseText += " [inferred from uncommitted changes]"
			}
			resumeDirective := fmt.Sprintf("\n[ASE RESUME DIRECTIVE: Prior execution on conversation %s interrupted. Fallback to new conversation. Interrupted State: phase=%s, step=%s, story_id=%s, gate=%s. Resume execution from this checkpoint.]\n\n",
				sanitizeAlphanumeric(attemptedResumeConv),
				phaseText,
				sanitizeStep(job.InterruptedASEState["step"]),
				sanitizeAlphanumeric(fmt.Sprintf("%v", job.InterruptedASEState["story_id"])),
				sanitizeAlphanumeric(fmt.Sprintf("%v", job.InterruptedASEState["last_completed_gate"])),
			)
			prepRes.Prompt = resumeDirective + prepRes.Prompt
		}
	}

	var activePID int
	activeConvID := resumeConvID

	// Immediately post startup diagnostic chunk so UI never sits on blank "Waiting for output..."
	_ = sp.AppendRaw("stdout", []byte(fmt.Sprintf("[ASE-SUPERVISOR] Initializing execution for job %s on %s (engine: %s, kind: %s)\n", jobID, worktree, engineName, job.Kind)))
	if resumeConvID != "" {
		_ = sp.AppendRaw("stdout", []byte(fmt.Sprintf("[ASE-SUPERVISOR] Conversation resolved: %s --conversation=%s (source: sessionstore/metadata)\n", engineName, resumeConvID)))
	} else {
		_ = sp.AppendRaw("stdout", []byte(fmt.Sprintf("[ASE-SUPERVISOR] Starting new conversation session for story %s...\n", storyID)))
	}
	initAsePhase := "START"
	if job.InterruptedASEState != nil {
		if p, ok := job.InterruptedASEState["phase"].(string); ok && p != "" {
			initAsePhase = p
		}
	}
	_ = sp.AppendRaw("stdout", []byte(fmt.Sprintf("[ASE-SUPERVISOR] ASE State: story=%s, phase=%s\n", storyID, initAsePhase)))
	initDirtySummary := "clean"
	if dFiles := collectWorkspaceDirtyFiles(worktree); len(dFiles) > 0 {
		initDirtySummary = fmt.Sprintf("dirty (%d files modified/untracked)", len(dFiles))
	}
	_ = sp.AppendRaw("stdout", []byte(fmt.Sprintf("[ASE-SUPERVISOR] Fleet Git State: %s\n", initDirtySummary)))
	_ = checkpoint.Save(worktree, spool.DefaultDir(), checkpoint.State{
		JobID:          job.ID,
		ScheduleID:     job.ScheduleID,
		Engine:         engineName,
		WorktreePath:   worktree,
		ConversationID: activeConvID,
		StoryID:        job.StoryID,
		StepIndex:      0,
		StartedAt:      time.Now().UTC().Format(time.RFC3339),
		Status:         "RUNNING",
	})

	startTime := time.Now()
	flushCtx, stopFlush := context.WithCancel(ctx)
	defer stopFlush()

	currentBoundStoryID := job.StoryID
	if currentBoundStoryID == "" || currentBoundStoryID == "pending_harness_creation" {
		if initResolved := resolveLiveStoryID(worktree, prepRes.Prompt, currentBoundStoryID); initResolved != "" && initResolved != "pending_harness_creation" {
			currentBoundStoryID = initResolved
			job.StoryID = initResolved
		}
	}

	go func() {
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()
		var lastTickSent time.Time
		var lastSilenceWarning time.Time
		for {
			select {
			case <-flushCtx.Done():
				return
			case <-ticker.C:
				events := sp.Flush()
				now := time.Now()
				for _, ev := range events {
					if ev.Type == spool.EventOutputChunk {
						if err := controlplane.PostChunk(context.Background(), api, token, jobID, ev.Stream, ev.Text); err != nil {
							log.Printf("[harness] post chunk failed for %s: %v", jobID, err)
						} else {
							lastTickSent = now
						}
					} else if ev.Type == spool.EventOutputTick {
						if now.Sub(lastTickSent) >= 5*time.Second {
							elapsedSec := int(time.Since(startTime).Seconds())
							if err := controlplane.PostTick(context.Background(), api, token, jobID, ev.SilentForMs, os.Getpid(), activePID, elapsedSec); err != nil {
								log.Printf("[harness] post tick failed for %s: %v", jobID, err)
							} else {
								lastTickSent = now
							}
						}
						// Progress-aware liveness: check if engine has active child processes
						childProcs, _ := watchdog.GetDescendantProcesses(activePID)
						if len(childProcs) > 0 {
							if now.Sub(lastSilenceWarning) >= 30*time.Second {
								lastSilenceWarning = now
								elapsedStr := time.Since(startTime).Truncate(time.Second).String()
								summary, pids := watchdog.FormatActiveChildProcessesSummary(childProcs)
								diag := fmt.Sprintf("[iazio-harness] Execution active (job %s, elapsed: %s); engine waiting on active child task(s): %s (pids: %v)\n",
									jobID, elapsedStr, summary, pids)
								_ = controlplane.PostChunkWithTelemetry(context.Background(), api, token, jobID, "stdout", diag, summary)
							}
						} else if ev.SilentForMs >= 30000 && now.Sub(lastSilenceWarning) >= 30*time.Second {
							lastSilenceWarning = now
							harnessPID := os.Getpid()
							elapsedStr := time.Since(startTime).Truncate(time.Second).String()
							diag := fmt.Sprintf("[iazio-harness] Execution active (job %s, engine: %s, harness_pid: %d, tool_pid: %d, elapsed: %s); silent for %ds (waiting for model output/response)...\n",
								jobID, engineName, harnessPID, activePID, elapsedStr, ev.SilentForMs/1000)
							_ = controlplane.PostChunk(context.Background(), api, token, jobID, "stdout", diag)
						}

						// Dynamic Story ID & Branch sync
						if resolved := resolveLiveStoryID(worktree, "", currentBoundStoryID); resolved != "" && resolved != "pending_harness_creation" && resolved != currentBoundStoryID {
							prevStory := currentBoundStoryID
							currentBoundStoryID = resolved
							job.StoryID = resolved
							_ = lockHandle.Update(repolock.LockInfo{
								JobID:          job.ID,
								ScheduleID:     job.ScheduleID,
								PID:            os.Getpid(),
								HarnessPID:     os.Getpid(),
								ToolPID:        activePID,
								Engine:         engineName,
								ConversationID: activeConvID,
								StoryID:        resolved,
								Prompt:         job.Prompt,
								Status:         "RUNNING",
							})
							_ = controlplane.PostStoryBinding(context.Background(), api, token, jobID, resolved, "", "")
							bindMsg := fmt.Sprintf("[iazio-harness] Bound active story ID: %s (branch: %s, previously: %s)\n",
								resolved, resolveLiveGitBranch(worktree), prevStory)
							_ = sp.AppendRaw("stdout", []byte(bindMsg))
							_ = controlplane.PostChunk(context.Background(), api, token, jobID, "stdout", bindMsg)
						}
					}
				}
			}
		}
	}()

	printTimeout := "0"
	if job.MaxExecutionDurationSeconds > 0 && os.Getenv("IAZIO_ENFORCE_MAX_EXEC_TIMEOUT") == "1" {
		printTimeout = fmt.Sprintf("%ds", job.MaxExecutionDurationSeconds)
	}
	if envTimeout := os.Getenv("IAZIO_PRINT_TIMEOUT"); envTimeout != "" {
		printTimeout = envTimeout
	}
	modelName, agentName, disableSlashCommands := resolveExecutionOptions(job, prepRes.Prompt)
	runner := engines.CLI{Engine: engineName}

	initialBanner := fmt.Sprintf("\n[ASE-SUPERVISOR] Starting execution for job %s (kind: %s, engine: %s, worktree: %s)\n"+
		"[ASE-SUPERVISOR] Executed Prompt: %q\n",
		jobID, job.Kind, engineName, worktree, prepRes.Prompt)
	if resumeConvID != "" {
		initialBanner += fmt.Sprintf("[ASE-SUPERVISOR] Resuming Conversation: %s --conversation=%s\n", engineName, resumeConvID)
	}
	if job.ParentJobID != "" {
		initialBanner += fmt.Sprintf("[ASE-SUPERVISOR] Lineage: parent_job=%s, root_job=%s\n", job.ParentJobID, job.RootJobID)
	}
	_ = sp.AppendRaw("stdout", []byte(initialBanner))
	_ = controlplane.PostChunk(context.Background(), api, token, jobID, "stdout", initialBanner)

	var convWg sync.WaitGroup
	var convMu sync.Mutex
	var confirmedConvID string
	var execReq engines.ExecutionRequest

	execReq = engines.ExecutionRequest{
		Engine:                     engineName,
		Kind:                       job.Kind,
		StoryID:                    sessionstore.ExtractStoryID(storyID),
		Prompt:                     prepRes.Prompt,
		WorktreePath:               worktree,
		DocsHubPath:                docsHub,
		PrintTimeout:               printTimeout,
		Model:                      modelName,
		Agent:                      agentName,
		DisableSlashCommands:       disableSlashCommands,
		DangerouslySkipPermissions: true,
		ScheduleEnv:                job.EnvVars,
		LogFile:                    filepath.Join(spool.DefaultDir(), jobID+".engine.log"),
		ResumeConversationID:       resumeConvID,
		OnPID: func(pid int) {
			activePID = pid
			span.AddAnnotation(fmt.Sprintf("engine_process_started:pid=%d", pid))
			wd.Track(watchdog.TrackedJob{
				JobID:          job.ID,
				ScheduleID:     job.ScheduleID,
				WorktreePath:   targetDir,
				DocsHubPath:    docsHub,
				APIURL:         api,
				Engine:         engineName,
				StoryID:        job.StoryID,
				ConversationID: activeConvID,
				PID:            pid,
				HarnessPID:     os.Getpid(),
				ToolPID:        pid,
				Status:         watchdog.StatusRunning,
			})
			_ = lockHandle.Update(repolock.LockInfo{
				JobID:          job.ID,
				ScheduleID:     job.ScheduleID,
				PID:            os.Getpid(),
				HarnessPID:     os.Getpid(),
				ToolPID:        pid,
				Engine:         engineName,
				ConversationID: activeConvID,
				StoryID:        job.StoryID,
				Prompt:         job.Prompt,
				Status:         "RUNNING",
			})
			_ = controlplane.PostTick(ctx, api, token, job.ID, 0, os.Getpid(), pid)
			_ = checkpoint.Save(worktree, spool.DefaultDir(), checkpoint.State{
				JobID:          job.ID,
				ScheduleID:     job.ScheduleID,
				Engine:         engineName,
				WorktreePath:   worktree,
				ConversationID: activeConvID,
				StoryID:        job.StoryID,
				StepIndex:      1,
				PID:            pid,
				StartedAt:      time.Now().UTC().Format(time.RFC3339),
				Status:         "RUNNING",
			})
		},
		OnConversationID: func(cid string) {
			convMu.Lock()
			defer convMu.Unlock()

			if cid == "" || !sessionstore.IsValidConversationID(cid) {
				return
			}

			// Strict pinning: once a conversation ID has been confirmed and pinned for this job execution,
			// any differing subsequent conversation ID is an unpinned tertiary leak and must be rejected!
			if confirmedConvID != "" {
				if cid != confirmedConvID {
					log.Printf("[harness] ignoring unpinned tertiary conversation %s; keeping active conversation %s pinned for job %s", cid, confirmedConvID, jobID)
					convWg.Add(1)
					go func(droppedID, pinnedID string) {
						defer convWg.Done()
						_ = controlplane.PostConversationWithOptions(context.Background(), api, token, jobID, droppedID, controlplane.PostConversationOptions{
							Role:           "dropped_unpinned",
							FallbackReason: fmt.Sprintf("rejected tertiary conversation %s; active conversation %s is already pinned", droppedID, pinnedID),
						})
					}(cid, confirmedConvID)
				}
				return
			}

			isFallback := false
			fallbackReason := ""
			role := "initial_active"
			if resumeConvID != "" {
				if cid == resumeConvID {
					role = "initial_resumed"
				} else {
					isFallback = true
					role = "fallback_primary"
					fallbackReason = fmt.Sprintf("failed to attach to initial conversation %s; started new conversation %s", resumeConvID, cid)
					log.Printf("[harness] [job.resume.conversation_fallback] initial_conversation_id=%s active_conversation_id=%s reason=%q", resumeConvID, cid, fallbackReason)
				}
			} else if isPreflightFallback {
				isFallback = true
				role = "fallback_primary"
				fallbackReason = preflightFallbackReason
				log.Printf("[harness] [job.resume.conversation_fallback] initial_conversation_id=%s active_conversation_id=%s reason=%q", job.InitialConversationID, cid, fallbackReason)
			}

			confirmedConvID = cid
			activeConvID = cid

			span.AddAnnotation(fmt.Sprintf("conversation_detected:%s:fallback=%v:role=%s", cid, isFallback, role))
			_ = lockHandle.Update(repolock.LockInfo{
				JobID:          job.ID,
				ScheduleID:     job.ScheduleID,
				PID:            os.Getpid(),
				HarnessPID:     os.Getpid(),
				ToolPID:        activePID,
				Engine:         engineName,
				ConversationID: cid,
				StoryID:        job.StoryID,
				Prompt:         job.Prompt,
				Status:         "RUNNING",
			})
			_ = checkpoint.Save(worktree, spool.DefaultDir(), checkpoint.State{
				JobID:          job.ID,
				ScheduleID:     job.ScheduleID,
				Engine:         engineName,
				WorktreePath:   worktree,
				ConversationID: cid,
				StoryID:        job.StoryID,
				StepIndex:      2,
				PID:            activePID,
				StartedAt:      time.Now().UTC().Format(time.RFC3339),
				Status:         "RUNNING",
			})
			convWg.Add(1)
			asePhase := ""
			if job.InterruptedASEState != nil {
				if p, ok := job.InterruptedASEState["phase"].(string); ok {
					asePhase = p
				}
			}
			go func(c string, fb bool, fbReason, r, phase string) {
				defer convWg.Done()
				var err error
				actualCLIArgs := ""
				if !fb && resumeConvID != "" && c == resumeConvID {
					if engineName == "agent" {
						actualCLIArgs = fmt.Sprintf("--resume %s", c)
					} else {
						actualCLIArgs = fmt.Sprintf("--conversation=%s", c)
					}
				}
				opt := controlplane.PostConversationOptions{
					Fallback:          fb,
					FallbackReason:    fbReason,
					Role:              r,
					CLIArgs:           actualCLIArgs,
					ASEPhase:          phase,
					RehydrationStatus: preflightRehydrationStatus,
				}
				for attempt := 1; attempt <= 10; attempt++ {
					err = controlplane.PostConversationWithOptions(context.Background(), api, token, jobID, c, opt)
					if err == nil {
						return
					}
					time.Sleep(time.Duration(attempt*100) * time.Millisecond)
				}
				if err != nil {
					fatalMsg := fmt.Sprintf("[iazio-harness] FATAL: failed to register conversation ID %s for job %s with control plane after 10 attempts: %v\n", c, jobID, err)
					log.Printf("%s", fatalMsg)
					_ = sp.AppendRaw("stderr", []byte(fatalMsg))
					_ = controlplane.PostChunk(context.Background(), api, token, jobID, "stderr", fatalMsg)
				}
			}(cid, isFallback, fallbackReason, role, asePhase)
		},
	}
	var execRes engines.ExecutionResult
	var execErr error
	if attachToolPID > 0 && watchdog.IsProcessAlive(attachToolPID) {
		activePID = attachToolPID
		execRes, execErr = attachRunningTool(ctx, api, token, jobID, job, attachToolPID, engineName, activeConvID, targetDir, docsHub, sp, lockHandle)
	} else {
		execRes, execErr = runner.Execute(ctx, execReq, sp)
	}
	span.AddAnnotation(fmt.Sprintf("engine_process_exited:code=%d", execRes.ExitCode))
	stopFlush()

	// Wait for any in-flight conversation ID registrations with a bounded timeout
	convDone := make(chan struct{})
	go func() {
		convWg.Wait()
		close(convDone)
	}()
	select {
	case <-convDone:
	case <-time.After(3 * time.Second):
	}

	// Final spool flush
	for _, ev := range sp.Flush() {
		if ev.Type == spool.EventOutputChunk {
			_ = controlplane.PostChunk(context.Background(), api, token, jobID, ev.Stream, ev.Text)
		}
	}

	// Execution Diagnosis & Anomaly Detection
	_ = sp.Sync()
	engineLogData, _ := os.ReadFile(execReq.LogFile)
	stderrLogData, _ := os.ReadFile(strings.TrimSuffix(execReq.LogFile, ".log") + ".stderr.log")
	combinedEngineLog := append(append([]byte(nil), engineLogData...), stderrLogData...)
	engineLogStr := string(combinedEngineLog)
	spoolLogData, _ := os.ReadFile(filepath.Join(spool.DefaultDir(), job.ID+".log"))
	combinedOutput := engineLogStr + "\n" + string(spoolLogData)

	diagReport := diagnostics.Diagnose(diagnostics.DiagnosticInput{
		JobID:        job.ID,
		StoryID:      job.StoryID,
		WorktreePath: worktree,
		ExitCode:     execRes.ExitCode,
		OutputText:   engineLogStr,
	})

	convMu.Lock()
	sessionConfirmed := (confirmedConvID == resumeConvID)
	alreadyFellBack := (confirmedConvID != "" && confirmedConvID != resumeConvID)
	convMu.Unlock()

	// Inspect engine execution log/stderr specifically (never LLM agent output or spool) for session attach errors (Finding B6, F5, H1)
	hasEngineSessionError := hasEngineSessionAttachError(combinedEngineLog)

	// Inspect engine execution log/stderr specifically (never LLM agent output or spool) for quota exhaustion errors (Finding M3, H1)
	isQuotaExhausted := hasEngineQuotaExhaustedError(combinedEngineLog)
	if !isQuotaExhausted {
		for _, f := range diagReport.Findings {
			if f.Code == diagnostics.CodeQuotaExhausted {
				isQuotaExhausted = true
				break
			}
		}
	}

	// Stage 2 fallback ONLY triggers if the engine actively failed to attach to resumeConvID.
	// Normal task failures (lint, tests, build) inside the session must NEVER trigger Stage 2 fallback (F1, F2).
	// If run 1 already fell back and pinned a new conversation (alreadyFellBack), Stage 2 must NOT run again (F1).
	isSessionAttachFailure := !sessionConfirmed && !alreadyFellBack && hasEngineSessionError && !isQuotaExhausted

	// Check for conversation resume failure auto-fallback (Stage 2 Fallback)
	// ONLY trigger if session could not be attached or unrecoverable session error occurred (F2)
	if resumeConvID != "" && execRes.ExitCode != 0 && ctx.Err() == nil && isSessionAttachFailure {
		fallbackReason := fmt.Sprintf("tool execution failed to resume on conversation %s (exit code %d)", resumeConvID, execRes.ExitCode)
		fallbackMsg := fmt.Sprintf("[iazio-harness] [job.resume.conversation_fallback] %s; activating Stage 2 fallback: starting new conversation...\n", fallbackReason)
		_ = sp.AppendRaw("stdout", []byte(fallbackMsg))
		_ = controlplane.PostChunk(context.Background(), api, token, jobID, "stdout", fallbackMsg)

		fallbackReq := execReq
		fallbackReq.ResumeConversationID = "" // clear to start a new conversation
		fallbackReq.Argv = nil

		// Reset confirmed conversation so the new fallback conversation can be pinned
		convMu.Lock()
		confirmedConvID = ""
		isPreflightFallback = true
		preflightFallbackReason = fallbackReason
		convMu.Unlock()

		// Fetch Canonical Rehydration Bundle from control plane
		var stage2RehydrationStatus map[string]any
		if resumeConvID != "" {
			bundle, bundleErr := controlplane.GetRehydrationBundle(ctx, api, token, resumeConvID)
			if bundleErr != nil {
				warnMsg := fmt.Sprintf("[iazio-harness] Warning: Failed to fetch canonical rehydration bundle for %s: %v\n", resumeConvID, bundleErr)
				_ = sp.AppendRaw("stderr", []byte(warnMsg))
				_ = controlplane.PostChunk(context.Background(), api, token, jobID, "stderr", warnMsg)
				stage2RehydrationStatus = map[string]any{
					"hydrated":               false,
					"source_conversation_id": resumeConvID,
					"error":                  bundleErr.Error(),
				}
			} else if bundle.BootstrapPrompt != "" {
				fallbackReq.Prompt = bundle.BootstrapPrompt + "\n\n" + fallbackReq.Prompt
				diagMsg := fmt.Sprintf("[iazio-harness] Injected canonical rehydration bundle from %s into fallback conversation (interrupted phase: %s, completed phases: %v)\n",
					resumeConvID, bundle.InterruptedPhase, bundle.CompletedPhases)
				_ = sp.AppendRaw("stdout", []byte(diagMsg))
				_ = controlplane.PostChunk(context.Background(), api, token, jobID, "stdout", diagMsg)
				stage2RehydrationStatus = map[string]any{
					"hydrated":                true,
					"source_conversation_id":  resumeConvID,
					"turns_count":             len(bundle.LastToolActions),
					"last_tool_actions_count": len(bundle.LastToolActions),
					"ase_phase":               bundle.InterruptedPhase,
				}
			} else {
				stage2RehydrationStatus = map[string]any{
					"hydrated":               false,
					"source_conversation_id": resumeConvID,
					"error":                  "empty rehydration bundle",
				}
			}
			if stage2RehydrationStatus != nil {
				preflightRehydrationStatus = stage2RehydrationStatus
				_ = controlplane.PostJobReadiness(context.Background(), api, token, jobID, nil, nil, stage2RehydrationStatus)
			}
		}

		if job.InterruptedASEState != nil && len(job.InterruptedASEState) > 0 {
			phase, _ := job.InterruptedASEState["phase"].(string)
			phaseSource, _ := job.InterruptedASEState["phase_source"].(string)
			if phase != "" && phase != "UNKNOWN" && phaseSource != "default" {
				phaseText := sanitizeAlphanumeric(phase)
				if phaseSource == "repo_halted_dirty" {
					phaseText += " [inferred from uncommitted changes]"
				}
				resumeDirective := fmt.Sprintf("\n[ASE RESUME DIRECTIVE: Prior execution on conversation %s failed with exit code %d. Fallback to new conversation. Interrupted State: phase=%s, step=%s, story_id=%s, gate=%s. Resume execution from this checkpoint.]\n\n",
					sanitizeAlphanumeric(resumeConvID),
					execRes.ExitCode,
					phaseText,
					sanitizeStep(job.InterruptedASEState["step"]),
					sanitizeAlphanumeric(fmt.Sprintf("%v", job.InterruptedASEState["story_id"])),
					sanitizeAlphanumeric(fmt.Sprintf("%v", job.InterruptedASEState["last_completed_gate"])),
				)
				fallbackReq.Prompt = resumeDirective + fallbackReq.Prompt
			}
		}

		execRes, execErr = runner.Execute(ctx, fallbackReq, sp)

		// Re-flush spool after fallback
		for _, ev := range sp.Flush() {
			if ev.Type == spool.EventOutputChunk {
				_ = controlplane.PostChunk(context.Background(), api, token, jobID, ev.Stream, ev.Text)
			}
		}

		// Wait for any in-flight conversation registration from the fallback run
		fallbackConvDone := make(chan struct{})
		go func() {
			convWg.Wait()
			close(fallbackConvDone)
		}()
		select {
		case <-fallbackConvDone:
		case <-time.After(3 * time.Second):
		}

		// Re-diagnose after fallback run
		_ = sp.Sync()
		engineLogData, _ = os.ReadFile(fallbackReq.LogFile)
		stderrLogData, _ = os.ReadFile(strings.TrimSuffix(fallbackReq.LogFile, ".log") + ".stderr.log")
		combinedEngineLog = append(append([]byte(nil), engineLogData...), stderrLogData...)
		engineLogStr = string(combinedEngineLog)
		spoolLogData, _ = os.ReadFile(filepath.Join(spool.DefaultDir(), job.ID+".log"))
		combinedOutput = engineLogStr + "\n" + string(spoolLogData)
		diagReport = diagnostics.Diagnose(diagnostics.DiagnosticInput{
			JobID:        job.ID,
			StoryID:      job.StoryID,
			WorktreePath: worktree,
			ExitCode:     execRes.ExitCode,
			OutputText:   engineLogStr,
		})
	}

	// Check for quota exhaustion auto-fallback (inspect engine log only, never spool/LLM text - Finding M3)
	isQuotaExhausted = hasEngineQuotaExhaustedError(combinedEngineLog)
	if !isQuotaExhausted && engineName == "agent" {
		isQuotaExhausted = hasEngineQuotaExhaustedError([]byte(spoolLogData))
	}
	if !isQuotaExhausted {
		for _, f := range diagReport.Findings {
			if f.Code == diagnostics.CodeQuotaExhausted {
				isQuotaExhausted = true
				break
			}
		}
	}

	if isQuotaExhausted && engineName == "agent" && ctx.Err() == nil {
		fallbackEngine := "agy"
		fallbackModel := "gemini-3.8-flash-high"
		fallbackMsg := fmt.Sprintf("[iazio-harness] Quota/usage limit exhausted on engine 'agent' (Cursor CLI): ActionRequiredError: You're out of usage.\n[iazio-harness] Activating auto-engine fallback: switching to '%s' (model: %s) and re-executing prompt in-place...\n", fallbackEngine, fallbackModel)
		_ = sp.AppendRaw("stdout", []byte(fallbackMsg))
		_ = controlplane.PostChunk(context.Background(), api, token, jobID, "stdout", fallbackMsg)

		fallbackReq := execReq
		fallbackReq.Engine = fallbackEngine
		fallbackReq.Model = fallbackModel
		fallbackReq.Argv = nil
		fallbackReq.ResumeConversationID = "" // clear to ensure fresh start on agy (Finding C2)

		// Reset confirmed conversation pin so the new agy conversation can be pinned as fallback_primary (Finding C2)
		convMu.Lock()
		confirmedConvID = ""
		isPreflightFallback = true
		preflightFallbackReason = "quota_exhausted_engine_switch: switched from agent to agy"
		convMu.Unlock()

		fallbackRunner := engines.CLI{Engine: fallbackEngine}
		if checkErr := fallbackRunner.HealthCheck(); checkErr == nil {
			log.Printf("[harness] Auto-engine fallback: switching from agent to agy for job %s", jobID)
			engineName = fallbackEngine
			modelName = fallbackModel
			execRes, execErr = fallbackRunner.Execute(ctx, fallbackReq, sp)

			// Re-flush spool after fallback
			for _, ev := range sp.Flush() {
				if ev.Type == spool.EventOutputChunk {
					_ = controlplane.PostChunk(context.Background(), api, token, jobID, ev.Stream, ev.Text)
				}
			}

			// Wait for any in-flight conversation registration from the quota fallback run
			fallbackConvDone := make(chan struct{})
			go func() {
				convWg.Wait()
				close(fallbackConvDone)
			}()
			select {
			case <-fallbackConvDone:
			case <-time.After(3 * time.Second):
			}

			// Re-diagnose after fallback run
			_ = sp.Sync()
			engineLogData, _ = os.ReadFile(fallbackReq.LogFile)
			stderrLogData, _ = os.ReadFile(strings.TrimSuffix(fallbackReq.LogFile, ".log") + ".stderr.log")
			combinedEngineLog = append(append([]byte(nil), engineLogData...), stderrLogData...)
			engineLogStr = string(combinedEngineLog)
			spoolLogData, _ = os.ReadFile(filepath.Join(spool.DefaultDir(), job.ID+".log"))
			combinedOutput = engineLogStr + "\n" + string(spoolLogData)
			diagReport = diagnostics.Diagnose(diagnostics.DiagnosticInput{
				JobID:        job.ID,
				StoryID:      job.StoryID,
				WorktreePath: worktree,
				ExitCode:     execRes.ExitCode,
				OutputText:   engineLogStr,
			})
		} else {
			warnMsg := fmt.Sprintf("[iazio-harness] Auto-fallback engine '%s' is not installed on host PATH: %v\n", fallbackEngine, checkErr)
			_ = sp.AppendRaw("stdout", []byte(warnMsg))
			_ = controlplane.PostChunk(context.Background(), api, token, jobID, "stdout", warnMsg)
		}
	}

	// In-Place Conversational Auto-Continuation Loop:
	// If the engine turn exits while ASE completion gates (clean_working_tree, story DoD, review scheme, git-flow merge)
	// are unmet, or background tasks were terminated on exit / print timeout, do NOT fail the job.
	// Immediately re-invoke the engine in-place on the active conversation until all completion gates pass.
	maxAutoResumes := 10
	if envMax := os.Getenv("IAZIO_MAX_AUTO_RESUMES"); envMax != "" {
		if n, err := strconv.Atoi(envMax); err == nil && n > 0 {
			maxAutoResumes = n
		}
	}
	autoResumeCount := 0
	lastProgressFp := ""
	consecutiveStalls := 0

	isContinuationTrigger := func() (shouldResume bool, unmetGates []string, triggerReason string) {
		// 1. If print timeout or background task terminated occurred
		hasPrintTimeout := strings.Contains(combinedOutput, "print timeout after") ||
			strings.Contains(combinedOutput, "with turn in progress; returning partial output")
		hasTerminatedBgTasks := strings.Contains(combinedOutput, "terminating 1 background task(s) on exit") ||
			(strings.Contains(combinedOutput, "terminating") && strings.Contains(combinedOutput, "background task(s) on exit"))

		if hasPrintTimeout {
			return true, []string{"turn_in_progress"}, "print_timeout_with_turn_in_progress"
		}
		if hasTerminatedBgTasks {
			return true, []string{"background_tasks_interrupted"}, "background_tasks_terminated_on_exit"
		}
		if execRes.WasAutoReaped || strings.Contains(combinedOutput, "[background_task.auto_reaped]") {
			return true, []string{"background_tasks_reaped"}, "hung_background_tasks_reaped"
		}
		if execErr != nil && (errors.Is(execErr, context.DeadlineExceeded) || strings.Contains(execErr.Error(), "timeout")) {
			return true, []string{"execution_timeout"}, "execution_timeout"
		}
		if strings.Contains(combinedOutput, "root agent idle; waiting up to") ||
			strings.Contains(combinedOutput, "inactivity limit") {
			return true, []string{"agent_idle_or_inactivity"}, "agent_idle_or_inactivity"
		}

		// 2. Unmet ASE flow gates: when child engine exited with code 0 but left an incomplete story flow or dirty worktree
		if execRes.ExitCode == 0 && diagReport.SIR != nil && diagReport.SIR.AutoHealable {
			var unmet []string
			for _, f := range diagReport.Findings {
				if f.Code == diagnostics.CodeIncompleteASEFlow {
					unmet = append(unmet, "clean_working_tree", "story_definition_of_done")
				} else if f.Code == diagnostics.CodeDirtyWorktreePostExit {
					unmet = append(unmet, "clean_working_tree")
				}
			}
			if len(unmet) > 0 {
				return true, unmet, "unmet_ase_completion_gates"
			}
		}

		if os.Getenv("IAZIO_ENABLE_IN_PLACE_AUTO_RESUME") == "1" && execRes.ExitCode != 0 && !diagReport.ASEComplete {
			return true, []string{"incomplete_ase_flow"}, fmt.Sprintf("exit_code_%d", execRes.ExitCode)
		}

		return false, nil, ""
	}

	for {
		shouldResume, unmetGatesList, triggerReason := isContinuationTrigger()
		if !shouldResume || autoResumeCount >= maxAutoResumes {
			break
		}

		headSHA := resolveGitHead(worktree)
		dirtyFiles := collectWorkspaceDirtyFiles(worktree)
		reviewsCount := countReviewReports(worktree)
		currentFp := fmt.Sprintf("%s:%s:%d:%d", headSHA, resolveLiveGitBranch(worktree), len(dirtyFiles), reviewsCount)
		if currentFp != lastProgressFp {
			consecutiveStalls = 0
			lastProgressFp = currentFp
		} else {
			consecutiveStalls++
			if consecutiveStalls >= 3 {
				stallMsg := fmt.Sprintf("[iazio-harness] Zero forward progress observed across %d consecutive continuation turns; halting auto-continuation.\n", consecutiveStalls)
				_ = sp.AppendRaw("stdout", []byte(stallMsg))
				_ = controlplane.PostChunk(context.Background(), api, token, jobID, "stdout", stallMsg)
				break
			}
		}

		resumeTargetID := activeConvID
		if resumeTargetID == "" {
			resumeTargetID = confirmedConvID
		}
		if resumeTargetID == "" {
			resumeTargetID = resumeConvID
		}
		if resumeTargetID == "" && !isControlPlaneResume {
			resumeTargetID = resolveConversationFallback(worktree, currentBoundStoryID, engineName)
		}
		if resumeTargetID == "" {
			break
		}

		activeConvID = resumeTargetID
		confirmedConvID = resumeTargetID
		autoResumeCount++

		if resolved := resolveLiveStoryID(worktree, combinedOutput, currentBoundStoryID); resolved != "" && resolved != "pending_harness_creation" && resolved != currentBoundStoryID {
			prevStory := currentBoundStoryID
			currentBoundStoryID = resolved
			job.StoryID = resolved
			_ = controlplane.PostStoryBinding(context.Background(), api, token, jobID, resolved, "", "")
			bindMsg := fmt.Sprintf("[iazio-harness] Bound active story ID: %s (branch: %s, previously: %s)\n",
				resolved, resolveLiveGitBranch(worktree), prevStory)
			_ = sp.AppendRaw("stdout", []byte(bindMsg))
			_ = controlplane.PostChunk(context.Background(), api, token, jobID, "stdout", bindMsg)
		}

		interruptedNotice := ""
		if triggerReason == "background_tasks_terminated_on_exit" || triggerReason == "print_timeout_with_turn_in_progress" {
			interruptedNotice = "Notice: A background task (such as make run-review-scheme or make validate-local) was interrupted when the previous turn exited. Immediately resume, verify or restart review/validation checks, and proceed through Phase 4 (review scheme) and Phase 5 (git-flow auto-merge) until origin/main is synced and working tree is clean.\n"
		}

		autoResumeMsg := fmt.Sprintf("\n[iazio-harness] Engine turn exited prior to full ASE completion (unmet gates: [%s]). Initiating in-job continuation turn (#%d/%d) on conversation %s (trigger: %s)...\n"+
			"[ASE-SUPERVISOR] Active Story: %s (branch: %s)\n"+
			"[ASE-SUPERVISOR] Fleet Git State: %d dirty file(s) preserved\n"+
			"[ASE-SUPERVISOR] Injecting continuation prompt into %s --conversation=%s...\n\n",
			strings.Join(unmetGatesList, ", "),
			autoResumeCount, maxAutoResumes,
			resumeTargetID, triggerReason,
			currentBoundStoryID, resolveLiveGitBranch(worktree),
			len(dirtyFiles),
			engineName, resumeTargetID,
		)
		_ = sp.AppendRaw("stdout", []byte(autoResumeMsg))
		_ = controlplane.PostChunk(context.Background(), api, token, jobID, "stdout", autoResumeMsg)
		childProcsNow, _ := watchdog.GetDescendantProcesses(activePID)
		activeChildSummary, _ := watchdog.FormatActiveChildProcessesSummary(childProcsNow)
		_ = controlplane.PostTurnContinued(context.Background(), api, token, jobID, autoResumeCount, resumeTargetID, unmetGatesList, activeChildSummary)

		resumeReq := execReq
		resumeReq.Prompt = fmt.Sprintf("resume — continue autonomous story execution until all story completion gates pass.\n"+
			"Active Story: %s (branch: %s)\n"+
			"Unmet ASE Gates: [%s]\n"+
			"Working Tree: %d modified/untracked file(s) present.\n%s",
			currentBoundStoryID, resolveLiveGitBranch(worktree),
			strings.Join(unmetGatesList, ", "), len(dirtyFiles),
			interruptedNotice,
		)
		resumeReq.ResumeConversationID = resumeTargetID
		resumeReq.PrintTimeout = "0"
		resumeReq.Argv = nil

		execRes, execErr = runner.Execute(ctx, resumeReq, sp)

		// Flush spool
		for _, ev := range sp.Flush() {
			if ev.Type == spool.EventOutputChunk {
				_ = controlplane.PostChunk(context.Background(), api, token, jobID, ev.Stream, ev.Text)
			}
		}

		_ = sp.Sync()
		engineLogData, _ = os.ReadFile(resumeReq.LogFile)
		stderrLogData, _ = os.ReadFile(strings.TrimSuffix(resumeReq.LogFile, ".log") + ".stderr.log")
		combinedEngineLog = append(append([]byte(nil), engineLogData...), stderrLogData...)
		engineLogStr = string(combinedEngineLog)
		spoolLogData, _ = os.ReadFile(filepath.Join(spool.DefaultDir(), job.ID+".log"))
		combinedOutput = engineLogStr + "\n" + string(spoolLogData)

		diagStoryID := currentBoundStoryID
		if diagStoryID == "" || diagStoryID == "pending_harness_creation" {
			diagStoryID = resolveLiveStoryID(worktree, combinedOutput, job.StoryID)
		}
		diagReport = diagnostics.Diagnose(diagnostics.DiagnosticInput{
			JobID:        job.ID,
			StoryID:      diagStoryID,
			WorktreePath: worktree,
			ExitCode:     execRes.ExitCode,
			OutputText:   engineLogStr,
		})

		if execRes.ExitCode == 0 && (diagReport.ASEComplete || len(collectWorkspaceDirtyFiles(worktree)) == 0) {
			successMsg := "[ASE-SUPERVISOR] In-place auto-resume completed successfully!\n"
			_ = sp.AppendRaw("stdout", []byte(successMsg))
			_ = controlplane.PostChunk(context.Background(), api, token, jobID, "stdout", successMsg)
			break
		}
	}

	diagFinalStoryID := currentBoundStoryID
	if diagFinalStoryID == "" || diagFinalStoryID == "pending_harness_creation" {
		diagFinalStoryID = resolveLiveStoryID(worktree, combinedOutput, job.StoryID)
	}
	diagReport = diagnostics.Diagnose(diagnostics.DiagnosticInput{
		JobID:        job.ID,
		StoryID:      diagFinalStoryID,
		WorktreePath: worktree,
		ExitCode:     execRes.ExitCode,
		OutputText:   engineLogStr,
	})

	if len(diagReport.Findings) > 0 {
		for _, f := range diagReport.Findings {
			diagMsg := fmt.Sprintf("[iazio-harness] Anomaly detected: [%s] (%s): %s Remediation: %s\n", f.Code, f.Severity, f.Message, f.Remediation)
			_ = sp.AppendRaw("stdout", []byte(diagMsg))
			_ = controlplane.PostChunk(context.Background(), api, token, jobID, "stdout", diagMsg)
		}
		if diagReport.SIR != nil {
			sirMsg := fmt.Sprintf("[iazio-harness] Emitted Structured Intervention Request (SIR): %s (category: %s, auto-healable: %v)\n", diagReport.SIR.ID, diagReport.SIR.Category, diagReport.SIR.AutoHealable)
			_ = sp.AppendRaw("stdout", []byte(sirMsg))
			_ = controlplane.PostChunk(context.Background(), api, token, jobID, "stdout", sirMsg)
			if diagReport.SIRPath != "" {
				persMsg := fmt.Sprintf("[iazio-harness] SIR report persisted to: %s\n", diagReport.SIRPath)
				_ = sp.AppendRaw("stdout", []byte(persMsg))
				_ = controlplane.PostChunk(context.Background(), api, token, jobID, "stdout", persMsg)
			}
		}
		summaryText := "\n" + diagReport.TroubleshootingSummary() + "\n"
		_ = sp.AppendRaw("stdout", []byte(summaryText))
		_ = controlplane.PostChunk(context.Background(), api, token, jobID, "stdout", summaryText)
	}
	if diagReport.ASEComplete && diagReport.Healthy && execRes.ExitCode != 0 {
		overMsg := fmt.Sprintf("[iazio-harness] ASE verified complete on origin/main with clean worktree. Reconciling exit code %d to 0 (COMPLETED).\n", execRes.ExitCode)
		_ = sp.AppendRaw("stdout", []byte(overMsg))
		_ = controlplane.PostChunk(context.Background(), api, token, jobID, "stdout", overMsg)
		execRes.ExitCode = 0
		execErr = nil
	} else if !diagReport.Healthy && execRes.ExitCode == 0 {
		execRes.ExitCode = 1
	}

	exitOpts := controlplane.ExitOptions{
		ExitCode:    execRes.ExitCode,
		Engine:      engineName,
		Model:       modelName,
		ASEComplete: diagReport.ASEComplete,
	}
	if execRes.ExitCode != 0 || !diagReport.Healthy {
		if len(diagReport.Findings) > 0 {
			f := diagReport.Findings[0]
			exitOpts.DiagnosticCode = string(f.Code)
			exitOpts.Reason = string(f.Code)
			exitOpts.ErrorMessage = f.Message
			exitOpts.Remediation = f.Remediation
		}
		for _, line := range strings.Split(combinedOutput, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.Contains(trimmed, "ActionRequiredError") || strings.Contains(trimmed, "out of usage") {
				exitOpts.ErrorMessage = trimmed
				exitOpts.DiagnosticCode = string(diagnostics.CodeQuotaExhausted)
				exitOpts.Reason = string(diagnostics.CodeQuotaExhausted)
				exitOpts.Remediation = "Switch execution engine (e.g. to agy with Gemini Flash / Auto model) or increase model usage limit."
				break
			}
		}
		if exitOpts.Reason == "" {
			exitOpts.Reason = "NON_ZERO_EXIT"
		}
	}

	dirtyFiles := collectWorkspaceDirtyFiles(worktree)
	if activeConvID == "" && !isControlPlaneResume {
		activeConvID = resolveConversationFallback(worktree, job.StoryID, engineName)
		if activeConvID != "" {
			confirmedConvID = activeConvID
		}
	}
	resumable := (activeConvID != "" || confirmedConvID != "" || resumeConvID != "")
	if !resumable && (len(job.ConversationIDs) > 0 || job.StoryID != "") {
		// In-progress story executions and jobs with recorded conversations are always resumable
		resumable = true
	}
	nonResumableReason := ""
	if !resumable {
		nonResumableReason = "No conversation session established before termination"
	}
	terminationCode := "COMPLETED_SUCCESS"
	terminationMessage := "Job execution completed successfully."
	if execRes.WasAutoReaped {
		terminationCode = "BACKGROUND_TASK_AUTO_REAPED"
		terminationMessage = fmt.Sprintf("Auto-reaped %d background task(s) after inactivity while root agent was idle", len(execRes.BackgroundTasks))
		resumable = true
		nonResumableReason = ""
	} else if execRes.ExitCode != 0 || execErr != nil {
		terminationCode = "EXIT_CODE_NONZERO"
		if exitOpts.Reason != "" {
			terminationCode = exitOpts.Reason
		}
		if exitOpts.ErrorMessage != "" {
			terminationMessage = exitOpts.ErrorMessage
		} else if execErr != nil {
			terminationMessage = execErr.Error()
		} else {
			terminationMessage = fmt.Sprintf("Engine process exited with non-zero exit code %d", execRes.ExitCode)
		}
	}

	exitOpts.TerminationReasonCode = terminationCode
	exitOpts.TerminationReasonMessage = terminationMessage
	exitOpts.Resumable = resumable
	exitOpts.NonResumableReason = nonResumableReason
	exitOpts.WorkspaceDirtyFiles = dirtyFiles
	exitOpts.BackgroundTasks = execRes.BackgroundTasks

	// Save terminal execution checkpoint
	finalStatus := "COMPLETED"
	if execErr != nil || execRes.ExitCode != 0 {
		finalStatus = "FAILED"
	}
	_ = checkpoint.Save(worktree, spool.DefaultDir(), checkpoint.State{
		JobID:                    job.ID,
		ScheduleID:               job.ScheduleID,
		Engine:                   engineName,
		WorktreePath:             worktree,
		ConversationID:           activeConvID,
		StoryID:                  job.StoryID,
		StepIndex:                3,
		StartedAt:                time.Now().UTC().Format(time.RFC3339),
		Status:                   finalStatus,
		ExitCode:                 execRes.ExitCode,
		ASEComplete:              diagReport.ASEComplete,
		TerminationReasonCode:    terminationCode,
		TerminationReasonMessage: terminationMessage,
		Resumable:                resumable,
		NonResumableReason:       nonResumableReason,
		WorkspaceDirtyFiles:      dirtyFiles,
		BackgroundTasks:          execRes.BackgroundTasks,
	})

	// Explicitly release checkout file lock before posting exit to control plane,
	// ensuring subsequent jobs can immediately acquire the lock without race or contention.
	_ = lock.Release()

	span.SetTag("iazio.status", finalStatus)
	span.SetTag("iazio.exit_code", fmt.Sprintf("%d", execRes.ExitCode))
	if exitOpts.DiagnosticCode != "" {
		span.SetTag("iazio.diagnostic_code", exitOpts.DiagnosticCode)
	}
	if exitOpts.Reason != "" {
		span.SetTag("iazio.reason", exitOpts.Reason)
	}
	if execErr != nil {
		span.SetError(execErr)
	}

	var postExitErr error
	for attempt := 1; attempt <= 3; attempt++ {
		postExitErr = controlplane.PostExitWithOptions(context.Background(), api, token, jobID, exitOpts)
		if postExitErr == nil {
			break
		}
		time.Sleep(time.Duration(attempt*250) * time.Millisecond)
	}
	if postExitErr != nil {
		fmt.Fprintf(os.Stderr, "warning: post exit failed for job %s: %v\n", jobID, postExitErr)
	} else {
		span.AddAnnotation("post_exit_complete")
	}

	if finalStatus == "COMPLETED" {
		wd.Complete(job.ID, execRes.ExitCode)
	} else {
		errMsg := ""
		if execErr != nil {
			errMsg = execErr.Error()
		} else if exitOpts.ErrorMessage != "" {
			errMsg = exitOpts.ErrorMessage
		}
		wd.Fail(job.ID, execRes.ExitCode, errMsg)
	}

	if execErr != nil {
		return execErr
	}
	if execRes.ExitCode != 0 {
		return fmt.Errorf("engine %s exited with code %d", engineName, execRes.ExitCode)
	}
	return nil
}

func formatVersion() string {
	if commit == "" || commit == "unknown" {
		return "iazio-harness " + version
	}
	sha := commit
	if len(sha) > 12 {
		sha = sha[:12]
	}
	return "iazio-harness " + version + "+" + sha
}

func resolveExecutionOptions(job controlplane.JobDetail, promptBody string) (model string, agent string, disableSlash bool) {
	model = job.Model
	if model == "" && job.EnvVars != nil {
		model = job.EnvVars["MODEL"]
	}
	if model == "" {
		model = "gemini-3.8-flash-high"
	}

	agent = job.Agent
	if agent == "" && job.EnvVars != nil {
		agent = job.EnvVars["AGENT"]
	}

	disableSlash = true
	trimmedPrompt := strings.TrimSpace(promptBody)
	if job.DisableSlashCommands != nil {
		disableSlash = *job.DisableSlashCommands
	} else if strings.HasPrefix(trimmedPrompt, "/") {
		disableSlash = false
	} else if job.EnvVars != nil {
		if strings.ToLower(job.EnvVars["ENABLE_SLASH_COMMANDS"]) == "true" ||
			strings.ToLower(job.EnvVars["DISABLE_SLASH_COMMANDS"]) == "false" {
			disableSlash = false
		}
	}
	return
}

func runFleetStatusCommand(args []string) error {
	repoPath := "."
	var jsonOutput bool
	for i := 0; i < len(args); i++ {
		switch {
		case (args[i] == "--repo" || args[i] == "--worktree-path") && i+1 < len(args):
			i++
			repoPath = args[i]
		case strings.HasPrefix(args[i], "--repo="):
			repoPath = strings.TrimPrefix(args[i], "--repo=")
		case strings.HasPrefix(args[i], "--worktree-path="):
			repoPath = strings.TrimPrefix(args[i], "--worktree-path=")
		case args[i] == "--json":
			jsonOutput = true
		}
	}
	res, err := diagnostics.InspectRepoFleetStatus(context.Background(), repoPath)
	if err != nil {
		return err
	}
	if jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(res)
	}
	fmt.Print(res.DiagnosticSummary)
	return nil
}

func runHealASECommand(args []string) error {
	repoPath := "."
	for i := 0; i < len(args); i++ {
		switch {
		case (args[i] == "--repo" || args[i] == "--worktree-path") && i+1 < len(args):
			i++
			repoPath = args[i]
		case strings.HasPrefix(args[i], "--repo="):
			repoPath = strings.TrimPrefix(args[i], "--repo=")
		case strings.HasPrefix(args[i], "--worktree-path="):
			repoPath = strings.TrimPrefix(args[i], "--worktree-path=")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := diagnostics.ReapOrphanedProcesses(ctx, repoPath); err != nil {
		fmt.Fprintf(os.Stderr, "warning: reap orphaned processes: %v\n", err)
	}
	if err := diagnostics.ResetASELocks(repoPath); err != nil {
		return fmt.Errorf("reset ase locks: %w", err)
	}
	fmt.Printf("Successfully healed ASE state and reaped orphaned processes for %s\n", repoPath)
	return nil
}

func runServiceCommand(args []string) error {
	action := "status"
	force := false
	dry := false
	var extraArgs []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--dry-run":
			dry = true
		case "--force", "-f":
			force = true
		default:
			if !strings.HasPrefix(args[i], "-") && action == "status" {
				action = args[i]
			} else {
				extraArgs = append(extraArgs, args[i])
			}
		}
	}

	bin, err := os.Executable()
	if err != nil {
		bin = "iazio-harness"
	} else if resolved, err := filepath.EvalSymlinks(bin); err == nil {
		bin = resolved
	}

	home, _ := os.UserHomeDir()
	goos := runtime.GOOS

	switch action {
	case "install":
		if dry {
			fmt.Println(service.ActionText(goos, action, bin, force))
			return nil
		}
		switch goos {
		case "darwin":
			return service.InstallDarwin(bin)
		case "windows":
			return service.InstallWindows(bin)
		default:
			if service.IsSystemdAvailable() {
				return service.InstallLinux(bin)
			}
			return service.InstallUserDaemon(bin)
		}
	case "dev-install":
		src := bin
		if len(extraArgs) > 0 {
			src = extraArgs[0]
		}
		if dry {
			fmt.Printf("hot-swap %s and restart supervisor\n", src)
			return nil
		}
		if err := service.DevInstall(src, home, nil); err != nil {
			return err
		}
		fmt.Println("iazio-harness dev version installed and supervisor active")
		return nil
	case "pause":
		if dry {
			fmt.Println("pause supervisor and place maintenance lock")
			return nil
		}
		if err := service.PauseService(home, nil); err != nil {
			return err
		}
		fmt.Println("iazio-harness service paused (maintenance mode active)")
		return nil
	case "resume":
		if dry {
			fmt.Println("resume supervisor and remove maintenance lock")
			return nil
		}
		if err := service.ResumeService(home, nil); err != nil {
			return err
		}
		fmt.Println("iazio-harness service resumed (maintenance mode cleared)")
		return nil
	case "revive-check":
		if dry {
			fmt.Println("check supervisor health and auto-revive if inactive")
			return nil
		}
		return service.ReviveCheck(home, nil)
	case "start", "stop", "restart", "uninstall":
		if dry {
			fmt.Println(service.ActionText(goos, action, bin, force))
			return nil
		}
		if goos == "linux" && !service.IsSystemdAvailable() {
			switch action {
			case "start":
				return service.StartUserDaemonAt(home, bin, nil)
			case "stop":
				return service.StopUserDaemonAt(home)
			case "restart":
				_ = service.StopUserDaemonAt(home)
				return service.StartUserDaemonAt(home, bin, nil)
			case "uninstall":
				return service.UninstallUserDaemonAt(home)
			}
		}
		_, err := service.Apply(goos, action, bin, false, force, nil)
		return err
	case "status":
		if goos == "linux" && !service.IsSystemdAvailable() {
			running, pid := service.StatusUserDaemonAt(home)
			if running {
				fmt.Printf("running (pid: %d)\n", pid)
				return nil
			}
			fmt.Println("installed but not running")
			return nil
		}
		txt := service.ActionText(goos, action, bin, force)
		fmt.Println(txt)
		return nil
	default:
		return fmt.Errorf("unknown service action %s", action)
	}
}

func runDaemonCommand(args []string) error {
	interval := watchdog.DefaultInterval
	for i := 0; i < len(args); i++ {
		if (args[i] == "--interval" || args[i] == "-i") && i+1 < len(args) {
			i++
			if d, err := time.ParseDuration(args[i]); err == nil {
				interval = d
			}
		}
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	log.Printf("[iazio-harness] Starting execution supervisor daemon (pid: %d, interval: %v)...", os.Getpid(), interval)
	wd := watchdog.New(watchdog.Options{})
	return wd.Run(ctx, interval)
}

func runVerifyReleaseCommand(args []string) error {
	var api, token string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--api-url":
			i++
			if i < len(args) {
				api = args[i]
			}
		case "--token":
			i++
			if i < len(args) {
				token = args[i]
			}
		}
	}
	if api == "" {
		api = os.Getenv("IAZIO_HARNESS_API_URL")
	}
	if api == "" {
		api = os.Getenv("IAZIO_API_URL")
	}
	if api == "" {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			if b, err := os.ReadFile(filepath.Join(home, ".iazio", "api_url")); err == nil {
				api = strings.TrimSpace(string(b))
			}
		}
	}
	if api == "" {
		api = "http://127.0.0.1:8080"
	}
	if token == "" {
		if t, err := auth.Bearer(context.Background()); err == nil && t != "" {
			token = t
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	fmt.Printf("[iazio-harness] Running post-release live verification against %s...\n", api)
	res, err := controlplane.VerifyLiveRelease(ctx, api, token)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[iazio-harness] Live verification failed: %v\n", err)
		return err
	}

	fmt.Println("[iazio-harness] Live verification passed successfully:")
	for _, detail := range res.Details {
		fmt.Printf("  - %s\n", detail)
	}
	return nil
}

func collectWorkspaceDirtyFiles(worktree string) []string {
	if worktree == "" {
		return nil
	}
	cmd := exec.Command("git", "-C", worktree, "status", "--porcelain")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	lines := strings.Split(string(out), "\n")
	var dirty []string
	for _, l := range lines {
		trimmed := strings.TrimSpace(l)
		if len(trimmed) > 3 {
			filePath := strings.TrimSpace(trimmed[2:])
			if filePath != "" {
				dirty = append(dirty, filePath)
			}
		}
	}
	return dirty
}
