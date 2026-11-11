package gui

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
)

// The only file the GUI writes: gui.lock in the user's own config folder (never on a card or a
// destination), so this user's next `gui` can reopen the running window. Allow-listed in
// internal/safety_test.go.

type lockFile struct {
	Port  int
	Mint  string
	Admin bool // the server runs as administrator/root
}

// writeLock records how this user's next `gui` can reach this server (owner-only file).
func (sv *Server) writeLock(dir string, admin bool) {
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}
	sv.mint = secret()
	b, _ := json.Marshal(lockFile{Port: sv.ln.Addr().(*net.TCPAddr).Port, Mint: sv.mint, Admin: admin})
	p := filepath.Join(dir, "gui.lock")
	if os.WriteFile(p, b, 0o600) == nil {
		sv.lockPath = p
	}
}

// removeLock deletes gui.lock when the server stops, unless another server (an elevated one
// started meanwhile) has written its own.
func (sv *Server) removeLock() {
	if sv.lockPath == "" {
		return
	}
	var l lockFile
	if b, err := os.ReadFile(sv.lockPath); err == nil && json.Unmarshal(b, &l) == nil && l.Mint == sv.mint {
		os.Remove(sv.lockPath)
	}
}
