// Package gui is the local web GUI: a net/http server on 127.0.0.1 serving embedded HTML/CSS/JS
// that renders an app.Session. Local-only and offline: a single-use launch token becomes an
// HttpOnly SameSite=Strict session cookie; every request checks Host (DNS rebinding), every
// change also Origin + X-CSRF; no CORS; strict CSP; every asset embedded.
package gui

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/app"
)

//go:embed web
var webFS embed.FS

// Options for Start.
type Options struct {
	Port      int           // 0 = any free port
	IdleAfter time.Duration // stop when no window and no job for this long (default 60 s; < 0 = never)
	LockDir   string        // per-user folder for gui.lock, so `gui` can reopen a running window ("" = none)
}

// Server is a running GUI.
type Server struct {
	sess   *app.Session
	ln     net.Listener
	host   string // 127.0.0.1:port
	http   *http.Server
	cancel context.CancelFunc
	done   chan struct{}

	mu       sync.Mutex
	everSeen bool            // a window has connected at least once
	lockPath string          // gui.lock (removed when the server stops)
	mint     string          // secret in gui.lock: lets this user's `gui` ask for a fresh link
	tokens   map[string]bool // unused launch tokens
	sessions map[string]bool // session cookie values
	csrf     string
	clients  int
	lastSeen time.Time
	subs     map[chan app.Event]bool
}

func secret() string {
	b := make([]byte, 32) // 256 bits
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// Start listens on 127.0.0.1 and serves the session until ctx ends, Close is called, or the GUI
// has been idle (no window, no job) for Options.IdleAfter.
func Start(ctx context.Context, s *app.Session, opt Options) (*Server, error) {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", opt.Port))
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	sv := &Server{sess: s, ln: ln, host: ln.Addr().String(), cancel: cancel, done: make(chan struct{}),
		tokens: map[string]bool{}, sessions: map[string]bool{}, csrf: secret(), lastSeen: time.Now(), subs: map[chan app.Event]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", sv.index)
	mux.HandleFunc("GET /assets/{file}", sv.asset)
	mux.HandleFunc("GET /api/csrf", sv.apiCSRF)
	mux.HandleFunc("GET /api/view", sv.apiView)
	mux.HandleFunc("POST /api/do", sv.apiDo)
	mux.HandleFunc("GET /api/events", sv.apiEvents)
	mux.HandleFunc("GET /api/dirs", sv.apiDirs)
	mux.HandleFunc("POST /api/open-dest", sv.apiOpenDest)
	sv.http = &http.Server{Handler: sv.guard(mux), ReadHeaderTimeout: 10 * time.Second}
	go sv.http.Serve(ln)
	go sv.fanout()
	idle := opt.IdleAfter
	if idle == 0 {
		idle = 60 * time.Second
	}
	if opt.LockDir != "" {
		sv.writeLock(opt.LockDir)
	}
	go func() {
		defer close(sv.done)
		defer sv.removeLock()
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				sv.http.Close()
				return
			case <-t.C:
				step := s.View().Step // outside sv.mu: requests never wait on the session
				sv.mu.Lock()
				stop := idle > 0 && shouldStop(sv.clients, time.Since(sv.lastSeen), step, sv.everSeen, idle)
				sv.mu.Unlock()
				if stop {
					cancel()
				}
			}
		}
	}()
	return sv, nil
}

// shouldStop: only when no window is connected and no job runs, after `after` since the last
// window left — or 10 minutes when no window has ever connected (the link may be opened late).
func shouldStop(clients int, idleFor time.Duration, step app.Step, everSeen bool, after time.Duration) bool {
	if !everSeen {
		after = max(after, 10*time.Minute)
	}
	return clients == 0 && step != app.StepRun && idleFor >= after
}

// Reopen asks a running GUI of this user (found through gui.lock in dir) for a fresh link.
func Reopen(dir string) (string, bool) {
	b, err := os.ReadFile(filepath.Join(dir, "gui.lock"))
	if err != nil {
		return "", false
	}
	var l lockFile
	if json.Unmarshal(b, &l) != nil || l.Port == 0 {
		return "", false
	}
	req, _ := http.NewRequest("POST", fmt.Sprintf("http://127.0.0.1:%d/internal/mint", l.Port), nil)
	req.Header.Set("X-Mint", l.Mint)
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Do(req)
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	var out struct{ URL string }
	if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&out) != nil || out.URL == "" {
		return "", false
	}
	return out.URL, true
}

// URL is a fresh single-use launch link.
func (sv *Server) URL() string { return sv.NewURL() }

// NewURL mints another single-use link (reopening the window mid-job).
func (sv *Server) NewURL() string {
	t := secret()
	sv.mu.Lock()
	sv.tokens[t] = true
	sv.mu.Unlock()
	return "http://" + sv.host + "/?t=" + t
}

