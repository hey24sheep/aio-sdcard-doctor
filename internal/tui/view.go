package tui

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/app"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/job"
)

// The screen: a header (the card mark, the app, the chosen card), the step track, one framed
// panel with the current step, and the keys at the bottom. Every line fits the terminal.

// The smallest terminal the screens are laid out for.
const minW, minH = 60, 20

type palette struct {
	ink, steel, signal, safe, risk, fail, contact, rule lipgloss.Style
	styled                                              bool // bold/reverse reach the terminal (not NO_COLOR or a pipe)
}

func (m *Model) pal() palette {
	c := func(tok string) lipgloss.Style { return lipgloss.NewStyle().Foreground(Colour(tok)) }
	return palette{ink: c("ink"), steel: c("steel"), signal: c("signal"), safe: c("safe"), risk: c("risk"), fail: c("fail"),
		contact: c("contact"), rule: c("rule"), styled: lipgloss.NewStyle().Bold(true).Render("x") != "x"}
}

func (p palette) tone(t string) lipgloss.Style {
	switch t {
	case "safe":
		return p.safe
	case "risk":
		return p.risk
	case "fail":
		return p.fail
	}
	return lipgloss.NewStyle()
}

var bold = lipgloss.NewStyle().Bold(true)

func (m *Model) sym(fancy, plain string) string {
	if m.opt.ASCII {
		return plain
	}
	return fancy
}

var stepNames = map[app.Step]string{app.StepDrive: "Card", app.StepAction: "What to do", app.StepChoice: "What to copy",
	app.StepDest: "Where", app.StepOptions: "Options", app.StepReview: "Check", app.StepElevate: "Administrator",
	app.StepRun: "Working", app.StepRecoverList: "Pick files", app.StepResults: "Done"}

func gb(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%d KB", (n+1023)/1024)
}

// files is "1 file", "2 files", "1,000 files".
func files(n int) string {
	if n == 1 {
		return "1 file"
	}
	return commas(n) + " files"
}

func commas(n int) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// View draws the screen. Questions (the engine's, "Stop now?", "Leave the list?") take the
// panel's place, so the answer keys and the question are the only things to read.
func (m *Model) View() string {
	if m.w < minW || m.h < minH {
		return m.finish(fmt.Sprintf("Enlarge the terminal to at least %d×%d (now %d×%d).", minW, minH, m.w, m.h))
	}
	p := m.pal()
	m.inner = m.w - 5 // "│  " + text + " │"
	var title, tone string
	var body, tail []string
	hints := m.hints()
	switch {
	case m.leaveAsk != "":
		title, tone, hints = "Leave the list?", "risk", []hint{{"y", "leave"}, {"n", "stay on the list"}}
		body = append([]string{""}, m.para(lipgloss.NewStyle(), "", "The files found are forgotten; finding them again takes as long as it did this time.")...)
	case m.stopAsk:
		title, tone, hints = "Stop now?", "risk", []hint{{"y", "stop"}, {"n", "keep going"}}
		what := "The current block is finished and saved; running the same thing again continues where it stopped."
		if m.v.Req.Action == "fix" && m.v.Req.FixOp != "health-test" { // a system tool can't be stopped half-way safely
			what = "The system tool finishes what it started (stopping it half-way could damage the card); its result is shown when it ends."
		}
		body = append([]string{""}, m.para(lipgloss.NewStyle(), "", what)...)
	case m.v.Issue != nil:
		title, tone, body = m.v.Issue.Issue.Title, "risk", m.question(p)
		hints = []hint{{m.sym("←→", "Left/Right"), "choose"}, {"Enter", "answer"}, {"Esc", "the safe choice"}}
	default:
		body = m.body(p)
		if m.flash != "" { // the problem and busy lines stay on screen however long the page
			tail = append(tail, "")
			tail = append(tail, m.para(p.fail, "", m.sym("✖ ", "x ")+"Problem: "+m.flash)...)
		} else if m.v.Error != "" && m.v.Step != app.StepResults {
			tail = append(tail, "")
			tail = append(tail, m.para(p.fail, "", m.sym("✖ ", "x ")+"Problem: "+m.v.Error)...)
		}
		if m.v.Busy != "" {
			tail = append(tail, "", p.steel.Render(m.v.Busy+"…"))
		}
		if m.help {
			tail = append(tail, "")
			tail = append(tail, m.para(p.steel, "", "↑/↓ or j/k move · Enter choose · Esc back · q quit · ? less help — for a screen reader use: aio-sdcard-doctor --plain")...)
		}
	}
	lines := []string{m.header(p), m.track(p)}
	lines = append(lines, m.panel(p, title, tone, body, tail, m.h-3)...)
	lines = append(lines, m.footer(p, hints))
	return m.finish(strings.Join(lines, "\n"))
}

