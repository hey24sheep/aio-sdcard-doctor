package profile

import (
	"io/fs"
	"path"
	"regexp"
	"strings"
)

// Rule tags paths of a card layout: Force puts the file in a category, Sniff asks the scanner to
// identify it from its content (unnamed fragments).
type Rule struct {
	Pattern *regexp.Regexp
	Suggest string
	Force   Category
	Sniff   bool
}

// Profile is a card layout. DetectAny holds regexes matched against the card's paths (folders
// end with "/"); any match activates the profile.
type Profile struct {
	Name      string
	DetectAny []string
	Rules     []Rule
}

func re(s string) *regexp.Regexp { return regexp.MustCompile("(?i)" + s) }

var fragmentRules = []Rule{
	{Pattern: re(`^LOST\.DIR/`), Sniff: true},
	{Pattern: re(`^FOUND\.\d{3}/`), Sniff: true},
}

// Builtin is every known layout in priority order; Generic is always last and always active.
var Builtin = []Profile{
	{Name: "Android", DetectAny: []string{`^Android/$`, `^LOST\.DIR/$`, `^DCIM/Camera/$`},
		Rules: append([]Rule{{Pattern: re(`(^|/)WhatsApp( Business)?/(Databases|Backups)/`), Force: ChatBackups}}, fragmentRules...)},
	{Name: "DCFCamera", DetectAny: []string{`^DCIM/\d{3}[A-Z0-9_]{5}/$`}},
	{Name: "AVCHD", DetectAny: []string{`^PRIVATE/AVCHD/$`}, Rules: []Rule{{Pattern: re(`^PRIVATE/AVCHD/`), Force: Videos}}},
	{Name: "SonyXAVC", DetectAny: []string{`^PRIVATE/M4ROOT/$`}, Rules: []Rule{{Pattern: re(`^PRIVATE/M4ROOT/`), Force: Videos}}},
	{Name: "GoPro", DetectAny: []string{`^DCIM/\d{3}GOPRO/$`}},
	{Name: "DJI", DetectAny: []string{`^DCIM/\d{3}MEDIA/DJI_`}},
	{Name: "Insta360", DetectAny: []string{`^DCIM/.*\.ins[vp]$`}},
	{Name: "Generic", Rules: fragmentRules},
}

// detectDepth and detectCap bound the walk Detect does over the card (layouts are near the root).
const (
	detectDepth = 3
	detectCap   = 20000
)

// Detect returns the layouts present on src, in Builtin order, Generic last.
func Detect(src fs.FS) []Profile {
	type compiled struct {
		p   Profile
		res []*regexp.Regexp
	}
	var cs []compiled
	for _, p := range Builtin {
		c := compiled{p: p}
		for _, d := range p.DetectAny {
			c.res = append(c.res, re(d))
		}
		cs = append(cs, c)
	}
	hit := make([]bool, len(cs))
	seen := 0
	fs.WalkDir(src, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == "." {
			return nil
		}
		if seen++; seen > detectCap {
			return fs.SkipAll
		}
		rel := p
		if d.IsDir() {
			rel += "/"
		}
		for i, c := range cs {
			for _, r := range c.res {
				if !hit[i] && r.MatchString(rel) {
					hit[i] = true
				}
			}
		}
		if d.IsDir() && strings.Count(p, "/") >= detectDepth-1 {
			return fs.SkipDir
		}
		return nil
	})
	var out []Profile
	for i, c := range cs {
		if hit[i] || c.p.Name == "Generic" {
			out = append(out, c.p)
		}
	}
	return out
}

// ByNames returns the named built-in profiles (case-insensitive), Generic always included last.
func ByNames(names []string) []Profile {
	want := map[string]bool{"generic": true}
	for _, n := range names {
		want[strings.ToLower(strings.TrimSpace(n))] = true
	}
	var out []Profile
	for _, p := range Builtin {
		if want[strings.ToLower(p.Name)] {
			out = append(out, p)
		}
	}
	return out
}

// Decision is how one file is handled. Nothing is ever skipped here: Suggest only tags it.
type Decision struct {
	Cat     Category
	Sniff   bool
	Suggest string
	Profile string
}

// Decide classifies rel ("/"-separated, relative to the card root). siblings maps lowercased
// "dir/stem" of media files to their category, for sidecars (.THM, .LRV, .SRT, .XMP …).
func Decide(active []Profile, rel string, siblings map[string]Category) Decision {
	d := Decision{Suggest: suggestFor(rel)}
	for _, p := range active {
		for _, r := range p.Rules {
			if !r.Pattern.MatchString(rel) {
				continue
			}
			d.Profile = p.Name
			if r.Force != "" {
				d.Cat = r.Force
				return d
			}
			if r.Sniff {
				d.Cat, d.Sniff = Other, true
				return d
			}
		}
	}
	ext := path.Ext(rel)
	if IsSidecar(ext) {
		d.Cat = Other
		if c, ok := siblings[strings.ToLower(strings.TrimSuffix(rel, ext))]; ok {
			d.Cat = c
		}
		return d
	}
	if c, ok := ByExt(ext); ok {
		d.Cat = c
		return d
	}
	d.Cat = Other
	return d
}
