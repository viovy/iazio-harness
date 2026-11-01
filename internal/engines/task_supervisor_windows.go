//go:build windows

package engines

import (
	"strconv"

	"github.com/viovy/iazio-harness/internal/procutil"
)

func killProcessPlatform(pid int) error {
	return procutil.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(pid)).Run()
}
