// Package service renders and installs the user-level service/daemon for iazio-harness.
package service

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"
)

const harnessLabel = "io.iazio.iazio-harness"
const watcherLabel = "io.iazio.iazio-harness-watcher"
const watchdogLabel = "io.iazio.iazio-harness-watchdog"

// DefaultFleetAPI is the default canonical fleet control plane API URL.
const DefaultFleetAPI = ""

// UnitText returns the launchd, systemd, or scheduler text for a dry run.
func UnitText(goos, bin string) string {
	return ActionText(goos, "install", bin, false)
}

func resolveHarnessAPIURL() string {
	if apiEnv := strings.TrimSpace(os.Getenv("IAZIO_HARNESS_API_URL")); apiEnv != "" {
		return apiEnv
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		cfgPath := filepath.Join(home, ".iazio", "config.json")
		if data, err := os.ReadFile(cfgPath); err == nil {
			var cfg struct {
				Endpoints map[string]string `json:"endpoints"`
			}
			if err := json.Unmarshal(data, &cfg); err == nil && cfg.Endpoints != nil {
				if v, ok := cfg.Endpoints["harness_api"]; ok && strings.TrimSpace(v) != "" {
					return strings.TrimSpace(v)
				}
				if v, ok := cfg.Endpoints["harness-api"]; ok && strings.TrimSpace(v) != "" {
					return strings.TrimSpace(v)
				}
			}
		}
		if b, err := os.ReadFile(filepath.Join(home, ".iazio", "api_url")); err == nil {
			if s := strings.TrimSpace(string(b)); s != "" {
				return s
			}
		}
	}
	return DefaultFleetAPI
}

// ActionText returns the dry-run text for a service action.
// Install yields a launchd plist, a systemd user unit, or a Task Scheduler command.
func ActionText(goos, action, bin string, force bool) string {
	if action == "" {
		action = "install"
	}
	switch action {
	case "dev-install":
		return fmt.Sprintf("hot-swap %s and restart supervisor", bin)
	case "pause":
		return "pause supervisor and place maintenance lock"
	case "resume":
		return "resume supervisor and remove maintenance lock"
	case "revive-check":
		return "check supervisor health and auto-revive if inactive"
	}
	if action == "install" {
		switch goos {
		case "darwin":
			return darwinPlist(bin, harnessLogPath())
		case "windows":
			flag := ""
			if force {
				flag = " /F"
			}
			setApi := ""
			if apiEnv := resolveHarnessAPIURL(); apiEnv != "" {
				setApi = fmt.Sprintf("set IAZIO_HARNESS_API_URL=%s&& ", apiEnv)
			}
			return fmt.Sprintf(`schtasks /Create /TN iazio-harness /SC ONLOGON%s /TR "cmd /C set IAZIO_HARNESS_NONINTERACTIVE=1&& %s\"%s\" daemon"`, flag, setApi, bin)
		default:
			return linuxUnit(bin)
		}
	}
	switch goos {
	case "darwin":
		return fmt.Sprintf("launchctl %s gui/%d/%s", action, os.Getuid(), harnessLabel)
	case "windows":
		switch action {
		case "start", "restart":
			return "schtasks /Run /TN iazio-harness"
		case "stop":
			return "schtasks /End /TN iazio-harness"
		case "uninstall":
			return "schtasks /Delete /TN iazio-harness /F"
		default:
			return "schtasks /Query /TN iazio-harness"
		}
	default:
		return fmt.Sprintf("systemctl --user %s iazio-harness.service", action)
	}
}

// Apply returns the service text. A dry run does not call execFn.
func Apply(goos, action, bin string, dry, force bool, execFn func(name string, args ...string) error) (string, error) {
	text := ActionText(goos, action, bin, force)
	if dry || execFn == nil {
		return text, nil
	}
	argv := controlArgv(goos, action, force)
	if len(argv) == 0 {
		return text, nil
	}
	if err := execFn(argv[0], argv[1:]...); err != nil {
		return text, err
	}
	return text, nil
}

func controlArgv(goos, action string, force bool) []string {
	switch goos {
	case "darwin":
		switch action {
		case "install":
			return []string{"launchctl", "bootstrap", "gui/iazio-harness"}
		case "uninstall":
			return []string{"launchctl", "bootout", "gui/iazio-harness"}
		default:
			return []string{"launchctl", action, "gui/iazio-harness"}
		}
	case "windows":
		switch action {
		case "install":
			args := []string{"schtasks", "/Create", "/TN", "iazio-harness"}
			if force {
				args = append(args, "/F")
			}
			return args
		case "start", "restart":
			return []string{"schtasks", "/Run", "/TN", "iazio-harness"}
		case "stop":
			return []string{"schtasks", "/End", "/TN", "iazio-harness"}
		case "uninstall":
			return []string{"schtasks", "/Delete", "/TN", "iazio-harness", "/F"}
		default:
			return []string{"schtasks", "/Query", "/TN", "iazio-harness"}
		}
	default:
		if action == "install" {
			args := []string{"systemctl", "--user", "enable", "--now"}
			if force {
				args = append(args, "--force")
			}
			return append(args, "iazio-harness.service")
		}
		return []string{"systemctl", "--user", action, "iazio-harness.service"}
	}
}

