package fsread

import (
	"io"

	"github.com/diskfs/go-diskfs/filesystem/iso9660"
)

// openISO reads ISO9660 (+ Rock Ridge/Joliet) with diskfs/go-diskfs (MIT) through our
// read-only storage.
func openISO(r io.ReaderAt, size int64) (driver, error) {
	f, err := iso9660.Read(newStorage(r, size), size, 0, 2048)
	if err != nil {
		return nil, err
	}
	return &diskfsDriver{fsys: f}, nil
}
