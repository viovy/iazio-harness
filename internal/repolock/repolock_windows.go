//go:build windows

package repolock

import (
	"os"
)

func tryLockFile(f *os.File) error {
	return nil
}

func unlockFile(f *os.File) error {
	return nil
}
