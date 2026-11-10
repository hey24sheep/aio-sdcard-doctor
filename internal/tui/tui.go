// Package tui is the keyboard-driven terminal UI: a Bubble Tea renderer of app.Session, so it
// behaves exactly like the GUI. Status is never colour alone (words and symbols, ASCII with
// --ascii), NO_COLOR is respected, and every line is cut to the terminal width.
package tui

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/app"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/job"
)

// Options change how the TUI looks.
type Options struct {
	ASCII       bool // no Unicode symbols or box characters
	NoAnimation bool
	NoBell      bool
}

// Model is the Bubble Tea model.
type Model struct {
	s   *app.Session
	v   app.View
	opt Options

	w, h     int
	cur      int
	lastStep app.Step
	text     string // the text being typed on this step (path, folder, filter)
	typing   bool
	item     int // options: which text option is being typed
	flash    string
	accept   bool
	label    string
	size     string
	field    int // 0 none, 1 label, 2 size (card tools confirmation)
	stopAsk  bool
	issueSel int
	help     bool
	bellFor  string

	cursorLine int // set while drawing: the line of the highlighted row (for scrolling)
}

type viewMsg struct{}
type tickMsg struct{}

// New makes the model for a session.
func New(s *app.Session, opt Options) *Model {
	m := &Model{s: s, opt: opt, w: 80, h: 24}
	m.v = cleanView(s.View())
	m.lastStep = m.v.Step
	return m
}

// Run starts the TUI on the terminal and returns when the user quits.
func Run(s *app.Session, opt Options) error {
	m := New(s, opt)
	p := tea.NewProgram(m, tea.WithAltScreen())
	go func() {
		for range s.Events() {
			p.Send(viewMsg{})
		}
	}()
	_, err := p.Run()
	return err
}

// Init starts the card-list refresh.
func (m *Model) Init() tea.Cmd { return tick() }

func tick() tea.Cmd { return tea.Tick(2*time.Second, func(time.Time) tea.Msg { return tickMsg{} }) }

func (m *Model) do(name string, kv ...string) bool {
	args := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		args[kv[i]] = kv[i+1]
	}
	err := m.s.Do(app.Command{Name: name, Args: args})
	m.refresh()
	if err != nil {
		m.flash = cleanStr(err.Error())
		return false
	}
	m.flash = ""
	return true
}

func (m *Model) refresh() {
	m.v = cleanView(m.s.View()) // card text can't break the screen or reach the terminal
	if m.v.Step != m.lastStep {
		m.lastStep = m.v.Step
		m.cur, m.typing, m.accept, m.field, m.label, m.size, m.stopAsk = 0, false, false, 0, "", "", false
		m.text = ""
		if m.v.Step == app.StepDest {
			m.text, m.typing = m.v.Req.Dest, true
		}
		if m.v.Step == app.StepRecoverList && m.v.Recover != nil {
			m.text = m.v.Recover.Filter
		}
	}
	if m.v.Issue == nil {
		m.issueSel = 0
	}
}

// Update handles keys, window sizes and session changes.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
	case viewMsg:
		before := m.v.Step
		m.refresh()
		return m, m.bell(before)
	case tickMsg:
		if m.v.Step == app.StepDrive && !m.typing {
			s := m.s
			return m, tea.Batch(tick(), func() tea.Msg { s.Do(app.Command{Name: "refresh"}); return viewMsg{} })
		}
		return m, tick()
	case tea.KeyMsg:
		return m, m.key(msg)
	}
	return m, nil
}

// bell rings once when a run finishes or a question needs an answer.
func (m *Model) bell(before app.Step) tea.Cmd {
	if m.opt.NoBell {
		return nil
	}
	why := ""
	switch {
	case m.v.Issue != nil:
		why = "issue " + m.v.Issue.ID
	case before == app.StepRun && m.v.Step != app.StepRun:
		why = "done"
	}
	if why == "" || why == m.bellFor {
		return nil
	}
	m.bellFor = why
	return func() tea.Msg { os.Stderr.WriteString("\a"); return nil }
}

func (m *Model) items() int {
	switch m.v.Step {
	case app.StepDrive:
		return len(m.v.Drives)
	case app.StepAction:
		return len(m.actionList())
	case app.StepChoice:
		return len(m.v.Choices)
	case app.StepOptions:
		return len(m.optionList())
	case app.StepRecoverList:
		if m.v.Recover != nil {
			return len(m.v.Recover.Rows)
		}
	case app.StepResults:
		return len(m.v.Next) + 1
	}
	return 0
}

