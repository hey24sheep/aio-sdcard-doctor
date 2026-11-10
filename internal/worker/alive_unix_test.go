//go:build !windows

package worker

import "syscall"

// alive reports whether a process is still running (zombies count as gone).
func alive(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	var ws syscall.WaitStatus
	wpid, _ := syscall.Wait4(pid, &ws, syscall.WNOHANG, nil)
	return wpid == 0
}
