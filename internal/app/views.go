package app

import (
	"slices"
	"strings"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/drives"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/fsread"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/job"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/recovery"
)

// ActionRow is one thing the user can do with a card.
type ActionRow struct {
	Name    string `json:"name"`
	Label   string `json:"label"`
	Desc    string `json:"desc"`
	Enabled bool   `json:"enabled"`
	Reason  string `json:"reason,omitempty"`
	Danger  bool   `json:"danger,omitempty"`
	Admin   bool   `json:"admin,omitempty"`
}

// Actions is the one table both front ends use (labels say what it does for the person).
var Actions = []ActionRow{
	{Name: "rescue", Label: "Copy files off the card", Desc: "Copies every file it can read, working around damaged areas"},
	{Name: "recover", Label: "Find deleted files", Desc: "Looks for deleted and lost files and lets you pick what to bring back", Admin: true},
	{Name: "image", Label: "Make a full backup image", Desc: "Copies the whole card, sector by sector, into one file", Admin: true},
	{Name: "verify", Label: "Check a copy", Desc: "Confirms every copied file matches what was read from the card"},
	{Name: "scan", Label: "See what's on the card", Desc: "Lists files, sizes and problems without copying anything"},
	{Name: "fix", Label: "Card tools (repair, format, test)", Desc: "Repairs, formats or tests the card — these change the card", Danger: true, Admin: true},
}

// FixTools are the card tools, in fix.Available's order (Check, Repair, Format, Health test).
var FixTools = []ActionRow{
	{Name: "check", Label: "Check the file system", Desc: "Changes nothing"},
	{Name: "repair", Label: "Repair the file system", Desc: "Can move damaged files to FOUND.000 / LOST.DIR", Danger: true},
	{Name: "format", Label: "Format the card", Desc: "Erases everything on it", Danger: true},
	{Name: "health-test", Label: "Test the card for bad areas and fake capacity", Desc: "Erases everything on it; takes a while", Danger: true},
}

// toolsFor greys the tools this OS can't run on this card (caps: fix.Available; nil = not known,
// the check page says).
func toolsFor(caps []job.Capability) []ActionRow {
	out := slices.Clone(FixTools)
	for i := range out {
		out[i].Enabled = true
		if i < len(caps) && !caps[i].Enabled {
			out[i].Enabled, out[i].Reason = false, caps[i].Reason
		}
	}
	return out
}

// undeleteFor is Undelete's availability when the card's file system keeps no usable deleted
// records (nil: it works, or isn't known until the card is opened).
func undeleteFor(d drives.Drive) *job.Capability {
	fs := strings.ToLower(d.FS)
	if strings.HasPrefix(fs, "ext") {
		fs = "ext4"
	}
	if fs != "ntfs" && fs != "ext4" {
		return nil
	}
	for _, c := range fsread.Capabilities(&fsread.Volume{FS: fs}, nil) {
		if c.Name == "Undelete" && !c.Enabled {
			return &c
		}
	}
	return nil
}

// RunLabels name the action while it runs and when it's done ("Copy files" → "Copying files").
var RunLabels = map[string][2]string{
	"rescue": {"Copying files", "Copied"}, "recover": {"Looking for deleted files", "Recovered"},
	"image": {"Making the backup image", "Imaged"}, "verify": {"Checking the copy", "Checked"},
	"scan": {"Looking at the card", "Listed"}, "fix": {"Working on the card", "Done"},
}

// Kinds of a typed source (Drive.FS of a path the user chose instead of a drive).
const (
	KindFolder = "folder"
	KindImage  = "image file"
)