// finish folds typography for --ascii and cuts every line to the width.
func (m *Model) finish(s string) string {
	if m.opt.ASCII {
		s = asciiFold.Replace(s)
	}
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = m.fit(lines[i], m.w)
	}
	return strings.Join(lines, "\n")
}

func (m *Model) fit(s string, w int) string {
	if lipgloss.Width(s) <= w {
		return s
	}
	return ansi.Truncate(s, w, m.sym("…", "."))
}

func nonEmpty(s []string) []string {
	var out []string
	for _, x := range s {
		if x != "" {
			out = append(out, x)
		}
	}
	return out
}

// window keeps the lines that fit, around the cursor's line.
func window(body []string, room int, cursor int) []string {
	if len(body) <= room {
		return body
	}
	start := max(0, min(cursor-room/2, len(body)-room))
	return body[start : start+room]
}

// header: the card mark and name, the chosen card as a chip, and whether it is only read.
func (m *Model) header(p palette) string {
	h := p.contact.Render(m.sym("▣", "[#]")) + " " + bold.Render("SD card doctor")
	if c := m.v.Card; c != nil {
		bits := []string{c.Label, c.FS}
		if c.Total > 0 {
			bits = append(bits, gb(int64(c.Total)))
		}
		chip := strings.Join(nonEmpty(bits), "  ")
		if p.styled {
			chip = p.signal.Reverse(true).Bold(true).Render(" " + chip + " ")
		} else {
			chip = "[" + chip + "]"
		}
		mode := p.safe.Render(m.sym("● ", "* ") + "Only read")
		if r := m.v.Req; r.Action == "fix" && r.FixOp != "check" { // repair, format, health test write
			mode = p.risk.Render(m.sym("▲ ", "! ") + "Card tools write to the card")
		}
		h += "   " + chip + "  " + mode
	} else {
		h += "   " + p.steel.Render("no card chosen")
	}
	if right := p.steel.Render("? help"); !m.typing && m.field == 0 && lipgloss.Width(h)+2+lipgloss.Width(right) <= m.w {
		h += strings.Repeat(" ", m.w-lipgloss.Width(h)-lipgloss.Width(right)) + right
	}
	return h
}

// track is the step bar: done ✔, current ◉, to come ○ — the symbol says it, not the colour. On a
// narrow terminal: "Step 3 of 8  What to copy" and a row of the same symbols.
func (m *Model) track(p palette) string {
	steps := m.v.Steps
	if len(steps) == 0 {
		steps = []app.Step{app.StepDrive, app.StepAction}
	}
	at := slices.Index(steps, m.v.Step)
	if at < 0 { // a step off the main path (the pick list after the listing): shown after Working
		at = slices.Index(steps, app.StepRun) + 1
		steps = slices.Insert(slices.Clone(steps), at, m.v.Step)
	}
	mark := func(i int) (string, lipgloss.Style) {
		switch {
		case i < at:
			return m.sym("✔", "*"), p.safe
		case i == at:
			return m.sym("◉", "(o)"), p.signal.Bold(true)
		}
		return m.sym("○", "o"), p.steel
	}
	var parts []string
	for i, st := range steps {
		s, style := mark(i)
		label := p.steel.Render(stepNames[st])
		if i == at {
			label = p.signal.Bold(true).Render(stepNames[st])
		} else if i < at {
			label = stepNames[st]
		}
		parts = append(parts, style.Render(s)+" "+label)
	}
	full := strings.Join(parts, p.rule.Render(m.sym(" ── ", " - ")))
	if lipgloss.Width(full) <= m.w {
		return full
	}
	where := p.signal.Bold(true).Render(fmt.Sprintf("Step %d of %d  %s", at+1, len(steps), stepNames[m.v.Step]))
	var dots []string
	for i := range steps {
		s, style := mark(i)
		dots = append(dots, style.Render(s))
	}
	sep := ""
	if m.opt.ASCII {
		sep = " "
	}
	out := where + "   " + strings.Join(dots, sep)
	if at+1 < len(steps) {
		if next := p.steel.Render("   next: " + stepNames[steps[at+1]]); lipgloss.Width(out+next) <= m.w {
			out += next
		}
	}
	return out
}

