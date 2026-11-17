package gui

import (
	"encoding/json"
	"fmt"
	"html"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The only files the GUI writes: gui.lock in the user's own config folder (never on a card or a
// destination), so this user's next `gui` can reopen the running window; and on Linux and macOS
// the short-lived launch page in the user's own cache folder. Allow-listed in
// internal/safety_test.go.

type lockFile struct {
	Port  int
	Mint  string
	Admin bool // the server runs as administrator/root
}

// writeLock records how this user's next `gui` can reach this server (owner-only file). A lock
// of another server that is still running is kept (`gui --port N` while one runs), so `gui` still
// reopens the window holding its job — unless this one is elevated and that one isn't.
func (sv *Server) writeLock(dir string, admin bool) {
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}
	p := filepath.Join(dir, "gui.lock")
	port := sv.ln.Addr().(*net.TCPAddr).Port
	var old lockFile
	if b, err := os.ReadFile(p); err == nil && json.Unmarshal(b, &old) == nil && old.Port != 0 && old.Port != port && !(admin && !old.Admin) {
		if _, live := mint(old); live { // that GUI still answers with its secret (not just anything on the port)
			return
		}
	}
	sv.mint = secret()
	b, _ := json.Marshal(lockFile{Port: port, Mint: sv.mint, Admin: admin})
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

// launchTarget is what the browser is started with. On Linux and macOS any local user can read a
// program's command line (ps): there it is an owner-only page in dir (this user's private cache
// folder) that sends the browser on to link, so the single-use token is never on that command
// line. Windows shows a process's command line only to its own user and administrators.
func launchTarget(goos, link, dir string) string {
	if goos == "windows" {
		return link
	}
	p, err := launchPage(dir, link)
	if err != nil {
		return link // can't write the page: the link itself still works
	}
	p = filepath.ToSlash(p)
	if !strings.HasPrefix(p, "/") { // C:/… (a test on Windows)
		p = "/" + p
	}
	return (&url.URL{Scheme: "file", Path: p}).String()
}

// launchPage writes the page. It is removed a minute later — spent by then, or unused and no
// longer wanted; pages left by a `gui` that exited straight away (it only reopened a window) are
// removed by the next one.
func launchPage(dir, link string) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	old, _ := filepath.Glob(filepath.Join(dir, "open-*.html"))
	for _, o := range old {
		if st, err := os.Stat(o); err == nil && time.Since(st.ModTime()) > time.Minute {
			os.Remove(o)
		}
	}
	f, err := os.CreateTemp(dir, "open-*.html") // owner-only (0600)
	if err != nil {
		return "", err
	}
	l := html.EscapeString(link)
	_, err = fmt.Fprintf(f, "<!doctype html><meta charset=\"utf-8\"><meta http-equiv=\"refresh\" content=\"0;url=%s\">"+
		"<title>SD card doctor</title><p><a href=\"%s\">Open SD card doctor</a></p>\n", l, l)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(f.Name())
		return "", err
	}
	time.AfterFunc(time.Minute, func() { os.Remove(f.Name()) })
	return f.Name(), nil
}

// cacheBase is this user's cache folder, always an absolute path: with none (no home folder) a
// new private folder in the temp folder — never a path relative to wherever the program runs
// (a card, even).
func cacheBase(dir string, err error) string {
	if err == nil && filepath.IsAbs(dir) {
		return dir
	}
	if d, err := os.MkdirTemp("", "aio-sdcard-doctor-"); err == nil { // 0700, ours alone
		return d
	}
	return filepath.Join(os.TempDir(), "aio-sdcard-doctor")
}
