package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/spf13/cobra"
)

// configScreen identifies which panel configApp is currently showing.
type configScreen int

const (
	screenMenu configScreen = iota
	screenColor
	screenTheme
	screenThreshold
	screenThresholdMenu
	screenDefault
)

// configMenuItem is one selectable row on the main settings screen. section
// starts a new section header right before this item; empty continues the
// previous section without repeating its header.
type configMenuItem struct {
	key     string
	section string
	label   string
}

// configMenuItemsSettings is what the root/menu picker's "config →" opens
// (m.hasParent == true): preferences only. It omits "Default" — the same
// screen (SET DEFAULT ACCOUNT: permission mode, workspace trust, and
// account) is already one press away as the menu's own "default →" row, so
// offering it a second time here would just be noise.
var configMenuItemsSettings = []configMenuItem{
	{"mask", "", "Mask emails"},
	{"accent", "Appearance", "Accent"},
	{"bar", "", "Usage bar"},
	{"theme", "", "Theme"},
	{"warn", "Usage thresholds", "Warning"},
	{"danger", "", "Danger"},
}

// configMenuItemsConfig is what direct `cpro config` opens instead
// (m.hasParent == false): configMenuItemsSettings' own items, plus Default
// restored, leading — a direct `cpro config` has no menu to reach it from
// otherwise, so this view keeps it: it's the one place the whole surface is
// visible at once.
var configMenuItemsConfig = []configMenuItem{
	{"default", "", "Default"},
	{"mask", "", "Mask emails"},
	{"accent", "Appearance", "Accent"},
	{"bar", "", "Usage bar"},
	{"theme", "", "Theme"},
	{"warn", "Usage thresholds", "Warning"},
	{"danger", "", "Danger"},
}

// menuItems returns whichever of the two item sets above this configApp
// instance's own screenMenu should show — the lighter one when reached from
// the root/menu picker, the complete one for a direct `cpro config`
// invocation. The same `hasParent` flag that already distinguishes the two
// for Esc/backOut purposes (see the configApp type doc comment) is the
// correct signal here too: it's set once, at construction (runConfigUI),
// from exactly the same "was this opened from the picker" fact.
func (m *configApp) menuItems() []configMenuItem {
	if m.hasParent {
		return configMenuItemsSettings
	}
	return configMenuItemsConfig
}

// menuTitle is screenMenu's own border title. Always "CONFIG" regardless of
// hasParent — the item sets differ (each view omits the rows its own entry
// point already offers one press away), but the screen has a single name.
func (m *configApp) menuTitle() string { return "CONFIG" }

// isForwardConfigItem reports whether key's row opens a further screen
// (so → acts as an Enter alias on it, and the menu's footer advertises
// "→ Open") as opposed to an immediate in-place toggle ("mask") that
// → must never accidentally trigger.
func isForwardConfigItem(key string) bool {
	switch key {
	case "theme", "default", "accent", "bar", "warn", "danger":
		return true
	default:
		return false
	}
}

// colorPickerState is the accent/bar color selector's own state, live while
// the stack's current screen is screenColor.
type colorPickerState struct {
	target string // "accent" or "bar" — which config field Enter saves into
	title  string
	saved  string                       // the hex currently persisted — shown once, separately, not part of the cursorable list below (selecting the value already active would be a no-op)
	cursor int                          // index into list — independent of saved; moving it never persists anything
	list   []struct{ Name, Hex string } // colorPalette minus the saved entry
}

// thresholdState is the warning/danger threshold editor's own state, live
// while the stack's current screen is screenThreshold.
type thresholdState struct {
	target   string // "warn" or "danger"
	title    string
	value    float64 // live-edited; only persisted on Enter
	min, max float64 // clamp range, from the other threshold's current value — see enterThreshold
}

// thresholdMenuState is the small "Threshold / Color" sub-menu
// screenThresholdMenu shows for "Warning"/"Danger" — see enterThresholdMenu.
// It exists so both the percentage AND the color reachable from one
// main-menu row are visibly, separately editable (the requesting spec's own
// explicit requirement), while still reusing screenThreshold/screenColor
// verbatim rather than building a new combined editor.
type thresholdMenuState struct {
	target string // "warn" or "danger" — which pair of preferences this is
	title  string // "Warning" or "Danger"
	cursor int    // 0 = Threshold, 1 = Color
}

// themeState is the Theme picker's own state, live while the stack's current
// screen is screenTheme. Mirrors colorPickerState's saved/cursor split
// (see below): moving the cursor through the four themes never persists
// anything on its own, only Enter does.
type themeState struct {
	saved  string // the theme name currently persisted
	cursor int    // index into borderThemes
}

