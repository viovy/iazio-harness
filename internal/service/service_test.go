package service

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
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
	for _, goos := range []string{"linux", "darwin", "windows"} {
		for _, act := range actions {
			txt := ActionText(goos, act, "/bin/iazio-harness", false)
			if txt == "" {
				t.Errorf("empty text for %s %s", goos, act)
			}
		}
	}
	// test default action
	if txt := ActionText("linux", "", "/bin/iazio-harness", false); txt == "" {
		t.Error("expected non-empty text for default action")
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

func TestUnitText(t *testing.T) {
	for _, goos := range []string{"linux", "darwin", "windows"} {
		txt := UnitText(goos, "/opt/iazio/bin/iazio-harness")
		if txt == "" {
			t.Errorf("empty UnitText for %s", goos)
		}
	}
}

func TestApply_DryAndExec(t *testing.T) {
	// dry run
	txt, err := Apply("linux", "install", "/bin/iazio-harness", true, false, nil)
	if err != nil || txt == "" {
		t.Fatalf("Apply dry run failed: %v", err)
	}

	// nil execFn
	txt, err = Apply("linux", "status", "/bin/iazio-harness", false, false, nil)
	if err != nil || txt == "" {
		t.Fatalf("Apply with nil execFn failed: %v", err)
	}

	// with execFn
	called := false
	execFn := func(name string, args ...string) error {
		called = true
		return nil
	}
	_, err = Apply("linux", "status", "/bin/iazio-harness", false, false, execFn)
	if err != nil || !called {
		t.Fatalf("expected execFn called, err: %v", err)
	}

	// with error
	errFn := func(name string, args ...string) error {
		return errors.New("exec error")
	}
	_, err = Apply("linux", "status", "/bin/iazio-harness", false, false, errFn)
	if err == nil {
		t.Fatal("expected error from Apply")
	}
}

func TestControlArgv(t *testing.T) {
	// darwin
	args := controlArgv("darwin", "install", false)
	if len(args) < 2 || args[0] != "launchctl" || args[1] != "bootstrap" {
		t.Errorf("darwin install argv mismatch: %v", args)
	}
	args = controlArgv("darwin", "uninstall", false)
	if len(args) < 2 || args[1] != "bootout" {
		t.Errorf("darwin uninstall argv mismatch: %v", args)
	}
	args = controlArgv("darwin", "status", false)
	if len(args) < 2 || args[1] != "status" {
		t.Errorf("darwin status argv mismatch: %v", args)
	}

	// windows
	args = controlArgv("windows", "install", false)
	if len(args) < 4 || args[0] != "schtasks" {
		t.Errorf("windows install argv mismatch: %v", args)
	}
	args = controlArgv("windows", "install", true)
	if len(args) < 5 || args[len(args)-1] != "/F" {
		t.Errorf("windows install force argv mismatch: %v", args)
	}
	args = controlArgv("windows", "start", false)
	if len(args) < 3 || args[1] != "/Run" {
		t.Errorf("windows start argv mismatch: %v", args)
	}
	args = controlArgv("windows", "stop", false)
	if len(args) < 3 || args[1] != "/End" {
		t.Errorf("windows stop argv mismatch: %v", args)
	}
	args = controlArgv("windows", "uninstall", false)
	if len(args) < 3 || args[1] != "/Delete" {
		t.Errorf("windows uninstall argv mismatch: %v", args)
	}
	args = controlArgv("windows", "query", false)
	if len(args) < 3 || args[1] != "/Query" {
		t.Errorf("windows query argv mismatch: %v", args)
	}

	// linux
	args = controlArgv("linux", "install", false)
	if len(args) < 4 || args[0] != "systemctl" || args[2] != "enable" {
		t.Errorf("linux install argv mismatch: %v", args)
	}
	args = controlArgv("linux", "install", true)
	if len(args) < 6 || args[4] != "--force" {
		t.Errorf("linux install force argv mismatch: %v", args)
	}
	args = controlArgv("linux", "restart", false)
	if len(args) < 4 || args[2] != "restart" {
		t.Errorf("linux restart argv mismatch: %v", args)
	}
}

func TestDarwinPlists(t *testing.T) {
	wPlist := DarwinWatcherPlist("/opt/iazio/bin/iazio-harness", "/tmp/log")
	if !strings.Contains(wPlist, watcherLabel) || !strings.Contains(wPlist, "restart") {
		t.Fatalf("unexpected DarwinWatcherPlist: %s", wPlist)
	}

	dogPlist := DarwinWatchdogPlist("/opt/iazio/bin/iazio-harness", "/tmp/log")
	if !strings.Contains(dogPlist, watchdogLabel) || !strings.Contains(dogPlist, "revive-check") {
		t.Fatalf("unexpected DarwinWatchdogPlist: %s", dogPlist)
	}
}

func TestInstallDarwinAt_and_Resilience(t *testing.T) {
	tempDir := t.TempDir()
	mockRun := func(name string, args ...string) error {
		return nil
	}

	// test nil runner error
	if err := InstallDarwinAt(tempDir, "/bin/iazio-harness", nil); err == nil {
		t.Fatal("expected error with nil runner")
	}

	// test InstallDarwinAt
	if err := InstallDarwinAt(tempDir, "/bin/iazio-harness", mockRun); err != nil {
		t.Fatalf("InstallDarwinAt failed: %v", err)
	}

	plistPath := filepath.Join(tempDir, "Library", "LaunchAgents", harnessLabel+".plist")
	if _, err := os.Stat(plistPath); err != nil {
		t.Fatalf("plist missing: %v", err)
	}

	// test InstallDarwinResilience
	if err := InstallDarwinResilience(tempDir, "/bin/iazio-harness", mockRun); err != nil {
		t.Fatalf("InstallDarwinResilience failed: %v", err)
	}

	// test UninstallDarwinResilience
	if err := UninstallDarwinResilience(tempDir, mockRun); err != nil {
		t.Fatalf("UninstallDarwinResilience failed: %v", err)
	}

	// test runner error in InstallDarwinAt
	errRun := func(name string, args ...string) error {
		if len(args) > 0 && args[0] == "bootstrap" {
			return errors.New("bootstrap failed")
		}
		return nil
	}
	if err := InstallDarwinAt(tempDir, "/bin/iazio-harness", errRun); err == nil {
		t.Fatal("expected bootstrap error")
	}
}

func TestInstallLinuxAt(t *testing.T) {
	tempDir := t.TempDir()
	mockRun := func(name string, args ...string) error {
		return nil
	}

	// nil runner error
	if err := InstallLinuxAt(tempDir, "/bin/iazio-harness", nil); err == nil {
		t.Fatal("expected error with nil runner")
	}

	if err := InstallLinuxAt(tempDir, "/bin/iazio-harness", mockRun); err != nil {
		t.Fatalf("InstallLinuxAt failed: %v", err)
	}

	unitPath := filepath.Join(tempDir, ".config", "systemd", "user", "iazio-harness.service")
	if _, err := os.Stat(unitPath); err != nil {
		t.Fatalf("unit missing: %v", err)
	}

	// test daemon-reload failure
	reloadErrRun := func(name string, args ...string) error {
		if len(args) > 1 && args[1] == "daemon-reload" {
			return errors.New("reload error")
		}
		return nil
	}
	if err := InstallLinuxAt(tempDir, "/bin/iazio-harness", reloadErrRun); err == nil {
		t.Fatal("expected reload error")
	}
}

func TestInstallWindowsAt(t *testing.T) {
	tempDir := t.TempDir()
	mockRun := func(name string, args ...string) error {
		return nil
	}

	// nil runner error
	if err := InstallWindowsAt(tempDir, `C:\bin\iazio-harness.exe`, "testuser", nil); err == nil {
		t.Fatal("expected error with nil runner")
	}

	if err := InstallWindowsAt(tempDir, `C:\bin\iazio-harness.exe`, "testuser", mockRun); err != nil {
		t.Fatalf("InstallWindowsAt failed: %v", err)
	}

	batPath := filepath.Join(tempDir, ".iazio", "tasks", "iazio-harness.bat")
	xmlPath := filepath.Join(tempDir, ".iazio", "tasks", "iazio-harness.xml")
	if _, err := os.Stat(batPath); err != nil {
		t.Fatalf("bat missing: %v", err)
	}
	xmlData, err := os.ReadFile(xmlPath)
	if err != nil {
		t.Fatalf("xml missing: %v", err)
	}
	if len(xmlData) < 2 || xmlData[0] != 0xFF || xmlData[1] != 0xFE {
		t.Fatalf("xml missing UTF-16 LE BOM")
	}

	// test register failure
	regErrRun := func(name string, args ...string) error {
		if len(args) > 0 && args[0] == "/Create" {
			return errors.New("create error")
		}
		return nil
	}
	if err := InstallWindowsAt(tempDir, `C:\bin\iazio-harness.exe`, "testuser", regErrRun); err == nil {
		t.Fatal("expected register error")
	}
}

func TestWindowsLogonUser(t *testing.T) {
	u := windowsLogonUser()
	if u == "" {
		t.Fatal("expected non-empty windows logon user")
	}
}

func TestWSLDetection(t *testing.T) {
	// with WSL_DISTRO_NAME
	env := map[string]string{"WSL_DISTRO_NAME": "Debian"}
	getenv := func(key string) string { return env[key] }
	if !IsWSLWith(getenv, nil) {
		t.Errorf("expected WSL detection via WSL_DISTRO_NAME")
	}

	// with osrelease containing "microsoft"
	env = map[string]string{}
	readOSRelease := func() ([]byte, error) {
		return []byte("Linux 5.15.133.1-microsoft-standard-WSL2"), nil
	}
	if !IsWSLWith(getenv, readOSRelease) {
		t.Errorf("expected WSL detection via /proc/sys/kernel/osrelease")
	}

	// non-WSL
	readOSReleaseNonWSL := func() ([]byte, error) {
		return []byte("Linux 6.8.0-45-generic"), nil
	}
	if IsWSLWith(getenv, readOSReleaseNonWSL) {
		t.Errorf("expected non-WSL")
	}
}

func TestUserDaemon_StopAndStatus(t *testing.T) {
	tempDir := t.TempDir()

	// Initially not running
	running, pid := StatusUserDaemonAt(tempDir)
	if running || pid != 0 {
		t.Fatalf("expected not running, got running=%v pid=%d", running, pid)
	}

	// Write PID of current process
	currentPID := os.Getpid()
	pidPath := filepath.Join(tempDir, ".iazio", "harness.pid")
	_ = os.MkdirAll(filepath.Dir(pidPath), 0o755)
	_ = os.WriteFile(pidPath, []byte(strconv.Itoa(currentPID)), 0o644)

	running, pid = StatusUserDaemonAt(tempDir)
	if !running || pid != currentPID {
		t.Fatalf("expected running with pid %d, got %v (%d)", currentPID, running, pid)
	}

	if !isDaemonPIDAlive(pidPath) {
		t.Fatal("expected isDaemonPIDAlive to be true for current PID")
	}

	// StopUserDaemonAt (with invalid PID to avoid killing self)
	_ = os.WriteFile(pidPath, []byte("99999999"), 0o644)
	if err := StopUserDaemonAt(tempDir); err != nil {
		t.Fatalf("StopUserDaemonAt failed: %v", err)
	}
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Fatal("expected pid file deleted after StopUserDaemonAt")
	}

	// UninstallUserDaemonAt
	runnerPath := filepath.Join(tempDir, ".iazio", "bin", "iazio-harness-runner.sh")
	_ = os.WriteFile(runnerPath, []byte("#!/bin/sh\n"), 0o755)
	if err := UninstallUserDaemonAt(tempDir); err != nil {
		t.Fatalf("UninstallUserDaemonAt failed: %v", err)
	}
	if _, err := os.Stat(runnerPath); !os.IsNotExist(err) {
		t.Fatal("expected runner script deleted after UninstallUserDaemonAt")
	}
}

