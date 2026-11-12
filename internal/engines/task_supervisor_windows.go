//go:build windows

package engines

import (
	"strconv"

	"github.com/viovy/iazio-core/pkg/procutil"
)

func killProcessPlatform(pid int) error {
	return procutil.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(pid)).Run()
}
