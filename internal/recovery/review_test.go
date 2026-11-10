package recovery

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/destfs"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/drives"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/fsread"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/job"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/manifest"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/profile"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/testutil"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/worker"
)

func plan(t *testing.T, r *Recover) job.Plan {
	t.Helper()
	if r.StallTimeout == 0 {
		r.StallTimeout = 5 * time.Second
	}
	p, err := r.Plan(context.Background(), env())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func blocked(p job.Plan, kind string) bool {
	for _, is := range p.Issues {
		if is.Severity == job.Block && is.Kind == kind {
			return true
		}
	}
	return false
}

// TestValidationChecksStructure (review I1): a deleted MP3 whose clusters now hold other data
// (a header, then garbage) is not written as recovered.
func TestValidationChecksStructure(t *testing.T) {
	song := append([]byte("ID3\x03\x00\x00\x00\x00\x00\x00"), 0xFF, 0xFB, 0x90, 0x00)
	song = append(song, bytes.Repeat([]byte("not audio "), 3000)...)
	img := testutil.FAT32Image(t, testutil.Card{"MUSIC/SONG.MP3": {Data: song}}, 64<<20)
	testutil.DeleteFATFile(t, img, "SONG    MP3")
	dst := filepath.Join(t.TempDir(), "out")
	run(t, &Recover{From: img, Dest: dst})
	e := entries(t, dst)["MUSIC/_ONG.MP3"]
	if e.Status != manifest.DELETED_UNRECOVERABLE || !strings.Contains(e.Error, "mp3") {
		t.Fatalf("entry %+v", e)
	}
}

// TestDeepIndexOtherCardBlocked (review I3): a folder holding another card's recovery is refused.
func TestDeepIndexOtherCardBlocked(t *testing.T) {
	a := testutil.FAT32Image(t, testutil.Card{"A.JPG": {Data: realJPEG(t, 50, 50, 1)}}, 64<<20)
	b := testutil.FAT32Image(t, testutil.Card{"B.JPG": {Data: realJPEG(t, 50, 50, 2)}}, 64<<20)
	raw, _ := os.ReadFile(b)
	copy(raw[67:71], []byte{0xB0, 0xB1, 0xB2, 0xB3}) // FAT32 volume serial: another card
	os.WriteFile(b, raw, 0o644)
	dst := filepath.Join(t.TempDir(), "out")
	run(t, &Recover{From: a, Dest: dst, Deep: true})
	r := &Recover{From: b, Dest: dst, Deep: true}
	defer r.Close()
	if p := plan(t, r); !blocked(p, "dest-other-card") {
		t.Fatalf("issues %+v", p.Issues)
	}
}

// TestDeepIndexKeepsAllTypes (review I3, M1): finds of types not chosen now are listed EXCLUDED and
// recovered when a later run chooses them; a --thorough run after an aligned one scans again.
func TestDeepIndexKeepsAllTypes(t *testing.T) {
	lost := realJPEG(t, 140, 100, 7)
	img := testutil.FAT32Image(t, testutil.Card{"LOST.JPG": {Data: lost}}, 64<<20)
	testutil.DeleteFATFile(t, img, "LOST    JPG")
	dst := filepath.Join(t.TempDir(), "out")
	_, res := run(t, &Recover{From: img, Dest: dst, Deep: true, NoUndelete: true, Types: []profile.Category{profile.Videos}})
	if res.Counters[string(manifest.EXCLUDED)] == 0 {
		t.Fatalf("photo not listed as excluded: %v", res.Counters)
	}
	_, res = run(t, &Recover{From: img, Dest: dst, Deep: true, NoUndelete: true, Types: []profile.Category{profile.Photos}})
	if res.Counters[string(manifest.CARVED)] != 1 {
		t.Fatalf("second run with photos: %v", res.Counters)
	}
	r := &Recover{From: img, Dest: dst, Deep: true, NoUndelete: true, Thorough: true, List: true}
	run(t, r)
	if r.resumedIndex {
		t.Fatal("--thorough reused the aligned index")
	}
}

// TestCompressRerunNotDuplicate (review I4): a second --compress run knows the file is stored.
func TestCompressRerunNotDuplicate(t *testing.T) {
	c := card(t)
	dst := filepath.Join(t.TempDir(), "out")
	run(t, &Recover{From: c.path, Dest: dst, Compress: true})
	_, res := run(t, &Recover{From: c.path, Dest: dst, Compress: true})
	if res.Counters[string(manifest.ALREADY_COPIED)] != 1 || res.Counters[string(manifest.DUPLICATE)] != 0 {
		t.Fatalf("second run %v", res.Counters)
	}
	if e := entries(t, dst)["DCIM/_ONE.JPG"]; e.Status != manifest.DELETED_RECOVERED || e.Dest == "" {
		t.Fatalf("entry %+v", e)
	}
}

// TestMovedFileRecoveredAgain (review I4): a recovered file the user moved away comes back on a
// re-run instead of being called a duplicate of itself.
func TestMovedFileRecoveredAgain(t *testing.T) {
	c := card(t)
	dst := filepath.Join(t.TempDir(), "out")
	run(t, &Recover{From: c.path, Dest: dst})
	f := filepath.Join(dst, "Undeleted", "DCIM", "_ONE.JPG")
	os.Remove(f)
	_, res := run(t, &Recover{From: c.path, Dest: dst})
	if got, err := os.ReadFile(f); err != nil || !bytes.Equal(got, c.gone) || res.Counters[string(manifest.DUPLICATE)] != 0 {
		t.Fatalf("err %v counters %v", err, res.Counters)
	}
}

// TestDestinationFullStops (review M4): a full destination stops the run with an error; files are
// not reported as unrecoverable because the destination failed.
func TestDestinationFullStops(t *testing.T) {
	c := card(t)
	dst := filepath.Join(t.TempDir(), "out")
	os.MkdirAll(dst, 0o755)
	full := errors.New("no space left on the destination")
	r := &Recover{From: c.path, Dest: dst, StallTimeout: 5 * time.Second}
	defer r.Close()
	p := plan(t, r)
	e := env()
	e.Out = destfs.NewQuota(destfs.New(dst, c.path), 1000, full)
	if _, err := r.Run(context.Background(), e, p); !errors.Is(err, full) {
		t.Fatalf("err %v", err)
	}
}

// TestCardLockedByOtherAction (review I3: R14): recover takes the per-card lock like rescue.
func TestCardLockedByOtherAction(t *testing.T) {
	c := card(t)
	r := &Recover{From: c.path, Dest: filepath.Join(t.TempDir(), "out"), StallTimeout: 5 * time.Second}
	defer r.Close()
	p := plan(t, r)
	unlock, err := job.LockSource(r.src.Identity().LockKey())
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if _, err := r.Run(context.Background(), env(), p); err == nil {
		t.Fatal("ran while another action held the card")
	}
}

// fakeSrc is a card image with bad sectors, read through a fake source (no worker).
type fakeSrc struct {
	dev  io.ReaderAt
	size int64
}

func (f *fakeSrc) ReadDir(string) ([]fs.DirEntry, error) { return nil, errors.ErrUnsupported }
func (f *fakeSrc) Stat(string) (fs.FileInfo, error)      { return nil, errors.ErrUnsupported }
func (f *fakeSrc) Open(string) (fs.File, error)          { return nil, errors.ErrUnsupported }
func (f *fakeSrc) OpenAt(string) (io.ReaderAt, int64, io.Closer, error) {
	return nil, 0, nil, errors.ErrUnsupported
}
func (f *fakeSrc) Kind() string              { return "fat32" }
func (f *fakeSrc) Identity() drives.Identity { return drives.Identity{FSSerial: "FAKE0001"} }
func (f *fakeSrc) Volumes() []fsread.Volume  { return []fsread.Volume{{Size: f.size, FS: "unknown"}} }
func (f *fakeSrc) Caps() []job.Capability    { return nil }
func (f *fakeSrc) ReadError() string         { return "unreadable file system" }
func (f *fakeSrc) Close() error              { return nil }
func (f *fakeSrc) OpenDevice() (io.ReaderAt, int64, io.Closer, error) {
	return f.dev, f.size, io.NopCloser(nil), nil
}

type badAt struct {
	r        io.ReaderAt
	from, to int64
}

func (b badAt) ReadAt(p []byte, off int64) (int, error) {
	if off < b.to && off+int64(len(p)) > b.from {
		return 0, errors.New("I/O error")
	}
	return b.r.ReadAt(p, off)
}

// TestUnreadableAreasListed (review I5): a find with a bad sector inside it is listed damaged, and
// an area that couldn't be read is listed too — nothing disappears silently.
func TestUnreadableAreasListed(t *testing.T) {
	img := make([]byte, 8<<20)
	jpg := realJPEG(t, 300, 200, 4)
	copy(img[1<<20:], jpg)
	dev := badAt{r: bytes.NewReader(img), from: 1<<20 + int64(len(jpg)) - 1024, to: 1<<20 + int64(len(jpg)) - 512}
	dst := filepath.Join(t.TempDir(), "out")
	r := &Recover{From: filepath.Join(t.TempDir(), "card.img"), Dest: dst, Deep: true,
		OpenSource: func(worker.Spec, time.Duration) (Source, error) {
			return &fakeSrc{dev: dev, size: int64(len(img))}, nil
		}}
	os.WriteFile(r.From, nil, 0o644)
	run(t, r)
	var damaged, unreadable bool
	for _, e := range entries(t, dst) {
		damaged = damaged || e.Status == manifest.CARVED_DAMAGED && strings.Contains(e.Error, "read")
		unreadable = unreadable || e.Status == manifest.UNREADABLE
	}
	if !damaged || !unreadable {
		t.Fatalf("damaged %v unreadable %v: %v", damaged, unreadable, entries(t, dst))
	}
}

// Final review I2: a find too big for a FAT32 destination is reported and the run goes on with the
// next file (it used to stop the whole recovery on that file, every time).
// Re-review #3: a ZIP part never splits one file, so Compress doesn't lift the limit; Linux
// calls FAT "vfat" and macOS "msdos".
func TestRecoverFAT32TooLargeContinues(t *testing.T) {
	old := fatMaxFile
	fatMaxFile = 10_000
	defer func() { fatMaxFile = old }()
	for _, tc := range []struct {
		fs       string
		compress bool
	}{{"FAT32", false}, {"FAT32", true}, {"vfat", false}, {"msdos", true}} {
		t.Run(fmt.Sprintf("%s compress=%v", tc.fs, tc.compress), func(t *testing.T) {
			c := card(t)
			dst := filepath.Join(t.TempDir(), "out")
			r := &Recover{From: c.path, Dest: dst, Compress: tc.compress,
				LookupDrive: func(string) (drives.Drive, error) { return drives.Drive{FS: tc.fs, Free: 1 << 40}, nil }}
			_, res := run(t, r)
			es := entries(t, dst)
			if e := es["DCIM/_ONE.JPG"]; e.Status != manifest.FAILED || !strings.Contains(e.Error, "FAT32") {
				t.Fatalf("big file %+v", e)
			}
			if e := es["DCIM/_AD.JPG"]; e.Status == "" {
				t.Fatal("the run stopped at the big file: the next one was never looked at")
			}
			if res.Clean {
				t.Fatal("clean with a file left behind")
			}
		})
	}
}

// Final review I3: a photo over 100 MP is not decoded (memory), but it is not called damaged
// either — it is recovered, and the report says it was not checked picture by picture.
func TestRecoverHugePhotoNotDamaged(t *testing.T) {
	jpg := realJPEG(t, 64, 48, 9)
	i := bytes.Index(jpg, []byte{0xFF, 0xC0})
	if i < 0 {
		t.Fatal("no SOF0")
	}
	copy(jpg[i+5:i+9], []byte{0x27, 0x10, 0x2E, 0xE0}) // 10000 × 12000 = 120 MP
	img := testutil.FAT32Image(t, testutil.Card{"DCIM/PANO.JPG": {Data: jpg}}, 64<<20)
	testutil.DeleteFATFile(t, img, "PANO    JPG")
	dst := filepath.Join(t.TempDir(), "out")
	run(t, &Recover{From: img, Dest: dst})
	e := entries(t, dst)["DCIM/_ANO.JPG"]
	if e.Status != manifest.DELETED_RECOVERED || !strings.Contains(e.Error, "not decoded") {
		t.Fatalf("entry %+v", e)
	}
}
