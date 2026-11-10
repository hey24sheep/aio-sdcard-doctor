package worker

import (
	"os"
	"os/exec"
	"unsafe"

	"golang.org/x/sys/windows"
)

func prepare(*exec.Cmd) {}

// afterStart puts the worker in a Job Object: 1 GiB process memory cap, and the worker is
// killed when the job handle closes — including when this process dies (R9).
func afterStart(cmd *exec.Cmd, p *proc) func() {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return func() {}
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_PROCESS_MEMORY | windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
		ProcessMemoryLimit: hardMemLimit,
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return func() {}
	}
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err == nil {
		windows.AssignProcessToJobObject(job, h)
		windows.CloseHandle(h)
	}
	return func() { windows.CloseHandle(job) }
}

// redirectStdout points the OS-level stdout handle at stderr too, for code that writes to the
// handle rather than os.Stdout.
func redirectStdout() {
	windows.SetStdHandle(windows.STD_OUTPUT_HANDLE, windows.Handle(os.Stderr.Fd()))
}
