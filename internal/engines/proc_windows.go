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
	procOpenProcess              = kernel32.NewProc("OpenProcess")
)

const (
	jobObjectExtendedLimitInformationClass = 9
	jobObjectLimitKillOnJobClose           = 0x2000
	processSetQuota                        = 0x0100
	processTerminate                       = 0x0001
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
	BasicLimitInformation  jobObjectBasicLimitInformation
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

// SetProcGroup configures cmd to run in its own job object on Windows.
func SetProcGroup(cmd *exec.Cmd) {}

// PostStartProcessGroup assigns the started process to a Windows Job Object with KILL_ON_JOB_CLOSE.
func PostStartProcessGroup(cmd *exec.Cmd) {
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
		uintptr(jobObjectExtendedLimitInformationClass),
		uintptr(unsafe.Pointer(&info)),
		unsafe.Sizeof(info),
	)
	rProc, _, _ := procOpenProcess.Call(uintptr(processSetQuota|processTerminate), 0, uintptr(cmd.Process.Pid))
	if rProc != 0 {
		hProc := rProc
		procAssignProcessToJobObject.Call(jobHandle, hProc)
		procCloseHandle.Call(hProc)
	}

	harnessJobMu.Lock()
	harnessJobs[cmd.Process.Pid] = jobHandle
	harnessJobMu.Unlock()
}

// StopProcessGroup terminates the Windows Job Object and process tree.
func StopProcessGroup(cmd *exec.Cmd) {
	KillProcessGroup(cmd)
}

// KillProcessGroup forcefully terminates the process and job object on Windows.
func KillProcessGroup(cmd *exec.Cmd) {
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

func setProcGroup(cmd *exec.Cmd)          { SetProcGroup(cmd) }
func postStartProcessGroup(cmd *exec.Cmd) { PostStartProcessGroup(cmd) }
func stopProcessGroup(cmd *exec.Cmd)      { StopProcessGroup(cmd) }
func killProcessGroup(cmd *exec.Cmd)      { KillProcessGroup(cmd) }
