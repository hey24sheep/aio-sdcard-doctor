package drives

import "path/filepath"

// RealPath is p with its deepest existing ancestor resolved to where it really is: through
// symbolic links, and on Windows also junctions, folder mount points and subst letters (a
// destination reached that way can be on the card). The part that doesn't exist yet is joined
// back. p, made absolute, when nothing can be resolved.
func RealPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = filepath.Clean(p)
	}
	existing, rest := abs, ""
	for i := 0; i < 4096; i++ { // bounded walk up to the deepest existing ancestor
		if r, ok := finalPath(existing); ok {
			return filepath.Join(r, rest)
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			break
		}
		rest = filepath.Join(filepath.Base(existing), rest)
		existing = parent
	}
	return abs
}

// VolumeSerial is the file-system serial of the volume that really holds p (its deepest
// existing ancestor), normalized like NormalizeSerial; "" where it can't be read this way.
func VolumeSerial(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return ""
	}
	for i := 0; i < 4096; i++ {
		if s, ok := volumeSerial(abs); ok {
			return NormalizeSerial(s)
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			break
		}
		abs = parent
	}
	return ""
}
