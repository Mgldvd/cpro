package main

import "strings"

// borderTheme is the fixed set of border runes every panel in cpro draws
// with — the shared abstraction "Theme" (cpro config) picks between, so no
// screen ever hardcodes "╭─"/"│"/"╰─" (or any other theme's own runes)
// itself. Only the runes vary between themes; color never does — every
// caller still applies its own accent/status color the same way regardless
// of which theme is active (see renderPanel, tui.go, and renderRootPanel,
// rootui.go). TopRight/BottomRight (decision 0033) are the closing-corner
// counterparts to TopLeft/BottomLeft — every other screen in cpro draws only
// a left rail with no closing right border (renderPanel, tui.go), but cpro
// status's full-view account cards are genuinely boxed on both sides (see
// renderFullView, main.go), so they need the matching right corner for
// whichever theme is active rather than a hardcoded "╮"/"╯". Vertical itself
// already doubles as both the left and right rail rune (it's the same
// character on both sides in every theme), so no separate right-rail field
// is needed.
type borderTheme struct {
	Name                                           string
	TopLeft, Horizontal, Vertical, BottomLeft, Tee string
	TopRight, BottomRight                          string
}

// Top/Bottom/Rail/Divider are the actual two-or-one-rune sequences a screen
// draws with — "╭─"/"╰─"/"│"/"├─" for Rounded, and so on — rather than
// exposing the raw fields for every caller to concatenate itself.
func (t borderTheme) Top() string     { return t.TopLeft + t.Horizontal }
func (t borderTheme) Bottom() string  { return t.BottomLeft + t.Horizontal }
func (t borderTheme) Rail() string    { return t.Vertical }
func (t borderTheme) Divider() string { return t.Tee + t.Horizontal }

// borderThemes is the full, fixed set of themes cpro config's Theme picker
// offers, in display order. Rounded (index 1) is the original, and remains
// the default — see currentTheme and applyPreferences (ui.go).
var borderThemes = []borderTheme{
	{"Minimal", "┌", "─", "│", "└", "├", "┐", "┘"},
	{"Rounded", "╭", "─", "│", "╰", "├", "╮", "╯"},
	{"Heavy", "┏", "━", "┃", "┗", "┣", "┓", "┛"},
	{"Double", "╔", "═", "║", "╚", "╠", "╗", "╝"},
}

// currentTheme is the live, in-effect border theme every panel in cpro
// renders with (renderPanel, tui.go; renderRootPanel, rootui.go; the plain
// accent()-printed panels in main.go/maintenance.go) — Rounded by default,
// overridden once at startup by applyPreferences from cpro config theme
// (ui.go), exactly like accentMode/barColorSafe.
var currentTheme = borderThemes[1]

// themeByName resolves a theme by name, case-insensitively — used by
// applyPreferences and the scriptable "cpro config theme NAME".
func themeByName(name string) (borderTheme, bool) {
	for _, t := range borderThemes {
		if strings.EqualFold(t.Name, name) {
			return t, true
		}
	}
	return borderTheme{}, false
}

// themeNames returns borderThemes' names lowercased, for ValidArgs on the
// scriptable "cpro config theme" command, mirroring paletteNames (ui.go).
func themeNames() []string {
	names := make([]string, len(borderThemes))
	for i, t := range borderThemes {
		names[i] = strings.ToLower(t.Name)
	}
	return names
}