// Done is closed when the server has stopped.
func (sv *Server) Done() <-chan struct{} { return sv.done }

// Close stops the server.
func (sv *Server) Close() {
	sv.cancel()
	<-sv.done
}

func (sv *Server) headers(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "DENY")
}

func (sv *Server) authed(r *http.Request) bool {
	c, err := r.Cookie(sv.cookieName())
	if err != nil {
		return false
	}
	sv.mu.Lock()
	defer sv.mu.Unlock()
	return sv.sessions[c.Value]
}

// guard applies the Host check and the security headers to everything, and the session check
// to everything except the login itself.
func (sv *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sv.headers(w)
		if r.Host != sv.host {
			http.Error(w, "wrong host", http.StatusForbidden)
			return
		}
		if r.URL.Path == "/internal/mint" && r.Method == http.MethodPost {
			if sv.mint == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Mint")), []byte(sv.mint)) != 1 {
				http.Error(w, "refused", http.StatusForbidden)
				return
			}
			writeJSON(w, 200, map[string]string{"URL": sv.NewURL()})
			return
		}
		if r.URL.Path == "/" && r.URL.Query().Get("t") != "" {
			if sv.authed(r) { // already logged in (an old link reopened): just show the app
				http.Redirect(w, r, "/", http.StatusSeeOther)
				return
			}
			sv.login(w, r)
			return
		}
		if !sv.authed(r) {
			http.Error(w, "Open the link that aio-sdcard-doctor printed (it works once). Run `aio-sdcard-doctor gui` again for a new one.", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet {
			if subtle.ConstantTimeCompare([]byte(r.Header.Get("Origin")), []byte("http://"+sv.host)) != 1 ||
				subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF")), []byte(sv.csrf)) != 1 {
				http.Error(w, "refused: cross-site request", http.StatusForbidden)
				return
			}
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

func (sv *Server) login(w http.ResponseWriter, r *http.Request) {
	t := r.URL.Query().Get("t")
	sv.mu.Lock()
	ok := sv.tokens[t]
	delete(sv.tokens, t) // single use, whatever happens
	var sess string
	if ok {
		sess = secret()
		sv.sessions[sess] = true
	}
	sv.mu.Unlock()
	if !ok {
		http.Error(w, "This link has been used already. Run `aio-sdcard-doctor gui` again for a new one.", http.StatusForbidden)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: sv.cookieName(), Value: sess, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
	http.Redirect(w, r, "/", http.StatusSeeOther) // drop the token from the address bar
}

// cookieName includes the port: cookies don't separate ports, so two GUIs on one computer would
// otherwise overwrite each other's session.
func (sv *Server) cookieName() string {
	return fmt.Sprintf("aio_session_%d", sv.ln.Addr().(*net.TCPAddr).Port)
}

func (sv *Server) index(w http.ResponseWriter, r *http.Request) {
	b, _ := fs.ReadFile(webFS, "web/index.html")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(b)
}

var mimeOf = map[string]string{".js": "text/javascript; charset=utf-8", ".css": "text/css; charset=utf-8", ".svg": "image/svg+xml"}

func (sv *Server) asset(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	b, err := fs.ReadFile(webFS, "web/"+name)
	if err != nil || strings.Contains(name, "..") {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", mimeOf[filepath.Ext(name)])
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(b)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func (sv *Server) apiCSRF(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]string{"CSRF": sv.csrf})
}

func (sv *Server) apiView(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"View": sv.sess.View(), "Theme": app.Theme})
}

func (sv *Server) apiDo(w http.ResponseWriter, r *http.Request) {
	var c app.Command
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&c); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad request"})
		return
	}
	if err := sv.sess.Do(c); err != nil {
		writeJSON(w, 422, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]string{})
}

// fanout copies the session's events to every connected window.
func (sv *Server) fanout() {
	for ev := range sv.sess.Events() {
		sv.mu.Lock()
		for ch := range sv.subs {
			if !deliver(ch, ev) {
				delete(sv.subs, ch) // too slow: disconnect; the window reconnects and reloads the view
				close(ch)
			}
		}
		sv.mu.Unlock()
	}
}

// deliver hands ev to a window without ever blocking. A missed progress tick is harmless; a
// missed state, question or finish is not, so then it reports false (disconnect that window).
func deliver(ch chan app.Event, ev app.Event) bool {
	select {
	case ch <- ev:
		return true
	default:
		return ev.Kind == "progress"
	}
}

