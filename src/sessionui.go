//go:build linux

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/spf13/cobra"
)

// sessionScreen identifies which panel sessionApp is currently showing.
type sessionScreen int

const (
	screenSessionMenu     sessionScreen = iota
	screenSessionList                   // "list" — a read-only, searchable browse of every recorded session
	screenContinuePicker                // "continue" — the same browse/search, but Enter/→ proceeds to a destination account
	screenContinueAccount               // the destination-account picker that follows a chosen session
	screenDeleteConfirm                 // "delete" — the retype-the-session-ID guard before deleting (decision 0051)
)

// sessionMenuItem is one selectable row on the SESSIONS screen.
type sessionMenuItem struct{ key, label, desc string }

// sessionMenuItems are the entry points the SESSIONS screen exposes (decisions
// 0025, 0051) — deliberately not every session subcommand: "continue" leads to
// the interactive migration picker, "list" is a read-only view of the same
// underlying data for a quick glance without picking anything, and "delete"
// reuses that same browse to pick a session for deletion (decision 0044's
// parity rule: the row's own operation is `cpro session delete`, which this
// screen only builds the argv for).
var sessionMenuItems = []sessionMenuItem{
	{"continue", "continue", "Continue a session"},
	{"list", "list", "View sessions"},
	{"delete", "delete", "Delete a session"},
}

// sessionScreenDescription is SESSIONS' own static subtitle, shown below the
// panel regardless of cursor position (there are only two, already-labeled
// rows here — nothing to disambiguate further per row the way rootPickerApp's
// own highlighted-row description does).
const sessionScreenDescription = "Manage Claude sessions"

func isForwardSessionItem(key string) bool { return key == "continue" || key == "delete" }

// sessionItemLabel appends the same trailing " →" forward-navigation cue
// configItemLabel/rootNameColumn give every other forward row in cpro.
func sessionItemLabel(it sessionMenuItem) string {
	if isForwardSessionItem(it.key) {
		return it.label + " →"
	}
	return it.label
}

// sessionListState is the browse/search state shared by screenSessionList
// and screenContinuePicker — both list and filter the same sessionEntry
// slice, differing only in what Enter/→ does with the highlighted row (see
// sessionApp.pickerMode). The state machine itself is the shared
// browseList[sessionEntry] (browseui.go), configured with the session
// haystack (project name, path, session ID, owning account via
// displayEmail).
type sessionListState struct {
	browseList[sessionEntry]
}

// sessionAccountState is the destination-account picker's own state
// (screenContinueAccount) — the shared browseList[string] (browseui.go), the
// same account state machine configApp's defaultAccountState and rootui.go's
// RUN ACCOUNT frame use, adapted to live inside sessionApp's single
// tea.Program.
type sessionAccountState struct {
	browseList[string]
}

// sessionApp is cpro session's interactive screen (decision 0025): a small
// custom bubbletea.Model, the same shape configApp already established for
// cpro config/cpro permissions — SESSIONS (screenSessionMenu) is always this
// stack's own root when opened via cpro session or the root/menu picker's
// own "session →" row; a direct cpro session continue with no arguments
// instead seeds screenContinuePicker itself as the root (runSessionContinueUI),
// mirroring runPermissionsUI's own "skip straight to the one screen this
// invocation is about" shape. screenSessionList and screenContinueAccount
// are always nested frames, pushed from whichever screen is currently root.
//
// hasParent/backOut/exitArmed/exitArmedGen follow configApp's own convention
// exactly: hasParent means this was opened from the root/menu picker (still
// a screen to go back to in that other process — see rootui.go's
// pickCommandArgs), so Esc/← at the stack's own root frame sets backOut and
// quits; no parent means this screen IS the root of its own interactive
// session, so Esc there arms cpro's usual double-Esc-to-exit instead
// (reusing rootPickerApp's exitArmTimeout/exitArmExpiredMsg directly, same as
// configApp does). A nested frame's Esc/← never touches any of this — it
// just pops one level, checked via m.stack.atRoot().
type sessionApp struct {
	cmd *cobra.Command
	s   *store
	c   config
	err error

	color bool
	width int
	// height is the last known terminal height (tea.WindowSizeMsg); used only
	// to scroll a list screen's rows (browseui.go's scrollRange).
	height int

	hasParent bool
	backOut   bool

	exitArmed    bool
	exitArmedGen int

	stack  navStack[sessionScreen]
	cursor int // SESSIONS menu cursor, 0..len(sessionMenuItems)-1

	picker     sessionListState
	pickerMode string // "continue" or "list" — which SESSIONS row opened m.picker

	continuing *sessionEntry // the session chosen from screenContinuePicker, awaiting a destination account
	account    sessionAccountState

	// accountUsage is DESTINATION ACCOUNT's own Session-usage cache, the exact
	// counterpart of rootPickerApp.accountUsage/configApp.accountUsage
	// (decisions 0031/0036): keyed by the real email, filled by the same
	// fetchAccountPickerUsage and read by the same accountListLines, so this
	// screen shows live Session usage too rather than being the one account
	// picker that doesn't.
	accountUsage map[string]runAccountUsage

	// pendingDelete/confirmInput/confirmErr are the DELETE SESSION frame's own
	// state (decisions 0051/0052) — the sessions chosen for deletion, the typed
	// confirmation, and the mismatch message, mirroring rootPickerApp's own
	// remove-confirm fields (decision 0042).
	pendingDelete []sessionEntry
	confirmInput  string
	confirmErr    string

	// deleteSel is DELETE SESSION's own multi-select set (decision 0052), keyed
	// by sessionEntryKey so a row stays checked across a search that filters it
	// out and back in. Reset every time the delete picker opens.
	deleteSel map[string]bool

	// hiddenActive is how many sessions the delete picker left out of its list
	// because a live Claude process is using them right now (deletableSessions,
	// session.go). The picker lists only what it can actually delete — that is
	// the whole point of this screen, never offering a choice that would then
	// fail — and this count is what lets it say so, rather than looking like a
	// session list that quietly lost rows.
	hiddenActive int

	// active marks the rows a live Claude process is using right now
	// (activeSessionKeys, re-derived from /proc on every visit), so continuing
	// one reads as a deliberate choice: two Claude processes writing the same
	// conversation fork it. alsoIn is CONTINUE SESSION's record of the other
	// accounts holding a copy of a row's session — that picker shows one row
	// per session ID (the newest copy, the one resumeSession continues from).
	active map[string]bool
	alsoIn map[string][]string

	// pickerLoad is the title-loading command openPicker queues
	// (loadSessionTitles); Init or the key handler that opened the picker hands
	// it to bubbletea, so a slow read never delays the first frame.
	pickerLoad tea.Cmd

	// accountAuth is DESTINATION ACCOUNT's per-account auth result, filled
	// asynchronously like accountUsage (fetchSessionAuth): true for a valid
	// claude.ai session, false for signed out/invalid, absent while unknown.
	accountAuth map[string]bool

	picked []string // the final argv, once a choice is finalized
}

