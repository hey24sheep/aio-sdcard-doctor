package rescue

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/drives"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/job"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/source"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/testutil"
)

// offers records the choices of the "card stopped responding" question and stops there.
type offers struct{ got []job.Choice }

func (o *offers) Ask(_ context.Context, is job.Issue) job.Choice {
	if is.Kind == "card-not-responding" {
		o.got = is.Choices
		return job.Stop
	}
	return is.Default
}

// OU7: "make a full backup image instead" only makes sense for a whole card or device; copying
// one folder of the card (or an image file) offered it and then ended with nothing to do.
func TestImageInsteadOfferedOnlyForAWholeCard(t *testing.T) {
	card := testutil.CameraCard()
	for name, sub := range map[string]string{"whole card": "", "one folder": "DCIM"} {
		t.Run(name, func(t *testing.T) {
			src, dst := testutil.WriteFolder(t, card), filepath.Join(t.TempDir(), "out")
			f := newFake(t, src)
			for rel := range card {
				f.openErr[rel] = []error{source.ErrTooManyRestarts}
			}
			from := filepath.Join(src, sub)
			r := newRescue(from, dst, f)
			r.LookupDrive = func(p string) (drives.Drive, error) {
				if p == dst || filepath.Dir(p) == filepath.Dir(dst) {
					return drives.Drive{Root: filepath.VolumeName(dst) + `\`, PhysicalID: "disk", FS: "NTFS", Free: 1 << 40}, nil
				}
				return drives.Drive{Root: src, Removable: true, PhysicalID: "card:" + src, FS: "exFAT"}, nil
			}
			o := &offers{}
			env := job.Env{Prompt: o}
			p, err := r.Plan(context.Background(), env)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			r.Run(context.Background(), env, p)
			if want := sub == ""; slices.Contains(o.got, job.Image) != want {
				t.Fatalf("choices %v (image offered should be %v)", o.got, want)
			}
		})
	}
}
