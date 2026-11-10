package job

import (
	"os"
	"os/exec"
	"strconv"
)

// inhibitOS runs the system's caffeinate, tied to our pid so it also ends if we crash.
func inhibitOS(string) (func(), error) {
	cmd := exec.Command("/usr/bin/caffeinate", "-i", "-w", strconv.Itoa(os.Getpid()))
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return func() { cmd.Process.Kill(); cmd.Wait() }, nil
}
