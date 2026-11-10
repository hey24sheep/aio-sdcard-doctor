package worker

import "net/rpc"

// startProc launches the worker with its RPC pipe and stderr drain, under OS memory limits.
func startProc() (*proc, error) {
	cmd := Command()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	errs := newRing()
	cmd.Stderr = errs // exec copies stderr in its own goroutine, always drained
	prepare(cmd)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &proc{cmd: cmd, stderr: errs, done: make(chan struct{}), pid: cmd.Process.Pid,
		killFn: cmd.Process.Kill}
	p.limits = afterStart(cmd, p)
	p.rpc = rpc.NewClient(pipe{ReadCloser: stdout, w: stdin})
	return p, nil
}