func TestResolveHarnessAPIURL_ConfigSources(t *testing.T) {
	// from env
	t.Setenv("IAZIO_HARNESS_API_URL", "http://env-api:8080")
	if u := resolveHarnessAPIURL(); u != "http://env-api:8080" {
		t.Errorf("expected env url, got: %s", u)
	}
}

func TestHarnessLogPath(t *testing.T) {
	p := harnessLogPath()
	if p == "" || !strings.Contains(p, "harness.log") {
		t.Errorf("unexpected harness log path: %s", p)
	}
}

func TestResolveHarnessAPIURL_ConfigFiles(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("IAZIO_HARNESS_API_URL", "")
	t.Setenv("HOME", tempDir)

	// initially empty -> DefaultFleetAPI
	if u := resolveHarnessAPIURL(); u != DefaultFleetAPI {
		t.Fatalf("expected DefaultFleetAPI, got %s", u)
	}

	// with .iazio/config.json endpoints.harness_api
	iazioDir := filepath.Join(tempDir, ".iazio")
	_ = os.MkdirAll(iazioDir, 0o755)
	cfgFile := filepath.Join(iazioDir, "config.json")
	_ = os.WriteFile(cfgFile, []byte(`{"endpoints":{"harness_api":"http://cfg-api:8080"}}`), 0o644)
	if u := resolveHarnessAPIURL(); u != "http://cfg-api:8080" {
		t.Fatalf("expected cfg-api:8080, got %s", u)
	}

	// with .iazio/config.json endpoints["harness-api"]
	_ = os.WriteFile(cfgFile, []byte(`{"endpoints":{"harness-api":"http://dash-api:8080"}}`), 0o644)
	if u := resolveHarnessAPIURL(); u != "http://dash-api:8080" {
		t.Fatalf("expected dash-api:8080, got %s", u)
	}

	// with .iazio/api_url fallback
	_ = os.Remove(cfgFile)
	_ = os.WriteFile(filepath.Join(iazioDir, "api_url"), []byte("http://raw-api:8080\n"), 0o644)
	if u := resolveHarnessAPIURL(); u != "http://raw-api:8080" {
		t.Fatalf("expected raw-api:8080, got %s", u)
	}
}

