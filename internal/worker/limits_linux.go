package worker

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

// prepare makes the kernel kill the worker if this process dies (R9).
func prepare(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}

// afterStart caps the worker's address space at 1 GiB. Skipped under the race detector, whose
// shadow memory needs far more address space.
func afterStart(cmd *exec.Cmd, _ *proc) func() {
	if !raceEnabled {
		lim := unix.Rlimit{Cur: hardMemLimit, Max: hardMemLimit}
		unix.Prlimit(cmd.Process.Pid, unix.RLIMIT_AS, &lim, nil)
	}
	return func() {}
}

func redirectStdout() {}
