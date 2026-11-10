// Package profile classifies files into categories and knows card layouts (Android, cameras,
// action cams, drones). Profiles only suggest exclusions; nothing is skipped unless the user
// chooses it.
package profile

// Category is the top-level output folder a file is copied into.
type Category string

const (
	Photos      Category = "Photos"
	Videos      Category = "Videos"
	Music       Category = "Music"
	Documents   Category = "Documents"
	Archives    Category = "Archives"
	ChatBackups Category = "Chat-Backups"
	Other       Category = "Other"
)

// Priority is the copy order: most precious and smallest-risk first (spec order; the place of
// Archives is an assumption — the spec order omits it).
var Priority = []Category{Photos, Documents, Archives, ChatBackups, Music, Videos, Other}

// Rank returns c's position in Priority (unknown categories last).
func Rank(c Category) int {
	for i, p := range Priority {
		if p == c {
			return i
		}
	}
	return len(Priority)
}
