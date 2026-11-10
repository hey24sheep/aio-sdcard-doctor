package source

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/drives"
)

// folder is a card mounted by the OS, read through os.DirFS (which refuses paths leaving the root).
type folder struct {
	fs.FS
	root string
	id   drives.Identity
}

// OpenFolder opens an OS-mounted card (or any folder) read-only.
func OpenFolder(root string) (Source, error) {
	st, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		return nil, &fs.PathError{Op: "open", Path: root, Err: fs.ErrInvalid}
	}
	f := &folder{FS: os.DirFS(root), root: root}
	if d, err := drives.Lookup(root); err == nil {
		f.id = d.ID
	}
	return f, nil
}

func (f *folder) ReadDir(name string) ([]fs.DirEntry, error) { return fs.ReadDir(f.FS, name) }
func (f *folder) Stat(name string) (fs.FileInfo, error)      { return fs.Stat(f.FS, name) }
func (f *folder) Kind() string                               { return "folder" }
func (f *folder) Identity() drives.Identity                  { return f.id }

// Root is the folder on disk (used to re-open the card at a new mount point after a re-plug).
func (f *folder) Root() string { return f.root }

// OpenAt opens name with os.Open (read-only) after the same path checks os.DirFS applies.
func (f *folder) OpenAt(name string) (io.ReaderAt, int64, io.Closer, error) {
	file, err := f.FS.Open(name) // validates name; an *os.File opened O_RDONLY
	if err != nil {
		return nil, 0, nil, err
	}
	of, ok := file.(*os.File)
	if !ok {
		file.Close()
		return nil, 0, nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	st, err := of.Stat()
	if err != nil {
		of.Close()
		return nil, 0, nil, err
	}
	return of, st.Size(), of, nil
}

// FullPath maps a source name to the OS path (for messages and the lock-file checks).
func (f *folder) FullPath(name string) string { return filepath.Join(f.root, filepath.FromSlash(name)) }
