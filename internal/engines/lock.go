package engines

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// ErrLockHeld is returned when the checkout lock is already held.
// Callers must not exec the IDE CLI.
var ErrLockHeld = errors.New("checkout lock held")

// Lock is an exclusive hold on one checkout for the process lifetime of the job.
type Lock struct {
	f    *os.File
	Path string
}

// DefaultLockDir returns ~/.iazio/locks.
func DefaultLockDir() string {
	if p := os.Getenv("IAZIO_HARNESS_LOCK_DIR"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(".iazio", "locks")
	}
	return filepath.Join(home, ".iazio", "locks")
}

// CheckoutDir returns the docs hub for story_refinement and the worktree otherwise.
func CheckoutDir(kind, worktree, docsHub string) (string, error) {
	path := worktree
	if kind == KindStoryRefinement {
		path = docsHub
	}
	if path == "" {
		return "", errors.New("checkout path is empty")
	}
	return path, nil
}

// Hold takes an exclusive non-blocking lock for the checkout.
// The lock file lives outside the checkout so the worktree stays clean.
// Release when the process is finished with the job.
func Hold(checkout, lockDir string) (*Lock, error) {
	if checkout == "" {
		return nil, errors.New("checkout path is empty")
	}
	if lockDir == "" {
		lockDir = DefaultLockDir()
	}
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(filepath.Clean(checkout)))
	path := filepath.Join(lockDir, hex.EncodeToString(sum[:])+".lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrLockHeld
		}
		return nil, err
	}
	return &Lock{f: f, Path: path}, nil
}

// Release unlocks the checkout.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	cerr := l.f.Close()
	l.f = nil
	if err != nil {
		return err
	}
	return cerr
}