// panel frames the body in a rounded box, the lines windowed around the cursor; a question gets
// its title in the top edge, in the risk colour.
func (m *Model) panel(p palette, title, tone string, body, tail []string, height int) []string {
	edge := p.rule
	if tone != "" {
		edge = p.tone(tone)
	}
	in := m.inner
	room := height - 2
	body = append(window(body, max(room-len(tail), 1), m.cursorLine), tail...)
	if m.opt.ASCII { // before cutting to the width: "…" becomes "..."
		for i := range body {
			body[i] = asciiFold.Replace(body[i])
		}
	}
	tl, tr, bl, br, hz, vt := "╭", "╮", "╰", "╯", "─", "│"
	if m.opt.ASCII {
		tl, tr, bl, br, hz, vt = "+", "+", "+", "+", "-", "|"
	}
	top := edge.Render(tl + strings.Repeat(hz, in+3) + tr)
	if title != "" {
		t := m.fit(m.sym("▲ ", "! ")+title, in-2)
		top = edge.Render(tl+hz+" ") + edge.Bold(true).Render(t) + edge.Render(" "+strings.Repeat(hz, max(0, in-lipgloss.Width(t)))+tr)
	}
	out := []string{top}
	for i := 0; i < room; i++ {
		l := ""
		if i < len(body) {
			l = m.fit(body[i], in)
		}
		out = append(out, edge.Render(vt)+"  "+l+strings.Repeat(" ", max(0, in-lipgloss.Width(l)))+" "+edge.Render(vt))
	}
	return append(out, edge.Render(bl+strings.Repeat(hz, in+3)+br))
}

// para wraps text to the panel (after indent) and styles each line.
func (m *Model) para(style lipgloss.Style, indent, text string) []string {
	var out []string
	for _, l := range wrapText(text, m.inner-lipgloss.Width(indent)) {
		out = append(out, indent+style.Render(l))
	}
	return out
}

type hint struct{ key, what string }

// hints are the keys of this screen, most important first: a narrow terminal drops the last.
func (m *Model) hints() []hint {
	ud := m.sym("↑↓", "Up/Down")
	switch {
	case m.typing:
		return []hint{{"", "Type"}, {"Enter", "OK"}, {"Esc", "cancel"}}
	case m.field > 0:
		return []hint{{"", "Type"}, {"Tab", "next field"}, {"Enter", "change the card"}, {"Esc", "stop typing"}}
	}
	switch m.v.Step {
	case app.StepDrive:
		return []hint{{ud, "move"}, {"Enter", "choose"}, {"q", "quit"}, {"s", "system drives"}, {"p", "folder or image"}, {"r", "refresh"}, {"?", "help"}}
	case app.StepReview:
		var h []hint
		if m.hasWarnings() { // first: the end of a long line is what a narrow terminal drops
			h = append(h, hint{"a", "accept warnings"})
		}
		if m.v.Confirm != nil {
			h = append(h, hint{"Tab", "type label and size"})
		}
		h = append(h, hint{"Enter", "start"})
		if m.v.Req.Action != "fix" { // card tools have no folder
			h = append(h, hint{"e", "change folder"})
		}
		return append(h, hint{"Esc", "back"}, hint{"q", "quit"})
	case app.StepRun:
		return []hint{{"x", "stop"}, {"Ctrl+C", "stop"}, {"", "the copy keeps going if you just wait"}}
	case app.StepRecoverList:
		return []hint{{ud, "move"}, {"Space", "tick"}, {"Enter", "recover ticked"}, {"a", "tick all"}, {"/", "filter"}, {"n", "start over"}}
	case app.StepResults:
		return []hint{{ud, "move"}, {"Enter", "choose"}, {"n", "start over"}, {"q", "quit"}}
	}
	return []hint{{ud, "move"}, {"Enter", "choose"}, {"Esc", "back"}, {"q", "quit"}, {"?", "help"}}
}

