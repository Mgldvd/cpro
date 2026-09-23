package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"github.com/spf13/cobra"
)

// rootPickerEntry is one selectable row in the bare-`cpro` command picker.
// group is presentation-only metadata (see rootGroups/rootGroupShadeIndex)
// that picks which of the five accent-derived rail shades this row's "│"
// uses — it is never rendered as text and has no effect on behavior.
// shortLabel is the short, always-visible right-side metadata for this row
// (2-4 words); description is the longer, below-panel text shown only for
// the currently highlighted row, populated from the command's own cobra
// Short text (see rootPickerFilteredEntries) rather than duplicated here, so
// the picker and `cpro --help` never drift apart. permMode and defaultAcct
// are the two pieces of metadata that aren't static: only the "default"
// entry ever sets them (see applyLiveMeta), to the live, currently saved
// permission mode key and default account — rootRow renders them as a
// colored "● Label  ∙  account" in place of the generic dimmed shortLabel
// when non-empty, so that row always shows what's actually in effect rather
// than a fixed description. They're merged onto one entry, not two, because
// they're the pair of values `cpro run` resolves from when given no flags —
// "SET DEFAULT ACCOUNT" (configui.go) is the one screen that edits both.
type rootPickerEntry struct {
	name        string
	group       string
	shortLabel  string
	description string
	permMode    string
	defaultAcct string
}

// rootGroups is the fixed order of the picker's five internal semantic
// groups, from the rail's most subdued shade (Core) to its brightest — the
// main accent color itself (About). Groups are never rendered as text
// anywhere in the picker; they only select a rail shade (see
// rootGroupShadeIndex/deriveAccentShades, tui.go).
var rootGroups = []string{"Core", "Account", "Settings", "System", "About"}

// rootGroupShadeIndex returns group's position in rootGroups (and so which
// of deriveAccentShades(accentMode, len(rootGroups))'s shades it uses),
// falling back to the last/brightest shade for a group that isn't listed —
// which should never happen given rootPickerMeta below, but a graceful
// fallback here is cheaper than a panic over a picker color.
func rootGroupShadeIndex(group string) int {
	for i, g := range rootGroups {
		if g == group {
			return i
		}
	}
	return len(rootGroups) - 1
}

// rootPickerMeta is the full command palette's static, picker-only
// presentation data — group and shortLabel — in the exact order commands are
// shown. A command not listed here (e.g. one added later without updating
// this table) simply never appears in either picker —
// rootPickerFilteredEntries filters this table down to whatever names it's
// asked for and root actually has, rather than the reverse, so a stale entry
// here (a removed command) is silently dropped too. "run"/"status"/"watch"
// carry a group/shortLabel here purely for the reduced launcher's own use
// (rootLauncherNames) — none of the three are menu-eligible (rootMenuNames
// excludes them, same as "menu" itself), since the launcher already covers
// them and `cpro menu` would otherwise just repeat it.
// config's own group is "Core" — reused, not a sixth group: "Core" only ever
// renders in the Root frame (run/status/watch) or the Menu frame
// (config/default), never both at once, so there's no shading conflict, and
// it's what puts config and default together in one leading shade/section at
// the top of the Menu frame, ahead of Account. "default" sits directly below
// "config", same group, so there's no blank rail separator between them.
// "default" merges what used to be two separate entries, "permissions" and
// "account" — both edited the pair of values non-interactive `cpro run`
// resolves from when given no flags, so they're one screen and one row now
// ("SET DEFAULT ACCOUNT", configui.go) rather than two.
var rootPickerMeta = []struct{ name, group, shortLabel string }{
	{"run", "Core", "Claude Code"},
	{"status", "Core", "Usage"},
	{"watch", "Core", "Live usage"},
	{"menu", "About", "More commands ..."},
	{"config", "Core", "Preferences"},
	{"default", "Core", "Run defaults"},
	{"login", "Account", "Sign in"},
	{"logout", "Account", "Sign out"},
	{"remove", "Account", "Delete profile"},
	{"session", "Settings", "Sessions"},
	{"system", "Settings", "System credentials"},
	{"doctor", "Settings", "Diagnostics"},
	{"install", "System", "Install cpro"},
	{"completion", "System", "Shell setup"},
	{"info", "About", "cpro information"},
	{"help", "About", "Help"},
	{"version", "About", "Version"},
}

// rootLauncherNames is the reduced set — and display order — of commands the
// bare-`cpro` launcher shows: the handful of everyday actions, plus "menu",
// the gateway to everything else (rootMenuNames).
var rootLauncherNames = []string{"run", "watch", "status", "menu"}

// rootMenuExcluded are the names rootPickerMeta lists that never appear in
// `cpro menu`'s own full palette: "menu" itself (which would let it nest),
// and "run"/"status"/"watch" (already one press away in the reduced
// launcher every path to Menu passes through first, so repeating them here
// would just be noise).
var rootMenuExcluded = map[string]bool{"menu": true, "run": true, "status": true, "watch": true}

// rootMenuNames is every picker-eligible command in rootPickerMeta's own
// order, minus rootMenuExcluded — `cpro menu`'s own entry list.
func rootMenuNames() []string {
	names := make([]string, 0, len(rootPickerMeta))
	for _, meta := range rootPickerMeta {
		if !rootMenuExcluded[meta.name] {
			names = append(names, meta.name)
		}
	}
	return names
}

// rootPickerEntries is rootPickerMeta as rootPickerEntry values, with no
// command filtering and no real description (there's no *cobra.Command to
// pull a Short from without a root) — used directly by the pure, no-pty unit
// tests that only need name/group/shortLabel. Production code always goes
// through rootPickerFilteredEntries instead, which attaches the real
// description from each command's own cobra Short.
var rootPickerEntries = func() []rootPickerEntry {
	entries := make([]rootPickerEntry, len(rootPickerMeta))
	for i, meta := range rootPickerMeta {
		entries[i] = rootPickerEntry{name: meta.name, group: meta.group, shortLabel: meta.shortLabel}
	}
	return entries
}()

// rootPickerFilteredEntries builds one picker's entry list: names, in the
// order given, filtered down to whatever root actually has registered
// (everything IsAvailableCommand reports, plus "help" itself, which
// IsAvailableCommand always excludes), with each entry's description
// populated from that command's own cobra Short. The one shared builder both
// the root launcher (rootLauncherNames) and `cpro menu` (rootMenuNames)
// call — a different names slice is the only thing that distinguishes the
// two pickers, not a second implementation — pulled out as its own pure
// function so a picker's content is unit-testable without spinning up a pty
// or a bubbletea program.
func rootPickerFilteredEntries(root *cobra.Command, names []string) []rootPickerEntry {
	byCmd := make(map[string]*cobra.Command, len(root.Commands()))
	for _, c := range root.Commands() {
		if c.IsAvailableCommand() || c.Name() == "help" {
			byCmd[c.Name()] = c
		}
	}
	byMeta := make(map[string]struct{ group, shortLabel string }, len(rootPickerMeta))
	for _, meta := range rootPickerMeta {
		byMeta[meta.name] = struct{ group, shortLabel string }{meta.group, meta.shortLabel}
	}
	entries := make([]rootPickerEntry, 0, len(names))
	for _, name := range names {
		cmd, ok := byCmd[name]
		if !ok {
			continue
		}
		meta := byMeta[name]
		entries = append(entries, rootPickerEntry{name: name, group: meta.group, shortLabel: meta.shortLabel, description: cmd.Short})
	}
	return entries
}

// applyLiveMeta sets the one entry whose metadata isn't static
// (rootPickerMeta): "default" shows both the currently saved
// config.PermissionMode and config.Default, read fresh from s, so the row
// shows what is really in effect rather than a fixed description.
// Best-effort: a read failure leaves both empty and rootRow falls back to
// the static shortLabel, same as any other row; so does an unset
// config.Default, which has no account to name yet. Mutates entries in
// place (relying on slice reference semantics) so callers can refresh a
// frame already on m.stack without rebuilding it.
func applyLiveMeta(entries []rootPickerEntry, s *store) {
	if s == nil {
		return
	}
	c, err := s.read()
	if err != nil {
		return
	}
	for i := range entries {
		if entries[i].name != "default" {
			continue
		}
		entries[i].permMode = effectivePermissionMode(c.PermissionMode)
		// displayEmail, never the raw address: masking is a display
		// preference and this is a display string (decision 0024).
		if c.Default != "" {
			entries[i].defaultAcct = displayEmail(c.Default)
		}
	}
}

// isForwardEntry reports whether name's row opens a further screen — so →
// acts as an Enter alias on it, and its screen's footer advertises
// "→ Open" — as opposed to a leaf row (status/watch/login/... /version)
// that only Enter ever executes. "config"/"default" are included even
// though pushing either crosses into a separate program (see
// pickCommandArgs) — from this screen's own point of view it's still "open a
// further screen", not "run a command and exit"; "default" (formerly
// "permissions"/"account", separately) mirrors "config"'s existing
// round-trip exactly. "run" joined this list in decision 0019: selecting it
// no longer execs Claude directly, it opens the interactive run flow's own
// RUN ACCOUNT frame (pushRunAccount) — STEP 4's actual execution only
// happens once RUN MODE's own Enter (updateRunMode) finalizes it.
func isForwardEntry(name string) bool {
	switch name {
	case "menu", "config", "default", "system", "run", "session", "watch",
		// decision 0039: these three open a screen of their own now (an
		// account list for logout/remove, an email field for login) instead
		// of dropping out of the picker into a separate huh prompt, so they
		// take the same " →" cue and → alias every other forward row has.
		"login", "logout", "remove":
		return true
	default:
		return false
	}
}

// accountArgCommands are the commands whose single required EMAIL argument
// the picker now supplies itself (decision 0039), mapped to the title their
// own frame carries. pickCommandArgs consults this to return such a pick
// immediately, instead of falling through to pickRequiredArgs and asking for
// an argument the user has already given.
var accountArgCommands = map[string]string{
	"login":  "LOGIN",
	"logout": "LOGOUT",
	"remove": "REMOVE",
}

// rootFrameKind distinguishes the shapes a rootPickerApp frame can take.
type rootFrameKind int

const (
	frameList          rootFrameKind = iota // a searchable rootPickerEntry list — the reduced launcher or the full menu
	frameSubmenu                            // the small, fixed "System credentials" export/import choice
	frameRunAccount                         // a searchable list of registered accounts — see rootFrame.command
	frameRunMode                            // STEP 3: the 5 permissionModes rows, preselected from the saved Settings default
	frameWatchMode                          // the small, fixed "Full"/"Compact" choice for selecting watch (decision 0029)
	frameEmailInput                         // a free-text email field for an account that isn't registered yet (login — decision 0039)
	frameRemoveConfirm                      // the in-app "retype the account" guard before `remove` (decision 0042)
)

