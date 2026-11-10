package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/drives"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/fix"
)

// Seams for fix (tests use a made-up card; TestMain traps real tools and devices).
var (
	listDevicesFn = drives.ListDevices
	fixGOOS       = runtime.GOOS
	fixStoreDir   = "" // "" = the user's config folder
)

// findTarget resolves --drive: a drive letter / mount point, or a whole disk (\\.\PhysicalDrive2,
// /dev/sdb, /dev/disk4) for a card with no mounted volume.
func findTarget(drive string) (fix.Target, error) {
	ds, err := listDevicesFn()
	if err != nil {
		return fix.Target{}, err
	}
	if strings.HasPrefix(drive, `\\.\PhysicalDrive`) || strings.HasPrefix(drive, "/dev/") {
		for _, d := range ds {
			if strings.EqualFold(d.Path, drive) {
				t := fix.Target{Disk: d}
				if len(d.Volumes) > 0 {
					t.Vol = d.Volumes[0]
				}
				return t, nil
			}
			for _, v := range d.Volumes {
				if v.Device == drive {
					return fix.Target{Vol: v, Disk: d}, nil
				}
			}
		}
		return fix.Target{}, fmt.Errorf("no disk %s found", drive)
	}
	v, err := lookupDrive(normFrom(drive))
	if err != nil {
		return fix.Target{}, err
	}
	for _, d := range ds {
		if d.PhysicalID != "" && d.PhysicalID == v.PhysicalID {
			return fix.Target{Vol: v, Disk: d}, nil
		}
	}
	return fix.Target{}, fmt.Errorf("couldn't find the disk %s is on — try again as administrator, or name the disk", drive)
}

// verifiedRescue: the manifest is of this card and its last verify said ALL FILES MATCH.
func verifiedRescue(manifestPath string, t fix.Target) (bool, string) {
	if manifestPath == "" {
		return false, ""
	}
	b, err := os.ReadFile(manifestPath)
	if err != nil {
		return false, "can't read " + manifestPath
	}
	var m struct {
		Volume  drives.Identity
		Entries []struct{ Status string }
	}
	if json.Unmarshal(b, &m) != nil {
		return false, manifestPath + " is not a rescue manifest"
	}
	left := 0
	for _, e := range m.Entries {
		switch e.Status {
		case "FAILED", "PARTIAL", "STALLED", "LOCKED", "UNREADABLE":
			left++
		}
	}
	if left > 0 {
		return false, fmt.Sprintf("that rescue couldn't read %d files or folders — they may still be only on the card", left)
	}
	if t.Vol.ID.FSSerial == "" && len(t.Vol.ID.Fingerprint) == 0 {
		return false, "this card has no readable identity (no file-system serial), so no rescue can be matched to it"
	}
	if ok, _, _ := m.Volume.Matches(t.Vol.ID); !ok {
		return false, "that rescue is of a different card"
	}
	sums, _ := filepath.Glob(filepath.Join(filepath.Dir(manifestPath), "verify_summary_*.txt"))
	sort.Strings(sums)
	if len(sums) == 0 {
		return false, "that rescue was never verified — run verify first"
	}
	if s, _ := os.ReadFile(sums[len(sums)-1]); !strings.Contains(string(s), "ALL FILES MATCH") {
		return false, "the last verify of that rescue found problems"
	}
	return true, ""
}

