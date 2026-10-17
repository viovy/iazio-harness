// Package sandbox provides copy-on-write workspace snapshots and diff inspection.
package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// WorkspaceSnapshot represents a copy-on-write snapshot of a directory.
type WorkspaceSnapshot struct {
	ID           string            `json:"id"`
	OriginalPath string            `json:"original_path"`
	SnapshotPath string            `json:"snapshot_path"`
	CreatedAt    time.Time         `json:"created_at"`
	FileHashes   map[string]string `json:"file_hashes"` // relative path -> sha256
}

// CreateSnapshot creates a snapshot of sourceDir in an isolated snapshot directory.
func CreateSnapshot(sourceDir, snapshotID string) (*WorkspaceSnapshot, error) {
	if strings.TrimSpace(sourceDir) == "" {
		return nil, fmt.Errorf("sourceDir is required")
	}
	absSource, err := filepath.Abs(sourceDir)
	if err != nil {
		return nil, fmt.Errorf("abs sourceDir: %w", err)
	}

	if snapshotID == "" {
		snapshotID = fmt.Sprintf("snap-%d", time.Now().UnixNano())
	}

	tempDir, err := os.MkdirTemp("", "iazio-snapshot-"+snapshotID+"-*")
	if err != nil {
		return nil, fmt.Errorf("create snapshot tempdir: %w", err)
	}

	hashes := make(map[string]string)

	err = filepath.WalkDir(absSource, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(absSource, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}

		base := d.Name()
		if d.IsDir() {
			if base == ".git" || base == "node_modules" || base == "tmp" || base == ".dart_tool" {
				return filepath.SkipDir
			}
			return os.MkdirAll(filepath.Join(tempDir, rel), 0755)
		}

		targetFile := filepath.Join(tempDir, rel)
		h, err := copyAndHash(path, targetFile)
		if err != nil {
			return err
		}
		hashes[rel] = h
		return nil
	})

	if err != nil {
		_ = os.RemoveAll(tempDir)
		return nil, fmt.Errorf("populate snapshot: %w", err)
	}

	return &WorkspaceSnapshot{
		ID:           snapshotID,
		OriginalPath: absSource,
		SnapshotPath: tempDir,
		CreatedAt:    time.Now().UTC(),
		FileHashes:   hashes,
	}, nil
}

// RestoreSnapshot restores the snapshot into targetDir, resetting any modifications made during an evaluation.
func RestoreSnapshot(snap *WorkspaceSnapshot, targetDir string) error {
	if snap == nil || snap.SnapshotPath == "" {
		return fmt.Errorf("snapshot is nil or invalid")
	}

	absTarget, err := filepath.Abs(targetDir)
	if err != nil {
		return fmt.Errorf("abs targetDir: %w", err)
	}

	return filepath.WalkDir(snap.SnapshotPath, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(snap.SnapshotPath, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}

		destPath := filepath.Join(absTarget, rel)
		if d.IsDir() {
			return os.MkdirAll(destPath, 0755)
		}

		return copyFile(path, destPath)
	})
}

// ComputeDiff compares currentDir against the snapshot and returns added, modified, and deleted files.
func ComputeDiff(snap *WorkspaceSnapshot, currentDir string) (added, modified, deleted []string, err error) {
	if snap == nil {
		return nil, nil, nil, fmt.Errorf("snapshot is nil")
	}

	absCurrent, err := filepath.Abs(currentDir)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("abs currentDir: %w", err)
	}

	seen := make(map[string]bool)

	err = filepath.WalkDir(absCurrent, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(absCurrent, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}

		base := d.Name()
		if d.IsDir() {
			if base == ".git" || base == "node_modules" || base == "tmp" || base == ".dart_tool" {
				return filepath.SkipDir
			}
			return nil
		}

		seen[rel] = true
		currHash, err := hashFile(path)
		if err != nil {
			return err
		}

		origHash, exists := snap.FileHashes[rel]
		if !exists {
			added = append(added, rel)
		} else if origHash != currHash {
			modified = append(modified, rel)
		}
		return nil
	})

	if err != nil {
		return nil, nil, nil, err
	}

	for rel := range snap.FileHashes {
		if !seen[rel] {
			deleted = append(deleted, rel)
		}
	}

	return added, modified, deleted, nil
}

// Cleanup releases snapshot files from disk.
func (snap *WorkspaceSnapshot) Cleanup() error {
	if snap == nil || snap.SnapshotPath == "" {
		return nil
	}
	return os.RemoveAll(snap.SnapshotPath)
}

func copyAndHash(src, dst string) (string, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer in.Close()

	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return "", err
	}

	out, err := os.Create(dst)
	if err != nil {
		return "", err
	}
	defer out.Close()

	h := sha256.New()
	mw := io.MultiWriter(out, h)
	if _, err := io.Copy(mw, in); err != nil {
		return "", err
	}

	return hex.EncodeToString(h.Sum(nil)[:16]), nil
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)[:16]), nil
}
