package main

import (
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/fang"
	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"
)

func terminalOutput(w io.Writer) bool {
	if profiled, ok := w.(*colorprofile.Writer); ok {
		return terminalOutput(profiled.Forward)
	}
	if crlf, ok := w.(crlfWriter); ok {
		return terminalOutput(crlf.w)
	}
	f, ok := w.(*os.File)
	return ok && term.IsTerminal(f.Fd())
}

func uiOutput(f *os.File) io.Writer {
	_, noColor := os.LookupEnv("NO_COLOR")
	if noColor || !terminalOutput(f) || os.Getenv("TERM") == "dumb" {
		return &colorprofile.Writer{Forward: f, Profile: colorprofile.NoTTY}
	}
	return f
}

func accent(w io.Writer, text, color string) string {
	if _, disabled := os.LookupEnv("NO_COLOR"); disabled || !terminalOutput(w) || os.Getenv("TERM") == "dumb" {
		return text
	}
	return lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(color)).Render(text)
}

// usageColorFor returns the accent color a usage percentage should render in:
// barColorSafe below barWarnThreshold, warningColor from there, dangerColor
// from barDangerThreshold — the single source of truth for that threshold
// logic, shared by every usage indicator (the proportional bars below, cpro
// status's full and compact views, and their narrow single-glyph fallback).
// Both warningColor and dangerColor are user-configurable (cpro config,
// default Orange/Red) — never hardcoded here.
func usageColorFor(value float64) string {
	switch {
	case value >= barDangerThreshold:
		return dangerColor
	case value >= barWarnThreshold:
		return warningColor
	default:
		return barColorSafe
	}
}

// usageBarWidth renders value as a proportional bar of exactly width glyphs
// (█ filled, ░ empty), colored by usageColorFor.
func usageBarWidth(w io.Writer, value float64, width int) string {
	value = max(0, min(100, value))
	filled := int(value/100*float64(width) + .5)
	filled = max(0, min(width, filled))
	bar := strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
	return accent(w, bar, usageColorFor(value))
}

// usageBar renders value as cpro list's full-view 20-glyph proportional bar.
func usageBar(w io.Writer, value float64) string {
	return usageBarWidth(w, value, 20)
}

// usageGlyph is the single-block indicator cpro list's compact-narrow layout falls
// back to when the terminal can't fit a proportional bar: always one solid █,
// colored by usageColorFor like the proportional bars — the color carries the
// signal instead of the fill.
func usageGlyph(w io.Writer, value float64) string {
	return accent(w, "█", usageColorFor(max(0, min(100, value))))
}

// pctText formats value as a whole-percent string, clamped to [0,100].
func pctText(value float64) string {
	return fmt.Sprintf("%.0f%%", max(0, min(100, value)))
}

// outputWidth returns the terminal column width behind w, or 0 when w isn't a real
// terminal or its size can't be determined — cpro list's full/compact/
// compact-narrow responsive rendering treats 0 as "unconstrained" (always the
// widest layout), since a non-terminal writer (redirected output, cpro watch's
// piped-to-a-file case) has no meaningful column count to fit.
func outputWidth(w io.Writer) int {
	if !terminalOutput(w) {
		return 0
	}
	switch v := w.(type) {
	case *colorprofile.Writer:
		return outputWidth(v.Forward)
	case crlfWriter:
		return outputWidth(v.w)
	case *os.File:
		width, _, err := term.GetSize(v.Fd())
		if err != nil || width <= 0 {
			return 0
		}
		return width
	default:
		return 0
	}
}

// padEnd right-pads s with spaces until its visible (ANSI-aware, via
// visibleWidth) width reaches width; s already at or beyond width is returned
// unchanged. Used to align a name/label column even when it may carry color
// styling (e.g. an expired account's red email).
func padEnd(s string, width int) string {
	if pad := width - visibleWidth(s); pad > 0 {
		return s + strings.Repeat(" ", pad)
	}
	return s
}

// padStart is padEnd's left-padding sibling: it right-aligns s within width
// visible columns. Used for a fixed-width numeric column (RUN ACCOUNT's own
// "  0%"/" 79%"/"100%" Session percentage, rootui.go) where the column must
// stay straight across rows regardless of how many digits a value has.
func padStart(s string, width int) string {
	if pad := width - visibleWidth(s); pad > 0 {
		return strings.Repeat(" ", pad) + s
	}
	return s
}

