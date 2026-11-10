package drives

import "path/filepath"

// partitionParent maps a partition name (sdb1, mmcblk0p1) to its whole disk via sysfs.
func partitionParent(name string) string { return filepath.Base(parentDisk("/dev/" + name)) }
