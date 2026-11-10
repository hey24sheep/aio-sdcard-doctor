package worker

import (
	"os/exec"
	"strconv"
	"strings"
	"time"
)

func prepare(*exec.Cmd) {}

// afterStart samples the worker's resident memory every 2 s and kills it above 1 GiB (macOS
// has no per-process cap we can set from outside). The sampler stops when the worker ends.
func afterStart(cmd *exec.Cmd, p *proc) func() {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				out, err := exec.Command("/bin/ps", "-o", "rss=", "-p", strconv.Itoa(cmd.Process.Pid)).Output()
				if err != nil {
					return
				}
				if kb, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64); err == nil && kb*1024 > hardMemLimit {
					cmd.Process.Kill()
					return
				}
			}
		}
	}()
	return func() { close(stop); <-done }
}

func redirectStdout() {}