// configApp is cpro config's interactive screen: a small, custom bubbletea.Model
// rather than huh Fields, because this design — a grouped menu with a
// right-aligned value column, a color picker with separate cursor/saved-value
// markers, and a live left/right percentage stepper — doesn't fit huh's built-in
// field types. bubbletea itself isn't a new dependency: it's what huh is already
// built on, and this package already drives it directly elsewhere (escGuardField,
// watchLoop).
//
// Every screen deeper than the main menu (Color/Theme/Threshold/
// ThresholdMenu, and Default when reached from CONFIG) is a frame on
// m.stack (navStack[configScreen], tui.go): entering one pushes it, Esc/←
// pops it — one generic mechanism replacing what used to be a single
// hardcoded returnScreen field (which only ever supported going back exactly
// one level to exactly one predetermined screen). The main menu (screenMenu,
// via runConfigUI) is always this stack's own root frame; Default
// (screenDefault) usually is too (via runDefaultUI — its own top-level entry
// point, `cpro default`, mirroring `cpro config`'s) but, uniquely, isn't
// always: reached through CONFIG's own "Default →" row (activateMenuItem)
// it's pushed as an ordinary nested frame instead, exactly like the
// color/theme/threshold screens — updateDefault/viewDefault check
// m.stack.atRoot() to tell the two cases apart. exitRootScreen (the shared
// Esc/← handler for whichever screen actually is the stack's own root) is
// the one screen-level meaning that depends on context rather than always
// popping: hasParent (set by the caller — runConfigUI/runDefaultUI) means
// this configApp was opened from the root/menu picker and still has a
// screen to go back to (in a different program — see rootui.go's
// pickCommandArgs), so Esc/← sets backOut and quits this program so the
// caller can relaunch the picker; no parent (cpro config/cpro default
// invoked directly) means this screen IS the root of its own interactive
// session, so Esc now arms a double-Esc-to-exit (exitArmed/arm — the same
// convention and timeout rootPickerApp's own outermost frame uses) rather
// than quitting immediately, since a direct invocation is exactly the
// "leaving the whole program" case that convention exists for; a nested
// frame's own Esc/← never arms anything, it just pops one level, the same
// as it always has.
type configApp struct {
	cmd *cobra.Command
	s   *store
	c   config // local mirror of persisted state, updated only after a successful save
	err error  // a save failure; surfaced by runConfigUI once the program exits

	color bool // whether to apply ANSI styling — same NO_COLOR/TERM/terminalOutput gate as accent(), computed once
	width int  // last known terminal width, from tea.WindowSizeMsg; 0 until the first one arrives
	// height is the last known terminal height, from the same message; 0
	// until then. Used only for scrolling a list screen's rows (browseui.go's
	// scrollRange) so the highlighted row stays on screen on a short terminal.
	height int

	hasParent bool // true when reached from the root/menu picker — see the type doc above
	backOut   bool // true once Esc/← at the menu (stack root) with hasParent has been pressed; runConfigUI's own return value

	// exitArmed/exitArmedGen are configApp's own copy of rootPickerApp's
	// double-Esc-to-exit state — reusing exitArmTimeout/exitArmExpiredMsg
	// directly (rootui.go; not receiver-specific) rather than duplicating the
	// constant/message type, just the per-instance fields and the arm()
	// method that sets them. Only ever meaningful when the current screen is
	// the stack's own root with no parent (a direct `cpro config`/`cpro
	// default` invocation) — see exitRootScreen.
	exitArmed    bool
	exitArmedGen int

	stack  navStack[configScreen]
	cursor int // menu screen cursor, 0..len(m.menuItems())-1

	colorState  colorPickerState
	threshState thresholdState
	threshMenu  thresholdMenuState
	themeState  themeState

	// defCursor/defPreview are the SET DEFAULT ACCOUNT screen's own state:
	// defCursor is the flat 0..defaultRowCount()-1 cursor over the Workspace
	// trust row (0), the five permission modes, then one row per registered
	// account; defPreview is the key of the mode whose description line is
	// currently shown (the highlighted candidate, not necessarily
	// m.c.PermissionMode, the saved one, until Enter — see updateDefault) —
	// landing on the trust row or an account row leaves it exactly as it
	// was, since neither has a mode description of its own to preview.
	defCursor  int
	defPreview string
}

// runConfigUI runs the interactive cpro config screen to completion,
// persisting each change through the same store.update path (see
// saveAutoTrust and friends in config.go) as the scriptable cpro config
// trust|mask|accent|bar|warn-at|danger-at — this only changes how the value
// is chosen, not how it's stored. hasParent tells the menu screen (the
// stack's own root) what Esc/← there means: true when this is being opened
// from the root/menu picker (rootui.go), which still has a screen — Menu —
// to go back to once this program exits; the returned backOut reports
// exactly that back-out (as opposed to a real exit via Ctrl+C or, with no
// parent, Esc itself), which is what the caller uses to decide whether to
// relaunch the picker instead of returning.
func runConfigUI(cmd *cobra.Command, s *store, c config, hasParent bool) (backOut bool, err error) {
	m := &configApp{
		cmd:       cmd,
		s:         s,
		c:         c,
		color:     tuiColorEnabled(cmd.ErrOrStderr()),
		hasParent: hasParent,
		stack:     newNavStack(screenMenu),
	}
	return runConfigAppProgram(cmd, m)
}

// runDefaultUI runs the SET DEFAULT ACCOUNT screen standalone, as m.stack's
// own root frame, for `cpro default` (main.go) and the root/menu picker's
// own "default" entry (rootui.go's pickCommandArgs, mirroring its existing
// "config" round-trip) — replacing what used to be two separate top-level
// screens/commands, Permissions and Account, now merged into one screen
// (permission mode, workspace trust, and the default account are all `cpro
// run` defaults). defCursor/defPreview are seeded here exactly as
// activateMenuItem's own "default" case does, since this is now the only
// other place that seeding happens.
func runDefaultUI(cmd *cobra.Command, s *store, c config, hasParent bool) (backOut bool, err error) {
	m := &configApp{
		cmd:        cmd,
		s:          s,
		c:          c,
		color:      tuiColorEnabled(cmd.ErrOrStderr()),
		hasParent:  hasParent,
		stack:      newNavStack(screenDefault),
		defCursor:  1 + permissionModeIndex(effectivePermissionMode(c.PermissionMode)),
		defPreview: effectivePermissionMode(c.PermissionMode),
	}
	return runConfigAppProgram(cmd, m)
}

// runConfigAppProgram runs m's tea.Program to completion — the shared tail
// runConfigUI and runDefaultUI both need, since m's own stack root
// (screenMenu vs. screenDefault) is the only thing that differs between
// them. bubbletea gets the raw terminal file directly, not cmd.ErrOrStderr():
// under NO_COLOR/TERM=dumb that's a *colorprofile.Writer (see uiOutput,
// ui.go), not a real *os.File, and without one to drive raw-mode/cursor
// control directly, bubbletea's render loop never produces a first frame at
// all — the same issue rootui.go's own root picker already hit and fixed
// this same way; m.color (not bubbletea's own color-profile detection)
// already decides, in every string this screen builds (styleText), whether
// a color escape is emitted at all, so there's nothing color-related left
// for bubbletea itself to convert regardless of which writer it's given.
func runConfigAppProgram(cmd *cobra.Command, m *configApp) (backOut bool, err error) {
	p := tea.NewProgram(m, tea.WithContext(cmd.Context()), tea.WithInput(os.Stdin), tea.WithOutput(os.Stderr))
	if _, err := p.Run(); err != nil {
		return false, err
	}
	return m.backOut, m.err
}

// Init has nothing to start: every screen seeds its own state inline in its
// own runXxxUI (or activateMenuItem, when pushed from CONFIG), so there is no
// case left that needs a tea.Cmd handed to the runtime before the first
// frame renders.
func (m *configApp) Init() tea.Cmd {
	return nil
}

