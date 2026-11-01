package proof

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/viovy/iazio-harness/internal/procutil"
)

var (
	ErrBypassOrNoOpDetected = errors.New("BYPASS_OR_NOOP_DETECTED: job claimed completion without verifiable execution proof")
)

// WorktreeSnapshot captures a cryptographic fingerprint of the repository state
// before and after execution, explicitly partitioning story markdown specifications
// (docs/stories/*) from real code/infrastructure deliverables.
type WorktreeSnapshot struct {
	HeadSHA              string   `json:"head_sha"`
	NonSpecMutationHash  string   `json:"non_spec_mutation_hash"`
	NonSpecChangedFiles  []string `json:"non_spec_changed_files"`
	StorySpecContentHash string   `json:"story_spec_content_hash"`
	HasIndexLock         bool     `json:"has_index_lock"`
	DirtyFileCount       int      `json:"dirty_file_count"`
}

// CaptureWorktreeSnapshot computes an exact cryptographic snapshot of the repo
// BEFORE and AFTER execution, separating pre-scaffolded story markdown files
// (docs/stories/*) from actual code/repo deliverables.
func CaptureWorktreeSnapshot(repoDir string) (WorktreeSnapshot, error) {
	var snap WorktreeSnapshot

	if _, err := os.Stat(filepath.Join(repoDir, ".git", "index.lock")); err == nil {
		snap.HasIndexLock = true
	}

	headOut, err := procutil.Command("git", "-C", repoDir, "rev-parse", "HEAD").Output()
	if err != nil {
		return snap, fmt.Errorf("git rev-parse HEAD failed: %w", err)
	}
	snap.HeadSHA = strings.TrimSpace(string(headOut))

	statusOut, err := procutil.Command("git", "-C", repoDir, "status", "--porcelain", "-uall").Output()
	if err != nil {
		return snap, fmt.Errorf("git status failed: %w", err)
	}

	lines := strings.Split(strings.TrimSpace(string(statusOut)), "\n")
	nonSpecHasher := sha256.New()
	specHasher := sha256.New()

	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		snap.DirtyFileCount++
		relPath := strings.TrimSpace(line[2:])
		fullPath := filepath.Join(repoDir, relPath)
		content, _ := os.ReadFile(fullPath)

		if strings.HasPrefix(relPath, "docs/stories/") || relPath == "docs/stories" {
			specHasher.Write([]byte(relPath))
			specHasher.Write(content)
		} else {
			snap.NonSpecChangedFiles = append(snap.NonSpecChangedFiles, relPath)
			nonSpecHasher.Write([]byte(line))
			nonSpecHasher.Write(content)
		}
	}

	snap.NonSpecMutationHash = hex.EncodeToString(nonSpecHasher.Sum(nil))
	snap.StorySpecContentHash = hex.EncodeToString(specHasher.Sum(nil))
	return snap, nil
}

