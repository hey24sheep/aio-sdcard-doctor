// Package recovery is the "recover deleted files" action (Recuva-style): deleted files the file
// system still names (undelete) and files found by their signatures in free space (deep scan,
// our own carver). Every file is validated before it is written; a damaged one is only listed,
// with its path and the reason. Nothing is ever written to the card.
package recovery

import (
	"context"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/archive"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/carve"
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
	"github.com/hey24sheep/aio-sdcard-doctor/internal/rescue"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/salvage"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/scan"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/source"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/validate"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/worker"
)

// States shown in the results list (dot + word, never colour only).
const (
	Excellent     = "Excellent"
	Poor          = "Poor"
	Unrecoverable = "Unrecoverable"
)

// Row is one entry of the results list: a deleted file or a deep-scan find.
type Row struct {
	ID       string // "D:<path>" or "C:<offset>"
	Kind     string // "deleted" | "carved"
	Name     string
	Path     string // "?" for deep-scan finds (the name is gone)
	Size     int64
	MTime    time.Time
	State    string
	Comment  string
	Ext      string
	Cat      profile.Category
	Extents  [][2]int64 // volume byte ranges [offset, length]
	internal string     // the file system's state, or "complete"/"truncated" for a carve
}

// Source is what recover reads: the worker client (raw device or image).
type Source interface {
	source.Source
	OpenDevice() (io.ReaderAt, int64, io.Closer, error)
	Volumes() []fsread.Volume
	Caps() []job.Capability
	ReadError() string
	Close() error
}

// Recover is the action.
type Recover struct {
	From         string
	Volume       int
	Dest         string
	Types        []profile.Category // empty = every type
	MinSize      int64              // files smaller are listed, not extracted (CLI default 20 KB)
	SkipRescued  string             // a rescue's manifest.json: files with the same SHA-256 are DUPLICATE
	NoUndelete   bool
	Deep         bool
	Thorough     bool     // deep scan at every byte offset, not only sector starts
	States       []string // which states to extract (default Excellent, Poor)
	Select       []string // only these IDs or paths (from a --list CSV); empty = all
	List         bool     // index and write the list only
	Flat         bool     // "Restore folder structure" off: Undeleted/<Category>/<name>
	Compress     bool
	StallTimeout time.Duration

	// Seams.
	OpenSource  func(worker.Spec, time.Duration) (Source, error)
	LookupDrive func(string) (drives.Drive, error)
	Free        func(string) (uint64, error)
	Now         func() time.Time

	src          Source
	rows         []Row
	resumedIndex bool
}

func (r *Recover) Name() string { return "recover" }

func (r *Recover) defaults() {
	if r.OpenSource == nil {
		r.OpenSource = func(s worker.Spec, stall time.Duration) (Source, error) { return worker.Spawn(s, stall) }
	}
	if r.LookupDrive == nil {
		r.LookupDrive = drives.Lookup
	}
	if r.Free == nil {
		r.Free = destfs.Free
	}
	if r.Now == nil {
		r.Now = time.Now
	}
	if r.StallTimeout == 0 {
		r.StallTimeout = 30 * time.Second
	}
	if len(r.States) == 0 {
		r.States = []string{Excellent, Poor}
	}
}

// Rows is the results list of the last run.
func (r *Recover) Rows() []Row { return r.rows }

// Close ends the reader process.
func (r *Recover) Close() error {
	if r.src != nil {
		err := r.src.Close()
		r.src = nil
		return err
	}
	return nil
}

func (r *Recover) reportsDir() string { return filepath.Join(r.Dest, "Reports") }
func (r *Recover) storePath() string  { return filepath.Join(r.reportsDir(), "recover.db") }

func capOf(cs []job.Capability, name string) job.Capability {
	for _, c := range cs {
		if c.Name == name {
			return c
		}
	}
	return job.Capability{Name: name, Reason: "not available"}
}

