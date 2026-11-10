package job

import (
	"os"

	"github.com/godbus/dbus/v5"
)

// inhibitOS takes a systemd-logind sleep/idle inhibitor lock over D-Bus; closing the returned
// file descriptor releases it (and the kernel closes it if we crash).
func inhibitOS(reason string) (func(), error) {
	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		return nil, err
	}
	var fd dbus.UnixFD
	obj := conn.Object("org.freedesktop.login1", "/org/freedesktop/login1")
	if err := obj.Call("org.freedesktop.login1.Manager.Inhibit", 0,
		"sleep:idle", "aio-sdcard-doctor", reason, "block").Store(&fd); err != nil {
		conn.Close()
		return nil, err
	}
	f := os.NewFile(uintptr(fd), "logind-inhibit")
	return func() { f.Close(); conn.Close() }, nil
}
