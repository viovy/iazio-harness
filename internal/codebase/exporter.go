// Package codebase provides submodule discovery, Git tree diffing, and deterministic symbol-aware code export.
package codebase

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/viovy/iazio-harness/internal/redact"
)

const (
	// MaxFileSizeBytes is the maximum source file size indexed (512 KB).
	MaxFileSizeBytes = 512 * 1024
	// BinaryProbeBytes is the number of bytes inspected for null bytes to detect binary content.
	BinaryProbeBytes = 8000
	gitmodulesFile   = ".gitmodules"
)

// RepoInfo describes a fleet submodule or superproject repository.
type RepoInfo struct {
	Name          string `json:"name"`
	Path          string `json:"path"`
	DefaultBranch string `json:"default_branch"`
	GitTreeSHA    string `json:"git_tree_sha"`
	Description   string `json:"description,omitempty"`
}

// ReposResult contains the list of discovered repositories.
type ReposResult struct {
	Repos []RepoInfo `json:"repos"`
}

// ExportRequest specifies parameters for incremental codebase chunk export.
type ExportRequest struct {
	RepoName    string `json:"repo_name"`
	BaseTreeSHA string `json:"base_tree_sha,omitempty"`
	MaxChunks   int    `json:"max_chunks,omitempty"`
}

// ExportResult contains the export chunks and deleted files metadata.
type ExportResult struct {
	RepoName     string      `json:"repo_name"`
	BaseTreeSHA  string      `json:"base_tree_sha"`
	GitTreeSHA   string      `json:"git_tree_sha"`
	UpToDate     bool        `json:"up_to_date"`
	DeletedFiles []string    `json:"deleted_files"`
	Chunks       []CodeChunk `json:"chunks"`
}

// SubmoduleEntry represents an entry parsed from .gitmodules.
type SubmoduleEntry struct {
	Name        string
	Path        string
	URL         string
	Branch      string
	Description string
}

type fileBlob struct {
	path string
	sha  string
}

// DiscoverRepos inspects fleetRoot/.gitmodules and superproject root, returning all repositories.
func DiscoverRepos(ctx context.Context, fleetRoot string) (*ReposResult, error) {
	if fleetRoot == "" {
		fleetRoot = ResolveFleetRoot()
	}
	absFleetRoot, err := filepath.Abs(fleetRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve fleet root abs path: %w", err)
	}

	var repos []RepoInfo

	// 1. Superproject root repository
	rootTreeSHA, _ := gitRevParseTree(ctx, absFleetRoot, "HEAD^{tree}")
	rootBranch, _ := gitDetectBranch(ctx, absFleetRoot)
	if rootBranch == "" {
		rootBranch = "main"
	}
	rootName := resolveRootRepoName(absFleetRoot)
	rootDesc := resolveRepoDescription(absFleetRoot, "")
	if rootDesc == "" {
		rootDesc = "Fleet superproject control plane"
	}

	repos = append(repos, RepoInfo{
		Name:          rootName,
		Path:          ".",
		DefaultBranch: rootBranch,
		GitTreeSHA:    rootTreeSHA,
		Description:   rootDesc,
	})

	// 2. Submodules from .gitmodules
	gitmodulesPath := filepath.Join(absFleetRoot, gitmodulesFile)
	if _, err := os.Stat(gitmodulesPath); err == nil {
		entries, err := parseGitmodules(gitmodulesPath)
		if err != nil {
			return nil, fmt.Errorf("failed to parse .gitmodules: %w", err)
		}

		for _, sub := range entries {
			subDir := filepath.Join(absFleetRoot, filepath.FromSlash(sub.Path))
			if fi, err := os.Stat(subDir); err != nil || !fi.IsDir() {
				// Submodule path uninitialized or missing; include with empty tree
				repos = append(repos, RepoInfo{
					Name:          sub.Name,
					Path:          sub.Path,
					DefaultBranch: defaultSubBranch(sub.Branch),
					GitTreeSHA:    "",
					Description:   sub.Description,
				})
				continue
			}

			subTreeSHA, _ := gitRevParseTree(ctx, subDir, "HEAD^{tree}")
			subBranch := sub.Branch
			if subBranch == "" {
				subBranch, _ = gitDetectBranch(ctx, subDir)
			}
			if subBranch == "" {
				subBranch = "main"
			}

			subDesc := sub.Description
			if subDesc == "" {
				subDesc = resolveRepoDescription(subDir, "")
			}

			repos = append(repos, RepoInfo{
				Name:          sub.Name,
				Path:          sub.Path,
				DefaultBranch: subBranch,
				GitTreeSHA:    subTreeSHA,
				Description:   subDesc,
			})
		}
	}

	return &ReposResult{Repos: repos}, nil
}