func TestCopyBinaryAtomic_EdgeCases(t *testing.T) {
	tempDir := t.TempDir()
	src := filepath.Join(tempDir, "bin1")
	_ = os.WriteFile(src, []byte("echo test"), 0o755)

	// src == dest
	if err := copyBinaryAtomic(src, src); err != nil {
		t.Fatalf("expected nil when src == dest, got: %v", err)
	}

	// non-existent src
	if err := copyBinaryAtomic(filepath.Join(tempDir, "missing"), filepath.Join(tempDir, "dest")); err == nil {
		t.Fatal("expected error for missing src")
	}

	// normal copy
	dest := filepath.Join(tempDir, "sub", "bin2")
	if err := copyBinaryAtomic(src, dest); err != nil {
		t.Fatalf("copyBinaryAtomic failed: %v", err)
	}
	b, err := os.ReadFile(dest)
	if err != nil || string(b) != "echo test" {
		t.Fatalf("content mismatch: %s", string(b))
	}
}

func TestReviveCheck_ActiveSystemd(t *testing.T) {
	tempDir := t.TempDir()
	calls := []string{}
	cleanup := SetExecCommandForTest(func(name string, args ...string) error {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return nil
	}, func(name string, args ...string) ([]byte, error) {
		// Mock systemctl is-active returning inactive
		return []byte("inactive\n"), errors.New("exit 3")
	})
	defer cleanup()

	if err := ReviveCheck(tempDir, nil); err != nil {
		t.Fatalf("ReviveCheck failed: %v", err)
	}
}

