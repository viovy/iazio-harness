// Package autopilot provides integration with the autopilot CLI and story governance commands.
package autopilot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/viovy/iazio-harness/internal/procutil"
)

// CreateStoryOptions contains semantic fields required to scaffold a story via autopilot.
type CreateStoryOptions struct {
	Submodule            string   `json:"submodule"`
	Title                string   `json:"title"`
	Priority             string   `json:"priority,omitempty"`
	Problem              string   `json:"problem"`
	AcceptanceCriteria   []string `json:"acceptance_criteria"`
	TargetFiles          []string `json:"target_files"`
	DependsOn            []string `json:"depends_on,omitempty"`
	VerificationCommands []string `json:"verification_commands,omitempty"`
	Category             string   `json:"category,omitempty"`
	WorkingDir           string   `json:"working_dir,omitempty"`
	StoriesDir           string   `json:"stories_dir,omitempty"`
}

// StoryResult represents the completed story metadata emitted to stdout.
type StoryResult struct {
	StoryID      string `json:"story_id"`
	Submodule    string `json:"submodule"`
	StoryPath    string `json:"story_path"`
	IndexUpdated bool   `json:"index_updated"`
}

// Client defines the interface for interacting with autopilot governance CLI.
type Client interface {
	CreateStory(ctx context.Context, opts CreateStoryOptions) (*StoryResult, error)
	ValidateIndex(ctx context.Context, workingDir string) error
}

// CLIClient is the concrete implementation that executes autopilot.
type CLIClient struct {
	AutopilotBinary string
}

// NewCLIClient constructs an autopilot CLIClient.
func NewCLIClient(binaryPath string) *CLIClient {
	if binaryPath == "" {
		if env := os.Getenv("AUTOPILOT_BIN"); env != "" {
			binaryPath = env
		} else {
			binaryPath = "autopilot"
		}
	}
	return &CLIClient{AutopilotBinary: binaryPath}
}

// DeriveSlug converts a title into a clean kebab-case slug.
func DeriveSlug(title string) string {
	reg := regexp.MustCompile(`[^a-zA-Z0-9]+`)
	slug := reg.ReplaceAllString(strings.ToLower(strings.TrimSpace(title)), "-")
	slug = strings.Trim(slug, "-")
	if slug == "" {
		return "story"
	}
	if len(slug) > 60 {
		slug = slug[:60]
		slug = strings.TrimRight(slug, "-")
	}
	return slug
}

// CreateStory invokes `autopilot story create`, enriches the markdown content, and verifies index parity.
func (c *CLIClient) CreateStory(ctx context.Context, opts CreateStoryOptions) (*StoryResult, error) {
	if strings.TrimSpace(opts.Title) == "" {
		return nil, errors.New("title is required")
	}

	category := strings.TrimSpace(opts.Category)
	if category == "" {
		if strings.HasPrefix(strings.ToLower(opts.Submodule), "iazio") {
			category = "IAZIO"
		} else {
			category = "GOV"
		}
	}

	priority := strings.ToUpper(strings.TrimSpace(opts.Priority))
	if priority == "" {
		priority = "P2"
	}

	slug := DeriveSlug(opts.Title)
	workingDir := opts.WorkingDir
	if workingDir == "" {
		workingDir = "."
	}

	storiesDir := opts.StoriesDir
	if storiesDir == "" {
		storiesDir = filepath.Join(workingDir, "docs", "stories")
	}

	// 1. Try atomic autopilot story-create --json delegation via stdin (Story 11 contract)
	payloadBytes, mErr := json.Marshal(map[string]any{
		"submodule":             opts.Submodule,
		"title":                 opts.Title,
		"priority":              priority,
		"problem":               opts.Problem,
		"acceptance_criteria":   opts.AcceptanceCriteria,
		"target_files":          opts.TargetFiles,
		"depends_on":            opts.DependsOn,
		"verification_commands": opts.VerificationCommands,
	})
	if mErr == nil {
		cmdJSON := procutil.CommandContext(ctx, c.AutopilotBinary, "story-create", "--json")
		cmdJSON.Dir = workingDir
		cmdJSON.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "AUTOPILOT_NON_INTERACTIVE=1")
		cmdJSON.Stdin = bytes.NewReader(payloadBytes)
		var outBuf, errBuf bytes.Buffer
		cmdJSON.Stdout = &outBuf
		cmdJSON.Stderr = &errBuf
		if runErr := cmdJSON.Run(); runErr == nil {
			var res struct {
				OK           bool   `json:"ok"`
				StoryID      string `json:"story_id"`
				Submodule    string `json:"submodule"`
				StoryPath    string `json:"story_path"`
				IndexUpdated bool   `json:"index_updated"`
				Validated    bool   `json:"validated"`
			}
			if jErr := json.Unmarshal(outBuf.Bytes(), &res); jErr == nil && res.StoryID != "" {
				return &StoryResult{
					StoryID:      res.StoryID,
					Submodule:    res.Submodule,
					StoryPath:    res.StoryPath,
					IndexUpdated: res.IndexUpdated,
				}, nil
			}
		}
	}

	// 2. Fallback to legacy autopilot story create / scaffold
	args := []string{
		"story", "create",
		"-json",
		"-category", category,
		"-priority", priority,
		"-slug", slug,
		"-title", opts.Title,
		"-stories-dir", storiesDir,
	}

	cmd := procutil.CommandContext(ctx, c.AutopilotBinary, args...)
	cmd.Dir = workingDir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "AUTOPILOT_NON_INTERACTIVE=1")

	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("autopilot story create failed: %w (stderr: %s)", err, stderrBuf.String())
	}

	var scaffoldOut struct {
		StoryID string `json:"story_id"`
		Path    string `json:"path"`
	}
	if err := json.Unmarshal(stdoutBuf.Bytes(), &scaffoldOut); err != nil {
		return nil, fmt.Errorf("failed to parse autopilot stdout %q: %w", stdoutBuf.String(), err)
	}

	storyPath := scaffoldOut.Path
	if storyPath == "" {
		return nil, fmt.Errorf("autopilot story create returned empty path: %s", stdoutBuf.String())
	}

	// 2. Enrich the generated story markdown file
	if err := c.enrichStoryFile(storyPath, opts); err != nil {
		// Clean up partial story file on failure
		_ = os.Remove(storyPath)
		return nil, fmt.Errorf("failed to enrich story markdown: %w", err)
	}

	// 3. Run validation gate
	if err := c.ValidateIndex(ctx, workingDir); err != nil {
		_ = os.Remove(storyPath)
		return nil, fmt.Errorf("autopilot story index validation failed: %w", err)
	}

	// Calculate repo-relative path
	relPath := storyPath
	if rel, err := filepath.Rel(workingDir, storyPath); err == nil && !strings.HasPrefix(rel, "..") {
		relPath = filepath.ToSlash(rel)
	} else {
		relPath = filepath.ToSlash(relPath)
	}

	return &StoryResult{
		StoryID:      scaffoldOut.StoryID,
		Submodule:    opts.Submodule,
		StoryPath:    relPath,
		IndexUpdated: true,
	}, nil
}

