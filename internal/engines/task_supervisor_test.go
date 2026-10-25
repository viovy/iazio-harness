package engines

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

type mockStream struct {
	chunks []string
}

func (m *mockStream) AppendRaw(stream string, data []byte) error {
	m.chunks = append(m.chunks, string(data))
	return nil
}

func TestTaskSupervisor_DetectsRootAgentIdleAndReaps(t *testing.T) {
	// Spawn a long-running dummy child process (e.g. sleep 30)
	childCmd := exec.Command("sleep", "30")
	if err := childCmd.Start(); err != nil {
		t.Fatalf("failed to start dummy child: %v", err)
	}
	defer func() {
		if childCmd.Process != nil {
			_ = childCmd.Process.Kill()
		}
	}()

	stream := &mockStream{}
	// Supervisor with very fast inactivity limit for testing (100ms)
	sup := NewTaskSupervisor(os.Getpid(), stream, 100*time.Millisecond)
	sup.MaxWaitDuration = 200 * time.Millisecond
	defer sup.Stop()

	// Feed root idle message to trigger watchdog arming
	sup.OnOutput([]byte("root agent idle; waiting up to 1h0m0s for 1 background task(s)"))

	// Wait for watchdog to trigger reap
	time.Sleep(400 * time.Millisecond)

	if !sup.WasAutoReaped() {
		t.Fatalf("expected supervisor to have auto-reaped background tasks after inactivity")
	}

	reaped := sup.ReapedTasks()
	if len(reaped) == 0 {
		t.Logf("Reaped tasks list: %v (found: %d)", reaped, len(reaped))
	}
}

func TestFindChildProcesses_FindsSpawnedChildren(t *testing.T) {
	cmd := exec.Command("sleep", "10")
	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start test child: %v", err)
	}
	defer func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	}()

	children := FindChildProcesses(os.Getpid())
	found := false
	for _, ch := range children {
		if ch.PID == cmd.Process.Pid {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected to find child PID %d in FindChildProcesses, got %v", cmd.Process.Pid, children)
	}
}