// sessionTitlesMsg delivers loadSessionTitles' results, keyed by
// sessionEntryKey.
type sessionTitlesMsg map[string]string

// sessionAuthMsg delivers one DESTINATION ACCOUNT row's auth check.
type sessionAuthMsg struct {
	email string
	ok    bool
}

// loadSessionTitles reads every listed session's title (sessionTitle,
// session.go) off the UI goroutine and delivers them in one message.
func loadSessionTitles(s *store, entries []sessionEntry) tea.Cmd {
	if s == nil || len(entries) == 0 {
		return nil
	}
	paths := make(map[string]string, len(entries))
	for _, e := range entries {
		paths[sessionEntryKey(e)] = filepath.Join(s.profile(e.email), "projects", e.dirName, e.sessionID+".jsonl")
	}
	return func() tea.Msg {
		titles := make(sessionTitlesMsg, len(paths))
		for key, path := range paths {
			if t := sessionTitle(path); t != "" {
				titles[key] = t
			}
		}
		return titles
	}
}

// fetchSessionAuth checks each account's session the way every other command
// does (authStatus + validAuth), one command per account so a slow check never
// holds up another row.
func fetchSessionAuth(s *store, emails []string) tea.Cmd {
	if s == nil || len(emails) == 0 {
		return nil
	}
	cmds := make([]tea.Cmd, len(emails))
	for i, email := range emails {
		cmds[i] = func() tea.Msg {
			auth, err := authStatus(s.profile(email))
			return sessionAuthMsg{email: email, ok: err == nil && validAuth(email, auth)}
		}
	}
	return tea.Batch(cmds...)
}

// takePickerLoad hands over (once) the command openPicker queued.
func (m *sessionApp) takePickerLoad() tea.Cmd {
	cmd := m.pickerLoad
	m.pickerLoad = nil
	return cmd
}

// runSessionUI runs the SESSIONS screen to completion — cpro session's own
// direct invocation, and the root/menu picker's "session →" round trip
// (rootui.go's pickCommandArgs, mirroring its existing "config"/
// "permissions" round trips). hasParent has the same meaning runConfigUI's
// own parameter does.
func runSessionUI(cmd *cobra.Command, s *store, c config, hasParent bool) (picked []string, backOut bool, err error) {
	m := &sessionApp{
		cmd:       cmd,
		s:         s,
		c:         c,
		color:     tuiColorEnabled(cmd.ErrOrStderr()),
		hasParent: hasParent,
		stack:     newNavStack(screenSessionMenu),
	}
	return runSessionAppProgram(cmd, m)
}

// runSessionContinueUI runs the CONTINUE SESSION picker directly as the
// stack's own root, skipping the SESSIONS menu — for cpro session continue
// invoked with no arguments (decision 0025), the same "this one screen IS
// the whole program" shape runPermissionsUI already uses for cpro
// permissions vs. Permissions nested inside CONFIG.
func runSessionContinueUI(cmd *cobra.Command, s *store, c config, hasParent bool) (picked []string, backOut bool, err error) {
	m := &sessionApp{
		cmd:       cmd,
		s:         s,
		c:         c,
		color:     tuiColorEnabled(cmd.ErrOrStderr()),
		hasParent: hasParent,
		stack:     newNavStack(screenContinuePicker),
	}
	m.openPicker("continue")
	return runSessionAppProgram(cmd, m)
}

// runSessionAppProgram is the shared tail runSessionUI and
// runSessionContinueUI both need, since m's own stack root is the only thing
// that differs between them — the same split runConfigUI/runPermissionsUI
// use around runConfigAppProgram.
func runSessionAppProgram(cmd *cobra.Command, m *sessionApp) (picked []string, backOut bool, err error) {
	p := tea.NewProgram(m, tea.WithContext(cmd.Context()), tea.WithInput(os.Stdin), tea.WithOutput(os.Stderr))
	if _, err := p.Run(); err != nil {
		return nil, false, err
	}
	return m.picked, m.backOut, m.err
}

func (m *sessionApp) Init() tea.Cmd { return m.takePickerLoad() }