// footer shows the keys as pills (reverse video), or "Enter choose" in plain text without styling.
func (m *Model) footer(p palette, hs []hint) string {
	out := ""
	for _, h := range hs {
		var s string
		switch {
		case h.key == "":
			s = p.steel.Render(h.what)
		case p.styled:
			s = p.steel.Reverse(true).Render(" "+h.key+" ") + " " + h.what
		default:
			s = h.key + " " + h.what
		}
		next := s
		if out != "" {
			next = out + "  " + s
		}
		if lipgloss.Width(next)+1 > m.w {
			break
		}
		out = next
	}
	return " " + out
}

// warnKey names a plan's warnings, to tell when a new check shows different ones.
func warnKey(p *job.Plan) string {
	var k []string
	for _, is := range p.Issues {
		if is.Severity == job.Warn {
			k = append(k, is.Kind+"\x00"+is.Title+"\x00"+is.Detail)
		}
	}
	return strings.Join(k, "\x00")
}

func (m *Model) hasWarnings() bool {
	if m.v.Plan == nil {
		return false
	}
	for _, is := range m.v.Plan.Issues {
		if is.Severity == job.Warn {
			return true
		}
	}
	return false
}

// mark starts a row: a bar for the highlighted one (a symbol, so it shows without colour).
func (m *Model) mark(i int) string {
	if i == m.cur {
		return m.pal().signal.Bold(true).Render(m.sym("▌ ", "> "))
	}
	return "  "
}

// cont starts a row's second line, under its first.
func (m *Model) cont(i int) string {
	if i == m.cur && !m.opt.ASCII {
		return m.pal().signal.Render("▌") + "   "
	}
	return "    "
}

// desc is a row's second line, under its first: wrapped, never cut off.
func (m *Model) desc(i int, style lipgloss.Style, text string) []string {
	var out []string
	for _, l := range wrapText(text, m.inner-4) {
		out = append(out, m.cont(i)+style.Render(l))
	}
	return out
}

// hi is a row's title: bold when highlighted.
func (m *Model) hi(i int, s string) string {
	if i == m.cur {
		return bold.Render(s)
	}
	return s
}

// kv lays out "Key: value" lines with the keys in a column (keys up to 12 wide; longer ones are
// shown as they are). "Warning:" lines get the warning symbol and colour.
func (m *Model) kv(p palette, ls []string) []string {
	keyW := 0
	split := func(l string) (string, string, bool) {
		k, v, ok := strings.Cut(l, ": ")
		if !ok || len([]rune(k)) > 12 {
			return "", l, false
		}
		return k, v, true
	}
	for _, l := range ls {
		if k, _, ok := split(l); ok {
			keyW = max(keyW, len([]rune(k))+1)
		}
	}
	var out []string
	for _, l := range ls {
		k, v, ok := split(l)
		switch {
		case ok && k == "Warning":
			out = append(out, m.para(p.risk, "  ", m.sym("▲ ", "! ")+l)...)
		case ok:
			key := p.steel.Render(fmt.Sprintf("%-*s", keyW, k+":"))
			style := lipgloss.NewStyle()
			if strings.Contains(v, "ERASE EVERYTHING") {
				style = p.fail.Bold(true)
			}
			for j, w := range wrapText(v, m.inner-keyW-3) {
				if j == 0 {
					out = append(out, "  "+key+" "+style.Render(w))
				} else {
					out = append(out, "  "+strings.Repeat(" ", keyW+1)+style.Render(w))
				}
			}
		default:
			out = append(out, m.para(lipgloss.NewStyle(), "  ", l)...)
		}
	}
	return out
}

