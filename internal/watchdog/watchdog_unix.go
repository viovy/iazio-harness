//go:build !windows

package watchdog

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/viovy/iazio-harness/internal/procutil"
)

func defaultIsProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return process.Signal(syscall.Signal(0)) == nil
}

func platformGetDescendantProcesses(rootPID int) ([]ProcessInfo, error) {
	if rootPID <= 0 {
		return nil, nil
	}
	// Try /proc first (fast on Linux)
	if procs, err := scanProcDescendants(rootPID); err == nil && len(procs) > 0 {
		return procs, nil
	}
	// Fallback to ps (macOS / BSD)
	return scanPSDescendants(rootPID)
}

func scanProcDescendants(rootPID int) ([]ProcessInfo, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}

	childrenMap := make(map[int][]ProcessInfo)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}

		statBytes, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "stat"))
		if err != nil {
			continue
		}
		statStr := string(statBytes)
		lastParen := strings.LastIndex(statStr, ")")
		if lastParen == -1 || lastParen+2 >= len(statStr) {
			continue
		}
		firstParen := strings.Index(statStr, "(")
		comm := ""
		if firstParen != -1 && firstParen < lastParen {
			comm = statStr[firstParen+1 : lastParen]
		}
		rest := strings.Fields(statStr[lastParen+2:])
		if len(rest) < 2 {
			continue
		}
		// rest[0] is state, rest[1] is ppid
		ppid, err := strconv.Atoi(rest[1])
		if err != nil {
			continue
		}

		cmdline := comm
		cmdBytes, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if err == nil && len(cmdBytes) > 0 {
			cleanCmd := string(bytes.ReplaceAll(cmdBytes, []byte{0}, []byte(" ")))
			cleanCmd = strings.TrimSpace(cleanCmd)
			if cleanCmd != "" {
				cmdline = cleanCmd
			}
		}

		pInfo := ProcessInfo{
			PID:     pid,
			PPID:    ppid,
			Command: comm,
			Args:    cmdline,
		}
		childrenMap[ppid] = append(childrenMap[ppid], pInfo)
	}

	return collectDescendantsBFS(rootPID, childrenMap), nil
}

func scanPSDescendants(rootPID int) ([]ProcessInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	cmd := procutil.CommandContext(ctx, "ps", "-eo", "pid,ppid,args")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	childrenMap := make(map[int][]ProcessInfo)
	lines := strings.Split(string(out), "\n")
	for i, line := range lines {
		if i == 0 {
			continue // header
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 3 {
			continue
		}
		pid, err1 := strconv.Atoi(parts[0])
		ppid, err2 := strconv.Atoi(parts[1])
		if err1 != nil || err2 != nil || pid <= 0 {
			continue
		}
		// Find start of args
		idx1 := strings.Index(line, parts[0])
		idx2 := strings.Index(line[idx1+len(parts[0]):], parts[1])
		argsStart := idx1 + len(parts[0]) + idx2 + len(parts[1])
		args := strings.TrimSpace(line[argsStart:])

		pInfo := ProcessInfo{
			PID:     pid,
			PPID:    ppid,
			Command: parts[2],
			Args:    args,
		}
		childrenMap[ppid] = append(childrenMap[ppid], pInfo)
	}

	return collectDescendantsBFS(rootPID, childrenMap), nil
}

func collectDescendantsBFS(rootPID int, childrenMap map[int][]ProcessInfo) []ProcessInfo {
	var result []ProcessInfo
	visited := make(map[int]bool)
	queue := []int{rootPID}
	visited[rootPID] = true

	for len(queue) > 0 {
		curr := queue[0]
		queue = queue[1:]

		for _, child := range childrenMap[curr] {
			if !visited[child.PID] {
				visited[child.PID] = true
				result = append(result, child)
				queue = append(queue, child.PID)
			}
		}
	}

	return result
}
