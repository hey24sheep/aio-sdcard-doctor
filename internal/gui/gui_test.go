package gui

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/app"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/drives"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/job"
)

// A made-up card and a fake action: nothing here touches a real drive.
type fakeAction struct{ ask bool }

func (f *fakeAction) Name() string { return "rescue" }
func (f *fakeAction) Plan(context.Context, job.Env) (job.Plan, error) {
	return job.Plan{Summary: []string{"From: Q:"}}, nil
}
func (f *fakeAction) Run(ctx context.Context, env job.Env, _ job.Plan) (job.Result, error) {
	if f.ask {
		env.Prompt.Ask(ctx, job.Issue{Kind: "stalled", Title: "Card not responding", Choices: []job.Choice{job.Wait, job.Stop}, Default: job.Wait})
	}
	return job.Result{Counters: map[string]int{"OK": 1}, Clean: true}, nil
}

type server struct {
	t       *testing.T
	base    string // http://127.0.0.1:port
	launch  string // with ?t=token
	srv     *Server
	sess    *app.Session
	ask     bool
	lockDir string
}

func start(t *testing.T) *server {
	t.Helper()
	sv := &server{t: t}
	ctx, cancel := context.WithCancel(context.Background())
	sv.sess = app.NewSession(ctx, app.Deps{
		ListDrives: func() ([]drives.Drive, error) {
			return []drives.Drive{{Root: `Q:\`, Label: "CAMERA", FS: "exFAT", Total: 64 << 30, Removable: true}}, nil
		},
		NewAction:   func(app.Request) (job.Action, error) { return &fakeAction{ask: sv.ask}, nil },
		IsAdmin:     func() bool { return true },
		DefaultDest: func(string) string { return `D:\rescue` },
	})
	sv.lockDir = t.TempDir()
	srv, err := Start(ctx, sv.sess, Options{LockDir: sv.lockDir})
	if err != nil {
		t.Fatal(err)
	}
	sv.srv, sv.launch = srv, srv.URL()
	u, _ := url.Parse(sv.launch)
	sv.base = u.Scheme + "://" + u.Host
	t.Cleanup(func() { srv.Close(); sv.sess.Close(); cancel() })
	return sv
}

// client logs in with the launch URL and returns a client holding the session cookie + CSRF token.
func (sv *server) client() (*http.Client, string) {
	sv.t.Helper()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, Timeout: 5 * time.Second}
	resp, err := c.Get(sv.srv.NewURL()) // every client logs in with its own single-use link
	if err != nil || resp.StatusCode != 200 {
		sv.t.Fatalf("login: %v %v", err, resp)
	}
	resp.Body.Close()
	resp, err = c.Get(sv.base + "/api/csrf")
	if err != nil || resp.StatusCode != 200 {
		sv.t.Fatalf("csrf: %v %v", err, resp)
	}
	var tok struct{ CSRF string }
	json.NewDecoder(resp.Body).Decode(&tok)
	resp.Body.Close()
	return c, tok.CSRF
}

func (sv *server) post(c *http.Client, csrf, origin string, cmd app.Command) *http.Response {
	b, _ := json.Marshal(cmd)
	req, _ := http.NewRequest("POST", sv.base+"/api/do", strings.NewReader(string(b)))
	req.Header.Set("Content-Type", "application/json")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if csrf != "" {
		req.Header.Set("X-CSRF", csrf)
	}
	resp, err := c.Do(req)
	if err != nil {
		sv.t.Fatal(err)
	}
	return resp
}

func TestBindsLoopbackOnly(t *testing.T) {
	sv := start(t)
	if !strings.HasPrefix(sv.base, "http://127.0.0.1:") {
		t.Fatalf("url %s", sv.base)
	}
}

func TestRejectsMissingToken(t *testing.T) {
	sv := start(t)
	for _, p := range []string{"/", "/api/view", "/assets/app.js", "/?t=wrong"} {
		resp, err := http.Get(sv.base + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s without a session: %d", p, resp.StatusCode)
		}
	}
}

func TestTokenSingleUse(t *testing.T) {
	sv := start(t)
	jar0, _ := cookiejar.New(nil)
	first, _ := (&http.Client{Jar: jar0}).Get(sv.launch) // uses the token
	first.Body.Close()
	if first.StatusCode != 200 {
		t.Fatalf("first use: %d", first.StatusCode)
	}
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	resp, _ := c.Get(sv.launch)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("token reused: %d", resp.StatusCode)
	}
	// a fresh link can be minted (reopening the window mid-job)
	jar2, _ := cookiejar.New(nil)
	c2 := &http.Client{Jar: jar2}
	resp, _ = c2.Get(sv.srv.NewURL())
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("fresh link: %d", resp.StatusCode)
	}
}

func TestRejectsWrongHost(t *testing.T) {
	sv := start(t)
	c, _ := sv.client()
	req, _ := http.NewRequest("GET", sv.base+"/api/view", nil)
	req.Host = "evil.example:80" // DNS rebinding: the browser sends the attacker's host name
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("wrong Host: %d", resp.StatusCode)
	}
}

func TestRejectsCrossOriginPost(t *testing.T) {
	sv := start(t)
	c, csrf := sv.client()
	resp := sv.post(c, csrf, "http://evil.example", app.Command{Name: "refresh"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin: %d", resp.StatusCode)
	}
	resp = sv.post(c, csrf, "", app.Command{Name: "refresh"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("no Origin: %d", resp.StatusCode)
	}
}

func TestRejectsMissingCSRF(t *testing.T) {
	sv := start(t)
	c, _ := sv.client()
	resp := sv.post(c, "", sv.base, app.Command{Name: "refresh"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("no CSRF: %d", resp.StatusCode)
	}
	c2, csrf := sv.client()
	resp = sv.post(c2, csrf, sv.base, app.Command{Name: "refresh"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("good request: %d", resp.StatusCode)
	}
}

func TestNoCORSHeadersAndCSP(t *testing.T) {
	sv := start(t)
	c, _ := sv.client()
	for _, p := range []string{"/", "/api/view", "/assets/app.js"} {
		req, _ := http.NewRequest("GET", sv.base+p, nil)
		req.Header.Set("Origin", "http://evil.example")
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.Header.Get("Access-Control-Allow-Origin") != "" {
			t.Errorf("%s: CORS header", p)
		}
		if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") || !strings.Contains(csp, "frame-ancestors 'none'") {
			t.Errorf("%s: CSP %q", p, csp)
		}
		if resp.Header.Get("X-Content-Type-Options") != "nosniff" || resp.Header.Get("Referrer-Policy") != "no-referrer" {
			t.Errorf("%s: headers %v", p, resp.Header)
		}
	}
}

func TestCookieFlags(t *testing.T) {
	sv := start(t)
	resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Get(sv.launch)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	ck := resp.Header.Get("Set-Cookie")
	if !strings.Contains(ck, "HttpOnly") || !strings.Contains(ck, "SameSite=Strict") {
		t.Fatalf("cookie %q", ck)
	}
}

func TestViewAndDo(t *testing.T) {
	sv := start(t)
	c, csrf := sv.client()
	resp := sv.post(c, csrf, sv.base, app.Command{Name: "select-drive", Args: map[string]string{"root": `Q:\`}})
	resp.Body.Close()
	resp, _ = c.Get(sv.base + "/api/view")
	var v struct {
		View  app.View
		Theme map[string][2]string
	}
	json.NewDecoder(resp.Body).Decode(&v)
	resp.Body.Close()
	if v.View.Step != app.StepAction || v.View.Card == nil || v.Theme["signal"][0] != "#1F4FD1" {
		t.Fatalf("view %+v theme %v", v.View.Step, v.Theme["signal"])
	}
	// a refused command comes back as 422 with the message to show
	resp = sv.post(c, csrf, sv.base, app.Command{Name: "choose-action", Args: map[string]string{"action": "bogus"}})
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 422 || !strings.Contains(string(b), "bogus") {
		t.Fatalf("refused: %d %s", resp.StatusCode, b)
	}
}

func TestSSEDeliversIssueAndAnswer(t *testing.T) {
	sv := start(t)
	sv.ask = true
	c, csrf := sv.client()
	c.Timeout = 0
	ev, err := c.Get(sv.base + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	defer ev.Body.Close()
	if ct := ev.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	for _, cmd := range []app.Command{{Name: "select-drive", Args: map[string]string{"root": `Q:\`}}, {Name: "quick-rescue"}} {
		sv.post(c, csrf, sv.base, cmd).Body.Close()
	}
	waitStep(t, sv.sess, app.StepReview)
	sv.post(c, csrf, sv.base, app.Command{Name: "start"}).Body.Close()
	sc := bufio.NewScanner(ev.Body)
	id := ""
	for sc.Scan() && id == "" {
		line := sc.Text()
		if strings.HasPrefix(line, "data: ") && strings.Contains(line, `"kind":"issue"`) {
			var e app.Event
			json.Unmarshal([]byte(line[6:]), &e)
			id = e.IssueID
		}
	}
	if id == "" {
		t.Fatal("no issue event")
	}
	sv.post(c, csrf, sv.base, app.Command{Name: "answer", Args: map[string]string{"id": id, "choice": "stop"}}).Body.Close()
	waitStep(t, sv.sess, app.StepResults)
}

func waitStep(t *testing.T, s *app.Session, st app.Step) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if v := s.View(); v.Step == st && v.Busy == "" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("never reached %s (at %s)", st, s.View().Step)
}

func TestDirsListsDirectoriesOnly(t *testing.T) {
	sv := start(t)
	c, _ := sv.client()
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, "sub"), 0o755)
	os.WriteFile(filepath.Join(dir, "file.txt"), []byte("x"), 0o644)
	resp, _ := c.Get(sv.base + "/api/dirs?path=" + url.QueryEscape(dir))
	var d struct {
		Path, Parent string
		Dirs         []string
	}
	json.NewDecoder(resp.Body).Decode(&d)
	resp.Body.Close()
	if !slices.Equal(d.Dirs, []string{"sub"}) {
		t.Fatalf("dirs %+v", d)
	}
}

func TestAssetsEmbeddedNoExternalURLs(t *testing.T) {
	err := fs.WalkDir(webFS, "web", func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		b, _ := fs.ReadFile(webFS, p)
		for _, bad := range []string{"http://", "https://", "//cdn", "fonts.googleapis"} {
			if strings.Contains(string(b), bad) && !strings.Contains(string(b), "http://www.w3.org/2000/svg") {
				t.Errorf("%s refers to %s", p, bad)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestLaunchArgsPerOS(t *testing.T) {
	has := func(p string) bool { return strings.Contains(p, "msedge") || strings.Contains(p, "chromium") }
	w := launchCommands("windows", "http://127.0.0.1:1/?t=x", `C:\Users\me\AppData\Local`, has)
	if len(w) == 0 || !strings.Contains(w[0][0], "msedge") || !slices.Contains(w[0], "--app=http://127.0.0.1:1/?t=x") {
		t.Errorf("windows %v", w)
	}
	l := launchCommands("linux", "http://u", "/home/me/.cache", has)
	if l[0][0] != "chromium" || l[len(l)-1][0] != "xdg-open" {
		t.Errorf("linux %v", l)
	}
	m := launchCommands("darwin", "http://u", "/tmp", func(string) bool { return false })
	if m[len(m)-1][0] != "open" {
		t.Errorf("darwin %v", m)
	}
}
