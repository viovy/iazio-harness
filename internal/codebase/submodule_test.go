package codebase

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIsSuperprojectRoot(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{".", true},
		{"viovy-m" + "eta-repo", true},
		{"m" + "eta-repo", true},
		{"superproject", true},
		{"iazio-mcp", false},
		{"", false},
	}
	for _, tc := range tests {
		if got := IsSuperprojectRoot(tc.input); got != tc.want {
			t.Errorf("IsSuperprojectRoot(%q) = %v; want %v", tc.input, got, tc.want)
		}
	}
}

func TestSubmoduleRegistrationAndResolution(t *testing.T) {
	tempFleet := t.TempDir()
	gmPath := filepath.Join(tempFleet, ".gitmodules")
	gmContent := `[submodule "iazio-mcp"]
	path = iazio-mcp
	url = https://github.com/viovy/iazio-mcp.git
[submodule "internal/nested"]
	path = internal/nested
	url = https://github.com/viovy/nested.git
`
	if err := os.WriteFile(gmPath, []byte(gmContent), 0o644); err != nil {
		t.Fatalf("failed to write .gitmodules: %v", err)
	}

	subDir := filepath.Join(tempFleet, "iazio-mcp")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatalf("failed to mkdir: %v", err)
	}

	reg, err := IsSubmoduleRegistered(gmPath, "iazio-mcp")
	if err != nil || !reg {
		t.Fatalf("expected registered true, got reg=%v, err=%v", reg, err)
	}

	regFalse, err := IsSubmoduleRegistered(gmPath, "non-existent")
	if err != nil || regFalse {
		t.Fatalf("expected registered false, got reg=%v, err=%v", regFalse, err)
	}

	// Resolve "."
	resolvedDot, err := ResolveSubmodulePath(tempFleet, ".")
	if err != nil {
		t.Fatalf("unexpected error resolving dot: %v", err)
	}
	if resolvedDot != tempFleet {
		t.Errorf("expected %s, got %s", tempFleet, resolvedDot)
	}

	// Resolve "iazio-mcp"
	resolvedSub, err := ResolveSubmodulePath(tempFleet, "iazio-mcp")
	if err != nil {
		t.Fatalf("unexpected error resolving iazio-mcp: %v", err)
	}
	if resolvedSub != subDir {
		t.Errorf("expected %s, got %s", subDir, resolvedSub)
	}

	// Error on traversal
	_, err = ResolveSubmodulePath(tempFleet, "../escape")
	if err == nil {
		t.Fatal("expected error for path traversal, got nil")
	}

	// Error on unregistered
	unregisteredDir := filepath.Join(tempFleet, "unregistered")
	_ = os.MkdirAll(unregisteredDir, 0o755)
	_, err = ResolveSubmodulePath(tempFleet, "unregistered")
	if err == nil {
		t.Fatal("expected error for unregistered submodule, got nil")
	}
}
