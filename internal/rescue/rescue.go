// Package rescue copies every readable file off a card: priority order, bad-sector salvage,
// verify-after-write, never replacing a good copy with a worse one, resumable, and asking the
// user whenever something goes wrong.
package rescue

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/archive"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/destfs"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/drives"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/elevate"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/fsread"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/ioerr"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/job"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/manifest"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/paths"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/preflight"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/profile"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/report"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/retry"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/salvage"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/scan"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/source"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/worker"
)

// Source is what rescue reads from: the worker client (or a test fake).
type Source interface {
	source.Source
	Retarget(worker.Spec) error
	ResetBudget()
	Close() error
}

// Choices specific to rescue questions.
const (
	SkipVideos job.Choice = "skip-videos"
	SkipRest   job.Choice = "skip-rest" // skip the remaining files, report them FAILED
	KeepTrying job.Choice = "continue"
)

// notRespondingDefault: a person deciding gets "wait" highlighted; an unattended --yes run skips
// the remaining files (reported FAILED) instead of waiting forever.
func notRespondingDefault(interactive bool) job.Choice {
	if interactive {
		return job.Wait
	}
	return SkipRest
}

// ErrStoppedByUser is returned when the user answered "stop" to a question during the run.
var ErrStoppedByUser = errors.New("stopped by you — run the same command again to continue where it stopped")

// ErrImageInstead is returned when the user chose "image the card instead".
var ErrImageInstead = errors.New("you chose to make a full backup image of the card instead")

// Rescue is the "Copy files" action.
type Rescue struct {
	From                      string
	Raw                       bool
	Volume                    int
	VolumePicked              bool // the user chose the partition (--volume): no "more partitions" note
	Dest                      string
	Types                     []profile.Category // empty or containing "all" = everything, Other included
	Exclude                   []string           // suggestion ids the user chose to skip ("recommended" = all recommended)
	Apps                      []string           // only files of these apps (whatsapp, telegram)
	SkipEmpty                 bool               // leave out 0-byte files (the user's tick)
	Backup                    string             // the user's choice, for the summary ("Quick", "Custom")
	Profiles                  []string           // empty = detect
	Compress                  bool
	Fresh                     bool // "start over": copy every file again, never "already copied"
	Interactive               bool
	StallTimeout, WaitTimeout time.Duration

	// Seams (nil = production behaviour).
	OpenSource  func(worker.Spec, time.Duration) (Source, error)
	Present     func(id drives.Identity, from string) (string, bool)
	Free        func(string) (uint64, error)
	LookupDrive func(string) (drives.Drive, error)
	Now         func() time.Time
	PollEvery   time.Duration
	CoolDown    time.Duration

	src      Source
	scanned  bool
	unlock   func() // the per-card lock, taken by Plan, released by Run (when it ends) or Close
	inv      scan.Inventory
	selected []scan.Item
	excluded map[string]string // rel → why (user's choice)
	srcDrive drives.Drive
	dstDrive drives.Drive
}

func (r *Rescue) Name() string { return "rescue" }

func (r *Rescue) defaults() {
	if r.StallTimeout == 0 {
		r.StallTimeout = 30 * time.Second
	}
	if r.OpenSource == nil {
		r.OpenSource = func(s worker.Spec, st time.Duration) (Source, error) { return worker.Spawn(s, st) }
	}
	if r.Present == nil {
		r.Present = present
	}
	if r.Free == nil {
		r.Free = destfs.Free
	}
	if r.LookupDrive == nil {
		r.LookupDrive = drives.Lookup
	}
	if r.Now == nil {
		r.Now = time.Now
	}
	if r.PollEvery == 0 {
		r.PollEvery = time.Second
	}
	if r.CoolDown == 0 {
		r.CoolDown = 2 * time.Second
	}
}

// present finds the card again after it was removed (see drives.FindCard).
func present(id drives.Identity, from string) (string, bool) { return drives.FindCard(id, from) }

