package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	os.Exit(run(os.Args[1:], strings.TrimSpace(os.Getenv("MOCK_AGY_SCENARIO")), os.Stdout, os.Stderr))
}

func run(args []string, scenario string, stdout, stderr io.Writer) int {
	var conversationID string
	var printPrompt string
	var model string
	var agent string
	var logFile string
	var positional []string

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
		case strings.HasPrefix(arg, "--print="):
			printPrompt = strings.TrimPrefix(arg, "--print=")
		case arg == "--print":
			if i+1 < len(args) {
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
		case arg == "--print-timeout" || strings.HasPrefix(arg, "--print-timeout="):
			if !strings.Contains(arg, "=") && i+1 < len(args) {
				i++
			}
		case arg == "--dangerously-skip-permissions", arg == "--disable-slash-commands":
			// Boolean flags
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

	// Ensure conversation DB directory exists if conversationID is provided
	if conversationID != "" {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			convDir := filepath.Join(home, ".gemini", "antigravity-cli", "conversations")
			_ = os.MkdirAll(convDir, 0755)
			dbPath := filepath.Join(convDir, conversationID+".db")
			_ = os.WriteFile(dbPath, []byte("mock-db"), 0644)
		}
	}

	if logFile != "" {
		_ = os.MkdirAll(filepath.Dir(logFile), 0755)
		if f, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644); err == nil {
			defer f.Close()
			stdout = io.MultiWriter(stdout, f)
			stderr = io.MultiWriter(stderr, f)
		}
	}

	switch scenario {
	case "ordinary_prompt_with_mcp_daemons":
		// Scenario 1: Print normal output, spawn persistent background child daemon that keeps
		// pipes inherited, and exit 0 immediately.
		fmt.Fprintf(stdout, "[mock-agy] Executing ordinary prompt (model: %s, agent: %s)...\n", model, agent)
		fmt.Fprintf(stdout, "[mock-agy] Prompt: %s\n", fullPrompt)
		fmt.Fprintln(stdout, "[mock-agy] Initializing background MCP daemons: autopilot-mcp, chrome-devtools-mcp...")

		// Spawn background sleeper that holds pipes open
		cmd := exec.Command("sleep", "60")
		cmd.Stdout = stdout
		cmd.Stderr = stderr
		_ = cmd.Start()

		fmt.Fprintln(stdout, "[mock-agy] Execution completed successfully. Primary engine PID exiting 0.")
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
			_ = exec.Command("git", "add", "ase_committed_feature.txt").Run()
			_ = exec.Command("git", "commit", "-m", "feat: complete ASE story flow").Run()
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
			_ = exec.Command("git", "add", "bg_hang_recovered_commit.txt").Run()
			_ = exec.Command("git", "commit", "-m", "chore: finish tasks after bg hang").Run()
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
		_ = exec.Command("git", "add", testFile).Run()
		_ = exec.Command("git", "commit", "-m", "feat(story): legitimate story execution [STORY-GOV-3715]").Run()
		for i := 1; i <= 6; i++ {
			fmt.Fprintf(stdout, "[mock-agy] Progress checkpoint %d: verified test gates passing...\n", i)
		}
		fmt.Fprintln(stdout, "[mock-agy] Story STORY-GOV-3715 executed successfully with cryptographic proof of mutation.")
		return 0

	default:
		// Default simple execution
		if conversationID != "" {
			fmt.Fprintf(stdout, "Resumed conversation %s\n", conversationID)
		} else {
			cid := "mock-conv-0000-1111-2222-3333"
			fmt.Fprintf(stdout, "Created conversation %s\n", cid)
		}
		fmt.Fprintf(stdout, "[mock-agy] Executed prompt: %s\n", fullPrompt)
		if logFile != "" {
			_ = os.WriteFile(logFile, []byte("[mock-agy] Log file output\n"), 0644)
		}
		return 0
	}
}
