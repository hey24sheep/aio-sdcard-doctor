package tui

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/app"
)

// The look shared by the terminal UI and the command line: colour tokens, the status words and
// symbols, the progress bar, the speed sparkline and "time left". Nothing here decides anything:
// every status keeps its word, so it never rests on colour (NO_COLOR) or on a symbol (--ascii).

// extraTokens are the terminal-only tokens next to app.Theme (shared with the GUI): the gold of
// the card's contacts for the brand mark, and the panel rule. Measured (WCAG) on a white and a
// #1E1E1E terminal: contact 4.68 / 8.79, rule 3.01 / 3.69 (a border: 3:1).
var extraTokens = map[string][2]string{
	"contact": {"#9A6B12", "#E2B65A"},
	"rule":    {"#8A96A3", "#6A7887"},
}

// Token is a colour token as {light, dark} hex.
func Token(tok string) [2]string {
	if t, ok := app.Theme[tok]; ok {
		return t
	}
	return extraTokens[tok]
}

// Colour is a token as a lipgloss colour that follows the terminal's light or dark background.
func Colour(tok string) lipgloss.AdaptiveColor {
	t := Token(tok)
	return lipgloss.AdaptiveColor{Light: t[0], Dark: t[1]}
}

// Tone sorts a result counter into safe, risk or fail ("" = neither: shown plainly). Only names
// known to mean "saved" get the safe tone; an unknown one is never shown as good news.
func Tone(counter string) string {
	switch strings.ToUpper(counter) {
	case "OK", "ALREADY-COPIED", "DELETED-RECOVERED", "CARVED", "DONE":
		return "safe"
	case "PARTIAL", "STALLED", "PROBLEMS-FOUND", "DELETED-UNRECOVERABLE":
		return "risk"
	case "FAILED", "MISSING", "CORRUPT", "WRONG-SIZE", "UNREADABLE":
		return "fail"
	}
	return ""
}

// ToneOrder: safe counters first, then risk, then fail, then the rest — the good news reads
// first and the problems sit together.
func ToneOrder(tone string) int {
	switch tone {
	case "safe":
		return 0
	case "risk":
		return 1
	case "fail":
		return 2
	}
	return 3
}

// Symbol is the status symbol of a tone (Unicode, or ASCII with ascii).
func Symbol(tone string, ascii bool) string {
	u, a := map[string]string{"safe": "✔", "risk": "▲", "fail": "✖"}[tone], map[string]string{"safe": "+", "risk": "!", "fail": "x"}[tone]
	if ascii {
		return a
	}
	return u
}

var eighths = []string{"", "▏", "▎", "▍", "▌", "▋", "▊", "▉"}

// Bar is a progress bar w cells wide: whole blocks, then the partial eighth of the next cell
// (smooth on a slow card), then the empty track. ASCII: '#' and '-'. done and rest are the two
// parts, so each can be coloured.
func Bar(frac float64, w int, ascii bool) (done, rest string) {
	if w < 1 {
		return "", ""
	}
	frac = math.Max(0, math.Min(1, frac))
	if ascii {
		n := int(frac * float64(w))
		return strings.Repeat("#", n), strings.Repeat("-", w-n)
	}
	e := int(frac * float64(w) * 8)
	full, part := e/8, e%8
	done = strings.Repeat("█", full) + eighths[part]
	used := full
	if part > 0 {
		used++
	}
	return done, strings.Repeat("░", w-used)
}

// Spark is a sparkline of the last w samples, scaled to the largest (▁…█, ASCII " .:-=+*#").
func Spark(samples []float64, w int, ascii bool) string {
	if w < 1 || len(samples) == 0 {
		return ""
	}
	if len(samples) > w {
		samples = samples[len(samples)-w:]
	}
	top := 0.0
	for _, s := range samples {
		top = math.Max(top, s)
	}
	ramp := []rune("▁▂▃▄▅▆▇█")
	if ascii {
		ramp = []rune(" .:-=+*#")
	}
	var b strings.Builder
	for _, s := range samples {
		i := 0
		if top > 0 {
			i = int(math.Round(s / top * float64(len(ramp)-1)))
		}
		b.WriteRune(ramp[max(0, min(i, len(ramp)-1))])
	}
	return b.String()
}

// TimeLeft is "about 12 min left" from what is left and the current speed ("" when unknown).
func TimeLeft(left int64, bps float64) string {
	if left <= 0 || bps <= 0 {
		return ""
	}
	d := time.Duration(float64(left) / bps * float64(time.Second))
	switch {
	case d < time.Minute:
		return "less than a minute left"
	case d < time.Hour:
		return fmt.Sprintf("about %d min left", int(math.Ceil(d.Minutes())))
	case d < 48*time.Hour:
		h := int(d.Hours())
		m := int(d.Minutes()) - 60*h
		if m == 0 {
			return fmt.Sprintf("about %d h left", h)
		}
		return fmt.Sprintf("about %d h %d min left", h, m)
	}
	return fmt.Sprintf("about %d days left", int(math.Round(d.Hours()/24)))
}

// asciiFold swaps the typographic marks of the program's own text for ASCII look-alikes, for
// --ascii terminals that can't show them. Other characters (a card's accented file names) stay.
var asciiFold = strings.NewReplacer("—", "-", "–", "-", "·", "|", "…", "...", "×", "x", "←", "<-", "→", "->",
	"↑", "^", "↓", "v", "’", "'", "‘", "'", "“", `"`, "”", `"`, "▏", "_", "•", "*")

// wrapText wraps plain text to w columns (words first, long words broken).
func wrapText(s string, w int) []string {
	if w < 1 || lipgloss.Width(s) <= w {
		return []string{s}
	}
	return strings.Split(ansi.Wrap(s, w, " /\\-"), "\n")
}

// bigDigits are the run screen's percentage, three cells high, drawn with half blocks.
var bigDigits = map[rune][3]string{
	'0': {"█▀█", "█ █", "▀▀▀"}, '1': {"▀█ ", " █ ", "▀▀▀"}, '2': {"▀▀█", "█▀▀", "▀▀▀"},
	'3': {"▀▀█", " ▀█", "▀▀▀"}, '4': {"█ █", "▀▀█", "  ▀"}, '5': {"█▀▀", "▀▀█", "▀▀▀"},
	'6': {"█▀▀", "█▀█", "▀▀▀"}, '7': {"▀▀█", "  █", "  ▀"}, '8': {"█▀█", "█▀█", "▀▀▀"},
	'9': {"█▀█", "▀▀█", "▀▀▀"}, '%': {"▀ ▄", "▄▀ ", "  ▀"},
}

// Big draws s ("64%") in bigDigits: three lines of equal width.
func Big(s string) [3]string {
	var out [3]string
	for i, r := range s {
		g, ok := bigDigits[r]
		if !ok {
			continue
		}
		for row := range out {
			if i > 0 {
				out[row] += " "
			}
			out[row] += g[row]
		}
	}
	return out
}
