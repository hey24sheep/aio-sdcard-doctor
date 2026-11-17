package cli

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/drives"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/fix"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/fsread"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/profile"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/scan"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/testutil"
)

// cardInReader makes card a removable card's volume (every temp folder is on one fixed disk).
func cardInReader(t *testing.T, card string) {
	t.Helper()
	old, oldList := lookupDrive, drives.Lister
	t.Cleanup(func() { lookupDrive, drives.Lister = old, oldList })
	vol := drives.Drive{Root: card, FS: "exFAT", Removable: true, PhysicalID: "CARDX"}
	lookupDrive = func(p string) (drives.Drive, error) {
		if within(p, card) {
			return vol, nil
		}
		return drives.Drive{Root: filepath.VolumeName(p) + string(filepath.Separator), FS: "NTFS", PhysicalID: "disk"}, nil
	}
	drives.Lister = func() ([]drives.Drive, error) { return []drives.Drive{vol}, nil }
}

// SA-3/OA2: scan --to was guarded only against the folder scanned: a report could land on the
// card (from a folder on it) or the guard panicked with a Go stack.
func TestScanReportNeverOnTheCard(t *testing.T) {
	card := testutil.WriteFolder(t, testutil.AndroidCard(false))
	cardInReader(t, card)
	for name, args := range map[string][]string{
		"folder on the card, report at its root": {"--from", filepath.Join(card, "DCIM"), "--to", card},
		"report inside the folder scanned":       {"--from", card, "--to", filepath.Join(card, "out")},
	} {
		var code int
		var errOut string
		func() {
			defer func() {
				if p := recover(); p != nil {
					t.Fatalf("%s: panic %v", name, p)
				}
			}()
			code, _, errOut = cli(t, false, "", append([]string{"scan"}, args...)...)
		}()
		if code != ExitUsage || !strings.Contains(errOut, "on the card") {
			t.Errorf("%s: exit %d: %s", name, code, errOut)
		}
		if m, _ := filepath.Glob(filepath.Join(card, "*", "Reports")); len(m) > 0 {
			t.Errorf("%s: report written on the card: %v", name, m)
		}
		if _, err := os.Stat(filepath.Join(card, "Reports")); err == nil {
			t.Errorf("%s: report written on the card", name)
		}
	}
}