// rootFrame is one frame of rootPickerApp's navStack: a searchable list
// (frameList, the reduced launcher or the full menu, or frameRunAccount's
// account rows — both keep their state in list, the shared
// browseList[rootPickerEntry] from browseui.go) or one of the small
// fixed-choice frames (frameSubmenu/frameRunMode/frameWatchMode, which index
// cursor directly and carry no list), plus frameEmailInput's free-text field.
// account carries the email chosen in the frameRunAccount frame that pushed
// a frameRunMode, so that frame's own Enter can build the final
// "run --account ..." argv without threading it through anywhere else.
//
// cursor is meaningful only for the fixed-choice frames; the searchable ones
// keep their selection inside list (list.cursor/list.fcursor), which is also
// where their query/filtered state lives. Search state being per frame is why
// popFrame (not a bare m.stack.pop) is what every pop here goes through: it
// clears the revealed frame's search so backing out one level lands on the
// same plain browse list it left from.
type rootFrame struct {
	kind    rootFrameKind
	title   string // "" for the picker's own outermost frame; "MENU"/"SYSTEM"/"RUN ACCOUNT"/"RUN MODE"/"LOGOUT"/"REMOVE"/"LOGIN" otherwise
	list    browseList[rootPickerEntry]
	cursor  int    // frameSubmenu/frameRunMode/frameWatchMode only
	account string // frameRunMode only: the account chosen in STEP 2
	// interval is frameWatchMode only (decision 0045): the refresh interval
	// chosen on the screen's own Interval row, carried here so Enter on a
	// mode row can build the final argv. A one-run value, never persisted.
	interval time.Duration

	// command is what a frameRunAccount/frameEmailInput frame does once an
	// account is chosen (decision 0039). Empty means the run flow's own
	// STEP 2 — Enter pushes RUN MODE, as it always has. Non-empty ("logout",
	// "remove", "login") means this frame was pushed to supply that
	// command's own EMAIL argument, so Enter finalizes `m.picked` as
	// {command, email} instead, and pickCommandArgs returns it without
	// prompting for the argument a second time.
	//
	// frameRunAccount's name is historical — the interactive run flow
	// (decision 0019) introduced it, and decision 0039 widened it to every
	// step that picks an already-registered account rather than renaming it
	// across ~90 call sites and assertions.
	command string
}

// rootPickerApp is the bare-`cpro` interactive command picker: a compact
// command palette (one row per command, no group headings or "├─"
// separators — just a blank rail row between groups while browsing, dropped
// entirely once search narrows the list to one flat result set) sharing
// configApp's visual language (styleText/renderFooter, tui.go) — the
// always-visible below-panel description for just the highlighted row, the
// five-shade semantic-group rail, and search-as-you-type with no explicit
// "/" shortcut don't fit a huh Select cleanly. Two states in one screen
// (normal browsing and search) rather than two tea.Programs, so there's no
// flicker switching between them: typing a printable character while
// browsing switches straight into search with that character as the first
// of the query, and clearing the query (Backspace to empty, or Esc)
// switches straight back.
//
// Every screen this program can show past its own outermost frame — the
// full "MENU" palette, the "SYSTEM" submenu — is a frame on m.stack
// (navStack[rootFrame], tui.go): pushMenu/pushSystem push one, Esc/←
// (updateNormal/updateSubmenu) pop it — one generic mechanism replacing what
// used to be two different one-off fields (a submenu bool, a
// launcherEntries slice swap). "config" is the one entry that leaves this
// program entirely (a separate `cpro config` cobra command/process); Esc
// backing all the way out of *that* screen is handled one level up, in
// pickCommandArgs, by relaunching this program with its stack resumed
// exactly where "config" was picked from — see pickCommandArgs's own doc
// comment for why that one case can't just be another pushed frame.
//
// Esc while browsing the stack's own outermost frame uses cpro's usual
// double-Esc-to-exit convention (exitArmed), the same as the huh-based
// prompts one level down (escGuardField, ui.go) — signaled two ways at
// once: the footer's own key hint changes from "Esc² to exit" to
// "Esc¹ again" in the configured danger color (dangerColor, ui.go), and
// railShade turns the whole five-shade rail/both panel edges that same
// solid color, replacing the gradient rather than tinting it; any key other
// than a second Esc cancels the arm (and still does its own normal thing).
// The arm also expires on its own after exitArmTimeout via a
// generation-tagged tea.Tick (arm/exitArmExpiredMsg), so walking away
// doesn't leave a stray next keystroke exiting the picker. Esc while
// actively searching keeps its own, different meaning (clear the query) and
// never arms exit or pops a frame at all.
type rootPickerApp struct {
	root  *cobra.Command
	s     *store // set by pickCommandArgs; used by pushRunAccount/pushRunMode (decision 0019) to read accounts/the saved permission default. nil in the pure unit tests that construct this type directly without it — pushRunAccount degrades to a clear error, pushRunMode to defaultPermissionMode
	stack navStack[rootFrame]

	color  bool
	width  int
	height int      // terminal rows (tea.WindowSizeMsg); 0 until the first one arrives — see visibleRows
	shades []string // deriveAccentShades(accentMode, len(rootGroups)); shades[0] darkest, shades[len-1] == accentMode

	exitArmed    bool // true after a first Esc at the stack's own root frame; a second consecutive Esc exits — see updateNormal
	exitArmedGen int  // bumped every time exitArmed is newly set (never on cancel) — see exitArmExpiredMsg

	emailInput string // frameEmailInput only: the address typed so far (decision 0039)
	emailErr   string // normalizeEmail's own rejection message for emailInput, shown under the field; cleared on the next keystroke

	picked     []string // the chosen command (plus args), set right before tea.Quit; nil means cancelled
	runFlowErr error    // set instead of picking anything when the run flow (decision 0019) can't proceed — e.g. no registered accounts; shown inline (flowErrorLine) and cleared on the next keystroke, and still returned by pickCommandArgs if the program exits with one set (decision 0043)

	// accountUsage is RUN ACCOUNT's Session-usage cache (decision 0031),
	// keyed by the *real* email — never a masked alias, which is a display
	// string only (decision 0024/0030). It lives on the app, not the frame,
	// for two reasons: a row's usage survives entering and leaving search
	// (so filtering re-renders from cache and never refetches, which the
	// spec requires), and an in-flight fetch that lands after the frame was
	// popped updates a harmless map entry instead of a stale frame.
	//
	// configApp (configui.go) carries the identical field for DEFAULT
	// ACCOUNT (decision 0036) — a separate map, since the two screens are
	// separate tea.Programs with no shared process-wide state to hold one
	// in — but both are filled by the same fetchAccountPickerUsage and read
	// by the same accountPickerRow, so there is exactly one usage-fetch
	// implementation and one row-rendering implementation behind both.
	accountUsage map[string]runAccountUsage
}

// runAccountUsage is one account's Session/Week usage cell in the RUN
// ACCOUNT picker. The zero value is the pre-arrival state every row starts
// in, which is exactly what renders as the "S ·  --" placeholder: a row is
// always selectable, whether or not its usage ever arrives. session and week
// come from the same accountUsage fetch (usage.go), so they always load or
// fail together — there's no case where one is known and the other isn't.
type runAccountUsage struct {
	loaded  bool    // a fetch finished for this account — with or without an error
	failed  bool    // that fetch failed; keep the "--" placeholder rather than showing 0%
	session float64 // five-hour window utilization, 0-100
	week    float64 // seven-day window utilization, 0-100
}

// runAccountUsageMsg carries one account's finished usage fetch back into the
// tea loop. One message per account rather than one for the whole batch, so
// each row updates in place the moment its own fetch lands and a single slow
// account never holds up the others.
type runAccountUsageMsg struct {
	email string
	usage runAccountUsage
}

// fetchAccountPickerUsage returns one tea.Cmd per account, to be run as a
// tea.Batch: bubbletea executes each in its own goroutine, which is what makes
// these concurrent without this file hand-rolling a WaitGroup/channel fan-in
// the way renderAccountSnapshot (main.go) has to for its own synchronous,
// non-tea context. Both go through the same loadUsage (usage.go) — the shared
// service cpro status/watch already use, including its on-disk cache, failure
// backoff and stale-value fallback (decision 0065), so there is no second
// usage cache or freshness policy here. A stale cached value still renders as
// a real percentage (the best available answer for picking an account); an
// outright failure with no cache, or a session window that has reset since
// the value was fetched, falls back to "--".
//
// A free function (originally fetchRunAccountUsage, a *rootPickerApp method)
// since decision 0036: DEFAULT ACCOUNT (configui.go) needs the identical
// fetch — same loadUsage source, same runAccountUsageMsg shape — from a
// different tea.Model with its own *store, not a rootPickerApp receiver.
func fetchAccountPickerUsage(s *store, emails []string) tea.Cmd {
	cmds := make([]tea.Cmd, 0, len(emails))
	for _, email := range emails {
		cmds = append(cmds, func() tea.Msg {
			usage, _, err := loadUsage(s.profile(email), usageEndpoint)
			// A session window that reset since a stale value was fetched has no
			// meaningful percentage left (decision 0065): "--", not the old one.
			if err != nil || windowElapsed(usage.FiveHour) {
				return runAccountUsageMsg{email: email, usage: runAccountUsage{loaded: true, failed: true}}
			}
			return runAccountUsageMsg{email: email, usage: runAccountUsage{loaded: true, session: usage.FiveHour.Utilization, week: usage.SevenDay.Utilization}}
		})
	}
	return tea.Batch(cmds...)
}

// exitArmTimeout is how long a first Esc at the stack's own root frame stays
// armed before automatically expiring back to the normal "Esc² to exit"
// footer — long enough to comfortably press a second Esc, short enough that
// walking away doesn't leave the picker silently primed to exit on the next
// stray keystroke.
const exitArmTimeout = 2 * time.Second

// exitArmExpiredMsg is what arm's own tea.Tick delivers once exitArmTimeout
// elapses. gen is the exitArmedGen value that was current when this
// particular timer was started — Update only actually clears exitArmed if
// gen still matches m.exitArmedGen, so a stale timer from an arm that was
// already cancelled (and possibly re-armed since) can never clear a newer
// one; see arm's own doc comment for the race this avoids.
type exitArmExpiredMsg struct{ gen int }

// arm sets exitArmed and starts a fresh exitArmTimeout, returning the
// tea.Cmd that delivers exitArmExpiredMsg once it elapses. Bumping
// exitArmedGen here — and nowhere else — is what makes a stale timer
// harmless: Esc, then some other key (cancel, gen unchanged), then Esc again
// (re-arm, gen bumped) leaves the *first* Esc's now-stale timer carrying the
// *old* gen, so when it eventually fires it fails the gen check in Update
// and does nothing, instead of incorrectly clearing the second arm.
func (m *rootPickerApp) arm() tea.Cmd {
	m.exitArmed = true
	m.exitArmedGen++
	gen := m.exitArmedGen
	return tea.Tick(exitArmTimeout, func(time.Time) tea.Msg { return exitArmExpiredMsg{gen: gen} })
}

// popFrame pops one frame, clearing the revealed frame's own search state so
// backing out one level lands on the plain browse list it left from. Search
// state now lives per frame (rootFrame.list, browseui.go) rather than on the
// app, so without this a query typed before pushing a child would still be
// active when the user came back.
func (m *rootPickerApp) popFrame() {
	if m.stack.pop() {
		m.stack.current().list.clearSearch()
	}
}

