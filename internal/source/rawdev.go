package source

import (
	"errors"
	"io"
	"io/fs"
	"os"
)

// openImageFile opens a card image (card.img) read-only.
func openImageFile(p string) (io.ReaderAt, int64, io.Closer, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, 0, nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, nil, err
	}
	return f, st.Size(), f, nil
}

func needsAdmin(err error) error {
	if errors.Is(err, fs.ErrPermission) {
		return errors.Join(ErrNeedsAdmin, err)
	}
	return err
}
