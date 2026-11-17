package gui

import (
	"context"
	"encoding/json"
	"errors"
	"html"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/app"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/drives"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/job"
)

// M4 review #4: `gui` while a GUI runs gets a fresh link to it (reopening a closed window mid-job).
func TestReopenMintsFreshLink(t *testing.T) {
	sv := start(t) // started with a lock dir (see start)
	url, ok := Reopen(sv.lockDir, false)
	if !ok || !strings.HasPrefix(url, sv.base+"/?t=") {
		t.Fatalf("reopen: %q %v", url, ok)
	}
	jar, _ := cookiejar.New(nil)
	resp, err := (&http.Client{Jar: jar}).Get(url)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("fresh link: %v %v", err, resp)
	}
	resp.Body.Close()
	// a forged mint request without the lock file's secret is refused
	req, _ := http.NewRequest("POST", sv.base+"/internal/mint", nil)
	req.Header.Set("X-Mint", "wrong")
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("forged mint: %d", resp.StatusCode)
	}
	sv.srv.Close()
	if _, ok := Reopen(sv.lockDir, false); ok {
		t.Fatal("reopened a GUI that has stopped")
	}
}

// M4 review #4: opening a used link in a window that is already logged in goes home, not 403.
func TestUsedLinkWithSessionGoesHome(t *testing.T) {
	sv := start(t)
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	resp, _ := c.Get(sv.launch)
	resp.Body.Close()
	resp, _ = c.Get(sv.launch)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("used link with a session: %d", resp.StatusCode)
	}
}

// M4 review #4: two GUIs on one computer don't overwrite each other's session cookie.
func TestCookieNamePerPort(t *testing.T) {
	sv := start(t)
	resp, _ := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Get(sv.launch)
	resp.Body.Close()
	port := sv.base[strings.LastIndex(sv.base, ":")+1:]
	if ck := resp.Header.Get("Set-Cookie"); !strings.HasPrefix(ck, "aio_session_"+port+"=") {
		t.Fatalf("cookie %q", ck)
	}
}

// M4 review #7: a window too slow to keep up is disconnected (it reconnects and reloads the view)
// rather than silently missing a question.
func TestSlowWindowIsResynced(t *testing.T) {
	ch := make(chan app.Event, 1)
	if !deliver(ch, app.Event{Kind: "progress"}) {
		t.Fatal("first event refused")
	}
	if !deliver(ch, app.Event{Kind: "progress"}) {
		t.Fatal("a dropped progress tick must not disconnect")
	}
	if deliver(ch, app.Event{Kind: "issue"}) {
		t.Fatal("a question that can't be delivered must disconnect the window")
	}
}

// M4 review #14: before any window has connected the GUI waits much longer (the link may be opened late).
func TestIdleWaitsLongerBeforeFirstWindow(t *testing.T) {
	if shouldStop(0, 2*time.Minute, app.StepDrive, false, false, time.Minute) {
		t.Error("stopped 2 min after start with no window ever opened")
	}
	if !shouldStop(0, 11*time.Minute, app.StepDrive, false, false, time.Minute) {
		t.Error("never stops when nobody opens the link")
	}
	if !shouldStop(0, 2*time.Minute, app.StepResults, true, false, time.Minute) {
		t.Error("window closed 2 min ago, no job: should stop")
	}
	if shouldStop(0, time.Hour, app.StepRun, true, false, time.Minute) {
		t.Error("stopped during a run")
	}
	if shouldStop(0, 30*time.Second, app.StepDrive, true, false, time.Minute) {
		t.Error("ignored IdleAfter")
	}
}

var _ = context.Background

// Adversarial pass U3: "Run as Administrator: aio-sdcard-doctor gui" while the plain window was
// still alive reopened that same window, back on the "needs administrator" page. An elevated
// `gui` starts its own server instead.
func TestElevatedGUIDoesNotReopenPlainOne(t *testing.T) {
	sv := start(t) // a plain (not elevated) server
	if _, ok := Reopen(sv.lockDir, true); ok {
		t.Fatal("an elevated gui reopened the plain window")
	}
	if _, ok := Reopen(sv.lockDir, false); !ok {
		t.Fatal("a plain gui no longer reopens the plain window")
	}
}

