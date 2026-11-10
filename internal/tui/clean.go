package tui

import (
	"maps"
	"strings"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/app"
)

// cleanStr makes card-controlled text safe for a terminal: newlines, escape sequences, bells and
// other control characters (C0, DEL, C1) become spaces, so a crafted file name can neither break
// the layout nor send commands to the terminal.
func cleanStr(s string) string {
	if strings.IndexFunc(s, isControl) < 0 {
		return s
	}
	return strings.Map(func(r rune) rune {
		if isControl(r) {
			return ' '
		}
		return r
	}, s)
}

func isControl(r rune) bool { return r < 0x20 || r == 0x7f || r >= 0x80 && r < 0xa0 }

func cleanAll(ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = cleanStr(s)
	}
	return out
}

// cleanView returns a copy of v with every string the card (or an error about it) can influence
// cleaned. The session's own data is never modified.
func cleanView(v app.View) app.View {
	c := cleanStr
	v.Busy, v.Error, v.Instruction, v.RunLabel = c(v.Busy), c(v.Error), c(v.Instruction), c(v.RunLabel)
	v.Req.From, v.Req.Dest, v.Req.Manifest, v.Req.Target = c(v.Req.From), c(v.Req.Dest), c(v.Req.Manifest), c(v.Req.Target)
	ds := append(v.Drives[:0:0], v.Drives...)
	for i := range ds {
		ds[i].Label, ds[i].Root, ds[i].FS = c(ds[i].Label), c(ds[i].Root), c(ds[i].FS)
	}
	v.Drives = ds
	if v.Card != nil {
		cd := *v.Card
		cd.Label, cd.Root, cd.FS = c(cd.Label), c(cd.Root), c(cd.FS)
		v.Card = &cd
	}
	as := append(v.Actions[:0:0], v.Actions...)
	for i := range as {
		as[i].Reason = c(as[i].Reason)
	}
	v.Actions = as
	cs := append(v.Choices[:0:0], v.Choices...)
	for i := range cs {
		cs[i].Title, cs[i].Desc = c(cs[i].Title), c(cs[i].Desc)
	}
	v.Choices = cs
	if v.Plan != nil {
		p := *v.Plan
		p.Summary, p.Result = cleanAll(p.Summary), cleanAll(p.Result)
		is := append(p.Issues[:0:0], p.Issues...)
		for i := range is {
			is[i].Title, is[i].Detail = c(is[i].Title), c(is[i].Detail)
		}
		p.Issues = is
		v.Plan = &p
	}
	if v.Confirm != nil {
		cf := *v.Confirm
		cf.Label, cf.Size = c(cf.Label), c(cf.Size)
		v.Confirm = &cf
	}
	if v.Progress != nil {
		pr := *v.Progress
		pr.Item, pr.Note, pr.Phase = c(pr.Item), c(pr.Note), c(pr.Phase)
		v.Progress = &pr
	}
	if v.Issue != nil {
		iv := *v.Issue
		iv.Issue.Title, iv.Issue.Detail = c(iv.Issue.Title), c(iv.Issue.Detail)
		v.Issue = &iv
	}
	if v.Result != nil {
		r := *v.Result
		r.Reports = cleanAll(r.Reports)
		r.Counters = map[string]int{}
		for k, n := range maps.All(v.Result.Counters) {
			r.Counters[c(k)] = n
		}
		v.Result = &r
	}
	v.Logs = cleanAll(v.Logs)
	ns := append(v.Next[:0:0], v.Next...)
	for i := range ns {
		ns[i].Label, ns[i].Why = c(ns[i].Label), c(ns[i].Why)
	}
	v.Next = ns
	if v.Recover != nil {
		rv := *v.Recover
		rows := append(rv.Rows[:0:0], rv.Rows...)
		for i := range rows {
			rows[i].Name, rows[i].Path, rows[i].Comment, rows[i].Modified = c(rows[i].Name), c(rows[i].Path), c(rows[i].Comment), c(rows[i].Modified)
		}
		rv.Rows, rv.Filter = rows, c(rv.Filter)
		v.Recover = &rv
	}
	return v
}
