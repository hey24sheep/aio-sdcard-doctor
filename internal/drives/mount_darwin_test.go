package drives

import "testing"

// macOS mounts the system read-only at / and the user's data at /System/Volumes/Data, joined by
// firmlinks: a path's longest mount-point prefix is "/", which is the wrong volume. A folder in
// the user's temp directory must resolve to the writable data volume.
func TestLookupFollowsFirmlinks(t *testing.T) {
	d, err := Lookup(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if d.Root == "/" || d.WriteProtected {
		t.Fatalf("temp folder resolved to %q (write-protected %v)", d.Root, d.WriteProtected)
	}
}
