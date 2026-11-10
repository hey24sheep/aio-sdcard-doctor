package job

import "context"

// Severity orders issues: Block stops the action, Warn needs confirmation, Info is shown.
type Severity int

const (
	Info Severity = iota
	Warn
	Block
)

func (s Severity) String() string {
	switch s {
	case Warn:
		return "warning"
	case Block:
		return "blocked"
	}
	return "info"
}

// MarshalText makes a severity a word in JSON ("info", "warning", "blocked"), as front ends show it.
func (s Severity) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

// UnmarshalText reads the words back.
func (s *Severity) UnmarshalText(b []byte) error {
	switch string(b) {
	case "warning":
		*s = Warn
	case "blocked":
		*s = Block
	default:
		*s = Info
	}
	return nil
}

// Choice is an answer to an Issue.
type Choice string

const (
	Continue Choice = "continue"
	Retry    Choice = "retry"
	Skip     Choice = "skip"
	Wait     Choice = "wait"
	Stop     Choice = "stop"
	Resume   Choice = "resume"
	New      Choice = "new"
	Image    Choice = "image"
	Elevate  Choice = "elevate"
)

// Issue is a question or notice raised to the user.
type Issue struct {
	Kind, Title, Detail string
	Severity            Severity
	Choices             []Choice
	Default             Choice
}

// Prompter asks the user about an Issue.
type Prompter interface {
	Ask(ctx context.Context, is Issue) Choice
}

// AutoPrompter answers for non-interactive runs: the Default when it is safe (or Yes is set),
// otherwise "stop". Block issues always stop.
type AutoPrompter struct{ Yes bool }

// safe defaults never discard data, write to a card, or accept a risk on the user's behalf.
var safe = map[Choice]bool{Retry: true, Wait: true, Resume: true, Stop: true}

// Ask implements Prompter.
func (a AutoPrompter) Ask(_ context.Context, is Issue) Choice {
	if is.Severity == Block || is.Default == "" {
		return Stop
	}
	if a.Yes || safe[is.Default] {
		return is.Default
	}
	return Stop
}