// Plan opens the card read-only, says what can be found and checks the destination.
func (r *Recover) Plan(ctx context.Context, env job.Env) (job.Plan, error) {
	r.defaults()
	var p job.Plan
	if st, err := os.Stat(r.From); err == nil && st.IsDir() {
		hint := `the card's device, e.g. \\.\E: on Windows or /dev/sdb on Linux`
		if d, err := r.LookupDrive(r.From); err == nil && d.Device != "" {
			hint = d.Device
		}
		p.Issues = append(p.Issues, job.Issue{Kind: "needs-device", Severity: job.Block,
			Title:   "Recovering deleted files reads the card directly, not a folder",
			Detail:  "Choose the device (" + hint + ") or a card image file.",
			Choices: []job.Choice{job.Stop}, Default: job.Stop})
		return p, nil
	}
	if r.src == nil {
		src, err := r.OpenSource(worker.Spec{From: r.From, Raw: true, Volume: r.Volume}, r.StallTimeout)
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
	caps := r.src.Caps()
	undel := capOf(caps, "Undelete")
	if r.src.ReadError() != "" {
		undel = job.Capability{Name: "Undelete", Reason: "the file system can't be read — use Deep scan"}
	}
	p.Caps = []job.Capability{{Name: "Undelete", Enabled: undel.Enabled, Reason: undel.Reason},
		{Name: "Skip live files", Enabled: capOf(caps, "SkipLive").Enabled, Reason: capOf(caps, "SkipLive").Reason},
		{Name: "Deep scan", Enabled: true}}

	// destination checks (same rules as everywhere: never onto the card being read)
	srcDrive := drives.Drive{Removable: true}
	if rescue.IsDevicePath(r.From) {
		srcDrive.PhysicalID = drives.PhysicalOf(r.From)
	}
	dst, _ := r.LookupDrive(r.Dest)
	in := preflight.Input{Action: "recover", Src: srcDrive, Dst: dst, DstPath: r.Dest, Admin: elevate.IsAdmin(),
		NeedsAdmin: rescue.IsDevicePath(r.From)}
	if free, err := r.Free(r.Dest); err == nil {
		in.Dst.Free = free
	} else if free, err := r.Free(filepath.Dir(r.Dest)); err == nil {
		in.Dst.Free = free
	}

	if _, err := os.Stat(r.storePath()); err == nil {
		st, err := job.OpenStore(r.storePath())
		switch {
		case errors.Is(err, job.ErrBusy):
			p.Issues = append(p.Issues, job.Issue{Kind: "busy", Severity: job.Block, Title: job.ErrBusy.Error(),
				Choices: []job.Choice{job.Stop}, Default: job.Stop})
		case err == nil:
			if m, err := manifest.Open(st); err == nil && (m.Volume.FSSerial != "" || m.Volume.Model != "") {
				if ok, _, _ := m.Volume.Matches(r.src.Identity()); !ok {
					p.Issues = append(p.Issues, job.Issue{Kind: "dest-other-card", Severity: job.Block,
						Title: "This folder holds a recovery of a different card", Detail: "Choose a new folder.",
						Choices: []job.Choice{job.Stop}, Default: job.Stop})
				}
			}
			st.Close()
		}
	}
	counts := map[string]int{}
	var found int64
	var listErr error
	if undel.Enabled && !r.NoUndelete {
		if u, ok := r.src.(source.Undeleter); ok {
			listErr = u.Deleted(ctx, func(d source.Deleted) error {
				st, _ := mapState(d.State)
				counts[st]++
				if st != Unrecoverable {
					found += d.Size
					in.Items = append(in.Items, scan.Item{Rel: d.Path, Size: d.Size})
				}
				return nil
			})
		}
	}
	p.Issues = append(p.Issues, preflight.Check(in)...)
	if listErr != nil && !errors.Is(listErr, errors.ErrUnsupported) {
		p.Summary = append(p.Summary, "Note: the list of deleted files is incomplete — "+listErr.Error())
	}
	if _, err := os.Stat(r.SkipRescued); r.SkipRescued == "" || err != nil {
		p.Issues = append(p.Issues, job.Issue{Kind: "rescue-first", Severity: job.Warn,
			Title:   "Rescue first: it is faster and keeps names and folders",
			Detail:  "No rescue of this card was named (--skip-rescued); recovered files may repeat files that still exist.",
			Choices: []job.Choice{job.Continue, job.Stop}, Default: job.Continue})
	}
	p.Bytes, p.Items = found, counts[Excellent]+counts[Poor]

	what := []string{}
	if undel.Enabled && !r.NoUndelete {
		what = append(what, fmt.Sprintf("deleted files the card still names: %d (%d Excellent, %d Poor, %d Unrecoverable)",
			counts[Excellent]+counts[Poor]+counts[Unrecoverable], counts[Excellent], counts[Poor], counts[Unrecoverable]))
	} else if r.NoUndelete {
		what = append(what, "deleted files with names: not looked for (--no-undelete)")
	} else {
		what = append(what, "deleted files with names: not available — "+undel.Reason)
	}
	if r.Deep {
		mode := "at sector starts"
		if r.Thorough {
			mode = "at every byte (thorough, slower)"
		}
		what = append(what, "deep scan of free space for files by their signatures, "+mode)
	}
	types := "every type"
	if len(r.Types) > 0 {
		var s []string
		for _, t := range r.Types {
			s = append(s, string(t))
		}
		types = strings.Join(s, ", ")
	}
	p.Summary = append([]string{
		fmt.Sprintf("From: %s — read directly; nothing on it is changed", r.From),
		fmt.Sprintf("To:   %s", r.Dest),
		"Look for: " + strings.Join(what, "; "),
		fmt.Sprintf("Recover: %s states, %s%s", strings.Join(r.States, " + "), types, minNote(r.MinSize)),
	}, p.Summary...)
	if r.List {
		p.Summary = append(p.Summary, "List only: nothing is recovered (--list)")
		p.Result = []string{fmt.Sprintf("The results list (State, Name, Path, Size, Modified, Comment) as a CSV in %s", r.reportsDir()),
			"Pick lines from it and pass the file with --select to recover just those"}
		return p, nil
	}
	sinkNote := "Recovered files in " + filepath.Join(r.Dest, "Undeleted") + " (original folders) and " + filepath.Join(r.Dest, "Carved") + " (by type, named recovered_<offset>)"
	if r.Flat {
		sinkNote = "Recovered files in " + filepath.Join(r.Dest, "Undeleted") + " and " + filepath.Join(r.Dest, "Carved") + ", sorted by type"
	}
	if r.Compress {
		sinkNote = "Recovered files in ZIP files in " + r.Dest + " (Undeleted-0001.zip, Carved-0001.zip, …)"
	}
	p.Result = []string{sinkNote,
		"Every file is checked before it is written; a damaged one is not written, only listed with the reason",
		"Files that repeat a rescued or already-recovered file are counted as duplicates and not kept",
		fmt.Sprintf("A report of every file found (CSV) in %s", r.reportsDir()),
		"Stop any time; running the same command again continues where it stopped"}
	return p, nil
}

func minNote(n int64) string {
	if n <= 0 {
		return ""
	}
	return ", files of at least " + preflight.GB(n) + " (smaller ones are listed only)"
}

// mapState turns a file system's state into the results-list state and comment.
func mapState(fs string) (string, string) {
	switch fs {
	case "recoverable":
		return Excellent, "No overwritten clusters detected"
	case "fragmented-unknown":
		return Poor, "Some of its clusters were reused or it was fragmented — may come back damaged"
	case "overwritten":
		return Unrecoverable, "Overwritten"
	}
	return Unrecoverable, "Directory entry damaged"
}

func carvedState(complete bool) (string, string) {
	if complete {
		return Excellent, "Found by its signature in free space"
	}
	return Unrecoverable, "truncated — only the start of the file can be found"
}

type liveRun struct {
	path   string
	off, n int64
}

// annotate names the live file that reused a deleted file's space.
func annotate(rows []Row, live []liveRun) {
	sort.Slice(live, func(i, j int) bool { return live[i].off < live[j].off })
	for i := range rows {
		rw := &rows[i]
		if rw.internal != "overwritten" && rw.internal != "fragmented-unknown" {
			continue
		}
		var by []string
		for _, e := range rw.Extents {
			j := sort.Search(len(live), func(k int) bool { return live[k].off+live[k].n > e[0] })
			for ; j < len(live) && live[j].off < e[0]+e[1] && len(by) < 3; j++ {
				if len(by) == 0 || by[len(by)-1] != live[j].path {
					by = append(by, live[j].path)
				}
			}
		}
		if len(by) == 0 {
			continue
		}
		if rw.internal == "overwritten" {
			rw.Comment = "Overwritten by " + strings.Join(by, ", ")
		} else {
			rw.Comment = "Partly overwritten by " + strings.Join(by, ", ") + " — may come back damaged"
		}
	}
}

const (
	bucketDeep  = "deep_index"
	bucketState = "deep_state"
	bucketBad   = "deep_unreadable"
)

type storedHit struct {
	Offset, Size int64
	Ext          string
	Cat          profile.Category
	Complete     bool
	Unreadable   bool
}

func (r *Recover) index(ctx context.Context, env job.Env, st *job.Store, dev io.ReaderAt, volOff, volSize int64) error {
	r.rows = nil
	var live []liveRun
	if lm, ok := r.src.(source.LiveMapper); ok {
		err := lm.LiveExtents(ctx, func(p string, off, n int64) error {
			live = append(live, liveRun{p, off, n})
			return nil
		})
		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case err != nil && !errors.Is(err, errors.ErrUnsupported) && env.Log != nil:
			env.Log("warn", "Where live files sit is only partly known ("+err.Error()+"): deep scan may also list live files")
		}
	}
	undel := capOf(r.src.Caps(), "Undelete")
	if undel.Enabled && !r.NoUndelete && r.src.ReadError() == "" {
		if u, ok := r.src.(source.Undeleter); ok {
			err := u.Deleted(ctx, func(d source.Deleted) error {
				state, comment := mapState(d.State)
				ext := strings.TrimPrefix(strings.ToLower(path.Ext(d.Path)), ".")
				cat, ok := profile.ByExt(ext)
				if !ok {
					cat = profile.Other
				}
				r.rows = append(r.rows, Row{ID: "D:" + d.Path, Kind: "deleted", Name: path.Base(d.Path), Path: d.Path,
					Size: d.Size, MTime: d.MTime, State: state, Comment: comment, Ext: ext, Cat: cat,
					Extents: d.Extents, internal: d.State})
				return nil
			})
			if errors.Is(err, worker.ErrListCapped) && env.Log != nil {
				env.Log("warn", err.Error()+"; the files listed so far are used")
			} else if err != nil && !errors.Is(err, errors.ErrUnsupported) {
				return err
			}
		}
	}
	r.rows = dedupe(r.rows)
	annotate(r.rows, live)
	if !r.Deep {
		return nil
	}
	opt := carve.Options{Align: 512}
	if r.Thorough {
		opt.Align = 1
	}
	// the saved index belongs to one card, volume and scan mode; anything else starts over
	key := fmt.Sprintf("%s|%d|%d|%d", r.src.Identity().LockKey(), opt.Align, volOff, volSize)
	if k, _, _ := job.Get[string](st, bucketState, "key"); k != key {
		job.ClearBucket(st, bucketDeep)
		job.ClearBucket(st, bucketBad)
		job.Put(st, bucketState, "pos", int64(0))
		if err := job.Put(st, bucketState, "key", key); err != nil {
			return err
		}
	}
	pos, _, _ := job.Get[int64](st, bucketState, "pos")
	r.resumedIndex = pos > 0
	seen := map[int64]bool{}
	job.ForEach(st, bucketDeep, func(_ string, h storedHit) error {
		seen[h.Offset] = true
		r.rows = append(r.rows, carvedRow(h))
		return nil
	})
	job.ForEach(st, bucketBad, func(_ string, a [2]int64) error {
		r.rows = append(r.rows, unreadableRow(a[0], a[1]))
		return nil
	})
	if pos >= volSize {
		return nil
	}
	var storeErr error
	opt.Unreadable = func(off, n int64) {
		if err := job.Put(st, bucketBad, fmt.Sprintf("%016x", off), [2]int64{off, n}); err != nil && storeErr == nil {
			storeErr = err
		}
		r.rows = append(r.rows, unreadableRow(off, n))
	}
	if capOf(r.src.Caps(), "SkipLive").Enabled {
		for _, l := range live {
			opt.Skip = append(opt.Skip, [2]int64{l.off, l.off + l.n})
		}
	}
	vol := io.NewSectionReader(dev, volOff, volSize)
	err := carve.Scan(ctx, vol, volSize, opt, pos, func(h carve.Hit) error {
		if seen[h.Offset] { // every type is kept: --types is applied when recovering (EXCLUDED rows)
			return nil
		}
		sh := storedHit{Offset: h.Offset, Size: h.Size, Ext: h.Ext, Cat: h.Cat, Complete: h.Complete, Unreadable: h.Unreadable}
		if err := job.Put(st, bucketDeep, fmt.Sprintf("%016x", h.Offset), sh); err != nil {
			return err
		}
		r.rows = append(r.rows, carvedRow(sh))
		return nil
	}, func(p int64) error {
		if env.Progress != nil {
			env.Progress(job.Progress{Phase: "Deep scan", Done: p, Total: volSize})
		}
		return job.Put(st, bucketState, "pos", p)
	})
	if err == nil {
		err = storeErr
	}
	return err
}

