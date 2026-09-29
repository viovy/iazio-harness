//go:build windows

package engines

import "os"

func fileLock(f *os.File) error {
	return nil
}

func fileUnlock(f *os.File) error {
	return nil
}