// Export executes incremental tree diffing or full export and returns CodeChunks.
func Export(ctx context.Context, fleetRoot string, req ExportRequest) (*ExportResult, error) {
	if fleetRoot == "" {
		fleetRoot = ResolveFleetRoot()
	}
	absFleetRoot, err := filepath.Abs(fleetRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve fleet root abs path: %w", err)
	}

	cleanRepo := strings.TrimSpace(req.RepoName)
	if cleanRepo == "" {
		return nil, errors.New("repo_name is required")
	}
	if err := validateRelativeRepoPath(cleanRepo); err != nil && !IsSuperprojectRoot(cleanRepo) {
		return nil, err
	}

	targetDir, err := resolveRepoDirectory(absFleetRoot, cleanRepo)
	if err != nil {
		return nil, fmt.Errorf("resolve repo directory for %q: %w", cleanRepo, err)
	}

	// Resolve current HEAD^{tree}
	currentTreeSHA, err := gitRevParseTree(ctx, targetDir, "HEAD^{tree}")
	if err != nil {
		return nil, fmt.Errorf("failed to resolve current HEAD^{tree} in %s: %w", targetDir, err)
	}

	baseTreeSHA := strings.TrimSpace(req.BaseTreeSHA)

	// O(1) Fast-Path: up to date
	if baseTreeSHA != "" && baseTreeSHA == currentTreeSHA {
		return &ExportResult{
			RepoName:     cleanRepo,
			BaseTreeSHA:  baseTreeSHA,
			GitTreeSHA:   currentTreeSHA,
			UpToDate:     true,
			DeletedFiles: []string{},
			Chunks:       []CodeChunk{},
		}, nil
	}

	var deletedFiles []string
	var filesToProcess []fileBlob

	if baseTreeSHA != "" {
		// Incremental diff: git diff-tree -r --no-commit-id <base> <current>
		diffOut, err := runGitCmd(ctx, targetDir, "diff-tree", "-r", "--no-commit-id", baseTreeSHA, currentTreeSHA)
		if err != nil {
			// If baseTreeSHA is unknown (e.g. stale history), gracefully fall back to full tree export
			lsOut, lsErr := runGitCmd(ctx, targetDir, "ls-tree", "-r", currentTreeSHA)
			if lsErr != nil {
				return nil, fmt.Errorf("git diff-tree failed (%v) and fallback ls-tree failed: %w", err, lsErr)
			}
			filesToProcess = parseLsTreeOutput(lsOut)
		} else {
			deletedFiles, filesToProcess = parseDiffTreeOutput(diffOut)
		}
	} else {
		// Full tree export: git ls-tree -r <current>
		lsOut, err := runGitCmd(ctx, targetDir, "ls-tree", "-r", currentTreeSHA)
		if err != nil {
			return nil, fmt.Errorf("git ls-tree failed in %s: %w", targetDir, err)
		}
		filesToProcess = parseLsTreeOutput(lsOut)
	}

	if deletedFiles == nil {
		deletedFiles = []string{}
	}

	var chunks []CodeChunk

	for _, fb := range filesToProcess {
		normPath := NormalizeFilePath(fb.path)
		if isExcludedPath(normPath) {
			continue
		}

		// Check size: git cat-file -s <sha>
		sizeStr, err := runGitCmd(ctx, targetDir, "cat-file", "-s", fb.sha)
		if err != nil {
			continue
		}
		size, _ := strconv.Atoi(strings.TrimSpace(sizeStr))
		if size > MaxFileSizeBytes {
			continue
		}

		// Read blob content directly from Git ODB
		content, err := runGitCmdRaw(ctx, targetDir, "cat-file", "blob", fb.sha)
		if err != nil {
			continue
		}

		// Skip binary files
		if isBinaryContent(content) {
			continue
		}

		// Egress secret scrubbing
		scrubbed := redact.Egress(string(content))

		// Chunk file
		fileChunks := ChunkFile(cleanRepo, normPath, currentTreeSHA, fb.sha, []byte(scrubbed))
		if req.MaxChunks > 0 && len(chunks)+len(fileChunks) > req.MaxChunks {
			remain := req.MaxChunks - len(chunks)
			if remain > 0 {
				chunks = append(chunks, fileChunks[:remain]...)
			}
			break
		}
		chunks = append(chunks, fileChunks...)
	}

	if chunks == nil {
		chunks = []CodeChunk{}
	}

	return &ExportResult{
		RepoName:     cleanRepo,
		BaseTreeSHA:  baseTreeSHA,
		GitTreeSHA:   currentTreeSHA,
		UpToDate:     false,
		DeletedFiles: deletedFiles,
		Chunks:       chunks,
	}, nil
}

