package proof

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

var (
	ErrBypassOrNoOpDetected = errors.New("BYPASS_OR_NOOP_DETECTED: job claimed completion without verifiable execution proof")
)

// ExecutionProof encapsulates the complete cryptographic and operational proof
// that an autonomous story execution or agent prompt actually executed.
type ExecutionProof struct {
	PreGitHeadSHA    string `json:"pre_git_head_sha"`
	PostGitHeadSHA   string `json:"post_git_head_sha"`
	PreGitTreeSHA    string `json:"pre_git_tree_sha"`
	PostGitTreeSHA   string `json:"post_git_tree_sha"`
	AllocatedStoryID string `json:"allocated_story_id"`
	StdoutBytes      int    `json:"stdout_bytes"`
	StdoutLineCount  int    `json:"stdout_line_count"`
	RawTranscript    string `json:"raw_transcript"`
	AgentPID         int    `json:"agent_pid"`
	DurationMs       int64  `json:"duration_ms"`
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
		if proof.PreGitHeadSHA == "" || proof.PostGitHeadSHA == "" ||
			proof.PreGitTreeSHA == "" || proof.PostGitTreeSHA == "" {
			return fmt.Errorf("%w: missing pre/post Git SHA telemetry", ErrBypassOrNoOpDetected)
		}

		// If neither HEAD commit nor working tree SHA changed, zero work was done!
		if proof.PreGitHeadSHA == proof.PostGitHeadSHA && proof.PreGitTreeSHA == proof.PostGitTreeSHA {
			return fmt.Errorf("%w: zero repository mutation (HEAD=%s and Tree=%s unchanged)",
				ErrBypassOrNoOpDetected, proof.PostGitHeadSHA, proof.PostGitTreeSHA)
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
	headOut, err := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		return "", "", fmt.Errorf("git rev-parse HEAD failed: %w", err)
	}
	treeOut, err := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "HEAD^{tree}").Output()
	if err != nil {
		return "", "", fmt.Errorf("git rev-parse HEAD^{tree} failed: %w", err)
	}
	return strings.TrimSpace(string(headOut)), strings.TrimSpace(string(treeOut)), nil
}
