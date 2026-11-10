package source

import (
	"errors"
	"io"
)

// ExtentReader reads a file made of byte ranges [offset, length] of a volume that starts at Off
// on Dev (a deleted file's clusters, a deep-scan find).
type ExtentReader struct {
	Dev     io.ReaderAt
	Off     int64
	Extents [][2]int64
}

// ReadAt implements io.ReaderAt over the concatenated extents.
func (e ExtentReader) ReadAt(p []byte, off int64) (int, error) {
	n := 0
	for _, x := range e.Extents {
		if len(p) == 0 {
			break
		}
		if off >= x[1] {
			off -= x[1]
			continue
		}
		k := min(int64(len(p)), x[1]-off)
		m, err := e.Dev.ReadAt(p[:k], e.Off+x[0]+off)
		n += m
		if err != nil && !(errors.Is(err, io.EOF) && int64(m) == k) {
			return n, err
		}
		p, off = p[k:], 0
	}
	if len(p) > 0 {
		return n, io.EOF
	}
	return n, nil
}
