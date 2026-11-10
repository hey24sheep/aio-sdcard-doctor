package cli

import (
	"bytes"
	"image"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/testutil"
)

func jpegBytes(t *testing.T, seed byte) []byte {
	m := image.NewGray(image.Rect(0, 0, 90, 70))
	v := uint32(seed)
	for i := range m.Pix {
		v = v*1103515245 + 12345
		m.Pix[i] = byte(v >> 16)
	}
	var b bytes.Buffer
	jpeg.Encode(&b, m, &jpeg.Options{Quality: 90})
	return b.Bytes()
}

func deletedCard(t *testing.T) (img string, gone []byte) {
	gone = jpegBytes(t, 4)
	img = testutil.FAT32Image(t, testutil.Card{"DCIM/KEEP.JPG": {Data: jpegBytes(t, 5)}, "DCIM/GONE.JPG": {Data: gone}}, 64<<20)
	testutil.DeleteFATFile(t, img, "GONE    JPG")
	return img, gone
}

func TestRecoverCommand(t *testing.T) {
	img, gone := deletedCard(t)
	dst := filepath.Join(t.TempDir(), "out")
	code, out, errOut := cli(t, false, "", "recover", "--from", img, "--to", dst, "--min-size", "0", "--yes")
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	for _, want := range []string{"Please check before it starts", "deleted files the card still names: 1 (1 Excellent", "Rescue first", "DELETED-RECOVERED"} {
		if !strings.Contains(out+errOut, want) {
			t.Errorf("output lacks %q:\n%s\n%s", want, out, errOut)
		}
	}
	if got, err := os.ReadFile(filepath.Join(dst, "Undeleted", "DCIM", "_ONE.JPG")); err != nil || !bytes.Equal(got, gone) {
		t.Fatalf("recovered: %v", err)
	}
}

func TestRecoverNeedsYesNonTTY(t *testing.T) {
	img, _ := deletedCard(t)
	dst := filepath.Join(t.TempDir(), "out")
	if code, _, errOut := cli(t, false, "", "recover", "--from", img, "--to", dst); code != ExitError || !strings.Contains(errOut, "--yes") {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(dst, "Undeleted")); err == nil {
		t.Fatal("recovered without confirmation")
	}
}

func TestRecoverListThenSelect(t *testing.T) {
	img, gone := deletedCard(t)
	dst := filepath.Join(t.TempDir(), "out")
	if code, out, errOut := cli(t, false, "", "recover", "--from", img, "--to", dst, "--list", "--yes"); code != 0 {
		t.Fatalf("list exit %d\n%s\n%s", code, out, errOut)
	}
	lists, _ := filepath.Glob(filepath.Join(dst, "Reports", "recover_list_*.csv"))
	if len(lists) != 1 {
		t.Fatalf("lists %v", lists)
	}
	if _, err := os.Stat(filepath.Join(dst, "Undeleted")); err == nil {
		t.Fatal("--list recovered files")
	}
	sel := filepath.Join(t.TempDir(), "pick.txt")
	os.WriteFile(sel, []byte("DCIM/_ONE.JPG\n"), 0o644)
	if code, out, errOut := cli(t, false, "", "recover", "--from", img, "--to", dst, "--select", sel, "--min-size", "0", "--yes"); code != 0 {
		t.Fatalf("select exit %d\n%s\n%s", code, out, errOut)
	}
	if got, _ := os.ReadFile(filepath.Join(dst, "Undeleted", "DCIM", "_ONE.JPG")); !bytes.Equal(got, gone) {
		t.Fatal("selected file not recovered")
	}
}

func TestRecoverFlagErrors(t *testing.T) {
	img, _ := deletedCard(t)
	for _, args := range [][]string{
		{"recover", "--from", img},                                    // no --to
		{"recover", "--from", img, "--to", "x", "--min-size", "lots"}, // bad size
		{"recover", "--from", img, "--to", "x", "--states", "unrecoverable"},
	} {
		if code, _, errOut := cli(t, false, "", args...); code != ExitUsage {
			t.Errorf("%v: exit %d (%s)", args, code, errOut)
		}
	}
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]int64{"0": 0, "20KB": 20 << 10, "1.5MB": 3 << 19, "2gb": 2 << 30, "512": 512} {
		if got, err := parseSize(in); err != nil || got != want {
			t.Errorf("%s → %d %v (want %d)", in, got, err, want)
		}
	}
}