func parseDiffTreeOutput(out string) ([]string, []fileBlob) {
	var deleted []string
	var modified []fileBlob

	lines := strings.Split(out, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Format: :old_mode new_mode old_sha new_sha status\tpath[\tnew_path]
		tabParts := strings.Split(line, "\t")
		if len(tabParts) < 2 {
			continue
		}
		meta := strings.Fields(tabParts[0])
		if len(meta) < 5 {
			continue
		}

		newMode := meta[1]
		newSHA := meta[3]
		status := meta[4]
		path := tabParts[1]

		// Skip gitlinks (submodule commits)
		if newMode == "160000" || meta[0] == ":160000" {
			continue
		}

		switch {
		case strings.HasPrefix(status, "D"):
			deleted = append(deleted, NormalizeFilePath(path))
		case strings.HasPrefix(status, "R"):
			deleted = append(deleted, NormalizeFilePath(path))
			if len(tabParts) >= 3 {
				newPath := tabParts[2]
				modified = append(modified, fileBlob{path: NormalizeFilePath(newPath), sha: newSHA})
			}
		case strings.HasPrefix(status, "A"), strings.HasPrefix(status, "M"), strings.HasPrefix(status, "C"):
			modified = append(modified, fileBlob{path: NormalizeFilePath(path), sha: newSHA})
		}
	}

	return deleted, modified
}

func parseLsTreeOutput(out string) []fileBlob {
	var files []fileBlob

	lines := strings.Split(out, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Format: <mode> <type> <sha>\t<path>
		tabParts := strings.Split(line, "\t")
		if len(tabParts) < 2 {
			continue
		}
		meta := strings.Fields(tabParts[0])
		if len(meta) < 3 {
			continue
		}
		objType := meta[1]
		sha := meta[2]
		path := tabParts[1]

		if objType == "blob" {
			files = append(files, fileBlob{path: NormalizeFilePath(path), sha: sha})
		}
	}

	return files
}

func isBinaryContent(content []byte) bool {
	probeLen := len(content)
	if probeLen > BinaryProbeBytes {
		probeLen = BinaryProbeBytes
	}
	for i := 0; i < probeLen; i++ {
		if content[i] == 0 {
			return true
		}
	}
	return false
}

