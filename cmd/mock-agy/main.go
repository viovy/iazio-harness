package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/viovy/iazio-core/pkg/procutil"
)

func main() {
	base := strings.TrimSuffix(strings.ToLower(filepath.Base(os.Args[0])), ".exe")
	if strings.HasSuffix(base, "-mcp") || (len(os.Args) > 1 && os.Args[1] == "serve") {
		time.Sleep(30 * time.Second)
		os.Exit(0)
	}
	code := run(os.Args[1:], strings.TrimSpace(os.Getenv("MOCK_AGY_SCENARIO")), os.Stdout, os.Stderr)
	if code == 0 {
		if out, err := procutil.Command("git", "remote").Output(); err == nil && strings.Contains(string(out), "origin") {
			_ = procutil.Command("git", "push").Run()
		}
	}
	os.Exit(code)
}

func run(args []string, scenario string, stdout, stderr io.Writer) int {
	var conversationID string
	var printPrompt string
	var model string
	var agent string
	var logFile string
	var outputFormat string
	var inputFormat string
	var mode string
	var effort string
	var addDirs []string
	var skipPermissions bool
	var continueSession bool
	var jsonOutput bool
	var positional []string

	// Check for top-level subcommands first
	if len(args) > 0 {
		subcmd := args[0]
		switch subcmd {
		case "version", "--version", "-v":
			for _, a := range args[1:] {
				if a == "--json" {
					jsonOutput = true
				}
			}
			if jsonOutput {
				fmt.Fprintln(stdout, `{"version": "1.15.0-mock", "channel": "stable"}`)
			} else {
				fmt.Fprintln(stdout, "antigravity 1.15.0-mock")
			}
			return 0
		case "models":
			for _, a := range args[1:] {
				if a == "--json" {
					jsonOutput = true
				}
			}
			if jsonOutput {
				fmt.Fprintln(stdout, `[{"id":"gemini-2.5-pro","name":"Gemini 2.5 Pro"},{"id":"gemini-2.5-flash","name":"Gemini 2.5 Flash"}]`)
			} else {
				fmt.Fprintln(stdout, "Available models:\n  - gemini-2.5-pro\n  - gemini-2.5-flash")
			}
			return 0
		case "agents":
			for _, a := range args[1:] {
				if a == "--json" {
					jsonOutput = true
				}
			}
			if jsonOutput {
				fmt.Fprintln(stdout, `[{"name":"general"},{"name":"autonomous"}]`)
			} else {
				fmt.Fprintln(stdout, "Available agents:\n  - general\n  - autonomous")
			}
			return 0
		case "help", "--help", "-h":
			fmt.Fprintln(stdout, "Usage: agy [options] [prompt]")
			fmt.Fprintln(stdout, "Flags:")
			fmt.Fprintln(stdout, "  -p, --print, --prompt <text>      Headless non-interactive execution")
			fmt.Fprintln(stdout, "  -c, --continue, --conversation    Target conversation session")
			fmt.Fprintln(stdout, "  --model <model>                   Model identifier")
			fmt.Fprintln(stdout, "  --agent <agent>                   Agent persona identifier")
			fmt.Fprintln(stdout, "  --dangerously-skip-permissions    Bypass tool execution confirmation gates")
			return 0
		}
	}

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case strings.HasPrefix(arg, "--conversation="):
			conversationID = strings.TrimPrefix(arg, "--conversation=")
		case arg == "--conversation":
			if i+1 < len(args) {
				conversationID = args[i+1]
				i++
			}
		case arg == "-c" || arg == "--continue":
			continueSession = true
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				conversationID = args[i+1]
				i++
			}
		case strings.HasPrefix(arg, "--print="):
			printPrompt = strings.TrimPrefix(arg, "--print=")
		case strings.HasPrefix(arg, "--prompt="):
			printPrompt = strings.TrimPrefix(arg, "--prompt=")
		case arg == "--print" || arg == "-p" || arg == "--prompt":
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
				printPrompt = args[i+1]
				i++
			}
		case strings.HasPrefix(arg, "--model="):
			model = strings.TrimPrefix(arg, "--model=")
		case arg == "--model":
			if i+1 < len(args) {
				model = args[i+1]
				i++
			}
		case strings.HasPrefix(arg, "--agent="):
			agent = strings.TrimPrefix(arg, "--agent=")
		case arg == "--agent":
			if i+1 < len(args) {
				agent = args[i+1]
				i++
			}
		case strings.HasPrefix(arg, "--log-file="):
			logFile = strings.TrimPrefix(arg, "--log-file=")
		case arg == "--log-file":
			if i+1 < len(args) {
				logFile = args[i+1]
				i++
			}
		case strings.HasPrefix(arg, "--output-format="):
			outputFormat = strings.TrimPrefix(arg, "--output-format=")
		case arg == "--output-format":
			if i+1 < len(args) {
				outputFormat = args[i+1]
				i++
			}
		case strings.HasPrefix(arg, "--input-format="):
			inputFormat = strings.TrimPrefix(arg, "--input-format=")
		case arg == "--input-format":
			if i+1 < len(args) {
				inputFormat = args[i+1]
				i++
			}
		case strings.HasPrefix(arg, "--mode="):
			mode = strings.TrimPrefix(arg, "--mode=")
		case arg == "--mode":
			if i+1 < len(args) {
				mode = args[i+1]
				i++
			}
		case strings.HasPrefix(arg, "--effort="):
			effort = strings.TrimPrefix(arg, "--effort=")
		case arg == "--effort":
			if i+1 < len(args) {
				effort = args[i+1]
				i++
			}
		case strings.HasPrefix(arg, "--add-dir="):
			addDirs = append(addDirs, strings.TrimPrefix(arg, "--add-dir="))
		case arg == "--add-dir":
			if i+1 < len(args) {
				addDirs = append(addDirs, args[i+1])
				i++
			}
		case arg == "--print-timeout" || strings.HasPrefix(arg, "--print-timeout="):
			if !strings.Contains(arg, "=") && i+1 < len(args) {
				i++
			}
		case arg == "--dangerously-skip-permissions":
			skipPermissions = true
		case arg == "--disable-slash-commands":
			// Boolean flag
		default:
			if !strings.HasPrefix(arg, "--") {
				positional = append(positional, arg)
			}
		}
	}

	fullPrompt := printPrompt
	if fullPrompt == "" && len(positional) > 0 {
		fullPrompt = strings.Join(positional, " ")
	}
	_ = outputFormat
	_ = inputFormat
	_ = mode
	_ = effort
	_ = addDirs
	_ = continueSession

	// Ensure conversation DB directory exists if conversationID is provided
	if conversationID != "" {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			convDir := filepath.Join(home, ".gemini", "antigravity-cli", "conversations")
			_ = os.MkdirAll(convDir, 0755)
			dbPath := filepath.Join(convDir, conversationID+".db")
			_ = os.WriteFile(dbPath, []byte("mock-db"), 0644)
		}
	}

	var logFileWriter io.Writer
	if logFile != "" {
		_ = os.MkdirAll(filepath.Dir(logFile), 0755)
		if f, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644); err == nil {
			defer f.Close()
			logFileWriter = f
			stdout = io.MultiWriter(stdout, f)
			stderr = io.MultiWriter(stderr, f)
		}
	}

	if (!skipPermissions && strings.Contains(strings.ToLower(fullPrompt), "require_permission")) || scenario == "permission_denied" {
		fmt.Fprintln(stderr, "jetski: no output produced — a tool required the 'command' permission that headless mode cannot prompt for, so it was auto-denied. To allow tools to run without interactive approval, pass --dangerously-skip-permissions.")
		return 1
	}

	switch scenario {
	case "session_corrupted":
		fmt.Fprintln(stderr, "Error: database disk image is malformed: conversation session corrupted")
		return 1

	case "quota_exhausted":
		fmt.Fprintln(stderr, "ResourceExhausted: 429 You have exhausted your current token quota. Please wait or upgrade your tier.")
		return 1

	case "ordinary_prompt_with_mcp_daemons":
		// Scenario 1: Print normal output, spawn persistent background child daemon that keeps
		// pipes inherited, and exit 0 immediately.
		fmt.Fprintf(stdout, "[mock-agy] Executing ordinary prompt (model: %s, agent: %s)...\n", model, agent)
		fmt.Fprintf(stdout, "[mock-agy] Prompt: %s\n", fullPrompt)
		fmt.Fprintln(stdout, "[mock-agy] Initializing background MCP daemons: autopilot-mcp, chrome-devtools-mcp...")
		fmt.Fprintln(stdout, "[mock-agy] Execution completed successfully. Primary engine PID exiting 0.")

		if stdout == os.Stdout {
			// Spawn background sleeper that holds pipes open when running as subprocess
			cmd := procutil.Command("sleep", "60")
			cmd.Stdout = stdout
			cmd.Stderr = stderr
			_ = cmd.Start()
		}
		return 0

	case "agent_restart_and_upgrade_mid_run":
		// Scenario 2: Runs streaming output for several seconds.
		fmt.Fprintln(stdout, "[mock-agy] Starting execution for agent restart/upgrade resilience test...")
		if conversationID != "" {
			fmt.Fprintf(stdout, "[mock-agy] Active conversation: %s\n", conversationID)
		}
		stepDelay := 300 * time.Millisecond
		if os.Getenv("MOCK_AGY_FAST") == "1" {
			stepDelay = time.Millisecond
		}
		for i := 1; i <= 5; i++ {
			fmt.Fprintf(stdout, "[mock-agy] Processing step %d/5: active and streaming...\n", i)
			time.Sleep(stepDelay)
		}
		fmt.Fprintln(stdout, "[mock-agy] All steps completed successfully.")
		return 0

	case "ase_print_timeout_in_job_continuation":
		// Scenario 3: Turn 1 creates modified files in worktree and outputs print timeout, exiting 0.
		// Turn 2 receives "resume" and cleans/commits files, completing cleanly.
		stateMarker := filepath.Join(os.TempDir(), "mock_agy_ase_turn.tmp")
		_, mErr := os.Stat(stateMarker)
		isTurn2 := (mErr == nil) || strings.Contains(strings.ToLower(fullPrompt), "resume")

		if !isTurn2 {
			// Turn 1
			_ = os.WriteFile("ase_worktree_change.tmp", []byte("uncommitted change"), 0644)
			_ = os.WriteFile(stateMarker, []byte("turn1_done"), 0644)
			fmt.Fprintln(stdout, "Created conversation c0ffee00-1234-5678-9abc-000000000003")
			fmt.Fprintln(stdout, "[mock-agy] Turn 1 executing ASE story flow...")
			fmt.Fprintln(stdout, "[agy] print timeout after 1h0m0s with turn in progress; returning partial output")
			return 0
		} else {
			// Turn 2 (resumed in-place)
			_ = os.Remove("ase_worktree_change.tmp")
			_ = os.Remove(stateMarker)
			_ = os.WriteFile("ase_committed_feature.txt", []byte("feature committed"), 0644)
			_ = procutil.Command("git", "add", "ase_committed_feature.txt").Run()
			_ = procutil.Command("git", "commit", "-m", "feat: complete ASE story flow").Run()
			fmt.Fprintf(stdout, "[mock-agy] Turn 2 resumed in-place with conversation %s.\n", conversationID)
			fmt.Fprintln(stdout, "[mock-agy] Committed changes, cleaned worktree, and satisfied ASE gates.")
			fmt.Fprintln(stdout, "[mock-agy] Story flow complete. Exit 0.")
			return 0
		}

	case "background_task_hang_recovery":
		// Scenario 4: Simulates a hung background child process with root agent idle.
		stateMarker := filepath.Join(os.TempDir(), "mock_agy_bg_hang.tmp")
		_, mErr := os.Stat(stateMarker)
		isTurn2 := (mErr == nil) || strings.Contains(strings.ToLower(fullPrompt), "resume")

		if isTurn2 {
			_ = os.Remove(stateMarker)
			_ = os.WriteFile("bg_hang_recovered_commit.txt", []byte("recovery committed"), 0644)
			_ = procutil.Command("git", "add", "bg_hang_recovered_commit.txt").Run()
			_ = procutil.Command("git", "commit", "-m", "chore: finish tasks after bg hang").Run()
			fmt.Fprintf(stdout, "[mock-agy] Resumed after background task hang was reaped on conversation %s.\n", conversationID)
			fmt.Fprintln(stdout, "[mock-agy] Finished remaining tasks.")
			return 0
		}
		_ = os.WriteFile(stateMarker, []byte("hung"), 0644)
		fmt.Fprintln(stdout, "Created conversation c0ffee00-1234-5678-9abc-000000000004")
		fmt.Fprintln(stdout, "root agent idle; waiting up to 1h0m0s for 1 background task(s)")
		fmt.Fprintln(stdout, "[ASE-SUPERVISOR] [background_task.auto_reaped] Inactivity limit (60s) exceeded while root agent is idle. Terminating 1 background task(s) to unblock resume cycle...")
		return 143

	case "multi_attempt_log_preservation_and_schedule_sync":
		// Scenario 5: Attempt 1 crashes transiently; Attempt 2 resumes and succeeds.
		marker := filepath.Join(os.TempDir(), "mock_agy_attempt_marker.tmp")
		data, err := os.ReadFile(marker)
		if err != nil || string(data) != "attempt_2" {
			_ = os.WriteFile(marker, []byte("attempt_2"), 0644)
			fmt.Fprintln(stdout, "[mock-agy] Attempt 1 startup logs: initializing environment and loading configuration...")
			fmt.Fprintln(stderr, "[mock-agy] Transient network reset occurred. Crashing attempt 1.")
			return 1
		}
		_ = os.Remove(marker)
		_ = os.WriteFile("resumed_attempt_2.txt", []byte("resumed deliverable"), 0644)
		_ = procutil.Command("git", "add", "resumed_attempt_2.txt").Run()
		_ = procutil.Command("git", "commit", "-m", "chore: complete task on attempt 2").Run()
		fmt.Fprintln(stdout, "[mock-agy] Attempt 2 startup logs: resuming from checkpoint...")
		fmt.Fprintln(stdout, "[mock-agy] Attempt 2 succeeded cleanly. Task finished.")
		return 0

	case "dirty_worktree_resume_and_queue_unblock":
		// Scenario 6: First run leaves uncommitted files and fails with 10;
		// Resume cleans up and completes 0.
		stateMarker := filepath.Join(os.TempDir(), "mock_agy_dirty_state.tmp")
		_, mErr := os.Stat(stateMarker)
		isResume := (mErr == nil && strings.Contains(strings.ToLower(fullPrompt), "resume"))

		if !isResume {
			_ = os.WriteFile(stateMarker, []byte("dirty"), 0644)
			_ = os.WriteFile("dirty_file_1324.tmp", []byte("dirty worktree content"), 0644)
			fmt.Fprintln(stdout, "[mock-agy] Execution terminated leaving dirty worktree.")
			return 10
		}
		_ = os.Remove(stateMarker)
		_ = os.Remove("dirty_file_1324.tmp")
		fmt.Fprintln(stdout, "[mock-agy] Resume on existing worktree succeeded. Worktree clean.")
		return 0

	case "in_place_harness_auto_resume", "nonzero_exit_inplace_resume":
		// Scenario 10 / 14: Turn 1 fails transiently with configurable exit code (1, 2, 10); Turn 2 resumes in-place and succeeds.
		stateMarker := filepath.Join(os.TempDir(), "mock_agy_inplace_resume.tmp")
		if customMarker := os.Getenv("MOCK_AGY_STATE_MARKER"); customMarker != "" {
			stateMarker = customMarker
		}
		_, mErr := os.Stat(stateMarker)
		if mErr != nil {
			_ = os.WriteFile(stateMarker, []byte("attempt_1_done"), 0644)
			cid := "mock-conv-1111-2222-3333-444455556666"
			fmt.Fprintf(stdout, "Created conversation %s\n", cid)
			if home, err := os.UserHomeDir(); err == nil && home != "" {
				convDir := filepath.Join(home, ".gemini", "antigravity-cli", "conversations")
				_ = os.MkdirAll(convDir, 0755)
				_ = os.WriteFile(filepath.Join(convDir, cid+".db"), []byte("mock-db"), 0644)
			}
			failCode := 1
			if rawCode := strings.TrimSpace(os.Getenv("MOCK_AGY_EXIT_CODE")); rawCode != "" {
				if rawCode == "2" {
					failCode = 2
				} else if rawCode == "10" {
					failCode = 10
				}
			}
			fmt.Fprintf(stderr, "[mock-agy] Transient network reset on attempt 1. Exiting %d.\n", failCode)
			return failCode
		}
		_ = os.Remove(stateMarker)
		if entries, err := os.ReadDir(filepath.Join("docs", "stories")); err == nil {
			for _, e := range entries {
				if !e.IsDir() && strings.HasSuffix(e.Name(), ".story.md") {
					p := filepath.Join("docs", "stories", e.Name())
					if b, err := os.ReadFile(p); err == nil && strings.Contains(string(b), "status: IN_PROGRESS") {
						updated := strings.ReplaceAll(string(b), "status: IN_PROGRESS", "status: DONE")
						_ = os.WriteFile(p, []byte(updated), 0644)
						_ = procutil.Command("git", "add", p).Run()
					}
				}
			}
		}
		_ = os.WriteFile("inplace_resumed_file.txt", []byte("inplace recovery content\n"), 0644)
		_ = procutil.Command("git", "add", "inplace_resumed_file.txt").Run()
		_ = procutil.Command("git", "commit", "-m", "chore: auto-resumed in-place (STORY-GOV-3799)").Run()
		if conversationID != "" {
			fmt.Fprintf(stdout, "Resumed conversation %s\n", conversationID)
		}
		fmt.Fprintln(stdout, "[mock-agy] In-place auto-resume attempt 2 succeeded cleanly.")
		return 0

	case "empty_conversation_dirty_worktree_resume":
		// Scenario 11: Empty conversation ID + dirty worktree resume
		if conversationID != "" {
			fmt.Fprintf(stderr, "Error: expected empty conversation ID, got %q\n", conversationID)
			return 1
		}
		if !strings.Contains(fullPrompt, "complete current /autonomous-story-execution flow") {
			fmt.Fprintf(stderr, "Error: expected prompt to complete dirty ASE flow, got %q\n", fullPrompt)
			return 1
		}
		_ = os.Remove("dirty_worktree_file.tmp")
		_ = os.WriteFile("completed_story_work.txt", []byte("story work finished\n"), 0644)
		_ = procutil.Command("git", "add", ".").Run()
		_ = procutil.Command("git", "commit", "-m", "chore: complete unanchored dirty flow").Run()
		cid := "mock-conv-dirty-uuid-0000-1111-2222"
		fmt.Fprintf(stdout, "Created conversation %s\n", cid)
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			convDir := filepath.Join(home, ".gemini", "antigravity-cli", "conversations")
			_ = os.MkdirAll(convDir, 0755)
			_ = os.WriteFile(filepath.Join(convDir, cid+".db"), []byte("mock-db"), 0644)
		}
		fmt.Fprintln(stdout, "[mock-agy] Unanchored dirty worktree flow completed successfully.")
		return 0

	case "empty_conversation_clean_worktree_story_restart":
		// Scenario 12: Empty conversation ID + clean worktree story restart
		if conversationID != "" {
			fmt.Fprintf(stderr, "Error: expected empty conversation ID, got %q\n", conversationID)
			return 1
		}
		if !strings.Contains(fullPrompt, "Execute story") {
			fmt.Fprintf(stderr, "Error: expected Execute story prompt, got %q\n", fullPrompt)
			return 1
		}
		_ = os.WriteFile("story_initialized.txt", []byte("story initialized\n"), 0644)
		_ = procutil.Command("git", "add", ".").Run()
		_ = procutil.Command("git", "commit", "-m", "feat: start story from scratch").Run()
		cid := "mock-conv-clean-uuid-0000-1111-2222"
		fmt.Fprintf(stdout, "Created conversation %s\n", cid)
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			convDir := filepath.Join(home, ".gemini", "antigravity-cli", "conversations")
			_ = os.MkdirAll(convDir, 0755)
			_ = os.WriteFile(filepath.Join(convDir, cid+".db"), []byte("mock-db"), 0644)
		}
		fmt.Fprintln(stdout, "[mock-agy] Story restarted from scratch on clean worktree successfully.")
		return 0

	case "noop_bypass_empty_stdout":
		// Scenario 7: No-Op Bypass with empty stdout / synthetic wrapper summary
		fmt.Fprintln(stdout, "[+] prompt executed via agy (exit 0)")
		return 0

	case "noop_bypass_zero_mutation":
		// Scenario 7b: No-Op Bypass where stdout volume exists but zero git changes occur
		fmt.Fprintln(stdout, "[mock-agy] Executing story flow without performing repository mutations...")
		for i := 1; i <= 6; i++ {
			fmt.Fprintf(stdout, "[mock-agy] Step %d: simulating story execution without git changes...\n", i)
		}
		fmt.Fprintln(stdout, "[mock-agy] Pretending all story gates passed with zero git commits. Exit 0.")
		return 0

	case "legitimate_story_execution":
		// Scenario 8: Legitimate Mutating Story Execution
		fmt.Fprintln(stdout, "[mock-agy] Starting legitimate autonomous story execution for STORY-GOV-3715...")
		fmt.Fprintln(stdout, "[mock-agy] Allocating and verifying story id: STORY-GOV-3715")
		testFile := "legitimate_story_mutation.txt"
		_ = os.WriteFile(testFile, []byte("Verified story mutation content\n"), 0644)
		_ = procutil.Command("git", "add", testFile).Run()
		_ = procutil.Command("git", "commit", "-m", "feat(story): legitimate story execution [STORY-GOV-3715]").Run()
		for i := 1; i <= 6; i++ {
			fmt.Fprintf(stdout, "[mock-agy] Progress checkpoint %d: verified test gates passing...\n", i)
		}
		fmt.Fprintln(stdout, "[mock-agy] Story STORY-GOV-3715 executed successfully with cryptographic proof of mutation.")
		return 0

	case "gemini_web_mcp_sort_prompts_flow":
		// Scenario: Gemini Web -> iazio-mcp -> iazio-harness-api -> iazio-agent -> iazio-harness -> mock-agy (job_54dcf0141811092f)
		// If --dangerously-skip-permissions was NOT passed, reproduce the exact silent exit-0 permission denial from job_54dcf0141811092f.
		if !skipPermissions {
			fmt.Fprintln(stdout, `jetski: no output produced — a tool required the "command" permission that headless mode cannot prompt for, so it was auto-denied. Add an allow-rule under permissions.allow in settings.json (e.g. command(<target>)). Alternatively, re-run with --dangerously-skip-permissions to auto-approve all tools.`)
			return 0
		}
		cid := "c0ffee00-54dc-f014-1811-092f00003838"
		fmt.Fprintf(stdout, "Created conversation %s\n", cid)
		fmt.Fprintln(stdout, "ALLOCATED_STORY_ID: STORY-WEB-3838")
		_ = os.WriteFile(".session_story_id", []byte("STORY-WEB-3838\n"), 0644)
		_ = os.MkdirAll(".autopilot", 0755)
		_ = os.WriteFile(filepath.Join(".autopilot", "state.json"), []byte(`{"story_id":"STORY-WEB-3838","title":"Sort prompts alphabetically in iazio-web /prompts view"}`), 0644)
		_ = os.MkdirAll(filepath.Join("docs", "stories"), 0755)
		storyFile := filepath.Join("docs", "stories", "2026-10-31-web-3838-sort-prompts-alphabetically.story.md")
		_ = os.WriteFile(storyFile, []byte("---\nid: STORY-WEB-3838\ntitle: Sort prompts alphabetically in iazio-web /prompts view\nstatus: DONE\n---\n"), 0644)
		_ = os.MkdirAll(filepath.Join("lib", "screens"), 0755)
		codeFile := filepath.Join("lib", "screens", "prompts_screen.dart")
		_ = os.WriteFile(codeFile, []byte("// Sorted alphabetically A-Z\n"), 0644)
		_ = procutil.Command("git", "add", "docs", "lib").Run()
		_ = procutil.Command("git", "commit", "-m", "feat(iazio-web): sort prompts alphabetically in /prompts view [STORY-WEB-3838]").Run()
		fmt.Fprintln(stdout, "[mock-agy] Executed story STORY-WEB-3838: sorted prompts alphabetically in iazio-web /prompts view and verified unit tests.")
		return 0

	case "job_1350_dirty_worktree_post_exit":
		// Scenario: job-1350 (sch-1348, Prompt: "Execute story STORY-ROOT-245", Kind: "ordinary", Engine: "autopilot" / "mock-agy")
		// Turn 1 exits 0 leaving 4 modified/untracked files without completing story.
		// Turn 2 (in-place continuation) cleans dirty files, marks STORY-ROOT-245 DONE, and commits cleanly.
		stateMarker := filepath.Join(os.TempDir(), "mock_agy_job_1350_turn.tmp")
		if customMarker := os.Getenv("MOCK_AGY_STATE_MARKER"); customMarker != "" {
			stateMarker = customMarker
		}
		_, mErr := os.Stat(stateMarker)
		isTurn2 := (mErr == nil) || strings.Contains(strings.ToLower(fullPrompt), "resume")
		if !isTurn2 {
			_ = os.WriteFile(stateMarker, []byte("turn1"), 0644)
			for i := 1; i <= 4; i++ {
				_ = os.WriteFile(fmt.Sprintf("dirty_1350_%d.txt", i), []byte("uncommitted work\n"), 0644)
			}
			fmt.Fprintln(stdout, "Created conversation c0ffee00-1350-1348-9abc-000000001350")
			fmt.Fprintln(stdout, "[mock-agy] Turn 1 exiting 0 with 4 dirty files in worktree...")
			return 0
		}
		_ = os.Remove(stateMarker)
		for i := 1; i <= 4; i++ {
			_ = os.Remove(fmt.Sprintf("dirty_1350_%d.txt", i))
		}
		_ = os.MkdirAll(filepath.Join("docs", "stories"), 0755)
		storyFile := filepath.Join("docs", "stories", "story-root-245.story.md")
		_ = os.WriteFile(storyFile, []byte("---\nid: STORY-ROOT-245\ntitle: Fix dirty worktree post exit\nstatus: DONE\n---\n"), 0644)
		_ = procutil.Command("git", "add", ".").Run()
		_ = procutil.Command("git", "commit", "-m", "feat: complete STORY-ROOT-245 cleanly").Run()
		fmt.Fprintln(stdout, "[mock-agy] Turn 2 cleaned dirty worktree and completed STORY-ROOT-245.")
		return 0

	case "buffered_stdout_brain_transcript_streaming":
		// Scenario (job_3ffa5d0be1649f0e / STORY-GOV-3851):
		// Real `agy --print --log-file` buffers stdout until exit, writing live progress only to
		// --log-file and ~/.gemini/antigravity-cli/brain/<convID>/.system_generated/logs/transcript.jsonl.
		cid := conversationID
		if cid == "" {
			cid = "ba042127-79e5-4cb2-b35c-5d47145ef4fa"
		}
		if logFileWriter != nil {
			fmt.Fprintf(logFileWriter, "Created conversation %s\n", cid)
		}
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			convDir := filepath.Join(home, ".gemini", "antigravity-cli", "conversations")
			_ = os.MkdirAll(convDir, 0755)
			_ = os.WriteFile(filepath.Join(convDir, cid+".db"), []byte("mock-db"), 0644)

			logsDir := filepath.Join(home, ".gemini", "antigravity-cli", "brain", cid, ".system_generated", "logs")
			_ = os.MkdirAll(logsDir, 0755)
			transcriptPath := filepath.Join(logsDir, "transcript.jsonl")
			step1 := `{"step_index":1,"type":"USER_INPUT","content":"/autonomous-story-execution STORY-GOV-3850"}` + "\n"
			step2 := `{"step_index":2,"type":"PLANNER_RESPONSE","thinking":"Inspecting repository live output pipeline","tool_calls":[{"name":"read_file","arguments":{"Path":"/home/romeo/work/meta-repo-02/AGENTS.md"}},{"name":"run_command","arguments":{"CommandLine":"autopilot status","toolSummary":"Check autopilot status"}}]}` + "\n"
			_ = os.WriteFile(transcriptPath, []byte(step1+step2), 0644)

			_ = os.WriteFile(".session_story_id", []byte("STORY-GOV-3850\n"), 0644)
			waitDur := 2200 * time.Millisecond
			if os.Getenv("MOCK_AGY_FAST") == "1" {
				waitDur = 100 * time.Millisecond
			}
			time.Sleep(waitDur)

			step3 := `{"step_index":3,"type":"PLANNER_RESPONSE","content":"Completed STORY-GOV-3850 implementation and verified all gates."}` + "\n"
			if tf, err := os.OpenFile(transcriptPath, os.O_WRONLY|os.O_APPEND, 0644); err == nil {
				_, _ = tf.WriteString(step3)
				_ = tf.Close()
			}
		}
		_ = os.MkdirAll(filepath.Join("docs", "stories"), 0755)
		storyFile := filepath.Join("docs", "stories", "2026-10-31-gov-3850-live-stream.story.md")
		_ = os.WriteFile(storyFile, []byte("---\nid: STORY-GOV-3850\ntitle: Verify live transcript streaming\nstatus: DONE\n---\n"), 0644)
		_ = procutil.Command("git", "add", "docs").Run()
		_ = procutil.Command("git", "commit", "-m", "feat(gov-3850): verify live transcript streaming [STORY-GOV-3850]").Run()
		return 0

	case "background_drain_pid_liveness_and_auto_resume":
		// Scenario (job_3ffa5d0be1649f0e / STORY-GOV-3852):
		// Reproduces job_3ffa5d0be1649f0e on host=debian repo=/home/romeo/work/meta-repo-02
		// Bound Story: STORY-GOV-3850, Active Conversation: ba042127-c63f-4b1e-a42e-95b307e61bbc.
		// Turn 1:
		//  - Resolves conversation ba042127-c63f-4b1e-a42e-95b307e61bbc and binds STORY-GOV-3850 (status: IN_PROGRESS)
		//  - Emits active chatty transcript steps while tool_pid is alive across the initial timeout window (verifying timeout deferral)
		//  - Then logs "WaitForConversationFullyIdle: root agent idle; waiting up to 1h0m0s for 1 background task(s)" and blocks on a background child
		// Turn 2 (auto-resumed with --conversation=ba042127-c63f-4b1e-a42e-95b307e61bbc):
		//  - Completes STORY-GOV-3850 (status: DONE), commits cleanly, and exits 0.
		cid := "ba042127-c63f-4b1e-a42e-95b307e61bbc"
		storyFile := filepath.Join("docs", "stories", "2026-10-31-gov-3850-sort-prompts-alphabetically-iazio-web.story.md")
		if conversationID == cid || strings.Contains(strings.ToLower(fullPrompt), "resume and complete") {
			if home, err := os.UserHomeDir(); err == nil && home != "" {
				logsDir := filepath.Join(home, ".gemini", "antigravity-cli", "brain", cid, ".system_generated", "logs")
				_ = os.MkdirAll(logsDir, 0755)
				transcriptPath := filepath.Join(logsDir, "transcript.jsonl")
				step4 := `{"step_index":4,"type":"PLANNER_RESPONSE","content":"Auto-resumed conversation ba042127-c63f-4b1e-a42e-95b307e61bbc and completed STORY-GOV-3850."}` + "\n"
				if tf, err := os.OpenFile(transcriptPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644); err == nil {
					_, _ = tf.WriteString(step4)
					_ = tf.Close()
				}
			}
			_ = os.MkdirAll(filepath.Join("docs", "stories"), 0755)
			_ = os.WriteFile(storyFile, []byte("---\nid: STORY-GOV-3850\ntitle: Sort Prompts Alphabetically in iazio-web (/prompts) and Underlying API\nstatus: DONE\n---\n"), 0644)
			_ = procutil.Command("git", "add", "docs").Run()
			_ = procutil.Command("git", "commit", "-m", "feat(gov-3850): complete STORY-GOV-3850 after auto-resume [STORY-GOV-3850]").Run()
			fmt.Fprintf(stdout, "Resumed conversation %s\n", cid)
			fmt.Fprintln(stdout, "[mock-agy] Auto-resumed conversation ba042127-c63f-4b1e-a42e-95b307e61bbc and completed STORY-GOV-3850.")
			return 0
		}

		// Turn 1:
		if logFileWriter != nil {
			fmt.Fprintf(logFileWriter, "Created conversation %s\n", cid)
		} else {
			fmt.Fprintf(stdout, "Created conversation %s\n", cid)
		}
		_ = os.WriteFile(".session_story_id", []byte("STORY-GOV-3850\n"), 0644)
		_ = os.MkdirAll(filepath.Join("docs", "stories"), 0755)
		_ = os.WriteFile(storyFile, []byte("---\nid: STORY-GOV-3850\ntitle: Sort Prompts Alphabetically in iazio-web (/prompts) and Underlying API\nstatus: IN_PROGRESS\n---\n"), 0644)

		var transcriptPath string
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			convDir := filepath.Join(home, ".gemini", "antigravity-cli", "conversations")
			_ = os.MkdirAll(convDir, 0755)
			_ = os.WriteFile(filepath.Join(convDir, cid+".db"), []byte("mock-db"), 0644)

			logsDir := filepath.Join(home, ".gemini", "antigravity-cli", "brain", cid, ".system_generated", "logs")
			_ = os.MkdirAll(logsDir, 0755)
			transcriptPath = filepath.Join(logsDir, "transcript.jsonl")
			step1 := `{"step_index":1,"type":"USER_INPUT","content":"/autonomous-story-execution STORY-GOV-3850"}` + "\n"
			step2 := `{"step_index":2,"type":"PLANNER_RESPONSE","thinking":"Implementing STORY-GOV-3850 on debian meta-repo-02","tool_calls":[{"name":"run_command","arguments":{"CommandLine":"go test ./...","toolSummary":"Run unit tests"}}]}` + "\n"
			_ = os.WriteFile(transcriptPath, []byte(step1+step2), 0644)
		}

		// Spawn an active child process and emit chatty transcript steps across 480ms so tool_pid liveness defers a 300ms timeout
		stepSleep := 120 * time.Millisecond
		drainSleep := 30 * time.Second
		if os.Getenv("MOCK_AGY_UNIT") == "1" {
			stepSleep = 5 * time.Millisecond
			drainSleep = 5 * time.Millisecond
		} else {
			childCmd := procutil.Command("sleep", "30")
			_ = childCmd.Start()
		}
		for i := 0; i < 4; i++ {
			time.Sleep(stepSleep)
			if transcriptPath != "" {
				step := fmt.Sprintf(`{"step_index":%d,"type":"PLANNER_RESPONSE","content":"Executing active task step %d while tool_pid is live"}`+"\n", 10+i, i+1)
				if tf, err := os.OpenFile(transcriptPath, os.O_WRONLY|os.O_APPEND, 0644); err == nil {
					_, _ = tf.WriteString(step)
					_ = tf.Close()
				}
			}
		}

		// Now enter WaitForConversationFullyIdle background drain
		drainLine := "2026/10/31 17:48:00 WaitForConversationFullyIdle: root agent idle; waiting up to 1h0m0s for 1 background task(s)\n"
		if logFileWriter != nil {
			_, _ = io.WriteString(logFileWriter, drainLine)
		} else {
			_, _ = io.WriteString(stdout, drainLine)
		}
		// Block until supervisor reaps background drain
		time.Sleep(drainSleep)
		return 0

	case "job_110f47f09551baba_ase_complete_bg_drain_and_pointer_story":
		// Scenario (job_110f47f09551baba / STORY-GOV-3874):
		// Reproduces job_110f47f09551baba where:
		// 1. Primary story STORY-GOV-3873 and pointer story STORY-GOV-POINTERS-20261101-044815 both complete (status: DONE) and are committed cleanly.
		// 2. .session_story_id is overwritten by the pointer bump helper with STORY-GOV-POINTERS-20261101-044815.
		// 3. Brain transcript step #321 emits:
		//    "Autonomous story execution is complete for both STORY-GOV-3873 and STORY-GOV-POINTERS-20261101-044815"
		// 4. agy enters "WaitForConversationFullyIdle: root agent idle; waiting up to 1h0m0s for 1 background task(s)" without flushing stdout and blocks.
		// 5. prompt_runner must detect ASE completion during background drain (without needing a redundant Turn 2 auto-resume or reporting BACKGROUND_DRAIN_TIMEOUT)
		//    and preserve STORY-GOV-3873 as the primary StoryID.
		cid := conversationID
		if cid == "" {
			cid = "18f40eb1-d86f-41bf-a69f-7d9de6536cb6"
		}
		if logFileWriter != nil {
			fmt.Fprintf(logFileWriter, "Created conversation %s\n", cid)
		} else {
			fmt.Fprintf(stdout, "Created conversation %s\n", cid)
		}
		_ = os.MkdirAll(filepath.Join("docs", "stories"), 0755)
		primaryStoryFile := filepath.Join("docs", "stories", "2026-11-01-gov-3873-prompts-alphabetical-sorting-and-ui-verification.story.md")
		pointerStoryFile := filepath.Join("docs", "stories", "2026-11-01-gov-pointers-20261101-044815.story.md")
		_ = os.WriteFile(primaryStoryFile, []byte("---\nid: STORY-GOV-3873\ntitle: Prompts Alphabetical Sorting and UI Verification\nstatus: DONE\n---\n"), 0644)
		_ = os.WriteFile(pointerStoryFile, []byte("---\nid: STORY-GOV-POINTERS-20261101-044815\ntitle: Reconcile Submodule Pointers\nstatus: DONE\n---\n"), 0644)
		_ = procutil.Command("git", "add", "docs").Run()
		_ = procutil.Command("git", "commit", "-m", "chore(pointers): reconcile submodule pointers [STORY-GOV-3873]").Run()

		// Simulate pointer bump overwriting .session_story_id while .autopilot/ase-state.json also exists
		_ = os.WriteFile(".session_story_id", []byte("STORY-GOV-POINTERS-20261101-044815\n"), 0644)
		_ = os.MkdirAll(".autopilot", 0755)
		_ = os.WriteFile(filepath.Join(".autopilot", "ase-state.json"), []byte(`{"story_id":"STORY-GOV-3873","title":"Prompts Alphabetical Sorting and UI Verification","story_file":"docs/stories/2026-11-01-gov-3873-prompts-alphabetical-sorting-and-ui-verification.story.md"}`), 0644)

		if home, err := os.UserHomeDir(); err == nil && home != "" {
			convDir := filepath.Join(home, ".gemini", "antigravity-cli", "conversations")
			_ = os.MkdirAll(convDir, 0755)
			_ = os.WriteFile(filepath.Join(convDir, cid+".db"), []byte("mock-db"), 0644)

			logsDir := filepath.Join(home, ".gemini", "antigravity-cli", "brain", cid, ".system_generated", "logs")
			_ = os.MkdirAll(logsDir, 0755)
			transcriptPath := filepath.Join(logsDir, "transcript.jsonl")
			step1 := `{"step_index":1,"type":"USER_INPUT","content":"/autonomous-story-execution STORY-GOV-3873"}` + "\n"
			step321 := `{"step_index":321,"type":"PLANNER_RESPONSE","content":"Autonomous story execution is complete for both [` + "`STORY-GOV-3873`" + `](file:///home/romeo/work/meta-repo/docs/stories/2026-11-01-gov-3873-prompts-alphabetical-sorting-and-ui-verification.story.md) and [` + "`STORY-GOV-POINTERS-20261101-044815`" + `](file:///home/romeo/work/meta-repo/docs/stories/2026-11-01-gov-pointers-20261101-044815.story.md)."}` + "\n"
			_ = os.WriteFile(transcriptPath, []byte(step1+step321), 0644)
		}

		drainLine := "2026/11/01 05:39:45 WaitForConversationFullyIdle: root agent idle; waiting up to 1h0m0s for 1 background task(s)\n"
		if logFileWriter != nil {
			_, _ = io.WriteString(logFileWriter, drainLine)
		} else {
			_, _ = io.WriteString(stdout, drainLine)
		}
		drainSleep := 30 * time.Second
		if os.Getenv("MOCK_AGY_UNIT") == "1" {
			drainSleep = 5 * time.Millisecond
		}
		time.Sleep(drainSleep)
		return 0

	case "job_cbbb1363993215f9_wave4_pulled_done_story_override":
		// Scenario (job_cbbb1363993215f9 / Wave 4 story.create_and_execute):
		// 1. At turn start, git pull updates an older DONE story file docs/stories/2026-11-04-gov-5144-resilient-user-services.story.md
		//    before .session_story_id is written.
		// 2. After a brief pause (allowing syncTurnTelemetry to poll), the agent allocates STORY-GOV-5154,
		//    writes STORY-GOV-5154 to .session_story_id, creates docs/stories/2026-11-05-gov-5154-canonical-ai-hygiene-and-skills.story.md,
		//    marks it DONE, and commits cleanly.
		cid := conversationID
		if cid == "" {
			cid = "884ed49e-4331-4f9a-919d-28bd2ffde6a1"
		}
		if logFileWriter != nil {
			fmt.Fprintf(logFileWriter, "Created conversation %s\n", cid)
		} else {
			fmt.Fprintf(stdout, "Created conversation %s\n", cid)
		}
		_ = os.MkdirAll(filepath.Join("docs", "stories"), 0755)
		pulledDoneStory := filepath.Join("docs", "stories", "2026-11-04-gov-5144-resilient-user-services.story.md")
		_ = os.WriteFile(pulledDoneStory, []byte("---\nid: STORY-GOV-5144\ntitle: Resilient User Services\nstatus: DONE\n---\n"), 0644)
		_ = procutil.Command("git", "add", "docs").Run()
		_ = procutil.Command("git", "commit", "-m", "chore: simulate git pull of STORY-GOV-5144").Run()

		// Pause briefly so syncTurnTelemetry observes the pulled DONE story before .session_story_id exists
		pause := 150 * time.Millisecond
		if os.Getenv("MOCK_AGY_UNIT") == "1" {
			pause = 25 * time.Millisecond
		}
		time.Sleep(pause)

		// Now allocate STORY-GOV-5154 in .session_story_id and docs/stories/
		_ = os.WriteFile(".session_story_id", []byte("STORY-GOV-5154\n"), 0644)
		newStory := filepath.Join("docs", "stories", "2026-11-05-gov-5154-canonical-ai-hygiene-and-skills.story.md")
		_ = os.WriteFile(newStory, []byte("---\nid: STORY-GOV-5154\ntitle: Wave 4 Canonical AI Hygiene and Skills\nstatus: IN_PROGRESS\n---\n"), 0644)
		time.Sleep(pause)

		// Mark STORY-GOV-5154 DONE and commit cleanly
		_ = os.WriteFile(newStory, []byte("---\nid: STORY-GOV-5154\ntitle: Wave 4 Canonical AI Hygiene and Skills\nstatus: DONE\n---\n"), 0644)
		_ = procutil.Command("git", "add", "docs").Run()
		_ = procutil.Command("git", "commit", "-m", "feat(ai): complete Wave 4 canonical AI hygiene [STORY-GOV-5154]").Run()
		fmt.Fprintf(stdout, "Completed Wave 4 story STORY-GOV-5154 cleanly.\n")
		return 0

	case "job_38eeeb536a5a7130_crash_resume_and_new_conv_fallback":
		// Scenario (job_38eeeb536a5a7130 mutual supervision & crash resilience):
		// Turn 1: Starts conversation 1aee4b3a-fedd-4c71-a7b5-4b816e7664a5, binds STORY-IAZIO-0185, and crashes (exit 1).
		// Turn 2: Resumes conversation 1aee4b3a-fedd-4c71-a7b5-4b816e7664a5, and crashes again with a corrupted session error (exit 1).
		// Turn 3: iazio-harness falls back to a NEW conversation (empty conversationID), completes STORY-IAZIO-0185 (status: DONE), and commits cleanly (exit 0).
		stateMarker := filepath.Join(os.TempDir(), "mock_agy_38eeeb53_turn.tmp")
		if customMarker := os.Getenv("MOCK_AGY_STATE_MARKER"); customMarker != "" {
			stateMarker = customMarker
		}
		rawTurn, _ := os.ReadFile(stateMarker)
		turnState := strings.TrimSpace(string(rawTurn))
		initialConv := "1aee4b3a-fedd-4c71-a7b5-4b816e7664a5"
		fallbackConv := "2bff5c4b-a111-4c71-a7b5-999988887777"
		storyFile := filepath.Join("docs", "stories", "2026-11-05-iazio-0185-telemetry-and-harvester.story.md")

		if turnState == "" {
			_ = os.WriteFile(stateMarker, []byte("turn1_crashed"), 0644)
			if logFileWriter != nil {
				fmt.Fprintf(logFileWriter, "Created conversation %s\n", initialConv)
			} else {
				fmt.Fprintf(stdout, "Created conversation %s\n", initialConv)
			}
			_ = os.WriteFile(".session_story_id", []byte("STORY-IAZIO-0185\n"), 0644)
			_ = os.MkdirAll(filepath.Join("docs", "stories"), 0755)
			_ = os.WriteFile(storyFile, []byte("---\nid: STORY-IAZIO-0185\ntitle: Telemetry and Harvester Fixes\nstatus: IN_PROGRESS\n---\n"), 0644)
			fmt.Fprintln(stderr, "[mock-agy] Turn 1 unexpected crash while executing STORY-IAZIO-0185.")
			return 1
		}
		if turnState == "turn1_crashed" {
			_ = os.WriteFile(stateMarker, []byte("turn2_corrupted"), 0644)
			fmt.Fprintf(stderr, "Error: database disk image is malformed: conversation session corrupted for %s\n", conversationID)
			return 1
		}
		_ = os.Remove(stateMarker)
		if conversationID != "" {
			fmt.Fprintf(stderr, "Error: expected new conversation fallback (empty conversationID), got %q\n", conversationID)
			return 1
		}
		if logFileWriter != nil {
			fmt.Fprintf(logFileWriter, "Created conversation %s\n", fallbackConv)
		} else {
			fmt.Fprintf(stdout, "Created conversation %s\n", fallbackConv)
		}
		_ = os.MkdirAll(filepath.Join("docs", "stories"), 0755)
		_ = os.WriteFile(storyFile, []byte("---\nid: STORY-IAZIO-0185\ntitle: Telemetry and Harvester Fixes\nstatus: DONE\n---\n"), 0644)
		_ = procutil.Command("git", "add", "docs").Run()
		_ = procutil.Command("git", "commit", "-m", "feat(iazio-0185): complete STORY-IAZIO-0185 in new conversation fallback [STORY-IAZIO-0185]").Run()
		fmt.Fprintf(stdout, "[mock-agy] Completed STORY-IAZIO-0185 in new conversation %s.\n", fallbackConv)
		return 0

	case "job_bbd74e1ad21fbc40_terminal_planner_response_no_bg_drain_marker",
		"job_bbd74e1ad21fbc40_terminal_planner_with_mcp_child":
		// Scenario (job_bbd74e1ad21fbc40 / STORY-GOV-5177):
		// Reproduces job_bbd74e1ad21fbc40 where:
		// 1. Primary story STORY-GOV-5176 is marked status: DONE in docs/stories/ and committed cleanly, while
		//    conversation output also mentions an older historical story ID (STORY-GOV-5175).
		// 2. Brain transcript step #135 is a terminal PLANNER_RESPONSE (status: "DONE", tool_calls: []) containing
		//    the final "## CRAFT Closeout" and "## Fleet Git Status Summary".
		// 3. agy --print NEVER emits "WaitForConversationFullyIdle" (inBgDrain == false) and hangs after step #135
		//    (optionally with a long-lived "autopilot-mcp serve" child process running under agy).
		// 4. CLIPromptRunner must detect terminal PLANNER_RESPONSE + ASE worktree completion after bgDrainGrace,
		//    ignore MCP server child processes, reap the hung agy process group, and return Status="completed", ExitCode=0, StoryID="STORY-GOV-5176".
		cid := conversationID
		if cid == "" {
			cid = "0092d1cb-cc35-41ce-9143-a480dca5e283"
		}
		if logFileWriter != nil {
			fmt.Fprintf(logFileWriter, "Created conversation %s\n", cid)
		} else {
			fmt.Fprintf(stdout, "Created conversation %s\n", cid)
		}
		_ = os.MkdirAll(filepath.Join("docs", "stories"), 0755)
		storyFile := filepath.Join("docs", "stories", "2026-11-06-gov-5176-remediate-all-10-findings.story.md")
		_ = os.WriteFile(storyFile, []byte("---\nid: STORY-GOV-5176\ntitle: Remediate All 10 Findings (F-01 through F-10) from Canonical AI, Autopilot, and Iazio Deep Review\nstatus: DONE\n---\n"), 0644)
		_ = procutil.Command("git", "add", "docs").Run()
		_ = procutil.Command("git", "commit", "-m", "feat(gov-5176): remediate all 10 findings [STORY-GOV-5176]").Run()
		_ = os.WriteFile(".session_story_id", []byte("STORY-GOV-5176\n"), 0644)

		if home, err := os.UserHomeDir(); err == nil && home != "" {
			convDir := filepath.Join(home, ".gemini", "antigravity-cli", "conversations")
			_ = os.MkdirAll(convDir, 0755)
			_ = os.WriteFile(filepath.Join(convDir, cid+".db"), []byte("mock-db"), 0644)

			logsDir := filepath.Join(home, ".gemini", "antigravity-cli", "brain", cid, ".system_generated", "logs")
			_ = os.MkdirAll(logsDir, 0755)
			transcriptPath := filepath.Join(logsDir, "transcript.jsonl")
			step1 := `{"step_index":1,"type":"USER_INPUT","status":"DONE","content":"Follow up after STORY-GOV-5175 and execute STORY-GOV-5176"}` + "\n"
			step135 := `{"step_index":135,"type":"PLANNER_RESPONSE","status":"DONE","content":"All 10 findings for STORY-GOV-5176 have been remediated and verified.\n\n## CRAFT Closeout\n\n## Fleet Git Status Summary\n"}` + "\n"
			_ = os.WriteFile(transcriptPath, []byte(step1+step135), 0644)
		}

		if scenario == "job_bbd74e1ad21fbc40_terminal_planner_with_mcp_child" {
			// Spawn a genuine non-exec'd child binary named "autopilot-mcp" with arg "serve"
			if selfExe, err := os.Executable(); err == nil && selfExe != "" {
				if mcpDir, err := os.MkdirTemp("", "mock-mcp-bin-*"); err == nil {
					defer os.RemoveAll(mcpDir)
					mcpBinName := "autopilot-mcp"
					if strings.HasSuffix(strings.ToLower(selfExe), ".exe") {
						mcpBinName += ".exe"
					}
					mcpBinPath := filepath.Join(mcpDir, mcpBinName)
					if err := os.Symlink(selfExe, mcpBinPath); err != nil {
						if b, rErr := os.ReadFile(selfExe); rErr == nil {
							_ = os.WriteFile(mcpBinPath, b, 0o755)
						}
					}
					mcpChild := procutil.Command(mcpBinPath, "serve")
					_ = mcpChild.Start()
					defer func() {
						if mcpChild != nil && mcpChild.Process != nil {
							_ = mcpChild.Process.Kill()
						}
					}()
				}
			}
		}

		// Hang WITHOUT printing WaitForConversationFullyIdle
		hangSleep := 30 * time.Second
		if os.Getenv("MOCK_AGY_UNIT") == "1" {
			hangSleep = 5 * time.Millisecond
		}
		time.Sleep(hangSleep)
		return 0

	case "sch_1569_fallback_new_conversation_resume":
		// Reproduces sch-1569 / job-1570 (STORY-GOV-5206):
		// 1. Rejects poisoned conversation 287e8a2b-1e95-4aa1-b2f9-9b151134c574 from stale job-1408 checkpoint.
		// 2. On initial fallback_new_conversation run (conversationID == ""), creates 8d5d3c3d-1eb6-4517-a64b-df5b27cff909.
		// 3. On auto-resume run, requires conversationID == "8d5d3c3d-1eb6-4517-a64b-df5b27cff909" (from job.ActiveConversationID).
		if conversationID == "287e8a2b-1e95-4aa1-b2f9-9b151134c574" {
			fmt.Fprintln(stderr, "[mock-agy] FATAL: poisoned conversation 287e8a2b-1e95-4aa1-b2f9-9b151134c574 from job-1408 leaked into job-1570")
			return 2
		}
		targetConv := "8d5d3c3d-1eb6-4517-a64b-df5b27cff909"
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			convDir := filepath.Join(home, ".gemini", "antigravity-cli", "conversations")
			_ = os.MkdirAll(convDir, 0755)
			_ = os.WriteFile(filepath.Join(convDir, targetConv+".db"), []byte("mock-db"), 0644)
			logsDir := filepath.Join(home, ".gemini", "antigravity-cli", "brain", targetConv, ".system_generated", "logs")
			_ = os.MkdirAll(logsDir, 0755)
			transcriptPath := filepath.Join(logsDir, "transcript.jsonl")
			step := `{"step_index":1,"type":"PLANNER_RESPONSE","status":"DONE","content":"Executing STORY-ROOT-45276 on fallback conversation"}` + "\n"
			_ = os.WriteFile(transcriptPath, []byte(step), 0644)
		}
		if conversationID == "" {
			fmt.Fprintf(stdout, "Created conversation %s\n", targetConv)
			fmt.Fprintln(stdout, "[mock-agy] Initial fallback_new_conversation turn started")
			return 0
		}
		if conversationID != targetConv {
			fmt.Fprintf(stderr, "[mock-agy] unexpected resume conversation %q, want %q\n", conversationID, targetConv)
			return 1
		}
		fmt.Fprintf(stdout, "Resumed conversation %s\n", targetConv)
		fmt.Fprintln(stdout, "[mock-agy] Resumed fallback conversation cleanly for job-1570")
		return 0

	case "pc1_meta_repo_04_alternates_rebase_and_adhoc_ase_brain_flow":
		// Reproduces STORY-IAZIO-0201 (D1-D5):
		// Phase "in_flight_phase0": writes a realistic agy brain transcript.jsonl with JSON-escaped Windows
		// workspace path ("C:\\\\...\\\\meta-repo-04\\\\AGENTS.md") and an active PLANNER_RESPONSE tool_call
		// before .session_story_id or story branch exists.
		// Phase "complete_story": marks STORY-IAZIO-0201 DONE with full DoD/Reviews, commits it on main,
		// and appends a terminal PLANNER_RESPONSE without tool_calls.
		cid := conversationID
		if cid == "" {
			cid = "1f2e5efc-e041-4c89-b25a-fe32389d66cd"
		}
		brainRoot := strings.TrimSpace(os.Getenv("IAZIO_BRAIN_DIR"))
		if brainRoot == "" {
			if home, err := os.UserHomeDir(); err == nil && home != "" {
				brainRoot = filepath.Join(home, ".gemini", "antigravity-cli", "brain")
			}
		}
		repoPath := strings.TrimSpace(os.Getenv("MOCK_AGY_REPO_PATH"))
		if repoPath == "" {
			if wd, err := os.Getwd(); err == nil {
				repoPath = wd
			}
		}
		logsDir := filepath.Join(brainRoot, cid, ".system_generated", "logs")
		_ = os.MkdirAll(logsDir, 0o755)
		transcriptPath := filepath.Join(logsDir, "transcript.jsonl")

		escapedAgentsPath := strings.ReplaceAll(filepath.Join(repoPath, "AGENTS.md"), `\`, `\\`)
		step0 := `{"step_index":0,"type":"USER_INPUT","status":"DONE","content":"<USER_REQUEST>/autonomous-story-execution deep review and fix reason why this repo reports in iazio-web \"Detected In-Flight Story: STORY-GOV-3404\" and 0 adhoc ASE.</USER_REQUEST>"}` + "\n"
		step1 := fmt.Sprintf(`{"step_index":1,"type":"PLANNER_RESPONSE","status":"DONE","tool_calls":[{"name":"read_file","args":{"Path":%q}}]}`, escapedAgentsPath) + "\n"

		phase := strings.TrimSpace(os.Getenv("MOCK_AGY_PHASE"))
		if phase == "complete_story" {
			storiesDir := filepath.Join(repoPath, "docs", "stories")
			_ = os.MkdirAll(storiesDir, 0o755)
			storyPath := filepath.Join(storiesDir, "2026-11-09-iazio-0201-windows-wsl-alternates-rebase-and-adhoc-ase-detection.story.md")
			storyMarkdown := "---\nid: STORY-IAZIO-0201\ntitle: Fix Windows/WSL Alternates, Stranded Maintenance Rebase, and Ad-Hoc ASE Detection\nstatus: DONE\n---\n## 5. Definition of Done\n- [x] All D1-D5 discoveries remediated and verified\n## 7. Reviews\n| Tool | Status |\n|---|---|\n| tdd | PASS |\n"
			_ = os.WriteFile(storyPath, []byte(storyMarkdown), 0o644)
			_ = os.WriteFile(filepath.Join(repoPath, ".session_story_id"), []byte("STORY-IAZIO-0201\n"), 0o644)
			_ = procutil.Command("git", "-C", repoPath, "add", "docs").Run()
			_ = procutil.Command("git", "-C", repoPath, "commit", "-m", "fix(iazio-0201): remediate D1-D5 discoveries [STORY-IAZIO-0201]").Run()

			step2 := `{"step_index":2,"type":"PLANNER_RESPONSE","status":"DONE","content":"## CRAFT Closeout\nAll D1-D5 discoveries fixed and verified for STORY-IAZIO-0201."}` + "\n"
			_ = os.WriteFile(transcriptPath, []byte(step0+step1+step2), 0o644)
			fmt.Fprintf(stdout, "[mock-agy] Completed STORY-IAZIO-0201 and wrote terminal PLANNER_RESPONSE for %s\n", cid)
			return 0
		}

		_ = os.WriteFile(transcriptPath, []byte(step0+step1), 0o644)
		fmt.Fprintf(stdout, "Created conversation %s\n", cid)
		fmt.Fprintf(stdout, "[mock-agy] Active Phase 0 brain transcript written for conversation %s\n", cid)
		return 0

	default:
		// Default simple execution
		if conversationID != "" {
			fmt.Fprintf(stdout, "Resumed conversation %s\n", conversationID)
		} else {
			cid := "mock-conv-0000-1111-2222-33334444"
			fmt.Fprintf(stdout, "Created conversation %s\n", cid)
			if home, err := os.UserHomeDir(); err == nil && home != "" {
				convDir := filepath.Join(home, ".gemini", "antigravity-cli", "conversations")
				_ = os.MkdirAll(convDir, 0755)
				dbPath := filepath.Join(convDir, cid+".db")
				_ = os.WriteFile(dbPath, []byte("mock-db"), 0644)
			}
		}
		fmt.Fprintf(stdout, "[mock-agy] Executed prompt: %s\n", fullPrompt)
		return 0
	}
}
