// Package elevate asks for Administrator/root only when an action needs raw device access.
// We never see a password: Windows shows its UAC prompt, unix runs sudo in the terminal.
package elevate

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"runtime"
	"strings"
)

// ErrDeclined means the user cancelled the prompt, sudo failed, or there is no terminal to ask in.
var ErrDeclined = errors.New("elevation declined")

// ExitError carries the elevated child's non-zero exit code.
type ExitError struct{ Code int }

func (e *ExitError) Error() string { return fmt.Sprintf("elevated run exited with code %d", e.Code) }

const exeName = "aio-sdcard-doctor"

// Instruction is the exact command a user can run themselves when we can't elevate.
func Instruction(args []string) string { return instruction(runtime.GOOS, args) }

func instruction(goos string, args []string) string {
	if goos == "windows" {
		return "Run as Administrator: " + strings.TrimSpace(exeName+" "+windowsParams(args))
	}
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = shQuote(a)
	}
	return strings.TrimSpace("sudo " + exeName + " " + strings.Join(q, " "))
}

var shSafe = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

func shQuote(s string) string {
	if shSafe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// windowsQuote quotes one argument the way CommandLineToArgvW reads it back.
func windowsQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\"") {
		return s
	}
	var b strings.Builder
	b.WriteByte('"')
	slashes := 0
	for _, r := range s {
		switch r {
		case '\\':
			slashes++
			continue
		case '"':
			b.WriteString(strings.Repeat(`\`, 2*slashes+1))
		default:
			b.WriteString(strings.Repeat(`\`, slashes))
		}
		slashes = 0
		b.WriteRune(r)
	}
	b.WriteString(strings.Repeat(`\`, 2*slashes))
	b.WriteByte('"')
	return b.String()
}

// WindowFlag tells an elevated Windows child that it runs in a console of its own, which closes
// when it exits: it waits for Enter so its result can be read. Main strips it before parsing.
const WindowFlag = "--elevated-window"

// childParams is the elevated Windows child's command line.
func childParams(args []string) string {
	return windowsParams(append(args[:len(args):len(args)], WindowFlag))
}

func windowsParams(args []string) string {
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = windowsQuote(a)
	}
	return strings.Join(q, " ")
}

// unixDeps are the seams relaunchUnix uses (fakes in tests).
type unixDeps struct {
	isTTY   func() bool
	hasSudo func() bool
	exe     func() (string, error)
	run     func(ctx context.Context, argv []string) int // exit code
}

// relaunchUnix re-runs us under sudo in the same terminal. `sudo -v` authenticates first, so an
// auth failure (ErrDeclined) is told apart from the child's own exit code.
func relaunchUnix(ctx context.Context, args []string, d unixDeps) error {
	if !d.isTTY() || !d.hasSudo() {
		return ErrDeclined
	}
	exe, err := d.exe()
	if err != nil {
		return err
	}
	if d.run(ctx, []string{"sudo", "-v"}) != 0 {
		return ErrDeclined
	}
	if code := d.run(ctx, append([]string{"sudo", "--", exe}, args...)); code != 0 {
		return &ExitError{Code: code}
	}
	return nil
}