func darwinPlist(bin, logPath string) string {
	apiXml := ""
	if apiEnv := resolveHarnessAPIURL(); apiEnv != "" {
		apiXml = fmt.Sprintf("\n\t\t<key>IAZIO_HARNESS_API_URL</key>\n\t\t<string>%s</string>", apiEnv)
	}
	homeXml := ""
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		homeXml = fmt.Sprintf("\n\t\t<key>HOME</key>\n\t\t<string>%s</string>", home)
	}
	userXml := ""
	if u := os.Getenv("USER"); u != "" {
		userXml = fmt.Sprintf("\n\t\t<key>USER</key>\n\t\t<string>%s</string>", u)
	} else if curr, err := user.Current(); err == nil && curr.Username != "" {
		userXml = fmt.Sprintf("\n\t\t<key>USER</key>\n\t\t<string>%s</string>", curr.Username)
	}
	pathXml := ""
	if p := os.Getenv("PATH"); p != "" {
		pathXml = fmt.Sprintf("\n\t\t<key>PATH</key>\n\t\t<string>%s</string>", p)
	}
	maintenancePath := ""
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		maintenancePath = filepath.Join(home, ".iazio", "maintenance.lock")
	} else {
		maintenancePath = filepath.Join(".iazio", "maintenance.lock")
	}
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
		<string>daemon</string>
	</array>
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
		<key>PathState</key>
		<dict>
			<key>%s</key>
			<false/>
		</dict>
	</dict>
	<key>RunAtLoad</key>
	<true/>
	<key>StandardOutPath</key>
	<string>%s</string>
	<key>StandardErrorPath</key>
	<string>%s</string>
	<key>EnvironmentVariables</key>
	<dict>
		<key>IAZIO_HARNESS_NONINTERACTIVE</key>
		<string>1</string>%s%s%s%s
	</dict>
</dict>
</plist>
`, harnessLabel, bin, maintenancePath, logPath, logPath, apiXml, homeXml, userXml, pathXml)
}

// DarwinWatcherPlist creates the LaunchAgent definition that triggers on binary replacement.
func DarwinWatcherPlist(bin, logPath string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
		<string>service</string>
		<string>restart</string>
	</array>
	<key>WatchPaths</key>
	<array>
		<string>%s</string>
	</array>
	<key>StandardOutPath</key>
	<string>%s</string>
	<key>StandardErrorPath</key>
	<string>%s</string>
</dict>
</plist>
`, watcherLabel, bin, bin, logPath, logPath)
}