// pushMenu transitions this picker from its current frame into the full
// command palette, remembering exactly where it was pushed from on
// m.stack — Esc/← pops back to it with a single press (updateNormal),
// never exitArmed's double-Esc, which stays reserved for the stack's own
// root frame. "menu" is never itself returned as a picked command, the same
// way "system" (pushSystem, below) is never picked directly but opens its
// own submenu frame instead.
func (m *rootPickerApp) pushMenu() {
	m.stack.push(rootFrame{kind: frameList, title: "MENU", list: newCommandBrowseList(rootPickerFilteredEntries(m.root, rootMenuNames()))})
	applyLiveMeta(m.stack.current().list.items, m.s)
}

// pushSystem transitions this picker into the "System credentials" submenu —
// export/import, systemSubmenuItems (system.go) — the one deliberate
// exception to this screen being otherwise a flat searchable list. Esc/←
// pops it with a single press, same as pushMenu.
func (m *rootPickerApp) pushSystem() {
	m.stack.push(rootFrame{kind: frameSubmenu, title: "SYSTEM"})
}

// watchModeItems are the two mode rows the WATCH MODE screen shows (decision
// 0029) — "Full" maps to plain `cpro watch`, "Compact" to `cpro watch
// --compact` (updateWatchMode), reusing watch's own existing implementation
// entirely; this frame only ever decides which already-supported invocation
// to build.
var watchModeItems = []struct{ label, desc string }{
	{"Full", "One block per account: status, Session, Week, active sessions"},
	{"Compact", "One row per account with side-by-side Session/Week bars"},
}

// watchIntervalChoices are the refresh intervals the WATCH MODE screen's own
// Interval row steps through (decision 0045) — the same fixed set whether or
// not it matches `cpro watch --interval`'s 5s minimum, which every choice
// here clears. 60s (index 3, watchDefaultInterval) is the default, matching
// both `cpro watch`'s own flag default and the 60s usage cache.
var watchIntervalChoices = []time.Duration{
	5 * time.Second, 15 * time.Second, 30 * time.Second,
	time.Minute, 5 * time.Minute, 15 * time.Minute,
}

// watchDefaultInterval is the interval WATCH MODE starts on, and the one the
// picker omits from the argv entirely — plain `cpro watch` already defaults
// to it, so building only ["watch"] there keeps the picked command
// byte-identical to what typing it by hand produces.
const watchDefaultInterval = time.Minute

