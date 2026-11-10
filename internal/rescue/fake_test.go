package rescue

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/drives"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/job"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/salvage"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/salvage/faultreader"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/source"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/worker"
)

const envWorker = "AIO_RESCUE_TEST_WORKER"

func TestMain(m *testing.M) {
	if os.Getenv(envWorker) == "1" {
		os.Exit(worker.Main())
	}
	if os.Getenv(envChild) != "" {
		childRescue()
		return
	}
	worker.Command = func() *exec.Cmd {
		c := exec.Command(os.Args[0], "-test.run=^$")
		c.Env = append(os.Environ(), envWorker+"=1")
		return c
	}
	salvage.RetrySleep = 0
	goleak.VerifyTestMain(m)
}

// fake is a folder source with fault injection.
type fake struct {
	source.Source
	mu        sync.Mutex
	root      string
	bad       map[string][]faultreader.Range
	openErr   map[string][]error // popped per OpenAt; the last one repeats
	opens     map[string]int
	opt       faultreader.Options
	retargets int
	gone      bool
	id        drives.Identity
	listErr   map[string]error // folders that can't be listed (a damaged directory)
}

func (f *fake) ReadDir(name string) ([]fs.DirEntry, error) {
	f.mu.Lock()
	err := f.listErr[name]
	f.mu.Unlock()
	if err != nil {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: err}
	}
	return f.Source.ReadDir(name)
}

func newFake(t testing.TB, dir string) *fake {
	src, err := source.OpenFolder(dir)
	if err != nil {
		t.Fatal(err)
	}
	return &fake{Source: src, root: dir, id: drives.Identity{FSSerial: fmt.Sprintf("FAKE-%x", sha256.Sum256([]byte(dir)))[:20], Size: 64 << 30}, bad: map[string][]faultreader.Range{}, openErr: map[string][]error{}, opens: map[string]int{}}
}

func (f *fake) OpenAt(name string) (io.ReaderAt, int64, io.Closer, error) {
	f.mu.Lock()
	f.opens[name]++
	if f.gone {
		f.mu.Unlock()
		return nil, 0, nil, errors.New("device not ready")
	}
	if q := f.openErr[name]; len(q) > 0 {
		err := q[0]
		if len(q) > 1 {
			f.openErr[name] = q[1:]
		}
		f.mu.Unlock()
		if err != nil {
			return nil, 0, nil, err
		}
	} else {
		f.mu.Unlock()
	}
	b, err := os.ReadFile(filepath.Join(f.root, filepath.FromSlash(name)))
	if err != nil {
		return nil, 0, nil, err
	}
	return faultreader.New(b, f.bad[name], f.opt), int64(len(b)), io.NopCloser(nil), nil
}

// Identity belongs to the card folder (stable across runs and re-plugs) and is unique to it, so
// parallel test packages never share a card lock.
func (f *fake) Identity() drives.Identity { return f.id }

func (f *fake) Retarget(s worker.Spec) error {
	src, err := source.OpenFolder(s.From)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Source, f.root, f.gone = src, s.From, false
	f.retargets++
	return nil
}
func (f *fake) ResetBudget() {}
func (f *fake) Close() error { return nil }

// prompter answers by issue kind and records what was asked.
type prompter struct {
	mu      sync.Mutex
	answers map[string]job.Choice
	asked   map[string]int
}

func (p *prompter) Ask(_ context.Context, is job.Issue) job.Choice { return p.ask(is) }

func (p *prompter) ask(is job.Issue) job.Choice {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.asked == nil {
		p.asked = map[string]int{}
	}
	p.asked[is.Kind]++
	if c, ok := p.answers[is.Kind]; ok {
		return c
	}
	return is.Default
}

func testDrives(src, dst string) func(string) (drives.Drive, error) {
	return func(p string) (drives.Drive, error) {
		if p == src {
			return drives.Drive{Root: src, Removable: true, PhysicalID: "card", FS: "exFAT"}, nil
		}
		return drives.Drive{Root: filepath.VolumeName(dst) + `\`, PhysicalID: "disk", FS: "NTFS", Free: 1 << 40}, nil
	}
}

func newRescue(src, dst string, f *fake) *Rescue {
	return &Rescue{From: src, Dest: dst, Interactive: true,
		OpenSource:  func(worker.Spec, time.Duration) (Source, error) { return f, nil },
		Present:     func(drives.Identity, string) (string, bool) { return src, true },
		Free:        func(string) (uint64, error) { return 1 << 40, nil },
		LookupDrive: testDrives(src, dst),
		CoolDown:    time.Millisecond, PollEvery: time.Millisecond, WaitTimeout: 300 * time.Millisecond,
	}
}
