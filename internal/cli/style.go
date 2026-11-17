package cli

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"golang.org/x/term"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/job"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/preflight"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/tui"
)

// Look is how a person's terminal is written to: status symbols and words, bold headings, colour
// and a progress line redrawn in place. StdIO sets it on a terminal; scripts, pipes, --plain and
// tests have none and get the plain, stable lines they always got. Every status keeps its word
// ("✔ OK", "✖ FAILED"), so nothing rests on colour or on a symbol.
type Look struct {
	Color bool       // colour: not with NO_COLOR, TERM=dumb or a console without ANSI support
	VT    bool       // the terminal understands ANSI (erase the line when redrawing)
	ASCII bool       // TERM=dumb: ASCII symbols only
	Width func() int // the terminal's columns now
	off   bool       // --plain
	r     *lipgloss.Renderer
}

// newLook is the look of this process's terminal, or nil when output isn't a person's terminal.
// The terminal's light or dark background is asked for here, once, before anything reads the
// keyboard (the answer comes back on it).
func newLook(tty bool) *Look {
	if !tty || !term.IsTerminal(int(os.Stderr.Fd())) {
		return nil
	}
	dumb := os.Getenv("TERM") == "dumb"
	l := &Look{VT: !dumb && enableVT(), ASCII: dumb, r: lipgloss.NewRenderer(os.Stdout)}
	l.Width = func() int {
		if w, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil && w > 0 {
			return w
		}
		return 80
	}
	l.Color = l.VT && os.Getenv("NO_COLOR") == "" && l.r.NewStyle().Foreground(lipgloss.Color("#FF0000")).Render("x") != "x"
	if l.Color {
		l.r.HasDarkBackground()
	}
	return l
}

// sty writes with a Look; the zero sty writes plain text.
type sty struct{ l *Look }

func styleOf(io IO) sty {
	if io.Look == nil || io.Look.off {
		return sty{}
	}
	return sty{io.Look}
}

func (s sty) on() bool { return s.l != nil }

// paint colours t with a token ("" = the terminal's own text colour), bold if asked.
func (s sty) paint(tok string, bold bool, t string) string {
	if s.l == nil || !s.l.Color || t == "" {
		return t
	}
	st := s.l.r.NewStyle().Bold(bold)
	if tok != "" {
		st = st.Foreground(tui.Colour(tok))
	}
	return st.Render(t)
}

func (s sty) head(t string) string { return s.paint("", true, t) }
func (s sty) dim(t string) string  { return s.paint("steel", false, t) }

// status is a tone's symbol and word, coloured: "✔ OK", "▲ PARTIAL", "✖ FAILED".
func (s sty) status(tone, word string) string {
	if sym := tui.Symbol(tone, s.ascii()); sym != "" {
		word = sym + " " + word
	}
	return s.paint(tone, true, word)
}

func (s sty) ascii() bool { return s.l == nil || s.l.ASCII }

func (s sty) width() int {
	if s.l == nil || s.l.Width == nil {
		return 80
	}
	return max(20, s.l.Width())
}

// kvLines lays out "Key: value" plan lines with the keys in a column (keys up to 12 wide);
// "Warning: …" gets the warning symbol and colour.
func (s sty) kvLines(ls []string) []string {
	keyW := 0
	for _, l := range ls {
		if k, _, ok := strings.Cut(l, ": "); ok && len([]rune(k)) <= 12 {
			keyW = max(keyW, len([]rune(k))+1)
		}
	}
	out := make([]string, 0, len(ls))
	for _, l := range ls {
		k, v, ok := strings.Cut(l, ": ")
		switch {
		case ok && k == "Warning":
			out = append(out, "  "+s.status("risk", l))
		case ok && len([]rune(k)) <= 12:
			if strings.Contains(v, "ERASE EVERYTHING") {
				v = s.paint("fail", true, v)
			}
			out = append(out, "  "+s.dim(fmt.Sprintf("%-*s", keyW, k+":"))+" "+v)
		default:
			out = append(out, "  "+l)
		}
	}
	return out
}