// DarwinWatchdogPlist creates the LaunchAgent definition that periodically revives the service.
func DarwinWatchdogPlist(bin, logPath string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
		<string>service</string>
		<string>revive-check</string>
	</array>
	<key>StartInterval</key>
	<integer>60</integer>
	<key>StandardOutPath</key>
	<string>%s</string>
	<key>StandardErrorPath</key>
	<string>%s</string>
</dict>
</plist>
`, watchdogLabel, bin, logPath, logPath)
}

// InstallDarwinResilience installs and enables the watcher and watchdog LaunchAgents.
func InstallDarwinResilience(home, bin string, run func(name string, args ...string) error) error {
	if run == nil {
		run = execCommand
	}
	launchAgentsDir := filepath.Join(home, "Library", "LaunchAgents")
	logDir := filepath.Join(home, ".iazio", "logs")
	logPath := filepath.Join(logDir, "harness.log")
	if err := os.MkdirAll(launchAgentsDir, 0o755); err != nil {
		return fmt.Errorf("create LaunchAgents directory: %w", err)
	}
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return fmt.Errorf("create log directory: %w", err)
	}

	watcherPlistPath := filepath.Join(launchAgentsDir, watcherLabel+".plist")
	if err := os.WriteFile(watcherPlistPath, []byte(DarwinWatcherPlist(bin, logPath)), 0o644); err != nil {
		return fmt.Errorf("write watcher plist: %w", err)
	}

	watchdogPlistPath := filepath.Join(launchAgentsDir, watchdogLabel+".plist")
	if err := os.WriteFile(watchdogPlistPath, []byte(DarwinWatchdogPlist(bin, logPath)), 0o644); err != nil {
		return fmt.Errorf("write watchdog plist: %w", err)
	}

	domain := "gui/" + strconv.Itoa(os.Getuid())
	watcherTarget := domain + "/" + watcherLabel
	watchdogTarget := domain + "/" + watchdogLabel

	_ = run("launchctl", "bootout", watcherTarget)
	if err := run("launchctl", "bootstrap", domain, watcherPlistPath); err != nil {
		return fmt.Errorf("launchctl bootstrap watcher: %w", err)
	}
	if err := run("launchctl", "enable", watcherTarget); err != nil {
		return fmt.Errorf("launchctl enable watcher: %w", err)
	}

	_ = run("launchctl", "bootout", watchdogTarget)
	if err := run("launchctl", "bootstrap", domain, watchdogPlistPath); err != nil {
		return fmt.Errorf("launchctl bootstrap watchdog: %w", err)
	}
	if err := run("launchctl", "enable", watchdogTarget); err != nil {
		return fmt.Errorf("launchctl enable watchdog: %w", err)
	}
	return nil
}

// UninstallDarwinResilience unloads and deletes the watcher and watchdog LaunchAgents.
func UninstallDarwinResilience(home string, run func(name string, args ...string) error) error {
	if run == nil {
		run = execCommand
	}
	domain := "gui/" + strconv.Itoa(os.Getuid())
	_ = run("launchctl", "bootout", domain+"/"+watcherLabel)
	_ = run("launchctl", "bootout", domain+"/"+watchdogLabel)

	launchAgentsDir := filepath.Join(home, "Library", "LaunchAgents")
	_ = os.Remove(filepath.Join(launchAgentsDir, watcherLabel+".plist"))
	_ = os.Remove(filepath.Join(launchAgentsDir, watchdogLabel+".plist"))
	return nil
}

func harnessLogPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "/tmp/iazio-harness.log"
	}
	return filepath.Join(home, ".iazio", "logs", "harness.log")
}

// InstallDarwin registers and starts the user-level LaunchAgent on macOS.
func InstallDarwin(bin string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home: %w", err)
	}
	return InstallDarwinAt(home, bin, execCommand)
}

// InstallDarwinAt writes the plist into ~/Library/LaunchAgents and bootstraps it.
func InstallDarwinAt(home, bin string, run func(name string, args ...string) error) error {
	if run == nil {
		return errors.New("launchctl runner is required")
	}
	dir := filepath.Join(home, "Library", "LaunchAgents")
	logDir := filepath.Join(home, ".iazio", "logs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return err
	}
	plistPath := filepath.Join(dir, harnessLabel+".plist")
	logPath := filepath.Join(logDir, "harness.log")
	if err := os.WriteFile(plistPath, []byte(darwinPlist(bin, logPath)), 0o644); err != nil {
		return err
	}
	domain := "gui/" + strconv.Itoa(os.Getuid())
	target := domain + "/" + harnessLabel
	_ = run("launchctl", "bootout", target)
	if err := run("launchctl", "bootstrap", domain, plistPath); err != nil {
		return fmt.Errorf("launchctl bootstrap: %w", err)
	}
	if err := run("launchctl", "enable", target); err != nil {
		return fmt.Errorf("launchctl enable: %w", err)
	}
	return nil
}

func linuxUnit(bin string) string {
	apiLine := ""
	if apiEnv := resolveHarnessAPIURL(); apiEnv != "" {
		apiLine = fmt.Sprintf("Environment=IAZIO_HARNESS_API_URL=%s\n", apiEnv)
	}
	return fmt.Sprintf(`[Unit]
Description=iazio-harness daemon and execution supervisor
After=network.target

[Service]
Type=simple
ExecStart=%s daemon
Restart=always
RestartSec=3s
StartLimitIntervalSec=0
OOMScoreAdjust=-500
Environment=IAZIO_HARNESS_NONINTERACTIVE=1
%s
[Install]
WantedBy=default.target
`, bin, apiLine)
}

// LinuxWatcherPath returns the systemd .path unit definition to monitor binary replacements.
func LinuxWatcherPath(bin string) string {
	return fmt.Sprintf(`[Unit]
Description=Watch iazio-harness binary for updates and dev replacements
After=network.target

[Path]
PathModified=%s
Unit=iazio-harness-watcher.service

[Install]
WantedBy=default.target
`, bin)
}

// LinuxWatcherService returns the systemd .service unit triggered by .path changes.
func LinuxWatcherService() string {
	return `[Unit]
Description=Auto-restart iazio-harness on binary replacement
After=network.target

[Service]
Type=oneshot
ExecStart=/usr/bin/systemctl --user restart iazio-harness.service
`
}

// LinuxWatchdogTimer returns the periodic timer definition that triggers revive-check.
func LinuxWatchdogTimer() string {
	return `[Unit]
Description=iazio-harness auto-revive watchdog timer
After=network.target

[Timer]
OnBootSec=1m
OnUnitActiveSec=2m
AccuracySec=10s
Unit=iazio-harness-watchdog.service

[Install]
WantedBy=timers.target default.target
`
}

// LinuxWatchdogService returns the watchdog service definition executing revive-check.
func LinuxWatchdogService(bin string) string {
	return fmt.Sprintf(`[Unit]
Description=iazio-harness auto-revive watchdog
After=network.target

[Service]
Type=oneshot
ExecStart=%s service revive-check
`, bin)
}

// InstallLinuxResilience installs and enables the .path watcher and .timer watchdog units.
func InstallLinuxResilience(home, bin string, run func(name string, args ...string) error) error {
	if run == nil {
		run = execCommand
	}
	unitDir := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		return err
	}

	pathUnitFile := filepath.Join(unitDir, "iazio-harness-watcher.path")
	if err := os.WriteFile(pathUnitFile, []byte(LinuxWatcherPath(bin)), 0o644); err != nil {
		return fmt.Errorf("write watcher.path: %w", err)
	}

	serviceUnitFile := filepath.Join(unitDir, "iazio-harness-watcher.service")
	if err := os.WriteFile(serviceUnitFile, []byte(LinuxWatcherService()), 0o644); err != nil {
		return fmt.Errorf("write watcher.service: %w", err)
	}

	timerUnitFile := filepath.Join(unitDir, "iazio-harness-watchdog.timer")
	if err := os.WriteFile(timerUnitFile, []byte(LinuxWatchdogTimer()), 0o644); err != nil {
		return fmt.Errorf("write watchdog.timer: %w", err)
	}

	dogUnitFile := filepath.Join(unitDir, "iazio-harness-watchdog.service")
	if err := os.WriteFile(dogUnitFile, []byte(LinuxWatchdogService(bin)), 0o644); err != nil {
		return fmt.Errorf("write watchdog.service: %w", err)
	}

	_ = run("systemctl", "--user", "daemon-reload")
	_ = run("systemctl", "--user", "enable", "--now", "iazio-harness-watcher.path")
	_ = run("systemctl", "--user", "enable", "--now", "iazio-harness-watchdog.timer")
	return nil
}

func copyBinaryAtomic(src, dest string) error {
	if src == dest {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	tmp := fmt.Sprintf("%s.tmp.%d", dest, time.Now().UnixNano())
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dest)
}

// DevInstall safely hot-swaps the active binary with srcBinary and ensures the service is running.
func DevInstall(srcBinary, home string, run func(name string, args ...string) error) error {
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		home = h
	}
	if run == nil {
		run = execCommand
	}
	if srcBinary == "" {
		exe, err := os.Executable()
		if err != nil {
			return fmt.Errorf("resolve executable: %w", err)
		}
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			srcBinary = resolved
		} else {
			srcBinary = exe
		}
	}
	srcBinary, err := filepath.Abs(srcBinary)
	if err != nil {
		return err
	}
	info, err := os.Stat(srcBinary)
	if err != nil {
		return fmt.Errorf("source binary not found: %w", err)
	}
	if info.IsDir() {
		return fmt.Errorf("source binary is a directory: %s", srcBinary)
	}

	destDir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return err
	}
	binName := "iazio-harness"
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	dest := filepath.Join(destDir, binName)

	if err := copyBinaryAtomic(srcBinary, dest); err != nil {
		return fmt.Errorf("atomic binary swap failed: %w", err)
	}

	// Remove maintenance lock if present
	_ = os.Remove(filepath.Join(home, ".iazio", "maintenance.lock"))

	// Trigger restart
	switch runtime.GOOS {
	case "darwin":
		domain := "gui/" + strconv.Itoa(os.Getuid())
		_ = run("launchctl", "kickstart", "-k", domain+"/"+harnessLabel)
	case "windows":
		_ = run("schtasks", "/End", "/TN", "iazio-harness")
		_ = run("schtasks", "/Run", "/TN", "iazio-harness")
	default:
		if IsSystemdAvailable() {
			_ = run("systemctl", "--user", "restart", "iazio-harness.service")
		} else {
			_ = StopUserDaemonAt(home)
			_ = StartUserDaemonAt(home, dest, run)
		}
	}
	return nil
}

// ReviveCheck verifies supervisor health and auto-restarts if inactive and not in maintenance mode.
func ReviveCheck(home string, run func(name string, args ...string) error) error {
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		home = h
	}
	if run == nil {
		run = execCommand
	}

	maintenancePath := filepath.Join(home, ".iazio", "maintenance.lock")
	if _, err := os.Stat(maintenancePath); err == nil {
		// Maintenance lock exists, do not revive
		return nil
	}

	if os.Getenv("IAZIO_DISABLE_SYSTEMD") == "1" {
		running, _ := StatusUserDaemonAt(home)
		if !running {
			binName := "iazio-harness"
			bin := filepath.Join(home, ".local", "bin", binName)
			_ = StartUserDaemonAt(home, bin, run)
		}
		return nil
	}

	switch runtime.GOOS {
	case "darwin":
		domain := "gui/" + strconv.Itoa(os.Getuid())
		out, err := execCommandOutput("launchctl", "list", harnessLabel)
		if err != nil || !strings.Contains(string(out), harnessLabel) {
			_ = run("launchctl", "kickstart", "-k", domain+"/"+harnessLabel)
		}
	case "windows":
		out, err := execCommandOutput("schtasks", "/Query", "/TN", "iazio-harness", "/FO", "LIST", "/V")
		if err == nil {
			running, _, ok := ParseSchtasksStatus(string(out))
			if ok && !running {
				_ = run("schtasks", "/Run", "/TN", "iazio-harness")
			}
		}
	default:
		if IsSystemdAvailable() {
			out, err := execCommandOutput("systemctl", "--user", "is-active", "iazio-harness.service")
			if err != nil || strings.TrimSpace(string(out)) != "active" {
				_ = run("systemctl", "--user", "restart", "iazio-harness.service")
			}
		} else {
			running, _ := StatusUserDaemonAt(home)
			if !running {
				binName := "iazio-harness"
				bin := filepath.Join(home, ".local", "bin", binName)
				_ = StartUserDaemonAt(home, bin, run)
			}
		}
	}
	return nil
}

// ReconcileServiceConfiguration inspects user service units for iazio-harness and corrects missing or drifted configuration.
func ReconcileServiceConfiguration(home, bin string, run func(name string, args ...string) error) error {
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		home = h
	}
	if run == nil {
		run = execCommand
	}
	if bin == "" {
		bin = filepath.Join(home, ".local", "bin", "iazio-harness")
		if runtime.GOOS == "windows" {
			bin += ".exe"
		}
	}

	switch runtime.GOOS {
	case "darwin":
		launchAgentsDir := filepath.Join(home, "Library", "LaunchAgents")
		_ = os.MkdirAll(launchAgentsDir, 0o755)
		logDir := filepath.Join(home, ".iazio", "logs")
		_ = os.MkdirAll(logDir, 0o755)
		logPath := filepath.Join(logDir, "harness.log")

		harnessPlist := filepath.Join(launchAgentsDir, harnessLabel+".plist")
		data, err := os.ReadFile(harnessPlist)
		domain := "gui/" + strconv.Itoa(os.Getuid())
		target := domain + "/" + harnessLabel
		if err != nil || !strings.Contains(string(data), bin) {
			_ = os.WriteFile(harnessPlist, []byte(darwinPlist(bin, logPath)), 0o644)
			_ = run("launchctl", "bootout", target)
			_ = run("launchctl", "bootstrap", domain, harnessPlist)
			_ = run("launchctl", "enable", target)
		}
		_ = InstallDarwinResilience(home, bin, run)
		return nil
	case "windows":
		tasksDir := filepath.Join(home, ".iazio", "tasks")
		batPath := filepath.Join(tasksDir, "iazio-harness.bat")
		xmlPath := filepath.Join(tasksDir, "iazio-harness.xml")
		needsInstall := false
		if data, err := os.ReadFile(batPath); err != nil || !strings.Contains(string(data), bin) {
			needsInstall = true
		} else if xData, err := os.ReadFile(xmlPath); err != nil || (!strings.Contains(string(xData), "PT1M") && !bytes.Contains(xData, []byte{'P', 0, 'T', 0, '1', 0, 'M', 0})) {
			needsInstall = true
		}
		if needsInstall {
			return InstallWindowsAt(home, bin, windowsLogonUser(), run)
		}
		return nil
	default:
		if IsSystemdAvailable() {
			unitDir := filepath.Join(home, ".config", "systemd", "user")
			_ = os.MkdirAll(unitDir, 0o755)
			serviceFile := filepath.Join(unitDir, "iazio-harness.service")
			needsReload := false
			expectedService := linuxUnit(bin)
			data, err := os.ReadFile(serviceFile)
			if err != nil || !strings.Contains(string(data), bin) || !strings.Contains(string(data), "daemon") {
				if err := os.WriteFile(serviceFile, []byte(expectedService), 0o644); err != nil {
					return fmt.Errorf("write iazio-harness.service: %w", err)
				}
				needsReload = true
			}
			if needsReload {
				_ = run("systemctl", "--user", "daemon-reload")
			}
			_ = run("systemctl", "--user", "enable", "iazio-harness.service")
			_ = InstallLinuxResilience(home, bin, run)
			return nil
		}
		return InstallUserDaemonAt(home, bin, run)
	}
}

// PauseService sets maintenance mode and stops the supervisor.
func PauseService(home string, run func(name string, args ...string) error) error {
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		home = h
	}
	if run == nil {
		run = execCommand
	}
	iazioDir := filepath.Join(home, ".iazio")
	_ = os.MkdirAll(iazioDir, 0o755)
	if err := os.WriteFile(filepath.Join(iazioDir, "maintenance.lock"), []byte(time.Now().Format(time.RFC3339)), 0o644); err != nil {
		return err
	}
	if os.Getenv("IAZIO_DISABLE_SYSTEMD") == "1" {
		_ = StopUserDaemonAt(home)
		return nil
	}
	switch runtime.GOOS {
	case "darwin":
		domain := "gui/" + strconv.Itoa(os.Getuid())
		_ = run("launchctl", "stop", domain+"/"+harnessLabel)
	case "windows":
		_ = run("schtasks", "/End", "/TN", "iazio-harness")
	default:
		if IsSystemdAvailable() {
			_ = run("systemctl", "--user", "stop", "iazio-harness.service")
		} else {
			_ = StopUserDaemonAt(home)
		}
	}
	return nil
}

// ResumeService clears maintenance mode and restarts the supervisor.
func ResumeService(home string, run func(name string, args ...string) error) error {
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		home = h
	}
	if run == nil {
		run = execCommand
	}
	_ = os.Remove(filepath.Join(home, ".iazio", "maintenance.lock"))
	if os.Getenv("IAZIO_DISABLE_SYSTEMD") == "1" {
		binName := "iazio-harness"
		bin := filepath.Join(home, ".local", "bin", binName)
		_ = StartUserDaemonAt(home, bin, run)
		return nil
	}

	switch runtime.GOOS {
	case "darwin":
		domain := "gui/" + strconv.Itoa(os.Getuid())
		_ = run("launchctl", "start", domain+"/"+harnessLabel)
	case "windows":
		_ = run("schtasks", "/Run", "/TN", "iazio-harness")
	default:
		if IsSystemdAvailable() {
			_ = run("systemctl", "--user", "start", "iazio-harness.service")
		} else {
			binName := "iazio-harness"
			bin := filepath.Join(home, ".local", "bin", binName)
			_ = StartUserDaemonAt(home, bin, run)
		}
	}
	return nil
}

// ParseSchtasksStatus inspects output from `schtasks /Query /TN ... /FO LIST /V`
func ParseSchtasksStatus(out string) (running bool, status string, ok bool) {
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "Status:") {
			parts := strings.SplitN(trimmed, ":", 2)
			if len(parts) == 2 {
				val := strings.TrimSpace(parts[1])
				return strings.EqualFold(val, "Running"), val, true
			}
		}
	}
	return false, "", false
}

// IsWSL returns true if running inside Windows Subsystem for Linux.
func IsWSL() bool {
	return IsWSLWith(os.Getenv, func() ([]byte, error) {
		return os.ReadFile("/proc/sys/kernel/osrelease")
	})
}

// IsWSLWith detects WSL using the provided environment and osrelease fetcher.
func IsWSLWith(getenv func(string) string, readOSRelease func() ([]byte, error)) bool {
	if getenv("WSL_DISTRO_NAME") != "" || getenv("WSL_INTEROP") != "" {
		return true
	}
	if readOSRelease != nil {
		if b, err := readOSRelease(); err == nil {
			s := strings.ToLower(string(b))
			if strings.Contains(s, "microsoft") || strings.Contains(s, "wsl") {
				return true
			}
		}
	}
	return false
}

// IsSystemdAvailable checks whether systemd user bus is operational.
func IsSystemdAvailable() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	if os.Getenv("IAZIO_DISABLE_SYSTEMD") == "1" {
		return false
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		return false
	}
	cmd := exec.Command("systemctl", "--user", "is-system-running")
	out, _ := cmd.CombinedOutput()
	state := strings.TrimSpace(string(out))
	return state == "running" || state == "degraded"
}

// InstallLinux registers and enables the user-level systemd unit on Linux.
func InstallLinux(bin string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home: %w", err)
	}
	return InstallLinuxAt(home, bin, execCommand)
}

// InstallUserDaemon writes the fallback user-level launcher script for environments without systemd.
func InstallUserDaemon(bin string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home: %w", err)
	}
	return InstallUserDaemonAt(home, bin, execCommand)
}

// InstallUserDaemonAt writes launcher script and starts the fallback background daemon.
func InstallUserDaemonAt(home, bin string, run func(name string, args ...string) error) error {
	if run == nil {
		run = execCommand
	}
	dir := filepath.Join(home, ".local", "bin")
	logDir := filepath.Join(home, ".iazio", "logs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return err
	}
	runnerPath := filepath.Join(dir, "iazio-harness-runner.sh")
	logPath := filepath.Join(logDir, "harness.log")
	pidPath := filepath.Join(home, ".iazio", "harness.pid")

	script := fmt.Sprintf(`#!/bin/sh
PIDFILE="%s"
LOGFILE="%s"
BIN="%s"

if [ -f "$PIDFILE" ]; then
    PID=$(cat "$PIDFILE" 2>/dev/null)
    if [ -n "$PID" ] && kill -0 "$PID" 2>/dev/null; then
        exit 0
    fi
fi

export IAZIO_HARNESS_NONINTERACTIVE=1
nohup "$BIN" daemon >> "$LOGFILE" 2>&1 &
echo $! > "$PIDFILE"
`, pidPath, logPath, bin)

	if err := os.WriteFile(runnerPath, []byte(script), 0o755); err != nil {
		return err
	}

	return StartUserDaemonAt(home, bin, run)
}

// StartUserDaemonAt starts the daemon if not currently running.
func StartUserDaemonAt(home, bin string, run func(name string, args ...string) error) error {
	if run == nil {
		run = execCommand
	}
	runnerPath := filepath.Join(home, ".local", "bin", "iazio-harness-runner.sh")
	if _, err := os.Stat(runnerPath); err != nil {
		return fmt.Errorf("runner script missing; run 'service install' first")
	}
	return run("sh", runnerPath)
}

// StopUserDaemonAt terminates the user daemon if running.
func StopUserDaemonAt(home string) error {
	pidPath := filepath.Join(home, ".iazio", "harness.pid")
	data, err := os.ReadFile(pidPath)
	if err != nil {
		return nil
	}
	pidStr := strings.TrimSpace(string(data))
	pid, err := strconv.Atoi(pidStr)
	if err != nil || pid <= 0 {
		_ = os.Remove(pidPath)
		return nil
	}
	proc, err := os.FindProcess(pid)
	if err == nil {
		_ = proc.Signal(syscall.SIGTERM)
		time.Sleep(100 * time.Millisecond)
		if isDaemonPIDAlive(pidPath) {
			_ = proc.Kill()
		}
	}
	_ = os.Remove(pidPath)
	return nil
}

// StatusUserDaemonAt checks if the background daemon process is alive.
func StatusUserDaemonAt(home string) (bool, int) {
	pidPath := filepath.Join(home, ".iazio", "harness.pid")
	data, err := os.ReadFile(pidPath)
	if err != nil {
		return false, 0
	}
	pidStr := strings.TrimSpace(string(data))
	pid, err := strconv.Atoi(pidStr)
	if err != nil || pid <= 0 {
		return false, 0
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false, 0
	}
	if err := proc.Signal(syscall.Signal(0)); err == nil {
		return true, pid
	}
	return false, 0
}

// UninstallUserDaemonAt cleans up fallback runner scripts and terminates daemon.
func UninstallUserDaemonAt(home string, run ...func(name string, args ...string) error) error {
	_ = StopUserDaemonAt(home)
	runnerPath := filepath.Join(home, ".local", "bin", "iazio-harness-runner.sh")
	_ = os.Remove(runnerPath)
	return nil
}

func isDaemonPIDAlive(pidPath string) bool {
	data, err := os.ReadFile(pidPath)
	if err != nil {
		return false
	}
	pidStr := strings.TrimSpace(string(data))
	pid, err := strconv.Atoi(pidStr)
	if err != nil || pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

// InstallLinuxAt writes the systemd user unit and enables it via systemctl.
func InstallLinuxAt(home, bin string, run func(name string, args ...string) error) error {
	if run == nil {
		return errors.New("systemctl runner is required")
	}
	dir := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	unitPath := filepath.Join(dir, "iazio-harness.service")
	if err := os.WriteFile(unitPath, []byte(linuxUnit(bin)), 0o644); err != nil {
		return err
	}
	if err := run("systemctl", "--user", "daemon-reload"); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w", err)
	}
	if err := run("systemctl", "--user", "enable", "--now", "iazio-harness.service"); err != nil {
		return fmt.Errorf("systemctl enable --now: %w", err)
	}
	// Install path and timer resilience
	_ = InstallLinuxResilience(home, bin, run)
	return nil
}

// InstallWindows registers a scheduled task for the current user running on logon.
func InstallWindows(bin string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home: %w", err)
	}
	return InstallWindowsAt(home, bin, windowsLogonUser(), execCommand)
}

// InstallWindowsAt writes the launcher and task XML, then registers the task.
func InstallWindowsAt(home, bin, user string, run func(name string, args ...string) error) error {
	if run == nil {
		return errors.New("schtasks runner is required")
	}
	dir := filepath.Join(home, ".iazio", "tasks")
	logDir := filepath.Join(home, ".iazio", "logs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return err
	}
	batPath := filepath.Join(dir, "iazio-harness.bat")
	xmlPath := filepath.Join(dir, "iazio-harness.xml")
	logPath := filepath.Join(logDir, "harness.log")

	apiLine := ""
	if apiEnv := resolveHarnessAPIURL(); apiEnv != "" {
		apiLine = fmt.Sprintf("set \"IAZIO_HARNESS_API_URL=%s\"\r\n", apiEnv)
	}
	bat := fmt.Sprintf("@echo off\r\nset \"IAZIO_HARNESS_NONINTERACTIVE=1\"\r\n%s\"%s\" daemon >> \"%s\" 2>&1\r\n", apiLine, bin, logPath)
	xmlDoc := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <Triggers>
    <LogonTrigger>
      <Enabled>true</Enabled>
      <UserId>%s</UserId>
    </LogonTrigger>
    <TimeTrigger>
      <Enabled>true</Enabled>
      <StartBoundary>2026-01-01T00:00:00</StartBoundary>
      <Repetition>
        <Interval>PT5M</Interval>
        <StopAtDurationEnd>false</StopAtDurationEnd>
      </Repetition>
    </TimeTrigger>
  </Triggers>
  <Principals>
    <Principal id="Author">
      <LogonType>InteractiveToken</LogonType>
      <RunLevel>LeastPrivilege</RunLevel>
    </Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <StartWhenAvailable>true</StartWhenAvailable>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <RestartOnFailure><Interval>PT1M</Interval><Count>999</Count></RestartOnFailure>
  </Settings>
  <Actions Context="Author">
    <Exec>
      <Command>%s</Command>
    </Exec>
  </Actions>
</Task>`, user, batPath)

	if err := os.WriteFile(batPath, []byte(bat), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(xmlPath, encodeUTF16LEWithBOM(xmlDoc), 0o644); err != nil {
		return err
	}

	_ = run("schtasks", "/Delete", "/TN", "iazio-harness", "/F")
	args := []string{"schtasks", "/Create", "/TN", "iazio-harness", "/XML", xmlPath, "/F"}
	if err := run(args[0], args[1:]...); err != nil {
		return fmt.Errorf("schtasks register: %w", err)
	}
	_ = run("schtasks", "/Run", "/TN", "iazio-harness")
	return nil
}

