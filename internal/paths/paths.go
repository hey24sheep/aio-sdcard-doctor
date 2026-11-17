// Package paths turns card file names into safe destination paths: every segment is sanitised,
// long paths are shortened exactly like the reference script's Get-SafeRelPath, and name
// collisions (case, NFC/NFD, sanitising) are resolved deterministically.
package paths

import (
	"crypto/sha1"
	"encoding/hex"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/profile"
)

// MaxPath is the destination path length limit (the reference script's -MaxPath 240).
const MaxPath = 240

// shortHash is Get-ShortHash: SHA-1 of the lowercased UTF-8 text, first n hex digits.
func shortHash(s string, n int) string {
	h := sha1.Sum([]byte(strings.ToLower(s)))
	return hex.EncodeToString(h[:])[:n]
}

// cut returns the longest prefix of s whose length is ≤ n units, never splitting a character.
func cut(s string, n int) string {
	used := 0
	for i, r := range s {
		w := runeUnits(r)
		if used+w > n {
			return s[:i]
		}
		used += w
	}
	return s
}

// short is the script's $short block: trim, then cut to n and drop trailing spaces/dots.
func short(t string, n int) string {
	t = strings.TrimSpace(t)
	if plen(t) > n {
		return strings.TrimRight(cut(t, n), " .")
	}
	return t
}

// sanitize makes one path segment safe on every destination (R1), with Windows' name rules when
// win is set.
func sanitize(seg string, win bool) string {
	if seg == "." || seg == ".." {
		return "_"
	}
	var b strings.Builder
	for _, r := range seg {
		switch {
		case r == '/' || r == '\\' || r < 0x20 || r == utf8.RuneError:
			b.WriteByte('_')
		case win && strings.ContainsRune(`<>:"|?*`, r):
			b.WriteByte('_')
		default:
			b.WriteRune(r)
		}
	}
	s := strings.TrimRight(b.String(), ". ")
	if s == "" {
		return "_"
	}
	if win && reserved[strings.ToUpper(strings.TrimRight(strings.SplitN(s, ".", 2)[0], " "))] {
		s = "_" + s
	}
	return s
}

var reserved = func() map[string]bool {
	m := map[string]bool{"CON": true, "PRN": true, "AUX": true, "NUL": true, "CONIN$": true, "CONOUT$": true}
	for _, p := range []string{"COM", "LPT"} {
		for _, d := range []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "¹", "²", "³"} {
			m[p+d] = true
		}
	}
	return m
}()

// clean sanitises every segment of a "/"-separated relative path.
func clean(rel string) string { return cleanAs(rel, windowsNames) }

func cleanAs(rel string, win bool) string {
	segs := strings.Split(rel, "/")
	for i, s := range segs {
		segs[i] = sanitize(s, win)
	}
	return strings.Join(segs, "/")
}

// ext is [IO.Path]::GetExtension: from the last dot, "" when the dot ends the name.
func ext(file string) string {
	e := path.Ext(file)
	if e == "." {
		return ""
	}
	return e
}

// SafeRel returns rel ("/"-separated, sanitised) unchanged when base+sep+rel fits in max;
// otherwise a shortened, deterministic version (exact port of Get-SafeRelPath). Lengths are
// UTF-16 units on Windows and bytes elsewhere. The result is "/"-separated.
func SafeRel(base, rel string, max int) string {
	rel = clean(rel)
	sep := string(filepath.Separator)
	native := strings.ReplaceAll(rel, "/", sep)
	baseLen := plen(base)
	if baseLen+1+plen(native) <= max {
		return rel
	}
	parts := strings.Split(rel, "/")
	file, dirs := parts[len(parts)-1], parts[:len(parts)-1]
	e := ext(file)
	if plen(e) > 10 {
		e = ""
	}
	stem := strings.TrimSuffix(file, e)
	fh := shortHash(native, 8)
	sd := make([]string, len(dirs))
	for i, d := range dirs {
		if plen(d) > 24 {
			sd[i] = short(d, 16) + "~" + shortHash(d, 4)
		} else {
			sd[i] = d
		}
	}
	newFile := short(stem, 40) + "~" + fh + e
	try := func(segs ...string) (string, bool) {
		c := strings.Join(segs, "/")
		return finish(c), baseLen+1+plen(strings.ReplaceAll(c, "/", sep)) <= max
	}
	if c, ok := try(append(append([]string{}, sd...), newFile)...); ok {
		return c
	}
	dh, top := "root", ""
	if len(sd) > 0 {
		dh = shortHash(strings.Join(sd, sep), 8)
		top = short(sd[0], 16)
	}
	if c, ok := try("_long", top+"~"+dh, newFile); ok {
		return c
	}
	c, _ := try("_long", dh, short(stem, 12)+"~"+fh+e)
	return c
}

// finish trims trailing dots/spaces from every output segment ("_" if nothing is left).
func finish(rel string) string {
	segs := strings.Split(rel, "/")
	for i, s := range segs {
		if s = strings.TrimRight(s, ". "); s == "" {
			s = "_"
		}
		segs[i] = s
	}
	return strings.Join(segs, "/")
}