// counterLines are a result's counts, the good news first, each with its status symbol.
func (s sty) counterLines(counters map[string]int) []string {
	var keys []string
	for k := range counters {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		oi, oj := tui.ToneOrder(tui.Tone(keys[i])), tui.ToneOrder(tui.Tone(keys[j]))
		return oi < oj || oi == oj && keys[i] < keys[j]
	})
	var out []string
	for _, k := range keys {
		t := tui.Tone(k)
		name := fmt.Sprintf("%-22s", cleanText(k))
		if sym := tui.Symbol(t, s.ascii()); sym != "" {
			name = sym + " " + name
		} else {
			name = "  " + name
		}
		out = append(out, "  "+s.paint(t, t != "", name)+fmt.Sprintf(" %10s", preflight.Commas(counters[k])))
	}
	return out
}

// report bolds the headings of a plain-text report (scan) for the terminal: unindented lines
// ending in ":", the title, and the "Next step" line. The text itself is unchanged.
func (s sty) report(text string) string {
	if !s.on() {
		return text
	}
	ls := strings.Split(text, "\n")
	for i, l := range ls {
		switch {
		case i == 0 && strings.HasPrefix(l, "Card scan"):
			ls[i] = s.head(l)
		case strings.HasPrefix(l, "Next step:") || strings.HasPrefix(l, "Next:"):
			k, v, _ := strings.Cut(l, ":")
			ls[i] = s.paint("signal", true, k+":") + v
		case l != "" && !strings.HasPrefix(l, " ") && strings.HasSuffix(l, ":"):
			ls[i] = s.head(l)
		case strings.HasSuffix(l, "(recommended)"):
			ls[i] = strings.TrimSuffix(l, "(recommended)") + s.paint("safe", false, "(recommended)")
		}
	}
	return strings.Join(ls, "\n")
}

// progressLine is the live line on a terminal, cut to w columns: what is happening, a bar, the
// percentage, files done of all, speed, time left and the file being read. On a narrow terminal
// the bar, the percentage and the files done stay; the rest goes, the least important first.
func (s sty) progressLine(pr job.Progress, w int) string {
	frac, pct := 0.0, 0.0
	if pr.Total > 0 {
		frac = float64(pr.Done) / float64(pr.Total)
		pct = 100 * frac
	}
	l, r := "▕", "▏"
	if s.ascii() {
		l, r = "[", "]"
	}
	done, rest := tui.Bar(frac, max(10, min(w/6, 24)), s.ascii())
	parts := []string{l + s.paint("signal", false, done) + s.paint("rule", false, rest) + r + fmt.Sprintf(" %3.0f%%", pct)}
	if pr.ItemsTotal > 0 {
		parts = append(parts, fmt.Sprintf("%s of %s files", preflight.Commas(pr.Items), preflight.Commas(pr.ItemsTotal)))
	} else {
		parts = append(parts, preflight.GB(pr.Done)+" of "+preflight.GB(pr.Total))
	}
	if pr.BPS > 0 {
		parts = append(parts, fmt.Sprintf("%.1f MB/s", pr.BPS/1e6))
	}
	if left := tui.TimeLeft(pr.Total-pr.Done, pr.BPS); left != "" {
		parts = append(parts, left)
	}
	line := ""
	for _, p := range parts {
		if p == "" {
			continue
		}
		next := p
		if line != "" {
			next = line + "  " + p
		}
		if lipgloss.Width(next) > w-1 {
			break
		}
		line = next
	}
	// what is happening ("Copying files") leads when there is room for it too
	if ph := cleanText(pr.Phase); ph != "" && lipgloss.Width(ph)+2+lipgloss.Width(line) <= w-1 {
		line = ph + "  " + line
	}
	if item := cleanText(pr.Item); item != "" {
		if room := w - 1 - lipgloss.Width(line) - 2; room >= 12 {
			line += "  " + s.dim(shorten(item, room))
		}
	}
	return ansi.Truncate(line, w-1, "")
}

// redrawEvery: the live line on a terminal moves smoothly; scripts get a line a second.
const redrawEvery = 250 * time.Millisecond

// exitMessage marks the first line of a message the program ends with: ✖ for an error, ▲ when
// administrator rights are needed. The words are unchanged.
func (s sty) exitMessage(code int, msg string) string {
	if !s.on() {
		return msg
	}
	first, rest, _ := strings.Cut(msg, "\n")
	switch code {
	case ExitAdmin:
		first = s.status("risk", first)
	case ExitOK, ExitPartial:
	default:
		first = s.status("fail", first)
	}
	if rest != "" {
		return first + "\n" + rest
	}
	return first
}
