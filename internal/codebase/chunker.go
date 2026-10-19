// Package codebase provides submodule discovery, tree diffing, and deterministic symbol-aware code chunking.
package codebase

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	// MaxChunkLines is the maximum number of lines per chunk.
	MaxChunkLines = 80
	// MinChunkLines is the minimum number of lines before breaking on a declaration boundary.
	MinChunkLines = 25
	// ChunkOverlapLines is the overlap when splitting an oversized single function or block.
	ChunkOverlapLines = 10
)

var (
	goFuncRegex       = regexp.MustCompile(`(?m)^func\s+(?:\([^)]+\)\s+)?([A-Za-z0-9_]+)`)
	goTypeRegex       = regexp.MustCompile(`(?m)^type\s+([A-Za-z0-9_]+)\s+(?:struct|interface)`)
	pyFuncRegex       = regexp.MustCompile(`(?m)^\s*(?:async\s+)?def\s+([A-Za-z0-9_]+)`)
	pyClassRegex      = regexp.MustCompile(`(?m)^\s*class\s+([A-Za-z0-9_]+)`)
	jsFuncRegex       = regexp.MustCompile(`(?m)^\s*(?:export\s+)?(?:default\s+)?(?:async\s+)?function\s+([A-Za-z0-9_]+)`)
	jsClassRegex      = regexp.MustCompile(`(?m)^\s*(?:export\s+)?class\s+([A-Za-z0-9_]+)`)
	jsArrowRegex      = regexp.MustCompile(`(?m)^\s*(?:export\s+)?(?:const|let|var)\s+([A-Za-z0-9_]+)\s*=\s*(?:async\s+)?\([^)]*\)\s*=>`)
	rustFnRegex       = regexp.MustCompile(`(?m)^\s*(?:pub\s+)?(?:async\s+)?fn\s+([A-Za-z0-9_]+)`)
	rustStructRegex   = regexp.MustCompile(`(?m)^\s*(?:pub\s+)?(?:struct|enum|trait|impl)\s+([A-Za-z0-9_]+)`)
	markdownHeadRegex = regexp.MustCompile(`(?m)^#{1,6}\s+(.+)$`)
)

// CodeChunk describes a snippet of source code tagged with AST/symbol, line numbers, and Git SHA metadata.
type CodeChunk struct {
	RepoName    string `json:"repo_name"`
	FilePath    string `json:"file_path"`
	StartLine   int    `json:"start_line"`
	EndLine     int    `json:"end_line"`
	Symbol      string `json:"symbol,omitempty"`
	Content     string `json:"content"`
	FileBlobSHA string `json:"file_blob_sha"`
	GitTreeSHA  string `json:"git_tree_sha"`
}

// CalculateGitBlobSHA returns the 40-character hexadecimal Git blob SHA-1 for content.
// Format matches `git hash-object`: sha1("blob <size>\0<content>").
func CalculateGitBlobSHA(content []byte) string {
	h := sha1.New()
	header := fmt.Sprintf("blob %d\x00", len(content))
	h.Write([]byte(header))
	h.Write(content)
	return hex.EncodeToString(h.Sum(nil))
}

// NormalizeFilePath converts Windows separators, trims leading './' or '/', and returns a clean POSIX path.
func NormalizeFilePath(p string) string {
	clean := strings.ReplaceAll(p, "\\", "/")
	clean = filepath.ToSlash(filepath.Clean(clean))
	clean = strings.TrimPrefix(clean, "./")
	clean = strings.TrimPrefix(clean, "/")
	return clean
}

