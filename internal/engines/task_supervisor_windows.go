//go:build windows

package engines

import (
	"os/exec"
	"strconv"
)

func killProcessPlatform(pid int) error {
	return exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(pid)).Run()
}