func (m *Model) body(p palette) []string {
	v := m.v
	var b []string
	h1 := func(s string) { b = append(b, bold.Render(s)) }
	add := func(ls ...string) { b = append(b, ls...) }
	m.cursorLine = 0
	at := func(i int) {
		if i == m.cur {
			m.cursorLine = len(b)
		}
	}
	switch v.Step {
	case app.StepDrive:
		h1("Choose the card")
		add(m.para(p.steel, "", "Nothing on the card is changed unless you choose Card tools later and confirm.")...)
		add("")
		if len(v.Drives) == 0 || !v.Drives[0].Removable {
			add(m.para(lipgloss.NewStyle(), "", p.risk.Render(m.sym("▲ ", "! "))+"Insert the card — it will appear here automatically.")...)
			add("")
		}
		if len(v.Drives) > 0 {
			add(p.steel.Render(fmt.Sprintf("  %-14s %-6s %-6s %9s", "Name", "Drive", "Type", "Size")))
		}
		for i, d := range v.Drives {
			var tags []string
			if d.Removable {
				tags = append(tags, p.safe.Render("Removable"))
			}
			if d.System {
				tags = append(tags, p.risk.Render("System"))
			}
			if d.Locked {
				tags = append(tags, p.fail.Render("Locked"))
			}
			if d.WriteProtected {
				tags = append(tags, "Write-protected")
			}
			if d.FS == app.KindNotResponding {
				tags = append(tags, p.fail.Render("Not responding"))
			} else if d.RAW {
				tags = append(tags, p.risk.Render("File system not readable"))
			}
			at(i)
			row := m.hi(i, fmt.Sprintf("%-14s %-6s %-6s %9s", d.Label, d.Root, d.FS, gb(int64(d.Total))))
			add(m.mark(i) + row + "  " + strings.Join(tags, "  "))
		}
		if v.HiddenSystem > 0 {
			add("", p.steel.Render(fmt.Sprintf("%d system drive hidden — press s to show", v.HiddenSystem)))
		}
		if m.typing {
			add("", "Folder or image file: "+m.input(p, m.text))
		} else {
			add("", p.steel.Render("Press p to use a folder or a card image file instead."))
		}
	case app.StepAction:
		h1("What do you want to do?")
		add("")
		for i, it := range m.actionList() {
			at(i)
			switch {
			case it.quick:
				add(m.mark(i) + p.signal.Bold(true).Render("Copy everything off this card"))
				add(m.desc(i, p.steel, "Every file, nothing skipped — you'll check the details before it starts")...)
				add("")
			case !it.row.Enabled:
				add(m.mark(i) + p.steel.Render(it.row.Label+" (not available)"))
				add(m.desc(i, p.risk, it.row.Reason)...)
			case it.row.Danger:
				add(p.rule.Render(strings.Repeat(m.sym("─", "-"), min(m.inner, 40))))
				if i == m.cur {
					m.cursorLine = len(b)
				}
				add(m.mark(i) + p.fail.Bold(true).Render(m.sym("▲ ", "! ")+it.row.Label))
				add(m.desc(i, p.steel, it.row.Desc)...)
			default:
				add(m.mark(i) + m.hi(i, it.row.Label))
				add(m.desc(i, p.steel, it.row.Desc)...)
			}
		}
	case app.StepChoice:
		h1("What should be copied?")
		add(m.para(p.steel, "", "Files left out are still listed in the report, with the reason.")...)
		add("")
		for i, c := range v.Choices {
			at(i)
			add(m.mark(i) + m.hi(i, fmt.Sprintf("%-16s", c.Title)) + fmt.Sprintf(" %s · %s", files(c.Files), gb(c.Bytes)))
			add(m.desc(i, p.steel, c.Desc)...)
		}
	case app.StepDest:
		if v.Req.Action == "image" {
			h1("Where should the image file go?")
		} else {
			h1("Where should the files go?")
		}
		if v.Req.Action == "image" { // one file, as the GUI asks
			add(p.steel.Render(`Choose a file name on another disk, e.g. D:\card.img.`), "", "Image file: "+m.input(p, m.text))
		} else {
			add(p.steel.Render("A folder on another disk — never on the card itself."), "", "Folder: "+m.input(p, m.text))
		}
	case app.StepOptions:
		h1("Options")
		add("")
		for i, o := range m.optionList() {
			at(i)
			switch {
			case o.name == "continue":
				add("")
				if i == m.cur {
					m.cursorLine = len(b)
				}
				add(m.mark(i) + p.signal.Bold(true).Render("Continue"))
			case o.text:
				val := o.value
				if m.typing && m.item == i {
					val = m.input(p, m.text)
				}
				add(m.mark(i) + m.hi(i, o.label+":") + " " + val)
			default:
				box := "[ ] "
				if o.radio {
					box = "( ) "
					if o.on {
						box = m.sym("(•) ", "(*) ")
					}
				} else if o.on {
					box = "[x] "
				}
				if o.off && !o.on {
					add(m.mark(i) + p.steel.Render(box+o.label+" (not available)"))
					add(m.desc(i, p.risk, o.why)...)
					break
				}
				line := m.mark(i) + box + m.hi(i, o.label)
				if o.hint != "" && lipgloss.Width(line+"  "+o.hint) <= m.inner {
					add(line + "  " + p.steel.Render(o.hint))
				} else {
					add(line)
					if o.hint != "" { // no room beside it: underneath, never cut off
						add(m.desc(i, p.steel, o.hint)...)
					}
				}
				if o.off {
					add(m.desc(i, p.risk, o.why)...)
				}
			}
		}
	case app.StepReview:
		h1("Please check before it starts")
		if v.Plan == nil {
			break
		}
		add("")
		add(m.kv(p, v.Plan.Summary)...)
		if len(v.Plan.Result) > 0 {
			add("", bold.Render("You'll get"))
			for _, l := range v.Plan.Result {
				add(m.para(lipgloss.NewStyle(), "  ", l)...)
			}
		}
		warns := 0
		for _, is := range v.Plan.Issues {
			switch {
			case is.Severity == job.Block && is.Kind != "needs-admin":
				add("", p.fail.Bold(true).Render(m.sym("✖ ", "x ")+"Can't start: ")+is.Title)
				add(m.para(lipgloss.NewStyle(), "    ", is.Detail)...)
			case is.Severity == job.Warn:
				warns++
				add("", p.risk.Bold(true).Render(m.sym("▲ ", "! ")+"Warning: ")+is.Title)
				add(m.para(lipgloss.NewStyle(), "    ", is.Detail)...)
			default:
				add("", p.steel.Render("Note: ")+is.Title)
			}
		}
		if v.CanStartOver {
			box := "[ ]"
			if v.Req.Fresh {
				box = "[x]"
			}
			add("", box+" Start over instead of continuing where it stopped "+p.steel.Render("(press o)"))
		}
		if warns > 0 {
			box := "[ ]"
			if m.accept {
				box = "[x]"
			}
			add("", box+" I've read the warnings and want to go ahead "+p.steel.Render("(press a)"))
		}
		if c := v.Confirm; c != nil {
			field := func(f int, typed string) string {
				if m.field == f {
					return m.input(p, typed)
				}
				return typed
			}
			add("")
			add(m.para(p.fail.Bold(true), "", m.sym("▲ ", "! ")+"Type the card's label and size to confirm — this changes the card")...)
			add(fmt.Sprintf("  Label (%s): %s", c.Label, field(1, m.label)),
				fmt.Sprintf("  Size (%s): %s", c.Size, field(2, m.size)))
		}
	case app.StepElevate:
		h1("This needs administrator rights")
		add("Reading the card directly needs them. Quit and run:", "", "  "+bold.Render(v.Instruction))
	case app.StepRun:
		b = m.runBody(p, b)
	case app.StepRecoverList:
		h1("Pick the files to bring back")
		rv := v.Recover
		if rv == nil {
			break
		}
		shown := commas(rv.Shown) + " match"
		if rv.Listed < rv.Shown { // only a page is listed: say so, and how to reach the rest
			shown += fmt.Sprintf(" (first %s listed — / to filter, a ticks all %s)", commas(rv.Listed), commas(rv.Shown))
		}
		add(m.para(p.steel, "", fmt.Sprintf("%s found · %s · %s ticked · unrecoverable files can't be ticked", commas(rv.Total), shown, commas(rv.Ticked)))...)
		if m.typing {
			add("Filter: " + m.input(p, m.text))
		}
		add("")
		for i, r := range rv.Rows {
			at(i)
			box := "[ ]"
			if r.Ticked {
				box = "[x]"
			}
			if !r.Tickable {
				box = " - "
			}
			pathW := max(12, min(40, m.inner-32)) // the size column stays in view; a long path is cut in the middle
			row := fmt.Sprintf("%s%s %s %s %9s", m.mark(i), box, m.state(p, r.State), m.hi(i, fmt.Sprintf("%-*s", pathW, middle(r.Path, pathW))), gb(r.Size))
			switch {
			case r.Comment == "":
				add(row)
			case lipgloss.Width(row)+2+lipgloss.Width(r.Comment) <= m.inner:
				add(row + "  " + p.steel.Render(r.Comment))
			default:
				add(row)
				add(m.desc(i, p.steel, r.Comment)...)
			}
		}
	case app.StepResults:
		b = m.resultsBody(p, b)
	}
	return b
}

