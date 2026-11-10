package scan

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"go.uber.org/goleak"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/fsread"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/profile"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/source"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/testutil"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

func scanFolder(t testing.TB, card testutil.Card) Inventory {
	t.Helper()
	src, err := source.OpenFolder(testutil.WriteFolder(t, card))
	if err != nil {
		t.Fatal(err)
	}
	inv, err := Scan(context.Background(), src, profile.Detect(src))
	if err != nil {
		t.Fatal(err)
	}
	return inv
}

func checkCard(t *testing.T, card testutil.Card, inv Inventory) {
	t.Helper()
	if len(inv.Items) != len(card) {
		t.Fatalf("%d items for %d files", len(inv.Items), len(card))
	}
	for _, it := range inv.Items {
		want, ok := card[it.Rel]
		if !ok {
			t.Fatalf("unexpected %s", it.Rel)
		}
		if it.Cat != want.Cat || it.Suggest != want.Suggest || it.Fragment != want.Fragment || it.Size != int64(len(want.Data)) {
			t.Errorf("%s: got cat=%s suggest=%q frag=%v size=%d, want %+v", it.Rel, it.Cat, it.Suggest, it.Fragment, it.Size, want.Expect)
		}
	}
}

func TestScanAndroidCard(t *testing.T) {
	card := testutil.AndroidCard(false)
	inv := scanFolder(t, card)
	checkCard(t, card, inv)
	if strings.Join(inv.Profiles, ",") != "Android,Generic" {
		t.Errorf("profiles %v", inv.Profiles)
	}
	if g := inv.Suggested["thumbnails"]; g.Files != 1 || g.Bytes != 800 {
		t.Errorf("thumbnails group %+v", g)
	}
	for _, it := range inv.Items {
		if it.Rel == "LOST.DIR/1111" && it.Ext != ".png" {
			t.Errorf("sniffed ext %q", it.Ext)
		}
	}
}

func TestScanCameraCard(t *testing.T) {
	checkCard(t, testutil.CameraCard(), scanFolder(t, testutil.CameraCard()))
}
func TestScanGoProCard(t *testing.T) {
	checkCard(t, testutil.GoProCard(), scanFolder(t, testutil.GoProCard()))
}
func TestScanDJICard(t *testing.T) {
	checkCard(t, testutil.DJICard(), scanFolder(t, testutil.DJICard()))
}
func TestScanAVCHDCard(t *testing.T) {
	checkCard(t, testutil.AVCHDCard(), scanFolder(t, testutil.AVCHDCard()))
}

// TestScanNoImplicitSkip: every file is listed, suggestions only tag.
func TestScanNoImplicitSkip(t *testing.T) {
	card := testutil.AndroidCard(false)
	inv := scanFolder(t, card)
	seen := map[string]bool{}
	for _, it := range inv.Items {
		seen[it.Rel] = true
	}
	for rel := range card {
		if !seen[rel] {
			t.Errorf("%s dropped", rel)
		}
	}
}

func TestScanFatImageEqualsFolder(t *testing.T) {
	card := testutil.AndroidCard(true)
	folder := scanFolder(t, card)
	img := testutil.FAT32Image(t, card, 64<<20)
	f, err := os.Open(img)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st, _ := f.Stat()
	vs, err := fsread.Probe(f, st.Size())
	if err != nil {
		t.Fatal(err)
	}
	src, err := fsread.Open(f, vs[0])
	if err != nil {
		t.Fatal(err)
	}
	raw, err := Scan(context.Background(), src, profile.Detect(src))
	if err != nil {
		t.Fatal(err)
	}
	if len(raw.Items) != len(folder.Items) {
		in := map[string]bool{}
		for _, it := range raw.Items {
			in[it.Rel] = true
		}
		var missing []string
		for _, it := range folder.Items {
			if !in[it.Rel] {
				missing = append(missing, it.Rel)
			}
		}
		t.Fatalf("raw %d items, folder %d; missing from the image: %v", len(raw.Items), len(folder.Items), missing)
	}
	for i := range raw.Items {
		a, b := raw.Items[i], folder.Items[i]
		if a.Rel != b.Rel || a.Cat != b.Cat || a.Size != b.Size || a.Suggest != b.Suggest {
			t.Errorf("item %d: raw %+v folder %+v", i, a, b)
		}
	}
}

func TestLostDirTinyFiles(t *testing.T) {
	inv := scanFolder(t, testutil.Card{"LOST.DIR/1": {Data: []byte("ab")}, "LOST.DIR/2": {Data: []byte{}}})
	for _, it := range inv.Items {
		if it.Cat != profile.Other || !it.Fragment {
			t.Errorf("%+v", it)
		}
	}
}

func TestSortOrder(t *testing.T) {
	inv := scanFolder(t, testutil.AndroidCard(false))
	prev := Item{}
	for i, it := range inv.Items {
		if i > 0 && less(it, prev) {
			t.Fatalf("%s before %s", prev.Rel, it.Rel)
		}
		prev = it
	}
	if !inv.Items[len(inv.Items)-1].Fragment {
		t.Error("fragments must come last")
	}
	if inv.Items[0].Cat != profile.Photos {
		t.Errorf("first item %+v", inv.Items[0])
	}
}

func TestUnlistableDir(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs unix permissions as non-root")
	}
	dir := testutil.WriteFolder(t, testutil.Card{"ok/a.jpg": {Data: []byte("x")}, "locked/b.jpg": {Data: []byte("y")}})
	os.Chmod(filepath.Join(dir, "locked"), 0)
	defer os.Chmod(filepath.Join(dir, "locked"), 0o755)
	src, _ := source.OpenFolder(dir)
	inv, err := Scan(context.Background(), src, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Unlistable) != 1 || !strings.Contains(inv.Unlistable[0], "locked") {
		t.Fatalf("%v", inv.Unlistable)
	}
}

func TestScanCancelled(t *testing.T) {
	src, _ := source.OpenFolder(testutil.WriteFolder(t, testutil.CameraCard()))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Scan(ctx, src, nil); err == nil {
		t.Fatal("cancelled scan returned no error")
	}
}

func BenchmarkScan10k(b *testing.B) {
	card := testutil.Card{}
	for i := 0; i < 10_000; i++ {
		card[fmt.Sprintf("DCIM/Camera/%03d/IMG_%05d.jpg", i/500, i)] = testutil.File{Data: []byte("x")}
	}
	dir := testutil.WriteFolder(b, card)
	src, _ := source.OpenFolder(dir)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Scan(context.Background(), src, profile.Detect(src))
	}
}
