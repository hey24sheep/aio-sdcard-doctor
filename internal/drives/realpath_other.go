//go:build !windows

package drives

import "path/filepath"

func finalPath(p string) (string, bool) {
	r, err := filepath.EvalSymlinks(p)
	return r, err == nil
}

// volumeSerial: elsewhere a path's volume comes from its mount (Lookup); no serial by handle.
func volumeSerial(string) (string, bool) { return "", false }