func (m *sessionApp) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case runAccountUsageMsg:
		if m.accountUsage == nil {
			m.accountUsage = map[string]runAccountUsage{}
		}
		m.accountUsage[msg.email] = msg.usage
		return m, nil
	case sessionAuthMsg:
		if m.accountAuth == nil {
			m.accountAuth = map[string]bool{}
		}
		m.accountAuth[msg.email] = msg.ok
		return m, nil
	case sessionTitlesMsg:
		for i := range m.picker.items {
			if t, ok := msg[sessionEntryKey(m.picker.items[i])]; ok {
				m.picker.items[i].title = t
			}
		}
		if m.picker.searching() {
			m.picker.refilter() // the title is part of what a query matches
		}
		return m, nil
	case exitArmExpiredMsg:
		if msg.gen == m.exitArmedGen {
			m.exitArmed = false
		}
		return m, nil
	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			m.picked = nil
			return m, tea.Quit
		}
		switch *m.stack.current() {
		case screenSessionList, screenContinuePicker:
			if m.picker.searching() {
				return m.updatePickerSearch(msg)
			}
			return m.updatePickerBrowse(msg)
		case screenContinueAccount:
			if m.account.searching() {
				return m.updateAccountSearch(msg)
			}
			return m.updateAccountBrowse(msg)
		case screenDeleteConfirm:
			return m.updateDeleteConfirm(msg)
		default:
			return m.updateSessionMenu(msg)
		}
	}
	return m, nil
}

func (m *sessionApp) View() tea.View {
	var content string
	switch *m.stack.current() {
	case screenSessionList, screenContinuePicker:
		if m.picker.searching() {
			content = m.viewPickerSearch()
		} else {
			content = m.viewPickerBrowse()
		}
	case screenContinueAccount:
		if m.account.searching() {
			content = m.viewAccountSearch()
		} else {
			content = m.viewAccountBrowse()
		}
	case screenDeleteConfirm:
		content = m.viewDeleteConfirm()
	default:
		content = m.viewSessionMenu()
	}
	v := tea.NewView(content)
	// Same alt-screen rationale as configApp/rootPickerApp: a session list can
	// run taller than a short terminal, and without a dedicated buffer
	// bubbletea's relative-repositioning renderer corrupts the redraw once
	// that's forced the terminal to scroll.
	v.AltScreen = true
	return v
}

// frameColor is the border color every sessionApp screen renders with:
// dangerColor while the stack's own root frame is armed to exit, accentMode
// otherwise. Unlike configApp (where only screenMenu/screenPermissions can
// ever be the root), several different screens here can be — SESSIONS
// itself, or CONTINUE SESSION when opened via cpro session continue with no
// arguments — so this checks m.stack.atRoot() rather than assuming which
// screen it is.
func (m *sessionApp) frameColor() string {
	if m.stack.atRoot() && m.exitArmed {
		return dangerColor
	}
	return accentMode
}

// exitRootScreen is sessionApp's own copy of configApp's identically-named,
// identically-behaved method — see its doc comment there for the full
// rationale (decision 0023, reused verbatim for session management here).
func (m *sessionApp) exitRootScreen() (tea.Model, tea.Cmd) {
	if m.hasParent {
		m.backOut = true
		return m, tea.Quit
	}
	if m.exitArmed {
		return m, tea.Quit
	}
	return m, m.arm()
}

// arm is sessionApp's own copy of rootPickerApp's/configApp's identically-named
// method, reusing exitArmTimeout/exitArmExpiredMsg (rootui.go) directly.
func (m *sessionApp) arm() tea.Cmd {
	m.exitArmed = true
	m.exitArmedGen++
	gen := m.exitArmedGen
	return tea.Tick(exitArmTimeout, func(time.Time) tea.Msg { return exitArmExpiredMsg{gen: gen} })
}

// backAndExitHints is the trailing footer segment(s) for whichever screen is
// currently showing: a nested frame (or a root frame with hasParent) always
// backs out with a single, unarmed "← Back"/"Esc Back" pair; only a screen
// that IS the stack's own root with no parent at all arms cpro's usual
// double-Esc-to-exit convention instead.
func (m *sessionApp) backAndExitHints() [][2]string {
	if !m.stack.atRoot() || m.hasParent {
		return [][2]string{{"←", "Back"}, {"Esc", "Back"}}
	}
	if m.exitArmed {
		return [][2]string{{"", styleText(m.color, "Esc¹ again", dangerColor)}}
	}
	return [][2]string{{"Esc²", "to exit"}}
}

func (m *sessionApp) updateSessionMenu(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() != "esc" {
		m.exitArmed = false
	}
	n := len(sessionMenuItems)
	switch msg.String() {
	case "up":
		m.cursor = (m.cursor - 1 + n) % n
	case "down":
		m.cursor = (m.cursor + 1) % n
	case "esc":
		return m.exitRootScreen()
	case "left":
		if m.hasParent {
			return m.exitRootScreen()
		}
	case "enter":
		return m, m.activateSessionMenuItem(sessionMenuItems[m.cursor].key)
	case "right":
		if key := sessionMenuItems[m.cursor].key; isForwardSessionItem(key) {
			return m, m.activateSessionMenuItem(key)
		}
	}
	return m, nil
}

// activateSessionMenuItem opens "continue →" (the migration picker), "list" (a
// read-only view of the same data), or "delete →" (the same browse, picking a
// session to delete) — see sessionApp's own doc comment for why these are the
// only rows, not every session subcommand.
func (m *sessionApp) activateSessionMenuItem(key string) tea.Cmd {
	switch key {
	case "continue":
		m.openPicker("continue")
		m.stack.push(screenContinuePicker)
	case "list":
		m.openPicker("list")
		m.stack.push(screenSessionList)
	case "delete":
		m.openPicker("delete")
		m.stack.push(screenSessionList)
	}
	return m.takePickerLoad()
}

