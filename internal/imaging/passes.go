package imaging

import (
	"context"
	"crypto/sha256"
	"fmt"
	"time"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/destfs"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/drives"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/job"
)

const (
	chunk      = 1 << 20
	sector     = 4 << 10
	minSkip    = 64 << 10
	maxSkipCap = 64 << 20
	fpEvery    = 64 << 20 // a fingerprint sample every 64 MiB (first 1 MiB of each)
	maxFP      = 8
)

// imgRun is one Run's state.
type imgRun struct {
	im        *Image
	ctx       context.Context
	env       job.Env
	dev       Device
	img       destfs.ImageFile
	out       destfs.FS
	m         *Mapfile
	meta      *Meta
	size      int64
	buf       []byte
	saved     time.Time
	lastCheck time.Time
	thr       job.Throttle
}

func (x *imgRun) ask(is job.Issue) job.Choice {
	if x.env.Prompt == nil {
		return job.AutoPrompter{}.Ask(x.ctx, is)
	}
	return x.env.Prompt.Ask(x.ctx, is)
}

// read reads [pos, pos+n) from the device. If the card was pulled, it asks the user to put it
// back, waits for the same card and reopens it, then reads again.
func (x *imgRun) read(pos int64, n int) (int, error) {
	b := x.buf[:n]
	got, err := x.dev.ReadAt(b, pos)
	if err == nil || (got == n) {
		return got, nil
	}
	// Checking presence lists every disk, so it is throttled: a run of bad sectors on a card
	// that is still there is just bad sectors (I12).
	now := x.im.Now()
	if !x.lastCheck.IsZero() && now.Sub(x.lastCheck) < x.im.PresenceEvery {
		return got, err
	}
	x.lastCheck = now
	path, ok := x.im.Present(x.im.id, x.im.Device)
	if ok && path == x.im.Device {
		return got, err // a bad area on a present card
	}
	if !ok {
		c := x.ask(job.Issue{Kind: "card-removed", Severity: job.Warn, Title: "The card was removed — reinsert it",
			Detail: "Put the same card back in; imaging continues where it stopped.", Choices: []job.Choice{job.Wait, job.Stop}, Default: job.Wait})
		if c != job.Wait {
			return 0, fmt.Errorf("stopped: the card was removed")
		}
		if werr := waitUntil(x.ctx, x.im.PollEvery, x.im.WaitTimeout, func() bool {
			path, ok = x.im.Present(x.im.id, x.im.Device)
			return ok
		}); werr != nil {
			return 0, fmt.Errorf("the card did not come back: %w", werr)
		}
	}
	if err := x.dev.Reopen(path); err != nil {
		return 0, err
	}
	// The device that came back must be the same card before one more sector is written (C1).
	if ok, _ := x.fingerprintMatches(); !ok {
		return 0, errDifferentCard
	}
	x.im.Device = path
	return x.dev.ReadAt(x.buf[:n], pos)
}

// write stores good data, asking about a full destination (retry / stop).
func (x *imgRun) write(pos int64, b []byte) error {
	for i := 0; ; i++ {
		_, err := x.img.WriteAt(b, pos)
		if err == nil {
			return nil
		}
		c := x.ask(job.Issue{Kind: "disk-full", Severity: job.Warn, Title: "Can't write to the image: " + err.Error(),
			Detail: "Free some space, then retry. Imaging resumes where it stopped.", Choices: []job.Choice{job.Retry, job.Stop}, Default: job.Stop})
		if c != job.Retry || i > 100 {
			return err
		}
	}
}

// good marks [pos, pos+n) finished after writing it, and records fingerprint samples.
func (x *imgRun) good(pos int64, n int) error {
	if err := x.write(pos, x.buf[:n]); err != nil {
		return err
	}
	x.m.Set(pos, int64(n), Finished)
	if pos%fpEvery == 0 && n == min(chunk, int(x.size-pos)) && len(x.meta.Fingerprint) < maxFP {
		have := false
		where := fmt.Sprintf("off:%d", pos)
		for _, f := range x.meta.Fingerprint {
			have = have || f.Where == where
		}
		if !have {
			x.meta.Fingerprint = append(x.meta.Fingerprint, drives.FPBlock{Where: where, SHA256: fmt.Sprintf("%X", sha256.Sum256(x.buf[:n]))})
		}
	}
	return nil
}

// checkpoint saves the map (and metadata) every SaveEvery.
func (x *imgRun) checkpoint(force bool) error {
	now := x.im.Now()
	if !force && now.Sub(x.saved) < x.im.SaveEvery {
		x.progress()
		return nil
	}
	x.saved = now
	x.progress()
	if err := x.img.Sync(); err != nil { // the map may only claim data that is on disk (I10)
		return err
	}
	return x.im.save(x.out, x.m, x.meta)
}

func (x *imgRun) progress() {
	if x.env.Progress != nil && x.thr.Ok() {
		x.env.Progress(job.Progress{Phase: fmt.Sprintf("Imaging, pass %d", x.m.Pass), Done: x.m.Bytes(Finished), Total: x.size,
			Note: fmt.Sprintf("%d bytes unreadable so far", x.m.Bytes(BadSector)+x.m.Bytes(NonTrimmed)+x.m.Bytes(NonScraped))})
	}
}