// watchIntervalLabel renders a duration the way the Interval row and the
// --interval flag read it: "5s"/"15s"/"30s"/"1m"/"5m"/"15m", never the
// time.Duration String form's trailing "0s" ("1m0s").
func watchIntervalLabel(d time.Duration) string {
	if d >= time.Minute && d%time.Minute == 0 {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%ds", int(d.Seconds()))
}

// adjustWatchInterval steps d to the neighbouring entry in
// watchIntervalChoices, clamping at either end (there is no wrap: 5s and 15m
// are hard stops) and falling back to the default for an unrecognized value.
func adjustWatchInterval(d time.Duration, delta int) time.Duration {
	idx := -1
	for i, choice := range watchIntervalChoices {
		if choice == d {
			idx = i
			break
		}
	}
	if idx < 0 {
		idx = 3 // watchDefaultInterval's own index
	}
	idx = max(0, min(len(watchIntervalChoices)-1, idx+delta))
	return watchIntervalChoices[idx]
}

// watchArgv builds the argv WATCH MODE's own Enter/→ produces for frame: the
// selected mode's flag (full is bare) plus --interval only when it differs
// from the default, so the common case stays byte-identical to typing `cpro
// watch`/`cpro watch --compact` by hand, and the interval is a one-run choice
// that is never persisted (decision 0045).
func watchArgv(frame *rootFrame) []string {
	argv := []string{"watch"}
	if frame.cursor == 1 {
		argv = append(argv, "--compact")
	}
	if frame.interval != 0 && frame.interval != watchDefaultInterval {
		argv = append(argv, "--interval", watchIntervalLabel(frame.interval))
	}
	return argv
}

// pushWatchMode transitions this picker into the WATCH MODE screen —
// selecting "watch →" no longer launches `cpro watch` directly, matching the
// interactive run flow's own "run" (STEP 2/3) and "session" precedent of
// opening a further screen rather than executing immediately. There's no
// persisted watch-mode preference to preselect from (the task's own spec
// says not to introduce one), so this always starts on "Full" (cursor 0) and
// the 60s default interval, the same defaults plain `cpro watch` (no flags)
// already has; the interval chosen here applies to that one run only.
func (m *rootPickerApp) pushWatchMode() {
	m.stack.push(rootFrame{kind: frameWatchMode, title: "WATCH MODE", interval: watchDefaultInterval})
}

// pushRunAccount transitions into STEP 2 of the interactive run flow
// (decision 0019): pick which account this one run uses. A per-run choice
// only — it never writes back config.Default, the separate setting
// non-interactive `cpro run` reads instead (Settings' own "Run defaults"
// group, configui.go). Returns an error instead of pushing when there are no
// registered accounts to choose from, mirroring the old pickAccount's own
// "no accounts found" error (ui.go, removed) — the caller (activateEntry)
// stores it in m.runFlowErr, which the current frame renders inline instead
// of closing (decision 0043).
//
// The returned tea.Cmd starts every account's Session-usage fetch (decision
// 0031) — the frame is pushed and rendered immediately regardless, so the list
// is navigable before any usage arrives; each row fills in as its own fetch
// lands (runAccountUsageMsg). Accounts already in m.accountUsage from an
// earlier visit to this frame are not refetched: the cache is what keeps
// re-entering the screen, and searching within it, from reissuing requests.
func (m *rootPickerApp) pushRunAccount() (tea.Cmd, error) {
	return m.pushAccountPick("", "RUN ACCOUNT")
}

// pushAccountPick is pushRunAccount generalized (decision 0039): the same
// searchable, usage-annotated list of registered accounts, pushed either for
// the run flow's own STEP 2 (command "", title "RUN ACCOUNT") or to supply an
// already-registered EMAIL argument to another command — `logout`/`remove`,
// which previously dropped out of the picker into a plain huh text field
// asking the user to retype an address they had already registered.
//
// command is stored on the frame and is what its own Enter/→ consults; see
// rootFrame.command and updateRunAccount. Everything else — rendering,
// search, the shared usage cache — is identical for every caller, which is
// the point: decision 0036's own invariant is that any picker where the user
// must choose an account shows the same live Session usage, through the same
// component.
func (m *rootPickerApp) pushAccountPick(command, title string) (tea.Cmd, error) {
	if m.s == nil {
		return nil, fmt.Errorf("no accounts found; run cpro login EMAIL")
	}
	c, err := m.s.read()
	if err != nil {
		return nil, err
	}
	if len(c.Accounts) == 0 {
		return nil, fmt.Errorf("no accounts found; run cpro login EMAIL")
	}
	emails := make([]string, 0, len(c.Accounts))
	for email := range c.Accounts {
		emails = append(emails, email)
	}
	sort.Strings(emails)
	entries := make([]rootPickerEntry, len(emails))
	for i, email := range emails {
		entries[i] = rootPickerEntry{name: email}
	}
	m.stack.push(rootFrame{kind: frameRunAccount, title: title, list: newAccountEntryBrowseList(entries), command: command})

	if m.accountUsage == nil {
		m.accountUsage = map[string]runAccountUsage{}
	}
	pending := make([]string, 0, len(emails))
	for _, email := range emails {
		if _, ok := m.accountUsage[email]; !ok {
			pending = append(pending, email)
		}
	}
	if len(pending) == 0 {
		return nil, nil
	}
	return fetchAccountPickerUsage(m.s, pending), nil
}

// pushAccountArg opens whichever screen supplies name's own EMAIL argument
// (decision 0039), which differs by what the address has to be:
//
//   - logout/remove act on an account cpro already has a profile for, so
//     they get the searchable account list (pushAccountPick) — the same one
//     RUN ACCOUNT uses, live Session usage included. Retyping an address the
//     tool already knows was the whole complaint.
//   - login registers a *new* account, so there is nothing to list; it gets
//     the free-text field instead (pushEmailInput).
//
// Only the list case can fail (no registered accounts yet), and it fails the
// same way pushRunAccount always has: a clear error the caller surfaces via
// m.runFlowErr, rendered inline in the frame (decision 0043).
func (m *rootPickerApp) pushAccountArg(name string) (tea.Cmd, error) {
	title := accountArgCommands[name]
	if name == "login" {
		m.pushEmailInput(name, title)
		return nil, nil
	}
	return m.pushAccountPick(name, title)
}

// pushEmailInput transitions into the free-text email field (decision 0039).
// Unlike pushAccountPick's list, `login`'s own EMAIL is by definition an
// account cpro does not know yet, so there is nothing to pick from — it has to
// be typed. It used to be typed into a huh field that took over the terminal
// as its own separate program, visually unrelated to the picker it was
// launched from; this keeps it inside the same bubbletea program and the same
// panel chrome every other screen draws with.
func (m *rootPickerApp) pushEmailInput(command, title string) {
	m.stack.push(rootFrame{kind: frameEmailInput, title: title, command: command})
	m.emailInput, m.emailErr = "", ""
}

// updateEmailInput handles the email field: printable characters append,
// Backspace deletes, Esc/← pops back, and Enter validates through the exact
// same normalizeEmail every non-interactive `cpro login EMAIL` already runs
// (main.go's accountCommand), so the picker can never accept an address the
// command itself would reject. A rejected address stays on screen with the
// validator's own message rather than clearing what was typed — the whole
// point of keeping this in-app is that a typo is correctable in place.
func (m *rootPickerApp) updateEmailInput(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	frame := m.stack.current()
	switch msg.String() {
	case "esc", "left":
		m.popFrame()
		m.emailInput, m.emailErr = "", ""
	case "backspace":
		r := []rune(m.emailInput)
		if len(r) > 0 {
			m.emailInput = string(r[:len(r)-1])
		}
		m.emailErr = ""
	case "enter":
		email, err := normalizeEmail(m.emailInput)
		if err != nil {
			m.emailErr = err.Error()
			return m, nil
		}
		m.picked = []string{frame.command, email}
		return m, tea.Quit
	default:
		if text := msg.Key().Text; text != "" {
			m.emailInput += text
			m.emailErr = ""
		}
	}
	return m, nil
}

// viewEmailInput renders the email field in the same single-color panel every
// other small screen here uses (renderPanel, tui.go — not the main list's
// five-shade rail), with the typed value followed by the same "_" cursor
// searchLine already draws, so typing here reads identically to typing in any
// search field. A validation message renders in dangerColor below the field.
func (m *rootPickerApp) viewEmailInput() string {
	frame := m.stack.current()
	lines := []string{"  Email: " + m.emailInput + styleText(m.color, "_", accentMode)}
	if m.emailErr != "" {
		lines = append(lines, "", "  "+styleText(m.color, m.emailErr, dangerColor))
	}
	body := renderPanel(m.color, accentMode, screenTitle(frame.title), lines)
	footer := renderFooter(m.color, accentMode,
		[2]string{"↵", "Continue"}, [2]string{"←", "Back"}, [2]string{"Esc", "Back"})
	return body + "\n\n" + footer
}

// pushRunMode transitions into STEP 3: pick a permission mode for this one
// run, preselected from the default that applies to the account chosen in STEP
// 2 — its own per-account override when it has one, otherwise the saved global
// Settings default (c.PermissionMode); see permissionModeForAccount. A sensible
// starting point, per the spec, without forcing a reselection every time;
// changing it here is a one-run override only, never persisted (see
// updateRunMode). account is the email chosen in STEP 2, carried on the new
// frame so this frame's own Enter can build the final argv without threading
// it through rootPickerApp itself.
func (m *rootPickerApp) pushRunMode(account string) {
	mode := defaultPermissionMode
	if m.s != nil {
		if c, err := m.s.read(); err == nil {
			mode = permissionModeForAccount(c, account)
		}
	}
	m.stack.push(rootFrame{kind: frameRunMode, title: "RUN MODE", cursor: permissionModeIndex(mode), account: account})
}

// pickCommandArgs drives one command picker to completion — the reduced
// bare-`cpro` launcher or the full `cpro menu` palette, depending only on
// which names it's given (rootLauncherNames or rootMenuNames; see
// rootPickerFilteredEntries) — and returns the command name (plus any
// required positional arguments, via the existing pickRequiredArgs/
// requiredArgTokens — unchanged) to run, exactly as if the user had typed it
// directly. It does not run the command itself: the caller (root.RunE /
// `menu`'s RunE, main.go) still does that through the normal cmd.Execute()
// path, so command logic is never duplicated between the two pickers or
// between either picker and direct CLI use.
//
// "config" is special-cased in the loop below because it's the one entry
// this screen can't push as one of its own frames: `cpro config` is a
// separate cobra command with its own interactive program (configApp,
// configui.go), not a state this rootPickerApp can render in place. Rather
// than returning ["config"] straight up to the caller (which would run it
// and end the whole picker, with no way back), this loop runs it itself,
// right here, and — if the user backs out of it (runConfigUI's own backOut,
// which only ever happens because it was opened with hasParent: true) —
// relaunches this same picker with its navStack resumed exactly as it stood
// the moment "config" was picked (captured in resume below), so the round
// trip through a second program looks, from the outside, like popping back
// to Menu the same way any in-process frame does. "default" gets the
// identical round trip, just against runDefaultUI instead of runConfigUI —
// see the loop below.
//
// "run" (decision 0019) doesn't leave this program at all — RUN ACCOUNT/RUN
// MODE are ordinary pushed frames — but its own picked argv (built by
// updateRunMode) still gets special handling here, after the tea program
// exits: the decision-0007 "Running: ..." announcement is printed here,
// once, in the same spot "config" does its own post-exit work, rather than
// from inside the live bubbletea program (which must never interleave a raw
// Println with its own rendering).
func pickCommandArgs(root *cobra.Command, names []string, title string) ([]string, error) {
	if !terminalInput() || !terminalOutput(root.ErrOrStderr()) {
		return nil, fmt.Errorf("the picker requires an interactive terminal; run cpro --help")
	}
	root.InitDefaultHelpCmd()
	root.InitDefaultCompletionCmd()

	s, err := openStore()
	if err != nil {
		return nil, err
	}

	byName := make(map[string]*cobra.Command)
	for _, c := range root.Commands() {
		if c.IsAvailableCommand() || c.Name() == "help" {
			byName[c.Name()] = c
		}
	}

	var resume *navStack[rootFrame]
	for {
		m := &rootPickerApp{
			root:   root,
			s:      s,
			color:  tuiColorEnabled(root.ErrOrStderr()),
			shades: deriveAccentShades(accentMode, len(rootGroups)),
		}
		if resume != nil {
			m.stack = *resume
		} else {
			m.stack = newNavStack(rootFrame{kind: frameList, title: title, list: newCommandBrowseList(rootPickerFilteredEntries(root, names))})
			applyLiveMeta(m.stack.current().list.items, s)
		}
		// bubbletea gets the raw terminal file directly, not root.ErrOrStderr():
		// under NO_COLOR/TERM=dumb that's a *colorprofile.Writer (see uiOutput,
		// ui.go), not a real *os.File, and without one to drive raw-mode/cursor
		// control directly, bubbletea's render loop never produces a first frame
		// at all — confirmed live (it spins indefinitely instead of erroring).
		// This needs no color-profile option of its own either way: m.color
		// already decides, in every string this screen builds (styleText/
		// dimStyle), whether a color escape is emitted in the first place, so
		// there's nothing color-related left for bubbletea itself to convert.
		//
		// View() sets tea.View.AltScreen: the full command list (12 rows, plus
		// the panel edges, description, and footer) is taller than plenty of
		// real terminal windows. Without a dedicated alt-screen buffer,
		// bubbletea's default renderer repositions the cursor with *relative*
		// up-moves, assuming the previous frame is still fully on screen; once a
		// frame taller than the window has forced the terminal to scroll, that
		// assumption breaks and every subsequent redraw overlaps stale content
		// instead of replacing it — confirmed live on a short pty. The alt
		// screen sidesteps this entirely (the same fix every full-screen
		// terminal picker/launcher uses) at the cost of taking over the whole
		// terminal while open and restoring its prior contents on exit,
		// standard for a screen this size — cpro config's smaller settings menu
		// doesn't need it and isn't changed here.
		p := tea.NewProgram(m, tea.WithContext(root.Context()), tea.WithInput(os.Stdin), tea.WithOutput(os.Stderr))
		if _, err := p.Run(); err != nil {
			return nil, err
		}
		if m.runFlowErr != nil {
			return nil, m.runFlowErr
		}
		if m.picked == nil {
			// Esc/Ctrl+C: cliError (ui.go) special-cases this exact sentinel to exit
			// quietly rather than showing an ERROR box — cancelling a picker isn't an
			// error, matching every other picker in cpro (huh's own Esc/Ctrl+C path
			// returns the same error).
			return nil, huh.ErrUserAborted
		}
		if m.picked[0] == "run" {
			// updateRunMode already built the full argv — exactly what typing
			// `cpro run --account EMAIL <flags>` by hand would produce. Parse
			// it back apart with the same runArgs (main.go) `cpro run` itself
			// uses, purely to announce it (decision 0007) before handing it
			// back to be executed identically to any other picked command.
			email, forwarded, _, err := runArgs(m.picked[1:])
			if err != nil {
				return nil, err
			}
			announceCommand(root, email, forwarded)
			return m.picked, nil
		}
		if m.picked[0] == "config" {
			c, err := s.read()
			if err != nil {
				return nil, err
			}
			if backOut, err := runConfigUI(root, s, c, true); err != nil {
				return nil, err
			} else if backOut {
				// Backing out of a separate program (config/default/session)
				// resumes this frame exactly as it stood — but with its
				// search cleared, so returning lands on the plain browse
				// list rather than re-entering the query that was used to
				// find the row in the first place.
				m.stack.current().list.clearSearch()
				resume = &m.stack
				continue
			}
		}
		if m.picked[0] == "session" {
			// Same round trip as "config"/"default" above: runSessionUI is
			// a separate program (sessionui.go), not a frame this screen can
			// render in place. Unlike either of those, a completed pick here
			// (the user drilled into "continue →", chose a session and a
			// destination account) doesn't just persist something and pop —
			// it produces a real argv to execute, exactly like the "run" case
			// above, so it's returned immediately rather than falling through
			// to the resume loop.
			c, err := s.read()
			if err != nil {
				return nil, err
			}
			picked, backOut, err := runSessionUI(root, s, c, true)
			if err != nil {
				return nil, err
			}
			if picked != nil {
				return picked, nil
			}
			if backOut {
				// Backing out of a separate program (config/default/session)
				// resumes this frame exactly as it stood — but with its
				// search cleared, so returning lands on the plain browse
				// list rather than re-entering the query that was used to
				// find the row in the first place.
				m.stack.current().list.clearSearch()
				resume = &m.stack
				continue
			}
		}
		if m.picked[0] == "default" {
			// Same round trip as "config" above: runDefaultUI (formerly two
			// separate programs, runPermissionsUI and runDefaultAccountUI —
			// merged into one "SET DEFAULT ACCOUNT" screen) is a separate
			// program, not a frame this screen can render in place. A change
			// made there (the permission mode, or the default account) IS
			// reflected on this screen's own MENU row once resumed —
			// applyLiveMeta refreshes that frame's "default" entry from the
			// freshly read config before resuming, so the row's colored dot
			// never shows a stale value.
			c, err := s.read()
			if err != nil {
				return nil, err
			}
			if backOut, err := runDefaultUI(root, s, c, true); err != nil {
				return nil, err
			} else if backOut {
				applyLiveMeta(m.stack.current().list.items, s)
				// Backing out of a separate program (config/default/session)
				// resumes this frame exactly as it stood — but with its
				// search cleared, so returning lands on the plain browse
				// list rather than re-entering the query that was used to
				// find the row in the first place.
				m.stack.current().list.clearSearch()
				resume = &m.stack
				continue
			}
		}
		if _, ok := accountArgCommands[m.picked[0]]; ok && len(m.picked) > 1 {
			// login/logout/remove already carry the EMAIL their own in-app
			// frame collected (decision 0039) — falling through would ask
			// pickRequiredArgs for it a second time, in the separate huh
			// program this change exists to get rid of.
			return m.picked, nil
		}
		values, err := pickRequiredArgs(root, byName[m.picked[0]])
		if err != nil {
			return nil, err
		}
		return append(m.picked, values...), nil
	}
}

func (m *rootPickerApp) Init() tea.Cmd { return nil }

func (m *rootPickerApp) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil
	case exitArmExpiredMsg:
		if msg.gen == m.exitArmedGen {
			m.exitArmed = false
		}
		return m, nil
	case runAccountUsageMsg:
		// Record and re-render only (decision 0031). Deliberately touches
		// neither the current list's cursor nor its fcursor: usage arriving
		// mid-navigation must never move the selection out from under whoever
		// is already picking an account.
		if m.accountUsage == nil {
			m.accountUsage = map[string]runAccountUsage{}
		}
		m.accountUsage[msg.email] = msg.usage
		return m, nil
	case tea.KeyMsg:
		// Any key dismisses a previously shown frame-open error (decision
		// 0043) — it has been read, and the next action either succeeds or
		// sets its own.
		m.runFlowErr = nil
		if msg.String() == "ctrl+c" {
			m.picked = nil
			return m, tea.Quit
		}
		switch m.stack.current().kind {
		case frameSubmenu:
			return m.updateSubmenu(msg)
		case frameRunAccount:
			if m.stack.current().list.searching() {
				return m.updateRunAccountSearch(msg)
			}
			return m.updateRunAccount(msg)
		case frameRunMode:
			return m.updateRunMode(msg)
		case frameWatchMode:
			return m.updateWatchMode(msg)
		case frameEmailInput:
			return m.updateEmailInput(msg)
		case frameRemoveConfirm:
			return m.updateRemoveConfirm(msg)
		}
		if m.stack.current().list.searching() {
			return m.updateSearch(msg)
		}
		return m.updateNormal(msg)
	}
	return m, nil
}

