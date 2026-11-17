//go:build !windows

package cli

// enableVT: terminals elsewhere understand ANSI sequences.
func enableVT() bool { return true }
