package gate

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/viovy/iazio-core/pkg/procutil"
)

var reviewFilePathRegex = regexp.MustCompile(`(?i)(?:docs/)?reviews/[^\s|\x60"'\)\]]+?\.md`)

// StoryMarkdownMetadata captures parsed frontmatter and DoD/Reviews honesty signals from a .story.md file.
type StoryMarkdownMetadata struct {
	FilePath            string
	ID                  string
	Status              string
	ReviewSchemeScope   string
	Submodules          []string
	UncheckedTasks      []string
	UncheckedDoD        []string
	HasPendingReviews   bool
	DistinctReviewFiles []string
	TotalReviewFileRows int
	DishonestReasons    []string
	HonestDone          bool
}

// StoryHonestyReport summarizes aggregated story status and DoD/review honesty across candidate repos/hubs.
type StoryHonestyReport struct {
	Found         bool
	AllHonestDone bool
	Incomplete    bool
	Submodules    []string
	Reasons       []string
	Files         []StoryMarkdownMetadata
}

// ResolveSuperprojectRoot returns the git superproject working tree root when dir is a submodule,
// or empty string if dir is standalone or not inside a superproject.
func ResolveSuperprojectRoot(dir, gitBin string) string {
	if strings.TrimSpace(dir) == "" {
		return ""
	}
	if strings.TrimSpace(gitBin) == "" {
		gitBin = "git"
	}
	cmd := procutil.Command(gitBin, "rev-parse", "--show-superproject-working-tree")
	cmd.Dir = dir
	if out, err := cmd.Output(); err == nil {
		sp := strings.TrimSpace(string(out))
		if sp != "" {
			return filepath.Clean(sp)
		}
	}
	// Fallback only when dir/.git is a gitlink file (submodule), avoiding false matches for nested standalone repos.
	gitPath := filepath.Join(dir, ".git")
	if info, err := os.Stat(gitPath); err == nil && !info.IsDir() {
		parent := filepath.Dir(filepath.Clean(dir))
		for parent != "" && parent != "/" && parent != "." {
			if _, err := os.Stat(filepath.Join(parent, ".gitmodules")); err == nil {
				return filepath.Clean(parent)
			}
			next := filepath.Dir(parent)
			if next == parent {
				break
			}
			parent = next
		}
	}
	return ""
}

// MatchesStoryIDBoundary verifies that lowerStoryID / cleanID appears on non-alphanumeric boundaries in filename.
func MatchesStoryIDBoundary(filename, storyID string) bool {
	lowerFile := strings.ToLower(strings.TrimSuffix(filepath.Base(filename), ".story.md"))
	lowerFile = strings.TrimSuffix(lowerFile, ".md")
	lowerID := strings.ToLower(strings.TrimSpace(storyID))
	cleanID := strings.TrimPrefix(lowerID, "story-")
	for _, token := range []string{lowerID, cleanID} {
		if token == "" {
			continue
		}
		idx := strings.Index(lowerFile, token)
		for idx >= 0 {
			beforeOK := idx == 0 || !isStoryAlphaNum(lowerFile[idx-1])
			afterIdx := idx + len(token)
			afterOK := afterIdx == len(lowerFile) || !isStoryAlphaNum(lowerFile[afterIdx])
			if beforeOK && afterOK {
				return true
			}
			next := strings.Index(lowerFile[idx+1:], token)
			if next < 0 {
				break
			}
			idx += 1 + next
		}
	}
	return false
}

