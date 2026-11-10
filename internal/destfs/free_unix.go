//go:build !windows

package destfs

import "golang.org/x/sys/unix"

// Free is the space available to us on the volume holding path.
func Free(path string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(st.Bsize), nil
}