func encodeUTF16LEWithBOM(s string) []byte {
	u16 := utf16.Encode([]rune(s))
	buf := make([]byte, 2+len(u16)*2)
	buf[0] = 0xFF
	buf[1] = 0xFE
	for i, v := range u16 {
		buf[2+i*2] = byte(v)
		buf[2+i*2+1] = byte(v >> 8)
	}
	return buf
}

func windowsLogonUser() string {
	u, err := user.Current()
	if err == nil && u != nil && strings.TrimSpace(u.Username) != "" {
		return strings.TrimSpace(u.Username)
	}
	domain := strings.TrimSpace(os.Getenv("USERDOMAIN"))
	usr := strings.TrimSpace(os.Getenv("USERNAME"))
	if domain != "" && usr != "" {
		return domain + `\` + usr
	}
	if usr != "" {
		return usr
	}
	return "Administrator"
}

var execCommand = func(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	return cmd.Run()
}

var execCommandOutput = func(name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	return cmd.CombinedOutput()
}

// SetExecCommandForTest overrides execCommand and execCommandOutput for unit testing.
func SetExecCommandForTest(runner func(name string, args ...string) error, statusChecker ...func(name string, args ...string) ([]byte, error)) func() {
	prev := execCommand
	prevOut := execCommandOutput
	if runner != nil {
		execCommand = runner
	}
	if len(statusChecker) > 0 && statusChecker[0] != nil {
		execCommandOutput = statusChecker[0]
	}
	return func() {
		execCommand = prev
		execCommandOutput = prevOut
	}
}
