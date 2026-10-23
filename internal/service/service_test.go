package service

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestActionText_Install(t *testing.T) {
	for _, goos := range []string{"linux", "darwin", "windows"} {
		txt := ActionText(goos, "install", "/usr/local/bin/iazio-harness", false)
		if txt == "" {
			t.Errorf("empty action text for %s", goos)
		}
		if !strings.Contains(txt, "iazio-harness") {
			t.Errorf("missing iazio-harness in action text for %s: %s", goos, txt)
		}
	}
}

func TestActionText_Commands(t *testing.T) {
	actions := []string{"start", "stop", "restart", "uninstall", "status", "dev-install", "pause", "resume", "revive-check"}
	for _, act := range actions {
		txt := ActionText("linux", act, "/bin/iazio-harness", false)
		if txt == "" {
			t.Errorf("empty text for linux %s", act)
		}
	}
}

func TestLinuxUnit_Rendering(t *testing.T) {
	t.Setenv("IAZIO_HARNESS_API_URL", "http://192.168.0.81:8090")
	unit := linuxUnit("/usr/bin/iazio-harness")
	if !strings.Contains(unit, "ExecStart=/usr/bin/iazio-harness daemon") {
		t.Errorf("expected ExecStart with daemon, got: %s", unit)
	}
	if !strings.Contains(unit, "Restart=always") {
		t.Errorf("expected Restart=always, got: %s", unit)
	}
	if !strings.Contains(unit, "Environment=IAZIO_HARNESS_API_URL=http://192.168.0.81:8090") {
		t.Errorf("expected IAZIO_HARNESS_API_URL environment variable, got: %s", unit)
	}
}

func TestDarwinPlist_Rendering(t *testing.T) {
	t.Setenv("IAZIO_HARNESS_API_URL", "http://192.168.0.81:8090")
	plist := darwinPlist("/usr/local/bin/iazio-harness", "/var/log/harness.log")
	if !strings.Contains(plist, "<string>io.iazio.iazio-harness</string>") {
		t.Errorf("expected label in plist, got: %s", plist)
	}
	if !strings.Contains(plist, "<string>daemon</string>") {
		t.Errorf("expected daemon argument in plist, got: %s", plist)
	}
	if !strings.Contains(plist, "<string>http://192.168.0.81:8090</string>") {
		t.Errorf("expected api url in plist, got: %s", plist)
	}
}

func TestDevInstall_AtomicSwap(t *testing.T) {
	tempDir := t.TempDir()
	srcBin := filepath.Join(tempDir, "mock-source-binary")
	if err := os.WriteFile(srcBin, []byte("#!/bin/sh\necho dev-harness\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	mockExecCalls := []string{}
	cleanup := SetExecCommandForTest(func(name string, args ...string) error {
		mockExecCalls = append(mockExecCalls, name+" "+strings.Join(args, " "))
		return nil
	})
	defer cleanup()

	// Dev install targeting mock home
	if err := DevInstall(srcBin, tempDir, nil); err != nil {
		t.Fatalf("DevInstall failed: %v", err)
	}

	binName := "iazio-harness"
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	destPath := filepath.Join(tempDir, ".iazio", "bin", binName)
	data, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatalf("destination binary missing: %v", err)
	}
	if !strings.Contains(string(data), "dev-harness") {
		t.Fatalf("destination binary content mismatch: %s", string(data))
	}
}

func TestPauseAndResumeService(t *testing.T) {
	tempDir := t.TempDir()
	calls := []string{}
	cleanup := SetExecCommandForTest(func(name string, args ...string) error {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return nil
	})
	defer cleanup()

	// Pause service
	if err := PauseService(tempDir, nil); err != nil {
		t.Fatalf("PauseService failed: %v", err)
	}
	lockFile := filepath.Join(tempDir, ".iazio", "maintenance.lock")
	if _, err := os.Stat(lockFile); err != nil {
		t.Fatalf("expected maintenance lock file to exist: %v", err)
	}

	// ReviveCheck during maintenance should do nothing
	callsBeforeRevive := len(calls)
	if err := ReviveCheck(tempDir, nil); err != nil {
		t.Fatalf("ReviveCheck failed: %v", err)
	}
	if len(calls) != callsBeforeRevive {
		t.Fatalf("ReviveCheck should not restart during maintenance, got calls: %v", calls[callsBeforeRevive:])
	}

	// Resume service
	if err := ResumeService(tempDir, nil); err != nil {
		t.Fatalf("ResumeService failed: %v", err)
	}
	if _, err := os.Stat(lockFile); !os.IsNotExist(err) {
		t.Fatalf("expected maintenance lock to be deleted")
	}
}

func TestLinuxResilienceUnits(t *testing.T) {
	tempDir := t.TempDir()
	calls := []string{}
	cleanup := SetExecCommandForTest(func(name string, args ...string) error {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return nil
	})
	defer cleanup()

	bin := "/usr/local/bin/iazio-harness"
	if err := InstallLinuxResilience(tempDir, bin, nil); err != nil {
		t.Fatalf("InstallLinuxResilience failed: %v", err)
	}

	unitDir := filepath.Join(tempDir, ".config", "systemd", "user")
	files := []string{
		"iazio-harness-watcher.path",
		"iazio-harness-watcher.service",
		"iazio-harness-watchdog.timer",
		"iazio-harness-watchdog.service",
	}
	for _, f := range files {
		p := filepath.Join(unitDir, f)
		if _, err := os.Stat(p); err != nil {
			t.Errorf("missing resilience file: %s", f)
		}
	}
}

func TestUserDaemon_Fallback(t *testing.T) {
	tempDir := t.TempDir()
	bin := filepath.Join(tempDir, "mock-bin")
	_ = os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755)

	calls := []string{}
	cleanup := SetExecCommandForTest(func(name string, args ...string) error {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return nil
	})
	defer cleanup()

	if err := InstallUserDaemonAt(tempDir, bin, nil); err != nil {
		t.Fatalf("InstallUserDaemonAt failed: %v", err)
	}

	runnerScript := filepath.Join(tempDir, ".iazio", "bin", "iazio-harness-runner.sh")
	data, err := os.ReadFile(runnerScript)
	if err != nil {
		t.Fatalf("runner script missing: %v", err)
	}
	if !strings.Contains(string(data), "nohup") || !strings.Contains(string(data), "daemon") {
		t.Fatalf("runner script content unexpected: %s", string(data))
	}
}

func TestParseSchtasksStatus(t *testing.T) {
	sample := `
Folder: \
HostName:                             PC1
TaskName:                             \iazio-harness
Status:                               Running
`
	running, status, ok := ParseSchtasksStatus(sample)
	if !ok || !running || status != "Running" {
		t.Errorf("ParseSchtasksStatus expected Running, got running=%v status=%s ok=%v", running, status, ok)
	}

	sampleStopped := `
TaskName:                             \iazio-harness
Status:                               Ready
`
	running, status, ok = ParseSchtasksStatus(sampleStopped)
	if !ok || running || status != "Ready" {
		t.Errorf("ParseSchtasksStatus expected Ready, got running=%v status=%s ok=%v", running, status, ok)
	}
}