// truncatePath shortens a session's working directory to fit width visible
// columns, keeping the tail (the most specific, identifying part of a path) and
// marking the cut with a leading "…" — used by cpro list's full view so a deep
// path doesn't blow out the panel's alignment on a narrow terminal.
func truncatePath(path string, width int) string {
	r := []rune(path)
	if width < 4 || len(r) <= width {
		return path
	}
	return "…" + string(r[len(r)-(width-1):])
}

// formatDuration renders d as whole days, hours, and minutes, dropping leading zero
// units (e.g. "5d 13h 42m", "2h 5m", "40m"). Negative durations render as "0m".
func formatDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	days := int(d / (24 * time.Hour))
	hours := int(d/time.Hour) % 24
	minutes := int(d/time.Minute) % 60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh %dm", days, hours, minutes)
	case hours > 0:
		return fmt.Sprintf("%dh %dm", hours, minutes)
	default:
		return fmt.Sprintf("%dm", minutes)
	}
}

// formatCountdown renders the time remaining until target (see formatDuration).
func formatCountdown(target time.Time) string {
	return formatDuration(time.Until(target))
}

// usageReset parses an ISO-8601 resets_at timestamp and, when valid, returns the
// local date/time it falls on (see fullResetLine, which pairs this with
// formatCountdown for the "Resets ..." line's two halves) and true. A blank or
// unparsable value (an account that hasn't started a usage window yet) reports
// false, telling the caller to omit the reset line entirely rather than print a
// blank one.
func usageReset(value string) (time.Time, bool) {
	if value == "" {
		return time.Time{}, false
	}
	reset, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, false
	}
	return reset, true
}

// windowElapsedText replaces a countdown whose reset time has already passed.
const windowElapsedText = "reset"

// windowElapsed reports whether win's reset time has passed. A live fetch
// always describes the current window, so this only happens with a stale
// value — one whose utilization belongs to a window that no longer exists
// and must not be shown as current (decision 0065).
func windowElapsed(win usageWindow) bool {
	reset, ok := usageReset(win.ResetsAt)
	return ok && !reset.After(time.Now())
}

// usageAge is how old a usage value is, e.g. "12m ago".
func usageAge(st usageStatus) string {
	return formatDuration(time.Since(st.FetchedAt)) + " ago"
}

// usageStaleNote is the line a status card adds under a stale value: its age
// and why it isn't current, with the one action that fixes an expired token
// (cpro never refreshes one — Claude Code does, when it runs). Empty when the
// value is live.
func usageStaleNote(st usageStatus) string {
	if !st.Stale {
		return ""
	}
	note := "updated " + usageAge(st)
	if st.Reason != "" {
		note += " · " + st.Reason
	}
	if st.Reason == "token expired" {
		note += " — run Claude on this account once"
	}
	return note
}

func doctorLine(w io.Writer, ok bool, name, detail string) string {
	mark, color := "✓", "#34D399"
	if !ok {
		mark, color = "✗", "#F87171"
	}
	return fmt.Sprintf("%s %-18s %s", accent(w, mark, color), name, detail)
}

func cliError(w io.Writer, styles fang.Styles, err error) {
	var child *exec.ExitError
	if errors.As(err, &child) {
		return
	} // Claude already printed its diagnostic.
	if errors.Is(err, huh.ErrUserAborted) {
		return
	} // Cancelling a picker (Esc/Ctrl+C) isn't an error worth an ERROR box.
	if !terminalOutput(w) {
		fmt.Fprintln(w, "Error:", err)
		return
	}
	styles.ErrorText = styles.ErrorText.UnsetTransform()
	fang.DefaultErrorHandler(w, styles, err)
}

func formAccessible() bool {
	_, noColor := os.LookupEnv("NO_COLOR")
	return os.Getenv("ACCESSIBLE") != "" || noColor || os.Getenv("TERM") == "dumb"
}

// accentRunning colors the "Running: ..." announcement (announceCommand,
// below) printed after the interactive run flow (rootui.go's RUN ACCOUNT/RUN
// MODE frames) picks an account and permission mode — fixed, not
// user-configurable, unlike accentMode/barColorSafe/warningColor/dangerColor
// below. Decision 0008's per-step account/mode picker colors are gone along
// with the huh-based pickAccount/pickRunMode they colored (decision 0019):
// the new flow is one continuous bubbletea screen styled like every other
// rootPickerApp frame, so only this final announcement keeps its own accent.
const accentRunning = "#F472B6"

