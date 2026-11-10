// Package job is the lifecycle every action shares: Plan (inventory, pre-flight issues,
// capabilities) → user review → Run (resumable state in a Store, questions through a Prompter).
package job

import (
	"context"
	"time"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/destfs"
)

// Capability is an action or option that is either available or greyed out with a reason.
type Capability struct {
	Name    string
	Enabled bool
	Reason  string // ≤ 60 chars, shown when !Enabled
}

// Progress is what an action reports while it runs (≤ 10 Hz).
type Progress struct {
	Phase, Item       string
	Done, Total       int64
	Items, ItemsTotal int
	BPS               float64
	Counters          map[string]int
	Note              string
}

// Env is what an action gets from its front end.
type Env struct {
	Out      destfs.FS
	Prompt   Prompter
	Progress func(Progress)
	Now      func() time.Time
	Log      func(level, msg string)
}

// Plan is the result of an action's pre-flight. Summary says what will happen and Result what
// the user will get; both are shown for one confirmation before Run.
type Plan struct {
	Summary    []string
	Result     []string
	Bytes      int64
	Items      int
	Issues     []Issue
	Resume     *ResumeInfo
	NeedsAdmin bool
	Caps       []Capability
}

// ResumeInfo describes a run that can be continued.
type ResumeInfo struct {
	Done, Total int
	Started     time.Time
}

// Result is what a finished run reports.
type Result struct {
	Counters map[string]int
	Reports  []string
	Clean    bool
}

// Action is the shared lifecycle of scan, rescue, image, verify, recover and fix.
type Action interface {
	Name() string
	Plan(ctx context.Context, env Env) (Plan, error)
	Run(ctx context.Context, env Env, p Plan) (Result, error)
}