// Planner assigns every file of an inventory a unique destination path "<Category>/<rel>".
// Add every file first, then call Plan: the result does not depend on the order files were seen.
// Claim the destinations earlier runs recorded first, so a name never moves to another file.
type Planner struct {
	base   string
	max    int
	groups map[string][]string // collision key → original rels (sorted)
	keyOf  map[string]string   // cat + "\x00" + rel → key
	alias  map[string]string   // collision key of a file's __PARTIAL name → that file
	claims map[string]string   // folded destination an earlier run stored a file at → that file
	pclaim map[string]string   // folded __PARTIAL name of a claimed destination → that file
	own    map[string]string   // cat + "\x00" + rel → the destination it claimed
	// Windows applies Windows' name rules (reserved characters, device names) whatever system
	// this runs on: set it for a FAT, exFAT or NTFS destination.
	Windows bool
}

func (p *Planner) clean(rel string) string { return cleanAs(rel, windowsNames || p.Windows) }

// NewPlanner plans paths under base (the destination root) with the given length cap.
func NewPlanner(base string, max int) *Planner {
	return &Planner{base: base, max: max, groups: map[string][]string{}, keyOf: map[string]string{},
		alias: map[string]string{}, claims: map[string]string{}, pclaim: map[string]string{}, own: map[string]string{}}
}

func id(cat profile.Category, rel string) string { return string(cat) + "\x00" + rel }

// fold is how a destination compares on a case-insensitive, normalising file system.
func fold(p string) string { return strings.ToLower(norm.NFC.String(p)) }

// partialName is p with __PARTIAL before the extension (the name a copy with unreadable parts
// gets; the same rule as archive.PartialName).
func partialName(p string) string {
	e := path.Ext(p)
	return strings.TrimSuffix(p, e) + "__PARTIAL" + e
}

// Add registers a file. Collisions are keyed on the sanitised, NFC-normalised, lowercased path.
func (p *Planner) Add(cat profile.Category, rel string) {
	k := id(cat, rel)
	if _, ok := p.keyOf[k]; ok {
		return
	}
	key := fold(string(cat) + "/" + p.clean(rel))
	p.keyOf[k] = key
	g := append(p.groups[key], rel)
	sort.Strings(g)
	p.groups[key] = g
	ak := fold(string(cat) + "/" + partialName(p.clean(rel)))
	if a, ok := p.alias[ak]; !ok || rel < a {
		p.alias[ak] = rel
	}
}

// Claim reserves dest ("<Category>/<path>", as Plan returned it in an earlier run) for the card
// file rel, whose copy an earlier run stored there: rel keeps that destination and no other file
// is planned onto it, whichever files this run lists or selects.
func (p *Planner) Claim(dest, rel string) {
	cat, _, ok := strings.Cut(dest, "/")
	if !ok {
		return
	}
	// The claimed file's copy may be its __PARTIAL name (or become it): no other file takes that
	// name either, even when the claimed file isn't on the card this run.
	if pk := fold(partialName(dest)); p.pclaim[pk] == "" || rel < p.pclaim[pk] {
		p.pclaim[pk] = rel
	}
	k := fold(dest)
	if o, ok := p.claims[k]; ok {
		if o <= rel {
			return // two rows naming one copy (left by an older version): the smaller name keeps it
		}
		delete(p.own, id(profile.Category(cat), o))
	}
	p.claims[k] = rel
	p.own[id(profile.Category(cat), rel)] = dest
}

// Plan returns "<Category>/<safe rel>" for a file. A file keeps the destination it claimed; the
// byte-smallest of colliding names keeps the plain name; the others — and a file whose plain
// name, or whose __PARTIAL name, is another file's — get "~<4 hex of the original name>" before
// the extension.
func (p *Planner) Plan(cat profile.Category, rel string) string {
	p.Add(cat, rel)
	if d, ok := p.own[id(cat, rel)]; ok {
		return d
	}
	name := p.clean(rel)
	key := p.keyOf[id(cat, rel)]
	catBase := p.base + string(filepath.Separator) + string(cat)
	plain := string(cat) + "/" + SafeRel(catBase, name, p.max)
	g := p.groups[key]
	hashed := len(g) > 1 && g[0] != rel
	if a, ok := p.alias[key]; ok && a != rel {
		hashed = true // a card file literally named like another file's __PARTIAL copy
	}
	for _, d := range []string{plain, partialName(plain)} {
		if o, ok := p.claims[fold(d)]; ok && o != rel {
			hashed = true // an earlier run stored another file there
		}
		if o, ok := p.pclaim[fold(d)]; ok && o != rel {
			hashed = true // another file's partial copy is (or may be) there
		}
	}
	if !hashed {
		return plain
	}
	h := sha1.Sum([]byte(rel))
	dir, file := path.Split(name)
	e := ext(file)
	name = dir + strings.TrimSuffix(file, e) + "~" + hex.EncodeToString(h[:])[:4] + e
	return string(cat) + "/" + SafeRel(catBase, name, p.max)
}