func (m *rootPickerApp) View() tea.View {
	var content string
	switch m.stack.current().kind {
	case frameSubmenu:
		content = m.viewSubmenu()
	case frameRunAccount:
		if m.stack.current().list.searching() {
			content = m.viewRunAccountSearch()
		} else {
			content = m.viewRunAccount()
		}
	case frameRunMode:
		content = m.viewRunMode()
	case frameWatchMode:
		content = m.viewWatchMode()
	case frameEmailInput:
		content = m.viewEmailInput()
	case frameRemoveConfirm:
		content = m.viewRemoveConfirm()
	default:
		if m.stack.current().list.searching() {
			content = m.viewSearch()
		} else {
			content = m.viewNormal()
		}
	}
	v := tea.NewView(content)
	v.AltScreen = true // see pickCommandArgs for why this screen needs it
	return v
}

// updateSubmenu handles the "System credentials" submenu: up/down between
// its two entries, Enter picks "system export"/"system import" (run exactly
// like any other picked command — see pickCommandArgs), Esc/← pops back to
// whatever frame pushed it.
func (m *rootPickerApp) updateSubmenu(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	frame := m.stack.current()
	switch msg.String() {
	case "up":
		frame.cursor = (frame.cursor - 1 + len(systemSubmenuItems)) % len(systemSubmenuItems)
	case "down":
		frame.cursor = (frame.cursor + 1) % len(systemSubmenuItems)
	case "esc", "left":
		m.popFrame()
	case "enter":
		m.picked = []string{"system", systemSubmenuItems[frame.cursor].key}
		return m, tea.Quit
	}
	return m, nil
}

// viewSubmenu renders the "System credentials" submenu: a small, single-color
// panel (renderPanel, tui.go — same as configApp's own sub-screens) rather
// than the main list's five-shade rail, since there's no grouping to show for
// two items. No search here either — two items doesn't need it.
func (m *rootPickerApp) viewSubmenu() string {
	frame := m.stack.current()
	labelWidth := 0
	for _, item := range systemSubmenuItems {
		labelWidth = max(labelWidth, visibleWidth(item.label))
	}
	lines := make([]string, len(systemSubmenuItems))
	for i, item := range systemSubmenuItems {
		cursor := "  "
		if i == frame.cursor {
			cursor = styleText(m.color, "❯", accentMode) + " "
		}
		lines[i] = cursor + padEnd(item.label, labelWidth+7) + item.desc
	}
	panel := renderPanel(m.color, accentMode, screenTitle(frame.title), lines)
	footer := renderFooter(m.color, accentMode, [2]string{"↑↓", "Navigate"}, [2]string{"←", "Back"}, [2]string{"↵", "Select"}, [2]string{"Esc", "Back"})
	return panel + "\n\n" + footer
}

// rootForward gates the → alias on this screen: → opens only a row that leads
// to a further screen (isForwardEntry), never a leaf command, while Enter
// always acts. Passed to the shared browseList's key handler.
func rootForward(e rootPickerEntry) bool { return isForwardEntry(e.name) }

// activateEntry is what Enter (or →, on a forward entry) does with the
// highlighted command row, from either the browse list or a search result —
// one shared action so a command that opens a further screen (menu/system/
// run/watch/login/logout/remove) behaves identically whether it was found by
// browsing or by typing its name, and every other command is picked as the
// argv to execute.
func (m *rootPickerApp) activateEntry(e rootPickerEntry) (tea.Model, tea.Cmd) {
	switch e.name {
	case "system":
		// "system" isn't runnable on its own (main.go's newSystemCommand has
		// no RunE) — it opens its own submenu frame instead of picking a
		// command directly.
		m.pushSystem()
		return m, nil
	case "menu":
		m.pushMenu()
		return m, nil
	case "run":
		// "run" isn't picked directly either (decision 0019) — it opens STEP 2
		// of the interactive run flow instead.
		cmd, err := m.pushRunAccount()
		if err != nil {
			// Shown inline (viewNormal/viewSearch) and the picker stays open,
			// rather than closing and printing the reason afterwards
			// (decision 0043).
			m.runFlowErr = err
			return m, nil
		}
		return m, cmd
	case "watch":
		// "watch" isn't picked directly either (decision 0029) — it opens the
		// WATCH MODE screen instead, matching "run"'s own shape.
		m.pushWatchMode()
		return m, nil
	case "login", "logout", "remove":
		// Decision 0039: these supply their own EMAIL argument in-app now,
		// rather than being picked bare and leaving pickRequiredArgs to prompt
		// for it in a separate program.
		cmd, err := m.pushAccountArg(e.name)
		if err != nil {
			m.runFlowErr = err
			return m, nil
		}
		return m, cmd
	}
	m.picked = []string{e.name}
	return m, tea.Quit
}

func (m *rootPickerApp) updateNormal(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	frame := m.stack.current()
	// Any key other than Esc cancels an armed exit and still performs its own
	// normal action in the same keystroke — the same "any other key clears the
	// warning" convention escGuardField (ui.go) uses for the double-Esc
	// prompts one level down, just expressed here as the footer's own
	// "Esc¹ again" reverting to "Esc² to exit" (viewNormal) instead of a
	// printed warning line — any pending exitArmTimeout for this arm is left
	// to expire on its own; arm's exitArmedGen check is what keeps it from
	// mattering once it does (see arm's own doc comment). Key handling itself
	// (movement, type-to-search, the → gate) is the shared browseList's
	// (browseui.go).
	if msg.String() != "esc" {
		m.exitArmed = false
	}
	switch frame.list.browseKey(msg, browseKeyOpts[rootPickerEntry]{leftBack: true, forward: rootForward}) {
	case browseSelect:
		if e, ok := frame.list.selected(); ok {
			return m.activateEntry(e)
		}
	case browseBack:
		// A single press backs out one level — never exitArmed's double-Esc,
		// which stays reserved for actually leaving the picker from its own
		// outermost frame. ← at the stack's own root is deliberately a no-op:
		// only Esc arms exit there.
		if msg.String() == "left" && m.stack.atRoot() {
			return m, nil
		}
		if !m.stack.atRoot() {
			m.popFrame()
			return m, nil
		}
		if m.exitArmed {
			m.picked = nil
			return m, tea.Quit
		}
		return m, m.arm()
	}
	return m, nil
}

func (m *rootPickerApp) updateSearch(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	frame := m.stack.current()
	if frame.list.searchKey(msg, browseKeyOpts[rootPickerEntry]{forward: rootForward}) == browseSelect {
		if e, ok := frame.list.searchSelected(); ok {
			return m.activateEntry(e)
		}
	}
	return m, nil
}

// updateRunAccount handles STEP 2 (RUN ACCOUNT, decision 0019) while
// browsing (not actively searching): up/down over the registered-account
// list, Esc/← pops back to STEP 1, Enter or → proceeds to STEP 3
// (pushRunMode) with the highlighted account — decision 0022 added → as a
// consistent forward-select alias for Enter here, matching every other
// screen's own →/Enter convention, even though an account row isn't a
// "forward entry" in the isForwardEntry sense (it's a selectable value, not
// a child menu — see viewRunAccount, which deliberately shows no trailing
// " →" on any row) — and typing starts a search exactly like the top-level
// command list does (updateNormal's own default case).
func (m *rootPickerApp) updateRunAccount(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	frame := m.stack.current()
	switch frame.list.browseKey(msg, browseKeyOpts[rootPickerEntry]{leftBack: true, forward: forwardAll[rootPickerEntry]}) {
	case browseSelect:
		if e, ok := frame.list.selected(); ok {
			return m.chooseAccount(frame, e.name)
		}
	case browseBack:
		m.popFrame()
	}
	return m, nil
}

// accountEnterHint is what an account list's own footer says Enter does
// (decision 0039). The run flow genuinely continues to another step, but
// logout/remove execute the moment an account is picked — telling either of
// those "Continue" would promise a confirmation step that, for logout, does
// not exist. (`remove` does still ask you to retype the address afterwards,
// in its own RunE; that guard is deliberate and unchanged.)
func accountEnterHint(command string) string {
	switch command {
	case "logout":
		return "Sign out"
	case "remove":
		return "Remove"
	default:
		return "Continue"
	}
}

// chooseAccount finalizes an account-list frame's own selection (decision
// 0039): the run flow (frame.command == "") continues to STEP 3, `logout` is
// finished the moment an account is picked, and `remove` — whose profile
// deletion is irreversible — first pushes the in-app retype-the-account guard
// (frameRemoveConfirm, decision 0042) rather than dropping to the raw terminal
// prompt its RunE uses when invoked directly. Both Enter and → land here, so
// decision 0022's forward-select alias keeps working identically for every
// caller.
func (m *rootPickerApp) chooseAccount(frame *rootFrame, email string) (tea.Model, tea.Cmd) {
	if frame.command == "" {
		m.pushRunMode(email)
		return m, nil
	}
	if frame.command == "remove" {
		m.pushRemoveConfirm(frame.title, email)
		return m, nil
	}
	m.picked = []string{frame.command, email}
	return m, tea.Quit
}

// pushRemoveConfirm transitions into the in-app confirmation for `remove`
// (decision 0042): the same "retype the account" guard the command's own
// RunE enforces when invoked directly, but rendered in the picker's own
// chrome instead of a bare terminal prompt, so the polished account list no
// longer ends in an abrupt `fmt.Fscanln`. email is the real account being
// removed, carried on the frame (rootFrame.account) so Enter can both
// validate the answer and build the final argv.
func (m *rootPickerApp) pushRemoveConfirm(title, email string) {
	m.stack.push(rootFrame{kind: frameRemoveConfirm, title: title, account: email})
	m.emailInput, m.emailErr = "", ""
}

// removeConfirmMatches is the guard's comparison: the typed answer must equal
// the real account exactly, or — while masking is on (decision 0024) — the
// alias actually shown on screen, since asking the user to retype a value the
// UI deliberately hides would both leak the address and be unanswerable.
func removeConfirmMatches(answer, email string) bool {
	return answer == email || answer == displayEmail(email)
}

// updateRemoveConfirm handles the confirmation field: printable characters
// append, Backspace deletes, Esc/← pops back to the account list without
// removing anything, and Enter only finalizes once removeConfirmMatches
// accepts the answer. A mismatch stays on screen with a message rather than
// cancelling, the same "a typo is correctable in place" rule the LOGIN email
// field keeps. The final argv carries --yes because this screen *is* the
// confirmation; the command's own guard is deliberately not run a second
// time, and a direct `cpro remove EMAIL` still prompts (or errors
// non-interactively) exactly as before.
func (m *rootPickerApp) updateRemoveConfirm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	frame := m.stack.current()
	switch msg.String() {
	case "esc", "left":
		m.popFrame()
		m.emailInput, m.emailErr = "", ""
	case "backspace":
		r := []rune(m.emailInput)
		if len(r) > 0 {
			m.emailInput = string(r[:len(r)-1])
		}
		m.emailErr = ""
	case "enter":
		if !removeConfirmMatches(m.emailInput, frame.account) {
			m.emailErr = "does not match; type the account to confirm"
			return m, nil
		}
		m.picked = []string{"remove", frame.account, "--yes"}
		return m, tea.Quit
	default:
		if text := msg.Key().Text; text != "" {
			m.emailInput += text
			m.emailErr = ""
		}
	}
	return m, nil
}