// accentMode is cpro's general/default accent: the run-mode picker, the top-level
// command picker, headers, and most plain confirmation lines. barColorSafe is the
// usage bar's color below warningColor/dangerColor's own thresholds
// (barWarnThreshold/barDangerThreshold), which always take priority over it
// regardless of preference, so a near-limit warning is never silenced by
// theming. warningColor/dangerColor double as the "press Esc again to exit"
// two-step warning border everywhere that convention appears
// (escGuardField below, watchLoop, rootPickerApp's exitArmed rail) — using
// dangerColor there specifically, not warningColor, since exiting is the more
// severe of the two. All four default to their original fixed values (purple,
// green, orange, red) and are overridden once at startup by applyPreferences
// from cpro config accent/bar/warning-color/danger-color (see colorPalette) —
// everything else treats them as constants.
var (
	accentMode   = "#A78BFA"
	barColorSafe = "#34D399"
	warningColor = "#FB923C"
	dangerColor  = "#F87171"
)

// barWarnThreshold/barDangerThreshold are the usage percentages at which usageBar
// switches the bar to warningColor, then dangerColor — the near-limit warning that
// always overrides barColorSafe/theming (see usageBar). Default to 80/90 and are
// overridden once at startup by applyPreferences from cpro config warn-at/
// danger-at.
var (
	barWarnThreshold   = 80.0
	barDangerThreshold = 90.0
)

// maskEmailEnabled/emailMaskTable are the live, in-process source of truth
// for whether email masking is on and, if so, each account's own alias —
// mirroring accentMode/barColorSafe above: overridden once at startup by
// applyPreferences, then kept current for the rest of the run by
// saveMaskEmail (config.go, the only place either is persisted) so a toggle
// mid-session is reflected on the very next render, not just future
// invocations. displayEmail is the one formatter every UI/output path that
// shows an account's email must call — never a raw `email`/`account.Email`
// interpolation — so a screen can't accidentally bypass masking by reading
// config.MaskEmail itself instead of going through here.
var (
	maskEmailEnabled bool
	emailMaskTable   map[string]string
)

// displayEmail returns email unchanged when masking is off, or its
// registered alias (emailMaskTable) when on — falling back to the real
// email if a mask is somehow still missing (e.g. a config predating decision
// 0024's eager per-login backfill) rather than showing nothing at all. This
// is the single formatter decision 0024 requires every render site to go
// through instead of interpolating an account's email directly.
func displayEmail(email string) string {
	if !maskEmailEnabled {
		return email
	}
	if alias, ok := emailMaskTable[email]; ok {
		return alias
	}
	return email
}

// applyPreferences overrides accentMode/barColorSafe/warningColor/dangerColor/
// barWarnThreshold/barDangerThreshold/currentTheme from stored preferences. A
// blank/zero preference (never set, or reset) leaves the built-in default in
// place — this is also how an existing config predating any of these fields
// falls back to sensible defaults with no migration needed.
func applyPreferences(c config) {
	if c.AccentColor != "" {
		accentMode = c.AccentColor
	}
	if c.BarColor != "" {
		barColorSafe = c.BarColor
	}
	if c.WarningColor != "" {
		warningColor = c.WarningColor
	}
	if c.DangerColor != "" {
		dangerColor = c.DangerColor
	}
	if c.BarWarnThreshold > 0 {
		barWarnThreshold = c.BarWarnThreshold
	}
	if c.BarDangerThreshold > 0 {
		barDangerThreshold = c.BarDangerThreshold
	}
	if c.Theme != "" {
		if t, ok := themeByName(c.Theme); ok {
			currentTheme = t
		}
	}
	maskEmailEnabled = c.MaskEmail
	emailMaskTable = c.EmailMasks
}

