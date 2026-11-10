package preflight

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/drives"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/job"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/manifest"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/scan"
)

const GiB = 1 << 30

// base is a clean rescue: removable card, roomy NTFS destination, empty destination folder.
func base(t *testing.T) Input {
	dst := t.TempDir()
	return Input{
		Action:  "rescue",
		Src:     drives.Drive{Root: `E:\`, Removable: true, PhysicalID: "PhysicalDrive2", FS: "exFAT"},
		Dst:     drives.Drive{Root: filepath.VolumeName(dst) + `\`, FS: "NTFS", Free: 500 * GiB, PhysicalID: "PhysicalDrive0"},
		DstPath: dst,
		Items:   []scan.Item{{Rel: "a.jpg", Size: 1 << 20}, {Rel: "b.mp4", Size: 2 * GiB}},
	}
}

func kinds(is []job.Issue) map[string]job.Issue {
	m := map[string]job.Issue{}
	for _, i := range is {
		m[i.Kind] = i
	}
	return m
}

func expect(t *testing.T, in Input, kind string, sev job.Severity) job.Issue {
	t.Helper()
	is, ok := kinds(Check(in))[kind]
	if !ok {
		t.Fatalf("no %s issue in %+v", kind, Check(in))
	}
	if is.Severity != sev || is.Title == "" || len(is.Choices) == 0 {
		t.Fatalf("%s: %+v", kind, is)
	}
	return is
}

func TestPreflightCleanNoIssues(t *testing.T) {
	if is := Check(base(t)); len(is) != 0 {
		t.Fatalf("clean run raised %+v", is)
	}
}

func TestPreflightDestInsideSource(t *testing.T) {
	in := base(t)
	in.Src.Root = filepath.Dir(in.DstPath)
	expect(t, in, "dest-inside-source", job.Block)
}

func TestPreflightSamePhysicalDisk(t *testing.T) {
	in := base(t)
	in.Dst.PhysicalID = "PhysicalDrive2"
	expect(t, in, "same-disk", job.Block)
}

func TestPreflightForeignNonEmptyDest(t *testing.T) {
	in := base(t)
	os.WriteFile(filepath.Join(in.DstPath, "someone-elses.txt"), []byte("x"), 0o644)
	expect(t, in, "dest-foreign", job.Block)
	os.MkdirAll(filepath.Join(in.DstPath, "Reports"), 0o755)
	os.WriteFile(filepath.Join(in.DstPath, "Reports", "state.db"), []byte("x"), 0o644)
	if _, ok := kinds(Check(in))["dest-foreign"]; ok {
		t.Fatal("our own folder refused")
	}
}

func TestPreflightSourceLocked(t *testing.T) {
	in := base(t)
	in.Src.Locked = true
	is := expect(t, in, "source-locked", job.Block)
	if !strings.Contains(is.Detail, "unlock") {
		t.Fatalf("%+v", is)
	}
}

func TestPreflightDestWriteProtected(t *testing.T) {
	in := base(t)
	in.Dst.WriteProtected = true
	expect(t, in, "dest-not-writable", job.Block)
}

func TestPreflightImageNoSpace(t *testing.T) {
	in := base(t)
	in.Action, in.DeviceSize, in.Dst.Free = "image", 64*GiB, 64*GiB
	expect(t, in, "no-space-image", job.Block)
}

func TestPreflightImageOnFAT32(t *testing.T) {
	in := base(t)
	in.Action, in.DeviceSize, in.Dst.FS = "image", 8*GiB, "FAT32"
	is := expect(t, in, "fat-image", job.Block)
	if !strings.Contains(is.Detail, "NTFS/exFAT") {
		t.Fatalf("%+v", is)
	}
}

func TestPreflightShortSpace(t *testing.T) {
	in := base(t)
	in.Dst.Free = 2 * GiB
	is := expect(t, in, "short-space", job.Warn)
	if !strings.Contains(is.Title, "short by") {
		t.Fatalf("%+v", is)
	}
	in.Already = func(src string) (manifest.Entry, bool) {
		return manifest.Entry{Status: manifest.OK}, src == "b.mp4"
	}
	if _, ok := kinds(Check(in))["short-space"]; ok {
		t.Fatal("already-copied bytes counted as needed")
	}
}

func TestPreflightFATBigFiles(t *testing.T) {
	in := base(t)
	in.Dst.FS = "FAT32"
	in.Items = append(in.Items, scan.Item{Rel: "huge.mp4", Size: 5 * GiB})
	is := expect(t, in, "fat-big-files", job.Warn)
	if !strings.Contains(is.Detail, "huge.mp4") {
		t.Fatalf("%+v", is)
	}
}

func TestPreflightSystemDriveLow(t *testing.T) {
	in := base(t)
	in.Dst.System, in.Dst.Free = true, 15*GiB
	expect(t, in, "system-drive-low", job.Warn)
}

func TestPreflightNotRemovable(t *testing.T) {
	in := base(t)
	in.Src.Removable = false
	expect(t, in, "not-removable", job.Warn)
}

func TestPreflightInfoIssues(t *testing.T) {
	in := base(t)
	in.Suggested = map[string]scan.Group{"thumbnails": {Files: 12, Bytes: 4096}}
	in.Src.RAW = true
	in.NeedsAdmin = true
	in.Resume = &job.ResumeInfo{Done: 7812, Total: 9624}
	expect(t, in, "suggestions", job.Info)
	expect(t, in, "raw-source", job.Info)
	if is := expect(t, in, "needs-admin", job.Info); is.Default != job.Elevate {
		t.Fatalf("%+v", is)
	}
	if is := expect(t, in, "resume", job.Info); !strings.Contains(is.Title, "7,812 of 9,624") || is.Default != job.Resume {
		t.Fatalf("%+v", is)
	}
	in.Admin = true
	if _, ok := kinds(Check(in))["needs-admin"]; ok {
		t.Fatal("needs-admin raised while elevated")
	}
}

// TestPreflightTwiceStillOurs: the writability probe leaves Reports/ behind; planning again in
// the same run must not then call the folder foreign.
func TestPreflightTwiceStillOurs(t *testing.T) {
	in := base(t)
	Check(in)
	if _, ok := kinds(Check(in))["dest-foreign"]; ok {
		t.Fatal("second pre-flight refused the folder the first one probed")
	}
}

// Copying between two internal drives on one physical disk is allowed (with a note); only a
// card or USB source must go to another disk.
func TestSameDiskInternalIsWarning(t *testing.T) {
	in := base(t)
	in.Src.Removable = false
	in.Dst.PhysicalID = in.Src.PhysicalID
	ks := kinds(Check(in))
	if _, ok := ks["same-disk"]; ok {
		t.Fatal("internal→internal transfer blocked")
	}
	if is, ok := ks["same-disk-internal"]; !ok || is.Severity != job.Warn {
		t.Fatalf("%+v", Check(in))
	}
}

// Checking a destination that doesn't exist yet leaves nothing behind (the user may cancel).
func TestPreflightLeavesNothingBehind(t *testing.T) {
	in := base(t)
	in.DstPath = filepath.Join(in.DstPath, "new rescue")
	Check(in)
	if _, err := os.Stat(in.DstPath); !os.IsNotExist(err) {
		t.Fatal("pre-flight created the destination")
	}
}