// passes runs (or resumes) the ddrescue-style passes. Each is bounded by the device size.
func (x *imgRun) passes() error {
	steps := []struct {
		pass   int
		status byte
		fn     func() error
	}{
		{1, NonTried, func() error { return x.copyPass(true) }},
		{2, NonTried, func() error { return x.copyPass(false) }},
		{3, NonTrimmed, x.trimPass},
		{4, NonScraped, x.scrapePass},
		{5, BadSector, x.retryPass},
	}
	for _, s := range steps {
		if s.pass < x.m.Pass {
			continue
		}
		if s.pass > x.m.Pass {
			x.m.Pass, x.m.CurPos = s.pass, 0
		}
		x.m.CurStatus = s.status
		if err := s.fn(); err != nil {
			return err
		}
		if err := x.checkpoint(true); err != nil {
			return err
		}
	}
	x.m.Pass, x.m.CurStatus, x.m.CurPos = 5, Finished, x.size
	return x.checkpoint(true)
}

// copyPass reads untried areas in 1 MiB pieces. With skip, an error skips ahead (64 KiB,
// doubling per consecutive error, capped) so the readable part of a card is saved first.
func (x *imgRun) copyPass(skip bool) error {
	pos := int64(0)
	if skip {
		pos = x.m.CurPos
	}
	cap := max(int64(minSkip), min(int64(maxSkipCap), x.size/1000))
	var skipBy int64
	for {
		if err := x.ctx.Err(); err != nil {
			return err
		}
		b, ok := x.m.next(NonTried, pos)
		if !ok {
			return nil
		}
		n := int(min(int64(chunk), b.Size))
		got, err := x.read(b.Pos, n)
		if x.ctx.Err() != nil {
			return x.ctx.Err()
		}
		switch {
		case err == nil && got == n:
			if werr := x.good(b.Pos, n); werr != nil {
				return werr
			}
			skipBy = 0
			pos = b.Pos + int64(n)
		case isFatal(err):
			return err
		default:
			x.m.Set(b.Pos, int64(n), NonTrimmed)
			pos = b.Pos + int64(n)
			if skip {
				skipBy = min(cap, max(int64(minSkip), skipBy*2))
				pos += skipBy
			}
		}
		x.m.CurPos = pos
		if err := x.checkpoint(false); err != nil {
			return err
		}
	}
}

func (x *imgRun) sectorAt(pos int64) (bool, error) {
	n := int(min(int64(sector), x.size-pos))
	got, err := x.read(pos, n)
	if x.ctx.Err() != nil {
		return false, x.ctx.Err()
	}
	if isFatal(err) {
		return false, err
	}
	if err == nil && got == n {
		return true, x.good(pos, n)
	}
	x.m.Set(pos, int64(n), BadSector)
	return false, nil
}

// trimPass reads each non-trimmed area sector by sector from both edges inward until the first
// error on each side; what is left in the middle is marked non-scraped.
func (x *imgRun) trimPass() error {
	for {
		if err := x.ctx.Err(); err != nil {
			return err
		}
		b, ok := x.m.next(NonTrimmed, 0)
		if !ok {
			return nil
		}
		lo, hi := b.Pos, b.Pos+b.Size
		for lo < hi {
			ok, err := x.sectorAt(lo)
			if err != nil {
				return err
			}
			lo += min(int64(sector), hi-lo)
			if !ok {
				break
			}
		}
		for hi > lo {
			start := max(lo, hi-sector)
			ok, err := x.sectorAt(start)
			if err != nil {
				return err
			}
			hi = start
			if !ok {
				break
			}
		}
		if hi > lo {
			x.m.Set(lo, hi-lo, NonScraped)
		}
		x.m.CurPos = b.Pos
		if err := x.checkpoint(false); err != nil {
			return err
		}
	}
}

// scrapePass reads every non-scraped sector once.
func (x *imgRun) scrapePass() error {
	for {
		if err := x.ctx.Err(); err != nil {
			return err
		}
		b, ok := x.m.next(NonScraped, 0)
		if !ok {
			return nil
		}
		if _, err := x.sectorAt(b.Pos); err != nil {
			return err
		}
		x.m.CurPos = b.Pos
		if err := x.checkpoint(false); err != nil {
			return err
		}
	}
}

// retryPass re-reads bad sectors RetryPasses times (0–5).
func (x *imgRun) retryPass() error {
	for r := 0; r < x.im.RetryPasses; r++ {
		var bad []Block
		for _, b := range x.m.Blocks {
			if b.Status == BadSector {
				bad = append(bad, b)
			}
		}
		for _, b := range bad {
			for pos := b.Pos; pos < b.Pos+b.Size; pos += sector {
				if err := x.ctx.Err(); err != nil {
					return err
				}
				if _, err := x.sectorAt(pos); err != nil {
					return err
				}
			}
			if err := x.checkpoint(false); err != nil {
				return err
			}
		}
	}
	return nil
}

// fingerprintMatches re-reads the recorded samples from the device (R2). Unreadable samples are
// skipped; at least 3 (or all, if fewer) must match and none may differ.
func (x *imgRun) fingerprintMatches() (bool, error) {
	var now []drives.FPBlock
	for _, f := range x.meta.Fingerprint {
		var off int64
		fmt.Sscanf(f.Where, "off:%d", &off)
		n := int(min(int64(chunk), x.size-off))
		got, err := x.dev.ReadAt(x.buf[:n], off)
		h := ""
		if err == nil && got == n {
			h = fmt.Sprintf("%X", sha256.Sum256(x.buf[:n]))
		}
		now = append(now, drives.FPBlock{Where: f.Where, SHA256: h})
	}
	ok, _, compared := drives.Identity{Fingerprint: x.meta.Fingerprint}.Matches(drives.Identity{Fingerprint: now})
	if ok && compared == 0 && x.m.Bytes(Finished) > 0 {
		// Nothing could be compared: only the file-system serial can still vouch for the card.
		now := drives.NormalizeSerial(x.dev.Identity().FSSerial) // the device as it is now, not as first seen
		ok = now != "" && drives.NormalizeSerial(x.meta.FSSerial) == now
	}
	return ok, nil
}
