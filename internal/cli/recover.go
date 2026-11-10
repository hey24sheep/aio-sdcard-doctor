package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/recovery"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/worker"
)

// openRecoverSource is a seam over worker.Spawn for recover (tests use mock devices).
var openRecoverSource = func(s worker.Spec, stall time.Duration) (recovery.Source, error) { return worker.Spawn(s, stall) }

// parseSize reads 512, 20KB, 1.5MB, 2GB (binary units, any case).
func parseSize(s string) (int64, error) {
	t := strings.ToUpper(strings.TrimSpace(s))
	mult := int64(1)
	for _, u := range []struct {
		suffix string
		m      int64
	}{{"KB", 1 << 10}, {"MB", 1 << 20}, {"GB", 1 << 30}, {"K", 1 << 10}, {"M", 1 << 20}, {"G", 1 << 30}, {"B", 1}} {
		if strings.HasSuffix(t, u.suffix) {
			t, mult = strings.TrimSuffix(t, u.suffix), u.m
			break
		}
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
	if err != nil || f < 0 {
		return 0, fmt.Errorf("%q is not a size (e.g. 20KB, 1MB)", s)
	}
	return int64(f * float64(mult)), nil
}

func recoverCmd(io IO, g *globals) *cobra.Command {
	var from, to, types, minSize, skipRescued, selectFile, states string
	var noUndelete, deep, thorough, compress, list, flat bool
	stall := 30 * time.Second
	c := &cobra.Command{
		Use:   "recover --from E: --to DIR [--deep]",
		Short: "Find deleted files (Recuva-style): names the card still keeps, plus a deep scan by signature",
		RunE: func(cmd *cobra.Command, args []string) error {
			if from == "" {
				if !io.TTY {
					return exit(ExitUsage, "%s", noFromMessage("recover"))
				}
				picked, err := pickSource(cmd.Context(), io, "searched for deleted files")
				if err != nil {
					return exit(ExitUsage, "%v", err)
				}
				from = picked
			}
			if to == "" {
				return exit(ExitUsage, "recover: --to is required (a folder on another disk)")
			}
			if thorough && !deep {
				return exit(ExitUsage, "recover: --thorough only changes the deep scan: add --deep")
			}
			if skipRescued != "" && !fileExists(skipRescued) {
				return exit(ExitUsage, "recover: --skip-rescued %s: no such file (a rescue's Reports/manifest.json)", skipRescued)
			}
			ts, err := parseTypes(types)
			if err != nil {
				return exit(ExitUsage, "recover: %v", err)
			}
			minBytes, err := parseSize(minSize)
			if err != nil {
				return exit(ExitUsage, "recover: --min-size: %v", err)
			}
			var sts []string
			for _, s := range strings.Split(states, ",") {
				switch strings.ToLower(strings.TrimSpace(s)) {
				case "excellent":
					sts = append(sts, recovery.Excellent)
				case "poor":
					sts = append(sts, recovery.Poor)
				case "":
				default:
					return exit(ExitUsage, "recover: --states takes excellent and/or poor (unrecoverable files are listed, never written)")
				}
			}
			var sel []string
			if selectFile != "" {
				if sel, err = recovery.ReadSelection(selectFile); err != nil {
					return exit(ExitUsage, "recover: --select: %v", err)
				}
				if len(sel) == 0 {
					return exit(ExitUsage, "recover: --select %s lists nothing", selectFile)
				}
			}
			to, _ = filepath.Abs(to)
			r := &recovery.Recover{From: rawSource(normFrom(from)), Dest: to, Types: ts, MinSize: minBytes, SkipRescued: skipRescued,
				NoUndelete: noUndelete, Deep: deep, Thorough: thorough, States: sts, Select: sel, List: list, Flat: flat,
				Compress: compress, StallTimeout: stall, OpenSource: openRecoverSource, LookupDrive: lookupDrive}
			if skipRescued == "" { // a rescue in the same folder is used automatically
				if p := filepath.Join(to, "Reports", "manifest.json"); fileExists(p) {
					r.SkipRescued = p
				}
			}
			err = runAction(cmd.Context(), io, g, r, os.Args[1:], nil)
			if err == nil && list {
				fmt.Fprintf(io.Out, "\nNext: keep the lines you want in that CSV and run recover again with --select <file>\n")
			}
			return err
		},
	}
	f := c.Flags()
	f.StringVar(&from, "from", "", "card: drive (E:), device (\\\\.\\E:, /dev/sdb) or card image file")
	f.StringVar(&to, "to", "", "folder for the recovered files (on another disk)")
	f.StringVar(&types, "types", "all", "photos,videos,music,documents,archives,chat,other or all")
	f.StringVar(&minSize, "min-size", "20KB", "files smaller than this are listed, not recovered (0 = all)")
	f.StringVar(&skipRescued, "skip-rescued", "", "a rescue's manifest.json: files it already holds count as duplicates")
	f.StringVar(&selectFile, "select", "", "recover only the files in this list (a trimmed --list CSV, or one path per line)")
	f.StringVar(&states, "states", "excellent,poor", "which files to recover: excellent and/or poor")
	f.BoolVar(&noUndelete, "no-undelete", false, "don't look for deleted files the card still names")
	f.BoolVar(&deep, "deep", false, "also deep scan free space for files by their signatures (slower)")
	f.BoolVar(&thorough, "thorough", false, "with --deep, look at every byte offset, not only sector starts (much slower)")
	f.BoolVar(&compress, "compress", false, "store recovered files in ZIP files (max compression)")
	f.BoolVar(&list, "list", false, "only list what can be recovered (CSV), recover nothing")
	f.BoolVar(&flat, "flat", false, "don't restore folders: sort recovered files by type")
	f.BoolVar(&g.yes, "yes", false, "accept warnings and start without asking")
	f.DurationVar(&stall, "stall-timeout", stall, "a single read taking longer than this restarts the reader")
	return c
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}
