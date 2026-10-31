//go:build windows

package engines

import (
	"os/exec"
	"strconv"
	"syscall"
)

func killProcessPlatform(pid int) error {
	cmd := exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(pid))
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: 0x08000000,
		HideWindow:    true,
	}
	return cmd.Run()
}