func (m *Model) key(k tea.KeyMsg) tea.Cmd {
	s := k.String()
	// questions from the engine come first
	if iv := m.v.Issue; iv != nil {
		ch := iv.Issue.Choices
		switch s {
		case "left", "shift+tab":
			m.issueSel = (m.issueSel + len(ch) - 1) % len(ch)
		case "right", "tab":
			m.issueSel = (m.issueSel + 1) % len(ch)
		case "enter":
			m.do("answer", "id", iv.ID, "choice", string(ch[m.issueSel]))
		case "esc": // Esc is the safe answer
			safe := iv.Issue.Default
			for _, c := range ch {
				if c == job.Stop {
					safe = c
				}
			}
			m.do("answer", "id", iv.ID, "choice", string(safe))
		}
		return nil
	}
	if m.stopAsk {
		switch s {
		case "y", "Y":
			m.stopAsk = false
			m.do("cancel")
		case "n", "N", "esc":
			m.stopAsk = false
		}
		return nil
	}
	if s == "ctrl+c" {
		if m.v.Step == app.StepRun {
			m.stopAsk = true
			return nil
		}
		return tea.Quit
	}
	if m.typing || m.field > 0 {
		return m.typeKey(k)
	}
	switch s {
	case "q":
		if m.v.Step != app.StepRun {
			return tea.Quit
		}
	case "?":
		m.help = !m.help
		return nil
	case "up", "k":
		if m.cur > 0 {
			m.cur--
		}
		return nil
	case "down", "j":
		if m.cur < m.items()-1 {
			m.cur++
		}
		return nil
	case "esc", "left", "backspace":
		if m.v.Step != app.StepRun && m.v.Step != app.StepResults {
			m.do("back")
		}
		return nil
	}
	return m.stepKey(s)
}

// typeKey edits the focused text (path, folder, filter, label, size).
func (m *Model) typeKey(k tea.KeyMsg) tea.Cmd {
	target := &m.text
	switch m.field {
	case 1:
		target = &m.label
	case 2:
		target = &m.size
	}
	switch k.Type {
	case tea.KeyRunes, tea.KeySpace:
		*target += cleanStr(string(k.Runes))
	case tea.KeyBackspace:
		if r := []rune(*target); len(r) > 0 {
			*target = string(r[:len(r)-1])
		}
	case tea.KeyTab:
		if m.v.Confirm != nil {
			m.field = (m.field + 1) % 3
		}
	case tea.KeyEsc:
		switch {
		case m.field > 0:
			m.field = 0
		case m.v.Step == app.StepDest:
			m.do("back")
		default:
			m.typing = false
		}
	case tea.KeyEnter:
		return m.submitText()
	}
	return nil
}

func (m *Model) submitText() tea.Cmd {
	switch {
	case m.field > 0:
		m.field = 0
		return m.start()
	case m.v.Step == app.StepDrive:
		if m.do("select-path", "path", strings.TrimSpace(m.text)) {
			m.typing = false
		}
	case m.v.Step == app.StepDest:
		m.do("set-dest", "path", strings.TrimSpace(m.text))
	case m.v.Step == app.StepOptions:
		opts := m.optionList()
		m.typing = false
		m.do("set-option", "name", opts[m.item].name, "value", strings.TrimSpace(m.text))
	case m.v.Step == app.StepRecoverList:
		m.typing = false
		m.do("recover-filter", "text", m.text)
	}
	return nil
}

func (m *Model) start() tea.Cmd {
	args := []string{"label", m.label, "size", m.size}
	if m.accept {
		args = append(args, "accept", "1")
	}
	m.do("start", args...)
	return nil
}

type actionItem struct {
	quick bool
	row   app.ActionRow
}

func (m *Model) actionList() []actionItem {
	var out []actionItem
	for _, a := range m.v.Actions {
		if a.Name == "rescue" && a.Enabled {
			out = append(out, actionItem{quick: true})
		}
	}
	for _, a := range m.v.Actions {
		if !a.Danger {
			out = append(out, actionItem{row: a})
		}
	}
	for _, a := range m.v.Actions {
		if a.Danger {
			out = append(out, actionItem{row: a})
		}
	}
	return out
}

