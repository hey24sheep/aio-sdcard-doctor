// Package verify re-checks a rescue (or a new card the rescue was copied to) against the
// SHA-256 of every file, resumably and read-only on the target. It also reads the reference
// PowerShell copy_*.csv reports, compressed rescues (ZIP parts or their extracted contents) and
// card images.
package verify

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ulikunitz/xz"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/destfs"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/drives"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/job"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/manifest"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/preflight"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/report"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/retry"
)

// Verify is the Verify action.
type Verify struct {
	Manifest string // manifest.json, or a legacy copy_*.csv
	Target   string // where the files are now (default: the rescue folder)
	Restart  bool
	Image    string // verify card.img (or card.img.xz) against card.img.json instead
	Reports  string // where progress and reports go (default: next to the manifest)

	// Seams.
	LookupDrive func(string) (drives.Drive, error)
	MaxFile     int64 // FAT warning threshold (4 GiB − 1)
	Now         func() time.Time
	WaitTimeout time.Duration

	items   []item
	reports string
}

type item struct {
	Source, Dest string
	Bytes        int64
	SHA          string
}

// Result row.
type row struct {
	Status, Path string
	Bytes        int64
	Error        string
}

func (v *Verify) Name() string { return "verify" }

func (v *Verify) defaults() {
	if v.LookupDrive == nil {
		v.LookupDrive = drives.Lookup
	}
	if v.MaxFile == 0 {
		v.MaxFile = preflight.FATMaxFile
	}
	if v.Now == nil {
		v.Now = time.Now
	}
}

var recoveredPrefix = regexp.MustCompile(`^Recovered[\\/]`)