// openPicker (re)loads every recorded session across every registered
// account (listSessions, session.go) and resets browse/search state — called
// once per visit to screenSessionList or screenContinuePicker, so a session
// copied/created/deleted since the last visit is never stale. Opening the
// delete picker also clears its multi-select set: a fresh visit starts with
// nothing checked.
//
// Delete is the one mode that narrows what it loads: a session a live Claude
// process is writing is filtered out (deletableSessions, session.go) so this
// screen only ever offers something it can actually delete, and the number left
// out is kept to be shown. Re-derived from /proc on every visit, like the list
// itself — nothing about "active" is stored, so a terminal that was killed
// leaves nothing behind to un-stick.
func (m *sessionApp) openPicker(mode string) {
	m.pickerMode = mode
	entries := listSessions(m.s, m.c)
	m.hiddenActive = 0
	m.active, m.alsoIn = nil, nil
	if mode == "delete" {
		var active []sessionEntry
		entries, active = deletableSessions(m.s, m.c, entries)
		m.hiddenActive = len(active)
		m.deleteSel = map[string]bool{}
	} else {
		m.active = activeSessionKeys(m.s, m.c)
		if mode == "continue" {
			entries, m.alsoIn = dedupeSessions(entries)
		}
	}
	m.picker = sessionListState{browseList: newSessionBrowseList(entries)}
	m.pickerLoad = loadSessionTitles(m.s, entries)
}

// isActive reports whether a live Claude process is using e — or, in
// CONTINUE SESSION's deduplicated list, any other copy of the same session.
func (m *sessionApp) isActive(e sessionEntry) bool {
	if m.active[sessionEntryKey(e)] {
		return true
	}
	for _, email := range m.alsoIn[sessionEntryKey(e)] {
		if m.active[sessionEntryKey(sessionEntry{email: email, dirName: e.dirName, sessionID: e.sessionID})] {
			return true
		}
	}
	return false
}

func (m *sessionApp) updatePickerBrowse(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() != "esc" {
		m.exitArmed = false
	}
	if m.deletePickerKey(msg, false) {
		return m, nil
	}
	switch m.picker.browseKey(msg, browseKeyOpts[sessionEntry]{leftBack: true, forward: forwardAll[sessionEntry]}) {
	case browseSelect:
		if e, ok := m.picker.selected(); ok {
			return m, m.pickSession(e, false)
		}
	case browseBack:
		if m.stack.atRoot() {
			return m.exitRootScreen()
		}
		m.stack.pop()
	}
	return m, nil
}

// pickSession is what Enter/→ on a highlighted row does, in one place so the
// browse and search paths can't diverge: "continue" proceeds to the
// destination-account picker, "delete" opens the confirmation, and "list" — the
// read-only view — does nothing (see sessionApp's own doc comment).
//
// For delete, the effective selection is every checked visible row; with none
// checked it falls back to the highlighted row, so deleting one session stays a
// single Enter and checking rows is an opt-in widening, never a requirement.
func (m *sessionApp) pickSession(e sessionEntry, searching bool) tea.Cmd {
	switch m.pickerMode {
	case "continue":
		return m.selectSession(&e)
	case "delete":
		entries := m.checkedDeleteEntries(searching)
		if len(entries) == 0 {
			entries = []sessionEntry{e}
		}
		m.beginDelete(entries)
	}
	return nil
}

// updatePickerSearch is updatePickerBrowse's actively-searching counterpart.
// Its key handling — including the "Esc here only ever clears the query,
// never pops or exits" rule every search screen in cpro follows — is the
// shared browseList's.
func (m *sessionApp) updatePickerSearch(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.deletePickerKey(msg, true) {
		return m, nil
	}
	if m.picker.searchKey(msg, browseKeyOpts[sessionEntry]{forward: forwardAll[sessionEntry]}) == browseSelect {
		if e, ok := m.picker.searchSelected(); ok {
			return m, m.pickSession(e, true)
		}
	}
	return m, nil
}

// deletePickerKey handles the multi-select keys DELETE SESSION adds on top of
// the shared browse/search state machine (decision 0052), returning true when
// the key was consumed. Tab and Ctrl+A work while searching too, since neither
// is otherwise bound there; Space only works while browsing, because in search
// mode it is a legitimate character to type into the query.
func (m *sessionApp) deletePickerKey(msg tea.KeyMsg, searching bool) bool {
	if m.pickerMode != "delete" {
		return false
	}
	switch msg.String() {
	case "tab":
		// Tab walks as it checks, so a run of rows can be marked with repeated
		// presses — the "check a lot quickly" key. Space stays the stay-put
		// toggle.
		m.toggleHighlightedDelete(searching)
		if searching {
			m.picker.searchMove(1)
		} else {
			m.picker.move(1)
		}
		return true
	case "ctrl+a":
		m.toggleAllDelete(searching)
		return true
	}
	// The space bar is the named key "space" in bubbletea v2 — verified both
	// against the vendored key table and through a real pty — so it must be
	// caught here. Otherwise it falls through to the shared browse machine,
	// which treats any printable character as "start typing to search": a search
	// whose query is spaces then silently filters every row out, which is
	// exactly what a missed Space press looks like on this screen.
	// Browse-only: in search mode a space is a legitimate query character.
	if !searching && msg.String() == "space" {
		m.toggleHighlightedDelete(false)
		return true
	}
	return false
}

func (m *sessionApp) toggleHighlightedDelete(searching bool) {
	var e sessionEntry
	var ok bool
	if searching {
		e, ok = m.picker.searchSelected()
	} else {
		e, ok = m.picker.selected()
	}
	if !ok {
		return
	}
	if m.deleteSel == nil {
		m.deleteSel = map[string]bool{}
	}
	key := sessionEntryKey(e)
	if m.deleteSel[key] {
		delete(m.deleteSel, key)
		return
	}
	m.deleteSel[key] = true
}

