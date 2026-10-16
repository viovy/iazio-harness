package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSandbox_CreateAndCleanup(t *testing.T) {
	srcDir, err := os.MkdirTemp("", "sandbox-src-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(srcDir)

	testFile := filepath.Join(srcDir, "hello.txt")
	if err := os.WriteFile(testFile, []byte("original content"), 0644); err != nil {
		t.Fatal(err)
	}

	sb, err := Create(srcDir, Options{})
	if err != nil {
		t.Fatalf("Create sandbox failed: %v", err)
	}
	defer sb.Cleanup()

	sandboxedFile := filepath.Join(sb.SandboxPath, "hello.txt")
	content, err := os.ReadFile(sandboxedFile)
	if err != nil {
		t.Fatalf("read sandboxed file: %v", err)
	}
	if string(content) != "original content" {
		t.Errorf("content mismatch: got %q, want %q", string(content), "original content")
	}

	// Mutate sandboxed file
	if err := os.WriteFile(sandboxedFile, []byte("modified content"), 0644); err != nil {
		t.Fatal(err)
	}

	// Original must stay untouched
	origContent, err := os.ReadFile(testFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(origContent) != "original content" {
		t.Errorf("original file was unexpectedly modified: %q", string(origContent))
	}

	// Test Cleanup
	if err := sb.Cleanup(); err != nil {
		t.Fatalf("cleanup failed: %v", err)
	}
	if _, err := os.Stat(sb.SandboxPath); !os.IsNotExist(err) {
		t.Errorf("sandbox path still exists after cleanup")
	}
}

func TestSandbox_Diff(t *testing.T) {
	srcDir, err := os.MkdirTemp("", "sandbox-diff-src-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(srcDir)

	testFile := filepath.Join(srcDir, "doc.txt")
	if err := os.WriteFile(testFile, []byte("initial line\n"), 0644); err != nil {
		t.Fatal(err)
	}

	sb, err := Create(srcDir, Options{})
	if err != nil {
		t.Fatalf("Create sandbox failed: %v", err)
	}
	defer sb.Cleanup()

	// 1. Initial diff must be empty
	initialDiff, err := sb.Diff()
	if err != nil {
		t.Fatalf("Diff failed before mutation: %v", err)
	}
	if initialDiff != "" {
		t.Errorf("expected empty diff initially, got: %q", initialDiff)
	}

	// 2. Modify a file in the sandbox
	sandboxedFile := filepath.Join(sb.SandboxPath, "doc.txt")
	if err := os.WriteFile(sandboxedFile, []byte("initial line\nadded line\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// 3. Diff must now reflect the change
	diff, err := sb.Diff()
	if err != nil {
		t.Fatalf("Diff failed after mutation: %v", err)
	}
	if diff == "" {
		t.Errorf("expected non-empty diff after mutating file")
	}
	if !strings.Contains(diff, "+added line") {
		t.Errorf("diff does not contain expected change: %s", diff)
	}
}
