package tui

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/app"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/job"
)

func TestBarIsSmoothAndExact(t *testing.T) {
	for _, c := range []struct {
		frac float64
		w    int
	}{{0, 20}, {0.5, 20}, {0.64, 30}, {0.01, 10}, {1, 16}, {1.5, 8}, {-1, 8}} {
		done, rest := Bar(c.frac, c.w, false)
		if n := utf8.RuneCountInString(done + rest); n != c.w {
			t.Errorf("%.2f of %d: %d cells %q", c.frac, c.w, n, done+rest)
		}
		d, r := Bar(c.frac, c.w, true)
		if len(d+r) != c.w || strings.Trim(d, "#") != "" || strings.Trim(r, "-") != "" {
			t.Errorf("ASCII %.2f of %d: %q", c.frac, c.w, d+r)
		}
	}
	if done, _ := Bar(0.53, 10, false); done != "█████▎" { // 5.3 cells: five whole, a quarter of the next
		t.Errorf("partial block: %q", done)
	}
}

func TestSparkScalesToTheFastest(t *testing.T) {
	if s := Spark([]float64{0, 5, 10}, 10, false); s != "▁▅█" {
		t.Errorf("spark %q", s)
	}
	if s := Spark([]float64{1, 2, 3, 4}, 2, true); len(s) != 2 || s[1] != '#' { // the last w samples
		t.Errorf("ASCII spark %q", s)
	}
	if Spark(nil, 10, false) != "" || Spark([]float64{1}, 0, false) != "" {
		t.Error("empty spark")
	}
}

func TestTimeLeftInWords(t *testing.T) {
	for _, c := range []struct {
		left int64
		bps  float64
		want string
	}{{100, 0, ""}, {0, 10, ""}, {30e6, 1e6, "less than a minute left"}, {12 * 60e6, 1e6, "about 12 min left"},
		{3600e6, 1e6, "about 1 h left"}, {(3600 + 20*60) * 1e6, 1e6, "about 1 h 20 min left"}, {5 * 86400e6, 1e6, "about 5 days left"}} {
		if got := TimeLeft(c.left, c.bps); got != c.want {
			t.Errorf("%d at %.0f: %q, want %q", c.left, c.bps, got, c.want)
		}
	}
}

// An unknown counter is never shown as good news.
func TestToneOnlyCallsKnownCountersSafe(t *testing.T) {
	for k, want := range map[string]string{"OK": "safe", "ALREADY-COPIED": "safe", "PARTIAL": "risk", "FAILED": "fail", "MISSING": "fail",
		"EXCLUDED": "", "KEPT-FROM-EARLIER-RUN": "", "SOMETHING-NEW": ""} {
		if got := Tone(k); got != want {
			t.Errorf("%s: %q, want %q", k, got, want)
		}
	}
	if Symbol("", false) != "" || Symbol("fail", true) != "x" {
		t.Error("symbols")
	}
}

func TestBigDigitsLineUp(t *testing.T) {
	for _, s := range []string{"0%", "64%", "100%"} {
		b := Big(s)
		if w := lipgloss.Width(b[0]); w == 0 || lipgloss.Width(b[1]) != w || lipgloss.Width(b[2]) != w {
			t.Errorf("%s: rows %q", s, b)
		}
	}
}

// The step track says which step is current with a symbol, so it shows without colour too.
func TestTrackMarksStepsWithSymbols(t *testing.T) {
	r := newRig(t)
	r.key("enter")
	sc := r.screen()
	if !strings.Contains(sc, "✔ Card") || !strings.Contains(sc, "◉ What to do") {
		t.Fatalf("track:\n%s", sc)
	}
	r.m.opt.ASCII = true
	if sc := r.screen(); !strings.Contains(sc, "* Card") || !strings.Contains(sc, "(o) What to do") {
		t.Fatalf("ASCII track:\n%s", sc)
	}
}

// --ascii: every screen in ASCII (the program's own text; the made-up card's text is ASCII too).
func TestASCIIModeIsASCIIEverywhere(t *testing.T) {
	r := newRig(t)
	r.m.opt.ASCII = true
	check := func(where string) {
		t.Helper()
		for i, l := range strings.Split(r.screen(), "\n") {
			for _, c := range l {
				if c > 0x7e {
					t.Fatalf("%s line %d has %q: %s", where, i, c, l)
				}
			}
		}
	}
	for _, sz := range [][2]int{{60, 20}, {100, 30}} {
		r.size(sz[0], sz[1])
		check("cards")
	}
	r.key("enter")
	check("what to do")
	r.key("enter")
	check("check")
	r.key("enter")
	check("done")
}

// The run screen: the percentage, files safe of all, the bar, speed, time left and the file.
func TestRunScreenShowsTheWholeState(t *testing.T) {
	old := sampleEvery
	sampleEvery = 0
	defer func() { sampleEvery = old }()
	r := newRig(t)
	r.mutate = func(a *fakeAction) {
		a.plan.Items = 2031
		a.run = func(ctx context.Context, env job.Env) (job.Result, error) {
			for _, bps := range []float64{20e6, 30e6} {
				env.Progress(job.Progress{Phase: "Copying files", Item: "DCIM/IMG_1.JPG", Done: 64 << 20, Total: 100 << 20, BPS: bps, Counters: map[string]int{"OK": 1301}})
			}
			<-ctx.Done()
			return job.Result{}, ctx.Err()
		}
	}
	r.key("enter", "enter")
	r.m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	for i := 0; i < 200 && (r.m.v.Step != app.StepRun || r.m.v.Progress == nil || len(r.m.speeds) < 2); i++ {
		time.Sleep(5 * time.Millisecond)
		r.m.Update(viewMsg{})
	}
	r.want(t, "1,301 of 2,031 files safe so far", "64%", "30.0 MB/s", "left", "DCIM/IMG_1.JPG", "Speed")
	for _, sz := range [][2]int{{60, 20}, {80, 24}, {120, 40}} {
		r.size(sz[0], sz[1])
		for i, l := range strings.Split(r.screen(), "\n") {
			if lipgloss.Width(l) > sz[0] {
				t.Errorf("%dx%d line %d too wide", sz[0], sz[1], i)
			}
		}
	}
}
