package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/app"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/gui"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/tui"
)

// launchMode decides what running the program with no command opens: double-clicked (it owns a
// fresh console window) → the GUI; typed in a terminal → the TUI; --plain or a script → the
// drive list and usage.
func launchMode(doubleClick, tty, plain bool) string {
	switch {
	case doubleClick:
		return "gui"
	case tty && !plain:
		return "tui"
	}
	return "usage"
}

// Seams (tests never start a real front end).
var (
	doubleClicked = isDoubleClicked
	runTUI        = func(io IO, opt tui.Options) error {
		s := app.NewSession(context.Background(), appDeps("tui"))
		defer s.Close()
		return tui.Run(s, opt)
	}
	runGUI = startGUI
)

// guiLockDir is the per-user folder for gui.lock.
func guiLockDir() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "aio-sdcard-doctor")
}

// startGUI reopens a running GUI of this user if there is one (a closed window mid-job), else
// starts one. Ctrl+C in the console stops it.
func startGUI(ctx context.Context, io IO, noOpen bool, port int) error {
	lock := guiLockDir()
	if url, ok := gui.Reopen(lock, isAdmin()); ok && port == 0 {
		fmt.Fprintf(io.Out, "SD card doctor is already running — reopened at %s\n", url)
		if !noOpen {
			if err := openWindow(url); err != nil {
				fmt.Fprintf(io.Err, "Couldn't open a window (%v) — open the link above in your browser.\n", err)
			}
		}
		return nil
	}
	s := app.NewSession(ctx, appDeps("gui"))
	defer s.Close()
	srv, err := gui.Start(ctx, s, gui.Options{Port: port, LockDir: lock, Admin: isAdmin()})
	if err != nil {
		return exit(ExitError, "gui: %v", err)
	}
	url := srv.URL()
	fmt.Fprintf(io.Out, "SD card doctor is open at %s\n", url)
	fmt.Fprintln(io.Out, "That link works once, on this computer only. Close this window (or press Ctrl+C) to quit.")
	if !noOpen {
		if err := openWindow(url); err != nil {
			fmt.Fprintf(io.Err, "Couldn't open a window (%v) — open the link above in your browser.\n", err)
		}
	}
	select {
	case <-srv.Done():
	case <-ctx.Done(): // Ctrl+C: a running job stops at the end of its block
		srv.Close()
	}
	return nil
}

func tuiCmd(io IO) *cobra.Command {
	var opt tui.Options
	c := &cobra.Command{
		Use:   "tui",
		Short: "Open the keyboard-driven terminal interface (screen readers: use --plain commands instead)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := runTUI(io, opt); err != nil {
				return exit(ExitError, "tui: %v", err)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&opt.ASCII, "ascii", false, "plain ASCII symbols (no ✔ ▲ ✖ █)")
	c.Flags().BoolVar(&opt.NoAnimation, "no-animation", false, "update without animation")
	c.Flags().BoolVar(&opt.NoBell, "no-bell", false, "don't ring the terminal bell when done or when a decision is needed")
	return c
}
