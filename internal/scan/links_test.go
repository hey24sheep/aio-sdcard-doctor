package scan

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/source"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/testutil"
)

// Symbolic links are not followed, but they are listed — never silently dropped.
func TestScanListsLinks(t *testing.T) {
	dir := testutil.WriteFolder(t, testutil.Card{"DCIM/a.jpg": {Data: []byte("x")}})
	if err := os.Symlink(filepath.Join(dir, "DCIM", "a.jpg"), filepath.Join(dir, "shortcut.jpg")); err != nil {
		t.Skipf("symlinks unavailable here: %v", err)
	}
	src, _ := source.OpenFolder(dir)
	inv, err := Scan(context.Background(), src, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Links) != 1 || inv.Links[0] != "shortcut.jpg" {
		t.Fatalf("links %v", inv.Links)
	}
}
