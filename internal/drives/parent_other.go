//go:build !linux

package drives

func partitionParent(string) string { return "" }
