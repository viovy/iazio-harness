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
