package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/job"
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
	w := p.io.Err
	fmt.Fprintf(w, "\n%s\n", is.Title)
	if is.Detail != "" {
		fmt.Fprintf(w, "  %s\n", is.Detail)
	}
	def := 1
	for i, c := range is.Choices {
		mark := ""
		if c == is.Default {
			mark, def = " (default)", i+1
		}
		fmt.Fprintf(w, "  %d) %s%s\n", i+1, label(c), mark)
	}
	for {
		fmt.Fprintf(w, "Choose 1-%d [%d]: ", len(is.Choices), def)
		a, ok := readAnswer(ctx, p.io.In)
		switch {
		case !ok:
			return job.Stop
		case a == "":
			return is.Choices[def-1]
		}
		if n, err := strconv.Atoi(a); err == nil && n >= 1 && n <= len(is.Choices) {
			return is.Choices[n-1]
		}
		fmt.Fprintf(w, "Please type a number from 1 to %d.\n", len(is.Choices))
	}
}

// printer writes progress at most once a second plus one line per problem file.
type printer struct {
	io    IO
	plain bool
	mu    sync.Mutex
	last  time.Time
	shown bool
}

func (p *printer) progress(pr job.Progress) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if time.Since(p.last) < time.Second {
		return
	}
	p.last = time.Now()
	pct := 0.0
	if pr.Total > 0 {
		pct = 100 * float64(pr.Done) / float64(pr.Total)
	}
	line := fmt.Sprintf("%s  %3.0f%%  %s / %s  %s", pr.Phase, pct, gb(pr.Done), gb(pr.Total), shorten(pr.Item, 50))
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

func shorten(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return "…" + string(r[len(r)-n+1:])
}

func gb(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.2f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%d KB", n>>10)
}