// colorPalette is the fixed set of colors offered for every one of cpro's
// color preferences (accent, the usage bar's "safe" tier, and now the usage
// warning/danger tiers — cpro config accent/bar/warning-color/danger-color).
// Purple/Blue/Cyan/Green keep their original hex values from the previous
// 8-color palette (Yellow/Pink dropped) for any already-stored preference to
// keep resolving to the same name; Amber reuses the old Yellow hex, under its
// new name. Orange and Red were added specifically for Warning/Danger color
// (matching usageColorFor's own original hardcoded warn/danger hex values
// exactly, so picking the palette's "Orange"/"Red" for Warning/Danger color
// reproduces cpro's previous, non-configurable behavior bit for bit) — added
// here, in the one shared palette, rather than special-cased only for
// thresholds, so accent/bar can select them too if a user wants to.
var colorPalette = []struct{ Name, Hex string }{
	{"Purple", "#A78BFA"},
	{"Violet", "#8B5CF6"},
	{"Blue", "#60A5FA"},
	{"Cyan", "#22D3EE"},
	{"Green", "#34D399"},
	{"Amber", "#FBBF24"},
	{"Orange", "#FB923C"},
	{"Red", "#F87171"},
	{"Rose", "#FB7185"},
}

// paletteNames returns colorPalette's names lowercased, for ValidArgs on the
// scriptable cpro config accent/bar commands.
func paletteNames() []string {
	names := make([]string, len(colorPalette))
	for i, c := range colorPalette {
		names[i] = strings.ToLower(c.Name)
	}
	return names
}

// colorByName resolves a palette color by name, case-insensitively.
func colorByName(name string) (string, error) {
	for _, c := range colorPalette {
		if strings.EqualFold(c.Name, name) {
			return c.Hex, nil
		}
	}
	return "", fmt.Errorf("unknown color %q; choose one of %s", name, strings.Join(paletteNames(), ", "))
}

// colorName resolves a hex color back to its palette name, for display; a hex
// value outside the palette (shouldn't normally happen, since it's only ever set
// via the config UI or colorByName) is shown as-is.
func colorName(hex string) string {
	for _, c := range colorPalette {
		if c.Hex == hex {
			return c.Name
		}
	}
	return hex
}

// emailMaskUsers and emailMaskHosts are word banks for building a random,
// pronounceable placeholder email (e.g. "kufi@ponuri") when cpro config's "Mask
// emails" is on, so a real address can be hidden from a shared screen without
// blanking the line. Indexed A-Z x 3 options, matching how they were supplied; the
// letter and option are both picked at random in newEmailMask, so which row/column
// contributed isn't meaningful on its own.
var emailMaskUsers = [26][3]string{
	{"Avir", "Anef", "Atul"}, {"Benu", "Bire", "Bofu"}, {"Cefu", "Cire", "Cune"},
	{"Dafi", "Dovu", "Denu"}, {"Ebal", "Enuf", "Evir"}, {"Fenu", "Firu", "Foba"},
	{"Gavi", "Genu", "Gofu"}, {"Habu", "Heni", "Hovu"}, {"Ibel", "Inuf", "Ivor"},
	{"Jafi", "Jenu", "Jovu"}, {"Kebu", "Kiro", "Kufi"}, {"Lafu", "Leni", "Loru"},
	{"Mebi", "Mofu", "Munel"}, {"Nafi", "Neku", "Nubi"}, {"Obel", "Ofir", "Onuf"},
	{"Pavi", "Peku", "Ponu"}, {"Quef", "Quir", "Quon"}, {"Rabu", "Reni", "Rofu"},
	{"Savi", "Seku", "Sonu"}, {"Tebi", "Tafu", "Tovu"}, {"Ubel", "Ufir", "Unel"},
	{"Vafi", "Veku", "Vonu"}, {"Wabu", "Weni", "Wofu"}, {"Xalu", "Xefi", "Xonu"},
	{"Yabi", "Yenu", "Yofu"}, {"Zafi", "Zeku", "Zonu"},
}

var emailMaskHosts = [26][3]string{
	{"Avirel", "Anefur", "Atulon"}, {"Benuri", "Bireto", "Bofali"}, {"Cefuro", "Cirane", "Cunelo"},
	{"Dafiru", "Dovena", "Denulo"}, {"Ebalun", "Enufar", "Evirel"}, {"Fenuri", "Firalo", "Fobena"},
	{"Gaviru", "Genalo", "Gofeni"}, {"Haburo", "Henali", "Hovena"}, {"Ibelun", "Inufar", "Ivoren"},
	{"Jafiru", "Jenalo", "Jovena"}, {"Keburo", "Kirafi", "Kufeno"}, {"Lafiru", "Lenavo", "Loruni"},
	{"Mebiru", "Mofena", "Munelo"}, {"Nafiru", "Nekavo", "Nubeli"}, {"Obelun", "Ofiral", "Onufar"},
	{"Paviru", "Pekano", "Poneli"}, {"Quevul", "Quenor", "Quinel"}, {"Rabiru", "Renavo", "Rofeni"},
	{"Saviru", "Sekano", "Soneli"}, {"Tebiru", "Tafeno", "Toveli"}, {"Ubelar", "Ufiron", "Unelio"},
	{"Vafiru", "Vekano", "Voneli"}, {"Wabiru", "Wenalo", "Wofeni"}, {"Xaluro", "Xefani", "Xoneli"},
	{"Yabiru", "Yenalo", "Yofeni"}, {"Zafiru", "Zekano", "Zoneli"},
}

