package codebase

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Tester",
		"GIT_AUTHOR_EMAIL=tester@example.com",
		"GIT_COMMITTER_NAME=Tester",
		"GIT_COMMITTER_EMAIL=tester@example.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed in %s: %v (output: %s)", strings.Join(args, " "), dir, err, string(out))
	}
	return strings.TrimSpace(string(out))
}

func TestCodebaseRepos(t *testing.T) {
	ctx := context.Background()
	fleetDir := t.TempDir()

	// Init root superproject
	runGit(t, fleetDir, "init")
	runGit(t, fleetDir, "config", "user.name", "Tester")
	runGit(t, fleetDir, "config", "user.email", "tester@example.com")

	// Create root files
	readmePath := filepath.Join(fleetDir, "README.md")
	if err := os.WriteFile(readmePath, []byte("# Superproject\nRoot repository.\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Create submodule directory sub1
	sub1Dir := filepath.Join(fleetDir, "sub1")
	if err := os.MkdirAll(sub1Dir, 0755); err != nil {
		t.Fatal(err)
	}
	runGit(t, sub1Dir, "init")
	runGit(t, sub1Dir, "config", "user.name", "Tester")
	runGit(t, sub1Dir, "config", "user.email", "tester@example.com")

	sub1File := filepath.Join(sub1Dir, "handler.go")
	if err := os.WriteFile(sub1File, []byte("package sub1\n\nfunc Handle() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit(t, sub1Dir, "add", "handler.go")
	runGit(t, sub1Dir, "commit", "-m", "initial sub1 commit")

	// Create .gitmodules in fleet root
	gitmodulesContent := `[submodule "sub1"]
	path = sub1
	url = https://github.com/example/sub1.git
	description = Submodule 1 description
	branch = main
`
	if err := os.WriteFile(filepath.Join(fleetDir, ".gitmodules"), []byte(gitmodulesContent), 0644); err != nil {
		t.Fatal(err)
	}

	runGit(t, fleetDir, "add", "README.md", ".gitmodules")
	runGit(t, fleetDir, "commit", "-m", "initial superproject commit")

	res, err := DiscoverRepos(ctx, fleetDir)
	if err != nil {
		t.Fatalf("DiscoverRepos failed: %v", err)
	}

	if len(res.Repos) < 2 {
		t.Fatalf("expected at least 2 repos (root + sub1), got %d", len(res.Repos))
	}

	// Verify root repo
	var rootFound, sub1Found bool
	for _, r := range res.Repos {
		if r.Path == "." {
			rootFound = true
			if r.GitTreeSHA == "" {
				t.Error("root git_tree_sha should not be empty")
			}
		}
		if r.Name == "sub1" && r.Path == "sub1" {
			sub1Found = true
			if r.GitTreeSHA == "" {
				t.Error("sub1 git_tree_sha should not be empty")
			}
			if r.Description != "Submodule 1 description" {
				t.Errorf("sub1 description = %q, expected 'Submodule 1 description'", r.Description)
			}
			if r.DefaultBranch != "main" {
				t.Errorf("sub1 default_branch = %q, expected 'main'", r.DefaultBranch)
			}
		}
	}

	if !rootFound {
		t.Error("root superproject repo was not found in DiscoverRepos")
	}
	if !sub1Found {
		t.Error("submodule sub1 was not found in DiscoverRepos")
	}
}

func TestCodebaseIncrementalExport(t *testing.T) {
	ctx := context.Background()
	repoDir := t.TempDir()

	runGit(t, repoDir, "init")
	runGit(t, repoDir, "config", "user.name", "Tester")
	runGit(t, repoDir, "config", "user.email", "tester@example.com")

	// Commit 1: math.go, guide.md, legacy.go, vendor/lib.go, logo.png (binary), secret.go
	if err := os.MkdirAll(filepath.Join(repoDir, "pkg"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repoDir, "docs"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repoDir, "vendor"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repoDir, "assets"), 0755); err != nil {
		t.Fatal(err)
	}

	mathGo := "package pkg\n\nfunc Add(a, b int) int {\n\treturn a + b\n}\n"
	if err := os.WriteFile(filepath.Join(repoDir, "pkg", "math.go"), []byte(mathGo), 0644); err != nil {
		t.Fatal(err)
	}

	guideMd := "# User Guide\n\nThis is the documentation guide.\n"
	if err := os.WriteFile(filepath.Join(repoDir, "docs", "guide.md"), []byte(guideMd), 0644); err != nil {
		t.Fatal(err)
	}

	legacyGo := "package pkg\n\nfunc LegacyCompute() string {\n\treturn \"legacy\"\n}\n"
	if err := os.WriteFile(filepath.Join(repoDir, "pkg", "legacy.go"), []byte(legacyGo), 0644); err != nil {
		t.Fatal(err)
	}

	vendorGo := "package vendor\n\nfunc VendorFunc() {}\n"
	if err := os.WriteFile(filepath.Join(repoDir, "vendor", "lib.go"), []byte(vendorGo), 0644); err != nil {
		t.Fatal(err)
	}

	binaryLogo := []byte("PNG\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01")
	if err := os.WriteFile(filepath.Join(repoDir, "assets", "logo.png"), binaryLogo, 0644); err != nil {
		t.Fatal(err)
	}

	secretGo := "package pkg\n\n// Secret apiKey = \"sk-live-1234567890abcdef\"\nfunc Auth() {}\n"
	if err := os.WriteFile(filepath.Join(repoDir, "pkg", "secret.go"), []byte(secretGo), 0644); err != nil {
		t.Fatal(err)
	}

	runGit(t, repoDir, "add", "-A")
	runGit(t, repoDir, "commit", "-m", "commit 1")

	tree1SHA := runGit(t, repoDir, "rev-parse", "HEAD^{tree}")

	// 1. Initial Full Export (base_tree_sha: "")
	reqFull := ExportRequest{
		RepoName:    ".",
		BaseTreeSHA: "",
	}
	resFull, err := Export(ctx, repoDir, reqFull)
	if err != nil {
		t.Fatalf("full export failed: %v", err)
	}

	if resFull.UpToDate {
		t.Error("initial export should have up_to_date: false")
	}
	if resFull.GitTreeSHA != tree1SHA {
		t.Errorf("expected git_tree_sha %q, got %q", tree1SHA, resFull.GitTreeSHA)
	}
	if len(resFull.DeletedFiles) != 0 {
		t.Errorf("expected 0 deleted files on initial export, got %d", len(resFull.DeletedFiles))
	}

	// Verify exclusions and secret scrubbing
	var foundMath, foundGuide, foundLegacy, foundSecret bool
	for _, c := range resFull.Chunks {
		if strings.HasPrefix(c.FilePath, "vendor/") {
			t.Errorf("vendor path %q should have been excluded", c.FilePath)
		}
		if strings.HasSuffix(c.FilePath, "logo.png") {
			t.Errorf("binary path %q should have been excluded", c.FilePath)
		}
		if c.FilePath == "pkg/math.go" {
			foundMath = true
			if c.Symbol != "Add" {
				t.Errorf("expected symbol Add, got %q", c.Symbol)
			}
		}
		if c.FilePath == "docs/guide.md" {
			foundGuide = true
		}
		if c.FilePath == "pkg/legacy.go" {
			foundLegacy = true
		}
		if c.FilePath == "pkg/secret.go" {
			foundSecret = true
			if strings.Contains(c.Content, "sk-live-1234567890abcdef") {
				t.Errorf("secret token was not redacted in chunk content: %s", c.Content)
			}
		}
	}
	if !foundMath || !foundGuide || !foundLegacy || !foundSecret {
		t.Errorf("missing expected chunks: math=%v, guide=%v, legacy=%v, secret=%v", foundMath, foundGuide, foundLegacy, foundSecret)
	}

	// 2. Fast-Path O(1) Check when base_tree_sha == git_tree_sha
	reqFast := ExportRequest{
		RepoName:    ".",
		BaseTreeSHA: tree1SHA,
	}
	resFast, err := Export(ctx, repoDir, reqFast)
	if err != nil {
		t.Fatalf("fast path export failed: %v", err)
	}
	if !resFast.UpToDate {
		t.Error("expected up_to_date: true for matching tree SHA")
	}
	if len(resFast.Chunks) != 0 {
		t.Errorf("expected 0 chunks on fast path, got %d", len(resFast.Chunks))
	}
	if len(resFast.DeletedFiles) != 0 {
		t.Errorf("expected 0 deleted files on fast path, got %d", len(resFast.DeletedFiles))
	}

	// Commit 2: Modify math.go, Delete legacy.go, Add new_feature.go
	mathGoV2 := "package pkg\n\nfunc Add(a, b int) int {\n\treturn a + b\n}\n\nfunc Multiply(a, b int) int {\n\treturn a * b\n}\n"
	if err := os.WriteFile(filepath.Join(repoDir, "pkg", "math.go"), []byte(mathGoV2), 0644); err != nil {
		t.Fatal(err)
	}

	runGit(t, repoDir, "rm", "pkg/legacy.go")

	newFeatureGo := "package pkg\n\nfunc NewFeature() bool {\n\treturn true\n}\n"
	if err := os.WriteFile(filepath.Join(repoDir, "pkg", "new_feature.go"), []byte(newFeatureGo), 0644); err != nil {
		t.Fatal(err)
	}

	runGit(t, repoDir, "add", "pkg/math.go", "pkg/new_feature.go")
	runGit(t, repoDir, "commit", "-m", "commit 2: modify math, rm legacy, add new_feature")

	tree2SHA := runGit(t, repoDir, "rev-parse", "HEAD^{tree}")

	// Add dirty uncommitted file and modify working copy to verify reading committed blobs directly from Git
	dirtyScratch := filepath.Join(repoDir, "pkg", "dirty_uncommitted.go")
	if err := os.WriteFile(dirtyScratch, []byte("package pkg\nfunc Dirty() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	// Dirty edit to math.go in working tree (uncommitted)
	if err := os.WriteFile(filepath.Join(repoDir, "pkg", "math.go"), []byte("package pkg\nfunc UNCOMMITTED() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// 3. Incremental Export from tree1SHA to tree2SHA
	reqIncr := ExportRequest{
		RepoName:    ".",
		BaseTreeSHA: tree1SHA,
	}
	resIncr, err := Export(ctx, repoDir, reqIncr)
	if err != nil {
		t.Fatalf("incremental export failed: %v", err)
	}

	if resIncr.UpToDate {
		t.Error("expected up_to_date: false on incremental changes")
	}
	if resIncr.BaseTreeSHA != tree1SHA || resIncr.GitTreeSHA != tree2SHA {
		t.Errorf("unexpected tree SHAs: base=%q, git=%q", resIncr.BaseTreeSHA, resIncr.GitTreeSHA)
	}

	// Verify deleted_files contains pkg/legacy.go
	var legacyDeleted bool
	for _, d := range resIncr.DeletedFiles {
		if d == "pkg/legacy.go" {
			legacyDeleted = true
		}
	}
	if !legacyDeleted {
		t.Errorf("expected pkg/legacy.go in deleted_files, got %v", resIncr.DeletedFiles)
	}

	// Verify chunks contain modified math.go and added new_feature.go, and NOT unchanged guide.md
	var mathUpdated, newFeatureAdded bool
	for _, c := range resIncr.Chunks {
		if c.FilePath == "docs/guide.md" {
			t.Error("unchanged file docs/guide.md should not be re-chunked in incremental export")
		}
		if c.FilePath == "pkg/dirty_uncommitted.go" {
			t.Error("uncommitted dirty file should never be indexed")
		}
		if c.FilePath == "pkg/math.go" {
			mathUpdated = true
			if strings.Contains(c.Content, "UNCOMMITTED") {
				t.Error("math.go chunk contains uncommitted dirty edit instead of committed Git blob")
			}
			if !strings.Contains(c.Content, "Multiply") {
				t.Error("math.go chunk missing committed Multiply function")
			}
		}
		if c.FilePath == "pkg/new_feature.go" {
			newFeatureAdded = true
			if c.Symbol != "NewFeature" {
				t.Errorf("expected symbol NewFeature, got %q", c.Symbol)
			}
		}
	}

	if !mathUpdated {
		t.Error("expected pkg/math.go in incremental chunks")
	}
	if !newFeatureAdded {
		t.Error("expected pkg/new_feature.go in incremental chunks")
	}
}
