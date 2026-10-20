package autopilot

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDeriveSlug(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"Add Fleet Repository Discovery Tools", "add-fleet-repository-discovery-tools"},
		{"Fix bug: #123 (critical!)", "fix-bug-123-critical"},
		{"   leading and trailing spaces   ", "leading-and-trailing-spaces"},
		{"", "story"},
	}

	for _, c := range cases {
		got := DeriveSlug(c.in)
		if got != c.want {
			t.Errorf("DeriveSlug(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestEnrichStoryFile(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "test.story.md")

	initial := `---
id: STORY-001
---

## User Story

**As a** dev
**I want** stuff
**So that** things

## 1. Rationale

Initial rationale placeholder.

## 2. Requirements

1. initial req

## 3. Acceptance Criteria

- [ ] initial ac

## 4. Notes

Notes here.
`
	if err := os.WriteFile(filePath, []byte(initial), 0644); err != nil {
		t.Fatalf("failed to write initial file: %v", err)
	}

	client := NewCLIClient("echo")
	opts := CreateStoryOptions{
		Problem:            "Real problem statement here.",
		TargetFiles:        []string{"internal/test.go"},
		AcceptanceCriteria: []string{"Criteria 1 met", "Criteria 2 met"},
		DependsOn:          []string{"STORY-UPSTREAM"},
	}

	if err := client.enrichStoryFile(filePath, opts); err != nil {
		t.Fatalf("enrichStoryFile failed: %v", err)
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("read file failed: %v", err)
	}
	content := string(data)

	if !strings.Contains(content, "Real problem statement here.") {
		t.Error("expected problem in rationale")
	}
	if !strings.Contains(content, "internal/test.go") {
		t.Error("expected target files in requirements")
	}
	if !strings.Contains(content, "- [ ] Criteria 1 met") {
		t.Error("expected acceptance criteria")
	}
	if !strings.Contains(content, "## 5. Dependencies\n\n- STORY-UPSTREAM") {
		t.Error("expected dependencies")
	}
}

func TestCLIClient_CreateStory_MockProcess(t *testing.T) {
	tmpDir := t.TempDir()

	// Create fake autopilot binary script
	fakeAutopilot := filepath.Join(tmpDir, "fake-autopilot")
	storyPath := filepath.Join(tmpDir, "2026-10-19-story.md")
	// The fake script writes a valid base story file and outputs json
	script := fmt.Sprintf(`#!/bin/sh
cmd="$1"
sub="$2"
if [ "$cmd" = "story" ] && [ "$sub" = "create" ]; then
  cat <<'EOF' > "%s"
---
id: STORY-TEST-999
---
## 1. Rationale
Old
## 2. Requirements
Old
## 3. Acceptance Criteria
- [ ] Old
EOF
  echo '{"story_id":"STORY-TEST-999","path":"%s"}'
  exit 0
fi
if [ "$cmd" = "story" ] && [ "$sub" = "index-gate" ]; then
  exit 0
fi
exit 2
`, storyPath, storyPath)

	if err := os.WriteFile(fakeAutopilot, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write fake autopilot: %v", err)
	}

	client := NewCLIClient(fakeAutopilot)
	opts := CreateStoryOptions{
		Submodule:          "iazio-mcp",
		Title:              "Add New Feature",
		Problem:            "Problem statement",
		AcceptanceCriteria: []string{"Feature works"},
		TargetFiles:        []string{"main.go"},
		WorkingDir:         tmpDir,
	}

	res, err := client.CreateStory(context.Background(), opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if res.StoryID != "STORY-TEST-999" {
		t.Errorf("expected story ID STORY-TEST-999, got %s", res.StoryID)
	}
	if !res.IndexUpdated {
		t.Error("expected index_updated=true")
	}

	// Verify enriched file exists
	content, err := os.ReadFile(storyPath)
	if err != nil {
		t.Fatalf("failed to read story file: %v", err)
	}
	if !strings.Contains(string(content), "Problem statement") {
		t.Error("expected problem statement in enriched file")
	}
}

func TestCLIClient_CreateStory_RollbackOnValidationFailure(t *testing.T) {
	tmpDir := t.TempDir()

	fakeAutopilot := filepath.Join(tmpDir, "fake-autopilot-fail")
	storyPath := filepath.Join(tmpDir, "2026-10-19-fail-story.md")
	// The fake script creates the file, but index-gate fails
	script := fmt.Sprintf(`#!/bin/sh
cmd="$1"
sub="$2"
if [ "$cmd" = "story" ] && [ "$sub" = "create" ]; then
  echo "content" > "%s"
  echo '{"story_id":"STORY-FAIL-001","path":"%s"}'
  exit 0
fi
if [ "$cmd" = "story" ] && [ "$sub" = "index-gate" ]; then
  echo "index gate failed: missing entry" >&2
  exit 1
fi
exit 2
`, storyPath, storyPath)

	if err := os.WriteFile(fakeAutopilot, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write fake script: %v", err)
	}

	client := NewCLIClient(fakeAutopilot)
	opts := CreateStoryOptions{
		Submodule:          "iazio-mcp",
		Title:              "Failing Feature",
		Problem:            "Problem",
		AcceptanceCriteria: []string{"AC"},
		TargetFiles:        []string{"f.go"},
		WorkingDir:         tmpDir,
	}

	_, err := client.CreateStory(context.Background(), opts)
	if err == nil {
		t.Fatal("expected error from failed index-gate, got nil")
	}

	// Verify story file was cleaned up on failure
	if _, err := os.Stat(storyPath); !os.IsNotExist(err) {
		t.Errorf("expected story file %s to be deleted on failure, but it still exists", storyPath)
	}
}

func TestCLIClient_CreateStory_AtomicDelegation(t *testing.T) {
	tmpDir := t.TempDir()
	fakeAutopilot := filepath.Join(tmpDir, "fake-autopilot-atomic")

	script := `#!/bin/sh
cmd="$1"
sub="$2"
if [ "$cmd" = "story-create" ] && [ "$sub" = "--json" ]; then
  # Read stdin payload to verify it arrives
  payload=$(cat)
  if [ -z "$payload" ]; then
    echo '{"ok":false,"rule_id":"EMPTY_STDIN"}'
    exit 1
  fi
  echo '{"ok":true,"story_id":"STORY-TEST-0131","submodule":"iazio-mcp","story_path":"docs/stories/2026-10-19-test.story.md","index_updated":true,"validated":true}'
  exit 0
fi
exit 2
`
	if err := os.WriteFile(fakeAutopilot, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write fake script: %v", err)
	}

	client := NewCLIClient(fakeAutopilot)
	opts := CreateStoryOptions{
		Submodule:          "iazio-mcp",
		Title:              "Route Fleet Repository Discovery",
		Problem:            "Problem statement",
		AcceptanceCriteria: []string{"AC1"},
		TargetFiles:        []string{"internal/mcp/tool.go"},
		WorkingDir:         tmpDir,
	}

	res, err := client.CreateStory(context.Background(), opts)
	if err != nil {
		t.Fatalf("CreateStory failed: %v", err)
	}
	if res.StoryID != "STORY-TEST-0131" {
		t.Errorf("expected STORY-TEST-0131, got %s", res.StoryID)
	}
	if res.Submodule != "iazio-mcp" {
		t.Errorf("expected iazio-mcp, got %s", res.Submodule)
	}
	if res.StoryPath != "docs/stories/2026-10-19-test.story.md" {
		t.Errorf("unexpected story path: %s", res.StoryPath)
	}
	if !res.IndexUpdated {
		t.Errorf("expected IndexUpdated=true")
	}
}

func TestNewCLIClientDefaults(t *testing.T) {
	t.Setenv("AUTOPILOT_BIN", "/custom/autopilot")
	c1 := NewCLIClient("")
	if c1.AutopilotBinary != "/custom/autopilot" {
		t.Errorf("expected /custom/autopilot, got %s", c1.AutopilotBinary)
	}

	t.Setenv("AUTOPILOT_BIN", "")
	c2 := NewCLIClient("")
	if c2.AutopilotBinary != "autopilot" {
		t.Errorf("expected autopilot, got %s", c2.AutopilotBinary)
	}
}


