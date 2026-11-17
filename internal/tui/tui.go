// Package tui is the keyboard-driven terminal UI: a Bubble Tea renderer of app.Session, so it
// behaves exactly like the GUI. Status is never colour alone (words and symbols, ASCII with
// --ascii), NO_COLOR is respected, and every line is cut to the terminal width.
package tui

import (
	"os"
	"slices"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

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
	warns    string // the warnings accept was ticked for
	label    string
	size     string
	field    int // 0 none, 1 label, 2 size (card tools confirmation)
	stopAsk  bool
	leaveAsk string // the key ("n" start over, "q" quit) waiting for "Leave the list? y/n"
	issueID  string // the question issueSel belongs to
	issueSel int
	help     bool
	bellFor  string

	cursorLine int // set while drawing: the line of the highlighted row (for scrolling)
	inner      int // set while drawing: the panel's text width

	speeds     []float64 // recent speeds of this run, for the sparkline
	lastSample time.Time
}

// sampleEvery spaces the speed samples (a test draws a run without waiting).
var sampleEvery = time.Second

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
		m.cur, m.typing, m.accept, m.field, m.label, m.size, m.stopAsk, m.leaveAsk = 0, false, false, 0, "", "", false, ""
		m.text = ""
		m.speeds = nil
		if m.v.Step == app.StepDest {
			m.text, m.typing = m.v.Req.Dest, true
		}
		if m.v.Step == app.StepRecoverList && m.v.Recover != nil {
			m.text = m.v.Recover.Filter
		}
	}
	if pr := m.v.Progress; m.v.Step == app.StepRun && pr != nil && pr.BPS > 0 && time.Since(m.lastSample) >= sampleEvery {
		m.speeds = append(m.speeds, pr.BPS)
		if len(m.speeds) > 240 {
			m.speeds = m.speeds[len(m.speeds)-240:]
		}
		m.lastSample = time.Now()
	}
	if m.v.Plan != nil { // checked again on the same page (Start over): new warnings need a new tick
		if w := warnKey(m.v.Plan); w != m.warns {
			m.warns, m.accept = w, false
		}
	}
	if m.v.Issue == nil {
		m.issueSel, m.issueID = 0, ""
	} else {
		m.stopAsk, m.leaveAsk = false, "" // the engine's question comes first, and must be on screen to be answered
		if m.v.Issue.ID != m.issueID {    // a new question starts on its default, as the GUI does
			m.issueID, m.issueSel = m.v.Issue.ID, max(0, slices.Index(m.v.Issue.Issue.Choices, m.v.Issue.Issue.Default))
		}
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
		case "esc", "ctrl+c": // Esc (and Ctrl+C) is the safe answer
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
	if m.leaveAsk != "" {
		switch s {
		case "y", "Y", "ctrl+c": // a second Ctrl+C means it
			k := m.leaveAsk
			m.leaveAsk = ""
			if k == "n" {
				m.do("restart")
				return nil
			}
			return tea.Quit
		case "n", "N", "esc":
			m.leaveAsk = ""
		}
		return nil
	}
	if s == "ctrl+c" {
		switch m.v.Step {
		case app.StepRun:
			m.stopAsk = true
			return nil
		case app.StepRecoverList: // a list that may have taken hours: ask first
			m.leaveAsk = "q"
			return nil
		}
		return tea.Quit
	}
	if m.typing || m.field > 0 {
		return m.typeKey(k)
	}
	switch s {
	case "q":
		if m.v.Step == app.StepRecoverList {
			m.leaveAsk = "q"
			return nil
		}
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
		// a stray key never throws away a run's result or a pick list that took long to make
		if m.v.Step != app.StepRun && m.v.Step != app.StepResults && m.v.Step != app.StepRecoverList {
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
		case "n": // leave the list on purpose, like the GUI's Start over — after asking
			m.leaveAsk = "n"
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
