package imaging

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/archive"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/destfs"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/drives"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/elevate"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/job"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/preflight"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/retry"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/source"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/worker"
)

var waitUntil = retry.WaitUntil

// Device is the raw card being imaged (the worker in production).
type Device interface {
	io.ReaderAt
	Size() int64
	Identity() drives.Identity
	Reopen(path string) error
	Close() error
}

// Meta is card.img.json.
type Meta struct {
	Device, Model, PhysicalID string
	FSSerial                  string // file-system serial of the card when imaging started
	Size, BadBytes            int64
	SHA256                    string // of the complete raw image
	XZ                        string // compressed image file, when Compress was ticked
	Complete                  bool
	Created                   time.Time
	Fingerprint               []drives.FPBlock
}

// Image is the "Make a full backup image" action.
type Image struct {
	Device       string // \\.\PhysicalDriveN, /dev/sdX, /dev/diskN, or an image file
	Out          string // card.img
	RetryPasses  int    // 0–5
	StallTimeout time.Duration
	WaitTimeout  time.Duration
	Compress     bool
	KeepRaw      bool

	// Seams.
	OpenDevice    func() (Device, error)
	Present       func(id drives.Identity, path string) (string, bool)
	LookupDrive   func(string) (drives.Drive, error)
	Now           func() time.Time
	SaveEvery     time.Duration
	PollEvery     time.Duration
	PresenceEvery time.Duration // how often a failed read may re-check that the card is there

	dev  Device
	size int64
	id   drives.Identity // the card: worker identity plus serial/model from the device list
}

// errDifferentCard: the device that came back is not the card being imaged (R2: no override).
var errDifferentCard = errors.New("this looks like a different card than the one in the image (content differs) — start a new image")

func (im *Image) Name() string { return "image" }

func (im *Image) defaults() {
	if im.StallTimeout == 0 {
		im.StallTimeout = 30 * time.Second
	}
	if im.OpenDevice == nil {
		im.OpenDevice = func() (Device, error) { return openWorkerDevice(im.Device, im.StallTimeout) }
	}
	if im.Present == nil {
		im.Present = presentDevice
	}
	if im.LookupDrive == nil {
		im.LookupDrive = drives.Lookup
	}
	if im.Now == nil {
		im.Now = time.Now
	}
	if im.SaveEvery == 0 {
		im.SaveEvery = 30 * time.Second
	}
	if im.PollEvery == 0 {
		im.PollEvery = time.Second
	}
	if im.PresenceEvery == 0 {
		im.PresenceEvery = 30 * time.Second
	}
	im.RetryPasses = max(0, min(5, im.RetryPasses))
}

func (im *Image) mapPath() string  { return im.Out + ".map" }
func (im *Image) metaPath() string { return im.Out + ".json" }

// Close releases the reader process.
func (im *Image) Close() error {
	if im.dev != nil {
		err := im.dev.Close()
		im.dev = nil
		return err
	}
	return nil
}