// input is typed text with its cursor.
func (m *Model) input(p palette, s string) string {
	return bold.Render(s) + p.signal.Render(m.sym("▏", "_"))
}

// runBody is the live run: the percentage large, files safe, the bar, speed with its recent
// history, time left and the file being read.
func (m *Model) runBody(p palette, b []string) []string {
	v := m.v
	b = append(b, bold.Render(nonEmptyOr(v.RunLabel, "Working")))
	pr := v.Progress
	if pr == nil {
		pr = &job.Progress{}
	}
	safe := pr.Counters["OK"] + pr.Counters["ALREADY-COPIED"] + pr.Counters["DELETED-RECOVERED"] + pr.Counters["CARVED"]
	frac, pct := 0.0, 0
	if pr.Total > 0 {
		frac = float64(pr.Done) / float64(pr.Total)
		pct = int(100 * pr.Done / pr.Total)
	}
	hero := fmt.Sprintf("%s files safe so far", commas(safe))
	if v.Plan != nil && v.Plan.Items > 0 {
		hero = fmt.Sprintf("%s of %s files safe so far", commas(safe), commas(v.Plan.Items))
	}
	info := []string{bold.Render(hero), fmt.Sprintf("%s of %s", gb(pr.Done), gb(pr.Total)), p.steel.Render(TimeLeft(pr.Total-pr.Done, pr.BPS))}
	b = append(b, "")
	if !m.opt.ASCII && m.inner >= 50 {
		big := Big(fmt.Sprintf("%d%%", pct))
		for i, row := range big {
			b = append(b, p.signal.Bold(true).Render(row)+"    "+info[i])
		}
	} else {
		b = append(b, bold.Render(fmt.Sprintf("%d%%", pct))+"  "+info[0], info[1], info[2])
	}
	b = append(b, "")
	barW := m.inner - 6
	done, rest := Bar(frac, barW, m.opt.ASCII)
	b = append(b, p.signal.Render(done)+p.rule.Render(rest)+fmt.Sprintf(" %3d%%", pct), "")
	if pr.BPS > 0 {
		speed := fmt.Sprintf("%.1f MB/s", pr.BPS/1e6)
		line := p.steel.Render("Speed  ") + fmt.Sprintf("%-11s", speed)
		if sp := Spark(m.speeds, m.inner-lipgloss.Width(line)-2, m.opt.ASCII); len(m.speeds) > 1 && sp != "" {
			line += " " + p.signal.Render(sp)
		}
		b = append(b, line)
	}
	if pr.Item != "" {
		b = append(b, p.steel.Render("Now    ")+middle(pr.Item, m.inner-7))
	}
	if pr.Note != "" {
		b = append(b, m.para(p.risk, "", m.sym("▲ ", "! ")+pr.Note)...)
	}
	if n := len(v.Logs); n > 0 {
		b = append(b, "")
		for _, l := range v.Logs[max(0, n-6):] {
			b = append(b, m.para(p.steel, "", l)...)
		}
	}
	return b
}

