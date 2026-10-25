//go:build windows

package watchdog

import (
	"syscall"
	"unsafe"
)

var (
	kernel32               = syscall.NewLazyDLL("kernel32.dll")
	procOpenProcess        = kernel32.NewProc("OpenProcess")
	procCloseHandle        = kernel32.NewProc("CloseHandle")
	procGetExitCodeProcess = kernel32.NewProc("GetExitCodeProcess")
)

const (
	processQueryLimitedInformation = 0x1000
	stillActive                    = 259
)

func defaultIsProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, _, _ := procOpenProcess.Call(uintptr(processQueryLimitedInformation), 0, uintptr(pid))
	if h == 0 {
		return false
	}
	defer procCloseHandle.Call(h)
	var exitCode uint32
	r, _, _ := procGetExitCodeProcess.Call(h, uintptr(unsafe.Pointer(&exitCode)))
	if r == 0 {
		return false
	}
	return exitCode == stillActive
}

func platformGetDescendantProcesses(rootPID int) ([]ProcessInfo, error) {
	if rootPID <= 0 {
		return nil, nil
	}
	handle, err := syscall.CreateToolhelp32Snapshot(syscall.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer syscall.CloseHandle(handle)

	var entry syscall.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	if err := syscall.Process32First(handle, &entry); err != nil {
		return nil, err
	}

	parentToChildren := make(map[int][]ProcessInfo)
	for {
		pid := int(entry.ProcessID)
		ppid := int(entry.ParentProcessID)
		cmd := syscall.UTF16ToString(entry.ExeFile[:])
		if pid != ppid && pid > 0 {
			parentToChildren[ppid] = append(parentToChildren[ppid], ProcessInfo{
				PID:     pid,
				PPID:    ppid,
				Command: cmd,
			})
		}
		if err := syscall.Process32Next(handle, &entry); err != nil {
			break
		}
	}

	var descendants []ProcessInfo
	queue := []int{rootPID}
	visited := map[int]bool{rootPID: true}

	for len(queue) > 0 {
		curr := queue[0]
		queue = queue[1:]

		for _, child := range parentToChildren[curr] {
			if !visited[child.PID] {
				visited[child.PID] = true
				descendants = append(descendants, child)
				queue = append(queue, child.PID)
			}
		}
	}

	return descendants, nil
}
