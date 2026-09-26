package prompt

import "testing"

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
	path, err := WriteTranscript(dir, "job-1", "secret transcript")
	if err != nil {
		t.Fatal(err)
	}
	body, err := Fill("read {{.SourceTranscriptPath}}", Values{SourceTranscriptPath: path})
	if err != nil {
		t.Fatal(err)
	}
	if body == "secret transcript" || len(body) == 0 {
		t.Fatal(body)
	}
	if Marker("job-1") != "harness-job: job-1" {
		t.Fatal("marker")
	}
}