func unreadableRow(off, n int64) Row {
	return Row{ID: "U:" + strconv.FormatInt(off, 10), Kind: "unreadable", Name: fmt.Sprintf("[unreadable area at byte %d]", off),
		Path: "?", Size: n, State: Unrecoverable, Cat: profile.Other, Extents: [][2]int64{{off, n}},
		Comment: fmt.Sprintf("%d bytes could not be read, so they were not searched", n), internal: "unreadable"}
}

func carvedRow(h storedHit) Row {
	state, comment := carvedState(h.Complete)
	if h.Unreadable {
		comment = "an area inside it could not be read — only the start was found"
	}
	internal := "complete"
	if !h.Complete {
		internal = "truncated"
	}
	return Row{ID: "C:" + strconv.FormatInt(h.Offset, 10), Kind: "carved",
		Name: fmt.Sprintf("[recovered_%d].%s", h.Offset, h.Ext), Path: "?", Size: h.Size, State: state,
		Comment: fmt.Sprintf("%s (at byte %d)", comment, h.Offset), Ext: h.Ext, Cat: h.Cat,
		Extents: [][2]int64{{h.Offset, h.Size}}, internal: internal}
}

func hasCat(cs []profile.Category, c profile.Category) bool {
	for _, x := range cs {
		if x == c {
			return true
		}
	}
	return false
}