// viewRemoveConfirm renders the confirmation in the same single-color panel
// every other small screen here uses (renderPanel, tui.go), with the account
// (displayEmail, so masking applies) named in the prompt, the typed value
// followed by the same "_" cursor the search/email fields draw, and a
// mismatch message in dangerColor below it.
func (m *rootPickerApp) viewRemoveConfirm() string {
	frame := m.stack.current()
	shown := displayEmail(frame.account)
	lines := []string{
		"  This permanently removes credentials, settings, and history for:",
		"  " + styleText(m.color, shown, accentMode),
		"",
		"  Type " + shown + " to confirm: " + m.emailInput + styleText(m.color, "_", accentMode),
	}
	if m.emailErr != "" {
		lines = append(lines, "", "  "+styleText(m.color, m.emailErr, dangerColor))
	}
	body := renderPanel(m.color, accentMode, screenTitle(frame.title), lines)
	footer := renderFooter(m.color, accentMode,
		[2]string{"↵", "Remove"}, [2]string{"←", "Back"}, [2]string{"Esc", "Back"})
	return body + "\n\n" + footer
}

// updateRunAccountSearch is updateRunAccount's actively-searching
// counterpart — the same shape as accountPickerApp's own updateSearch
// (browseui.go), which this frame's search behavior was modeled on. Esc here
// only ever clears the query, same as every other search screen in cpro —
// it never also pops a frame in the same keystroke, decision 0022's own →
// addition included: → and Enter both select here, but there's no ← alias,
// consistent with search mode not handling ← anywhere else either.
func (m *rootPickerApp) updateRunAccountSearch(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	frame := m.stack.current()
	if frame.list.searchKey(msg, browseKeyOpts[rootPickerEntry]{forward: forwardAll[rootPickerEntry]}) == browseSelect {
		if e, ok := frame.list.searchSelected(); ok {
			return m.chooseAccount(frame, e.name)
		}
	}
	return m, nil
}

// updateRunMode handles STEP 3 (RUN MODE): up/down over the 5
// permissionModes rows (permissions.go — the exact same table Settings' own
// Permissions screen and cpro run itself use), Esc/← pops back to STEP 2,
// Enter finalizes the interactive flow. The resulting argv is exactly what
// typing `cpro run --account EMAIL <flags>` by hand would produce — built
// through permissionModeArgs, the same builder applyPermissionDefaults
// (permissions.go) uses for every other invocation, so this can never drift
// from what direct `cpro run` does with the same mode. See pickCommandArgs's
// own "run" handling for the decision-0007 announce line printed right
// before this is executed. Changing the mode here is a one-run override
// only — nothing here ever writes back config.PermissionMode. → is a
// consistent forward-select alias for Enter here too (decision 0022),
// matching RUN ACCOUNT's own →/Enter convention — same caveat as there: a
// mode row isn't a "forward entry," just a selectable value, so it carries
// no trailing " →" of its own (see viewRunMode).
func (m *rootPickerApp) updateRunMode(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	frame := m.stack.current()
	switch msg.String() {
	case "up":
		frame.cursor = (frame.cursor - 1 + len(permissionModes)) % len(permissionModes)
	case "down":
		frame.cursor = (frame.cursor + 1) % len(permissionModes)
	case "esc", "left":
		m.popFrame()
	case "enter", "right":
		mode := permissionModes[frame.cursor].Key
		if mode == "yolo" {
			// A YOLO choice here becomes an explicit --dangerously-skip-permissions
			// in the argv below — the one case applyPermissionDefaults' own
			// requireYOLOSupport check (claude.go) always skips, since an
			// explicit per-invocation flag deliberately bypasses the "is this
			// just the configured default" check. This is the interactive
			// flow's own equivalent of what the old pickRunMode (ui.go,
			// removed) checked before ever returning a "danger" choice: fail
			// clearly here, before Claude starts, rather than building an
			// argv that would still show prompts.
			if err := requireYOLOSupport(); err != nil {
				m.runFlowErr = err
				return m, tea.Quit
			}
			// Same reasoning, for a higher-precedence enterprise/organization
			// policy this account's own claude reports (decision 0034): fail
			// clearly here too, rather than build a "no prompts" argv cpro
			// already knows that policy could still interrupt. Skipped when
			// m.s is nil (the pure unit tests that construct this type
			// directly with no store), matching pushRunAccount/pushRunMode's
			// own degrade-without-a-store convention.
			if m.s != nil {
				if err := requireNoManagedPermissionsPolicy(m.s.profile(frame.account)); err != nil {
					m.runFlowErr = err
					return m, tea.Quit
				}
			}
			// Same reasoning, for Claude Code's own root/sudo refusal
			// (decision 0035): fail clearly here too, rather than let it
			// surface as Claude's own raw stderr after this screen already
			// quit.
			if err := requireNotRoot(); err != nil {
				m.runFlowErr = err
				return m, tea.Quit
			}
		}
		m.picked = append([]string{"run", "--account", frame.account}, permissionModeArgs(mode)...)
		return m, tea.Quit
	}
	return m, nil
}

// updateWatchMode handles the WATCH MODE screen (decision 0029, extended by
// decision 0045 with its own Interval row): up/down over the two mode rows
// plus the Interval row, Enter/→ on a mode row finalizes the choice into the
// exact argv typing `cpro watch`/`cpro watch --compact [--interval D]` by
// hand would produce (reusing the existing watch implementation entirely —
// this frame only decides which invocation to build), and ←/Esc backs out.
// On the Interval row, ←/→ step the interval instead (clamping at 5s/15m) and
// Enter does nothing, so reaching the interval can never accidentally start a
// watch with the mode the cursor just left. The interval is a one-run choice:
// nothing here writes a preference.
func (m *rootPickerApp) updateWatchMode(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	frame := m.stack.current()
	rows := len(watchModeItems) + 1 // + the Interval row
	intervalRow := len(watchModeItems)
	switch msg.String() {
	case "up":
		frame.cursor = (frame.cursor - 1 + rows) % rows
	case "down":
		frame.cursor = (frame.cursor + 1) % rows
	case "esc":
		m.popFrame()
	case "left":
		if frame.cursor == intervalRow {
			frame.interval = adjustWatchInterval(frame.interval, -1)
			return m, nil
		}
		m.popFrame()
	case "right":
		if frame.cursor == intervalRow {
			frame.interval = adjustWatchInterval(frame.interval, 1)
			return m, nil
		}
		m.picked = watchArgv(frame)
		return m, tea.Quit
	case "enter":
		if frame.cursor == intervalRow {
			return m, nil
		}
		m.picked = watchArgv(frame)
		return m, tea.Quit
	}
	return m, nil
}

// subsequenceMatch reports whether every rune of query appears in s in
// order, though not necessarily contiguously (e.g. "cfg" matches "config") —
// a small, dependency-free stand-in for fuzzy matching (cpro has no fuzzy-
// matching library already in its dependency graph to reuse instead), used
// as rootPickerScore's third ranking tier, between a contiguous substring
// match and a short-metadata/description match. An empty query trivially
// matches everything, consistent with Contains/HasPrefix's own behavior on
// an empty query.
func subsequenceMatch(s, query string) bool {
	r := []rune(query)
	if len(r) == 0 {
		return true
	}
	i := 0
	for _, c := range s {
		if c == r[i] {
			i++
			if i == len(r) {
				return true
			}
		}
	}
	return false
}

// rootPickerScore ranks e against a lowercased, non-empty query across five
// tiers, each strictly outranking the next: an exact command-name match,
// then a name prefix, then a contiguous name substring — none of which are
// length-gated, since a real hit at these tiers is already a strong, precise
// signal — then, only once query is at least fuzzyMatchMinLength long, a name
// fuzzy/subsequence match (subsequenceMatch), then a short-metadata
// substring match, then a detailed-description substring match — so, e.g., a
// query naming "shell" surfaces completion (via its "Shell setup" metadata)
// even though "shell" appears nowhere in the command name itself. ok is
// false when e doesn't match at all, and must be excluded from results.
//
// The three loose tiers (fuzzy name, metadata, description) share one
// minimum length: below it, almost everything shares a letter or two in some
// order somewhere in its name/metadata/description (this list's short
// metadata and descriptions are full of "co", via "Code"/"account"), so a
// one- or two-character query would flood the result list with everything
// and defeat the point of narrowing it. fuzzyMatchMinLength was picked as
// the shortest length where the spec's own worked examples ("co" -> config,
// completion only) hold exactly: at 2, "co" also fuzzy/metadata-matches
// "doctor" (a genuine, if incidental, c...o subsequence) and "use"/"run"
// (via "account"/"Code" in their metadata), pulling in results the example
// doesn't show.
const fuzzyMatchMinLength = 3

func rootPickerScore(e rootPickerEntry, query string) (score int, ok bool) {
	name := strings.ToLower(e.name)
	meta := strings.ToLower(e.shortLabel)
	desc := strings.ToLower(e.description)
	loose := len(query) >= fuzzyMatchMinLength
	switch {
	case name == query:
		return 5, true
	case strings.HasPrefix(name, query):
		return 4, true
	case strings.Contains(name, query):
		return 3, true
	case loose && subsequenceMatch(name, query):
		return 2, true
	case loose && strings.Contains(meta, query):
		return 1, true
	case loose && strings.Contains(desc, query):
		return 0, true
	default:
		return 0, false
	}
}

// rootRowGap/rootRowMarker are the row layout's two fixed-width pieces: the
// gap between the aligned command-name column and the short metadata column
// (see the mockup this screen matches: every row's metadata starts exactly
// 3 columns past the arrow column described below), and the cursor's
// reserved slot ("❯ ", or two spaces when this row isn't selected) so a
// command name never shifts left/right as the selection moves.
const (
	rootRowGap    = 3
	rootRowMarker = 4 // visible width of "  ❯ " / "    "
)

// rootArrowColWidth is the fixed-width slot reserved right after the
// (nameWidth-wide) command-name column for the " →" forward-entry cue —
// " →" itself (a space plus the arrow) on a forward entry, or two blank
// columns on a leaf one. Reserving this as its own fixed column, rather than
// gluing " →" straight onto the name and padding the combined text (the
// pre-alignment shape), is what makes every row's arrow — or its absence —
// land in the same visible column regardless of how long that row's own name
// is: reported live as the arrows not lining up between rows.
const rootArrowColWidth = 2

// rootNameColumn renders e's command-name column: its name padded to
// nameWidth, immediately followed by the fixed-width arrow slot above — " →"
// for a forward entry (isForwardEntry), two blank columns otherwise. name
// itself (and so search/scoring, which key off e.name, not this rendering)
// is never touched.
func rootNameColumn(e rootPickerEntry, nameWidth int) string {
	arrow := "  "
	if isForwardEntry(e.name) {
		arrow = " →"
	}
	return padEnd(e.name, nameWidth) + arrow
}

// metaBudget returns how many visible columns are left for a row's short
// metadata once the rail, the marker slot, the (nameWidth-wide) command-name
// column, the fixed arrow slot, and rootRowGap are accounted for — or -1
// when the real terminal width isn't known yet (see Update's WindowSizeMsg
// case), in which case the caller should show metadata in full rather than
// guess. This is what makes metadata (never the rail, the cursor, or the
// command name) the thing that degrades — truncates, then disappears — on a
// narrow terminal (see rootRow), while the command list itself never wraps.
func (m *rootPickerApp) metaBudget(nameWidth int) int {
	if m.width <= 0 {
		return -1
	}
	const rail = 1 // the styled "│" itself — rootRow's own marker supplies all its line's indentation, so renderRootPanel adds no separating space after it
	return m.width - rail - rootRowMarker - nameWidth - rootArrowColWidth - rootRowGap
}