func TestReviveCheck_DisabledSystemd(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("IAZIO_DISABLE_SYSTEMD", "1")

	// Create bin and runner script so StartUserDaemonAt works
	binDir := filepath.Join(tempDir, ".iazio", "bin")
	_ = os.MkdirAll(binDir, 0o755)
	bin := filepath.Join(binDir, "iazio-harness")
	_ = os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755)
	runner := filepath.Join(binDir, "iazio-harness-runner.sh")
	_ = os.WriteFile(runner, []byte("#!/bin/sh\n"), 0o755)

	calls := []string{}
	cleanup := SetExecCommandForTest(func(name string, args ...string) error {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return nil
	})
	defer cleanup()

	if err := ReviveCheck(tempDir, nil); err != nil {
		t.Fatalf("ReviveCheck with disabled systemd failed: %v", err)
	}
	if len(calls) == 0 {
		t.Fatal("expected runner execution when daemon inactive")
	}
}

func TestPauseAndResume_DisabledSystemd(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("IAZIO_DISABLE_SYSTEMD", "1")

	binDir := filepath.Join(tempDir, ".iazio", "bin")
	_ = os.MkdirAll(binDir, 0o755)
	bin := filepath.Join(binDir, "iazio-harness")
	_ = os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755)
	runner := filepath.Join(binDir, "iazio-harness-runner.sh")
	_ = os.WriteFile(runner, []byte("#!/bin/sh\n"), 0o755)

	calls := []string{}
	cleanup := SetExecCommandForTest(func(name string, args ...string) error {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return nil
	})
	defer cleanup()

	if err := PauseService(tempDir, nil); err != nil {
		t.Fatalf("PauseService failed: %v", err)
	}

	if err := ResumeService(tempDir, nil); err != nil {
		t.Fatalf("ResumeService failed: %v", err)
	}
	if len(calls) == 0 {
		t.Fatal("expected runner execution on ResumeService")
	}
}

