// Package app is the interactive flow shared by the terminal UI and the web GUI: a Session holds
// the steps (card → what to do → choices → where → options → check → run → done), validation,
// capabilities, engine prompts and events once, so both front ends always behave the same.
package app

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/drives"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/job"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/recovery"
)

// Step is one screen of the flow.
type Step string

// Steps.
const (
	StepDrive       Step = "drive"
	StepAction      Step = "action"
	StepChoice      Step = "choice" // what to copy (rescue)
	StepDest        Step = "dest"
	StepOptions     Step = "options"
	StepReview      Step = "review"
	StepElevate     Step = "elevate"
	StepRun         Step = "run"
	StepRecoverList Step = "recover-list"
	StepResults     Step = "results"
)

// flow is the order of steps after "what to do", per action.
var flow = map[string][]Step{
	"scan":    {StepReview},
	"rescue":  {StepChoice, StepDest, StepOptions, StepReview},
	"image":   {StepDest, StepOptions, StepReview},
	"verify":  {StepOptions, StepReview},
	"recover": {StepDest, StepOptions, StepReview},
	"fix":     {StepOptions, StepReview},
}

// Request is everything the user chose; Deps.NewAction turns it into a job.Action.
type Request struct {
	Action    string   `json:"action"`
	From      string   `json:"from"`
	Dest      string   `json:"dest"`
	Backup    string   `json:"backup"` // preset id (rescue)
	Compress  bool     `json:"compress"`
	SkipEmpty bool     `json:"skipEmpty"`
	Deep      bool     `json:"deep"`
	Thorough  bool     `json:"thorough"`
	List      bool     `json:"list"`   // recover: index and list only
	Select    []string `json:"select"` // recover: the ticked rows
	Manifest  string   `json:"manifest"`
	Target    string   `json:"target"`
	FixOp     string   `json:"fixOp"`
	Fresh     bool     `json:"fresh"` // start over instead of continuing an earlier run
	NewLabel  string   `json:"newLabel"`
}

// Deps are the outside world (fakes in tests).
type Deps struct {
	ListDrives  func() ([]drives.Drive, error)
	NewAction   func(Request) (job.Action, error)
	Choices     func(ctx context.Context, from string) ([]ChoiceRow, error) // backup choices with counts (scans the card)
	IsAdmin     func() bool
	Instruction func(args []string) string
	DefaultDest func(from string) string
	Now         func() time.Time
	FixTools    func(from string) []job.Capability // fix.Available for this card (nil: not known)
}

// Command is what a renderer asks the session to do.
type Command struct {
	Name string            `json:"name"`
	Args map[string]string `json:"args"`
}

// Event tells renderers something changed. Progress events are coalesced (latest wins).
type Event struct {
	Kind     string        `json:"kind"` // state, progress, issue, log, done
	Progress *job.Progress `json:"progress,omitempty"`
	Issue    *job.Issue    `json:"issue,omitempty"`
	IssueID  string        `json:"issueId,omitempty"`
	Log      string        `json:"log,omitempty"`
}

type pending struct {
	id    string
	issue job.Issue
	ch    chan job.Choice
}

// Session is one person's flow.
type Session struct {
	ctx      context.Context
	stop     context.CancelFunc
	deps     Deps
	bgCancel context.CancelFunc // the running pre-flight or choices scan

	mu         sync.Mutex
	step       Step
	history    []Step
	drives     []drives.Drive
	showSystem bool
	card       *drives.Drive
	req        Request
	choices    []ChoiceRow
	busy       string
	err        string
	action     job.Action
	plan       *job.Plan
	progress   *job.Progress
	pend       *pending
	issueSeq   int
	result     *job.Result
	logs       []string
	next       []NextStep
	instr      string
	runCancel  context.CancelFunc
	rec        recoverState
	tools      []ActionRow     // card tools for this card
	undelete   *job.Capability // Undelete can't work on this card
	wg         sync.WaitGroup

	queue  []Event
	notify chan struct{}
	out    chan Event
	done   chan struct{}
	closed bool
}