func isStoryAlphaNum(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// ParseStoryMarkdown extracts strict frontmatter and verifies Tasks, Definition of Done, and Reviews honesty.
func ParseStoryMarkdown(content, expectedStoryID, filename string) (bool, StoryMarkdownMetadata) {
	meta := StoryMarkdownMetadata{FilePath: filename}
	trimmedDoc := strings.TrimLeft(content, "\ufeff \t\r\n")
	lines := strings.Split(trimmedDoc, "\n")
	if len(lines) < 3 || strings.TrimSpace(lines[0]) != "---" {
		return false, meta
	}
	closingIdx := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			closingIdx = i
			break
		}
	}
	if closingIdx < 1 {
		return false, meta
	}

	inSubmodulesList := false
	seenSub := make(map[string]bool)
	for i := 1; i < closingIdx; i++ {
		rawLine := strings.TrimRight(lines[i], "\r")
		trimmed := strings.TrimSpace(rawLine)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if inSubmodulesList {
			if strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "* ") {
				sub := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(trimmed, "- "), "* "))
				sub = strings.Trim(sub, `"'`)
				if sub != "" && !seenSub[sub] {
					seenSub[sub] = true
					meta.Submodules = append(meta.Submodules, sub)
				}
				continue
			}
			if !strings.HasPrefix(rawLine, " ") && !strings.HasPrefix(rawLine, "\t") {
				inSubmodulesList = false
			}
		}
		key, val, ok := strings.Cut(trimmed, ":")
		if !ok {
			continue
		}
		k := strings.TrimSpace(key)
		if idx := strings.Index(val, " #"); idx >= 0 {
			val = val[:idx]
		}
		val = strings.Trim(strings.TrimSpace(val), `"'`)
		switch k {
		case "id", "story_id":
			if meta.ID == "" {
				meta.ID = val
			}
		case "status":
			if meta.Status == "" {
				meta.Status = strings.ToUpper(val)
			}
		case "review_scheme_scope":
			meta.ReviewSchemeScope = strings.ToLower(val)
		case "submodules":
			inSubmodulesList = true
			if strings.HasPrefix(val, "[") && strings.HasSuffix(val, "]") {
				inner := strings.TrimSuffix(strings.TrimPrefix(val, "["), "]")
				for _, part := range strings.Split(inner, ",") {
					sub := strings.Trim(strings.TrimSpace(part), `"'`)
					if sub != "" && !seenSub[sub] {
						seenSub[sub] = true
						meta.Submodules = append(meta.Submodules, sub)
					}
				}
				inSubmodulesList = false
			}
		}
	}

	targetSID := strings.TrimSpace(expectedStoryID)
	if targetSID != "" {
		if meta.ID != "" {
			if !strings.EqualFold(meta.ID, targetSID) {
				return false, meta
			}
		} else if filename == "" || !MatchesStoryIDBoundary(filename, targetSID) {
			return false, meta
		}
	}

	// Scan markdown body sections for unchecked Tasks / DoD items and _pending_ or single-file duplicate Reviews.
	section := ""
	seenReviewFile := make(map[string]bool)
	for i := closingIdx + 1; i < len(lines); i++ {
		trimmed := strings.TrimSpace(strings.TrimRight(lines[i], "\r"))
		if strings.HasPrefix(trimmed, "#") {
			heading := strings.ToLower(strings.TrimSpace(strings.TrimLeft(trimmed, "#")))
			switch {
			case strings.Contains(heading, "definition of done") || heading == "dod" || strings.HasSuffix(heading, " dod"):
				section = "dod"
			case strings.Contains(heading, "task"):
				section = "tasks"
			case strings.Contains(heading, "review"):
				section = "reviews"
			default:
				section = ""
			}
			continue
		}
		if trimmed == "" {
			continue
		}
		lowerLine := strings.ToLower(trimmed)
		switch section {
		case "tasks":
			if strings.HasPrefix(trimmed, "- [ ]") || strings.HasPrefix(trimmed, "* [ ]") {
				meta.UncheckedTasks = append(meta.UncheckedTasks, trimmed)
			}
		case "dod":
			if strings.HasPrefix(trimmed, "- [ ]") || strings.HasPrefix(trimmed, "* [ ]") {
				meta.UncheckedDoD = append(meta.UncheckedDoD, trimmed)
			}
		case "reviews":
			if strings.Contains(lowerLine, "_pending_") {
				meta.HasPendingReviews = true
			}
			if strings.HasPrefix(trimmed, "|") && !strings.HasPrefix(trimmed, "|---") && !strings.Contains(trimmed, "| ---") {
				if match := reviewFilePathRegex.FindString(trimmed); match != "" {
					norm := filepath.ToSlash(strings.ToLower(strings.TrimSpace(match)))
					meta.TotalReviewFileRows++
					if !seenReviewFile[norm] {
						seenReviewFile[norm] = true
						meta.DistinctReviewFiles = append(meta.DistinctReviewFiles, norm)
					}
				}
			}
		}
	}

	if len(meta.UncheckedTasks) > 0 {
		meta.DishonestReasons = append(meta.DishonestReasons, "unchecked_tasks")
	}
	if len(meta.UncheckedDoD) > 0 {
		meta.DishonestReasons = append(meta.DishonestReasons, "unchecked_dod")
	}
	if meta.HasPendingReviews {
		meta.DishonestReasons = append(meta.DishonestReasons, "pending_reviews_table")
	}
	if meta.ReviewSchemeScope == "full" && meta.TotalReviewFileRows >= 2 && len(meta.DistinctReviewFiles) == 1 {
		meta.DishonestReasons = append(meta.DishonestReasons, "duplicate_review_file_reuse")
	}

	isStatusDone := meta.Status == "DONE" || meta.Status == "COMPLETED" || meta.Status == "COMPLETE"
	meta.HonestDone = isStatusDone && len(meta.DishonestReasons) == 0
	return true, meta
}

