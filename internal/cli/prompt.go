package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/job"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/preflight"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/tui"
)

var choiceText = map[job.Choice]string{
	job.Continue: "Continue", job.Retry: "Retry", job.Skip: "Skip", job.Wait: "Wait and keep trying",
	job.Stop: "Stop", job.Resume: "Resume where it stopped", job.New: "Start over", job.Image: "Make a full backup image instead",
	job.Elevate: "Restart as administrator", "skip-videos": "Skip the remaining videos",
	"skip-rest": "Skip the remaining files (report them FAILED, run again later)",
}

func label(c job.Choice) string {
	if t, ok := choiceText[c]; ok {
		return t
	}
	return string(c)
}

// errNoAnswer: the terminal closed (EOF) or Ctrl+C at a question. Nothing is ever chosen for
// the user in that case.
var errNoAnswer = errors.New("no answer — nothing was started or changed")

// lines delivers a reader's lines from one background goroutine, so a question can give up on
// Ctrl+C without losing or blocking later input. The channel closes at EOF.
var lineSources sync.Map

func lineChan(r io.Reader) <-chan string {
	if ch, ok := lineSources.Load(r); ok {
		return ch.(chan string)
	}
	ch := make(chan string, 64)
	if v, loaded := lineSources.LoadOrStore(r, ch); loaded {
		return v.(chan string)
	}
	go func() {
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			ch <- strings.TrimSpace(sc.Text())
		}
		close(ch)
	}()
	return ch
}

// readAnswer waits for one line; ok is false on EOF or when ctx ends (Ctrl+C).
func readAnswer(ctx context.Context, r io.Reader) (string, bool) {
	select {
	case s, ok := <-lineChan(r):
		return s, ok
	case <-ctx.Done():
		return "", false
	}
}

// readLine is readAnswer without a context (EOF reads as an empty answer).
func readLine(r io.Reader) string {
	s, _ := readAnswer(context.Background(), r)
	return s
}

// ttyPrompter asks on the terminal with numbered choices (Enter = the highlighted default, which
// the person sees). No answer (EOF, Ctrl+C) is "stop".
type ttyPrompter struct {
	io IO
	mu sync.Mutex
}

func (p *ttyPrompter) Ask(ctx context.Context, is job.Issue) job.Choice {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(is.Choices) == 0 {
		return is.Default
	}
	w, st := p.io.Err, styleOf(p.io)
	title := cleanText(is.Title) // titles and details name the card's files
	if st.on() {
		title = st.status("risk", title)
	}
	fmt.Fprintf(w, "\n%s\n", title)
	if is.Detail != "" {
		fmt.Fprintf(w, "  %s\n", cleanText(is.Detail))
	}
	def := 1
	for i, c := range is.Choices {
		mark := ""
		if c == is.Default {
			mark, def = " (default)", i+1
		}
		fmt.Fprintf(w, "  %s %s%s\n", st.paint("signal", true, fmt.Sprintf("%d)", i+1)), label(c), st.dim(mark))
	}
	for {
		fmt.Fprintf(w, "Choose 1-%d [%d]: ", len(is.Choices), def)
		a, ok := readAnswer(ctx, p.io.In)
		switch {
		case !ok: // Ctrl+C or the terminal closed: the safe answer, as Esc in the windows
			if slices.Contains(is.Choices, job.Stop) {
				return job.Stop
			}
			return is.Default
		case a == "":
			return is.Choices[def-1]
		}
		if n, err := strconv.Atoi(a); err == nil && n >= 1 && n <= len(is.Choices) {
			return is.Choices[n-1]
		}
		fmt.Fprintf(w, "Please type a number from 1 to %d.\n", len(is.Choices))
	}
}

// printer writes progress at most once a second plus one line per problem file. On a person's
// terminal (a Look) the progress is one line redrawn in place, and a problem line is printed
// above it.
type printer struct {
	io    IO
	plain bool
	mu    sync.Mutex
	last  time.Time
	shown bool
	line  string // the live line on screen (a Look)
}

func (p *printer) progress(pr job.Progress) {
	p.mu.Lock()
	defer p.mu.Unlock()
	// at most once a second — and always the last one, so a finished run never ends on "0%"
	st := styleOf(p.io)
	every := time.Second
	if st.on() && !p.plain {
		every = redrawEvery
	}
	if done := pr.Total > 0 && pr.Done >= pr.Total; !done && time.Since(p.last) < every {
		return
	}
	p.last = time.Now()
	if st.on() && !p.plain {
		p.line = st.progressLine(pr, st.width())
		p.redraw()
		return
	}
	pct := 0.0
	if pr.Total > 0 {
		pct = 100 * float64(pr.Done) / float64(pr.Total)
	}
	line := fmt.Sprintf("%s  %3.0f%%  %s / %s  %s", cleanText(pr.Phase), pct, preflight.GB(pr.Done), preflight.GB(pr.Total), shorten(cleanText(pr.Item), 50))
	if p.plain {
		fmt.Fprintln(p.io.Out, line)
	} else {
		fmt.Fprintf(p.io.Out, "\r%-100s", line)
		p.shown = true
	}
}

func (p *printer) log(level, msg string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	msg = cleanText(msg) // log lines name the card's files
	if st := styleOf(p.io); st.on() && !p.plain && p.shown {
		fmt.Fprint(p.io.Out, p.clear()) // the problem above, the live line again below it
		if level == "warn" {
			msg = "  " + st.paint("risk", false, tui.Symbol("risk", st.ascii())+" "+msg)
		}
		fmt.Fprintln(p.io.Out, msg)
		p.redraw()
		return
	}
	if p.shown {
		fmt.Fprintln(p.io.Out)
		p.shown = false
	}
	if level == "warn" {
		fmt.Fprintln(p.io.Out, "  "+msg)
	} else {
		fmt.Fprintln(p.io.Out, msg)
	}
}

// redraw puts the live line on screen in place of the last one.
func (p *printer) redraw() {
	fmt.Fprint(p.io.Out, p.clear()+p.line)
	p.shown = true
}

// clear returns to the start of the live line and blanks it.
func (p *printer) clear() string {
	if p.io.Look != nil && p.io.Look.VT {
		return "\r\x1b[K"
	}
	return "\r" + strings.Repeat(" ", max(0, styleOf(p.io).width()-1)) + "\r"
}

func shorten(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return "…" + string(r[len(r)-n+1:])
}