type option struct {
	name, label, hint string
	on, text, radio   bool
	value             string
	off               bool   // can't be changed here
	why               string // why, shown under it
}

func (m *Model) optionList() []option {
	r := m.v.Req
	var o []option
	switch r.Action {
	case "rescue":
		o = []option{{name: "compress", label: "Compress (max) into ZIP files", hint: "saves little on photos and videos", on: r.Compress},
			{name: "skip-empty", label: "Leave out empty (0-byte) files", hint: "still listed in the report", on: r.SkipEmpty}}
	case "image":
		o = []option{{name: "compress", label: "Compress the image (card.img.xz) when finished", on: r.Compress}}
	case "recover":
		deep := option{name: "deep", label: "Deep scan", hint: "slower: also finds files whose names are gone", on: r.Deep}
		if u := m.v.Undelete; u != nil { // the only way to find deleted files on this card
			deep.off, deep.why = true, "Undelete isn't available here ("+u.Reason+") — deep scan stays on"
		}
		o = []option{deep,
			{name: "thorough", label: "Thorough", hint: "much slower: every byte, not only sector starts", on: r.Thorough},
			{name: "compress", label: "Compress into ZIP files", on: r.Compress}}
	case "verify":
		o = []option{{name: "manifest", label: "The rescue to check (Reports/manifest.json)", text: true, value: r.Manifest},
			{name: "target", label: "Where the files are now (empty = the rescue folder)", text: true, value: r.Target}}
	case "fix":
		for _, t := range m.v.Tools { // unavailable ones stay listed, greyed, with why
			o = append(o, option{name: "fix-op", value: t.Name, label: t.Label, hint: t.Desc, radio: true, on: r.FixOp == t.Name,
				off: !t.Enabled, why: t.Reason})
		}
	}
	return append(o, option{name: "continue", label: "Continue"})
}

func (m *Model) stepKey(s string) tea.Cmd {
	v := m.v
	switch v.Step {
	case app.StepDrive:
		switch s {
		case "enter":
			if m.cur < len(v.Drives) {
				m.do("select-drive", "root", v.Drives[m.cur].Root)
			}
		case "s":
			m.do("show-system")
		case "r":
			m.do("refresh")
		case "p", "/":
			m.typing, m.text = true, ""
		}
	case app.StepAction:
		if s != "enter" {
			return nil
		}
		items := m.actionList()
		if m.cur >= len(items) {
			return nil
		}
		it := items[m.cur]
		switch {
		case it.quick:
			m.do("quick-rescue")
		case !it.row.Enabled:
			m.flash = it.row.Label + ": " + it.row.Reason
		default:
			m.do("choose-action", "action", it.row.Name)
		}
	case app.StepChoice:
		if s == "enter" && m.cur < len(v.Choices) {
			m.do("set-choice", "id", v.Choices[m.cur].ID)
		}
	case app.StepOptions:
		opts := m.optionList()
		o := opts[min(m.cur, len(opts)-1)]
		if s != "enter" && s != " " {
			return nil
		}
		switch {
		case o.off:
			m.flash = o.label + ": " + o.why
		case o.name == "continue":
			if s == "enter" {
				m.do("next")
			}
		case o.radio:
			m.do("set-option", "name", o.name, "value", o.value)
		case o.text:
			m.typing, m.item, m.text = true, m.cur, o.value
		default:
			val := "1"
			if o.on {
				val = "0"
			}
			m.do("set-option", "name", o.name, "value", val)
		}
	case app.StepReview:
		switch s {
		case "a":
			m.accept = !m.accept
		case "e":
			m.do("edit-dest")
		case "o":
			if v.CanStartOver {
				val := "1"
				if v.Req.Fresh {
					val = "0"
				}
				m.do("set-option", "name", "fresh", "value", val)
			}
		case "tab":
			if v.Confirm != nil {
				m.field = 1
			}
		case "enter":
			return m.start()
		}
	case app.StepRun:
		if s == "x" {
			m.stopAsk = true
		}
	case app.StepRecoverList:
		rows := []app.RecoverRow{}
		if v.Recover != nil {
			rows = v.Recover.Rows
		}
		switch s {
		case " ":
			if m.cur < len(rows) {
				m.do("recover-tick", "id", rows[m.cur].ID)
			}
		case "a":
			m.do("recover-tick-all")
		case "/":
			m.typing = true
		case "enter":
			m.do("recover-start")
		}
	case app.StepResults:
		if s == "enter" {
			if m.cur < len(v.Next) {
				m.do("next-action", "action", v.Next[m.cur].Action)
			} else {
				m.do("restart")
			}
		}
		if s == "n" {
			m.do("restart")
		}
	}
	return nil
}