// Adversarial pass 2 OU2: a launch link opened in a window that is already signed in went home
// but stayed valid for the server's life — anyone who got the link later could sign in with it.
func TestLaunchLinkSpentBySignedInWindow(t *testing.T) {
	sv := start(t)
	c, _ := sv.client() // signed in
	link := sv.srv.NewURL()
	resp, err := c.Get(link)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("signed-in window: %v %v", err, resp)
	}
	resp.Body.Close()
	resp, err = (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Get(link)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || resp.Header.Get("Set-Cookie") != "" {
		t.Fatalf("a link used by a signed-in window signed in someone else: %d %q", resp.StatusCode, resp.Header.Get("Set-Cookie"))
	}
}

// Adversarial pass 2 SU8: `gui --port N` while a GUI runs overwrote its gui.lock, so the next
// `gui` could no longer reopen the window that holds the running job.
func TestSecondServerKeepsTheLiveLock(t *testing.T) {
	sv := start(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s2 := app.NewSession(ctx, app.Deps{ListDrives: func() ([]drives.Drive, error) { return nil, nil },
		NewAction: func(app.Request) (job.Action, error) { return &fakeAction{}, nil }})
	defer s2.Close()
	other, err := Start(ctx, s2, Options{LockDir: sv.lockDir})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	url, ok := Reopen(sv.lockDir, false)
	if !ok || !strings.HasPrefix(url, sv.base+"/") {
		t.Fatalf("reopen went to %q (%v), not the first window at %s", url, ok, sv.base)
	}
	other.Close()
	if _, ok := Reopen(sv.lockDir, false); !ok {
		t.Fatal("the second server removed the first one's lock")
	}
}

// Adversarial pass 2 OU10: the window closed during a long recover listing; the list appeared
// with no window to see it and the GUI stopped 60 s later, losing it.
func TestIdleKeepsAnUnseenList(t *testing.T) {
	for _, st := range []app.Step{app.StepRecoverList, app.StepResults} {
		if shouldStop(0, time.Hour, st, true, true, time.Minute) {
			t.Errorf("stopped on %s that no window has seen", st)
		}
		if !shouldStop(0, 2*time.Minute, st, true, false, time.Minute) {
			t.Errorf("never stops on %s already seen", st)
		}
	}
}

// Adversarial pass 3 OU8: a crashed GUI's gui.lock whose port another program now uses made every
// later GUI keep that dead lock: `gui` could never reopen the running window. Only a GUI that
// answers the old lock's own secret counts as running.
func TestStaleLockOnReusedPortIsReplaced(t *testing.T) {
	other, err := net.Listen("tcp", "127.0.0.1:0") // an unrelated program on the old port
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	go func() {
		for {
			c, err := other.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	dir := t.TempDir()
	b, _ := json.Marshal(lockFile{Port: other.Addr().(*net.TCPAddr).Port, Mint: "crashed"})
	os.WriteFile(filepath.Join(dir, "gui.lock"), b, 0o600)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := app.NewSession(ctx, app.Deps{ListDrives: func() ([]drives.Drive, error) { return nil, nil },
		NewAction: func(app.Request) (job.Action, error) { return &fakeAction{}, nil }})
	defer s.Close()
	srv, err := Start(ctx, s, Options{LockDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	if url, ok := Reopen(dir, false); !ok || !strings.HasPrefix(url, "http://"+srv.host+"/") {
		t.Fatalf("the running GUI can't be reopened: %q %v", url, ok)
	}
}

// Adversarial pass 3 OU12: "Open the folder" after an image handed the file manager the image's
// path even when that file isn't there (compressed, stopped early): it opens the folder instead.
func TestOpenFolderOfAnImage(t *testing.T) {
	dir := t.TempDir()
	img := filepath.Join(dir, "card.img")
	if got := folderToOpen(img); got != dir {
		t.Fatalf("missing image: opens %q", got)
	}
	os.WriteFile(img, []byte("x"), 0o600)
	if got := folderToOpen(img); got != dir {
		t.Fatalf("image file: opens %q", got)
	}
	if got := folderToOpen(dir); got != dir {
		t.Fatalf("folder: opens %q", got)
	}
}

// Adversarial pass 3 (security, OU note): on Linux and macOS every local user can read a
// program's command line (ps), and the browser was started with the launch link on it. The
// browser opens an owner-only page instead, which sends it on to the link.
func TestLaunchLinkNeverOnTheBrowsersCommandLine(t *testing.T) {
	dir := t.TempDir()
	link := "http://127.0.0.1:5/?t=secret&x=<y>"
	if got := launchTarget("windows", link, dir); got != link { // Windows: other users can't read it
		t.Fatalf("windows opens %q", got)
	}
	old := filepath.Join(dir, "open-old.html") // left by a `gui` that exited straight away
	os.WriteFile(old, []byte("x"), 0o600)
	stale := time.Now().Add(-2 * time.Minute)
	os.Chtimes(old, stale, stale)
	for _, goos := range []string{"linux", "darwin"} {
		target := launchTarget(goos, link, dir)
		if strings.Contains(target, "secret") || !strings.HasPrefix(target, "file://") {
			t.Fatalf("%s opens %q", goos, target)
		}
		u, err := url.Parse(target)
		if err != nil {
			t.Fatal(err)
		}
		p := u.Path
		if runtime.GOOS == "windows" {
			p = strings.TrimPrefix(p, "/")
		}
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), `http-equiv="refresh"`) || !strings.Contains(string(b), html.EscapeString(link)) {
			t.Fatalf("page:\n%s", b)
		}
		if st, _ := os.Stat(p); runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
			t.Fatalf("page mode %v", st.Mode())
		}
	}
	if _, err := os.Stat(old); err == nil {
		t.Fatal("an old launch page was left")
	}
}

// The link is signed in from a page the browser opened from a file: a redirect there would carry
// the start of a cross-site navigation, and the SameSite=Strict cookie would not be sent with it.
// Signing in answers with a page that moves on to the app from the app's own address.
func TestSignInMovesOnFromItsOwnPage(t *testing.T) {
	sv := start(t)
	resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Get(sv.launch)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Set-Cookie") == "" || !strings.Contains(string(b), `content="0;url=/"`) {
		t.Fatalf("sign-in: %d %q\n%s", resp.StatusCode, resp.Header.Get("Set-Cookie"), b)
	}
}

// Adversarial pass 3 SU11: with no cache folder (no HOME), the browser profile was a relative
// path — inside whatever folder the program ran from, a card even.
func TestBrowserProfileAlwaysAbsolute(t *testing.T) {
	got := cacheBase("", errors.New("no home"))
	if !filepath.IsAbs(got) || !strings.HasPrefix(got, os.TempDir()) {
		t.Fatalf("cache base %q", got)
	}
	os.Remove(got)
	if got := cacheBase(`relative`, nil); !filepath.IsAbs(got) {
		t.Fatalf("relative cache dir kept: %q", got)
	} else {
		os.Remove(got)
	}
}

// Adversarial pass 3 SU9: a result nobody came back for kept a window-less GUI running forever.
func TestUnseenResultWaitsOnlySoLong(t *testing.T) {
	if !shouldStop(0, unseenFor+time.Minute, app.StepResults, true, true, time.Minute) {
		t.Fatal("an unseen result keeps the GUI running forever")
	}
	if shouldStop(0, unseenFor-time.Minute, app.StepResults, true, true, time.Minute) {
		t.Fatal("an unseen result is dropped too soon")
	}
}