func (m *configApp) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case exitArmExpiredMsg:
		if msg.gen == m.exitArmedGen {
			m.exitArmed = false
		}
		return m, nil
	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
		switch *m.stack.current() {
		case screenColor:
			return m.updateColor(msg)
		case screenTheme:
			return m.updateTheme(msg)
		case screenThreshold:
			return m.updateThreshold(msg)
		case screenThresholdMenu:
			return m.updateThresholdMenu(msg)
		case screenDefault:
			return m.updateDefault(msg)
		default:
			return m.updateMenu(msg)
		}
	}
	return m, nil
}

func (m *configApp) View() tea.View {
	var content string
	switch *m.stack.current() {
	case screenColor:
		content = m.viewColor()
	case screenTheme:
		content = m.viewTheme()
	case screenThreshold:
		content = m.viewThreshold()
	case screenThresholdMenu:
		content = m.viewThresholdMenu()
	case screenDefault:
		content = m.viewDefault()
	default:
		content = m.viewMenu()
	}
	// A failed save is shown inline, below the screen, instead of only being
	// returned by runConfigUI after the program exits (decision 0043) — m.err
	// is cleared at the start of every save attempt, so it always describes
	// the most recent one and still propagates on exit if that one failed.
	if m.err != nil {
		content += "\n\n" + styleText(m.color, truncateToWidth(m.err.Error(), m.width), dangerColor)
	}
	v := tea.NewView(content)
	// The SET DEFAULT ACCOUNT screen (Trust row + mode rows + account rows +
	// description line) can run taller than a real terminal window, and once
	// that forces the terminal to scroll, bubbletea's default inline
	// renderer — which moves the cursor with *relative* up-moves, assuming
	// the previous frame is still fully on screen right above it — can no
	// longer find its way back to the top of its own last frame: confirmed
	// live as the exact bug reported here, the old menu's content left
	// behind above a freshly drawn "╭─ Permissions" panel, with "cpro
	// config" appearing to repeat. rootui.go's root picker hit this
	// identical issue for the same reason (a tall command list) and fixed it
	// the same way: the alt screen buffer sidesteps it entirely, since
	// switching screens there is a full clear-and-redraw rather than a
	// relative reposition. Applied to every configApp screen, not just this
	// one, so none of them can regress into this the next time a screen
	// grows past a short terminal's height.
	v.AltScreen = true
	return v
}

// onOffMark renders a boolean as its filled/hollow square ("▣"/"▢") and
// label ("On"/"Off") — distinct from the round "●"/"○" a radio-style
// single-select row (a permission mode, an account, a saved color) uses to
// mark which one of several options is active: a square reads as "this one
// setting is on or off," a dot as "this is the chosen one among many."
// Filled means the setting is active.
func onOffMark(enabled bool) (dot, label string) {
	if enabled {
		return "▣", "On"
	}
	return "▢", "Off"
}

// menuValueGap is the minimum column gap between the longest menu label and the
// start of its value column, on a wide-enough terminal — see viewMenu.
const menuValueGap = 19

// thresholdValueCell renders a Warning/Danger menu row's value column: its
// percentage, then the row's own color dot and name — the shape both "warn"
// and "danger" show ("80%   ● Orange"), sharing one implementation rather
// than two copies differing only in which threshold/color field feeds it.
func thresholdValueCell(color bool, pct float64, hex string) (plainLen int, value string) {
	pctLabel := fmt.Sprintf("%.0f%%", pct)
	name := colorName(hex)
	return len(pctLabel) + 3 + len("●") + 1 + len(name), pctLabel + "   " + styleText(color, "●", hex) + " " + name
}

// narrowWidth is the terminal width below which viewMenu tightens its padding
// instead of the usual generous gap, per the "must remain usable in narrower
// terminals" requirement.
const narrowWidth = 60

// configItemLabel is it's visible label-column text: it.label plus a
// trailing " →" for a row that opens a further screen (isForwardConfigItem),
// or the bare label for one that toggles/edits in place — the same visual
// cue rootNameColumn (rootui.go) gives the root/menu picker's own
// forward-navigable rows. Purely a rendering-time decoration: it.key/label
// themselves are untouched.
func configItemLabel(it configMenuItem) string {
	if isForwardConfigItem(it.key) {
		return it.label + " →"
	}
	return it.label
}

