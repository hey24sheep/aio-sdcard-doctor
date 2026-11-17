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
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/app"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/drives"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/elevate"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/job"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/preflight"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/rescue"
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

// IO is where the CLI talks; TTY means a person can answer questions. Look (nil for scripts,
// pipes and tests) is how a person's terminal is written to.
type IO struct {
	In       io.Reader
	Out, Err io.Writer
	TTY      bool
	Look     *Look
}

// StdIO is the process's terminal.
func StdIO() IO {
	tty := term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
	return IO{In: os.Stdin, Out: os.Stdout, Err: os.Stderr, TTY: tty, Look: newLook(tty)}
}

// Version is set by main.
var Version = "dev"

// exitError carries an exit code out of a cobra command.
type exitError struct {
	code   int
	msg    string
	stands bool // the run ended on its own terms: its outcome is shown even after Ctrl+C
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
	return exitCode(ctx, root.ExecuteContext(ctx), io)
}

// exitCode maps a command's outcome to the process exit code and prints its message.
func exitCode(ctx context.Context, err error, io IO) int {
	var ee *exitError
	switch {
	case err == nil:
		return ExitOK
	case ctx.Err() != nil && !stands(err): // Ctrl+C, whatever was being asked or done (a menu, the admin question)
		fmt.Fprintln(io.Err, styleOf(io).status("risk", "Interrupted.")+" Run the same command again to resume.")
		return ExitInterrupted
	case errors.Is(err, errNoAnswer): // the terminal closed at a question: nothing was started
		fmt.Fprintln(io.Err, err)
		return ExitUsage
	case errors.As(err, &ee):
		if ee.msg != "" {
			fmt.Fprintln(io.Err, styleOf(io).exitMessage(ee.code, cleanLines(ee.msg))) // messages can name the card's files
		}
		return ee.code
	default:
		fmt.Fprintln(io.Err, styleOf(io).status("fail", "Error:"), cleanLines(err.Error()))
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
		Use:   "aio-sdcard-doctor",
		Short: "Get your photos off a failing SD card, safely.",
		Long: "Scan, copy (rescue), image and verify SD cards and USB drives. The card is only ever read, " +
			"except by fix (Card tools), which asks you to type the card's label and size first.",
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
	root.PersistentPreRun = func(*cobra.Command, []string) {
		if g.plain && io.Look != nil { // --plain: the plain lines, for screen readers and logs
			io.Look.off = true
		}
	}
	cobra.EnableCommandSorting = false // the commands in the order of the task: scan, rescue, image, verify…
	root.SetUsageTemplate(strings.Replace(root.UsageTemplate(), "Available Commands:", "Commands:", 1))
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
		if d.FS == app.KindNotResponding {
			tags = append(tags, "Not responding")
		} else if d.RAW {
			tags = append(tags, "Unreadable file system")
		}
		fmt.Fprintf(io.Out, "  %-12s %-16s %-6s %10s  %s\n", cleanText(d.Root), cleanText(d.Label), cleanText(d.FS), preflight.GB(int64(d.Total)), strings.Join(tags, ", "))
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
			fmt.Fprintf(io.Out, "Warning (accepted with --yes): %s — %s\n", cleanText(is.Title), cleanText(is.Detail))
		case is.Severity == job.Warn:
			if c := pr.Ask(ctx, is); c != job.Continue {
				if !io.TTY && !g.yes {
					return exit(ExitError, "Stopped: %s\n  %s\n  (non-interactive: add --yes to accept warnings)", is.Title, is.Detail)
				}
				return exit(ExitError, "Stopped: %s", is.Title)
			}
			if !io.TTY { // accepted by --yes: still shown, never silent
				fmt.Fprintf(io.Out, "Warning (accepted with --yes): %s — %s\n", cleanText(is.Title), cleanText(is.Detail))
			}
		default:
			fmt.Fprintf(io.Out, "%s %s", styleOf(io).dim("Note:"), cleanText(is.Title))
			if is.Detail != "" {
				fmt.Fprintf(io.Out, " — %s", cleanText(is.Detail))
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
	if err != nil && (len(res.Counters) > 0 || slices.ContainsFunc(res.Reports, func(r string) bool { return r != "" })) {
		printResultOf(io, res, true) // what was done before it stopped, and where its report is
	}
	// Ctrl+C during a card tool lets the system tool finish what it started: what it then reports
	// is the outcome (only the health test, our own, stops at a block and continues next time)
	toolEnded := a.Name() == "fix" && !errors.Is(err, context.Canceled)
	if err != nil {
		if ctx.Err() != nil && !toolEnded {
			return ctx.Err()
		}
		if errors.Is(err, rescue.ErrImageInstead) { // the person's choice: say how to make that image
			if h := imageInsteadHint(a); h != "" {
				return final(exit(ExitError, "%v\n%s", err, h))
			}
			return final(exit(ExitError, "%v", err))
		}
		if !io.TTY && !g.yes && a.Name() == "rescue" { // the hint is about rescue's bad-area questions
			return final(exit(ExitError, "%v\n  (non-interactive: add --yes to let it skip bad areas and continue on its own)", err))
		}
		return final(exit(ExitError, "%v", err))
	}
	printResult(io, res)
	if !res.Clean {
		return final(exit(ExitPartial, ""))
	}
	return nil
}

// final marks a run's own outcome: it stands even when Ctrl+C was pressed while it ended.
func final(err error) error {
	if ee, ok := err.(*exitError); ok {
		ee.stands = true
	}
	return err
}

func stands(err error) bool {
	var ee *exitError
	return errors.As(err, &ee) && ee.stands
}

// confirm shows what the action will do and what the user will get, and asks once. A script
// confirms with --yes; without it nothing starts.
func confirm(ctx context.Context, io IO, g *globals, plan job.Plan) error {
	st := styleOf(io)
	fmt.Fprintln(io.Out, "\n"+st.head("Please check before it starts:"))
	summary := cleanAll(plan.Summary) // the summary names the card and its folders
	if st.on() {
		summary = st.kvLines(summary)
	} else {
		for i, l := range summary {
			summary[i] = "  " + l
		}
	}
	for _, l := range summary {
		fmt.Fprintln(io.Out, l)
	}
	if len(plan.Result) > 0 {
		fmt.Fprintln(io.Out, st.head("You'll get:"))
		for _, l := range plan.Result {
			fmt.Fprintln(io.Out, "  "+cleanText(l))
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

// imageInsteadHint is the command that makes the image the person chose instead of the copy:
// of the whole disk the card is (not one partition of it, nor a folder on it), next to the copy
// so far. No trailing \ on the card: "E:\" would escape the closing quote in a Windows command line.
func imageInsteadHint(a job.Action) string {
	r, ok := a.(*rescue.Rescue)
	if !ok {
		return ""
	}
	if st, err := os.Stat(r.From); err == nil && st.Mode().IsRegular() {
		return "" // an image file is already an image
	}
	from := wholeDiskOf(r.From)
	if len(from) > 1 {
		from = strings.TrimRight(from, `\/`)
	}
	return fmt.Sprintf(`Next: aio-sdcard-doctor image --from "%s" --to "%s"`, from, filepath.Join(r.Dest, "card.img"))
}

// wholeDiskOf is the physical disk a card's volume, volume device or folder is on (from itself
// when that isn't known).
func wholeDiskOf(from string) string {
	disk := func(id string) string {
		if runtime.GOOS == "windows" {
			return `\\.\` + id
		}
		return "/dev/" + id
	}
	isDev := strings.HasPrefix(from, `\\.\`) || strings.HasPrefix(from, "/dev/")
	if isDev {
		if ds, err := drives.Lister(); err == nil {
			for _, d := range ds {
				if d.PhysicalID != "" && d.Device != "" && strings.EqualFold(strings.TrimRight(d.Device, `\/`), strings.TrimRight(from, `\/`)) {
					return disk(d.PhysicalID)
				}
			}
		}
		return from
	}
	if d, err := drives.Lookup(from); err == nil && d.PhysicalID != "" {
		return disk(d.PhysicalID)
	}
	return from
}

func printResult(io IO, res job.Result) { printResultOf(io, res, false) }

// printResultOf prints a run's counts and reports. On a person's terminal it starts with the
// verdict (symbol, word, colour) and lists the good news first.
func printResultOf(io IO, res job.Result, stopped bool) {
	if st := styleOf(io); st.on() {
		verdict := st.status("safe", "Done")
		switch {
		case stopped:
			verdict = st.status("fail", "Stopped")
		case !res.Clean:
			verdict = st.status("risk", "Done, with problems")
		}
		fmt.Fprintln(io.Out, "\n"+st.head("Result")+"  "+verdict)
		for _, l := range st.counterLines(res.Counters) {
			fmt.Fprintln(io.Out, l)
		}
		for _, r := range res.Reports {
			if r != "" {
				fmt.Fprintln(io.Out, st.dim("Report:"), cleanText(r)) // the report folder can carry the card's label
			}
		}
		return
	}
	var keys []string
	for k := range res.Counters {
		keys = append(keys, k)
	}
	sortStrings(keys)
	fmt.Fprintln(io.Out, "\nResult:")
	for _, k := range keys {
		fmt.Fprintf(io.Out, "  %-22s %8d\n", cleanText(k), res.Counters[k])
	}
	for _, r := range res.Reports {
		if r != "" {
			fmt.Fprintln(io.Out, "Report:", cleanText(r)) // the report folder can carry the card's label
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

func cleanAll(ls []string) []string {
	out := make([]string, len(ls))
	for i, l := range ls {
		out[i] = cleanText(l)
	}
	return out
}
