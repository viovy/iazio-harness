//go:build !windows

package engines

import (
	"errors"
	"os"
	"syscall"
)

// FileLock acquires a non-blocking exclusive lock on f.
func FileLock(f *os.File) error {
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EWOULDBLOCK) {
			return ErrLockHeld
		}
		return err
	}
	return nil
}

// FileUnlock releases the lock on f.
func FileUnlock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}

func fileLock(f *os.File) error   { return FileLock(f) }
func fileUnlock(f *os.File) error { return FileUnlock(f) }
