// Package scan lists everything on a card and classifies it. Nothing is ever dropped:
// suggestion matches are only tagged and grouped for the user to decide on.
package scan

import (
	"context"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/profile"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/source"
)

// Item is one file on the card.
type Item struct {
	Rel      string
	Size     int64
	MTime    time.Time
	Cat      profile.Category
	Ext      string // the extension used for the copy (sniffed for LOST.DIR fragments)
	Fragment bool   // LOST.DIR / FOUND.NNN piece identified by its content
	Suggest  string // suggestion it matches ("" = none); never applied here
}

// Group counts the files a suggestion matches.
type Group struct {
	Files int
	Bytes int64
}

// Inventory is the scan result, in copy-priority order.
type Inventory struct {
	Items      []Item
	Unlistable []string // folders that could not be listed (possible damage), with the error
	Links      []string // symbolic links and special files: listed, not followed or copied
	Profiles   []string
	Suggested  map[string]Group
}

// sniffBytes is how much of an unnamed fragment is read to identify it.
const sniffBytes = 512

// less is the copy priority: named files before fragments, then category priority, then
// smallest first, then path.
func less(a, b Item) bool {
	if a.Fragment != b.Fragment {
		return !a.Fragment
	}
	if ra, rb := profile.Rank(a.Cat), profile.Rank(b.Cat); ra != rb {
		return ra < rb
	}
	if a.Size != b.Size {
		return a.Size < b.Size
	}
	return a.Rel < b.Rel
}

// Scan walks src and classifies every file with the active profiles.
func Scan(ctx context.Context, src source.Source, active []profile.Profile) (Inventory, error) {
	if len(active) == 0 {
		active = profile.ByNames(nil)
	}
	inv := Inventory{Suggested: map[string]Group{}}
	for _, p := range active {
		inv.Profiles = append(inv.Profiles, p.Name)
	}
	type raw struct {
		rel   string
		size  int64
		mtime time.Time
	}
	var files []raw
	err := fs.WalkDir(src, ".", func(p string, d fs.DirEntry, err error) error {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if err != nil {
			if p == "." {
				return err
			}
			inv.Unlistable = append(inv.Unlistable, p+": "+err.Error())
			if d == nil || d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			inv.Unlistable = append(inv.Unlistable, p+": "+err.Error())
			return nil
		}
		if !fi.Mode().IsRegular() && fi.Mode()&fs.ModeType != 0 {
			inv.Links = append(inv.Links, p) // reported, never silently dropped
			return nil
		}
		files = append(files, raw{p, fi.Size(), fi.ModTime()})
		return nil
	})
	if err != nil {
		return inv, err
	}
	siblings := map[string]profile.Category{}
	for _, f := range files {
		ext := path.Ext(f.rel)
		if c, ok := profile.ByExt(ext); ok && !profile.IsSidecar(ext) {
			siblings[strings.ToLower(strings.TrimSuffix(f.rel, ext))] = c
		}
	}
	inv.Items = make([]Item, 0, len(files))
	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return inv, err
		}
		d := profile.Decide(active, f.rel, siblings)
		it := Item{Rel: f.rel, Size: f.size, MTime: f.mtime, Cat: d.Cat, Ext: path.Ext(f.rel), Suggest: d.Suggest}
		if d.Sniff {
			it.Fragment = true
			it.Cat, it.Ext = profile.Other, ""
			if head := readHead(src, f.rel); len(head) > 0 {
				if c, ext, ok := profile.Sniff(head); ok {
					it.Cat, it.Ext = c, ext
				}
			}
		}
		if !d.Sniff && it.Cat == profile.Other && it.Size >= 12 && !profile.IsSidecar(it.Ext) {
			// An unknown extension: look at the content so a photo or video named oddly is not
			// left out by a choice that skips "Other" (the name is kept).
			if head := readHead(src, f.rel); len(head) > 0 {
				if c, _, ok := profile.Sniff(head); ok {
					it.Cat = c
				}
			}
		}
		if it.Suggest != "" {
			g := inv.Suggested[it.Suggest]
			g.Files++
			g.Bytes += it.Size
			inv.Suggested[it.Suggest] = g
		}
		inv.Items = append(inv.Items, it)
	}
	sort.Slice(inv.Items, func(i, j int) bool { return less(inv.Items[i], inv.Items[j]) })
	return inv, nil
}

func readHead(src source.Source, rel string) []byte {
	r, size, c, err := src.OpenAt(rel)
	if err != nil {
		return nil
	}
	defer c.Close()
	b := make([]byte, min(size, sniffBytes))
	n, err := r.ReadAt(b, 0)
	if err != nil && err != io.EOF {
		return nil
	}
	return b[:n]
}
