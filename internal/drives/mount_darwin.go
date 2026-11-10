package drives

import (
	"path/filepath"

	"golang.org/x/sys/unix"
)

// mountOf asks the kernel which mount holds path (or its nearest existing parent). On macOS the
// path prefix lies: firmlinks put /Users, /private/var and friends on /System/Volumes/Data,
// while the longest mount-point prefix is the read-only system volume at /.
func mountOf(path string) string {
	for p := path; ; p = filepath.Dir(p) {
		var st unix.Statfs_t
		if unix.Statfs(p, &st) == nil {
			return cstring(st.Mntonname[:])
		}
		if p == filepath.Dir(p) {
			return ""
		}
	}
}
