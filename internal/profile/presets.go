package profile

import (
	"regexp"
	"strings"
)

// Preset is a backup choice the user picks (the CLI's --backup, the first question in the app).
// Nothing is ever chosen for the user: scripts must name a preset or custom types.
type Preset struct {
	ID, Title, Desc string
	Types           []Category // nil = every category, Other included
	Apps            []string   // only files belonging to these apps (AppIDs)
	SkipJunk        bool       // also skip the recommended suggestions (thumbnails, system folders…)
}

var personal = []Category{Photos, Videos, Music, Documents, Archives, ChatBackups}

// Presets in menu order. "custom" is not a preset: the user ticks types and skips.
var Presets = []Preset{
	{ID: "quick", Title: "Quick", Desc: "Photos, documents and chat backups — smallest and fastest",
		Types: []Category{Photos, Documents, ChatBackups}, SkipJunk: true},
	{ID: "standard", Title: "Standard", Desc: "All photos, videos, music, documents, archives and chat backups — skips thumbnails, system junk and unknown files",
		Types: personal, SkipJunk: true},
	{ID: "full", Title: "Full", Desc: "Every file on the card, nothing skipped"},
	{ID: "photos", Title: "Photos only", Desc: "Photos (incl. camera RAW)", Types: []Category{Photos}, SkipJunk: true},
	{ID: "videos", Title: "Videos only", Desc: "Videos", Types: []Category{Videos}, SkipJunk: true},
	{ID: "music", Title: "Music only", Desc: "Music and audio", Types: []Category{Music}, SkipJunk: true},
	{ID: "documents", Title: "Documents only", Desc: "PDFs, office files, text", Types: []Category{Documents}, SkipJunk: true},
	{ID: "whatsapp", Title: "WhatsApp only", Desc: "WhatsApp photos, videos, voice notes, documents and chat backups", Apps: []string{"whatsapp"}, SkipJunk: true},
	{ID: "telegram", Title: "Telegram only", Desc: "Telegram photos, videos, music and documents", Apps: []string{"telegram"}, SkipJunk: true},
}

// PresetIDs lists the preset ids in menu order.
func PresetIDs() []string {
	ids := make([]string, len(Presets))
	for i, p := range Presets {
		ids[i] = p.ID
	}
	return ids
}

// PresetByID finds a preset (case-insensitive).
func PresetByID(id string) (Preset, bool) {
	for _, p := range Presets {
		if strings.EqualFold(p.ID, id) {
			return p, true
		}
	}
	return Preset{}, false
}

// AppNames are the display names of the app filters.
var AppNames = map[string]string{"whatsapp": "WhatsApp", "telegram": "Telegram"}

// appPaths are the folders each app keeps its files in (old and new Android layouts).
var appPaths = map[string]*regexp.Regexp{
	"whatsapp": regexp.MustCompile(`(?i)(^|/)WhatsApp( Business)?/|^Android/media/com\.whatsapp(\.w4b)?/`),
	"telegram": regexp.MustCompile(`(?i)(^|/)Telegram( X)?/|^Android/(media|data)/org\.(telegram\.messenger(\.\w+)?|thunderdog\.challegram)/`),
}

// InApp reports whether rel belongs to the app.
func InApp(app, rel string) bool {
	re, ok := appPaths[app]
	return ok && re.MatchString(rel)
}
