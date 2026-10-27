package proof

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestVerifyExecutionProof(t *testing.T) {
	tests := []struct {
		name    string
		action  string
		proof   ExecutionProof
		wantErr error
	}{
		{
			name:   "reject synthetic fallback transcript",
			action: "/autonomous-story-execution",
			proof: ExecutionProof{
				RawTranscript: "[+] prompt executed via agy (exit 0)",
			},
			wantErr: ErrBypassOrNoOpDetected,
		},
		{
			name:   "reject empty transcript",
			action: "/autonomous-story-execution",
			proof: ExecutionProof{
				RawTranscript: "   ",
			},
			wantErr: ErrBypassOrNoOpDetected,
		},
		{
			name:   "reject insufficient stdout volume",
			action: "/autonomous-story-execution",
			proof: ExecutionProof{
				RawTranscript:   "short output\nline 2",
				StdoutBytes:     20,
				StdoutLineCount: 2,
			},
			wantErr: ErrBypassOrNoOpDetected,
		},
		{
			name:   "reject zero repo mutation when SHAs identical",
			action: "/autonomous-story-execution",
			proof: ExecutionProof{
				RawTranscript:    "line 1\nline 2\nline 3\nline 4\nline 5\n" + string(make([]byte, 300)),
				StdoutBytes:      350,
				StdoutLineCount:  6,
				PreGitHeadSHA:    "commit-sha-1",
				PostGitHeadSHA:   "commit-sha-1",
				PreGitTreeSHA:    "tree-sha-1",
				PostGitTreeSHA:   "tree-sha-1",
				AllocatedStoryID: "STORY-GOV-3715",
			},
			wantErr: ErrBypassOrNoOpDetected,
		},
		{
			name:   "reject unallocated story id",
			action: "/autonomous-story-execution",
			proof: ExecutionProof{
				RawTranscript:    "line 1\nline 2\nline 3\nline 4\nline 5\n" + string(make([]byte, 300)),
				StdoutBytes:      350,
				StdoutLineCount:  6,
				PreGitHeadSHA:    "commit-sha-1",
				PostGitHeadSHA:   "commit-sha-2",
				PreGitTreeSHA:    "tree-sha-1",
				PostGitTreeSHA:   "tree-sha-2",
				AllocatedStoryID: "pending-allocation",
			},
			wantErr: ErrBypassOrNoOpDetected,
		},
		{
			name:   "accept valid mutating execution proof",
			action: "/autonomous-story-execution",
			proof: ExecutionProof{
				RawTranscript:    "valid agent execution\nline 2\nline 3\nline 4\nline 5\n" + string(make([]byte, 300)),
				StdoutBytes:      350,
				StdoutLineCount:  6,
				PreGitHeadSHA:    "commit-sha-1",
				PostGitHeadSHA:   "commit-sha-2",
				PreGitTreeSHA:    "tree-sha-1",
				PostGitTreeSHA:   "tree-sha-2",
				AllocatedStoryID: "STORY-GOV-3715",
			},
			wantErr: nil,
		},
		{
			name:   "accept non-mutating query action with sufficient telemetry",
			action: "query",
			proof: ExecutionProof{
				RawTranscript:   "query results line 1\nline 2\nline 3\nline 4\nline 5\n" + string(make([]byte, 300)),
				StdoutBytes:     350,
				StdoutLineCount: 6,
			},
			wantErr: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := VerifyExecutionProof(tc.action, tc.proof)
			if tc.wantErr == nil && err != nil {
				t.Fatalf("expected nil error, got: %v", err)
			}
			if tc.wantErr != nil {
				if err == nil {
					t.Fatalf("expected error containing %v, got nil", tc.wantErr)
				}
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("expected error wrapping %v, got: %v", tc.wantErr, err)
				}
			}
		})
	}
}

