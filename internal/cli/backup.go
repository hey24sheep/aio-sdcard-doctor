package cli

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/preflight"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/profile"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/rescue"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/scan"
)

// menuItem is one line of the "What should be copied?" menu.
type menuItem struct {
	id, title, desc string
	preset          *profile.Preset // nil = Custom
}

// menuChoices is every preset, then Custom.
func menuChoices() []menuItem {
	var out []menuItem
	for i := range profile.Presets {
		p := &profile.Presets[i]
		out = append(out, menuItem{id: p.ID, title: p.Title, desc: p.Desc, preset: p})
	}
	return append(out, menuItem{id: "custom", title: "Custom", desc: "Choose the types, apps and skips yourself"})
}

// defaultChoice is the menu's highlighted line (Enter): Standard. The person sees it marked.
const defaultChoice = 2

// needChoice is the message for a script that didn't say what to copy.
func needChoice() string {
	var b strings.Builder
	b.WriteString("rescue: choose what to copy with --backup:\n")
	for _, m := range menuChoices() {
		if m.preset != nil {
			fmt.Fprintf(&b, "  --backup %-10s %s\n", m.id, m.desc)
		}
	}
	b.WriteString("  or a custom choice: --types photos,videos,… [--exclude recommended] [--apps whatsapp,telegram]\n")
	b.WriteString("Add --skip-empty to leave out 0-byte files and --compress for ZIP output.")
	return b.String()
}

// parseApps reads --apps.
func parseApps(s string) ([]string, error) {
	var out []string
	for _, a := range strings.Split(s, ",") {
		a = strings.ToLower(strings.TrimSpace(a))
		if a == "" {
			continue
		}
		if _, ok := profile.AppNames[a]; !ok {
			return nil, fmt.Errorf("unknown app %q (use whatsapp, telegram)", a)
		}
		out = append(out, a)
	}
	return out, nil
}

// applySelection copies a selection onto the rescue.
func applySelection(r *rescue.Rescue, s rescue.Selection) {
	r.Types, r.Apps, r.Exclude, r.Backup, r.SkipEmpty = s.Types, s.Apps, s.Exclude, s.Name, s.SkipEmpty
}

// ask prints q and waits for an answer; EOF or Ctrl+C is errNoAnswer.
func ask(ctx context.Context, io IO, q string) (string, error) {
	fmt.Fprint(io.Err, q)
	a, ok := readAnswer(ctx, io.In)
	if !ok {
		fmt.Fprintln(io.Err)
		return "", errNoAnswer
	}
	return a, nil
}

// yesNo asks until it gets yes, no or Enter (the shown default).
func yesNo(ctx context.Context, io IO, q string, def bool) (bool, error) {
	hint := "[y/N]"
	if def {
		hint = "[Y/n]"
	}
	for {
		a, err := ask(ctx, io, q+" "+hint+" ")
		if err != nil {
			return false, err
		}
		switch strings.ToLower(a) {
		case "":
			return def, nil
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		}
		fmt.Fprintln(io.Err, "Please answer y or n.")
	}
}

// pickNumber asks for 1..n until it gets one (Enter = def when def > 0).
func pickNumber(ctx context.Context, io IO, n, def int) (int, error) {
	for {
		q := fmt.Sprintf("Choose 1-%d: ", n)
		if def > 0 {
			q = fmt.Sprintf("Choose 1-%d [%d]: ", n, def)
		}
		a, err := ask(ctx, io, q)
		if err != nil {
			return 0, err
		}
		if a == "" && def > 0 {
			return def, nil
		}
		if k, err := strconv.Atoi(a); err == nil && k >= 1 && k <= n {
			return k, nil
		}
		fmt.Fprintf(io.Err, "Please type a number from 1 to %d.\n", n)
	}
}

// describe is a choice in words ("Quick, not compressed").
func describe(s rescue.Selection, compress bool) string {
	d := s.Name
	if d == "" {
		d = "Custom"
	}
	if s.SkipEmpty {
		d += ", empty files left out"
	}
	if compress {
		return d + ", compressed"
	}
	return d + ", not compressed"
}

// otherSummary describes the files of unknown types that only Full and Custom copy.
func otherSummary(inv scan.Inventory) string {
	n, bytes, exts := 0, int64(0), map[string]int{}
	for _, it := range inv.Items {
		if it.Cat == profile.Other {
			n++
			bytes += it.Size
			e := strings.ToLower(path.Ext(it.Rel))
			if e == "" {
				e = "(no extension)"
			}
			exts[e]++
		}
	}
	if n == 0 {
		return ""
	}
	var top []string
	for e := range exts {
		top = append(top, e)
	}
	sort.Slice(top, func(i, j int) bool {
		return exts[top[i]] > exts[top[j]] || exts[top[i]] == exts[top[j]] && top[i] < top[j]
	})
	if len(top) > 5 {
		top = top[:5]
	}
	return fmt.Sprintf("Unknown file types: %s files (%s), e.g. %s — only Full or Custom copies them.",
		preflight.Commas(n), preflight.GB(bytes), strings.Join(top, " "))
}