// viewMenu renders the main settings screen: every preference, grouped into
// sections, each row showing its current value in a right-aligned column
// (dot+label for booleans and colors, plain "NN%" for thresholds — no dot there,
// since a percentage has no on/off or saved/unsaved state to mark).
func (m *configApp) viewMenu() string {
	items := m.menuItems()
	labelWidth := 0
	for _, it := range items {
		labelWidth = max(labelWidth, len(configItemLabel(it)))
	}
	gap := menuValueGap
	if m.width > 0 && m.width < narrowWidth {
		gap = 3
	}
	valueCol := labelWidth + gap

	var lines []string
	idx := 0
	for _, it := range items {
		if it.section != "" {
			// Unconditional before a named section, including the first one
			// with a header: the target layout opens with a blank rail row
			// right after "╭─ cpro settings" — not immediately adjacent to
			// the panel's own top border.
			lines = append(lines, "")
			lines = append(lines, styleText(m.color, it.section, accentMode))
			lines = append(lines, "")
		} else if idx == 0 {
			// The very first row (Mask emails) has no header of its own —
			// Security was dropped as a heading once Permissions moved out
			// and left it as the only member — but still needs that same
			// leading blank rail row, just with no header text in between.
			lines = append(lines, "")
		}
		cursor := "  "
		if idx == m.cursor {
			cursor = styleText(m.color, "❯", accentMode) + " "
		}
		label := fmt.Sprintf("%-*s", labelWidth, configItemLabel(it))

		var plainLen int
		var value string
		switch it.key {
		case "mask":
			dot, text := onOffMark(m.c.MaskEmail)
			plainLen, value = len(dot)+1+len(text), styleText(m.color, dot, accentMode)+" "+text
		case "accent":
			hex := orDefault(m.c.AccentColor, accentMode)
			name := colorName(hex)
			plainLen, value = len("●")+1+len(name), styleText(m.color, "●", hex)+" "+name
		case "bar":
			hex := orDefault(m.c.BarColor, barColorSafe)
			name := colorName(hex)
			plainLen, value = len("●")+1+len(name), styleText(m.color, "●", hex)+" "+name
		case "theme":
			name := orDefault(m.c.Theme, currentTheme.Name)
			// The theme's own top-left corner rune stands in for the usual
			// "●" marker here — a preview of the chosen style itself (e.g.
			// "╔ Double"), not a generic filled dot, since there's no single
			// color swatch to show for a border style.
			corner := currentTheme.TopLeft
			if t, ok := themeByName(name); ok {
				corner = t.TopLeft
			}
			plainLen, value = len(corner)+1+len(name), styleText(m.color, corner, accentMode)+" "+name
		case "warn":
			plainLen, value = thresholdValueCell(m.color, orDefaultFloat(m.c.BarWarnThreshold, barWarnThreshold), orDefault(m.c.WarningColor, warningColor))
		case "danger":
			plainLen, value = thresholdValueCell(m.color, orDefaultFloat(m.c.BarDangerThreshold, barDangerThreshold), orDefault(m.c.DangerColor, dangerColor))
		case "default":
			// Both values SET DEFAULT ACCOUNT edits, side by side — the same
			// "● Label  ∙  account" shape the root/menu picker's own merged
			// "default →" row uses (rootRow, rootui.go), so the two never
			// drift into two different combined representations.
			mode := permissionModeByKey(m.c.PermissionMode)
			acct := "Not set"
			if m.c.Default != "" {
				acct = displayEmail(m.c.Default)
			}
			combined := styleText(m.color, "●", mode.Color) + " " + mode.Label + usageCellSeparator + acct
			plainLen, value = len("●")+1+len(mode.Label)+len(usageCellSeparator)+len(acct), combined
		}
		pad := valueCol - labelWidth
		if pad < 1 {
			pad = 1
		}
		_ = plainLen // padding is computed from the fixed label width, not the value — values are left as typed after it
		lines = append(lines, cursor+label+strings.Repeat(" ", pad)+value)
		idx++
	}

	body := renderPanel(m.color, m.frameColor(), screenTitle(m.menuTitle()), lines)
	hints := [][2]string{{"↑↓", "Navigate"}}
	if m.hasParent {
		hints = append(hints, [2]string{"←", "Back"})
	}
	hints = append(hints, [2]string{"→", "Open"}, [2]string{"↵", "Edit"})
	// screenMenu is always this stack's own root frame (never pushed as a
	// nested screen), so the only real distinction left is hasParent: SETTINGS
	// (via the picker) backs out with a single Esc, CONFIG (direct `cpro
	// config`) now arms a double-Esc-to-exit — decision 0023 — the same
	// convention rootPickerApp's own outermost frame uses (exitArmed/arm,
	// rootui.go), reusing its exitArmTimeout/exitArmExpiredMsg directly.
	switch {
	case m.hasParent:
		hints = append(hints, [2]string{"Esc", "Back"})
	case m.exitArmed:
		hints = append(hints, [2]string{"", styleText(m.color, "Esc¹ again", dangerColor)})
	default:
		hints = append(hints, [2]string{"Esc²", "to exit"})
	}
	footer := renderFooter(m.color, accentMode, hints...)
	return body + "\n\n" + footer
}

// frameColor is the border color screenMenu/screenPermissions render with:
// dangerColor while armed to exit (decision 0023), accentMode otherwise. Only
// these two screens can ever be the stack's own root with no parent — the
// one state exitArmed ever gets set in — so this needs no further guard
// beyond m.exitArmed itself; every other screen (color/theme/threshold
// pickers, and Permissions when nested inside CONFIG) never arms and always
// renders in accentMode.
func (m *configApp) frameColor() string {
	if m.exitArmed {
		return dangerColor
	}
	return accentMode
}

// exitRootScreen handles Esc/← on whichever screen is currently m.stack's own
// root frame — screenMenu (cpro config/the picker's "config" entry) or
// screenPermissions (cpro permissions/the picker's "permissions" entry,
// decision 0021, once Permissions became its own top-level entry point
// rather than always being pushed from Settings): with a parent (opened from
// the root/menu picker) it signals backOut so the caller relaunches that
// picker instead of this program just exiting; with none (invoked directly),
// this screen IS the root of its own interactive session, so it exits
// immediately, exactly as before.
func (m *configApp) exitRootScreen() (tea.Model, tea.Cmd) {
	if m.hasParent {
		m.backOut = true
		return m, tea.Quit
	}
	// No parent means this Esc would actually exit the whole program — a
	// direct `cpro config`/`cpro permissions` invocation — which now arms a
	// double-Esc-to-exit (decision 0023) instead of quitting on the first
	// press, matching rootPickerApp's own outermost-frame convention exactly
	// (reusing its exitArmTimeout/exitArmExpiredMsg — see arm, below).
	if m.exitArmed {
		return m, tea.Quit
	}
	return m, m.arm()
}

// arm is configApp's own copy of rootPickerApp's identically-named method
// (rootui.go) — see exitArmed's own field comment for why the timeout
// constant and expiry message are shared but this method isn't.
func (m *configApp) arm() tea.Cmd {
	m.exitArmed = true
	m.exitArmedGen++
	gen := m.exitArmedGen
	return tea.Tick(exitArmTimeout, func(time.Time) tea.Msg { return exitArmExpiredMsg{gen: gen} })
}

func (m *configApp) updateMenu(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Any key other than Esc cancels an armed exit (decision 0023) and still
	// performs its own normal action in the same keystroke — the same
	// convention rootPickerApp's own updateNormal uses for exitArmed.
	if msg.String() != "esc" {
		m.exitArmed = false
	}
	items := m.menuItems()
	switch msg.String() {
	case "up":
		m.cursor = (m.cursor - 1 + len(items)) % len(items)
	case "down":
		m.cursor = (m.cursor + 1) % len(items)
	case "esc":
		return m.exitRootScreen()
	case "left":
		if m.hasParent {
			return m.exitRootScreen()
		}
	case "enter":
		return m, m.activateMenuItem(items[m.cursor].key)
	case "right":
		if key := items[m.cursor].key; isForwardConfigItem(key) {
			return m, m.activateMenuItem(key)
		}
	}
	return m, nil
}