func actionsFor(d drives.Drive) []ActionRow {
	out := make([]ActionRow, len(Actions))
	for i, a := range Actions {
		a.Enabled = true
		switch {
		case d.Locked:
			a.Enabled, a.Reason = false, "Unlock the card first (it's encrypted) — in Explorer or Disk Utility"
		case d.FS == KindFolder && (a.Name == "image" || a.Name == "recover" || a.Name == "fix"):
			a.Enabled, a.Reason = false, "Needs the card itself, not a folder — choose the card in the list"
		case a.Name == "fix" && d.FS == KindImage:
			a.Enabled, a.Reason = false, "A card image file can't be repaired — choose the card itself"
		case a.Name == "image" && d.FS == KindImage:
			a.Enabled, a.Reason = false, "This is already an image — copy files from it instead"
		case a.Name == "fix" && d.System:
			a.Enabled, a.Reason = false, "This is your system disk"
		}
		out[i] = a
	}
	return out
}

// ChoiceRow is one backup choice with what it would copy from this card.
type ChoiceRow struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Desc  string `json:"desc"`
	Files int    `json:"files"`
	Bytes int64  `json:"bytes"`
}

// NextStep is a suggestion after a result.
type NextStep struct {
	Action string `json:"action"`
	Label  string `json:"label"`
	Why    string `json:"why"`
}

func nextSteps(req Request, res job.Result) []NextStep {
	switch req.Action {
	case "rescue":
		return []NextStep{{Action: "verify", Label: "Check the copy", Why: "Confirms every copied file matches what was read from the card"}}
	case "scan":
		return []NextStep{{Action: "rescue", Label: "Copy files off the card", Why: "Now that you've seen what's there"}}
	case "image":
		return []NextStep{{Action: "rescue", Label: "Copy files from the image", Why: "The card can rest; the image holds everything"}}
	case "recover":
		return []NextStep{{Action: "scan", Label: "See what's on the card", Why: "Check what else is still there"}}
	}
	return nil
}

// IssueView is a question the engine is waiting on.
type IssueView struct {
	ID    string    `json:"id"`
	Issue job.Issue `json:"issue"`
}

// ConfirmView is what the user must type before a card tool changes the card.
type ConfirmView struct {
	Label string `json:"label"`
	Size  string `json:"size"`
}

// RecoverRow is one line of the results list.
type RecoverRow struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Path     string `json:"path"`
	Size     int64  `json:"size"`
	Modified string `json:"modified"`
	State    string `json:"state"`
	Comment  string `json:"comment"`
	Ticked   bool   `json:"ticked"`
	Tickable bool   `json:"tickable"`
}

// RecoverView is the Recuva-style list: the filtered page and totals.
type RecoverView struct {
	Rows   []RecoverRow `json:"rows"`
	Total  int          `json:"total"`
	Shown  int          `json:"shown"`
	Ticked int          `json:"ticked"`
	Filter string       `json:"filter"`
}

const pageRows = 200

type recoverState struct {
	rows   []recovery.Row
	ticked map[string]bool
	filter string
}

func newRecoverState(rows []recovery.Row) recoverState {
	return recoverState{rows: rows, ticked: map[string]bool{}}
}

func (r *recoverState) visible() []recovery.Row {
	var out []recovery.Row
	for _, x := range r.rows {
		if r.filter == "" || strings.Contains(strings.ToLower(x.Path+" "+x.Name+" "+x.State), r.filter) {
			out = append(out, x)
		}
	}
	return out
}

func (r *recoverState) tick(id string) error {
	for _, x := range r.rows {
		if x.ID == id {
			if x.State == recovery.Unrecoverable {
				return errorf("%s can't be brought back: %s", x.Name, x.Comment)
			}
			r.ticked[id] = !r.ticked[id]
			return nil
		}
	}
	return errorf("no file %s in the list", id)
}

func (r *recoverState) tickAll() {
	for _, x := range r.visible() {
		if x.State != recovery.Unrecoverable {
			r.ticked[x.ID] = true
		}
	}
}

func (r *recoverState) selected() []string {
	var out []string
	for _, x := range r.rows {
		if r.ticked[x.ID] {
			out = append(out, x.ID)
		}
	}
	return out
}

