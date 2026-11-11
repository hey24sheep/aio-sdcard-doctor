package fix

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/drives"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/elevate"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/job"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/preflight"
)

// Fix is the action: one operation on one card, after Gate. The front end asks for the typed
// confirmation (Confirm) after the user has seen the plan.
type Fix struct {
	Op         string
	Target     Target
	Verified   bool
	AllowFixed bool
	NewLabel   string
	Replug     bool
	NewTest    bool // health test: forget a stopped test and start over
	BinaryDisk string
	Out        io.Writer // the OS tool's output

	// Seams.
	GOOS        string
	Admin       func() bool
	StoreDir    string                          // health-test progress (default: the user's config folder)
	ListDevices func() ([]drives.Device, error) // to find the card again after a replug
}

func (f *Fix) Name() string { return "fix" }

func (f *Fix) defaults() {
	if f.GOOS == "" {
		f.GOOS = runtime.GOOS
	}
	if f.Admin == nil {
		f.Admin = elevate.IsAdmin
	}
	if f.Out == nil {
		f.Out = io.Discard
	}
	if f.ListDevices == nil {
		f.ListDevices = drives.ListDevices
	}
	if f.StoreDir == "" {
		dir, err := os.UserConfigDir()
		if err != nil {
			dir = os.TempDir()
		}
		f.StoreDir = filepath.Join(dir, "aio-sdcard-doctor")
	}
}

var capName = map[string]string{OpCheck: "Check", OpRepair: "Repair", OpFormat: "Format", OpHealth: "Health test"}

func block(kind, title, detail string) job.Issue {
	return job.Issue{Kind: kind, Severity: job.Block, Title: title, Detail: detail, Choices: []job.Choice{job.Stop}, Default: job.Stop}
}

// Plan checks the operation is available and allowed, and says exactly what will happen.
func (f *Fix) Plan(ctx context.Context, env job.Env) (job.Plan, error) {
	f.defaults()
	var p job.Plan
	t := f.Target
	p.Caps = Available(f.GOOS, t, t.Vol.FS)
	for _, c := range p.Caps {
		if c.Name == capName[f.Op] && !c.Enabled {
			p.Issues = append(p.Issues, block("not-available", c.Name+" isn't available for this card", c.Reason))
			return p, nil
		}
	}
	warns, err := Gate(t, GateInput{Op: f.Op, Verified: f.Verified, AllowFixed: f.AllowFixed, Admin: f.Admin(), BinaryDisk: f.BinaryDisk})
	var ref *Refused
	switch {
	case errors.As(err, &ref):
		p.Issues = append(p.Issues, block("refused", "Fix won't touch this disk", ref.Reason))
		return p, nil
	case errors.Is(err, ErrNeedsAdmin):
		p.NeedsAdmin = true
		p.Issues = append(p.Issues, job.Issue{Kind: "needs-admin", Severity: job.Block,
			Title: "Fixing a card needs administrator rights", Detail: elevate.Instruction(nil),
			Choices: []job.Choice{job.Elevate, job.Stop}, Default: job.Elevate})
		return p, nil
	case err != nil:
		return p, err
	}
	for _, w := range warns {
		p.Issues = append(p.Issues, job.Issue{Kind: "not-verified", Severity: job.Warn, Title: "No verified rescue of this card",
			Detail: w, Choices: []job.Choice{job.Continue, job.Stop}, Default: job.Stop})
	}
	var argv []string
	if f.Op != OpHealth {
		if argv, err = Command(f.GOOS, f.Op, t, f.NewLabel); err != nil {
			p.Issues = append(p.Issues, block("no-command", "This can't be done here", err.Error()))
			return p, nil
		}
	}
	var vols []string
	for _, v := range t.Disk.Volumes {
		vols = append(vols, strings.TrimSpace(fmt.Sprintf("%s %s %s", v.Root, v.Label, v.FS)))
	}
	if len(vols) == 0 {
		vols = []string{"no mounted volume"}
	}
	p.Summary = []string{
		fmt.Sprintf("Card: %s, %s (%s) — volumes: %s", nonEmpty(t.Disk.Model, "card"), SizeText(t), nonEmpty(t.Disk.Path, t.Vol.Root), strings.Join(vols, "; ")),
	}
	switch f.Op {
	case OpCheck:
		p.Summary = append(p.Summary, "Do: check the file system for errors, changing nothing ("+strings.Join(argv, " ")+")")
		p.Result = []string{"A list of the problems found, if any; Repair fixes them"}
	case OpRepair:
		p.Summary = append(p.Summary, "Do: repair the file system with your OS's own tool ("+strings.Join(argv, " ")+")",
			"A repair can move damaged files to FOUND.000 / LOST.DIR or remove them")
		p.Result = []string{"The file system repaired; run Rescue again to see what is readable now"}
	case OpFormat:
		p.Summary = append(p.Summary, fmt.Sprintf("Do: ERASE EVERYTHING on %s and make a new %s file system (%s)",
			nonEmpty(strings.TrimSpace(t.Vol.Root+" "+t.Vol.Label), nonEmpty(t.Vol.Device, "the card")), strings.ToUpper(newFS(t)), strings.Join(argv, " ")))
		p.Result = []string{"An empty card, ready for your camera or phone (format it there too if it asks)"}
	case OpHealth:
		p.Summary = append(p.Summary, fmt.Sprintf("Do: ERASE EVERYTHING — write a test pattern over all %s, then read it back", SizeText(t)))
		if f.Replug {
			p.Summary = append(p.Summary, "Halfway you'll be asked to unplug and reinsert the card (don't format it when asked)")
		}
		p.Result = []string{"Bad areas found and the card's real capacity (fake cards claim more than they hold)",
			"The card is left without a partition or file system: afterwards, " + FormatElsewhere(f.GOOS),
			"Stop any time; running the same command again continues where it stopped"}
	}
	p.Summary = append(p.Summary, fmt.Sprintf("To confirm you'll type the label %q and the size %q", ConfirmLabel(t), SizeText(t)))
	return p, nil
}