// resultsBody: the verdict (symbol, word, colour), the counts with the good news first, the
// report, then what to do next.
func (m *Model) resultsBody(p palette, b []string) []string {
	v := m.v
	res := v.Result
	title, tone := "Done", "safe"
	switch {
	case v.ImageInstead:
		title, tone = "Stopped to make a full backup image instead", "risk"
	case v.Error != "":
		title, tone = "Stopped", "fail"
	case res != nil && !res.Clean:
		title, tone = "Done, with problems", "risk"
	}
	b = append(b, p.tone(tone).Bold(true).Render(Symbol(tone, m.opt.ASCII)+" "+title))
	if v.Error != "" {
		b = append(b, m.para(p.fail, "", m.sym("✖ ", "x ")+v.Error)...)
	}
	if res != nil {
		if a := v.Req.Action; (a == "rescue" || a == "recover") && len(res.Counters) > 0 {
			safe := res.Counters["OK"] + res.Counters["ALREADY-COPIED"] + res.Counters["DELETED-RECOVERED"] + res.Counters["CARVED"]
			line := files(safe) + " safe"
			if v.Plan != nil && v.Plan.Items > 0 {
				line = fmt.Sprintf("%s of %s safe", commas(safe), files(v.Plan.Items))
			}
			b = append(b, bold.Render(line))
		}
		var keys []string
		for k := range res.Counters {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool {
			oi, oj := ToneOrder(Tone(keys[i])), ToneOrder(Tone(keys[j]))
			return oi < oj || oi == oj && keys[i] < keys[j]
		})
		b = append(b, "")
		for _, k := range keys {
			t := Tone(k)
			s := Symbol(t, m.opt.ASCII)
			if s == "" {
				s = " "
			}
			b = append(b, "  "+p.tone(t).Render(s+" "+fmt.Sprintf("%-22s", k))+fmt.Sprintf(" %10s", commas(res.Counters[k])))
		}
		for _, r := range res.Reports {
			if r != "" {
				b = append(b, "")
				b = append(b, m.para(lipgloss.NewStyle(), "", p.steel.Render("Report  ")+r)...)
			}
		}
	}
	if len(v.Logs) > 0 {
		b = append(b, "")
	}
	for _, l := range v.Logs {
		b = append(b, m.para(p.steel, "", l)...)
	}
	b = append(b, "", bold.Render("Next"))
	for i, n := range v.Next {
		if i == m.cur {
			m.cursorLine = len(b)
		}
		b = append(b, m.mark(i)+p.signal.Bold(true).Render(n.Label))
		b = append(b, m.desc(i, p.steel, n.Why)...)
	}
	if m.cur == len(v.Next) {
		m.cursorLine = len(b)
	}
	return append(b, m.mark(len(v.Next))+m.hi(len(v.Next), "Start over"))
}