// chooseBackup asks what to copy, showing what each choice would take from this card. prev is
// the choice of an earlier run into the same folder, offered first.
func chooseBackup(ctx context.Context, io IO, inv scan.Inventory, r *rescue.Rescue, prev *rescue.Selection, prevCompress bool) (keptCompress bool, err error) {
	skipEmpty := r.SkipEmpty // a --skip-empty flag survives whatever is picked here
	if prev != nil {
		same, err := yesNo(ctx, io, fmt.Sprintf("Use the same choice as last time (%s)?", describe(*prev, prevCompress)), true)
		if err != nil {
			return false, err
		}
		if same {
			applySelection(r, *prev)
			r.SkipEmpty = r.SkipEmpty || skipEmpty
			return true, nil
		}
	}
	choices := menuChoices()
	fmt.Fprintln(io.Err, "\nWhat should be copied?")
	for i, m := range choices {
		count := ""
		if m.preset != nil {
			n, b := rescue.Count(inv, rescue.SelectionFor(*m.preset))
			count = fmt.Sprintf("%7s files %9s", preflight.Commas(n), preflight.GB(b))
		}
		mark := ""
		if i+1 == defaultChoice {
			mark = " (default)"
		}
		fmt.Fprintf(io.Err, "  %2d) %-15s %-27s %s%s\n", i+1, m.title, count, m.desc, mark)
	}
	if o := otherSummary(inv); o != "" {
		fmt.Fprintln(io.Err, "  "+o)
	}
	pick, err := pickNumber(ctx, io, len(choices), defaultChoice)
	if err != nil {
		return false, err
	}
	m := choices[pick-1]
	if m.preset != nil {
		s := rescue.SelectionFor(*m.preset)
		s.SkipEmpty = skipEmpty
		applySelection(r, s)
		return false, nil
	}
	s, err := chooseCustom(ctx, io, inv)
	if err != nil {
		return false, err
	}
	s.SkipEmpty = skipEmpty
	applySelection(r, s)
	return false, nil
}

// chooseCustom asks each type on the card (at least one must be ticked), the app filter and the
// skip suggestions.
func chooseCustom(ctx context.Context, io IO, inv scan.Inventory) (rescue.Selection, error) {
	s := rescue.Selection{Name: "Custom"}
	counts := map[profile.Category]scan.Group{}
	for _, it := range inv.Items {
		g := counts[it.Cat]
		g.Files++
		g.Bytes += it.Size
		counts[it.Cat] = g
	}
	for len(s.Types) == 0 {
		for _, c := range profile.Priority {
			g, ok := counts[c]
			if !ok {
				continue
			}
			yes, err := yesNo(ctx, io, fmt.Sprintf("Copy %s (%s files, %s)?", c, preflight.Commas(g.Files), preflight.GB(g.Bytes)), true)
			if err != nil {
				return s, err
			}
			if yes {
				s.Types = append(s.Types, c)
			}
		}
		if len(counts) == 0 {
			return s, fmt.Errorf("the card has no files")
		}
		if len(s.Types) == 0 {
			fmt.Fprintln(io.Err, "Nothing selected — pick at least one type.")
		}
	}
	fmt.Fprintln(io.Err, "Only one app's files?\n  1) No, all apps (default)\n  2) WhatsApp only\n  3) Telegram only")
	app, err := pickNumber(ctx, io, 3, 1)
	if err != nil {
		return s, err
	}
	switch app {
	case 2:
		s.Apps = []string{"whatsapp"}
	case 3:
		s.Apps = []string{"telegram"}
	}
	if s.Exclude, err = askSuggestions(ctx, io, inv.Suggested); err != nil {
		return s, err
	}
	return s, nil
}

// askSuggestions asks about each skip suggestion; the answer is always the user's.
func askSuggestions(ctx context.Context, io IO, sug map[string]scan.Group) ([]string, error) {
	var chosen []string
	for _, id := range profile.SuggestionIDs() {
		g, ok := sug[id]
		if !ok {
			continue
		}
		s := profile.Suggestions[id]
		skip, err := yesNo(ctx, io, fmt.Sprintf("Skip %s — %d files (%s)? %s", s.Title, g.Files, preflight.GB(g.Bytes), s.Why), s.Recommended)
		if err != nil {
			return nil, err
		}
		if skip {
			chosen = append(chosen, id)
		}
	}
	return chosen, nil
}

// askOptions asks the two ticks that apply to any choice: empty files and compression.
func askOptions(ctx context.Context, io IO, r *rescue.Rescue, askEmpty, askCompress bool) error {
	if askEmpty {
		empty := 0
		sel, _ := rescue.Select(r.Inventory(), r.Selection())
		for _, it := range sel {
			if it.Size == 0 {
				empty++
			}
		}
		if empty > 0 {
			yes, err := yesNo(ctx, io, fmt.Sprintf("Copy %d empty (0-byte) files too? They hold no data but keep folders complete.", empty), true)
			if err != nil {
				return err
			}
			r.SkipEmpty = !yes
		}
	}
	if askCompress {
		yes, err := yesNo(ctx, io, "Compress into ZIP files? Saves little on photos and videos, more on documents.", r.Compress)
		if err != nil {
			return err
		}
		r.Compress = yes
	}
	return nil
}
