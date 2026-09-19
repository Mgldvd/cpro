package main

import (
	"fmt"
	"image/color"
	"io"
	"os"
	"strings"

	"charm.land/lipgloss/v2"
)

// navStack is the one reusable navigation history every hierarchical cpro TUI
// screen shares — push a new frame, pop back to the previous one, read
// what's current — instead of each screen (the root/menu picker, cpro
// config) reinventing its own one-off "go back a level" mechanism (a lone
// bool flag, a single "return to X" field). pop is a no-op (returns false)
// at the last remaining frame: that frame is this stack's own root, and
// callers use atRoot/the false return to decide whether Esc there means
// "back" or "exit" instead of ever popping past it.
type navStack[T any] struct{ frames []T }

// newNavStack creates a stack containing only root — the frame Esc/pop can
// never remove.
func newNavStack[T any](root T) navStack[T] { return navStack[T]{frames: []T{root}} }

// push adds a new current frame on top of the stack.
func (s *navStack[T]) push(f T) { s.frames = append(s.frames, f) }

// pop removes the current frame and reveals the one beneath it, reporting
// whether it actually did so — false at the root frame, which is never
// removed.
func (s *navStack[T]) pop() bool {
	if len(s.frames) <= 1 {
		return false
	}
	s.frames = s.frames[:len(s.frames)-1]
	return true
}

// current returns a pointer to the top frame, so a caller can mutate it in
// place (e.g. a list screen's own cursor) without a separate setter.
func (s *navStack[T]) current() *T { return &s.frames[len(s.frames)-1] }

// atRoot reports whether this stack is down to its one root frame — the
// point at which Esc/pop should mean "exit" rather than "back".
func (s *navStack[T]) atRoot() bool { return len(s.frames) <= 1 }

// screenTitle builds the "claude cpro[ - NAME]" border title every cpro TUI
// screen shows — the border doubles as this screen's own location
// indicator, so nothing prints a second, separate title line above the
// panel. name == "" is reserved for the one true outermost screen (bare
// `cpro`'s reduced launcher); every other screen, including a nested screen
// launched directly with nothing above it (e.g. `cpro config` on its own),
// always carries its own name.
func screenTitle(name string) string {
	if name == "" {
		return "claude cpro"
	}
	return "claude cpro - " + name
}

// tuiColorEnabled decides whether a live bubbletea screen should apply ANSI
// styling to the given writer, mirroring accent()'s NO_COLOR/TERM=dumb/
// terminal-output gate. Every custom bubbletea screen in cpro (cpro config,
// the root command picker) computes this once at startup rather than
// rechecking per render, since the target writer doesn't change mid-program.
func tuiColorEnabled(w io.Writer) bool {
	_, noColor := os.LookupEnv("NO_COLOR")
	return !noColor && terminalOutput(w) && os.Getenv("TERM") != "dumb"
}

// styleText applies hex as a foreground color to text when color is true — a
// lipgloss equivalent of accent() for repeated use inside a live-rendering
// bubbletea View. An empty hex, or color == false, renders text unstyled.
func styleText(color bool, text, hex string) string {
	if !color || hex == "" {
		return text
	}
	return lipgloss.NewStyle().Foreground(lipgloss.Color(hex)).Render(text)
}

// renderPanel draws lines inside the minimal left-rule frame shared by every
// custom bubbletea screen in cpro: a top-left corner + title to open, a
// vertical rail in front of every line, a bottom-left corner to close —
// deliberately no right border and no full-width top/bottom rule, so it
// reads as one visual unit without looking like a boxed GUI widget. The
// actual runes always come from currentTheme (theme.go, user-configurable
// via cpro config), never hardcoded here — this is the one shared
// implementation every such screen (cpro config, the root picker's
// submenus, cpro system's account picker) draws its frame through, so
// Theme only ever needs to change in one place to apply everywhere.
func renderPanel(color bool, accentHex, title string, lines []string) string {
	bar := styleText(color, currentTheme.Rail(), accentHex)
	var b strings.Builder
	b.WriteString(styleText(color, currentTheme.Top()+" "+title, accentHex))
	for _, line := range lines {
		b.WriteByte('\n')
		if line == panelDividerLine {
			b.WriteString(styleText(color, currentTheme.Divider(), accentHex))
			continue
		}
		b.WriteString(bar)
		if line != "" {
			b.WriteString(" " + line)
		}
	}
	b.WriteByte('\n')
	b.WriteString(styleText(color, currentTheme.Bottom(), accentHex))
	return b.String()
}

