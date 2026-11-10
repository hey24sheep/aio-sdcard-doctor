package profile

import "regexp"

// Suggestion is an exclusion the user may choose. It is never applied on its own.
type Suggestion struct {
	ID, Title, Why string
	Recommended    bool
}

type suggestRule struct {
	id string
	re *regexp.Regexp
}

// suggestOrder is the match order (first match wins) and the display order.
var suggestOrder = []suggestRule{
	{"thumbnails", regexp.MustCompile(`(?i)(^|/)\.thumbnails/`)},
	{"system-folders", regexp.MustCompile(`(?i)(^|/)(System Volume Information|\$RECYCLE\.BIN|\.Trashes|\.Spotlight-V100|\.fseventsd)/`)},
	{"mac-metadata", regexp.MustCompile(`(?i)(^|/)\._`)},
	{"camera-cache", regexp.MustCompile(`(?i)^CANONMSC/|^MISC/.*\.CTG$`)},
	{"android-app-data", regexp.MustCompile(`(?i)^Android/(data|obb)/`)},
	{"chkdsk-fragments", regexp.MustCompile(`(?i)^FOUND\.\d{3}/`)},
}

// Suggestions are the built-in exclusion suggestions by ID.
var Suggestions = map[string]Suggestion{
	"thumbnails":       {"thumbnails", "Thumbnail caches", "Gallery thumbnail cache, rebuilt by the phone", true},
	"system-folders":   {"system-folders", "System folders", "OS bookkeeping, not your files", true},
	"mac-metadata":     {"mac-metadata", "macOS metadata files (._*)", "macOS resource-fork stubs", true},
	"camera-cache":     {"camera-cache", "Camera index cache", "Camera index cache", true},
	"android-app-data": {"android-app-data", "Android app data (Android/data, obb)", "App caches/game data — usually re-downloadable but can hold app downloads", false},
	"chkdsk-fragments": {"chkdsk-fragments", "chkdsk fragments (FOUND.000)", "chkdsk-recovered pieces; sniffed like LOST.DIR, may contain photos", false},
}

// SuggestionIDs lists suggestion IDs in display order.
func SuggestionIDs() []string {
	ids := make([]string, len(suggestOrder))
	for i, s := range suggestOrder {
		ids[i] = s.id
	}
	return ids
}

func suggestFor(rel string) string {
	for _, s := range suggestOrder {
		if s.re.MatchString(rel) {
			return s.id
		}
	}
	return ""
}