func (r *recoverState) view() *RecoverView {
	if r.rows == nil {
		return nil
	}
	vis := r.visible()
	v := &RecoverView{Total: len(r.rows), Shown: len(vis), Filter: r.filter}
	for _, x := range r.rows {
		if r.ticked[x.ID] {
			v.Ticked++
		}
	}
	for i, x := range vis {
		if i >= pageRows {
			break
		}
		mod := ""
		if !x.MTime.IsZero() {
			mod = x.MTime.Format("2006-01-02 15:04")
		}
		v.Rows = append(v.Rows, RecoverRow{ID: x.ID, Name: x.Name, Path: x.Path, Size: x.Size, Modified: mod, State: x.State,
			Comment: x.Comment, Ticked: r.ticked[x.ID], Tickable: x.State != recovery.Unrecoverable})
	}
	return v
}

// View is everything a renderer needs to draw the current step.
type View struct {
	Step         Step            `json:"step"`
	Steps        []Step          `json:"steps"` // the path for this action, for the step bar
	Busy         string          `json:"busy,omitempty"`
	Error        string          `json:"error,omitempty"`
	Drives       []drives.Drive  `json:"drives"`
	HiddenSystem int             `json:"hiddenSystem"`
	Card         *drives.Drive   `json:"card,omitempty"`
	Actions      []ActionRow     `json:"actions,omitempty"`
	Tools        []ActionRow     `json:"tools,omitempty"`    // card tools, greyed with the reason when this OS can't run one
	Undelete     *job.Capability `json:"undelete,omitempty"` // set when Undelete can't work on this card (Deep scan is then on)
	Req          Request         `json:"req"`
	Choices      []ChoiceRow     `json:"choices,omitempty"`
	Plan         *job.Plan       `json:"plan,omitempty"`
	Confirm      *ConfirmView    `json:"confirm,omitempty"`
	Instruction  string          `json:"instruction,omitempty"`
	Progress     *job.Progress   `json:"progress,omitempty"`
	Issue        *IssueView      `json:"issue,omitempty"`
	Result       *job.Result     `json:"result,omitempty"`
	Logs         []string        `json:"logs,omitempty"`
	Next         []NextStep      `json:"next,omitempty"`
	Recover      *RecoverView    `json:"recover,omitempty"`
	RunLabel     string          `json:"runLabel,omitempty"`
	Admin        bool            `json:"admin"`
	CanStartOver bool            `json:"canStartOver"` // an earlier run can be continued; the user may start over instead
}

// View is a snapshot of the session.
func (s *Session) View() View {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := View{Step: s.step, Busy: s.busy, Error: s.err, Req: s.req, Choices: s.choices, Plan: s.plan,
		Instruction: s.instr, Progress: s.progress, Result: s.result, Next: s.next, Admin: s.deps.IsAdmin(),
		Tools: s.tools, Undelete: s.undelete}
	v.Steps = append([]Step{StepDrive, StepAction}, flow[s.req.Action]...)
	if s.req.Action != "" {
		v.Steps = append(v.Steps, StepRun, StepResults)
		v.RunLabel = RunLabels[s.req.Action][0]
	}
	for _, d := range s.drives {
		if d.System && !s.showSystem {
			v.HiddenSystem++
			continue
		}
		v.Drives = append(v.Drives, d)
	}
	if s.card != nil {
		c := *s.card
		v.Card = &c
		v.Actions = actionsFor(c)
	}
	if s.pend != nil {
		v.Issue = &IssueView{ID: s.pend.id, Issue: s.pend.issue}
	}
	if cf, ok := s.action.(confirmer); ok && needsTyped(s.req) {
		l, sz := cf.ConfirmText()
		v.Confirm = &ConfirmView{Label: l, Size: sz}
	}
	v.Logs = append(v.Logs, s.logs...)
	if s.plan != nil {
		for _, is := range s.plan.Issues {
			v.CanStartOver = v.CanStartOver || is.Kind == "resume"
		}
		v.CanStartOver = v.CanStartOver || s.req.Fresh
	}
	v.Recover = s.rec.view()
	return v
}