// activateMenuItem handles Enter (or → for a forward-navigable row, see
// isForwardConfigItem) on the menu: toggling a boolean immediately (saving
// it in place, no secondary screen — see the spec), or pushing the color
// picker / threshold editor for the rest. Returns a tea.Cmd (decision 0036,
// nil for every case but "account") since entering DEFAULT ACCOUNT also
// starts that screen's own Session-usage fetch, the same way rootui.go's
// pushRunAccount does for RUN ACCOUNT — every other case still just mutates
// state/pushes a frame synchronously, as before.
func (m *configApp) activateMenuItem(key string) tea.Cmd {
	switch key {
	case "mask":
		next := !m.c.MaskEmail
		m.err = nil
		if err := saveMaskEmail(m.s, next); err != nil {
			m.err = err
			return nil
		}
		m.c.MaskEmail = next
	case "accent":
		m.enterColor("accent", "Accent color", orDefault(m.c.AccentColor, accentMode))
	case "bar":
		m.enterColor("bar", "Usage bar color", orDefault(m.c.BarColor, barColorSafe))
	case "theme":
		m.enterTheme()
	case "warn":
		m.enterThresholdMenu("warn", "Warning")
	case "danger":
		m.enterThresholdMenu("danger", "Danger")
	case "default":
		// Reachable from CONFIG's own main menu, pushing SET DEFAULT ACCOUNT
		// as a nested frame rather than the stack root it is when reached
		// directly (cpro default) or from the picker's own "default →": Esc/←
		// on it here pops back to CONFIG, not exitRootScreen — see
		// updateDefault's own m.stack.atRoot() check.
		m.defCursor = 1 + permissionModeIndex(effectivePermissionMode(m.c.PermissionMode))
		m.defPreview = effectivePermissionMode(m.c.PermissionMode)
		m.stack.push(screenDefault)
	}
	return nil
}

// enterColor pushes the color picker for target ("accent", "bar",
// "warning-color", or "danger-color" — the one shared component behind all
// four, per the requesting spec's explicit "reuse the same color-picker
// component" instruction), preselecting the cursor on the first entry of the
// palette with saved excluded — reselecting what's already active would be a
// no-op, so it's shown separately instead (see colorPickerState). Esc/←
// pops back to whatever pushed it (screenMenu for accent/bar,
// screenThresholdMenu for warning-color/danger-color), via m.stack — no
// separate "return to" field needed.
func (m *configApp) enterColor(target, title, saved string) {
	list := make([]struct{ Name, Hex string }, 0, len(colorPalette))
	for _, c := range colorPalette {
		if c.Hex != saved {
			list = append(list, c)
		}
	}
	m.colorState = colorPickerState{target: target, title: title, saved: saved, list: list}
	m.stack.push(screenColor)
}

func (m *configApp) viewColor() string {
	lines := []string{"", "  " + styleText(m.color, "●", m.colorState.saved) + " " + colorName(m.colorState.saved), ""}
	for i, c := range m.colorState.list {
		cursor := "  "
		if i == m.colorState.cursor {
			cursor = styleText(m.color, "❯", accentMode) + " "
		}
		lines = append(lines, cursor+styleText(m.color, "●", c.Hex)+" "+c.Name)
	}
	lines = append(lines, "")
	footer := renderFooter(m.color, accentMode, [2]string{"↑↓", "Navigate"}, [2]string{"↵", "Save"}, [2]string{"Esc", "Back"})
	return renderPanel(m.color, accentMode, screenTitle(strings.ToUpper(m.colorState.title)), lines) + "\n\n" + footer
}

func (m *configApp) updateColor(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	n := len(m.colorState.list)
	switch msg.String() {
	case "up":
		m.colorState.cursor = (m.colorState.cursor - 1 + n) % n
	case "down":
		m.colorState.cursor = (m.colorState.cursor + 1) % n
	case "esc", "left":
		m.stack.pop()
	case "enter":
		chosen := m.colorState.list[m.colorState.cursor]
		m.err = nil
		var err error
		switch m.colorState.target {
		case "accent":
			err = saveAccentColor(m.s, chosen.Hex)
		case "bar":
			err = saveBarColor(m.s, chosen.Hex)
		case "warning-color":
			err = saveWarningColor(m.s, chosen.Hex)
		case "danger-color":
			err = saveDangerColor(m.s, chosen.Hex)
		}
		if err != nil {
			m.err = err
		} else {
			switch m.colorState.target {
			case "accent":
				m.c.AccentColor = chosen.Hex
			case "bar":
				m.c.BarColor = chosen.Hex
			case "warning-color":
				m.c.WarningColor = chosen.Hex
			case "danger-color":
				m.c.DangerColor = chosen.Hex
			}
		}
		m.stack.pop()
	}
	return m, nil
}

// enterThresholdMenu pushes the small "Threshold / Color" sub-menu for
// target ("warn" or "danger") — the Warning/Danger row's own Enter action —
// see thresholdMenuState.
func (m *configApp) enterThresholdMenu(target, title string) {
	m.threshMenu = thresholdMenuState{target: target, title: title}
	m.stack.push(screenThresholdMenu)
}

func (m *configApp) viewThresholdMenu() string {
	target := m.threshMenu.target
	pct, hex := orDefaultFloat(m.c.BarWarnThreshold, barWarnThreshold), orDefault(m.c.WarningColor, warningColor)
	if target == "danger" {
		pct, hex = orDefaultFloat(m.c.BarDangerThreshold, barDangerThreshold), orDefault(m.c.DangerColor, dangerColor)
	}
	// Both rows here open a further screen (the threshold stepper, the color
	// picker) rather than editing in place, so both carry the same " →" cue
	// rootNameColumn/configItemLabel give every other forward-navigable row.
	rows := []struct{ label, value string }{
		{"Threshold →", fmt.Sprintf("%.0f%%", pct)},
		{"Color →", styleText(m.color, "●", hex) + " " + colorName(hex)},
	}
	labelWidth := 0
	for _, row := range rows {
		labelWidth = max(labelWidth, len(row.label))
	}
	lines := []string{""}
	for i, row := range rows {
		cursor := "  "
		if i == m.threshMenu.cursor {
			cursor = styleText(m.color, "❯", accentMode) + " "
		}
		lines = append(lines, cursor+padEnd(row.label, labelWidth+1)+row.value)
	}
	lines = append(lines, "")
	footer := renderFooter(m.color, accentMode,
		[2]string{"↑↓", "Navigate"}, [2]string{"←", "Back"}, [2]string{"→", "Open"}, [2]string{"↵", "Edit"}, [2]string{"Esc", "Back"})
	return renderPanel(m.color, accentMode, screenTitle(strings.ToUpper(m.threshMenu.title)), lines) + "\n\n" + footer
}

