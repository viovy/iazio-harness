// Package prompt fills stored prompt placeholders at spawn without rewriting the stored revision.
package prompt

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// KindStoryRefinement is the job kind that drafts from an idea transcript on the docs hub.
const KindStoryRefinement = "story_refinement"

// DirMode is the mode used when creating the spool directory.
const DirMode os.FileMode = 0o755

// Values are the only placeholders a stored prompt may reference.
type Values struct {
	StoryID              string
	StoryFilePath        string
	TargetLeafName       string
	WorktreePath         string
	DocsHubPath          string
	SourceTranscriptPath string
}

// Input is the stored prompt plus the spawn context needed to fill it.
type Input struct {
	Kind       string
	JobID      string
	Body       string
	Transcript string
	SpoolDir   string
	Values     Values
}

// Result is the prompt text sent to the IDE CLI.
// The stored revision is not modified.
type Result struct {
	Prompt               string
	SourceTranscriptPath string
}

var placeholderRE = regexp.MustCompile(`\{\{\.([A-Za-z0-9_]+)\}\}`)

// Fill replaces known placeholders. A placeholder with an empty value is an error.
// Fill does not write the result anywhere.
func Fill(body string, v Values) (string, error) {
	matches := placeholderRE.FindAllStringSubmatch(body, -1)
	repl := map[string]string{}
	for _, m := range matches {
		name := m[1]
		val, ok := v.lookup(name)
		if !ok || strings.TrimSpace(val) == "" {
			return "", fmt.Errorf("placeholder %s has no value", name)
		}
		repl[m[0]] = val
	}
	out := body
	for key, val := range repl {
		out = strings.ReplaceAll(out, key, val)
	}
	if placeholderRE.MatchString(out) {
		return "", errors.New("unresolved placeholder")
	}
	return out, nil
}

// Prepare fills the stored body and, for story_refinement, writes the idea transcript
// beside the spool. Transcript bytes are not copied into the prompt string.
// The marker line harness-job: <job_id> is prepended to the text sent to the CLI.
func Prepare(in Input) (Result, error) {
	if err := safeJobID(in.JobID); err != nil {
		return Result{}, err
	}
	values := in.Values
	var transcriptPath string
	if in.Kind == KindStoryRefinement {
		if strings.TrimSpace(in.SpoolDir) == "" {
			return Result{}, errors.New("spool dir is required")
		}
		if err := os.MkdirAll(in.SpoolDir, DirMode); err != nil {
			return Result{}, err
		}
		transcriptPath = filepath.Join(in.SpoolDir, in.JobID+".transcript.md")
		if err := os.WriteFile(transcriptPath, []byte(in.Transcript), 0o644); err != nil {
			return Result{}, err
		}
		values.SourceTranscriptPath = transcriptPath
	}
	filled, err := Fill(in.Body, values)
	if err != nil {
		return Result{}, err
	}
	if in.Kind == KindStoryRefinement && in.Transcript != "" && strings.Contains(filled, in.Transcript) {
		return Result{}, errors.New("transcript bytes must not be placed in the prompt")
	}
	prompt := "harness-job: " + in.JobID + "\n" + filled
	return Result{Prompt: prompt, SourceTranscriptPath: transcriptPath}, nil
}

func (v Values) lookup(name string) (string, bool) {
	switch name {
	case "StoryID":
		return v.StoryID, true
	case "StoryFilePath":
		return v.StoryFilePath, true
	case "TargetLeafName":
		return v.TargetLeafName, true
	case "WorktreePath":
		return v.WorktreePath, true
	case "DocsHubPath":
		return v.DocsHubPath, true
	case "SourceTranscriptPath":
		return v.SourceTranscriptPath, true
	default:
		return "", false
	}
}

func safeJobID(id string) error {
	if id == "" || id == "." || id == ".." || strings.Contains(id, "..") {
		return errors.New("invalid job id")
	}
	if strings.ContainsAny(id, `/\`) {
		return errors.New("invalid job id")
	}
	return nil
}