func TestCaptureGitSHAs(t *testing.T) {
	tempDir := t.TempDir()
	ctx := context.Background()

	// Empty git repo
	cmd := exec.Command("git", "init", tempDir)
	if err := cmd.Run(); err != nil {
		t.Fatalf("git init failed: %v", err)
	}
	_ = exec.Command("git", "-C", tempDir, "config", "user.name", "Test").Run()
	_ = exec.Command("git", "-C", tempDir, "config", "user.email", "test@example.com").Run()

	// Initial commit
	testFile := filepath.Join(tempDir, "file.txt")
	_ = os.WriteFile(testFile, []byte("content 1"), 0644)
	_ = exec.Command("git", "-C", tempDir, "add", ".").Run()
	_ = exec.Command("git", "-C", tempDir, "commit", "-m", "init").Run()

	head1, tree1, err := CaptureGitSHAs(ctx, tempDir)
	if err != nil {
		t.Fatalf("CaptureGitSHAs failed: %v", err)
	}
	if head1 == "" || tree1 == "" {
		t.Fatalf("expected non-empty head and tree SHAs, got head=%q, tree=%q", head1, tree1)
	}

	// Make another commit
	_ = os.WriteFile(testFile, []byte("content 2"), 0644)
	_ = exec.Command("git", "-C", tempDir, "add", ".").Run()
	_ = exec.Command("git", "-C", tempDir, "commit", "-m", "update").Run()

	head2, tree2, err := CaptureGitSHAs(ctx, tempDir)
	if err != nil {
		t.Fatalf("CaptureGitSHAs 2 failed: %v", err)
	}
	if head2 == head1 || tree2 == tree1 {
		t.Fatalf("expected SHAs to diverge, got head1=%s head2=%s tree1=%s tree2=%s", head1, head2, tree1, tree2)
	}
}

func TestCaptureWorktreeSnapshot_And_VerifyStrictStoryExecution(t *testing.T) {
	tempDir := t.TempDir()

	runGit := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", tempDir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, string(out))
		}
	}

	runGit("init")
	runGit("config", "user.name", "Test")
	runGit("config", "user.email", "test@example.com")

	// Base file
	_ = os.WriteFile(filepath.Join(tempDir, "main.go"), []byte("package main\n"), 0644)
	runGit("add", ".")
	runGit("commit", "-m", "init")

	// 1. Capture initial snapshot
	snap1, err := CaptureWorktreeSnapshot(tempDir)
	if err != nil {
		t.Fatalf("CaptureWorktreeSnapshot failed: %v", err)
	}
	if snap1.DirtyFileCount != 0 || snap1.HasIndexLock {
		t.Fatalf("expected clean worktree, got %+v", snap1)
	}

	// 2. Pre-scaffold a story file in docs/stories/
	storiesDir := filepath.Join(tempDir, "docs", "stories")
	_ = os.MkdirAll(storiesDir, 0755)
	_ = os.WriteFile(filepath.Join(storiesDir, "story-123.story.md"), []byte("# Story\n"), 0644)

	snap2, err := CaptureWorktreeSnapshot(tempDir)
	if err != nil {
		t.Fatalf("CaptureWorktreeSnapshot 2 failed: %v", err)
	}

	// Non-spec mutation hash MUST NOT change when only docs/stories/* changes!
	if snap1.NonSpecMutationHash != snap2.NonSpecMutationHash {
		t.Fatalf("expected NonSpecMutationHash to be identical when only story spec changed, got snap1=%s snap2=%s",
			snap1.NonSpecMutationHash, snap2.NonSpecMutationHash)
	}

	// Strict PoE must catch this no-op bypass
	stdout := "valid stdout output\nline 2\nline 3\nline 4\nline 5\n" + string(make([]byte, 300))
	code, err := VerifyStrictStoryExecution("/autonomous-story-execution", snap1, snap2, stdout)
	if !errors.Is(err, ErrBypassOrNoOpDetected) || code != "PRE_SCAFFOLD_NOOP_BYPASS" {
		t.Fatalf("expected PRE_SCAFFOLD_NOOP_BYPASS, got code=%s err=%v", code, err)
	}

	// 3. Mutate non-spec file
	_ = os.WriteFile(filepath.Join(tempDir, "feature.go"), []byte("package main\n// new code\n"), 0644)
	snap3, err := CaptureWorktreeSnapshot(tempDir)
	if err != nil {
		t.Fatalf("CaptureWorktreeSnapshot 3 failed: %v", err)
	}

	if snap1.NonSpecMutationHash == snap3.NonSpecMutationHash {
		t.Fatalf("expected NonSpecMutationHash to diverge when non-spec file created")
	}

	code, err = VerifyStrictStoryExecution("/autonomous-story-execution", snap1, snap3, stdout)
	if err != nil || code != "VERIFIED" {
		t.Fatalf("expected VERIFIED, got code=%s err=%v", code, err)
	}

	// 4. Verify index lock detection
	_ = os.WriteFile(filepath.Join(tempDir, ".git", "index.lock"), []byte("lock"), 0644)
	snapLock, _ := CaptureWorktreeSnapshot(tempDir)
	if !snapLock.HasIndexLock {
		t.Fatalf("expected HasIndexLock to be true when .git/index.lock exists")
	}
}
