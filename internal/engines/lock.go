package engines

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

// PrimaryLockPath returns the path to ~/.iazio/locks/<hash>.lock for a checkout.
func PrimaryLockPath(checkout, lockDir string) string {
	if lockDir == "" {
		lockDir = DefaultLockDir()
	}
	sum := sha256.Sum256([]byte(filepath.Clean(checkout)))
	return filepath.Join(lockDir, hex.EncodeToString(sum[:])+".lock")
}

// FallbackLockPath returns the path to <checkout>/.git/execution.lock
func FallbackLockPath(checkout string) string {
	gitDir := filepath.Join(checkout, ".git")
	if fi, err := os.Stat(gitDir); err == nil && !fi.IsDir() {
		return filepath.Join(checkout, ".git-execution.lock")
	}
	return filepath.Join(gitDir, "execution.lock")
}

// Hold takes an exclusive non-blocking lock for the checkout.
// The lock file lives outside the checkout so the worktree stays clean.
// If the primary lockDir is unwritable, it automatically falls back to .git/execution.lock.
// Release when the process is finished with the job.
func Hold(checkout, lockDir string) (*Lock, error) {
	if checkout == "" {
		return nil, errors.New("checkout path is empty")
	}
	if lockDir == "" {
		lockDir = DefaultLockDir()
	}

	var f *os.File
	var lockPath string
	var err error

	if mkErr := os.MkdirAll(lockDir, 0o755); mkErr == nil {
		lockPath = PrimaryLockPath(checkout, lockDir)
		f, err = os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	}

	if f == nil || err != nil {
		lockPath = FallbackLockPath(checkout)
		_ = os.MkdirAll(filepath.Dir(lockPath), 0o755)
		f, err = os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
		if err != nil {
			return nil, fmt.Errorf("failed to open lock file at %s: %w", lockPath, err)
		}
	}

	if err := fileLock(f); err != nil {
		_ = f.Close()
		return nil, err
	}
	return &Lock{f: f, Path: lockPath}, nil
}

// Release unlocks the checkout.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := fileUnlock(l.f)
	cerr := l.f.Close()
	l.f = nil
	if err != nil {
		return err
	}
	return cerr
}
