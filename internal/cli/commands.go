package cli

import (
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/destfs"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/drives"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/fsread"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/imaging"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/job"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/preflight"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/profile"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/report"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/rescue"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/scan"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/source"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/verify"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/worker"
)

// lookupDrive is a seam for tests (every temp folder is on the same disk).
var lookupDrive = drives.Lookup

// openSource opens the card through the worker (a seam: tests use mock cards, never real disks).
var openSource = func(s worker.Spec, stall time.Duration) (rescue.Source, error) { return worker.Spawn(s, stall) }

var typeNames = map[string]profile.Category{
	"photos": profile.Photos, "videos": profile.Videos, "music": profile.Music, "documents": profile.Documents,
	"archives": profile.Archives, "chat": profile.ChatBackups, "other": profile.Other,
}

// parseTypes reads --types; "all" means every category, Other included.
func parseTypes(s string) ([]profile.Category, error) {
	var out []profile.Category
	for _, t := range strings.Split(strings.ToLower(s), ",") {
		t = strings.TrimSpace(t)
		switch {
		case t == "":
			continue // a stray comma is never "everything"
		case t == "all":
			return nil, nil
		case typeNames[t] != "":
			out = append(out, typeNames[t])
		default:
			return nil, fmt.Errorf("unknown type %q (use photos,videos,music,documents,archives,chat,other,all)", t)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no types given (use photos,videos,music,documents,archives,chat,other,all)")
	}
	return out, nil
}

// parseExclude reads --exclude: none, recommended, or suggestion ids.
func parseExclude(s string) ([]string, error) {
	var out []string
	for _, e := range strings.Split(s, ",") {
		e = strings.TrimSpace(e)
		switch {
		case e == "" || e == "none":
		case e == "recommended":
			out = append(out, e)
		default:
			if _, ok := profile.Suggestions[e]; !ok {
				return nil, fmt.Errorf("unknown exclusion %q (use none, recommended or: %s)", e, strings.Join(profile.SuggestionIDs(), ", "))
			}
			out = append(out, e)
		}
	}
	return out, nil
}

var unsafeLabel = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

func defaultDest(from string) string {
	label := "card"
	if d, err := drives.Lookup(from); err == nil && d.Label != "" {
		label = unsafeLabel.ReplaceAllString(d.Label, "_")
	}
	wd, _ := os.Getwd()
	return filepath.Join(wd, fmt.Sprintf("aio-sdcard-doctor-%s-%s", label, time.Now().Format("20060102")))
}

func normFrom(from string) string {
	if strings.HasPrefix(from, `\\.\`) || strings.HasPrefix(from, "/dev/") {
		return from
	}
	if runtime.GOOS == "windows" && len(strings.TrimRight(from, `\/`)) <= 2 {
		return drives.NormalizeRoot(from)
	}
	if abs, err := filepath.Abs(from); err == nil {
		return abs
	}
	return from
}

// rawSource maps a drive letter or mount point to its volume device for --raw (I4); device
// paths and image files are used as they are.
func rawSource(from string) string {
	if isRawSource(from) {
		return from
	}
	d, err := lookupDrive(from)
	if err == nil && d.Device != "" && strings.EqualFold(strings.TrimRight(d.Root, `/\`), strings.TrimRight(from, `/\`)) {
		return d.Device
	}
	return from
}

// waitTimeout is how long to wait for a removed card: a person decides on a terminal (0 =
// as long as they like); unattended runs give up after 10 minutes (I11).
func waitTimeout(io IO) time.Duration {
	if io.TTY {
		return 0
	}
	return 10 * time.Minute
}

// isRawSource: a device path or a card image file is read with the built-in parsers.
func isRawSource(from string) bool {
	if strings.HasPrefix(from, `\\.\`) || strings.HasPrefix(from, "/dev/") {
		return true
	}
	st, err := os.Stat(from)
	return err == nil && st.Mode().IsRegular()
}

// parseProfiles reads --profiles: auto (nil), or card layouts by name; a misspelt name is an
// error, never silently dropped.
func parseProfiles(s string) ([]string, error) {
	if strings.TrimSpace(s) == "" || strings.EqualFold(strings.TrimSpace(s), "auto") {
		return nil, nil
	}
	var known []string
	for _, p := range profile.Builtin {
		known = append(known, strings.ToLower(p.Name))
	}
	var out []string
	for _, n := range strings.Split(s, ",") {
		n = strings.ToLower(strings.TrimSpace(n))
		if n == "" {
			continue
		}
		if !slices.Contains(known, n) {
			return nil, fmt.Errorf("unknown card layout %q (use auto or: %s)", n, strings.Join(known, ", "))
		}
		out = append(out, n)
	}
	return out, nil
}

// volumeFlagCheck: --volume picks a partition of a card read directly; on a folder or a mounted
// drive it would be ignored.
func volumeFlagCheck(cmd *cobra.Command, raw bool, from string) error {
	if cmd.Flags().Changed("volume") && !raw && !isRawSource(from) {
		return exit(ExitUsage, "%s: --volume picks a partition when the card is read directly: add --raw, or name a device or image file", cmd.Name())
	}
	return nil
}

// partitionNote tells the user a card read directly has more partitions than the one read (one
// per run), unless they picked it with --volume. Scan only: rescue's own plan says it, with "into
// a new folder" (a rescue folder holds one partition's copy).
func partitionNote(src any, picked bool, volume int) string {
	v, ok := src.(interface{ Volumes() []fsread.Volume })
	if !ok || picked || len(v.Volumes()) < 2 {
		return ""
	}
	n := len(v.Volumes())
	return fmt.Sprintf("Note: this card has %d partitions; only partition %d is read. To read another, run again with --volume N (0 to %d).\n",
		n, volume+1, n-1)
}

// scanRoots are the paths a scan report must never be written into: the source itself and,
// when it is a card, its volume and every mounted volume of the same physical disk (what rescue
// protects too).
func scanRoots(from string) []string {
	roots := []string{from}
	phys := ""
	switch {
	case rescue.IsDevicePath(from):
		phys = drives.PhysicalOf(from)
		if strings.HasPrefix(from, `\\.\`) && len(from) == 6 && from[5] == ':' { // \\.\E: is E:\
			roots = append(roots, from[4:6]+`\`)
		}
	case !isRawSource(from): // a folder: its whole volume, when that volume is a card
		if d, err := lookupDrive(from); err == nil && d.Removable && d.Root != "" {
			roots = append(roots, d.Root)
			phys = d.PhysicalID
		}
	}
	if phys != "" {
		if ds, err := drives.Lister(); err == nil {
			for _, d := range ds {
				if d.PhysicalID == phys && d.Root != "" {
					roots = append(roots, d.Root)
				}
			}
		}
	}
	return roots
}

// within: p is root or inside it, by where they really are (a junction, subst letter or
// symbolic link can put a harmless-looking path on the card).
func within(p, root string) bool {
	norm := func(s string) string {
		s = drives.RealPath(s)
		if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
			s = strings.ToLower(s)
		}
		return strings.TrimRight(s, `\/`)
	}
	p, root = norm(p), norm(root)
	return p == root || strings.HasPrefix(p, root+string(filepath.Separator))
}

// --- scan -------------------------------------------------------------------------------------

func scanCmd(io IO, g *globals) *cobra.Command {
	var from, to, profiles string
	var raw bool
	var volume int
	stall := 30 * time.Second
	c := &cobra.Command{
		Use:   "scan --from E: [--to DIR]",
		Short: "List what is on the card, by type and folder, and what could be skipped",
		RunE: func(cmd *cobra.Command, args []string) error {
			if from == "" {
				if !io.TTY {
					return exit(ExitUsage, "%s", noFromMessage("scan"))
				}
				picked, err := pickSource(cmd.Context(), io, "scanned")
				if err != nil {
					return exit(ExitUsage, "%v", err)
				}
				from = picked
			}
			from = normFrom(from)
			if raw {
				from = rawSource(from)
			}
			if err := volumeFlagCheck(cmd, raw, from); err != nil {
				return err
			}
			names, err := parseProfiles(profiles)
			if err != nil {
				return exit(ExitUsage, "scan: %v", err)
			}
			if to != "" {
				if err := usableTo("scan", to, false); err != nil {
					return err
				}
				to, _ = filepath.Abs(to)
				for _, r := range scanRoots(from) {
					if rescue.IsDevicePath(to) || !rescue.IsDevicePath(r) && within(to, r) {
						return exit(ExitUsage, "scan: %s is on the card being scanned — choose a folder on another disk (the card is only ever read)", to)
					}
				}
			}
			raw = raw || isRawSource(from)
			src, err := worker.Spawn(worker.Spec{From: from, Raw: raw, Volume: volume}, stall)
			if errors.Is(err, source.ErrNeedsAdmin) {
				return adminFlow(cmd.Context(), io, os.Args[1:])
			}
			if err != nil {
				return exit(ExitError, "opening %s: %v", from, err)
			}
			defer src.Close()
			src.SetContext(cmd.Context()) // Stop (Ctrl+C) ends a read stuck on the card
			if msg := src.ReadError(); msg != "" {
				return exit(ExitUsage, "%s: %s", from, msg)
			}
			active := profile.Detect(src)
			if names != nil {
				active = profile.ByNames(names)
			}
			inv, err := scan.Scan(cmd.Context(), src, active)
			if err != nil {
				return exit(ExitError, "%v", err)
			}
			text := partitionNote(src, cmd.Flags().Changed("volume"), volume) + scanSummary(from, src.Kind(), inv)
			fmt.Fprint(io.Out, styleOf(io).report(text)) // the headings bold on a terminal; the saved summary stays plain
			if to != "" {
				csvPath, err := writeScan(to, scanRoots(from), inv, text)
				if err != nil {
					return exit(ExitError, "%v", err)
				}
				fmt.Fprintln(io.Out, "Report:", cleanText(csvPath))
			}
			return nil
		},
	}
	c.Flags().StringVar(&from, "from", "", "card: drive (E:), folder, device (\\\\.\\PhysicalDrive2, /dev/sdb) or image file")
	c.Flags().StringVar(&to, "to", "", "write scan_<stamp>.csv and a summary into DIR/Reports")
	c.Flags().BoolVar(&raw, "raw", false, "read the card directly with the built-in parsers (needs admin for devices)")
	c.Flags().IntVar(&volume, "volume", 0, "which volume (partition) to read, with --raw")
	c.Flags().StringVar(&profiles, "profiles", "auto", "card layouts: auto or a list (android,dcfcamera,gopro,dji,avchd,sonyxavc,insta360)")
	return c
}

func scanSummary(from, kind string, inv scan.Inventory) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Card scan  %s\nSource: %s   (read-only, %s)\nCard layout: %s\n\nBy category:\n",
		time.Now().Format("2006-01-02 15:04:05"), from, kind, strings.Join(inv.Profiles, ", "))
	type agg struct {
		n int
		b int64
	}
	cats, tops := map[profile.Category]*agg{}, map[string]*agg{}
	var total int64
	for _, it := range inv.Items {
		if cats[it.Cat] == nil {
			cats[it.Cat] = &agg{}
		}
		cats[it.Cat].n++
		cats[it.Cat].b += it.Size
		top := strings.SplitN(it.Rel, "/", 2)[0]
		if !strings.Contains(it.Rel, "/") {
			top = "(card root)"
		}
		if tops[top] == nil {
			tops[top] = &agg{}
		}
		tops[top].n++
		tops[top].b += it.Size
		total += it.Size
	}
	for _, c := range profile.Priority {
		if a := cats[c]; a != nil {
			fmt.Fprintf(&b, "  %-18s %14s  %12s\n", c, preflight.Files(a.n), preflight.GB(a.b))
		}
	}
	fmt.Fprintf(&b, "  %-18s %14s  %12s\n\nTop folders by size:\n", "TOTAL", preflight.Files(len(inv.Items)), preflight.GB(total))
	var names []string
	for n := range tops {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool { return tops[names[i]].b > tops[names[j]].b })
	for i, n := range names {
		if i == 25 {
			break
		}
		fmt.Fprintf(&b, "  %-40s %13s  %12s\n", n, preflight.Files(tops[n].n), preflight.GB(tops[n].b))
	}
	b.WriteString("\nCould be skipped — only if you choose (rescue --exclude …):\n")
	if len(inv.Suggested) == 0 {
		b.WriteString("  nothing\n")
	}
	for _, id := range profile.SuggestionIDs() {
		if g, ok := inv.Suggested[id]; ok {
			s := profile.Suggestions[id]
			rec := ""
			if s.Recommended {
				rec = "  (recommended)"
			}
			fmt.Fprintf(&b, "  %-18s %12s %10s  %s%s\n", id, preflight.Files(g.Files), preflight.GB(g.Bytes), s.Why, rec)
		}
	}
	if len(inv.Links) > 0 {
		fmt.Fprintf(&b, "\nShortcuts / symbolic links (listed, not followed): %d\n", len(inv.Links))
	}
	fmt.Fprintf(&b, "\nFolders that could not be listed (possible damage): %d\n", len(inv.Unlistable))
	for i, u := range inv.Unlistable {
		if i == 30 {
			break
		}
		b.WriteString("  " + u + "\n")
	}
	if strings.ContainsAny(from, " \t") { // pasted into a shell, a path with spaces must be quoted
		from = `"` + from + `"`
	}
	b.WriteString("\nNext step: aio-sdcard-doctor rescue --from " + from + "\n")
	return cleanLines(b.String()) // folder names and paths come from the card: none reaches the terminal raw
}

// usableTo turns a --to the write guard couldn't use (stray quotes, "aux", "a:b") into a usage
// error that says why, before filepath.Abs hides it and long before anything is written (SU1).
// For image, --to is a file: its folder is checked.
func usableTo(cmd, to string, file bool) error {
	if to == "" {
		return nil
	}
	if file {
		to = filepath.Dir(to)
	}
	if err := destfs.CheckRoot(to); err != nil {
		return exit(ExitUsage, "%s: %v", cmd, err)
	}
	return nil
}

func writeScan(to string, roots []string, inv scan.Inventory, text string) (string, error) {
	reports := filepath.Join(to, "Reports")
	out := destfs.New(to, roots...)
	if err := out.MkdirAll(reports); err != nil {
		return "", err
	}
	stamp := report.Stamp(time.Now())
	var buf bytes.Buffer
	buf.WriteString("\xef\xbb\xbf")
	w := csv.NewWriter(&buf)
	w.Write([]string{"Category", "Top", "Path", "Bytes", "Modified", "Suggestion", "Fragment"})
	for _, it := range inv.Items {
		// names come from the card: never a formula when the report is opened in a spreadsheet
		w.Write([]string{string(it.Cat), report.Cell(path.Dir(it.Rel)), report.Cell(it.Rel), strconv.FormatInt(it.Size, 10),
			it.MTime.Format("2006-01-02 15:04:05"), report.Cell(it.Suggest), strconv.FormatBool(it.Fragment)})
	}
	w.Flush()
	csvPath := report.UniquePath(filepath.Join(reports, "scan_"+stamp+".csv"))
	if err := destfs.WriteFileAtomic(out, csvPath, buf.Bytes()); err != nil {
		return "", err
	}
	sum := report.UniquePath(filepath.Join(reports, "scan_summary_"+stamp+".txt"))
	return csvPath, destfs.WriteFileAtomic(out, sum, append([]byte("\xef\xbb\xbf"), text...))
}

// --- rescue -----------------------------------------------------------------------------------

func rescueCmd(io IO, g *globals) *cobra.Command {
	var from, to, types, exclude, profiles, backup, apps string
	var raw, compress, startOver, skipEmpty bool
	var volume int
	stall, wait := 30*time.Second, 10*time.Minute
	c := &cobra.Command{
		Use:   "rescue --from E: [--to DIR] --backup quick|standard|full|…",
		Short: "Copy the files you choose off the card (bad areas zero-filled and marked __PARTIAL)",
		Long: "Copy the files you choose off the card, most precious first. The card is only read.\n" +
			"Choose with --backup (quick, standard, full, photos, videos, music, documents, whatsapp, telegram)\n" +
			"or a custom choice (--types, --exclude, --apps); on a terminal you are asked. Nothing is skipped\n" +
			"without your choice. Run it again to resume or retry.\n" +
			"When a file keeps failing, interactive runs keep trying by default; --yes runs skip the bad areas.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if from == "" {
				if !io.TTY {
					return exit(ExitUsage, "%s", noFromMessage("rescue"))
				}
				picked, err := pickSource(cmd.Context(), io, "copied from")
				if err != nil {
					return exit(ExitUsage, "%v", err)
				}
				from = picked
			}
			fl := cmd.Flags()
			custom := fl.Changed("types") || fl.Changed("exclude") || fl.Changed("apps")
			var sel rescue.Selection
			switch {
			case fl.Changed("backup") && !strings.EqualFold(backup, "custom"):
				if custom {
					return exit(ExitUsage, "rescue: --backup %s can't be combined with --types/--exclude/--apps (use --backup custom)", backup)
				}
				p, ok := profile.PresetByID(backup)
				if !ok {
					return exit(ExitUsage, "rescue: unknown --backup %q\n%s", backup, needChoice())
				}
				sel = rescue.SelectionFor(p)
			case custom || fl.Changed("backup"):
				ts, err := parseTypes(types)
				if err != nil {
					return exit(ExitUsage, "rescue: %v", err)
				}
				ex, err := parseExclude(exclude)
				if err != nil {
					return exit(ExitUsage, "rescue: %v", err)
				}
				as, err := parseApps(apps)
				if err != nil {
					return exit(ExitUsage, "rescue: %v", err)
				}
				sel = rescue.Selection{Name: "Custom", Types: ts, Exclude: ex, Apps: as}
			case !io.TTY:
				return exit(ExitUsage, "%s", needChoice()) // nothing is chosen on the user's behalf
			}
			chosen := fl.Changed("backup") || custom
			sel.SkipEmpty = skipEmpty
			from = normFrom(from)
			if to == "" {
				to = defaultDest(from)
			}
			if raw {
				from = rawSource(from)
			}
			if err := volumeFlagCheck(cmd, raw, from); err != nil {
				return err
			}
			names, err := parseProfiles(profiles)
			if err != nil {
				return exit(ExitUsage, "rescue: %v", err)
			}
			if err := usableTo("rescue", to, false); err != nil {
				return err
			}
			to, _ = filepath.Abs(to)
			if !cmd.Flags().Changed("wait-timeout") {
				wait = waitTimeout(io)
			}
			// The plan notes a card with more partitions than the one copied (VolumePicked silences it).
			r := &rescue.Rescue{From: from, Raw: raw || isRawSource(from), Volume: volume, VolumePicked: fl.Changed("volume"),
				Dest: to, Compress: compress, Fresh: startOver, Interactive: io.TTY, StallTimeout: stall, WaitTimeout: wait,
				LookupDrive: lookupDrive, OpenSource: func(s worker.Spec, stall time.Duration) (rescue.Source, error) {
					return openSource(s, stall)
				}}
			applySelection(r, sel)
			r.Profiles = names
			env := g.env(io)
			ctx := cmd.Context()
			plan, err := r.Plan(ctx, env)
			defer r.Close()
			if err != nil {
				return exit(ExitError, "%v", err)
			}
			if r.Raw {
				for _, cp := range plan.Caps {
					if cp.Name == "Read" && !cp.Enabled {
						return exit(ExitUsage, "--raw: %s", cp.Reason)
					}
				}
			}
			if plan.NeedsAdmin && !isAdmin() {
				// Ask for admin rights first: the card can't be listed yet, so a menu would show
				// nothing, and the elevated run asks again anyway.
				return adminFlow(ctx, io, os.Args[1:])
			}
			if io.TTY && !chosen {
				var prev *rescue.Selection
				prevCompress := false
				if p, c, ok := rescue.PreviousChoice(to); ok {
					prev, prevCompress = &p, c
				}
				kept, err := chooseBackup(ctx, io, r.Inventory(), r, prev, prevCompress)
				if err == nil && kept && !fl.Changed("compress") {
					r.Compress = prevCompress
				}
				if err == nil {
					err = askOptions(ctx, io, r, !fl.Changed("skip-empty") && !kept, !fl.Changed("compress") && !kept)
				}
				if err != nil {
					return exit(ExitUsage, "%v", err)
				}
				if plan, err = r.Plan(ctx, env); err != nil {
					return exit(ExitError, "%v", err)
				}
			}
			return runPlanned(ctx, io, g, r, env, plan, os.Args[1:], func(c job.Choice) { r.Fresh = c == job.New })
		},
	}
	f := c.Flags()
	f.StringVar(&from, "from", "", "card: drive (E:), folder, device or image file")
	f.StringVar(&to, "to", "", "destination folder (default ./aio-sdcard-doctor-<label>-<yyyymmdd>)")
	f.StringVar(&backup, "backup", "", "what to copy: quick, standard, full, photos, videos, music, documents, whatsapp, telegram, custom")
	f.StringVar(&types, "types", "all", "custom: photos,videos,music,documents,archives,chat,other,all (all includes other)")
	f.StringVar(&exclude, "exclude", "none", "custom: none | recommended | suggestion ids (thumbnails,system-folders,…)")
	f.StringVar(&apps, "apps", "", "custom: only files of these apps (whatsapp,telegram)")
	f.BoolVar(&skipEmpty, "skip-empty", false, "leave out empty (0-byte) files")
	f.BoolVar(&compress, "compress", false, "store files in ZIP parts at maximum compression")
	f.BoolVar(&raw, "raw", false, "read the card directly with the built-in parsers (needs admin)")
	f.IntVar(&volume, "volume", 0, "which volume (partition) to read, with --raw")
	f.BoolVar(&g.yes, "yes", false, "accept warnings without asking (non-interactive runs)")
	f.BoolVar(&startOver, "start-over", false, "copy every file again instead of resuming")
	f.DurationVar(&stall, "stall-timeout", stall, "a single read taking longer than this restarts the reader")
	f.DurationVar(&wait, "wait-timeout", wait, "how long to wait for a removed card (0 = forever; interactive default)")
	f.StringVar(&profiles, "profiles", "auto", "card layouts: auto or a list")
	return c
}

// --- image ------------------------------------------------------------------------------------

// deviceFor maps a drive letter or mount point to its physical device; device paths and image
// files are used as given.
func deviceFor(from string) string {
	if strings.HasPrefix(from, `\\.\`) || strings.HasPrefix(from, "/dev/") {
		return from
	}
	if st, err := os.Stat(from); err == nil && st.Mode().IsRegular() {
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

// imageSource: an image is of a whole card — a drive (its root), a device or an image file —
// never of a folder, which would image the whole disk the folder is on (SU6).
func imageSource(from string) error {
	if isRawSource(from) {
		return nil
	}
	if d, err := lookupDrive(from); err == nil && sameRoot(from, d.Root) {
		return nil
	}
	return exit(ExitUsage, "image: --from takes the card itself (a drive such as E:, a mount point or a device), not a folder (%s) — copy a folder's files with rescue", cleanText(from))
}

func imageCmd(io IO, g *globals) *cobra.Command {
	var from, to string
	var retries int
	var compress, keepRaw bool
	stall := 30 * time.Second
	c := &cobra.Command{
		Use:   "image --from E: --to card.img",
		Short: "Make a full backup image of the card (ddrescue-style, resumable mapfile)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if from == "" {
				if !io.TTY {
					return exit(ExitUsage, "%s", noFromMessage("image"))
				}
				picked, err := pickSource(cmd.Context(), io, "imaged")
				if err != nil {
					return exit(ExitUsage, "%v", err)
				}
				from = picked
			}
			if to == "" {
				return exit(ExitUsage, "image: --to is required (the image file to write, e.g. card.img)")
			}
			if retries < 0 || retries > 5 {
				return exit(ExitUsage, "image: --retry-passes must be 0–5")
			}
			if keepRaw && !compress {
				return exit(ExitUsage, "image: --keep-raw goes with --compress (without it the image is kept as it is)")
			}
			if err := imageSource(normFrom(from)); err != nil {
				return err
			}
			if err := usableTo("image", to, true); err != nil {
				return err
			}
			to, _ = filepath.Abs(to)
			im := &imaging.Image{Device: deviceFor(normFrom(from)), Out: to, RetryPasses: retries, Compress: compress, KeepRaw: keepRaw,
				StallTimeout: stall, WaitTimeout: waitTimeout(io)}
			err := runAction(cmd.Context(), io, g, im, os.Args[1:], func(c job.Choice) {
				if c == job.New {
					fmt.Fprintln(io.Err, "To start a new image, choose a new --to name.")
				}
			})
			if err == nil {
				fmt.Fprint(io.Out, styleOf(io).report(imageNext(to, compress, keepRaw)))
			}
			return err
		},
	}
	f := c.Flags()
	f.StringVar(&from, "from", "", "card: drive (E:) or device (\\\\.\\PhysicalDrive2, /dev/sdb, /dev/disk4)")
	f.StringVar(&to, "to", "", "image file to write (card.img); card.img.map and card.img.json go next to it")
	f.IntVar(&retries, "retry-passes", 0, "extra passes over bad sectors (0–5)")
	f.BoolVar(&compress, "compress", false, "compress to card.img.xz when finished (verified)")
	f.BoolVar(&keepRaw, "keep-raw", false, "with --compress, keep the uncompressed card.img too")
	f.BoolVar(&g.yes, "yes", false, "accept warnings without asking")
	f.DurationVar(&stall, "stall-timeout", stall, "a single read taking longer than this restarts the reader")
	return c
}

// --- verify -----------------------------------------------------------------------------------

func verifyCmd(io IO, g *globals) *cobra.Command {
	var man, target, reports, img string
	var restart bool
	c := &cobra.Command{
		Use:   "verify --manifest DIR/Reports/manifest.json [--target DIR]",
		Short: "Re-check a copy (or a new card) against the SHA-256 of every rescued file",
		RunE: func(cmd *cobra.Command, args []string) error {
			if man == "" && img == "" {
				return exit(ExitUsage, "verify: --manifest or --image is required")
			}
			if strings.EqualFold(filepath.Ext(man), ".csv") && reports == "" {
				wd, _ := os.Getwd()
				reports = filepath.Join(wd, "aio-sdcard-doctor-verify-"+time.Now().Format("20060102"))
				fmt.Fprintln(io.Out, "Reports go to", reports, "(the old report folder is left untouched)")
			}
			if reports != "" {
				if err := destfs.New(reports).MkdirAll(reports); err != nil {
					return exit(ExitError, "%v", err)
				}
			}
			v := &verify.Verify{Manifest: man, Target: target, Restart: restart, Reports: reports, Image: img}
			return runAction(cmd.Context(), io, g, v, os.Args[1:], func(c job.Choice) { v.Restart = c == job.New })
		},
	}
	f := c.Flags()
	f.StringVar(&man, "manifest", "", "manifest.json of a rescue, or an old copy_*.csv report")
	f.StringVar(&target, "target", "", "where the files are now (default: the rescue folder)")
	f.StringVar(&reports, "reports", "", "where verify reports go (default: next to the manifest)")
	f.StringVar(&img, "image", "", "verify a card image (card.img or card.img.xz) against card.img.json")
	f.BoolVar(&restart, "restart", false, "check every file again instead of resuming")
	f.BoolVar(&g.yes, "yes", false, "accept warnings without asking")
	return c
}

// imageNext is the next step after an image: rescue reads card.img, not card.img.xz (which is
// all that is left after --compress without --keep-raw).
func imageNext(to string, compress, keepRaw bool) string {
	if compress && !keepRaw {
		return fmt.Sprintf("\nNext: verify --image \"%s\". To rescue files from it, decompress it to %s first (7-Zip, xz -dk), "+
			"or use --keep-raw next time.\n", to+".xz", filepath.Base(to))
	}
	return fmt.Sprintf("\nNext: rescue --from \"%s\" (no admin needed), or verify --image \"%s\"\n", to, to)
}