func isExcludedPath(p string) bool {
	norm := NormalizeFilePath(p)

	// Directory prefixes
	excludedPrefixes := []string{
		"vendor/",
		"node_modules/",
		".git/",
		"dist/",
		"build/",
		".idea/",
		".vscode/",
		"tmp/",
		"bin/",
		"obj/",
		".gradle/",
		"target/",
		"coverage/",
		".next/",
		".cache/",
	}
	for _, pref := range excludedPrefixes {
		if strings.HasPrefix(norm, pref) || strings.Contains(norm, "/"+pref) {
			return true
		}
	}

	// Suffixes and exact matches
	base := filepath.Base(norm)
	excludedSuffixes := []string{
		".pb.go",
		".min.js",
		".min.css",
		".map",
		".png",
		".jpg",
		".jpeg",
		".gif",
		".ico",
		".svg",
		".webp",
		".pdf",
		".zip",
		".tar.gz",
		".tgz",
		".gz",
		".exe",
		".dll",
		".so",
		".dylib",
		".o",
		".a",
		".pyc",
		".class",
		".jar",
		".lock",
	}
	for _, suf := range excludedSuffixes {
		if strings.HasSuffix(base, suf) {
			return true
		}
	}

	excludedFiles := []string{
		"package-lock.json",
		"pnpm-lock.yaml",
		"yarn.lock",
		"go.sum",
		"Cargo.lock",
	}
	for _, f := range excludedFiles {
		if base == f {
			return true
		}
	}

	return false
}

func parseGitmodules(path string) ([]SubmoduleEntry, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var entries []SubmoduleEntry
	var current *SubmoduleEntry

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "[submodule ") && strings.HasSuffix(line, "]") {
			if current != nil && current.Path != "" {
				entries = append(entries, *current)
			}
			rawName := strings.TrimPrefix(line, "[submodule ")
			rawName = strings.TrimSuffix(rawName, "]")
			name := strings.Trim(rawName, `"' `)
			current = &SubmoduleEntry{Name: name}
			continue
		}

		if current == nil {
			continue
		}

		if strings.Contains(line, "=") {
			parts := strings.SplitN(line, "=", 2)
			key := strings.TrimSpace(parts[0])
			val := strings.TrimSpace(parts[1])
			val = strings.Trim(val, `"' `)
			switch key {
			case "path":
				current.Path = val
				if current.Name == "" {
					current.Name = val
				}
			case "url":
				current.URL = val
			case "branch":
				current.Branch = val
			case "description":
				current.Description = val
			}
		}
	}

	if current != nil && current.Path != "" {
		entries = append(entries, *current)
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name < entries[j].Name
	})

	return entries, scanner.Err()
}

func defaultSubBranch(b string) string {
	if b != "" {
		return b
	}
	return "main"
}

func resolveRootRepoName(dir string) string {
	metaFile := filepath.Join(dir, "metadata.yml")
	if data, err := os.ReadFile(metaFile); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "id:") {
				val := strings.TrimSpace(strings.TrimPrefix(line, "id:"))
				if val != "" {
					return val
				}
			}
		}
	}
	return filepath.Base(dir)
}

func resolveRepoDescription(dir, def string) string {
	metaFile := filepath.Join(dir, "metadata.yml")
	if data, err := os.ReadFile(metaFile); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "summary:") {
				val := strings.TrimSpace(strings.TrimPrefix(line, "summary:"))
				if val != "" {
					return strings.Trim(val, `"'`)
				}
			}
		}
	}
	return def
}

func resolveRepoDirectory(fleetRoot, repo string) (string, error) {
	if IsSuperprojectRoot(repo) {
		return fleetRoot, nil
	}

	targetDir := filepath.Join(fleetRoot, filepath.FromSlash(repo))
	if fi, err := os.Stat(targetDir); err == nil && fi.IsDir() {
		return targetDir, nil
	}

	// Try lookup in .gitmodules
	gitmodulesPath := filepath.Join(fleetRoot, gitmodulesFile)
	if entries, err := parseGitmodules(gitmodulesPath); err == nil {
		for _, e := range entries {
			if e.Name == repo || e.Path == repo {
				subDir := filepath.Join(fleetRoot, filepath.FromSlash(e.Path))
				if fi, err := os.Stat(subDir); err == nil && fi.IsDir() {
					return subDir, nil
				}
			}
		}
	}

	return "", fmt.Errorf("repository %q not found under fleet root %s", repo, fleetRoot)
}