func nonEmptyOr(s, alt string) string {
	if s == "" {
		return alt
	}
	return s
}

func (m *Model) state(p palette, st string) string {
	switch st {
	case "Excellent":
		return p.safe.Render(m.sym("●", "*") + " Excellent    ")
	case "Poor":
		return p.risk.Render(m.sym("▲", "!") + " Poor         ")
	}
	return p.steel.Render(m.sym("○", "o") + " Unrecoverable")
}

// middle shortens a path in the middle, keeping its start and its file name.
func middle(s string, w int) string {
	r := []rune(s)
	if len(r) <= w || w < 10 {
		return s
	}
	half := (w - 1) / 2
	return string(r[:half]) + "…" + string(r[len(r)-(w-1-half):])
}

// question is the engine's question in the panel: what happened, then the choices as buttons
// (the chosen one in brackets and reverse video), side by side or, when they don't fit, stacked.
func (m *Model) question(p palette) []string {
	iv := m.v.Issue
	b := []string{""}
	if iv.Issue.Detail != "" {
		b = append(b, m.para(lipgloss.NewStyle(), "", iv.Issue.Detail)...)
		b = append(b, "")
	}
	var btns []string
	for i, c := range iv.Issue.Choices {
		l := " " + choiceLabel(c) + " "
		if i == m.issueSel {
			l = lipgloss.NewStyle().Reverse(true).Bold(true).Render("[" + choiceLabel(c) + "]")
		}
		btns = append(btns, l)
	}
	if row := strings.Join(btns, "  "); lipgloss.Width(row) <= m.inner {
		return append(b, row)
	}
	return append(b, btns...)
}

var choiceLabels = map[job.Choice]string{job.Continue: "Continue", job.Stop: "Stop", job.Retry: "Try again", job.Wait: "Wait for the card",
	"skip-rest": "Skip the rest (report them failed)", job.Skip: "Skip this file", job.Resume: "Continue where it stopped", job.New: "Start over",
	job.Elevate: "Restart as administrator", "skip-videos": "Skip videos", job.Image: "Make a full backup image instead"}

func choiceLabel(c job.Choice) string {
	if l, ok := choiceLabels[c]; ok {
		return l
	}
	return string(c)
}