// validator checks a found file in the reader process.
type validator interface {
	Validate(ext string, volOff int64, extents [][2]int64, size int64) (bool, string, error)
}

func (r *Recover) rescuedSums() map[string]bool {
	out := map[string]bool{}
	if r.SkipRescued == "" {
		return out
	}
	b, err := os.ReadFile(r.SkipRescued)
	if err != nil {
		return out
	}
	var m struct{ Entries []manifest.Entry }
	if json.Unmarshal(b, &m) == nil {
		for _, e := range m.Entries {
			if e.SHA256 != "" {
				out[strings.ToUpper(e.SHA256)] = true
			}
		}
	}
	return out
}

// destParts is where a row goes before sanitising: a pseudo-category folder and a relative name.
func (r *Recover) destParts(rw Row) (profile.Category, string) {
	if rw.Kind == "carved" || rw.Kind == "unreadable" {
		return profile.Category("Carved/" + string(rw.Cat)), fmt.Sprintf("recovered_%d.%s", rw.Extents[0][0], rw.Ext)
	}
	if r.Flat {
		return profile.Category("Undeleted/" + string(rw.Cat)), path.Base(rw.Path)
	}
	return "Undeleted", rw.Path
}

// plan gives every row a safe, unique destination (names come from a possibly damaged card:
// ".." and reserved characters are cleaned, colliding names get a short hash, long paths shortened).
func (r *Recover) plan(rows []Row) []string {
	pl := paths.NewPlanner(r.Dest, paths.MaxPath)
	for _, rw := range rows {
		pl.Add(r.destParts(rw))
	}
	out := make([]string, len(rows))
	for i, rw := range rows {
		out[i] = pl.Plan(r.destParts(rw))
	}
	return out
}

