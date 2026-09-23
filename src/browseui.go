//go:build linux

package main

import (
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// browseList is the one shared browse/search/cursor state machine behind
// every searchable list screen in cpro — the root/menu command picker, RUN
// ACCOUNT, DEFAULT ACCOUNT, system export's SELECT ACCOUNT, --resume's RESUME
// ACCOUNT, and cpro session's SESSION LIST/CONTINUE SESSION/DESTINATION
// ACCOUNT. Before this existed, seven refilter* methods across five types
// each reimplemented cursor movement, type-to-search filtering, backspace
// handling (including the "emptied the query, so leave search mode" rule) and
// the clamp of the search cursor back onto a shrunken result set; a behavior
// added to one picker (e.g. the root picker's scroll window, or the
// account-row Session-usage cell) simply did not reach the other four. Every
// one of them now owns one of these instead, and the behaviors live here once.
//
// It is deliberately generic over the row type rather than normalizing every
// list to plain strings: the screens genuinely hold different things
// (rootPickerEntry, sessionEntry, a bare account email) and keep rendering
// their own row for each, so the shared component owns state and key handling
// while each screen keeps its own presentation.
type browseList[T any] struct {
	items  []T
	cursor int // browse-mode selection

	query    string
	filtered []int // indices into items matching query, best match first
	fcursor  int   // filtered[fcursor] — search-mode selection

	// match returns the plain haystack a query is matched against for one
	// item. refilter lowercases it; display filtering always goes through
	// displayEmail for account rows (decision 0024), so search never needs —
	// or echoes back — a masked account's real address.
	match func(T) string
	// rank, when non-nil, decides whether an item matches a (lowercased)
	// query and how well, higher first; when nil every substring match ties
	// and the list keeps its own order. Only the command picker ranks
	// (rootPickerScore's five tiers); every account/session list uses the
	// plain case-insensitive substring rule.
	rank func(T, string) (int, bool)
}

// newBrowseList builds an empty list over match/rank; see the field docs.
func newBrowseList[T any](match func(T) string, rank func(T, string) (int, bool)) browseList[T] {
	return browseList[T]{match: match, rank: rank}
}

// newAccountBrowseList is the shared account-list configuration: items are
// account emails and a query matches the account's *displayed* identity via
// displayEmail (decision 0024), so searching while masking is on matches what
// is actually on screen and never the real address.
func newAccountBrowseList(emails []string) browseList[string] {
	b := newBrowseList(func(email string) string { return displayEmail(email) }, nil)
	b.setItems(emails)
	return b
}

// newSessionBrowseList is the SESSION LIST/CONTINUE SESSION configuration:
// a query matches a session's project name, its (best-effort decoded)
// working directory, its full session ID, and its owning account through
// displayEmail — see sessionRowMeta for the same fields rendered as a row's
// metadata.
func newSessionBrowseList(entries []sessionEntry) browseList[sessionEntry] {
	b := newBrowseList(func(e sessionEntry) string {
		return projectDisplayName(e.dirName) + " " + projectDisplayPath(e.dirName) + " " + e.sessionID + " " + displayEmail(e.email) + " " + e.title
	}, nil)
	b.setItems(entries)
	return b
}

// newCommandBrowseList is the command picker's configuration: the ranked
// rootPickerScore tiers, not a plain substring match, since a short command
// name benefits from exact/prefix/subsequence ranking — and its metadata and
// description are legitimate search targets ("shell" finds completion).
func newCommandBrowseList(entries []rootPickerEntry) browseList[rootPickerEntry] {
	b := newBrowseList(
		func(e rootPickerEntry) string { return e.name },
		func(e rootPickerEntry, query string) (int, bool) { return rootPickerScore(e, query) },
	)
	b.setItems(entries)
	return b
}

// newAccountEntryBrowseList is newAccountBrowseList's rootPickerEntry-shaped
// sibling: the root picker's RUN ACCOUNT/LOGOUT/REMOVE frames carry one entry
// per registered email (name only), and search there must be the same plain
// displayEmail substring match the standalone account pickers use — not the
// command picker's ranked rootPickerScore tiers, which would treat an email
// as a command name.
func newAccountEntryBrowseList(entries []rootPickerEntry) browseList[rootPickerEntry] {
	b := newBrowseList(func(e rootPickerEntry) string { return displayEmail(e.name) }, nil)
	b.setItems(entries)
	return b
}

// setItems replaces the list's contents and resets both cursor and search
// state — the one entry point every screen uses when (re)opening a list, so a
// stale query or a cursor past the new end can never survive a visit.
func (b *browseList[T]) setItems(items []T) {
	b.items = items
	b.cursor = 0
	b.clearSearch()
}

// searching reports whether the list is currently in search mode — the same
// m.query != "" test every screen used to spell out for itself.
func (b *browseList[T]) searching() bool { return b.query != "" }

// move shifts the browse cursor by delta, wrapping around both ends.
func (b *browseList[T]) move(delta int) {
	if len(b.items) == 0 {
		return
	}
	b.cursor = (b.cursor + delta + len(b.items)) % len(b.items)
}

// searchMove shifts the search cursor by delta, wrapping around both ends.
func (b *browseList[T]) searchMove(delta int) {
	if len(b.filtered) == 0 {
		return
	}
	b.fcursor = (b.fcursor + delta + len(b.filtered)) % len(b.filtered)
}

// selected returns the highlighted row while browsing.
func (b *browseList[T]) selected() (T, bool) {
	var zero T
	if len(b.items) == 0 {
		return zero, false
	}
	return b.items[b.cursor], true
}

// searchSelected returns the highlighted row while searching.
func (b *browseList[T]) searchSelected() (T, bool) {
	var zero T
	if len(b.filtered) == 0 {
		return zero, false
	}
	return b.items[b.filtered[b.fcursor]], true
}

// typeRune starts search mode with text (any printable keystroke, no "/"
// shortcut anywhere in cpro) or extends the query already in progress.
func (b *browseList[T]) typeRune(text string) {
	if text == "" {
		return
	}
	b.query += text
	b.refilter()
}

// backspace deletes the last query rune, leaving search mode entirely (and
// restoring the full browse list) once the query is empty.
func (b *browseList[T]) backspace() {
	r := []rune(b.query)
	if len(r) > 0 {
		b.query = string(r[:len(r)-1])
	}
	if b.query == "" {
		b.filtered, b.fcursor = nil, 0
		return
	}
	b.refilter()
}

// clearSearch leaves search mode, restoring the full browse list.
func (b *browseList[T]) clearSearch() { b.query, b.filtered, b.fcursor = "", nil, 0 }

// refilter recomputes filtered from query, ranked best-first when the list
// has a rank function and left in the list's own order otherwise (that order
// is the deliberate, deterministic tie-breaker), then clamps fcursor back
// onto the new result set.
func (b *browseList[T]) refilter() {
	query := strings.ToLower(b.query)
	if b.rank != nil {
		type scored struct{ idx, score int }
		matches := make([]scored, 0, len(b.items))
		for i, item := range b.items {
			if score, ok := b.rank(item, query); ok {
				matches = append(matches, scored{i, score})
			}
		}
		sort.SliceStable(matches, func(i, j int) bool { return matches[i].score > matches[j].score })
		b.filtered = make([]int, len(matches))
		for i, m := range matches {
			b.filtered[i] = m.idx
		}
	} else {
		var filtered []int
		for i, item := range b.items {
			if strings.Contains(strings.ToLower(b.match(item)), query) {
				filtered = append(filtered, i)
			}
		}
		b.filtered = filtered
	}
	if b.fcursor >= len(b.filtered) {
		b.fcursor = 0
	}
}

// browseKeyOutcome is what one keystroke meant at the list level, so a screen
// only has to supply the parts that are genuinely its own (what Enter does,
// what Esc means): everything else — cursor movement, type-to-search,
// backspace, and the "→ opens, never executes a leaf row" gate — is decided
// once, here.
type browseKeyOutcome int

const (
	browseNoop     browseKeyOutcome = iota // nothing this list does with that key
	browseConsumed                         // the list handled it; the screen need not act
	browseSelect                           // Enter/→ on the highlighted row — read selected()/searchSelected()
	browseBack                             // Esc (or ←, when enabled) while browsing — the screen decides
)

// browseKeyOpts are the two screen-dependent pieces of key behavior:
type browseKeyOpts[T any] struct {
	// forward, when non-nil, gates the → alias on the highlighted row: →
	// selects only when it returns true, while Enter always selects. The
	// root/menu picker passes isForwardEntry so → never executes a leaf row;
	// the account pickers pass nil (→ is not a select alias there).
	forward func(T) bool
	// leftBack makes ← behave exactly like Esc while browsing. The screens
	// that already had that (RUN ACCOUNT, DEFAULT ACCOUNT, session's
	// pickers) set it; the two standalone account pickers never did, and
	// pass false so ← stays a no-op.
	leftBack bool
}

// browseKey applies one keystroke while browsing.
func (b *browseList[T]) browseKey(msg tea.KeyMsg, opts browseKeyOpts[T]) browseKeyOutcome {
	switch key := msg.String(); key {
	case "up":
		b.move(-1)
		return browseConsumed
	case "down":
		b.move(1)
		return browseConsumed
	case "enter":
		if _, ok := b.selected(); ok {
			return browseSelect
		}
	case "right":
		if item, ok := b.selected(); ok && opts.forward != nil && opts.forward(item) {
			return browseSelect
		}
	case "esc":
		return browseBack
	case "left":
		if opts.leftBack {
			return browseBack
		}
	default:
		if text := msg.Key().Text; text != "" {
			b.typeRune(text)
			return browseConsumed
		}
	}
	return browseNoop
}

// searchKey applies one keystroke while searching. Esc here only ever clears
// the query — it never also pops a frame or exits in the same keystroke,
// matching every search screen in cpro.
func (b *browseList[T]) searchKey(msg tea.KeyMsg, opts browseKeyOpts[T]) browseKeyOutcome {
	switch key := msg.String(); key {
	case "up":
		b.searchMove(-1)
		return browseConsumed
	case "down":
		b.searchMove(1)
		return browseConsumed
	case "esc":
		b.clearSearch()
		return browseConsumed
	case "backspace":
		b.backspace()
		return browseConsumed
	case "enter":
		if _, ok := b.searchSelected(); ok {
			return browseSelect
		}
	case "right":
		if item, ok := b.searchSelected(); ok && opts.forward != nil && opts.forward(item) {
			return browseSelect
		}
	default:
		if text := msg.Key().Text; text != "" {
			b.typeRune(text)
			return browseConsumed
		}
	}
	return browseNoop
}

// forwardAll is the browseKeyOpts.forward predicate for a list where → is a
// plain select alias for every row — RUN ACCOUNT/LOGOUT/REMOVE, RESUME
// ACCOUNT, and the session pickers — as opposed to the command picker's own
// rootForward, which lets → act only on rows that open a further screen.
func forwardAll[T any](T) bool { return true }

// listChromeLines is the number of rendered lines outside a simple list
// panel's own body: the panel's top and bottom borders, one blank line, and
// the footer — the shape every account/session list draws with renderPanel +
// "\n\n" + renderFooter, and what scrollRange reserves so a list never
// overflows a short terminal.
const listChromeLines = 4

// scrollRange returns the [start,end) slice of a total-line list that fits a
// terminal height rows tall once chrome lines of non-list content (borders,
// description, footer — see listChromeLines/rootChromeLines) are reserved,
// keeping cursorLine on screen by centering it when there is room to either
// side and clamping at either end otherwise. height <= 0 (no WindowSizeMsg
// yet) leaves everything visible. This is the one implementation behind
// every list screen's scrolling: without it, Down past the bottom edge of the
// viewport moved the selection but never brought it back on screen, so Enter
// had no visible target.
func scrollRange(height, chrome, total, cursorLine int) (int, int) {
	if height <= 0 || total == 0 {
		return 0, total
	}
	available := max(1, height-chrome)
	if total <= available {
		return 0, total
	}
	start := max(0, min(total-available, cursorLine-available/2))
	return start, start + available
}

// scrollLines slices lines (and its optional parallel per-line color slice,
// for the root picker's multi-shade rail) down to scrollRange's window. A nil
// lineHex is returned nil, so callers with single-color panels can ignore it.
func scrollLines(height, chrome int, lines, lineHex []string, cursorLine int) ([]string, []string) {
	start, end := scrollRange(height, chrome, len(lines), cursorLine)
	if start == 0 && end == len(lines) {
		return lines, lineHex
	}
	if lineHex == nil {
		return lines[start:end], nil
	}
	return lines[start:end], lineHex[start:end]
}

// accountListLines renders a whole account list — identity plus the shared
// live Session-usage cell (accountPickerRow) — and scrolls it to keep the
// selected row visible at the real terminal size. Every account picker in
// cpro (RUN ACCOUNT, DEFAULT ACCOUNT, RESUME ACCOUNT, SELECT ACCOUNT,
// DESTINATION ACCOUNT) goes through this one function, so usage display,
// narrow-terminal degradation, alignment, and scrolling cannot drift between
// screens. selected indexes emails itself, so search callers pass their
// already-filtered slice and search cursor.
func accountListLines(color bool, width, height int, emails []string, selected int, usage map[string]runAccountUsage) []string {
	nameWidth := accountPickerNameWidth(emails)
	lines := make([]string, len(emails))
	for i, email := range emails {
		lines[i] = accountPickerRow(color, width, email, usage[email], i == selected, nameWidth)
	}
	start, end := scrollRange(height, listChromeLines, len(lines), selected)
	return lines[start:end]
}

// entryEmails unpacks the emails an account list of rootPickerEntry rows
// carries (RUN ACCOUNT/LOGOUT/REMOVE's own frame shape), so those screens can
// share accountListLines with the plain []string account pickers.
func entryEmails(entries []rootPickerEntry) []string {
	emails := make([]string, len(entries))
	for i, e := range entries {
		emails[i] = e.name
	}
	return emails
}

// accountPickerConfig is what makes accountPickerApp reusable across cpro's
// standalone (not nested in a bigger tea.Program) account pickers, currently
// system export's SELECT ACCOUNT and --resume's RESUME ACCOUNT: everything
// about the screen that isn't the shared browse/search/render machinery
// itself. title is the screenTitle (tui.go); actionVerb is the ↵ hint in both
// the normal and search footers; escVerb is the Esc hint in the normal footer
// (search's own Esc always reads "Clear", the same everywhere); forward wires
// → to also select the highlighted row (forwardAll[string], browseKey's own
// opts) and adds the "→ Select" footer hint — RESUME ACCOUNT wants this,
// SELECT ACCOUNT never has.
type accountPickerConfig struct {
	title      string
	actionVerb string
	escVerb    string
	forward    bool
}

// accountPickerApp is the shared tea.Model behind every standalone account
// picker in cpro: identical identity/Session-usage rows (accountListLines),
// identical browse/search key handling (browseList, browseui.go), differing
// from one screen to the next only in accountPickerConfig. Before this
// existed, exportPickerApp (system.go) and resumeAccountPickerApp
// (resumeui.go) were two separate types with line-for-line identical
// Init/Update/updateNormal/updateSearch/View/viewNormal/viewSearch bodies —
// the exact per-screen duplication browseList itself was introduced to
// eliminate at the cursor/search level, just one layer up.
type accountPickerApp struct {
	s *store
	browseList[string]
	cfg accountPickerConfig

	color  bool
	width  int
	height int

	accountUsage map[string]runAccountUsage

	picked string
}

// Init starts every offered account's Session-usage fetch, the same shared
// batch every other account picker uses.
func (m *accountPickerApp) Init() tea.Cmd {
	if m.s == nil || len(m.items) == 0 {
		return nil
	}
	return fetchAccountPickerUsage(m.s, m.items)
}

func (m *accountPickerApp) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
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
	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			m.picked = ""
			return m, tea.Quit
		}
		if m.searching() {
			return m.updateSearch(msg)
		}
		return m.updateNormal(msg)
	}
	return m, nil
}