// activateThresholdMenuRow handles Enter or → (both rows here are
// forward-navigable — see the "Threshold / Color" sub-menu's own spec) on
// the currently selected row: pushing the threshold editor or the color
// picker, matching m.threshMenu.target.
func (m *configApp) activateThresholdMenuRow() {
	target := m.threshMenu.target
	if m.threshMenu.cursor == 0 {
		if target == "warn" {
			danger := orDefaultFloat(m.c.BarDangerThreshold, barDangerThreshold)
			m.enterThreshold("warn", "Warning threshold", orDefaultFloat(m.c.BarWarnThreshold, barWarnThreshold), 1, danger-1)
		} else {
			warn := orDefaultFloat(m.c.BarWarnThreshold, barWarnThreshold)
			m.enterThreshold("danger", "Danger threshold", orDefaultFloat(m.c.BarDangerThreshold, barDangerThreshold), warn+1, 100)
		}
		return
	}
	if target == "warn" {
		m.enterColor("warning-color", "Warning color", orDefault(m.c.WarningColor, warningColor))
	} else {
		m.enterColor("danger-color", "Danger color", orDefault(m.c.DangerColor, dangerColor))
	}
}

func (m *configApp) updateThresholdMenu(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up":
		m.threshMenu.cursor = (m.threshMenu.cursor - 1 + 2) % 2
	case "down":
		m.threshMenu.cursor = (m.threshMenu.cursor + 1) % 2
	case "esc", "left":
		m.stack.pop()
	case "enter", "right":
		m.activateThresholdMenuRow()
	}
	return m, nil
}

// enterTheme pushes the Theme picker, preselecting the cursor on the
// currently saved theme (unlike enterColor, the saved entry stays IN the
// list here rather than being excluded and shown separately — the requesting
// spec's own mockup shows all four themes together, saved marked by ●
// in place, not pulled out into a leading line).
func (m *configApp) enterTheme() {
	saved := orDefault(m.c.Theme, currentTheme.Name)
	cursor := 0
	for i, t := range borderThemes {
		if strings.EqualFold(t.Name, saved) {
			cursor = i
		}
	}
	m.themeState = themeState{saved: saved, cursor: cursor}
	m.stack.push(screenTheme)
}

// viewTheme renders the four themes, each with its own name row (cursor +
// saved ●/○ marker, like every other on/off or saved-value row in this
// screen) followed by a 3-line preview of its own actual border runes
// (Top/Rail/Bottom, theme.go) — plain, uncolored: Theme only ever changes
// border characters, never color (the requesting spec's own explicit
// constraint), so nothing here should look like a fifth user-facing color.
func (m *configApp) viewTheme() string {
	var lines []string
	for i, t := range borderThemes {
		cursor := "  "
		if i == m.themeState.cursor {
			cursor = styleText(m.color, "❯", accentMode) + " "
		}
		dot := "○"
		if strings.EqualFold(t.Name, m.themeState.saved) {
			dot = "●"
		}
		lines = append(lines, cursor+styleText(m.color, dot, accentMode)+" "+t.Name)
		lines = append(lines, "      "+t.Top())
		lines = append(lines, "      "+t.Rail())
		lines = append(lines, "      "+t.Bottom())
		if i < len(borderThemes)-1 {
			lines = append(lines, "")
		}
	}
	footer := renderFooter(m.color, accentMode, [2]string{"↑↓", "Navigate"}, [2]string{"↵", "Save"}, [2]string{"Esc", "Back"})
	return renderPanel(m.color, accentMode, screenTitle("THEME"), lines) + "\n\n" + footer
}

func (m *configApp) updateTheme(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	n := len(borderThemes)
	switch msg.String() {
	case "up":
		m.themeState.cursor = (m.themeState.cursor - 1 + n) % n
	case "down":
		m.themeState.cursor = (m.themeState.cursor + 1) % n
	case "esc", "left":
		m.stack.pop()
	case "enter":
		chosen := borderThemes[m.themeState.cursor]
		m.err = nil
		if err := saveTheme(m.s, chosen); err != nil {
			m.err = err
		} else {
			m.c.Theme = chosen.Name
		}
		m.stack.pop()
	}
	return m, nil
}

// enterThreshold pushes the threshold editor for target ("warn" or
// "danger"), clamped to (min, max) so every reachable value is already
// valid — see updateThreshold — instead of allowing an out-of-range or
// order-violating value to be typed and rejected on save. This preserves
// the existing warn-below-danger constraint (saveBarWarnThreshold/
// saveBarDangerThreshold in config.go) without duplicating it: the clamp
// just makes it unreachable here rather than re-checked.
func (m *configApp) enterThreshold(target, title string, current, min, max float64) {
	m.threshState = thresholdState{target: target, title: title, value: current, min: min, max: max}
	m.stack.push(screenThreshold)
}

func (m *configApp) viewThreshold() string {
	lines := []string{
		"",
		"Usage percentage",
		"",
		styleText(m.color, "❯", accentMode) + " " + fmt.Sprintf("%.0f%%", m.threshState.value),
		"",
	}
	// ←→ here adjust the value, not navigation — this screen deliberately
	// has no separate "← Back" hint (only Esc backs out), the one exception
	// to every other nested screen's ←-also-backs-out convention.
	footer := renderFooter(m.color, accentMode, [2]string{"←→", "Adjust"}, [2]string{"↵", "Save"}, [2]string{"Esc", "Back"})
	return renderPanel(m.color, accentMode, screenTitle(strings.ToUpper(m.threshState.title)), lines) + "\n\n" + footer
}

func (m *configApp) updateThreshold(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "left", "down":
		m.threshState.value = max(m.threshState.min, m.threshState.value-1)
	case "right", "up":
		m.threshState.value = min(m.threshState.max, m.threshState.value+1)
	case "esc":
		m.stack.pop()
	case "enter":
		m.err = nil
		var err error
		if m.threshState.target == "warn" {
			err = saveBarWarnThreshold(m.s, m.threshState.value)
		} else {
			err = saveBarDangerThreshold(m.s, m.threshState.value)
		}
		if err != nil {
			m.err = err
		} else if m.threshState.target == "warn" {
			m.c.BarWarnThreshold = m.threshState.value
		} else {
			m.c.BarDangerThreshold = m.threshState.value
		}
		m.stack.pop()
	}
	return m, nil
}

// defaultAccounts is every registered account, sorted — the SET DEFAULT
// ACCOUNT screen's own account section renders one row per entry, the
// filled dot marking whichever is currently config.Default. Sorted rather
// than map-ordered so the rows never reshuffle between renders.
func (m *configApp) defaultAccounts() []string {
	emails := make([]string, 0, len(m.c.Accounts))
	for email := range m.c.Accounts {
		emails = append(emails, email)
	}
	sort.Strings(emails)
	return emails
}