// load reads manifest.json or a legacy PS copy CSV (later rows win, like Verify-NewCard.ps1).
func load(p string) ([]item, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	b = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))
	var out []item
	if strings.EqualFold(filepath.Ext(p), ".csv") {
		rows, err := csv.NewReader(bytes.NewReader(b)).ReadAll()
		if err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			return nil, errors.New("empty report")
		}
		col := map[string]int{}
		for i, h := range rows[0] {
			col[h] = i
		}
		for _, need := range []string{"Status", "Dest", "Bytes", "SHA256"} {
			if _, ok := col[need]; !ok {
				return nil, fmt.Errorf("%s: no %s column", filepath.Base(p), need)
			}
		}
		byDest := map[string]item{}
		for _, r := range rows[1:] {
			if len(r) < len(rows[0]) || (r[col["Status"]] != "OK" && r[col["Status"]] != "PARTIAL") {
				continue
			}
			d := strings.ReplaceAll(recoveredPrefix.ReplaceAllString(r[col["Dest"]], ""), `\`, "/")
			n, _ := strconv.ParseInt(r[col["Bytes"]], 10, 64)
			src := d
			if i, ok := col["Source"]; ok {
				src = r[i]
			}
			byDest[d] = item{Source: src, Dest: d, Bytes: n, SHA: r[col["SHA256"]]}
		}
		for _, it := range byDest {
			out = append(out, it)
		}
	} else {
		var doc struct{ Entries []manifest.Entry }
		if err := json.Unmarshal(b, &doc); err != nil {
			return nil, err
		}
		for _, e := range doc.Entries {
			switch e.Status {
			case manifest.OK, manifest.PARTIAL, manifest.DELETED_RECOVERED, manifest.CARVED:
				out = append(out, item{Source: e.Source, Dest: e.Dest, Bytes: e.Bytes, SHA: e.SHA256})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Dest < out[j].Dest })
	return out, nil
}

// Plan loads the manifest and checks the target.
func (v *Verify) Plan(ctx context.Context, env job.Env) (job.Plan, error) {
	v.defaults()
	var p job.Plan
	if v.Image != "" {
		return v.planImage()
	}
	items, err := load(v.Manifest)
	if err != nil {
		return p, fmt.Errorf("reading %s: %w", v.Manifest, err)
	}
	v.items, v.reports = items, filepath.Dir(v.Manifest)
	if v.Reports != "" {
		v.reports = v.Reports
	}
	if v.Target == "" {
		if strings.EqualFold(filepath.Ext(v.Manifest), ".csv") {
			return p, errors.New("a legacy report needs --target (where the files are now)")
		}
		v.Target = filepath.Dir(filepath.Dir(v.Manifest))
	}
	if _, err := os.Stat(v.Target); err != nil {
		p.Issues = append(p.Issues, job.Issue{Kind: "no-target", Severity: job.Block, Title: "Can't open the target: " + err.Error(),
			Choices: []job.Choice{job.Stop}, Default: job.Stop})
	}
	if d, err := v.LookupDrive(v.Target); err == nil && preflightFAT(d.FS) {
		var big []string
		for _, it := range items {
			if it.Bytes > v.MaxFile {
				big = append(big, it.Dest)
			}
		}
		if len(big) > 0 {
			p.Issues = append(p.Issues, job.Issue{Kind: "fat-target", Severity: job.Warn,
				Title:   fmt.Sprintf("The target is FAT32: %d files are too big to be on it", len(big)),
				Detail:  strings.Join(big[:min(10, len(big))], ", "),
				Choices: []job.Choice{job.Continue, job.Stop}, Default: job.Continue})
		}
	}
	for _, it := range items {
		p.Bytes += it.Bytes
	}
	p.Items = len(items)
	if !v.Restart {
		if s, err := job.OpenStore(v.storePath()); err == nil {
			done := 0
			job.ForEach(s, v.bucket(), func(string, row) error { done++; return nil })
			s.Close()
			if done > 0 && done < len(items) {
				p.Resume = &job.ResumeInfo{Done: done, Total: len(items)}
			} else if done >= len(items) {
				v.Restart = true // the last run finished: check everything again (I9)
			}
		}
	}
	p.Summary = []string{
		fmt.Sprintf("Check: %s files (%s) listed in %s", preflight.Commas(len(items)), preflight.GB(p.Bytes), v.Manifest),
		fmt.Sprintf("In:    %s — only read, nothing there is changed", v.Target),
	}
	p.Result = []string{
		"For every file: present, same size, same SHA-256 as when it was rescued",
		fmt.Sprintf("A report (verify_<time>.csv and a summary) in %s", v.reports),
	}
	return p, nil
}

func preflightFAT(fsName string) bool {
	switch strings.ToUpper(fsName) {
	case "FAT", "FAT12", "FAT16", "FAT32", "VFAT", "MSDOS":
		return true
	}
	return false
}

func (v *Verify) storePath() string { return filepath.Join(v.reports, "verify.db") }
func (v *Verify) bucket() string {
	abs, _ := filepath.Abs(v.Target)
	return "verify|" + abs
}

// Run checks every file (skipping those checked by an interrupted earlier run).
func (v *Verify) Run(ctx context.Context, env job.Env, p job.Plan) (job.Result, error) {
	v.defaults()
	for _, is := range p.Issues {
		if is.Severity == job.Block {
			return job.Result{}, fmt.Errorf("can't start: %s", is.Title)
		}
	}
	if v.Image != "" {
		return v.runImage(ctx)
	}
	out := env.Out
	if out == nil {
		// Writes go only into the reports folder (destination root). The target is protected as a
		// source too, unless the reports live inside it (verifying the rescue folder itself).
		var protect []string
		if rel, err := filepath.Rel(v.Target, v.reports); err != nil || !filepath.IsLocal(rel) {
			protect = append(protect, v.Target)
		}
		out = destfs.New(v.reports, protect...)
	}
	st, err := job.OpenStore(v.storePath())
	if err != nil {
		return job.Result{}, err
	}
	defer st.Close()
	if v.Restart {
		job.ClearBucket(st, v.bucket())
	}
	zips := map[string]*zip.ReadCloser{}
	defer func() {
		for _, z := range zips {
			z.Close()
		}
	}()
	counts := map[string]int{}
	var rows []row
	thr := job.Throttle{Every: 100 * time.Millisecond}
	var done int64
	for i, it := range v.items {
		if err := ctx.Err(); err != nil {
			return job.Result{Counters: counts}, err
		}
		r, ok, _ := job.Get[row](st, v.bucket(), it.Dest)
		if !ok || r.Status != "OK" { // only files already found OK are not read again
			r = v.check(it, zips)
			if (r.Status == "MISSING" || r.Status == "UNREADABLE") && !v.targetPresent() {
				if err := v.waitTarget(ctx, env); err != nil {
					return job.Result{Counters: counts}, err
				}
				r = v.check(it, zips)
			}
			if err := job.Put(st, v.bucket(), it.Dest, r); err != nil {
				return job.Result{Counters: counts}, err
			}
		}
		counts[r.Status]++
		rows = append(rows, r)
		done += it.Bytes
		if env.Progress != nil && thr.Ok() {
			env.Progress(job.Progress{Phase: "Verifying", Item: it.Dest, Done: done, Total: p.Bytes, Items: i + 1, ItemsTotal: len(v.items)})
		}
	}
	csvPath, err := v.writeReports(out, rows, counts, p.Bytes)
	return job.Result{Counters: counts, Clean: counts["OK"] == len(v.items), Reports: []string{csvPath}}, err
}

func (v *Verify) targetPresent() bool {
	_, err := os.Stat(v.Target)
	return err == nil
}

func (v *Verify) waitTarget(ctx context.Context, env job.Env) error {
	var c job.Choice = job.Wait
	if env.Prompt != nil {
		c = env.Prompt.Ask(ctx, job.Issue{Kind: "target-removed", Severity: job.Warn, Title: "The target was removed — reinsert it",
			Choices: []job.Choice{job.Wait, job.Stop}, Default: job.Wait})
	}
	if c != job.Wait {
		return errors.New("stopped: the target was removed")
	}
	timeout := v.WaitTimeout
	if timeout == 0 {
		timeout = 10 * time.Minute
	}
	return retry.WaitUntil(ctx, time.Second, timeout, v.targetPresent)
}

var partRe = regexp.MustCompile(`^(.+)-\d{4,}\.zip$`)

// check verifies one file: exists, same size, same SHA-256.
func (v *Verify) check(it item, zips map[string]*zip.ReadCloser) row {
	r := row{Path: it.Dest, Bytes: it.Bytes}
	if part, rest, ok := strings.Cut(it.Dest, ".zip/"); ok {
		part += ".zip"
		zp := filepath.Join(v.Target, filepath.FromSlash(part))
		if _, err := os.Stat(zp); err == nil {
			return v.checkZip(r, zp, rest, it, zips)
		}
		cands := []string{rest}
		if m := partRe.FindStringSubmatch(part); m != nil {
			cands = append(cands, m[1]+"/"+rest)
		}
		for _, c := range cands {
			p := filepath.Join(v.Target, filepath.FromSlash(c))
			if _, err := os.Stat(p); err == nil {
				return v.checkFile(r, p, it)
			}
		}
		r.Status = "MISSING"
		return r
	}
	return v.checkFile(r, filepath.Join(v.Target, filepath.FromSlash(it.Dest)), it)
}

func hashReader(rd io.Reader) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, rd); err != nil {
		return "", err
	}
	return fmt.Sprintf("%X", h.Sum(nil)), nil
}

func (v *Verify) checkFile(r row, p string, it item) row {
	f, err := os.Open(p)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		r.Status = "MISSING"
		return r
	case err != nil:
		r.Status, r.Error = "UNREADABLE", err.Error()
		return r
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		r.Status, r.Error = "UNREADABLE", err.Error()
		return r
	}
	if st.Size() != it.Bytes {
		r.Status, r.Error = "WRONG-SIZE", fmt.Sprintf("target %d, expected %d", st.Size(), it.Bytes)
		return r
	}
	sum, err := hashReader(f)
	if err != nil {
		r.Status, r.Error = "UNREADABLE", err.Error()
		return r
	}
	r.Status = "OK"
	if !strings.EqualFold(sum, it.SHA) {
		r.Status = "CORRUPT"
	}
	return r
}

func (v *Verify) checkZip(r row, zp, name string, it item, zips map[string]*zip.ReadCloser) row {
	z, ok := zips[zp]
	if !ok {
		var err error
		if z, err = zip.OpenReader(zp); err != nil {
			r.Status, r.Error = "UNREADABLE", err.Error()
			return r
		}
		zips[zp] = z
	}
	for _, f := range z.File {
		if f.Name != name {
			continue
		}
		if int64(f.UncompressedSize64) != it.Bytes {
			r.Status, r.Error = "WRONG-SIZE", fmt.Sprintf("archive %d, expected %d", f.UncompressedSize64, it.Bytes)
			return r
		}
		rc, err := f.Open()
		if err != nil {
			r.Status, r.Error = "UNREADABLE", err.Error()
			return r
		}
		sum, err := hashReader(rc)
		rc.Close()
		if err != nil {
			r.Status, r.Error = "UNREADABLE", err.Error()
			return r
		}
		r.Status = "OK"
		if !strings.EqualFold(sum, it.SHA) {
			r.Status = "CORRUPT"
		}
		return r
	}
	r.Status = "MISSING"
	return r
}

func (v *Verify) writeReports(out destfs.FS, rows []row, counts map[string]int, total int64) (string, error) {
	stamp := report.Stamp(v.Now())
	csvPath := report.UniquePath(filepath.Join(v.reports, "verify_"+stamp+".csv"))
	var b bytes.Buffer
	b.WriteString("\xef\xbb\xbf")
	w := csv.NewWriter(&b)
	w.Write([]string{"Status", "Path", "Bytes", "Error"})
	for _, r := range rows {
		w.Write([]string{r.Status, r.Path, strconv.FormatInt(r.Bytes, 10), r.Error})
	}
	w.Flush()
	if err := destfs.WriteFileAtomic(out, csvPath, b.Bytes()); err != nil {
		return "", err
	}
	var s strings.Builder
	fmt.Fprintf(&s, "Verification  %s\nTarget: %s   Files checked: %d   (%s)\n\n", v.Now().Format("2006-01-02 15:04:05"), v.Target, len(rows), preflight.GB(total))
	var keys []string
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&s, "  %-12s %8d\n", k, counts[k])
	}
	if counts["OK"] == len(rows) {
		s.WriteString("\nALL FILES MATCH - the copy is a perfect copy.\n")
	} else {
		s.WriteString("\nProblems (first 100):\n")
		n := 0
		for _, r := range rows {
			if r.Status != "OK" && n < 100 {
				fmt.Fprintf(&s, "  [%s] %s %s\n", r.Status, r.Path, r.Error)
				n++
			}
		}
		s.WriteString("\nMISSING = not on the target. WRONG-SIZE/CORRUPT = the copy went wrong (or a fake-capacity card).\n")
		s.WriteString("Copy those files again from the rescue and run verify again.\n")
	}
	fmt.Fprintf(&s, "Full detail: %s\n", csvPath)
	sumPath := report.UniquePath(filepath.Join(v.reports, "verify_summary_"+stamp+".txt"))
	return csvPath, destfs.WriteFileAtomic(out, sumPath, append([]byte("\xef\xbb\xbf"), s.String()...))
}

type imageMeta struct {
	Complete   bool
	SHA256, XZ string
	Size       int64
	BadBytes   int64
}

func (v *Verify) readMeta() (imageMeta, error) {
	var m imageMeta
	b, err := os.ReadFile(strings.TrimSuffix(v.Image, ".xz") + ".json")
	if err != nil {
		return m, err
	}
	return m, json.Unmarshal(b, &m)
}

func (v *Verify) planImage() (job.Plan, error) {
	var p job.Plan
	m, err := v.readMeta()
	if err != nil {
		return p, fmt.Errorf("reading the image's .json: %w", err)
	}
	if !m.Complete || m.SHA256 == "" {
		p.Issues = append(p.Issues, job.Issue{Kind: "image-incomplete", Severity: job.Block,
			Title:   "The image is not finished yet",
			Detail:  fmt.Sprintf("%s still unreadable or not yet read — run image again to continue (see the .map file).", preflight.GB(m.BadBytes)),
			Choices: []job.Choice{job.Stop}, Default: job.Stop})
	}
	p.Bytes, p.Items = m.Size, 1
	p.Summary = []string{fmt.Sprintf("Check: %s (%s) against its recorded SHA-256 — only read", v.Image, preflight.GB(m.Size))}
	p.Result = []string{"OK when the image is exactly as it was made; CORRUPT if anything changed"}
	return p, nil
}

func (v *Verify) runImage(ctx context.Context) (job.Result, error) {
	m, err := v.readMeta()
	if err != nil {
		return job.Result{}, err
	}
	raw := strings.TrimSuffix(v.Image, ".xz")
	var rd io.Reader
	f, err := os.Open(raw)
	if err != nil {
		if f, err = os.Open(raw + ".xz"); err != nil {
			return job.Result{}, err
		}
		if rd, err = xz.NewReader(f); err != nil {
			f.Close()
			return job.Result{}, err
		}
	} else {
		rd = f
	}
	defer f.Close()
	sum, err := hashReader(rd)
	if err != nil {
		return job.Result{Counters: map[string]int{"UNREADABLE": 1}}, err
	}
	if strings.EqualFold(sum, m.SHA256) {
		return job.Result{Counters: map[string]int{"OK": 1}, Clean: true}, nil
	}
	return job.Result{Counters: map[string]int{"CORRUPT": 1}}, nil
}
