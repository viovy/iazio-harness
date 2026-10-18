//go:build windows

package engines

import (
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"unsafe"
)

var (
	kernel32                     = syscall.NewLazyDLL("kernel32.dll")
	procCreateJobObjectW         = kernel32.NewProc("CreateJobObjectW")
	procSetInformationJobObject  = kernel32.NewProc("SetInformationJobObject")
	procAssignProcessToJobObject = kernel32.NewProc("AssignProcessToJobObject")
	procTerminateJobObject       = kernel32.NewProc("TerminateJobObject")
	procCloseHandle              = kernel32.NewProc("CloseHandle")
)

const (
	jobObjectExtendedLimitInformation = 9
	jobObjectLimitKillOnJobClose      = 0x2000
)

type jobObjectBasicLimitInformation struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

type ioCounters struct {
	ReadOperationCount  uint64
	WriteOperationCount uint64
	OtherOperationCount uint64
	ReadTransferCount   uint64
	WriteTransferCount  uint64
	OtherTransferCount  uint64
}

type jobObjectExtendedLimitInformation struct {
	BasicLimitInformation jobObjectBasicLimitInformation
	IoInfo                 ioCounters
	ProcessMemoryLimit     uintptr
	JobMemoryLimit         uintptr
	PeakProcessMemoryLimit uintptr
	PeakJobMemoryLimit     uintptr
}

var (
	harnessJobMu sync.Mutex
	harnessJobs  = make(map[int]uintptr)
)

func setProcGroup(cmd *exec.Cmd) {}

func postStartProcessGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil || cmd.Process.Pid <= 0 {
		return
	}
	r1, _, _ := procCreateJobObjectW.Call(0, 0)
	if r1 == 0 {
		return
	}
	jobHandle := r1
	info := jobObjectExtendedLimitInformation{
		BasicLimitInformation: jobObjectBasicLimitInformation{
			LimitFlags: jobObjectLimitKillOnJobClose,
		},
	}
	procSetInformationJobObject.Call(
		jobHandle,
		jobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		unsafe.Sizeof(info),
	)
	procAssignProcessToJobObject.Call(jobHandle, uintptr(syscall.Handle(cmd.Process.Pid)))

	harnessJobMu.Lock()
	harnessJobs[cmd.Process.Pid] = jobHandle
	harnessJobMu.Unlock()
}

func stopProcessGroup(cmd *exec.Cmd) {
	killProcessGroup(cmd)
}

func killProcessGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid
	if pid <= 0 {
		return
	}
	harnessJobMu.Lock()
	jobHandle, ok := harnessJobs[pid]
	delete(harnessJobs, pid)
	harnessJobMu.Unlock()

	if ok && jobHandle != 0 {
		procTerminateJobObject.Call(jobHandle, 1)
		procCloseHandle.Call(jobHandle)
	}
	_ = exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(pid)).Run()
	_ = cmd.Process.Kill()
}