// defaultAccountIndex maps the SET DEFAULT ACCOUNT screen's cursor to the
// account row it is on, or -1 when the cursor is on the Workspace trust row
// or a mode.
func (m *configApp) defaultAccountIndex() int {
	first := 1 + len(permissionModes)
	if m.defCursor < first || m.defCursor >= m.defaultRowCount() {
		return -1
	}
	return m.defCursor - first
}

// defaultRowCount is the SET DEFAULT ACCOUNT screen's total number of
// selectable rows: one for the Workspace "Trust working directory" row
// (index 0), one per permissionModes entry, and one per registered account.
// The trust row leads, ahead of the modes, matching the screen's own layout
// (viewDefault).
func (m *configApp) defaultRowCount() int {
	return 1 + len(permissionModes) + len(m.defaultAccounts())
}

func (m *configApp) updateDefault(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Same arm-cancel-on-any-other-key rule updateMenu applies — relevant
	// here only when this screen actually is the stack's own root (see
	// exitRootScreen), harmless otherwise.
	if msg.String() != "esc" {
		m.exitArmed = false
	}
	total := m.defaultRowCount()
	switch msg.String() {
	case "up":
		m.defCursor = (m.defCursor - 1 + total) % total
		m.syncDefPreview()
	case "down":
		m.defCursor = (m.defCursor + 1) % total
		m.syncDefPreview()
	case "esc", "left":
		// This screen isn't always the stack's own root: reached from
		// CONFIG (activateMenuItem's "default" case) it's a nested frame
		// like any other, popped the same way the color/theme/threshold
		// screens already are; reached directly (cpro default) or from the
		// picker's own "default →" it's still root, so exitRootScreen's
		// existing hasParent/arm logic applies.
		if m.stack.atRoot() {
			return m.exitRootScreen()
		}
		m.stack.pop()
	case "enter", "space", "tab":
		// Space and Tab are plain aliases for Enter here, not the
		// walk-and-check multi-select convenience Tab is elsewhere
		// (DELETE SESSION, sessionui.go): every row on this screen is a
		// single-select toggle/pick, so "select this row" is all either key
		// needs to mean. Safe to bind unconditionally — unlike a searchable
		// screen, this one has no query field for Space to type into or Tab
		// to be mistaken for.
		m.activateDefaultRow()
	}
	return m, nil
}

// syncDefPreview updates m.defPreview after a cursor move: landing on a mode
// row previews that candidate immediately, with no save — Enter is what
// actually saves it (activateDefaultRow). Landing on the Workspace trust row
// or an account row leaves m.defPreview exactly as it was — neither has a
// mode description of its own to preview, and AutoTrust/the default account
// aren't part of the claude argv a mode's description describes.
func (m *configApp) syncDefPreview() {
	if m.defCursor >= 1 && m.defCursor <= len(permissionModes) {
		m.defPreview = permissionModes[m.defCursor-1].Key
	}
}

// activateDefaultRow handles Enter on the SET DEFAULT ACCOUNT screen:
// toggling the Workspace trust row (defCursor == 0), selecting a mode
// (1 <= defCursor <= len(permissionModes)), or picking the default account
// (defCursor beyond both) — all three save immediately and stay on this
// screen; only Esc/← leaves it. There is no forward-navigable row here at
// all (unlike the old per-account mode editor this replaces): every row is
// a leaf, so → is never bound to anything on this screen.
//
// Both the mode and account branches also clear a lingering per-account
// override (config.PermissionModeByAccount) on the account that ends up
// being the default, if one exists — the scriptable `cpro config
// permission-mode MODE --account EMAIL` (permissions.go) is still how one
// gets set, and permissionModeForAccount still resolves it ahead of the
// global default for every real run, but this screen dropped its own
// interactive editor for it (the old per-account "Accounts" section) and
// has no other row left to show or clear one from. Without this, selecting
// a mode here silently does nothing for whichever account carries an old
// override: reported live as "picking a mode doesn't stick," for the
// account that was already the default and already pinned to a stale one.
func (m *configApp) activateDefaultRow() {
	m.err = nil
	if m.defCursor == 0 {
		next := !m.c.AutoTrust
		if err := saveAutoTrust(m.s, next); err != nil {
			m.err = err
			return
		}
		m.c.AutoTrust = next
		return
	}
	if m.defCursor <= len(permissionModes) {
		mode := permissionModes[m.defCursor-1].Key
		if err := savePermissionMode(m.s, mode); err != nil {
			m.err = err
			return
		}
		m.c.PermissionMode = mode
		m.clearDefaultAccountOverride()
		return
	}
	if idx := m.defaultAccountIndex(); idx >= 0 {
		if accounts := m.defaultAccounts(); idx < len(accounts) {
			email := accounts[idx]
			if err := saveDefaultAccount(m.s, email); err != nil {
				m.err = err
				return
			}
			m.c.Default = email
			m.clearDefaultAccountOverride()
		}
	}
}

// clearDefaultAccountOverride removes config.Default's own per-account
// permission-mode override, if it has one, so it actually follows whatever
// this screen shows as the global mode rather than a stale pin left over
// from the CLI or the old per-account editor. A no-op (and never surfaces
// m.err) when there's no default account yet or it carries no override —
// this is a best-effort tidy-up alongside the row's own save, not something
// a failure here should block that save over.
func (m *configApp) clearDefaultAccountOverride() {
	if m.c.Default == "" || !hasAccountPermissionOverride(m.c, m.c.Default) {
		return
	}
	if err := saveAccountPermissionMode(m.s, m.c.Default, ""); err == nil {
		delete(m.c.PermissionModeByAccount, m.c.Default)
	}
}

// workspaceTrustLabel is "Trust working directory" — the SET DEFAULT ACCOUNT
// screen's own name for the setting the main Settings menu used to call
// Auto-trust and config.json still calls AutoTrust (store.go) — no config
// migration, only the label and its location in the UI changed.
const workspaceTrustLabel = "Trust working directory"

// defaultContextGlyph/defaultContextLabel are the screen's own small,
// static, right-aligned context line — "! cpro run" — naming which
// non-interactive command these defaults actually apply to, since the
// screen itself edits three separate config fields with no command-shaped
// preview of its own (unlike the removed "Command preview" block this
// screen otherwise has no ancestor of).
const (
	defaultContextGlyph = "!"
	defaultContextLabel = "cpro run"
)