// toggleAllDelete checks every currently visible row — or clears them all when
// they are already every one checked, so pressing it twice is a no-op rather
// than a trap.
func (m *sessionApp) toggleAllDelete(searching bool) {
	visible := m.visibleDeleteEntries(searching)
	if len(visible) == 0 {
		return
	}
	if m.deleteSel == nil {
		m.deleteSel = map[string]bool{}
	}
	all := true
	for _, e := range visible {
		if !m.deleteSel[sessionEntryKey(e)] {
			all = false
			break
		}
	}
	for _, e := range visible {
		key := sessionEntryKey(e)
		if all {
			delete(m.deleteSel, key)
			continue
		}
		m.deleteSel[key] = true
	}
}

// visibleDeleteEntries is the row set Ctrl+A acts on: the filtered rows while
// searching, otherwise every loaded row, in list order.
func (m *sessionApp) visibleDeleteEntries(searching bool) []sessionEntry {
	if !searching {
		return m.picker.items
	}
	out := make([]sessionEntry, 0, len(m.picker.filtered))
	for _, i := range m.picker.filtered {
		out = append(out, m.picker.items[i])
	}
	return out
}

// checkedDeleteEntries is the checked subset of the currently visible rows, in
// list order — deliberately only visible ones, so Ctrl+A followed by a search
// narrows the batch instead of silently keeping rows that scrolled out of the
// filter.
func (m *sessionApp) checkedDeleteEntries(searching bool) []sessionEntry {
	var out []sessionEntry
	for _, e := range m.visibleDeleteEntries(searching) {
		if m.deleteSel[sessionEntryKey(e)] {
			out = append(out, e)
		}
	}
	return out
}

// selectSession records e as the session about to be continued and pushes
// the destination-account picker — every registered account, e's own owner
// included: which account continues a conversation is the user's call, made
// from the live Session usage each row shows, never something cpro guesses
// (picking the owner just resumes it in place, e.g. once its limit reset).
// Returns the destination screen's Session-usage fetch (the shared
// fetchAccountPickerUsage batch), or nil when every offered account is
// already cached.
func (m *sessionApp) selectSession(e *sessionEntry) tea.Cmd {
	m.continuing = e
	emails := make([]string, 0, len(m.c.Accounts))
	for email := range m.c.Accounts {
		emails = append(emails, email)
	}
	sort.Strings(emails)
	m.account = sessionAccountState{browseList: newAccountBrowseList(emails)}
	m.stack.push(screenContinueAccount)

	if m.s == nil {
		return nil
	}
	if m.accountUsage == nil {
		m.accountUsage = map[string]runAccountUsage{}
	}
	var pendingUsage, pendingAuth []string
	for _, email := range emails {
		if _, ok := m.accountUsage[email]; !ok {
			pendingUsage = append(pendingUsage, email)
		}
		if _, ok := m.accountAuth[email]; !ok {
			pendingAuth = append(pendingAuth, email)
		}
	}
	var usageCmd tea.Cmd
	if len(pendingUsage) > 0 {
		usageCmd = fetchAccountPickerUsage(m.s, pendingUsage)
	}
	return tea.Batch(usageCmd, fetchSessionAuth(m.s, pendingAuth))
}

// finalizeContinue builds the argv `cpro --resume SESSION_ID --account TO`
// dispatches to (main.go's hidden __resume command → resumeSession): copy
// just this one transcript into TO when it isn't already there, chdir to the
// session's own recorded working directory, and resume it by ID. That path,
// not `session continue`'s whole-directory copy from the current directory,
// is what a session picked from any directory needs — and the explicit
// --account is what keeps resumeSession from choosing an account itself.
// sessionApp still never migrates or runs anything on its own; this is the
// one place the argv is built (decision 0025).
func (m *sessionApp) finalizeContinue(to string) {
	m.picked = []string{"__resume", m.continuing.sessionID, "--account", to}
}

// beginDelete opens the in-app confirmation for `session delete` (decisions
// 0051/0052) — the same restate-the-target guard `cpro remove` uses (decision
// 0042), rendered in this program's own chrome instead of dropping to the
// command's bare terminal prompt, because this screen already IS the
// confirmation (that is exactly the half-migrated shape decision 0042 fixed for
// remove). One session asks for its ID back; several ask for the literal word
// "delete" instead, since retyping twenty IDs is not a guard anyone would
// actually pass. Like continue, this only ever builds argv: the deletion itself
// stays in deleteSessions (session.go), reached through the same `cpro session
// delete` command the argv spells out — sessionApp never deletes anything.
func (m *sessionApp) beginDelete(entries []sessionEntry) {
	m.pendingDelete = entries
	m.confirmInput, m.confirmErr = "", ""
	m.stack.push(screenDeleteConfirm)
}

// updateDeleteConfirm handles the confirmation field: printable characters
// append, Backspace deletes, Esc/← pops back to the session list without
// deleting anything (keeping the checked set, so the batch can be adjusted), and
// Enter only finalizes once the answer restates the intent — deleteConfirmMatches
// for a single session, bulkDeleteConfirmMatches for several. A mismatch stays on
// screen with a message rather than cancelling — the same "a typo is correctable
// in place" rule the remove-confirm and LOGIN email fields keep. The final argv
// carries --yes because this screen is the confirmation, so the command's own
// guard is deliberately not run a second time; a direct `cpro session delete ID`
// still prompts (or errors non-interactively) exactly as its own RunE defines.
func (m *sessionApp) updateDeleteConfirm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "left":
		m.stack.pop()
		m.pendingDelete = nil
		m.confirmInput, m.confirmErr = "", ""
	case "backspace":
		r := []rune(m.confirmInput)
		if len(r) > 0 {
			m.confirmInput = string(r[:len(r)-1])
		}
		m.confirmErr = ""
	case "enter":
		if len(m.pendingDelete) == 0 {
			return m, nil
		}
		if len(m.pendingDelete) == 1 {
			if !deleteConfirmMatches(m.confirmInput, m.pendingDelete[0].sessionID) {
				m.confirmErr = "does not match; type the session ID to confirm"
				return m, nil
			}
		} else if !bulkDeleteConfirmMatches(m.confirmInput) {
			m.confirmErr = "type " + bulkDeleteConfirmation + " to confirm"
			return m, nil
		}
		// Account-qualified selectors, so a batch spanning accounts deletes
		// exactly the rows that were checked. A single --account could not say
		// which account each ID belongs to when the same ID exists under two.
		args := make([]string, 0, len(m.pendingDelete)+3)
		args = append(args, "session", "delete")
		for _, e := range m.pendingDelete {
			args = append(args, e.email+":"+e.sessionID)
		}
		m.picked = append(args, "--yes")
		return m, tea.Quit
	default:
		if text := msg.Key().Text; text != "" {
			m.confirmInput += text
			m.confirmErr = ""
		}
	}
	return m, nil
}