func isRawPath(p string) bool {
	return len(p) > 4 && (p[:4] == `\\.\` || p[:5] == "/dev/")
}

// Plan opens the device and checks the destination.
func (im *Image) Plan(ctx context.Context, env job.Env) (job.Plan, error) {
	im.defaults()
	var p job.Plan
	p.NeedsAdmin = isRawPath(im.Device)
	if im.dev == nil {
		dev, err := im.OpenDevice()
		if errors.Is(err, source.ErrNeedsAdmin) {
			p.Issues = append(p.Issues, job.Issue{Kind: "needs-admin", Severity: job.Block,
				Title: "Imaging a card needs administrator rights", Choices: []job.Choice{job.Elevate, job.Stop}, Default: job.Elevate})
			return p, nil
		}
		if err != nil {
			return p, fmt.Errorf("opening %s: %w", im.Device, err)
		}
		im.dev, im.size = dev, dev.Size()
		im.id = dev.Identity()
		if d, ok := deviceInfo(im.Device); ok {
			im.id.DeviceSerial, im.id.Model, im.id.PhysicalID = d.Serial, d.Model, d.PhysicalID
		}
	}
	dir := filepath.Dir(im.Out)
	dst, _ := im.LookupDrive(dir)
	src := drives.Drive{Root: im.Device, Removable: true, PhysicalID: im.id.PhysicalID}
	if d, ok := deviceInfo(im.Device); ok {
		src.Removable = d.Removable
	}
	in := preflight.Input{Action: "image", Src: src, Dst: dst, DstPath: dir, DeviceSize: im.size,
		Admin: elevate.IsAdmin(), NeedsAdmin: p.NeedsAdmin}
	if free, err := destfs.Free(dir); err == nil && dst.Free == 0 {
		in.Dst.Free = free
	}
	_, mapErr := readOrBak(im.mapPath()) // the map, or its .bak after a power cut mid-save
	_, outErr := os.Stat(im.Out)
	switch {
	case mapErr == nil && outErr == nil:
		if m, err := loadMap(im.mapPath()); err == nil {
			in.Resume = &job.ResumeInfo{Done: int(m.Bytes(Finished) >> 20), Total: int(im.size >> 20)}
			if m.Size() != im.size {
				p.Issues = append(p.Issues, job.Issue{Kind: "image-other-card", Severity: job.Block,
					Title: "The existing image is of a different card (size differs)", Detail: "Choose a new image name.",
					Choices: []job.Choice{job.Stop}, Default: job.Stop})
			}
		}
		in.DeviceSize = 0 // the image file already exists at full size
	case outErr == nil:
		p.Issues = append(p.Issues, job.Issue{Kind: "image-exists", Severity: job.Block,
			Title: "A file with this name already exists", Detail: "Choose a new image name — nothing is overwritten.",
			Choices: []job.Choice{job.Stop}, Default: job.Stop})
	}
	p.Issues = append(p.Issues, preflight.Check(in)...)
	if im.Compress {
		if free := int64(in.Dst.Free); free < im.size+im.size/2+preflight.Reserve {
			p.Issues = append(p.Issues, job.Issue{Kind: "compress-space", Severity: job.Warn,
				Title:   "Compressing needs room for the raw image and the .xz until it is verified",
				Detail:  fmt.Sprintf("%s free; the raw image alone is %s.", preflight.GB(free), preflight.GB(im.size)),
				Choices: []job.Choice{job.Continue, job.Stop}, Default: job.Continue})
		}
	}
	p.Bytes = im.size
	model := im.id.Model
	if model == "" {
		model = "card"
	}
	p.Summary = []string{
		fmt.Sprintf("From: %s — %s, %s; only read, nothing on it is changed", im.Device, model, preflight.GB(im.size)),
		fmt.Sprintf("To:   %s (needs %s; %s free)", im.Out, preflight.GB(im.size), preflight.GB(int64(in.Dst.Free))),
		fmt.Sprintf("How:  good areas first, then the hard ones; %d extra passes over bad sectors", im.RetryPasses),
	}
	p.Result = []string{
		fmt.Sprintf("A sector-by-sector copy of the whole card in %s (unreadable sectors left as zeros)", im.Out),
		fmt.Sprintf("%s.map (opens in ddrescue/ddrescueview) and %s.json (size, checksum, bad bytes)", im.Out, im.Out),
	}
	if im.Compress {
		keep := "the uncompressed image is then removed"
		if im.KeepRaw {
			keep = "the uncompressed image is kept too"
		}
		p.Result = append(p.Result, fmt.Sprintf("Then %s.xz, checked against the image before use; %s", im.Out, keep))
	}
	p.Result = append(p.Result, "Stop any time; running the same command again continues exactly where it stopped",
		"Afterwards you can copy files or recover deleted ones from the image, without the card")
	return p, nil
}

func loadMap(p string) (*Mapfile, error) {
	b, err := readOrBak(p)
	if err != nil {
		return nil, err
	}
	return ParseMapfile(bytes.NewReader(b))
}

// readOrBak reads p, or the p.bak a save leaves for a moment (renamed aside before the new file
// goes in): a power cut right then must not make the image impossible to resume.
func readOrBak(p string) ([]byte, error) {
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		if bb, berr := os.ReadFile(p + ".bak"); berr == nil {
			return bb, nil
		}
	}
	return b, err
}

func (im *Image) save(out destfs.FS, m *Mapfile, meta *Meta) error {
	var b bytes.Buffer
	if err := m.Write(&b); err != nil {
		return err
	}
	if err := destfs.WriteFileAtomic(out, im.mapPath(), b.Bytes()); err != nil {
		return err
	}
	meta.BadBytes = m.Bytes(BadSector) + m.Bytes(NonTrimmed) + m.Bytes(NonScraped)
	j, _ := json.MarshalIndent(meta, "", "  ")
	return destfs.WriteFileAtomic(out, im.metaPath(), j)
}

func isFatal(err error) bool {
	return errors.Is(err, source.ErrTooManyRestarts) || errors.Is(err, context.Canceled) || errors.Is(err, errDifferentCard)
}

// Run images the card (or resumes).
func (im *Image) Run(ctx context.Context, env job.Env, p job.Plan) (job.Result, error) {
	im.defaults()
	for _, is := range p.Issues {
		if is.Severity == job.Block {
			return job.Result{}, fmt.Errorf("can't start: %s", is.Title)
		}
	}
	if im.dev == nil {
		return job.Result{}, errors.New("image: Plan first")
	}
	out := env.Out
	if out == nil {
		// Never write into any mounted volume of the card being imaged (C2).
		roots := []string{im.Device}
		if im.id.PhysicalID != "" {
			if ds, err := drives.Lister(); err == nil {
				for _, d := range ds {
					if d.PhysicalID == im.id.PhysicalID {
						roots = append(roots, d.Root)
					}
				}
			}
		}
		out = destfs.New(filepath.Dir(im.Out), roots...)
	}
	id := im.id
	unlock, err := job.LockSource(id.LockKey()) // the same per-card lock as rescue (R14)
	if err != nil {
		return job.Result{}, err
	}
	defer unlock()
	if release, err := job.Inhibit("imaging a card"); err == nil {
		defer release()
	}
	x := &imgRun{im: im, ctx: ctx, env: env, dev: im.dev, out: out, size: im.size, buf: make([]byte, chunk),
		destSerial: drives.VolumeSerial(filepath.Dir(im.Out)),
		thr:        job.Throttle{Every: 100 * time.Millisecond}}
	resume := false
	if m, err := loadMap(im.mapPath()); err == nil {
		x.m, resume = m, true
		x.meta = &Meta{}
		if b, err := readOrBak(im.metaPath()); err == nil {
			json.Unmarshal(b, x.meta)
		}
	} else {
		x.m = NewMapfile(im.size)
		x.meta = &Meta{Device: im.Device, Model: id.Model, PhysicalID: id.PhysicalID, FSSerial: id.FSSerial, Size: im.size, Created: im.Now()}
	}
	img, err := out.OpenImage(im.Out, im.size, resume)
	if err != nil {
		return job.Result{}, err
	}
	x.img = img
	if resume {
		ok, err := x.fingerprintMatches()
		if err != nil || !ok {
			img.Close()
			return job.Result{}, errDifferentCard
		}
	}
	x.saved = im.Now()
	err = x.passes()
	if serr := img.Sync(); err == nil {
		err = serr
	}
	if serr := im.save(out, x.m, x.meta); err == nil {
		err = serr
	}
	img.Close()
	res := job.Result{Counters: map[string]int{"finished MiB": int(x.m.Bytes(Finished) >> 20), "unreadable KiB": int(x.meta.BadBytes >> 10)},
		Reports: []string{im.mapPath(), im.metaPath()}}
	if err != nil {
		return res, err
	}
	sum, err := archive.HashFile(ctx, im.Out) // Stop works here too: a big image takes minutes
	if err != nil {
		return res, err
	}
	x.meta.SHA256, x.meta.Complete = sum, true
	if im.Compress {
		if _, err := archive.CompressImageSum(ctx, out, im.Out, im.Out+".xz", im.KeepRaw, sum, nil); err != nil {
			im.save(out, x.m, x.meta)
			return res, fmt.Errorf("compressing the image: %w (the raw image is kept)", err)
		}
		x.meta.XZ = im.Out + ".xz"
		res.Reports = append(res.Reports, x.meta.XZ)
	}
	if err := im.save(out, x.m, x.meta); err != nil {
		return res, err
	}
	res.Clean = x.meta.BadBytes == 0
	return res, nil
}

// workerDevice reads the raw card through the worker process (stall/respawn rules apply).
type workerDevice struct {
	c     *worker.Client
	r     io.ReaderAt
	cl    io.Closer
	size  int64
	stall time.Duration
}

func openWorkerDevice(path string, stall time.Duration) (Device, error) {
	c, err := worker.Spawn(worker.Spec{From: path, Raw: true}, stall)
	if err != nil {
		return nil, err
	}
	r, size, cl, err := c.OpenDevice()
	if err != nil {
		c.Close()
		return nil, err
	}
	return &workerDevice{c: c, r: r, cl: cl, size: size, stall: stall}, nil
}

func (d *workerDevice) ReadAt(p []byte, off int64) (int, error) { return d.r.ReadAt(p, off) }
func (d *workerDevice) Size() int64                             { return d.size }
func (d *workerDevice) Identity() drives.Identity {
	id := d.c.Identity()
	return id
}
func (d *workerDevice) Reopen(path string) error {
	if err := d.c.Retarget(worker.Spec{From: path, Raw: true}); err != nil {
		return err
	}
	r, size, cl, err := d.c.OpenDevice()
	if err != nil {
		return err
	}
	if size != d.size {
		cl.Close()
		return fmt.Errorf("the card that came back has a different size")
	}
	d.r, d.cl = r, cl
	return nil
}
func (d *workerDevice) Close() error { d.cl.Close(); return d.c.Close() }

func deviceInfo(path string) (drives.Device, bool) {
	devs, err := drives.DeviceLister()
	if err != nil {
		return drives.Device{}, false
	}
	for _, d := range devs {
		if d.Path == path {
			return d, true
		}
	}
	return drives.Device{}, false
}

// presentDevice finds the card again (drives.FindCard): never a same-size stranger.
var presentDevice = drives.FindCard
