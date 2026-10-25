package engines

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// BackgroundTaskInfo describes an active or reaped child/background process.
type BackgroundTaskInfo struct {
	TaskID            string    `json:"task_id,omitempty"`
	PID               int       `json:"pid"`
	Command           string    `json:"command"`
	StartedAt         time.Time `json:"started_at,omitempty"`
	LastOutputAt      time.Time `json:"last_output_at,omitempty"`
	LastOutputSnippet string    `json:"last_output_snippet,omitempty"`
	IdleDurationSec   float64   `json:"idle_duration_sec,omitempty"`
}

// FindChildProcesses enumerates all direct and indirect children of the given parent PID.
func FindChildProcesses(parentPID int) []BackgroundTaskInfo {
	if parentPID <= 0 {
		return nil
	}
	if runtime.GOOS == "windows" {
		return findChildProcessesWindows(parentPID)
	}
	return findChildProcessesUnix(parentPID)
}

func findChildProcessesUnix(parentPID int) []BackgroundTaskInfo {
	var results []BackgroundTaskInfo

	// Fast path: inspect /proc on Linux
	if _, err := os.Stat("/proc"); err == nil {
		entries, err := os.ReadDir("/proc")
		if err == nil {
			type procEntry struct {
				pid  int
				ppid int
			}
			var allProcs []procEntry
			for _, entry := range entries {
				if !entry.IsDir() {
					continue
				}
				pid, err := strconv.Atoi(entry.Name())
				if err != nil {
					continue
				}
				statPath := filepath.Join("/proc", entry.Name(), "stat")
				statBytes, err := os.ReadFile(statPath)
				if err != nil {
					continue
				}
				content := string(statBytes)
				rparen := strings.LastIndex(content, ")")
				if rparen < 0 || rparen+2 >= len(content) {
					continue
				}
				fields := strings.Fields(content[rparen+2:])
				if len(fields) >= 2 {
					ppid, err := strconv.Atoi(fields[1])
					if err == nil {
						allProcs = append(allProcs, procEntry{pid: pid, ppid: ppid})
					}
				}
			}

			// Breadth-first search for all descendants
			targetPIDs := map[int]bool{parentPID: true}
			changed := true
			for changed {
				changed = false
				for _, pe := range allProcs {
					if targetPIDs[pe.ppid] && !targetPIDs[pe.pid] {
						targetPIDs[pe.pid] = true
						changed = true
					}
				}
			}

			for pid := range targetPIDs {
				if pid == parentPID {
					continue
				}
				cmdlineBytes, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
				cmdStr := ""
				if err == nil {
					cmdStr = strings.TrimSpace(strings.ReplaceAll(string(cmdlineBytes), "\x00", " "))
				}
				if cmdStr == "" {
					commBytes, errComm := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "comm"))
					if errComm == nil {
						cmdStr = strings.TrimSpace(string(commBytes))
					}
				}
				if cmdStr == "" {
					cmdStr = fmt.Sprintf("child process [%d]", pid)
				}
				results = append(results, BackgroundTaskInfo{
					PID:       pid,
					Command:   cmdStr,
					StartedAt: time.Now().UTC(),
				})
			}
			return results
		}
	}

	// Fallback for macOS/BSD: ps -o pid=,ppid=,args=
	cmd := exec.Command("ps", "-eo", "pid,ppid,args")
	out, err := cmd.Output()
	if err == nil {
		scanner := bufio.NewScanner(strings.NewReader(string(out)))
		type procLine struct {
			pid     int
			ppid    int
			command string
		}
		var lines []procLine
		for scanner.Scan() {
			fields := strings.Fields(scanner.Text())
			if len(fields) >= 3 {
				pid, err1 := strconv.Atoi(fields[0])
				ppid, err2 := strconv.Atoi(fields[1])
				if err1 == nil && err2 == nil {
					lines = append(lines, procLine{
						pid:     pid,
						ppid:    ppid,
						command: strings.Join(fields[2:], " "),
					})
				}
			}
		}

		targetPIDs := map[int]bool{parentPID: true}
		changed := true
		for changed {
			changed = false
			for _, l := range lines {
				if targetPIDs[l.ppid] && !targetPIDs[l.pid] {
					targetPIDs[l.pid] = true
					changed = true
				}
			}
		}

		for _, l := range lines {
			if l.pid != parentPID && targetPIDs[l.pid] {
				results = append(results, BackgroundTaskInfo{
					PID:       l.pid,
					Command:   l.command,
					StartedAt: time.Now().UTC(),
				})
			}
		}
	}
	return results
}

func findChildProcessesWindows(parentPID int) []BackgroundTaskInfo {
	var results []BackgroundTaskInfo
	cmd := exec.Command("wmic", "process", "where", fmt.Sprintf("ParentProcessId=%d", parentPID), "get", "ProcessId,CommandLine", "/format:csv")
	out, err := cmd.Output()
	if err == nil {
		scanner := bufio.NewScanner(strings.NewReader(string(out)))
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" || strings.HasPrefix(strings.ToLower(line), "node") {
				continue
			}
			parts := strings.Split(line, ",")
			if len(parts) >= 3 {
				pidStr := strings.TrimSpace(parts[len(parts)-1])
				pid, err := strconv.Atoi(pidStr)
				if err == nil && pid > 0 && pid != parentPID {
					cmdStr := strings.TrimSpace(parts[1])
					results = append(results, BackgroundTaskInfo{
						PID:       pid,
						Command:   cmdStr,
						StartedAt: time.Now().UTC(),
					})
				}
			}
		}
	}
	return results
}