// InspectStoryHonesty checks all matching .story.md copies across dirs (including child hubs <dir>/*/docs/stories)
// and enforces that every copy is DONE with clean DoD/Tasks and honest Reviews.
func InspectStoryHonesty(dirs []string, storyID string) StoryHonestyReport {
	report := StoryHonestyReport{}
	targetSID := strings.TrimSpace(storyID)
	if targetSID == "" {
		return report
	}

	var patterns []string
	seenDir := make(map[string]bool)
	for _, dir := range dirs {
		cleanDir := strings.TrimSpace(dir)
		if cleanDir == "" {
			continue
		}
		cleanDir = filepath.Clean(cleanDir)
		if abs, err := filepath.Abs(cleanDir); err == nil {
			cleanDir = abs
		}
		if seenDir[cleanDir] {
			continue
		}
		seenDir[cleanDir] = true
		patterns = append(patterns,
			filepath.Join(cleanDir, "docs", "stories", "*.story.md"),
			filepath.Join(cleanDir, "*", "docs", "stories", "*.story.md"),
		)
	}

	seenFile := make(map[string]bool)
	seenSub := make(map[string]bool)
	seenReason := make(map[string]bool)
	allDone := true

	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			continue
		}
		for _, mPath := range matches {
			absPath := filepath.Clean(mPath)
			if abs, err := filepath.Abs(absPath); err == nil {
				absPath = abs
			}
			if eval, err := filepath.EvalSymlinks(absPath); err == nil {
				absPath = eval
			}
			if seenFile[absPath] {
				continue
			}
			seenFile[absPath] = true

			content, err := os.ReadFile(absPath)
			if err != nil {
				continue
			}
			matched, meta := ParseStoryMarkdown(string(content), targetSID, absPath)
			if !matched {
				continue
			}
			report.Found = true
			report.Files = append(report.Files, meta)
			for _, sub := range meta.Submodules {
				if !seenSub[sub] {
					seenSub[sub] = true
					report.Submodules = append(report.Submodules, sub)
				}
			}
			if !meta.HonestDone {
				allDone = false
				if meta.Status == "IN_PROGRESS" || meta.Status == "READY" || meta.Status == "TODO" || meta.Status == "BLOCKED" {
					report.Incomplete = true
					reason := filepath.Base(absPath) + ":status=" + meta.Status
					if !seenReason[reason] {
						seenReason[reason] = true
						report.Reasons = append(report.Reasons, reason)
					}
				} else if meta.Status != "DONE" && meta.Status != "COMPLETED" && meta.Status != "COMPLETE" {
					report.Incomplete = true
					reason := filepath.Base(absPath) + ":status=" + meta.Status
					if !seenReason[reason] {
						seenReason[reason] = true
						report.Reasons = append(report.Reasons, reason)
					}
				} else {
					// Status says DONE/COMPLETED, yet body DoD/Tasks/Reviews failed honesty checks -> treat as incomplete.
					report.Incomplete = true
				}
				for _, r := range meta.DishonestReasons {
					reason := filepath.Base(absPath) + ":" + r
					if !seenReason[reason] {
						seenReason[reason] = true
						report.Reasons = append(report.Reasons, reason)
					}
				}
			}
		}
	}

	if report.Found {
		report.AllHonestDone = allDone
	}
	return report
}

// ResolveScopedRepoDirs builds the deduplicated list of git repositories belonging to an ASE execution:
// worktreePath itself, spRoot (if worktreePath is inside a superproject), and any existing sibling
// submodule checkouts referenced by the story's `submodules:` frontmatter.
func ResolveScopedRepoDirs(worktreePath, spRoot string, storySubmodules []string) []string {
	var dirs []string
	seen := make(map[string]bool)
	addDir := func(d string) {
		clean := strings.TrimSpace(d)
		if clean == "" {
			return
		}
		clean = filepath.Clean(clean)
		if abs, err := filepath.Abs(clean); err == nil {
			clean = abs
		}
		if eval, err := filepath.EvalSymlinks(clean); err == nil {
			clean = eval
		}
		if seen[clean] {
			return
		}
		if _, err := os.Stat(filepath.Join(clean, ".git")); err != nil {
			return
		}
		seen[clean] = true
		dirs = append(dirs, clean)
	}

	addDir(worktreePath)
	addDir(spRoot)

	hubRoot := strings.TrimSpace(spRoot)
	if hubRoot == "" && worktreePath != "" {
		if _, err := os.Stat(filepath.Join(worktreePath, ".gitmodules")); err == nil {
			hubRoot = worktreePath
		}
	}
	if hubRoot != "" {
		for _, sub := range storySubmodules {
			s := strings.Trim(strings.TrimSpace(filepath.ToSlash(sub)), "/")
			if s == "" || s == "." || s == "meta-repo" || strings.HasPrefix(s, "meta-repo-") {
				continue
			}
			addDir(filepath.Join(hubRoot, filepath.FromSlash(s)))
		}
	}
	return dirs
}