// rootRow renders one command row: a reserved cursor slot, the command-name
// column (rootNameColumn, name padded to nameWidth plus the fixed-width
// arrow slot) and (space permitting — see metaBudget) its short metadata,
// dimmed (dimStyle) so it reads as secondary to the command name — except
// the "default" row (e.permMode/e.defaultAcct set, see applyLiveMeta): its
// metadata is "● Label  ∙  account", the "●" colored to the live mode's own
// risk color (mode.Color) — matching every other place cpro shows a
// permission-mode dot (the SET DEFAULT ACCOUNT screen itself), and the same
// "  ∙  " separator the account-usage cells use (runAccountSessionCell) —
// rather than a plain dimmed string like every other row's metadata. Reused
// identically by both the normal and search views so a row looks the same
// regardless of which list it's currently part of — including the "→"
// suffix, which must survive filtering exactly like the rest of the row.
func (m *rootPickerApp) rootRow(e rootPickerEntry, selected bool, nameWidth int) string {
	marker := "    "
	if selected {
		marker = "  " + styleText(m.color, "❯", accentMode) + " "
	}
	row := marker + rootNameColumn(e, nameWidth)
	meta := e.shortLabel
	dotColor := ""
	if e.name == "default" && e.permMode != "" {
		mode := permissionModeByKey(e.permMode)
		meta, dotColor = "● "+mode.Label, mode.Color
		if e.defaultAcct != "" {
			meta += usageCellSeparator + e.defaultAcct
		}
	}
	// Truncate the PLAIN text first, then split off the "●" prefix to color
	// it — truncateToWidth (tui.go) is explicitly not ANSI-aware, so coloring
	// before truncating would corrupt the width math.
	if budget := m.metaBudget(nameWidth); budget >= 0 {
		if budget < 2 {
			meta = ""
		} else {
			meta = truncateToWidth(meta, budget)
		}
	}
	if meta == "" {
		return row
	}
	row += strings.Repeat(" ", rootRowGap)
	if dotColor != "" && strings.HasPrefix(meta, "●") {
		row += styleText(m.color, "●", dotColor) + dimStyle(m.color, strings.TrimPrefix(meta, "●"))
	} else {
		row += dimStyle(m.color, meta)
	}
	return row
}

// renderRootPanel draws the picker's own frame in the configured Theme's own
// runes (currentTheme, theme.go — never hardcoded here): like tui.go's
// renderPanel, but with a per-line rail color (lineHex) instead of one color
// for the whole frame — what the five-shade semantic-group rail needs in
// normal mode, and what search mode uses too (every lineHex the same accent
// color there, since search results are one filtered set, not five groups).
// header, when non-empty, follows the top border (this screen's own
// "claude cpro[ - NAME]" title — see screenTitle, tui.go); the search
// field itself is a body line (see searchLine), not part of the border, so
// switching into search never changes where the title sits. Unlike tui.go's
// renderPanel, a body line is appended directly after the rail with no
// injected separating space — rootRow already builds each line's own
// complete leading indent (marker included), so an extra space here would
// push the cursor a column further right than the design calls for; a
// genuinely empty line (a blank group-separator row) is left as a bare rail
// with nothing after it.
func renderRootPanel(color bool, edgeTop, edgeBottom, header string, lineHex, lines []string) string {
	top := currentTheme.Top()
	if header != "" {
		top += " " + header
	}
	var b strings.Builder
	b.WriteString(styleText(color, top, edgeTop))
	for i, line := range lines {
		b.WriteByte('\n')
		b.WriteString(styleText(color, currentTheme.Rail(), lineHex[i]) + line)
	}
	b.WriteByte('\n')
	b.WriteString(styleText(color, currentTheme.Bottom(), edgeBottom))
	return b.String()
}

// rootChromeLines is the number of rendered lines outside the panel's own
// list body: the panel's top border, its bottom border, a blank line, the
// description, another blank line, and the footer. The actual scrolling is
// the shared scrollLines/scrollRange (browseui.go) — same rule (keep the
// cursor row on screen, centered when there's room, clamped at either end),
// now shared with every other list screen rather than living only here.
const rootChromeLines = 6

// searchLine is the first body row of a searchable frame: a bare "  Filter:"
// while browsing (list.query == ""), or "  Filter: " followed by the typed
// query and a trailing "_" cursor once the user starts typing — its own row,
// kept separate from the panel's title border. Its own small 2-space indent
// (unlike every entry row's own 4-column marker) sets it apart from both the
// rail and the command rows below it.
func (m *rootPickerApp) searchLine() string {
	query := m.stack.current().list.query
	if query == "" {
		return "  Filter:"
	}
	return "  Filter: " + query + styleText(m.color, "_", accentMode)
}

// railShade returns the rail color for group: the group's own gradient
// shade normally, or dangerColor — solid, the same configured danger color
// as every other double-Esc warning in cpro (escGuardField, ui.go) — for
// every row and both edges alike while a first Esc has armed exit, replacing
// the gradient rather than tinting it. Do not preserve the gradient during
// this state: a partially-colored rail would read as a sixth shade, not a
// warning. This is on top of, not instead of, the footer's own "Esc¹ again"
// (also dangerColor, viewNormal) — both signal the same armed state together.
func (m *rootPickerApp) railShade(group string) string {
	if m.exitArmed {
		return dangerColor
	}
	return m.shades[rootGroupShadeIndex(group)]
}

func (m *rootPickerApp) viewNormal() string {
	frame := m.stack.current()
	nameWidth := 0
	for _, e := range frame.list.items {
		nameWidth = max(nameWidth, visibleWidth(e.name))
	}
	searchHex := m.shades[0]
	if m.exitArmed {
		searchHex = dangerColor
	}
	lines := []string{m.searchLine()}
	lineHex := []string{searchHex}
	cursorLine := 0
	for i, e := range frame.list.items {
		if i > 0 && e.group != frame.list.items[i-1].group {
			// A blank rail row between groups — no heading text, no "├─",
			// just the upcoming group's own shade — restores a subtle
			// sense of grouping without spending a whole labeled row on it.
			lines = append(lines, "")
			lineHex = append(lineHex, m.railShade(e.group))
		}
		if i == frame.list.cursor {
			cursorLine = len(lines)
		}
		lines = append(lines, m.rootRow(e, i == frame.list.cursor, nameWidth))
		lineHex = append(lineHex, m.railShade(e.group))
	}
	lines, lineHex = scrollLines(m.height, rootChromeLines, lines, lineHex, cursorLine)
	edgeTop, edgeBottom := m.shades[0], m.shades[len(m.shades)-1]
	if m.exitArmed {
		edgeTop, edgeBottom = dangerColor, dangerColor
	}
	panel := renderRootPanel(m.color, edgeTop, edgeBottom, screenTitle(frame.title), lineHex, lines)
	desc := ""
	if len(frame.list.items) > 0 {
		desc = m.describe(frame.list.items[frame.list.cursor].description)
	}
	if line := m.flowErrorLine(); line != "" {
		desc = line
	}
	hints := [][2]string{{"↑↓", "Navigate"}}
	if !m.stack.atRoot() {
		hints = append(hints, [2]string{"←", "Back"})
	}
	hints = append(hints, [2]string{"→", "Open"}, [2]string{"↵", "Run"}, [2]string{"", "Type to search"})
	switch {
	case !m.stack.atRoot():
		// A nested frame never arms exit at all — Esc there always just
		// pops one level (updateNormal) — so its own footer stays the plain
		// "Esc Back" it always was.
		hints = append(hints, [2]string{"Esc", "Back"})
	case m.exitArmed:
		// The whole "Esc¹ again" segment (key and label alike) in Danger
		// color, not just the usual key-only styling every other hint here
		// gets — built as one pre-styled unit and passed through
		// renderFooter's own empty-key escape hatch (the same one "Type to
		// search" above already uses) rather than widening renderFooter's
		// signature for every other caller. This is in addition to, not
		// instead of, railShade's own rail/edge recolor above — both signal
		// the same armed state together.
		hints = append(hints, [2]string{"", styleText(m.color, "Esc¹ again", dangerColor)})
	default:
		hints = append(hints, [2]string{"Esc²", "to exit"})
	}
	footer := renderFooter(m.color, accentMode, hints...)
	return panel + "\n\n" + desc + "\n\n" + footer
}

func (m *rootPickerApp) viewSearch() string {
	frame := m.stack.current()
	title := screenTitle(frame.title)

	if len(frame.list.filtered) == 0 {
		panel := renderRootPanel(m.color, accentMode, accentMode, title, []string{accentMode, accentMode}, []string{m.searchLine(), "  No commands found"})
		footer := renderFooter(m.color, accentMode, [2]string{"", "Backspace to edit"}, [2]string{"Esc", "Clear"})
		return panel + "\n\n" + footer
	}

	nameWidth := 0
	for _, idx := range frame.list.filtered {
		nameWidth = max(nameWidth, visibleWidth(frame.list.items[idx].name))
	}
	lines := make([]string, 0, len(frame.list.filtered)+1)
	lineHex := make([]string, 0, len(frame.list.filtered)+1)
	lines = append(lines, m.searchLine())
	lineHex = append(lineHex, accentMode)
	for i, idx := range frame.list.filtered {
		lines = append(lines, m.rootRow(frame.list.items[idx], i == frame.list.fcursor, nameWidth))
		lineHex = append(lineHex, accentMode) // search results are one filtered set, not five groups; no blank separators either
	}
	lines, lineHex = scrollLines(m.height, rootChromeLines, lines, lineHex, frame.list.fcursor+1)
	panel := renderRootPanel(m.color, accentMode, accentMode, title, lineHex, lines)
	desc := m.describe(frame.list.items[frame.list.filtered[frame.list.fcursor]].description)
	if line := m.flowErrorLine(); line != "" {
		desc = line
	}
	footer := renderFooter(m.color, accentMode, [2]string{"↑↓", "Navigate"}, [2]string{"↵", "Run"}, [2]string{"Esc", "Clear"})
	return panel + "\n\n" + desc + "\n\n" + footer
}

// accountPickerNameWidth is the column every account-picker row's Session
// cell aligns against: the widest displayed account, so identities and
// percentages line up regardless of email length (decision 0031). Measured
// via visibleWidth (ANSI-aware) against displayEmail's output, never the raw
// address — while masking is on the alias is what's actually on screen, and
// it's a different length. A free function over plain emails, so the root
// picker's entry-shaped account lists (entryEmails, browseui.go) and the
// plain []string ones share the exact same measurement.
func accountPickerNameWidth(emails []string) int {
	width := 0
	for _, email := range emails {
		width = max(width, visibleWidth(displayEmail(email)))
	}
	return width
}