// KillChildProcess gracefully terminates a process, falling back to SIGKILL/force kill.
func KillChildProcess(pid int) error {
	if pid <= 0 {
		return nil
	}
	return killProcessPlatform(pid)
}

// Background task activity defaults.
const (
	DefaultBackgroundIdleGrace     = 45 * time.Second
	DefaultBackgroundSubagentStall = 90 * time.Second
	DefaultIdleTaskThreshold       = 15 * time.Second
	DefaultMaxCeilingDuration      = 5 * time.Minute
	DefaultActivityCheckInterval   = 5 * time.Second
)

// TaskSupervisor monitors engine output for root agent idle states and supervises background tasks.
type TaskSupervisor struct {
	ParentPID         int
	InactivityLimit   time.Duration
	SubagentLimit     time.Duration
	IdleTaskThreshold time.Duration
	MaxWaitDuration   time.Duration
	Stream            EventStream
	OnTasksReaped     func(tasks []BackgroundTaskInfo)

	mu               sync.Mutex
	rootIdleDetected bool
	hasSubagents     bool
	rootIdleAt       time.Time
	lastOutputAt     time.Time
	activeTasks      []BackgroundTaskInfo
	reapedTasks      []BackgroundTaskInfo
	wasAutoReaped    bool
	done             chan struct{}
	stopped          bool
}

// NewTaskSupervisor creates and starts an activity-aware background task supervisor.
func NewTaskSupervisor(parentPID int, stream EventStream, timeout ...time.Duration) *TaskSupervisor {
	inactTimeout := time.Duration(0)
	if len(timeout) > 0 && timeout[0] > 0 {
		inactTimeout = timeout[0]
	} else if env := os.Getenv("IAZIO_BACKGROUND_IDLE_GRACE_SEC"); env != "" {
		if sec, err := strconv.Atoi(env); err == nil && sec > 0 {
			inactTimeout = time.Duration(sec) * time.Second
		}
	} else if env := os.Getenv("IAZIO_ROOT_IDLE_TASK_TIMEOUT"); env != "" {
		if d, err := time.ParseDuration(env); err == nil && d > 0 {
			inactTimeout = d
		} else if sec, err := strconv.Atoi(env); err == nil && sec > 0 {
			inactTimeout = time.Duration(sec) * time.Second
		}
	}

	var subagentTimeout time.Duration
	var idleThreshold time.Duration
	var maxWait time.Duration

	if inactTimeout > 0 {
		subagentTimeout = DefaultBackgroundSubagentStall
		if env := os.Getenv("IAZIO_BACKGROUND_SUBAGENT_STALL_SEC"); env != "" {
			if sec, err := strconv.Atoi(env); err == nil && sec > 0 {
				subagentTimeout = time.Duration(sec) * time.Second
			}
		} else if subagentTimeout < inactTimeout {
			subagentTimeout = inactTimeout * 2
		}

		idleThreshold = DefaultIdleTaskThreshold
		if inactTimeout < idleThreshold {
			idleThreshold = inactTimeout / 2
		}

		maxWait = DefaultMaxCeilingDuration
		if inactTimeout*2 > maxWait {
			maxWait = inactTimeout * 2
		}
		if maxWait < 120*time.Second {
			maxWait = 120 * time.Second
		}
	}

	return &TaskSupervisor{
		ParentPID:         parentPID,
		InactivityLimit:   inactTimeout,
		SubagentLimit:     subagentTimeout,
		IdleTaskThreshold: idleThreshold,
		MaxWaitDuration:   maxWait,
		Stream:            stream,
		lastOutputAt:      time.Now().UTC(),
		done:              make(chan struct{}),
	}
}