// VerifyStrictStoryExecution prevents bypasses such as job_0828bc2214e083b5:
// Even if StoryID was pre-allocated AND docs/stories/*.story.md was pre-scaffolded
// in the worktree before enqueue, the job FAILS unless actual non-spec repository
// mutations (commits or file changes) occurred during the execution window.
func VerifyStrictStoryExecution(action string, pre, post WorktreeSnapshot, stdout string) (string, error) {
	trimmed := strings.TrimSpace(stdout)
	if trimmed == "" || strings.HasPrefix(trimmed, "[+] prompt executed via") {
		return "SYNTHETIC_OR_EMPTY_STDOUT", fmt.Errorf("%w: synthetic or empty stdout (%q)", ErrBypassOrNoOpDetected, trimmed)
	}

	if action == "/autonomous-story-execution" || action == "ase" || action == "story" || strings.Contains(action, "story") {
		headCommitted := pre.HeadSHA != "" && post.HeadSHA != "" && pre.HeadSHA != post.HeadSHA
		nonSpecWorktreeMutated := pre.NonSpecMutationHash != post.NonSpecMutationHash

		if !headCommitted && !nonSpecWorktreeMutated {
			return "PRE_SCAFFOLD_NOOP_BYPASS", fmt.Errorf(
				"%w: zero code/repo deliverables mutated during execution (pre_head=%s post_head=%s, pre_non_spec_hash=%s post_non_spec_hash=%s). Pre-scaffolding docs/stories/*.md without executing the story is rejected",
				ErrBypassOrNoOpDetected, pre.HeadSHA, post.HeadSHA, pre.NonSpecMutationHash[:min(12, len(pre.NonSpecMutationHash))], post.NonSpecMutationHash[:min(12, len(post.NonSpecMutationHash))],
			)
		}
	}
	return "VERIFIED", nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ExecutionProof encapsulates the complete cryptographic and operational proof
// that an autonomous story execution or agent prompt actually executed.
type ExecutionProof struct {
	PreGitHeadSHA    string            `json:"pre_git_head_sha"`
	PostGitHeadSHA   string            `json:"post_git_head_sha"`
	PreGitTreeSHA    string            `json:"pre_git_tree_sha"`
	PostGitTreeSHA   string            `json:"post_git_tree_sha"`
	PreSnapshot      *WorktreeSnapshot `json:"pre_snapshot,omitempty"`
	PostSnapshot     *WorktreeSnapshot `json:"post_snapshot,omitempty"`
	AllocatedStoryID string            `json:"allocated_story_id"`
	StdoutBytes      int               `json:"stdout_bytes"`
	StdoutLineCount  int               `json:"stdout_line_count"`
	RawTranscript    string            `json:"raw_transcript"`
	AgentPID         int               `json:"agent_pid"`
	DurationMs       int64             `json:"duration_ms"`
}

// VerifyExecutionProof enforces hard anti-lying and anti-noop invariants before
// any job is allowed to transition to StatusCompleted.
func VerifyExecutionProof(action string, proof ExecutionProof) error {
	// 1. Reject canned/synthetic wrapper summaries masquerading as output
	trimmed := strings.TrimSpace(proof.RawTranscript)
	if trimmed == "" || strings.HasPrefix(trimmed, "[+] prompt executed via") {
		return fmt.Errorf("%w: synthetic or empty transcript detected (%q)", ErrBypassOrNoOpDetected, trimmed)
	}

	// 2. Require minimum real telemetry volume from the underlying agent CLI
	if proof.StdoutBytes < 256 || proof.StdoutLineCount < 5 {
		return fmt.Errorf("%w: insufficient agent stdout telemetry (bytes=%d, lines=%d)",
			ErrBypassOrNoOpDetected, proof.StdoutBytes, proof.StdoutLineCount)
	}

	// 3. For mutating workflows (/autonomous-story-execution, story, ase), enforce Git mutation & Story allocation
	isMutating := action == "/autonomous-story-execution" || action == "ase" || action == "story" || strings.Contains(action, "story")
	if isMutating {
		// If fine-grained WorktreeSnapshots are present, use strict non-spec delta verification
		if proof.PreSnapshot != nil && proof.PostSnapshot != nil {
			if _, err := VerifyStrictStoryExecution(action, *proof.PreSnapshot, *proof.PostSnapshot, proof.RawTranscript); err != nil {
				return err
			}
		} else {
			if proof.PreGitHeadSHA == "" || proof.PostGitHeadSHA == "" ||
				proof.PreGitTreeSHA == "" || proof.PostGitTreeSHA == "" {
				return fmt.Errorf("%w: missing pre/post Git SHA telemetry", ErrBypassOrNoOpDetected)
			}

			// If neither HEAD commit nor working tree SHA changed, zero work was done!
			if proof.PreGitHeadSHA == proof.PostGitHeadSHA && proof.PreGitTreeSHA == proof.PostGitTreeSHA {
				return fmt.Errorf("%w: zero repository mutation (HEAD=%s and Tree=%s unchanged)",
					ErrBypassOrNoOpDetected, proof.PostGitHeadSHA, proof.PostGitTreeSHA)
			}
		}

		// Must have allocated a real Story ID (cannot remain empty or 'pending-allocation')
		if proof.AllocatedStoryID == "" || proof.AllocatedStoryID == "pending-allocation" {
			return fmt.Errorf("%w: story_id was never allocated (got %q)",
				ErrBypassOrNoOpDetected, proof.AllocatedStoryID)
		}
	}

	return nil
}

// CaptureGitSHAs resolves the current Git HEAD commit SHA and working tree SHA.
func CaptureGitSHAs(ctx context.Context, dir string) (headSHA string, treeSHA string, err error) {
	if dir == "" {
		return "", "", fmt.Errorf("empty directory")
	}
	headOut, err := procutil.CommandContext(ctx, "git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		return "", "", fmt.Errorf("git rev-parse HEAD failed: %w", err)
	}
	treeOut, err := procutil.CommandContext(ctx, "git", "-C", dir, "rev-parse", "HEAD^{tree}").Output()
	if err != nil {
		return "", "", fmt.Errorf("git rev-parse HEAD^{tree} failed: %w", err)
	}
	return strings.TrimSpace(string(headOut)), strings.TrimSpace(string(treeOut)), nil
}