// ChunkFile splits file content into deterministic CodeChunks aligned on declaration boundaries.
func ChunkFile(repoName, filePath, gitTreeSHA, blobSHA string, content []byte) []CodeChunk {
	normPath := NormalizeFilePath(filePath)
	if len(content) == 0 {
		return nil
	}

	if blobSHA == "" {
		blobSHA = CalculateGitBlobSHA(content)
	}

	rawText := string(content)
	lines := strings.Split(rawText, "\n")
	totalLines := len(lines)
	if totalLines == 0 {
		return nil
	}

	// Small file optimization: 1 chunk
	if totalLines <= MaxChunkLines {
		symbol := DetectPrimarySymbol(lines)
		return []CodeChunk{
			{
				RepoName:    repoName,
				FilePath:    normPath,
				StartLine:   1,
				EndLine:     totalLines,
				Symbol:      symbol,
				Content:     rawText,
				FileBlobSHA: blobSHA,
				GitTreeSHA:  gitTreeSHA,
			},
		}
	}

	// Boundary-aware chunking
	var chunks []CodeChunk
	curStart := 0 // 0-indexed line index
	curSymbol := DetectSymbolOnLine(lines[0])

	for i := 0; i < totalLines; i++ {
		line := lines[i]
		sym := DetectSymbolOnLine(line)

		chunkLen := i - curStart + 1
		isBoundary := sym != "" && i > curStart

		// Case 1: Reached a new declaration boundary with at least MinChunkLines
		if isBoundary && chunkLen >= MinChunkLines {
			// Chunk lines [curStart, i-1]
			endLineIdx := i - 1
			if endLineIdx < curStart {
				endLineIdx = curStart
			}
			chunkContent := strings.Join(lines[curStart:endLineIdx+1], "\n")
			chunks = append(chunks, CodeChunk{
				RepoName:    repoName,
				FilePath:    normPath,
				StartLine:   curStart + 1,
				EndLine:     endLineIdx + 1,
				Symbol:      curSymbol,
				Content:     chunkContent,
				FileBlobSHA: blobSHA,
				GitTreeSHA:  gitTreeSHA,
			})

			curStart = i
			curSymbol = sym
			continue
		}

		// Case 2: Hit MaxChunkLines limit
		if chunkLen >= MaxChunkLines {
			endLineIdx := i
			chunkContent := strings.Join(lines[curStart:endLineIdx+1], "\n")
			chunks = append(chunks, CodeChunk{
				RepoName:    repoName,
				FilePath:    normPath,
				StartLine:   curStart + 1,
				EndLine:     endLineIdx + 1,
				Symbol:      curSymbol,
				Content:     chunkContent,
				FileBlobSHA: blobSHA,
				GitTreeSHA:  gitTreeSHA,
			})

			// Check if we need overlap for oversized block
			nextStart := endLineIdx + 1 - ChunkOverlapLines
			if nextStart <= curStart || nextStart >= totalLines {
				nextStart = endLineIdx + 1
			}
			if nextStart < totalLines {
				curStart = nextStart
				i = curStart - 1 // will be incremented by loop
				if curSymbol == "" {
					curSymbol = DetectSymbolOnLine(lines[curStart])
				}
			} else {
				curStart = totalLines
				break
			}
		}
	}

	// Remaining lines
	if curStart < totalLines {
		chunkContent := strings.Join(lines[curStart:], "\n")
		remSymbol := curSymbol
		if remSymbol == "" {
			remSymbol = DetectPrimarySymbol(lines[curStart:])
		}
		chunks = append(chunks, CodeChunk{
			RepoName:    repoName,
			FilePath:    normPath,
			StartLine:   curStart + 1,
			EndLine:     totalLines,
			Symbol:      remSymbol,
			Content:     chunkContent,
			FileBlobSHA: blobSHA,
			GitTreeSHA:  gitTreeSHA,
		})
	}

	return chunks
}

// DetectSymbolOnLine detects if a line introduces a symbol or heading.
func DetectSymbolOnLine(line string) string {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return ""
	}

	if m := goFuncRegex.FindStringSubmatch(line); len(m) > 1 {
		return m[1]
	}
	if m := goTypeRegex.FindStringSubmatch(line); len(m) > 1 {
		return m[1]
	}
	if m := pyFuncRegex.FindStringSubmatch(line); len(m) > 1 {
		return m[1]
	}
	if m := pyClassRegex.FindStringSubmatch(line); len(m) > 1 {
		return m[1]
	}
	if m := jsFuncRegex.FindStringSubmatch(line); len(m) > 1 {
		return m[1]
	}
	if m := jsClassRegex.FindStringSubmatch(line); len(m) > 1 {
		return m[1]
	}
	if m := jsArrowRegex.FindStringSubmatch(line); len(m) > 1 {
		return m[1]
	}
	if m := rustFnRegex.FindStringSubmatch(line); len(m) > 1 {
		return m[1]
	}
	if m := rustStructRegex.FindStringSubmatch(line); len(m) > 1 {
		return m[1]
	}
	if m := markdownHeadRegex.FindStringSubmatch(line); len(m) > 1 {
		return strings.TrimSpace(m[1])
	}
	return ""
}

// DetectPrimarySymbol scans lines and returns the first top-level symbol encountered.
func DetectPrimarySymbol(lines []string) string {
	for _, l := range lines {
		if s := DetectSymbolOnLine(l); s != "" {
			return s
		}
	}
	return ""
}
