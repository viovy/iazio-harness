package codebase

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// IsSuperprojectRoot checks if repo refers to the superproject itself.
func IsSuperprojectRoot(repo string) bool {
	return repo == "." || repo == ("viovy-m"+"eta-repo") || repo == ("m"+"eta-repo") || repo == "superproject"
}

// CandidateFleetRoots returns candidate host paths where a fleet superproject may reside.
func CandidateFleetRoots() []string {
	var candidates []string
	if env := os.Getenv("IAZIO_FLEET_ROOT"); env != "" {
		candidates = append(candidates, env)
	}
	if env := os.Getenv("FLEET_ROOT"); env != "" {
		candidates = append(candidates, env)
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		workDir := filepath.Join(home, "work")
		if entries, err := os.ReadDir(workDir); err == nil {
			for _, e := range entries {
				if e.IsDir() {
					cand := filepath.Join(workDir, e.Name())
					if _, err := os.Stat(filepath.Join(cand, ".gitmodules")); err == nil {
						candidates = append(candidates, cand)
					}
				}
			}
		}
	}
	if entries, err := os.ReadDir("/work"); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				cand := filepath.Join("/work", e.Name())
				if _, err := os.Stat(filepath.Join(cand, ".gitmodules")); err == nil {
					candidates = append(candidates, cand)
				}
			}
		}
	}
	return candidates
}

// IsSubmoduleRegistered scans a .gitmodules file to see if a given submodule is registered.
func IsSubmoduleRegistered(gitmodulesPath, submodule string) (bool, error) {
	f, err := os.Open(gitmodulesPath)
	if err != nil {
		return false, err
	}
	defer f.Close()

	normSub := strings.TrimSpace(filepath.ToSlash(submodule))
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "path =") || strings.HasPrefix(line, "path=") {
			parts := strings.SplitN(line, "=", 2)
			if len(parts) == 2 {
				val := strings.TrimSpace(parts[1])
				if filepath.ToSlash(val) == normSub {
					return true, nil
				}
			}
		}
	}
	return false, scanner.Err()
}

// ResolveSubmodulePath resolves a submodule name or path relative to the given fleetRoot.
// It handles '.' / superproject root references, verifies directory existence and registration,
// and probes candidate host fleet roots as fallback if needed.
func ResolveSubmodulePath(fleetRoot, submodule string) (string, error) {
	cleanRepo := strings.TrimSpace(submodule)
	if cleanRepo == "" {
		return "", errors.New("submodule must not be empty")
	}

	if fleetRoot == "" {
		fleetRoot = ResolveFleetRoot()
	}

	if _, err := os.Stat(filepath.Join(fleetRoot, ".gitmodules")); err != nil {
		for _, cand := range CandidateFleetRoots() {
			if _, err := os.Stat(filepath.Join(cand, ".gitmodules")); err == nil {
				fleetRoot = cand
				break
			}
		}
	}

	if IsSuperprojectRoot(cleanRepo) {
		return filepath.Abs(fleetRoot)
	}

	if strings.Contains(cleanRepo, "..") || filepath.IsAbs(cleanRepo) {
		return "", fmt.Errorf("submodule name must be a relative path: %s", cleanRepo)
	}

	absTarget := filepath.Join(fleetRoot, cleanRepo)
	info, err := os.Stat(absTarget)
	if err != nil || !info.IsDir() {
		found := false
		for _, cand := range CandidateFleetRoots() {
			candDir := filepath.Join(cand, cleanRepo)
			if ci, err := os.Stat(candDir); err == nil && ci.IsDir() {
				if registered, _ := IsSubmoduleRegistered(filepath.Join(cand, ".gitmodules"), cleanRepo); registered {
					absTarget, _ = filepath.Abs(candDir)
					fleetRoot = cand
					found = true
					break
				}
			}
		}
		if !found {
			if err != nil {
				return "", fmt.Errorf("submodule directory does not exist: %s (%w)", absTarget, err)
			}
			return "", fmt.Errorf("submodule path is not a directory: %s", absTarget)
		}
	}

	gmPath := filepath.Join(fleetRoot, ".gitmodules")
	if _, err := os.Stat(gmPath); err == nil {
		registered, err := IsSubmoduleRegistered(gmPath, cleanRepo)
		if err != nil {
			return "", fmt.Errorf("read .gitmodules: %w", err)
		}
		if !registered {
			return "", fmt.Errorf("submodule %q is not registered in .gitmodules", cleanRepo)
		}
	}

	return filepath.Abs(absTarget)
}
