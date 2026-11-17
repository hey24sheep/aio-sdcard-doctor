package paths

import (
	"fmt"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/profile"
)

const max = 240

var base = strings.Repeat("b", 60)

func fits(t *testing.T, rel string) {
	t.Helper()
	if n := plen(base) + 1 + plen(filepath.FromSlash(rel)); n > max {
		t.Fatalf("%q: %d > %d", rel, n, max)
	}
	if !filepath.IsLocal(filepath.FromSlash(rel)) {
		t.Fatalf("%q is not local", rel)
	}
}

func TestUnchangedWhenShort(t *testing.T) {
	for _, rel := range []string{"a.jpg", "DCIM/Camera/IMG_0001.jpg", "x/y/z"} {
		if got := SafeRel(base, rel, max); got != rel {
			t.Errorf("%q → %q", rel, got)
		}
	}
}

func Test300CharName(t *testing.T) {
	rel := "dir/" + strings.Repeat("n", 300) + ".jpg"
	got := SafeRel(base, rel, max)
	fits(t, got)
	if !strings.HasSuffix(got, ".jpg") || !strings.HasPrefix(path.Base(got), strings.Repeat("n", 40)+"~") {
		t.Fatalf("got %q", got)
	}
}

func Test40LevelNesting(t *testing.T) {
	var segs []string
	for i := 0; i < 40; i++ {
		segs = append(segs, fmt.Sprintf("folder%02d", i))
	}
	got := SafeRel(base, strings.Join(segs, "/")+"/f.txt", max)
	fits(t, got)
	if !strings.HasPrefix(got, "_long/") {
		t.Fatalf("want flattened, got %q", got)
	}
}

func Test12LongFolders(t *testing.T) {
	var segs []string
	for i := 0; i < 12; i++ {
		segs = append(segs, fmt.Sprintf("a very long folder name number %02d here", i))
	}
	fits(t, SafeRel(base, strings.Join(segs, "/")+"/photo.jpg", max))
}

func TestSamePrefixDistinct(t *testing.T) {
	seen := map[string]string{}
	for i := 0; i < 100; i++ {
		rel := fmt.Sprintf("d/%s%03d.jpg", strings.Repeat("same prefix ", 20), i)
		got := SafeRel(base, rel, max)
		fits(t, got)
		if prev, dup := seen[strings.ToLower(got)]; dup {
			t.Fatalf("%q and %q both → %q", prev, rel, got)
		}
		seen[strings.ToLower(got)] = rel
	}
}

func TestExtensionless(t *testing.T) {
	got := SafeRel(base, "d/"+strings.Repeat("x", 250), max)
	fits(t, got)
	if strings.Contains(path.Base(got), ".") {
		t.Fatalf("got %q", got)
	}
}

func TestLongExtension(t *testing.T) {
	got := SafeRel(base, "d/"+strings.Repeat("x", 200)+".averyveryverylongext", max)
	fits(t, got)
	if strings.HasSuffix(got, ".averyveryverylongext") {
		t.Fatalf("long ext kept: %q", got)
	}
}

func TestTrailingDotSpaceSegments(t *testing.T) {
	got := SafeRel(base, "dir. /sub /file. ", max)
	for _, seg := range strings.Split(got, "/") {
		if strings.HasSuffix(seg, ".") || strings.HasSuffix(seg, " ") || seg == "" {
			t.Fatalf("segment %q in %q", seg, got)
		}
	}
	long := SafeRel(base, strings.Repeat("ab. ", 80)+"/x.jpg", max)
	fits(t, long)
	for _, seg := range strings.Split(long, "/") {
		if strings.HasSuffix(seg, ".") || strings.HasSuffix(seg, " ") {
			t.Fatalf("segment %q in %q", seg, long)
		}
	}
}

func TestUnicode(t *testing.T) {
	for _, name := range []string{"नमस्ते दुनिया ", "😀😀😀", "Ñandú", "[p1] photo"} {
		rel := "d/" + strings.Repeat(name, 40) + ".jpg"
		got := SafeRel(base, rel, max)
		fits(t, got)
		if !strings.HasSuffix(got, ".jpg") {
			t.Fatalf("%q", got)
		}
		if got2 := SafeRel(base, "d/"+name+".jpg", max); got2 != "d/"+strings.TrimRight(name, " ")+".jpg" && got2 != "d/"+name+".jpg" {
			t.Fatalf("short unicode changed: %q", got2)
		}
	}
}

func TestSanitizeTraversalAndControls(t *testing.T) {
	cases := map[string]string{
		"../etc/passwd": "_/etc/passwd", "a/./b": "a/_/b", "a/\x00b\x01": "a/_b_", "a//b": "a/_/b",
	}
	for in, want := range cases {
		if got := SafeRel(base, in, max); got != want {
			t.Errorf("%q → %q want %q", in, got, want)
		}
	}
}

func TestReservedNamesWindows(t *testing.T) {
	got := SafeRel(base, "CON/aux.txt/a<b>c:d|e?f*g\".jpg", max)
	if runtime.GOOS != "windows" {
		if got != "CON/aux.txt/a<b>c:d|e?f*g\".jpg" {
			t.Fatalf("non-windows changed: %q", got)
		}
		return
	}
	if got != "_CON/_aux.txt/a_b_c_d_e_f_g_.jpg" {
		t.Fatalf("got %q", got)
	}
}

func TestDeterministic(t *testing.T) {
	rel := "d/" + strings.Repeat("q", 300) + ".mp4"
	first, second := SafeRel(base, rel, max), SafeRel(base, rel, max)
	if first != second {
		t.Fatal("not deterministic")
	}
}