// viewDeleteConfirm renders the guard in the same single-color panel every other
// small screen here uses (renderPanel, tui.go). One session gets the same
// short-ID/project/account line the list row shows and its ID to retype; several
// get the batch listed (capped, with a count for the rest) and the literal word
// to type. The typed value keeps the usual "_" cursor and a mismatch message
// renders in dangerColor.
func (m *sessionApp) viewDeleteConfirm() string {
	var lines []string
	switch n := len(m.pendingDelete); {
	case n == 1:
		e := m.pendingDelete[0]
		shown := shortSessionID(e.sessionID)
		lines = []string{
			"  This permanently deletes the recorded transcript for:",
			"  " + styleText(m.color, shown+"  "+projectDisplayPath(e.dirName)+"  "+displayEmail(e.email), accentMode),
			"",
			"  Type " + shown + " to confirm: " + m.confirmInput + styleText(m.color, "_", accentMode),
		}
	case n > 1:
		const listed = 8
		lines = []string{fmt.Sprintf("  This permanently deletes %d recorded transcripts:", n)}
		for i, e := range m.pendingDelete {
			if i == listed {
				lines = append(lines, fmt.Sprintf("  … and %d more", n-listed))
				break
			}
			lines = append(lines, "  "+shortSessionID(e.sessionID)+"  "+projectDisplayPath(e.dirName)+"  "+displayEmail(e.email))
		}
		lines = append(lines, "",
			"  Type "+bulkDeleteConfirmation+" to confirm: "+m.confirmInput+styleText(m.color, "_", accentMode))
	}
	if m.confirmErr != "" {
		lines = append(lines, "", "  "+styleText(m.color, m.confirmErr, dangerColor))
	}
	body := renderPanel(m.color, m.frameColor(), screenTitle("DELETE SESSION"), lines)
	footer := renderFooter(m.color, accentMode,
		[2]string{"↵", "Delete"}, [2]string{"←", "Back"}, [2]string{"Esc", "Back"})
	return body + "\n\n" + footer
}

func (m *sessionApp) updateAccountBrowse(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch m.account.browseKey(msg, browseKeyOpts[string]{leftBack: true, forward: forwardAll[string]}) {
	case browseSelect:
		if email, ok := m.account.selected(); ok {
			m.finalizeContinue(email)
			return m, tea.Quit
		}
	case browseBack:
		// Always a nested frame under screenContinuePicker — never the
		// stack's own root — so this is always a plain pop, no exitRootScreen
		// check needed.
		m.stack.pop()
	}
	return m, nil
}

func (m *sessionApp) updateAccountSearch(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.account.searchKey(msg, browseKeyOpts[string]{forward: forwardAll[string]}) == browseSelect {
		if email, ok := m.account.searchSelected(); ok {
			m.finalizeContinue(email)
			return m, tea.Quit
		}
	}
	return m, nil
}

// viewSessionMenu renders the SESSIONS screen: two fixed rows with inline
// descriptions, the same small-fixed-list style rootPickerApp's own System
// submenu uses (no type-to-search — two items don't need it, matching that
// screen's own precedent — see sessionListState's screens below for where
// search actually earns its keep).
func (m *sessionApp) viewSessionMenu() string {
	labelWidth := 0
	for _, it := range sessionMenuItems {
		labelWidth = max(labelWidth, visibleWidth(sessionItemLabel(it)))
	}
	lines := make([]string, len(sessionMenuItems))
	for i, it := range sessionMenuItems {
		cursor := "  "
		if i == m.cursor {
			cursor = styleText(m.color, "❯", accentMode) + " "
		}
		lines[i] = cursor + padEnd(sessionItemLabel(it), labelWidth+7) + it.desc
	}
	body := renderPanel(m.color, m.frameColor(), screenTitle("SESSIONS"), lines)
	body += "\n\n" + sessionScreenDescription
	hints := [][2]string{{"↑↓", "Navigate"}, {"→", "Open"}, {"↵", "Select"}}
	hints = append(hints, m.backAndExitHints()...)
	footer := renderFooter(m.color, accentMode, hints...)
	return body + "\n\n" + footer
}

// sessionRowMeta is one row's plain (unstyled) metadata text — working
// directory, shortened session ID, and age, plus the owning account when
// sessions span more than one (see sessionsSpanMultipleAccounts) — kept
// unstyled so it can be safely truncated with truncateToWidth (tui.go, not
// ANSI-aware) before any color is applied, the same discipline rootRow
// (rootui.go) uses for its own metadata column.
func sessionRowMeta(e sessionEntry, showAccount bool) string {
	meta := truncatePath(projectDisplayPath(e.dirName), 40) + " · " + shortSessionID(e.sessionID) + " · " + formatDuration(time.Since(e.modTime)) + " ago"
	if showAccount {
		meta += " · " + displayEmail(e.email)
	}
	return meta
}

