package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/drives"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/elevate"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/fix"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/imaging"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/job"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/preflight"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/profile"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/recovery"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/rescue"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/scan"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/verify"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/worker"
)

const stall = 30 * time.Second

// ProductionDeps wires the session to the real drives and actions. frontEnd ("gui" or "tui") is
// what the administrator instruction tells the person to run again.
func ProductionDeps(frontEnd string) Deps {
	return Deps{
		ListDrives:  drives.List,
		NewAction:   NewAction,
		Choices:     Choices,
		IsAdmin:     elevate.IsAdmin,
		Instruction: func(args []string) string { return elevate.Instruction(append([]string{frontEnd}, args...)) },
		DefaultDest: DefaultDest,
		Now:         time.Now,
		FixTools: func(from string) []job.Capability {
			t, err := fixTargetFn(from)
			if err != nil {
				return nil // the check page explains
			}
			return fix.Available(runtime.GOOS, t, "")
		},
	}
}

// DefaultDest is a folder next to where the program runs, named after the card and the day.
func DefaultDest(from string) string {
	label := "card"
	if d, err := drives.Lookup(from); err == nil && d.Label != "" {
		label = strings.Map(func(r rune) rune {
			if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
				return r
			}
			return '_'
		}, d.Label)
	}
	dir, err := os.UserHomeDir()
	if err != nil {
		dir, _ = os.Getwd()
	}
	return filepath.Join(dir, fmt.Sprintf("aio-sdcard-doctor-%s-%s", label, time.Now().Format("20060102")))
}

func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

