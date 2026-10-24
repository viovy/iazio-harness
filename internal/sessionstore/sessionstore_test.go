package sessionstore

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestExtractStoryID(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"2026-10-09-review-v0-44c767-STORY-APP-0098.md", "STORY-APP-0098"},
		{"docs/stories/2026-10-09-app-0098-feature.story.md", "STORY-APP-0098"},
		{"feat/app-0100-dirty-resumption", "STORY-APP-0100"},
		{"STORY-ENG-3369", "STORY-ENG-3369"},
		{"no-story-here.txt", ""},
	}
	for _, tc := range cases {
		got := ExtractStoryID(tc.input)
		if got != tc.want {
			t.Errorf("ExtractStoryID(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestSessionStoreResolver(t *testing.T) {
	tmpDir := t.TempDir()
	worktree := filepath.Join(tmpDir, "my-repo")
	_ = os.MkdirAll(worktree, 0o755)

	spoolDir := filepath.Join(tmpDir, "spool")
	_ = os.MkdirAll(spoolDir, 0o755)

	brainDir := filepath.Join(tmpDir, "brain")
	_ = os.MkdirAll(brainDir, 0o755)

	resolver := &Resolver{
		SpoolDir: spoolDir,
		BrainDir: brainDir,
	}

	// 1. Initially no sessions
	sess, err := resolver.ResolveLatest(worktree)
	if err != ErrNoSessionFound || sess != nil {
		t.Fatalf("expected ErrNoSessionFound, got %v, %v", sess, err)
	}

	// 2. Add a brain session
	conv1 := "11111111-2222-3333-4444-555555555555"
	conv1Dir := filepath.Join(brainDir, conv1, ".system_generated", "logs")
	_ = os.MkdirAll(conv1Dir, 0o755)
	transcriptContent := `{"step_index":0,"content":"<USER_REQUEST>Fix something for STORY-APP-0098</USER_REQUEST>"}
{"step_index":1,"tool_calls":[{"name":"run_command","args":{"Cwd":"` + worktree + `"}}]}
`
	_ = os.WriteFile(filepath.Join(conv1Dir, "transcript.jsonl"), []byte(transcriptContent), 0o644)

	// Set mtime to 1 hour ago
	_ = os.Chtimes(filepath.Join(conv1Dir, "transcript.jsonl"), time.Now().Add(-1*time.Hour), time.Now().Add(-1*time.Hour))

	sess, err = resolver.ResolveLatest(worktree)
	if err != nil || sess == nil {
		t.Fatalf("expected to resolve conv1 from brain, got %v, err=%v", sess, err)
	}
	if sess.ConversationID != conv1 {
		t.Fatalf("expected conv1 %s, got %s", conv1, sess.ConversationID)
	}
	if sess.Source != "brain" {
		t.Fatalf("expected source 'brain', got %s", sess.Source)
	}
	if sess.StoryID != "STORY-APP-0098" {
		t.Fatalf("expected story ID 'STORY-APP-0098', got %q", sess.StoryID)
	}

	// 3. Add a newer spool engine log
	conv2 := "66666666-7777-8888-9999-000000000000"
	engineLog := "I1009 05:11:44.883963 1 server.go:1274] Created conversation " + conv2 + "\n" +
		"working in " + worktree + "\n"
	_ = os.WriteFile(filepath.Join(spoolDir, "job-42.engine.log"), []byte(engineLog), 0o644)

	sess, err = resolver.ResolveLatest(worktree)
	if err != nil || sess == nil {
		t.Fatalf("expected to resolve conv2 from spool, got %v, err=%v", sess, err)
	}
	if sess.ConversationID != conv2 {
		t.Fatalf("expected conv2 %s, got %s", conv2, sess.ConversationID)
	}
	if sess.Source != "spool" {
		t.Fatalf("expected source 'spool', got %s", sess.Source)
	}

	// 4. Add a review markdown file with story in filename
	revDir := filepath.Join(worktree, "docs", "reviews")
	_ = os.MkdirAll(revDir, 0o755)
	convRev := "4fc5bf81-6e70-4800-af36-8da466524b99"
	revContent := `---
date: 2026-10-09
tool: agy
story_id: STORY-APP-0098
scheme_iteration: 0
verdict: CLEAN
conversation_id: "` + convRev + `"
---
# Review
`
	revFile := filepath.Join(revDir, "2026-10-09-review-v0-44c767-STORY-APP-0098.md")
	_ = os.WriteFile(revFile, []byte(revContent), 0o644)
	_ = os.Chtimes(revFile, time.Now().Add(5*time.Minute), time.Now().Add(5*time.Minute))

	sess, err = resolver.ResolveForStory(worktree, "STORY-APP-0098")
	if err != nil || sess == nil {
		t.Fatalf("expected to resolve convRev for STORY-APP-0098, got %v, err=%v", sess, err)
	}
	if sess.ConversationID != convRev {
		t.Fatalf("expected convRev %s, got %s", convRev, sess.ConversationID)
	}
	if sess.StoryID != "STORY-APP-0098" {
		t.Fatalf("expected story ID 'STORY-APP-0098', got %q", sess.StoryID)
	}
	if sess.Verdict != "CLEAN" {
		t.Fatalf("expected verdict 'CLEAN', got %q", sess.Verdict)
	}

	// 5. Add a worktree checkpoint with an even newer timestamp
	conv3 := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	iazioDir := filepath.Join(worktree, ".iazio")
	_ = os.MkdirAll(iazioDir, 0o755)
	cpData, err := json.Marshal(map[string]any{
		"job_id":          "job-99",
		"engine":          "agy",
		"worktree_path":   worktree,
		"conversation_id": conv3,
		"started_at":      time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339),
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(iazioDir, "execution-checkpoint.json"), cpData, 0o644)

	sess, err = resolver.ResolveLatest(worktree)
	if err != nil || sess == nil {
		t.Fatalf("expected to resolve conv3 from checkpoint, got %v, err=%v", sess, err)
	}
	if sess.ConversationID != conv3 {
		t.Fatalf("expected conv3 %s, got %s", conv3, sess.ConversationID)
	}
	if sess.Source != "checkpoint" {
		t.Fatalf("expected source 'checkpoint', got %s", sess.Source)
	}
}

func TestDefaultResolver(t *testing.T) {
	r := DefaultResolver()
	if r == nil {
		t.Fatal("expected non-nil default resolver")
	}
	if r.SpoolDir == "" {
		t.Error("expected non-empty spool dir")
	}
}

func TestDetectDirtyWorktreeContext(t *testing.T) {
	resolver := DefaultResolver()

	// 1. Empty worktree path
	ctx := resolver.DetectDirtyWorktreeContext("")
	if ctx.IsDirty || ctx.StoryID != "" {
		t.Errorf("expected empty ctx for blank worktree")
	}

	// 2. Non-git directory
	emptyDir := t.TempDir()
	ctx = resolver.DetectDirtyWorktreeContext(emptyDir)
	if ctx.IsDirty {
		t.Errorf("expected non-dirty for empty non-git dir")
	}

	// 3. Git repo with clean status
	gitDir := t.TempDir()
	cmdInit := exec.Command("git", "init", "-b", "feat/app-0099-test", gitDir)
	_ = cmdInit.Run()
	cmdConfig1 := exec.Command("git", "-C", gitDir, "config", "user.email", "test@example.com")
	_ = cmdConfig1.Run()
	cmdConfig2 := exec.Command("git", "-C", gitDir, "config", "user.name", "Test Runner")
	_ = cmdConfig2.Run()

	_ = os.WriteFile(filepath.Join(gitDir, "README.md"), []byte("# Hello"), 0o644)
	_ = exec.Command("git", "-C", gitDir, "add", "README.md").Run()
	_ = exec.Command("git", "-C", gitDir, "commit", "-m", "initial").Run()

	ctx = resolver.DetectDirtyWorktreeContext(gitDir)
	if ctx.IsDirty {
		t.Errorf("expected clean git repo to not be dirty")
	}

	// 4. Dirty review file
	revDir := filepath.Join(gitDir, "docs", "reviews")
	_ = os.MkdirAll(revDir, 0o755)
	revContent := `---
date: 2026-10-20
tool: agy
story_id: STORY-APP-0099
verdict: CLEAN
iteration: 2
conversation_id: "conv-dirty-1234"
---
`
	_ = os.WriteFile(filepath.Join(revDir, "2026-10-20-review.md"), []byte(revContent), 0o644)
	ctx = resolver.DetectDirtyWorktreeContext(gitDir)
	if !ctx.IsDirty {
		t.Errorf("expected dirty status with untracked review file")
	}
	if ctx.StoryID != "STORY-APP-0099" {
		t.Errorf("expected STORY-APP-0099, got %s", ctx.StoryID)
	}
	if ctx.ConversationID != "conv-dirty-1234" {
		t.Errorf("expected conv-dirty-1234, got %s", ctx.ConversationID)
	}
	if ctx.Verdict != "CLEAN" {
		t.Errorf("expected CLEAN verdict, got %s", ctx.Verdict)
	}

	// 5. Dirty story file without review file
	_ = os.Remove(filepath.Join(revDir, "2026-10-20-review.md"))
	storyDir := filepath.Join(gitDir, "docs", "stories")
	_ = os.MkdirAll(storyDir, 0o755)
	_ = os.WriteFile(filepath.Join(storyDir, "2026-10-20-app-0099-feature.story.md"), []byte("# Story"), 0o644)
	ctx = resolver.DetectDirtyWorktreeContext(gitDir)
	if !ctx.IsDirty || ctx.StoryID != "STORY-APP-0099" {
		t.Errorf("expected STORY-APP-0099 from dirty story file, got %+v", ctx)
	}

	// 6. Dirty story file where filename has no story ID, but frontmatter does
	_ = os.Remove(filepath.Join(storyDir, "2026-10-20-app-0099-feature.story.md"))
	timestampStory := "---\nid: STORY-GOV-POINTERS-20261021-134604\ntitle: Governance Pointers\n---\n# Story\n"
	_ = os.WriteFile(filepath.Join(storyDir, "2026-10-21-governance-pointers-20261021-134604.story.md"), []byte(timestampStory), 0o644)
	ctx = resolver.DetectDirtyWorktreeContext(gitDir)
	if !ctx.IsDirty || ctx.StoryID != "STORY-GOV-POINTERS-20261021-134604" {
		t.Errorf("expected STORY-GOV-POINTERS-20261021-134604 from frontmatter, got %+v", ctx)
	}
}

func TestScanBrainSessionsByStory(t *testing.T) {
	brainDir := t.TempDir()
	convID := "22222222-3333-4444-5555-666666666666"
	logDir := filepath.Join(brainDir, convID, ".system_generated", "logs")
	_ = os.MkdirAll(logDir, 0o755)
	transcript := `{"step_index":0,"content":"Working on STORY-TEST-7777"}
{"step_index":1,"content":"Executing tests"}
`
	_ = os.WriteFile(filepath.Join(logDir, "transcript.jsonl"), []byte(transcript), 0o644)

	resolver := &Resolver{
		BrainDir: brainDir,
	}

	sessions := resolver.scanBrainSessionsByStory("/work/repo", "STORY-TEST-7777")
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session matching STORY-TEST-7777, got %d", len(sessions))
	}
	if sessions[0].ConversationID != convID {
		t.Errorf("expected %s, got %s", convID, sessions[0].ConversationID)
	}

	// Non-matching story returns 0
	none := resolver.scanBrainSessionsByStory("/work/repo", "STORY-UNKNOWN")
	if len(none) != 0 {
		t.Errorf("expected 0 sessions, got %d", len(none))
	}
}

func TestIsValidConversationID(t *testing.T) {
	valid := []string{
		"aad902c8-bda6-44c7-8a68-c468a1562399",
		"conv-1234",
		"session_abc-xyz_123",
	}
	for _, id := range valid {
		if !IsValidConversationID(id) {
			t.Errorf("expected %q to be valid", id)
		}
	}

	invalid := []string{
		"",
		"../traversal",
		"foo/bar",
		"foo\\bar",
		"id with spaces",
		"id;rm -rf /",
		"id$var",
	}
	for _, id := range invalid {
		if IsValidConversationID(id) {
			t.Errorf("expected %q to be invalid", id)
		}
	}
}


