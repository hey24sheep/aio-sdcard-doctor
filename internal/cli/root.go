// Package cli is the scriptable command line: scan, rescue, image and verify (recover, fix, tui
// and gui come with their milestones). It shares the job core with the front ends.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/drives"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/elevate"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/job"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/preflight"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/tui"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/worker"
)

// Exit codes.
const (
	ExitOK          = 0
	ExitError       = 1
	ExitUsage       = 2
	ExitPartial     = 3 // finished, but some files are PARTIAL / FAILED / not verified
	ExitAdmin       = 4
	ExitInterrupted = 130
)

// IO is where the CLI talks; TTY means a person can answer questions.
type IO struct {
	In       io.Reader
	Out, Err io.Writer
	TTY      bool
}

// StdIO is the process's terminal.
func StdIO() IO {
	return IO{In: os.Stdin, Out: os.Stdout, Err: os.Stderr,
		TTY: term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))}
}

// Version is set by main.
var Version = "dev"

// exitError carries an exit code out of a cobra command.
type exitError struct {
	code int
	msg  string
}

func (e *exitError) Error() string { return e.msg }

func exit(code int, format string, a ...any) error {
	return &exitError{code: code, msg: fmt.Sprintf(format, a...)}
}

// Seams for tests.
var (
	isAdmin  = elevate.IsAdmin
	relaunch = elevate.Relaunch
)

// Main runs the CLI and returns the process exit code.
func Main(args []string, io IO) (code int) {
	if len(args) > 0 && args[0] == "__source-worker" {
		return worker.Main()
	}
	if i := slices.Index(args, elevate.WindowFlag); i >= 0 { // an elevated child in its own window
		args = slices.Delete(slices.Clone(args), i, i+1)
		defer func() {
			if io.TTY {
				fmt.Fprintf(io.Err, "\nFinished (exit code %d). Press Enter to close this window.", code)
				readLine(io.In)
			}
		}()
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	root := newRoot(io)
	root.SetArgs(args)
	err := root.ExecuteContext(ctx)
	var ee *exitError
	switch {
	case err == nil:
		return ExitOK
	case ctx.Err() != nil && (errors.Is(err, errNoAnswer) || errors.Is(err, context.Canceled)):
		fmt.Fprintln(io.Err, "Interrupted. Run the same command again to resume.")
		return ExitInterrupted // Ctrl+C, even at a question
	case errors.Is(err, errNoAnswer): // the terminal closed at a question: nothing was started
		fmt.Fprintln(io.Err, err)
		return ExitUsage
	case errors.As(err, &ee):
		if ee.msg != "" {
			fmt.Fprintln(io.Err, ee.msg)
		}
		return ee.code
	case ctx.Err() != nil:
		fmt.Fprintln(io.Err, "Interrupted. Run the same command again to resume.")
		return ExitInterrupted
	default:
		fmt.Fprintln(io.Err, "Error:", err)
		if strings.Contains(err.Error(), "unknown") || strings.Contains(err.Error(), "flag") || strings.Contains(err.Error(), "argument") {
			return ExitUsage
		}
		return ExitError
	}
}

type globals struct {
	plain bool
	yes   bool
}

func newRoot(io IO) *cobra.Command {
	g := &globals{}
	root := &cobra.Command{
		Use:           "aio-sdcard-doctor",
		Short:         "Get your photos off a failing SD card, safely.",
		Long:          "Scan, copy (rescue), image and verify SD cards and USB drives. The card is only ever read.",
		Version:       Version,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			switch launchMode(doubleClicked(), io.TTY, g.plain) {
			case "gui":
				return runGUI(cmd.Context(), io, false, 0)
			case "tui":
				if err := runTUI(io, tui.Options{}); err != nil {
					return exit(ExitError, "%v", err)
				}
				return nil
			}
			listDrives(io)
			fmt.Fprintln(io.Out)
			return cmd.Usage()
		},
	}
	root.SetOut(io.Out)
	root.SetErr(io.Err)
	root.SetFlagErrorFunc(func(c *cobra.Command, err error) error { return exit(ExitUsage, "%v\n\n%s", err, c.UsageString()) })
	root.PersistentFlags().BoolVar(&g.plain, "plain", false, "plain line output (no interactive UI)")
	root.AddCommand(scanCmd(io, g), rescueCmd(io, g), imageCmd(io, g), verifyCmd(io, g), recoverCmd(io, g), fixCmd(io, g), guiCmd(io), tuiCmd(io), workerCmd())
	return root
}