// dedupe gives deleted files that read back with the same path (FAT loses a deleted name's
// first letter: GONE.JPG and BONE.JPG both become _ONE.JPG) distinct paths and IDs.
func dedupe(rows []Row) []Row {
	n := map[string]int{}
	for i := range rows {
		rw := &rows[i]
		if rw.Kind != "deleted" {
			continue
		}
		k := strings.ToLower(rw.Path)
		n[k]++
		if c := n[k]; c > 1 {
			e := path.Ext(rw.Path)
			rw.Path = fmt.Sprintf("%s (%d)%s", strings.TrimSuffix(rw.Path, e), c, e)
			rw.Name = path.Base(rw.Path)
		}
		rw.ID = "D:" + rw.Path
	}
	return rows
}

func (r *Recover) key(rw Row) string {
	if rw.Kind == "unreadable" {
		return "unreadable@" + strings.TrimPrefix(rw.ID, "U:")
	}
	if rw.Kind == "carved" {
		return "carved@" + strings.TrimPrefix(rw.ID, "C:")
	}
	return strings.TrimPrefix(rw.ID, "D:")
}

// why returns "" when the row is to be extracted, else why it is not.
func (r *Recover) why(rw Row, sel map[string]bool) string {
	switch {
	case len(sel) > 0 && !sel[rw.ID] && !sel[rw.Path]:
		return "not selected"
	case !contains(r.States, rw.State):
		return rw.State + " files not chosen (--states)"
	case len(r.Types) > 0 && !hasCat(r.Types, rw.Cat):
		return "type not chosen"
	case rw.Size < r.MinSize:
		return "smaller than " + preflight.GB(r.MinSize) + " (--min-size)"
	}
	return ""
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if strings.EqualFold(x, s) {
			return true
		}
	}
	return false
}