func nonEmpty(s, alt string) string {
	if s == "" {
		return alt
	}
	return s
}

// inUse recognises "close programs using the card" failures of the OS tools.
func inUse(out string) bool {
	o := strings.ToLower(out)
	for _, s := range []string{"in use", "cannot lock", "can't lock", "resource busy", "is busy", "unable to unmount", "couldn't unmount"} {
		if strings.Contains(o, s) {
			return true
		}
	}
	return false
}

// lockKey is the per-card lock key rescue and imaging use for this card: its file-system serial,
// or its disk (size and physical id) when it has none.
func (f *Fix) lockKey() string {
	if drives.NormalizeSerial(f.Target.Vol.ID.FSSerial) != "" {
		return f.Target.Vol.ID.LockKey()
	}
	return drives.Identity{Size: uint64(f.Target.Disk.Size), PhysicalID: f.Target.Disk.PhysicalID}.LockKey()
}

// Run performs the operation.
func (f *Fix) Run(ctx context.Context, env job.Env, p job.Plan) (job.Result, error) {
	f.defaults()
	res := job.Result{Counters: map[string]int{}, Clean: true}
	// one run per card (R14): never repair, format or test a card another window is reading
	unlock, err := job.LockSource(f.lockKey())
	if err != nil {
		return res, err
	}
	defer unlock()
	if f.Op == OpHealth {
		return f.health(ctx, env)
	}
	argv, err := Command(f.GOOS, f.Op, f.Target, f.NewLabel)
	if err != nil {
		return res, err
	}
	for try := 0; ; try++ {
		var buf bytes.Buffer
		err := Exec(ctx, argv, io.MultiWriter(f.Out, &buf))
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		code := 0
		if err != nil {
			var ec interface{ ExitCode() int }
			if !errors.As(err, &ec) {
				return res, fmt.Errorf("couldn't run %s: %w", argv[0], err) // not found, no rights, ...
			}
			code = ec.ExitCode()
		}
		outcome := interpret(argv[0], f.Op, code)
		if outcome == "fixed" && f.Op == OpRepair && argv[0] != "chkdsk" {
			outcome = f.recheck(ctx, buf.String())
		}
		if outcome != "ok" && outcome != "fixed" && inUse(buf.String()) && try < 5 {
			c := env.Prompt.Ask(ctx, job.Issue{Kind: "volume-in-use", Severity: job.Warn, Title: "Close programs using the card",
				Detail:  "Close Explorer/Finder windows, photo apps and anything else open on the card, then retry.",
				Choices: []job.Choice{job.Retry, job.Stop}, Default: job.Stop})
			if c == job.Retry {
				continue
			}
		}
		switch outcome {
		case "ok":
			res.Counters["DONE"] = 1
			return res, nil
		case "fixed":
			res.Counters["ERRORS-FIXED"] = 1
			return res, nil
		case "problems":
			res.Counters["PROBLEMS-FOUND"] = 1
			res.Clean = false
			return res, nil
		}
		return res, fmt.Errorf("%s didn't finish (exit code %d): %s", strings.Join(argv, " "), code, lastLine(buf.String()))
	}
}

// interpret reads an OS tool's exit code: chkdsk 0 none / 1 fixed / 2 cleanup / 3 couldn't
// fix (or, without /f, errors found); fsck bit 1 corrected, 2 reboot, 4 left uncorrected,
// 8+ operational error; diskutil, format and mkfs: 0 or failure.
func interpret(tool, op string, code int) string {
	switch {
	case tool == "chkdsk":
		switch code {
		case 0, 2:
			return "ok"
		case 1:
			if op == OpCheck {
				return "problems"
			}
			return "fixed"
		case 3:
			if op == OpCheck {
				return "problems"
			}
		}
		return "failed"
	case strings.HasPrefix(tool, "fsck") || tool == "e2fsck":
		fat := tool == "fsck.vfat" || tool == "fsck.fat" || tool == "fsck.msdos"
		switch {
		case code >= 8, fat && code == 2: // fsck.fat 2: usage error, the card wasn't touched
			return "failed"
		case code&2 != 0 && op != OpCheck: // 2: "reboot" for e2fsck, usage for fsck.fat behind the fsck wrapper
			return "fixed"
		case code&4 != 0:
			if op == OpCheck {
				return "problems"
			}
			return "failed"
		case code&1 != 0:
			if op == OpCheck {
				return "problems"
			}
			return "fixed"
		}
		return "ok"
	}
	if code == 0 {
		return "ok"
	}
	return "failed"
}