func sessionsSpanMultipleAccounts(entries []sessionEntry) bool {
	seen := map[string]bool{}
	for _, e := range entries {
		seen[e.email] = true
		if len(seen) > 1 {
			return true
		}
	}
	return false
}

// pickerTitle is the border title for whichever mode m.pickerMode is
// currently serving.
func (m *sessionApp) pickerTitle() string {
	switch m.pickerMode {
	case "continue":
		return "CONTINUE SESSION"
	case "delete":
		return "DELETE SESSION"
	default:
		return "SESSIONS LIST"
	}
}

// hiddenActiveNote is the one-line explanation DELETE SESSION prints whenever
// it left sessions out of its list, so a missing row reads as a rule ("that one
// is in use") instead of as a session that failed to load. Empty for every other
// mode and when nothing was hidden.
func (m *sessionApp) hiddenActiveNote() string {
	if m.pickerMode != "delete" || m.hiddenActive == 0 {
		return ""
	}
	verb := "are"
	if m.hiddenActive == 1 {
		verb = "is"
	}
	return fmt.Sprintf("%d session(s) in use by a running Claude session %s not listed.", m.hiddenActive, verb)
}

// viewPickerBrowse renders screenSessionList/screenContinuePicker while
// browsing (not actively searching): one line per session — deliberately a
// single row, not the three-line sketch in the requesting spec's own mockup,
// to stay consistent with every other list in cpro (rootRow, accountPickerApp,
// RUN ACCOUNT), which are all one-row-per-entry and responsively degrade
// rather than wrap.
func (m *sessionApp) viewPickerBrowse() string {
	st := m.picker
	if len(st.items) == 0 {
		lines := []string{"No sessions found"}
		if note := m.hiddenActiveNote(); note != "" {
			lines = append(lines, "", note)
		}
		body := renderPanel(m.color, m.frameColor(), screenTitle(m.pickerTitle()), lines)
		hints := [][2]string{{"↑↓", "Navigate"}}
		hints = append(hints, m.backAndExitHints()...)
		footer := renderFooter(m.color, accentMode, hints...)
		return body + "\n\n" + footer
	}
	multiAccount := sessionsSpanMultipleAccounts(st.items)
	nameWidth := 0
	for _, e := range st.items {
		nameWidth = max(nameWidth, visibleWidth(projectDisplayName(e.dirName)))
	}
	lines := make([]string, len(st.items))
	for i, e := range st.items {
		lines[i] = m.pickerRowLine(i, e, st.cursor, nameWidth, multiAccount)
	}
	// Keep the highlighted row on screen on a short terminal — the same
	// shared scroll window every other list in cpro uses (browseui.go).
	lines, _ = scrollLines(m.height, listChromeLines, lines, nil, st.cursor)
	body := renderPanel(m.color, m.frameColor(), screenTitle(m.pickerTitle()), lines)
	if note := m.hiddenActiveNote(); note != "" {
		body += "\n\n" + note
	}
	hints := [][2]string{{"↑↓", "Navigate"}}
	switch m.pickerMode {
	case "continue":
		hints = append(hints, [2]string{"→", "Select"}, [2]string{"↵", "Continue"})
	case "delete":
		hints = append(hints, [2]string{"Space", "Check"}, [2]string{"^A", "All"}, [2]string{"↵", "Delete"})
	}
	hints = append(hints, [2]string{"", "Type to search"})
	hints = append(hints, m.backAndExitHints()...)
	footer := renderFooter(m.color, accentMode, hints...)
	return body + "\n\n" + footer
}

// pickerRowLine renders one session row: cursor, the project/session name
// padded to align every row's metadata column, then dim metadata truncated
// to the terminal width — truncating the plain text first, then styling it,
// never the other way around (truncateToWidth isn't ANSI-aware).
func (m *sessionApp) pickerRowLine(i int, e sessionEntry, cursor int, nameWidth int, multiAccount bool) string {
	c := "  "
	if i == cursor {
		c = styleText(m.color, "❯", accentMode) + " "
	}
	// DELETE SESSION is the one mode that shows a checkbox, so what Ctrl+A /
	// Space have checked is visible rather than implied (decision 0052). Kept
	// unstyled when empty so it reads as an affordance, not a value.
	marker := ""
	if m.pickerMode == "delete" {
		if m.deleteSel[sessionEntryKey(e)] {
			marker = styleText(m.color, "[x]", accentMode) + " "
		} else {
			marker = "[ ] "
		}
	}
	name := padEnd(projectDisplayName(e.dirName), nameWidth)
	// A session a live Claude process is using carries a visible tag rather
	// than being hidden or blocked: continuing it is allowed, but forks the
	// conversation, so it should be a deliberate pick. (DELETE SESSION never
	// lists such a session in the first place — decision 0053.)
	tag := ""
	if m.pickerMode != "delete" && m.isActive(e) {
		tag = styleText(m.color, "● running", warningColor) + "  "
	}
	meta := sessionRowMeta(e, multiAccount)
	if e.title != "" {
		meta = truncateToWidth(e.title, 48) + " · " + meta
	}
	if others := m.alsoIn[sessionEntryKey(e)]; len(others) > 0 {
		shown := make([]string, len(others))
		for i, email := range others {
			shown[i] = displayEmail(email)
		}
		meta += " · also in " + strings.Join(shown, ", ")
	}
	if m.width > 0 {
		budget := m.width - visibleWidth(name) - visibleWidth(marker) - visibleWidth(tag) - 6
		if budget <= 0 {
			meta = ""
		} else {
			meta = truncateToWidth(meta, budget)
		}
	}
	if meta == "" && tag == "" {
		return c + marker + name
	}
	return c + marker + name + "  " + tag + dimStyle(m.color, meta)
}