// newEmailMask returns a fresh random "word@word" placeholder, lowercased to read
// like an email. Called once per account each time masking is toggled on (or off —
// see setMaskEmail) so the placeholder changes every time, and once more for any
// account still missing one (added after the last toggle — see
// renderAccountSnapshot).
func newEmailMask() string {
	user := emailMaskUsers[rand.IntN(26)][rand.IntN(3)]
	host := emailMaskHosts[rand.IntN(26)][rand.IntN(3)]
	return strings.ToLower(user) + "@" + strings.ToLower(host)
}

// accentTheme is ThemeCharm with the focused border, title, and selection cursor
// recolored to accent (so each step of a multi-step picker can carry its own color,
// unified with the plain-printed lines announceLine draws after the form exits) and
// the cursor glyph changed to "❯ " to match the rest of cpro's output.
func accentTheme(accent string) huh.Theme {
	return huh.ThemeFunc(func(isDark bool) *huh.Styles {
		t := *huh.ThemeCharm(isDark)
		color := lipgloss.Color(accent)
		t.Focused.Base = t.Focused.Base.BorderForeground(color)
		t.Focused.Title = t.Focused.Title.Foreground(color)
		t.Focused.SelectSelector = lipgloss.NewStyle().Foreground(color).SetString("❯ ")
		t.Focused.NextIndicator = t.Focused.NextIndicator.Foreground(color)
		t.Focused.PrevIndicator = t.Focused.PrevIndicator.Foreground(color)
		t.Focused.FocusedButton = t.Focused.FocusedButton.Background(color)
		t.Group.Title = t.Focused.Title
		return &t
	})
}

// escWarnWindow is how long a field stays armed after a first Esc: a second Esc
// within this window exits the picker; anything else lets it lapse back to normal.
const escWarnWindow = 3 * time.Second

// escWarnPoll re-checks an armed warning this often to clear it on its own once
// escWarnWindow has elapsed, instead of only reacting to the next keypress. It's a
// short repeating poll rather than one tea.Tick(escWarnWindow, ...): a bubbletea Cmd
// keeps a goroutine alive that the Program waits on during shutdown, and scheduling
// one for the whole window measurably delayed exiting on a confirming Esc (up to
// however much of the window was left — confirmed live). Rescheduling every
// escWarnPoll instead bounds that worst case to one short poll.
const escWarnPoll = 150 * time.Millisecond

// escGuardField wraps a Field so one Esc doesn't cancel the picker outright: the
// first Esc shows a red warning bar (see View) that clears on its own after
// escWarnWindow (see escWarnPollMsg); a second Esc within that window aborts the
// field exactly like Ctrl+C (tea.Interrupt), which every picker already surfaces as
// its usual "selection cancelled" error — nothing downstream needs to change. Any
// other key clears the warning immediately and the picker carries on untouched. A
// Select already binds a bare Esc to leaving filter-typing mode; that keeps its
// normal meaning here (an armed warning is not triggered while GetFiltering() is
// true), so it takes two separate, deliberate Esc presses to actually exit.
//
// It also starts filtering (a Select) on the first typed character instead of
// requiring "/" first: on a fresh, non-filtering field, a typed rune is preceded by
// a synthesized "/" keypress. That's deliberately routed through the field's own
// Update — not the Select.Filtering(true) builder — because Filtering(true) only
// sets the internal flag; it never runs the enable/disable step a real "/" keypress
// does (setFiltering, in field_select.go), which is what makes Esc mean "stop
// filtering" afterward. Set unconditionally instead, GetFiltering() would report
// true forever and the Esc handling above would never arm — confirmed live.
type escGuardField struct {
	huh.Field
	warnUntil time.Time
}

// escWarn wraps field with escGuardField.
func escWarn(field huh.Field) huh.Field {
	return &escGuardField{Field: field}
}

