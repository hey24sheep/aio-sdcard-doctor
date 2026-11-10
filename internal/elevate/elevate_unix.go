//go:build !windows

package elevate

import (
	"context"
	"errors"
	"os"
	"os/exec"

	"golang.org/x/term"
)

// IsAdmin reports whether we run as root.
func IsAdmin() bool { return os.Geteuid() == 0 }

// Relaunch re-runs the program with args under sudo, waits, and propagates its exit code.
func Relaunch(ctx context.Context, args []string) error {
	return relaunchUnix(ctx, args, unixDeps{
		isTTY:   func() bool { return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd())) },
		hasSudo: func() bool { _, err := exec.LookPath("sudo"); return err == nil },
		exe:     os.Executable,
		run:     runAttached,
	})
}

// runAttached runs argv with our terminal attached and returns its exit code.
func runAttached(ctx context.Context, argv []string) int {
	c := exec.CommandContext(ctx, argv[0], argv[1:]...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := c.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		return 1
	}
	return 0
}