// IsNonMainBranch returns (true, branch) if dir is checked out on a feature/fix/non-main branch, detached HEAD, or unprobeable git state.
func IsNonMainBranch(dir, gitBin string) (bool, string) {
	if strings.TrimSpace(dir) == "" {
		return false, ""
	}
	if strings.TrimSpace(gitBin) == "" {
		gitBin = "git"
	}
	cmd := procutil.Command(gitBin, "rev-parse", "--abbrev-ref", "HEAD")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		// Handle freshly initialized repos with unborn HEAD (e.g. `git init -b main` before first commit).
		symCmd := procutil.Command(gitBin, "symbolic-ref", "-q", "--short", "HEAD")
		symCmd.Dir = dir
		if symOut, symErr := symCmd.Output(); symErr == nil {
			symBr := strings.TrimSpace(string(symOut))
			if symBr == "main" || symBr == "master" {
				return false, symBr
			}
			if symBr != "" {
				return true, symBr
			}
		}
		if _, statErr := os.Stat(filepath.Join(dir, ".git")); statErr != nil {
			return false, ""
		}
		return true, fmt.Sprintf("probe_error(%v)", err)
	}
	br := strings.TrimSpace(string(out))
	if br == "main" || br == "master" {
		return false, br
	}
	if br == "" {
		return true, "empty_branch"
	}
	return true, br
}

// CountUnpushedCommits returns the number of commits in dir ahead of its upstream or origin/{main,master}.
func CountUnpushedCommits(dir, gitBin string) (int, error) {
	if strings.TrimSpace(dir) == "" {
		return 0, nil
	}
	if strings.TrimSpace(gitBin) == "" {
		gitBin = "git"
	}
	cmdRemote := procutil.Command(gitBin, "remote")
	cmdRemote.Dir = dir
	remoteOut, remoteErr := cmdRemote.Output()
	if remoteErr != nil {
		return 0, fmt.Errorf("git remote check failed in %s: %w", dir, remoteErr)
	}
	if strings.TrimSpace(string(remoteOut)) == "" {
		if os.Getenv("IAZIO_TEST_SKIP_REMOTE") == "1" {
			return 0, nil
		}
		return 0, fmt.Errorf("no git remotes configured in %s", dir)
	}
	for _, refSpec := range []string{"@{u}..HEAD", "origin/main..HEAD", "origin/master..HEAD"} {
		cmd := procutil.Command(gitBin, "rev-list", "--count", refSpec)
		cmd.Dir = dir
		if out, err := cmd.Output(); err == nil {
			if c, cErr := strconv.Atoi(strings.TrimSpace(string(out))); cErr == nil {
				return c, nil
			}
		}
	}
	return 0, fmt.Errorf("no upstream tracking ref or remote origin branch found in %s", dir)
}

// CheckSubmodulesClean verifies submodule gitlink status inside rootDir. When scopedSubmodules is non-empty,
// '+'/'U'/'-' drift is checked on scoped paths and '+'/'U' drift is checked on all lines.
func CheckSubmodulesClean(rootDir, gitBin string, scopedSubmodules []string) (bool, string) {
	if strings.TrimSpace(rootDir) == "" {
		return true, ""
	}
	if _, err := os.Stat(filepath.Join(rootDir, ".gitmodules")); err != nil {
		return true, ""
	}
	if strings.TrimSpace(gitBin) == "" {
		gitBin = "git"
	}
	cmd := procutil.Command(gitBin, "submodule", "status")
	cmd.Dir = rootDir
	out, err := cmd.Output()
	if err != nil {
		return false, fmt.Sprintf("submodule_status_failed in %s: %v", filepath.Base(rootDir), err)
	}
	scopedSet := make(map[string]bool)
	for _, s := range scopedSubmodules {
		norm := strings.Trim(strings.TrimSpace(filepath.ToSlash(s)), "/")
		if norm != "" && norm != "." {
			scopedSet[norm] = true
		}
	}
	for _, rawLine := range strings.Split(string(out), "\n") {
		if len(rawLine) < 2 {
			continue
		}
		prefix := rawLine[0]
		if prefix != '+' && prefix != 'U' && prefix != '-' {
			continue
		}
		fields := strings.Fields(rawLine[1:])
		subPath := ""
		if len(fields) >= 2 {
			subPath = strings.Trim(filepath.ToSlash(fields[1]), "/")
		}
		if prefix == '-' && len(scopedSet) > 0 && !scopedSet[subPath] {
			// Uninitialized unrelated optional submodule in a full meta-repo checkout does not block scoped work.
			continue
		}
		return false, fmt.Sprintf("submodule_gitlink_drift(%c %s in %s)", prefix, subPath, filepath.Base(rootDir))
	}
	return true, ""
}