// ---- view --------------------------------------------------------------------------------------

type palette struct{ ink, steel, signal, safe, risk, fail lipgloss.Style }

func (m *Model) pal() palette {
	c := func(tok string) lipgloss.Style {
		t := app.Theme[tok]
		return lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: t[0], Dark: t[1]})
	}
	return palette{ink: c("ink"), steel: c("steel"), signal: c("signal"), safe: c("safe"), risk: c("risk"), fail: c("fail")}
}

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

func commas(n int) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// View draws the screen: header, steps, the current step, help bar. Every line fits the width.
func (m *Model) View() string {
	if m.w < 80 || m.h < 24 {
		return fit("Enlarge the terminal to at least 80×24 (now "+fmt.Sprintf("%d×%d", m.w, m.h)+").", m.w)
	}
	p := m.pal()
	bold := lipgloss.NewStyle().Bold(true)
	head := bold.Render("SD card doctor")
	if c := m.v.Card; c != nil {
		bits := []string{c.Label, c.FS}
		if c.Total > 0 {
			bits = append(bits, gb(int64(c.Total)))
		}
		head += "  " + strings.Join(nonEmpty(bits), " · ") + "  " + p.safe.Render(m.sym("● ", "* ")+"Only read")
	} else {
		head += "  " + p.steel.Render("no card chosen")
	}
	steps := m.v.Steps
	if len(steps) == 0 {
		steps = []app.Step{app.StepDrive, app.StepAction}
	}
	var crumbs []string
	for i, st := range steps {
		label := fmt.Sprintf("%d %s", i+1, stepNames[st])
		if st == m.v.Step {
			label = p.signal.Bold(true).Render(label)
		} else {
			label = p.steel.Render(label)
		}
		crumbs = append(crumbs, label)
	}
	body := m.body(p)
	if m.flash != "" {
		body = append(body, "", p.fail.Render(m.sym("✖ ", "x ")+"Problem: ")+m.flash)
	} else if m.v.Error != "" && m.v.Step != app.StepResults {
		body = append(body, "", p.fail.Render(m.sym("✖ ", "x ")+"Problem: ")+m.v.Error)
	}
	if m.v.Busy != "" {
		body = append(body, "", p.steel.Render(m.v.Busy+"…"))
	}
	foot := p.steel.Render(m.helpBar())
	if m.help {
		foot = p.steel.Render("↑/↓ or j/k move · Enter choose · Esc back · q quit · ? less help — for a screen reader use: aio-sdcard-doctor --plain")
	}
	// fit: header, steps, blank, body (windowed around the cursor), blank, help
	room := m.h - 5
	body = window(body, room, m.cursorLine)
	trail := strings.Join(crumbs, p.steel.Render(m.sym(" › ", " > ")))
	if lipgloss.Width(trail) > m.w { // too long for this width: say where we are in words
		at := 0
		for i, st := range steps {
			if st == m.v.Step {
				at = i
			}
		}
		trail = p.signal.Bold(true).Render(fmt.Sprintf("Step %d of %d · %s", at+1, len(steps), stepNames[m.v.Step]))
		if at+1 < len(steps) {
			trail += p.steel.Render(" · next: " + stepNames[steps[at+1]])
		}
	}
	lines := append([]string{head, trail, ""}, body...)
	for len(lines) < m.h-1 {
		lines = append(lines, "")
	}
	lines = append(lines[:m.h-1], foot)
	for i := range lines {
		lines[i] = fit(lines[i], m.w)
	}
	view := strings.Join(lines, "\n")
	if m.v.Issue != nil {
		view = m.overlay(p)
	}
	if m.stopAsk {
		view = m.stopOverlay(p)
	}
	return view
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

func fit(s string, w int) string {
	if lipgloss.Width(s) <= w {
		return s
	}
	return ansi.Truncate(s, w, "…")
}

// window keeps the lines that fit, around the cursor's line.
func window(body []string, room int, cursor int) []string {
	if len(body) <= room {
		return body
	}
	start := max(0, min(cursor-room/2, len(body)-room))
	return body[start : start+room]
}

func (m *Model) helpBar() string {
	switch {
	case m.typing:
		return "type · Enter OK · Esc cancel"
	case m.field > 0:
		return "type · Tab next field · Enter change the card · Esc stop typing"
	}
	switch m.v.Step {
	case app.StepDrive:
		return "↑/↓ move · Enter choose · s system · p folder/image · r refresh · q quit · ? help"
	case app.StepReview:
		h := "Enter start · e change folder · Esc back · q quit"
		if m.v.Confirm != nil {
			h = "Tab type label and size · " + h
		}
		if m.hasWarnings() {
			h += " · a accept warnings"
		}
		return h
	case app.StepRun:
		return "Ctrl+C or x stop · the copy keeps going if you just wait"
	case app.StepRecoverList:
		return "↑/↓ move · Space tick · a tick all · / filter · Enter recover ticked · Esc back"
	case app.StepResults:
		return "Enter choose · n start over · q quit"
	}
	return "↑/↓ move · Enter choose · Esc back · q quit · ? help"
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

func (m *Model) mark(i int) string {
	if i == m.cur {
		return m.sym("› ", "> ")
	}
	return "  "
}

func (m *Model) body(p palette) []string {
	v := m.v
	var b []string
	h1 := func(s string) { b = append(b, lipgloss.NewStyle().Bold(true).Render(s)) }
	m.cursorLine = 0
	switch v.Step {
	case app.StepDrive:
		h1("Choose the card")
		b = append(b, p.steel.Render("Nothing on the card is changed unless you choose Card tools later and confirm."), "")
		if len(v.Drives) == 0 || !v.Drives[0].Removable {
			b = append(b, p.risk.Render(m.sym("▲ ", "! "))+"Insert the card — it will appear here automatically.", "")
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
			if d.RAW {
				tags = append(tags, p.risk.Render("File system not readable"))
			}
			if i == m.cur {
				m.cursorLine = len(b)
			}
			b = append(b, fmt.Sprintf("%s%-14s %-6s %-6s %9s  %s", m.mark(i), d.Label, d.Root, d.FS, gb(int64(d.Total)), strings.Join(tags, " ")))
		}
		if v.HiddenSystem > 0 {
			b = append(b, "", p.steel.Render(fmt.Sprintf("%d system drive hidden — press s to show", v.HiddenSystem)))
		}
		if m.typing {
			b = append(b, "", "Folder or image file: "+m.text+m.sym("▏", "_"))
		} else {
			b = append(b, "", p.steel.Render("Press p to use a folder or a card image file instead."))
		}
	case app.StepAction:
		h1("What do you want to do?")
		b = append(b, "")
		for i, it := range m.actionList() {
			if i == m.cur {
				m.cursorLine = len(b)
			}
			switch {
			case it.quick:
				b = append(b, m.mark(i)+p.signal.Bold(true).Render("Copy everything off this card"), "    "+p.steel.Render("Every file, nothing skipped — you'll check the details before it starts"), "")
			case !it.row.Enabled:
				b = append(b, m.mark(i)+p.steel.Render(it.row.Label+" (not available)"), "    "+p.risk.Render(it.row.Reason))
			case it.row.Danger:
				b = append(b, "", m.mark(i)+p.fail.Bold(true).Render(it.row.Label), "    "+p.steel.Render(it.row.Desc))
			default:
				b = append(b, m.mark(i)+it.row.Label, "    "+p.steel.Render(it.row.Desc))
			}
		}
	case app.StepChoice:
		h1("What should be copied?")
		b = append(b, p.steel.Render("Files left out are still listed in the report, with the reason."), "")
		for i, c := range v.Choices {
			if i == m.cur {
				m.cursorLine = len(b)
			}
			b = append(b, fmt.Sprintf("%s%-16s %s files · %s", m.mark(i), c.Title, commas(c.Files), gb(c.Bytes)), "    "+p.steel.Render(c.Desc))
		}
	case app.StepDest:
		if v.Req.Action == "image" {
			h1("Where should the image file go?")
		} else {
			h1("Where should the files go?")
		}
		b = append(b, p.steel.Render("A folder on another disk — never on the card itself."), "", "Folder: "+m.text+m.sym("▏", "_"))
	case app.StepOptions:
		h1("Options")
		b = append(b, "")
		for i, o := range m.optionList() {
			if i == m.cur {
				m.cursorLine = len(b)
			}
			switch {
			case o.name == "continue":
				b = append(b, "", m.mark(i)+p.signal.Bold(true).Render("Continue"))
			case o.text:
				val := o.value
				if m.typing && m.item == i {
					val = m.text + m.sym("▏", "_")
				}
				b = append(b, m.mark(i)+o.label+": "+val)
			default:
				box := "[ ] "
				if o.radio {
					box = "( ) "
					if o.on {
						box = "(•) "
					}
				} else if o.on {
					box = "[x] "
				}
				if o.off && !o.on {
					b = append(b, m.mark(i)+p.steel.Render(box+o.label+" (not available)"), "    "+p.risk.Render(o.why))
					break
				}
				line := m.mark(i) + box + o.label
				if o.hint != "" {
					line += "  " + p.steel.Render(o.hint)
				}
				b = append(b, line)
				if o.off {
					b = append(b, "    "+p.risk.Render(o.why))
				}
			}
		}
	case app.StepReview:
		h1("Please check before it starts")
		if v.Plan == nil {
			break
		}
		for _, l := range v.Plan.Summary {
			b = append(b, "  "+l)
		}
		if len(v.Plan.Result) > 0 {
			b = append(b, "", lipgloss.NewStyle().Bold(true).Render("You'll get"))
			for _, l := range v.Plan.Result {
				b = append(b, "  "+l)
			}
		}
		warns := 0
		for _, is := range v.Plan.Issues {
			switch {
			case is.Severity == job.Block && is.Kind != "needs-admin":
				b = append(b, "", p.fail.Render(m.sym("✖ ", "x ")+"Can't start: ")+is.Title, "    "+is.Detail)
			case is.Severity == job.Warn:
				warns++
				b = append(b, "", p.risk.Render(m.sym("▲ ", "! ")+"Warning: ")+is.Title, "    "+is.Detail)
			default:
				b = append(b, "", p.steel.Render("Note: ")+is.Title)
			}
		}
		if v.CanStartOver {
			box := "[ ]"
			if v.Req.Fresh {
				box = "[x]"
			}
			b = append(b, "", box+" Start over instead of continuing where it stopped (press o)")
		}
		if warns > 0 {
			box := "[ ]"
			if m.accept {
				box = "[x]"
			}
			b = append(b, "", box+" I've read the warnings and want to go ahead (press a)")
		}
		if c := v.Confirm; c != nil {
			cursor := func(f int) string {
				if m.field == f {
					return m.sym("▏", "_")
				}
				return ""
			}
			b = append(b, "", p.fail.Bold(true).Render("Type the card's label and size to confirm")+" — this changes the card",
				fmt.Sprintf("  Label (%s): %s%s", c.Label, m.label, cursor(1)),
				fmt.Sprintf("  Size (%s): %s%s", c.Size, m.size, cursor(2)))
		}
	case app.StepElevate:
		h1("This needs administrator rights")
		b = append(b, "Reading the card directly needs them. Quit and run:", "", "  "+v.Instruction)
	case app.StepRun:
		h1(nonEmptyOr(v.RunLabel, "Working"))
		pr := v.Progress
		if pr == nil {
			pr = &job.Progress{}
		}
		safe := pr.Counters["OK"] + pr.Counters["ALREADY-COPIED"] + pr.Counters["DELETED-RECOVERED"] + pr.Counters["CARVED"]
		pct := 0
		if pr.Total > 0 {
			pct = int(100 * pr.Done / pr.Total)
		}
		hero := fmt.Sprintf("%s files safe so far", commas(safe))
		if v.Plan != nil && v.Plan.Items > 0 {
			hero = fmt.Sprintf("%s of %s files safe so far", commas(safe), commas(v.Plan.Items))
		}
		barW := min(m.w-12, 60)
		fill := barW * pct / 100
		bar := strings.Repeat(m.sym("█", "#"), fill) + strings.Repeat(m.sym("░", "-"), barW-fill)
		b = append(b, lipgloss.NewStyle().Bold(true).Render(hero), "", p.signal.Render(bar)+fmt.Sprintf(" %d%%", pct),
			fmt.Sprintf("%s of %s", gb(pr.Done), gb(pr.Total)))
		if pr.BPS > 0 {
			b[len(b)-1] += fmt.Sprintf(" · %.1f MB/s", pr.BPS/1e6)
		}
		if pr.Item != "" {
			b = append(b, p.steel.Render(middle(pr.Item, m.w-4)))
		}
		if pr.Note != "" {
			b = append(b, p.risk.Render(m.sym("▲ ", "! ")+pr.Note))
		}
		if n := len(v.Logs); n > 0 {
			b = append(b, "")
			for _, l := range v.Logs[max(0, n-6):] {
				b = append(b, p.steel.Render(l))
			}
		}
	case app.StepRecoverList:
		h1("Pick the files to bring back")
		rv := v.Recover
		if rv == nil {
			break
		}
		b = append(b, p.steel.Render(fmt.Sprintf("%s found · %s shown · %s ticked · unrecoverable files can't be ticked", commas(rv.Total), commas(rv.Shown), commas(rv.Ticked))))
		if m.typing {
			b = append(b, "Filter: "+m.text+m.sym("▏", "_"))
		}
		b = append(b, "")
		for i, r := range rv.Rows {
			if i == m.cur {
				m.cursorLine = len(b)
			}
			box := "[ ]"
			if r.Ticked {
				box = "[x]"
			}
			if !r.Tickable {
				box = " - "
			}
			st := m.state(p, r.State)
			b = append(b, fmt.Sprintf("%s%s %s %-24s %9s  %s", m.mark(i), box, st, r.Path, gb(r.Size), p.steel.Render(r.Comment)))
		}
	case app.StepResults:
		res := v.Result
		title := "Done"
		switch {
		case v.Error != "":
			title = "Stopped"
		case res != nil && !res.Clean:
			title = "Done, with problems"
		}
		h1(title)
		if v.Error != "" {
			b = append(b, p.fail.Render(m.sym("✖ ", "x "))+v.Error)
		}
		if res != nil {
			var keys []string
			for k := range res.Counters {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			b = append(b, "")
			for _, k := range keys {
				b = append(b, fmt.Sprintf("  %-24s %10s", k, commas(res.Counters[k])))
			}
			for _, r := range res.Reports {
				if r != "" {
					b = append(b, "", p.steel.Render("Report: ")+r)
				}
			}
		}
		for _, l := range v.Logs {
			b = append(b, p.steel.Render(l))
		}
		b = append(b, "")
		for i, n := range v.Next {
			if i == m.cur {
				m.cursorLine = len(b)
			}
			b = append(b, m.mark(i)+p.signal.Bold(true).Render(n.Label)+"  "+p.steel.Render(n.Why))
		}
		if m.cur == len(v.Next) {
			m.cursorLine = len(b)
		}
		b = append(b, m.mark(len(v.Next))+"Start over")
	}
	return b
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

func (m *Model) overlay(p palette) string {
	iv := m.v.Issue
	var btns []string
	for i, c := range iv.Issue.Choices {
		l := " " + choiceLabel(c) + " "
		if i == m.issueSel {
			l = lipgloss.NewStyle().Reverse(true).Render("[" + choiceLabel(c) + "]")
		}
		btns = append(btns, l)
	}
	lines := []string{"", lipgloss.NewStyle().Bold(true).Render(p.risk.Render(m.sym("▲ ", "! ")) + iv.Issue.Title)}
	if iv.Issue.Detail != "" {
		lines = append(lines, "  "+iv.Issue.Detail)
	}
	lines = append(lines, "", "  "+strings.Join(btns, "  "), "", p.steel.Render("  ←/→ choose · Enter answer · Esc the safe choice"))
	return m.frame(lines)
}

func (m *Model) stopOverlay(p palette) string {
	return m.frame([]string{"", lipgloss.NewStyle().Bold(true).Render("Stop now?"),
		"  The current block is finished and saved; running the same thing again continues where it stopped.",
		"", "  y stop · n keep going"})
}

func (m *Model) frame(lines []string) string {
	for len(lines) < m.h {
		lines = append(lines, "")
	}
	lines = lines[:m.h]
	for i := range lines {
		lines[i] = fit(lines[i], m.w)
	}
	return strings.Join(lines, "\n")
}

var choiceLabels = map[job.Choice]string{job.Continue: "Continue", job.Stop: "Stop", job.Retry: "Try again", job.Wait: "Wait for the card",
	"skip-rest": "Skip the rest (report them failed)", job.Resume: "Continue where it stopped", job.New: "Start over",
	job.Elevate: "Restart as administrator", "skip-videos": "Skip videos", job.Image: "Make an image instead"}

func choiceLabel(c job.Choice) string {
	if l, ok := choiceLabels[c]; ok {
		return l
	}
	return string(c)
}
