package rescue

import (
	"strings"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/profile"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/scan"
)

// Selection is what the user chose to copy. Every file left out gets an EXCLUDED row with the
// reason, so the report always accounts for the whole card.
type Selection struct {
	Name      string             // shown in the summary: the preset title or "Custom"
	Types     []profile.Category // empty or containing "all" = every category, Other included
	Apps      []string           // only files of these apps (whatsapp, telegram)
	Exclude   []string           // suggestion ids to skip ("recommended" = all recommended)
	SkipEmpty bool               // leave out 0-byte files
}

// SelectionFor turns a preset into a selection.
func SelectionFor(p profile.Preset) Selection {
	s := Selection{Name: p.Title, Types: p.Types, Apps: p.Apps}
	if p.SkipJunk {
		s.Exclude = []string{"recommended"}
	}
	return s
}

func (s Selection) wantsType(c profile.Category) bool {
	if len(s.Types) == 0 {
		return true
	}
	for _, t := range s.Types {
		if t == c || strings.EqualFold(string(t), "all") {
			return true
		}
	}
	return false
}

func (s Selection) excludeIDs() map[string]bool {
	ids := map[string]bool{}
	for _, e := range s.Exclude {
		switch e = strings.TrimSpace(e); e {
		case "", "none":
		case "recommended":
			for id, sg := range profile.Suggestions {
				if sg.Recommended {
					ids[id] = true
				}
			}
		default:
			ids[e] = true
		}
	}
	return ids
}

func (s Selection) inApps(rel string) bool {
	if len(s.Apps) == 0 {
		return true
	}
	for _, a := range s.Apps {
		if profile.InApp(a, rel) {
			return true
		}
	}
	return false
}

func (s Selection) appNames() string {
	var n []string
	for _, a := range s.Apps {
		if v, ok := profile.AppNames[a]; ok {
			n = append(n, v)
		} else {
			n = append(n, a)
		}
	}
	return strings.Join(n, ", ")
}

// Select splits the inventory into the files to copy and the ones left out (rel → why).
func Select(inv scan.Inventory, s Selection) (selected []scan.Item, excluded map[string]string) {
	ex := s.excludeIDs()
	excluded = map[string]string{}
	for _, it := range inv.Items {
		switch {
		case it.Suggest != "" && ex[it.Suggest]:
			excluded[it.Rel] = it.Suggest
		case !s.inApps(it.Rel):
			excluded[it.Rel] = "not a " + s.appNames() + " file (you chose " + s.appNames() + " only)"
		case !s.wantsType(it.Cat):
			excluded[it.Rel] = "type not selected: " + string(it.Cat)
		case s.SkipEmpty && it.Size == 0:
			excluded[it.Rel] = "empty file (0 bytes) — you chose not to copy empty files"
		default:
			selected = append(selected, it)
		}
	}
	return selected, excluded
}

// Count is how many files and bytes a selection would copy (shown next to each choice).
func Count(inv scan.Inventory, s Selection) (files int, bytes int64) {
	sel, _ := Select(inv, s)
	for _, it := range sel {
		bytes += it.Size
	}
	return len(sel), bytes
}
