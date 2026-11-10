package ioerr

func codeTable() map[string][]uintptr {
	return map[string][]uintptr{
		"DiskFull": {112, 39}, "TooLarge": {223}, "Locked": {32, 33}, "WriteProtected": {19},
		"Permission": {5}, "Gone": {21, 1167, 55}, "UnrecognizedFS": {1005},
	}
}

func rootMissingCode() uintptr { return 3 }