// defaultContextLine right-aligns "! cpro run" within contentWidth — the
// panel's own natural content width (the widest of its other rows), not the
// real terminal width: this screen's panel, like every other one in cpro,
// is only ever as wide as its own longest line, so aligning to the terminal
// instead left it stranded many columns past the actual panel on any
// reasonably wide terminal. Rendered dim: it's contextual chrome, not a row.
func (m *configApp) defaultContextLine(contentWidth int) string {
	text := defaultContextGlyph + " " + defaultContextLabel
	if pad := contentWidth - visibleWidth(text); pad > 0 {
		text = strings.Repeat(" ", pad) + text
	}
	return dimStyle(m.color, text)
}

// viewDefault renders the SET DEFAULT ACCOUNT screen: a static "! cpro run"
// context line, the Workspace "Trust working directory" row (the same
// "▣"/"▢" on/off square onOffMark gives Mask emails on the main menu, plus
// "On"/"Off" and the label, in that order), the five permission modes
// (colored ●/○ per mode.Color — an
// inactive mode's ○ stays uncolored), one row per registered account
// (plain ●/○ in accentMode marking config.Default — no live Session usage
// here, unlike the account pickers elsewhere in cpro: this is a settings
// row, not a run-time choice), and — separated by a real mid-panel divider
// (panelDividerLine, tui.go) rather than just another blank row — a single
// short description of what the highlighted mode does, read straight off
// permissionMode.Description (permissions.go). Deliberately renders
// m.defPreview (the highlighted candidate mode, tracked by syncDefPreview)
// rather than m.c.PermissionMode (the saved one): the two only match once
// Enter has actually saved the highlighted row — permissionModeByKey
// resolves either through effectivePermissionMode, so an empty/unset
// m.defPreview still renders a real mode's description rather than nothing.
// AutoTrust and PermissionMode stay separate config fields — s.run
// (claude.go) is what ORs their effect together for YOLO — so the trust
// row's own glyph plays no part in this description either way: it isn't a
// claude argv flag, just a separate side effect (markTrusted) that happens
// before claude ever starts.
//
// This screen replaces what used to be two separate screens, Permissions
// and DEFAULT ACCOUNT (and, within Permissions, a per-account
// permission-mode override section) — merged into one because permission
// mode, workspace trust, and the default account are all values
// non-interactive `cpro run` falls back to when given no flags. The
// per-account override capability itself is unaffected — the scriptable
// `cpro config permission-mode MODE --account EMAIL` still sets it — only
// its own interactive editor (a screen within this screen) is gone; picking
// an account row here sets the *global* default account, not a per-account
// mode.
func (m *configApp) viewDefault() string {
	var lines []string

	// Every mode's dot always carries its own risk color (the "traffic light"),
	// not just the active one — filled (●) vs. hollow (○) marks which mode is
	// actually saved (m.c.PermissionMode), never the previewed one — color alone
	// never does either, so the rows read as a legend at a glance regardless of
	// which is active or which is merely highlighted.
	current := effectivePermissionMode(m.c.PermissionMode)
	for i, mode := range permissionModes {
		cursor := "  "
		if m.defCursor == i+1 {
			cursor = styleText(m.color, "❯", accentMode) + " "
		}
		dot := "○"
		if mode.Key == current {
			dot = "●"
		}
		lines = append(lines, cursor+styleText(m.color, dot, mode.Color)+" "+mode.Label)
	}

	// One row per registered account, single-select (●/○, accentMode —
	// unlike a permission mode, an account has no risk gradient of its
	// own): picking one sets it as config.Default immediately, the same
	// "stays on screen, only Esc/← leaves" convention every other row here
	// follows.
	if accounts := m.defaultAccounts(); len(accounts) > 0 {
		lines = append(lines, "")
		first := 1 + len(permissionModes)
		for i, email := range accounts {
			cursor := "  "
			if m.defCursor == first+i {
				cursor = styleText(m.color, "❯", accentMode) + " "
			}
			dot := "○"
			if email == m.c.Default {
				dot = "●"
			}
			lines = append(lines, cursor+styleText(m.color, dot, accentMode)+" "+displayEmail(email))
		}
	}

	// Measured over the mode/account rows only — the trust row and the
	// description are both one-off outliers wider than this actual list, and
	// letting either set this line's width would strand "! cpro run" many
	// columns further right than the rows it actually sits above.
	contentWidth := 0
	for _, line := range lines {
		contentWidth = max(contentWidth, visibleWidth(line))
	}
	context := []string{m.defaultContextLine(contentWidth), ""}

	trustCursor := "  "
	if m.defCursor == 0 {
		trustCursor = styleText(m.color, "❯", accentMode) + " "
	}
	glyph, state := onOffMark(m.c.AutoTrust)
	trust := trustCursor + styleText(m.color, glyph, accentMode) + " " + state + " - " + workspaceTrustLabel

	lines = append(append(context, trust, ""), lines...)
	lines = append(lines, "", panelDividerLine, "  "+permissionModeByKey(m.defPreview).Description)

	body := renderPanel(m.color, m.frameColor(), screenTitle("SET DEFAULT ACCOUNT"), lines)
	// SET DEFAULT ACCOUNT is reachable three ways: standalone (cpro default,
	// root, no parent — arms a double-Esc-to-exit, same as CONFIG), via the
	// picker (root, hasParent — single Esc backs out to Menu), or nested
	// inside CONFIG (activateMenuItem's "default" case — not the stack's own
	// root at all, single Esc/← just pops back to CONFIG like any other
	// nested screen, and never arms). hasParent and "nested" produce the
	// identical footer, so they're one case below.
	hints := [][2]string{{"↑↓", "Navigate"}}
	switch {
	case m.hasParent, !m.stack.atRoot():
		hints = append(hints, [2]string{"←", "Back"}, [2]string{"↵", "Select"}, [2]string{"Esc", "Back"})
	case m.exitArmed:
		hints = append(hints, [2]string{"↵", "Select"}, [2]string{"", styleText(m.color, "Esc¹ again", dangerColor)})
	default:
		hints = append(hints, [2]string{"↵", "Select"}, [2]string{"Esc²", "to exit"})
	}
	footer := renderFooter(m.color, accentMode, hints...)
	return body + "\n\n" + footer
}
