package gui

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"
	"time"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/app"
)

// M4 review #4: `gui` while a GUI runs gets a fresh link to it (reopening a closed window mid-job).
func TestReopenMintsFreshLink(t *testing.T) {
	sv := start(t) // started with a lock dir (see start)
	url, ok := Reopen(sv.lockDir)
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
	if _, ok := Reopen(sv.lockDir); ok {
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
	if shouldStop(0, 2*time.Minute, app.StepDrive, false, time.Minute) {
		t.Error("stopped 2 min after start with no window ever opened")
	}
	if !shouldStop(0, 11*time.Minute, app.StepDrive, false, time.Minute) {
		t.Error("never stops when nobody opens the link")
	}
	if !shouldStop(0, 2*time.Minute, app.StepResults, true, time.Minute) {
		t.Error("window closed 2 min ago, no job: should stop")
	}
	if shouldStop(0, time.Hour, app.StepRun, true, time.Minute) {
		t.Error("stopped during a run")
	}
	if shouldStop(0, 30*time.Second, app.StepDrive, true, time.Minute) {
		t.Error("ignored IdleAfter")
	}
}

var _ = context.Background