// IsDevicePath reports a raw device or volume path (needs admin to read).
func IsDevicePath(p string) bool {
	return strings.HasPrefix(p, `\\.\`) || strings.HasPrefix(p, "/dev/")
}

// physicalOf is a seam over drives.PhysicalOf.
var physicalOf = drives.PhysicalOf

// sourceRoots are the paths destfs must never write into: the source itself and, for a raw
// card, every mounted volume of that physical disk.
// volumeRoot maps a volume path \\.\E: to its root E:\ ("" for anything else).
func volumeRoot(p string) string {
	if strings.HasPrefix(p, `\\.\`) && len(p) == 6 && p[5] == ':' {
		return p[4:6] + `\`
	}
	return ""
}

func (r *Rescue) sourceRoots() []string {
	roots := []string{r.From}
	if root := volumeRoot(r.From); root != "" {
		roots = append(roots, root) // \\.\E: is E:\ — protected even if its disk can't be resolved
	}
	if phys := r.srcDrive.PhysicalID; phys != "" && r.Raw {
		if ds, err := drives.Lister(); err == nil {
			for _, d := range ds {
				if d.PhysicalID == phys {
					roots = append(roots, d.Root)
				}
			}
		}
	}
	return roots
}

func (r *Rescue) spec() worker.Spec { return worker.Spec{From: r.From, Raw: r.Raw, Volume: r.Volume} }

func (r *Rescue) reportsDir() string { return filepath.Join(r.Dest, "Reports") }
func (r *Rescue) storePath() string  { return filepath.Join(r.reportsDir(), "state.db") }

// PreviousChoice reads the backup choice and compress setting of an earlier run into dest.
func PreviousChoice(dest string) (s Selection, compress, ok bool) {
	p := filepath.Join(dest, "Reports", "state.db")
	if _, err := os.Stat(p); err != nil {
		return s, false, false
	}
	st, err := job.OpenStore(p)
	if err != nil {
		return s, false, false
	}
	defer st.Close()
	m, err := manifest.Open(st)
	if err != nil || m.Backup == "" {
		return s, false, false
	}
	return Selection{Name: m.Backup, Types: m.Types, Apps: m.Apps, Exclude: m.Excluded, SkipEmpty: m.SkipEmpty}, m.Compress, true
}

// Inventory is the last scan (after Plan).
func (r *Rescue) Inventory() scan.Inventory { return r.inv }

// Close releases the reader process.
func (r *Rescue) Close() error {
	if r.unlock != nil { // planned but never run
		r.unlock()
		r.unlock = nil
	}
	if r.src != nil {
		err := r.src.Close()
		r.src = nil
		return err
	}
	return nil
}

// wholeCard reports whether the copy reads a whole card (a device, or a volume at its root), the
// only case where "make a full backup image instead" leads anywhere (OU7): not one folder of the
// card, not an image file.
func (r *Rescue) wholeCard() bool {
	if IsDevicePath(r.From) {
		return true
	}
	if r.srcDrive.Root == "" {
		return false
	}
	a, b := filepath.Clean(r.From), filepath.Clean(r.srcDrive.Root)
	return a == b || runtime.GOOS == "windows" && strings.EqualFold(a, b)
}

// stopWith makes Stop (ctx) end every call into the reader process — a read stuck on a dying card,
// the re-open after a restart — not only the listings (OA12, SA-9).
func stopWith(src Source, ctx context.Context) {
	if c, ok := src.(interface{ SetContext(context.Context) }); ok {
		c.SetContext(ctx)
	}
}

// Selection is the user's choice as a Selection.
func (r *Rescue) Selection() Selection {
	return Selection{Name: r.Backup, Types: r.Types, Apps: r.Apps, Exclude: r.Exclude, SkipEmpty: r.SkipEmpty}
}

// Plan opens the card (through the worker), scans it, applies the user's choices and runs the
// pre-flight checks.
func (r *Rescue) Plan(ctx context.Context, env job.Env) (job.Plan, error) {
	r.defaults()
	var p job.Plan
	if err := destfs.CheckRoot(r.Dest); err != nil {
		return p, err // a pasted path with its quotes, "aux", "a:b": said plainly, before any write
	}
	if r.src == nil {
		src, err := r.OpenSource(r.spec(), r.StallTimeout)
		if errors.Is(err, source.ErrNeedsAdmin) {
			p.NeedsAdmin = true
			p.Issues = append(p.Issues, job.Issue{Kind: "needs-admin", Severity: job.Block,
				Title: "Reading the card directly needs administrator rights", Detail: elevate.Instruction(nil),
				Choices: []job.Choice{job.Elevate, job.Stop}, Default: job.Elevate})
			return p, nil
		}
		if err != nil {
			return p, fmt.Errorf("opening %s: %w", r.From, err)
		}
		r.src = src
	}
	stopWith(r.src, ctx)
	if re, ok := r.src.(interface{ ReadError() string }); ok && re.ReadError() != "" {
		p.Caps = fsCaps(r.src) // e.g. Read disabled: "not readable by built-in parsers — mount with the OS"
		p.Issues = append(p.Issues, job.Issue{Kind: "unreadable-fs", Severity: job.Block,
			Title: "The card's file system can't be read here", Detail: re.ReadError(),
			Choices: []job.Choice{job.Stop}, Default: job.Stop})
		return p, nil
	}
	active := profile.ByNames(r.Profiles)
	if len(r.Profiles) == 0 || (len(r.Profiles) == 1 && r.Profiles[0] == "auto") {
		active = profile.Detect(r.src)
	}
	r.srcDrive, _ = r.LookupDrive(r.From)
	if r.Raw {
		r.srcDrive.Root = ""
		if IsDevicePath(r.From) {
			// A raw card: its physical disk decides "same disk", so the rescue can never be
			// written onto any volume of the card being read (C2).
			r.srcDrive = drives.Drive{Removable: true, PhysicalID: physicalOf(r.From), Root: volumeRoot(r.From)}
		} else {
			r.srcDrive = drives.Drive{Removable: true} // a card image file: only the file itself is the source
		}
	}
	// one run per card (R14), from the first read: the plan scans the card, so it takes the lock
	if r.unlock == nil {
		u, err := r.lock(r.src.Identity())
		if errors.Is(err, job.ErrSourceBusy) {
			p.Issues = append(p.Issues, job.Issue{Kind: "busy", Severity: job.Block, Title: err.Error(),
				Detail: "Finish or close the other window first.", Choices: []job.Choice{job.Stop}, Default: job.Stop})
			return p, nil
		}
		if err != nil {
			return p, err
		}
		r.unlock = u
	}
	inv := r.inv
	if !r.scanned { // planning again after a menu answer reuses the scan: fewer reads of the card
		var err error
		if inv, err = scan.Scan(ctx, r.src, active); err != nil {
			return p, err
		}
		r.inv, r.scanned = inv, true
	}
	r.selected, r.excluded = Select(inv, r.Selection())
	r.dstDrive, _ = r.LookupDrive(r.Dest)
	in := preflight.Input{Action: "rescue", Src: r.srcDrive, Dst: r.dstDrive, DstPath: r.Dest, Items: r.selected,
		Suggested: undecided(inv.Suggested, r.Selection()), Admin: elevate.IsAdmin(), NeedsAdmin: r.Raw && IsDevicePath(r.From)}
	if free, err := r.Free(r.Dest); err == nil {
		in.Dst.Free = free
	} else if parent := filepath.Dir(r.Dest); parent != r.Dest {
		if free, err := r.Free(parent); err == nil {
			in.Dst.Free = free
		}
	}
	if !r.Raw {
		in.Src.Root = r.From
	}
	if _, err := os.Stat(r.storePath()); err == nil {
		s, err := job.OpenStore(r.storePath())
		if errors.Is(err, job.ErrBusy) {
			p.Issues = append(p.Issues, job.Issue{Kind: "busy", Severity: job.Block, Title: job.ErrBusy.Error(),
				Choices: []job.Choice{job.Stop}, Default: job.Stop})
		} else if err == nil {
			if m, err := manifest.Open(s); err == nil {
				if s.Recovered() {
					r.rebuild(env, m)
					m, _ = manifest.Open(s)
				}
				if m.Volume.FSSerial != "" || m.Volume.Model != "" {
					if ok, _, _ := m.Volume.Matches(r.src.Identity()); !ok || !r.sameCard(m) {
						p.Issues = append(p.Issues, job.Issue{Kind: "dest-other-card", Severity: job.Block,
							Title:   "This folder holds a rescue of a different card",
							Detail:  "Choose a new folder.",
							Choices: []job.Choice{job.Stop}, Default: job.Stop})
					}
				}
				done := 0
				m.Each(func(e manifest.Entry) error {
					if _, _, inZip := archive.ZipPart(e.Dest); e.Status == manifest.OK && (!inZip || archive.ZipIndexHas(s, e.Dest, e.Bytes)) {
						done++ // an entry of a ZIP part a crash left unfinished isn't done
					}
					return nil
				})
				if !(r.Fresh && r.Compress) { // Start over into ZIPs adds every file again
					in.Already = func(src string) (manifest.Entry, bool) {
						e, ok, _ := m.Get(src)
						return e, ok
					}
				}
				if done > 0 && done < len(r.selected) {
					in.Resume = &job.ResumeInfo{Done: done, Total: len(r.selected)}
				}
				p.Issues = append(p.Issues, preflight.Check(in)...)
				in.Already = nil
			}
			s.Close()
		}
	} else {
		p.Issues = append(p.Issues, preflight.Check(in)...)
	}
	if v, ok := r.src.(interface{ Volumes() []fsread.Volume }); ok && r.Raw && !r.VolumePicked && len(v.Volumes()) > 1 {
		// One partition per run: say so in every front end, not only the CLI.
		n := len(v.Volumes())
		p.Issues = append(p.Issues, job.Issue{Kind: "more-partitions", Severity: job.Info,
			Title:   fmt.Sprintf("This card has %d partitions; only partition %d is copied", n, r.Volume+1),
			Detail:  fmt.Sprintf("To copy another, run again with --volume N (0 to %d) into a new folder", n-1),
			Choices: []job.Choice{job.Continue}, Default: job.Continue})
	}
	for _, it := range r.selected {
		p.Bytes += it.Size
	}
	p.Items = len(r.selected)
	p.Caps = fsCaps(r.src)
	choice := r.Backup
	if choice == "" {
		choice = "Custom"
	}
	p.Summary = []string{
		fmt.Sprintf("From: %s — %s; only read, nothing on it is changed", r.From, r.sourceKind()),
		fmt.Sprintf("To:   %s%s", r.Dest, r.spaceNote(int64(in.Dst.Free), p.Bytes)),
		fmt.Sprintf("Copy: Backup: %s — %s (%s) from %s", choice, preflight.Files(len(r.selected)), preflight.GB(p.Bytes), r.From),
		fmt.Sprintf("      %s; card layout: %s", byCategory(r.selected), strings.Join(inv.Profiles, ", ")),
	}
	if len(r.excluded) > 0 {
		p.Summary = append(p.Summary, fmt.Sprintf("Left out by your choice: %s (%s) — each listed in the report with the reason", preflight.Files(len(r.excluded)), whyCounts(r.excluded)))
	}
	empty, comp := "empty files copied", "not compressed"
	if r.SkipEmpty {
		empty = "empty files left out"
	}
	if r.Compress {
		comp = "compressed into ZIP files"
	}
	p.Summary = append(p.Summary, "Options: "+empty+" · "+comp)
	if len(inv.Unlistable) > 0 {
		p.Summary = append(p.Summary, fmt.Sprintf("Warning: %s could not be listed (possible damage)", preflight.Folders(len(inv.Unlistable))))
	}
	if r.Compress {
		p.Result = append(p.Result, fmt.Sprintf("ZIP files in %s, one series per type (%s), opened by Windows, macOS, Linux or 7-Zip",
			r.Dest, filepath.Join(r.Dest, "Photos-0001.zip")))
	} else {
		p.Result = append(p.Result, fmt.Sprintf("Your files in %s, sorted into Photos, Videos, Music, … with the card's own folder names",
			r.Dest))
	}
	p.Result = append(p.Result,
		"Parts of a file that can't be read are filled with zeros and the file is named …__PARTIAL",
		fmt.Sprintf("A report of every file (CSV, summary, manifest.json) in %s", r.reportsDir()),
		"Stop any time; running the same command again continues where it stopped")
	return p, nil
}

// errEnough ends a walk over the manifest early.
var errEnough = errors.New("enough")

// sameCard backs up the serial check on resume (some devices give every card one serial): the
// size, when the card is read the same way as before, and a sample of up to 20 copied files, of
// which at least one must still be on the card with its size once 5 or more were looked at.
func (r *Rescue) sameCard(m *manifest.Manifest) bool {
	id := r.src.Identity()
	if m.SourceKind == r.src.Kind() && m.Volume.Size != 0 && id.Size != 0 && m.Volume.Size != id.Size {
		return false
	}
	sampled, found := 0, 0
	m.Each(func(e manifest.Entry) error {
		if e.Status != manifest.OK || strings.HasSuffix(e.Source, "/") {
			return nil
		}
		sampled++
		if fi, err := r.src.Stat(e.Source); err == nil && fi.Size() == e.Bytes {
			found++
			return errEnough
		}
		if sampled >= 20 {
			return errEnough
		}
		return nil
	})
	return sampled < 5 || found > 0
}

// rebuild refills a damaged progress record from manifest.json and the files on the
// destination, and tells the user.
func (r *Rescue) rebuild(env job.Env, m *manifest.Manifest) {
	n, err := manifest.Rebuild(m, filepath.Join(r.reportsDir(), "manifest.json"), r.Dest)
	switch {
	case env.Log == nil:
	case err != nil:
		env.Log("warn", "The progress record was damaged and could not be rebuilt ("+err.Error()+") — files are checked and copied again")
	default:
		env.Log("info", fmt.Sprintf("Rebuilt progress from your files — nothing was lost (%s)", preflight.Files(n)))
	}
}

// undecided keeps only the skip suggestions the user's choice hasn't already decided.
func undecided(all map[string]scan.Group, s Selection) map[string]scan.Group {
	ex := s.excludeIDs()
	out := map[string]scan.Group{}
	for id, g := range all {
		if !ex[id] {
			out[id] = g
		}
	}
	return out
}

// sourceKind describes the source in plain words.
func (r *Rescue) sourceKind() string {
	switch {
	case r.Raw && IsDevicePath(r.From):
		return "the card read directly (built-in reader)"
	case r.Raw:
		return "a card image file"
	case r.srcDrive.Removable:
		d := "a card or USB drive"
		if r.srcDrive.Label != "" {
			d += " labelled " + r.srcDrive.Label
		}
		return d
	}
	return "an internal drive or folder"
}

// spaceNote is " (NTFS, 120.0 GB free → about 78.3 GB free after)".
func (r *Rescue) spaceNote(free, need int64) string {
	if free <= 0 {
		return ""
	}
	fsName := r.dstDrive.FS
	if fsName != "" {
		fsName += ", "
	}
	after := free - need
	if after < 0 {
		return fmt.Sprintf(" (%s%s free — %s short)", fsName, preflight.GB(free), preflight.GB(-after))
	}
	return fmt.Sprintf(" (%s%s free → about %s free after)", fsName, preflight.GB(free), preflight.GB(after))
}

// byCategory is "Photos 8,287 · Videos 348 · …" for the files to copy.
func byCategory(items []scan.Item) string {
	n := map[profile.Category]int{}
	for _, it := range items {
		n[it.Cat]++
	}
	var parts []string
	for _, c := range profile.Priority {
		if n[c] > 0 {
			parts = append(parts, fmt.Sprintf("%s %s", c, preflight.Commas(n[c])))
		}
	}
	if len(parts) == 0 {
		return "nothing"
	}
	return strings.Join(parts, " · ")
}

// whyCounts groups left-out files by reason ("thumbnails 290 · type not selected: Other 19").
func whyCounts(excluded map[string]string) string {
	n := map[string]int{}
	for _, why := range excluded {
		if i := strings.Index(why, " ("); i > 0 {
			why = why[:i]
		}
		n[why]++
	}
	keys := make([]string, 0, len(n))
	for k := range n {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return n[keys[i]] > n[keys[j]] || n[keys[i]] == n[keys[j]] && keys[i] < keys[j] })
	var parts []string
	for i, k := range keys {
		if i == 4 {
			parts = append(parts, "…")
			break
		}
		parts = append(parts, fmt.Sprintf("%s %d", k, n[k]))
	}
	return strings.Join(parts, " · ")
}

func fsCaps(src Source) []job.Capability {
	if c, ok := src.(interface{ Caps() []job.Capability }); ok {
		return c.Caps()
	}
	return nil
}

// run is one Run's state.
type run struct {
	r          *Rescue
	env        job.Env
	ctx        context.Context
	cancel     context.CancelFunc
	out        destfs.FS
	sink       archive.Sink
	m          *manifest.Manifest
	plan       *paths.Planner
	copier     *salvage.Copier
	from       string
	id         drives.Identity
	counts     map[string]int
	sum        report.Summary
	mon        monitor
	done       int64
	total      int64
	thr        job.Throttle
	skipVid    bool
	giveUp     bool // the user chose to skip the rest after the card stopped responding
	st         *job.Store
	keptBad    int // rows from an earlier run, outside this choice, that are not fully copied
	zparts     archive.ZipParts
	destSerial string                    // the destination volume's serial when the run started ("" if unknown)
	destMoved  bool                      // the card came back where the destination was: nothing more is written
	replaced   map[string]manifest.Entry // a new copy's stored name → the earlier row it replaced
	retryWhy   string                    // why the current file is being tried again (for "gave up")
	held       *manifest.Entry           // this file's copy stored before the card went away (recorded, not yet counted)
	refused    *int64                    // this read's unreadable bytes when its copy was refused as worse
	skippedVid int                       // videos left out because the user chose to skip them
	handled    map[string]bool           // files finished in this run (for "N files left")
	kept       map[string]bool           // files outside this choice kept as an earlier run left them
}

func (x *run) ask(is job.Issue) job.Choice {
	x.sum.Issues = append(x.sum.Issues, is.Title)
	if x.env.Prompt == nil {
		return job.AutoPrompter{}.Ask(x.ctx, is)
	}
	return x.env.Prompt.Ask(x.ctx, is)
}

func (x *run) log(level, msg string) {
	if x.env.Log != nil {
		x.env.Log(level, msg)
	}
}

func (x *run) progress(item string) {
	if x.env.Progress != nil && x.thr.Ok() {
		x.env.Progress(job.Progress{Phase: "Copying files", Item: item, Done: x.done, Total: x.total, Counters: x.counts})
	}
}

// lockKey is one lock per card (R14): any folder of a removable card shares it. A folder on a
// fixed disk is not a card, so it is keyed by its path.
func lockKey(id drives.Identity, from string, removable bool) string {
	k := id.LockKey()
	if !removable {
		k += "|" + filepath.Clean(from)
	}
	return k
}

// lock takes the card's lock and, for a card, its physical disk's lock — every job reading that
// disk takes it, so a card without a serial is locked the same way whatever identity each job
// worked out for it (OA3). A folder on a fixed disk doesn't lock the whole disk.
func (r *Rescue) lock(id drives.Identity) (func(), error) {
	unlock, err := job.LockSource(lockKey(id, r.From, r.srcDrive.Removable))
	if err != nil {
		return nil, err
	}
	phys := r.srcDrive.PhysicalID
	if phys == "" {
		phys = id.PhysicalID
	}
	if k := (drives.Identity{PhysicalID: phys}).DiskLockKey(); k != "" && r.srcDrive.Removable {
		disk, err := job.LockSource(k)
		if err != nil {
			unlock()
			return nil, err
		}
		return func() { disk(); unlock() }, nil
	}
	return unlock, nil
}

// Run copies the planned files.
func (r *Rescue) Run(ctx context.Context, env job.Env, p job.Plan) (job.Result, error) {
	r.defaults()
	for _, is := range p.Issues {
		if is.Severity == job.Block {
			return job.Result{}, fmt.Errorf("can't start: %s", is.Title)
		}
	}
	if r.src == nil {
		return job.Result{}, errors.New("rescue: Plan first")
	}
	stopWith(r.src, ctx)
	out := env.Out
	if out == nil {
		out = destfs.New(r.Dest, r.sourceRoots()...)
	}
	if err := out.MkdirAll(r.reportsDir()); err != nil {
		return job.Result{}, err
	}
	st, err := job.OpenStore(r.storePath())
	if err != nil {
		return job.Result{}, err
	}
	defer st.Close()
	m, err := manifest.Open(st)
	if err != nil {
		return job.Result{}, err
	}
	if st.Recovered() {
		r.rebuild(env, m)
	}
	id := r.src.Identity()
	unlock := r.unlock // taken by Plan; held until this run ends
	r.unlock = nil
	if unlock == nil {
		var err error
		if unlock, err = r.lock(id); err != nil {
			return job.Result{}, err
		}
	}
	defer unlock()
	if release, err := job.Inhibit("copying files off a card"); err == nil {
		defer release()
	}
	m.Volume, m.SourceKind, m.SourceRoot, m.Profiles = id, r.src.Kind(), r.From, r.inv.Profiles
	m.Excluded = r.Exclude
	m.Backup, m.Types, m.Apps, m.SkipEmpty, m.Compress = r.Backup, r.Types, r.Apps, r.SkipEmpty, r.Compress
	if err := m.SaveMeta(); err != nil {
		return job.Result{}, err
	}
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	x := &run{r: r, env: env, ctx: rctx, cancel: cancel, out: out, m: m, st: st, copier: salvage.NewCopier(),
		from: r.From, id: id, counts: map[string]int{}, plan: paths.NewPlanner(r.Dest, paths.MaxPath),
		thr: job.Throttle{Every: 100 * time.Millisecond}, destSerial: drives.VolumeSerial(r.Dest)}
	x.plan.Windows = windowsFS(r.dstDrive.FS) || r.Compress // ZIP parts get opened in Explorer too
	// a crashed run in the other mode (Compress switched) left its temp files too
	archive.CleanTemps(out, r.Dest)
	x.sink = archive.FolderSink(out, r.Dest)
	if r.Compress {
		x.sink = archive.ZipSink(out, r.Dest, st)
	}
	start := r.Now()
	// Every copy an earlier run recorded keeps its name, even for files this run doesn't list or
	// select: otherwise a case-clashing name could be planned onto it and overwrite it.
	// Claimed under the name the file is planned by (a LOST.DIR fragment's carries its sniffed
	// extension), or the file's own earlier copy would look like another file's.
	key := map[string]string{}
	for _, it := range r.inv.Items {
		if k := destRel(it); k != it.Rel {
			key[it.Rel] = k
		}
	}
	m.Each(func(e manifest.Entry) error {
		if d := plannedDest(e); d != "" {
			k, ok := key[e.Source]
			if !ok {
				k = e.Source
			}
			x.plan.Claim(d, k)
		}
		return nil
	})
	for _, it := range r.selected {
		x.plan.Add(it.Cat, destRel(it))
		x.total += it.Size
	}
	x.recordExcluded()
	again, runErr := x.loop(r.selected)
	if x.destMoved { // the destination's path now leads to the card: write nothing more, not even reports
		return job.Result{Counters: x.counts}, runErr
	}
	if runErr == nil && len(again) > 0 {
		runErr = x.retryQueue(again)
	}
	if err := x.sink.Close(); err != nil && runErr == nil {
		x.log("warn", err.Error())
	}
	if fz, ok := x.sink.(interface{ Failed() []string }); ok {
		x.markFailed(fz.Failed())
	}
	x.dropSuperseded()
	x.recordUnlistable()
	if n := x.skippedVid; n > 0 { // the user's choice, so the run stays clean — but it shows
		v := preflight.Commas(n) + " videos"
		if n == 1 {
			v = "1 video"
		}
		x.sum.Issues = append(x.sum.Issues, v+" left out because you chose to skip the remaining videos (the card was degrading) — run again to copy them")
	}
	if x.keptBad > 0 {
		x.sum.Issues = append(x.sum.Issues, fmt.Sprintf("%d files from an earlier run are outside this choice and not fully copied — choose them again to retry", x.keptBad))
	}
	csvPath, repErr := x.writeReports(start)
	if repErr != nil {
		x.log("warn", "Some reports could not be written — your copied files are not affected: "+repErr.Error())
	}
	res := job.Result{Counters: x.counts, Clean: repErr == nil && x.counts["PARTIAL"]+x.counts["FAILED"]+x.counts["STALLED"]+x.counts["LOCKED"]+x.keptBad == 0}
	if csvPath != "" {
		res.Reports = []string{csvPath}
	}
	if runErr != nil && ctx.Err() != nil {
		return res, ctx.Err()
	}
	if runErr != nil && rctx.Err() != nil { // the user chose "stop" in a question
		return res, ErrStoppedByUser
	}
	return res, runErr
}

// recordUnlistable puts every folder the card couldn't list into the report as a FAILED row (its
// files were never seen, so they can't be listed one by one): nothing on the card disappears
// silently, the run isn't "clean", and a later verify or Fix knows the rescue is incomplete.
func (x *run) recordUnlistable() {
	now := map[string]bool{}
	put := func(e manifest.Entry) {
		if err := x.m.Put(e); err != nil {
			x.log("warn", err.Error())
			return
		}
		x.count(manifest.FAILED)
		x.sum.Add(e)
	}
	for _, u := range x.r.inv.Unlistable {
		dir, why, _ := strings.Cut(u, ": ")
		src := strings.TrimSuffix(dir, "/") + "/"
		now[src] = true
		put(manifest.Entry{Source: src, Status: manifest.FAILED,
			Error: "folder could not be listed completely (possible damage) — files in it that could not be listed were not copied: " + why})
	}
	for _, u := range x.r.inv.Unreadable {
		f, why, _ := strings.Cut(u, ": ")
		now[f] = true
		put(manifest.Entry{Source: f, Status: manifest.FAILED, Error: "its details could not be read (possible damage) — not copied: " + why})
	}
	// a folder that couldn't be listed before and was listed this time, or a name a damaged folder
	// listed twice that it lists once now: its old row goes
	var stale []string
	x.m.Each(func(e manifest.Entry) error {
		dup := strings.HasSuffix(e.Source, " with this name)")
		if (strings.HasSuffix(e.Source, "/") || dup) && e.Status == manifest.FAILED && !now[e.Source] {
			stale = append(stale, e.Source)
		}
		return nil
	})
	for _, s := range stale {
		x.m.Delete(s)
	}
	if n := len(x.r.inv.Unreadable); n > 0 {
		x.sum.Issues = append(x.sum.Issues, fmt.Sprintf("%s could not be read at all (possible damage) — try Image, or Find deleted files with Deep scan", preflight.Files(n)))
	}
	if n := len(x.r.inv.Unlistable); n > 0 {
		x.sum.Issues = append(x.sum.Issues, fmt.Sprintf("%s could not be listed completely; files in them that could not be listed were not copied — try Image, or Find deleted files with Deep scan", preflight.Folders(n)))
	}
}

// plannedDest is the destination the planner gave e's file in the run that stored it — the ZIP
// part and the __PARTIAL suffix taken off — or "" when nothing is stored.
func plannedDest(e manifest.Entry) string {
	d := e.Dest
	if d == "" {
		return ""
	}
	if part, name, ok := archive.ZipPart(d); ok {
		d = part[:strings.LastIndex(part, "-")] + "/" + name
	}
	if e.Status == manifest.PARTIAL {
		ext := path.Ext(d)
		d = strings.TrimSuffix(strings.TrimSuffix(d, ext), "__PARTIAL") + ext
	}
	return d
}

// destRel is the path a file is planned under: fragments get their sniffed extension.
func destRel(it scan.Item) string {
	if it.Fragment && it.Ext != "" && path.Ext(it.Rel) == "" {
		return it.Rel + it.Ext
	}
	return it.Rel
}

func (x *run) count(st manifest.Status) { x.counts[string(st)]++ }

// stored reports whether a file recorded in the manifest is still on the destination, in
// whichever form an earlier run stored it (a folder or a ZIP part), so switching "compress"
// between runs never reads the card again for files already saved.
func (x *run) stored(dest string, size int64) bool {
	if _, _, ok := archive.ZipPart(dest); ok {
		if x.zparts == nil {
			x.zparts = archive.ZipParts{}
		}
		return x.zparts.Stored(x.st, x.r.Dest, dest, size)
	}
	return archive.FolderSink(x.out, x.r.Dest).Has(dest, size)
}

// recordExcluded writes an EXCLUDED row for every file the user chose to leave out, and for
// symbolic links (listed, not followed).
func (x *run) recordExcluded() {
	for _, l := range x.r.inv.Links {
		e := manifest.Entry{Source: l, Status: manifest.EXCLUDED, Error: "symbolic link or special file (not followed)"}
		x.m.Put(e)
		x.count(manifest.EXCLUDED)
		x.sum.Add(e)
	}
	for _, ex := range sortedExcluded(x.r.excluded) {
		if old, ok, _ := x.m.Get(ex[0]); ok && old.Status != manifest.EXCLUDED {
			// From an earlier run with a wider choice: kept as it is, counted and reported.
			x.counts["KEPT-FROM-EARLIER-RUN"]++
			if x.kept == nil {
				x.kept = map[string]bool{}
			}
			x.kept[ex[0]] = true
			switch old.Status {
			case manifest.OK, manifest.ALREADY_COPIED:
			default:
				x.keptBad++
			}
			continue
		}
		e := manifest.Entry{Source: ex[0], Status: manifest.EXCLUDED, Error: ex[1]}
		x.m.Put(e)
		x.count(manifest.EXCLUDED)
		x.sum.Add(e)
	}
}

// loop handles items in order and returns the STALLED/LOCKED ones for a later retry.
func (x *run) loop(items []scan.Item) ([]scan.Item, error) {
	var again []scan.Item
	for _, it := range items {
		if err := x.ctx.Err(); err != nil {
			return again, err
		}
		if x.skipVid && it.Cat == profile.Videos {
			if old, ok, _ := x.m.Get(it.Rel); ok && (old.Status == manifest.OK || old.Status == manifest.PARTIAL) &&
				old.Dest != "" && x.stored(old.Dest, old.Bytes) {
				// Copied by an earlier run: kept as it is, counted and reported (as recordExcluded).
				x.counts["KEPT-FROM-EARLIER-RUN"]++
				if old.Status != manifest.OK {
					x.keptBad++
				}
				continue
			}
			e := manifest.Entry{Source: it.Rel, Category: it.Cat, Bytes: it.Size, Status: manifest.EXCLUDED, Error: "skipped by you (card degrading)"}
			x.m.Put(e)
			x.count(manifest.EXCLUDED)
			x.sum.Add(e)
			x.skippedVid++
			continue
		}
		st, err := x.one(it)
		x.done += it.Size
		x.progress(it.Rel)
		if err != nil {
			return again, err
		}
		if st == manifest.STALLED || st == manifest.LOCKED {
			again = append(again, it)
		}
	}
	return again, nil
}

func sortedExcluded(m map[string]string) [][2]string {
	out := make([][2]string, 0, len(m))
	for k, v := range m {
		out = append(out, [2]string{k, v})
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j][0] < out[j-1][0]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// retryQueue retries STALLED/LOCKED files after a cool-down. Files still locked are listed
// and the user decides (retry / skip); at most 3 rounds.
func (x *run) retryQueue(q []scan.Item) error {
	for round := 0; len(q) > 0 && round < 3; round++ {
		select {
		case <-x.ctx.Done():
			return x.ctx.Err()
		case <-time.After(x.r.CoolDown):
		}
		for _, it := range q {
			x.uncount(it.Rel)
		}
		again, err := x.loop(q)
		if err != nil {
			return err
		}
		var locked []scan.Item
		var names []string
		for _, it := range again {
			if e, _, _ := x.m.Get(it.Rel); e.Status == manifest.LOCKED {
				locked = append(locked, it)
				names = append(names, it.Rel)
			}
		}
		if len(locked) == 0 {
			return nil
		}
		c := x.ask(job.Issue{Kind: "files-locked", Severity: job.Warn,
			Title:   fmt.Sprintf("%d files are in use by another program", len(locked)),
			Detail:  "Close programs using the card, then retry: " + strings.Join(names[:min(10, len(names))], ", "),
			Choices: []job.Choice{job.Retry, job.Skip}, Default: job.Skip})
		if c != job.Retry {
			return nil
		}
		q = locked
	}
	return nil
}

// uncount removes a queued file's earlier status from the counters before it is retried.
func (x *run) uncount(rel string) {
	if e, ok, _ := x.m.Get(rel); ok && x.counts[string(e.Status)] > 0 {
		x.counts[string(e.Status)]--
		x.sum.Remove(e)                 // the summary counts it once too
		x.done = max(0, x.done-e.Bytes) // and progress doesn't pass 100%
	}
}

// one copies a single file and records its outcome.
func (x *run) one(it scan.Item) (manifest.Status, error) {
	r := x.r
	dest := x.plan.Plan(it.Cat, destRel(it))
	old, had, _ := x.m.Get(it.Rel)
	if !r.Fresh && had && old.Status == manifest.OK && old.Dest != "" && x.stored(old.Dest, old.Bytes) &&
		old.Bytes == it.Size && sameTime(old.SrcMTime, it.MTime) {
		x.count(manifest.ALREADY_COPIED)
		x.sum.Add(manifest.Entry{Status: manifest.ALREADY_COPIED, Source: it.Rel})
		return manifest.ALREADY_COPIED, nil
	}
	e := manifest.Entry{Source: it.Rel, Category: it.Cat, Bytes: it.Size, MTime: it.MTime, SrcMTime: it.MTime}
	var prev *manifest.Entry
	if had {
		prev = &old
	}
	if isFAT(r.dstDrive.FS) && it.Size > preflight.FATMaxFile {
		return x.finish(e, manifest.FAILED, "destination file system can't hold files this large (FAT32 4 GiB limit?)", prev)
	}
	for attempt := 0; ; attempt++ {
		if err := x.ctx.Err(); err != nil {
			return "", err
		}
		if c, err := x.spaceCheck(it.Size); err != nil || c == job.Stop {
			if err == nil {
				err = errors.New("stopped: the destination is full")
			}
			return "", err
		}
		x.retryWhy = "the card kept disappearing or not responding"
		st, msg, retryFile, err := x.copyOne(it, dest, old, had, &e)
		if h := x.held; h != nil {
			// The card went away after this read was stored: that copy is now the one to keep and
			// to compare the next read against.
			x.held = nil
			old, had, prev = *h, true, h
			if err != nil { // the run stops here: report the copy it holds
				x.count(h.Status)
				x.sum.Add(*h)
			}
		}
		if err != nil {
			return "", err
		}
		if retryFile {
			if attempt < 3 {
				continue
			}
			st, msg = manifest.FAILED, "gave up after 4 attempts: "+x.retryWhy
		}
		return x.finish(e, st, msg, prev)
	}
}

// sameTime compares modification times with FAT's 2-second resolution, also accepting a shift by
// a whole number of quarter hours up to 14 hours: Windows converts FAT local times with the
// current DST offset, and the same card read as a mounted folder and then raw (or as an image)
// can show every time moved by the local UTC offset (exFAT times without an offset, Linux vfat).
// The cost: a same-size edit an exact number of quarter hours later is not copied again.
func sameTime(a, b time.Time) bool {
	d := a.Sub(b)
	if d < 0 {
		d = -d
	}
	if d < 2*time.Second {
		return true
	}
	const q = 15 * time.Minute
	r := d % q
	return d <= 14*time.Hour+2*time.Second && (r <= 2*time.Second || q-r <= 2*time.Second)
}

// windowsFS reports a destination file system with Windows' name rules (FAT, exFAT, NTFS — the
// last as Linux names it, "fuseblk" for ntfs-3g included), even when written from Linux or macOS.
func windowsFS(fsName string) bool {
	switch strings.ToUpper(fsName) {
	case "EXFAT", "NTFS", "NTFS3", "FUSEBLK":
		return true
	}
	return isFAT(fsName)
}

func isFAT(fsName string) bool {
	switch strings.ToUpper(fsName) {
	case "FAT", "FAT12", "FAT16", "FAT32", "VFAT", "MSDOS":
		return true
	}
	return false
}

// spaceCheck makes sure the file fits before writing (proactive disk-full).
func (x *run) spaceCheck(size int64) (job.Choice, error) {
	if x.r.Compress {
		size *= 2 // spooled first, then added to its ZIP part
	}
	for i := 0; i < 1000; i++ {
		free, err := x.r.Free(x.r.Dest)
		if err != nil || int64(free) >= size+preflight.Reserve {
			return job.Continue, nil
		}
		c := x.ask(job.Issue{Kind: "disk-full", Severity: job.Warn,
			Title:   fmt.Sprintf("The destination is full (%s free, the next file needs %s)", preflight.GB(int64(free)), preflight.GB(size+preflight.Reserve)),
			Detail:  "Free some space on the destination, then retry. Everything copied so far is safe.",
			Choices: []job.Choice{job.Retry, job.Stop}, Default: job.Stop})
		if c != job.Retry {
			return job.Stop, nil
		}
	}
	return job.Stop, nil
}

// copyOne opens and copies one file. retry=true means "the card came back, try again".
func (x *run) copyOne(it scan.Item, dest string, old manifest.Entry, had bool, e *manifest.Entry) (manifest.Status, string, bool, error) {
	ra, size, c, err := x.r.src.OpenAt(it.Rel)
	if err != nil {
		return x.classify(err)
	}
	defer c.Close()
	head := make([]byte, min(size, 512))
	n, _ := ra.ReadAt(head, 0)
	ext := path.Ext(dest)
	e.Header = profile.HeaderCheck(ext, head[:n])
	b := &budgeted{r: ra, now: x.r.Now, budget: fileBudget(size), stop: x.cancel, ask: func() string {
		def := job.Skip
		if x.r.Interactive {
			def = KeepTrying
		}
		ch := x.ask(job.Issue{Kind: "file-slow", Severity: job.Warn,
			Title:   fmt.Sprintf("%s keeps failing", path.Base(it.Rel)),
			Detail:  "Keep trying, skip the bad areas (the file is saved as partial) or stop.",
			Choices: []job.Choice{KeepTrying, job.Skip, job.Stop}, Default: def})
		return string(ch)
	}}
	accept := func(res salvage.Result) bool {
		if !had || old.Bytes != it.Size || !sameTime(old.SrcMTime, it.MTime) {
			return true // nothing yet, or the file changed on the card: the new copy wins
		}
		if old.Dest == "" || !x.stored(old.Dest, old.Bytes) {
			return true // nothing stored, or the copy (good or partial) was deleted from the destination
		}
		cand := manifest.Entry{Status: manifest.OK, BadBytes: res.Bad}
		if res.Bad > 0 {
			cand.Status = manifest.PARTIAL
		}
		if x.r.Fresh {
			return !manifest.Better(cand, old) // Start over: the new copy wins unless it is worse
		}
		return manifest.Better(old, cand)
	}
	var cardErr error // set when reading the card failed, not writing the copy
	stored, res, err := x.sink.Put(x.ctx, dest, it.MTime, func(w io.Writer) (salvage.Result, error) {
		dw := &destWriter{w: w}
		res, err := x.copier.Copy(x.ctx, b, size, dw)
		if err != nil && dw.err == nil {
			cardErr = err
		}
		return res, err
	}, accept)
	if err != nil {
		if cardErr == nil {
			err = destError{err} // never the card's fault: no "card removed" or "degrading" question
		}
		return x.classify(err)
	}
	e.SHA256 = strings.ToUpper(fmt.Sprintf("%x", res.Sum))
	e.BadBytes = res.Bad
	if res.Bad > 0 {
		if gone, retry, err := x.cardGone(); gone || err != nil {
			if stored != "" {
				// What was read is on the destination: record it now, so it is never lost track
				// of — nor replaced by a worse read — whether the run goes on or stops here.
				h := *e
				h.Dest, h.Status = stored, manifest.PARTIAL
				h.Error = fmt.Sprintf("%d bytes unreadable (zero-filled) — the card went away while this file was read", res.Bad)
				x.noteReplaced(stored, old, had)
				if perr := x.m.Put(h); perr != nil && err == nil {
					err = perr
				}
				x.held = &h
			}
			return "", "", retry, err
		}
	}
	if stored == "" { // a worse copy than the one we already have
		*e = old
		x.refused = &res.Bad // the health monitor hears about this read, not the kept copy
		return old.Status, old.Error, false, nil
	}
	e.Dest = stored
	x.noteReplaced(stored, old, had)
	if res.Bad > 0 {
		return manifest.PARTIAL, fmt.Sprintf("%d bytes unreadable (zero-filled)", res.Bad), false, nil
	}
	return manifest.OK, "", false, nil
}

// noteReplaced remembers the earlier row a new copy replaced: a ZIP part that later fails falls
// back to it (markFailed).
func (x *run) noteReplaced(stored string, old manifest.Entry, had bool) {
	if had && old.Dest != "" && old.Dest != stored {
		if x.replaced == nil {
			x.replaced = map[string]manifest.Entry{}
		}
		x.replaced[stored] = old
	}
}

// destError is a failure writing the copy to the destination.
type destError struct{ err error }

func (e destError) Error() string { return e.err.Error() }
func (e destError) Unwrap() error { return e.err }

// destWriter remembers a failed write, telling a destination error from a card error.
type destWriter struct {
	w   io.Writer
	err error
}

func (d *destWriter) Write(p []byte) (int, error) {
	n, err := d.w.Write(p)
	if err != nil {
		d.err = err
	}
	return n, err
}

// classify turns a copy error into a status, or a question to the user.
func (x *run) classify(err error) (manifest.Status, string, bool, error) {
	var de destError
	switch {
	case x.ctx.Err() != nil:
		return "", "", false, x.ctx.Err()
	case errors.As(err, &de) && !ioerr.IsDiskFull(err) && !ioerr.IsTooLarge(err) && !ioerr.IsLocked(err):
		if st, serr := os.Stat(x.r.Dest); errors.Is(err, fs.ErrNotExist) || serr != nil || !st.IsDir() {
			return "", "", false, fmt.Errorf("stopped: the destination %s can't be reached any more (unplugged, or the network share dropped) — reconnect it, then run the same command again to continue (%w)", x.r.Dest, err)
		}
		return manifest.FAILED, "couldn't be written to the destination: " + err.Error(), false, nil
	case errors.Is(err, source.ErrTooManyRestarts):
		const notResponding = "the card stopped responding (skipped — run again to retry)"
		if x.giveUp {
			return manifest.FAILED, notResponding, false, nil
		}
		is := job.Issue{Kind: "card-not-responding", Severity: job.Warn,
			Title:   "The card stopped responding",
			Detail:  "Wait and keep trying, skip the remaining files (reported FAILED; run again later), or stop.",
			Choices: []job.Choice{job.Wait, SkipRest, job.Stop}, Default: notRespondingDefault(x.r.Interactive)}
		if x.r.wholeCard() {
			is.Detail = "Wait and keep trying, skip the remaining files (reported FAILED; run again later), make a full backup image instead (safer for a dying card), or stop."
			is.Choices = []job.Choice{job.Wait, SkipRest, job.Image, job.Stop}
		}
		c := x.ask(is)
		switch c {
		case job.Wait:
			x.r.src.ResetBudget()
			return "", "", true, nil
		case SkipRest:
			x.giveUp = true
			return manifest.FAILED, notResponding, false, nil
		case job.Image:
			return "", "", false, ErrImageInstead
		}
		return "", "", false, errors.New("stopped: the card stopped responding")
	case errors.Is(err, source.ErrDifferentCard):
		return "", "", false, source.ErrDifferentCard
	case errors.Is(err, source.ErrStalled):
		return manifest.STALLED, err.Error(), false, nil
	case errors.Is(err, source.ErrLocked) || ioerr.IsLocked(err):
		return manifest.LOCKED, err.Error(), false, nil
	case ioerr.IsDiskFull(err):
		c := x.ask(job.Issue{Kind: "disk-full", Severity: job.Warn, Title: "The destination is full",
			Detail:  "Free some space on the destination, then retry. Everything copied so far is safe.",
			Choices: []job.Choice{job.Retry, job.Stop}, Default: job.Stop})
		if c == job.Retry {
			x.retryWhy = "the destination stayed full"
			return "", "", true, nil
		}
		return "", "", false, errors.New("stopped: the destination is full")
	case ioerr.IsTooLarge(err):
		return manifest.FAILED, "destination file system can't hold files this large (FAT32 4 GiB limit?)", false, nil
	case errors.Is(err, source.ErrWorkerCrashed):
		return manifest.FAILED, "the card reader crashed on this file: " + err.Error(), false, nil
	}
	if gone, retry, gerr := x.cardGone(); gone || gerr != nil {
		return "", "", retry, gerr
	}
	if errors.Is(err, fs.ErrNotExist) {
		return manifest.FAILED, "file disappeared from the card", false, nil
	}
	return manifest.FAILED, err.Error(), false, nil
}

// cardGone checks whether the card was removed. If so it asks the user to reinsert it, waits for
// the same card (by identity) and points the reader at its new location: retry=true.
func (x *run) cardGone() (gone, retry bool, err error) {
	root, ok := x.r.Present(x.id, x.from)
	if ok && root == x.from {
		return false, false, nil
	}
	if !ok {
		c := x.ask(job.Issue{Kind: "card-removed", Severity: job.Warn, Title: "The card was removed — reinsert it",
			Detail:  "Put the same card back in; the copy continues where it stopped.",
			Choices: []job.Choice{job.Wait, job.Stop}, Default: job.Wait})
		if c != job.Wait {
			return true, false, errors.New("stopped: the card was removed")
		}
		werr := waitUntil(x.ctx, x.r.PollEvery, x.r.WaitTimeout, func() bool {
			root, ok = x.r.Present(x.id, x.from)
			return ok
		})
		if werr != nil {
			return true, false, fmt.Errorf("the card did not come back: %w", werr)
		}
	}
	// The card is back somewhere else: never write there either, and make sure the destination
	// didn't move with it (a hub reset can bring both back with their letters or mount points
	// swapped — the card could now be where the destination was).
	destfs.Protect(x.out, root)
	if preflight.Under(drives.RealPath(x.r.Dest), drives.RealPath(root)) ||
		x.destSerial != "" && drives.VolumeSerial(x.r.Dest) != x.destSerial {
		x.destMoved = true
		return true, false, errors.New("stopped: when the card came back, the destination folder was no longer on the same disk (drive letters or mount points changed) — check where both are, then run again")
	}
	x.from = root
	spec := x.r.spec()
	spec.From = root
	if err := x.r.src.Retarget(spec); err != nil {
		return true, false, err
	}
	x.log("info", "card is back at "+root)
	return true, true, nil
}

var waitUntil = retry.WaitUntil

// finish records a file's outcome. old is the row an earlier run left (nil if none).
func (x *run) finish(e manifest.Entry, st manifest.Status, msg string, old *manifest.Entry) (manifest.Status, error) {
	e.Status, e.Error = st, msg
	row := e
	if old != nil && e.Dest == "" && old.Dest != "" && manifest.Better(e, *old) && x.stored(old.Dest, old.Bytes) {
		if old.Bytes == e.Bytes && sameTime(old.SrcMTime, e.SrcMTime) {
			// Nothing better was stored this time and an earlier copy of this same file is still
			// on the destination: keep its row (status, copy, bad bytes) — it is what the
			// destination holds, and the next run must still compare against it — and note the
			// failure, worded so it stays true in later runs.
			row = *old
			row.Error = "a later attempt failed (" + msg + ") — the earlier copy is kept"
		} else {
			// The file changed on the card since that copy: it holds an older version, not this
			// file. Report this failure; the old copy stays on the destination untouched.
			row.Error = msg + " — an older version of this file is kept at " + old.Dest
		}
	}
	if err := x.m.Put(row); err != nil {
		return st, err
	}
	x.count(row.Status)
	x.sum.Add(row)
	if x.handled == nil {
		x.handled = map[string]bool{}
	}
	x.handled[e.Source] = true
	if st != manifest.OK && st != manifest.ALREADY_COPIED {
		x.log("warn", fmt.Sprintf("[%s] %s %s", st, e.Source, msg))
	}
	monSt, monBad := st, e.BadBytes
	if x.refused != nil {
		monSt, monBad, x.refused = manifest.PARTIAL, *x.refused, nil
	}
	if x.mon.file(monSt, e.Bytes, monBad) {
		left, bytes := 0, int64(0)
		for _, it := range x.r.selected { // not finished in this run, and not copied by an earlier one
			if g, _, _ := x.m.Get(it.Rel); !x.handled[it.Rel] && g.Status != manifest.OK {
				left++
				bytes += it.Size
			}
		}
		is := job.Issue{Kind: "card-degrading", Severity: job.Warn,
			Title:   fmt.Sprintf("The card is degrading — %s (%s) left", preflight.Files(left), preflight.GB(bytes)),
			Detail:  "Continue, skip the remaining videos (largest, slowest to read), or stop.",
			Choices: []job.Choice{job.Continue, SkipVideos, job.Stop}, Default: job.Continue}
		if x.r.wholeCard() {
			is.Detail = "Continue, skip the remaining videos (largest, slowest to read), make a full backup image instead, or stop."
			is.Choices = []job.Choice{job.Continue, SkipVideos, job.Image, job.Stop}
		}
		c := x.ask(is)
		switch c {
		case SkipVideos:
			x.skipVid = true
		case job.Image:
			return st, ErrImageInstead
		case job.Stop:
			return st, errors.New("stopped by you")
		}
	}
	return st, nil
}

// dropSuperseded removes a folder __PARTIAL copy an earlier run left once a better copy of the
// same file is safely in a finished ZIP part: it is our own file, and no row points at it any more.
func (x *run) dropSuperseded() {
	for stored, old := range x.replaced {
		_, _, inZip := archive.ZipPart(stored)
		_, _, oldInZip := archive.ZipPart(old.Dest)
		if !inZip || oldInZip || old.Status != manifest.PARTIAL || old.Dest == "" {
			continue
		}
		if cur, ok, _ := x.m.Get(old.Source); ok && cur.Dest == stored && archive.ZipIndexHas(x.st, stored, cur.Bytes) {
			x.out.Remove(filepath.Join(x.r.Dest, filepath.FromSlash(old.Dest)))
		}
	}
}

// markFailed turns entries whose ZIP part failed verification or couldn't be saved into FAILED
// rows — or back into the earlier run's row when that copy is still on the destination.
func (x *run) markFailed(stored []string) {
	if len(stored) == 0 {
		return
	}
	bad := map[string]bool{}
	for _, s := range stored {
		bad[s] = true
	}
	var fix []manifest.Entry
	x.m.Each(func(e manifest.Entry) error {
		if bad[e.Dest] {
			fix = append(fix, e)
		}
		return nil
	})
	const why = "the compressed part holding it could not be finished (failed verification or couldn't be saved) — run again to copy it again"
	for _, e := range fix {
		x.counts[string(e.Status)]--
		x.sum.Remove(e) // the summary must agree with the CSV
		n := e
		n.Status, n.Error, n.Dest, n.BadBytes, n.SHA256, n.Header = manifest.FAILED, why, "", 0, "", "" // no copy, no header check
		if old, ok := x.replaced[e.Dest]; ok && x.stored(old.Dest, old.Bytes) {
			n = old // the earlier copy is still there: never report less than what is on the destination
			n.Error = "a later copy failed (" + why + ") — the earlier copy is kept"
		}
		x.m.Put(n)
		x.count(n.Status)
		x.sum.Add(n)
	}
}

// writeReports writes manifest.json, the CSV and the summary. It returns the CSV's path ("" if
// it couldn't be written) and what failed, so the run never points at a report that isn't there.
func (x *run) writeReports(start time.Time) (string, error) {
	stamp := report.Stamp(x.r.Now())
	dir := x.r.reportsDir()
	csvPath := report.UniquePath(filepath.Join(dir, "copy_"+stamp+".csv"))
	var errs []error
	if err := x.m.ExportJSON(x.out, filepath.Join(dir, "manifest.json")); err != nil {
		errs = append(errs, fmt.Errorf("manifest.json: %w", err))
	}
	x.sum.CSV = csvPath
	if err := report.WriteCSV(x.out, csvPath, x.m.Each); err != nil {
		errs = append(errs, fmt.Errorf("%s: %w", filepath.Base(csvPath), err))
		x.sum.Issues = append(x.sum.Issues, "The list of every file (CSV) could not be written: "+err.Error())
		x.sum.CSV, csvPath = "not written (see above)", ""
	}
	x.sum.Title, x.sum.Source, x.sum.Dest, x.sum.Kind = "Copy", x.r.From, x.r.Dest, x.r.src.Kind()
	x.sum.Took, x.sum.Profiles = x.r.Now().Sub(start), x.r.inv.Profiles
	x.sum.Backup = x.r.Backup
	if x.sum.Backup == "" {
		x.sum.Backup = "Custom"
	}
	x.sum.Excluded = map[string]int{}
	x.sum.Kept = x.counts["KEPT-FROM-EARLIER-RUN"]
	for rel, why := range x.r.excluded {
		if x.kept[rel] {
			continue // kept from an earlier run, not excluded (shown as kept)
		}
		x.sum.Excluded[why]++
	}
	if err := x.sum.Write(x.out, report.UniquePath(filepath.Join(dir, "copy_summary_"+stamp+".txt"))); err != nil {
		errs = append(errs, fmt.Errorf("summary: %w", err))
	}
	return csvPath, errors.Join(errs...)
}
