//go:build !windows

package engines

import (
	"os/exec"
	"syscall"
)

// SetProcGroup configures cmd to run in its own process group on Unix.
func SetProcGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// PostStartProcessGroup is a hook called immediately after cmd.Start.
func PostStartProcessGroup(cmd *exec.Cmd) {}

// StopProcessGroup attempts graceful termination via SIGTERM to the process group.
func StopProcessGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid
	if pid <= 0 {
		return
	}
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
}

// KillProcessGroup forcefully terminates the process group via SIGKILL.
func KillProcessGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid
	if pid <= 0 {
		return
	}
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

func setProcGroup(cmd *exec.Cmd)          { SetProcGroup(cmd) }
func postStartProcessGroup(cmd *exec.Cmd) { PostStartProcessGroup(cmd) }
func stopProcessGroup(cmd *exec.Cmd)      { StopProcessGroup(cmd) }
func killProcessGroup(cmd *exec.Cmd)      { KillProcessGroup(cmd) }

func checkProcGroupAttrs(cmd *exec.Cmd) bool {
	return cmd != nil && cmd.SysProcAttr != nil && cmd.SysProcAttr.Setpgid
}
