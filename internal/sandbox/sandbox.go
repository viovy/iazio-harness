// Package sandbox provides deterministic, isolated ephemeral worktree sandboxing for agent evaluations.
package sandbox

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/viovy/iazio-harness/internal/procutil"
)

// Sandbox represents an isolated ephemeral filesystem snapshot.
type Sandbox struct {
	OriginalPath string
	SandboxPath  string
	CreatedAt    time.Time
}

// Options configures sandbox creation.
type Options struct {
	Prefix      string
	CopyGit     bool
	ExcludeDirs []string
}

// Create creates an ephemeral sandbox by copying the source directory into an isolated tempdir.
func Create(sourcePath string, opts Options) (*Sandbox, error) {
	if sourcePath == "" {
		return nil, fmt.Errorf("source path is required")
	}
	absSource, err := filepath.Abs(sourcePath)
	if err != nil {
		return nil, fmt.Errorf("resolve source path: %w", err)
	}
	fi, err := os.Stat(absSource)
	if err != nil {
		return nil, fmt.Errorf("stat source path: %w", err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("source path %s is not a directory", absSource)
	}

	prefix := opts.Prefix
	if prefix == "" {
		prefix = "iazio-harness-sandbox-"
	}

	tempDir, err := os.MkdirTemp("", prefix)
	if err != nil {
		return nil, fmt.Errorf("create sandbox tempdir: %w", err)
	}

	sb := &Sandbox{
		OriginalPath: absSource,
		SandboxPath:  tempDir,
		CreatedAt:    time.Now().UTC(),
	}

	excludes := map[string]bool{
		".git":         !opts.CopyGit,
		"node_modules": true,
		"tmp":          true,
		".dart_tool":   true,
		"build":        true,
	}
	for _, e := range opts.ExcludeDirs {
		excludes[e] = true
	}

	err = filepath.WalkDir(absSource, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, relErr := filepath.Rel(absSource, path)
		if relErr != nil {
			return relErr
		}
		if rel == "." {
			return nil
		}

		base := d.Name()
		if d.IsDir() && excludes[base] {
			return filepath.SkipDir
		}

		targetPath := filepath.Join(tempDir, rel)
		if d.IsDir() {
			return os.MkdirAll(targetPath, 0755)
		}

		// Copy file content
		return copyFile(path, targetPath)
	})

	if err != nil {
		_ = os.RemoveAll(tempDir)
		return nil, fmt.Errorf("populate sandbox: %w", err)
	}

	return sb, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	info, err := in.Stat()
	if err != nil {
		return err
	}

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode())
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()

	_, err = io.Copy(out, in)
	return err
}

// Diff returns the unified diff of modifications between OriginalPath and SandboxPath.
func (s *Sandbox) Diff() (string, error) {
	if s == nil || s.SandboxPath == "" || s.OriginalPath == "" {
		return "", fmt.Errorf("sandbox is not initialized")
	}

	cmd := procutil.Command("git", "--git-dir=/dev/null", "diff", "--no-index", "--no-color", s.OriginalPath, s.SandboxPath)
	cmd.Dir = s.SandboxPath
	out, err := cmd.CombinedOutput()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return strings.TrimSpace(string(out)), nil
		}
		return "", fmt.Errorf("git diff failed: %w (output: %s)", err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// Cleanup removes the ephemeral sandbox directory.
func (s *Sandbox) Cleanup() error {
	if s == nil || s.SandboxPath == "" {
		return nil
	}
	return os.RemoveAll(s.SandboxPath)
}