// viewPickerSearch is viewPickerBrowse's actively-searching counterpart,
// modeled on accountPickerApp's/configApp's own search views.
func (m *sessionApp) viewPickerSearch() string {
	st := m.picker
	header := screenTitle(m.pickerTitle()) + ": " + st.query + styleText(m.color, "_", accentMode)
	if len(st.filtered) == 0 {
		lines := []string{"No sessions found"}
		if note := m.hiddenActiveNote(); note != "" {
			lines = append(lines, "", note)
		}
		body := renderPanel(m.color, accentMode, header, lines)
		footer := renderFooter(m.color, accentMode, [2]string{"", "Backspace to edit"}, [2]string{"Esc", "Clear"})
		return body + "\n\n" + footer
	}
	multiAccount := sessionsSpanMultipleAccounts(st.items)
	nameWidth := 0
	for _, idx := range st.filtered {
		nameWidth = max(nameWidth, visibleWidth(projectDisplayName(st.items[idx].dirName)))
	}
	lines := make([]string, len(st.filtered))
	for i, idx := range st.filtered {
		lines[i] = m.pickerRowLine(i, st.items[idx], st.fcursor, nameWidth, multiAccount)
	}
	lines, _ = scrollLines(m.height, listChromeLines, lines, nil, st.fcursor)
	body := renderPanel(m.color, accentMode, header, lines)
	hints := [][2]string{{"↑↓", "Navigate"}}
	switch m.pickerMode {
	case "continue":
		hints = append(hints, [2]string{"→", "Select"}, [2]string{"↵", "Continue"})
	case "delete":
		hints = append(hints, [2]string{"Space", "Check"}, [2]string{"^A", "All"}, [2]string{"↵", "Delete"})
	}
	hints = append(hints, [2]string{"Esc", "Clear"})
	footer := renderFooter(m.color, accentMode, hints...)
	return body + "\n\n" + footer
}

// viewAccountBrowse renders the destination-account picker while browsing:
// the shared accountListLines (browseui.go) — the same live Session-usage
// row, alignment, and scroll window every other account picker in cpro uses.
func (m *sessionApp) viewAccountBrowse() string {
	lines := m.destinationLines(m.account.items, m.account.cursor)
	if len(lines) == 0 {
		lines = []string{"No accounts found"}
	}
	body := renderPanel(m.color, accentMode, screenTitle("DESTINATION ACCOUNT"), lines)
	body += m.continuingNote()
	footer := renderFooter(m.color, accentMode,
		[2]string{"↑↓", "Navigate"}, [2]string{"→", "Select"}, [2]string{"↵", "Continue"}, [2]string{"", "Type to search"}, [2]string{"←", "Back"}, [2]string{"Esc", "Back"})
	return body + "\n\n" + footer
}

// signedOutTag is what a DESTINATION ACCOUNT row whose own session is not
// valid carries, so an account that would fail at launch is visible before
// it is picked. It stays selectable — s.run reports the real error — since
// the check can be wrong (a transient `claude auth status` failure).
const signedOutTag = "○ signed out"

// destinationLines is accountListLines (browseui.go) — the same shared
// accountPickerRow and scroll window — plus signedOutTag on accounts
// fetchSessionAuth found signed out. Room for the tag is reserved from the
// width up front, so a narrow terminal degrades the row, never wraps it.
func (m *sessionApp) destinationLines(emails []string, cursor int) []string {
	width := m.width
	if width > 0 {
		width -= visibleWidth(signedOutTag) + 2
	}
	nameWidth := accountPickerNameWidth(emails)
	lines := make([]string, len(emails))
	for i, email := range emails {
		lines[i] = accountPickerRow(m.color, width, email, m.accountUsage[email], i == cursor, nameWidth)
		if ok, known := m.accountAuth[email]; known && !ok {
			lines[i] += "  " + styleText(m.color, signedOutTag, warningColor)
		}
	}
	start, end := scrollRange(m.height, listChromeLines, len(lines), cursor)
	return lines[start:end]
}

// continuingNote names the session being continued and the account it was
// recorded under, below DESTINATION ACCOUNT's panel, so choosing the owner
// (resume in place) or another account (move it there) is an informed pick.
func (m *sessionApp) continuingNote() string {
	if m.continuing == nil {
		return ""
	}
	e := m.continuing
	note := projectDisplayName(e.dirName) + " · " + shortSessionID(e.sessionID) + " · from " + displayEmail(e.email)
	if e.title != "" {
		note = e.title + " · " + note
	}
	if m.isActive(*e) {
		note += "\n" + styleText(m.color, "● running in another Claude process — continuing it forks the conversation", warningColor)
	}
	return "\n\n" + note
}

// viewAccountSearch is viewAccountBrowse's actively-searching counterpart.
func (m *sessionApp) viewAccountSearch() string {
	st := m.account
	header := screenTitle("DESTINATION ACCOUNT") + ": " + st.query + styleText(m.color, "_", accentMode)
	if len(st.filtered) == 0 {
		body := renderPanel(m.color, accentMode, header, []string{"No accounts found"})
		footer := renderFooter(m.color, accentMode, [2]string{"", "Backspace to edit"}, [2]string{"Esc", "Clear"})
		return body + "\n\n" + footer
	}
	emails := make([]string, len(st.filtered))
	for i, idx := range st.filtered {
		emails[i] = st.items[idx]
	}
	lines := m.destinationLines(emails, st.fcursor)
	body := renderPanel(m.color, accentMode, header, lines) + m.continuingNote()
	footer := renderFooter(m.color, accentMode, [2]string{"↑↓", "Navigate"}, [2]string{"→", "Select"}, [2]string{"↵", "Continue"}, [2]string{"Esc", "Clear"})
	return body + "\n\n" + footer
}