// runAccountUsageCell renders one usage window's indicator, e.g. "S █ 79%" or
// "W █ 65%" — the "█" colored by usageColorFor (ui.go), the same shared
// threshold logic cpro status/watch use, so a configured Warning/Danger
// threshold or color applies here identically and none of it is duplicated
// or hardcoded. Before a fetch lands (or after one failed with no cached
// value to fall back on) the cell is dim "<letter> ·  --" instead: a
// placeholder, never a misleading 0%. The percentage is right-aligned in 4
// columns ("  0%" through "100%") so the column stays straight across rows.
func runAccountUsageCell(color bool, letter string, loaded, failed bool, value float64) string {
	if !loaded || failed {
		return dimStyle(color, letter+" ·   --")
	}
	return letter + " " + styleText(color, "█", usageColorFor(value)) + " " + padStart(pctText(value), 4)
}

// runAccountSessionCell renders one account's Session (five-hour window)
// indicator — see runAccountUsageCell.
func runAccountSessionCell(color bool, u runAccountUsage) string {
	return runAccountUsageCell(color, "S", u.loaded, u.failed, u.session)
}

// runAccountWeekCell renders one account's Week (seven-day window) indicator
// — see runAccountUsageCell. Added alongside the Session cell so every
// account picker shows both windows a running session can actually run out
// of, not just the shorter one.
func runAccountWeekCell(color bool, u runAccountUsage) string {
	return runAccountUsageCell(color, "W", u.loaded, u.failed, u.week)
}

// usageCellSeparator joins the Session and Week cells, matching renderFooter's
// own "  ∙  " hint separator (tui.go) rather than inventing a second visual
// convention for the same job.
const usageCellSeparator = "  ∙  "

// accountPickerRow assembles one row shared by every account picker that
// shows live Session/Week usage — RUN ACCOUNT (this file), DEFAULT ACCOUNT
// (configui.go), RESUME ACCOUNT (resumeui.go), SELECT ACCOUNT (system.go) and
// DESTINATION ACCOUNT (sessionui.go) — so account identity, the usage cells,
// threshold coloring (via runAccountSessionCell/runAccountWeekCell/
// usageColorFor), the loading placeholder, and narrow-terminal degradation
// exist in exactly one place rather than five independently maintained
// copies. Takes color/width directly instead of a *rootPickerApp receiver
// precisely so the other tea.Models — with their own color/width fields and
// no rootPickerEntry concept, just plain email strings — can call it too.
//
// Degrades on a narrow terminal in the spec's own priority order, one field
// at a time rather than all-or-nothing: both percentages outlast the "S"/"W"
// labels and the colored indicators: the Session percentage — the window a
// running session actually blocks on next — outlasts the Week one; and
// account identity outlasts every percentage. A row never wraps onto a
// second line. width is the terminal width (0 = unknown/unconstrained, the
// same convention outputWidth/describe already use), so the full row renders
// whenever the real width isn't known to be too small for it. accountListLines
// (browseui.go) is what every caller actually uses: it assembles a whole
// scrolled list of these rows.
func accountPickerRow(color bool, width int, email string, u runAccountUsage, selected bool, nameWidth int) string {
	cursor := "  "
	if selected {
		cursor = styleText(color, "❯", accentMode) + " "
	}
	name := displayEmail(email)
	full := runAccountSessionCell(color, u) + usageCellSeparator + runAccountWeekCell(color, u)

	// 2 cursor + name column + 2 gap + cell. renderPanel adds its own
	// "│ " rail (2 columns) around every line it's given.
	if width <= 0 || 2+nameWidth+2+visibleWidth(full)+2 <= width {
		return cursor + padEnd(name, nameWidth) + "  " + full
	}

	// No room for the full cells: drop the "S "/"W " labels and the
	// indicators, keeping both percentages — the identity and the numbers
	// are what the spec says must survive longest.
	placeholder := !u.loaded || u.failed
	sessionShort, weekShort := "  --", "  --"
	if !placeholder {
		sessionShort, weekShort = padStart(pctText(u.session), 4), padStart(pctText(u.week), 4)
	}
	fits := func(s string) bool { return 2+nameWidth+1+visibleWidth(s)+2 <= width }

	if both := sessionShort + " ∙ " + weekShort; fits(both) {
		if placeholder {
			both = dimStyle(color, both)
		}
		return cursor + padEnd(name, nameWidth) + " " + both
	}

	// Still no room for both: drop Week entirely and keep just Session,
	// the window a running session actually blocks on next.
	if placeholder {
		sessionShort = dimStyle(color, sessionShort)
	}
	if fits(sessionShort) {
		return cursor + padEnd(name, nameWidth) + " " + sessionShort
	}
	return cursor + name // narrower still: identity only, never wrapped
}

// viewRunAccount renders STEP 2 while browsing: a small, single-color panel
// (renderPanel, tui.go — same as viewSubmenu, not the main list's five-shade
// rail, since a plain account list has no grouping to show). Each row carries
// its own live Session usage (decision 0031) through the shared
// accountListLines (browseui.go) — the same rows, the same cache, and the
// same scroll window every other account picker uses.
func (m *rootPickerApp) viewRunAccount() string {
	frame := m.stack.current()
	lines := accountListLines(m.color, m.width, m.height, entryEmails(frame.list.items), frame.list.cursor, m.accountUsage)
	body := renderPanel(m.color, accentMode, screenTitle(frame.title), lines)
	footer := renderFooter(m.color, accentMode,
		[2]string{"↑↓", "Navigate"}, [2]string{"→", "Select"}, [2]string{"↵", accountEnterHint(frame.command)}, [2]string{"", "Type to search"}, [2]string{"←", "Back"}, [2]string{"Esc", "Back"})
	return body + "\n\n" + footer
}

// viewRunAccountSearch is viewRunAccount's actively-searching counterpart.
// No "← Back" hint here: Esc/← don't pop a frame while actively searching
// anywhere else in cpro either — Esc only ever clears the query first
// (updateRunAccountSearch). Width and the scroll window are measured over the
// filtered rows only, so a narrowed result set aligns tightly rather than
// reserving space for rows that aren't shown.
func (m *rootPickerApp) viewRunAccountSearch() string {
	frame := m.stack.current()
	header := screenTitle(frame.title) + ": " + frame.list.query + styleText(m.color, "_", accentMode)
	if len(frame.list.filtered) == 0 {
		body := renderPanel(m.color, accentMode, header, []string{"No accounts found"})
		footer := renderFooter(m.color, accentMode, [2]string{"", "Backspace to edit"}, [2]string{"Esc", "Clear"})
		return body + "\n\n" + footer
	}
	entries := make([]rootPickerEntry, len(frame.list.filtered))
	for i, idx := range frame.list.filtered {
		entries[i] = frame.list.items[idx]
	}
	lines := accountListLines(m.color, m.width, m.height, entryEmails(entries), frame.list.fcursor, m.accountUsage)
	body := renderPanel(m.color, accentMode, header, lines)
	footer := renderFooter(m.color, accentMode, [2]string{"↑↓", "Navigate"}, [2]string{"→", "Select"}, [2]string{"↵", accountEnterHint(frame.command)}, [2]string{"Esc", "Clear"})
	return body + "\n\n" + footer
}

// viewRunMode renders STEP 3: the same 5 permission-mode rows viewPermissions
// (configui.go) draws — ❯ for the highlighted candidate, ●/○ colored per
// mode's own risk color (permissions.go) — with none of that screen's
// Workspace/Block-always/Command-preview sections, since this is a one-shot
// per-run choice, not a persisted setting: ● and ❯ are the same row here,
// there's no separate "saved" state to show independently.
func (m *rootPickerApp) viewRunMode() string {
	frame := m.stack.current()
	lines := make([]string, len(permissionModes))
	for i, mode := range permissionModes {
		cursor := "  "
		dot := "○"
		if i == frame.cursor {
			cursor = styleText(m.color, "❯", accentMode) + " "
			dot = "●"
		}
		lines[i] = cursor + styleText(m.color, dot, mode.Color) + " " + mode.Label
	}
	body := renderPanel(m.color, accentMode, screenTitle(frame.title), lines)
	footer := renderFooter(m.color, accentMode, [2]string{"↑↓", "Navigate"}, [2]string{"→", "Run"}, [2]string{"↵", "Run"}, [2]string{"←", "Back"}, [2]string{"Esc", "Back"})
	return body + "\n\n" + footer
}

// viewWatchMode renders the WATCH MODE screen: the two fixed mode rows with
// inline descriptions (the same small-fixed-list style rootPickerApp's own
// System submenu uses — no type-to-search, matching the task's own mockup),
// then a blank rail row and the Interval row (decision 0045). The interval
// row shows its value between ◀/▶ to advertise that ←/→ change it, and the
// footer swaps its Start hints for an Adjust hint whenever that row is
// selected, since Enter there deliberately does not start a watch.
func (m *rootPickerApp) viewWatchMode() string {
	frame := m.stack.current()
	labelWidth := visibleWidth("Interval")
	for _, item := range watchModeItems {
		labelWidth = max(labelWidth, visibleWidth(item.label))
	}
	lines := make([]string, len(watchModeItems))
	for i, item := range watchModeItems {
		cursor := "  "
		if i == frame.cursor {
			cursor = styleText(m.color, "❯", accentMode) + " "
		}
		lines[i] = cursor + padEnd(item.label, labelWidth+7) + item.desc
	}
	lines = append(lines, "")
	intervalCursor := "  "
	if frame.cursor == len(watchModeItems) {
		intervalCursor = styleText(m.color, "❯", accentMode) + " "
	}
	lines = append(lines, intervalCursor+padEnd("Interval", labelWidth+7)+
		styleText(m.color, "◀", accentMode)+" "+watchIntervalLabel(frame.interval)+" "+styleText(m.color, "▶", accentMode))
	body := renderPanel(m.color, accentMode, screenTitle(frame.title), lines)
	var footer string
	if frame.cursor == len(watchModeItems) {
		footer = renderFooter(m.color, accentMode, [2]string{"↑↓", "Navigate"}, [2]string{"←→", "Adjust"}, [2]string{"Esc", "Back"})
	} else {
		footer = renderFooter(m.color, accentMode, [2]string{"↑↓", "Navigate"}, [2]string{"→", "Start"}, [2]string{"↵", "Start"}, [2]string{"←", "Back"}, [2]string{"Esc", "Back"})
	}
	return body + "\n\n" + footer
}

// flowErrorLine renders a frame-open failure (m.runFlowErr — e.g. no
// registered accounts for run/logout/remove) as one below-panel line in
// dangerColor, so the picker shows the reason in its own chrome and stays
// open instead of closing and having the caller print it afterwards
// (decision 0043). It reuses describe's truncation, so it never wraps; empty
// when there is no error. Every list view that has a description line
// substitutes this for it, keeping the rendered line count (and thus
// rootChromeLines' scroll budget) unchanged.
func (m *rootPickerApp) flowErrorLine() string {
	if m.runFlowErr == nil {
		return ""
	}
	return styleText(m.color, m.describe(m.runFlowErr.Error()), dangerColor)
}

// describe truncates a below-panel description to the known terminal width,
// rather than letting it wrap — the panel above stays a fixed, narrow shape
// regardless of terminal size, and only this single plain-text line needs to
// adapt. Width 0 (no WindowSizeMsg yet) leaves it untouched.
func (m *rootPickerApp) describe(desc string) string {
	if m.width <= 0 {
		return desc
	}
	return truncateToWidth(desc, m.width)
}