func (f *escGuardField) armed() bool {
	return !f.warnUntil.IsZero() && time.Now().Before(f.warnUntil)
}

// escWarnPollMsg drives escGuardField's self-clearing warning: deadline pins it to
// the arm that scheduled it, so a stale poll from an earlier, already-superseded
// arm/disarm is a no-op instead of clearing (or rescheduling) the current one.
type escWarnPollMsg struct{ deadline time.Time }

func escWarnPollCmd(deadline time.Time) tea.Cmd {
	return tea.Tick(escWarnPoll, func(time.Time) tea.Msg { return escWarnPollMsg{deadline} })
}

// isTypingRune reports whether km represents a printable character rather than a
// navigation key (arrows, enter, tab, ...) or a Ctrl/Alt-modified one — Key.Text is
// documented as set exactly for the former.
func isTypingRune(km tea.KeyMsg) bool {
	return km.Key().Text != ""
}

func (f *escGuardField) Update(msg tea.Msg) (huh.Model, tea.Cmd) {
	if poll, ok := msg.(escWarnPollMsg); ok {
		if poll.deadline.Equal(f.warnUntil) {
			if f.armed() {
				return f, escWarnPollCmd(f.warnUntil)
			}
			f.warnUntil = time.Time{}
		}
		return f, nil
	}
	var startFilter tea.Cmd
	if km, ok := msg.(tea.KeyMsg); ok {
		filtering := false
		filterer, canFilter := f.Field.(interface{ GetFiltering() bool })
		if canFilter {
			filtering = filterer.GetFiltering()
		}
		switch {
		case km.String() == "esc":
			if !filtering {
				if f.armed() {
					return f, tea.Interrupt
				}
				f.warnUntil = time.Now().Add(escWarnWindow)
				return f, escWarnPollCmd(f.warnUntil)
			}
		case canFilter && !filtering && km.String() != "/" && isTypingRune(km):
			var inner huh.Model
			inner, startFilter = f.Field.Update(tea.KeyPressMsg{Text: "/", Code: '/'})
			if field, ok := inner.(huh.Field); ok {
				f.Field = field
			}
			f.warnUntil = time.Time{}
		default:
			f.warnUntil = time.Time{}
		}
	}
	inner, cmd := f.Field.Update(msg)
	if field, ok := inner.(huh.Field); ok {
		f.Field = field
	}
	return f, tea.Batch(startFilter, cmd)
}

// View prepends a red warning bar while armed. huh.Field.WithTheme is a one-shot
// setter (a second call on the same field is a no-op, by design — see
// field_select.go), so the field's own border can't be recolored after the form's
// initial theme pass; a matching "┃"-prefixed red bar (the same convention
// announceLine uses for plain printed lines) is the closest equivalent.
func (f *escGuardField) View() string {
	view := f.Field.View()
	if !f.armed() {
		return view
	}
	bar := lipgloss.NewStyle().Foreground(lipgloss.Color(dangerColor)).Render("┃")
	warning := bar + " " + lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(dangerColor)).Render("Press Esc again to exit cpro")
	return warning + "\n" + view
}

// runSelect drives a single-group Huh form on stderr, using the same accessible/TTY
// rules as the rest of the picker UI, themed in accent.
func runSelect(cmd *cobra.Command, group *huh.Group, accent string) error {
	accessible := formAccessible()
	form := huh.NewForm(group).
		WithTheme(accentTheme(accent)).WithAccessible(accessible).
		WithInput(os.Stdin).WithOutput(cmd.ErrOrStderr())
	if accessible {
		w := colorprofile.NewWriter(cmd.ErrOrStderr(), os.Environ())
		w.Profile = colorprofile.NoTTY
		form.WithOutput(w)
	}
	return form.RunWithContext(cmd.Context())
}

// requiredArgTokens returns the placeholder names of cmd's mandatory positional
// arguments, derived from its Use string: tokens outside [] brackets that don't
// start with "--" (e.g. "EMAIL" in "login EMAIL"). Bracketed or flag tokens (e.g.
// "[EMAIL]", "[--account EMAIL]") are optional and skipped.
func requiredArgTokens(use string) []string {
	tokens := strings.Fields(use)
	if len(tokens) <= 1 {
		return nil
	}
	var required []string
	depth := 0
	for _, tok := range tokens[1:] {
		depth += strings.Count(tok, "[") - strings.Count(tok, "]")
		if depth == 0 && !strings.ContainsAny(tok, "[]") && !strings.HasPrefix(tok, "--") {
			required = append(required, tok)
		}
	}
	return required
}