// --raw --from E: reads \\.\E:; the report must not go to E:\ nor any other volume of that card.
func TestScanRootsOfARawCard(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("drive letters")
	}
	old := drives.Lister
	defer func() { drives.Lister = old }()
	drives.Lister = func() ([]drives.Drive, error) {
		return []drives.Drive{{Root: `E:\`, PhysicalID: "PhysicalDrive9"}, {Root: `F:\`, PhysicalID: "PhysicalDrive9"},
			{Root: `C:\`, PhysicalID: "PhysicalDrive0"}}, nil
	}
	roots := scanRoots(`\\.\E:`)
	if !slices.Contains(roots, `E:\`) || !slices.Contains(roots, `F:\`) || slices.Contains(roots, `C:\`) {
		t.Fatalf("roots %v", roots)
	}
}

// SA-4: a "Photos only" rescue that verified clean counted as a verified rescue of the card, so
// Format didn't warn — and erased the videos that were only on the card.
func TestVerifiedRefusesARescueThatLeftFilesOut(t *testing.T) {
	dir := t.TempDir()
	man := filepath.Join(dir, "manifest.json")
	os.WriteFile(man, []byte(`{"Volume":{"FSSerial":"AAAA-1111"},"Entries":[`+
		`{"Source":"DCIM/a.jpg","Status":"OK"},{"Source":"DCIM/b.mp4","Status":"EXCLUDED"},{"Source":"DCIM/c.mp4","Status":"EXCLUDED"}]}`), 0o644)
	os.WriteFile(filepath.Join(dir, "verify_summary_20261110_000000_000.txt"), []byte("ALL FILES MATCH - the copy is a perfect copy.\n"), 0o644)
	var tg fix.Target
	tg.Vol.ID = drives.Identity{FSSerial: "AAAA-1111"}
	if ok, why := verifiedRescue(man, tg); ok || !strings.Contains(why, "2 files") {
		t.Fatalf("ok %v why %q", ok, why)
	}
}

// twoPartitionImage is a disk image with two FAT32 partitions (a camera card and a GoPro card).
func twoPartitionImage(t *testing.T) string {
	t.Helper()
	const part = 40 << 20
	a, _ := os.ReadFile(testutil.FAT32Image(t, testutil.CameraCard(), part))
	b, _ := os.ReadFile(testutil.FAT32Image(t, testutil.GoProCard(), part))
	img := make([]byte, 1<<20, 1<<20+2*part)
	for i, lba := range []uint32{2048, 2048 + part/512} {
		e := img[446+16*i:]
		e[4] = 0x0C
		binary.LittleEndian.PutUint32(e[8:], lba)
		binary.LittleEndian.PutUint32(e[12:], part/512)
	}
	img[510], img[511] = 0x55, 0xAA
	img = append(append(img, a...), b...)
	p := filepath.Join(t.TempDir(), "disk.img")
	if err := os.WriteFile(p, img, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// SA-1: a card with several partitions was read from the first one only, without a word.
func TestScanSaysWhenACardHasMorePartitions(t *testing.T) {
	img := twoPartitionImage(t)
	code, out, errOut := cli(t, false, "", "scan", "--from", img)
	if code != 0 || !strings.Contains(out+errOut, "2 partitions") || !strings.Contains(out+errOut, "--volume") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	code, out, errOut = cli(t, false, "", "scan", "--from", img, "--volume", "1")
	if code != 0 || strings.Contains(out+errOut, "2 partitions") || !strings.Contains(out, "GoPro") {
		t.Fatalf("--volume 1: exit %d\n%s\n%s", code, out, errOut)
	}
}

func TestRescueSaysWhenACardHasMorePartitions(t *testing.T) {
	img := twoPartitionImage(t)
	dst := filepath.Join(t.TempDir(), "out")
	code, out, errOut := cli(t, false, "", "rescue", "--from", img, "--to", dst, "--backup", "full", "--yes")
	if code != 0 || !strings.Contains(out+errOut, "2 partitions") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
}

// SA-5: a card with no partition (after a health test) has no label or serial to tell it from
// another card of the same size in the same reader; what is on its first sector does.
func TestFixVolumelessCardSwapNoticed(t *testing.T) {
	rig := mockCard(t)
	rig.disk.Path, rig.disk.Volumes = `\\.\PhysicalDriveMOCK7`, nil
	calls := 0
	diskFingerprint = func(string) string {
		calls++
		if calls > 1 {
			return "another card"
		}
		return "the tested card"
	}
	code, _, errOut := cli(t, false, "", "fix", "--drive", rig.disk.Path, "--health-test", "--yes",
		"--confirm-label", "MOCK7-cli", "--confirm-size", "64.0 GB")
	if code == 0 || !strings.Contains(errOut, "changed") || calls < 2 {
		t.Fatalf("exit %d calls %d: %s", code, calls, errOut)
	}
}

// OU7: image --compress (without --keep-raw) removes card.img, yet the next step named it.
func TestImageNextStepAfterCompress(t *testing.T) {
	if s := imageNext("card.img", true, false); strings.Contains(s, `rescue --from "card.img"`) || !strings.Contains(s, "card.img.xz") {
		t.Fatalf("compressed: %s", s)
	}
	if s := imageNext("card.img", true, true); !strings.Contains(s, `rescue --from "card.img"`) {
		t.Fatalf("kept raw: %s", s)
	}
}

// SU6: misspelt or meaningless flags were silently ignored.
func TestFlagMistakesAreUsageErrors(t *testing.T) {
	card := testutil.WriteFolder(t, testutil.CameraCard())
	for name, args := range map[string][]string{
		"scan --profiles typo":   {"scan", "--from", card, "--profiles", "andriod"},
		"rescue --profiles typo": {"rescue", "--from", card, "--to", filepath.Join(t.TempDir(), "out"), "--backup", "full", "--yes", "--profiles", "andriod"},
		"scan --volume, folder":  {"scan", "--from", card, "--volume", "1"},
		"image --keep-raw alone": {"image", "--from", card, "--to", filepath.Join(t.TempDir(), "card.img"), "--keep-raw"},
	} {
		code, _, errOut := cli(t, false, "", args...)
		if code != ExitUsage {
			t.Errorf("%s: exit %d: %s", name, code, errOut)
		}
		if strings.Contains(name, "profiles") && !strings.Contains(errOut, "android") {
			t.Errorf("%s: known names not listed: %s", name, errOut)
		}
	}
}

// A card's file and folder names reach the terminal: escape sequences, bells and the marks that
// reorder text are stripped from what scan prints.
func TestScanSummaryStripsControlCharacters(t *testing.T) {
	bad := "evil\x1b]52;c;cGF5bG9hZA==\x07\xe2\x80\xaegpj.exe"
	inv := scan.Inventory{Items: []scan.Item{{Rel: bad + "/a.jpg", Cat: profile.Photos, Size: 1}},
		Unlistable: []string{bad + "/sub"}}
	out := scanSummary("card"+"\x1b[2J", "fat32", inv)
	if strings.ContainsAny(out, "\x1b\x07\xe2\x80\xae") {
		t.Fatalf("control characters printed: %q", out)
	}
}

// %q doubled every backslash in a Windows path; the hint is quoted plainly.
func TestImageNextQuotesPlainly(t *testing.T) {
	to := filepath.Join(t.TempDir(), "card.img")
	if s := imageNext(to, false, false); !strings.Contains(s, `rescue --from "`+to+`"`) {
		t.Fatalf("%s", s)
	}
}

// OA2/SA-1: the swap check before a write fingerprinted only cards with no volume. A RAW volume
// has a drive letter and no file-system serial, so a same-size RAW card swapped into the reader
// while the user typed the confirmation was formatted.
func TestFixRawVolumeSwapNoticed(t *testing.T) {
	rig := mockCard(t)
	rig.vol.FS, rig.vol.RAW, rig.vol.Label, rig.vol.ID.FSSerial = "RAW", true, "", "" // no file system: no serial
	rig.disk.Volumes = []drives.Drive{rig.vol}
	var read []string
	diskFingerprint = func(dev string) string {
		read = append(read, dev)
		if len(read) > 1 {
			return "another card"
		}
		return "the card planned for"
	}
	code, _, errOut := cli(t, false, "", "fix", "--drive", "Q:", "--format", "--yes",
		"--confirm-label", "Q:", "--confirm-size", "64.0 GB")
	if code == 0 || len(rig.ran) != 0 || !strings.Contains(errOut, "changed") {
		t.Fatalf("exit %d ran %v read %v: %s", code, rig.ran, read, errOut)
	}
	if len(read) < 2 || read[0] != rig.vol.Device {
		t.Fatalf("fingerprinted %v, want the volume %s", read, rig.vol.Device)
	}
}

// OA4: a verified rescue of one folder (rescue --from E:\DCIM) carries the card's identity, and
// counted as a verified rescue of the whole card: Format then erased the rest without a warning.
// A recovery of deleted files isn't a rescue of the card either.
func TestVerifiedNeedsARescueOfTheWholeCard(t *testing.T) {
	var tg fix.Target
	tg.Vol = drives.Drive{Root: `Q:\`, Device: `\\.\Q:`, ID: drives.Identity{FSSerial: "AAAA-1111"}}
	tg.Disk.Path = `\\.\PhysicalDrive7`
	write := func(head string) string {
		dir := t.TempDir()
		man := filepath.Join(dir, "manifest.json")
		os.WriteFile(man, []byte(`{"Volume":{"FSSerial":"AAAA-1111"},`+head+`"Entries":[{"Source":"DCIM/a.jpg","Status":"OK"}]}`), 0o644)
		os.WriteFile(filepath.Join(dir, "verify_summary_20261110_000000_000.txt"), []byte("ALL FILES MATCH - the copy is a perfect copy.\n"), 0o644)
		return man
	}
	for name, c := range map[string]struct {
		head string
		ok   bool
		why  string
	}{
		"one folder":     {`"SourceKind":"folder","SourceRoot":"Q:\\DCIM",`, false, "not the whole card"},
		"recovery":       {`"SourceKind":"recover","SourceRoot":"Q:\\",`, false, "deleted files"},
		"older manifest": {`"SourceKind":"folder",`, false, "doesn't say what it copied"},
		"the card":       {`"SourceKind":"folder","SourceRoot":"Q:",`, true, ""},
		"the card, raw":  {`"SourceKind":"fat32","SourceRoot":"\\\\.\\Q:",`, true, ""},
		"the disk, raw":  {`"SourceKind":"fat32","SourceRoot":"\\\\.\\PhysicalDrive7",`, true, ""},
		"an image of it": {`"SourceKind":"fat32","SourceRoot":"D:\\card.img",`, false, "not the whole card"},
	} {
		if ok, why := verifiedRescue(write(c.head), tg); ok != c.ok || !strings.Contains(why, c.why) {
			t.Errorf("%s: ok %v why %q", name, ok, why)
		}
	}
}

// OA1: a manifest with no identity at all matched any card.
func TestVerifiedRefusesAManifestWithoutIdentity(t *testing.T) {
	dir := t.TempDir()
	man := filepath.Join(dir, "manifest.json")
	os.WriteFile(man, []byte(`{"SourceKind":"folder","SourceRoot":"Q:\\","Entries":[]}`), 0o644)
	os.WriteFile(filepath.Join(dir, "verify_summary_20261110_000000_000.txt"), []byte("ALL FILES MATCH\n"), 0o644)
	var tg fix.Target
	tg.Vol = drives.Drive{Root: `Q:\`, ID: drives.Identity{FSSerial: "AAAA-1111", Size: 64 << 30}}
	if ok, why := verifiedRescue(man, tg); ok || !strings.Contains(why, "different card") {
		t.Fatalf("ok %v why %q", ok, why)
	}
}

// SA-5: the confirmation printed the card's label as the card has it: escape sequences in a
// label reached the terminal.
func TestTypedConfirmStripsControlCharacters(t *testing.T) {
	rig := mockCard(t)
	rig.vol.Label = "CAM\x1b]52;c;cGF5bG9hZA==\x07\x1b[2J"
	rig.disk.Volumes = []drives.Drive{rig.vol}
	_, _, errOut := cli(t, true, "1\ny\nx\nx\n", "fix", "--drive", "Q:", "--format") // past the warning, start, label, size
	if strings.ContainsAny(errOut, "\x1b\x07") {
		t.Fatalf("control characters printed: %q", errOut)
	}
	if !strings.Contains(errOut, "type its label") {
		t.Fatalf("no confirmation asked: %q", errOut)
	}
}

// SU6: image --from <a folder> was accepted and imaged the whole disk the folder is on.
func TestImageFromAFolderIsAUsageError(t *testing.T) {
	card := testutil.WriteFolder(t, testutil.CameraCard())
	cardInReader(t, card)
	code, _, errOut := cli(t, false, "", "image", "--from", filepath.Join(card, "DCIM"), "--to", filepath.Join(t.TempDir(), "card.img"), "--yes")
	if code != ExitUsage || !strings.Contains(errOut, "folder") {
		t.Fatalf("exit %d: %s", code, errOut)
	}
}

// SU5: scan's CSV wrote card names as they are: a file named "=1+1.txt" became a formula when
// the report was opened in a spreadsheet.
func TestScanCSVCellsAreSafe(t *testing.T) {
	inv := scan.Inventory{Items: []scan.Item{{Rel: "=cmd/+x.jpg", Cat: profile.Photos, Size: 1}}}
	csvPath, err := writeScan(t.TempDir(), nil, inv, "summary")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(csvPath)
	if strings.Contains(string(b), ",=cmd") || strings.Contains(string(b), "\n=") {
		t.Fatalf("formula cell written:\n%s", b)
	}
	if !strings.Contains(string(b), "'=cmd") {
		t.Fatalf("path missing:\n%s", b)
	}
}

// SU10: "1 files", and a Next step that breaks on a path with spaces.
func TestScanSummaryWording(t *testing.T) {
	inv := scan.Inventory{Items: []scan.Item{{Rel: "Docs/a.pdf", Cat: profile.Documents, Size: 1}}}
	out := scanSummary(filepath.Join(t.TempDir(), "My Card"), "folder", inv)
	if strings.Contains(out, " 1 files") {
		t.Fatalf("plural for one:\n%s", out)
	}
	if !strings.Contains(out, `rescue --from "`) {
		t.Fatalf("path with spaces not quoted:\n%s", out)
	}
	if s := scanSummary("E:", "folder", inv); !strings.Contains(s, "rescue --from E:\n") {
		t.Fatalf("plain path quoted:\n%s", s)
	}
}

// OD13: --to is optional for rescue (it has a default), the usage line said otherwise.
func TestRescueUsageShowsToIsOptional(t *testing.T) {
	if u := rescueCmd(IO{}, &globals{}).Use; !strings.Contains(u, "[--to DIR]") {
		t.Fatalf("%s", u)
	}
}

// OA5: a card formatted without a partition table, unmounted (as Linux needs for repair and
// format), is listed as its own volume; its boot sector fills in the file system and label even
// when udev already gave its serial.
func TestUnmountedSuperfloppyCanBeFormatted(t *testing.T) {
	rig := mockCard(t)
	vol := drives.Drive{Device: "/dev/sdx", PhysicalID: "sdx", Total: 64 << 30, ID: drives.Identity{FSSerial: "1234-ABCD"}}
	rig.disk.Path, rig.disk.PhysicalID, rig.disk.Volumes = "/dev/sdx", "sdx", []drives.Drive{vol}
	old := probeVolume
	defer func() { probeVolume = old }()
	probeVolume = func(dev string) (fsread.Volume, bool) {
		return fsread.Volume{FS: "fat32", Label: "CAMERA", Serial: "1234-ABCD"}, dev == "/dev/sdx"
	}
	tg, err := findTarget("/dev/sdx")
	if err != nil {
		t.Fatal(err)
	}
	describeUnmounted(&tg)
	for _, c := range fix.Available("linux", tg, "") {
		if c.Name == "Format" && strings.Contains(c.Reason, "no partition") {
			t.Fatalf("format: %s", c.Reason)
		}
	}
	argv, err := fix.Command("linux", fix.OpFormat, tg, "")
	if err != nil || !slices.Contains(argv, "/dev/sdx") || !slices.Contains(argv, "CAMERA") || tg.Vol.FS != "fat32" {
		t.Fatalf("format %v %v (fs %q)", argv, err, tg.Vol.FS)
	}
}