func isDevice(p string) bool { return strings.HasPrefix(p, `\\.\`) || strings.HasPrefix(p, "/dev/") }

// rawVolume is the card's volume device (\\.\E:, /dev/sdb1) for reading it directly.
func rawVolume(from string) string {
	if isDevice(from) || isFile(from) {
		return from
	}
	if d, err := drives.Lookup(from); err == nil && d.Device != "" {
		return d.Device
	}
	return from
}

// wholeDisk is the card's physical disk (imaging).
func wholeDisk(from string) string {
	if isDevice(from) || isFile(from) {
		return from
	}
	d, err := drives.Lookup(from)
	if err != nil || d.PhysicalID == "" {
		return from
	}
	if runtime.GOOS == "windows" {
		return `\\.\` + d.PhysicalID
	}
	return "/dev/" + d.PhysicalID
}

// Choices scans the card (read-only) and counts what each backup choice would copy.
func Choices(ctx context.Context, from string) ([]ChoiceRow, error) {
	src, err := worker.Spawn(worker.Spec{From: from, Raw: isFile(from) || isDevice(from)}, stall)
	if err != nil {
		return nil, err
	}
	defer src.Close()
	inv, err := scan.Scan(ctx, src, profile.Detect(src))
	if err != nil {
		return nil, err
	}
	var out []ChoiceRow
	for _, p := range profile.Presets {
		n, b := rescue.Count(inv, rescue.SelectionFor(p))
		out = append(out, ChoiceRow{ID: p.ID, Title: p.Title, Desc: p.Desc, Files: n, Bytes: b})
	}
	return out, nil
}

// NewAction turns the user's choices into the action that does the work.
func NewAction(r Request) (job.Action, error) {
	switch r.Action {
	case "scan":
		return &scanAction{from: r.From}, nil
	case "rescue":
		p, ok := profile.PresetByID(r.Backup)
		if !ok {
			return nil, fmt.Errorf("choose what to copy")
		}
		s := rescue.SelectionFor(p)
		res := &rescue.Rescue{From: r.From, Raw: isFile(r.From) || isDevice(r.From), Dest: r.Dest, Compress: r.Compress,
			Types: s.Types, Apps: s.Apps, Exclude: s.Exclude, Backup: s.Name, SkipEmpty: r.SkipEmpty,
			Interactive: true, StallTimeout: stall, Fresh: r.Fresh}
		return res, nil
	case "image":
		return &imaging.Image{Device: wholeDisk(r.From), Out: r.Dest, Compress: r.Compress, StallTimeout: stall}, nil
	case "verify":
		return &verify.Verify{Manifest: r.Manifest, Target: r.Target, Restart: r.Fresh}, nil
	case "recover":
		rec := &recovery.Recover{From: rawVolume(r.From), Dest: r.Dest, Deep: r.Deep, Thorough: r.Thorough, List: r.List,
			Select: r.Select, Compress: r.Compress, StallTimeout: stall}
		if m := filepath.Join(r.Dest, "Reports", "manifest.json"); isFile(m) {
			rec.SkipRescued = m
		}
		return rec, nil
	case "fix":
		t, err := fixTargetFn(r.From)
		if err != nil {
			return nil, err
		}
		exe, _ := os.Executable()
		bin, _ := drives.Lookup(exe)
		return &fixAction{Fix: &fix.Fix{Op: r.FixOp, Target: t, NewLabel: r.NewLabel, Replug: true, NewTest: r.Fresh, BinaryDisk: bin.PhysicalID},
			from: r.From}, nil
	}
	return nil, fmt.Errorf("unknown action %q", r.Action)
}

// FixTarget finds the card's volume and its physical disk.
func FixTarget(from string) (fix.Target, error) {
	ds, err := drives.ListDevices()
	if err != nil {
		return fix.Target{}, err
	}
	if isDevice(from) {
		for _, d := range ds {
			if strings.EqualFold(d.Path, from) {
				t := fix.Target{Disk: d}
				if len(d.Volumes) > 0 {
					t.Vol = d.Volumes[0]
				}
				return t, nil
			}
		}
	}
	v, err := drives.Lookup(from)
	if err != nil {
		return fix.Target{}, err
	}
	for _, d := range ds {
		if d.PhysicalID != "" && d.PhysicalID == v.PhysicalID {
			return fix.Target{Vol: v, Disk: d}, nil
		}
	}
	return fix.Target{}, errors.New("couldn't find the disk this card is on — try again as administrator")
}

// fixAction adds the typed confirmation and a last look at the card right before it runs.
type fixAction struct {
	*fix.Fix
	from string
}

func (f *fixAction) ConfirmText() (string, string) {
	return fix.ConfirmLabel(f.Target), fix.SizeText(f.Target)
}

func (f *fixAction) Confirm(label, size string) error { return fix.Confirm(f.Target, label, size) }

// fixTargetFn is a seam (tests use a made-up card).
var fixTargetFn = FixTarget

// logWriter turns an OS tool's output into log lines the front ends show.
type logWriter struct {
	log  func(level, msg string)
	part string
}

func (w *logWriter) Write(b []byte) (int, error) {
	w.part += string(b)
	for {
		i := strings.IndexAny(w.part, "\r\n")
		if i < 0 {
			break
		}
		if line := strings.TrimSpace(w.part[:i]); line != "" {
			w.log("info", line)
		}
		w.part = w.part[i+1:]
	}
	return len(b), nil
}

func (f *fixAction) Run(ctx context.Context, env job.Env, p job.Plan) (job.Result, error) {
	if env.Log != nil {
		lw := &logWriter{log: env.Log}
		f.Fix.Out = lw // the tool's own words: what a check found, what a repair did
		defer func() {
			if s := strings.TrimSpace(lw.part); s != "" {
				env.Log("info", s)
			}
		}()
	}
	now, err := fixTargetFn(f.from)
	if err != nil || now.Disk.Path != f.Target.Disk.Path || now.Disk.Serial != f.Target.Disk.Serial ||
		now.Disk.Size != f.Target.Disk.Size || now.Vol.ID.FSSerial != f.Target.Vol.ID.FSSerial {
		return job.Result{}, errors.New("the card changed since you confirmed (unplugged or swapped) — nothing was done")
	}
	return f.Fix.Run(ctx, env, p)
}

// scanAction lists what's on the card without copying anything.
type scanAction struct {
	from string
	src  *worker.Client
}

func (s *scanAction) Name() string { return "scan" }

func (s *scanAction) Plan(ctx context.Context, env job.Env) (job.Plan, error) {
	src, err := worker.Spawn(worker.Spec{From: s.from, Raw: isFile(s.from) || isDevice(s.from)}, stall)
	if err != nil {
		return job.Plan{}, fmt.Errorf("opening %s: %w", s.from, err)
	}
	s.src = src
	return job.Plan{
		Summary: []string{"From: " + s.from + " — only read; nothing on it is changed",
			"Do: list every file by type and folder, and what could be skipped; nothing is copied"},
		Result: []string{"Files and sizes by type and by folder, and suggestions of what could be skipped"},
	}, nil
}

func (s *scanAction) Run(ctx context.Context, env job.Env, _ job.Plan) (job.Result, error) {
	inv, err := scan.Scan(ctx, s.src, profile.Detect(s.src))
	if err != nil {
		return job.Result{}, err
	}
	res := job.Result{Counters: map[string]int{}, Clean: len(inv.Unlistable) == 0}
	type cat struct {
		name  string
		n     int
		bytes int64
	}
	by := map[profile.Category]*cat{}
	for _, it := range inv.Items {
		c := by[it.Cat]
		if c == nil {
			c = &cat{name: string(it.Cat)}
			by[it.Cat] = c
		}
		c.n++
		c.bytes += it.Size
	}
	var cats []*cat
	for _, c := range by {
		cats = append(cats, c)
	}
	sort.Slice(cats, func(i, j int) bool { return cats[i].bytes > cats[j].bytes })
	for _, c := range cats {
		res.Counters[c.name] = c.n
		env.Log("info", fmt.Sprintf("%s: %s files, %s", c.name, preflight.Commas(c.n), preflight.GB(c.bytes)))
	}
	for id, g := range inv.Suggested {
		env.Log("info", fmt.Sprintf("Could be skipped (only if you choose): %s — %d files, %s", id, g.Files, preflight.GB(g.Bytes)))
	}
	if len(inv.Unlistable) > 0 {
		env.Log("warn", fmt.Sprintf("%d folders could not be listed (possible damage)", len(inv.Unlistable)))
	}
	return res, nil
}

func (s *scanAction) Close() error {
	if s.src != nil {
		return s.src.Close()
	}
	return nil
}
