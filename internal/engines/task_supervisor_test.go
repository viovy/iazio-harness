package engines

import (
	"os"
	"os/exec"
	"strings"
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
	dummyParent := exec.Command("sleep", "30")
	if err := dummyParent.Start(); err != nil {
		t.Fatalf("failed to start dummy parent: %v", err)
	}
	defer func() {
		if dummyParent.Process != nil {
			_ = dummyParent.Process.Kill()
		}
	}()

	stream := &mockStream{}
	var callbackCalled bool
	sup := NewTaskSupervisor(dummyParent.Process.Pid, stream, 100*time.Millisecond)
	sup.MaxWaitDuration = 200 * time.Millisecond
	sup.OnTasksReaped = func(tasks []BackgroundTaskInfo) {
		callbackCalled = true
	}
	defer sup.Stop()

	// Feed root idle message to trigger watchdog arming
	sup.OnOutput([]byte("root agent idle; waiting up to 1h0m0s for 1 background task(s)"))

	// Wait for watchdog to trigger reap
	time.Sleep(2500 * time.Millisecond)

	if !sup.WasAutoReaped() {
		t.Fatalf("expected supervisor to have auto-reaped background tasks after inactivity")
	}

	reaped := sup.ReapedTasks()
	t.Logf("Reaped tasks count: %d, callbackCalled: %v", len(reaped), callbackCalled)
}

func TestTaskSupervisor_EnvironmentConfig(t *testing.T) {
	t.Setenv("IAZIO_ROOT_IDLE_TASK_TIMEOUT", "45s")
	sup1 := NewTaskSupervisor(os.Getpid(), nil)
	if sup1.InactivityLimit != 45*time.Second {
		t.Errorf("expected 45s, got %v", sup1.InactivityLimit)
	}
	sup1.Stop()

	t.Setenv("IAZIO_ROOT_IDLE_TASK_TIMEOUT", "120")
	sup2 := NewTaskSupervisor(os.Getpid(), nil)
	if sup2.InactivityLimit != 120*time.Second {
		t.Errorf("expected 120s, got %v", sup2.InactivityLimit)
	}
	sup2.Stop()
}

func TestTaskSupervisor_OnOutputAndActiveTasks(t *testing.T) {
	sup := NewTaskSupervisor(os.Getpid(), nil, 10*time.Second)
	defer sup.Stop()

	// Normal output before idle
	sup.OnOutput([]byte("building project\n"))
	if len(sup.ActiveTasks()) != 0 {
		t.Errorf("expected 0 active tasks before idle")
	}

	// Idle output
	sup.OnOutput([]byte("root agent idle; waiting up to 1h0m0s for 1 background task(s)"))
	// More output updates lastOutputAt
	sup.OnOutput([]byte("progress 50%\n"))

	_ = sup.ActiveTasks()
	_ = sup.ReapedTasks()
	if sup.WasAutoReaped() {
		t.Errorf("should not be auto reaped yet")
	}
}

func TestKillChildProcess(t *testing.T) {
	if err := KillChildProcess(0); err != nil {
		t.Errorf("expected nil for pid 0, got %v", err)
	}
	if err := KillChildProcess(-1); err != nil {
		t.Errorf("expected nil for pid -1, got %v", err)
	}

	cmd := exec.Command("sleep", "20")
	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start sleep: %v", err)
	}

	pid := cmd.Process.Pid
	if err := KillChildProcess(pid); err != nil {
		t.Errorf("failed to kill process %d: %v", pid, err)
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

	// Test non-existent parent
	empty := FindChildProcesses(99999999)
	if len(empty) != 0 {
		t.Errorf("expected 0 children for non-existent parent")
	}
}

func TestTaskSupervisor_TypeB_IdleDaemonReap(t *testing.T) {
	dummyParent := exec.Command("sleep", "30")
	if err := dummyParent.Start(); err != nil {
		t.Fatalf("failed to start dummy parent: %v", err)
	}
	defer func() {
		if dummyParent.Process != nil {
			_ = dummyParent.Process.Kill()
		}
	}()

	stream := &mockStream{}
	// Set very short grace & idle threshold for fast unit test
	sup := NewTaskSupervisor(dummyParent.Process.Pid, stream, 80*time.Millisecond)
	sup.IdleTaskThreshold = 40 * time.Millisecond
	sup.MaxWaitDuration = 200 * time.Millisecond
	defer sup.Stop()

	// Simulate root agent idle wait message (as emitted by agy or opencode)
	sup.OnOutput([]byte("root agent idle; waiting up to 1h0m0s for 2 background task(s)"))

	// Wait enough for the 40ms Type B idle threshold to trip, well under 45s and never 1 hour
	time.Sleep(2500 * time.Millisecond)

	if !sup.WasAutoReaped() {
		t.Fatalf("expected supervisor to have auto-reaped Type B idle background tasks")
	}

	foundDrainedMsg := false
	for _, chunk := range stream.chunks {
		if len(chunk) > 0 && (strings.Contains(chunk, "background_task.drained_on_root_idle") || strings.Contains(chunk, "releasing workspace slot")) {
			foundDrainedMsg = true
			break
		}
	}
	if !foundDrainedMsg {
		t.Errorf("expected drained_on_root_idle stream event, got: %v", stream.chunks)
	}
}
