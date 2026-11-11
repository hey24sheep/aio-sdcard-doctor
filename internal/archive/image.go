package archive

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"

	"sync"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/destfs"
)

// segment is how much of the image one worker compresses into independent xz streams; segments
// are compressed in parallel and concatenated (xz, 7-Zip and libarchive read concatenated
// streams). Each worker holds one segment's output, so this bounds memory. A var for tests.
var segment int64 = 16 << 20

const (
	// dictCap bounds each encoder's memory (several run at once); a segment is no bigger.
	dictCap    = 8 << 20
	maxWorkers = 4
)

// HashFile is a file's SHA-256 (uppercase hex); Stop ends it between reads (a 256 GB image takes
// minutes to read).
func HashFile(ctx context.Context, p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	return hashReader(ctx, f)
}

func hashReader(ctx context.Context, r io.Reader) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, ctxReader{ctx, r}); err != nil {
		return "", err
	}
	return fmt.Sprintf("%X", h.Sum(nil)), nil
}

// ctxReader ends a long read when ctx is done.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// CompressImage turns a finished card image into an .xz file: compress (parallel streams),
// fsync, decompress and compare the SHA-256 with the image's, rename into place, and remove the
// raw image unless keep. It reads only the local image, never the card. Returns the image's
// SHA-256 (uppercase hex).
func CompressImage(ctx context.Context, out destfs.FS, img, dst string, keep bool, progress func(done, total int64)) (string, error) {
	return CompressImageSum(ctx, out, img, dst, keep, "", progress)
}

// CompressImageSum is CompressImage for an image whose SHA-256 is already known (imaging just
// computed it): the image isn't read a second time to hash it.
func CompressImageSum(ctx context.Context, out destfs.FS, img, dst string, keep bool, want string, progress func(done, total int64)) (string, error) {
	st, err := os.Stat(img)
	if err != nil {
		return "", err
	}
	size := st.Size()
	if want == "" {
		if want, err = HashFile(ctx, img); err != nil {
			return "", err
		}
	}
	tmp := dst + ".tmp"
	removeIfExists(out, tmp)
	w, err := out.Create(tmp)
	if err != nil {
		return "", err
	}
	fail := func(err error) (string, error) {
		out.Remove(tmp)
		return "", err
	}
	if err := compressParallel(ctx, img, size, w, progress); err != nil {
		w.Close()
		return fail(err)
	}
	if err := w.Sync(); err != nil {
		w.Close()
		return fail(err)
	}
	if err := w.Close(); err != nil {
		return fail(err)
	}
	got, err := hashXZ(ctx, tmp)
	if err != nil {
		return fail(err)
	}
	if got != want {
		return fail(fmt.Errorf("compressed image doesn't decompress to the original: %w", ErrVerify))
	}
	if err := destfs.RenameRetry(ctx, out, tmp, dst); err != nil {
		return fail(err)
	}
	if !keep {
		if err := out.Remove(img); err != nil {
			return want, err
		}
	}
	return want, nil
}

func hashXZ(ctx context.Context, p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	r, err := xz.NewReader(f)
	if err != nil {
		return "", err
	}
	return hashReader(ctx, r)
}