func planAll(order []string) map[string]string {
	p := NewPlanner(base, max)
	for _, r := range order {
		p.Add(profile.Photos, r)
	}
	out := map[string]string{}
	for _, r := range order {
		out[r] = p.Plan(profile.Photos, r)
	}
	return out
}

func TestCaseCollisionOrderIndependent(t *testing.T) {
	a := planAll([]string{"DCIM/IMG.jpg", "dcim/img.JPG", "DCIM/img.jpg"})
	b := planAll([]string{"DCIM/img.jpg", "dcim/img.JPG", "DCIM/IMG.jpg"})
	for k := range a {
		if a[k] != b[k] {
			t.Fatalf("%s: %q vs %q", k, a[k], b[k])
		}
	}
	if a["DCIM/IMG.jpg"] != "Photos/DCIM/IMG.jpg" { // byte-smallest keeps the plain name
		t.Fatalf("plain = %q", a["DCIM/IMG.jpg"])
	}
	seen := map[string]bool{}
	for _, v := range a {
		if seen[strings.ToLower(v)] {
			t.Fatalf("collision remains: %v", a)
		}
		seen[strings.ToLower(v)] = true
	}
}

func TestNFCvsNFDCollide(t *testing.T) {
	nfc, nfd := "d/Ñandú.jpg", "d/N\u0303andu\u0301.jpg"
	m := planAll([]string{nfd, nfc})
	if strings.EqualFold(m[nfc], m[nfd]) {
		t.Fatalf("NFC/NFD names not separated: %v", m)
	}
}

func FuzzPlanStaysInside(f *testing.F) {
	for _, s := range []string{"..", "../../x", "a/../../b", "\x00", "C:\\x", "/abs", "a\\..\\b", strings.Repeat("é", 300)} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, rel string) {
		p := NewPlanner(base, max)
		p.Add(profile.Other, rel)
		got := p.Plan(profile.Other, rel)
		fits(t, got)
		if !strings.HasPrefix(got, "Other/") {
			t.Fatalf("%q escaped category: %q", rel, got)
		}
	})
}

// A card file named like another file's __PARTIAL copy gets the suffix, so a partial copy can
// never land on it.
func TestPlannerReservesPartialNames(t *testing.T) {
	p := NewPlanner(`D:\out`, MaxPath)
	p.Add(profile.Photos, "DCIM/foo__PARTIAL.jpg")
	p.Add(profile.Photos, "DCIM/foo.jpg")
	if got := p.Plan(profile.Photos, "DCIM/foo.jpg"); got != "Photos/DCIM/foo.jpg" {
		t.Fatalf("foo.jpg: %s", got)
	}
	if got := p.Plan(profile.Photos, "DCIM/foo__PARTIAL.jpg"); strings.EqualFold(got, "Photos/DCIM/foo__PARTIAL.jpg") {
		t.Fatalf("foo__PARTIAL.jpg keeps the name foo.jpg's partial copy gets: %s", got)
	}
}

// A destination an earlier run recorded stays its file's, whichever files this run lists.
func TestPlannerKeepsClaimedNames(t *testing.T) {
	p := NewPlanner(`D:\out`, MaxPath)
	p.Claim("Documents/notes/a.txt", "notes/a.txt")
	p.Add(profile.Documents, "notes/A.TXT")
	p.Add(profile.Documents, "notes/a.txt")
	if got := p.Plan(profile.Documents, "notes/a.txt"); got != "Documents/notes/a.txt" {
		t.Fatalf("a.txt moved to %s", got)
	}
	if got := p.Plan(profile.Documents, "notes/A.TXT"); strings.EqualFold(got, "Documents/notes/a.txt") {
		t.Fatalf("A.TXT planned onto a.txt's copy: %s", got)
	}
}

// M2: names were made safe for the system the program runs on, not for the destination: from
// Linux or macOS, a card name with ':' failed on a FAT, exFAT or NTFS disk every run.
func TestPlannerUsesWindowsRulesForWindowsFileSystems(t *testing.T) {
	p := NewPlanner("/mnt/out", MaxPath)
	p.Windows = true
	if got := p.Plan(profile.Photos, `DCIM/a:b?.jpg`); got != "Photos/DCIM/a_b_.jpg" {
		t.Errorf("got %s", got)
	}
	if got := p.Plan(profile.Documents, `docs/CON.txt`); got != "Documents/docs/_CON.txt" {
		t.Errorf("got %s", got)
	}
}

// SC3: an earlier run's partial copy a__PARTIAL.jpg is that file's even when a.jpg isn't on the
// card this run: a card file literally named a__PARTIAL.jpg is planned elsewhere.
func TestPlannerKeepsClaimedPartialName(t *testing.T) {
	p := NewPlanner(`D:\out`, MaxPath)
	p.Claim("Photos/DCIM/a.jpg", "DCIM/a.jpg")
	if got := p.Plan(profile.Photos, "DCIM/a__PARTIAL.jpg"); strings.EqualFold(got, "Photos/DCIM/a__PARTIAL.jpg") {
		t.Fatalf("planned onto a.jpg's partial copy: %s", got)
	}
	// a file that claimed the literal name itself keeps it
	p = NewPlanner(`D:\out`, MaxPath)
	p.Claim("Photos/DCIM/a.jpg", "DCIM/a.jpg")
	p.Claim("Photos/DCIM/a__PARTIAL.jpg", "DCIM/a__PARTIAL.jpg")
	if got := p.Plan(profile.Photos, "DCIM/a__PARTIAL.jpg"); got != "Photos/DCIM/a__PARTIAL.jpg" {
		t.Fatalf("own claim lost: %s", got)
	}
	if got := p.Plan(profile.Photos, "DCIM/a.jpg"); got != "Photos/DCIM/a.jpg" {
		t.Fatalf("a.jpg moved: %s", got)
	}
}
