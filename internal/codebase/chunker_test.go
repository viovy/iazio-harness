package codebase

import (
	"fmt"
	"strings"
	"testing"
)

func TestNormalizeFilePath(t *testing.T) {
	cases := []struct {
		input    string
		expected string
	}{
		{"./internal/mcp/tools.go", "internal/mcp/tools.go"},
		{"/internal/mcp/tools.go", "internal/mcp/tools.go"},
		{"internal\\mcp\\tools.go", "internal/mcp/tools.go"},
		{"cmd/../internal/codebase/chunker.go", "internal/codebase/chunker.go"},
		{"main.go", "main.go"},
	}

	for _, tc := range cases {
		actual := NormalizeFilePath(tc.input)
		if actual != tc.expected {
			t.Errorf("NormalizeFilePath(%q) = %q, expected %q", tc.input, actual, tc.expected)
		}
	}
}

func TestCalculateGitBlobSHA(t *testing.T) {
	// "hello\n" in git:
	// printf "hello\n" | git hash-object -t blob --stdin => ce013625030ba8dba906f756967f9e9ca394464a
	content := []byte("hello\n")
	sha := CalculateGitBlobSHA(content)
	expected := "ce013625030ba8dba906f756967f9e9ca394464a"
	if sha != expected {
		t.Errorf("CalculateGitBlobSHA(%q) = %q, expected %q", string(content), sha, expected)
	}
}

func TestChunkerSymbolBoundaries(t *testing.T) {
	// Test Go function and struct detection
	goCode := `package test

type ServerConfig struct {
	Port int
	Host string
}

func HandleRequest(req string) string {
	return "ok: " + req
}
`
	chunks := ChunkFile("test-repo", "server.go", "tree123", "", []byte(goCode))
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk for small file, got %d", len(chunks))
	}
	if chunks[0].StartLine != 1 || chunks[0].EndLine != 11 {
		t.Errorf("expected lines 1..11, got %d..%d", chunks[0].StartLine, chunks[0].EndLine)
	}
	if chunks[0].Symbol != "ServerConfig" {
		t.Errorf("expected first symbol ServerConfig, got %q", chunks[0].Symbol)
	}

	// Test Markdown headings
	mdDoc := `# Architecture Overview

This is the system architecture documentation.

## Control Plane

The control plane coordinates operations.

## Worker Agents

Worker agents execute subtasks.
`
	mdChunks := ChunkFile("test-repo", "docs/arch.md", "tree123", "", []byte(mdDoc))
	if len(mdChunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(mdChunks))
	}
	if mdChunks[0].Symbol != "Architecture Overview" {
		t.Errorf("expected symbol 'Architecture Overview', got %q", mdChunks[0].Symbol)
	}
}

func TestChunker_BoundarySplittingAndOverlap(t *testing.T) {
	// Build a file with multiple 30-line functions
	var sb strings.Builder
	sb.WriteString("package multi\n\n")

	for i := 1; i <= 4; i++ {
		sb.WriteString(fmt.Sprintf("func Function%d() {\n", i))
		for j := 0; j < 28; j++ {
			sb.WriteString(fmt.Sprintf("\t// line %d in func %d\n", j, i))
		}
		sb.WriteString("}\n\n")
	}

	content := []byte(sb.String())
	chunks := ChunkFile("test-repo", "multi.go", "tree_abc", "blob_123", content)

	if len(chunks) < 3 {
		t.Fatalf("expected at least 3 chunks, got %d", len(chunks))
	}

	// Verify all chunks have non-empty symbols and valid line ranges
	for i, c := range chunks {
		if c.StartLine < 1 || c.EndLine < c.StartLine {
			t.Errorf("chunk %d has invalid range %d..%d", i, c.StartLine, c.EndLine)
		}
		if c.FileBlobSHA != "blob_123" {
			t.Errorf("chunk %d has unexpected blob sha %q", i, c.FileBlobSHA)
		}
		if c.GitTreeSHA != "tree_abc" {
			t.Errorf("chunk %d has unexpected tree sha %q", i, c.GitTreeSHA)
		}
	}
}

func TestChunker_OversizedSingleFunctionOverlap(t *testing.T) {
	// Build a single function that spans 150 lines
	var sb strings.Builder
	sb.WriteString("func EnormousFunction() {\n")
	for i := 1; i <= 148; i++ {
		sb.WriteString(fmt.Sprintf("\t// work line %d\n", i))
	}
	sb.WriteString("}\n")

	content := []byte(sb.String())
	chunks := ChunkFile("repo", "big.go", "tree1", "", content)

	if len(chunks) < 2 {
		t.Fatalf("expected >= 2 chunks for 150-line function, got %d", len(chunks))
	}

	// First chunk should end around line 80
	if chunks[0].EndLine != 80 {
		t.Errorf("chunk 0 end line = %d, expected 80", chunks[0].EndLine)
	}

	// Second chunk should have 10-line overlap (start line 71)
	if chunks[1].StartLine != 71 {
		t.Errorf("chunk 1 start line = %d, expected 71 (overlap 10)", chunks[1].StartLine)
	}

	if chunks[0].Symbol != "EnormousFunction" || chunks[1].Symbol != "EnormousFunction" {
		t.Errorf("expected symbol EnormousFunction on both chunks, got %q and %q", chunks[0].Symbol, chunks[1].Symbol)
	}
}

func TestChunker_EmptyFile(t *testing.T) {
	chunks := ChunkFile("repo", "empty.go", "tree", "blob", []byte(""))
	if len(chunks) != 0 {
		t.Errorf("expected 0 chunks for empty file, got %d", len(chunks))
	}
}
