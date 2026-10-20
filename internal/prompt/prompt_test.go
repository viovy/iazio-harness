package prompt

import (
	"strings"
	"testing"
)

func TestFillMissing(t *testing.T) {
	_, err := Fill("run {{.StoryID}}", Values{})
	if err == nil {
		t.Fatal("expected missing")
	}
	got, err := Fill("run {{.StoryID}}", Values{StoryID: "story-1"})
	if err != nil || got != "run story-1" {
		t.Fatal(got, err)
	}
}

func TestTranscriptNotInBody(t *testing.T) {
	dir := t.TempDir()
	res, err := Prepare(Input{
		JobID: "job-1", Kind: KindStoryRefinement, SpoolDir: dir,
		Body: "read {{.SourceTranscriptPath}}", Transcript: "secret transcript",
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Prompt, "secret transcript") {
		t.Fatal(res.Prompt)
	}
	if !strings.Contains(res.Prompt, "harness-job: job-1") {
		t.Fatal(res.Prompt)
	}
}

func TestPrepareResumeKind(t *testing.T) {
	res, err := Prepare(Input{
		JobID: "job-99", Kind: "resume",
		Body: "implement story", Values: Values{WorktreePath: "/repos/sample"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Prompt, "harness-job: job-99 [RESUME]") {
		t.Fatalf("expected resume header, got %q", res.Prompt)
	}
	if !strings.Contains(res.Prompt, "[RESUMPTION NOTICE]") {
		t.Fatalf("expected resumption notice, got %q", res.Prompt)
	}
	if !strings.Contains(res.Prompt, "implement story") {
		t.Fatalf("expected original prompt body, got %q", res.Prompt)
	}
}

func TestPrepareResumeEmptyStoryID(t *testing.T) {
	res, err := Prepare(Input{
		JobID: "job-100", Kind: "resume",
		Body: "finish story {{.StoryID}} in {{.WorktreePath}}", Values: Values{WorktreePath: "/repos/app"},
	})
	if err != nil {
		t.Fatalf("expected Prepare to succeed with fallback StoryID, got error: %v", err)
	}
	if !strings.Contains(res.Prompt, "finish story STORY-RESUME-INPLACE in /repos/app") {
		t.Fatalf("expected fallback StoryID in prompt, got %q", res.Prompt)
	}
}

func TestPromptValuesLookupAndSafeJobID(t *testing.T) {
	v := Values{
		StoryID:              "S-1",
		StoryFilePath:        "docs/stories/1.story.md",
		TargetLeafName:       "my-leaf",
		WorktreePath:         "/work/tree",
		DocsHubPath:          "/docs/hub",
		SourceTranscriptPath: "/tmp/trans.txt",
	}

	keys := []string{"StoryID", "StoryFilePath", "TargetLeafName", "WorktreePath", "DocsHubPath", "SourceTranscriptPath"}
	for _, k := range keys {
		val, ok := v.lookup(k)
		if !ok || val == "" {
			t.Errorf("expected lookup(%q) to succeed, got %q, %v", k, val, ok)
		}
	}
	_, ok := v.lookup("UnknownKey")
	if ok {
		t.Errorf("expected false for unknown key")
	}

	// safeJobID
	badIDs := []string{"", ".", "..", "a/b", "a\\b", "foo..bar"}
	for _, id := range badIDs {
		if err := safeJobID(id); err == nil {
			t.Errorf("expected safeJobID(%q) to fail", id)
		}
	}
	if err := safeJobID("valid-job-99_abc"); err != nil {
		t.Errorf("expected safeJobID to succeed: %v", err)
	}

	// Prepare edge cases
	_, err := Prepare(Input{
		JobID: "job-refine-err",
		Kind:  KindStoryRefinement,
		SpoolDir: "", // empty
	})
	if err == nil {
		t.Errorf("expected error for empty SpoolDir in KindStoryRefinement")
	}

	tmpDir := t.TempDir()
	_, err = Prepare(Input{
		JobID:      "job-leak",
		Kind:       KindStoryRefinement,
		SpoolDir:   tmpDir,
		Body:       "hello secret-transcript-leak",
		Transcript: "secret-transcript-leak",
	})
	if err == nil || !strings.Contains(err.Error(), "transcript bytes must not be placed") {
		t.Errorf("expected transcript leak error, got %v", err)
	}

	// Default fallback StoryID for non-resume
	nonResumeRes, err := Prepare(Input{
		JobID: "job-fallback-non-resume",
		Body:  "implement {{.StoryID}}",
	})
	if err != nil || !strings.Contains(nonResumeRes.Prompt, "resume complete current ase flow") {
		t.Errorf("expected default fallback story ID, got %v, res=%+v", err, nonResumeRes)
	}
}