// pickRequiredArgs prompts for cmd's required positional arguments (see
// requiredArgTokens): a select from cmd.ValidArgs when there is exactly one
// required argument and ValidArgs is set (e.g. "config trust {on|off}"), otherwise
// one free-text input per argument (e.g. "login EMAIL").
func pickRequiredArgs(root, cmd *cobra.Command) ([]string, error) {
	names := requiredArgTokens(cmd.Use)
	if len(names) == 0 {
		return nil, nil
	}
	if len(names) == 1 && len(cmd.ValidArgs) > 0 {
		options := make([]huh.Option[string], 0, len(cmd.ValidArgs))
		for _, v := range cmd.ValidArgs {
			options = append(options, huh.NewOption(v, v))
		}
		value := cmd.ValidArgs[0]
		if err := runSelect(root, huh.NewGroup(escWarn(huh.NewSelect[string]().
			Title(cmd.Name()).Description(cmd.Short).Options(options...).Value(&value))), accentMode); err != nil {
			return nil, fmt.Errorf("selection cancelled: %w", err)
		}
		return []string{value}, nil
	}
	values := make([]string, len(names))
	fields := make([]huh.Field, len(names))
	for i, name := range names {
		input := huh.NewInput().Title(name)
		if name == "EMAIL" {
			input = input.Placeholder("you@example.com").Validate(func(s string) error {
				_, err := normalizeEmail(s)
				return err
			})
		}
		fields[i] = escWarn(input.Value(&values[i]))
	}
	if err := runSelect(root, huh.NewGroup(fields...), accentMode); err != nil {
		return nil, fmt.Errorf("selection cancelled: %w", err)
	}
	return values, nil
}

// shellArg renders s as a shell word safe to paste into a terminal: unquoted when it
// only contains characters that never need quoting (true for almost every email),
// single-quoted otherwise.
func shellArg(s string) string {
	safe := s != ""
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("@._+-", r)) {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// announceCommand prints what is about to run, for the account and args (the
// chosen mode's forwarded arguments, if any), purely as information — cpro is
// already about to execute it, so this is not copied anywhere or paired with
// any clipboard confirmation/failure message. It never blocks or otherwise
// affects the actual run. See announceTarget for the one thing masking changes
// about it.
//
// Drawn in currentTheme's own runes (theme.go) — "╭─ Running:" / "│  ..." /
// "╰─" — the same accent(w, currentTheme.Top()/.Rail()/.Bottom(), color)
// idiom cpro info/doctor (maintenance.go) and cpro list/status (main.go)
// already use for a plain, non-bubbletea printed panel, so Theme applies
// here too instead of a fixed "┃" bar unrelated to it (the pre-decision-0037
// shape, via the now-removed announceLine). This is the one place that
// idiom draws with accentRunning instead of accentMode — this announcement
// keeps its own fixed, non-configurable accent (see accentRunning's own
// comment), Theme only ever changing which corner/rail runes are drawn, never
// which color they're drawn in.
func announceCommand(cmd *cobra.Command, email string, args []string) {
	w := cmd.OutOrStdout()
	cmd.Println(accent(w, currentTheme.Top()+" Running:", accentRunning))
	cmd.Println(accent(w, currentTheme.Rail(), accentRunning) + "  " + announceTarget(email, args))
	cmd.Println(accent(w, currentTheme.Bottom(), accentRunning))
}

// announceTarget renders the body of the announcement. With masking off it is
// the literal, copy-pasteable `cpro run --account EMAIL <flags>` cpro is about
// to execute. With masking on a command shape would be a lie: an alias is a
// display string, never an account identifier cpro run accepts (see
// store.resolve), so that line would fail with "account is not registered" if
// anyone actually ran it. So the masked form drops the command shape entirely
// and states the same facts as plain status text — the masked account and its
// permission mode's own label — keeping the account hidden without printing a
// command that doesn't work. The real email is still what gets exec'd either
// way; only this one line's wording changes.
func announceTarget(email string, args []string) string {
	if !maskEmailEnabled {
		parts := append([]string{"cpro", "run", "--account", shellArg(email)}, args...)
		return strings.Join(parts, " ")
	}
	target := displayEmail(email)
	if mode, ok := permissionModeForArgs(args); ok {
		return target + " · " + mode.Label
	}
	return target
}
