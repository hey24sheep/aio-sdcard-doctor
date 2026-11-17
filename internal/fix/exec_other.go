//go:build !windows

package fix

import "syscall"

// toolAttr: nothing to add outside Windows (Stop sends the tool an interrupt).
func toolAttr() *syscall.SysProcAttr { return nil }