// ValidateIndex runs `autopilot story index-gate` to guarantee index consistency.
func (c *CLIClient) ValidateIndex(ctx context.Context, workingDir string) error {
	cmd := procutil.CommandContext(ctx, c.AutopilotBinary, "story", "index-gate")
	if workingDir != "" {
		cmd.Dir = workingDir
	}
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "AUTOPILOT_NON_INTERACTIVE=1")

	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("index-gate validation error: %w (stderr: %s)", err, stderrBuf.String())
	}
	return nil
}

func (c *CLIClient) enrichStoryFile(filePath string, opts CreateStoryOptions) error {
	contentBytes, err := os.ReadFile(filePath)
	if err != nil {
		return err
	}
	content := string(contentBytes)

	// Replace Rationale / Problem
	if opts.Problem != "" {
		rationaleSection := fmt.Sprintf("## 1. Rationale\n\n%s\n", strings.TrimSpace(opts.Problem))
		content = replaceSection(content, "## 1. Rationale", rationaleSection)
	}

	// Replace or append Requirements with Target Files
	if len(opts.TargetFiles) > 0 || len(opts.VerificationCommands) > 0 {
		var reqs strings.Builder
		reqs.WriteString("## 2. Requirements\n\n")
		idx := 1
		if len(opts.TargetFiles) > 0 {
			reqs.WriteString(fmt.Sprintf("%d. Target files modified:\n", idx))
			for _, tf := range opts.TargetFiles {
				reqs.WriteString(fmt.Sprintf("   - `%s`\n", tf))
			}
			idx++
		}
		if len(opts.VerificationCommands) > 0 {
			reqs.WriteString(fmt.Sprintf("%d. Verification commands executed:\n", idx))
			for _, vc := range opts.VerificationCommands {
				reqs.WriteString(fmt.Sprintf("   - `%s`\n", vc))
			}
			idx++
		}
		content = replaceSection(content, "## 2. Requirements", reqs.String())
	}

	// Replace Acceptance Criteria
	if len(opts.AcceptanceCriteria) > 0 {
		var ac strings.Builder
		ac.WriteString("## 3. Acceptance Criteria\n\n")
		for _, item := range opts.AcceptanceCriteria {
			ac.WriteString(fmt.Sprintf("- [ ] %s\n", strings.TrimSpace(item)))
		}
		content = replaceSection(content, "## 3. Acceptance Criteria", ac.String())
	}

	// Update Dependencies if specified
	if len(opts.DependsOn) > 0 {
		var dep strings.Builder
		dep.WriteString("## 5. Dependencies\n\n")
		for _, d := range opts.DependsOn {
			dep.WriteString(fmt.Sprintf("- %s\n", strings.TrimSpace(d)))
		}
		content = replaceSection(content, "## 5. Dependencies", dep.String())
	}

	return os.WriteFile(filePath, []byte(content), 0644)
}

func replaceSection(content, header, replacement string) string {
	lines := strings.Split(content, "\n")
	startIdx := -1
	endIdx := -1

	for i, line := range lines {
		if strings.HasPrefix(line, header) {
			startIdx = i
			continue
		}
		if startIdx != -1 && strings.HasPrefix(line, "## ") {
			endIdx = i
			break
		}
	}

	if startIdx == -1 {
		return strings.TrimRight(content, "\n") + "\n\n" + strings.TrimRight(replacement, "\n") + "\n"
	}
	if endIdx == -1 {
		endIdx = len(lines)
	}

	newLines := make([]string, 0, len(lines))
	newLines = append(newLines, lines[:startIdx]...)
	newLines = append(newLines, strings.TrimRight(replacement, "\n"))
	newLines = append(newLines, lines[endIdx:]...)

	return strings.Join(newLines, "\n")
}
