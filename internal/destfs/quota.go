package destfs

import "sync/atomic"

// NewQuota wraps inner so that, across all files it creates, at most limit bytes are written;
// writes past it fail with err (used by tests to simulate a full disk).
func NewQuota(inner FS, limit int64, err error) FS {
	return &quota{FS: inner, limit: limit, err: err, used: new(atomic.Int64)}
}

type quota struct {
	FS
	limit int64
	err   error
	used  *atomic.Int64
}

// take reserves up to n bytes and returns how many may be written.
func (q *quota) take(n int) int {
	for {
		u := q.used.Load()
		room := q.limit - u
		if room <= 0 {
			return 0
		}
		k := int64(n)
		if k > room {
			k = room
		}
		if q.used.CompareAndSwap(u, u+k) {
			return int(k)
		}
	}
}

func (q *quota) Create(p string) (File, error) {
	f, err := q.FS.Create(p)
	if err != nil {
		return nil, err
	}
	return &quotaFile{File: f, q: q}, nil
}

func (q *quota) OpenImage(p string, size int64, resume bool) (ImageFile, error) {
	f, err := q.FS.OpenImage(p, size, resume)
	if err != nil {
		return nil, err
	}
	return &quotaImage{ImageFile: f, q: q}, nil
}

type quotaFile struct {
	File
	q *quota
}

func (f *quotaFile) Write(p []byte) (int, error) {
	k := f.q.take(len(p))
	n, err := f.File.Write(p[:k])
	if err == nil && k < len(p) {
		err = f.q.err
	}
	return n, err
}

type quotaImage struct {
	ImageFile
	q *quota
}

func (f *quotaImage) WriteAt(p []byte, off int64) (int, error) {
	k := f.q.take(len(p))
	n, err := f.ImageFile.WriteAt(p[:k], off)
	if err == nil && k < len(p) {
		err = f.q.err
	}
	return n, err
}