// Run indexes (resumable), then extracts the chosen files, each validated first.
func (r *Recover) Run(ctx context.Context, env job.Env, p job.Plan) (job.Result, error) {
	r.defaults()
	res := job.Result{Counters: map[string]int{}, Clean: true}
	var roots []string
	if !rescue.IsDevicePath(r.From) {
		roots = append(roots, r.From) // an image file: never written
	}
	out := env.Out
	if out == nil {
		out = destfs.New(r.Dest, roots...)
	}
	if err := out.MkdirAll(r.reportsDir()); err != nil {
		return res, err
	}
	st, err := job.OpenStore(r.storePath())
	if err != nil {
		return res, err
	}
	defer st.Close()
	m, err := manifest.Open(st)
	if err != nil {
		return res, err
	}
	m.Volume, m.SourceKind = r.src.Identity(), "recover"
	m.SaveMeta()
	unlock, err := job.LockSource(r.src.Identity().LockKey())
	if err != nil {
		return res, err
	}
	defer unlock()
	if release, err := job.Inhibit("recovering files from a card"); err == nil {
		defer release()
	}
	dev, devSize, closer, err := r.src.OpenDevice()
	if err != nil {
		return res, fmt.Errorf("reading the card directly: %w", err)
	}
	defer closer.Close()
	volOff, volSize := int64(0), devSize
	if vs := r.src.Volumes(); r.src.ReadError() == "" && r.Volume < len(vs) {
		volOff, volSize = vs[r.Volume].Offset, vs[r.Volume].Size
	}
	if err := r.index(ctx, env, st, dev, volOff, volSize); err != nil {
		return res, err
	}
	stamp := report.Stamp(r.Now())
	if r.List {
		p, err := r.writeList(out, stamp)
		if err != nil {
			return res, err
		}
		for _, rw := range r.rows {
			res.Counters[rw.State]++
		}
		res.Reports = append(res.Reports, p)
		return res, nil
	}

	sel := map[string]bool{}
	for _, s := range r.Select {
		sel[s] = true
	}
	rescued := r.rescuedSums()
	seen := map[string]string{} // SHA-256 → the entry that holds it
	m.Each(func(e manifest.Entry) error {
		if e.SHA256 != "" && (e.Status == manifest.DELETED_RECOVERED || e.Status == manifest.CARVED) {
			seen[e.SHA256] = e.Source
		}
		return nil
	})
	var sink archive.Sink = archive.FolderSink(out, r.Dest)
	if r.Compress {
		sink = archive.ZipSink(out, r.Dest, st)
	}
	cp := salvage.NewCopier()
	dests := r.plan(r.rows)
	// a FAT destination holds files up to 4 GiB; a ZIP part never splits one file, so Compress
	// doesn't help
	fatDest := false
	if d, err := r.LookupDrive(r.Dest); err == nil {
		fatDest = preflight.IsFAT(d.FS)
	}
	for i, rw := range r.rows {
		if err := ctx.Err(); err != nil {
			break
		}
		if env.Progress != nil {
			env.Progress(job.Progress{Phase: "Recovering", Item: dests[i], Items: i, ItemsTotal: len(r.rows)})
		}
		e := manifest.Entry{Source: r.key(rw), Category: rw.Cat, Bytes: rw.Size, SrcMTime: rw.MTime, MTime: rw.MTime}
		damaged := manifest.DELETED_UNRECOVERABLE
		good := manifest.DELETED_RECOVERED
		switch rw.Kind {
		case "carved":
			damaged, good = manifest.CARVED_DAMAGED, manifest.CARVED
		case "unreadable":
			damaged = manifest.UNREADABLE
		}
		if old, ok, _ := m.Get(e.Source); ok && old.Status == good && r.stillStored(st, old) {
			res.Counters[string(manifest.ALREADY_COPIED)]++
			continue
		}
		if rw.State == Unrecoverable {
			e.Status, e.Error = damaged, rw.Comment
		} else if why := r.why(rw, sel); why != "" {
			e.Status, e.Error = manifest.EXCLUDED, why
		} else if fatDest && rw.Size > fatMaxFile {
			e.Status, e.Error, res.Clean = manifest.FAILED, tooLargeForFAT, false
		} else {
			var err error
			e.Status, e.Error, e.Dest, e.SHA256, err = r.extract(ctx, rw, dests[i], e.Source, dev, volOff, sink, cp, rescued, seen, good, damaged)
			if err != nil && ioerr.IsTooLarge(err) {
				e.Status, e.Error, e.Dest, e.SHA256, err = manifest.FAILED, tooLargeForFAT, "", "", nil
				res.Clean = false
			}
			if err != nil {
				sink.Close()
				return res, fmt.Errorf("writing to the destination: %w", err)
			}
			if e.Status == damaged {
				res.Clean = false
			}
			if e.Status == good && rw.State == Poor {
				e.Error = strings.TrimSuffix("Poor: "+rw.Comment+"; "+e.Error, "; ")
			}
		}
		res.Counters[string(e.Status)]++
		if err := m.Put(e); err != nil {
			return res, err
		}
	}
	sink.Close()
	if zs, ok := sink.(interface{ Failed() []string }); ok && len(zs.Failed()) > 0 {
		res.Clean = false
	}
	csvPath := report.UniquePath(filepath.Join(r.reportsDir(), "recover_"+stamp+".csv"))
	if err := report.WriteCSV(out, csvPath, m.Each); err == nil {
		res.Reports = append(res.Reports, csvPath)
	}
	m.ExportJSON(out, filepath.Join(r.reportsDir(), "recover_manifest.json"))
	if ctx.Err() != nil {
		return res, ctx.Err()
	}
	return res, nil
}

