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
