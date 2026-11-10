//go:build !windows

package cli

// isDoubleClicked: only Windows starts a console program from a double-click.
func isDoubleClicked() bool { return false }