func (sv *Server) apiEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", 500)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	ch := make(chan app.Event, 64)
	sv.mu.Lock()
	sv.subs[ch] = true
	sv.clients++
	sv.everSeen = true
	sv.mu.Unlock()
	defer func() {
		sv.mu.Lock()
		if sv.subs[ch] {
			delete(sv.subs, ch)
			close(ch)
		}
		sv.clients--
		sv.lastSeen = time.Now()
		sv.mu.Unlock()
	}()
	fmt.Fprint(w, "retry: 2000\n\nevent: hello\ndata: {}\n\n")
	fl.Flush()
	hb := time.NewTicker(15 * time.Second)
	defer hb.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-hb.C:
			fmt.Fprint(w, ": heartbeat\n\n")
			fl.Flush()
		case ev, ok := <-ch:
			if !ok {
				return // disconnected for being slow: the browser reconnects and reloads
			}
			b, _ := json.Marshal(ev)
			fmt.Fprintf(w, "data: %s\n\n", b)
			fl.Flush()
		}
	}
}

// apiDirs lists the sub-folders of a folder on this computer (choosing where files go): folders
// only, never file names or contents. path "" lists the drives / filesystem root.
func (sv *Server) apiDirs(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Query().Get("path")
	out := struct {
		Path, Parent string
		Dirs         []string
	}{Dirs: []string{}}
	if p == "" {
		if runtime.GOOS == "windows" {
			for c := 'A'; c <= 'Z'; c++ {
				if _, err := os.Stat(string(c) + `:\`); err == nil {
					out.Dirs = append(out.Dirs, string(c)+`:\`)
				}
			}
			writeJSON(w, 200, out)
			return
		}
		p = "/"
	}
	p = filepath.Clean(p)
	es, err := os.ReadDir(p)
	if err != nil {
		writeJSON(w, 422, map[string]string{"error": "Can't open that folder"})
		return
	}
	out.Path = p
	if par := filepath.Dir(p); par != p {
		out.Parent = par
	}
	for _, e := range es {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") && !strings.HasPrefix(e.Name(), "$") {
			out.Dirs = append(out.Dirs, e.Name())
		}
	}
	sort.Slice(out.Dirs, func(i, j int) bool { return strings.ToLower(out.Dirs[i]) < strings.ToLower(out.Dirs[j]) })
	writeJSON(w, 200, out)
}

// apiOpenDest opens the destination folder of the current flow in the file manager.
func (sv *Server) apiOpenDest(w http.ResponseWriter, r *http.Request) {
	d := sv.sess.View().Req.Dest
	if d == "" {
		writeJSON(w, 422, map[string]string{"error": "nothing to open"})
		return
	}
	if st, err := os.Stat(d); err == nil && !st.IsDir() {
		d = filepath.Dir(d)
	}
	if err := openFolder(d); err != nil {
		writeJSON(w, 422, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]string{})
}

var openFolder = func(dir string) error {
	switch runtime.GOOS {
	case "windows":
		return exec.Command("explorer", dir).Start()
	case "darwin":
		return exec.Command("open", dir).Start()
	}
	return exec.Command("xdg-open", dir).Start()
}

// launchCommands lists ways to open url as an app window, best first.
func launchCommands(goos, url, cacheDir string, exists func(string) bool) [][]string {
	profile := filepath.Join(cacheDir, "aio-sdcard-doctor", "webview")
	app := []string{"--app=" + url, "--user-data-dir=" + profile, "--no-first-run", "--no-default-browser-check"}
	var out [][]string
	switch goos {
	case "windows":
		for _, b := range []string{
			`C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`,
			`C:\Program Files\Microsoft\Edge\Application\msedge.exe`,
			`C:\Program Files\Google\Chrome\Application\chrome.exe`,
		} {
			if exists(b) {
				out = append(out, append([]string{b}, app...))
			}
		}
		out = append(out, []string{"rundll32", "url.dll,FileProtocolHandler", url})
	case "darwin":
		for _, b := range []string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
		} {
			if exists(b) {
				out = append(out, append([]string{b}, app...))
			}
		}
		out = append(out, []string{"open", url})
	default:
		for _, b := range []string{"chromium", "chromium-browser", "google-chrome", "microsoft-edge"} {
			if exists(b) {
				out = append(out, append([]string{b}, app...))
			}
		}
		out = append(out, []string{"xdg-open", url})
	}
	return out
}

// OpenAppWindow opens url in an app-style browser window (Edge/Chrome --app), else the default
// browser. The browser always runs as the normal user.
func OpenAppWindow(url string) error {
	cache, _ := os.UserCacheDir()
	exists := func(p string) bool {
		if filepath.IsAbs(p) {
			_, err := os.Stat(p)
			return err == nil
		}
		_, err := exec.LookPath(p)
		return err == nil
	}
	var last error
	for _, c := range launchCommands(runtime.GOOS, url, cache, exists) {
		if err := exec.Command(c[0], c[1:]...).Start(); err == nil {
			return nil
		} else {
			last = err
		}
	}
	return errors.Join(errors.New("couldn't open a browser window"), last)
}