// keyOpts is the one place accountPickerConfig.forward turns into an actual
// browseKeyOpts — updateNormal and updateSearch both delegate the whole key
// handling (cursor movement, type-to-search, backspace) to browseList
// (browseui.go) and only supply what a picker's own → meaning is.
func (m *accountPickerApp) keyOpts() browseKeyOpts[string] {
	if m.cfg.forward {
		return browseKeyOpts[string]{forward: forwardAll[string]}
	}
	return browseKeyOpts[string]{}
}

func (m *accountPickerApp) updateNormal(km tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch m.browseKey(km, m.keyOpts()) {
	case browseSelect:
		if email, ok := m.selected(); ok {
			m.picked = email
			return m, tea.Quit
		}
	case browseBack:
		m.picked = ""
		return m, tea.Quit
	}
	return m, nil
}

func (m *accountPickerApp) updateSearch(km tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.searchKey(km, m.keyOpts()) == browseSelect {
		if email, ok := m.searchSelected(); ok {
			m.picked = email
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m *accountPickerApp) View() tea.View {
	var content string
	if m.searching() {
		content = m.viewSearch()
	} else {
		content = m.viewNormal()
	}
	v := tea.NewView(content)
	// Same fix as rootPickerApp's own View (rootui.go), for the same reason:
	// a long enough account list can exceed a real terminal's height, and
	// without the alt screen bubbletea's relative-cursor redraws corrupt once
	// that forces the terminal to scroll.
	v.AltScreen = true
	return v
}

func (m *accountPickerApp) viewNormal() string {
	lines := accountListLines(m.color, m.width, m.height, m.items, m.cursor, m.accountUsage)
	body := renderPanel(m.color, accentMode, screenTitle(m.cfg.title), lines)
	hints := [][2]string{{"↑↓", "Navigate"}}
	if m.cfg.forward {
		hints = append(hints, [2]string{"→", "Select"})
	}
	hints = append(hints, [2]string{"↵", m.cfg.actionVerb}, [2]string{"", "Type to search"}, [2]string{"Esc", m.cfg.escVerb})
	footer := renderFooter(m.color, accentMode, hints...)
	return body + "\n\n" + footer
}

func (m *accountPickerApp) viewSearch() string {
	header := screenTitle(m.cfg.title) + ": " + m.query + styleText(m.color, "_", accentMode)
	if len(m.filtered) == 0 {
		body := renderPanel(m.color, accentMode, header, []string{"No accounts found"})
		footer := renderFooter(m.color, accentMode, [2]string{"", "Backspace to edit"}, [2]string{"Esc", "Clear"})
		return body + "\n\n" + footer
	}
	emails := make([]string, len(m.filtered))
	for i, idx := range m.filtered {
		emails[i] = m.items[idx]
	}
	lines := accountListLines(m.color, m.width, m.height, emails, m.fcursor, m.accountUsage)
	body := renderPanel(m.color, accentMode, header, lines)
	footer := renderFooter(m.color, accentMode, [2]string{"↑↓", "Navigate"}, [2]string{"↵", m.cfg.actionVerb}, [2]string{"Esc", "Clear"})
	return body + "\n\n" + footer
}