// NewSession starts a session; Close ends it.
func NewSession(ctx context.Context, deps Deps) *Session {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.IsAdmin == nil {
		deps.IsAdmin = func() bool { return false }
	}
	if deps.Instruction == nil {
		deps.Instruction = func([]string) string { return "Run this program as administrator." }
	}
	if deps.DefaultDest == nil {
		deps.DefaultDest = func(string) string { return "" }
	}
	ctx, stop := context.WithCancel(ctx)
	s := &Session{ctx: ctx, stop: stop, deps: deps, step: StepDrive, notify: make(chan struct{}, 1), out: make(chan Event, 256), done: make(chan struct{})}
	s.refresh()
	s.wg.Add(1)
	go s.pump()
	return s
}

// Events delivers changes; it is closed by Close.
func (s *Session) Events() <-chan Event { return s.out }

// Close cancels anything running and stops the session's goroutines.
func (s *Session) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.stop() // ends the run and any pre-flight still going
	if s.runCancel != nil {
		s.runCancel()
	}
	if s.pend != nil {
		select {
		case s.pend.ch <- job.Stop:
		default:
		}
	}
	s.mu.Unlock()
	close(s.done)
	s.wg.Wait()
	s.mu.Lock()
	s.closeAction()
	s.mu.Unlock()
}

// pump hands queued events to the renderer without ever blocking the engine.
func (s *Session) pump() {
	defer s.wg.Done()
	defer close(s.out)
	for {
		select {
		case <-s.done:
			return
		case <-s.notify:
		}
		s.mu.Lock()
		q := s.queue
		s.queue = nil
		s.mu.Unlock()
		for _, ev := range q {
			select {
			case s.out <- ev:
			case <-s.done:
				return
			}
		}
	}
}