// workerCmd is the hidden reader process.
func workerCmd() *cobra.Command {
	return &cobra.Command{Use: "__source-worker", Hidden: true, RunE: func(*cobra.Command, []string) error {
		if code := worker.Main(); code != 0 {
			return exit(code, "")
		}
		return nil
	}}
}

func listDrives(io IO) {
	ds, err := drives.List()
	if err != nil {
		return
	}
	fmt.Fprintln(io.Out, "Drives:")
	for _, d := range ds {
		var tags []string
		if d.Removable {
			tags = append(tags, "Removable")
		}
		if d.System {
			tags = append(tags, "System")
		}
		if d.Locked {
			tags = append(tags, "Locked")
		}
		if d.WriteProtected {
			tags = append(tags, "Write-protected")
		}
		if d.RAW {
			tags = append(tags, "Unreadable file system")
		}
		fmt.Fprintf(io.Out, "  %-12s %-16s %-6s %10s  %s\n", d.Root, d.Label, d.FS, preflight.GB(int64(d.Total)), strings.Join(tags, ", "))
	}
}

// prompter asks on a terminal; elsewhere it answers automatically (--yes accepts warnings).
func (g *globals) prompter(io IO) job.Prompter {
	if io.TTY {
		return &ttyPrompter{io: io}
	}
	return job.AutoPrompter{Yes: g.yes}
}

// runAction plans an action, then runs it with runPlanned.
func runAction(ctx context.Context, io IO, g *globals, a job.Action, adminArgs []string, onResume func(job.Choice)) error {
	env := g.env(io)
	plan, err := a.Plan(ctx, env)
	if err != nil {
		return exit(ExitError, "%v", err)
	}
	if closer, ok := a.(interface{ Close() error }); ok {
		defer closer.Close()
	}
	return runPlanned(ctx, io, g, a, env, plan, adminArgs, onResume)
}

func (g *globals) env(io IO) job.Env {
	out := &printer{io: io, plain: g.plain || !io.TTY}
	return job.Env{Prompt: g.prompter(io), Progress: out.progress, Log: out.log, Now: time.Now}
}

// runPlanned settles every pre-flight issue with the user, runs the action and maps the outcome
// to an exit code. onResume receives the answer to a "resume or start over" question.
// beforeRun (optional) runs after the confirmation and before the action (fix's typed label/size).
func runPlanned(ctx context.Context, io IO, g *globals, a job.Action, env job.Env, plan job.Plan, adminArgs []string, onResume func(job.Choice), beforeRun ...func() error) error {
	pr := env.Prompt
	needsAdmin := plan.NeedsAdmin && !isAdmin()
	for _, is := range plan.Issues {
		switch {
		case is.Kind == "needs-admin":
			needsAdmin = needsAdmin || is.Severity == job.Block || !isAdmin()
		case is.Severity == job.Block:
			return exit(ExitError, "Can't start: %s\n  %s", is.Title, is.Detail)
		case is.Kind == "resume":
			c := pr.Ask(ctx, is)
			if onResume != nil {
				onResume(c)
			}
		case is.Severity == job.Warn && !io.TTY && g.yes && slices.Contains(is.Choices, job.Continue):
			// a script's --yes accepts a warning even when its default (for a person) is Stop
			fmt.Fprintf(io.Out, "Warning (accepted with --yes): %s — %s\n", is.Title, is.Detail)
		case is.Severity == job.Warn:
			if c := pr.Ask(ctx, is); c != job.Continue {
				if !io.TTY && !g.yes {
					return exit(ExitError, "Stopped: %s\n  %s\n  (non-interactive: add --yes to accept warnings)", is.Title, is.Detail)
				}
				return exit(ExitError, "Stopped: %s", is.Title)
			}
			if !io.TTY { // accepted by --yes: still shown, never silent
				fmt.Fprintf(io.Out, "Warning (accepted with --yes): %s — %s\n", is.Title, is.Detail)
			}
		default:
			fmt.Fprintf(io.Out, "Note: %s", is.Title)
			if is.Detail != "" {
				fmt.Fprintf(io.Out, " — %s", is.Detail)
			}
			fmt.Fprintln(io.Out)
		}
	}
	if needsAdmin {
		return adminFlow(ctx, io, adminArgs)
	}
	if err := confirm(ctx, io, g, plan); err != nil {
		return err
	}
	for _, f := range beforeRun {
		if f == nil {
			continue
		}
		if err := f(); err != nil {
			return err
		}
	}
	res, err := a.Run(ctx, env, plan)
	if io.TTY && !g.plain {
		fmt.Fprintln(io.Out) // end the progress line
	}
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !io.TTY && !g.yes && a.Name() == "rescue" { // the hint is about rescue's bad-area questions
			return exit(ExitError, "%v\n  (non-interactive: add --yes to let it skip bad areas and continue on its own)", err)
		}
		return exit(ExitError, "%v", err)
	}
	printResult(io, res)
	if !res.Clean {
		return exit(ExitPartial, "")
	}
	return nil
}

