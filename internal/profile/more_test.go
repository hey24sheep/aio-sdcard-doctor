package profile

import "testing"

// #8: real camera/phone formats missing from the first tables.
func TestMoreExtensions(t *testing.T) {
	cases := map[string]Category{"hif": Photos, "mpo": Photos, "360": Videos, "264": Videos, "h264": Videos, "dav": Videos,
		"3ga": Music, "m4b": Music, "crypt12": ChatBackups, "crypt14": ChatBackups, "crypt15": ChatBackups}
	for ext, want := range cases {
		if got, ok := ByExt(ext); !ok || got != want {
			t.Errorf("%s → %v %v, want %v", ext, got, ok, want)
		}
	}
}

// #12: Telegram's website and beta builds use their own package folders.
func TestTelegramPackages(t *testing.T) {
	for _, rel := range []string{"Android/media/org.telegram.messenger.web/Telegram/a.jpg",
		"Android/media/org.telegram.messenger.beta/Telegram/b.jpg", "Pictures/Telegram/c.jpg"} {
		if !InApp("telegram", rel) {
			t.Errorf("%s not Telegram", rel)
		}
	}
}