// emit queues an event (caller may hold mu). Consecutive progress events collapse into one.
func (s *Session) emitLocked(ev Event) {
	if ev.Kind == "progress" && len(s.queue) > 0 && s.queue[len(s.queue)-1].Kind == "progress" {
		s.queue[len(s.queue)-1] = ev
	} else if len(s.queue) < 4096 {
		s.queue = append(s.queue, ev)
	}
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

func (s *Session) refresh() { s.setDrives(s.deps.ListDrives()) }

func (s *Session) setDrives(ds []drives.Drive, err error) {
	if err != nil {
		s.err = err.Error()
		return
	}
	sort.SliceStable(ds, func(i, j int) bool { return ds[i].Removable && !ds[j].Removable })
	s.drives = ds
}

func (s *Session) goStep(st Step) {
	if s.step != st && s.step != StepRun { // a finished run is never a place to go back to
		s.history = append(s.history, s.step)
	}
	s.step, s.err = st, ""
}

// nextAfter is the step after cur in the current action's flow.
func (s *Session) nextAfter(cur Step) Step {
	f := flow[s.req.Action]
	for i, st := range f {
		if st == cur && i+1 < len(f) {
			return f[i+1]
		}
	}
	return StepReview
}

func (s *Session) closeAction() {
	if c, ok := s.action.(interface{ Close() error }); ok {
		c.Close()
	}
	s.action, s.plan = nil, nil
}

func arg(c Command, k string) string { return strings.TrimSpace(c.Args[k]) }

// Do performs one command. Errors are things the renderer should show; the view also carries them.
func (s *Session) Do(c Command) error {
	if c.Name == "refresh" { // reading the drives can be slow: never while holding the session
		ds, err := s.deps.ListDrives()
		s.mu.Lock()
		s.setDrives(ds, err)
		s.emitLocked(Event{Kind: "state"})
		s.mu.Unlock()
		return err
	}
	s.mu.Lock()
	err := s.do(c)
	if err != nil {
		s.err = err.Error()
	}
	s.emitLocked(Event{Kind: "state"})
	s.mu.Unlock()
	return err
}

func (s *Session) do(c Command) error {
	if s.busy != "" {
		switch c.Name {
		case "cancel", "back", "restart":
			s.stopBusy()
			if c.Name == "cancel" {
				s.err = "Stopped checking. Press Back to change something, or try again."
				return nil
			}
		case "answer":
		default:
			return errors.New("one moment — " + strings.ToLower(s.busy))
		}
	}
	if s.step == StepRun && c.Name != "cancel" && c.Name != "answer" {
		return errors.New("finish or stop the current run first")
	}
	switch c.Name {
	case "refresh":
		s.refresh()
	case "show-system":
		s.showSystem = !s.showSystem
	case "select-drive":
		root := arg(c, "root")
		for i := range s.drives {
			if s.drives[i].Root == root {
				d := s.drives[i]
				s.selectCard(&d)
				return nil
			}
		}
		return fmt.Errorf("no drive %s — is the card still in?", root)
	case "select-path": // a folder or a card image file
		p := arg(c, "path")
		if p == "" {
			return errors.New("type a folder or image file")
		}
		if len(p) == 2 && p[1] == ':' { // "E:" means the drive, not E:'s current folder
			p += `\`
		}
		for i := range s.drives {
			if strings.EqualFold(strings.TrimRight(s.drives[i].Root, `\/`), strings.TrimRight(p, `\/`)) {
				d := s.drives[i]
				s.selectCard(&d)
				return nil
			}
		}
		st, err := os.Stat(p)
		if err != nil {
			return fmt.Errorf("can't find %s", p)
		}
		kind := KindFolder
		if st.Mode().IsRegular() {
			kind = KindImage
		}
		s.selectCard(&drives.Drive{Root: p, Label: filepath.Base(p), FS: kind})
	case "edit-dest": // from the check page: change where the files go
		if !slices.Contains(flow[s.req.Action], StepDest) {
			return errors.New("this action has no folder to change")
		}
		s.closeAction()
		s.goStep(StepDest)
	case "choose-action":
		return s.chooseAction(arg(c, "action"))
	case "quick-rescue":
		if s.card == nil {
			return errors.New("choose a card first")
		}
		if err := s.chooseActionOpt("rescue", false); err != nil {
			return err
		}
		s.req.Backup, s.req.Dest = "full", s.deps.DefaultDest(s.card.Root)
		s.startPlan()
	case "set-choice":
		id := arg(c, "id")
		if id == "" {
			return errors.New("choose what to copy")
		}
		s.req.Backup = id
		s.goStep(s.nextAfter(StepChoice))
	case "set-dest":
		p := arg(c, "path")
		if err := s.checkDest(p); err != nil {
			return err
		}
		s.req.Dest = p
		s.goStep(s.nextAfter(StepDest))
	case "set-option":
		return s.setOption(arg(c, "name"), arg(c, "value"))
	case "next":
		if s.step != StepOptions {
			return errors.New("nothing to continue")
		}
		if s.req.Action == "fix" && s.req.FixOp == "" {
			return errors.New("choose what to do with the card")
		}
		if s.req.Action == "verify" && s.req.Manifest == "" {
			return errors.New("choose the rescue to check (its Reports/manifest.json)")
		}
		s.startPlan()
	case "back":
		if len(s.history) == 0 {
			return nil
		}
		if s.step == StepReview || s.step == StepElevate {
			s.closeAction()
		}
		if s.step == StepElevate && len(s.history) > 1 && s.history[len(s.history)-1] == StepReview {
			s.history = s.history[:len(s.history)-1] // checking again would only come back here
		}
		s.step, s.history, s.err = s.history[len(s.history)-1], s.history[:len(s.history)-1], ""
		switch {
		case s.step == StepReview: // check again: the earlier plan was closed
			s.closeAction()
			s.plan2()
		case s.step == StepChoice && len(s.choices) == 0:
			s.loadChoices()
		}
	case "start":
		return s.start(c)
	case "answer":
		return s.answer(arg(c, "id"), job.Choice(arg(c, "choice")))
	case "cancel":
		if s.runCancel != nil {
			s.runCancel()
		}
	case "next-action":
		return s.nextAction(arg(c, "action"))
	case "recover-filter":
		s.rec.filter = strings.ToLower(arg(c, "text"))
	case "recover-tick":
		return s.rec.tick(arg(c, "id"))
	case "recover-tick-all":
		s.rec.tickAll()
	case "recover-start":
		ids := s.rec.selected()
		if len(ids) == 0 {
			return errors.New("tick the files to bring back")
		}
		s.req.List, s.req.Select = false, ids
		s.startPlan()
	case "restart": // back to the card list, keeping nothing of the last flow
		s.closeAction()
		s.step, s.history, s.card, s.req, s.choices, s.result, s.progress, s.next, s.instr = StepDrive, nil, nil, Request{}, nil, nil, nil, nil, ""
		s.rec = recoverState{}
		s.refresh()
	default:
		return fmt.Errorf("unknown command %q", c.Name)
	}
	return nil
}

func (s *Session) selectCard(d *drives.Drive) {
	s.closeAction()
	s.card = d
	s.req = Request{From: d.Root}
	s.result, s.progress, s.next = nil, nil, nil
	s.goStep(StepAction)
}

func (s *Session) chooseAction(name string) error { return s.chooseActionOpt(name, true) }

// chooseActionOpt: withChoices=false skips scanning for the backup choices (the quick path
// already knows: Full).
func (s *Session) chooseActionOpt(name string, withChoices bool) error {
	if s.card == nil {
		return errors.New("choose a card first")
	}
	for _, a := range actionsFor(*s.card) {
		if a.Name == name {
			if !a.Enabled {
				return errors.New(a.Label + ": " + a.Reason)
			}
			from := s.req.From
			s.req = Request{Action: name, From: from}
			s.tools, s.undelete = nil, nil
			if name == "recover" {
				s.req.List = true // first find what can come back, then the user ticks
				if s.undelete = undeleteFor(*s.card); s.undelete != nil {
					s.req.Deep = true // the only way to find deleted files there
				}
			}
			s.goStep(flow[name][0])
			if flow[name][0] == StepReview {
				s.history = s.history[:len(s.history)-1]
				s.startPlan()
			}
			if s.step == StepChoice && withChoices {
				s.loadChoices()
			}
			if name == "fix" {
				s.loadTools()
			}
			return nil
		}
	}
	return fmt.Errorf("unknown action %q", name)
}

// loadTools finds which card tools this OS can run on the card (it looks at the disks).
func (s *Session) loadTools() {
	s.tools = toolsFor(nil)
	if s.deps.FixTools == nil {
		return
	}
	s.busy = "Checking which card tools work here"
	from := s.req.From
	ctx := s.bg()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		caps := s.deps.FixTools(from)
		s.mu.Lock()
		defer s.mu.Unlock()
		if ctx.Err() != nil {
			return
		}
		s.busy, s.tools = "", toolsFor(caps)
		if i := slices.IndexFunc(s.tools, func(t ActionRow) bool { return t.Name == s.req.FixOp }); i >= 0 && !s.tools[i].Enabled {
			s.req.FixOp = ""
		}
		s.emitLocked(Event{Kind: "state"})
	}()
}

func (s *Session) loadChoices() {
	if s.deps.Choices == nil {
		return
	}
	s.busy = "Looking at what's on the card"
	from := s.req.From
	ctx := s.bg()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		cs, err := s.deps.Choices(ctx, from)
		s.mu.Lock()
		defer s.mu.Unlock()
		if ctx.Err() != nil { // stopped or left: drop the result
			return
		}
		s.busy = ""
		if err != nil {
			s.err = "Couldn't look at the card: " + err.Error()
		}
		s.choices = cs
		s.emitLocked(Event{Kind: "state"})
	}()
}

func (s *Session) checkDest(p string) error {
	if p == "" {
		return errors.New("choose where to put the files")
	}
	if !filepath.IsAbs(p) && !strings.Contains(p, `:\`) && !strings.HasPrefix(p, "/") {
		return errors.New("use a full path, e.g. D:\\rescue or /home/me/rescue")
	}
	if s.card != nil && s.card.Root != "" && sameOrInside(p, s.card.Root) && s.req.Action != "image" {
		return errors.New("that folder is on the card itself — choose a folder on another disk")
	}
	return nil
}

func sameOrInside(p, root string) bool {
	np := strings.ToLower(filepath.Clean(p))
	nr := strings.ToLower(filepath.Clean(root))
	return np == nr || strings.HasPrefix(np, strings.TrimRight(nr, `\/`)+string(filepath.Separator))
}

func (s *Session) setOption(name, value string) error {
	on := value == "1" || value == "true" || value == "on"
	switch name {
	case "compress":
		s.req.Compress = on
	case "skip-empty":
		s.req.SkipEmpty = on
	case "deep":
		if !on && s.undelete != nil {
			return errors.New("Deep scan is the only way to find deleted files on this card — Undelete: " + s.undelete.Reason)
		}
		s.req.Deep = on
	case "thorough":
		s.req.Thorough = on
		s.req.Deep = s.req.Deep || on
	case "manifest":
		s.req.Manifest = value
	case "target":
		s.req.Target = value
	case "fix-op":
		switch value {
		case "check", "repair", "format", "health-test":
			for _, t := range s.tools {
				if t.Name == value && !t.Enabled {
					return errors.New(t.Label + ": " + t.Reason)
				}
			}
			s.req.FixOp = value
		default:
			return fmt.Errorf("unknown card tool %q", value)
		}
	case "label":
		s.req.NewLabel = value
	case "fresh": // start over instead of continuing: check again with that choice
		s.req.Fresh = on
		if s.step == StepReview {
			s.closeAction()
			s.plan2()
		}
	default:
		return fmt.Errorf("unknown option %q", name)
	}
	return nil
}

// startPlan builds the action and runs its pre-flight in the background.
func (s *Session) startPlan() {
	s.closeAction()
	s.goStep(StepReview)
	s.plan2()
}

// bg starts a cancellable background task (one at a time).
func (s *Session) bg() context.Context {
	if s.bgCancel != nil {
		s.bgCancel()
	}
	ctx, cancel := context.WithCancel(s.ctx)
	s.bgCancel = cancel
	return ctx
}

// stopBusy stops the background check and forgets it.
func (s *Session) stopBusy() {
	if s.bgCancel != nil {
		s.bgCancel()
		s.bgCancel = nil
	}
	s.busy = ""
}

// plan2 runs the pre-flight of the current request in the background, on the review step.
func (s *Session) plan2() {
	s.busy = "Checking everything before it starts"
	req := s.req
	ctx := s.bg()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		a, err := s.deps.NewAction(req)
		var p job.Plan
		if err == nil {
			p, err = a.Plan(ctx, s.env())
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if ctx.Err() != nil { // stopped or left: drop the result and its reader process
			if c, ok := a.(interface{ Close() error }); ok {
				c.Close()
			}
			return
		}
		s.busy = ""
		if err != nil {
			s.err = err.Error()
			if c, ok := a.(interface{ Close() error }); ok {
				c.Close()
			}
		} else {
			s.action, s.plan = a, &p
			if p.NeedsAdmin && !s.deps.IsAdmin() {
				s.instr = s.deps.Instruction(nil)
				s.goStep(StepElevate)
			}
		}
		s.emitLocked(Event{Kind: "state"})
	}()
}

func (s *Session) env() job.Env {
	return job.Env{Prompt: prompter{s}, Now: s.deps.Now,
		Progress: func(p job.Progress) {
			p.Counters = maps.Clone(p.Counters) // the engine keeps writing its own map
			s.mu.Lock()
			s.progress = &p
			s.emitLocked(Event{Kind: "progress", Progress: &p})
			s.mu.Unlock()
		},
		Log: func(level, msg string) {
			s.mu.Lock()
			if len(s.logs) >= 500 {
				s.logs = s.logs[1:]
			}
			s.logs = append(s.logs, msg)
			s.emitLocked(Event{Kind: "log", Log: msg})
			s.mu.Unlock()
		}}
}

func (s *Session) start(c Command) error {
	if s.step != StepReview || s.plan == nil || s.action == nil {
		return errors.New("nothing is ready to start")
	}
	warns := 0
	for _, is := range s.plan.Issues {
		switch {
		case is.Kind == "needs-admin" && !s.deps.IsAdmin():
			return errors.New("this needs administrator rights: " + s.deps.Instruction(nil))
		case is.Severity == job.Block && is.Kind != "needs-admin":
			return errors.New("can't start: " + is.Title)
		case is.Severity == job.Warn:
			warns++
		}
	}
	if warns > 0 && arg(c, "accept") != "1" {
		return errors.New("read the warnings and accept them to start")
	}
	if cf, ok := s.action.(confirmer); ok && needsTyped(s.req) {
		if err := cf.Confirm(c.Args["label"], c.Args["size"]); err != nil {
			label, size := cf.ConfirmText()
			return fmt.Errorf("type the label %q and the size %q exactly as shown (%v)", label, size, err)
		}
	}
	ctx, cancel := context.WithCancel(s.ctx)
	s.runCancel = cancel
	s.goStep(StepRun)
	s.progress, s.result, s.logs = nil, nil, nil
	a, plan, req := s.action, *s.plan, s.req
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer cancel()
		res, err := a.Run(ctx, s.env(), plan)
		s.mu.Lock()
		defer s.mu.Unlock()
		s.runCancel, s.pend = nil, nil
		s.result = &res
		if r, ok := a.(interface{ Rows() []recovery.Row }); ok && req.List && err == nil {
			s.rec = newRecoverState(r.Rows())
			s.closeAction()
			s.goStep(StepRecoverList)
			s.err = s.errOr(err, ctx)
		} else {
			s.next = nextSteps(req, res)
			s.closeAction()
			s.goStep(StepResults)
			s.err = s.errOr(err, ctx)
		}
		s.emitLocked(Event{Kind: "done"})
		s.emitLocked(Event{Kind: "state"})
	}()
	return nil
}

func (s *Session) errOr(err error, ctx context.Context) string {
	switch {
	case err != nil && ctx.Err() != nil:
		return "Stopped. Run the same action again to continue where it stopped."
	case err != nil:
		return err.Error()
	}
	return ""
}

type confirmer interface {
	ConfirmText() (label, size string)
	Confirm(label, size string) error
}

func needsTyped(r Request) bool { return r.Action == "fix" && r.FixOp != "check" }

func (s *Session) answer(id string, c job.Choice) error {
	p := s.pend
	if p == nil || p.id != id {
		return errors.New("that question is no longer open")
	}
	ok := false
	for _, x := range p.issue.Choices {
		ok = ok || x == c
	}
	if !ok {
		return fmt.Errorf("%q isn't one of the choices", c)
	}
	s.pend = nil
	p.ch <- c
	return nil
}

// prompter turns engine questions into issue events and waits for the renderer's answer. A
// renderer that goes away leaves the question pending; reconnecting shows it again in View.
type prompter struct{ s *Session }

func (p prompter) Ask(ctx context.Context, is job.Issue) job.Choice {
	s := p.s
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return job.Stop
	}
	s.issueSeq++
	pd := &pending{id: fmt.Sprint(s.issueSeq), issue: is, ch: make(chan job.Choice, 1)}
	s.pend = pd
	s.emitLocked(Event{Kind: "issue", Issue: &is, IssueID: pd.id})
	s.mu.Unlock()
	select {
	case c := <-pd.ch:
		return c
	case <-ctx.Done():
		return job.Stop
	}
}

func (s *Session) nextAction(name string) error {
	prev := s.req
	if err := s.chooseAction(name); err != nil {
		return err
	}
	if name == "verify" && prev.Action == "rescue" && prev.Dest != "" {
		s.req.Manifest = filepath.Join(prev.Dest, "Reports", "manifest.json")
	}
	return nil
}

func errorf(f string, a ...any) error { return fmt.Errorf(f, a...) }