// panelDividerLine is a sentinel a renderPanel caller puts in its lines slice
// to draw a real mid-panel divider (currentTheme.Divider(), e.g. "├─") in
// place of that row's rail — for a screen that groups its own rows into
// sections separated by more than a blank line (e.g. the SET DEFAULT ACCOUNT
// screen's mode/account list above its own description). Every existing
// renderPanel caller builds lines from real content or "", never this exact
// string, so the sentinel can never collide with genuine row text.
const panelDividerLine = "\x00panel-divider\x00"

// renderFooter renders the shared "key action ∙ key action" hint line. The
// caller separates it from the panel above with one blank line. A hint whose
// key is "" (e.g. "Type to search", which has no single key of its own) omits
// the key and its separating space rather than leaving a stray leading blank.
func renderFooter(color bool, accentHex string, hints ...[2]string) string {
	parts := make([]string, len(hints))
	for i, h := range hints {
		if h[0] == "" {
			parts[i] = h[1]
			continue
		}
		parts[i] = styleText(color, h[0], accentHex) + " " + h[1]
	}
	return strings.Join(parts, "  "+styleText(color, "∙", "")+"  ")
}

// dimStyle applies a dim/faint weight (SGR "faint") when color is true — the
// styleText equivalent for secondary information (e.g. the root picker's
// right-side command metadata) that should read as visually secondary without
// needing a hue of its own. A no-op, like styleText, when color is false.
func dimStyle(color bool, text string) string {
	if !color {
		return text
	}
	return lipgloss.NewStyle().Faint(true).Render(text)
}

// shadeDarkenMax bounds how much deriveAccentShades darkens its most subdued
// shade (index 0) — kept moderate on purpose: cpro's accent palette
// (colorPalette, ui.go) is already made of mid-to-bright pastel hues, so a
// conservative darken keeps every shade legible on both dark and light
// terminal backgrounds without either extreme risking disappearing. cpro has
// no existing terminal-background detection to lean on instead (ui.go's
// accent/styleText color model is a binary terminal-or-not switch, no
// graduated capability detection), so this fixed, conservative range is the
// deliberate fallback.
const shadeDarkenMax = 0.35

// deriveAccentShades returns n hex colors in the same hue family as accent,
// from the most subdued (index 0) up to accent itself, unchanged, at index
// n-1 — reusing lipgloss's own perceptual Lighten/Darken rather than
// hand-rolled RGB/HSL math, so every shade rides through exactly the same
// color system every other cpro accent color already does (accent()/
// styleText(), both ui.go). n<=1 returns accent unchanged.
func deriveAccentShades(accent string, n int) []string {
	if n <= 1 {
		return []string{accent}
	}
	base := lipgloss.Color(accent)
	shades := make([]string, n)
	for i := range shades {
		amount := shadeDarkenMax * float64(n-1-i) / float64(n-1)
		shades[i] = colorToHex(lipgloss.Darken(base, amount))
	}
	return shades
}

// colorToHex converts a color.Color (as returned by lipgloss.Darken/Lighten)
// back to cpro's own "#RRGGBB" color representation (colorPalette,
// accentMode, and every other color in ui.go).
func colorToHex(c color.Color) string {
	r, g, b, _ := c.RGBA()
	return fmt.Sprintf("#%02X%02X%02X", uint8(r>>8), uint8(g>>8), uint8(b>>8))
}

// visibleWidth is lipgloss.Width under cpro's own name, used wherever a custom
// screen measures or aligns a string that may already carry ANSI styling
// (e.g. a rendered "● Purple") — len() would count the escape bytes as width.
func visibleWidth(s string) int {
	return lipgloss.Width(s)
}

// truncateToWidth shortens s to fit within width cells, appending "…" when it
// had to cut anything. Used for the root picker's below-panel description,
// which is plain unstyled text, so a rune-count approximation of width is
// exact — real ANSI-aware measurement (visibleWidth) is reserved for text
// that might carry styling.
func truncateToWidth(s string, width int) string {
	if width <= 0 || len([]rune(s)) <= width {
		return s
	}
	if width == 1 {
		return "…"
	}
	r := []rune(s)
	return string(r[:width-1]) + "…"
}