func TestIsWSL_Call(t *testing.T) {
	_ = IsWSL()
}

func TestInstallWrappers(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("HOME", tempDir)
	cleanup := SetExecCommandForTest(func(name string, args ...string) error {
		return nil
	})
	defer cleanup()

	bin := filepath.Join(tempDir, "mock-bin")
	_ = os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755)

	if err := InstallLinux(bin); err != nil {
		t.Fatalf("InstallLinux failed: %v", err)
	}
	if err := InstallUserDaemon(bin); err != nil {
		t.Fatalf("InstallUserDaemon failed: %v", err)
	}
}

func TestDevInstall_ErrorsAndStartDaemonErrors(t *testing.T) {
	tempDir := t.TempDir()

	// missing source binary
	if err := DevInstall(filepath.Join(tempDir, "nonexistent"), tempDir, nil); err == nil {
		t.Error("expected error for nonexistent source")
	}

	// source binary is a directory
	dirBin := filepath.Join(tempDir, "somedir")
	_ = os.MkdirAll(dirBin, 0o755)
	if err := DevInstall(dirBin, tempDir, nil); err == nil {
		t.Error("expected error for directory source")
	}

	// StartUserDaemonAt without runner script
	if err := StartUserDaemonAt(tempDir, "somebin", nil); err == nil {
		t.Error("expected error for missing runner script")
	}
}

func TestInstallDarwin_And_InstallWindows(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv("HOME", tempHome)
	t.Setenv("WINDIR", tempHome)

	_ = InstallDarwin("/usr/local/bin/iazio-harness")
	_ = InstallWindows("/usr/local/bin/iazio-harness.exe")
}