// extract validates one file on the card and, if it passes, writes it.
// stillStored: an earlier run's copy is still where it was recorded (a folder file or a ZIP entry).
func (r *Recover) stillStored(st *job.Store, old manifest.Entry) bool {
	if old.Dest == "" {
		return false
	}
	if strings.Contains(old.Dest, ".zip/") {
		return archive.ZipIndexHas(st, old.Dest, old.Bytes)
	}
	fi, err := os.Stat(filepath.Join(r.Dest, filepath.FromSlash(old.Dest)))
	return err == nil && fi.Size() == old.Bytes
}

// extract validates one file on the card and, if it passes, writes it. err is a destination
// failure (the run stops); everything about the card's data is in the status and reason.
func (r *Recover) extract(ctx context.Context, rw Row, dest, key string, dev io.ReaderAt, volOff int64, sink archive.Sink,
	cp *salvage.Copier, rescued map[string]bool, seen map[string]string, good, damaged manifest.Status) (manifest.Status, string, string, string, error) {
	if len(rw.Extents) == 0 {
		return damaged, "no data location recorded", "", "", nil
	}
	src := source.ExtentReader{Dev: dev, Off: volOff, Extents: rw.Extents}
	ok, why := false, ""
	if v, isV := r.src.(validator); isV { // untrusted decoding runs in the worker (R17)
		var err error
		if ok, why, err = v.Validate(rw.Ext, volOff, rw.Extents, rw.Size); err != nil {
			ok, why = false, "the file could not be checked: "+err.Error()
		}
	} else if ok, why = validate.Check(rw.Ext, src, rw.Size); ok {
		note := why
		if ok, why = carve.Verify(rw.Ext, src, rw.Size); ok {
			why = note
		}
	}
	if !ok {
		return damaged, why, "", "", nil
	}
	var dup bool
	var bad int64
	stored, res, err := sink.Put(ctx, dest, rw.MTime,
		func(w io.Writer) (salvage.Result, error) { return cp.Copy(ctx, src, rw.Size, w) },
		func(res salvage.Result) bool {
			sum := strings.ToUpper(hex.EncodeToString(res.Sum[:]))
			bad = res.Bad
			dup = rescued[sum] || seen[sum] != "" && seen[sum] != key
			return res.Bad == 0 && !dup
		})
	sum := strings.ToUpper(hex.EncodeToString(res.Sum[:]))
	switch {
	case err != nil:
		return "", "", "", "", err
	case bad > 0:
		return damaged, fmt.Sprintf("%d bytes could not be read", bad), "", sum, nil
	case dup:
		return manifest.DUPLICATE, "same content as a rescued or recovered file", "", sum, nil
	}
	seen[sum] = key
	return good, why, stored, sum, nil // why: a note on a file that passed (validate.NotDecoded)
}

