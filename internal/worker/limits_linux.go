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

// afterStart caps the worker's writable private memory at 1 GiB (RLIMIT_DATA, Linux ≥ 4.7).
// Not RLIMIT_AS: the Go runtime reserves (PROT_NONE) far more address space than it uses, so an
// address-space cap kills a worker that has barely allocated anything. Skipped under the race
// detector, whose shadow memory is mapped writable.
func afterStart(cmd *exec.Cmd, _ *proc) func() {
	if !raceEnabled {
		lim := unix.Rlimit{Cur: hardMemLimit, Max: hardMemLimit}
		unix.Prlimit(cmd.Process.Pid, unix.RLIMIT_DATA, &lim, nil)
	}
	return func() {}
}

func redirectStdout() {}