func fixCmd(io IO, g *globals) *cobra.Command {
	var drive, man, label, confirmLabel, confirmSize string
	var check, repair, format, health, allowFixed, noReplug, newTest bool
	c := &cobra.Command{
		Use:   "fix --drive E: (--check | --repair | --format | --health-test)",
		Short: "Check, repair or format a card with your OS's own tools, or test it for bad areas and fake capacity",
		Long: "The only command that writes to a card. It refuses system disks, fixed disks, the disk holding a rescue\n" +
			"or this program, asks you to type the card's label and size, and warns unless a verified rescue exists.",
		RunE: func(cmd *cobra.Command, args []string) error {
			var ops []string
			for op, on := range map[string]bool{fix.OpCheck: check, fix.OpRepair: repair, fix.OpFormat: format, fix.OpHealth: health} {
				if on {
					ops = append(ops, op)
				}
			}
			if len(ops) != 1 {
				return exit(ExitUsage, "fix: choose one of --check, --repair, --format, --health-test")
			}
			if drive == "" {
				return exit(ExitUsage, "%s", noFromMessage("fix --drive"))
			}
			t, err := findTarget(drive)
			if err != nil {
				return exit(ExitError, "fix: %v", err)
			}
			verified, why := verifiedRescue(man, t)
			if why != "" {
				fmt.Fprintf(io.Err, "Note: %s\n", why)
			}
			exe, _ := os.Executable()
			bin, _ := lookupDrive(exe)
			f := &fix.Fix{Op: ops[0], Target: t, Verified: verified, AllowFixed: allowFixed, NewLabel: label,
				Replug: !noReplug, NewTest: newTest, BinaryDisk: bin.PhysicalID, Out: io.Out, GOOS: fixGOOS, Admin: isAdmin,
				StoreDir: fixStoreDir, ListDevices: listDevicesFn}
			typed := func() error {
				if f.Op != fix.OpCheck { // a check changes nothing
					if err := typedConfirm(cmd.Context(), io, t, confirmLabel, confirmSize); err != nil {
						return err
					}
				}
				// the card is looked up again right before anything runs: a card swapped while the
				// user was reading or typing is never touched
				now, err := findTarget(drive)
				if err != nil || !sameCard(t, now) {
					return exit(ExitError, "The card changed since you confirmed (unplugged or swapped) — nothing was done. Run the command again.")
				}
				return nil
			}
			if !io.TTY && f.Op != fix.OpCheck && (confirmLabel == "" || confirmSize == "") {
				return exit(ExitUsage, "fix: scripts confirm with --confirm-label %q --confirm-size %q (the card's label and size)",
					fix.ConfirmLabel(t), fix.SizeText(t))
			}
			env := g.env(io)
			plan, err := f.Plan(cmd.Context(), env)
			if err != nil {
				return exit(ExitError, "%v", err)
			}
			return runPlanned(cmd.Context(), io, g, f, env, plan, os.Args[1:], nil, typed)
		},
	}
	fl := c.Flags()
	fl.StringVar(&drive, "drive", "", "the card: drive (E:), mount point, or disk (\\\\.\\PhysicalDrive2, /dev/sdb, /dev/disk4)")
	fl.BoolVar(&check, "check", false, "check the file system (changes nothing)")
	fl.BoolVar(&repair, "repair", false, "repair the file system with the OS's tool")
	fl.BoolVar(&format, "format", false, "erase the card: new FAT32 (≤ 32 GB) or exFAT file system")
	fl.BoolVar(&health, "health-test", false, "erase the card: write a pattern everywhere and read it back (bad areas, fake capacity)")
	fl.BoolVar(&allowFixed, "allow-fixed", false, "allow a fixed disk (USB hard disk, internal disk)")
	fl.StringVar(&man, "manifest", "", "a verified rescue of this card (Reports/manifest.json): no 'rescue first' warning")
	fl.StringVar(&label, "label", "", "label for --format (default: keep the card's)")
	fl.BoolVar(&noReplug, "no-replug", false, "health test: don't ask to unplug and reinsert between writing and reading")
	fl.BoolVar(&newTest, "new-test", false, "health test: forget a stopped test (of this or another card) and start over")
	fl.StringVar(&confirmLabel, "confirm-label", "", "scripts: the card's label as shown (or its drive letter when it has none)")
	fl.StringVar(&confirmSize, "confirm-size", "", "scripts: the card's size as shown, e.g. \"64.0 GB\"")
	fl.BoolVar(&g.yes, "yes", false, "accept warnings without asking")
	return c
}

// sameCard: the disk and volume found now are the ones planned for.
func sameCard(a, b fix.Target) bool {
	return a.Disk.Path == b.Disk.Path && a.Disk.Serial == b.Disk.Serial && a.Disk.Size == b.Disk.Size &&
		a.Disk.Model == b.Disk.Model && a.Vol.Root == b.Vol.Root && a.Vol.ID.FSSerial == b.Vol.ID.FSSerial
}

// typedConfirm asks for (or checks) the label and size, exactly as shown.
func typedConfirm(ctx context.Context, io IO, t fix.Target, label, size string) error {
	if io.TTY {
		fmt.Fprintf(io.Err, "\nThis erases or changes the card. To go on, type its label (%s) and size (%s).\n", fix.ConfirmLabel(t), fix.SizeText(t))
		var err error
		if label, err = ask(ctx, io, "Label: "); err != nil {
			return exit(ExitError, "Cancelled — nothing was changed.")
		}
		if size, err = ask(ctx, io, "Size: "); err != nil {
			return exit(ExitError, "Cancelled — nothing was changed.")
		}
	}
	if err := fix.Confirm(t, label, size); err != nil {
		return exit(ExitError, "%v — nothing was changed.", err)
	}
	return nil
}
