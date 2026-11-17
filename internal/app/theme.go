package app

// Theme is the colour tokens both front ends use (lipgloss in the TUI, CSS custom properties in
// the GUI's styles.css, which must match). Contrast measured 2026-11-17 (WCAG): light on paper —
// ink 16.25, steel 5.54, signal 6.60, safe 5.52, risk 5.85, fail 6.08 (on a white terminal 17.57,
// 5.99, 7.13, 5.97, 6.33, 6.57); dark on paper — ink 15.71, steel 7.98, signal 7.80, safe 8.30,
// risk 9.92, fail 7.23 (on a #1E1E1E terminal 14.25, 7.24, 7.08, 7.53, 9.00, 6.56). Text on
// signal/fail buttons uses on-accent (white in light, paper in dark).
var Theme = map[string][2]string{ // token → {light, dark}
	"paper":     {"#F5F6F8", "#0E151C"},
	"ink":       {"#101A24", "#E8EEF3"},
	"steel":     {"#566573", "#9DADBA"},
	"signal":    {"#2347D6", "#86A6FF"},
	"safe":      {"#17723F", "#4CC38A"},
	"risk":      {"#8A5300", "#F0B44C"},
	"fail":      {"#B42318", "#FF7A6E"},
	"on-accent": {"#FFFFFF", "#0E151C"},
}
