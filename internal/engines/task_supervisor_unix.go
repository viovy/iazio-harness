//go:build !windows

package engines

import (
	"syscall"
	"time"
)

func killProcessPlatform(pid int) error {
	pgid, err := syscall.Getpgid(pid)
	target := pid
	if err == nil && pgid > 1 && pgid != syscall.Getpgrp() {
		target = -pgid
	}

	_ = syscall.Kill(target, syscall.SIGTERM)
	_ = syscall.Kill(pid, syscall.SIGTERM)
	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			return nil // Process exited
		}
		time.Sleep(50 * time.Millisecond)
	}

	if target < 0 {
		_ = syscall.Kill(target, syscall.SIGKILL)
	}
	return syscall.Kill(pid, syscall.SIGKILL)
}