// OnOutput is invoked whenever new engine output is emitted on stdout or stderr.
func (s *TaskSupervisor) OnOutput(p []byte) {
	if len(p) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	s.lastOutputAt = time.Now().UTC()
	text := string(p)

	lower := strings.ToLower(text)
	if strings.Contains(lower, "subagent") || strings.Contains(lower, "sub-agent") {
		s.hasSubagents = true
	}

	// Detect root agent idle completion barrier pattern
	isRootIdleMessage := strings.Contains(text, "root agent idle; waiting up to") ||
		strings.Contains(text, "WaitForConversationFullyIdle: root agent idle") ||
		strings.Contains(text, "waiting up to 1h0m0s for")

	if isRootIdleMessage && !s.rootIdleDetected {
		s.rootIdleDetected = true
		s.rootIdleAt = time.Now().UTC()

		children := FindChildProcesses(s.ParentPID)
		s.activeTasks = children

		if s.Stream != nil {
			headerMsg := ""
			if s.InactivityLimit > 0 {
				headerMsg = fmt.Sprintf("[ASE-SUPERVISOR] [background_task.supervisor] Root agent waiting on %d background task(s) (inactivity timeout: %v, max wait: %v).\n",
					len(children), s.InactivityLimit, s.MaxWaitDuration)
			} else {
				headerMsg = fmt.Sprintf("[ASE-SUPERVISOR] [background_task.supervisor] Root agent waiting on %d active background task(s) (unbounded execution).\n",
					len(children))
			}
			_ = s.Stream.AppendRaw("stdout", []byte(headerMsg))

			for _, ch := range children {
				cmdPreview := ch.Command
				if len(cmdPreview) > 80 {
					cmdPreview = cmdPreview[:77] + "..."
				}
				taskMsg := fmt.Sprintf("[ASE-SUPERVISOR] [background_task.active] Tracked child PID %d: %s\n", ch.PID, cmdPreview)
				_ = s.Stream.AppendRaw("stdout", []byte(taskMsg))
			}
		}

		go s.supervise()
	}
}

func (s *TaskSupervisor) supervise() {
	tickInterval := 500 * time.Millisecond
	if s.InactivityLimit > 0 {
		tickInterval = s.InactivityLimit / 4
		if tickInterval < 25*time.Millisecond {
			tickInterval = 25 * time.Millisecond
		} else if tickInterval > 2*time.Second {
			tickInterval = 2 * time.Second
		}
	}
	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			s.mu.Lock()
			if s.stopped || s.wasAutoReaped {
				s.mu.Unlock()
				return
			}
			if s.InactivityLimit <= 0 && s.MaxWaitDuration <= 0 {
				s.mu.Unlock()
				continue
			}
			now := time.Now().UTC()
			inactiveDur := now.Sub(s.lastOutputAt)
			totalIdleDur := now.Sub(s.rootIdleAt)

			effectiveGrace := s.InactivityLimit
			if s.hasSubagents && s.SubagentLimit > effectiveGrace {
				effectiveGrace = s.SubagentLimit
			}
			isGraceExpired := s.InactivityLimit > 0 && inactiveDur >= effectiveGrace
			isCeilingReached := s.MaxWaitDuration > 0 && totalIdleDur >= s.MaxWaitDuration
			isTypeBIdle := s.IdleTaskThreshold > 0 && (totalIdleDur >= s.IdleTaskThreshold) && (inactiveDur >= s.IdleTaskThreshold)

			shouldReap := isTypeBIdle || isGraceExpired || isCeilingReached
			if shouldReap {
				s.wasAutoReaped = true
				children := FindChildProcesses(s.ParentPID)
				if len(children) == 0 {
					children = s.activeTasks
				}
				s.reapedTasks = children
				s.mu.Unlock()

				s.executeReap(children, inactiveDur, totalIdleDur, isTypeBIdle)
				return
			}
			s.mu.Unlock()
		}
	}
}

func (s *TaskSupervisor) executeReap(tasks []BackgroundTaskInfo, inactiveDur, totalIdleDur time.Duration, isTypeB bool) {
	classification := "grace_expired"
	if isTypeB {
		classification = "idle_daemon_stall"
	}
	if s.Stream != nil {
		reapMsg := fmt.Sprintf("[ASE-SUPERVISOR] [background_task.drained_on_root_idle] Root agent idle drain completed (%s, idle: %v, inactive: %v). Terminating %d background task(s) to release workspace slot...\n",
			classification, totalIdleDur.Round(time.Millisecond), inactiveDur.Round(time.Millisecond), len(tasks))
		_ = s.Stream.AppendRaw("stdout", []byte(reapMsg))
	}

	for _, task := range tasks {
		_ = KillChildProcess(task.PID)
		if s.Stream != nil {
			_ = s.Stream.AppendRaw("stdout", []byte(fmt.Sprintf("[ASE-SUPERVISOR] [background_task.drained_on_root_idle] Reaped background task PID %d (%s)\n", task.PID, task.Command)))
		}
	}

	if s.OnTasksReaped != nil {
		s.OnTasksReaped(tasks)
	}

	// Sleep 500ms to allow parent process to notice child exit
	time.Sleep(500 * time.Millisecond)

	// If parent process is still alive waiting on background barrier, signal it to complete turn
	if s.ParentPID > 0 {
		_ = KillChildProcess(s.ParentPID)
	}
}

// ActiveTasks returns currently tracked background tasks.
func (s *TaskSupervisor) ActiveTasks() []BackgroundTaskInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]BackgroundTaskInfo(nil), s.activeTasks...)
}

// ReapedTasks returns background tasks that were auto-reaped.
func (s *TaskSupervisor) ReapedTasks() []BackgroundTaskInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]BackgroundTaskInfo(nil), s.reapedTasks...)
}

// WasAutoReaped returns true if background tasks were reaped due to inactivity.
func (s *TaskSupervisor) WasAutoReaped() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.wasAutoReaped
}

// Stop cleanly stops the supervisor.
func (s *TaskSupervisor) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.stopped {
		s.stopped = true
		close(s.done)
	}
}
