package fix

import (
	"strconv"
	"strings"
)

// volumesOnDisk picks, from every volume the OS knows (enumerate: \\?\Volume{GUID}\ names), those
// with an extent on physical disk number disk, lettered or not. A volume mounted without a letter
// (in a folder, or by a program) still holds the disk: the health test's writes under it fail and
// would be reported as bad areas (OA9). A volume whose extents can't be read is skipped: it may be
// on any disk, and an empty card-reader slot answers that way.
func volumesOnDisk(enumerate func() ([]string, error), extents func(vol string) ([]uint32, error), disk uint32) ([]string, error) {
	all, err := enumerate()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, v := range all {
		ds, err := extents(v)
		if err != nil {
			continue
		}
		for _, d := range ds {
			if d == disk {
				out = append(out, v)
				break
			}
		}
	}
	return out, nil
}

// diskNumber reads N from \\.\PhysicalDriveN.
func diskNumber(path string) (uint32, bool) {
	const prefix = `\\.\PhysicalDrive`
	n, err := strconv.ParseUint(strings.TrimPrefix(path, prefix), 10, 32)
	return uint32(n), err == nil && strings.HasPrefix(path, prefix)
}
