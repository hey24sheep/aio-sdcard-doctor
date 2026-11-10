package app

// Theme is the colour tokens both front ends use (lipgloss in the TUI, CSS custom properties in
// the GUI). Contrast on paper, measured 2026-11-10 (WCAG): light — ink 14.84, steel 5.01, signal
// 6.18, safe 4.88, risk 5.77, fail 5.99; dark — ink 14.73, steel 7.22, signal 7.11, safe 7.93,
// risk 9.48, fail 6.91. Text on signal/fail buttons uses on-accent (white in light, paper in dark).
var Theme = map[string][2]string{ // token → {light, dark}
	"paper":     {"#F2F5F3", "#121A21"},
	"ink":       {"#17212B", "#E6ECEF"},
	"steel":     {"#5B6B78", "#9AA8B3"},
	"signal":    {"#1F4FD1", "#7EA2FF"},
	"safe":      {"#1D7A45", "#4CC38A"},
	"risk":      {"#8A5300", "#F0B44C"},
	"fail":      {"#B42318", "#FF7A6E"},
	"on-accent": {"#FFFFFF", "#121A21"},
}