// recheck runs a check-only pass after an fsck repair said "corrected": fsck.fat exits 1 for a
// fatal error too (an unreadable boot sector), so only a clean check makes the repair "fixed".
func (f *Fix) recheck(ctx context.Context, repairOut string) string {
	argv, err := Command(f.GOOS, OpCheck, f.Target, "")
	if err != nil {
		return "fixed"
	}
	var buf bytes.Buffer
	if err := Exec(ctx, argv, io.MultiWriter(f.Out, &buf)); err != nil {
		fmt.Fprintln(f.Out, "A check after the repair still finds problems ("+err.Error()+"); the repair said:", lastLine(repairOut))
		return "failed"
	}
	return "fixed"
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

func (f *Fix) health(ctx context.Context, env job.Env) (job.Result, error) {
	res := job.Result{Counters: map[string]int{}}
	t := f.Target
	rw, c, err := OpenRaw(t)
	if err != nil {
		return res, err
	}
	closeOnce := func() { // the card handle is swapped after a replug; close whatever is open, once
		if c != nil {
			c.Close()
			c = nil
		}
	}
	defer closeOnce()
	if err := os.MkdirAll(f.StoreDir, 0o700); err != nil {
		return res, err
	}
	key := strings.NewReplacer(`\`, "_", "/", "_", ":", "_", ".", "_").Replace(t.Disk.PhysicalID + "_" + fmt.Sprint(t.Size()))
	opt := HealthOptions{StorePath: filepath.Join(f.StoreDir, "health_"+key+".db"), Replug: f.Replug}
	opt.Restart = f.NewTest
	opt.Reopen = func() (RW, error) {
		closeOnce()
		nt, err := f.findAgain()
		if err != nil {
			return nil, err
		}
		r, nc, err := OpenRaw(nt)
		if err != nil {
			return nil, err
		}
		c = nc
		return r, nil
	}
	rep, err := HealthTest(ctx, rw, t.Size(), env, opt)
	if err != nil {
		if t.Disk.Path != "" && strings.Contains(err.Error(), "run the same command again") {
			// the test wrote over the partition table: the drive letter or /dev/sdb1 is gone
			err = fmt.Errorf("%w (its drive letter or mount point may be gone by now — continue with --drive %s)", err, t.Disk.Path)
		}
		return res, err
	}
	var bad int64
	for _, r := range rep.Bad {
		bad += r.N
	}
	res.Counters["BAD-MB"] = int((bad + 1<<20 - 1) >> 20)
	res.Counters["REAL-CAPACITY-MB"] = int(rep.RealCapacity >> 20)
	res.Clean = len(rep.Bad) == 0 && !rep.Fake
	switch {
	case rep.Fake:
		res.Counters["FAKE-CAPACITY"] = 1
		env.Log("warn", fmt.Sprintf("FAKE CARD: it claims %s but holds about %s. Don't trust it with photos.", SizeText(t), preflight.GB(rep.RealCapacity)))
	case len(rep.Bad) > 0:
		env.Log("warn", fmt.Sprintf("%d bad areas (%s). Replace this card.", len(rep.Bad), preflight.GB(bad)))
	default:
		env.Log("info", "The whole card held the test pattern: no bad areas, real capacity as claimed. Before use, "+FormatElsewhere(f.GOOS)+".")
	}
	return res, nil
}

// findAgain finds the card after a replug: same serial (or model) and size.
func (f *Fix) findAgain() (Target, error) {
	// the OS takes a few seconds to list a card plugged back in: wait for it, within reason
	for start := time.Now(); ; time.Sleep(time.Second) {
		ds, err := f.ListDevices()
		if err != nil {
			return Target{}, err
		}
		for _, d := range ds {
			if d.Size == f.Target.Disk.Size && (f.Target.Disk.Serial != "" && d.Serial == f.Target.Disk.Serial ||
				f.Target.Disk.Serial == "" && d.Model == f.Target.Disk.Model && d.PhysicalID == f.Target.Disk.PhysicalID) {
				return Target{Disk: d}, nil
			}
		}
		if time.Since(start) >= findAgainWait {
			return Target{}, errors.New("the card isn't back yet — plug it in and run the same command again to continue")
		}
	}
}

// findAgainWait is how long a re-plugged card may take to show up (a variable for tests).
var findAgainWait = 30 * time.Second
