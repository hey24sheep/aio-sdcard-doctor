//go:build !darwin

package drives

// mountOf: elsewhere the longest mount-point prefix is the right volume.
func mountOf(string) string { return "" }