func gitRevParseTree(ctx context.Context, dir, ref string) (string, error) {
	out, err := runGitCmd(ctx, dir, "rev-parse", ref)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func gitDetectBranch(ctx context.Context, dir string) (string, error) {
	out, err := runGitCmd(ctx, dir, "symbolic-ref", "--short", "HEAD")
	if err == nil {
		branch := strings.TrimSpace(out)
		if branch != "" {
			return branch, nil
		}
	}

	out, err = runGitCmd(ctx, dir, "rev-parse", "--abbrev-ref", "HEAD")
	if err == nil {
		branch := strings.TrimSpace(out)
		if branch != "" && branch != "HEAD" {
			return branch, nil
		}
	}

	return "main", nil
}

func runGitCmd(ctx context.Context, dir string, args ...string) (string, error) {
	raw, err := runGitCmdRaw(ctx, dir, args...)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func runGitCmdRaw(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git %s in %s: %w (stderr: %s)", strings.Join(args, " "), dir, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// findGitmodulesRoot walks upward from startDir seeking the first directory containing .gitmodules.
func findGitmodulesRoot(startDir string) string {
	curr := startDir
	for {
		if _, err := os.Stat(filepath.Join(curr, gitmodulesFile)); err == nil {
			return curr
		}
		parent := filepath.Dir(curr)
		if parent == curr {
			break
		}
		curr = parent
	}
	return ""
}

// ResolveFleetRoot locates the fleet superproject root from env, cwd, or parent dirs.
func ResolveFleetRoot() string {
	if env := os.Getenv("IAZIO_FLEET_ROOT"); env != "" {
		if fi, err := os.Stat(env); err == nil && fi.IsDir() {
			if root := findGitmodulesRoot(env); root != "" {
				return root
			}
		}
		return env
	}

	dir, err := os.Getwd()
	if err != nil {
		dir = "."
	}
	if root := findGitmodulesRoot(dir); root != "" {
		return root
	}

	// Probe ~/.iazio/fleet_root if configured on host
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if b, err := os.ReadFile(filepath.Join(home, ".iazio", "fleet_root")); err == nil {
			p := strings.TrimSpace(string(b))
			if p != "" {
				if _, err := os.Stat(filepath.Join(p, gitmodulesFile)); err == nil {
					return p
				}
			}
		}
		// Probe standard host fleet locations
		workDir := filepath.Join(home, "work")
		if entries, err := os.ReadDir(workDir); err == nil {
			for _, e := range entries {
				if e.IsDir() {
					cand := filepath.Join(workDir, e.Name())
					if _, err := os.Stat(filepath.Join(cand, gitmodulesFile)); err == nil {
						return cand
					}
				}
			}
		}
	}

	return dir
}

func validateRelativeRepoPath(p string) error {
	clean := strings.TrimSpace(p)
	if clean == "" {
		return errors.New("repo must not be empty")
	}
	if strings.Contains(clean, "..") {
		return fmt.Errorf("path traversal not permitted in repo: %q", p)
	}
	if strings.HasPrefix(clean, "/") || strings.HasPrefix(clean, "\\") {
		return fmt.Errorf("absolute paths not permitted in repo: %q", p)
	}
	if len(clean) >= 2 && clean[1] == ':' && ((clean[0] >= 'a' && clean[0] <= 'z') || (clean[0] >= 'A' && clean[0] <= 'Z')) {
		return fmt.Errorf("volume prefixes not permitted in repo: %q", p)
	}
	return nil
}

