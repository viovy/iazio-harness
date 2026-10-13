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