// confirm shows what the action will do and what the user will get, and asks once. A script
// confirms with --yes; without it nothing starts.
func confirm(ctx context.Context, io IO, g *globals, plan job.Plan) error {
	fmt.Fprintln(io.Out, "\nPlease check before it starts:")
	for _, l := range plan.Summary {
		fmt.Fprintln(io.Out, "  "+l)
	}
	if len(plan.Result) > 0 {
		fmt.Fprintln(io.Out, "You'll get:")
		for _, l := range plan.Result {
			fmt.Fprintln(io.Out, "  "+l)
		}
	}
	if !io.TTY {
		if !g.yes {
			return exit(ExitError, "Not started: add --yes to confirm (the summary above is exactly what would happen).")
		}
		return nil
	}
	ok, err := yesNo(ctx, io, "Start now?", true)
	if err != nil {
		return exit(ExitUsage, "%v", err)
	}
	if !ok {
		return exit(ExitError, "Cancelled — nothing was started.")
	}
	return nil
}

// adminFlow restarts elevated (asking first on a terminal) or prints the command to run.
func adminFlow(ctx context.Context, io IO, args []string) error {
	instr := elevate.Instruction(args)
	if !io.TTY {
		return exit(ExitAdmin, "This needs administrator rights.\n  %s", instr)
	}
	fmt.Fprint(io.Err, "This needs administrator rights. Restart as Administrator/root? [Y/n] ")
	// no answer (EOF, Ctrl+C) is no: nothing is restarted for someone who didn't say yes
	if a, ok := readAnswer(ctx, io.In); !ok || a != "" && !strings.HasPrefix(strings.ToLower(a), "y") {
		return exit(ExitAdmin, "Not elevated.\n  %s", instr)
	}
	if runtime.GOOS == "windows" {
		fmt.Fprintln(io.Err, "Continuing in a new Administrator window — its result stays there until you press Enter.")
	}
	err := relaunch(ctx, args)
	var ee *elevate.ExitError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &ee):
		return exit(ee.Code, "")
	default:
		return exit(ExitAdmin, "Couldn't restart as administrator (%v).\n  %s", err, instr)
	}
}

func printResult(io IO, res job.Result) {
	var keys []string
	for k := range res.Counters {
		keys = append(keys, k)
	}
	sortStrings(keys)
	fmt.Fprintln(io.Out, "\nResult:")
	for _, k := range keys {
		fmt.Fprintf(io.Out, "  %-22s %8d\n", k, res.Counters[k])
	}
	for _, r := range res.Reports {
		if r != "" {
			fmt.Fprintln(io.Out, "Report:", r)
		}
	}
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
