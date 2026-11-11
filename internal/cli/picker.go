package cli

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/drives"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/preflight"
)

// listDrivesFn is a seam over drives.List (tests show a machine with or without a card).
var listDrivesFn = drives.List

const maxLooks = 30 // "press Enter to look again" is bounded

func driveLine(d drives.Drive) string {
	kind := "internal drive"
	if d.Removable {
		kind = "card / USB drive"
	}
	var tags []string
	if d.System {
		tags = append(tags, "system")
	}
	if d.Locked {
		tags = append(tags, "locked")
	}
	if d.RAW {
		tags = append(tags, "file system not readable by the OS")
	}
	t := ""
	if len(tags) > 0 {
		t = " (" + strings.Join(tags, ", ") + ")"
	}
	return fmt.Sprintf("%-12s %-14s %-6s %10s  %s%s", d.Root, d.Label, d.FS, preflight.GB(int64(d.Total)), kind, t)
}

// sortDrives puts cards and USB drives first.
func sortDrives(ds []drives.Drive) {
	sort.SliceStable(ds, func(i, j int) bool { return ds[i].Removable && !ds[j].Removable })
}

// noFromMessage is shown when a script gives no --from: what was found, and whether a card was.
func noFromMessage(cmd string) string {
	var b strings.Builder
	if cmd == "fix --drive" { // card tools name the card itself, not a source to copy from
		b.WriteString("fix: --drive is required (the card's drive letter or mount point, or its disk).\n")
	} else {
		fmt.Fprintf(&b, "%s: --from is required (a card, USB drive, internal drive, folder or image file).\n", cmd)
	}
	ds, _ := listDrivesFn()
	sortDrives(ds)
	if len(ds) == 0 || !ds[0].Removable {
		b.WriteString("No card or USB drive was found. Insert it, or name an internal drive or folder with --from.\n")
	}
	for _, d := range ds {
		b.WriteString("  " + driveLine(d) + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// pickSource asks which card or drive to read. With no card or USB drive connected it says so
// and offers to look again, or to read an internal drive or a folder instead.
func pickSource(ctx context.Context, io IO, purpose string) (string, error) {
	for look := 0; look < maxLooks; look++ {
		ds, err := listDrivesFn()
		if err != nil {
			return "", err
		}
		sortDrives(ds)
		hasCard := len(ds) > 0 && ds[0].Removable
		fmt.Fprintf(io.Err, "\nWhich drive should be %s?\n", purpose)
		if !hasCard {
			fmt.Fprintln(io.Err, "  No card or USB drive was found. Insert it and press Enter to look again,")
			fmt.Fprintln(io.Err, "  or copy from an internal drive or a folder below.")
		}
		for i, d := range ds {
			fmt.Fprintf(io.Err, "  %2d) %s\n", i+1, driveLine(d))
		}
		typeIt := len(ds) + 1
		fmt.Fprintf(io.Err, "  %2d) Type a folder or card image file\n", typeIt)
		def := 0
		if hasCard {
			def = 1
		}
		q := fmt.Sprintf("Choose 1-%d, or Enter to look again: ", typeIt)
		if def > 0 {
			q = fmt.Sprintf("Choose 1-%d [%d]: ", typeIt, def)
		}
		a, err := ask(ctx, io, q)
		if err != nil {
			return "", err
		}
		n := 0
		switch {
		case a == "" && def > 0:
			n = def
		case a == "":
			continue // look again
		default:
			if _, err := fmt.Sscanf(a, "%d", &n); err != nil || n < 1 || n > typeIt {
				fmt.Fprintf(io.Err, "Please type a number from 1 to %d.\n", typeIt)
				continue
			}
		}
		if n <= len(ds) {
			return ds[n-1].Root, nil
		}
		for {
			p, err := ask(ctx, io, "Folder or image file: ")
			if err != nil {
				return "", err
			}
			if p == "" {
				break
			}
			if _, err := os.Stat(p); err != nil {
				fmt.Fprintf(io.Err, "Can't open %s: %v\n", p, err)
				continue
			}
			return p, nil
		}
	}
	return "", fmt.Errorf("no drive chosen")
}