// writeList writes the results list as a CSV.
func (r *Recover) writeList(out destfs.FS, stamp string) (string, error) {
	var b strings.Builder
	b.WriteString("\xef\xbb\xbf")
	w := csv.NewWriter(&b)
	w.Write([]string{"State", "Name", "Path", "Size", "Modified", "Comment", "ID"})
	for _, rw := range r.rows {
		mt := ""
		if !rw.MTime.IsZero() {
			mt = rw.MTime.Format("2006-01-02 15:04:05")
		}
		w.Write([]string{rw.State, rw.Name, rw.Path, strconv.FormatInt(rw.Size, 10), mt, rw.Comment, rw.ID})
	}
	w.Flush()
	p := report.UniquePath(filepath.Join(r.reportsDir(), "recover_list_"+stamp+".csv"))
	return p, destfs.WriteFileAtomic(out, p, []byte(b.String()))
}

// ReadSelection reads IDs (or paths) from a --list CSV the user trimmed, or one per line.
func ReadSelection(p string) ([]string, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	s := strings.TrimPrefix(string(b), "\xef\xbb\xbf")
	recs, err := csv.NewReader(strings.NewReader(s)).ReadAll()
	if err != nil || len(recs) == 0 || len(recs[0]) < 7 || recs[0][0] != "State" {
		var out []string
		for _, l := range strings.Split(s, "\n") {
			if l = strings.TrimSpace(l); l != "" {
				out = append(out, l)
			}
		}
		return out, nil
	}
	var out []string
	for _, rec := range recs[1:] {
		if len(rec) >= 7 {
			out = append(out, rec[6])
		}
	}
	return out, nil
}

const tooLargeForFAT = "too large for the destination's FAT32 file system (over 4 GiB) — recover to an exFAT or NTFS disk"

// fatMaxFile is the largest file a FAT32 destination holds (a variable for tests).
var fatMaxFile = int64(preflight.FATMaxFile)