// compressParallel compresses [0,size) of img in segments with up to maxWorkers encoders and
// writes the streams to w in order. Exactly workers+1 segment kits exist (each worker's, plus the
// one being written), so memory doesn't grow with the image.
func compressParallel(ctx context.Context, img string, size int64, w io.Writer, progress func(done, total int64)) error {
	n := int((size + segment - 1) / segment)
	if n == 0 {
		n = 1
	}
	workers := min(maxWorkers, max(1, runtime.GOMAXPROCS(0)-1), n)
	type result struct {
		k   *segKit
		err error
	}
	kits := make(chan *segKit, workers+1)
	all := make([]*segKit, workers+1)
	for i := range all {
		all[i] = &segKit{}
		kits <- all[i]
	}
	defer func() { // after the workers are done (deferred below, so it runs first)
		for _, k := range all {
			if k.probe != nil {
				k.probe.Close()
			}
		}
	}()
	slots := make([]chan result, n)
	for i := range slots {
		slots[i] = make(chan result, 1)
	}
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	cctx, cancel := context.WithCancel(ctx)
	defer func() { cancel(); wg.Wait() }()
	wg.Add(1) // the scheduler itself, so Wait never runs while it may still Add a worker
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			select {
			case sem <- struct{}{}:
			case <-cctx.Done():
				for j := i; j < n; j++ {
					slots[j] <- result{err: cctx.Err()}
				}
				return
			}
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				var k *segKit
				select { // one is always free: kits held = workers holding sem + the writer's one
				case k = <-kits:
				case <-cctx.Done():
					slots[i] <- result{err: cctx.Err()}
					return
				}
				err := compressSegment(cctx, k, img, int64(i)*segment, min(segment, size-int64(i)*segment))
				slots[i] <- result{k, err}
			}(i)
		}
	}()
	var done int64
	for i := 0; i < n; i++ {
		r := <-slots[i]
		if r.err != nil { // before the token: a slot the scheduler filled on cancel holds none
			return r.err
		}
		<-sem
		if _, err := w.Write(r.k.buf.Bytes()); err != nil {
			return err
		}
		kits <- r.k
		done = min(size, done+segment)
		if progress != nil {
			progress(done, size)
		}
	}
	return nil
}

// compressSegment turns [off, off+n) of img into concatenated .xz streams: runs of 1 MiB pieces
// that compress go through LZMA, runs that don't (photos, videos, encrypted data) are stored.
// A quick zstd pass decides. The output goes to k.buf, reused from segment to segment.
func compressSegment(ctx context.Context, k *segKit, img string, off, n int64) error {
	f, err := os.Open(img)
	if err != nil {
		return err
	}
	defer f.Close()
	if k.probe == nil {
		if k.probe, err = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedFastest), zstd.WithEncoderConcurrency(1),
			zstd.WithLowerEncoderMem(true)); err != nil {
			return err
		}
	}
	buf := &k.buf
	buf.Reset()
	buf.Grow(int(n + n/storedChunk*3 + 256)) // stored output never grows past this
	if k.piece == nil {
		k.piece = make([]byte, 1<<20)
	}
	piece := k.piece
	var zw *xz.Writer
	var st *storedXZ
	closeXZ := func() error {
		if zw == nil {
			return nil
		}
		err := zw.Close()
		zw = nil
		return err
	}
	r := io.NewSectionReader(f, off, n)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, rerr := io.ReadFull(r, piece)
		if n > 0 {
			p := piece[:n]
			if k.trial = k.probe.EncodeAll(p, k.trial[:0]); len(k.trial) < n*9/10 { // worth LZMA
				if st != nil {
					st.close()
					st = nil
				}
				if zw == nil {
					if zw, err = (xz.WriterConfig{DictCap: dictCap, CheckSum: xz.CRC64}).NewWriter(buf); err != nil {
						return err
					}
				}
				if _, err := zw.Write(p); err != nil {
					return err
				}
			} else {
				if err := closeXZ(); err != nil {
					return err
				}
				if st == nil {
					st = newStoredXZ(buf)
				}
				st.write(p)
			}
		}
		if errors.Is(rerr, io.EOF) || errors.Is(rerr, io.ErrUnexpectedEOF) {
			break
		}
		if rerr != nil {
			return rerr
		}
	}
	if st != nil {
		st.close()
	}
	if buf.Len() == 0 { // an empty image is still one (empty) stream
		if zw, err = (xz.WriterConfig{DictCap: dictCap, CheckSum: xz.CRC64}).NewWriter(buf); err != nil {
			return err
		}
	}
	if err := closeXZ(); err != nil {
		return err
	}
	return nil
}

// segKit is one worker's reusable memory: its segment's output, read buffers and zstd probe.
type segKit struct {
	buf          bytes.Buffer
	piece, trial []byte
	probe        *zstd.Encoder
}
