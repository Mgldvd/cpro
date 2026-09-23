//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

	tea "charm.land/bubbletea/v2"
	"github.com/spf13/cobra"
)

func TestRunArgs(t *testing.T) {
	for _, tc := range []struct {
		args       []string
		email      string
		forward    []string
		help, fail bool
	}{
		{args: []string{"--dangerously-skip-permissions", "-p", "text with spaces"}, forward: []string{"--dangerously-skip-permissions", "-p", "text with spaces"}},
		{args: []string{"--account", "User@example.com", "--resume"}, email: "user@example.com", forward: []string{"--resume"}},
		{args: []string{"--account=user@example.com", "--", "--help"}, email: "user@example.com", forward: []string{"--help"}},
		{args: []string{"-p", "--account", "--help"}, forward: []string{"-p", "--account", "--help"}},
		{args: []string{"--help"}, help: true},
		{args: []string{"--account"}, fail: true},
		{args: []string{"--account="}, fail: true},
		{args: []string{"--account", "../escape"}, fail: true},
		{args: []string{"--account=a@b.com", "--account=c@d.com"}, fail: true},
	} {
		email, forward, help, err := runArgs(tc.args)
		if (err != nil) != tc.fail || (!tc.fail && (email != tc.email || help != tc.help || !reflect.DeepEqual(forward, tc.forward))) {
			t.Fatalf("%q: got %q %q %v %v", tc.args, email, forward, help, err)
		}
	}
}

// TestExtractRootResumeInvocation covers main.go's own root-level --resume/-r
// rewrite: only a literal leading --resume/-r/--resume= is ever recognized
// (matching runArgs' own "only a leading, recognized flag" discipline), and
// every other invocation — including a bare `cpro run --resume` (a real,
// pre-existing Claude argument forwarded through runArgs, see TestRunArgs
// above) and `cpro --help`/`--version` — passes through completely
// untouched.
func TestExtractRootResumeInvocation(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want []string
		ok   bool
	}{
		{args: []string{"--resume", "abc-123"}, want: []string{"__resume", "abc-123"}, ok: true},
		{args: []string{"-r", "abc-123"}, want: []string{"__resume", "abc-123"}, ok: true},
		{args: []string{"--resume=abc-123"}, want: []string{"__resume", "abc-123"}, ok: true},
		{args: []string{"--resume", "abc-123", "--account", "a@b.com"}, want: []string{"__resume", "abc-123", "--account", "a@b.com"}, ok: true},
		{args: []string{"--resume"}, want: []string{"__resume"}, ok: true},
		{args: []string{"run", "--resume"}, ok: false},
		{args: []string{"--help"}, ok: false},
		{args: []string{"--version"}, ok: false},
		{args: []string{"session", "continue", "a@b.com", "c@d.com", "--", "--resume", "abc"}, ok: false},
		{args: []string{}, ok: false},
	} {
		got, ok := extractRootResumeInvocation(tc.args)
		if ok != tc.ok {
			t.Fatalf("%q: ok=%v, want %v", tc.args, ok, tc.ok)
		}
		if ok && !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("%q: got %q, want %q", tc.args, got, tc.want)
		}
	}
}

// TestParseResumeArgs covers the hidden "__resume" command's own argument
// parser: the session ID always comes first, --account may appear before
// Claude's own arguments (mirroring runArgs), and -- explicitly ends cpro's
// own parsing, same as cpro run/cpro session continue.
func TestParseResumeArgs(t *testing.T) {
	for _, tc := range []struct {
		args               []string
		sessionID, account string
		forward            []string
		help, fail         bool
	}{
		{args: []string{"abc-123"}, sessionID: "abc-123"},
		{args: []string{"abc-123", "--account", "user@example.com"}, sessionID: "abc-123", account: "user@example.com"},
		{args: []string{"abc-123", "--account=user@example.com"}, sessionID: "abc-123", account: "user@example.com"},
		{args: []string{"abc-123", "--", "continue fixing tests"}, sessionID: "abc-123", forward: []string{"continue fixing tests"}},
		{args: []string{"abc-123", "--account", "user@example.com", "--", "continue"}, sessionID: "abc-123", account: "user@example.com", forward: []string{"continue"}},
		{args: []string{}},
		{args: []string{"--help"}, help: true},
		{args: []string{"abc-123", "--account"}, fail: true},
		{args: []string{"abc-123", "--account", "../escape"}, fail: true},
		{args: []string{"abc-123", "--account=x@y.com", "--account=z@y.com"}, fail: true},
	} {
		sessionID, account, forward, help, err := parseResumeArgs(tc.args)
		if (err != nil) != tc.fail {
			t.Fatalf("%q: err=%v, want fail=%v", tc.args, err, tc.fail)
		}
		if tc.fail {
			continue
		}
		if sessionID != tc.sessionID || account != tc.account || help != tc.help || !reflect.DeepEqual(forward, tc.forward) {
			t.Fatalf("%q: got sessionID=%q account=%q forward=%q help=%v, want sessionID=%q account=%q forward=%q help=%v",
				tc.args, sessionID, account, forward, help, tc.sessionID, tc.account, tc.forward, tc.help)
		}
	}
}

func TestUsage(t *testing.T) {
	profile := t.TempDir()
	credentials := `{"claudeAiOauth":{"accessToken":"secret-test-token"}}`
	if err := os.WriteFile(filepath.Join(profile, ".credentials.json"), []byte(credentials), 0600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer secret-test-token" || r.Header.Get("anthropic-beta") != "oauth-2025-04-20" {
			t.Error("missing OAuth headers")
		}
		fmt.Fprint(w, `{"five_hour":{"utilization":12.5,"resets_at":"2026-09-09T03:39:59Z"},"seven_day":{"utilization":47,"resets_at":"2026-09-15T11:59:59Z"}}`)
	}))

	usage, stale, err := loadUsage(profile, server.URL)
	if err != nil || stale || usage.FiveHour.Utilization != 12.5 || usage.SevenDay.Utilization != 47 {
		t.Fatalf("usage: %+v stale=%v err=%v", usage, stale, err)
	}
	if _, _, err := loadUsage(profile, server.URL); err != nil || calls != 1 {
		t.Fatalf("cache not used: calls=%d err=%v", calls, err)
	}
	cache, err := readUsageCache(filepath.Join(profile, "cpro-usage.json"))
	if err != nil {
		t.Fatal(err)
	}
	cache.FetchedAt = time.Now().Add(-2 * time.Minute)
	b, _ := json.Marshal(cache)
	if err := atomicWrite(filepath.Join(profile, "cpro-usage.json"), b); err != nil {
		t.Fatal(err)
	}
	server.Close()
	usage, stale, err = loadUsage(profile, server.URL)
	if err != nil || !stale || usage.SevenDay.Utilization != 47 {
		t.Fatalf("stale cache: %+v stale=%v err=%v", usage, stale, err)
	}
	if bar := usageBarWidth(&bytes.Buffer{}, -2, 20); strings.Contains(bar, "\x1b") || strings.Count(bar, "█") != 0 {
		t.Fatalf("usageBarWidth did not clamp a negative value to empty: %q", bar)
	}
	if bar := usageBarWidth(&bytes.Buffer{}, 120, 20); strings.Count(bar, "█") != 20 {
		t.Fatalf("usageBarWidth did not clamp an over-100 value to full: %q", bar)
	}
	if pct := pctText(-2); pct != "0%" {
		t.Fatalf("pctText did not clamp a negative value: %q", pct)
	}
	if pct := pctText(120); pct != "100%" {
		t.Fatalf("pctText did not clamp an over-100 value: %q", pct)
	}
	if _, ok := usageReset(""); ok {
		t.Fatal("usageReset should report false for a blank resets_at")
	}
	future := time.Now().Add(2 * time.Hour).Format(time.RFC3339Nano)
	reset, ok := usageReset(future)
	if !ok || formatCountdown(reset) == "" {
		t.Fatalf("usageReset: reset=%v ok=%v", reset, ok)
	}
}

// TestListRenderPrimitives exercises the pure helpers behind cpro list's
// full/compact/compact-narrow rendering (ui.go, main.go) directly, without a real
// terminal or account: color thresholds, ANSI-aware alignment/padding, path
// truncation, and the compact view's width-driven layout math.
func TestListRenderPrimitives(t *testing.T) {
	var buf bytes.Buffer // not *os.File, so accent()/usageBarWidth() never emit ANSI here.

	if got := usageColorFor(barWarnThreshold - 1); got != barColorSafe {
		t.Fatalf("usageColorFor: below warn threshold = %q, want safe %q", got, barColorSafe)
	}
	// Not hardcoded "#FB923C"/"#F87171": usageColorFor must use whatever
	// warningColor/dangerColor are actually configured to right now (default
	// Orange/Red — see colorPalette, ui.go), never its own fixed constant —
	// the requesting spec's own explicit "do not hardcode orange or red
	// anymore" requirement.
	if got := usageColorFor(barWarnThreshold); got != warningColor {
		t.Fatalf("usageColorFor: at warn threshold = %q, want the configured warningColor %q", got, warningColor)
	}
	if got := usageColorFor(barDangerThreshold); got != dangerColor {
		t.Fatalf("usageColorFor: at danger threshold = %q, want the configured dangerColor %q", got, dangerColor)
	}

	// Changing the configured Warning/Danger colors (as cpro config would)
	// must be reflected immediately, proving usageColorFor never fell back to
	// a hardcoded value under the hood.
	t.Run("usageColorFor reflects configured Warning/Danger colors, not fixed defaults", func(t *testing.T) {
		origWarn, origDanger := warningColor, dangerColor
		t.Cleanup(func() { warningColor, dangerColor = origWarn, origDanger })
		warningColor, dangerColor = "#FBBF24", "#8B5CF6" // Amber, Violet
		if got := usageColorFor(barWarnThreshold); got != "#FBBF24" {
			t.Fatalf("usageColorFor did not pick up the reconfigured warningColor: got %q", got)
		}
		if got := usageColorFor(barDangerThreshold); got != "#8B5CF6" {
			t.Fatalf("usageColorFor did not pick up the reconfigured dangerColor: got %q", got)
		}
		// Accent must stay completely independent of this — changing
		// Warning/Danger color must never move accentMode.
		if accentMode == "#FBBF24" || accentMode == "#8B5CF6" {
			t.Fatalf("changing warningColor/dangerColor must not affect accentMode, got %q", accentMode)
		}
	})

	if bar := usageBarWidth(&buf, 50, 16); strings.Count(bar, "█") != 8 || strings.Count(bar, "░") != 8 {
		t.Fatalf("usageBarWidth(50, 16): %q", bar)
	}
	if bar := usageGlyph(&buf, 0); bar != "█" {
		t.Fatalf("usageGlyph should always be one solid block regardless of value: %q", bar)
	}

	if got := padEnd("ab", 5); got != "ab   " {
		t.Fatalf("padEnd: %q", got)
	}
	if got := padEnd("abcdef", 3); got != "abcdef" {
		t.Fatalf("padEnd should not truncate: %q", got)
	}
	if got := truncatePath("/home/user/projects/my-app", 12); got != "…ects/my-app" {
		t.Fatalf("truncatePath: %q", got)
	}
	if got := truncatePath("/short", 12); got != "/short" {
		t.Fatalf("truncatePath should not touch a path that already fits: %q", got)
	}

	if got := fullRowWidth(0); got != fullRowDefault {
		t.Fatalf("fullRowWidth(0) should be the default width: %d", got)
	}
	if got := fullRowWidth(20); got != fullRowMin {
		t.Fatalf("fullRowWidth should clamp down to fullRowMin on a very narrow terminal: %d", got)
	}
	if got := fullRowWidth(200); got != fullRowDefault {
		t.Fatalf("fullRowWidth should cap at fullRowDefault on a wide terminal: %d", got)
	}
	// fullRowWidth now sizes the account card's total outer width (both rails/
	// corners included, decision 0033), not just the content row it used to —
	// a terminal narrower than fullRowDefault but still wider than fullRowMin
	// gets a card sized exactly to it, never wider than the real terminal.
	if got := fullRowWidth(60); got != 60 {
		t.Fatalf("fullRowWidth should track a terminal width between fullRowMin and fullRowDefault exactly: %d", got)
	}

	// compactBarCol locates the Week bar/glyph's column; the Total week row (see
	// renderCompactView) relies on this being identical whether or not the row
	// itself is rendered narrow, so every bar in the panel lines up vertically.
	if wide, narrow := compactBarCol(14, false), compactBarCol(14, true); wide == narrow {
		t.Fatalf("compactBarCol should differ between narrow and wide field widths: wide=%d narrow=%d", wide, narrow)
	}
	// 75/45, not the mockup's original 73/43: compactPctWidth is 4 (not 3), to fit
	// "100%" without throwing off alignment (see its doc comment).
	if got := compactRowWidth(14, false); got != 75 {
		t.Fatalf("compactRowWidth(14, wide) = %d, want 75", got)
	}
	if got := compactRowWidth(14, true); got != 45 {
		t.Fatalf("compactRowWidth(14, narrow) = %d, want 45", got)
	}
}

// TestStatusCardLayout exercises cpro status's full-view card layout
// (decision 0033, main.go) directly against renderFullView/
// newStatusCardLayout/statusUsageLine/statusSessionLine, with accounts
// carrying deliberately different data lengths — percentages spanning
// 0/9/35/84/100, runtimes from 12m to 17h 44m, a short vs. a very long
// (truncated) session path, a short vs. a long pid, an Expired account, an
// account with no usage data at all, and 3 vs. 1 vs. 0 sessions (covering
// singular/plural). No process or pty is needed: renderFullView never
// checks terminalOutput itself (that's the caller's job — see
// renderAccountSnapshot), so it can be driven directly against a
// bytes.Buffer, which also means accent()/usageBarWidth() never emit ANSI
// here — the same reasoning TestListRenderPrimitives already documents for
// itself.
func TestStatusCardLayout(t *testing.T) {
	origMask, origTable := maskEmailEnabled, emailMaskTable
	t.Cleanup(func() { maskEmailEnabled, emailMaskTable = origMask, origTable })
	maskEmailEnabled = false

	now := time.Now()
	resetIn := func(d time.Duration) string { return now.Add(d).Format(time.RFC3339Nano) }
	since := func(d time.Duration) time.Time { return now.Add(-d) }

	const emailA, emailB, emailC = "neku@quinel", "tafu@jovena", "unavailable@example.com"
	states := map[string]accountSnapshotState{
		// Percentages 84/88, three sessions with very different path
		// lengths (one deliberately long enough to force truncation), pid
		// widths (7 vs. 6 digits), and runtimes (1h 36m, 17h 44m, 12m).
		emailA: {
			authenticated: true,
			usage: accountUsage{
				FiveHour: usageWindow{Utilization: 84, ResetsAt: resetIn(2*time.Hour + 30*time.Minute)},
				SevenDay: usageWindow{Utilization: 88, ResetsAt: resetIn(3*time.Hour + 16*time.Minute)},
			},
			sessions: []runningSession{
				{PID: 1431805, Directory: "/home/user/workspace-claude-profiles", Since: since(96 * time.Minute)},
				{PID: 197761, Directory: "/home/user/claude-profiles", Since: since(17*time.Hour + 44*time.Minute)},
				{PID: 1941966, Directory: "/tmp/cc-daemon/host/spare/session/workspace/nested/deep/spare", Since: since(12 * time.Minute)},
			},
		},
		// The opposite ends of the percentage range (0%/100%), a
		// multi-day reset, an Expired auth state, and exactly one session
		// (singular "1 session"), with a short path/pid.
		emailB: {
			authenticated: false,
			usage: accountUsage{
				FiveHour: usageWindow{Utilization: 0, ResetsAt: resetIn(3*time.Hour + 30*time.Minute)},
				SevenDay: usageWindow{Utilization: 100, ResetsAt: resetIn(4*24*time.Hour + 15*time.Hour)},
			},
			sessions: []runningSession{
				{PID: 42, Directory: "/tmp/x", Since: since(2*time.Hour + 5*time.Minute)},
			},
		},
		// No usage data at all, and no running sessions (zero — plural).
		emailC: {authenticated: true, usageErr: fmt.Errorf("boom")},
	}
	emails := []string{emailA, emailB, emailC}

	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&buf)
	if err := renderFullView(cmd, &buf, 100, emails, states); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Contains(out, "\x1b") {
		t.Fatalf("bytes.Buffer output must never carry ANSI codes: %q", out)
	}

	layout := newStatusCardLayout(100, emails, states)
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")

	rail := currentTheme.Rail()
	var headers, bottoms, blanks []string
	usageRowsByAccount := map[string][]string{}
	sessionRowsByAccount := map[string][]string{}
	var totalWeekLine string
	currentAccount := ""
	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, currentTheme.TopLeft):
			headers = append(headers, line)
			for _, email := range emails {
				if strings.Contains(line, email) {
					currentAccount = email
				}
			}
		case strings.HasPrefix(line, currentTheme.BottomLeft):
			bottoms = append(bottoms, line)
			currentAccount = ""
		case strings.HasPrefix(line, "Total week"):
			totalWeekLine = line
		case strings.HasPrefix(line, rail):
			content := strings.TrimSuffix(strings.TrimPrefix(line, rail+" "), " "+rail)
			switch {
			case strings.HasPrefix(content, "S") || strings.HasPrefix(content, "W"):
				usageRowsByAccount[currentAccount] = append(usageRowsByAccount[currentAccount], content)
			case strings.HasPrefix(content, "·"):
				sessionRowsByAccount[currentAccount] = append(sessionRowsByAccount[currentAccount], content)
			case strings.TrimSpace(content) == "":
				blanks = append(blanks, line)
			}
		case line == "":
			blanks = append(blanks, line)
		}
	}

	// Every card's header/bottom/body line shares the exact same outer
	// width — "card right borders align".
	for _, line := range headers {
		if got := visibleWidth(line); got != layout.cardWidth {
			t.Fatalf("header width = %d, want cardWidth %d: %q", got, layout.cardWidth, line)
		}
		if !strings.HasSuffix(line, currentTheme.TopRight) {
			t.Fatalf("header must end with the theme's own top-right corner: %q", line)
		}
	}
	for _, line := range bottoms {
		if got := visibleWidth(line); got != layout.cardWidth {
			t.Fatalf("bottom width = %d, want cardWidth %d: %q", got, layout.cardWidth, line)
		}
		if !strings.HasSuffix(line, currentTheme.BottomRight) {
			t.Fatalf("bottom must end with the theme's own bottom-right corner: %q", line)
		}
	}

	// Singular vs. plural session count, and that Expired renders too — all
	// entirely in the header (no separate header line inside the card).
	if len(headers) != 3 {
		t.Fatalf("expected 3 card headers, got %d: %q", len(headers), headers)
	}
	mustContain := func(t *testing.T, haystack, want string) {
		t.Helper()
		if !strings.Contains(haystack, want) {
			t.Fatalf("expected %q in %q", want, haystack)
		}
	}
	allHeaders := strings.Join(headers, "\n")
	mustContain(t, allHeaders, "● Authenticated · 3 sessions")
	mustContain(t, allHeaders, "● Expired · 1 session ") // singular, not "1 sessions"
	mustContain(t, allHeaders, "● Authenticated · 0 sessions")
	if strings.Contains(allHeaders, "1 sessions") {
		t.Fatalf("singular session count must not say \"1 sessions\": %q", allHeaders)
	}

	// Usage rows: percentages right-aligned by ENDING position (the "%"
	// itself), and the reset separator "-" in the same column, across every
	// account that has usage data (emailC has none, and correctly
	// contributes no usage rows at all).
	if len(usageRowsByAccount[emailA]) != 2 || len(usageRowsByAccount[emailB]) != 2 {
		t.Fatalf("expected 2 usage rows (S, W) per account with usage data: %+v", usageRowsByAccount)
	}
	if rows := usageRowsByAccount[emailC]; len(rows) != 0 {
		t.Fatalf("an account with usageErr must render no S/W rows, got %+v", rows)
	}
	// Every content string here is plain, ANSI-free text, but it can still
	// carry multi-byte runes ("█"/"░"/"·"/"…") — index using runes, not raw
	// bytes, so a column position actually means a terminal column.
	runeIndex := func(rs []rune, target rune) int {
		for i, r := range rs {
			if r == target {
				return i
			}
		}
		return -1
	}
	runeLastIndex := func(rs []rune, target rune) int {
		last := -1
		for i, r := range rs {
			if r == target {
				last = i
			}
		}
		return last
	}

	var pctEndCol, dashCol = -1, -1
	for _, rows := range usageRowsByAccount {
		for _, content := range rows {
			rs := []rune(content)
			pct := runeIndex(rs, '%')
			dash := runeIndex(rs, '-')
			if pct < 0 || dash < 0 {
				t.Fatalf("usage row missing %% or -: %q", content)
			}
			if pctEndCol == -1 {
				pctEndCol, dashCol = pct, dash
			} else if pct != pctEndCol || dash != dashCol {
				t.Fatalf("usage row misaligned: %% at %d (want %d), - at %d (want %d): %q", pct, pctEndCol, dash, dashCol, content)
			}
		}
	}
	// Bar start column identical across every usage row too.
	barStartCol := layout.usageLabelWidth + 2
	for _, rows := range usageRowsByAccount {
		for _, content := range rows {
			rs := []rune(content)
			if rs[barStartCol] != '█' && rs[barStartCol] != '░' {
				t.Fatalf("bar does not start at the expected shared column %d: %q", barStartCol, content)
			}
		}
	}
	// 0%/100% themselves must still render correctly at the extremes.
	mustContain(t, strings.Join(usageRowsByAccount[emailB], "\n"), "0%")
	mustContain(t, strings.Join(usageRowsByAccount[emailB], "\n"), "100%")

	// Session rows: leading "·" marker and path both start in the same
	// column; the trailing "·" runtime separator lands in the same column
	// across every session row (short and long paths/pids/runtimes alike);
	// a long path truncates instead of wrapping; every row is exactly
	// contentWidth (no row exceeds the card, none wrap).
	if len(sessionRowsByAccount[emailA]) != 3 || len(sessionRowsByAccount[emailB]) != 1 {
		t.Fatalf("expected 3 session rows for %s and 1 for %s, got %+v", emailA, emailB, sessionRowsByAccount)
	}
	if rows := sessionRowsByAccount[emailC]; len(rows) != 0 {
		t.Fatalf("an account with no running sessions must render no session rows, got %+v", rows)
	}
	sepCol := -1
	allSessionRows := append(append([]string{}, sessionRowsByAccount[emailA]...), sessionRowsByAccount[emailB]...)
	for _, content := range allSessionRows {
		rs := []rune(content)
		if rs[0] != '·' || rs[1] != ' ' {
			t.Fatalf("session row must start with \"· \": %q", content)
		}
		sep := runeLastIndex(rs, '·') // the second "·" (index 0 is the row marker)
		if sep <= 0 {
			t.Fatalf("session row missing its trailing \"·\" runtime separator: %q", content)
		}
		if sepCol == -1 {
			sepCol = sep
		} else if sep != sepCol {
			t.Fatalf("session runtime separator misaligned: got column %d, want %d: %q", sep, sepCol, content)
		}
		if got := visibleWidth(content); got != layout.contentWidth {
			t.Fatalf("session row content width = %d, want contentWidth %d (a row must never wrap): %q", got, layout.contentWidth, content)
		}
	}
	longPathRow := sessionRowsByAccount[emailA][2] // the deliberately very long path
	if !strings.Contains(longPathRow, "…") {
		t.Fatalf("a path longer than the card's path column must truncate with \"…\": %q", longPathRow)
	}
	shortPIDRow := sessionRowsByAccount[emailB][0]
	mustContain(t, shortPIDRow, "42")
	mustContain(t, strings.Join(sessionRowsByAccount[emailA], "\n"), "1431805")

	// Runtimes: 12m/1h 36m/17h 44m/2h 5m all present, right-aligned to the
	// same trailing edge (every row content is already asserted the same
	// width above, and the runtime text is always its last characters).
	for email, want := range map[string][]string{emailA: {"1h 36m", "17h 44m", "12m"}, emailB: {"2h 5m"}} {
		joined := strings.Join(sessionRowsByAccount[email], "\n")
		for _, w := range want {
			mustContain(t, joined, w)
		}
	}

	// Exactly one blank line between usage and sessions, between cards, and
	// before Total week; none anywhere else (e.g. never right before a
	// bottom border, and emailC — no usage rows, no sessions — has no
	// internal blank row at all).
	if len(blanks) == 0 {
		t.Fatal("expected at least the blank separators between usage/sessions, cards, and Total week")
	}

	// Total week lives entirely outside every card: not rail-prefixed, and
	// its own percentage is the average of the accounts with valid usage
	// data (emailA 88%, emailB 100%; emailC excluded — 94%).
	if totalWeekLine == "" {
		t.Fatal("expected a Total week line")
	}
	if strings.HasPrefix(totalWeekLine, rail) {
		t.Fatalf("Total week must never be boxed inside a card: %q", totalWeekLine)
	}
	mustContain(t, totalWeekLine, "94%")

	// No outer "Accounts" container wraps the cards, and no "├─" divider
	// between them (decision 0033 — every account is its own independent
	// card).
	if strings.Contains(out, "Accounts") {
		t.Fatalf("full view must not wrap cards in an outer \"Accounts\" panel: %q", out)
	}
	if strings.Contains(out, currentTheme.Divider()) {
		t.Fatalf("full view must not draw a divider between cards: %q", out)
	}
}

func TestRunningSessions(t *testing.T) {
	profile := t.TempDir()
	if got := runningSessions(profile); len(got) != 0 {
		t.Fatalf("expected no sessions before any process: %+v", got)
	}

	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	var unlocked int32
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlocked))); errno != 0 {
		t.Fatal(errno)
	}
	var number uint32
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), syscall.TIOCGPTN, uintptr(unsafe.Pointer(&number))); errno != 0 {
		t.Fatal(errno)
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", number), os.O_RDWR|syscall.O_NOCTTY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer slave.Close()

	// A process with CLAUDE_CONFIG_DIR set but no tty (the default here, stdio
	// unset) must not be reported: it stands in for claude's own background
	// daemon/bg-pty-host/bg-spare helpers, which carry the same env var.
	headless := exec.Command("sleep", "5")
	headless.Env = append(os.Environ(), "CLAUDE_CONFIG_DIR="+profile)
	if err := headless.Start(); err != nil {
		t.Fatal(err)
	}
	defer headless.Process.Kill()
	time.Sleep(100 * time.Millisecond)
	if got := runningSessions(profile); len(got) != 0 {
		t.Fatalf("headless process must not be reported as a session: %+v", got)
	}
	if err := headless.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = headless.Wait()

	wantDir := t.TempDir()
	cmd := exec.Command("sleep", "5")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.Dir = wantDir
	cmd.Env = append(os.Environ(), "CLAUDE_CONFIG_DIR="+profile)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()

	var sessions []runningSession
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		sessions = runningSessions(profile)
		if len(sessions) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(sessions) != 1 || sessions[0].PID != cmd.Process.Pid {
		t.Fatalf("expected exactly the tty-attached process, got %+v (pid %d)", sessions, cmd.Process.Pid)
	}
	gotDir, err := filepath.EvalSymlinks(sessions[0].Directory)
	if err != nil {
		t.Fatal(err)
	}
	realWantDir, err := filepath.EvalSymlinks(wantDir)
	if err != nil {
		t.Fatal(err)
	}
	if gotDir != realWantDir {
		t.Fatalf("directory: got %q, want %q", gotDir, realWantDir)
	}
	if since := sessions[0].Since; since.IsZero() || time.Since(since) > time.Minute || time.Since(since) < 0 {
		t.Fatalf("implausible start time: %v", since)
	}

	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(runningSessions(profile)) == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("session still reported after the process exited")
}

func TestMaintenance(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	if err := checkNetwork(server.URL); err != nil {
		t.Fatal(err)
	}
	server.Close()
	if err := checkNetwork(server.URL); err == nil {
		t.Fatal("expected network failure")
	}

	dir := t.TempDir()
	source, target := filepath.Join(dir, "source"), filepath.Join(dir, "bin", "cpro")
	if err := os.WriteFile(source, []byte("cpro-test"), 0755); err != nil {
		t.Fatal(err)
	}
	installed, err := installExecutable(source, target)
	if err != nil || !installed {
		t.Fatalf("install: installed=%v err=%v", installed, err)
	}
	b, err := os.ReadFile(target)
	if err != nil || string(b) != "cpro-test" {
		t.Fatalf("installed content: %q err=%v", b, err)
	}
	info, _ := os.Stat(target)
	if info.Mode().Perm() != 0755 {
		t.Fatalf("installed mode: %o", info.Mode().Perm())
	}
	installed, err = installExecutable(target, target)
	if err != nil || installed {
		t.Fatalf("same-file install: installed=%v err=%v", installed, err)
	}
}

// The helper stands in for the external Claude executable, never real accounts.
func TestClaudeProcess(t *testing.T) {
	if os.Getenv("CPRO_TEST_HELPER") != "1" {
		return
	}
	args := os.Args
	for i, arg := range args {
		if arg == "--" {
			args = args[i+1:]
			break
		}
	}
	dir := os.Getenv("CLAUDE_CONFIG_DIR")
	// claudeSupportsFlag (permissions.go) is the only caller that ever runs a
	// bare "claude --help" with no CLAUDE_CONFIG_DIR set — it explicitly
	// clears that var itself, since it's a pure capability check with no
	// account context. Every other "--help" this fake process ever sees
	// (e.g. a literal Claude argument forwarded via "cpro run --account EMAIL
	// -- --help") goes through claudeCommand, which always sets
	// CLAUDE_CONFIG_DIR — so dir == "" reliably means "this is the
	// capability probe" without needing a separate test-only signal.
	if len(args) == 1 && args[0] == "--help" && dir == "" {
		// Real claude --help lists every supported flag; requireYOLOSupport
		// greps this output for "--permission-mode" and "bypassPermissions"
		// (decision 0038, superseding 0026's own "--dangerously-skip-permissions"/
		// "--add-dir" pair). CPRO_TEST_NO_YOLO_SUPPORT simulates an installed
		// version whose --permission-mode predates the bypassPermissions
		// value — the flag itself is present, but that one choice is missing
		// from its own list — the same "flag exists, value doesn't yet"
		// version gap Claude Code's own docs note ("--permission-mode ahora
		// también acepta bypassPermissions"), without needing a second fake
		// binary. The "supported" branch mirrors the real, live --help output
		// (2.1.268) verbatim: --permission-mode's choices listed inline,
		// bypassPermissions included.
		if os.Getenv("CPRO_TEST_NO_YOLO_SUPPORT") == "1" {
			fmt.Println(`Usage: claude [options]` + "\n" + `  --permission-mode <mode>  Permission mode to use for the session (choices: "acceptEdits", "auto", "manual", "dontAsk", "plan")`)
		} else {
			fmt.Println(`Usage: claude [options]` + "\n" + `  --permission-mode <mode>  Permission mode to use for the session (choices: "acceptEdits", "auto", "bypassPermissions", "manual", "dontAsk", "plan")`)
		}
		os.Exit(0)
	}
	if len(args) == 1 && args[0] == "--version" {
		fmt.Println("2.1.265 (Claude Code)")
		os.Exit(0)
	}
	if len(args) == 1 && args[0] == "doctor" {
		// requireNoManagedPermissionsPolicy (permissions.go, decision 0034)
		// greps this line the same way a real `claude doctor` prints it.
		// CPRO_TEST_MANAGED_POLICY simulates an account under an
		// enterprise/organization policy (the "fetched" case this codebase
		// has no real account to test against — see the decision record);
		// the default matches a real, policy-free Pro account observed live.
		if os.Getenv("CPRO_TEST_MANAGED_POLICY") == "1" {
			fmt.Println("Managed settings (remote): fetched")
		} else {
			fmt.Println("Managed settings (remote): not fetched — requires an Enterprise or Team subscription")
		}
		os.Exit(0)
	}
	if len(args) >= 2 && args[0] == "auth" {
		switch args[1] {
		case "login":
			email := args[len(args)-1]
			if os.Getenv("CPRO_TEST_WRONG_ACCOUNT") == "1" {
				email = "wrong@example.com"
			}
			_ = os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(fmt.Sprintf(`{"loggedIn":true,"email":%q,"authMethod":"claude.ai"}`, email)), 0600)
			if _, err := os.Stat(filepath.Join(dir, ".claude.json")); err != nil {
				_ = os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(`{"theme":"dark"}`), 0600)
			}
			if os.Getenv("CPRO_TEST_LOGIN_FAIL") == "1" {
				os.Exit(1)
			}
			os.Exit(0)
		case "status":
			b, err := os.ReadFile(filepath.Join(dir, ".credentials.json"))
			if err != nil {
				fmt.Println(`{"loggedIn":false,"authMethod":"none"}`)
				os.Exit(1)
			}
			fmt.Println(string(b))
			os.Exit(0)
		case "logout":
			_ = os.Remove(filepath.Join(dir, ".credentials.json"))
			os.Exit(0)
		}
	}
	if os.Getenv("CPRO_TEST_WAIT") == "1" {
		_ = os.WriteFile(os.Getenv("CPRO_TEST_READY"), []byte("ready"), 0600)
		for {
			time.Sleep(time.Second)
		}
	}
	_ = json.NewEncoder(os.Stdout).Encode(struct {
		Args      []string
		Directory string
	}{args, dir})
	fmt.Fprintln(os.Stderr, "claude stderr")
	if os.Getenv("CPRO_TEST_EXIT") == "7" {
		os.Exit(7)
	}
	os.Exit(0)
}

func TestCLI(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "cpro")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %s %v", out, err)
	}
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("NO_COLOR", "1")
	for _, pair := range os.Environ() {
		key := strings.SplitN(pair, "=", 2)[0]
		if strings.HasPrefix(key, "ANTHROPIC_") || strings.HasPrefix(key, "CLAUDE_CODE_") {
			t.Setenv(key, "")
		}
	}
	helper, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CPRO_TEST_HELPER", "1")
	t.Setenv("CPRO_TEST_BINARY", helper)
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte("#!/bin/sh\nexec \"$CPRO_TEST_BINARY\" -test.run='^TestClaudeProcess$' -- \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	run := func(code int, args ...string) (string, string) {
		t.Helper()
		cmd := exec.Command(bin, args...)
		var out, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &stderr
		err := cmd.Run()
		got := 0
		if err != nil {
			if e, ok := err.(*exec.ExitError); ok {
				got = e.ExitCode()
			} else {
				t.Fatal(err)
			}
		}
		if got != code {
			t.Fatalf("%q: exit %d, want %d; stdout %s stderr %s", args, got, code, &out, &stderr)
		}
		if strings.Contains(out.String(), "\x1b") || strings.Contains(stderr.String(), "\x1b") {
			t.Fatalf("escape sequences in redirected output: %q", args)
		}
		return out.String(), stderr.String()
	}
	for _, args := range [][]string{{"--help"}, {"doctor", "--help"}, {"install", "--help"}, {"login", "--help"}, {"list", "--help"}, {"ls", "--help"}, {"status", "--help"}, {"menu", "--help"}, {"info", "--help"}, {"logout", "--help"}, {"remove", "--help"}, {"run", "--help"}, {"default", "--help"}} {
		out, stderr := run(0, args...)
		if !strings.Contains(strings.ToLower(out), "usage") || stderr != "" || strings.Contains(out, "\x1b") {
			t.Fatalf("help streams: %q %q", out, stderr)
		}
	}
	for _, args := range [][]string{{"login"}, {"wat"}, {"run", "--account"}, {"login", "user@example.com"}, {"run"}} {
		run(1, args...)
	}
	// cpro default has no meaningful non-interactive rendering of a live
	// mode/account-picker screen, unlike cpro config (which falls back to a
	// plain-text summary) — outside a real terminal it's a hard error naming
	// the fallback instead.
	if _, stderr := run(1, "default"); !strings.Contains(stderr, "interactive terminal") || !strings.Contains(stderr, "cpro config") {
		t.Fatalf("expected cpro default to fail clearly outside a terminal, got stderr %q", stderr)
	}
	out, _ := run(0, "ls", "--json")
	if !strings.Contains(out, `"accounts":[]`) {
		t.Fatalf("empty list: %s", out)
	}
	s, err := openStore()
	if err != nil {
		t.Fatal(err)
	}
	seed := func(email string) {
		t.Helper()
		stage := t.TempDir()
		data := fmt.Sprintf(`{"loggedIn":true,"email":%q,"authMethod":"claude.ai"}`, email)
		for name, content := range map[string]string{".credentials.json": data, ".claude.json": `{"theme":"dark"}`} {
			if err := os.WriteFile(filepath.Join(stage, name), []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.installLogin(email, stage); err != nil {
			t.Fatal(err)
		}
	}
	seed("a@example.com")
	seed("b@example.com")
	// A pseudoterminal exercises the actual browser-login wrapper with the fake
	// Claude process, including identity validation and failed reauthentication.
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	var unlocked int32
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlocked))); errno != 0 {
		t.Fatal(errno)
	}
	var number uint32
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), syscall.TIOCGPTN, uintptr(unsafe.Pointer(&number))); errno != 0 {
		t.Fatal(errno)
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", number), os.O_RDWR|syscall.O_NOCTTY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer slave.Close()
	// Every interactive command below (login, the account/mode pickers) writes
	// its stderr to this same pty — draining it from the start is what lets
	// the mode-picker assertions further down prove no clipboard message
	// (success or failure) was ever printed, not just that stdout looks right.
	ptyCapture := drainPTY(master)
	login := func(wantSuccess bool, email string) {
		t.Helper()
		cmd := exec.Command(bin, "login", email)
		cmd.Stdin = slave
		out, err := cmd.CombinedOutput()
		if (err == nil) != wantSuccess {
			t.Fatalf("login: %s %v", out, err)
		}
	}
	login(true, "new@example.com")
	oldAuth, _ := os.ReadFile(filepath.Join(s.profile("a@example.com"), ".credentials.json"))
	t.Setenv("CPRO_TEST_WRONG_ACCOUNT", "1")
	login(false, "a@example.com")
	t.Setenv("CPRO_TEST_WRONG_ACCOUNT", "")
	t.Setenv("CPRO_TEST_LOGIN_FAIL", "1")
	login(false, "a@example.com")
	t.Setenv("CPRO_TEST_LOGIN_FAIL", "")
	newAuth, _ := os.ReadFile(filepath.Join(s.profile("a@example.com"), ".credentials.json"))
	if !bytes.Equal(oldAuth, newAuth) {
		t.Fatal("failed login changed existing authentication")
	}
	login(true, "a@example.com")
	out, stderr := run(0, "ls", "--json")
	if stderr != "" || !strings.Contains(out, `"default":"a@example.com"`) {
		t.Fatalf("list: %s %s", out, stderr)
	}
	// use is gone; set Default directly to exercise list --json's schema (its
	// own concern, independent of how a default ever gets set).
	if err := s.update(func(c *config) error { c.Default = "b@example.com"; return nil }); err != nil {
		t.Fatal(err)
	}
	out, _ = run(0, "list", "--json")
	var listed struct {
		Version  int
		Default  string
		Accounts []struct {
			Email         string
			Default       bool
			Authenticated bool
		}
	}
	if err := json.Unmarshal([]byte(out), &listed); err != nil {
		t.Fatalf("list --json: %s: %v", out, err)
	}
	if listed.Default != "b@example.com" {
		t.Fatalf("list --json default: %+v", listed)
	}
	byEmail := map[string]bool{}
	for _, a := range listed.Accounts {
		byEmail[a.Email] = a.Authenticated
		if a.Default != (a.Email == "b@example.com") {
			t.Fatalf("list --json default flag: %+v", a)
		}
	}
	if !byEmail["a@example.com"] || !byEmail["b@example.com"] || !byEmail["new@example.com"] {
		t.Fatalf("list --json authenticated: %+v", listed)
	}
	if summary, _ := run(0, "list"); !strings.Contains(summary, "Authenticated") {
		t.Fatalf("list summary: %s", summary)
	}
	// cpro run never prompts, in a terminal or not (decision 0019): with no
	// --account and no configured default it fails clearly, rather than
	// guessing or opening a picker.
	if err := s.update(func(c *config) error { c.Default = ""; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, stderr := run(1, "run", "--dangerously-skip-permissions"); !strings.Contains(stderr, "no default account configured") {
		t.Fatalf("expected a clear no-default-account error, got stderr %q", stderr)
	}

	// Once a default account is configured, a flagless cpro run resolves to
	// it — never a picker, never a guess.
	if err := s.update(func(c *config) error { c.Default = "b@example.com"; return nil }); err != nil {
		t.Fatal(err)
	}
	out, _ = run(0, "run", "--dangerously-skip-permissions")
	var defaultForward struct {
		Args      []string
		Directory string
	}
	if err := json.Unmarshal([]byte(out), &defaultForward); err != nil {
		t.Fatalf("cpro run (default account): %s: %v", out, err)
	}
	if defaultForward.Directory != s.profile("b@example.com") {
		t.Fatalf("expected the default account (b@example.com) to run, got directory %q", defaultForward.Directory)
	}

	// An explicit --account overrides the configured default for that one
	// invocation only — it's never written back as a new default.
	out, _ = run(0, "run", "--account", "new@example.com", "--dangerously-skip-permissions")
	var explicitForward struct {
		Args      []string
		Directory string
	}
	if err := json.Unmarshal([]byte(out), &explicitForward); err != nil {
		t.Fatalf("cpro run (explicit --account): %s: %v", out, err)
	}
	if explicitForward.Directory != s.profile("new@example.com") {
		t.Fatalf("expected --account to override the default, got directory %q", explicitForward.Directory)
	}
	if after, err := s.read(); err != nil || after.Default != "b@example.com" {
		t.Fatalf("explicit --account must never persist as a new default: got %+v (err %v)", after, err)
	}

	// cpro run must never touch the clipboard or mention it — decision 0016,
	// still true now that it's unconditionally non-interactive (the pty
	// captured every interactive command above, login included).
	if got := strings.ToLower(ptyCapture()); strings.Contains(got, "clipboard") {
		t.Fatalf("cpro run must never touch the clipboard or mention it, got stderr: %s", ptyCapture())
	}

	// cpro config reports and toggles the folder-trust auto-accept preference.
	out, _ = run(0, "config", "--json")
	if !strings.Contains(out, `"autoTrust":false`) {
		t.Fatalf("config default: %s", out)
	}
	run(0, "config", "trust", "on")
	out, _ = run(0, "config", "--json")
	if !strings.Contains(out, `"autoTrust":true`) {
		t.Fatalf("config after trust on: %s", out)
	}
	if summary, _ := run(0, "config"); !strings.Contains(summary, "Auto-trust:   on") {
		t.Fatalf("config summary: %s", summary)
	}
	// Auto-trust lives inside SET DEFAULT ACCOUNT, as "Trust working
	// directory" — its own top-level cpro default command (mirroring cpro
	// config), so this opens that directly rather than navigating into it
	// from Settings: the Workspace trust row leads (row 0), one row above
	// the current permission mode's own row, so a single Up reaches it, and
	// Enter there toggles it directly (no secondary confirm screen). A
	// direct invocation like this is now the stack's own root with no
	// parent, which arms a double-Esc-to-exit rather than quitting on the
	// first press.
	toggleCtx, toggleCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer toggleCancel()
	toggle := exec.CommandContext(toggleCtx, bin, "default")
	toggle.Stdin, toggle.Stderr = slave, slave
	if err := toggle.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if _, err := master.WriteString("\x1b[A"); err != nil { // ask -> Trust working directory (row 0)
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if _, err := master.WriteString("\r"); err != nil { // toggle trust off
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	for range 2 { // arm, then confirm — decision 0023
		if _, err := master.WriteString("\x1b"); err != nil {
			t.Fatal(err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err := toggle.Wait(); err != nil {
		t.Fatalf("interactive default: %v", err)
	}
	out, _ = run(0, "config", "--json")
	if !strings.Contains(out, `"autoTrust":false`) {
		t.Fatalf("interactive config did not disable auto-trust: %s", out)
	}
	run(0, "config", "trust", "on")
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	claudeJSON := filepath.Join(s.profile("a@example.com"), ".claude.json")
	type project struct {
		HasTrustDialogAccepted bool     `json:"hasTrustDialogAccepted"`
		AllowedTools           []string `json:"allowedTools,omitempty"`
	}
	type claudeConfig struct {
		Projects map[string]project `json:"projects,omitempty"`
	}
	// A directory Claude Code has never seen for this account still gets a
	// minimal fabricated project entry now — verified live against a real
	// Claude Code install that this doesn't break its startup (markTrusted's
	// own doc comment) — so YOLO's "zero prompts" guarantee actually holds on
	// a brand-new directory too, not just one Claude has already created an
	// entry for.
	run(0, "run", "--account", "a@example.com", "--", "--help")
	var fabricated claudeConfig
	if b, err := os.ReadFile(claudeJSON); err != nil || json.Unmarshal(b, &fabricated) != nil {
		t.Fatal(err)
	}
	if !fabricated.Projects[cwd].HasTrustDialogAccepted {
		t.Fatalf("expected auto-trust to fabricate a minimal trusted entry for a directory claude never created: %+v", fabricated)
	}
	// Once Claude Code has created a real entry (simulated here), auto-trust marks
	// it without disturbing its other fields.
	seeded := claudeConfig{Projects: map[string]project{cwd: {AllowedTools: []string{"Bash"}}}}
	b, err := json.Marshal(seeded)
	if err != nil {
		t.Fatal(err)
	}
	if err := atomicWrite(claudeJSON, b); err != nil {
		t.Fatal(err)
	}
	run(0, "run", "--account", "a@example.com", "--", "--help")
	var trusted claudeConfig
	trustData, err := os.ReadFile(claudeJSON)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(trustData, &trusted); err != nil {
		t.Fatal(err)
	}
	if !trusted.Projects[cwd].HasTrustDialogAccepted || !reflect.DeepEqual(trusted.Projects[cwd].AllowedTools, []string{"Bash"}) {
		t.Fatalf("auto-trust did not mark the existing entry for %s, or dropped other fields: %s", cwd, trustData)
	}
	run(0, "config", "trust", "off")
	t.Setenv("CLAUDE_CONFIG_DIR", "/must-not-be-used")
	out, stderr = run(0, "run", "--account", "a@example.com", "--dangerously-skip-permissions", "-p", "spaces ; $(literal)")
	var forwarded struct {
		Args      []string
		Directory string
	}
	if err := json.Unmarshal([]byte(out), &forwarded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(forwarded.Args, []string{"--dangerously-skip-permissions", "-p", "spaces ; $(literal)"}) || forwarded.Directory != s.profile("a@example.com") || stderr != "claude stderr\n" {
		t.Fatalf("forwarding: %s %s", out, stderr)
	}
	out, _ = run(0, "run", "--account", "b@example.com", "--", "--help")
	if !strings.Contains(out, `"--help"`) {
		t.Fatal(out)
	}
	c, _ := s.read()
	if c.Default != "b@example.com" {
		t.Fatal("run changed default")
	}
	t.Setenv("CPRO_TEST_EXIT", "7")
	run(7, "run", "--account", "b@example.com")
	t.Setenv("CPRO_TEST_EXIT", "")
	t.Setenv("ANTHROPIC_API_KEY", "never-print-this")
	_, stderr = run(1, "run", "--account", "b@example.com")
	if strings.Contains(stderr, "never-print-this") || !strings.Contains(stderr, "ANTHROPIC_API_KEY") {
		t.Fatal(stderr)
	}
	t.Setenv("ANTHROPIC_API_KEY", "")
	// A live run keeps the account locked and receives termination directly.
	t.Setenv("CPRO_TEST_WAIT", "1")
	ready := filepath.Join(dir, "ready")
	t.Setenv("CPRO_TEST_READY", ready)
	child := exec.Command(bin, "run", "--account", "b@example.com")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer child.Process.Kill()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("run did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// A live run with no tty (as this one has, by not setting Stdin) is real and
	// holding the account lock, but is not itself a reported session: it stands in
	// for a headless/scripted cpro run, not an interactive one. Sessions are
	// status's own concern now (list dropped them — see TestListLightweight).
	out, _ = run(0, "status", "--json")
	if !strings.Contains(out, `"email":"b@example.com","default":true,"authenticated":true,"sessions":[]`) {
		t.Fatalf("headless live run reported as a session: %s", out)
	}
	if _, stderr := run(1, "logout", "b@example.com"); !strings.Contains(stderr, "running Claude session") {
		t.Fatalf("expected logout refused by the live session, got %q", stderr)
	}
	run(1, "remove", "b@example.com", "--yes")
	if err := child.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := child.Wait(); err == nil {
		t.Fatal("expected termination")
	}
	if ws := child.ProcessState.Sys().(syscall.WaitStatus); !ws.Signaled() || ws.Signal() != syscall.SIGTERM {
		t.Fatalf("signal not preserved: %v", ws)
	}
	t.Setenv("CPRO_TEST_WAIT", "")
	// What a session leaves behind — Claude's background helpers, MCP servers,
	// jobs it started — carries the same CLAUDE_CONFIG_DIR and session tag but
	// is not the tagged process itself, so it must not block the account
	// (decision 0064). Previously they inherited the account lock and did.
	leftover := exec.Command("sleep", "30")
	leftover.Env = append(os.Environ(), "CLAUDE_CONFIG_DIR="+(&store{dir: filepath.Join(dir, "config", "cpro")}).profile("b@example.com"), cproSessionPIDEnv+"=1")
	if err := leftover.Start(); err != nil {
		t.Fatal(err)
	}
	defer leftover.Process.Kill()
	run(1, "remove", "a@example.com")
	run(0, "logout", "b@example.com")
	out, _ = run(0, "list", "--json")
	listed = struct {
		Version  int
		Default  string
		Accounts []struct {
			Email         string
			Default       bool
			Authenticated bool
		}
	}{}
	if err := json.Unmarshal([]byte(out), &listed); err != nil {
		t.Fatalf("list --json: %s: %v", out, err)
	}
	byEmail = map[string]bool{}
	for _, a := range listed.Accounts {
		byEmail[a.Email] = a.Authenticated
	}
	if byEmail["b@example.com"] || !byEmail["a@example.com"] {
		t.Fatalf("list --json after logout: %+v", listed)
	}
	if summary, _ := run(0, "list"); !strings.Contains(summary, "Signed out") {
		t.Fatalf("list summary after logout: %s", summary)
	}
	if summary, _ := run(0, "status"); !strings.Contains(summary, "Expired") {
		t.Fatalf("status summary after logout: %s", summary)
	}
	run(1, "run")
	run(0, "remove", "b@example.com", "--yes")
	c, _ = s.read()
	if c.Default != "" || c.Accounts["b@example.com"] {
		t.Fatal("remove did not clear default")
	}
	// Incomplete staged auth must not change an existing account.
	before, _ := os.ReadFile(filepath.Join(s.profile("a@example.com"), ".credentials.json"))
	stage := t.TempDir()
	_ = os.WriteFile(filepath.Join(stage, ".claude.json"), []byte(`{}`), 0600)
	if err := s.installLogin("a@example.com", stage); err == nil {
		t.Fatal("expected incomplete login failure")
	}
	after, _ := os.ReadFile(filepath.Join(s.profile("a@example.com"), ".credentials.json"))
	if !bytes.Equal(before, after) {
		t.Fatal("failed login overwrote credentials")
	}
	for path, mode := range map[string]os.FileMode{s.dir: 0700, s.profile("a@example.com"): 0700, filepath.Join(s.dir, "config.json"): 0600, filepath.Join(s.profile("a@example.com"), ".credentials.json"): 0600} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("permissions %s: %v %v", path, info, err)
		}
	}
	// Corrupt configuration must fail instead of silently resetting the registry.
	if err := os.WriteFile(filepath.Join(s.dir, "config.json"), []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	run(1, "ls")
}

// buildCLI builds cpro into a temp dir, points PATH at a fake `claude` (see
// TestClaudeProcess) that re-invokes this test binary, and isolates the account
// store under a temp XDG_CONFIG_HOME — the setup every test below needs, factored
// out of TestCLI's own copy of the same steps (left alone, to avoid touching an
// already-passing test) since several more tests need it too.
func buildCLI(t *testing.T) (bin string, s *store) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "cpro")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %s %v", out, err)
	}
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	for _, pair := range os.Environ() {
		key := strings.SplitN(pair, "=", 2)[0]
		if strings.HasPrefix(key, "ANTHROPIC_") || strings.HasPrefix(key, "CLAUDE_CODE_") {
			t.Setenv(key, "")
		}
	}
	// CLAUDE_CONFIG_DIR must be isolated too — and pointed somewhere
	// harmless rather than merely cleared. systemConfigPaths (claude.go)
	// honors it when set and falls back to the machine's real
	// ~/.claude.json + ~/.claude/.credentials.json when it isn't, so an
	// empty value would aim `cpro system export` at the developer's own
	// system-wide Claude Code credentials instead of away from them.
	//
	// This matters because cpro is developed *under* cpro: the variable
	// normally arrives already pointing at a real, logged-in account
	// profile. `cpro system export` writes credentials to whatever it
	// names, so every test exercising export (TestClaudeDesktopCredentialTarget,
	// TestEmailMaskingGlobal's own export picker) silently overwrote the
	// developer's live session and signed them out on each full `go test`
	// run. Isolating it here, at the one place every test builds its
	// environment, is what keeps that true for future tests too — the three
	// tests that need a specific system directory still set their own
	// afterwards, overriding this.
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(dir, "system"))
	helper, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CPRO_TEST_HELPER", "1")
	t.Setenv("CPRO_TEST_BINARY", helper)
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte("#!/bin/sh\nexec \"$CPRO_TEST_BINARY\" -test.run='^TestClaudeProcess$' -- \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	s, err = openStore()
	if err != nil {
		t.Fatal(err)
	}
	return bin, s
}

// openPTY opens a pseudoterminal pair for a test that drives an interactive
// picker, sized so huh's Select actually renders (confirmed live, building this
// session: without a window size, a pty defaults to 0x0 and Select's viewport
// renders nothing — though key handling still works, which is why TestCLI's own
// pty use above gets away without sizing it; a test that checks on-screen content,
// like the ones below, needs the size).
func openPTY(t *testing.T) (master, slave *os.File) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { master.Close() })
	var unlocked int32
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlocked))); errno != 0 {
		t.Fatal(errno)
	}
	var number uint32
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), syscall.TIOCGPTN, uintptr(unsafe.Pointer(&number))); errno != 0 {
		t.Fatal(errno)
	}
	slave, err = os.OpenFile(fmt.Sprintf("/dev/pts/%d", number), os.O_RDWR|syscall.O_NOCTTY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { slave.Close() })
	resizePTY(t, slave, 40, 120)
	return master, slave
}

// resizePTY re-applies slave's pty window size (see openPTY, fixed at 40x120) —
// used by tests that need a specific column width to exercise a responsive,
// terminal-width-driven layout (e.g. cpro list's full/compact/compact-narrow
// views, see outputWidth).
func resizePTY(t *testing.T, slave *os.File, rows, cols uint16) {
	t.Helper()
	type winsize struct{ Row, Col, Xpixel, Ypixel uint16 }
	ws := winsize{Row: rows, Col: cols}
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, slave.Fd(), syscall.TIOCSWINSZ, uintptr(unsafe.Pointer(&ws))); errno != 0 {
		t.Fatal(errno)
	}
}

// drainPTY continuously copies everything read from master into a buffer, and
// returns a function that reports what's been captured so far — safe to call
// after the driven process has exited or been killed, once its output has had a
// moment to arrive.
func drainPTY(master *os.File) func() string {
	var buf bytes.Buffer
	var mu sync.Mutex
	go func() {
		b := make([]byte, 4096)
		for {
			n, err := master.Read(b)
			if n > 0 {
				mu.Lock()
				buf.Write(b[:n])
				mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		return buf.String()
	}
}

// ansiRe strips ANSI/CSI/OSC escape sequences (color, cursor movement, terminal
// titles, Kitty keyboard protocol markers) from captured pty output, for asserting
// on rendered content that isn't itself testing styling. The "[!-/]*" segment
// matches a CSI sequence's optional intermediate byte(s) (e.g. the "$" in a
// DECRQM query like "\x1b[?2026$p") that precede its final letter — without it,
// that one query survives unstripped and glues onto whatever real content
// follows it on the same line (confirmed live: it broke a plain HasPrefix("╭")
// check once nothing but the query preceded the picker's first rendered line).
var ansiRe = regexp.MustCompile("\x1b(?:\\[[0-9;?<=>]*[!-/]*[a-zA-Z]|\\][^\a]*\a|[><=])")

func stripANSI(s string) string {
	return ansiRe.ReplaceAllString(s, "")
}

// rowHasArrow reports whether name's row in out carries the " →" forward
// cue — a regexp rather than a plain "name →" substring, since rootRow's
// arrow-column alignment (rootNameColumn) pads name out to the widest name
// in its own list before the arrow, so the exact gap between name and arrow
// varies by screen and even by search result set. [ \t]+, not \s+, so the
// match can't accidentally span a newline onto an unrelated arrow below.
func rowHasArrow(out, name string) bool {
	return regexp.MustCompile(regexp.QuoteMeta(name) + `[ \t]+→`).MatchString(out)
}

// TestInteractivePicker exercises "cpro" run with no arguments: the root
// command picker (main.go's root.RunE / pickCommandArgs, rootui.go).
func TestInteractivePicker(t *testing.T) {
	bin, _ := buildCLI(t)

	// Typing "vers" (no need to press "/" first) filters down to "version" alone;
	// Enter picks it, and root.RunE re-executes as "cpro version". "version"
	// only exists in the full palette (cpro menu), not the reduced launcher.
	t.Run("filter and select", func(t *testing.T) {
		master, slave := openPTY(t)
		capture := drainPTY(master)
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, "menu")
		cmd.Env = append(os.Environ(), "TERM=xterm-256color")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(600 * time.Millisecond)
		for _, r := range "vers" {
			if _, err := master.WriteString(string(r)); err != nil {
				t.Fatal(err)
			}
			time.Sleep(150 * time.Millisecond)
		}
		time.Sleep(300 * time.Millisecond)
		if _, err := master.WriteString("\r"); err != nil {
			t.Fatal(err)
		}
		if err := cmd.Wait(); err != nil {
			t.Fatalf("picker: %v; output %s", err, stripANSI(capture()))
		}
		if !strings.Contains(capture(), "cpro version "+version) {
			t.Fatalf("expected version output, got %q", stripANSI(capture()))
		}
	})

	// The picker is a flat list with no *selectable* group boundary (see
	// rootPickerMeta's internal, never-rendered "group" field) — two Downs
	// from the initial "config" (Core, past "default", the other Core
	// member) land on "login", Account's first member; the blank rail row
	// between the two groups (viewNormal) is rendered but, like the group
	// name itself, isn't a cursor position to land on or skip over. "login"
	// only exists in the full palette (cpro menu), not the reduced launcher.
	//
	// Proof is "which command Enter actually runs" (login's own required-arg
	// prompt for EMAIL), not a cumulative-capture text search for the
	// description line: a single-row cursor move onto an *adjacent* item is
	// exactly the kind of minimal, single-cell diff bubbletea's renderer may
	// paint via cursor-repositioning writes rather than a contiguous
	// retransmission of the whole line — confirmed live, this landed the
	// cursor on login correctly every time, but fragmented "Sign in to a
	// Claude account" across raw backspace-driven writes that a naive
	// substring search over the raw byte stream can't reassemble. See
	// TestRootPicker's own doc comment for the general rule this follows.
	t.Run("group boundaries are invisible; navigation just counts commands", func(t *testing.T) {
		master, slave := openPTY(t)
		capture := drainPTY(master)
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, "menu")
		cmd.Env = append(os.Environ(), "TERM=xterm-256color")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(600 * time.Millisecond)
		for range 2 { // config -> default -> login
			if _, err := master.Write([]byte{0x1b, '[', 'B'}); err != nil {
				t.Fatal(err)
			}
			time.Sleep(150 * time.Millisecond)
		}
		if _, err := master.WriteString("\r"); err != nil {
			t.Fatal(err)
		}
		time.Sleep(400 * time.Millisecond)
		out := stripANSI(capture())
		// Decision 0039: login opens its own in-app LOGIN frame now rather
		// than the separate huh prompt whose "you@example.com" placeholder
		// this used to look for. The assertion's job is unchanged — prove
		// the cursor actually landed on login — so it just matches the new
		// screen's own title instead.
		if !strings.Contains(out, "LOGIN") {
			t.Fatalf("expected Enter to open login's own LOGIN frame, proving the cursor landed there, got %q", out)
		}
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
}

// TestNavigationHierarchy covers the full Root -> Menu -> {Settings,
// Permissions} hierarchy end to end through a real pty: every screen's own
// "claude cpro[ - NAME]" title (screenTitle, tui.go), → opening a further
// screen (menu, then config, then permissions) exactly like Enter would, →
// doing nothing on a leaf row, and ←/Esc popping back one level at a time —
// including across the two cross-program seams (Menu -> Settings, and Menu
// -> Permissions, each a separate process under the hood — cpro config and
// cpro permissions, decision 0021) that pickCommandArgs's own loop
// (rootui.go) makes look like an ordinary pop from the outside. Permissions
// is no longer nested inside Settings (decision 0021 promoted it to its own
// top-level entry point, directly below "config" on the Menu frame), so
// this exercises it as a sibling round trip rather than a further push from
// within the Settings round trip. This is the riskiest class of code path
// the redesign added (everywhere else, "back" is an in-process
// navStack.pop()), so it gets its own end-to-end test proving the mechanism
// generalizes across both entry points, rather than relying only on the
// narrower per-screen coverage elsewhere.
func TestNavigationHierarchy(t *testing.T) {
	bin, _ := buildCLI(t)

	master, slave := openPTY(t)
	capture := drainPTY(master)
	// A generous timeout: this walks through two separate real processes (the
	// picker, then a nested `cpro config`), each doing its own openStore/
	// terminal setup — noticeably slower than an in-process frame update.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin)
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)

	send := func(s string) {
		if _, err := master.WriteString(s); err != nil {
			t.Fatal(err)
		}
		time.Sleep(250 * time.Millisecond)
	}
	down := func() { send("\x1b[B") }
	right := func() { send("\x1b[C") }
	left := func() { send("\x1b[D") }
	esc := func() { send("\x1b") }
	alive := func(t *testing.T, msg string) {
		t.Helper()
		if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
			t.Fatalf("%s: %v", msg, err)
		}
	}

	if out := stripANSI(capture()); !strings.Contains(out, "╭─ claude cpro\r") {
		t.Fatalf("expected the bare root title with no suffix, got %q", out)
	}

	// Title text itself isn't checked live here: bubbletea's diffing renderer
	// may repaint only the differing suffix of the border line via cursor
	// positioning (confirmed live: "╭─ claude cpro" stayed put and " - MENU"
	// landed elsewhere in the raw stream), the same class of fragmentation
	// TestInteractivePicker hit — see TestScreenTitles for the deterministic,
	// single-render check of the literal title strings instead. Content/footer
	// text unique to each screen is what's checked live here as proof of
	// actually being there.
	down() // run -> watch
	down() // watch -> status
	down() // status -> menu
	right()
	time.Sleep(300 * time.Millisecond)
	if out := stripANSI(capture()); !strings.Contains(out, "← Back") || !strings.Contains(out, "login") {
		t.Fatalf("expected → on \"menu\" to open the full palette (← Back footer, \"login\" row), got %q", out)
	}

	right() // cursor is on "config" (Menu's first entry) — → opens CONFIG
	time.Sleep(400 * time.Millisecond)
	if out := stripANSI(capture()); !strings.Contains(out, "Appearance") || !strings.Contains(out, "↵ Edit") {
		t.Fatalf("expected → on \"config\" to open CONFIG (\"Appearance\" section, \"↵ Edit\" footer), got %q", out)
	}
	// CONFIG reached via the picker (configMenuItemsSettings) omits the
	// "Default" row: it's already one press away as the menu's own
	// "default →" row, so offering it a second time here would be
	// confusing, repeated navigation — see TestConfigUI for the direct
	// `cpro config` invocation, which does show it (configMenuItemsConfig).
	if out := stripANSI(capture()); strings.Contains(out, "Default") {
		t.Fatalf("expected no Default row on CONFIG reached via the picker, got %q", out)
	}

	esc()                              // CONFIG (has a parent) -> pop to Menu, crossing back out of the separate `cpro config` process
	time.Sleep(700 * time.Millisecond) // a cross-program hop needs longer to settle than an in-process pop
	alive(t, "Esc from CONFIG should pop back to Menu, not exit")

	down() // config -> default (Menu's second entry, same Core group)
	right()
	time.Sleep(300 * time.Millisecond)
	if out := stripANSI(capture()); !strings.Contains(out, "SET DEFAULT ACCOUNT") {
		t.Fatalf("expected → on \"default\" to open the SET DEFAULT ACCOUNT screen, got %q", out)
	}

	right() // → on a permission-mode row is a leaf action; must not crash or navigate
	alive(t, "→ on a leaf row inside SET DEFAULT ACCOUNT must not exit")

	left()                             // ← on SET DEFAULT ACCOUNT (has a parent) -> pop to Menu, crossing back out of the separate `cpro default` process
	time.Sleep(700 * time.Millisecond) // a cross-program hop needs longer to settle than an in-process pop
	alive(t, "← from SET DEFAULT ACCOUNT should pop back to Menu, not exit")

	esc() // Menu -> pop to Root
	time.Sleep(300 * time.Millisecond)
	alive(t, "Esc from Menu should pop back to Root, not exit")

	esc() // Root: first Esc only arms exit
	time.Sleep(200 * time.Millisecond)
	alive(t, "the first Esc at the root should only arm exit, not exit immediately")

	esc() // Root: second consecutive Esc actually exits
	err := cmd.Wait()
	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 1 {
		t.Fatalf("expected exit 1 after popping all the way back to the root and double-Esc, got %v; output %q", err, stripANSI(capture()))
	}
}

// TestScreenTitles asserts the literal "claude cpro[ - NAME]" border title
// (screenTitle, tui.go) of every screen via a single, fresh render call
// rather than a live pty — bubbletea's diffing renderer may repaint only a
// changed suffix of the border line via cursor positioning rather than
// retransmitting it contiguously (see TestNavigationHierarchy's own comment,
// and TestInteractivePicker before it, for two live examples of exactly this
// fragmentation), which makes a title string an unreliable thing to grep for
// in a cumulative pty capture. A direct render sidesteps that entirely.
func TestScreenTitles(t *testing.T) {
	root := &rootPickerApp{shades: deriveAccentShades("#A78BFA", 5)}
	root.stack = newNavStack(rootFrame{kind: frameList, list: newCommandBrowseList([]rootPickerEntry{{name: "run"}})})
	if got := root.viewNormal(); !strings.Contains(got, "╭─ claude cpro\n") {
		t.Fatalf("expected the bare root title with no suffix, got %q", got)
	}

	menu := &rootPickerApp{shades: deriveAccentShades("#A78BFA", 5)}
	menu.stack = newNavStack(rootFrame{kind: frameList, title: "MENU", list: newCommandBrowseList([]rootPickerEntry{{name: "config"}})})
	if got := menu.viewNormal(); !strings.Contains(got, "╭─ claude cpro - MENU") {
		t.Fatalf("expected the Menu title, got %q", got)
	}

	system := &rootPickerApp{}
	system.stack = newNavStack(rootFrame{kind: frameSubmenu, title: "SYSTEM"})
	if got := system.viewSubmenu(); !strings.Contains(got, "╭─ claude cpro - SYSTEM") {
		t.Fatalf("expected the System submenu's title, got %q", got)
	}

	runAccount := &rootPickerApp{}
	runAccount.stack = newNavStack(rootFrame{kind: frameRunAccount, title: "RUN ACCOUNT", list: newAccountEntryBrowseList([]rootPickerEntry{{name: "a@example.com"}})})
	if got := runAccount.viewRunAccount(); !strings.Contains(got, "╭─ claude cpro - RUN ACCOUNT") {
		t.Fatalf("expected the RUN ACCOUNT title, got %q", got)
	}

	runMode := &rootPickerApp{}
	runMode.stack = newNavStack(rootFrame{kind: frameRunMode, title: "RUN MODE"})
	if got := runMode.viewRunMode(); !strings.Contains(got, "╭─ claude cpro - RUN MODE") {
		t.Fatalf("expected the RUN MODE title, got %q", got)
	}

	watchMode := &rootPickerApp{}
	watchMode.stack = newNavStack(rootFrame{kind: frameWatchMode, title: "WATCH MODE"})
	if got := watchMode.viewWatchMode(); !strings.Contains(got, "╭─ claude cpro - WATCH MODE") {
		t.Fatalf("expected the WATCH MODE title, got %q", got)
	}

	// Decision 0040 retired the "SETTINGS" title decision 0023 had
	// introduced: screenMenu is one screen and now says so from either entry
	// point. The item sets still differ (each view omits what its own entry
	// point already offers one press away) — TestConfigUI covers that — but
	// the border title no longer does.
	for _, hasParent := range []bool{true, false} {
		app := &configApp{stack: newNavStack(screenMenu), hasParent: hasParent}
		if got := app.viewMenu(); !strings.Contains(got, "╭─ claude cpro - CONFIG") {
			t.Fatalf("hasParent=%v: expected the CONFIG title, got %q", hasParent, got)
		}
		if got := app.viewMenu(); strings.Contains(got, "SETTINGS") {
			t.Fatalf("hasParent=%v: the SETTINGS title is gone (decision 0040), got %q", hasParent, got)
		}
	}

	theme := &configApp{stack: newNavStack(screenTheme)}
	if got := theme.viewTheme(); !strings.Contains(got, "╭─ claude cpro - THEME") {
		t.Fatalf("expected the Theme title, got %q", got)
	}

	def := &configApp{stack: newNavStack(screenDefault)}
	if got := def.viewDefault(); !strings.Contains(got, "╭─ claude cpro - SET DEFAULT ACCOUNT") {
		t.Fatalf("expected the SET DEFAULT ACCOUNT title, got %q", got)
	}

	color := &configApp{stack: newNavStack(screenColor)}
	color.colorState = colorPickerState{title: "Accent color"}
	if got := color.viewColor(); !strings.Contains(got, "╭─ claude cpro - ACCENT COLOR") {
		t.Fatalf("expected the (uppercased) Accent color title, got %q", got)
	}

	threshMenu := &configApp{stack: newNavStack(screenThresholdMenu)}
	threshMenu.threshMenu = thresholdMenuState{target: "warn", title: "Warning"}
	if got := threshMenu.viewThresholdMenu(); !strings.Contains(got, "╭─ claude cpro - WARNING") {
		t.Fatalf("expected the (uppercased) Warning sub-menu title, got %q", got)
	}

	export := &accountPickerApp{browseList: newAccountBrowseList([]string{"a@example.com"}), cfg: exportPickerConfig}
	if got := export.viewNormal(); !strings.Contains(got, "╭─ claude cpro - SELECT ACCOUNT") {
		t.Fatalf("expected the export account picker's title, got %q", got)
	}

}

// TestEscGuard exercises Esc-to-cancel across the picker (rootui.go, single
// Esc, no warning — see rootPickerApp's own doc comment for why it diverges
// from the rest of cpro's pickers) and the double-Esc-to-exit convention
// (ui.go's escGuardField), still very much alive one level down: the huh
// fields pickRequiredArgs opens after a command is chosen (e.g. login's EMAIL
// input) are unchanged and still use it.
func TestEscGuard(t *testing.T) {
	bin, _ := buildCLI(t)

	t.Run("first Esc does not exit the root picker; a second consecutive Esc does", func(t *testing.T) {
		master, slave := openPTY(t)
		capture := drainPTY(master)
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin)
		cmd.Env = append(os.Environ(), "TERM=xterm-256color")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(600 * time.Millisecond)
		if _, err := master.Write([]byte{0x1b}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(300 * time.Millisecond)
		if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
			t.Fatalf("a single Esc should not exit the root picker: %v", err)
		}
		if _, err := master.Write([]byte{0x1b}); err != nil {
			t.Fatal(err)
		}
		err := cmd.Wait()
		exitErr, ok := err.(*exec.ExitError)
		if !ok || exitErr.ExitCode() != 1 {
			t.Fatalf("expected exit 1 from a second Esc, got %v", err)
		}
		out := stripANSI(capture())
		if strings.Contains(out, "Press Esc again") {
			t.Fatalf("the root picker signals an armed exit via its rail color, not a printed warning line: %q", out)
		}
		if strings.Contains(out, "ERROR") {
			t.Fatalf("cancelling isn't an error, should not show an ERROR box: %q", out)
		}
	})

	// Decision 0039 replaced login's own separate huh EMAIL prompt — which
	// armed escGuardField's double-Esc — with an in-app frame on the picker's
	// navStack, so this now asserts the convention that actually governs
	// there: a nested frame with a parent to return to pops on a SINGLE Esc,
	// and only the stack's own root arms double-Esc (the same rule every
	// other nested frame here already follows). escGuardField itself is
	// untouched and still guards pickRequiredArgs, which remains the
	// fallback for any future picker command with a required argument —
	// there just is not one any more: login/logout/remove supply their own
	// EMAIL in-app now, and every other picker entry either takes no
	// argument or opens a screen of its own.
	t.Run("login's in-app email field pops back on a single Esc, and the picker stays alive", func(t *testing.T) {
		master, slave := openPTY(t)
		capture := drainPTY(master)
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, "menu") // "login" only exists in the full palette
		cmd.Env = append(os.Environ(), "TERM=xterm-256color")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(600 * time.Millisecond)
		for _, r := range "login" {
			if _, err := master.WriteString(string(r)); err != nil {
				t.Fatal(err)
			}
			time.Sleep(120 * time.Millisecond)
		}
		time.Sleep(200 * time.Millisecond)
		if _, err := master.WriteString("\r"); err != nil { // choose "login"
			t.Fatal(err)
		}
		time.Sleep(300 * time.Millisecond)
		if !strings.Contains(stripANSI(capture()), "LOGIN") {
			t.Fatalf("expected login to open its own in-app LOGIN frame, got %q", stripANSI(capture()))
		}
		if _, err := master.Write([]byte{0x1b}); err != nil { // a single Esc pops the nested frame
			t.Fatal(err)
		}
		time.Sleep(400 * time.Millisecond)
		// Liveness, not text, is the ground truth here: a pty capture is
		// cumulative, so the popped-to MENU frame's text is already in it
		// from before. The process still running is what proves one Esc
		// backed out rather than exiting — the same proof TestRootPicker's
		// own back-navigation subtests use.
		if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
			t.Fatalf("a single Esc on a nested frame must pop, not exit: %v", err)
		}
		if strings.Contains(stripANSI(capture()), "Press Esc again") {
			t.Fatalf("a nested frame must not arm the double-Esc warning: %q", stripANSI(capture()))
		}
		// Esc, Esc from the restored MENU root does exit, unchanged.
		for range 2 {
			if _, err := master.Write([]byte{0x1b}); err != nil {
				t.Fatal(err)
			}
			time.Sleep(200 * time.Millisecond)
		}
		err := cmd.Wait()
		exitErr, ok := err.(*exec.ExitError)
		if !ok || exitErr.ExitCode() != 1 {
			t.Fatalf("expected exit 1 from a confirmed Esc, got %v", err)
		}
		if strings.Contains(stripANSI(capture()), "ERROR") {
			t.Fatalf("cancelling isn't an error, should not show an ERROR box: %q", stripANSI(capture()))
		}
	})
}

// TestRootPickerExitArmed covers the root picker's double-Esc-to-exit state
// machine (exitArmed, rootui.go) directly — no pty needed: arming, confirming,
// and every way to cancel an armed exit (arrow key, Enter, typing), plus the
// rail actually turning red while armed and back to the gradient once
// cancelled, and that the whole mechanism still works with NO_COLOR (just
// with nothing visibly different). TestEscGuard and TestRootPicker cover the
// same behavior end to end through a real pty.
func TestRootPickerExitArmed(t *testing.T) {
	newModel := func() *rootPickerApp {
		root := rootCommand()
		root.InitDefaultHelpCmd()
		root.InitDefaultCompletionCmd()
		entries := rootPickerFilteredEntries(root, rootMenuNames())
		m := &rootPickerApp{color: true, shades: deriveAccentShades("#A78BFA", 5)}
		m.stack = newNavStack(rootFrame{kind: frameList, list: newCommandBrowseList(entries)})
		return m
	}
	escKey := tea.KeyPressMsg{Code: tea.KeyEsc}
	downKey := tea.KeyPressMsg{Code: tea.KeyDown}
	enterKey := tea.KeyPressMsg{Code: tea.KeyEnter}
	typeC := tea.KeyPressMsg{Code: 'c', Text: "c"}

	t.Run("first Esc arms exit without quitting", func(t *testing.T) {
		m := newModel()
		_, cmd := m.updateNormal(escKey)
		if !m.exitArmed {
			t.Fatal("expected exitArmed to be true after the first Esc")
		}
		// cmd is now the exitArmTimeout tea.Tick, not nil — the only way to
		// tell it apart from a real tea.Quit from here is that m.picked stays
		// nil (tea.Quit is only ever paired with either a real pick or an
		// explicit cancel, both of which set m.picked).
		if cmd == nil {
			t.Fatal("expected arming to start the exit-arm timeout command")
		}
		if m.picked != nil {
			t.Fatalf("expected nothing picked yet, got %v", m.picked)
		}
	})

	t.Run("second consecutive Esc quits (cancelled, not a pick)", func(t *testing.T) {
		m := newModel()
		m.updateNormal(escKey)
		_, cmd := m.updateNormal(escKey)
		if cmd == nil {
			t.Fatal("expected tea.Quit after a second consecutive Esc")
		}
		if m.picked != nil {
			t.Fatalf("expected picked to stay nil (cancelled), got %v", m.picked)
		}
	})

	t.Run("arrow key after first Esc cancels the arm and still moves the cursor", func(t *testing.T) {
		m := newModel()
		m.updateNormal(escKey)
		m.updateNormal(downKey)
		if m.exitArmed {
			t.Fatal("expected the arm to be cancelled by an arrow key")
		}
		if m.stack.current().list.cursor != 1 {
			t.Fatalf("expected the arrow key to still move the cursor, got cursor=%d", m.stack.current().list.cursor)
		}
	})

	t.Run("Enter after first Esc cancels the arm and still runs the selected command", func(t *testing.T) {
		m := newModel()
		m.updateNormal(escKey)
		_, cmd := m.updateNormal(enterKey)
		if m.exitArmed {
			t.Fatal("expected the arm to be cancelled by Enter")
		}
		if cmd == nil || len(m.picked) == 0 || m.picked[0] != m.stack.current().list.items[0].name {
			t.Fatalf("expected Enter to still pick the currently selected command, got picked=%v", m.picked)
		}
	})

	t.Run("typing after first Esc cancels the arm and still starts a search", func(t *testing.T) {
		m := newModel()
		m.updateNormal(escKey)
		m.updateNormal(typeC)
		if m.exitArmed {
			t.Fatal("expected the arm to be cancelled by typing")
		}
		if m.stack.current().list.query != "c" {
			t.Fatalf("expected typing to still start a search, got query=%q", m.stack.current().list.query)
		}
	})

	t.Run("first Esc turns the whole rail red AND changes the footer to Esc¹ again in Danger color", func(t *testing.T) {
		m := newModel()
		warnRGB := ansiTrueColor(t, dangerColor)

		before := m.viewNormal()
		if !strings.Contains(stripANSI(before), "Esc² to exit") {
			t.Fatalf("expected the normal \"Esc² to exit\" footer before any Esc, got %q", before)
		}
		if strings.Contains(before, warnRGB) {
			t.Fatalf("expected no danger red before any Esc, got %q", before)
		}

		m.updateNormal(escKey)
		armed := m.viewNormal()
		if !strings.Contains(armed, "Esc¹ again") {
			t.Fatalf("expected the footer to read \"Esc¹ again\" once exit is armed, got %q", armed)
		}
		if !strings.Contains(armed, warnRGB) {
			t.Fatalf("expected \"Esc¹ again\" to render in the configured Danger color, got %q", armed)
		}
		if !strings.Contains(armed, "╭─ claude cpro") {
			t.Fatalf("expected the rail's top edge to be recolored (still-styled \"╭─ claude cpro\" segment) while armed, got %q", armed)
		}
		// Both signals fire together: the rail/both edges turn solid Danger
		// red (see railShade), on top of the footer's own "Esc¹ again".
		// Only shades[:len-1] are checked for absence: deriveAccentShades'
		// last shade is accentMode itself unchanged, the same color the
		// cursor legitimately keeps using while armed — indistinguishable
		// from a "leaked" shade by a plain color search, unlike shades 0-3.
		for _, shade := range m.shades[:len(m.shades)-1] {
			if strings.Contains(armed, ansiTrueColor(t, shade)) {
				t.Fatalf("expected no gradient shade (%s) to survive on the rail while armed, got %q", shade, armed)
			}
		}

		m.updateNormal(downKey) // cancel
		after := m.viewNormal()
		if strings.Contains(after, warnRGB) {
			t.Fatalf("expected no danger red once the arm is cancelled, got %q", after)
		}
		if !strings.Contains(after, ansiTrueColor(t, m.shades[0])) {
			t.Fatalf("expected the normal gradient to return after cancelling, got %q", after)
		}
		if !strings.Contains(stripANSI(after), "Esc² to exit") {
			t.Fatalf("expected the normal footer to return after cancelling, got %q", after)
		}
	})

	t.Run("the armed footer uses whatever Danger color is actually configured, not a fixed red", func(t *testing.T) {
		orig := dangerColor
		t.Cleanup(func() { dangerColor = orig })
		dangerColor = "#8B5CF6" // Violet — deliberately not the default red, so this can't pass by coincidence
		m := newModel()
		m.updateNormal(escKey)
		armed := m.viewNormal()
		if !strings.Contains(armed, ansiTrueColor(t, "#8B5CF6")) {
			t.Fatalf("expected the armed footer to use the reconfigured Danger color, got %q", armed)
		}
		if strings.Contains(armed, ansiTrueColor(t, "#F87171")) {
			t.Fatalf("expected no trace of the old fixed red once Danger color is reconfigured, got %q", armed)
		}
	})

	t.Run("the armed state automatically expires and the footer reverts, without a real 2-second wait", func(t *testing.T) {
		m := newModel()
		if _, cmd := m.updateNormal(escKey); cmd == nil {
			t.Fatal("expected arming to return the exit-arm timeout command")
		}
		if !m.exitArmed {
			t.Fatal("expected exitArmed after the first Esc")
		}
		// Deliver the message exitArmTimeout would eventually produce
		// directly, rather than actually invoking the real tea.Tick command
		// (which blocks for the full exitArmTimeout internally) or sleeping
		// in the test.
		m.Update(exitArmExpiredMsg{gen: m.exitArmedGen})
		if m.exitArmed {
			t.Fatal("expected exitArmed to clear once its own timeout message arrives")
		}
		if !strings.Contains(stripANSI(m.viewNormal()), "Esc² to exit") {
			t.Fatalf("expected the footer to revert to \"Esc² to exit\" after expiring, got %q", m.viewNormal())
		}
	})

	t.Run("a stale timeout cannot cancel a newer armed state", func(t *testing.T) {
		m := newModel()
		m.updateNormal(escKey) // first arm, gen 1
		staleGen := m.exitArmedGen
		m.updateNormal(downKey) // cancelled by another key (gen stays the same, exitArmed false)
		m.updateNormal(escKey)  // second arm, gen bumped again
		if !m.exitArmed {
			t.Fatal("expected the second Esc to (re-)arm exit")
		}
		if staleGen == m.exitArmedGen {
			t.Fatalf("test setup broken: the stale gen (%d) must differ from the current one (%d)", staleGen, m.exitArmedGen)
		}

		m.Update(exitArmExpiredMsg{gen: staleGen})
		if !m.exitArmed {
			t.Fatal("the FIRST (stale) arm's timeout must not clear the second, still-active arm")
		}

		m.Update(exitArmExpiredMsg{gen: m.exitArmedGen})
		if m.exitArmed {
			t.Fatal("the second arm's own timeout should still clear it normally")
		}
	})

	t.Run("NO_COLOR: the two-step behavior still works with nothing to see", func(t *testing.T) {
		m := newModel()
		m.color = false
		m.updateNormal(escKey)
		if !m.exitArmed {
			t.Fatal("expected exitArmed to still be set even with color disabled")
		}
		if strings.Contains(m.viewNormal(), "\x1b") {
			t.Fatalf("expected no escape codes at all with color disabled: %q", m.viewNormal())
		}
		_, cmd := m.updateNormal(escKey)
		if cmd == nil {
			t.Fatal("expected a second Esc to still quit with color disabled")
		}
	})
}

// TestRootPickerEntries covers the picker's static content and default
// selection without needing a pty or a running program: rootPickerFilteredEntries
// (rootui.go) filtered against a real command tree — exact command order,
// each command's internal (never-rendered) semantic group, its short
// metadata, and that its description is the command's own real cobra Short
// rather than a duplicated string.
func TestRootPickerEntries(t *testing.T) {
	root := rootCommand()
	root.InitDefaultHelpCmd()
	root.InitDefaultCompletionCmd()
	entries := rootPickerFilteredEntries(root, rootMenuNames())

	wantOrder := []string{"config", "default", "login", "logout", "remove", "session", "system", "doctor", "install", "completion", "info", "help", "version"}
	if len(entries) != len(wantOrder) {
		t.Fatalf("expected %d entries, got %d: %+v", len(wantOrder), len(entries), entries)
	}
	for i, name := range wantOrder {
		if entries[i].name != name {
			t.Fatalf("expected %q at position %d, got %q: %+v", name, i, entries[i].name, entries)
		}
	}
	// "menu" (the root launcher's own gateway entry) and "run"/"status"/
	// "watch" (already one press away in the reduced launcher every path to
	// Menu passes through first) and "list" (dropped from both pickers —
	// still a real, directly-invocable command, just no longer
	// picker-visible) must never appear in the full menu.
	for _, e := range entries {
		if e.name == "menu" || e.name == "list" || e.name == "run" || e.name == "status" || e.name == "watch" {
			t.Fatalf("%q must not appear in cpro menu's own entry list: %+v", e.name, entries)
		}
	}

	wantGroup := map[string]string{
		// "default" is Core, not the "Account" group: that group is the
		// sign-in/sign-out/delete trio acting *on* accounts, while this row
		// edits cpro run's own defaults.
		"config": "Core", "default": "Core",
		"login": "Account", "logout": "Account", "remove": "Account",
		"session": "Settings", "system": "Settings", "doctor": "Settings",
		"install": "System", "completion": "System",
		"info": "About", "help": "About", "version": "About",
	}
	for _, e := range entries {
		if want := wantGroup[e.name]; e.group != want {
			t.Fatalf("%s: group = %q, want %q", e.name, e.group, want)
		}
		for _, group := range rootGroups {
			if e.name == group {
				t.Fatalf("group label %q leaked into a selectable entry's name: %+v", group, entries)
			}
		}
	}

	wantShortLabel := map[string]string{
		"login": "Sign in", "logout": "Sign out", "remove": "Delete profile", "session": "Sessions", "system": "System credentials",
		"config": "Preferences", "default": "Run defaults", "doctor": "Diagnostics",
		"install": "Install cpro", "completion": "Shell setup",
		"info": "cpro information", "help": "Help", "version": "Version",
	}
	for _, e := range entries {
		if want := wantShortLabel[e.name]; e.shortLabel != want {
			t.Fatalf("%s: shortLabel = %q, want %q", e.name, e.shortLabel, want)
		}
		if e.description == "" {
			t.Fatalf("%s: expected a non-empty description from the command's own cobra Short", e.name)
		}
	}
	// The description is the real thing, not a duplicated string: it must equal
	// the actual command's Short text.
	for _, e := range entries {
		if e.name == "config" && !strings.Contains(e.description, "configuration") {
			t.Fatalf(`config's description should be its real cobra Short, got %q`, e.description)
		}
	}
}

// TestRootLauncherEntries covers the reduced bare-`cpro` launcher's own entry
// list — the handful of everyday commands plus "menu" — separately from
// cpro menu's full list (TestRootPickerEntries): same builder
// (rootPickerFilteredEntries), a different names slice is the only thing
// that distinguishes the two.
func TestRootLauncherEntries(t *testing.T) {
	root := rootCommand()
	root.InitDefaultHelpCmd()
	root.InitDefaultCompletionCmd()
	entries := rootPickerFilteredEntries(root, rootLauncherNames)

	wantOrder := []string{"run", "watch", "status", "menu"}
	if len(entries) != len(wantOrder) {
		t.Fatalf("expected %d entries, got %d: %+v", len(wantOrder), len(entries), entries)
	}
	for i, name := range wantOrder {
		if entries[i].name != name {
			t.Fatalf("expected %q at position %d, got %q: %+v", name, i, entries[i].name, entries)
		}
	}
	if entries[0].name != "run" {
		t.Fatalf(`expected "run" first (the default selection), got %+v`, entries)
	}
	// Nothing outside the reduced set — no secondary/admin commands — leaks
	// into the launcher.
	for _, hidden := range []string{"list", "login", "logout", "remove", "system", "config", "doctor", "install", "completion", "info", "help", "version"} {
		for _, e := range entries {
			if e.name == hidden {
				t.Fatalf("%q should not appear in the reduced root launcher: %+v", hidden, entries)
			}
		}
	}
	if got := entries[len(entries)-1]; got.name != "menu" || got.shortLabel != "More commands ..." {
		t.Fatalf(`expected "menu" ("More commands ...") last, got %+v`, got)
	}
}

// TestForwardEntryArrows covers the visible " →" cue (rootNameColumn,
// rootui.go; configItemLabel, configui.go) directly, pure and without a pty:
// every command name that opens a further screen gets the arrow, every leaf
// command that only Enter executes does not — and the underlying name/key
// itself is never touched, only the rendered label.
func TestForwardEntryArrows(t *testing.T) {
	// login/logout/remove joined this set in decision 0039: each opens a
	// screen of its own now (an account list, or an email field) to supply
	// its own EMAIL argument in-app, so each takes the same " →" cue every
	// other forward row has.
	forward := map[string]bool{
		"menu": true, "config": true, "default": true, "system": true,
		"run": true, "session": true, "watch": true,
		"login": true, "logout": true, "remove": true,
	}
	for _, meta := range rootPickerMeta {
		e := rootPickerEntry{name: meta.name}
		got := rootNameColumn(e, len(meta.name))
		wantArrow := forward[meta.name]
		hasArrow := strings.HasSuffix(got, " →")
		if hasArrow != wantArrow {
			t.Fatalf("%s: rootNameColumn = %q, want arrow=%v", meta.name, got, wantArrow)
		}
		if !strings.HasPrefix(got, meta.name) {
			t.Fatalf("%s: rootNameColumn %q must start with the real name", meta.name, got)
		}
		// The underlying entry itself must stay untouched — decoration is
		// rendering-only, never stored back into what search/scoring key off.
		if e.name != meta.name {
			t.Fatalf("%s: rootNameColumn must not mutate the entry's own name, got %q", meta.name, e.name)
		}
	}

	forwardConfig := map[string]bool{"theme": true, "default": true, "accent": true, "bar": true, "warn": true, "danger": true}
	allConfigItems := append(append([]configMenuItem{}, configMenuItemsSettings...), configMenuItemsConfig...)
	for _, it := range allConfigItems {
		got := configItemLabel(it)
		wantArrow := forwardConfig[it.key]
		hasArrow := strings.HasSuffix(got, " →")
		if hasArrow != wantArrow {
			t.Fatalf("%s: configItemLabel = %q, want arrow=%v", it.key, got, wantArrow)
		}
		if !strings.HasPrefix(got, it.label) {
			t.Fatalf("%s: configItemLabel %q must start with the real label", it.key, got)
		}
	}
}

// TestInteractiveRunFlow covers rootui.go's RUN ACCOUNT/RUN MODE frames
// (decision 0019) directly, no pty — mirroring TestPermissionsPreviewTracksCursor's
// own reasoning: bubbletea's diffing renderer makes a same-screen cursor move
// unreliable to verify via a cumulative pty capture, so this drives
// pushRunAccount/updateRunAccount/updateRunAccountSearch/updateRunMode
// directly and asserts on the resulting frame/m.picked/m.runFlowErr state.
// TestInteractiveRunFlowEndToEnd (below) covers the same flow live, through a
// real pty, reaching the fake claude helper.
// TestAccountArgCommands covers decision 0039: login/logout/remove supply
// their own EMAIL argument from inside the picker instead of dropping out of
// it into a separate huh prompt. Pure, no pty — the same reasoning
// TestInteractiveRunFlow/TestRootPickerExitArmed already document for driving
// these update/view methods directly.
func TestAccountArgCommands(t *testing.T) {
	_, s := buildCLI(t)
	const emailA, emailB = "a@example.com", "b@example.com"
	if err := s.update(func(c *config) error {
		c.Accounts = map[string]bool{emailA: true, emailB: true}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	newModel := func() *rootPickerApp {
		m := &rootPickerApp{s: s, color: false, shades: deriveAccentShades("#A78BFA", 5)}
		m.stack = newNavStack(rootFrame{kind: frameList, list: newCommandBrowseList(rootPickerEntries)})
		return m
	}
	enterKey := tea.KeyPressMsg{Code: tea.KeyEnter}
	rightKey := tea.KeyPressMsg{Code: tea.KeyRight}
	escKey := tea.KeyPressMsg{Code: tea.KeyEsc}

	t.Run("logout finalizes from the account list; remove first opens the in-app confirm", func(t *testing.T) {
		m := newModel()
		if _, err := m.pushAccountArg("logout"); err != nil {
			t.Fatal(err)
		}
		frame := m.stack.current()
		if frame.kind != frameRunAccount || frame.command != "logout" {
			t.Fatalf("expected an account-list frame carrying logout, got %+v", frame)
		}
		if len(frame.list.items) != 2 {
			t.Fatalf("expected both registered accounts listed, got %+v", frame.list.items)
		}
		m.updateRunAccount(enterKey) // cursor 0 -> a@example.com, sorted first
		if want := []string{"logout", emailA}; !reflect.DeepEqual(m.picked, want) {
			t.Fatalf("logout: got picked=%v, want %v", m.picked, want)
		}

		// remove is destructive, so the account pick only opens the retype
		// guard (decision 0042); it must not finalize anything yet.
		m = newModel()
		if _, err := m.pushAccountArg("remove"); err != nil {
			t.Fatal(err)
		}
		m.updateRunAccount(enterKey)
		if m.picked != nil {
			t.Fatalf("remove must not finalize before confirmation, got %v", m.picked)
		}
		confirm := m.stack.current()
		if confirm.kind != frameRemoveConfirm || confirm.account != emailA {
			t.Fatalf("expected the confirm frame for a@example.com, got %+v", confirm)
		}
	})

	t.Run("Right Arrow finalizes too, same as Enter", func(t *testing.T) {
		m := newModel()
		if _, err := m.pushAccountArg("logout"); err != nil {
			t.Fatal(err)
		}
		m.updateRunAccount(tea.KeyPressMsg{Code: tea.KeyDown}) // -> b@example.com
		m.updateRunAccount(rightKey)
		if want := []string{"logout", emailB}; !reflect.DeepEqual(m.picked, want) {
			t.Fatalf("got picked=%v, want %v", m.picked, want)
		}
	})

	t.Run("search filters the list and Enter from search reaches the remove confirm", func(t *testing.T) {
		m := newModel()
		if _, err := m.pushAccountArg("remove"); err != nil {
			t.Fatal(err)
		}
		m.updateRunAccount(tea.KeyPressMsg{Code: 'b', Text: "b"})
		if view := m.viewRunAccountSearch(); !strings.Contains(view, emailB) {
			t.Fatalf("expected the filtered row, got %q", view)
		}
		m.updateRunAccountSearch(enterKey)
		confirm := m.stack.current()
		if confirm.kind != frameRemoveConfirm || confirm.account != emailB {
			t.Fatalf("expected the confirm frame for b@example.com, got %+v", confirm)
		}
		// Retyping the account is the guard; only then does it finalize, with
		// --yes because the in-app screen already *is* the confirmation.
		m.updateRemoveConfirm(enterKey)
		if m.picked != nil || m.emailErr == "" {
			t.Fatalf("expected an empty answer to be rejected in place, got picked=%v err=%q", m.picked, m.emailErr)
		}
		for _, r := range emailB {
			m.updateRemoveConfirm(tea.KeyPressMsg{Code: r, Text: string(r)})
		}
		m.updateRemoveConfirm(enterKey)
		if want := []string{"remove", emailB, "--yes"}; !reflect.DeepEqual(m.picked, want) {
			t.Fatalf("got picked=%v, want %v", m.picked, want)
		}
	})

	t.Run("remove's confirm accepts the masked alias, and Esc backs out without removing", func(t *testing.T) {
		origEnabled, origTable := maskEmailEnabled, emailMaskTable
		t.Cleanup(func() { maskEmailEnabled, emailMaskTable = origEnabled, origTable })
		const alias = "placeholder-fox"
		maskEmailEnabled, emailMaskTable = true, map[string]string{emailA: alias}

		m := newModel()
		if _, err := m.pushAccountArg("remove"); err != nil {
			t.Fatal(err)
		}
		m.updateRunAccount(enterKey) // choose a
		if view := m.viewRemoveConfirm(); !strings.Contains(view, alias) || strings.Contains(view, emailA) {
			t.Fatalf("expected only the alias in the confirm prompt, got %q", view)
		}
		// The alias shown on screen must be an acceptable answer (asking the
		// user to retype a hidden address would be unanswerable)...
		for _, r := range alias {
			m.updateRemoveConfirm(tea.KeyPressMsg{Code: r, Text: string(r)})
		}
		// ...but Esc before Enter still backs out to the account list without
		// removing anything.
		m.updateRemoveConfirm(escKey)
		if m.stack.current().kind != frameRunAccount || m.picked != nil {
			t.Fatalf("Esc must pop the confirm without picking anything, got frame=%v picked=%v", m.stack.current().kind, m.picked)
		}
		// The real address is accepted too (masking off, and on: the guard
		// compares against both so a masking toggle can't strand the user).
		m.updateRunAccount(enterKey)
		for _, r := range emailA {
			m.updateRemoveConfirm(tea.KeyPressMsg{Code: r, Text: string(r)})
		}
		m.updateRemoveConfirm(enterKey)
		if want := []string{"remove", emailA, "--yes"}; !reflect.DeepEqual(m.picked, want) {
			t.Fatalf("got picked=%v, want %v", m.picked, want)
		}
	})

	t.Run("these lists carry the same live Session usage RUN ACCOUNT does", func(t *testing.T) {
		// Decision 0036's own invariant: any account picker shows the same
		// usage, through the same component. A logout list is one, so it
		// must fetch and render exactly like RUN ACCOUNT's.
		m := newModel()
		cmd, err := m.pushAccountArg("logout")
		if err != nil {
			t.Fatal(err)
		}
		if cmd == nil {
			t.Fatal("expected the account list to start its usage fetches")
		}
		if view := m.viewRunAccount(); strings.Count(view, "--") != 4 {
			t.Fatalf("expected a placeholder per row before usage lands, got %q", view)
		}
		m.Update(runAccountUsageMsg{email: emailA, usage: runAccountUsage{loaded: true, session: 53, week: 53}})
		if view := m.viewRunAccount(); !strings.Contains(view, "53%") {
			t.Fatalf("expected the delivered percentage on the logout list, got %q", view)
		}
	})

	t.Run("the run flow itself is unchanged: an empty command still continues to RUN MODE", func(t *testing.T) {
		m := newModel()
		if _, err := m.pushRunAccount(); err != nil {
			t.Fatal(err)
		}
		if frame := m.stack.current(); frame.command != "" || frame.title != "RUN ACCOUNT" {
			t.Fatalf("expected the run flow's own untagged frame, got %+v", frame)
		}
		m.updateRunAccount(enterKey)
		if frame := m.stack.current(); frame.kind != frameRunMode || frame.account != emailA {
			t.Fatalf("expected RUN MODE, got %+v", frame)
		}
		if m.picked != nil {
			t.Fatalf("the run flow must not finalize at STEP 2, got %v", m.picked)
		}
	})

	t.Run("login types a new address, validated by the same normalizeEmail the command uses", func(t *testing.T) {
		m := newModel()
		if _, err := m.pushAccountArg("login"); err != nil {
			t.Fatal(err)
		}
		if frame := m.stack.current(); frame.kind != frameEmailInput || frame.command != "login" {
			t.Fatalf("expected login's own email-input frame, got %+v", frame)
		}
		type_ := func(s string) {
			for _, r := range s {
				m.updateEmailInput(tea.KeyPressMsg{Code: r, Text: string(r)})
			}
		}
		type_("not-an-email")
		m.updateEmailInput(enterKey)
		if m.picked != nil {
			t.Fatalf("an invalid address must not be accepted, got %v", m.picked)
		}
		if m.emailErr == "" {
			t.Fatal("expected the validator's own message to be shown")
		}
		if !strings.Contains(m.viewEmailInput(), m.emailErr) {
			t.Fatal("expected the validation message rendered on screen")
		}
		// The typed value survives so a typo is correctable in place.
		if m.emailInput != "not-an-email" {
			t.Fatalf("expected the typed value to survive rejection, got %q", m.emailInput)
		}
		for range len("not-an-email") {
			m.updateEmailInput(tea.KeyPressMsg{Code: tea.KeyBackspace})
		}
		type_("new@example.com")
		m.updateEmailInput(enterKey)
		if want := []string{"login", "new@example.com"}; !reflect.DeepEqual(m.picked, want) {
			t.Fatalf("got picked=%v, want %v", m.picked, want)
		}
	})

	t.Run("Esc pops each frame back without picking anything", func(t *testing.T) {
		for _, command := range []string{"login", "logout", "remove"} {
			m := newModel()
			if _, err := m.pushAccountArg(command); err != nil {
				t.Fatalf("%s: %v", command, err)
			}
			if m.stack.atRoot() {
				t.Fatalf("%s: expected a nested frame", command)
			}
			if m.stack.current().kind == frameEmailInput {
				m.updateEmailInput(escKey)
			} else {
				m.updateRunAccount(escKey)
			}
			if !m.stack.atRoot() {
				t.Fatalf("%s: expected Esc to pop back to the root frame", command)
			}
			if m.picked != nil {
				t.Fatalf("%s: Esc must not pick anything, got %v", command, m.picked)
			}
		}
	})

	t.Run("no registered accounts: logout and remove fail clearly instead of listing nothing", func(t *testing.T) {
		empty := &rootPickerApp{s: &store{dir: t.TempDir()}, color: false}
		empty.stack = newNavStack(rootFrame{kind: frameList, list: newCommandBrowseList(rootPickerEntries)})
		for _, command := range []string{"logout", "remove"} {
			if _, err := empty.pushAccountArg(command); err == nil {
				t.Fatalf("%s: expected a clear error with no accounts registered", command)
			}
		}
		// login is the exception: registering the first account is exactly
		// what it is for, so it must still open with none registered.
		if _, err := empty.pushAccountArg("login"); err != nil {
			t.Fatalf("login must work with no accounts registered: %v", err)
		}
	})
}

// TestPickerInlineErrors covers IMPROVEMENTS.md item 8 / decision 0043: a
// picker failure is shown inside the screen instead of only after the program
// exits. The most visible case was opening run/logout/remove with no
// registered accounts: the picker used to just close (m.runFlowErr set, then
// tea.Quit) and print the reason afterwards.
func TestPickerInlineErrors(t *testing.T) {
	t.Run("root picker shows a frame-open failure inline and stays open", func(t *testing.T) {
		m := &rootPickerApp{s: &store{dir: t.TempDir()}, color: false, shades: deriveAccentShades("#A78BFA", 5)}
		m.stack = newNavStack(rootFrame{kind: frameList, list: newCommandBrowseList(rootPickerEntries)})
		_, cmd := m.activateEntry(rootPickerEntry{name: "logout"})
		if cmd != nil {
			t.Fatal("expected the picker to stay open on a frame-open failure, got a quit command")
		}
		if m.runFlowErr == nil {
			t.Fatal("expected the failure to be recorded on m.runFlowErr")
		}
		if view := m.viewNormal(); !strings.Contains(view, "no accounts found") {
			t.Fatalf("expected the reason to render inline, got %q", view)
		}
		// The next keystroke means the message has been read; the frame keeps
		// working rather than the error lingering.
		m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
		if m.runFlowErr != nil {
			t.Fatal("expected the inline error to clear on the next key")
		}
		if view := m.viewNormal(); strings.Contains(view, "no accounts found") {
			t.Fatalf("expected the cleared error to disappear, got %q", view)
		}
	})

	t.Run("config shows a failed save inline and clears it on the next successful save", func(t *testing.T) {
		// The successful save below is a real Mask-emails toggle, and
		// saveMaskEmail updates the live masking globals as a side effect
		// (decision 0024); restore them so this can't leak into a later test.
		origEnabled, origTable := maskEmailEnabled, emailMaskTable
		t.Cleanup(func() { maskEmailEnabled, emailMaskTable = origEnabled, origTable })

		_, s := buildCLI(t)
		m := &configApp{s: s, c: config{}, color: false}
		m.stack = newNavStack(screenMenu)
		m.err = fmt.Errorf("could not save settings")
		if v := m.View(); !strings.Contains(v.Content, "could not save settings") {
			t.Fatalf("expected the save failure inline, got %q", v.Content)
		}
		// A later successful save clears the stale message.
		m.activateMenuItem("mask")
		if m.err != nil {
			t.Fatalf("expected a successful save to clear the error, got %v", m.err)
		}
		if v := m.View(); strings.Contains(v.Content, "could not save settings") {
			t.Fatalf("expected the stale error to disappear, got %q", v.Content)
		}
	})
}

func TestInteractiveRunFlow(t *testing.T) {
	_, s := buildCLI(t)
	if err := s.update(func(c *config) error {
		c.Accounts = map[string]bool{"a@example.com": true, "b@example.com": true}
		c.PermissionMode = "readonly"
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	newModel := func() *rootPickerApp {
		m := &rootPickerApp{s: s, color: false, shades: deriveAccentShades("#A78BFA", 5)}
		m.stack = newNavStack(rootFrame{kind: frameList, list: newCommandBrowseList(rootPickerEntries)})
		return m
	}
	downKey := tea.KeyPressMsg{Code: tea.KeyDown}
	enterKey := tea.KeyPressMsg{Code: tea.KeyEnter}
	escKey := tea.KeyPressMsg{Code: tea.KeyEsc}
	rightKey := tea.KeyPressMsg{Code: tea.KeyRight}
	leftKey := tea.KeyPressMsg{Code: tea.KeyLeft}

	t.Run("pushRunAccount populates every registered account, sorted", func(t *testing.T) {
		m := newModel()
		if _, err := m.pushRunAccount(); err != nil {
			t.Fatal(err)
		}
		frame := m.stack.current()
		if frame.kind != frameRunAccount || frame.title != "RUN ACCOUNT" {
			t.Fatalf("expected a RUN ACCOUNT frame, got %+v", frame)
		}
		if len(frame.list.items) != 2 || frame.list.items[0].name != "a@example.com" || frame.list.items[1].name != "b@example.com" {
			t.Fatalf("expected both accounts sorted, got %+v", frame.list.items)
		}
	})

	t.Run("no registered accounts surfaces a clear error instead of pushing", func(t *testing.T) {
		m := &rootPickerApp{s: &store{dir: t.TempDir()}}
		m.stack = newNavStack(rootFrame{kind: frameList, list: newCommandBrowseList(rootPickerEntries)})
		if _, err := m.pushRunAccount(); err == nil || !strings.Contains(err.Error(), "no accounts found") {
			t.Fatalf("expected a clear no-accounts error, got %v", err)
		}
		if !m.stack.atRoot() {
			t.Fatalf("expected no frame pushed when there are no accounts, got %+v", m.stack.current())
		}
	})

	t.Run("Enter on an account in RUN ACCOUNT pushes RUN MODE preselected from the saved default", func(t *testing.T) {
		m := newModel()
		if _, err := m.pushRunAccount(); err != nil {
			t.Fatal(err)
		}
		m.updateRunAccount(downKey) // a@example.com -> b@example.com
		m.updateRunAccount(enterKey)
		frame := m.stack.current()
		if frame.kind != frameRunMode || frame.account != "b@example.com" {
			t.Fatalf("expected RUN MODE with account b@example.com, got %+v", frame)
		}
		if permissionModes[frame.cursor].Key != "readonly" {
			t.Fatalf("expected RUN MODE preselected from the saved default (readonly), got %+v", permissionModes[frame.cursor])
		}
	})

	// Decision 0022: → is a consistent forward-select alias for Enter on
	// both RUN ACCOUNT and RUN MODE, matching every other screen's own
	// →/Enter convention — even though an account/mode row isn't a "forward
	// entry" in the isForwardEntry sense (no trailing " →" is ever shown on
	// either, since these are selectable values, not child menus).
	t.Run("Right Arrow on an account in RUN ACCOUNT selects it and pushes RUN MODE, same as Enter", func(t *testing.T) {
		m := newModel()
		if _, err := m.pushRunAccount(); err != nil {
			t.Fatal(err)
		}
		m.updateRunAccount(downKey) // a@example.com -> b@example.com
		m.updateRunAccount(rightKey)
		frame := m.stack.current()
		if frame.kind != frameRunMode || frame.account != "b@example.com" {
			t.Fatalf("expected Right Arrow to push RUN MODE with account b@example.com, got %+v", frame)
		}
	})

	t.Run("typing in RUN ACCOUNT filters by substring, Enter from search also proceeds", func(t *testing.T) {
		m := newModel()
		if _, err := m.pushRunAccount(); err != nil {
			t.Fatal(err)
		}
		m.updateRunAccount(tea.KeyPressMsg{Code: 'b', Text: "b"})
		if len(m.stack.current().list.filtered) != 1 || m.stack.current().list.items[m.stack.current().list.filtered[0]].name != "b@example.com" {
			t.Fatalf(`expected the query "b" to filter to b@example.com, got filtered=%v`, m.stack.current().list.filtered)
		}
		m.updateRunAccountSearch(enterKey)
		frame := m.stack.current()
		if frame.kind != frameRunMode || frame.account != "b@example.com" {
			t.Fatalf("expected Enter from search to push RUN MODE with the filtered account, got %+v", frame)
		}
	})

	t.Run("typing in RUN ACCOUNT filters by substring, Right Arrow from search also proceeds", func(t *testing.T) {
		m := newModel()
		if _, err := m.pushRunAccount(); err != nil {
			t.Fatal(err)
		}
		m.updateRunAccount(tea.KeyPressMsg{Code: 'b', Text: "b"})
		if len(m.stack.current().list.filtered) != 1 || m.stack.current().list.items[m.stack.current().list.filtered[0]].name != "b@example.com" {
			t.Fatalf(`expected the query "b" to filter to b@example.com, got filtered=%v`, m.stack.current().list.filtered)
		}
		m.updateRunAccountSearch(rightKey)
		frame := m.stack.current()
		if frame.kind != frameRunMode || frame.account != "b@example.com" {
			t.Fatalf("expected Right Arrow from search to push RUN MODE with the filtered account, got %+v", frame)
		}
	})

	t.Run("Esc from RUN MODE pops to RUN ACCOUNT, Esc from RUN ACCOUNT pops to ROOT", func(t *testing.T) {
		m := newModel()
		if _, err := m.pushRunAccount(); err != nil {
			t.Fatal(err)
		}
		m.updateRunAccount(enterKey) // -> RUN MODE
		if m.stack.current().kind != frameRunMode {
			t.Fatalf("expected RUN MODE pushed, got %+v", m.stack.current())
		}
		m.updateRunMode(escKey)
		if m.stack.current().kind != frameRunAccount {
			t.Fatalf("expected Esc from RUN MODE to pop back to RUN ACCOUNT, got %+v", m.stack.current())
		}
		m.updateRunAccount(escKey)
		if !m.stack.atRoot() {
			t.Fatalf("expected Esc from RUN ACCOUNT to pop back to the root frame, got %+v", m.stack.current())
		}
	})

	t.Run("Left Arrow from RUN MODE pops to RUN ACCOUNT, Left Arrow from RUN ACCOUNT pops to ROOT", func(t *testing.T) {
		m := newModel()
		if _, err := m.pushRunAccount(); err != nil {
			t.Fatal(err)
		}
		m.updateRunAccount(enterKey) // -> RUN MODE
		if m.stack.current().kind != frameRunMode {
			t.Fatalf("expected RUN MODE pushed, got %+v", m.stack.current())
		}
		m.updateRunMode(leftKey)
		if m.stack.current().kind != frameRunAccount {
			t.Fatalf("expected Left Arrow from RUN MODE to pop back to RUN ACCOUNT, got %+v", m.stack.current())
		}
		m.updateRunAccount(leftKey)
		if !m.stack.atRoot() {
			t.Fatalf("expected Left Arrow from RUN ACCOUNT to pop back to the root frame, got %+v", m.stack.current())
		}
	})

	t.Run("Enter on RUN MODE builds the exact cpro run argv via permissionModeArgs and quits", func(t *testing.T) {
		m := newModel()
		if _, err := m.pushRunAccount(); err != nil {
			t.Fatal(err)
		}
		m.updateRunAccount(enterKey) // account a@example.com -> RUN MODE, preselected readonly
		m.updateRunMode(downKey)     // readonly -> live
		m.updateRunMode(enterKey)
		want := append([]string{"run", "--account", "a@example.com"}, permissionModeArgs("live")...)
		if !reflect.DeepEqual(m.picked, want) {
			t.Fatalf("got picked=%v, want %v", m.picked, want)
		}
	})

	t.Run("Right Arrow on RUN MODE builds the exact cpro run argv, same as Enter", func(t *testing.T) {
		m := newModel()
		if _, err := m.pushRunAccount(); err != nil {
			t.Fatal(err)
		}
		m.updateRunAccount(enterKey) // account a@example.com -> RUN MODE, preselected readonly
		m.updateRunMode(downKey)     // readonly -> live
		m.updateRunMode(rightKey)
		want := append([]string{"run", "--account", "a@example.com"}, permissionModeArgs("live")...)
		if !reflect.DeepEqual(m.picked, want) {
			t.Fatalf("got picked=%v, want %v", m.picked, want)
		}
	})

	t.Run("selecting YOLO in RUN MODE checks requireYOLOSupport before building the argv", func(t *testing.T) {
		m := newModel()
		if _, err := m.pushRunAccount(); err != nil {
			t.Fatal(err)
		}
		m.updateRunAccount(enterKey)
		// Cursor starts preselected on the saved default ("readonly"), not
		// index 0 — walk forward until it actually reaches yolo, the last mode.
		for permissionModes[m.stack.current().cursor].Key != "yolo" {
			m.updateRunMode(downKey)
		}
		m.updateRunMode(enterKey)
		if m.runFlowErr != nil {
			t.Fatalf("expected the fake claude to support YOLO's required flags, got error %v", m.runFlowErr)
		}
		want := append([]string{"run", "--account", "a@example.com"}, permissionModeArgs("yolo")...)
		if !reflect.DeepEqual(m.picked, want) {
			t.Fatalf("got picked=%v, want %v", m.picked, want)
		}
	})

	t.Run("Right Arrow selecting YOLO in RUN MODE checks requireYOLOSupport too", func(t *testing.T) {
		m := newModel()
		if _, err := m.pushRunAccount(); err != nil {
			t.Fatal(err)
		}
		m.updateRunAccount(enterKey)
		for permissionModes[m.stack.current().cursor].Key != "yolo" {
			m.updateRunMode(downKey)
		}
		m.updateRunMode(rightKey)
		if m.runFlowErr != nil {
			t.Fatalf("expected the fake claude to support YOLO's required flags, got error %v", m.runFlowErr)
		}
		want := append([]string{"run", "--account", "a@example.com"}, permissionModeArgs("yolo")...)
		if !reflect.DeepEqual(m.picked, want) {
			t.Fatalf("got picked=%v, want %v", m.picked, want)
		}
	})

	// The interactive account/mode choices are per-run overrides only,
	// consistent whether finalized via Enter or → — neither ever writes
	// config.Default or config.PermissionMode back.
	t.Run("interactive account and permission selection are never persisted", func(t *testing.T) {
		before, err := s.read()
		if err != nil {
			t.Fatal(err)
		}
		m := newModel()
		if _, err := m.pushRunAccount(); err != nil {
			t.Fatal(err)
		}
		m.updateRunAccount(downKey) // -> b@example.com
		m.updateRunAccount(rightKey)
		for permissionModes[m.stack.current().cursor].Key != "yolo" {
			m.updateRunMode(downKey)
		}
		m.updateRunMode(rightKey)
		after, err := s.read()
		if err != nil {
			t.Fatal(err)
		}
		if before.Default != after.Default {
			t.Fatalf("expected the interactive account choice not to persist config.Default, before %q after %q", before.Default, after.Default)
		}
		if before.PermissionMode != after.PermissionMode {
			t.Fatalf("expected the interactive mode choice not to persist config.PermissionMode, before %q after %q", before.PermissionMode, after.PermissionMode)
		}
	})
}

// TestRunAccountUsage covers decision 0031's Session-usage column in the RUN
// ACCOUNT picker. Direct calls, no pty: usage arriving asynchronously and
// updating rows in place is exactly the same-screen, partial-rewrite case
// TestRootPickerExitArmed/TestEmailMaskingGlobal already document as
// unprovable through a cumulative diffed pty capture. Network-free throughout
// — every subtest that needs a percentage writes a fresh cpro-usage.json
// straight into the profile, the same trick TestStatusViews uses, since
// loadUsage treats a cache under a minute old as authoritative.
// seedUsageCache writes a fresh cpro-usage.json cache for email under s,
// with FiveHour.Utilization set to session — loadUsage treats a cache under
// a minute old as authoritative and skips the network call entirely, which
// is what makes tests driving a real usage fetch (TestRunAccountUsage,
// TestDefaultAccountUsage — decision 0036 — and TestStatusViews before them)
// deterministic and network-free. Shared across all three rather than each
// keeping its own copy of this seeding logic.
func seedUsageCache(t *testing.T, s *store, email string, session float64) {
	t.Helper()
	profile := s.profile(email)
	if err := privateDir(profile); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(usageCache{
		FetchedAt: time.Now(),
		Usage:     accountUsage{FiveHour: usageWindow{Utilization: session}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := atomicWrite(filepath.Join(profile, "cpro-usage.json"), append(b, '\n')); err != nil {
		t.Fatal(err)
	}
}

func TestRunAccountUsage(t *testing.T) {
	_, s := buildCLI(t)
	const emailA, emailB = "a@example.com", "b@example.com"
	if err := s.update(func(c *config) error {
		c.Accounts = map[string]bool{emailA: true, emailB: true}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	seedUsage := func(t *testing.T, email string, session float64) {
		seedUsageCache(t, s, email, session)
	}
	newModel := func() *rootPickerApp {
		m := &rootPickerApp{s: s, color: false, shades: deriveAccentShades("#A78BFA", 5)}
		m.stack = newNavStack(rootFrame{kind: frameList, list: newCommandBrowseList(rootPickerEntries)})
		return m
	}

	t.Run("accounts render before any usage arrives, with a placeholder", func(t *testing.T) {
		m := newModel()
		if _, err := m.pushRunAccount(); err != nil {
			t.Fatal(err)
		}
		// Nothing delivered yet: every row must already be on screen, each
		// showing "--" rather than a misleading 0%.
		view := m.viewRunAccount()
		for _, email := range []string{emailA, emailB} {
			if !strings.Contains(view, email) {
				t.Fatalf("expected %q to render before usage arrives, got %q", email, view)
			}
		}
		if strings.Count(view, "--") != 4 {
			t.Fatalf("expected a \"--\" placeholder for both accounts, got %q", view)
		}
		if strings.Contains(view, "0%") {
			t.Fatalf("expected no percentage at all before usage arrives, got %q", view)
		}
	})

	t.Run("a delivered usage message updates that row in place, percentage and all", func(t *testing.T) {
		m := newModel()
		if _, err := m.pushRunAccount(); err != nil {
			t.Fatal(err)
		}
		m.Update(runAccountUsageMsg{email: emailB, usage: runAccountUsage{loaded: true, session: 79, week: 79}})
		view := m.viewRunAccount()
		if !strings.Contains(view, "79%") {
			t.Fatalf("expected b's delivered percentage, got %q", view)
		}
		// A's fetch hasn't landed: its own row must still show the placeholder,
		// never borrow b's value.
		if strings.Count(view, "--") != 2 {
			t.Fatalf("expected a's placeholder to survive b's update, got %q", view)
		}
	})

	t.Run("usage arriving never moves the selection", func(t *testing.T) {
		m := newModel()
		if _, err := m.pushRunAccount(); err != nil {
			t.Fatal(err)
		}
		m.updateRunAccount(tea.KeyPressMsg{Code: tea.KeyDown}) // onto b
		before := m.stack.current().list.cursor
		m.Update(runAccountUsageMsg{email: emailA, usage: runAccountUsage{loaded: true, session: 12, week: 12}})
		m.Update(runAccountUsageMsg{email: emailB, usage: runAccountUsage{loaded: true, session: 34, week: 34}})
		if after := m.stack.current().list.cursor; after != before {
			t.Fatalf("usage updates moved the cursor from %d to %d", before, after)
		}
	})

	t.Run("a failed fetch keeps the placeholder and the account stays selectable", func(t *testing.T) {
		m := newModel()
		if _, err := m.pushRunAccount(); err != nil {
			t.Fatal(err)
		}
		m.Update(runAccountUsageMsg{email: emailA, usage: runAccountUsage{loaded: true, failed: true}})
		view := m.viewRunAccount()
		if !strings.Contains(view, "--") {
			t.Fatalf("expected a failed fetch to keep the placeholder, got %q", view)
		}
		if strings.Contains(strings.ToLower(view), "error") || strings.Contains(strings.ToLower(view), "failed") {
			t.Fatalf("expected no inline error message in the picker, got %q", view)
		}
		// The whole point: a failed usage lookup must not block the run flow.
		m.updateRunAccount(tea.KeyPressMsg{Code: tea.KeyEnter})
		if frame := m.stack.current(); frame.kind != frameRunMode || frame.account != emailA {
			t.Fatalf("expected selecting an account with failed usage to still reach RUN MODE, got %+v", frame)
		}
	})

	t.Run("the indicator color follows the configured thresholds, never a hardcoded one", func(t *testing.T) {
		// Same shared usageColorFor cpro status/watch use, so a configured
		// Warning/Danger threshold applies here identically.
		orig := []string{barColorSafe, warningColor, dangerColor}
		origWarn, origDanger := barWarnThreshold, barDangerThreshold
		t.Cleanup(func() {
			barColorSafe, warningColor, dangerColor = orig[0], orig[1], orig[2]
			barWarnThreshold, barDangerThreshold = origWarn, origDanger
		})
		barColorSafe, warningColor, dangerColor = "#111111", "#222222", "#333333"
		barWarnThreshold, barDangerThreshold = 80, 90

		for _, tc := range []struct {
			session float64
			want    string
		}{
			{79, "#111111"}, {84, "#222222"}, {94, "#333333"},
		} {
			got := runAccountSessionCell(true, runAccountUsage{loaded: true, session: tc.session})
			if want := ansiTrueColor(t, tc.want); !strings.Contains(got, want) {
				t.Fatalf("session %.0f%%: expected color %s (%s), got %q", tc.session, tc.want, want, got)
			}
		}
	})

	t.Run("search keeps each row's usage and never refetches", func(t *testing.T) {
		m := newModel()
		cmd, err := m.pushRunAccount()
		if err != nil {
			t.Fatal(err)
		}
		if cmd == nil {
			t.Fatal("expected a first visit to start usage fetches")
		}
		m.Update(runAccountUsageMsg{email: emailB, usage: runAccountUsage{loaded: true, session: 79, week: 79}})

		// Type to narrow down to b, then confirm the percentage survived into
		// the filtered view.
		m.updateRunAccount(tea.KeyPressMsg{Code: 'b', Text: "b"})
		if view := m.viewRunAccountSearch(); !strings.Contains(view, "79%") {
			t.Fatalf("expected the filtered row to keep its usage, got %q", view)
		}
		// Re-entering the frame with everything already cached must issue no
		// further fetches at all — the spec's "searching must not refetch" and
		// "don't introduce a second cache" requirements.
		m.Update(runAccountUsageMsg{email: emailA, usage: runAccountUsage{loaded: true, session: 5, week: 5}})
		m.stack.pop()
		again, err := m.pushRunAccount()
		if err != nil {
			t.Fatal(err)
		}
		if again != nil {
			t.Fatal("expected no refetch when every account's usage is already cached")
		}
	})

	t.Run("usage is looked up by the real account, never a masked alias", func(t *testing.T) {
		origEnabled, origTable := maskEmailEnabled, emailMaskTable
		t.Cleanup(func() { maskEmailEnabled, emailMaskTable = origEnabled, origTable })
		const alias = "placeholder-fox"
		maskEmailEnabled, emailMaskTable = true, map[string]string{emailA: alias, emailB: "placeholder-owl"}
		seedUsage(t, emailA, 42)

		m := newModel()
		cmd, err := m.pushRunAccount()
		if err != nil {
			t.Fatal(err)
		}
		if cmd == nil {
			t.Fatal("expected usage fetches to start")
		}
		// Run the batch for real: it resolves profiles from the cache seeded
		// above under the REAL email. If anything in that path used the alias
		// instead, the seeded value could not come back.
		msg := cmd()
		var delivered []runAccountUsageMsg
		switch v := msg.(type) {
		case runAccountUsageMsg:
			delivered = append(delivered, v)
		case tea.BatchMsg:
			for _, c := range v {
				if got, ok := c().(runAccountUsageMsg); ok {
					delivered = append(delivered, got)
				}
			}
		default:
			t.Fatalf("unexpected message type %T", msg)
		}
		var sawA bool
		for _, d := range delivered {
			m.Update(d)
			if d.email == emailA {
				sawA = true
				if d.usage.failed || d.usage.session != 42 {
					t.Fatalf("expected the real account's seeded usage, got %+v", d.usage)
				}
			}
		}
		if !sawA {
			t.Fatalf("expected a usage result keyed by the real email, got %+v", delivered)
		}
		view := m.viewRunAccount()
		if !strings.Contains(view, alias) || strings.Contains(view, emailA) {
			t.Fatalf("expected the masked alias on screen, not the real email, got %q", view)
		}
		if !strings.Contains(view, "42%") {
			t.Fatalf("expected the real account's usage rendered beside its alias, got %q", view)
		}
	})

	t.Run("rows stay aligned and degrade without wrapping on a narrow terminal", func(t *testing.T) {
		m := newModel()
		if _, err := m.pushRunAccount(); err != nil {
			t.Fatal(err)
		}
		m.Update(runAccountUsageMsg{email: emailA, usage: runAccountUsage{loaded: true, session: 7, week: 7}})
		m.Update(runAccountUsageMsg{email: emailB, usage: runAccountUsage{loaded: true, session: 100, week: 100}})

		// Wide: every row is the same visible width, so the S column lines up
		// even though "7%" and "100%" differ in length.
		m.width = 120
		rows := panelRows(t, m.viewRunAccount())
		if len(rows) != 2 {
			t.Fatalf("expected exactly 2 account rows, got %d: %+v", len(rows), rows)
		}
		if visibleWidth(rows[0]) != visibleWidth(rows[1]) {
			t.Fatalf("expected aligned rows, got widths %d and %d: %q / %q",
				visibleWidth(rows[0]), visibleWidth(rows[1]), rows[0], rows[1])
		}
		for _, row := range rows {
			if strings.Contains(row, "\t") {
				t.Fatalf("expected no tabs in a row, got %q", row)
			}
		}

		// Narrow: the percentage outlives the "S" label and the indicator, and
		// no row ever wraps onto a second line.
		m.width = 22
		narrow := panelRows(t, m.viewRunAccount())
		if len(narrow) != 2 {
			t.Fatalf("expected 2 rows even when narrow, got %d: %+v", len(narrow), narrow)
		}
		if !strings.Contains(narrow[1], "100%") {
			t.Fatalf("expected the percentage to survive a narrow terminal, got %q", narrow[1])
		}
		for _, row := range narrow {
			if visibleWidth(row) > m.width {
				t.Fatalf("row exceeds terminal width %d: %q (%d cols)", m.width, row, visibleWidth(row))
			}
		}
	})
}

// panelRows extracts a rendered panel's own account rows from a view — the
// "│ "-prefixed body lines, excluding the panel's top/bottom edges and
// everything below it (description, footer) — so an alignment assertion
// measures only the rows themselves.
func panelRows(t *testing.T, view string) []string {
	t.Helper()
	var rows []string
	for _, line := range strings.Split(view, "\n") {
		plain := stripANSI(line)
		if !strings.HasPrefix(plain, currentTheme.Rail()) {
			continue
		}
		rows = append(rows, strings.TrimRight(strings.TrimPrefix(plain, currentTheme.Rail()), " "))
	}
	return rows
}

// TestAccountPickerUsageEverywhere covers IMPROVEMENTS.md item 3: the
// invariant that every picker where the user must choose an account shows
// the same live Session usage, through the same component — used to hold
// only for RUN ACCOUNT (rootui.go). The other three (system export's SELECT
// ACCOUNT, --resume's RESUME ACCOUNT, and session's DESTINATION ACCOUNT) now
// render through the same accountPickerRow/accountListLines and fetch
// through the same fetchAccountPickerUsage, so this pins the behavior on all
// three. cpro config's own SET DEFAULT ACCOUNT screen (configui.go) is
// deliberately not one of these: it edits config.Default as a settings row,
// not a run-time choice, and carries no live usage of its own. Pure, no pty
// — same reasoning as TestRunAccountUsage.
func TestAccountPickerUsageEverywhere(t *testing.T) {
	_, s := buildCLI(t)
	const emailA, emailB = "a@example.com", "b@example.com"

	t.Run("SELECT ACCOUNT renders and updates live usage", func(t *testing.T) {
		m := &accountPickerApp{s: s, color: false, browseList: newAccountBrowseList([]string{emailA, emailB}), cfg: exportPickerConfig}
		if cmd := m.Init(); cmd == nil {
			t.Fatal("expected the export picker to start Session-usage fetches")
		}
		view := m.viewNormal()
		for _, email := range []string{emailA, emailB} {
			if !strings.Contains(view, email) {
				t.Fatalf("expected %q to render, got %q", email, view)
			}
		}
		if strings.Count(view, "--") != 4 {
			t.Fatalf("expected a placeholder for both accounts before usage arrives, got %q", view)
		}
		m.Update(runAccountUsageMsg{email: emailB, usage: runAccountUsage{loaded: true, session: 79, week: 79}})
		view = m.viewNormal()
		if !strings.Contains(view, "79%") {
			t.Fatalf("expected b's delivered percentage, got %q", view)
		}
		if strings.Count(view, "--") != 2 {
			t.Fatalf("expected a's placeholder to survive b's update, got %q", view)
		}
	})

	t.Run("RESUME ACCOUNT renders and updates live usage", func(t *testing.T) {
		m := &accountPickerApp{s: s, color: false, browseList: newAccountBrowseList([]string{emailA, emailB}), cfg: resumePickerConfig}
		if cmd := m.Init(); cmd == nil {
			t.Fatal("expected the resume picker to start Session-usage fetches")
		}
		m.Update(runAccountUsageMsg{email: emailB, usage: runAccountUsage{loaded: true, session: 79, week: 79}})
		view := m.viewNormal()
		if !strings.Contains(view, "79%") {
			t.Fatalf("expected b's delivered percentage, got %q", view)
		}
		if strings.Count(view, "--") != 2 {
			t.Fatalf("expected a's placeholder to survive b's update, got %q", view)
		}
	})

	t.Run("DESTINATION ACCOUNT renders and updates live usage, fetched on entry", func(t *testing.T) {
		c := config{Accounts: map[string]bool{emailA: true, emailB: true}}
		m := &sessionApp{s: s, c: c, color: false, stack: newNavStack(screenContinuePicker)}
		owner := sessionEntry{email: emailA, dirName: projectDirName("/home/user/proj"), sessionID: "11111111-1111-4111-8111-111111111111"}
		if cmd := m.selectSession(&owner); cmd == nil {
			t.Fatal("expected entering DESTINATION ACCOUNT to start that account's usage fetch")
		}
		if *m.stack.current() != screenContinueAccount {
			t.Fatalf("expected screenContinueAccount, got %v", *m.stack.current())
		}
		m.Update(runAccountUsageMsg{email: emailB, usage: runAccountUsage{loaded: true, session: 79, week: 79}})
		if view := m.viewAccountBrowse(); !strings.Contains(view, "79%") {
			t.Fatalf("expected the destination account's delivered percentage, got %q", view)
		}
		// Search is pure presentation and must not drop the already-fetched
		// usage (the same "filtering never refetches" rule RUN ACCOUNT keeps).
		m.account.typeRune("b")
		if view := m.viewAccountSearch(); !strings.Contains(view, "79%") {
			t.Fatalf("expected the filtered row to keep its usage, got %q", view)
		}
	})
}

// TestListScrollingEverywhere covers IMPROVEMENTS.md item 2: the scroll window
// that keeps the highlighted row on screen existed only in the root command
// picker; the other list screens let Down move the selection off the bottom
// edge, where Enter then had no visible target. These now go through the
// shared scrollLines (browseui.go). Pure rendering at a deliberately short
// terminal height, so it is deterministic and needs no pty.
func TestListScrollingEverywhere(t *testing.T) {
	const visible = 3 // rows the short terminal below can show
	height := listChromeLines + visible

	emails := make([]string, 12)
	for i := range emails {
		emails[i] = fmt.Sprintf("acct%02d@example.com", i)
	}

	t.Run("SELECT ACCOUNT (export)", func(t *testing.T) {
		m := &accountPickerApp{color: false, browseList: newAccountBrowseList(emails), cfg: exportPickerConfig}
		m.height = height
		if rows := panelRows(t, m.viewNormal()); len(rows) != visible {
			t.Fatalf("expected %d visible rows, got %d: %v", visible, len(rows), rows)
		}
		if view := m.viewNormal(); !strings.Contains(view, emails[0]) || strings.Contains(view, emails[len(emails)-1]) {
			t.Fatalf("cursor at the top should show the first rows only, got %q", view)
		}
		m.cursor = len(emails) - 1
		if view := m.viewNormal(); !strings.Contains(view, emails[len(emails)-1]) || strings.Contains(view, emails[0]) {
			t.Fatalf("cursor at the bottom should scroll the last row into view, got %q", view)
		}
	})

	t.Run("RESUME ACCOUNT", func(t *testing.T) {
		m := &accountPickerApp{color: false, browseList: newAccountBrowseList(emails), cfg: resumePickerConfig}
		m.height = height
		m.cursor = len(emails) - 1
		view := m.viewNormal()
		if rows := panelRows(t, view); len(rows) != visible {
			t.Fatalf("expected %d visible rows, got %d: %v", visible, len(rows), rows)
		}
		if !strings.Contains(view, emails[len(emails)-1]) || strings.Contains(view, emails[0]) {
			t.Fatalf("cursor at the bottom should scroll the last row into view, got %q", view)
		}
	})

	t.Run("SESSION LIST", func(t *testing.T) {
		entries := make([]sessionEntry, 12)
		for i := range entries {
			entries[i] = sessionEntry{
				email:     "owner@example.com",
				dirName:   projectDirName(fmt.Sprintf("/home/user/proj%02d", i)),
				sessionID: fmt.Sprintf("%08d-0000-4000-8000-000000000000", i),
			}
		}
		m := &sessionApp{color: false, stack: newNavStack(screenSessionList), pickerMode: "list"}
		m.picker = sessionListState{browseList: newSessionBrowseList(entries)}
		m.picker.cursor = len(entries) - 1
		m.height = height
		view := m.viewPickerBrowse()
		if rows := panelRows(t, view); len(rows) != visible {
			t.Fatalf("expected %d visible rows, got %d: %v", visible, len(rows), rows)
		}
		last := projectDisplayName(entries[len(entries)-1].dirName)
		first := projectDisplayName(entries[0].dirName)
		if !strings.Contains(view, last) || strings.Contains(view, first) {
			t.Fatalf("cursor at the bottom should scroll %q into view and %q out, got %q", last, first, view)
		}
	})
}

// TestBrowseListKeys covers the shared browse/search state machine directly
// (browseui.go) — the one component every list screen now delegates to — so
// its own contract (wrapping movement, the "backspace to empty leaves search
// mode" rule, the → forward gate, and the ranked-vs-plain filter split) is
// pinned independently of any one screen's rendering.
func TestBrowseListKeys(t *testing.T) {
	t.Run("account lists use a plain substring match and wrap both ways", func(t *testing.T) {
		b := newAccountBrowseList([]string{"a@example.com", "b@example.com", "c@example.com"})
		b.move(1)
		if item, _ := b.selected(); item != "b@example.com" {
			t.Fatalf("expected the down move to land on b, got %q", item)
		}
		b.move(-1)
		b.move(-1)
		if item, _ := b.selected(); item != "c@example.com" {
			t.Fatalf("expected up from the first row to wrap to the last, got %q", item)
		}
		b.typeRune("B")
		if !b.searching() || len(b.filtered) != 1 {
			t.Fatalf("expected a case-insensitive single match, got query=%q filtered=%v", b.query, b.filtered)
		}
		if item, _ := b.searchSelected(); item != "b@example.com" {
			t.Fatalf("expected the search selection to be b, got %q", item)
		}
		// Backspacing to empty must leave search mode entirely, restoring the
		// full browse list rather than an empty result set.
		b.backspace()
		if b.searching() || b.filtered != nil {
			t.Fatalf("expected an emptied query to leave search mode, got query=%q filtered=%v", b.query, b.filtered)
		}
	})

	t.Run("the command list's → only acts on forward entries", func(t *testing.T) {
		b := newCommandBrowseList(rootPickerEntries)
		leaf, forward := -1, -1
		for i, e := range b.items {
			if e.name == "status" {
				leaf = i
			}
			if e.name == "config" {
				forward = i
			}
		}
		opts := browseKeyOpts[rootPickerEntry]{forward: rootForward}
		b.cursor = leaf
		if got := b.browseKey(tea.KeyPressMsg{Code: tea.KeyRight}, opts); got != browseNoop {
			t.Fatalf("expected → on the leaf row status to do nothing, got %v", got)
		}
		b.cursor = forward
		if got := b.browseKey(tea.KeyPressMsg{Code: tea.KeyRight}, opts); got != browseSelect {
			t.Fatalf("expected → on config to select it, got %v", got)
		}
		// Enter selects either kind — only → is gated.
		b.cursor = leaf
		if got := b.browseKey(tea.KeyPressMsg{Code: tea.KeyEnter}, opts); got != browseSelect {
			t.Fatalf("expected Enter on a leaf row to select it, got %v", got)
		}
	})

	t.Run("Esc only ever clears the query while searching", func(t *testing.T) {
		b := newAccountBrowseList([]string{"a@example.com"})
		b.typeRune("a")
		if got := b.searchKey(tea.KeyPressMsg{Code: tea.KeyEscape}, browseKeyOpts[string]{}); got != browseConsumed {
			t.Fatalf("expected Esc in search mode to be consumed as a clear, got %v", got)
		}
		if b.searching() {
			t.Fatal("expected Esc to have cleared the query")
		}
	})
}

// TestWatchModeFlow covers decision 0029's WATCH MODE screen directly, no
// pty — the same reasoning TestInteractiveRunFlow/TestRootPickerExitArmed
// already give: bubbletea's diffing renderer makes a same-screen cursor move
// unreliable to prove via a cumulative pty capture, so pushWatchMode/
// updateWatchMode/viewWatchMode are driven and rendered directly instead.
func TestWatchModeFlow(t *testing.T) {
	newModel := func() *rootPickerApp {
		root := rootCommand()
		root.InitDefaultHelpCmd()
		root.InitDefaultCompletionCmd()
		entries := rootPickerFilteredEntries(root, rootLauncherNames)
		m := &rootPickerApp{root: root, shades: deriveAccentShades("#A78BFA", 5)}
		m.stack = newNavStack(rootFrame{kind: frameList, list: newCommandBrowseList(entries)})
		return m
	}
	enterKey := tea.KeyPressMsg{Code: tea.KeyEnter}
	rightKey := tea.KeyPressMsg{Code: tea.KeyRight}
	leftKey := tea.KeyPressMsg{Code: tea.KeyLeft}
	escKey := tea.KeyPressMsg{Code: tea.KeyEsc}
	downKey := tea.KeyPressMsg{Code: tea.KeyDown}
	upKey := tea.KeyPressMsg{Code: tea.KeyUp}

	findCursor := func(m *rootPickerApp, name string) int {
		for i, e := range m.stack.current().list.items {
			if e.name == name {
				return i
			}
		}
		t.Fatalf("%q not found in launcher entries", name)
		return -1
	}

	t.Run("Enter on watch opens WATCH MODE, not an immediate run", func(t *testing.T) {
		m := newModel()
		m.stack.current().list.cursor = findCursor(m, "watch")
		m.updateNormal(enterKey)
		if m.stack.current().kind != frameWatchMode {
			t.Fatalf("expected Enter on watch to push frameWatchMode, got kind %v", m.stack.current().kind)
		}
		if m.picked != nil {
			t.Fatal("expected nothing picked yet — WATCH MODE itself decides the argv")
		}
	})

	t.Run("Right Arrow on watch also opens WATCH MODE", func(t *testing.T) {
		m := newModel()
		m.stack.current().list.cursor = findCursor(m, "watch")
		m.updateNormal(rightKey)
		if m.stack.current().kind != frameWatchMode {
			t.Fatalf("expected -> on watch to push frameWatchMode, got kind %v", m.stack.current().kind)
		}
	})

	t.Run("Full (default) selection builds plain \"watch\"", func(t *testing.T) {
		m := newModel()
		m.pushWatchMode()
		_, cmd := m.updateWatchMode(enterKey)
		if cmd == nil {
			t.Fatal("expected finalizing to quit")
		}
		want := []string{"watch"}
		if !reflect.DeepEqual(m.picked, want) {
			t.Fatalf("picked = %v, want %v", m.picked, want)
		}
	})

	t.Run("Compact selection builds \"watch --compact\"", func(t *testing.T) {
		m := newModel()
		m.pushWatchMode()
		m.updateWatchMode(downKey)
		m.updateWatchMode(enterKey)
		want := []string{"watch", "--compact"}
		if !reflect.DeepEqual(m.picked, want) {
			t.Fatalf("picked = %v, want %v", m.picked, want)
		}
	})

	t.Run("Right Arrow on Compact also builds \"watch --compact\"", func(t *testing.T) {
		m := newModel()
		m.pushWatchMode()
		m.updateWatchMode(downKey)
		m.updateWatchMode(rightKey)
		want := []string{"watch", "--compact"}
		if !reflect.DeepEqual(m.picked, want) {
			t.Fatalf("picked = %v, want %v", m.picked, want)
		}
	})

	t.Run("cursor wraps and defaults to Full", func(t *testing.T) {
		m := newModel()
		m.pushWatchMode()
		if m.stack.current().cursor != 0 {
			t.Fatalf("expected WATCH MODE to default to Full (cursor 0), got %d", m.stack.current().cursor)
		}
	})

	t.Run("Esc pops back to ROOT", func(t *testing.T) {
		m := newModel()
		m.pushWatchMode()
		m.updateWatchMode(escKey)
		if m.stack.current().kind != frameList {
			t.Fatalf("expected Esc to pop back to the root frame list, got kind %v", m.stack.current().kind)
		}
		if m.picked != nil {
			t.Fatal("expected nothing picked after backing out")
		}
	})

	t.Run("Left Arrow pops back to ROOT", func(t *testing.T) {
		m := newModel()
		m.pushWatchMode()
		m.updateWatchMode(leftKey)
		if m.stack.current().kind != frameList {
			t.Fatalf("expected <- to pop back to the root frame list, got kind %v", m.stack.current().kind)
		}
	})

	// Decision 0045: WATCH MODE gained an Interval row so `--interval` is
	// reachable from the picker, without persisting anything.
	t.Run("the Interval row steps the interval and builds --interval", func(t *testing.T) {
		m := newModel()
		m.pushWatchMode()
		m.updateWatchMode(downKey) // Full -> Compact
		m.updateWatchMode(downKey) // Compact -> Interval
		if got := m.stack.current().cursor; got != len(watchModeItems) {
			t.Fatalf("expected the cursor on the Interval row (%d), got %d", len(watchModeItems), got)
		}
		// Enter on the Interval row must not accidentally start a watch.
		if _, cmd := m.updateWatchMode(enterKey); cmd != nil || m.picked != nil {
			t.Fatalf("expected Enter on the Interval row to do nothing, got picked=%v cmd=%v", m.picked, cmd)
		}
		m.updateWatchMode(rightKey) // 1m -> 5m
		if got := m.stack.current().interval; got != 5*time.Minute {
			t.Fatalf("expected -> to step the interval to 5m, got %s", got)
		}
		m.updateWatchMode(leftKey) // back to 1m
		m.updateWatchMode(leftKey) // 30s
		m.updateWatchMode(leftKey) // 15s
		if got := m.stack.current().interval; got != 15*time.Second {
			t.Fatalf("expected <- to step down to 15s, got %s", got)
		}
		m.updateWatchMode(upKey) // Interval -> Compact
		m.updateWatchMode(upKey) // Compact -> Full
		m.updateWatchMode(enterKey)
		if want := []string{"watch", "--interval", "15s"}; !reflect.DeepEqual(m.picked, want) {
			t.Fatalf("picked = %v, want %v", m.picked, want)
		}
	})

	t.Run("the interval clamps at 5s and 15m and the default is omitted from the argv", func(t *testing.T) {
		m := newModel()
		m.pushWatchMode()
		m.updateWatchMode(downKey)
		m.updateWatchMode(downKey) // Interval row
		for range 10 {
			m.updateWatchMode(rightKey)
		}
		if got := m.stack.current().interval; got != 15*time.Minute {
			t.Fatalf("expected the top of the range to be 15m, got %s", got)
		}
		for range 10 {
			m.updateWatchMode(leftKey)
		}
		if got := m.stack.current().interval; got != 5*time.Second {
			t.Fatalf("expected the bottom of the range to be 5s, got %s", got)
		}
		// A plain Full start at the default still builds the bare argv.
		m = newModel()
		m.pushWatchMode()
		m.updateWatchMode(enterKey)
		if want := []string{"watch"}; !reflect.DeepEqual(m.picked, want) {
			t.Fatalf("picked = %v, want %v", m.picked, want)
		}
	})

	t.Run("Compact plus a non-default interval carries both flags", func(t *testing.T) {
		m := newModel()
		m.pushWatchMode()
		m.updateWatchMode(downKey)
		m.updateWatchMode(downKey) // Interval row
		m.updateWatchMode(rightKey)
		m.updateWatchMode(rightKey) // 1m -> 5m -> 15m
		m.updateWatchMode(upKey)    // Compact
		m.updateWatchMode(enterKey)
		if want := []string{"watch", "--compact", "--interval", "15m"}; !reflect.DeepEqual(m.picked, want) {
			t.Fatalf("picked = %v, want %v", m.picked, want)
		}
	})
}

// TestInteractiveRunFlowEndToEnd drives the full STEP1->4 flow (bare cpro ->
// "run" -> RUN ACCOUNT -> RUN MODE -> execute) through a real pty end to
// end, reaching the fake claude helper (TestClaudeProcess) with the expected
// argv and printing the decision-0007 announce line first — the strongest
// form of "the interactive flow uses the exact same command builder as cpro
// run" (TestInteractiveRunFlow's own unit tests already check the argv
// construction in isolation; this checks the whole thing actually runs).
func TestInteractiveRunFlowEndToEnd(t *testing.T) {
	bin, s := buildCLI(t)
	master, slave := openPTY(t)
	capture := drainPTY(master)

	loginCmd := exec.Command(bin, "login", "a@example.com")
	loginCmd.Stdin = slave
	if out, err := loginCmd.CombinedOutput(); err != nil {
		t.Fatalf("login: %s %v", out, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin)
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	// The interactive picker renders to stderr (tea.WithOutput, rootui.go), so
	// only stdin/stderr need the pty; stdout stays clean for announceCommand's
	// own "Running: ..." lines and the fake claude's JSON output that follows
	// once the picker's tea.Program exits and the real invocation execs —
	// exactly the separation TestPermissionsRunIntegration's own non-pty
	// `cpro run` checks rely on, just with a pty in front for the picker.
	cmd.Stdin, cmd.Stderr = slave, slave
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)

	send := func(s string) {
		if _, err := master.WriteString(s); err != nil {
			t.Fatal(err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	right := func() { send("\x1b[C") }
	down := func() { send("\x1b[B") }
	enter := func() { send("\r") }

	right() // "run" is highlighted by default (STEP 1) -> opens RUN ACCOUNT (STEP 2)
	time.Sleep(300 * time.Millisecond)
	enter() // the only registered account -> RUN MODE (STEP 3), preselected "ask"
	time.Sleep(300 * time.Millisecond)
	for range 4 { // ask -> edit -> readonly -> live -> yolo
		down()
	}
	// STEP 4: → launches Claude too (decision 0022), not just Enter — end to
	// end proof alongside TestInteractiveRunFlow's own pure Right Arrow checks.
	right()

	if err := cmd.Wait(); err != nil {
		t.Fatalf("interactive run flow: %v; stderr %q; stdout %q", err, stripANSI(capture()), stdout.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "Running:") {
		t.Fatalf("expected the decision-0007 announce line, got %q", out)
	}
	if !strings.Contains(out, "cpro run --account a@example.com --permission-mode bypassPermissions") {
		t.Fatalf("expected the announced command to match the actual invocation, got %q", out)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	var forwarded struct {
		Args      []string
		Directory string
	}
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &forwarded); err != nil {
		t.Fatalf("parsing fake claude output: %s: %v", out, err)
	}
	if !reflect.DeepEqual(forwarded.Args, yoloArgs()) {
		t.Fatalf("expected yolo's argv forwarded to claude, got %v", forwarded.Args)
	}
	if forwarded.Directory != s.profile("a@example.com") {
		t.Fatalf("expected a@example.com's profile directory, got %q", forwarded.Directory)
	}
}

// TestRootPickerScore covers the search ranking rules (rootui.go) directly:
// exact/prefix/substring/fuzzy name matches outrank short-metadata matches,
// which outrank description-only matches, and the spec's own worked examples
// hold exactly: "co" -> config+completion only, "conf" -> config alone,
// "comp" -> completion alone, "shell" -> completion (via its metadata, not
// its name), "log" -> login+logout (by name prefix).
func TestRootPickerScore(t *testing.T) {
	matchNames := func(query string) []string {
		var names []string
		for _, e := range rootPickerEntries {
			if _, ok := rootPickerScore(e, strings.ToLower(query)); ok {
				names = append(names, e.name)
			}
		}
		return names
	}
	hasAll := func(got []string, want ...string) bool {
		for _, w := range want {
			found := false
			for _, g := range got {
				if g == w {
					found = true
				}
			}
			if !found {
				return false
			}
		}
		return true
	}

	if got := matchNames("co"); len(got) != 2 || !hasAll(got, "config", "completion") {
		t.Fatalf(`"co" should match config and completion (both contain it as a name prefix), got %v`, got)
	}
	if got := matchNames("conf"); len(got) != 1 || got[0] != "config" {
		t.Fatalf(`"conf" should match only config, got %v`, got)
	}
	if got := matchNames("comp"); len(got) != 1 || got[0] != "completion" {
		t.Fatalf(`"comp" should match only completion, got %v`, got)
	}
	if got := matchNames("shell"); len(got) != 1 || got[0] != "completion" {
		t.Fatalf(`"shell" should match completion via its "Shell setup" metadata, got %v`, got)
	}
	if got := matchNames("usage"); len(got) != 2 || !hasAll(got, "status", "watch") {
		t.Fatalf(`"usage" should match status and watch via their metadata, got %v`, got)
	}
	if got := matchNames("log"); len(got) != 2 || !hasAll(got, "login", "logout") {
		t.Fatalf(`"log" should match login and logout, got %v`, got)
	}
	// "list" was dropped from both pickers (still a real, directly-invocable
	// command — just no longer picker-visible), taking its "Accounts"
	// metadata out of the searchable set with it.
	if got := matchNames("accounts"); len(got) != 0 {
		t.Fatalf(`"accounts" should match nothing now that list is picker-invisible, got %v`, got)
	}

	exact, _ := rootPickerScore(rootPickerEntry{name: "run", description: "x"}, "run")
	prefix, _ := rootPickerScore(rootPickerEntry{name: "run", description: "x"}, "ru")
	substr, _ := rootPickerScore(rootPickerEntry{name: "trun", description: "x"}, "run")
	fuzzy, _ := rootPickerScore(rootPickerEntry{name: "rowuntil", description: "x"}, "run") // "r","u","n" appear in order, not contiguously
	metaOnly, _ := rootPickerScore(rootPickerEntry{name: "zzz", shortLabel: "has run in it"}, "run")
	descOnly, _ := rootPickerScore(rootPickerEntry{name: "zzz", description: "has run in it"}, "run")
	if !(exact > prefix && prefix > substr && substr > fuzzy && fuzzy > metaOnly && metaOnly > descOnly) {
		t.Fatalf("ranking order violated: exact=%d prefix=%d substr=%d fuzzy=%d metaOnly=%d descOnly=%d",
			exact, prefix, substr, fuzzy, metaOnly, descOnly)
	}
	if _, ok := rootPickerScore(rootPickerEntry{name: "zzz", description: "yyy"}, "run"); ok {
		t.Fatal("a query with no match anywhere must not match")
	}
}

// TestSubsequenceMatch covers subsequenceMatch (rootui.go) directly: cpro has
// no fuzzy-matching library to reuse, so this small stand-in is exercised on
// its own rather than only indirectly through rootPickerScore.
func TestSubsequenceMatch(t *testing.T) {
	if !subsequenceMatch("config", "cfg") {
		t.Fatal(`"cfg" should be a subsequence of "config"`)
	}
	if subsequenceMatch("config", "gcf") {
		t.Fatal(`"gcf" is not in order in "config", should not match`)
	}
	if !subsequenceMatch("anything", "") {
		t.Fatal("an empty query should trivially match, like Contains/HasPrefix do")
	}
}

// TestRootPickerRefilter covers refilter's ranking/ordering and its fcursor
// clamp when a shrinking query drops the previously highlighted result.
func TestRootPickerRefilter(t *testing.T) {
	m := &rootPickerApp{}
	m.stack = newNavStack(rootFrame{kind: frameList, list: newCommandBrowseList(rootPickerEntries)})
	m.stack.current().list.query = "co"
	m.stack.current().list.refilter()
	// config and completion both match "co" as a name *prefix*, the same
	// tier, so a stable sort keeps them in rootPickerEntries' own table
	// order. See TestRootPickerScore for the tiers themselves.
	if len(m.stack.current().list.filtered) != 2 {
		t.Fatalf(`expected 2 results for "co", got %d: %v`, len(m.stack.current().list.filtered), m.stack.current().list.filtered)
	}
	entries := m.stack.current().list.items
	gotOrder := []string{
		entries[m.stack.current().list.filtered[0]].name,
		entries[m.stack.current().list.filtered[1]].name,
	}
	if !reflect.DeepEqual(gotOrder, []string{"config", "completion"}) {
		t.Fatalf("expected config before completion (table order, tied prefix score), got %v", gotOrder)
	}
	m.stack.current().list.fcursor = 1
	m.stack.current().list.query = "conf"
	m.stack.current().list.refilter()
	if len(m.stack.current().list.filtered) != 1 || m.stack.current().list.fcursor != 0 {
		t.Fatalf("expected fcursor to clamp to 0 on a single-result set, got fcursor=%d filtered=%v", m.stack.current().list.fcursor, m.stack.current().list.filtered)
	}
}

// TestDeriveAccentShades covers tui.go's shared gradient helper directly:
// shade count, a monotonic dark-to-light progression that lands exactly on
// the input accent color at the brightest end, and that different accent
// colors (Purple vs. Blue vs. Rose) produce entirely different — but each
// internally consistent — shade sets, rather than one hardcoded palette.
func TestDeriveAccentShades(t *testing.T) {
	brightness := func(hex string) int {
		var r, g, b int
		if _, err := fmt.Sscanf(hex, "#%02X%02X%02X", &r, &g, &b); err != nil {
			t.Fatalf("not a #RRGGBB color: %q: %v", hex, err)
		}
		return r + g + b
	}

	for _, accent := range []string{"#A78BFA", "#60A5FA", "#FB7185"} {
		shades := deriveAccentShades(accent, 5)
		if len(shades) != 5 {
			t.Fatalf("%s: expected 5 shades, got %d: %v", accent, len(shades), shades)
		}
		if shades[4] != accent {
			t.Fatalf("%s: brightest/last shade should be the accent color itself unchanged, got %q", accent, shades[4])
		}
		seen := map[string]bool{}
		for i, s := range shades {
			if seen[s] {
				t.Fatalf("%s: shade %d (%q) duplicates an earlier shade: %v", accent, i, s, shades)
			}
			seen[s] = true
			if i > 0 && brightness(shades[i-1]) >= brightness(s) {
				t.Fatalf("%s: shades should strictly brighten from index 0 to 4, got %v", accent, shades)
			}
		}
	}

	purple := deriveAccentShades("#A78BFA", 5)
	blue := deriveAccentShades("#60A5FA", 5)
	for i := range purple {
		if purple[i] == blue[i] {
			t.Fatalf("Purple and Blue should never produce the same shade at index %d: %q", i, purple[i])
		}
	}

	if got := deriveAccentShades("#A78BFA", 1); len(got) != 1 || got[0] != "#A78BFA" {
		t.Fatalf("n<=1 should return the accent color unchanged, got %v", got)
	}
}

// TestRootRowResponsive covers rootRow's metadata degradation directly (no
// pty needed): full metadata at ample width, a truncated "…" form once
// space is tight, and no metadata at all — never a lone ellipsis — once
// there's essentially no room, while the command name and cursor slot are
// never touched at any width.
func TestRootRowResponsive(t *testing.T) {
	entry := rootPickerEntry{name: "completion", shortLabel: "Shell setup"}
	nameWidth := 10 // len("completion")

	m := &rootPickerApp{color: false, width: 0} // unknown width: never degrades
	if got := m.rootRow(entry, false, nameWidth); !strings.Contains(got, "Shell setup") {
		t.Fatalf("unknown width should show metadata in full, got %q", got)
	}

	// rail(1) + marker(4) + nameWidth(10) + arrow slot(2) + gap(3) = 20
	// columns of overhead before metadata even starts; +6 leaves just enough
	// room for a truncated, but non-empty, "Shell…"-style metadata.
	m.width = 20 + 6
	got := m.rootRow(entry, false, nameWidth)
	if !strings.Contains(got, "completion") {
		t.Fatalf("the command name must never be dropped, got %q", got)
	}
	if !strings.HasSuffix(got, "…") || strings.Contains(got, "Shell setup") {
		t.Fatalf("expected truncated metadata ending in an ellipsis, got %q", got)
	}

	m.width = 20 + 1 // budget < 2: metadata must disappear entirely, not shrink to "…" alone
	got = m.rootRow(entry, false, nameWidth)
	if !strings.Contains(got, "completion") {
		t.Fatalf("the command name must never be dropped, got %q", got)
	}
	if strings.Contains(got, "…") || strings.Contains(got, "Shell") {
		t.Fatalf("expected metadata hidden entirely at this width, got %q", got)
	}

	// Cursor/name alignment: a selected and an unselected row must place the
	// command name at the same visible column (the marker slot is
	// fixed-width) — measured via visibleWidth, not a byte index, since "❯"
	// is a multi-byte rune but only one column wide.
	m.width = 0
	sel := m.rootRow(entry, true, nameWidth)
	unsel := m.rootRow(entry, false, nameWidth)
	selCol := visibleWidth(strings.SplitN(sel, "completion", 2)[0])
	unselCol := visibleWidth(strings.SplitN(unsel, "completion", 2)[0])
	if selCol != unselCol {
		t.Fatalf("selected/unselected rows should align the command name at the same column: sel=%d unsel=%d", selCol, unselCol)
	}
}

// TestApplyLiveMeta covers rootui.go's applyLiveMeta directly: it sets only
// the "default" entry's own permMode and defaultAcct, from a freshly read
// store, and is a silent no-op — leaving every field as it was — when
// there's no store to read from, rather than erroring or panicking (the
// picker's own initial entries build and pushMenu both call it
// unconditionally, including from contexts with no live account store).
func TestApplyLiveMeta(t *testing.T) {
	_, s := buildCLI(t)
	if err := s.update(func(c *config) error {
		c.PermissionMode = "yolo"
		c.Accounts = map[string]bool{"pick@example.com": true}
		c.Default = "pick@example.com"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	entries := []rootPickerEntry{{name: "config"}, {name: "default"}, {name: "login"}}
	applyLiveMeta(entries, s)
	for _, e := range entries {
		if e.name == "default" {
			if e.permMode != "yolo" {
				t.Fatalf("expected default entry's permMode to be set to the saved mode, got %q", e.permMode)
			}
			if e.defaultAcct != "pick@example.com" {
				t.Fatalf("expected default entry's defaultAcct to be set to the saved default, got %q", e.defaultAcct)
			}
			continue
		}
		if e.permMode != "" || e.defaultAcct != "" {
			t.Fatalf("%s: expected permMode/defaultAcct to stay empty, got %+v", e.name, e)
		}
	}

	// Masking is a display preference and defaultAcct is a display string,
	// so the alias is what lands here — never the real address.
	origEnabled, origTable := maskEmailEnabled, emailMaskTable
	t.Cleanup(func() { maskEmailEnabled, emailMaskTable = origEnabled, origTable })
	maskEmailEnabled = true
	emailMaskTable = map[string]string{"pick@example.com": "placeholder-elk"}
	masked := []rootPickerEntry{{name: "default"}}
	applyLiveMeta(masked, s)
	if masked[0].defaultAcct != "placeholder-elk" {
		t.Fatalf("expected the masked alias, got %q", masked[0].defaultAcct)
	}
	maskEmailEnabled, emailMaskTable = origEnabled, origTable

	// An unset default has no account to name: the row falls back to its
	// static shortLabel rather than rendering an empty "● ".
	if err := s.update(func(c *config) error { c.Default = ""; return nil }); err != nil {
		t.Fatal(err)
	}
	unset := []rootPickerEntry{{name: "default"}}
	applyLiveMeta(unset, s)
	if unset[0].defaultAcct != "" {
		t.Fatalf("expected no defaultAcct when none is saved, got %q", unset[0].defaultAcct)
	}

	// No store to read from (e.g. a pure entries build with no live account
	// store) must leave every live field untouched, not panic or error.
	noStore := []rootPickerEntry{{name: "default"}}
	applyLiveMeta(noStore, nil)
	if noStore[0].defaultAcct != "" || noStore[0].permMode != "" {
		t.Fatalf("expected a nil store to be a silent no-op, got %+v", noStore[0])
	}
}

// TestRootRowPermissionsColoredDot covers rootRow's special-case rendering
// for the merged "default" entry: its metadata is "● <mode label>  ∙
// <account>" with the "●" colored to the mode's own risk color (matching
// every other place cpro shows a permission-mode dot), unlike every other
// row's plain dimStyle(shortLabel) — and truncation (at a narrow width)
// still degrades gracefully without corrupting the color escape, since
// rootRow truncates the plain text first and colors only the surviving "●"
// prefix afterward. Direct calls, no pty — a per-row color check is exactly
// the kind of same-cell diff TestRootPickerGradient's own doc comment warns
// is unreliable to verify via a cumulative pty capture.
func TestRootRowPermissionsColoredDot(t *testing.T) {
	mode := permissionModeByKey("yolo")
	entry := rootPickerEntry{name: "default", shortLabel: "Run defaults", permMode: "yolo", defaultAcct: "neku@yenalo"}
	nameWidth := 7 // len("default") — the arrow slot (rootArrowColWidth) is separate

	m := &rootPickerApp{color: true, width: 0} // unknown width: never degrades
	got := m.rootRow(entry, false, nameWidth)
	if !strings.Contains(stripANSI(got), "● "+mode.Label+"  ∙  neku@yenalo") {
		t.Fatalf("expected the live mode and account, not the static shortLabel, got %q", got)
	}
	if strings.Contains(got, "Run defaults") {
		t.Fatalf("expected the static shortLabel to be replaced, got %q", got)
	}
	if !strings.Contains(got, ansiTrueColor(t, mode.Color)) {
		t.Fatalf("expected the ● to carry the mode's own risk color (%s), got %q", mode.Color, got)
	}

	// A live mode with no default account yet (e.g. no account registered):
	// just "● <mode label>", no trailing separator/account.
	modeOnly := rootPickerEntry{name: "default", shortLabel: "Run defaults", permMode: "yolo"}
	got = m.rootRow(modeOnly, false, nameWidth)
	if !strings.Contains(stripANSI(got), "● "+mode.Label) || strings.Contains(got, "∙") {
		t.Fatalf("expected just the mode with no account/separator, got %q", got)
	}

	// An entry with no live permMode at all (e.g. the pure rootPickerEntries
	// test table, or a store read failure) falls back to the plain, dimmed
	// shortLabel like any other row.
	fallback := rootPickerEntry{name: "default", shortLabel: "Run defaults"}
	got = m.rootRow(fallback, false, nameWidth)
	if !strings.Contains(got, "Run defaults") {
		t.Fatalf("expected the static shortLabel fallback with no live permMode, got %q", got)
	}
	if strings.Contains(got, "●") {
		t.Fatalf("expected no ● glyph without a live permMode, got %q", got)
	}

	// Narrow terminal: metadata truncates without corrupting the color
	// escape — the plain text is truncated first, then split into its ●
	// prefix and the (possibly truncated) remainder.
	m.width = 1 /*rail*/ + 4 /*marker*/ + nameWidth + 2 /*arrow slot*/ + 3 /*gap*/ + 4
	got = m.rootRow(entry, false, nameWidth)
	if !strings.Contains(got, ansiTrueColor(t, mode.Color)) {
		t.Fatalf("expected the ● to stay colored even once metadata is truncated, got %q", got)
	}
	if !strings.Contains(got, "…") {
		t.Fatalf("expected truncated metadata ending in an ellipsis at this width, got %q", got)
	}
}

// TestVisibleWindow covers the shared scrolling-viewport helper (browseui.go's
// scrollLines/scrollRange, the one every list screen now uses): unscrolled
// when everything fits (unknown height, or a tall enough one), and, once it
// doesn't, a window that always contains cursorLine, clamped so it never
// scrolls past either end of the list.
func TestVisibleWindow(t *testing.T) {
	lines := make([]string, 20)
	hex := make([]string, 20)
	for i := range lines {
		lines[i] = fmt.Sprintf("line%d", i)
		hex[i] = "#000000"
	}

	if got, _ := scrollLines(0, rootChromeLines, lines, hex, 19); len(got) != len(lines) {
		t.Fatalf("unknown height should show everything, got %d of %d lines", len(got), len(lines))
	}

	// taller than the list: still no scrolling needed
	if got, _ := scrollLines(rootChromeLines+25, rootChromeLines, lines, hex, 5); len(got) != len(lines) {
		t.Fatalf("a tall enough terminal should show everything, got %d of %d lines", len(got), len(lines))
	}

	// room for only 5 of the 20 lines: must scroll
	height := rootChromeLines + 5
	contains := func(got []string, want string) bool {
		for _, g := range got {
			if g == want {
				return true
			}
		}
		return false
	}
	if got, gotHex := scrollLines(height, rootChromeLines, lines, hex, 0); len(got) != 5 || !contains(got, "line0") {
		t.Fatalf("cursor at the top should keep the window at the top, got %v (hex len %d)", got, len(gotHex))
	}
	if got, _ := scrollLines(height, rootChromeLines, lines, hex, 19); len(got) != 5 || !contains(got, "line19") {
		t.Fatalf("cursor at the bottom should keep the window at the bottom, got %v", got)
	}
	if got, _ := scrollLines(height, rootChromeLines, lines, hex, 10); len(got) != 5 || !contains(got, "line10") {
		t.Fatalf("cursor in the middle should still be inside the window, got %v", got)
	}
	// lineHex must stay in lockstep with lines (same slice bounds).
	if got, gotHex := scrollLines(height, rootChromeLines, lines, hex, 10); len(got) != len(gotHex) {
		t.Fatalf("lines and lineHex should have the same length: %d vs %d", len(got), len(gotHex))
	}
}

// TestVisibleWidthIgnoresStyling covers tui.go's shared width helper: a
// styled string's visible width must equal its plain counterpart's, even
// though its byte length is longer once ANSI escapes are added — the property
// renderPanel/viewMenu/viewNormal rely on to align columns from strings that
// may or may not carry accent color.
func TestVisibleWidthIgnoresStyling(t *testing.T) {
	plain := "Purple"
	styled := styleText(true, plain, "#A78BFA")
	if styled == plain {
		t.Fatal("expected styleText to actually add ANSI codes here, or this test proves nothing")
	}
	if len(styled) == len(plain) {
		t.Fatal("expected styled to be byte-longer than plain (sanity check that styling was really applied)")
	}
	if visibleWidth(styled) != visibleWidth(plain) {
		t.Fatalf("visibleWidth should ignore ANSI styling: styled=%d plain=%d", visibleWidth(styled), visibleWidth(plain))
	}
}

func TestTruncateToWidth(t *testing.T) {
	if got := truncateToWidth("short", 20); got != "short" {
		t.Fatalf("short strings should be untouched, got %q", got)
	}
	long := "This is a much longer description than fits"
	got := truncateToWidth(long, 10)
	if n := len([]rune(got)); n != 10 || !strings.HasSuffix(got, "…") {
		t.Fatalf("expected a 10-cell truncation ending in an ellipsis, got %q (%d runes)", got, n)
	}
}

// TestRootPickerNoResultView renders viewSearch directly for a query with no
// matches, so unlike a pty capture (cumulative across every frame in a run,
// so it can't prove something is ABSENT from the final frame — see
// TestRootPicker) this is a single, fresh, one-shot render: it can reliably
// assert the no-result state has no "❯" cursor at all, since there's nothing
// in it to select.
func TestRootPickerNoResultView(t *testing.T) {
	m := &rootPickerApp{}
	m.stack = newNavStack(rootFrame{kind: frameList, list: newCommandBrowseList(rootPickerEntries)})
	m.stack.current().list.query = "zzz"
	m.stack.current().list.refilter()
	if len(m.stack.current().list.filtered) != 0 {
		t.Fatalf(`expected "zzz" to match nothing, got %v`, m.stack.current().list.filtered)
	}
	out := m.viewSearch()
	if !strings.Contains(out, "No commands found") {
		t.Fatalf("expected the empty-result message, got %q", out)
	}
	if strings.Contains(out, "❯") {
		t.Fatalf("expected no selectable row in the no-result state, got %q", out)
	}
}

// TestRootPicker drives the root command picker (rootui.go) through a real
// pseudoterminal, covering the redesign's specific requirements: navigation,
// live descriptions, type-to-search with no "/" shortcut, search editing
// (Backspace, empty-query-returns-to-normal, Esc-clears), the no-result
// state, executing both a plain and a searched-and-selected command, and
// terminal restoration. Esc-to-exit and the invisible group boundary between
// commands are covered by TestEscGuard and TestInteractivePicker respectively.
func TestRootPicker(t *testing.T) {
	bin, _ := buildCLI(t)

	drive := func(t *testing.T, steps func(master *os.File, capture func() string)) string {
		t.Helper()
		master, slave := openPTY(t)
		capture := drainPTY(master)
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin)
		cmd.Env = append(os.Environ(), "TERM=xterm-256color")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(500 * time.Millisecond)
		steps(master, capture)
		out := stripANSI(capture())
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return out
	}

	send := func(master *os.File, s string) {
		if _, err := master.WriteString(s); err != nil {
			t.Fatal(err)
		}
		time.Sleep(150 * time.Millisecond)
	}
	down := func(master *os.File) { send(master, "\x1b[B") }
	backspace := func(master *os.File) { send(master, "\x7f") }
	esc := func(master *os.File) { send(master, "\x1b") }
	enter := func(master *os.File) { send(master, "\r") }

	t.Run("default screen: default selection is run, its description shows", func(t *testing.T) {
		out := drive(t, func(master *os.File, capture func() string) {})
		if !strings.Contains(out, "❯ run") {
			t.Fatalf("expected the cursor on \"run\" by default, got %q", out)
		}
		if !strings.Contains(out, "Run Claude Code") {
			t.Fatalf("expected run's description below the panel, got %q", out)
		}
		if !rowHasArrow(out, "menu") {
			t.Fatalf(`expected "menu" to carry the visible " →" cue (it opens the full palette), got %q`, out)
		}
		if !rowHasArrow(out, "run") {
			t.Fatalf(`expected "run" to carry the visible " →" cue too (it opens the interactive run flow — decision 0019), got %q`, out)
		}
		if !rowHasArrow(out, "watch") {
			t.Fatalf(`expected "watch" to carry the visible " →" cue too (it opens the WATCH MODE screen — decision 0029), got %q`, out)
		}
		if rowHasArrow(out, "status") {
			t.Fatalf(`"status" is a leaf command and must not carry the " →" cue, got %q`, out)
		}
		// The reduced launcher: only the everyday actions plus "menu" — no
		// secondary/admin commands.
		for _, hidden := range []string{"login", "logout", "remove", "system", "config", "doctor", "install", "completion", "info", "help", "version"} {
			if strings.Contains(out, hidden) {
				t.Fatalf("the root launcher should not expose %q, got %q", hidden, out)
			}
		}
	})

	t.Run("selecting menu from the launcher opens the full palette", func(t *testing.T) {
		master, slave := openPTY(t)
		capture := drainPTY(master)
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin)
		cmd.Env = append(os.Environ(), "TERM=xterm-256color")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(500 * time.Millisecond)
		down(master) // run -> watch
		down(master) // watch -> status
		down(master) // status -> menu
		enter(master)
		time.Sleep(300 * time.Millisecond)
		out := stripANSI(capture())
		for _, want := range []string{"login", "config", "doctor", "install", "completion", "info", "help", "version"} {
			if !strings.Contains(out, want) {
				t.Fatalf("expected selecting \"menu\" to open the full palette (missing %q), got %q", want, out)
			}
		}
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	t.Run("Esc from the full palette (opened via menu) goes back to the launcher, not exit", func(t *testing.T) {
		master, slave := openPTY(t)
		capture := drainPTY(master)
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin)
		cmd.Env = append(os.Environ(), "TERM=xterm-256color")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(500 * time.Millisecond)
		down(master)  // run -> watch
		down(master)  // watch -> status
		down(master)  // status -> menu
		enter(master) // opens the full palette in place, not a second process
		time.Sleep(600 * time.Millisecond)
		if out := stripANSI(capture()); !strings.Contains(out, "Back") {
			t.Fatalf("expected the full palette's footer to mention \"Back\" when reached via menu, got %q", out)
		}
		esc(master) // a single Esc must go back to the launcher, not arm/exit
		time.Sleep(300 * time.Millisecond)
		if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
			t.Fatalf("a single Esc after opening the full palette via menu should go back, not exit: %v", err)
		}
		if out := stripANSI(capture()); !strings.Contains(out, "Esc² to exit") {
			t.Fatalf("expected the launcher's own footer (\"Esc to exit\") after backing out of the full palette, got %q", out)
		}
		// Prove this is genuinely the launcher's own (short) entry list again,
		// not merely a stale frame still on screen: from a freshly reset
		// cursor on "run", three Downs only reaches "menu" on the 4-item
		// launcher (they'd reach "login" on the 14-item full palette).
		// Re-entering it should reopen the full palette exactly as before.
		down(master)
		down(master)
		down(master)
		enter(master)
		time.Sleep(300 * time.Millisecond)
		out := stripANSI(capture())
		for _, want := range []string{"login", "config", "doctor"} {
			if !strings.Contains(out, want) {
				t.Fatalf("expected re-entering \"menu\" from the launcher to reopen the full palette (missing %q), got %q", want, out)
			}
		}
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	t.Run("double-Esc still exits the launcher after backing out of the full palette", func(t *testing.T) {
		master, slave := openPTY(t)
		capture := drainPTY(master)
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin)
		cmd.Env = append(os.Environ(), "TERM=xterm-256color")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(500 * time.Millisecond)
		down(master)
		down(master)
		down(master)
		enter(master) // launcher -> full palette
		time.Sleep(300 * time.Millisecond)
		esc(master) // full palette -> back to launcher (single press, not an arm)
		time.Sleep(300 * time.Millisecond)
		esc(master) // launcher: first Esc arms exit
		time.Sleep(300 * time.Millisecond)
		if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
			t.Fatalf("the first Esc back at the launcher should only arm exit, not exit immediately: %v", err)
		}
		esc(master) // second consecutive Esc actually exits
		err := cmd.Wait()
		exitErr, ok := err.(*exec.ExitError)
		if !ok || exitErr.ExitCode() != 1 {
			t.Fatalf("expected exit 1 after backing out of the full palette and then double-Esc, got %v; output %q", err, stripANSI(capture()))
		}
	})

	// bubbletea redraws in place, rewriting only what actually changed (see
	// renderPanel/viewNormal) — a same-screen cursor move may only touch the
	// description line and a couple of cells, not retransmit whole rows. So the
	// reliable way to check where two Downs actually land is by what changes on
	// every move (the description line, which viewNormal always rewrites in
	// full) and, more directly, by which command Enter goes on to run.
	t.Run("up/down navigation moves the cursor and updates the description", func(t *testing.T) {
		out := drive(t, func(master *os.File, capture func() string) {
			down(master) // run -> watch
			down(master) // watch -> status
		})
		if !strings.Contains(out, "Show account usage, sessions, and status") {
			t.Fatalf("expected status's description after two Downs, got %q", out)
		}
	})

	t.Run("up/down navigation: Enter runs the command actually landed on", func(t *testing.T) {
		master, slave := openPTY(t)
		capture := drainPTY(master)
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin)
		cmd.Env = append(os.Environ(), "TERM=xterm-256color")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(500 * time.Millisecond)
		down(master) // run -> watch
		down(master) // watch -> status
		send(master, "\r")
		if err := cmd.Wait(); err != nil {
			t.Fatalf("cpro: %v; output %q", err, stripANSI(capture()))
		}
		// status registers no accounts in this test's isolated config dir (see
		// buildCLI), so its fast, network-free path prints exactly this.
		if !strings.Contains(stripANSI(capture()), "No accounts found") {
			t.Fatalf("expected two Downs to land on \"status\" and Enter to run it, got %q", stripANSI(capture()))
		}
	})

	// "watch" (also in the reduced launcher) is deliberately not used here:
	// unlike status, it never exits on its own, only on Esc Esc/Ctrl+C — the
	// wrong shape for a test that just wants a clean, one-shot Wait().
	t.Run("Enter executes the selected command", func(t *testing.T) {
		master, slave := openPTY(t)
		capture := drainPTY(master)
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin)
		cmd.Env = append(os.Environ(), "TERM=xterm-256color")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(500 * time.Millisecond)
		for _, r := range "status" {
			send(master, string(r))
		}
		send(master, "\r")
		if err := cmd.Wait(); err != nil {
			t.Fatalf("cpro: %v; output %q", err, stripANSI(capture()))
		}
		if !strings.Contains(stripANSI(capture()), "No accounts found") {
			t.Fatalf("expected the status command to actually run, got %q", stripANSI(capture()))
		}
	})

	t.Run("typing immediately searches, no / required, and updates on every keystroke", func(t *testing.T) {
		out := drive(t, func(master *os.File, capture func() string) {
			send(master, "c")
		})
		if !strings.Contains(out, "╭─ claude cpro") {
			t.Fatalf("expected the title to stay on the border while searching, got %q", out)
		}
		if !strings.Contains(out, "Filter:") {
			t.Fatalf("expected typing to switch straight into the Search panel, got %q", out)
		}
		if !strings.Contains(out, "c_") {
			t.Fatalf("expected the typed character with a trailing cursor, got %q", out)
		}

		// config/completion only exist in the full palette (cpro menu), not
		// the reduced launcher — a dedicated launch rather than drive().
		master, slave := openPTY(t)
		capture := drainPTY(master)
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, "menu")
		cmd.Env = append(os.Environ(), "TERM=xterm-256color")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(500 * time.Millisecond)
		send(master, "c")
		send(master, "o")
		time.Sleep(200 * time.Millisecond)
		out = stripANSI(capture())
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		// Not asserting the literal contiguous "co_" here: with the alt
		// screen now active (see pickCommandArgs/View), bubbletea leans on
		// fine-grained cell diffing even for a query growing by one
		// character, so the two digits of the query can legitimately land
		// in the raw byte stream via separate cursor-positioned writes
		// rather than one contiguous "co_" run. The functional proof that
		// the query actually became "co" is that filtering narrowed to the
		// commands containing it — config and completion by prefix, account
		// by substring since decision 0040 (see TestRootPickerScore for the
		// ranking rule itself) — rather than showing the whole list.
		// Presence-only, deliberately: this capture is cumulative, so text
		// from the pre-search frame is still in it and an absence check
		// would be meaningless here — the rule this file documents at the
		// top of TestRootPicker. The narrowing itself is asserted where it
		// can be proven, in TestRootPickerScore/TestRootPickerRefilter.
		if !strings.Contains(out, "config") || !strings.Contains(out, "completion") {
			t.Fatalf(`expected config and completion for "co", got %q`, out)
		}
		// The " →" cue must survive filtering: "config" opens Settings and
		// keeps it in search results, "completion" is a leaf and never gets one.
		if !rowHasArrow(out, "config") {
			t.Fatalf(`expected "config" to keep its " →" cue in search results, got %q`, out)
		}
		if rowHasArrow(out, "completion") {
			t.Fatalf(`expected "completion" (a leaf command) to never carry " →", got %q`, out)
		}
	})

	// The search-mode footer ("Esc Clear", no "Type to search") and the
	// normal-mode footer ("Esc² to exit", with "Type to search") are textually
	// distinct, so checking which one shows up is a reliable positive signal for
	// which mode a run ended in — unlike checking for the ABSENCE of "╭─ Search"
	// or a stale query fragment, which can't distinguish "never appeared" from
	// "appeared in an earlier frame, off-screen by now": bubbletea's redraws are
	// captured cumulatively here, so an in-between frame's content is still
	// physically present in the byte stream even once a real terminal has long
	// since overwritten it on screen.
	t.Run("Backspace edits the query; emptying it returns to the normal picker", func(t *testing.T) {
		out := drive(t, func(master *os.File, capture func() string) {
			send(master, "c")
			send(master, "o")
			backspace(master)
		})
		if !strings.Contains(out, "c_") {
			t.Fatalf(`expected the query to read "c_" after one Backspace, got %q`, out)
		}
		if !strings.Contains(out, "Esc Clear") {
			t.Fatalf("expected to still be in search mode after one Backspace, got %q", out)
		}

		out = drive(t, func(master *os.File, capture func() string) {
			send(master, "c")
			backspace(master)
		})
		if !strings.Contains(out, "Esc² to exit") {
			t.Fatalf("expected Backspace on a one-character query to return to the normal picker, got %q", out)
		}
	})

	t.Run("Esc clears the search and returns to the normal picker", func(t *testing.T) {
		out := drive(t, func(master *os.File, capture func() string) {
			send(master, "c")
			send(master, "o")
			esc(master)
		})
		if !strings.Contains(out, "Esc² to exit") {
			t.Fatalf("expected Esc to clear the search and return to the normal picker, got %q", out)
		}
	})

	t.Run("double-Esc-to-exit still works after returning from search to normal mode", func(t *testing.T) {
		master, slave := openPTY(t)
		capture := drainPTY(master)
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin)
		cmd.Env = append(os.Environ(), "TERM=xterm-256color")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(500 * time.Millisecond)
		send(master, "c") // enter search
		esc(master)       // clear search, back to normal mode
		esc(master)       // arms exit (a fresh state, not left over from search)
		time.Sleep(300 * time.Millisecond)
		if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
			t.Fatalf("the Esc that cleared search should not also count toward exiting: %v", err)
		}
		esc(master) // confirms it
		err := cmd.Wait()
		exitErr, ok := err.(*exec.ExitError)
		if !ok || exitErr.ExitCode() != 1 {
			t.Fatalf("expected exit 1 after returning from search and then double-Esc, got %v; output %q", err, stripANSI(capture()))
		}
	})

	t.Run("no-result state: no selection, Enter is a no-op", func(t *testing.T) {
		master, slave := openPTY(t)
		capture := drainPTY(master)
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin)
		cmd.Env = append(os.Environ(), "TERM=xterm-256color")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(500 * time.Millisecond)
		for _, r := range "xyz" {
			send(master, string(r))
		}
		out := stripANSI(capture())
		if !strings.Contains(out, "No commands found") {
			t.Fatalf("expected the empty-result state, got %q", out)
		}
		send(master, "\r") // Enter with no results must do nothing
		time.Sleep(200 * time.Millisecond)
		if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
			t.Fatalf("Enter with no results should not exit the picker: %v", err)
		}
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	t.Run("Enter executes the highlighted search result", func(t *testing.T) {
		master, slave := openPTY(t)
		capture := drainPTY(master)
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, "menu") // "completion" only exists in the full palette
		cmd.Env = append(os.Environ(), "TERM=xterm-256color")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(500 * time.Millisecond)
		for _, r := range "comp" { // uniquely matches "completion" (see TestRootPickerScore)
			send(master, string(r))
		}
		send(master, "\r")
		if err := cmd.Wait(); err != nil {
			t.Fatalf("cpro: %v; output %q", err, stripANSI(capture()))
		}
		// completion (cobra's built-in command) takes no positional args of its
		// own, so it has nothing left to prompt for and just shows its usage —
		// listing bash/zsh/fish/powershell — proof the search result actually ran.
		out := stripANSI(capture())
		if !strings.Contains(out, "bash") || !strings.Contains(out, "zsh") {
			t.Fatalf("expected completion's own usage listing the shells, got %q", out)
		}
	})

	t.Run("Ctrl+C exits safely, same as Esc", func(t *testing.T) {
		master, slave := openPTY(t)
		capture := drainPTY(master)
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin)
		cmd.Env = append(os.Environ(), "TERM=xterm-256color")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(500 * time.Millisecond)
		send(master, "\x03") // Ctrl+C
		err := cmd.Wait()
		exitErr, ok := err.(*exec.ExitError)
		if !ok || exitErr.ExitCode() != 1 {
			t.Fatalf("expected exit 1 from Ctrl+C, got %v", err)
		}
		if strings.Contains(stripANSI(capture()), "ERROR") {
			t.Fatalf("cancelling isn't an error, should not show an ERROR box: %q", stripANSI(capture()))
		}
	})

	t.Run("NO_COLOR renders the same flat picker with no escape codes", func(t *testing.T) {
		master, slave := openPTY(t)
		capture := drainPTY(master)
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, "menu") // needs "config" from the full palette
		cmd.Env = append(os.Environ(), "TERM=xterm-256color", "NO_COLOR=1")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(500 * time.Millisecond)
		out := capture()
		if strings.Contains(out, "\x1b[38") {
			t.Fatalf("NO_COLOR should suppress every color escape code, got %q", out)
		}
		plain := stripANSI(out)
		for _, want := range []string{"╭─", "❯ config", "Preferences", "╰─"} {
			if !strings.Contains(plain, want) {
				t.Fatalf("NO_COLOR picker missing %q: %q", want, plain)
			}
		}
		if !rowHasArrow(plain, "config") {
			t.Fatalf("NO_COLOR picker missing config's \" →\" cue: %q", plain)
		}
		// The double-Esc-to-exit behavior is a two-step state machine, not a
		// rendering effect — it must still require two Esc presses even
		// though NO_COLOR means there's nothing to see for the first one.
		if _, err := master.Write([]byte{0x1b}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(300 * time.Millisecond)
		if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
			t.Fatalf("a single Esc should not exit the picker under NO_COLOR either: %v", err)
		}
		if _, err := master.Write([]byte{0x1b}); err != nil {
			t.Fatal(err)
		}
		err := cmd.Wait()
		exitErr, ok := err.(*exec.ExitError)
		if !ok || exitErr.ExitCode() != 1 {
			t.Fatalf("expected exit 1 from a second Esc under NO_COLOR, got %v", err)
		}
	})

	t.Run("terminal is restored on exit", func(t *testing.T) {
		master, slave := openPTY(t)
		capture := drainPTY(master)
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin)
		cmd.Env = append(os.Environ(), "TERM=xterm-256color")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(500 * time.Millisecond)
		esc(master) // arms exit
		esc(master) // confirms it
		_ = cmd.Wait()
		echo := exec.Command("echo", "restored")
		echo.Stdin, echo.Stdout, echo.Stderr = slave, slave, slave
		if err := echo.Run(); err != nil {
			t.Fatalf("terminal not usable after cpro exited: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
		if !strings.Contains(capture(), "restored") {
			t.Fatalf("expected the terminal to still echo output after cpro exited, got %q", stripANSI(capture()))
		}
	})
}

// TestRootPickerResponsive drives the real picker through a pty sized at a
// few specific column widths, covering the redesign's responsive-metadata
// requirement end to end (see the pure, no-pty TestRootRowResponsive for the
// underlying degrade logic in isolation): the full command list — one row
// per command, never wrapped — stays intact at every width, while metadata
// truncates, then disappears, as the terminal narrows. It also checks
// column alignment: every row's command name lands in the same column, and
// so does every row's (equal-width) metadata, at a width wide enough to show
// it in full.
func TestRootPickerResponsive(t *testing.T) {
	bin, _ := buildCLI(t)

	// "cpro menu", not bare "cpro": every subtest below exercises the full
	// command palette (metadata alignment/truncation and scrolling across
	// every command), which now lives under menu — the reduced bare-cpro
	// launcher only ever shows run/status/watch/menu (see TestRootPicker's
	// own "default screen" subtest for that one).
	renderAt := func(t *testing.T, cols int) string {
		t.Helper()
		master, slave := openPTY(t)
		resizePTY(t, slave, 40, uint16(cols))
		capture := drainPTY(master)
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, "menu")
		cmd.Env = append(os.Environ(), "TERM=xterm-256color")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(500 * time.Millisecond)
		out := stripANSI(capture())
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return out
	}
	commandNames := []string{"config", "default", "login", "logout", "remove", "session", "system", "doctor", "install", "completion", "info", "help", "version"}
	// isCommandRow distinguishes a real command row from a blank group-separator
	// row (also rendered as a bare "│", restoring subtle grouping — see
	// viewNormal): a separator has nothing after the rail at all.
	isCommandRow := func(line string) bool {
		return strings.HasPrefix(line, "│") && line != "│" && line != "│\r" && !strings.Contains(line, "Filter:")
	}

	t.Run("wide: full metadata, one row per command", func(t *testing.T) {
		out := renderAt(t, 120)
		lines := strings.Split(out, "\n")
		rows := 0
		for _, line := range lines {
			if isCommandRow(line) {
				rows++
			}
		}
		if rows != len(commandNames) {
			t.Fatalf("expected exactly %d command rows, got %d: %q", len(commandNames), rows, out)
		}
		if !strings.Contains(out, "completion     Shell setup") {
			t.Fatalf("expected completion's metadata in full at 120 columns, got %q", out)
		}
	})

	t.Run("narrow: metadata truncates but every command name and row survives", func(t *testing.T) {
		out := renderAt(t, 25) // metaBudget(nameWidth=10, "completion" is the longest name) = 25-1-4-10-2-3 = 5: room for a truncated, non-empty label
		lines := strings.Split(out, "\n")
		rows := 0
		for _, line := range lines {
			if isCommandRow(line) {
				rows++
				for _, name := range commandNames {
					// Each row's own command name must be present verbatim,
					// never abbreviated (only metadata degrades).
					if strings.Contains(line, "❯ "+name) || strings.Contains(line, "  "+name) {
						goto found
					}
				}
				t.Fatalf("row %q doesn't contain any known, un-truncated command name", line)
			found:
			}
		}
		if rows != len(commandNames) {
			t.Fatalf("expected all %d commands to still render as one row each, got %d: %q", len(commandNames), rows, out)
		}
		if !strings.Contains(out, "…") {
			t.Fatalf("expected at least one truncated metadata value ending in an ellipsis at 25 columns, got %q", out)
		}
		if strings.Contains(out, "Shell setup") {
			t.Fatalf("completion's metadata should be truncated, not shown in full, at 25 columns: %q", out)
		}
	})

	t.Run("very narrow: metadata disappears entirely, command names remain", func(t *testing.T) {
		// 20, not 15: nameWidth is 10 ("completion", the longest name), and
		// the app never wraps or truncates a command name itself (only
		// metadata degrades) — a terminal narrower than rail+marker+
		// nameWidth+arrow-slot (1+4+10+2=17) would hard-wrap "completion"
		// itself at the real terminal level, which is a display artifact of
		// an unrealistically narrow terminal, not something this test is about.
		out := renderAt(t, 20) // metaBudget(nameWidth=10) = 20-1-4-10-2-3 = 0: hidden entirely, no name wraps
		for _, name := range commandNames {
			if !strings.Contains(out, name) {
				t.Fatalf("expected %q to still render at 15 columns, got %q", name, out)
			}
		}
		for _, line := range strings.Split(out, "\n") {
			if !isCommandRow(line) {
				continue // the below-panel description line legitimately truncates with "…" too; only rows matter here
			}
			if strings.Contains(line, "…") {
				t.Fatalf("a lone ellipsis with no real metadata text should not render on a row; metadata should disappear entirely: %q", line)
			}
			for _, meta := range []string{"Claude Code", "Sign in", "Shell setup", "Preferences"} {
				if strings.Contains(line, meta) {
					t.Fatalf("metadata %q should be fully hidden at 15 columns, got %q", meta, line)
				}
			}
		}
		lines := strings.Split(out, "\n")
		rows := 0
		for _, line := range lines {
			if isCommandRow(line) {
				rows++
			}
		}
		if rows != len(commandNames) {
			t.Fatalf("expected all %d commands to still render as one row each even at 15 columns, got %d", len(commandNames), rows)
		}
	})

	t.Run("command names and metadata align in their own columns", func(t *testing.T) {
		out := renderAt(t, 120)
		labels := map[string]bool{}
		for _, e := range rootPickerEntries {
			if e.name == "default" {
				continue // rendered specially as "● <mode label>  ∙  account", not its static shortLabel — see below
			}
			labels[e.shortLabel] = true
		}
		// Measured per line (not across the whole multi-line render, where
		// visibleWidth would report the widest line overall, not a column
		// position) — each command row's metadata should start at the same
		// visible column as every other row's. The "default" row's own
		// metadata starts with "●" (rootRow's colored-dot rendering) instead
		// of a plain shortLabel string — it's the only row that ever renders
		// that glyph, so it doubles as this row's own marker here.
		var cols []int
		for _, line := range strings.Split(out, "\n") {
			if !strings.HasPrefix(line, "│") {
				continue
			}
			if idx := strings.Index(line, "●"); idx >= 0 {
				cols = append(cols, visibleWidth(line[:idx]))
				continue
			}
			for label := range labels {
				if idx := strings.Index(line, label); idx >= 0 {
					cols = append(cols, visibleWidth(line[:idx]))
					break
				}
			}
		}
		// rootPickerEntries (all of rootPickerMeta) includes "menu", which
		// cpro menu's own rendering never shows (see rootMenuNames) — that's
		// the count of actual rows expected here, not rootPickerEntries'.
		if want := len(rootMenuNames()); len(cols) != want {
			t.Fatalf("expected metadata found on all %d rows, found %d in %q", want, len(cols), out)
		}
		for i, col := range cols {
			if col != cols[0] {
				t.Fatalf("row %d's metadata starts at column %d, row 0's at %d — misaligned", i, col, cols[0])
			}
		}
	})

	// Regression test: on a terminal shorter than the picker's own content
	// (12 command rows plus 4 blank group separators, the panel edges,
	// description, and footer — about 22 lines), bubbletea's default
	// (non-altscreen) renderer repositions the cursor with *relative*
	// up-moves that assume the previous frame is still fully on screen. Once
	// a frame taller than the window forces the terminal to scroll, that
	// assumption breaks, and every redraw overlaps stale content instead of
	// replacing it — confirmed live (reported by the user as a duplicated
	// panel header and a truncated command list). See View()/pickCommandArgs
	// for the fix: tea.View.AltScreen.
	t.Run("short terminal: no corrupted/duplicated redraws", func(t *testing.T) {
		master, slave := openPTY(t)
		resizePTY(t, slave, 15, 100) // fewer rows than the ~21-line render
		capture := drainPTY(master)
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin) // the reduced launcher already has watch
		cmd.Env = append(os.Environ(), "TERM=xterm-256color")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(500 * time.Millisecond)
		for range 1 { // run -> watch
			if _, err := master.WriteString("\x1b[B"); err != nil {
				t.Fatal(err)
			}
			time.Sleep(150 * time.Millisecond)
		}
		time.Sleep(200 * time.Millisecond)
		if strings.Count(stripANSI(capture()), "Filter:") > 1 {
			t.Fatalf("duplicated panel header — the exact corruption reported live: %q", stripANSI(capture()))
		}
		// "which command actually ran" is the reliable proof Down landed on
		// watch, the same convention TestRootPicker itself relies on for a
		// cumulative, diffed pty capture. Selecting "watch" now opens WATCH
		// MODE first (decision 0029) rather than running immediately, so a
		// second Enter confirms the default "Full" selection there.
		if _, err := master.WriteString("\r"); err != nil {
			t.Fatal(err)
		}
		time.Sleep(300 * time.Millisecond)
		if _, err := master.WriteString("\r"); err != nil {
			t.Fatal(err)
		}
		time.Sleep(500 * time.Millisecond)
		out := stripANSI(capture())
		if !strings.Contains(out, "No accounts found") {
			t.Fatalf("expected watch's fast, network-free path to have run, got %q", out)
		}
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	// Regression test: on a terminal shorter than the full command list,
	// scrolling down past the bottom of the visible window used to move the
	// selection without ever bringing it back on screen — a command near the
	// end (or the very last one, "version") became permanently unreachable/
	// unconfirmable, reported live. visibleWindow (rootui.go) fixes this by
	// scrolling the list body to keep the cursor's row always visible,
	// leaving the description/footer always shown (only the body scrolls).
	t.Run("short terminal: scrolling reaches the last command", func(t *testing.T) {
		master, slave := openPTY(t)
		resizePTY(t, slave, 15, 100)
		capture := drainPTY(master)
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, "menu") // "version" only exists in the full palette
		cmd.Env = append(os.Environ(), "TERM=xterm-256color")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(500 * time.Millisecond)
		for range 12 { // config -> ... -> version, the last of 13 commands
			if _, err := master.WriteString("\x1b[B"); err != nil {
				t.Fatal(err)
			}
			time.Sleep(120 * time.Millisecond)
		}
		time.Sleep(300 * time.Millisecond)
		plain := stripANSI(capture())
		if !strings.Contains(plain, "version") || !strings.Contains(plain, "Print the cpro version") {
			t.Fatalf("expected to scroll down to \"version\" and see its description, got %q", plain)
		}
		if strings.Count(plain, "Filter:") > 1 {
			t.Fatalf("duplicated panel header while scrolling: %q", plain)
		}
		if _, err := master.WriteString("\r"); err != nil {
			t.Fatal(err)
		}
		if err := cmd.Wait(); err != nil {
			t.Fatalf("cpro: %v; output %q", err, stripANSI(capture()))
		}
		if !strings.Contains(capture(), "cpro version "+version) {
			t.Fatalf("expected the scrolled-to \"version\" command to actually run, got %q", stripANSI(capture()))
		}
	})
}

// ansiTrueColor formats hex ("#RRGGBB") as the decimal "R;G;B" fragment
// lipgloss's truecolor SGR sequences carry (e.g. "38;2;167;139;250" for
// foreground) — used below to check which of the five derived accent shades
// a given rendered line actually carries, without hardcoding a full escape
// sequence.
func ansiTrueColor(t *testing.T, hex string) string {
	t.Helper()
	var r, g, b int
	if _, err := fmt.Sscanf(hex, "#%02X%02X%02X", &r, &g, &b); err != nil {
		t.Fatalf("not a #RRGGBB color: %q: %v", hex, err)
	}
	return fmt.Sprintf("%d;%d;%d", r, g, b)
}

// TestRootPickerGradient covers the redesign's color-specific requirements
// that TestRootPicker (plain-text, stripANSI'd) can't: the five-shade
// semantic-group rail actually derives from the configured Accent color (and
// updates when that preference changes), the cursor always uses the plain
// Accent color rather than its row's group shade, the panel's opening/
// closing rail use the first/last shade, and search mode collapses back to
// one uniform Accent color instead of the five-shade gradient.
func TestRootPickerGradient(t *testing.T) {
	bin, _ := buildCLI(t)

	// launch starts the picker and sends each element of keys as one atomic
	// write (a multi-byte sequence like "\x1b[B" must arrive together — sent
	// byte-by-byte with a delay in between, as a plain string iterated by
	// rune would do, a lone leading \x1b arrives on its own and is read back
	// as a real standalone Esc, exiting the picker before the rest of the
	// sequence even arrives).
	// "cpro menu", not bare "cpro": these subtests exercise the full
	// five-group gradient (install/completion/config/doctor/help/version
	// aren't in the reduced launcher, which only ever spans two groups).
	launch := func(t *testing.T, cols int, keys ...string) string {
		t.Helper()
		master, slave := openPTY(t)
		resizePTY(t, slave, 40, uint16(cols))
		capture := drainPTY(master)
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, "menu")
		cmd.Env = append(os.Environ(), "TERM=xterm-256color")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(500 * time.Millisecond)
		for _, key := range keys {
			if _, err := master.WriteString(key); err != nil {
				t.Fatal(err)
			}
			time.Sleep(80 * time.Millisecond)
		}
		time.Sleep(200 * time.Millisecond)
		out := capture()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return out
	}

	// lineFor returns the last rendered occurrence of a line containing
	// needle — bubbletea's redraws are captured cumulatively (see
	// TestRootPicker's own doc comment on this), so the LAST match is the
	// final on-screen state. It matches against the ANSI-stripped line
	// (raw lines carry a color reset between "❯" and the command name, which
	// would otherwise break a plain substring search for e.g. "❯ run") but
	// returns the original, still-styled line, and only considers actual
	// panel rows ("│"/"╭"/"╰"-prefixed) — excluding the below-panel
	// description line, whose text (e.g. "...cpro configuration...") can
	// otherwise collide with a command name substring (e.g. "config").
	lineFor := func(out, needle string) string {
		norm := func(s string) []string { return strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n") }
		rawLines := norm(out)
		plainLines := norm(stripANSI(out))
		var last string
		for i, plain := range plainLines {
			isPanelRow := strings.HasPrefix(plain, "│") || strings.HasPrefix(plain, "╭") || strings.HasPrefix(plain, "╰")
			if isPanelRow && strings.Contains(plain, needle) && i < len(rawLines) {
				last = rawLines[i]
			}
		}
		return last
	}

	t.Run("five shades derived from the default Accent (Purple)", func(t *testing.T) {
		out := launch(t, 120)
		shades := deriveAccentShades("#A78BFA", 5) // ui.go's default accentMode
		rgb := func(i int) string { return ansiTrueColor(t, shades[i]) }

		if !strings.Contains(lineFor(out, "╭─"), rgb(0)) {
			t.Fatalf("╭─ should use shade 0 (%s), got %q", shades[0], lineFor(out, "╭─"))
		}
		if !strings.Contains(lineFor(out, "╰─"), rgb(4)) {
			t.Fatalf("╰─ should use shade 4 (%s), got %q", shades[4], lineFor(out, "╰─"))
		}
		for name, shadeIdx := range map[string]int{
			"config": 0,              // Core
			"login":  1, "remove": 1, // Account
			"system": 2, "doctor": 2, // Settings
			"install": 3, "completion": 3, // System
			"help": 4, "version": 4, // About
		} {
			line := lineFor(out, " "+name+" ")
			if line == "" {
				line = lineFor(out, "❯ "+name+" ") // the default "config" row also carries the cursor
			}
			if !strings.Contains(line, rgb(shadeIdx)) {
				t.Fatalf("%s: expected shade %d (%s) on its row, got %q", name, shadeIdx, shades[shadeIdx], line)
			}
		}

		// The cursor itself (on "config", a Core/shade-0 row) must use the plain
		// Accent color (shade 4, the last one — see deriveAccentShades), not
		// shade 0, even though the rest of that same row's rail is shade 0.
		configLine := lineFor(out, "❯ config")
		if !strings.Contains(configLine, rgb(4)) {
			t.Fatalf("the cursor should render in the main Accent color, got %q", configLine)
		}
		if strings.Count(configLine, rgb(0)) != 1 {
			t.Fatalf("expected exactly one shade-0 escape on config's row (the rail, not the cursor), got %q", configLine)
		}
	})

	// Rendered directly (m.cursor set to login's index) rather than driven
	// live through a pty: bubbletea's diffing renderer only rewrites what
	// actually changed between frames (see TestRootPicker's own doc comment
	// on this — a cursor move may only touch a couple of cells, never
	// retransmitting the full destination row), so a cumulative pty capture
	// can't reliably prove what color a NON-default row's cursor ends up in.
	// A fresh, single viewNormal() call sidesteps that entirely.
	t.Run("cursor keeps the main Accent color on a different group's row", func(t *testing.T) {
		root := rootCommand()
		root.InitDefaultHelpCmd()
		root.InitDefaultCompletionCmd()
		entries := rootPickerFilteredEntries(root, rootMenuNames())
		loginIdx := -1
		for i, e := range entries {
			if e.name == "login" {
				loginIdx = i
			}
		}
		if loginIdx < 0 {
			t.Fatal("login not found in the picker's entries")
		}
		shades := deriveAccentShades("#A78BFA", 5)
		m := &rootPickerApp{color: true, shades: shades}
		m.stack = newNavStack(rootFrame{kind: frameList, list: newCommandBrowseList(entries)})
		m.stack.current().list.cursor = loginIdx
		loginLine := lineFor(m.viewNormal(), "❯ login")
		if loginLine == "" {
			t.Fatalf("expected a rendered ❯ login row, got %q", m.viewNormal())
		}
		if !strings.Contains(loginLine, ansiTrueColor(t, shades[4])) {
			t.Fatalf("the cursor on login (Account, shade 1) should still render in the main Accent (shade 4): %q", loginLine)
		}
		if !strings.Contains(loginLine, ansiTrueColor(t, shades[1])) {
			t.Fatalf("login's own rail should still be shade 1 (Account), got %q", loginLine)
		}
	})

	t.Run("search mode uses one Accent color, not the five-shade gradient", func(t *testing.T) {
		// A direct, single render (like the "cursor keeps the main Accent
		// color" subtest just above), not a live pty: going from browsing to
		// search collapses a row's rail from its group shade to plain accent
		// while its glyph position can stay the same, which is exactly the
		// same-cell, style-only diff bubbletea's renderer may leave to a
		// cursor-positioned partial rewrite rather than reissuing a fresh
		// color escape — confirmed live, a cumulative pty capture picked up
		// a "completion" row with no color escape at all (inheriting
		// whatever SGR state the previous row's reset left behind), not a
		// rendering bug in rootRow/viewSearch itself. See TestRootPicker's
		// own doc comment for the general rule this follows.
		root := rootCommand()
		root.InitDefaultHelpCmd()
		root.InitDefaultCompletionCmd()
		entries := rootPickerFilteredEntries(root, rootMenuNames())
		shades := deriveAccentShades("#A78BFA", 5)
		m := &rootPickerApp{color: true, shades: shades}
		m.stack = newNavStack(rootFrame{kind: frameList, list: newCommandBrowseList(entries)})
		m.stack.current().list.query = "co"
		m.stack.current().list.refilter()
		out := m.viewSearch()
		configLine := lineFor(out, "config")
		completionLine := lineFor(out, "completion")
		accentRGB := ansiTrueColor(t, shades[4])
		for _, line := range []string{configLine, completionLine} {
			if !strings.Contains(line, accentRGB) {
				t.Fatalf("search-mode row should use the main Accent color, got %q", line)
			}
		}
		// config and completion sit in different groups (shade 2 and shade 3)
		// in normal mode — neither of those group shades should appear at
		// all while search is active.
		for _, shadeIdx := range []int{1, 2, 3} { // Account, Settings, System — none of which is the main Accent
			shadeRGB := ansiTrueColor(t, shades[shadeIdx])
			if shadeRGB == accentRGB {
				continue
			}
			if strings.Contains(configLine, shadeRGB) || strings.Contains(completionLine, shadeRGB) {
				t.Fatalf("search mode should not show any group rail shade (%s), got config=%q completion=%q", shades[shadeIdx], configLine, completionLine)
			}
		}
	})

	// Last, deliberately: this permanently changes the isolated test config's
	// Accent to Blue, which every earlier subtest above assumes is still the
	// default Purple.
	t.Run("changing Accent changes the picker's gradient", func(t *testing.T) {
		set := exec.Command(bin, "config", "accent", "blue")
		if out, err := set.CombinedOutput(); err != nil {
			t.Fatalf("cpro config accent blue: %v: %s", err, out)
		}
		out := launch(t, 120)
		purpleShades := deriveAccentShades("#A78BFA", 5)
		blueShades := deriveAccentShades("#60A5FA", 5)
		top := lineFor(out, "╭─")
		if strings.Contains(top, ansiTrueColor(t, purpleShades[0])) {
			t.Fatalf("still showing the old Purple-derived shade after switching Accent to Blue: %q", top)
		}
		if !strings.Contains(top, ansiTrueColor(t, blueShades[0])) {
			t.Fatalf("expected ╭─ to reflect the newly configured Blue accent, got %q", top)
		}
	})
}

// TestWatch exercises "cpro watch": periodic redraws, colors surviving the raw
// terminal mode watchLoop puts stdin into, and stopping it with Esc Esc or
// Ctrl+C. It registers no accounts, so every redraw is the fast, network-free
// "No accounts found" path — renderAccountSnapshot's usage fetch is exercised
// elsewhere (TestUsage, TestCLI's ls/list checks), and pulling it into a timing
// -sensitive test here would make the timing depend on network reachability too.
func TestWatch(t *testing.T) {
	bin, _ := buildCLI(t)

	t.Run("refreshes and Esc Esc exits", func(t *testing.T) {
		master, slave := openPTY(t)
		capture := drainPTY(master)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, "watch", "--interval", "5s")
		cmd.Env = append(os.Environ(), "TERM=xterm-256color")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(11 * time.Second) // initial redraw plus at least one 5s tick
		if _, err := master.Write([]byte{0x1b}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(400 * time.Millisecond)
		out := capture()
		if n := strings.Count(out, "No accounts found"); n < 2 {
			t.Fatalf("expected at least 2 refreshes in 11s at a 5s interval, got %d: %q", n, stripANSI(out))
		}
		if !strings.Contains(out, "38;2;") {
			t.Fatalf("expected truecolor codes (from the Esc warning) to survive raw-mode output: %q", stripANSI(out))
		}
		if _, err := master.Write([]byte{0x1b}); err != nil {
			t.Fatal(err)
		}
		if err := cmd.Wait(); err != nil {
			t.Fatalf("watch Esc Esc exit: %v; output %q", err, stripANSI(capture()))
		}
	})

	t.Run("Ctrl+C exits", func(t *testing.T) {
		master, slave := openPTY(t)
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, "watch", "--interval", "5s")
		cmd.Env = append(os.Environ(), "TERM=xterm-256color")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(600 * time.Millisecond)
		if _, err := master.Write([]byte{0x03}); err != nil {
			t.Fatal(err)
		}
		if err := cmd.Wait(); err != nil {
			t.Fatalf("watch Ctrl+C exit: %v", err)
		}
	})

	t.Run("non-interactive output has no escape codes and stops on SIGINT", func(t *testing.T) {
		var out bytes.Buffer
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, "watch", "--interval", "5s")
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(600 * time.Millisecond)
		if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
			t.Fatal(err)
		}
		if err := cmd.Wait(); err == nil {
			t.Fatal("expected SIGINT to terminate a redirected cpro watch")
		}
		if strings.Contains(out.String(), "\x1b") {
			t.Fatalf("redirected watch output should have no escape codes: %q", out.String())
		}
	})
}

// TestWatchModePickerEndToEnd covers decision 0029's interactive picker path
// through a real pty end to end: bare cpro -> "watch" -> WATCH MODE -> Full
// or Compact -> the exact same watchLoop direct `cpro watch`/`cpro watch
// --compact` already use — distinguishing Full from Compact by their own
// real, structurally different output (Full's own boxed per-account card,
// "╭─ EMAIL ...", decision 0033 — renderCompactView's bare "╭─" never carries
// an account's email in its border) rather than a printed announcement,
// since watch (unlike run) has none. Also confirms direct `cpro watch`/`cpro
// watch --compact` remain completely unchanged (no picker, no WATCH MODE).
func TestWatchModePickerEndToEnd(t *testing.T) {
	bin, s := buildCLI(t)
	stage := t.TempDir()
	if err := os.WriteFile(filepath.Join(stage, ".credentials.json"), []byte(`{"loggedIn":true,"email":"watch-mode@example.com","authMethod":"claude.ai"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, ".claude.json"), []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.installLogin("watch-mode@example.com", stage); err != nil {
		t.Fatal(err)
	}

	send := func(master *os.File, str string) {
		if _, err := master.WriteString(str); err != nil {
			t.Fatal(err)
		}
		time.Sleep(150 * time.Millisecond)
	}
	down := func(master *os.File) { send(master, "\x1b[B") }
	right := func(master *os.File) { send(master, "\x1b[C") }
	enter := func(master *os.File) { send(master, "\r") }
	esc := func(master *os.File) { send(master, "\x1b") }

	drivePicker := func(t *testing.T, steps func(master *os.File)) string {
		t.Helper()
		master, slave := openPTY(t)
		capture := drainPTY(master)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin)
		cmd.Env = append(os.Environ(), "TERM=xterm-256color")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(500 * time.Millisecond)
		down(master) // run -> watch
		steps(master)
		time.Sleep(700 * time.Millisecond)
		out := stripANSI(capture())
		esc(master)
		esc(master)
		_ = cmd.Wait()
		return out
	}

	t.Run("Right Arrow on watch opens WATCH MODE; Enter starts Full", func(t *testing.T) {
		out := drivePicker(t, func(master *os.File) {
			right(master)
			time.Sleep(300 * time.Millisecond)
			enter(master) // Full is the default selection
		})
		if !strings.Contains(out, "claude cpro - WATCH MODE") {
			t.Fatalf("expected WATCH MODE to have opened, got %q", out)
		}
		if !strings.Contains(out, "╭─ watch-mode@example.com") {
			t.Fatalf("expected Full's own real output (renderFullView, a boxed per-account card), got %q", out)
		}
	})

	t.Run("Enter on watch also opens WATCH MODE; Down then Right starts Compact", func(t *testing.T) {
		out := drivePicker(t, func(master *os.File) {
			enter(master)
			time.Sleep(300 * time.Millisecond)
			down(master) // Full -> Compact
			right(master)
		})
		if !strings.Contains(out, "claude cpro - WATCH MODE") {
			t.Fatalf("expected WATCH MODE to have opened, got %q", out)
		}
		if strings.Contains(out, "╭─ watch-mode@example.com") {
			t.Fatalf("expected Compact's own real output (renderCompactView, no boxed per-account card), got %q", out)
		}
	})

	t.Run("Left Arrow backs out of WATCH MODE to a live ROOT", func(t *testing.T) {
		// Liveness plus a ground-truth re-entry, not a cumulative-capture text
		// search — a raw pty capture can't prove a frame *popped* rather than
		// merely looking stale (see TestRootPicker's/TestSessionUI's own
		// documented presence-only discipline).
		master, slave := openPTY(t)
		capture := drainPTY(master)
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin)
		cmd.Env = append(os.Environ(), "TERM=xterm-256color")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(500 * time.Millisecond)
		down(master) // run -> watch
		enter(master)
		time.Sleep(300 * time.Millisecond)
		send(master, "\x1b[D") // Left Arrow
		time.Sleep(300 * time.Millisecond)
		if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
			t.Fatalf("expected the process to still be alive after backing out of WATCH MODE to ROOT, got %v", err)
		}
		// Re-entering watch from the restored ROOT frame proves the stack was
		// genuinely popped back to ROOT, not just a stale-looking render.
		down(master) // run -> watch
		enter(master)
		time.Sleep(300 * time.Millisecond)
		out := stripANSI(capture())
		if !strings.Contains(out, "claude cpro - WATCH MODE") {
			t.Fatalf("expected to re-enter WATCH MODE from the restored ROOT, got %q", out)
		}
		esc(master)
		esc(master)
		_ = cmd.Wait()
	})

	t.Run("direct cpro watch is unchanged: no picker, no WATCH MODE", func(t *testing.T) {
		master, slave := openPTY(t)
		capture := drainPTY(master)
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, "watch", "--interval", "5s")
		cmd.Env = append(os.Environ(), "TERM=xterm-256color")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(700 * time.Millisecond)
		out := stripANSI(capture())
		if strings.Contains(out, "WATCH MODE") {
			t.Fatalf("direct cpro watch must never show the WATCH MODE picker, got %q", out)
		}
		if !strings.Contains(out, "╭─ watch-mode@example.com") {
			t.Fatalf("expected direct cpro watch to still render the full view immediately, got %q", out)
		}
		esc(master)
		esc(master)
		_ = cmd.Wait()
	})

	t.Run("direct cpro watch --compact is unchanged", func(t *testing.T) {
		master, slave := openPTY(t)
		capture := drainPTY(master)
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, "watch", "--compact", "--interval", "5s")
		cmd.Env = append(os.Environ(), "TERM=xterm-256color")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(700 * time.Millisecond)
		out := stripANSI(capture())
		if strings.Contains(out, "WATCH MODE") {
			t.Fatalf("direct cpro watch --compact must never show the WATCH MODE picker, got %q", out)
		}
		if strings.Contains(out, "╭─ watch-mode@example.com") {
			t.Fatalf("expected direct cpro watch --compact to still render the compact view immediately, got %q", out)
		}
		esc(master)
		esc(master)
		_ = cmd.Wait()
	})
}

// TestStatusViews exercises "cpro status"'s full, compact, and compact-narrow
// rendering (main.go's renderFullView/renderCompactView) against real registered
// accounts, through a pty so terminalOutput reports true. Usage percentages are
// injected directly into each profile's cpro-usage.json cache (see usage.go's
// usageCache) rather than fetched — loadUsage treats a cache fresher than a
// minute as authoritative and skips the network call entirely — which is what
// lets this test pin exact values across the safe/warn/danger thresholds without
// a live Anthropic endpoint. This is the detailed dashboard that used to live
// under "cpro list" — moved here, unchanged, when list became a lightweight
// account listing (see TestListLightweight for that).
func TestStatusViews(t *testing.T) {
	bin, s := buildCLI(t)
	seed := func(email string, fiveHour, sevenDay float64) {
		t.Helper()
		stage := t.TempDir()
		data := fmt.Sprintf(`{"loggedIn":true,"email":%q,"authMethod":"claude.ai"}`, email)
		for name, content := range map[string]string{".credentials.json": data, ".claude.json": `{}`} {
			if err := os.WriteFile(filepath.Join(stage, name), []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.installLogin(email, stage); err != nil {
			t.Fatal(err)
		}
		resetAt := time.Now().Add(2 * time.Hour).Format(time.RFC3339Nano)
		cache := usageCache{FetchedAt: time.Now(), Usage: accountUsage{
			FiveHour: usageWindow{Utilization: fiveHour, ResetsAt: resetAt},
			SevenDay: usageWindow{Utilization: sevenDay, ResetsAt: resetAt},
		}}
		b, err := json.Marshal(cache)
		if err != nil {
			t.Fatal(err)
		}
		if err := atomicWrite(filepath.Join(s.profile(email), "cpro-usage.json"), b); err != nil {
			t.Fatal(err)
		}
	}
	seed("danger@example.com", 92, 85) // Session >= barDangerThreshold(90); Week in the warn band [80,90).
	seed("safe@example.com", 10, 20)   // both well under barWarnThreshold(80).

	render := func(t *testing.T, cols int, args ...string) string {
		t.Helper()
		master, slave := openPTY(t)
		resizePTY(t, slave, 40, uint16(cols))
		capture := drainPTY(master)
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.Env = append(os.Environ(), "TERM=xterm-256color")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		if err := cmd.Wait(); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		time.Sleep(100 * time.Millisecond)
		return capture()
	}

	t.Run("full view", func(t *testing.T) {
		out := render(t, 120, "status")
		plain := stripANSI(out)
		// Decision 0033: each account is its own independent, fully boxed
		// card (no outer "Accounts" panel, no "├─" divider between them),
		// minimal S/W usage rows (not "Session"/"Week" spelled out), and no
		// "Active sessions"/"None"/"PID"/"running" labels — the count lives
		// in the header alone.
		for _, want := range []string{
			"╭─ safe@example.com", "╭─ danger@example.com", "╰─", "●",
			"● Authenticated · 0 sessions", "Total week", "92%", "85%", "10%", "20%",
		} {
			if !strings.Contains(plain, want) {
				t.Fatalf("full view missing %q: %s", want, plain)
			}
		}
		for _, absent := range []string{
			"╭─ Accounts", "├─", "Session", "Week", "Active sessions", "None", "Resets", "pid ", "running",
		} {
			if strings.Contains(plain, absent) {
				t.Fatalf("full view should never contain %q (decision 0033's minimal redesign): %s", absent, plain)
			}
		}
		// There's no default/current account anymore: list presents every
		// account on equal footing, with no "❯" next to any of them.
		if strings.Contains(plain, "❯") {
			t.Fatalf("list should not render a selection marker next to any account: %s", plain)
		}
		if strings.Contains(plain, "░█▀▀") {
			t.Fatalf("the old ASCII logo should be gone: %s", plain)
		}
		if !strings.Contains(out, "38;2;248;113;113") { // danger red, from the 92% Session bar/percent
			t.Fatalf("expected danger red for a 92%% usage value: %q", out)
		}
		if !strings.Contains(out, "38;2;251;146;60") { // warn orange, from the 85% Week bar/percent
			t.Fatalf("expected warn orange for an 85%% usage value: %q", out)
		}
		if !strings.Contains(out, "38;2;52;211;153") { // safe green (also the "●" Authenticated dot)
			t.Fatalf("expected safe green somewhere in the output: %q", out)
		}

		// Alignment survives real ANSI styling (a real pty, unlike
		// TestStatusCardLayout's plain bytes.Buffer): both cards' headers and
		// bottoms share one card width, and every "S"/"W" row's "%" and "-"
		// land in the same column across both accounts.
		var cardWidth int
		var pctCol, dashCol = -1, -1
		for _, line := range strings.Split(plain, "\n") {
			switch {
			case strings.HasPrefix(line, currentTheme.TopLeft), strings.HasPrefix(line, currentTheme.BottomLeft):
				if w := visibleWidth(line); cardWidth == 0 {
					cardWidth = w
				} else if w != cardWidth {
					t.Fatalf("card border width drifted: got %d, want %d: %q", w, cardWidth, line)
				}
			case strings.HasPrefix(line, currentTheme.Rail()+" S"), strings.HasPrefix(line, currentTheme.Rail()+" W"):
				rs := []rune(line)
				pct, dash := -1, -1
				for i, r := range rs {
					if r == '%' && pct == -1 {
						pct = i
					}
					if r == '-' && dash == -1 {
						dash = i
					}
				}
				if pct == -1 || dash == -1 {
					t.Fatalf("usage row missing %% or -: %q", line)
				}
				if pctCol == -1 {
					pctCol, dashCol = pct, dash
				} else if pct != pctCol || dash != dashCol {
					t.Fatalf("usage row misaligned across accounts: %% at %d (want %d), - at %d (want %d): %q", pct, pctCol, dash, dashCol, line)
				}
			}
		}
	})

	t.Run("compact view", func(t *testing.T) {
		out := render(t, 120, "status", "--compact")
		plain := stripANSI(out)
		if !strings.Contains(plain, "╭─") || !strings.Contains(plain, "╰─") {
			t.Fatalf("compact view missing panel rails: %s", plain)
		}
		if !strings.Contains(plain, "S ") || !strings.Contains(plain, "W ") {
			t.Fatalf("compact view missing S/W fields: %s", plain)
		}
		if strings.Contains(plain, "❯") {
			t.Fatalf("compact view should not render a selection marker next to any account: %s", plain)
		}
		if !strings.Contains(plain, "Total week") {
			t.Fatalf("compact view missing the Total week row: %s", plain)
		}
		if strings.Count(plain, "█")+strings.Count(plain, "░") == 0 {
			t.Fatalf("compact view should render proportional bars: %s", plain)
		}
	})

	t.Run("compact-narrow view", func(t *testing.T) {
		wide := stripANSI(render(t, 120, "status", "--compact"))
		narrow := stripANSI(render(t, 40, "status", "--compact"))
		if strings.Count(narrow, "█") >= strings.Count(wide, "█") {
			t.Fatalf("narrow layout should use far fewer bar glyphs than wide: narrow=%q wide=%q", narrow, wide)
		}
		if !strings.Contains(narrow, "W █") {
			t.Fatalf("narrow Total week row should keep its \"W\" label on the single glyph: %s", narrow)
		}
	})

	t.Run("NO_COLOR", func(t *testing.T) {
		colored := stripANSI(render(t, 120, "status"))
		t.Setenv("NO_COLOR", "1")
		out := render(t, 120, "status")
		if strings.Contains(out, "\x1b") {
			t.Fatalf("NO_COLOR should suppress every escape code: %q", out)
		}
		// The geometry (every line's visible width) must be identical with
		// or without color — color must never substitute for alignment.
		coloredLines, plainLines := strings.Split(colored, "\n"), strings.Split(out, "\n")
		if len(coloredLines) != len(plainLines) {
			t.Fatalf("NO_COLOR produced a different number of lines: %d vs %d", len(plainLines), len(coloredLines))
		}
		for i := range coloredLines {
			if visibleWidth(coloredLines[i]) != visibleWidth(plainLines[i]) {
				t.Fatalf("NO_COLOR line %d width drifted: %q vs %q", i, plainLines[i], coloredLines[i])
			}
		}
	})

	// Real, /proc-scanned running sessions (see runningSessions, claude.go) —
	// not seeded usage cache data — driving the header's singular/plural
	// wording and the session rows' real path/pid, end to end through a real
	// pty (proving alignment survives ANSI styling for session rows too, not
	// just usage rows above).
	t.Run("active sessions: singular/plural, real path/pid, no wrap", func(t *testing.T) {
		spawnSession := func(t *testing.T, profileDir, dir string) *exec.Cmd {
			t.Helper()
			master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0600)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { master.Close() })
			var unlocked int32
			if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlocked))); errno != 0 {
				t.Fatal(errno)
			}
			var number uint32
			if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), syscall.TIOCGPTN, uintptr(unsafe.Pointer(&number))); errno != 0 {
				t.Fatal(errno)
			}
			slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", number), os.O_RDWR|syscall.O_NOCTTY, 0600)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { slave.Close() })
			cmd := exec.Command("sleep", "30")
			cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "CLAUDE_CONFIG_DIR="+profileDir)
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
			return cmd
		}

		dirA, dirB1, dirB2 := t.TempDir(), t.TempDir(), t.TempDir()
		sessA := spawnSession(t, s.profile("danger@example.com"), dirA)
		sessB1 := spawnSession(t, s.profile("safe@example.com"), dirB1)
		sessB2 := spawnSession(t, s.profile("safe@example.com"), dirB2)

		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if len(runningSessions(s.profile("danger@example.com"))) == 1 && len(runningSessions(s.profile("safe@example.com"))) == 2 {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}

		out := stripANSI(render(t, 120, "status"))
		if !strings.Contains(out, "· 1 session ") {
			t.Fatalf("expected singular \"1 session\" for the account with exactly one real running session: %s", out)
		}
		if !strings.Contains(out, "· 2 sessions") {
			t.Fatalf("expected plural \"2 sessions\" for the account with two real running sessions: %s", out)
		}
		for _, pid := range []int{sessA.Process.Pid, sessB1.Process.Pid, sessB2.Process.Pid} {
			if !strings.Contains(out, fmt.Sprintf("%d", pid)) {
				t.Fatalf("expected pid %d among the rendered session rows: %s", pid, out)
			}
		}
		// No card line ever wraps: header/body/bottom all share one width.
		var width int
		for _, line := range strings.Split(out, "\n") {
			if line == "" {
				continue
			}
			if strings.HasPrefix(line, currentTheme.TopLeft) || strings.HasPrefix(line, currentTheme.Rail()) || strings.HasPrefix(line, currentTheme.BottomLeft) {
				if w := visibleWidth(line); width == 0 {
					width = w
				} else if w != width {
					t.Fatalf("card line width drifted (a row wrapped): got %d, want %d: %q", w, width, line)
				}
			}
		}
	})
}

// TestListLightweight covers "cpro list"'s own scope after the list/status
// split: identity and authentication state only. It deliberately checks for
// the ABSENCE of everything that moved to "cpro status" (usage bars, reset
// times, session/PID detail, a total-week summary) as directly as it checks
// for the presence of what list actually shows, since the whole point of the
// split is that list no longer does or renders any of that.
func TestListLightweight(t *testing.T) {
	bin, s := buildCLI(t)
	run := func(code int, args ...string) (string, string) {
		t.Helper()
		cmd := exec.Command(bin, args...)
		var out, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &stderr
		err := cmd.Run()
		got := 0
		if err != nil {
			if e, ok := err.(*exec.ExitError); ok {
				got = e.ExitCode()
			} else {
				t.Fatal(err)
			}
		}
		if got != code {
			t.Fatalf("%q: exit %d, want %d; stdout %s stderr %s", args, got, code, &out, &stderr)
		}
		return out.String(), stderr.String()
	}
	login := func(email string) {
		t.Helper()
		stage := t.TempDir()
		data := fmt.Sprintf(`{"loggedIn":true,"email":%q,"authMethod":"claude.ai"}`, email)
		if err := os.WriteFile(filepath.Join(stage, ".credentials.json"), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(stage, ".claude.json"), []byte(`{}`), 0600); err != nil {
			t.Fatal(err)
		}
		if err := s.installLogin(email, stage); err != nil {
			t.Fatal(err)
		}
	}
	login("auth@example.com")
	login("signedout@example.com")
	run(0, "logout", "signedout@example.com") // still registered, no longer authenticated

	t.Run("--json: identity and auth state only, no sessions key at all", func(t *testing.T) {
		out, _ := run(0, "list", "--json")
		var listed struct {
			Version  int
			Default  string
			Accounts []struct {
				Email         string
				Default       bool
				Authenticated bool
			}
		}
		if err := json.Unmarshal([]byte(out), &listed); err != nil {
			t.Fatalf("list --json: %s: %v", out, err)
		}
		byEmail := map[string]bool{}
		for _, a := range listed.Accounts {
			byEmail[a.Email] = a.Authenticated
		}
		if !byEmail["auth@example.com"] || byEmail["signedout@example.com"] {
			t.Fatalf("list --json authenticated state: %+v", listed)
		}
		if strings.Contains(out, "session") { // catches "sessions", "Sessions", etc. — the key must not exist at all
			t.Fatalf("list --json must not have a sessions field at all (that's status's concern), got %s", out)
		}
	})

	t.Run("--compact no longer exists on list (moved to status)", func(t *testing.T) {
		_, stderr := run(1, "list", "--compact")
		if !strings.Contains(stderr, "unknown flag") {
			t.Fatalf("expected --compact to be rejected on list, got %s", stderr)
		}
	})

	t.Run("plain (non-tty) output: identity and auth state only", func(t *testing.T) {
		out, _ := run(0, "list")
		if !strings.Contains(out, "auth@example.com") || !strings.Contains(out, "Authenticated") {
			t.Fatalf("expected the authenticated account listed, got %q", out)
		}
		if !strings.Contains(out, "signedout@example.com") || !strings.Contains(out, "Signed out") {
			t.Fatalf("expected the signed-out account listed as such, got %q", out)
		}
		for _, absent := range []string{"Session", "Week", "session running", "Active sessions", "Total week", "Resets", "%"} {
			if strings.Contains(out, absent) {
				t.Fatalf("plain list output should never contain %q (that moved to status), got %q", absent, out)
			}
		}
	})

	t.Run("interactive panel: accounts and auth state, no cursor, no usage/session data", func(t *testing.T) {
		master, slave := openPTY(t)
		resizePTY(t, slave, 40, 120)
		capture := drainPTY(master)
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, "list")
		cmd.Env = append(os.Environ(), "TERM=xterm-256color")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		if err := cmd.Wait(); err != nil {
			t.Fatalf("cpro list: %v; output %q", err, stripANSI(capture()))
		}
		time.Sleep(100 * time.Millisecond)
		plain := stripANSI(capture())

		for _, want := range []string{"╭─ Accounts", "╰─", "●", "○", "Authenticated", "Signed out", "auth@example.com", "signedout@example.com"} {
			if !strings.Contains(plain, want) {
				t.Fatalf("list panel missing %q: %s", want, plain)
			}
		}
		if strings.Contains(plain, "❯") {
			t.Fatalf("list is informational only and must not render a selection cursor: %s", plain)
		}
		for _, absent := range []string{
			"Session", "Week", "█", "░", "Resets", "Active sessions", "Total week",
			"%", // no usage percentage anywhere
		} {
			if strings.Contains(plain, absent) {
				t.Fatalf("list panel should never contain %q (usage/session data moved to status): %s", absent, plain)
			}
		}
	})
}

// TestEmailMaskingGlobal covers decision 0024: masking is centralized
// through one formatter (displayEmail, ui.go) that every UI/output path
// reads live account state through, rather than each screen implementing its
// own — so turning it on protects every navigable screen at once, not just
// cpro list/status (which already had their own, now-consolidated version).
// Every subtest below shares one pair of logged-in accounts and one
// mask-on toggle so the two aliases (read once from config.json) are
// asserted identical everywhere they appear, proving they're the same
// persisted value everywhere, not independently regenerated per screen.
func TestEmailMaskingGlobal(t *testing.T) {
	bin, s := buildCLI(t)
	run := func(code int, args ...string) (string, string) {
		t.Helper()
		cmd := exec.Command(bin, args...)
		var out, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &stderr
		err := cmd.Run()
		got := 0
		if err != nil {
			if e, ok := err.(*exec.ExitError); ok {
				got = e.ExitCode()
			} else {
				t.Fatal(err)
			}
		}
		if got != code {
			t.Fatalf("%q: exit %d, want %d; stdout %s stderr %s", args, got, code, &out, &stderr)
		}
		return out.String(), stderr.String()
	}
	login := func(email string) {
		t.Helper()
		stage := t.TempDir()
		data := fmt.Sprintf(`{"loggedIn":true,"email":%q,"authMethod":"claude.ai"}`, email)
		if err := os.WriteFile(filepath.Join(stage, ".credentials.json"), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(stage, ".claude.json"), []byte(`{}`), 0600); err != nil {
			t.Fatal(err)
		}
		if err := s.installLogin(email, stage); err != nil {
			t.Fatal(err)
		}
	}
	const emailA, emailB = "alice@example.com", "bob@example.net"
	login(emailA) // becomes config.Default (first login) — see installLogin
	login(emailB)
	run(0, "config", "mask", "on")

	cfg, err := s.read()
	if err != nil {
		t.Fatal(err)
	}
	aliasA, aliasB := cfg.EmailMasks[emailA], cfg.EmailMasks[emailB]
	if aliasA == "" || aliasB == "" || aliasA == aliasB {
		t.Fatalf("expected two distinct, non-empty aliases once masking is on, got %+v", cfg.EmailMasks)
	}
	// No fragment of the real address should survive into its own alias —
	// the whole point of an opaque "word@word" placeholder over a partial
	// mask like "a***@example.com".
	for email, alias := range map[string]string{emailA: aliasA, emailB: aliasB} {
		local, domain, _ := strings.Cut(email, "@")
		if strings.Contains(alias, local) || strings.Contains(alias, domain) {
			t.Fatalf("alias %q for %q leaks a fragment of the real address", alias, email)
		}
	}
	assertNoRealEmail := func(t *testing.T, out string) {
		t.Helper()
		if strings.Contains(out, emailA) || strings.Contains(out, emailB) {
			t.Fatalf("expected no real email while masking is on, got %q", out)
		}
	}

	t.Run("cpro list masks accounts (plain and interactive), --json stays unmasked for scripts", func(t *testing.T) {
		out, _ := run(0, "list")
		assertNoRealEmail(t, out)
		if !strings.Contains(out, aliasA) || !strings.Contains(out, aliasB) {
			t.Fatalf("expected both aliases in plain list output, got %q", out)
		}
		jsonOut, _ := run(0, "list", "--json")
		if !strings.Contains(jsonOut, emailA) || !strings.Contains(jsonOut, emailB) {
			t.Fatalf("expected --json to keep the real, scriptable email even while masking is on, got %q", jsonOut)
		}

		master, slave := openPTY(t)
		resizePTY(t, slave, 40, 120)
		capture := drainPTY(master)
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, "list")
		cmd.Env = append(os.Environ(), "TERM=xterm-256color")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		if err := cmd.Wait(); err != nil {
			t.Fatalf("cpro list: %v; output %q", err, stripANSI(capture()))
		}
		time.Sleep(100 * time.Millisecond)
		plain := stripANSI(capture())
		assertNoRealEmail(t, plain)
		if !strings.Contains(plain, aliasA) || !strings.Contains(plain, aliasB) {
			t.Fatalf("expected both aliases in the interactive list panel, got %q", plain)
		}
	})

	t.Run("cpro status masks accounts (plain and interactive)", func(t *testing.T) {
		out, _ := run(0, "status", "--json")
		if !strings.Contains(out, emailA) {
			t.Fatalf("expected --json to keep the real email even while masking is on, got %q", out)
		}
		master, slave := openPTY(t)
		resizePTY(t, slave, 40, 120)
		capture := drainPTY(master)
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, "status", "--compact")
		cmd.Env = append(os.Environ(), "TERM=xterm-256color")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(600 * time.Millisecond)
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		plain := stripANSI(capture())
		assertNoRealEmail(t, plain)
		if !strings.Contains(plain, aliasA) || !strings.Contains(plain, aliasB) {
			t.Fatalf("expected both aliases in cpro status --compact, got %q", plain)
		}
	})

	t.Run("RUN ACCOUNT masks every account and search matches the alias, not the real email", func(t *testing.T) {
		master, slave := openPTY(t)
		capture := drainPTY(master)
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin)
		cmd.Env = append(os.Environ(), "TERM=xterm-256color")
		cmd.Stdin, cmd.Stderr = slave, slave
		var stdout bytes.Buffer
		cmd.Stdout = &stdout
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(500 * time.Millisecond)
		send := func(s string) {
			if _, err := master.WriteString(s); err != nil {
				t.Fatal(err)
			}
			time.Sleep(200 * time.Millisecond)
		}
		send("\x1b[C") // "run" is highlighted by default -> RUN ACCOUNT
		time.Sleep(300 * time.Millisecond)
		plain := stripANSI(capture())
		assertNoRealEmail(t, plain)
		if !strings.Contains(plain, aliasA) || !strings.Contains(plain, aliasB) {
			t.Fatalf("expected both aliases in RUN ACCOUNT, got %q", plain)
		}
		// Search must match the visible alias, not the real email — typing
		// the real local-part must find nothing, typing the alias must.
		local, _, _ := strings.Cut(emailA, "@")
		for _, r := range local {
			send(string(r))
		}
		time.Sleep(200 * time.Millisecond)
		if out := stripANSI(capture()); !strings.Contains(out, "No accounts found") {
			t.Fatalf("expected searching the real local-part to find nothing while masked, got %q", out)
		}
		for range len([]rune(local)) {
			send("\x7f") // backspace
		}
		aliasLocal, _, _ := strings.Cut(aliasA, "@")
		for _, r := range aliasLocal {
			send(string(r))
		}
		time.Sleep(200 * time.Millisecond)
		filtered := stripANSI(capture())
		assertNoRealEmail(t, filtered)
		if !strings.Contains(filtered, aliasA) {
			t.Fatalf("expected searching the alias to find it, got %q", filtered)
		}
		send("\r") // pick the filtered account, proceed to RUN MODE
		time.Sleep(300 * time.Millisecond)
		send("\r") // pick the preselected mode, launch
		if err := cmd.Wait(); err != nil {
			t.Fatalf("interactive run flow: %v; stderr %q; stdout %q", err, stripANSI(capture()), stdout.String())
		}
		// The real account (not its alias) must still be what actually ran —
		// masking is presentation-only, see claude.go's own real invocation.
		var forwarded struct{ Directory string }
		lines := strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n")
		if err := json.Unmarshal([]byte(lines[len(lines)-1]), &forwarded); err != nil {
			t.Fatalf("parsing fake claude output: %s: %v", stdout.String(), err)
		}
		if forwarded.Directory != s.profile(emailA) {
			t.Fatalf("expected the real account's profile directory despite searching by alias, got %q", forwarded.Directory)
		}
	})

	t.Run("CONFIG's Default row and the SET DEFAULT ACCOUNT screen mask accounts", func(t *testing.T) {
		master, slave := openPTY(t)
		capture := drainPTY(master)
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, "config")
		cmd.Env = append(os.Environ(), "TERM=xterm-256color")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(500 * time.Millisecond)
		send := func(s string) {
			if _, err := master.WriteString(s); err != nil {
				t.Fatal(err)
			}
			time.Sleep(200 * time.Millisecond)
		}
		plain := stripANSI(capture())
		assertNoRealEmail(t, plain)
		if !strings.Contains(plain, aliasA) {
			t.Fatalf("expected CONFIG's own Default row to show the default account's alias, got %q", plain)
		}
		send("\r") // Default is cursor 0 — opens SET DEFAULT ACCOUNT nested
		time.Sleep(200 * time.Millisecond)
		screenOut := stripANSI(capture())
		assertNoRealEmail(t, screenOut)
		if !strings.Contains(screenOut, aliasA) || !strings.Contains(screenOut, aliasB) {
			t.Fatalf("expected both aliases in SET DEFAULT ACCOUNT's own account rows, got %q", screenOut)
		}
		send("\x1b") // cancel
		for range 2 {
			send("\x1b") // arm, then confirm
		}
		if err := cmd.Wait(); err != nil {
			t.Fatalf("cpro config: %v; output %q", err, stripANSI(capture()))
		}
	})

	// Direct calls, no pty: bubbletea's diffing renderer makes a same-screen
	// state change (toggling Mask emails, then re-rendering the very same
	// CONFIG menu) unreliable to prove via a cumulative pty capture — the
	// same reasoning TestPermissionsPreviewTracksCursor/TestRootPickerExitArmed
	// already document for exactly this class of test. Runs against its own
	// isolated store (not the outer test's shared bin/s) so toggling here
	// can't leave the shared config's MaskEmail in the wrong state for the
	// subtests that follow.
	t.Run("toggling Mask emails updates the currently running screen immediately, no restart", func(t *testing.T) {
		origEnabled, origTable := maskEmailEnabled, emailMaskTable
		t.Cleanup(func() { maskEmailEnabled, emailMaskTable = origEnabled, origTable })

		isolated := &store{dir: t.TempDir()}
		if err := isolated.update(func(c *config) error {
			c.Accounts = map[string]bool{emailA: true}
			c.Default = emailA
			c.MaskEmail = true
			c.EmailMasks = map[string]string{emailA: aliasA}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		maskEmailEnabled, emailMaskTable = true, map[string]string{emailA: aliasA}

		c, err := isolated.read()
		if err != nil {
			t.Fatal(err)
		}
		m := &configApp{s: isolated, c: c, stack: newNavStack(screenMenu)}
		before := m.viewMenu()
		if !strings.Contains(before, aliasA) || strings.Contains(before, emailA) {
			t.Fatalf("expected the alias while masking starts on, got %q", before)
		}

		m.cursor = 1 // default(0), mask(1) — configMenuItemsConfig's own order (hasParent defaults false)
		m.updateMenu(tea.KeyPressMsg{Code: tea.KeyEnter})
		afterOff := m.viewMenu()
		if strings.Contains(afterOff, aliasA) || !strings.Contains(afterOff, emailA) {
			t.Fatalf("expected the real email immediately after disabling masking, no restart, got %q", afterOff)
		}

		// Re-enabling generates a fresh alias (saveMaskEmail's own documented
		// behavior — a placeholder never survives past the toggle that showed
		// it), so this checks that *some* alias is shown, not specifically
		// aliasA again; stability while masking stays continuously on (never
		// toggled) is what the other subtests above already prove, reading
		// the one alias persisted by the outer test's own single mask-on
		// toggle identically across list/status/RUN ACCOUNT/Settings.
		m.updateMenu(tea.KeyPressMsg{Code: tea.KeyEnter}) // toggle back on
		afterOn := m.viewMenu()
		if strings.Contains(afterOn, emailA) {
			t.Fatalf("expected the real email hidden immediately after re-enabling masking, no restart, got %q", afterOn)
		}
		newAlias, ok := emailMaskTable[emailA]
		if !ok || newAlias == "" || !strings.Contains(afterOn, newAlias) {
			t.Fatalf("expected a freshly generated alias immediately after re-enabling masking, no restart, got %q (table %+v)", afterOn, emailMaskTable)
		}
	})

	t.Run("system export's own account picker masks accounts", func(t *testing.T) {
		master, slave := openPTY(t)
		capture := drainPTY(master)
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, "system", "export")
		cmd.Env = append(os.Environ(), "TERM=xterm-256color")
		cmd.Stdin, cmd.Stderr = slave, slave
		var stdout bytes.Buffer
		cmd.Stdout = &stdout
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(500 * time.Millisecond)
		plain := stripANSI(capture())
		assertNoRealEmail(t, plain)
		if !strings.Contains(plain, aliasA) || !strings.Contains(plain, aliasB) {
			t.Fatalf("expected both aliases in the export account picker, got %q", plain)
		}
		if _, err := master.WriteString("\x1b"); err != nil { // cancel
			t.Fatal(err)
		}
		_ = cmd.Wait()
	})

	t.Run("underlying persisted account identities are unchanged by masking", func(t *testing.T) {
		final, err := s.read()
		if err != nil {
			t.Fatal(err)
		}
		if !final.Accounts[emailA] || !final.Accounts[emailB] {
			t.Fatalf("expected both real emails to remain the actual registered account keys, got %+v", final.Accounts)
		}
		if final.Default != emailA {
			t.Fatalf("expected the real email to remain the stored default account, got %q", final.Default)
		}
	})
}

// TestEmailMaskingIdentityIntegrity locks in the specific invariant Kaneo
// task TASK-13 requires: an alias is a presentation string only and must
// never work as an account identifier anywhere real business logic runs.
// cpro run, cpro session continue, and cpro system export must all resolve
// and operate on the real email while masking is on, and an alias must be
// rejected exactly like any other unregistered account. TestEmailMaskingGlobal
// already covers rendering/search; this test covers the actual side-effecting
// operations the task calls out by name.
func TestEmailMaskingIdentityIntegrity(t *testing.T) {
	bin, s := buildCLI(t)
	login := func(email string) {
		t.Helper()
		stage := t.TempDir()
		data := fmt.Sprintf(`{"loggedIn":true,"email":%q,"authMethod":"claude.ai"}`, email)
		if err := os.WriteFile(filepath.Join(stage, ".credentials.json"), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(stage, ".claude.json"), []byte(`{}`), 0600); err != nil {
			t.Fatal(err)
		}
		if err := s.installLogin(email, stage); err != nil {
			t.Fatal(err)
		}
	}
	const emailA, emailB = "mask-a@example.com", "mask-b@example.net"
	login(emailA)
	login(emailB)

	run := func(t *testing.T, wantCode int, dir string, args ...string) (string, string) {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Dir = dir
		cmd.Env = os.Environ()
		var out, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &stderr
		err := cmd.Run()
		got := 0
		if err != nil {
			if e, ok := err.(*exec.ExitError); ok {
				got = e.ExitCode()
			} else {
				t.Fatal(err)
			}
		}
		if got != wantCode {
			t.Fatalf("%q: exit %d, want %d; stdout %s stderr %s", args, got, wantCode, &out, &stderr)
		}
		return out.String(), stderr.String()
	}

	run(t, 0, "", "config", "mask", "on")
	cfg, err := s.read()
	if err != nil {
		t.Fatal(err)
	}
	aliasA, aliasB := cfg.EmailMasks[emailA], cfg.EmailMasks[emailB]
	if aliasA == "" || aliasB == "" {
		t.Fatalf("expected both accounts to have aliases once masking is on, got %+v", cfg.EmailMasks)
	}

	t.Run("cpro run uses the real account while masking is on", func(t *testing.T) {
		out, _ := run(t, 0, "", "run", "--account", emailA)
		var forwarded struct {
			Args      []string
			Directory string
		}
		if err := json.Unmarshal([]byte(out), &forwarded); err != nil {
			t.Fatalf("parsing fake claude output: %s: %v", out, err)
		}
		if forwarded.Directory != s.profile(emailA) {
			t.Fatalf("expected CLAUDE_CONFIG_DIR for the real account %q, got %q", emailA, forwarded.Directory)
		}
	})

	t.Run("cpro run rejects an alias as an account identifier", func(t *testing.T) {
		_, stderr := run(t, 1, "", "run", "--account", aliasA)
		if !strings.Contains(stderr, "not registered") {
			t.Fatalf("expected the alias to be rejected as an unregistered account, got %q", stderr)
		}
	})

	t.Run("cpro session continue uses the real accounts while masking is on", func(t *testing.T) {
		projectDir := t.TempDir()
		dirName := projectDirName(projectDir)
		srcSessions := filepath.Join(s.profile(emailA), "projects", dirName)
		if err := os.MkdirAll(srcSessions, 0700); err != nil {
			t.Fatal(err)
		}
		const sessionID = "22222222-2222-2222-2222-222222222222"
		if err := os.WriteFile(filepath.Join(srcSessions, sessionID+".jsonl"), []byte(`{"type":"user"}`+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		run(t, 0, projectDir, "session", "continue", emailA, emailB)
		dst := filepath.Join(s.profile(emailB), "projects", dirName, sessionID+".jsonl")
		if _, err := os.Stat(dst); err != nil {
			t.Fatalf("expected the transcript under the real destination account's profile, got %v", err)
		}
	})

	t.Run("cpro system export writes the real account's own credentials", func(t *testing.T) {
		systemDir := t.TempDir()
		t.Setenv("CLAUDE_CONFIG_DIR", systemDir)
		run(t, 0, "", "system", "export", emailA)
		got, err := os.ReadFile(filepath.Join(systemDir, ".credentials.json"))
		if err != nil {
			t.Fatal(err)
		}
		want, err := os.ReadFile(filepath.Join(s.profile(emailA), ".credentials.json"))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Fatalf("expected the exported credentials to match the real account %q, got %s want %s", emailA, got, want)
		}
	})

	// Toggling masking may only ever touch MaskEmail and the alias table it
	// owns (EmailMasks — regenerated on every toggle by design, see
	// saveMaskEmail). Every other field, account keys and Default included,
	// must come back byte-identical: a toggle is a display preference, never
	// an identity or auth migration.
	t.Run("toggling masking changes only the masking preference and its alias table", func(t *testing.T) {
		before, err := s.read()
		if err != nil {
			t.Fatal(err)
		}
		run(t, 0, "", "config", "mask", "off")
		run(t, 0, "", "config", "mask", "on")
		after, err := s.read()
		if err != nil {
			t.Fatal(err)
		}
		if !after.MaskEmail {
			t.Fatalf("expected masking to end up on, got %+v", after)
		}
		before.MaskEmail, after.MaskEmail = false, false
		before.EmailMasks, after.EmailMasks = nil, nil
		if !reflect.DeepEqual(before, after) {
			t.Fatalf("toggling masking changed more than the masking preference:\nbefore %+v\nafter  %+v", before, after)
		}
		// Credentials are a separate store from config.json and must be just
		// as untouched — the task's "masking never reloads or rewrites
		// credentials" requirement.
		for _, email := range []string{emailA, emailB} {
			raw, err := os.ReadFile(filepath.Join(s.profile(email), ".credentials.json"))
			if err != nil {
				t.Fatalf("%s: credentials should survive a masking toggle: %v", email, err)
			}
			if !strings.Contains(string(raw), email) {
				t.Fatalf("%s: expected the real email to remain inside its own credentials, got %s", email, raw)
			}
		}
	})
}

// TestAnnounceTarget covers the one thing masking changes about the decision-0007
// announce line (announceTarget, ui.go). Masking off: the literal, runnable
// command, unchanged. Masking on: no command shape at all — an alias is never an
// account identifier cpro run accepts, so printing `cpro run --account <alias>`
// would be a command that fails if anyone ran it (TASK-13's "never generate"
// rule). Pure, in-process; restores the package-level mask globals so it can't
// leak into another test sharing this binary.
func TestAnnounceTarget(t *testing.T) {
	origEnabled, origTable := maskEmailEnabled, emailMaskTable
	t.Cleanup(func() { maskEmailEnabled, emailMaskTable = origEnabled, origTable })

	const email, alias = "real@example.com", "placeholder-fox"
	readonly := permissionModeArgs("readonly")

	maskEmailEnabled, emailMaskTable = false, map[string]string{email: alias}
	got := announceTarget(email, readonly)
	want := "cpro run --account real@example.com --permission-mode plan"
	if got != want {
		t.Fatalf("masking off: got %q, want %q", got, want)
	}

	maskEmailEnabled = true
	got = announceTarget(email, readonly)
	if want := alias + " · Read-only"; got != want {
		t.Fatalf("masking on: got %q, want %q", got, want)
	}
	if strings.Contains(got, email) {
		t.Fatalf("masking on: the real email must not appear, got %q", got)
	}
	// The critical half of the rule: no runnable-looking command, so nothing
	// invites a copy-paste that would fail against an alias.
	if strings.Contains(got, "cpro run") || strings.Contains(got, "--account") {
		t.Fatalf("masking on: expected no command shape, got %q", got)
	}

	// "ask" carries no flags at all — empty args must still resolve to its own
	// label rather than falling through to the bare-account fallback.
	if got, want := announceTarget(email, permissionModeArgs("ask")), alias+" · Ask for everything"; got != want {
		t.Fatalf("masking on, ask mode: got %q, want %q", got, want)
	}
	// Arbitrary forwarded Claude flags are nobody's mode argv: fall back to the
	// account alone rather than mislabelling them as a mode.
	if got, want := announceTarget(email, []string{"--model", "opus"}), alias; got != want {
		t.Fatalf("masking on, non-mode args: got %q, want %q", got, want)
	}
}

// TestAnnounceCommand covers decision 0037: the "Running: ..." announcement
// (announceCommand, ui.go) draws in currentTheme's own runes — "╭─ Running:"
// / "│  <command>" / "╰─" — via the same accent(w, currentTheme.Top()/
// .Rail()/.Bottom(), color) idiom cpro info/doctor (maintenance.go) and cpro
// list/status (main.go) already use, replacing the fixed "┃"-per-line bar
// announceLine drew before this decision (now removed — this was its only
// caller). Pure, in-process: builds a bare *cobra.Command with its own
// captured stdout buffer rather than a real subprocess, the same pattern
// TestListRenderPrimitives/TestStatusCardLayout already use for a
// non-interactive printed panel.
func TestAnnounceCommand(t *testing.T) {
	t.Run("draws the current theme's own top/rail/bottom runes around the command", func(t *testing.T) {
		for _, theme := range borderThemes {
			orig := currentTheme
			t.Cleanup(func() { currentTheme = orig })
			currentTheme = theme

			var buf bytes.Buffer
			cmd := &cobra.Command{Use: "cpro"}
			cmd.SetOut(&buf)
			announceCommand(cmd, "a@example.com", permissionModeArgs("yolo"))

			got := buf.String()
			want := theme.Top() + " Running:\n" +
				theme.Rail() + "  cpro run --account a@example.com --permission-mode bypassPermissions\n" +
				theme.Bottom() + "\n"
			if got != want {
				t.Fatalf("theme %s: got %q, want %q", theme.Name, got, want)
			}
		}
	})

	t.Run("non-terminal output stays plain, no theme runes or color escapes", func(t *testing.T) {
		var buf bytes.Buffer
		cmd := &cobra.Command{Use: "cpro"}
		cmd.SetOut(&buf) // a *bytes.Buffer is never a terminal — accent()/terminalOutput degrade to plain text
		announceCommand(cmd, "a@example.com", permissionModeArgs("readonly"))
		got := buf.String()
		want := currentTheme.Top() + " Running:\n" +
			currentTheme.Rail() + "  cpro run --account a@example.com --permission-mode plan\n" +
			currentTheme.Bottom() + "\n"
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
		if strings.Contains(got, "\x1b[") {
			t.Fatalf("expected no ANSI escapes on non-terminal output, got %q", got)
		}
	})
}

// TestDisplayEmail covers the shared formatter (ui.go) directly: real email
// when masking is off, the registered alias when on, and a safe fallback to
// the real email if a mask is somehow still missing (an older config
// predating decision 0024's eager per-login backfill) rather than showing
// nothing. Saves and restores the package-level mask globals so this pure,
// in-process test can't leak state into any other test sharing this binary.
func TestDisplayEmail(t *testing.T) {
	origEnabled, origTable := maskEmailEnabled, emailMaskTable
	t.Cleanup(func() { maskEmailEnabled, emailMaskTable = origEnabled, origTable })

	maskEmailEnabled = false
	emailMaskTable = map[string]string{"a@example.com": "kufi@ponuri"}
	if got := displayEmail("a@example.com"); got != "a@example.com" {
		t.Fatalf("masking off: expected the real email, got %q", got)
	}

	maskEmailEnabled = true
	if got := displayEmail("a@example.com"); got != "kufi@ponuri" {
		t.Fatalf("masking on: expected the registered alias, got %q", got)
	}
	if got := displayEmail("b@example.com"); got != "b@example.com" {
		t.Fatalf("masking on, no alias registered yet: expected a safe fallback to the real email, got %q", got)
	}
}

// TestConfigPreferences covers cpro config accent|bar|mask: the scriptable form,
// Cobra-level rejection of an invalid value (cobra.OnlyValidArgs), and that the
// interactive target picker's "accent" case really reaches setAccentColor and not
// just the scriptable path exercised first.
func TestConfigPreferences(t *testing.T) {
	bin, s := buildCLI(t)
	run := func(code int, args ...string) (string, string) {
		t.Helper()
		cmd := exec.Command(bin, args...)
		var out, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &stderr
		err := cmd.Run()
		got := 0
		if err != nil {
			if e, ok := err.(*exec.ExitError); ok {
				got = e.ExitCode()
			} else {
				t.Fatal(err)
			}
		}
		if got != code {
			t.Fatalf("%q: exit %d, want %d; stdout %s stderr %s", args, got, code, &out, &stderr)
		}
		return out.String(), stderr.String()
	}

	run(0, "config", "accent", "blue")
	run(0, "config", "bar", "cyan")
	run(0, "config", "mask", "on")
	run(1, "config", "accent", "notacolor")
	run(1, "config", "bar", "notacolor")
	run(1, "config", "trust", "maybe")
	run(1, "config", "mask", "maybe")
	// Yellow/pink aren't in the palette at all, for any color preference —
	// dropped from the original 8-color set (see colorPalette, ui.go).
	// Orange and Red, by contrast, WERE added to the shared palette
	// specifically so Warning/Danger color (below) could pick them — that
	// also makes them valid, non-special-cased choices for accent/bar too,
	// unlike the previous palette where they didn't exist at all.
	run(1, "config", "bar", "yellow")
	run(1, "config", "accent", "pink")
	run(0, "config", "accent", "orange")
	run(0, "config", "bar", "red")
	run(0, "config", "accent", "blue") // restore, so the assertions below hold
	run(0, "config", "bar", "cyan")    // restore

	out, _ := run(0, "config", "--json")
	var cfg struct {
		AccentColor string `json:"accentColor"`
		BarColor    string `json:"barColor"`
		MaskEmail   bool   `json:"maskEmail"`
	}
	if err := json.Unmarshal([]byte(out), &cfg); err != nil {
		t.Fatalf("config --json: %s: %v", out, err)
	}
	if cfg.AccentColor != "Blue" || cfg.BarColor != "Cyan" || !cfg.MaskEmail {
		t.Fatalf("scriptable config preferences did not persist: %+v", cfg)
	}
	run(0, "config", "mask", "off")

	// Warning/danger color: scriptable, shares the same palette/validation as
	// accent/bar (colorCommand, config.go), persisted, and defaults to
	// Orange/Red when never set.
	out, _ = run(0, "config", "--json")
	var colorDefaults struct {
		WarningColor string `json:"warningColor"`
		DangerColor  string `json:"dangerColor"`
	}
	if err := json.Unmarshal([]byte(out), &colorDefaults); err != nil {
		t.Fatalf("config --json: %s: %v", out, err)
	}
	if colorDefaults.WarningColor != "Orange" || colorDefaults.DangerColor != "Red" {
		t.Fatalf("expected default Warning=Orange/Danger=Red before ever setting them, got %+v", colorDefaults)
	}
	run(1, "config", "warning-color", "notacolor")
	run(1, "config", "danger-color", "notacolor")
	run(0, "config", "warning-color", "amber")
	run(0, "config", "danger-color", "violet")
	out, _ = run(0, "config", "--json")
	if err := json.Unmarshal([]byte(out), &colorDefaults); err != nil {
		t.Fatalf("config --json: %s: %v", out, err)
	}
	if colorDefaults.WarningColor != "Amber" || colorDefaults.DangerColor != "Violet" {
		t.Fatalf("warning/danger color did not persist: %+v", colorDefaults)
	}
	run(0, "config", "warning-color", "orange") // restore
	run(0, "config", "danger-color", "red")     // restore

	// Theme: scriptable, validated, persisted, and defaults to Rounded.
	out, _ = run(0, "config", "--json")
	var themeCfg struct {
		Theme string `json:"theme"`
	}
	if err := json.Unmarshal([]byte(out), &themeCfg); err != nil {
		t.Fatalf("config --json: %s: %v", out, err)
	}
	if themeCfg.Theme != "Rounded" {
		t.Fatalf("expected the default theme to be Rounded before ever setting it, got %+v", themeCfg)
	}
	run(1, "config", "theme", "notatheme")
	run(0, "config", "theme", "heavy")
	out, _ = run(0, "config", "--json")
	if err := json.Unmarshal([]byte(out), &themeCfg); err != nil {
		t.Fatalf("config --json: %s: %v", out, err)
	}
	if themeCfg.Theme != "Heavy" {
		t.Fatalf("theme did not persist: %+v", themeCfg)
	}
	run(0, "config", "theme", "rounded") // restore

	// Bar warn/danger thresholds: scriptable, validated against each other, and
	// persisted.
	run(0, "config", "warn-at", "70")
	run(0, "config", "danger-at", "95")
	run(1, "config", "warn-at", "150")   // out of range
	run(1, "config", "warn-at", "95")    // not below the current danger threshold
	run(1, "config", "danger-at", "70")  // not above the current warn threshold
	run(1, "config", "danger-at", "abc") // not a number

	out, _ = run(0, "config", "--json")
	var thresholds struct {
		BarWarnThreshold   float64 `json:"barWarnThreshold"`
		BarDangerThreshold float64 `json:"barDangerThreshold"`
	}
	if err := json.Unmarshal([]byte(out), &thresholds); err != nil {
		t.Fatalf("config --json: %s: %v", out, err)
	}
	if thresholds.BarWarnThreshold != 70 || thresholds.BarDangerThreshold != 95 {
		t.Fatalf("bar thresholds did not persist: %+v", thresholds)
	}

	if _, err := s.read(); err != nil {
		t.Fatal(err)
	}
}

// TestConfigUI drives the interactive cpro config screen (configApp in
// configui.go) through a real pseudoterminal — CONFIG, which now renders
// identically whether opened directly (`cpro config`) or from the root/menu
// picker's own "config →" (see TestNavigationHierarchy's own assertion that
// the item list matches), including its own "Default →" row's one
// behavioral difference from every other entry point into that screen:
// pushed here it's a nested frame (Esc/← pops back to CONFIG) rather than
// the root-frame entry point it is everywhere else (cpro default directly,
// or the picker's "default →").
//
// This test covers menu ordering, up/down navigation, direct boolean
// toggling, the color selector (Esc cancels, Enter saves — reused verbatim
// for accent/bar/warning-color/danger-color), the Theme picker (Esc cancels,
// Enter saves, cursor/saved independence), the Warning/Danger "Threshold /
// Color" sub-menu (both independently editable, Esc pops back one level at a
// time rather than straight to the main menu), persistence of everything
// through to config.json, and that every screen renders recognizable
// content.
//
// Menu order throughout this test: default(0), mask(1), accent(2), bar(3),
// theme(4), warn(5), danger(6) — see configMenuItemsConfig (this test drives
// `cpro config` directly, hasParent false, so Default leads). Ungrouped
// (alongside mask); Security otherwise has no second member to head a
// section for. Auto-trust is not a row on this menu at all; it lives inside
// the SET DEFAULT ACCOUNT screen as "Trust working directory" — see
// TestPermissionsUI.
//
// Every subtest that actually exits the program (as opposed to backing out
// of a nested screen) sends Esc twice: CONFIG is the stack's own root with
// no parent, so — decision 0023, the same convention rootPickerApp's own
// outermost frame uses — the first Esc only arms a double-Esc-to-exit rather
// than quitting immediately.
func TestConfigUI(t *testing.T) {
	bin, s := buildCLI(t)

	// Known starting point: defaults, so initial-value assertions below are
	// meaningful rather than accidental.
	if err := s.update(func(c *config) error { return nil }); err != nil {
		t.Fatal(err)
	}

	drive := func(t *testing.T, steps func(master *os.File, capture func() string)) string {
		t.Helper()
		master, slave := openPTY(t)
		capture := drainPTY(master)
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, "config")
		cmd.Env = append(os.Environ(), "TERM=xterm-256color")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(500 * time.Millisecond)
		steps(master, capture)
		if err := cmd.Wait(); err != nil {
			t.Fatalf("cpro config: %v; output %q", err, stripANSI(capture()))
		}
		return stripANSI(capture())
	}

	send := func(master *os.File, s string) {
		if _, err := master.WriteString(s); err != nil {
			t.Fatal(err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	down := func(master *os.File) { send(master, "\x1b[B") }
	up := func(master *os.File) { send(master, "\x1b[A") }
	left := func(master *os.File) { send(master, "\x1b[D") }
	right := func(master *os.File) { send(master, "\x1b[C") }
	enter := func(master *os.File) { send(master, "\r") }
	esc := func(master *os.File) { send(master, "\x1b") }
	// exit actually leaves the program from the menu root — CONFIG has no
	// parent, so (decision 0023) a single Esc only arms the exit; a second,
	// consecutive one confirms it. Every other, singular esc() call in this
	// test either cancels/pops a nested screen (never arms anything) or is
	// the exit's own first, arming press.
	exit := func(master *os.File) { esc(master); esc(master) }

	t.Run("initial screen shows Default/Mask leading, then the two groups, with current values", func(t *testing.T) {
		out := drive(t, func(master *os.File, capture func() string) {
			exit(master)
		})
		for _, want := range []string{
			"Default", "Mask emails",
			"Appearance", "Accent", "Usage bar", "Theme",
			"Usage thresholds", "Warning", "Danger",
			"Off",        // MaskEmail defaults false
			"80%", "90%", // default thresholds
			"Orange", "Red", // default warning/danger colors
			"Rounded", // default theme
		} {
			if !strings.Contains(out, want) {
				t.Fatalf("CONFIG menu missing %q; got %q", want, out)
			}
		}
		if strings.Index(out, "Default") > strings.Index(out, "Mask emails") ||
			strings.Index(out, "Mask emails") > strings.Index(out, "Appearance") ||
			strings.Index(out, "Appearance") > strings.Index(out, "Usage thresholds") {
			t.Fatalf("sections out of order (want Default, Mask emails, Appearance, Usage thresholds): %q", out)
		}
		// Auto-trust lives inside the SET DEFAULT ACCOUNT screen itself as
		// "Trust working directory" — it's never a CONFIG row of its own, and
		// there's still no "Security" heading (Default/Mask emails are both
		// ungrouped).
		if strings.Contains(out, "Auto-trust") || strings.Contains(out, "Trust working directory") || strings.Contains(out, "Security") {
			t.Fatalf("expected no Auto-trust/Trust/Security row or heading on CONFIG's own menu, got %q", out)
		}
		// A former, now-removed group ("General") must not have come back.
		if strings.Contains(out, "General") {
			t.Fatalf("expected no stray \"General\" group, got %q", out)
		}
	})

	t.Run("up wraps from the first item to the last", func(t *testing.T) {
		// Danger is the last item (default, mask, accent, bar, theme, warn,
		// danger) — a forward row that opens the Threshold/Color sub-menu, so
		// this checks the wrap landed there via its own content rather than a
		// persisted value.
		out := drive(t, func(master *os.File, capture func() string) {
			up(master) // default (first) -> wraps to danger (last)
			enter(master)
			esc(master) // sub-menu -> CONFIG
			exit(master)
		})
		if !strings.Contains(out, "DANGER") {
			t.Fatalf("expected up from the first item to wrap to Danger (the last item) and open its sub-menu, got %q", out)
		}
	})

	t.Run("Esc on the menu exits without changes", func(t *testing.T) {
		before, err := s.read()
		if err != nil {
			t.Fatal(err)
		}
		drive(t, func(master *os.File, capture func() string) {
			down(master)
			exit(master)
		})
		after, err := s.read()
		if err != nil {
			t.Fatal(err)
		}
		if before.AutoTrust != after.AutoTrust || before.MaskEmail != after.MaskEmail {
			t.Fatalf("Esc on the menu should not persist anything: before %+v after %+v", before, after)
		}
	})

	t.Run("Enter toggles a boolean directly, no secondary screen", func(t *testing.T) {
		// Mask emails is a plain boolean toggle, one Down from Default (the
		// first item).
		drive(t, func(master *os.File, capture func() string) {
			down(master)  // default -> mask
			enter(master) // toggle Mask emails on
			exit(master)
		})
		c, err := s.read()
		if err != nil {
			t.Fatal(err)
		}
		if !c.MaskEmail {
			t.Fatalf("expected MaskEmail to be toggled on, got %+v", c)
		}
		drive(t, func(master *os.File, capture func() string) {
			down(master)
			enter(master) // toggle it back off
			exit(master)
		})
		c, err = s.read()
		if err != nil {
			t.Fatal(err)
		}
		if c.MaskEmail {
			t.Fatalf("expected MaskEmail to be toggled back off, got %+v", c)
		}
	})

	t.Run("color selector: navigation, Esc cancels, Enter saves — shared by accent, bar, warning-color, danger-color", func(t *testing.T) {
		if err := s.update(func(c *config) error { c.AccentColor = colorPalette[2].Hex; return nil }); err != nil { // Blue
			t.Fatal(err)
		}
		var accentScreen string
		drive(t, func(master *os.File, capture func() string) {
			down(master) // default -> mask
			down(master) // mask -> accent
			enter(master)
			accentScreen = stripANSI(capture())
			down(master) // move off the first list entry before cancelling
			esc(master)  // cancel: back on the menu
			exit(master) // actually exit
		})
		for _, want := range []string{"ACCENT COLOR", "Blue", "Purple", "Violet", "Cyan", "Green", "Amber", "Orange", "Red", "Rose"} {
			if !strings.Contains(accentScreen, want) {
				t.Fatalf("accent selector missing %q; got %q", want, accentScreen)
			}
		}
		c, err := s.read()
		if err != nil {
			t.Fatal(err)
		}
		if c.AccentColor != colorPalette[2].Hex {
			t.Fatalf("Esc in the color selector must not persist a change, got %+v", c)
		}

		var barScreen string
		drive(t, func(master *os.File, capture func() string) {
			down(master) // default -> mask
			down(master) // mask -> accent
			down(master) // accent -> bar
			enter(master)
			barScreen = stripANSI(capture())
			enter(master) // save the first offered entry
			exit(master)
		})
		if !strings.Contains(barScreen, "USAGE BAR COLOR") {
			t.Fatalf("bar selector missing its title; got %q", barScreen)
		}
		c, err = s.read()
		if err != nil {
			t.Fatal(err)
		}
		if c.BarColor == "" {
			t.Fatalf("Enter in the color selector should have persisted a bar color, got %+v", c)
		}
		found := false
		for _, entry := range colorPalette {
			if entry.Hex == c.BarColor {
				found = true
			}
		}
		if !found {
			t.Fatalf("persisted bar color %q is not in the palette", c.BarColor)
		}
	})

	t.Run("Theme picker: navigation, Esc cancels, Enter saves, applies immediately", func(t *testing.T) {
		if err := s.update(func(c *config) error { c.Theme = ""; return nil }); err != nil { // default: Rounded
			t.Fatal(err)
		}
		var themeScreen string
		drive(t, func(master *os.File, capture func() string) {
			down(master) // default -> mask
			down(master) // mask -> accent
			down(master) // accent -> bar
			down(master) // bar -> theme
			enter(master)
			themeScreen = stripANSI(capture())
			down(master) // move the cursor without saving
			esc(master)  // cancel: back on the menu
			exit(master)
		})
		for _, want := range []string{"Theme", "Minimal", "Rounded", "Heavy", "Double", "┌─", "╭─", "┏━", "╔═"} {
			if !strings.Contains(themeScreen, want) {
				t.Fatalf("theme picker missing %q; got %q", want, themeScreen)
			}
		}
		c, err := s.read()
		if err != nil {
			t.Fatal(err)
		}
		if c.Theme != "" {
			t.Fatalf("Esc in the theme picker must not persist a change, got %+v", c)
		}

		afterSave := drive(t, func(master *os.File, capture func() string) {
			down(master) // default -> mask
			down(master) // mask -> accent
			down(master) // accent -> bar
			down(master) // bar -> theme
			enter(master)
			down(master)  // Rounded -> Heavy
			enter(master) // save Heavy
			exit(master)  // exit from the main menu, now itself rendered in Heavy
		})
		c, err = s.read()
		if err != nil {
			t.Fatal(err)
		}
		if c.Theme != "Heavy" {
			t.Fatalf("Enter in the theme picker should have persisted \"Heavy\", got %+v", c)
		}
		// Applied immediately, in the same run: the main menu's own panel,
		// re-rendered right after saving, must already use Heavy's runes.
		if !strings.Contains(afterSave, "┏━") || !strings.Contains(afterSave, "┗━") {
			t.Fatalf("expected the settings panel itself to switch to Heavy's border immediately after saving, got %q", afterSave)
		}
		// The Theme row's own value marker previews the chosen style's actual
		// top-left corner rune (e.g. "┏ Heavy"), not a generic "●" dot — there's
		// no single color swatch to show for a border style.
		if !strings.Contains(afterSave, "┏ Heavy") {
			t.Fatalf("expected the Theme row to show Heavy's own corner rune as its marker (\"┏ Heavy\"), got %q", afterSave)
		}
		if err := s.update(func(c *config) error { c.Theme = ""; return nil }); err != nil { // restore
			t.Fatal(err)
		}
	})

	t.Run("Warning/Danger threshold-menu: Threshold and Color are independently editable, Esc pops back one level at a time", func(t *testing.T) {
		if err := s.update(func(c *config) error {
			c.BarWarnThreshold, c.BarDangerThreshold = 80, 90
			c.WarningColor, c.DangerColor = "", ""
			return nil
		}); err != nil {
			t.Fatal(err)
		}

		var warnMenuScreen, warnThresholdScreen string
		drive(t, func(master *os.File, capture func() string) {
			for range 5 { // default -> mask -> accent -> bar -> theme -> warn
				down(master)
			}
			enter(master) // opens the Threshold/Color sub-menu, not the stepper directly
			warnMenuScreen = stripANSI(capture())
			enter(master) // Threshold is first — opens the percentage stepper
			warnThresholdScreen = stripANSI(capture())
			right(master) // 80 -> 81, clamped below danger (90)
			esc(master)   // cancel: back to the Threshold/Color sub-menu, not the main menu
			esc(master)   // sub-menu -> main menu
			exit(master)  // main menu -> exit
		})
		if !strings.Contains(warnMenuScreen, "Threshold") || !strings.Contains(warnMenuScreen, "Color") || !strings.Contains(warnMenuScreen, "80%") {
			t.Fatalf("warning sub-menu missing expected content; got %q", warnMenuScreen)
		}
		// Not "Warning threshold" (the screen's own title): it shares a literal
		// "Warning" prefix with the sub-menu screen's own "╭─ Warning" title
		// immediately before it, so bubbletea's diffing renderer may only
		// write the " threshold" suffix via cursor positioning rather than
		// retransmitting the whole string contiguously — confirmed live, this
		// exact assertion flaked on that overlap (see TestRootPicker's own
		// doc comment for the general rule). "Usage percentage" is unique to
		// this screen's own body and shares no prefix with anything before it.
		if !strings.Contains(warnThresholdScreen, "Usage percentage") || !strings.Contains(warnThresholdScreen, "80%") {
			t.Fatalf("warning threshold editor missing expected content; got %q", warnThresholdScreen)
		}
		c, err := s.read()
		if err != nil {
			t.Fatal(err)
		}
		if c.BarWarnThreshold != 80 {
			t.Fatalf("Esc in the threshold editor must not persist a change, got %+v", c)
		}

		var dangerColorScreen string
		drive(t, func(master *os.File, capture func() string) {
			for range 6 { // default -> mask -> accent -> bar -> theme -> warn -> danger
				down(master)
			}
			enter(master) // Threshold/Color sub-menu for danger
			down(master)  // Threshold -> Color
			enter(master) // opens the danger color picker
			dangerColorScreen = stripANSI(capture())
			enter(master) // save the first offered entry
			esc(master)   // back on the sub-menu (returnScreen), not the main menu
			esc(master)   // sub-menu -> main menu
			exit(master)  // main menu -> exit
		})
		// Not "Danger color" (the screen's own title): it shares a literal
		// "Danger" prefix with the sub-menu's own "╭─ Danger" title right
		// before it, risking the same cumulative-capture overlap the
		// threshold editor's own checks above avoid. "Purple" (the palette's
		// own first list entry, unique to this screen) sidesteps it.
		if !strings.Contains(dangerColorScreen, "Purple") || !strings.Contains(dangerColorScreen, "Red") {
			t.Fatalf("danger color picker missing expected content; got %q", dangerColorScreen)
		}
		c, err = s.read()
		if err != nil {
			t.Fatal(err)
		}
		if c.DangerColor == "" {
			t.Fatalf("Enter in the danger color picker should have persisted a color, got %+v", c)
		}
		if c.BarDangerThreshold != 0 && c.BarDangerThreshold != 90 {
			t.Fatalf("saving the danger color must not have touched the danger threshold, got %+v", c)
		}

		var dangerThresholdScreen string
		drive(t, func(master *os.File, capture func() string) {
			for range 6 {
				down(master)
			}
			enter(master) // Threshold/Color sub-menu for danger
			enter(master) // Threshold is first
			dangerThresholdScreen = stripANSI(capture())
			left(master) // 90 -> 89
			enter(master)
			esc(master) // sub-menu
			exit(master)
		})
		// See the warning threshold editor's own check above for why this
		// looks for "Usage percentage", not the "Danger threshold" title.
		if !strings.Contains(dangerThresholdScreen, "Usage percentage") || !strings.Contains(dangerThresholdScreen, "90%") {
			t.Fatalf("danger threshold editor missing expected content; got %q", dangerThresholdScreen)
		}
		c, err = s.read()
		if err != nil {
			t.Fatal(err)
		}
		if c.BarDangerThreshold != 89 {
			t.Fatalf("Enter in the threshold editor should have persisted 89, got %+v", c)
		}

		// Clamp: warning can't be pushed to or past the current danger threshold
		// (85 + 5 unclamped presses would reach 90; the editor must stop at 88).
		if err := s.update(func(c *config) error {
			c.BarWarnThreshold, c.BarDangerThreshold = 85, 89
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		drive(t, func(master *os.File, capture func() string) {
			for range 5 {
				down(master)
			}
			enter(master) // warn sub-menu
			enter(master) // Threshold
			for range 5 {
				right(master) // would overshoot 89 without clamping
			}
			enter(master)
			esc(master) // sub-menu
			exit(master)
		})
		c, err = s.read()
		if err != nil {
			t.Fatal(err)
		}
		if c.BarWarnThreshold != 88 {
			t.Fatalf("warning threshold should have clamped at 88 (one below danger), got %+v", c)
		}
	})

	// CONFIG's "Default →" row (cursor 0) pushes SET DEFAULT ACCOUNT as a
	// nested frame — unlike cpro default directly, or the picker's own
	// "default →" (both root frames) — so a single Esc there pops back to
	// CONFIG rather than exiting, and CONFIG's own menu re-renders with the
	// newly saved mode/account immediately, since it's the exact same
	// configApp/m.c the whole time (no cross-program hop, no separate
	// state). Covers both halves the merged screen now edits — permission
	// mode and the default account — in one pass, replacing what used to be
	// two separate subtests for two separate screens.
	t.Run("Default row is nested inside CONFIG: single Esc pops back, saved mode/account update the row live", func(t *testing.T) {
		if err := s.update(func(c *config) error {
			c.PermissionMode = "ask"
			c.AutoTrust = false
			c.Accounts = map[string]bool{"a@example.com": true, "b@example.com": true}
			c.Default = ""
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		master, slave := openPTY(t)
		capture := drainPTY(master)
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, "config")
		cmd.Env = append(os.Environ(), "TERM=xterm-256color")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(500 * time.Millisecond)
		enter(master) // Default is cursor 0 — opens SET DEFAULT ACCOUNT nested inside CONFIG
		time.Sleep(300 * time.Millisecond)
		nested := stripANSI(capture())
		if !strings.Contains(nested, "SET DEFAULT ACCOUNT") || !strings.Contains(nested, workspaceTrustLabel) ||
			!strings.Contains(nested, "a@example.com") || !strings.Contains(nested, "b@example.com") {
			t.Fatalf("expected SET DEFAULT ACCOUNT to open nested inside CONFIG with both accounts listed, got %q", nested)
		}
		for range 4 { // ask -> edit -> readonly -> live -> yolo
			down(master)
		}
		enter(master) // select YOLO — saves, screen stays open (activateDefaultRow)
		time.Sleep(200 * time.Millisecond)
		down(master)  // yolo -> a@example.com (the first account row)
		enter(master) // save a@example.com as the default — stays open
		time.Sleep(200 * time.Millisecond)
		esc(master) // single Esc: nested frame, pops back to CONFIG (never arms)
		time.Sleep(300 * time.Millisecond)
		if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
			t.Fatalf("a single Esc from SET DEFAULT ACCOUNT nested inside CONFIG should pop back, not exit: %v", err)
		}
		backOnConfig := stripANSI(capture())
		if !strings.Contains(backOnConfig, "CONFIG") {
			t.Fatalf("expected to be back on CONFIG's own menu, got %q", backOnConfig)
		}
		if !strings.Contains(backOnConfig, "YOLO (no prompts)") || !strings.Contains(backOnConfig, "a@example.com") {
			t.Fatalf("expected CONFIG's own Default row to already show the newly saved mode and account, got %q", backOnConfig)
		}
		exit(master) // arm, then confirm — CONFIG itself is the stack's own root
		if err := cmd.Wait(); err != nil {
			t.Fatalf("cpro config: %v; output %q", err, stripANSI(capture()))
		}
		c, err := s.read()
		if err != nil {
			t.Fatal(err)
		}
		if c.PermissionMode != "yolo" {
			t.Fatalf("expected the mode selected inside CONFIG's nested Default screen to persist, got %+v", c)
		}
		if c.Default != "a@example.com" {
			t.Fatalf("expected the account selected inside CONFIG's nested Default screen to persist, got %+v", c)
		}
	})

	t.Run("terminal is restored on exit", func(t *testing.T) {
		master, slave := openPTY(t)
		capture := drainPTY(master)
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, "config")
		cmd.Env = append(os.Environ(), "TERM=xterm-256color")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(500 * time.Millisecond)
		for range 2 { // arm, then confirm — decision 0023, no parent means double-Esc
			if _, err := master.WriteString("\x1b"); err != nil {
				t.Fatal(err)
			}
			time.Sleep(200 * time.Millisecond)
		}
		if err := cmd.Wait(); err != nil {
			t.Fatalf("cpro config: %v; output %q", err, stripANSI(capture()))
		}
		// The alt-screen/raw-mode program must leave the terminal in a normal,
		// still-usable state: a plain command run right after on the same pty
		// should work exactly as if cpro config had never touched it.
		echo := exec.Command("echo", "restored")
		echo.Stdin, echo.Stdout, echo.Stderr = slave, slave, slave
		if err := echo.Run(); err != nil {
			t.Fatalf("terminal not usable after cpro config exited: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
		if !strings.Contains(capture(), "restored") {
			t.Fatalf("expected the terminal to still echo output after cpro config exited, got %q", stripANSI(capture()))
		}
	})
}

// TestConfigAppUsesAltScreen pins the fix for a reported bug: opening a tall
// screen (e.g. SET DEFAULT ACCOUNT) left the previous menu's content on
// screen above the new panel, with "cpro config" appearing to repeat —
// bubbletea's default
// inline renderer moves the cursor with *relative* up-moves assuming the
// previous frame is still fully visible right above it, which breaks once a
// screen taller than the terminal has forced real scrolling (confirmed live
// with tmux capture-pane, which composites what a real terminal ends up
// showing — a naive concatenation of the raw byte stream can't tell an
// absolute-position full redraw, which alt screen mode uses, apart from
// actual leftover content, so that isn't how this is checked). rootui.go's
// root picker hit the identical issue and fixed it the same way. This is a
// fast, direct check of the fix's actual mechanism (every configApp screen's
// View sets tea.View.AltScreen) rather than a pty capture that would just
// re-hit the same measurement problem the bug itself is about.
func TestConfigAppUsesAltScreen(t *testing.T) {
	for _, screen := range []configScreen{screenMenu, screenColor, screenTheme, screenThreshold, screenThresholdMenu, screenDefault} {
		m := &configApp{stack: newNavStack(screen)}
		if v := m.View(); !v.AltScreen {
			t.Fatalf("screen %v: View().AltScreen = false, want true", screen)
		}
	}
}

// TestPermissionModes covers the static catalog itself: all five modes exist
// with the risk colors the Permissions screen's spec calls for (green/yellow/
// orange/red), and effectivePermissionMode/isPermissionMode fall back safely
// for an empty, unset, or hand-edited-to-garbage config.PermissionMode — the
// backward-compatibility and malformed-entry cases requested for this
// feature.
func TestPermissionModes(t *testing.T) {
	wantLabels := []string{"Ask for everything", "Edit without asking", "Read-only", "Live a little", "YOLO (no prompts)"}
	wantColors := map[string]string{
		"ask":      "#34D399", // green
		"edit":     "#FBBF24", // yellow/amber
		"readonly": "#FB923C", // orange
		"live":     "#FB7185", // rose
		"yolo":     "#F87171", // red
	}
	if len(permissionModes) != 5 {
		t.Fatalf("expected exactly 5 permission modes, got %d: %+v", len(permissionModes), permissionModes)
	}
	for i, mode := range permissionModes {
		if mode.Label != wantLabels[i] {
			t.Fatalf("mode %d: label %q, want %q", i, mode.Label, wantLabels[i])
		}
		want, ok := wantColors[mode.Key]
		if !ok {
			t.Fatalf("unexpected mode key %q", mode.Key)
		}
		if mode.Color != want {
			t.Fatalf("mode %q: color %q, want %q", mode.Key, mode.Color, want)
		}
	}

	// Only one mode can be "active" at a time: each key is unique.
	seen := map[string]bool{}
	for _, mode := range permissionModes {
		if seen[mode.Key] {
			t.Fatalf("duplicate permission mode key %q", mode.Key)
		}
		seen[mode.Key] = true
	}

	// Backward compatibility: a config predating this feature has an empty
	// PermissionMode, which must resolve to "ask" (Claude's own real default,
	// no extra flags — see permissionModeArgs below), not an error or a panic.
	if got := effectivePermissionMode(""); got != defaultPermissionMode {
		t.Fatalf(`effectivePermissionMode(""): got %q, want %q`, got, defaultPermissionMode)
	}
	// A malformed/hand-edited config.json ("permissionMode": "yolo-typo") must
	// fall back the same way, not panic or silently pick an arbitrary mode.
	if got := effectivePermissionMode("yolo-typo"); got != defaultPermissionMode {
		t.Fatalf(`effectivePermissionMode("yolo-typo"): got %q, want %q`, got, defaultPermissionMode)
	}
	if isPermissionMode("yolo-typo") {
		t.Fatal(`isPermissionMode("yolo-typo") should be false`)
	}
	if !isPermissionMode("yolo") {
		t.Fatal(`isPermissionMode("yolo") should be true`)
	}
	// permissionModeByKey must never panic on garbage input either.
	if got := permissionModeByKey("yolo-typo").Key; got != defaultPermissionMode {
		t.Fatalf("permissionModeByKey(garbage).Key: got %q, want %q", got, defaultPermissionMode)
	}
}

// TestPermissionModeArgs covers the mode -> Claude argv mapping: "ask" adds
// nothing (Claude's own default), "readonly" reuses exactly the flags
// ui.go's existing runModeArgs already ships and tests for cpro run's
// classic Plan/Danger picker (--permission-mode plan), "edit" is
// --permission-mode acceptEdits, "live" is a bare --dangerously-skip-permissions,
// and "yolo" is --permission-mode bypassPermissions (decision 0038,
// superseding the earlier --dangerously-skip-permissions --add-dir / pair —
// see yoloArgs' own doc comment, permissions.go, for why).
func TestPermissionModeArgs(t *testing.T) {
	for _, tc := range []struct {
		mode string
		want []string
	}{
		{"ask", nil},
		{"edit", []string{"--permission-mode", "acceptEdits"}},
		{"readonly", []string{"--permission-mode", "plan"}},
		{"live", []string{"--dangerously-skip-permissions"}},
		{"yolo", []string{"--permission-mode", "bypassPermissions"}},
		{"", nil},        // unset -> same as "ask"
		{"garbage", nil}, // malformed -> same as "ask"
	} {
		if got := permissionModeArgs(tc.mode); !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("permissionModeArgs(%q): got %v, want %v", tc.mode, got, tc.want)
		}
	}
	// "Live a little" and YOLO are deliberately distinct modes with distinct
	// argv (decision 0038 re-established this after --add-dir's removal made
	// it a live concern again: yoloEffective, claude.go, and
	// permissionModeForArgs, ui.go, both depend on every mode's Args staying
	// unique) — "live" skips normal prompts but keeps the outside-working-
	// directory read block; only "yolo" opens that up too.
	if got := permissionModeArgs("live"); len(got) != 1 || got[0] != "--dangerously-skip-permissions" {
		t.Fatalf(`expected "live" to be exactly [--dangerously-skip-permissions], got %v`, got)
	}
	// decision 0026 (superseded by 0038, same underlying finding): an earlier
	// --settings JSON override here (disabling
	// permissions.blockReadsOutsideWorkingDirectories) was proven, against a
	// real invocation, to be silently defeated whenever an account's own
	// persisted user settings.json already carried that key as true —
	// exactly the restriction reported live as still prompting even with
	// --dangerously-skip-permissions alone. Reaching the account's own
	// settings.json directly (ensureBlockReadsOutsideWorkingDirectories/
	// ensureYOLOSettings, claude.go) is what actually does that work now;
	// this argv's own job is just requesting bypassPermissions mode itself.
	args := permissionModeArgs("yolo")
	if len(args) != 2 || args[0] != "--permission-mode" || args[1] != "bypassPermissions" {
		t.Fatalf("expected yolo's argv to be [--permission-mode bypassPermissions], got %v", args)
	}
}

// TestPermissionArgsMatchesRunInvocation is the "description matches actual
// cpro run arguments" requirement, checked directly rather than by
// inspection: permissionModeArgs (what applyPermissionDefaults itself uses
// as each mode's baseline argv — decision 0020 removed blocked commands, so
// this is the only ingredient besides the mode itself) and
// applyPermissionDefaults (what claude.go's store.run actually calls, right
// before every real invocation) must produce identical argv for the same
// config, in the common case of nothing already forwarded.
func TestPermissionArgsMatchesRunInvocation(t *testing.T) {
	for _, c := range []config{
		{},
		{PermissionMode: "edit"},
		{PermissionMode: "readonly"},
		{PermissionMode: "live"},
		{PermissionMode: "yolo"},
	} {
		preview := permissionModeArgs(c.PermissionMode)
		actual := applyPermissionDefaults(c, "a@example.com", nil)
		if !reflect.DeepEqual(preview, actual) {
			t.Fatalf("preview/actual mismatch for %+v: preview=%v actual=%v", c, preview, actual)
		}
	}
}

// TestPermissionsPreviewTracksCursor covers the SET DEFAULT ACCOUNT screen's live
// mode description (decision 0038, replacing the earlier "Command preview"
// argv block) independently of a pty: bubbletea's diffing renderer can only
// rewrite the handful of cells that actually changed on a same-screen cursor
// move, which is exactly the class of change TestPermissionsUI's own doc
// comment says is unsafe to verify by re-capturing and comparing text — so,
// like TestRootPickerExitArmed one level up, this drives configApp's
// update/view methods directly with no pty at all. It covers: the
// description updates to the highlighted candidate mode immediately on
// cursor movement, with no save (m.c.PermissionMode, activatePermissionRow's
// own target, stays untouched until Enter); Esc/← leaves the previously
// saved mode exactly as it was; a blocked-command toggle updates the
// description immediately too; and the description is read straight off
// permissionMode.Description, the same struct permissionModeArgs itself
// reads Args from — so a not-yet-saved description and the eventual real
// invocation can never diverge on which mode they're describing.
func TestPermissionsPreviewTracksCursor(t *testing.T) {
	newModel := func(t *testing.T, mode string) *configApp {
		t.Helper()
		m := &configApp{s: &store{dir: t.TempDir()}, c: config{PermissionMode: mode}, color: false}
		m.defCursor = 1 + permissionModeIndex(effectivePermissionMode(mode))
		m.defPreview = effectivePermissionMode(mode)
		return m
	}
	downKey := tea.KeyPressMsg{Code: tea.KeyDown}
	escKey := tea.KeyPressMsg{Code: tea.KeyEsc}
	enterKey := tea.KeyPressMsg{Code: tea.KeyEnter}

	viewHasPreviewFor := func(t *testing.T, m *configApp, mode string) {
		t.Helper()
		view := m.viewDefault()
		description := permissionModeByKey(mode).Description
		if !strings.Contains(view, description) {
			t.Fatalf("expected description %q (mode %q) in the rendered screen, got %q", description, mode, view)
		}
	}

	t.Run("cursor movement previews the candidate immediately, without saving", func(t *testing.T) {
		m := newModel(t, "ask")
		for _, want := range []string{"edit", "readonly", "live", "yolo"} {
			m.updateDefault(downKey)
			if m.defPreview != want {
				t.Fatalf("permPreview = %q, want %q", m.defPreview, want)
			}
			if m.c.PermissionMode != "ask" {
				t.Fatalf("expected the saved mode to stay %q until Enter, got %q", "ask", m.c.PermissionMode)
			}
			viewHasPreviewFor(t, m, want)
		}
	})

	t.Run("Esc/back leaves the previously saved mode untouched", func(t *testing.T) {
		m := newModel(t, "ask")
		m.updateDefault(downKey) // preview -> "edit", not saved
		m.updateDefault(downKey) // preview -> "readonly", not saved
		m.updateDefault(escKey)
		if m.c.PermissionMode != "ask" {
			t.Fatalf("expected saved mode to remain %q after Esc, got %q", "ask", m.c.PermissionMode)
		}
	})

	t.Run("Enter saves exactly the highlighted candidate, not the previously saved one", func(t *testing.T) {
		m := newModel(t, "ask")
		m.updateDefault(downKey) // -> edit
		m.updateDefault(downKey) // -> readonly
		m.updateDefault(downKey) // -> live
		m.updateDefault(enterKey)
		if m.c.PermissionMode != "live" {
			t.Fatalf("expected Enter to save the highlighted mode %q, got %q", "live", m.c.PermissionMode)
		}
	})

	t.Run("moving onto the Trust working directory row keeps previewing the last-highlighted mode", func(t *testing.T) {
		m := newModel(t, "ask")
		m.updateDefault(downKey) // -> edit (a mode row)
		m.updateDefault(downKey) // -> readonly
		m.updateDefault(downKey) // -> live
		m.updateDefault(downKey) // -> yolo
		m.updateDefault(downKey) // -> wraps past yolo (the last mode) to Trust working directory (row 0)
		if m.defPreview != "yolo" {
			t.Fatalf("expected the preview to keep showing the last-highlighted mode %q, got %q", "yolo", m.defPreview)
		}
		viewHasPreviewFor(t, m, "yolo")
	})

	t.Run("toggling Trust working directory does not change the preview", func(t *testing.T) {
		m := newModel(t, "yolo") // cursor starts on yolo's own row (the last mode)
		before := m.viewDefault()
		m.updateDefault(downKey) // yolo (the last mode) -> wraps to Trust working directory (row 0)
		m.activateDefaultRow()   // toggles it on
		if m.err != nil {
			t.Fatal(m.err)
		}
		after := m.viewDefault()
		// The description sits on the panel's own last "│ "-prefixed content
		// line, right before renderPanel's closing border rune — extracting
		// that line, rather than searching for a now-removed "Command
		// preview" heading, is what makes this assertion robust to decision
		// 0038's own rendering change.
		lastContentLine := func(s string) string {
			t.Helper()
			lines := strings.Split(s, "\n")
			for i := len(lines) - 1; i >= 0; i-- {
				if strings.HasPrefix(stripANSI(lines[i]), currentTheme.Rail()) {
					return lines[i]
				}
			}
			t.Fatalf("expected a panel content line, got %q", s)
			return ""
		}
		if lastContentLine(before) != lastContentLine(after) {
			t.Fatalf("expected AutoTrust to have no effect on the mode description: before %q after %q", lastContentLine(before), lastContentLine(after))
		}
	})
}

// accountRowLine returns the one Permissions-screen row mentioning email, so an
// assertion can talk about that account's own row instead of the whole panel.
func accountRowLine(view, email string) string {
	for _, line := range strings.Split(view, "\n") {
		if strings.Contains(line, email) {
			return line
		}
	}
	return ""
}

// TestDefaultRowClearsAccountOverride covers the fix for a live-reported bug:
// picking a mode (or a new default account) on SET DEFAULT ACCOUNT silently
// did nothing for an account already carrying a per-account
// PermissionModeByAccount override (settable via the scriptable `cpro config
// permission-mode MODE --account EMAIL`, or a leftover from the old
// per-account editor this screen's own "Accounts" section used to have) —
// permissionModeForAccount always resolves that override ahead of the global
// default for every real run, and this screen has no row left to show or
// clear one from. activateDefaultRow now clears config.Default's own
// override whenever a mode or account row is picked, so the screen's own
// promise ("this sets what cpro run falls back to") actually holds for
// whichever account that is.
func TestDefaultRowClearsAccountOverride(t *testing.T) {
	newModel := func(t *testing.T, c config) *configApp {
		t.Helper()
		s := &store{dir: t.TempDir()}
		if err := s.update(func(cur *config) error { *cur = c; return nil }); err != nil {
			t.Fatal(err)
		}
		m := &configApp{s: s, c: c, color: false}
		m.defCursor = 1 + permissionModeIndex(effectivePermissionMode(c.PermissionMode))
		m.defPreview = effectivePermissionMode(c.PermissionMode)
		return m
	}
	upKey := tea.KeyPressMsg{Code: tea.KeyUp}
	enterKey := tea.KeyPressMsg{Code: tea.KeyEnter}

	t.Run("selecting a mode clears the current default account's own override", func(t *testing.T) {
		m := newModel(t, config{
			Version:                 1,
			Default:                 "a@example.com",
			Accounts:                map[string]bool{"a@example.com": true, "b@example.com": true},
			PermissionMode:          "yolo",
			PermissionModeByAccount: map[string]string{"a@example.com": "live", "b@example.com": "readonly"},
		})
		for range 4 { // yolo -> live -> readonly -> edit -> ask
			m.updateDefault(upKey)
		}
		m.updateDefault(enterKey)
		if m.err != nil {
			t.Fatal(m.err)
		}
		if m.c.PermissionMode != "ask" {
			t.Fatalf("expected the global mode to save as %q, got %+v", "ask", m.c)
		}
		if _, ok := m.c.PermissionModeByAccount["a@example.com"]; ok {
			t.Fatalf("expected the default account's own override cleared, got %+v", m.c.PermissionModeByAccount)
		}
		if m.c.PermissionModeByAccount["b@example.com"] != "readonly" {
			t.Fatalf("expected an unrelated account's override to survive untouched, got %+v", m.c.PermissionModeByAccount)
		}
		c, err := m.s.read()
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := c.PermissionModeByAccount["a@example.com"]; ok {
			t.Fatalf("expected the clear to persist to disk, got %+v", c.PermissionModeByAccount)
		}
	})

	t.Run("picking a new default account clears that account's own override", func(t *testing.T) {
		m := newModel(t, config{
			Version:                 1,
			Default:                 "a@example.com",
			Accounts:                map[string]bool{"a@example.com": true, "b@example.com": true},
			PermissionMode:          "ask",
			PermissionModeByAccount: map[string]string{"b@example.com": "yolo"},
		})
		// Row layout: trust(0), 5 modes(1..5), then accounts sorted — a(6), b(7).
		m.defCursor = 7
		m.updateDefault(enterKey)
		if m.err != nil {
			t.Fatal(m.err)
		}
		if m.c.Default != "b@example.com" {
			t.Fatalf("expected b@example.com to become the default, got %+v", m.c)
		}
		if _, ok := m.c.PermissionModeByAccount["b@example.com"]; ok {
			t.Fatalf("expected the new default account's own override cleared, got %+v", m.c.PermissionModeByAccount)
		}
	})

	t.Run("no default account yet, or no override: a no-op, never an error", func(t *testing.T) {
		m := newModel(t, config{Version: 1, Accounts: map[string]bool{}, PermissionMode: "ask"})
		m.defCursor = 2 // trust(0), ask(1), edit(2)
		m.updateDefault(enterKey)
		if m.err != nil {
			t.Fatal(m.err)
		}
		if m.c.PermissionMode != "edit" {
			t.Fatalf("expected the mode to still save with no default account set, got %+v", m.c)
		}
	})
}

// TestApplyPermissionDefaults covers applyPermissionDefaults' precedence
// rule: an explicit permission-mode flag already in forwarded (a literal CLI
// argument, or the interactive run flow's own RUN MODE step, rootui.go,
// which appends its own flags before this is ever called) wins over the
// persisted config default.
func TestApplyPermissionDefaults(t *testing.T) {
	c := config{PermissionMode: "yolo"}

	// No existing args: the config-level mode applies.
	got := applyPermissionDefaults(c, "a@example.com", nil)
	want := []string{"--permission-mode", "bypassPermissions"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("no existing args: got %v, want %v", got, want)
	}

	// An explicit --permission-mode already forwarded (e.g. the interactive
	// run flow's own Read-only choice) suppresses the config-level mode.
	got = applyPermissionDefaults(c, "a@example.com", []string{"--permission-mode", "plan"})
	want = []string{"--permission-mode", "plan"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("existing --permission-mode: got %v, want %v", got, want)
	}

	// An explicit --dangerously-skip-permissions already forwarded also
	// suppresses the config-level mode.
	got = applyPermissionDefaults(c, "a@example.com", []string{"--dangerously-skip-permissions"})
	want = []string{"--dangerously-skip-permissions"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("existing --dangerously-skip-permissions: got %v, want %v", got, want)
	}

	// Backward compatibility: a config predating this feature (zero value) and
	// nothing forwarded must not add anything at all — cpro run's existing,
	// tested behavior for every config from before this feature.
	got = applyPermissionDefaults(config{}, "a@example.com", []string{"-p", "hello"})
	want = []string{"-p", "hello"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("zero-value config must not change forwarded args: got %v, want %v", got, want)
	}
}

// TestPermissionModeForAccount covers decision 0050's resolution order: an
// account's own override wins when it is stored and still a real mode; an
// absent or malformed override falls back to the global config.PermissionMode,
// and from there to defaultPermissionMode — so a hand-edited config.json can
// never make a run resolve to a mode that does not exist.
func TestPermissionModeForAccount(t *testing.T) {
	c := config{
		PermissionMode:          "live",
		PermissionModeByAccount: map[string]string{"yolo@example.com": "yolo", "broken@example.com": "yolo-typo"},
	}
	cases := []struct{ account, want string }{
		{"yolo@example.com", "yolo"},   // its own override
		{"broken@example.com", "live"}, // malformed override -> global
		{"other@example.com", "live"},  // no entry at all -> global
	}
	for _, tc := range cases {
		if got := permissionModeForAccount(c, tc.account); got != tc.want {
			t.Fatalf("permissionModeForAccount(%q): got %q, want %q", tc.account, got, tc.want)
		}
	}
	// Neither a global default nor an override: Claude's own real default.
	if got := permissionModeForAccount(config{}, "nobody@example.com"); got != defaultPermissionMode {
		t.Fatalf("empty config: got %q, want %q", got, defaultPermissionMode)
	}
	// The override must reach the actual argv, and only for that account.
	if got, want := applyPermissionDefaults(c, "yolo@example.com", nil), []string{"--permission-mode", "bypassPermissions"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("override argv: got %v, want %v", got, want)
	}
	if got, want := applyPermissionDefaults(c, "other@example.com", nil), []string{"--dangerously-skip-permissions"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("global-fallback argv (live): got %v, want %v", got, want)
	}
}

// TestSaveAccountPermissionMode covers decision 0050's persistence: setting,
// replacing and clearing one account's override without touching another's,
// plus the two guards every saveXxx here applies (unknown mode, unregistered
// account) and the global setter staying independent of the per-account map.
func TestSaveAccountPermissionMode(t *testing.T) {
	dir := t.TempDir()
	old := `{"version":1,"default":"a@example.com","accounts":{"a@example.com":true,"b@example.com":true}}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(old), 0600); err != nil {
		t.Fatal(err)
	}
	s := &store{dir: dir}

	if err := saveAccountPermissionMode(s, "a@example.com", "yolo"); err != nil {
		t.Fatalf("setting an override: %v", err)
	}
	c, err := s.read()
	if err != nil {
		t.Fatal(err)
	}
	if c.PermissionModeByAccount["a@example.com"] != "yolo" {
		t.Fatalf("override not persisted: %+v", c.PermissionModeByAccount)
	}
	if _, ok := c.PermissionModeByAccount["b@example.com"]; ok {
		t.Fatalf("setting one account's override must not touch another: %+v", c.PermissionModeByAccount)
	}

	if err := saveAccountPermissionMode(s, "a@example.com", "readonly"); err != nil {
		t.Fatalf("replacing an override: %v", err)
	}
	if c, _ := s.read(); c.PermissionModeByAccount["a@example.com"] != "readonly" {
		t.Fatalf("override not replaced: %+v", c.PermissionModeByAccount)
	}

	if err := saveAccountPermissionMode(s, "a@example.com", ""); err != nil {
		t.Fatalf("clearing an override: %v", err)
	}
	if c, _ := s.read(); len(c.PermissionModeByAccount) != 0 {
		t.Fatalf("override not cleared: %+v", c.PermissionModeByAccount)
	}

	if err := saveAccountPermissionMode(s, "a@example.com", "yolo-typo"); err == nil {
		t.Fatal("expected an unknown mode to be rejected")
	}
	if err := saveAccountPermissionMode(s, "nobody@example.com", "yolo"); err == nil {
		t.Fatal("expected an unregistered account to be rejected")
	}

	if err := savePermissionMode(s, "ask"); err != nil {
		t.Fatalf("the global setter must keep working: %v", err)
	}
	if c, _ := s.read(); c.PermissionMode != "ask" {
		t.Fatalf("global mode not saved: %q", c.PermissionMode)
	}
}

// TestPermissionModeDescriptions covers permissionMode.Description
// (decision 0038, permissions.go), the replacement for the removed
// formatCommandPreview: every mode in the fixed permissionModes table must
// carry a non-empty, single-line description — a blank one would render an
// empty gap on the Permissions screen, and a multi-line one would break its
// one-line-per-mode layout there and in the RUN MODE frame's own rendering
// (rootui.go, which reuses this same table).
func TestPermissionModeDescriptions(t *testing.T) {
	for _, mode := range permissionModes {
		if mode.Description == "" {
			t.Fatalf("mode %q has no Description", mode.Key)
		}
		if strings.Contains(mode.Description, "\n") {
			t.Fatalf("mode %q Description must be a single line, got %q", mode.Key, mode.Description)
		}
	}
	// permissionModeByKey resolves an empty/malformed key through
	// effectivePermissionMode first (decision 0038 leans on this so
	// viewPermissions never renders an empty description for an
	// unset/garbage m.permPreview) — confirm it too.
	if got, want := permissionModeByKey("").Description, permissionModeByKey("ask").Description; got != want {
		t.Fatalf("permissionModeByKey(\"\").Description: got %q, want %q (ask's own)", got, want)
	}
}

// TestConfigBackwardCompatibility covers an existing config.json written
// before this feature existed (no "permissionMode"/"blockedCommands" keys at
// all) still loading cleanly, with both fields defaulting exactly as if they
// had never been part of the schema.
func TestConfigBackwardCompatibility(t *testing.T) {
	dir := t.TempDir()
	old := `{"version":1,"default":"a@example.com","accounts":{"a@example.com":true},"autoTrust":true}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(old), 0600); err != nil {
		t.Fatal(err)
	}
	s := &store{dir: dir}
	c, err := s.read()
	if err != nil {
		t.Fatalf("reading a pre-feature config.json: %v", err)
	}
	if c.PermissionMode != "" {
		t.Fatalf("expected an empty PermissionMode from a config predating this feature, got %q", c.PermissionMode)
	}
	if len(c.BlockedCommands) != 0 {
		t.Fatalf("expected no BlockedCommands from a config predating this feature, got %+v", c.BlockedCommands)
	}
	if got := effectivePermissionMode(c.PermissionMode); got != defaultPermissionMode {
		t.Fatalf("effective mode for a pre-feature config: got %q, want %q", got, defaultPermissionMode)
	}
	if got := applyPermissionDefaults(c, "a@example.com", []string{"-p", "hi"}); !reflect.DeepEqual(got, []string{"-p", "hi"}) {
		t.Fatalf("a pre-feature config must not change what cpro run forwards: got %v", got)
	}
}

// TestLegacyBlockedCommandsIgnored covers decision 0020's backward-compat
// requirement: a config.json written before "Block always" was removed (with
// real "blockedCommands" entries already in it) must still load without
// error, round-tripping the field unread — never rendered, never applied to
// a real cpro run invocation.
func TestLegacyBlockedCommandsIgnored(t *testing.T) {
	dir := t.TempDir()
	legacy := `{"version":1,"default":"a@example.com","accounts":{"a@example.com":true},"permissionMode":"yolo","blockedCommands":[{"command":"rm -rf","enabled":true}]}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}
	s := &store{dir: dir}
	c, err := s.read()
	if err != nil {
		t.Fatalf("a config.json with legacy blockedCommands must still load: %v", err)
	}
	if len(c.BlockedCommands) != 1 || c.BlockedCommands[0].Command != "rm -rf" {
		t.Fatalf("expected the legacy field to still round-trip (unread, unrendered), got %+v", c.BlockedCommands)
	}
	// Never applied: cpro run's actual argv depends only on the permission
	// mode, nothing from the legacy field ever reaches --disallowedTools.
	got := applyPermissionDefaults(c, "a@example.com", nil)
	want := []string{"--permission-mode", "bypassPermissions"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("legacy blockedCommands must not affect cpro run's argv: got %v, want %v", got, want)
	}
	for _, a := range got {
		if strings.Contains(a, "disallowedTools") || strings.Contains(a, "rm -rf") {
			t.Fatalf("legacy blockedCommands leaked into cpro run's argv: %v", got)
		}
	}
}

// TestPermissionsUI drives the SET DEFAULT ACCOUNT screen through a real
// pseudoterminal, launched directly via cpro default (its own top-level
// entry point, no longer reachable by navigating into cpro config first —
// see TestConfigUI for CONFIG's own coverage and TestNavigationHierarchy for
// the picker's "default" round trip) — so every subtest here starts already
// on the screen and needs only a single Esc to exit (no parent). Where TestConfigUI's own
// navigation-value checks are safe
// re-capturing text mid-session (a Select screen transition is a large-enough
// diff that bubbletea repaints in full — confirmed by that suite's own,
// already-passing color/threshold assertions), a same-screen change here
// (selecting a different mode, toggling one blocked command) is exactly the
// small, same-screen diff TestRootPicker's own doc comment warns is unsafe to
// re-capture-and-compare: bubbletea may only touch the handful of cells that
// actually changed. So every test below either seeds config.json before
// launching a fresh process (checking that state's one full initial render),
// or performs the change then verifies it landed via s.read() — persisted
// state, not re-captured text — never both a live toggle and a text
// assertion of its effect in the same run.
func TestPermissionsUI(t *testing.T) {
	bin, s := buildCLI(t)

	drive := func(t *testing.T, steps func(master *os.File, capture func() string)) string {
		t.Helper()
		master, slave := openPTY(t)
		capture := drainPTY(master)
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, "default")
		cmd.Env = append(os.Environ(), "TERM=xterm-256color")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(500 * time.Millisecond)
		steps(master, capture)
		if err := cmd.Wait(); err != nil {
			t.Fatalf("cpro default: %v; output %q", err, stripANSI(capture()))
		}
		return capture() // raw, not stripped: color assertions need the escapes
	}

	send := func(master *os.File, str string) {
		if _, err := master.WriteString(str); err != nil {
			t.Fatal(err)
		}
		time.Sleep(150 * time.Millisecond)
	}
	down := func(master *os.File) { send(master, "\x1b[B") }
	enter := func(master *os.File) { send(master, "\r") }
	// esc exits the whole program in every subtest below — there's no nested
	// screen inside SET DEFAULT ACCOUNT to merely pop out of — so it's two
	// presses, not one: a direct `cpro default` invocation is the stack's
	// own root with no parent, which arms a double-Esc-to-exit rather than
	// quitting on the first press.
	esc := func(master *os.File) { send(master, "\x1b"); send(master, "\x1b") }

	t.Run("renders all five permission modes with the correct risk colors", func(t *testing.T) {
		for _, mode := range permissionModes {
			mode := mode
			t.Run(mode.Key, func(t *testing.T) {
				if err := s.update(func(c *config) error { c.PermissionMode = mode.Key; return nil }); err != nil {
					t.Fatal(err)
				}
				out := drive(t, func(master *os.File, capture func() string) {
					esc(master) // cpro default opens the screen directly, no parent — arm+confirm exits
				})
				plain := stripANSI(out)
				for _, m := range permissionModes {
					if !strings.Contains(plain, m.Label) {
						t.Fatalf("expected all five mode labels visible regardless of which is active; missing %q: %q", m.Label, plain)
					}
				}
				// Every mode's own risk color is always visible — a "traffic
				// light" legend — not just the currently active one's.
				for _, m := range permissionModes {
					if !strings.Contains(out, ansiTrueColor(t, m.Color)) {
						t.Fatalf("expected mode %q's risk color (%s) always visible, not just when active (current: %q), got %q", m.Key, m.Color, mode.Key, out)
					}
				}
			})
		}
	})

	t.Run("selecting a permission mode persists it", func(t *testing.T) {
		if err := s.update(func(c *config) error { c.PermissionMode = "ask"; return nil }); err != nil {
			t.Fatal(err)
		}
		drive(t, func(master *os.File, capture func() string) {
			down(master) // ask -> edit
			enter(master)
			esc(master)
		})
		c, err := s.read()
		if err != nil {
			t.Fatal(err)
		}
		if c.PermissionMode != "edit" {
			t.Fatalf("expected the selected mode to persist as %q, got %+v", "edit", c)
		}
	})

	// The next three subtests cover the Workspace "Trust working directory"
	// row: it renders with its own "▣"/"▢" glyph (distinct from the "●"/"○"
	// every mode/account row uses), toggles in place, persists through the
	// existing AutoTrust config field (no rename, no migration), and never
	// leaks into the mode description below it (it isn't a claude argv flag).
	t.Run("Trust working directory row renders with its current state", func(t *testing.T) {
		if err := s.update(func(c *config) error { c.AutoTrust = true; return nil }); err != nil {
			t.Fatal(err)
		}
		out := stripANSI(drive(t, func(master *os.File, capture func() string) {
			esc(master)
		}))
		if !strings.Contains(out, "Trust working directory") {
			t.Fatalf("expected the \"Trust working directory\" row, got %q", out)
		}
		// The trust row is its own single line — extracting it narrows the
		// window to just that row, so the check below can't accidentally
		// match a "●"/"○" on a mode or account row instead.
		var trustLine string
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(line, "Trust working directory") {
				trustLine = line
				break
			}
		}
		if !strings.Contains(trustLine, "▣") {
			t.Fatalf("expected the enabled trust row to carry a filled ▣, got %q", trustLine)
		}
	})

	t.Run("toggling Trust working directory persists through the existing AutoTrust field", func(t *testing.T) {
		if err := s.update(func(c *config) error {
			c.PermissionMode = "ask" // known starting cursor row (1) — cpro default opens directly on it
			c.AutoTrust = false
			c.Accounts = map[string]bool{}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		drive(t, func(master *os.File, capture func() string) {
			for range 5 { // cursor starts on "ask" (row 1) -> past the remaining 4 modes -> wraps to Trust working directory (row 0)
				down(master)
			}
			enter(master)
			esc(master)
		})
		c, err := s.read()
		if err != nil {
			t.Fatal(err)
		}
		if !c.AutoTrust {
			t.Fatalf("expected AutoTrust to be toggled on, got %+v", c)
		}
		if c.PermissionMode != "ask" {
			t.Fatalf("expected toggling trust to leave PermissionMode untouched, got %+v", c)
		}
		// Round-trip through the raw file too — no config migration: the key
		// is still literally "autoTrust", the field this screen has always
		// used, just surfaced somewhere else in the UI.
		raw, err := os.ReadFile(filepath.Join(s.dir, "config.json"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), `"autoTrust": true`) {
			t.Fatalf(`expected config.json to still use the "autoTrust" key, got %s`, raw)
		}
	})

	t.Run("toggling Trust working directory does not change the mode description", func(t *testing.T) {
		if err := s.update(func(c *config) error {
			c.PermissionMode = "yolo"
			c.AutoTrust = false
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		before := stripANSI(drive(t, func(master *os.File, capture func() string) {
			esc(master)
		}))
		if err := s.update(func(c *config) error { c.AutoTrust = true; return nil }); err != nil {
			t.Fatal(err)
		}
		after := stripANSI(drive(t, func(master *os.File, capture func() string) {
			esc(master)
		}))
		// The description sits on the panel's own last "│ "-prefixed content
		// line — the same extraction TestPermissionsPreviewTracksCursor's own
		// identically-named subtest uses, here proven end to end through a
		// real subprocess rather than a direct configApp call.
		lastContentLine := func(s string) string {
			t.Helper()
			lines := strings.Split(s, "\n")
			for i := len(lines) - 1; i >= 0; i-- {
				if strings.HasPrefix(lines[i], currentTheme.Rail()) {
					return lines[i]
				}
			}
			t.Fatalf("expected a panel content line, got %q", s)
			return ""
		}
		if lastContentLine(before) != lastContentLine(after) {
			t.Fatalf("expected AutoTrust to have no effect on the mode description: before %q after %q", lastContentLine(before), lastContentLine(after))
		}
	})

	t.Run("the rendered description matches permissionMode.Description for the current mode", func(t *testing.T) {
		for _, tc := range []struct{ name, mode string }{
			{"ask", "ask"},
			{"readonly", "readonly"},
			{"yolo", "yolo"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				if err := s.update(func(c *config) error {
					c.PermissionMode = tc.mode
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				out := stripANSI(drive(t, func(master *os.File, capture func() string) {
					esc(master)
				}))
				description := permissionModeByKey(tc.mode).Description
				if !strings.Contains(out, description) {
					t.Fatalf("expected description %q (from permissionModeByKey, the same struct cpro run's own argv reads Args from) in the rendered screen, got %q", description, out)
				}
			})
		}
	})

	t.Run("NO_COLOR: state stays legible without color", func(t *testing.T) {
		if err := s.update(func(c *config) error {
			c.PermissionMode = "yolo"
			c.AutoTrust = true
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		master, slave := openPTY(t)
		capture := drainPTY(master)
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, "default")
		cmd.Env = append(os.Environ(), "TERM=xterm-256color", "NO_COLOR=1")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(500 * time.Millisecond)
		esc(master) // arm + confirm
		if err := cmd.Wait(); err != nil {
			t.Fatalf("cpro default: %v; output %q", err, stripANSI(capture()))
		}
		out := capture()
		if strings.Contains(out, "38;2;") {
			t.Fatalf("NO_COLOR must disable truecolor styling entirely, got %q", stripANSI(out))
		}
		plain := stripANSI(out)
		if !strings.Contains(plain, "YOLO (no prompts)") || !strings.Contains(plain, "●") {
			t.Fatalf("expected the active mode's name and filled marker still visible without color, got %q", plain)
		}
		if !strings.Contains(plain, "Trust working directory") || !strings.Contains(plain, "▣") {
			t.Fatalf("expected the enabled trust row still visible without color, got %q", plain)
		}
	})
}

// TestPermissionsRunIntegration is the strongest form of the "preview matches
// actual cpro run arguments" requirement: not just permissionModeArgs and
// applyPermissionDefaults agreeing in isolation (TestPermissionArgsMatchesRunInvocation),
// but a real "cpro run" invocation, through the full command tree and
// store.run (claude.go), actually reaching the fake claude process
// (TestClaudeProcess) with the configured permission mode applied — end to
// end, "config -> permission mode -> command builder -> cpro run" with no
// step mocked out. Also covers decision 0020's backward-compat requirement
// directly at this end-to-end level: a legacy BlockedCommands value already
// sitting in config.json must never reach the real claude invocation.
func TestPermissionsRunIntegration(t *testing.T) {
	bin, s := buildCLI(t)

	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	var unlocked int32
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlocked))); errno != 0 {
		t.Fatal(errno)
	}
	var number uint32
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), syscall.TIOCGPTN, uintptr(unsafe.Pointer(&number))); errno != 0 {
		t.Fatal(errno)
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", number), os.O_RDWR|syscall.O_NOCTTY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer slave.Close()
	loginCmd := exec.Command(bin, "login", "a@example.com")
	loginCmd.Stdin = slave
	if out, err := loginCmd.CombinedOutput(); err != nil {
		t.Fatalf("login: %s %v", out, err)
	}

	if err := s.update(func(c *config) error {
		c.PermissionMode = "yolo"
		// A legacy BlockedCommands value, as if this config.json predated
		// decision 0020 — must round-trip harmlessly and never reach claude.
		c.BlockedCommands = []blockedCommand{{Command: "rm -rf", Enabled: true}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// No forwarded arguments and no attached tty: run goes straight to
	// store.run without any interactive picker in the way.
	out, err := exec.Command(bin, "run", "--account", "a@example.com").Output()
	if err != nil {
		t.Fatalf("cpro run: %s %v", out, err)
	}
	var forwarded struct {
		Args      []string
		Directory string
	}
	if err := json.Unmarshal(out, &forwarded); err != nil {
		t.Fatalf("parsing the fake claude's recorded args: %s: %v", out, err)
	}

	wantArgs := []string{"--permission-mode", "bypassPermissions"}
	if !reflect.DeepEqual(forwarded.Args, wantArgs) {
		t.Fatalf("cpro run did not forward the configured permission mode to claude:\ngot  %v\nwant %v", forwarded.Args, wantArgs)
	}
	if strings.Contains(strings.Join(forwarded.Args, " "), "rm -rf") || strings.Contains(strings.Join(forwarded.Args, " "), "disallowedTools") {
		t.Fatalf("legacy BlockedCommands must never reach the real claude invocation: %v", forwarded.Args)
	}
}

// TestYOLOImpliesTrust covers the coherence fix between permission mode and
// workspace trust (s.run, claude.go): YOLO explicitly promises "no prompts",
// so cpro run must not leave the separate folder-trust dialog as a loophole
// just because AutoTrust itself happens to be off. AutoTrust and
// PermissionMode stay two independent config fields — never merged — this
// only checks that s.run ORs their effect together for the one side effect
// (markTrusted) that actually suppresses a prompt.
func TestYOLOImpliesTrust(t *testing.T) {
	bin, s := buildCLI(t)
	_, slave := openPTY(t)
	loginCmd := exec.Command(bin, "login", "a@example.com")
	loginCmd.Stdin = slave
	if out, err := loginCmd.CombinedOutput(); err != nil {
		t.Fatalf("login: %s %v", out, err)
	}

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	claudeJSON := filepath.Join(s.profile("a@example.com"), ".claude.json")
	type project struct {
		HasTrustDialogAccepted bool `json:"hasTrustDialogAccepted"`
	}
	type claudeConfig struct {
		Projects map[string]project `json:"projects,omitempty"`
	}
	// markTrusted fabricates a minimal entry when none exists (see its own
	// doc comment, claude.go) — seeded here anyway, the same way a real first
	// run would have created it, so this test isolates the OR-logic between
	// AutoTrust and YOLO rather than also exercising fabrication (covered by
	// TestCLI's own subtest).
	seeded := claudeConfig{Projects: map[string]project{cwd: {}}}
	b, err := json.Marshal(seeded)
	if err != nil {
		t.Fatal(err)
	}
	if err := atomicWrite(claudeJSON, b); err != nil {
		t.Fatal(err)
	}

	if err := s.update(func(c *config) error {
		c.AutoTrust = false // explicitly off — the point of this test
		c.PermissionMode = "yolo"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(bin, "run", "--account", "a@example.com").Output(); err != nil {
		t.Fatalf("cpro run: %s %v", out, err)
	}

	data, err := os.ReadFile(claudeJSON)
	if err != nil {
		t.Fatal(err)
	}
	var got claudeConfig
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if !got.Projects[cwd].HasTrustDialogAccepted {
		t.Fatalf("expected YOLO to imply workspace trust even with AutoTrust off, got %+v", got)
	}

	// Sanity: AutoTrust itself is untouched on disk — the two settings stay
	// independent internally, only their effect is ORed together for YOLO.
	c, err := s.read()
	if err != nil {
		t.Fatal(err)
	}
	if c.AutoTrust {
		t.Fatalf("expected AutoTrust to remain false in config (unmerged setting), got %+v", c)
	}
}

// TestYOLOSupport covers requireYOLOSupport/claudeSupportsFlag (permissions.go)
// directly — the "fail clearly, before Claude starts, if the installed
// version can't actually deliver zero permission prompts" requirement —
// against the fake claude helper's own --help output (TestClaudeProcess).
// buildCLI's PATH/CPRO_TEST_* setup is what makes claudePath() resolve the
// fake helper here too, so these run in-process rather than through a
// spawned cpro subprocess.
func TestYOLOSupport(t *testing.T) {
	buildCLI(t)

	t.Run("supported: no error", func(t *testing.T) {
		ok, err := claudeSupportsFlag("--permission-mode")
		if err != nil {
			t.Fatalf("claudeSupportsFlag: %v", err)
		}
		if !ok {
			t.Fatal("expected the fake claude's --help to list --permission-mode")
		}
		if ok, err := claudeSupportsFlag("bypassPermissions"); err != nil || !ok {
			t.Fatalf("claudeSupportsFlag(bypassPermissions): ok=%v err=%v", ok, err)
		}
		if err := requireYOLOSupport(); err != nil {
			t.Fatalf("expected no error when both are supported, got %v", err)
		}
	})

	t.Run("unsupported: a clear error, not a silent fallback", func(t *testing.T) {
		t.Setenv("CPRO_TEST_NO_YOLO_SUPPORT", "1")
		// The simulated older claude still has --permission-mode itself — it's
		// specifically the bypassPermissions value that's missing from its own
		// choices list (decision 0038's own "flag exists, value doesn't yet"
		// scenario, matching Claude Code's own documented version gap).
		if ok, err := claudeSupportsFlag("--permission-mode"); err != nil || !ok {
			t.Fatalf("claudeSupportsFlag(--permission-mode): ok=%v err=%v", ok, err)
		}
		ok, err := claudeSupportsFlag("bypassPermissions")
		if err != nil {
			t.Fatalf("claudeSupportsFlag: %v", err)
		}
		if ok {
			t.Fatal("expected the simulated older claude's --help to NOT list bypassPermissions")
		}
		err = requireYOLOSupport()
		if err == nil {
			t.Fatal("expected requireYOLOSupport to fail for an installed claude missing a required value")
		}
		if !strings.Contains(err.Error(), "bypassPermissions") {
			t.Fatalf("expected the error to name the missing value, got %v", err)
		}
	})
}

// TestSessionContinuePermissionResolution is decision 0026 extended to
// session.go: proves, against the fake claude helper, that `cpro session
// continue` reaches Claude through the exact same permission
// resolver/argv-builder as `cpro run` — no separate, session-specific
// permission logic — for both the configured YOLO default (the exact
// invariant task #9 fixed) and a non-YOLO mode (proving continue doesn't
// force YOLO on every resumed session). The interactive picker's own built
// argv (sessionApp.finalizeContinue, sessionui.go) is provably identical in
// shape to the explicit form driven here — both are literally the same
// ["session","continue",from,to,"--","--resume",id] argv fed to this one
// cobra RunE — see TestSessionAppDirect's own coverage of that argv.
func TestSessionContinuePermissionResolution(t *testing.T) {
	bin, s := buildCLI(t)

	login := func(email string) {
		t.Helper()
		stage := t.TempDir()
		data := fmt.Sprintf(`{"loggedIn":true,"email":%q,"authMethod":"claude.ai"}`, email)
		if err := os.WriteFile(filepath.Join(stage, ".credentials.json"), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(stage, ".claude.json"), []byte(`{}`), 0600); err != nil {
			t.Fatal(err)
		}
		if err := s.installLogin(email, stage); err != nil {
			t.Fatal(err)
		}
	}
	login("perm-from@example.com")
	login("perm-to@example.com")

	projectDir := t.TempDir()
	dirName := projectDirName(projectDir)
	srcSessions := filepath.Join(s.profile("perm-from@example.com"), "projects", dirName)
	if err := os.MkdirAll(srcSessions, 0700); err != nil {
		t.Fatal(err)
	}
	const sessionID = "22222222-2222-2222-2222-222222222222"
	if err := os.WriteFile(filepath.Join(srcSessions, sessionID+".jsonl"), []byte(`{"type":"user"}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}

	forwardedArgs := func(t *testing.T, dir string) []string {
		t.Helper()
		cmd := exec.Command(bin, "session", "continue", "perm-from@example.com", "perm-to@example.com", "--", "--resume", sessionID)
		cmd.Dir = dir
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("cpro session continue: %s %v", out, err)
		}
		lines := strings.SplitN(string(out), "\n", 2)
		if len(lines) < 2 {
			t.Fatalf("expected a claude-args line after the copy confirmation, got %q", out)
		}
		var forwarded struct{ Args []string }
		if err := json.Unmarshal([]byte(lines[1]), &forwarded); err != nil {
			t.Fatalf("parsing the fake claude's recorded args: %s: %v", lines[1], err)
		}
		return forwarded.Args
	}

	t.Run("YOLO default: resumed session carries the same bypassPermissions mode as cpro run", func(t *testing.T) {
		if err := s.update(func(c *config) error { c.PermissionMode = "yolo"; return nil }); err != nil {
			t.Fatal(err)
		}
		got := forwardedArgs(t, projectDir)
		want := []string{"--permission-mode", "bypassPermissions", "--resume", sessionID}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})

	t.Run("Read-only default: session continue respects it, never forces YOLO", func(t *testing.T) {
		if err := s.update(func(c *config) error { c.PermissionMode = "readonly"; return nil }); err != nil {
			t.Fatal(err)
		}
		got := forwardedArgs(t, projectDir)
		want := []string{"--permission-mode", "plan", "--resume", sessionID}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("expected session continue to respect the configured Read-only mode, not force YOLO: got %v, want %v", got, want)
		}
	})

	// Decision 0034: session continue shares store.run's settings.json
	// toggle too, not just its argv builder — the destination account's
	// (TO_EMAIL's) own persisted blockReadsOutsideWorkingDirectories must
	// track the effective mode of the resumed session exactly like a plain
	// cpro run does.
	t.Run("YOLO default: destination account's settings.json is unblocked", func(t *testing.T) {
		if err := s.update(func(c *config) error { c.PermissionMode = "yolo"; return nil }); err != nil {
			t.Fatal(err)
		}
		forwardedArgs(t, projectDir)
		b, err := os.ReadFile(filepath.Join(s.profile("perm-to@example.com"), "settings.json"))
		if err != nil {
			t.Fatal(err)
		}
		var parsed struct {
			Permissions struct {
				BlockReadsOutsideWorkingDirectories bool `json:"blockReadsOutsideWorkingDirectories"`
			} `json:"permissions"`
		}
		if err := json.Unmarshal(b, &parsed); err != nil {
			t.Fatal(err)
		}
		if parsed.Permissions.BlockReadsOutsideWorkingDirectories {
			t.Fatal("expected the destination account's settings.json to be unblocked after a YOLO-default session continue")
		}
	})

	t.Run("Read-only default afterward: destination account's settings.json is re-blocked", func(t *testing.T) {
		if err := s.update(func(c *config) error { c.PermissionMode = "readonly"; return nil }); err != nil {
			t.Fatal(err)
		}
		forwardedArgs(t, projectDir)
		b, err := os.ReadFile(filepath.Join(s.profile("perm-to@example.com"), "settings.json"))
		if err != nil {
			t.Fatal(err)
		}
		var parsed struct {
			Permissions struct {
				BlockReadsOutsideWorkingDirectories bool `json:"blockReadsOutsideWorkingDirectories"`
			} `json:"permissions"`
		}
		if err := json.Unmarshal(b, &parsed); err != nil {
			t.Fatal(err)
		}
		if !parsed.Permissions.BlockReadsOutsideWorkingDirectories {
			t.Fatal("expected the destination account's settings.json to be re-blocked after leaving YOLO")
		}
	})
}

// TestYOLONoPromptsEndToEnd drives "cpro run" all the way through
// store.run (claude.go) with PermissionMode "yolo" — an installed claude
// that supports the required flags must actually reach it with the full
// bypass argv (proving the earlier version check didn't block a legitimate
// run), and one that doesn't must fail clearly and never invoke claude at
// all (the fake helper's JSON echo — proof it ran — must be completely
// absent from stdout), not silently start it in a more restrictive mode.
func TestYOLONoPromptsEndToEnd(t *testing.T) {
	bin, s := buildCLI(t)
	stage := t.TempDir()
	data := `{"loggedIn":true,"email":"yolo@example.com","authMethod":"claude.ai"}`
	if err := os.WriteFile(filepath.Join(stage, ".credentials.json"), []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, ".claude.json"), []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.installLogin("yolo@example.com", stage); err != nil {
		t.Fatal(err)
	}
	if err := s.update(func(c *config) error { c.PermissionMode = "yolo"; return nil }); err != nil {
		t.Fatal(err)
	}

	t.Run("supported claude: reaches Claude with the full bypass argv", func(t *testing.T) {
		cmd := exec.Command(bin, "run", "--account", "yolo@example.com")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("cpro run: %s %v", out, err)
		}
		var forwarded struct{ Args []string }
		if err := json.Unmarshal(out, &forwarded); err != nil {
			t.Fatalf("parsing the fake claude's recorded args: %s: %v", out, err)
		}
		want := []string{"--permission-mode", "bypassPermissions"}
		if !reflect.DeepEqual(forwarded.Args, want) {
			t.Fatalf("got %v, want %v", forwarded.Args, want)
		}
	})

	t.Run("unsupported claude: a clear error, claude never started", func(t *testing.T) {
		cmd := exec.Command(bin, "run", "--account", "yolo@example.com")
		cmd.Env = append(os.Environ(), "CPRO_TEST_NO_YOLO_SUPPORT=1")
		var out, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &stderr
		err := cmd.Run()
		exitErr, ok := err.(*exec.ExitError)
		if !ok || exitErr.ExitCode() == 0 {
			t.Fatalf("expected a non-zero exit, got err=%v stdout=%s stderr=%s", err, &out, &stderr)
		}
		if !strings.Contains(stderr.String(), "bypassPermissions") {
			t.Fatalf("expected a clear error naming the unsupported value, got stderr=%s", &stderr)
		}
		// The fake claude always echoes {"Args":...,"Directory":...} on stdout
		// when it actually runs — its total absence here is what proves cpro
		// never started it, rather than silently falling back to a more
		// restrictive mode that still shows prompts.
		if strings.Contains(out.String(), `"Args"`) {
			t.Fatalf("claude must never have started, got stdout=%s", &out)
		}
	})
}

// TestEnsureBlockReadsOutsideWorkingDirectories covers the helper (claude.go)
// decision 0034 added directly: creating settings.json from nothing,
// preserving unrelated existing keys (the exact shape of a real account's
// file — theme, skipDangerousModePermissionPrompt — observed live), toggling
// both ways, and the documented no-op when the persisted value already
// matches.
func TestEnsureBlockReadsOutsideWorkingDirectories(t *testing.T) {
	readBlocked := func(t *testing.T, path string) (bool, map[string]any) {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var data map[string]any
		if err := json.Unmarshal(b, &data); err != nil {
			t.Fatal(err)
		}
		permissions, _ := data["permissions"].(map[string]any)
		blocked, _ := permissions["blockReadsOutsideWorkingDirectories"].(bool)
		return blocked, data
	}

	t.Run("creates settings.json when absent", func(t *testing.T) {
		profile := t.TempDir()
		path := filepath.Join(profile, "settings.json")
		if _, err := os.Stat(path); err == nil {
			t.Fatal("test setup: settings.json should not already exist")
		}
		if err := ensureBlockReadsOutsideWorkingDirectories(profile, false); err != nil {
			t.Fatal(err)
		}
		blocked, _ := readBlocked(t, path)
		if blocked {
			t.Fatal("expected blockReadsOutsideWorkingDirectories false after creating with blocked=false")
		}
	})

	t.Run("preserves unrelated existing keys", func(t *testing.T) {
		profile := t.TempDir()
		path := filepath.Join(profile, "settings.json")
		seed := `{"theme":"dark","skipDangerousModePermissionPrompt":true,"permissions":{"blockReadsOutsideWorkingDirectories":true}}`
		if err := os.WriteFile(path, []byte(seed), 0600); err != nil {
			t.Fatal(err)
		}
		if err := ensureBlockReadsOutsideWorkingDirectories(profile, false); err != nil {
			t.Fatal(err)
		}
		blocked, data := readBlocked(t, path)
		if blocked {
			t.Fatal("expected blockReadsOutsideWorkingDirectories false")
		}
		if data["theme"] != "dark" {
			t.Fatalf("expected theme to be preserved untouched, got %v", data["theme"])
		}
		if data["skipDangerousModePermissionPrompt"] != true {
			t.Fatalf("expected skipDangerousModePermissionPrompt to be preserved untouched, got %v", data["skipDangerousModePermissionPrompt"])
		}
	})

	t.Run("toggles both ways", func(t *testing.T) {
		profile := t.TempDir()
		path := filepath.Join(profile, "settings.json")
		if err := ensureBlockReadsOutsideWorkingDirectories(profile, true); err != nil {
			t.Fatal(err)
		}
		if blocked, _ := readBlocked(t, path); !blocked {
			t.Fatal("expected blockReadsOutsideWorkingDirectories true")
		}
		if err := ensureBlockReadsOutsideWorkingDirectories(profile, false); err != nil {
			t.Fatal(err)
		}
		if blocked, _ := readBlocked(t, path); blocked {
			t.Fatal("expected blockReadsOutsideWorkingDirectories false after toggling back")
		}
	})

	t.Run("no-op when already the requested value", func(t *testing.T) {
		profile := t.TempDir()
		path := filepath.Join(profile, "settings.json")
		if err := ensureBlockReadsOutsideWorkingDirectories(profile, true); err != nil {
			t.Fatal(err)
		}
		before, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
		if err := ensureBlockReadsOutsideWorkingDirectories(profile, true); err != nil {
			t.Fatal(err)
		}
		after, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if !before.ModTime().Equal(after.ModTime()) {
			t.Fatal("expected no rewrite when the persisted value already matches the requested one")
		}
	})
}

// TestEnsureYOLOSettings covers ensureYOLOSettings (claude.go, decisions
// 0035 and 0058): creating settings.json from nothing, preserving unrelated
// existing keys, toggling both ways (enabling writes
// permissions.defaultMode, permissions.additionalDirectories, and the
// top-level skipDangerousModePermissionPrompt together; disabling removes
// all three rather than writing an "off" value), the documented no-op when
// the persisted state already matches, and — the one behavior
// TestEnsureBlockReadsOutsideWorkingDirectories has no equivalent of, since
// that function owns a single boolean rather than keys shared with whatever
// else might set them — that a hand-configured defaultMode/
// additionalDirectories this function did not itself set is left untouched
// rather than silently overwritten or deleted.
func TestEnsureYOLOSettings(t *testing.T) {
	readSettings := func(t *testing.T, path string) (mode string, dirs []any, skip bool, data map[string]any) {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, &data); err != nil {
			t.Fatal(err)
		}
		permissions, _ := data["permissions"].(map[string]any)
		mode, _ = permissions["defaultMode"].(string)
		dirs, _ = permissions["additionalDirectories"].([]any)
		skip, _ = data[skipDangerousModePermissionPromptKey].(bool)
		return mode, dirs, skip, data
	}

	t.Run("creates settings.json when absent", func(t *testing.T) {
		profile := t.TempDir()
		path := filepath.Join(profile, "settings.json")
		if err := ensureYOLOSettings(profile, true); err != nil {
			t.Fatal(err)
		}
		mode, dirs, skip, _ := readSettings(t, path)
		if mode != "bypassPermissions" {
			t.Fatalf("expected defaultMode bypassPermissions, got %q", mode)
		}
		if len(dirs) != 1 || dirs[0] != "/" {
			t.Fatalf("expected additionalDirectories [\"/\"], got %v", dirs)
		}
		if !skip {
			t.Fatal("expected skipDangerousModePermissionPrompt set true, so a real interactive launch never stops at Claude Code's own bypass-mode confirmation screen")
		}
	})

	t.Run("preserves unrelated existing keys", func(t *testing.T) {
		profile := t.TempDir()
		path := filepath.Join(profile, "settings.json")
		seed := `{"theme":"dark","permissions":{"blockReadsOutsideWorkingDirectories":false}}`
		if err := os.WriteFile(path, []byte(seed), 0600); err != nil {
			t.Fatal(err)
		}
		if err := ensureYOLOSettings(profile, true); err != nil {
			t.Fatal(err)
		}
		_, _, _, data := readSettings(t, path)
		if data["theme"] != "dark" {
			t.Fatalf("expected theme to be preserved untouched, got %v", data["theme"])
		}
		permissions, _ := data["permissions"].(map[string]any)
		if blocked, _ := permissions["blockReadsOutsideWorkingDirectories"].(bool); blocked {
			t.Fatal("expected blockReadsOutsideWorkingDirectories to be preserved untouched")
		}
	})

	t.Run("toggles both ways", func(t *testing.T) {
		profile := t.TempDir()
		path := filepath.Join(profile, "settings.json")
		if err := ensureYOLOSettings(profile, true); err != nil {
			t.Fatal(err)
		}
		if mode, dirs, skip, _ := readSettings(t, path); mode != "bypassPermissions" || len(dirs) != 1 || !skip {
			t.Fatalf("expected enabled state, got mode=%q dirs=%v skip=%v", mode, dirs, skip)
		}
		if err := ensureYOLOSettings(profile, false); err != nil {
			t.Fatal(err)
		}
		mode, dirs, skip, data := readSettings(t, path)
		if mode != "" || dirs != nil || skip {
			t.Fatalf("expected all three keys removed after disabling, got mode=%q dirs=%v skip=%v", mode, dirs, skip)
		}
		if permissions, ok := data["permissions"].(map[string]any); ok {
			if _, has := permissions["defaultMode"]; has {
				t.Fatal("expected defaultMode key removed, not just emptied")
			}
			if _, has := permissions["additionalDirectories"]; has {
				t.Fatal("expected additionalDirectories key removed, not just emptied")
			}
		}
		if _, has := data[skipDangerousModePermissionPromptKey]; has {
			t.Fatal("expected skipDangerousModePermissionPrompt key removed, not just emptied")
		}
	})

	t.Run("no-op when already the requested value", func(t *testing.T) {
		profile := t.TempDir()
		path := filepath.Join(profile, "settings.json")
		if err := ensureYOLOSettings(profile, true); err != nil {
			t.Fatal(err)
		}
		before, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
		if err := ensureYOLOSettings(profile, true); err != nil {
			t.Fatal(err)
		}
		after, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if !before.ModTime().Equal(after.ModTime()) {
			t.Fatal("expected no rewrite when the persisted value already matches the requested one")
		}
	})

	t.Run("disabling when nothing was ever enabled is a no-op", func(t *testing.T) {
		profile := t.TempDir()
		path := filepath.Join(profile, "settings.json")
		seed := `{"theme":"dark"}`
		if err := os.WriteFile(path, []byte(seed), 0600); err != nil {
			t.Fatal(err)
		}
		before, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
		if err := ensureYOLOSettings(profile, false); err != nil {
			t.Fatal(err)
		}
		after, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if !before.ModTime().Equal(after.ModTime()) {
			t.Fatal("expected no rewrite when disabling a profile that was never enabled")
		}
	})

	t.Run("leaves a hand-configured defaultMode/additionalDirectories alone", func(t *testing.T) {
		profile := t.TempDir()
		path := filepath.Join(profile, "settings.json")
		seed := `{"permissions":{"defaultMode":"acceptEdits","additionalDirectories":["/srv/data"]}}`
		if err := os.WriteFile(path, []byte(seed), 0600); err != nil {
			t.Fatal(err)
		}
		if err := ensureYOLOSettings(profile, false); err != nil {
			t.Fatal(err)
		}
		mode, dirs, _, _ := readSettings(t, path)
		if mode != "acceptEdits" {
			t.Fatalf("expected hand-configured defaultMode left alone, got %q", mode)
		}
		if len(dirs) != 1 || dirs[0] != "/srv/data" {
			t.Fatalf("expected hand-configured additionalDirectories left alone, got %v", dirs)
		}
	})
}

// TestRequireNotRoot covers requireNotRoot (permissions.go, decision 0035)
// against this test process's own real, current, non-root UID — the same
// "check real state rather than add a parallel test-only path" convention
// CLAUDE.md documents for terminalInput. Root itself is not simulated: there
// is no safe, sandboxed way to actually become root inside this test suite,
// and the function itself is a single os.Geteuid() == 0 comparison with
// nothing else to exercise.
func TestRequireNotRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("this test process is running as root; requireNotRoot's positive case cannot be exercised here")
	}
	if err := requireNotRoot(); err != nil {
		t.Fatalf("expected no error for a non-root process, got %v", err)
	}
}

// TestYOLOSettingsToggle is decision 0034's own end-to-end proof (via the
// fake claude helper, no real API cost) that store.run keeps the account's
// own settings.json in sync with the ACTUAL effective mode of each
// invocation, not just the configured default: a configured YOLO default
// sets blockReadsOutsideWorkingDirectories false, a later configured
// non-YOLO mode restores it to true (so leaving YOLO doesn't leave the
// restriction silently disabled forever — the "Live a little/YOLO must stay
// distinct" requirement, now proven at the settings-file level too, not just
// in the argv each mode builds), and an EXPLICIT forwarded
// --permission-mode bypassPermissions also counts as YOLO for this purpose
// even when the configured default is something else entirely
// (proving the settings toggle reads the final args, not just
// config.PermissionMode). Decision 0035 extended every subtest here to also
// assert permissions.defaultMode/additionalDirectories (ensureYOLOSettings,
// claude.go) follow the exact same on/off rhythm as
// blockReadsOutsideWorkingDirectories, since both are driven by the same
// yoloEffective value in store.run.
func TestYOLOSettingsToggle(t *testing.T) {
	bin, s := buildCLI(t)
	stage := t.TempDir()
	data := `{"loggedIn":true,"email":"settings-toggle@example.com","authMethod":"claude.ai"}`
	if err := os.WriteFile(filepath.Join(stage, ".credentials.json"), []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, ".claude.json"), []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.installLogin("settings-toggle@example.com", stage); err != nil {
		t.Fatal(err)
	}
	settingsPath := filepath.Join(s.profile("settings-toggle@example.com"), "settings.json")
	blocked := func(t *testing.T) bool {
		t.Helper()
		b, err := os.ReadFile(settingsPath)
		if err != nil {
			t.Fatal(err)
		}
		var parsed struct {
			Permissions struct {
				BlockReadsOutsideWorkingDirectories bool `json:"blockReadsOutsideWorkingDirectories"`
			} `json:"permissions"`
		}
		if err := json.Unmarshal(b, &parsed); err != nil {
			t.Fatal(err)
		}
		return parsed.Permissions.BlockReadsOutsideWorkingDirectories
	}
	yoloSettingsEnabled := func(t *testing.T) bool {
		t.Helper()
		b, err := os.ReadFile(settingsPath)
		if err != nil {
			t.Fatal(err)
		}
		var parsed struct {
			SkipDangerousModePermissionPrompt bool `json:"skipDangerousModePermissionPrompt"`
			Permissions                       struct {
				DefaultMode           string   `json:"defaultMode"`
				AdditionalDirectories []string `json:"additionalDirectories"`
			} `json:"permissions"`
		}
		if err := json.Unmarshal(b, &parsed); err != nil {
			t.Fatal(err)
		}
		return parsed.Permissions.DefaultMode == "bypassPermissions" &&
			len(parsed.Permissions.AdditionalDirectories) == 1 &&
			parsed.Permissions.AdditionalDirectories[0] == "/" &&
			parsed.SkipDangerousModePermissionPrompt
	}

	t.Run("configured YOLO default disables the block", func(t *testing.T) {
		if err := s.update(func(c *config) error { c.PermissionMode = "yolo"; return nil }); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command(bin, "run", "--account", "settings-toggle@example.com").Output(); err != nil {
			t.Fatalf("cpro run: %s %v", out, err)
		}
		if blocked(t) {
			t.Fatal("expected blockReadsOutsideWorkingDirectories false after a configured YOLO run")
		}
		if !yoloSettingsEnabled(t) {
			t.Fatal("expected defaultMode bypassPermissions and additionalDirectories [\"/\"] after a configured YOLO run")
		}
	})

	t.Run("a later configured non-YOLO default restores the block", func(t *testing.T) {
		if err := s.update(func(c *config) error { c.PermissionMode = "readonly"; return nil }); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command(bin, "run", "--account", "settings-toggle@example.com").Output(); err != nil {
			t.Fatalf("cpro run: %s %v", out, err)
		}
		if !blocked(t) {
			t.Fatal("expected blockReadsOutsideWorkingDirectories restored to true once YOLO is no longer the effective mode")
		}
		if yoloSettingsEnabled(t) {
			t.Fatal("expected defaultMode/additionalDirectories removed once YOLO is no longer the effective mode")
		}
	})

	t.Run("explicit forwarded YOLO flag also disables the block, independent of the configured default", func(t *testing.T) {
		if err := s.update(func(c *config) error { c.PermissionMode = "ask"; return nil }); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command(bin, "run", "--account", "settings-toggle@example.com", "--",
			"--permission-mode", "bypassPermissions").Output()
		if err != nil {
			t.Fatalf("cpro run: %s %v", out, err)
		}
		if blocked(t) {
			t.Fatal("expected an explicit forwarded --permission-mode bypassPermissions to disable the block even with a non-YOLO configured default")
		}
		if !yoloSettingsEnabled(t) {
			t.Fatal("expected defaultMode/additionalDirectories set by an explicit forwarded YOLO choice too")
		}
	})

	t.Run("a plain configured Ask run afterward restores the block again", func(t *testing.T) {
		// PermissionMode is already "ask" from the previous subtest; a plain
		// run (no forwarded flags) must not leave the previous subtest's
		// explicit override lingering in the settings file.
		if out, err := exec.Command(bin, "run", "--account", "settings-toggle@example.com").Output(); err != nil {
			t.Fatalf("cpro run: %s %v", out, err)
		}
		if !blocked(t) {
			t.Fatal("expected blockReadsOutsideWorkingDirectories restored to true for a plain Ask run")
		}
		if yoloSettingsEnabled(t) {
			t.Fatal("expected defaultMode/additionalDirectories removed after the previous subtest's explicit override, not left lingering")
		}
	})
}

// TestRequireNoManagedPermissionsPolicy covers requireNoManagedPermissionsPolicy
// (permissions.go, decision 0034) against the fake claude helper's own
// simulated `claude doctor` output: no known policy is a nil error (the
// default, matching a real Pro account observed live), CPRO_TEST_MANAGED_POLICY=1
// simulates an account under an enterprise/organization policy and must
// produce the exact "YOLO unavailable" message, and a claude that fails
// `doctor` entirely degrades to "no known policy" rather than blocking an
// unrelated run.
func TestRequireNoManagedPermissionsPolicy(t *testing.T) {
	buildCLI(t)
	profile := t.TempDir()

	t.Run("no managed policy: nil error", func(t *testing.T) {
		if err := requireNoManagedPermissionsPolicy(profile); err != nil {
			t.Fatalf("expected no error for an account with no managed policy, got %v", err)
		}
	})

	t.Run("managed policy fetched: a clear YOLO-unavailable error", func(t *testing.T) {
		t.Setenv("CPRO_TEST_MANAGED_POLICY", "1")
		err := requireNoManagedPermissionsPolicy(profile)
		if err == nil {
			t.Fatal("expected an error when a managed policy is in effect")
		}
		if !strings.Contains(err.Error(), "YOLO unavailable") {
			t.Fatalf("expected the exact \"YOLO unavailable\" message shape, got %v", err)
		}
		if !strings.Contains(err.Error(), "cpro cannot guarantee zero permission prompts") {
			t.Fatalf("expected the error to explicitly say cpro cannot guarantee zero prompts, got %v", err)
		}
	})
}

// TestYOLOOutsideDirectoryReadIntegration is decision 0026's own real,
// end-to-end regression proof — not just "does the argv string contain the
// right flag" (TestPermissionModeArgs/TestYOLONoPromptsEndToEnd, which use
// the fake claude helper) but "does a REAL, installed, authenticated claude,
// given cpro's actual YOLO argv, actually read a file outside its working
// directory with no interactive 'Do you want to proceed?' approval prompt
// for permissions.blockReadsOutsideWorkingDirectories." This is exactly the
// test that first proved the old --settings override could be silently
// defeated by a conflicting value already present in an account's own
// persisted user settings.json (see the decision record for the full
// investigation) — and the test that would fail if that regresses.
//
// Deliberately gated behind CPRO_TEST_REAL_CLAUDE_INTEGRATION=1 (unset by
// default, so the normal `go test ./...` run stays network-free and
// cost-free): it spends a small amount of real API usage against whichever
// claude installation is already authenticated in the environment it runs
// in. Run it on demand:
//
//	CPRO_TEST_REAL_CLAUDE_INTEGRATION=1 go test ./... -run TestYOLOOutsideDirectoryReadIntegration -v
func TestYOLOOutsideDirectoryReadIntegration(t *testing.T) {
	if os.Getenv("CPRO_TEST_REAL_CLAUDE_INTEGRATION") != "1" {
		t.Skip("set CPRO_TEST_REAL_CLAUDE_INTEGRATION=1 to run this real, API-calling integration test")
	}
	realClaude, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude not found in PATH")
	}

	workspace := t.TempDir()
	outside := t.TempDir()
	const marker = "CPRO_OUTSIDE_READ_OK"
	probe := filepath.Join(outside, "probe.txt")
	if err := os.WriteFile(probe, []byte(marker+"\n"), 0600); err != nil {
		t.Fatal(err)
	}

	prompt := fmt.Sprintf("Use the Read tool to read %s and output exactly its contents, nothing else.", probe)
	args := append(append([]string{}, yoloArgs()...), "--permission-prompts", "none", "-p", prompt, "--output-format", "json")
	cmd := exec.Command(realClaude, args...)
	cmd.Dir = workspace
	cmd.Env = os.Environ()
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("claude invocation failed: %v; stderr=%s", err, &stderr)
	}

	var result struct {
		Result            string `json:"result"`
		PermissionDenials []struct {
			ToolName string `json:"tool_name"`
		} `json:"permission_denials"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("could not parse claude's JSON output: %v; raw=%s", err, &stdout)
	}
	for _, d := range result.PermissionDenials {
		if d.ToolName == "Read" {
			t.Fatalf("expected no Read permission denial for an outside-working-directory read under YOLO; claude's final answer was %q", result.Result)
		}
	}
	if !strings.Contains(result.Result, marker) {
		t.Fatalf("expected claude's final answer to contain the probe file's contents (%q), got %q", marker, result.Result)
	}
}

// TestYOLOShellParserFallbackIntegration is decision 0034's own real,
// end-to-end regression proof for the OTHER class of outside-working-
// directory prompt decision 0026's --add-dir fix left standing: a Bash
// command whose actual target path Claude's own shell parser cannot
// statically analyze falls back to consulting
// permissions.blockReadsOutsideWorkingDirectories' raw persisted value
// directly, bypassing --add-dir's allowlist entirely — this test drives the
// real, installed `claude` through the actual built `cpro run` binary (not a
// direct claude invocation with hand-built argv, since the fix under test,
// ensureBlockReadsOutsideWorkingDirectories, lives in store.run itself, not
// in yoloArgs()) and proves all four reported sub-classes together in one
// real call, plus the direct read decision 0026 already covered, stays
// clean:
//
//  1. simple shell expansion of a variable whose value is only known at
//     runtime (an inherited env var, not a literal assigned in the same
//     command — that narrower case was already unaffected even before this
//     decision, since a same-command assignment is still statically
//     analyzable; a real inherited env var is the actual reported case)
//  2. an inline interpreter (`python3 -c ...`)
//  3. a runtime-determined `find ... -exec` argument
//  4. a runtime-computed path (command substitution via `$(...)`)
//
// Manually reproduced first (see decision 0034's own record) against this
// exact account with permissions.blockReadsOutsideWorkingDirectories forced
// true in its real settings.json: classes 1-4 above were all auto-denied
// with "no approval surface... target path only knowable at runtime"
// reasoning, while the direct Read (decision 0026's own case) still
// succeeded — proving --add-dir alone does not reach this fallback path.
// With the fix applied (this test's own live proof, not just the manual
// one), every class succeeds and permission_denials is empty.
//
// Deliberately gated behind CPRO_TEST_REAL_CLAUDE_INTEGRATION=1 (unset by
// default, same convention as TestYOLOOutsideDirectoryReadIntegration/
// TestSessionContinueYOLOOutsideDirectoryReadIntegration) — it spends a
// small amount of real API usage against whichever claude installation is
// already authenticated in the environment it runs in, and asserts on the
// actual response text, not just exit code 0: any of the reported prompt/
// denial phrasings ("do you want to proceed", "blockreadsoutsideworking",
// "shell parser cannot analyze", "cannot be checked against the read
// block", "computed at run time"/"runtime-determined", "asks the person")
// appearing anywhere in the raw output fails the test, even if claude still
// happened to exit 0 and even if some phrasing decision 0034's own manual
// investigation didn't happen to see verbatim shows up instead. Run it on
// demand:
//
//	CPRO_TEST_REAL_CLAUDE_INTEGRATION=1 go test ./... -run TestYOLOShellParserFallbackIntegration -v
func TestYOLOShellParserFallbackIntegration(t *testing.T) {
	if os.Getenv("CPRO_TEST_REAL_CLAUDE_INTEGRATION") != "1" {
		t.Skip("set CPRO_TEST_REAL_CLAUDE_INTEGRATION=1 to run this real, API-calling integration test")
	}
	realClaude, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude not found in PATH")
	}
	statusOut, err := exec.Command(realClaude, "auth", "status", "--json").Output()
	if err != nil {
		t.Skipf("claude auth status failed, skipping (no authenticated account in this environment): %v", err)
	}
	var status struct {
		LoggedIn bool   `json:"loggedIn"`
		Email    string `json:"email"`
	}
	if err := json.Unmarshal(statusOut, &status); err != nil || !status.LoggedIn || status.Email == "" {
		t.Skip("no authenticated claude account in this environment, skipping")
	}

	dir := t.TempDir()
	bin := filepath.Join(dir, "cpro")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %s %v", out, err)
	}
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	s, err := openStore()
	if err != nil {
		t.Fatal(err)
	}
	// Seed this real, currently-authenticated account's own real credentials
	// into a fresh, hermetic cpro profile (a temp XDG_CONFIG_HOME above), the
	// same copy-real-creds approach
	// TestSessionContinueYOLOOutsideDirectoryReadIntegration already uses —
	// s.run's own validAuth check requires the registered email to match the
	// real one in its credentials exactly.
	realConfigDir := os.Getenv("CLAUDE_CONFIG_DIR")
	if realConfigDir == "" {
		realConfigDir = filepath.Join(os.Getenv("HOME"), ".claude")
	}
	profile := s.profile(status.Email)
	if err := privateDir(profile); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".credentials.json", ".claude.json"} {
		data, err := os.ReadFile(filepath.Join(realConfigDir, name))
		if err != nil {
			t.Fatalf("reading real %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(profile, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.update(func(c *config) error {
		c.Accounts[status.Email] = true
		c.PermissionMode = "yolo"
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	workspace := t.TempDir()
	outside := t.TempDir()
	write := func(name, marker string) string {
		t.Helper()
		p := filepath.Join(outside, name)
		if err := os.WriteFile(p, []byte(marker+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	const (
		expansionMarker = "CPRO_SIMPLE_EXPANSION_OK"
		pythonMarker    = "CPRO_INLINE_PYTHON_OK"
		findMarker      = "CPRO_RUNTIME_FIND_OK"
		computedMarker  = "CPRO_COMPUTED_PATH_OK"
	)
	expansionProbe := write("expansion.txt", expansionMarker)
	pythonProbe := write("python.txt", pythonMarker)
	findProbe := write("find_marker.txt", findMarker)
	computedProbe := write("computed.txt", computedMarker)

	prompt := fmt.Sprintf(`Run each of these 4 bash commands, one at a time, using the Bash tool exactly as written. Do not ask for confirmation, do not simplify or rewrite them, and do not skip any even if one is denied.

1) cat "$EXPANSION_PROBE_PATH"
2) python3 -c "print(open('%s').read())"
3) find %s -name %q -exec cat {} \;
4) DIR=$(dirname %s); cat "$DIR/computed.txt"

Report each command's raw output on its own line, prefixed CHECK1/CHECK2/CHECK3/CHECK4.`,
		pythonProbe, outside, filepath.Base(findProbe), computedProbe)

	args := []string{"run", "--account", status.Email, "--",
		"--permission-prompts", "none", "-p", prompt, "--output-format", "json"}
	cmd := exec.Command(bin, args...)
	cmd.Dir = workspace
	cmd.Env = append(os.Environ(), "EXPANSION_PROBE_PATH="+expansionProbe)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("cpro run failed: %v; stdout=%s stderr=%s", err, &stdout, &stderr)
	}

	var result struct {
		Result            string `json:"result"`
		PermissionDenials []struct {
			ToolName string `json:"tool_name"`
			ToolUse  any    `json:"tool_input"`
		} `json:"permission_denials"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("could not parse claude's JSON output: %v; raw=%s", err, &stdout)
	}
	if len(result.PermissionDenials) != 0 {
		t.Fatalf("expected zero permission denials for any of the 4 shell-parser-fallback classes under YOLO, got %d: %+v; claude's final answer was %q", len(result.PermissionDenials), result.PermissionDenials, result.Result)
	}
	for _, marker := range []string{expansionMarker, pythonMarker, findMarker, computedMarker} {
		if !strings.Contains(result.Result, marker) {
			t.Fatalf("expected claude's final answer to contain marker %q (one of the 4 shell-parser-fallback probes), got %q", marker, result.Result)
		}
	}
	lower := strings.ToLower(result.Result)
	for _, phrase := range []string{
		"do you want to proceed",
		"blockreadsoutsideworking",
		"shell parser cannot analyze",
		"cannot be checked against the read block",
		"computed at run time",
		"runtime-determined",
		"asks the person",
	} {
		if strings.Contains(lower, phrase) {
			t.Fatalf("expected no permission-confirmation language in claude's output, found %q in %q", phrase, result.Result)
		}
	}

	// The fix under test is store.run's own settings.json toggle
	// (ensureBlockReadsOutsideWorkingDirectories, claude.go) — confirm it
	// actually ran for this real account, not just that the probes happened
	// to succeed for some other reason.
	b, err := os.ReadFile(filepath.Join(profile, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var settings struct {
		Permissions struct {
			BlockReadsOutsideWorkingDirectories bool `json:"blockReadsOutsideWorkingDirectories"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal(b, &settings); err != nil {
		t.Fatal(err)
	}
	if settings.Permissions.BlockReadsOutsideWorkingDirectories {
		t.Fatal("expected this real account's own settings.json to have blockReadsOutsideWorkingDirectories false after a configured-YOLO cpro run")
	}
}

// TestYOLOInteractiveNoBypassWarningIntegration is decision 0058's own real,
// end-to-end proof of the gap every prior YOLO "zero prompts" test — this
// file's own TestYOLONoPromptsEndToEnd (the fake claude helper) and every
// other real-claude integration test above (all headless, via -p) — could
// not have caught: a genuinely *interactive* launch (no -p, exactly what
// cpro run's own syscall.Exec into a normal session produces) stops at
// Claude Code's own one-time "WARNING: Claude Code running in Bypass
// Permissions mode / Yes, I accept" confirmation screen unless the account's
// settings.json also carries the top-level skipDangerousModePermissionPrompt
// key — undocumented on the official settings-reference page, confirmed by
// community documentation and live manual testing on this machine (see
// ensureYOLOSettings's own doc comment, claude.go) — which
// ensureBlockReadsOutsideWorkingDirectories/ensureYOLOSettings's own prior
// two keys never touched. This drives the real, installed claude through the
// actual built cpro run binary via a real pseudoterminal (no message ever
// sent to Claude — asserting only the very first screen after launch means
// this spends no real API usage, unlike the other real-integration tests
// above), and asserts the warning's own distinctive text is absent while a
// normal ready session (its "bypass permissions on" status line) is present.
// Also manually verified, in the same live session that found this gap, that
// a command with more than one `cd` and a subshell — Claude Code's own two
// documented examples of the shell-parser-fallback case decision 0034 fixed —
// run with no prompt at all once this fix is applied; not re-asserted here
// as a scripted probe since doing so needs a real conversation turn (real
// API usage) on top of what TestYOLOShellParserFallbackIntegration already
// spends on the four classes it does cover headlessly.
//
// Deliberately gated behind CPRO_TEST_REAL_CLAUDE_INTEGRATION=1, same
// convention as every other real-claude test in this file. Run it on demand:
//
//	CPRO_TEST_REAL_CLAUDE_INTEGRATION=1 go test ./... -run TestYOLOInteractiveNoBypassWarningIntegration -v
func TestYOLOInteractiveNoBypassWarningIntegration(t *testing.T) {
	if os.Getenv("CPRO_TEST_REAL_CLAUDE_INTEGRATION") != "1" {
		t.Skip("set CPRO_TEST_REAL_CLAUDE_INTEGRATION=1 to run this real, interactive integration test")
	}
	realClaude, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude not found in PATH")
	}
	statusOut, err := exec.Command(realClaude, "auth", "status", "--json").Output()
	if err != nil {
		t.Skipf("claude auth status failed, skipping (no authenticated account in this environment): %v", err)
	}
	var status struct {
		LoggedIn bool   `json:"loggedIn"`
		Email    string `json:"email"`
	}
	if err := json.Unmarshal(statusOut, &status); err != nil || !status.LoggedIn || status.Email == "" {
		t.Skip("no authenticated claude account in this environment, skipping")
	}

	dir := t.TempDir()
	bin := filepath.Join(dir, "cpro")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %s %v", out, err)
	}
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	s, err := openStore()
	if err != nil {
		t.Fatal(err)
	}
	// Same real-credentials-copy approach TestYOLOShellParserFallbackIntegration
	// uses: s.run's own validAuth check requires the registered email to
	// match the real one in its credentials exactly.
	realConfigDir := os.Getenv("CLAUDE_CONFIG_DIR")
	if realConfigDir == "" {
		realConfigDir = filepath.Join(os.Getenv("HOME"), ".claude")
	}
	profile := s.profile(status.Email)
	if err := privateDir(profile); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".credentials.json", ".claude.json"} {
		data, err := os.ReadFile(filepath.Join(realConfigDir, name))
		if err != nil {
			t.Fatalf("reading real %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(profile, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.update(func(c *config) error {
		c.Accounts[status.Email] = true
		c.PermissionMode = "yolo"
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	workspace := t.TempDir()
	master, slave := openPTY(t)
	capture := drainPTY(master)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "run", "--account", status.Email)
	cmd.Dir = workspace
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	time.Sleep(4 * time.Second) // real claude's own startup, not the fake helper's — needs longer to settle
	out := stripANSI(capture())
	if strings.Contains(out, "Bypass Permissions mode") || strings.Contains(out, "Yes, I accept") {
		t.Fatalf("expected no bypass-mode confirmation screen on a real interactive YOLO launch, got %q", out)
	}
	if !strings.Contains(out, "bypass permissions on") {
		t.Fatalf("expected a normal ready session already in bypass-permissions mode, got %q", out)
	}
}

// TestSystemCredentials covers "cpro system export"/"cpro system import"
// non-interactively (a direct EMAIL argument for export, always-non-
// interactive for import): resolving/validating accounts, the actual
// credential transfer in both directions, conflict/error handling, and that
// no credential value ever appears in printed output. "system" (the real,
// unmanaged Claude Code credential store — see systemConfigPaths, claude.go)
// is a temp directory for the whole test, via CLAUDE_CONFIG_DIR: that's the
// one env var systemConfigPaths deliberately honors (unlike claudeCommand's
// own per-account override), so this is a real exercise of the same code
// path a bare, cpro-less `claude` would use, just pointed somewhere hermetic
// rather than a real $HOME.
func TestSystemCredentials(t *testing.T) {
	bin, s := buildCLI(t)
	systemDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", systemDir)

	run := func(code int, args ...string) (string, string) {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Env = os.Environ()
		var out, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &stderr
		err := cmd.Run()
		got := 0
		if err != nil {
			if e, ok := err.(*exec.ExitError); ok {
				got = e.ExitCode()
			} else {
				t.Fatal(err)
			}
		}
		if got != code {
			t.Fatalf("%q: exit %d, want %d; stdout %s stderr %s", args, got, code, &out, &stderr)
		}
		return out.String(), stderr.String()
	}
	login := func(email, accessToken string) {
		t.Helper()
		stage := t.TempDir()
		data := fmt.Sprintf(`{"loggedIn":true,"email":%q,"authMethod":"claude.ai","claudeAiOauth":{"accessToken":%q}}`, email, accessToken)
		if err := os.WriteFile(filepath.Join(stage, ".credentials.json"), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(stage, ".claude.json"), []byte(`{}`), 0600); err != nil {
			t.Fatal(err)
		}
		if err := s.installLogin(email, stage); err != nil {
			t.Fatal(err)
		}
	}
	writeSystemCredentials := func(t *testing.T, raw string) {
		t.Helper()
		if err := os.MkdirAll(systemDir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(systemDir, ".credentials.json"), []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
	}
	clearSystemCredentials := func(t *testing.T) {
		t.Helper()
		_ = os.Remove(filepath.Join(systemDir, ".credentials.json"))
	}

	t.Run("exporting an existing authenticated account", func(t *testing.T) {
		login("export-ok@example.com", "token-export-ok")
		out, _ := run(0, "system", "export", "export-ok@example.com")
		if !strings.Contains(out, "Credentials exported") || !strings.Contains(out, "export-ok@example.com") {
			t.Fatalf("expected a concise confirmation, got %q", out)
		}
		got, err := os.ReadFile(filepath.Join(systemDir, ".credentials.json"))
		if err != nil {
			t.Fatal(err)
		}
		want, err := os.ReadFile(filepath.Join(s.profile("export-ok@example.com"), ".credentials.json"))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Fatalf("system credentials don't match the exported account's own: got %s want %s", got, want)
		}
	})

	t.Run("exporting a missing account", func(t *testing.T) {
		_, stderr := run(1, "system", "export", "never-registered@example.com")
		if !strings.Contains(stderr, "not registered") {
			t.Fatalf("expected a clear \"not registered\" error, got %q", stderr)
		}
	})

	t.Run("exporting an unauthenticated account", func(t *testing.T) {
		login("export-unauth@example.com", "token-export-unauth")
		run(0, "logout", "export-unauth@example.com")
		_, stderr := run(1, "system", "export", "export-unauth@example.com")
		if !strings.Contains(stderr, "not authenticated") {
			t.Fatalf("expected a clear \"not authenticated\" error, got %q", stderr)
		}
	})

	t.Run("export does not create a cpro default account", func(t *testing.T) {
		before, err := s.read()
		if err != nil {
			t.Fatal(err)
		}
		login("export-nodefault@example.com", "token-export-nodefault")
		run(0, "system", "export", "export-nodefault@example.com")
		after, err := s.read()
		if err != nil {
			t.Fatal(err)
		}
		if after.Default != before.Default {
			t.Fatalf("export must never change the default account: before %q after %q", before.Default, after.Default)
		}
	})

	t.Run("importing a new system account", func(t *testing.T) {
		writeSystemCredentials(t, `{"loggedIn":true,"email":"import-new@example.com","authMethod":"claude.ai"}`)
		before, err := s.read()
		if err != nil {
			t.Fatal(err)
		}
		if before.Accounts["import-new@example.com"] {
			t.Fatal("test setup: account should not already exist")
		}
		out, _ := run(0, "system", "import")
		if !strings.Contains(out, "Imported") || !strings.Contains(out, "import-new@example.com") {
			t.Fatalf("expected \"Imported\" and the email, got %q", out)
		}
		after, err := s.read()
		if err != nil {
			t.Fatal(err)
		}
		if !after.Accounts["import-new@example.com"] {
			t.Fatal("expected the new account to be registered after import")
		}
		if after.Default != before.Default {
			t.Fatalf("import must never change the default account: before %q after %q", before.Default, after.Default)
		}
	})

	t.Run("importing an existing account updates it, preserving profile/history metadata", func(t *testing.T) {
		login("import-existing@example.com", "token-original")
		profile := s.profile("import-existing@example.com")
		// A stand-in for "profile/history data" beyond the two auth files —
		// import must never touch this.
		marker := filepath.Join(profile, "cpro-usage.json")
		if err := os.WriteFile(marker, []byte(`{"marker":true}`), 0600); err != nil {
			t.Fatal(err)
		}
		settingsBefore, err := os.ReadFile(filepath.Join(profile, ".claude.json"))
		if err != nil {
			t.Fatal(err)
		}

		writeSystemCredentials(t, `{"loggedIn":true,"email":"import-existing@example.com","authMethod":"claude.ai","claudeAiOauth":{"accessToken":"token-updated"}}`)
		out, _ := run(0, "system", "import")
		if !strings.Contains(out, "Updated") || !strings.Contains(out, "import-existing@example.com") {
			t.Fatalf("expected \"Updated\" (not \"Imported\") and the email, got %q", out)
		}

		if _, err := os.Stat(marker); err != nil {
			t.Fatalf("import must preserve existing profile/history data, marker file gone: %v", err)
		}
		settingsAfter, err := os.ReadFile(filepath.Join(profile, ".claude.json"))
		if err != nil {
			t.Fatal(err)
		}
		if string(settingsAfter) != string(settingsBefore) {
			t.Fatalf("import must not touch .claude.json (settings/trust history): before %s after %s", settingsBefore, settingsAfter)
		}
		updatedCreds, err := os.ReadFile(filepath.Join(profile, ".credentials.json"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(updatedCreds), "token-updated") {
			t.Fatalf("expected the account's credentials to reflect the newly imported ones, got %s", updatedCreds)
		}
	})

	t.Run("missing system credentials", func(t *testing.T) {
		clearSystemCredentials(t)
		before, err := s.read()
		if err != nil {
			t.Fatal(err)
		}
		_, stderr := run(1, "system", "import")
		if !strings.Contains(stderr, "no system Claude credentials found") {
			t.Fatalf("expected a clear \"no system credentials\" error, got %q", stderr)
		}
		after, err := s.read()
		if err != nil {
			t.Fatal(err)
		}
		if len(after.Accounts) != len(before.Accounts) {
			t.Fatalf("missing system credentials must not create an account: before %+v after %+v", before.Accounts, after.Accounts)
		}
	})

	t.Run("malformed system credentials", func(t *testing.T) {
		before, err := s.read()
		if err != nil {
			t.Fatal(err)
		}
		writeSystemCredentials(t, `{not valid json`)
		_, stderr := run(1, "system", "import")
		if !strings.Contains(stderr, "not valid JSON") {
			t.Fatalf("expected a clear \"not valid JSON\" error, got %q", stderr)
		}
		after, err := s.read()
		if err != nil {
			t.Fatal(err)
		}
		if len(after.Accounts) != len(before.Accounts) {
			t.Fatalf("malformed system credentials must not create an account: before %+v after %+v", before.Accounts, after.Accounts)
		}
	})

	t.Run("malformed but valid-JSON system credentials (not a logged-in session)", func(t *testing.T) {
		writeSystemCredentials(t, `{"loggedIn":false,"authMethod":"none"}`)
		_, stderr := run(1, "system", "import")
		if !strings.Contains(stderr, "not a valid, logged-in claude.ai session") {
			t.Fatalf("expected a clear error, got %q", stderr)
		}
	})

	t.Run("no credential values appear in output or errors", func(t *testing.T) {
		const secret = "super-secret-oauth-token-xyz"
		login("secret-check@example.com", secret)
		outcomes := [][]string{
			{"system", "export", "secret-check@example.com"},
			{"system", "export", "never-registered@example.com"}, // error path
		}
		writeSystemCredentials(t, fmt.Sprintf(`{"loggedIn":true,"email":"secret-check@example.com","authMethod":"claude.ai","claudeAiOauth":{"accessToken":%q}}`, secret))
		outcomes = append(outcomes, []string{"system", "import"})
		writeSystemCredentials(t, `{not valid json`)
		outcomes = append(outcomes, []string{"system", "import"}) // error path
		for _, args := range outcomes {
			cmd := exec.Command(bin, args...)
			cmd.Env = os.Environ()
			var out, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &out, &stderr
			_ = cmd.Run()
			if strings.Contains(out.String(), secret) || strings.Contains(stderr.String(), secret) {
				t.Fatalf("%q leaked the credential value: stdout=%q stderr=%q", args, out.String(), stderr.String())
			}
		}
	})
}

// TestSystemUI covers cpro system's two interactive screens through a real
// pseudoterminal: the root picker's "System credentials" submenu (rootui.go)
// and cpro system export's own account picker (system.go), reached when
// export is run with no EMAIL. Presence-only assertions throughout, per
// TestRootPicker's own doc comment: bubbletea's diffing renderer means a
// same-screen or cross-screen redraw may not retransmit unchanged text, so
// checking for the ABSENCE of something from an earlier frame in a
// cumulative pty capture is unreliable — this only ever checks that
// expected content showed up somewhere, and verifies an actual state change
// (a file written, an account registered) rather than re-parsing text for it.
func TestSystemUI(t *testing.T) {
	bin, s := buildCLI(t)
	systemDir := t.TempDir()

	login := func(email string) {
		t.Helper()
		stage := t.TempDir()
		data := fmt.Sprintf(`{"loggedIn":true,"email":%q,"authMethod":"claude.ai"}`, email)
		if err := os.WriteFile(filepath.Join(stage, ".credentials.json"), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(stage, ".claude.json"), []byte(`{}`), 0600); err != nil {
			t.Fatal(err)
		}
		if err := s.installLogin(email, stage); err != nil {
			t.Fatal(err)
		}
	}

	drive := func(t *testing.T, env, args []string, steps func(master *os.File, capture func() string)) string {
		t.Helper()
		master, slave := openPTY(t)
		capture := drainPTY(master)
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.Env = append(append(os.Environ(), "TERM=xterm-256color"), env...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(500 * time.Millisecond)
		steps(master, capture)
		if err := cmd.Wait(); err != nil {
			t.Fatalf("%v: %v; output %q", args, err, stripANSI(capture()))
		}
		return stripANSI(capture())
	}
	send := func(master *os.File, s string) {
		if _, err := master.WriteString(s); err != nil {
			t.Fatal(err)
		}
		time.Sleep(150 * time.Millisecond)
	}
	down := func(master *os.File) { send(master, "\x1b[B") }
	enter := func(master *os.File) { send(master, "\r") }
	esc := func(master *os.File) { send(master, "\x1b") }

	t.Run("system submenu renders and Esc backs out to the root list", func(t *testing.T) {
		// Not the shared drive() helper: this ends in a cancelling Esc
		// sequence, which exits 1 by cpro's own convention (see TestEscGuard)
		// — a successful run of this check, not a failure to fatal on.
		master, slave := openPTY(t)
		capture := drainPTY(master)
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, "menu") // "system" only exists in the full palette
		cmd.Env = append(os.Environ(), "TERM=xterm-256color")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(500 * time.Millisecond)
		for range 6 { // config -> default -> login -> logout -> remove -> session -> system
			down(master)
		}
		enter(master) // open the submenu
		esc(master)   // single Esc: submenu -> root list (not a cancelled pick)
		esc(master)   // root list's own double-Esc: arm exit
		esc(master)   // confirm exit
		if err := cmd.Wait(); err != nil {
			if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 1 {
				t.Fatalf("expected a clean exit 1 from the cancelling Esc sequence, got %v", err)
			}
		}
		out := stripANSI(capture())
		for _, want := range []string{"System credentials", "export", "import", "Send account to system", "Add system account to cpro"} {
			if !strings.Contains(out, want) {
				t.Fatalf("submenu missing %q, got %q", want, out)
			}
		}
	})

	t.Run("selecting export from the submenu runs cpro system export", func(t *testing.T) {
		login("submenu-export@example.com")
		out := drive(t, []string{"CLAUDE_CONFIG_DIR=" + systemDir}, []string{"menu"}, func(master *os.File, capture func() string) {
			for range 6 { // config -> default -> login -> logout -> remove -> session -> system
				down(master)
			}
			enter(master) // open the submenu
			enter(master) // "export" is first — pick it
			time.Sleep(300 * time.Millisecond)
			enter(master) // the account picker's own screen: only one account, select it
		})
		if !strings.Contains(out, "Credentials exported") {
			t.Fatalf("expected the submenu's \"export\" to actually run cpro system export, got %q", out)
		}
		if _, err := os.ReadFile(filepath.Join(systemDir, ".credentials.json")); err != nil {
			t.Fatalf("expected the system credential store to have been written: %v", err)
		}
	})

	t.Run("export without an account opens the picker; selecting exports immediately", func(t *testing.T) {
		if err := os.Remove(filepath.Join(systemDir, ".credentials.json")); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		login("picker-export@example.com")
		out := drive(t, []string{"CLAUDE_CONFIG_DIR=" + systemDir}, []string{"system", "export"}, func(master *os.File, capture func() string) {
			enter(master) // the picker's own account list; select the only entry
		})
		for _, want := range []string{"SELECT ACCOUNT", "picker-export@example.com", "Export", "Cancel"} {
			if !strings.Contains(out, want) {
				t.Fatalf("export account picker missing %q, got %q", want, out)
			}
		}
		got, err := os.ReadFile(filepath.Join(systemDir, ".credentials.json"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(got), "picker-export@example.com") {
			t.Fatalf("expected the picked account's credentials to be exported, got %s", got)
		}
	})

	t.Run("Esc in the export picker cancels without exporting", func(t *testing.T) {
		if err := os.Remove(filepath.Join(systemDir, ".credentials.json")); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		login("picker-cancel@example.com")
		master, slave := openPTY(t)
		capture := drainPTY(master)
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, "system", "export")
		cmd.Env = append(append(os.Environ(), "TERM=xterm-256color"), "CLAUDE_CONFIG_DIR="+systemDir)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(500 * time.Millisecond)
		esc(master)
		err := cmd.Wait()
		if err == nil {
			t.Fatal("expected a cancelled export picker to exit non-zero")
		}
		if strings.Contains(stripANSI(capture()), "ERROR") {
			t.Fatalf("cancelling isn't an error, should not show an ERROR box: %q", stripANSI(capture()))
		}
		if _, err := os.Stat(filepath.Join(systemDir, ".credentials.json")); !os.IsNotExist(err) {
			t.Fatalf("expected no system credentials file after cancelling, got err=%v", err)
		}
	})
}

// TestClaudeDesktopCredentialTarget covers decision 0028: `cpro system
// export`'s new per-client reporting, and that Claude Desktop's own
// account session is only ever detected and reported on, never fabricated
// or written to — see desktop.go's own doc comments for the investigation
// (Desktop's top-level session is a separate, Electron-safeStorage-encrypted
// OAuth token issued to a different OAuth client than Claude Code's CLI).
// TestBuildCLIIsolatesCredentialStores is a guard, not a feature test: it
// asserts the test harness itself can never aim a credential write at the
// developer's own account. cpro is developed under cpro, so the suite
// inherits CLAUDE_CONFIG_DIR pointing at a real, logged-in profile, and
// `cpro system export` writes credentials to whatever systemConfigPaths
// resolves — which silently signed the developer out on every full test run
// until buildCLI started isolating it. Both failure shapes are covered: the
// inherited real profile, and the unset case, which falls back to the
// machine's real ~/.claude instead.
func TestBuildCLIIsolatesCredentialStores(t *testing.T) {
	buildCLI(t)

	dir := os.Getenv("CLAUDE_CONFIG_DIR")
	if dir == "" {
		t.Fatal("CLAUDE_CONFIG_DIR must be pointed at a throwaway directory, not cleared: systemConfigPaths falls back to the real ~/.claude when it is unset")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(dir, filepath.Join(home, ".config", "cpro")) {
		t.Fatalf("CLAUDE_CONFIG_DIR still points inside the real cpro config directory (%q) — a system export in any test would overwrite a live account's credentials", dir)
	}
	claudeJSON, credentials, err := systemConfigPaths()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{claudeJSON, credentials} {
		if strings.HasPrefix(p, filepath.Join(home, ".claude")) || p == filepath.Join(home, ".claude.json") {
			t.Fatalf("systemConfigPaths resolves to the machine's real credential store (%q)", p)
		}
		if !strings.HasPrefix(p, os.TempDir()) && !strings.Contains(p, "/T/") {
			t.Logf("note: system store resolves to %q — ensure it is a per-test temp path", p)
		}
	}
}

func TestClaudeDesktopCredentialTarget(t *testing.T) {
	bin, s := buildCLI(t)

	login := func(email string) {
		t.Helper()
		stage := t.TempDir()
		data := fmt.Sprintf(`{"loggedIn":true,"email":%q,"authMethod":"claude.ai"}`, email)
		if err := os.WriteFile(filepath.Join(stage, ".credentials.json"), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(stage, ".claude.json"), []byte(`{}`), 0600); err != nil {
			t.Fatal(err)
		}
		if err := s.installLogin(email, stage); err != nil {
			t.Fatal(err)
		}
	}
	login("desktop-target@example.com")

	desktopConfigDir := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "Claude")

	t.Run("not installed: reported as such, no sign-in-required text", func(t *testing.T) {
		cmd := exec.Command(bin, "system", "export", "desktop-target@example.com")
		cmd.Env = append(os.Environ(), "CPRO_TEST_NO_CLAUDE_DESKTOP=1")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("cpro system export: %s %v", out, err)
		}
		got := string(out)
		if !strings.Contains(got, "Claude Code") || !strings.Contains(got, "Updated") {
			t.Fatalf("expected Claude Code reported as Updated, got %q", got)
		}
		if !strings.Contains(got, "Claude Desktop") || !strings.Contains(got, "Not installed") {
			t.Fatalf("expected Claude Desktop reported as Not installed, got %q", got)
		}
		if strings.Contains(got, "Sign-in required") {
			t.Fatalf("a target reported Not installed must not also show the Sign-in required detail: %q", got)
		}
	})

	t.Run("installed (via its own config file): sign-in required, never fabricated as Updated", func(t *testing.T) {
		if err := os.MkdirAll(desktopConfigDir, 0700); err != nil {
			t.Fatal(err)
		}
		// A config file Desktop itself would only ever write — cpro must never
		// touch it, only detect its presence.
		before := []byte(`{"oauth:tokenCache":"unrelated-desktop-own-value"}`)
		configPath := filepath.Join(desktopConfigDir, "config.json")
		if err := os.WriteFile(configPath, before, 0600); err != nil {
			t.Fatal(err)
		}

		cmd := exec.Command(bin, "system", "export", "desktop-target@example.com")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("cpro system export: %s %v", out, err)
		}
		got := string(out)
		if !strings.Contains(got, "Claude Desktop") || !strings.Contains(got, "Sign-in required") {
			t.Fatalf("expected Claude Desktop reported as Sign-in required once detected as installed, got %q", got)
		}
		for _, line := range strings.Split(got, "\n") {
			if strings.HasPrefix(line, "Claude Desktop") && strings.Contains(line, "●") {
				t.Fatalf("Claude Desktop's own row must never render the filled/Updated dot: %q", line)
			}
		}

		// Desktop's own config file must be byte-for-byte untouched — cpro
		// never writes to it, only reads its existence.
		after, err := os.ReadFile(configPath)
		if err != nil {
			t.Fatal(err)
		}
		if string(after) != string(before) {
			t.Fatalf("Claude Desktop's own config file must never be modified by cpro system export; got %q, want %q", after, before)
		}

		// No secret-shaped content (the fake Desktop token value above, or the
		// exported account's own credential contents) ever appears in cpro's
		// own printed output.
		if strings.Contains(got, "unrelated-desktop-own-value") {
			t.Fatalf("cpro must never echo Claude Desktop's own stored values: %q", got)
		}
	})
}

// TestSessionContinue covers "cpro session continue" (session.go): copying a
// directory's Claude session transcripts from one cpro account to another and
// handing off into `claude --resume` under the target account.
func TestSessionContinue(t *testing.T) {
	bin, s := buildCLI(t)

	login := func(email string) {
		t.Helper()
		stage := t.TempDir()
		data := fmt.Sprintf(`{"loggedIn":true,"email":%q,"authMethod":"claude.ai"}`, email)
		if err := os.WriteFile(filepath.Join(stage, ".credentials.json"), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(stage, ".claude.json"), []byte(`{}`), 0600); err != nil {
			t.Fatal(err)
		}
		if err := s.installLogin(email, stage); err != nil {
			t.Fatal(err)
		}
	}
	login("session-from@example.com")
	login("session-to@example.com")

	projectDir := t.TempDir()
	dirName := projectDirName(projectDir)
	srcSessions := filepath.Join(s.profile("session-from@example.com"), "projects", dirName)
	if err := os.MkdirAll(srcSessions, 0700); err != nil {
		t.Fatal(err)
	}
	const sessionID = "11111111-1111-1111-1111-111111111111"
	if err := os.WriteFile(filepath.Join(srcSessions, sessionID+".jsonl"), []byte(`{"type":"user"}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}

	run := func(t *testing.T, wantCode int, dir string, args ...string) (string, string) {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Dir = dir
		cmd.Env = os.Environ()
		var out, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &stderr
		err := cmd.Run()
		got := 0
		if err != nil {
			if e, ok := err.(*exec.ExitError); ok {
				got = e.ExitCode()
			} else {
				t.Fatal(err)
			}
		}
		if got != wantCode {
			t.Fatalf("%q: exit %d, want %d; stdout %s stderr %s", args, got, wantCode, &out, &stderr)
		}
		return out.String(), stderr.String()
	}

	t.Run("missing accounts are rejected", func(t *testing.T) {
		_, stderr := run(t, 1, projectDir, "session", "continue", "nobody@example.com", "session-to@example.com")
		if !strings.Contains(stderr, "not registered") {
			t.Fatalf("expected a missing-account error, got %q", stderr)
		}
	})

	t.Run("no session history for this directory", func(t *testing.T) {
		other := t.TempDir()
		_, stderr := run(t, 1, other, "session", "continue", "session-from@example.com", "session-to@example.com")
		if !strings.Contains(stderr, "no Claude session history") {
			t.Fatalf("expected a no-history error, got %q", stderr)
		}
	})

	t.Run("copies transcripts and resumes under the target account", func(t *testing.T) {
		out, _ := run(t, 0, projectDir, "session", "continue", "session-from@example.com", "session-to@example.com")
		lines := strings.SplitN(out, "\n", 2)
		if !strings.Contains(lines[0], "Copied 1 session file(s) to session-to@example.com") {
			t.Fatalf("expected a copy confirmation, got %q", out)
		}
		if len(lines) < 2 || !strings.Contains(lines[1], `"--resume"`) {
			t.Fatalf("expected claude to be invoked with --resume, got %q", out)
		}

		dst := filepath.Join(s.profile("session-to@example.com"), "projects", dirName, sessionID+".jsonl")
		if _, err := os.Stat(dst); err != nil {
			t.Fatalf("expected the transcript copied into the target account's profile: %v", err)
		}
		src, err := os.ReadFile(filepath.Join(srcSessions, sessionID+".jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(srcSessions, sessionID+".jsonl")); err != nil {
			t.Fatalf("the source transcript must not be removed: %v", err)
		}
		got, err := os.ReadFile(dst)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(src) {
			t.Fatalf("copied transcript does not match the source: got %q want %q", got, src)
		}
	})

	t.Run("re-running does not overwrite or duplicate the copy", func(t *testing.T) {
		out, _ := run(t, 0, projectDir, "session", "continue", "session-from@example.com", "session-to@example.com")
		if !strings.Contains(out, "Copied 0 session file(s)") {
			t.Fatalf("expected nothing new to copy on a second run, got %q", out)
		}
	})
}

// TestCommandTreeAudit is decision 0025's own explicit "does the menu leave
// anything important inaccessible" check: every real, available top-level
// command must be either picker-reachable (present in rootPickerMeta) or a
// deliberate, named exception. A new top-level command that forgets to also
// add itself to rootPickerMeta fails this test instead of silently staying
// unreachable from cpro menu.
func TestCommandTreeAudit(t *testing.T) {
	root := rootCommand()
	root.InitDefaultHelpCmd()
	root.InitDefaultCompletionCmd()

	// "list" (decision 0010) is the one deliberate exception: a real, fully
	// working direct command that stays picker-absent on purpose, since
	// "status" already covers the interactive/at-a-glance use case a picker
	// serves.
	deliberateExceptions := map[string]bool{"list": true}

	inMeta := map[string]bool{}
	for _, meta := range rootPickerMeta {
		inMeta[meta.name] = true
	}
	for _, c := range root.Commands() {
		if !c.IsAvailableCommand() {
			continue
		}
		name := c.Name()
		if !inMeta[name] && !deliberateExceptions[name] {
			t.Fatalf("%q is a registered top-level command but missing from rootPickerMeta (add it, or record it as a deliberate, named exception)", name)
		}
	}
}

// TestSessionList covers `cpro session list` (decision 0044): the CLI
// counterpart of the SESSIONS LIST screen, added so the picker no longer
// performs an operation with no command behind it. Reads the same
// listSessions data; plain text and --json.
func TestSessionList(t *testing.T) {
	bin, s := buildCLI(t)
	login := func(email string) {
		t.Helper()
		stage := t.TempDir()
		data := fmt.Sprintf(`{"loggedIn":true,"email":%q,"authMethod":"claude.ai"}`, email)
		if err := os.WriteFile(filepath.Join(stage, ".credentials.json"), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(stage, ".claude.json"), []byte(`{}`), 0600); err != nil {
			t.Fatal(err)
		}
		if err := s.installLogin(email, stage); err != nil {
			t.Fatal(err)
		}
	}
	seed := func(email, cwd, sessionID string, age time.Duration) {
		t.Helper()
		dir := filepath.Join(s.profile(email), "projects", projectDirName(cwd))
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, sessionID+".jsonl")
		if err := os.WriteFile(path, []byte("{}\n"), 0600); err != nil {
			t.Fatal(err)
		}
		when := time.Now().Add(-age)
		if err := os.Chtimes(path, when, when); err != nil {
			t.Fatal(err)
		}
	}
	run := func(t *testing.T, wantCode int, args ...string) string {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Env = os.Environ()
		out, err := cmd.Output()
		got := 0
		if err != nil {
			if e, ok := err.(*exec.ExitError); ok {
				got = e.ExitCode()
			} else {
				t.Fatal(err)
			}
		}
		if got != wantCode {
			t.Fatalf("%q: exit %d, want %d; out %s", args, got, wantCode, out)
		}
		return string(out)
	}

	t.Run("no sessions: a clear, non-error message", func(t *testing.T) {
		if out := run(t, 0, "session", "list"); !strings.Contains(out, "No sessions found") {
			t.Fatalf("expected an empty-list message, got %q", out)
		}
	})

	login("list-a@example.com")
	login("list-b@example.com")
	const newID = "11111111-1111-4111-8111-111111111111"
	const oldID = "22222222-2222-4222-8222-222222222222"
	seed("list-a@example.com", "/home/user/projects/alpha", oldID, 2*time.Hour)
	seed("list-b@example.com", "/home/user/projects/beta", newID, 3*time.Minute)

	t.Run("plain output is one line per session, newest first, with the full ID", func(t *testing.T) {
		lines := strings.Split(strings.TrimSpace(run(t, 0, "session", "list")), "\n")
		if len(lines) != 2 {
			t.Fatalf("expected 2 session lines, got %d: %v", len(lines), lines)
		}
		if !strings.Contains(lines[0], newID) || !strings.Contains(lines[0], "/home/user/projects/beta") || !strings.Contains(lines[0], "list-b@example.com") {
			t.Fatalf("expected the newest session first with its full ID/path/account, got %q", lines[0])
		}
		if !strings.Contains(lines[1], oldID) {
			t.Fatalf("expected the older session second, got %q", lines[1])
		}
		if strings.Contains(lines[0], shortSessionID(newID)) && !strings.Contains(lines[0], newID) {
			t.Fatalf("expected the untruncated session ID, got %q", lines[0])
		}
	})

	t.Run("--json carries the same data in machine form", func(t *testing.T) {
		var got struct {
			Version  int `json:"version"`
			Sessions []struct {
				SessionID    string    `json:"sessionId"`
				Account      string    `json:"account"`
				Project      string    `json:"project"`
				Directory    string    `json:"directory"`
				LastActivity time.Time `json:"lastActivity"`
			} `json:"sessions"`
		}
		if err := json.Unmarshal([]byte(run(t, 0, "session", "list", "--json")), &got); err != nil {
			t.Fatal(err)
		}
		if got.Version != 1 || len(got.Sessions) != 2 {
			t.Fatalf("unexpected JSON shape: %+v", got)
		}
		if got.Sessions[0].SessionID != newID || got.Sessions[0].Account != "list-b@example.com" ||
			got.Sessions[0].Project != "beta" || got.Sessions[0].Directory != "/home/user/projects/beta" {
			t.Fatalf("unexpected newest session: %+v", got.Sessions[0])
		}
	})

	t.Run("masking hides the real account in plain output but never in --json", func(t *testing.T) {
		if err := s.update(func(c *config) error {
			c.MaskEmail = true
			c.EmailMasks = map[string]string{"list-b@example.com": "placeholder-fox"}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			maskEmailEnabled, emailMaskTable = false, nil
		})
		maskEmailEnabled, emailMaskTable = true, map[string]string{"list-b@example.com": "placeholder-fox"}

		out := run(t, 0, "session", "list")
		if strings.Contains(out, "list-b@example.com") || !strings.Contains(out, "placeholder-fox") {
			t.Fatalf("expected the alias in plain output, got %q", out)
		}
		if j := run(t, 0, "session", "list", "--json"); !strings.Contains(j, "list-b@example.com") {
			t.Fatalf("expected --json to stay unmasked, got %q", j)
		}
	})
}

// TestSessionIDFromCmdline pins the one exact way a live process says which
// conversation it is writing (session.go): both spellings of every resume flag,
// both `--flag value` and `--flag=value`, and no answer at all when nothing
// names one — the case that falls back to "newest transcript in its directory".
func TestSessionIDFromCmdline(t *testing.T) {
	const id = "26074c99-80ca-4211-9c87-4510d79a27da"
	for _, argv := range [][]string{
		{"claude", "--resume", id},
		{"claude", "-r", id},
		{"claude", "--session-id", id},
		{"claude", "--resume=" + id},
		{"claude", "--permission-mode", "bypassPermissions", "--resume", id},
	} {
		if got := sessionIDFromCmdline(argv, "--resume", "-r", "--session-id"); got != id {
			t.Fatalf("argv %q: got %q, want %s", argv, got, id)
		}
	}
	for _, argv := range [][]string{
		{"claude"},
		{"claude", "--resume"},
		{"claude", "--continue"},
		{"claude", "--resume", ""},
	} {
		if got := sessionIDFromCmdline(argv, "--resume", "-r", "--session-id"); got != "" {
			t.Fatalf("argv %q: got %q, want no id", argv, got)
		}
	}
}

// liveSessionProcess spawns a real, long-lived process that looks to /proc
// exactly like a live Claude session under this account — CLAUDE_CONFIG_DIR in
// its environment and `--resume <id>` in its command line, the two things
// liveProcessesForProfile and sessionIDFromCmdline read. It runs no claude and
// writes no transcript; only the detection is under test. Killed with its whole
// process group on cleanup, so a stray helper can never linger and be mistaken
// for a live session by a later test.
func liveSessionProcess(t *testing.T, s *store, email, sessionID string) {
	t.Helper()
	// A trailing ":" keeps the shell from exec-replacing itself with sleep (which
	// would discard the fake argv this test depends on).
	cmd := exec.Command("/bin/sh", "-c", "sleep 300; :", "claude", "--permission-mode", "bypassPermissions", "--resume", sessionID)
	cmd.Env = append(os.Environ(), "CLAUDE_CONFIG_DIR="+s.profile(email))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})
	// Wait for the process to actually exist before its /proc is read: Start
	// only guarantees the fork, and the scan is the very next thing this test
	// does.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(liveProcessesForProfile(s.profile(email))) > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the fake live session never became visible in /proc")
}

// TestSessionDelete covers decision 0051's core in isolation: resolving a
// selector to exactly one recorded session (full ID, unique prefix, account
// filter, and both failure modes — nothing matches, and the same ID recorded
// under two accounts), the deletion itself, and the retype guard's own
// comparison.
func TestSessionDelete(t *testing.T) {
	s := &store{dir: t.TempDir()}
	login := func(email string) {
		t.Helper()
		stage := t.TempDir()
		data := fmt.Sprintf(`{"loggedIn":true,"email":%q,"authMethod":"claude.ai"}`, email)
		if err := os.WriteFile(filepath.Join(stage, ".credentials.json"), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(stage, ".claude.json"), []byte(`{}`), 0600); err != nil {
			t.Fatal(err)
		}
		if err := s.installLogin(email, stage); err != nil {
			t.Fatal(err)
		}
	}
	seed := func(email, cwd, sessionID string) string {
		t.Helper()
		dir := filepath.Join(s.profile(email), "projects", projectDirName(cwd))
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, sessionID+".jsonl")
		if err := os.WriteFile(path, []byte("{}\n"), 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	const (
		idA      = "26074c99-80ca-4211-9c87-4510d79a27da"
		idB      = "a93fe208-0000-4000-8000-000000000000"
		idShared = "55555555-5555-4555-8555-555555555555"
	)
	login("del-a@example.com")
	login("del-b@example.com")
	pathA := seed("del-a@example.com", "/home/user/projects/alpha", idA)
	seed("del-b@example.com", "/home/user/projects/beta", idB)
	seed("del-a@example.com", "/home/user/projects/alpha", idShared)
	seed("del-b@example.com", "/home/user/projects/beta", idShared)

	c, err := s.read()
	if err != nil {
		t.Fatal(err)
	}

	t.Run("resolves a full ID and a unique prefix, scoped by account when given", func(t *testing.T) {
		got, err := resolveSession(s, c, idA, "")
		if err != nil || got.sessionID != idA || got.email != "del-a@example.com" {
			t.Fatalf("full ID: got %+v, err %v", got, err)
		}
		got, err = resolveSession(s, c, idA[:8], "")
		if err != nil || got.sessionID != idA {
			t.Fatalf("unique prefix: got %+v, err %v", got, err)
		}
		got, err = resolveSession(s, c, idB, "del-b@example.com")
		if err != nil || got.email != "del-b@example.com" {
			t.Fatalf("account-scoped: got %+v, err %v", got, err)
		}
	})

	t.Run("no match is an error, never an empty delete", func(t *testing.T) {
		if _, err := resolveSession(s, c, "does-not-exist", ""); err == nil || !strings.Contains(err.Error(), "no recorded session matches") {
			t.Fatalf("expected a clear no-match error, got %v", err)
		}
		if _, err := resolveSession(s, c, idB, "del-a@example.com"); err == nil {
			t.Fatal("expected an account filter that excludes the only match to error")
		}
	})

	t.Run("the same ID under two accounts is ambiguous unless the account picks one", func(t *testing.T) {
		if _, err := resolveSession(s, c, idShared, ""); err == nil || !strings.Contains(err.Error(), "matches 2 sessions") {
			t.Fatalf("expected an ambiguity error naming both candidates, got %v", err)
		}
		got, err := resolveSession(s, c, idShared, "del-b@example.com")
		if err != nil || got.email != "del-b@example.com" {
			t.Fatalf("an explicit account must disambiguate: got %+v, err %v", got, err)
		}
	})

	t.Run("deleteSession removes exactly that transcript, and reports an already-gone one", func(t *testing.T) {
		got, err := resolveSession(s, c, idA, "del-a@example.com")
		if err != nil {
			t.Fatal(err)
		}
		if err := deleteSession(s, got); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if _, err := os.Stat(pathA); !os.IsNotExist(err) {
			t.Fatalf("expected the transcript to be gone, stat err %v", err)
		}
		if _, err := os.Stat(filepath.Join(filepath.Dir(pathA), idShared+".jsonl")); err != nil {
			t.Fatalf("deleting one session must not touch a sibling: %v", err)
		}
		if err := deleteSession(s, got); err == nil || !strings.Contains(err.Error(), "already gone") {
			t.Fatalf("expected an already-gone error on a second delete, got %v", err)
		}
	})

	t.Run("the guard accepts the full ID or the displayed short form, nothing else", func(t *testing.T) {
		if !deleteConfirmMatches(idA, idA) {
			t.Fatal("the full ID must confirm")
		}
		if !deleteConfirmMatches(shortSessionID(idA), idA) {
			t.Fatal("the short form the list displays must confirm")
		}
		for _, bad := range []string{"", idA[:8], "yes", shortSessionID(idB)} {
			if deleteConfirmMatches(bad, idA) {
				t.Fatalf("%q must not confirm %s", bad, idA)
			}
		}
	})

	t.Run("an account-qualified selector picks that account's copy", func(t *testing.T) {
		got, err := resolveSessions(s, c, []string{"del-a@example.com:" + idShared}, "")
		if err != nil || len(got) != 1 || got[0].email != "del-a@example.com" {
			t.Fatalf("qualified selector: got %+v, err %v", got, err)
		}
	})

	t.Run("resolveSessions keeps order, collapses duplicates, and aborts on one bad selector", func(t *testing.T) {
		got, err := resolveSessions(s, c, []string{idB, idB, idB[:6]}, "del-b@example.com")
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].sessionID != idB {
			t.Fatalf("expected three selectors naming one session to collapse to it, got %+v", got)
		}
		if _, err := resolveSessions(s, c, []string{"del-a@example.com:does-not-exist"}, ""); err == nil {
			t.Fatal("expected one unknown selector to abort the whole call, not delete a subset")
		}
	})

	t.Run("deleteSessions removes a cross-account batch and reports a stale entry without aborting", func(t *testing.T) {
		a, err := resolveSession(s, c, idShared, "del-a@example.com")
		if err != nil {
			t.Fatal(err)
		}
		b, err := resolveSession(s, c, idShared, "del-b@example.com")
		if err != nil {
			t.Fatal(err)
		}
		stale := a
		stale.sessionID = "deadbeef-0000-4000-8000-000000000000"
		deleted, err := deleteSessions(s, []sessionEntry{stale, b})
		if err == nil || !strings.Contains(err.Error(), "already gone") {
			t.Fatalf("expected the stale entry reported, got %v", err)
		}
		if deleted != 1 {
			t.Fatalf("expected exactly the live entry deleted, got %d", deleted)
		}
		if _, statErr := os.Stat(filepath.Join(s.profile(b.email), "projects", b.dirName, b.sessionID+".jsonl")); !os.IsNotExist(statErr) {
			t.Fatalf("expected the live entry gone, stat err %v", statErr)
		}
	})

	t.Run("the bulk guard wants the word, case- and space-insensitively", func(t *testing.T) {
		for _, ok := range []string{"delete", "Delete", " delete "} {
			if !bulkDeleteConfirmMatches(ok) {
				t.Fatalf("%q should confirm a bulk delete", ok)
			}
		}
		for _, bad := range []string{"", "yes", "deletes", "delete all"} {
			if bulkDeleteConfirmMatches(bad) {
				t.Fatalf("%q must not confirm a bulk delete", bad)
			}
		}
	})

	t.Run("an account in use is skipped by name, and the free accounts still go", func(t *testing.T) {
		lockedPath := seed("del-a@example.com", "/home/user/projects/locked", "cccccccc-0000-4000-8000-00000000000a")
		freePath := seed("del-b@example.com", "/home/user/projects/free", "dddddddd-0000-4000-8000-00000000000b")
		locked, err := resolveSession(s, c, "cccccccc", "del-a@example.com")
		if err != nil {
			t.Fatal(err)
		}
		free, err := resolveSession(s, c, "dddddddd", "del-b@example.com")
		if err != nil {
			t.Fatal(err)
		}

		// An EXCLUSIVE lock is what a profile-wide operation (login/logout/
		// remove) holds, and it is the only thing that still blocks a delete: a
		// live session's own shared lock deliberately does not (decision 0053),
		// which is the collision this feature exists to remove. flock conflicts
		// across two open descriptions even within one process, so this
		// simulates "another profile-wide cpro operation is running under this
		// account" without spawning one.
		hold, err := s.accountLock("del-a@example.com", true)
		if err != nil {
			t.Fatal(err)
		}
		defer hold.Close()

		deleted, err := deleteSessions(s, []sessionEntry{locked, free})
		if err == nil {
			t.Fatal("expected the busy account reported, not a silent partial delete")
		}
		if !strings.Contains(err.Error(), "del-a@example.com") || !strings.Contains(err.Error(), "1 session(s) skipped") {
			t.Fatalf("expected the busy account named with what it cost, got %v", err)
		}
		// The lock this test holds is its own process's, so /proc can name it —
		// which is the whole point of the diagnostic: "wait for it" is useless
		// advice without saying what to wait for.
		if want := fmt.Sprintf("held by pid %d", os.Getpid()); !strings.Contains(err.Error(), want) {
			t.Fatalf("expected the holder named (%q), got %v", want, err)
		}
		if deleted != 1 {
			t.Fatalf("expected only the free account's session deleted, got %d", deleted)
		}
		if _, statErr := os.Stat(freePath); !os.IsNotExist(statErr) {
			t.Fatalf("the free account's session should be gone, stat err %v", statErr)
		}
		if _, statErr := os.Stat(lockedPath); statErr != nil {
			t.Fatalf("the busy account's session must be untouched, stat err %v", statErr)
		}
	})

	t.Run("a session a live Claude process is writing is refused by name, and the rest still go", func(t *testing.T) {
		livePath := seed("del-a@example.com", "/home/user/projects/live", "eeeeeeee-0000-4000-8000-00000000000e")
		idlePath := seed("del-b@example.com", "/home/user/projects/idle", "abababab-0000-4000-8000-0000000000ab")
		live, err := resolveSession(s, c, "eeeeeeee", "del-a@example.com")
		if err != nil {
			t.Fatal(err)
		}
		idle, err := resolveSession(s, c, "abababab", "del-b@example.com")
		if err != nil {
			t.Fatal(err)
		}
		liveSessionProcess(t, s, "del-a@example.com", live.sessionID)

		deletable, active := deletableSessions(s, c, []sessionEntry{live, idle})
		if len(active) != 1 || active[0].sessionID != live.sessionID {
			t.Fatalf("expected only the live session to count as active, got %+v (deletable %+v)", active, deletable)
		}
		if len(deletable) != 1 || deletable[0].sessionID != idle.sessionID {
			t.Fatalf("expected only the idle session offered for deletion, got %+v", deletable)
		}

		deleted, err := deleteSessions(s, []sessionEntry{live, idle})
		if err == nil || !strings.Contains(err.Error(), shortSessionID(live.sessionID)) ||
			!strings.Contains(err.Error(), "in use by a running Claude session") {
			t.Fatalf("expected the live session refused by name, got %v", err)
		}
		if deleted != 1 {
			t.Fatalf("expected only the idle session deleted, got %d", deleted)
		}
		if _, statErr := os.Stat(livePath); statErr != nil {
			t.Fatalf("the live session's transcript must be untouched: %v", statErr)
		}
		if _, statErr := os.Stat(idlePath); !os.IsNotExist(statErr) {
			t.Fatalf("the idle session should be gone, stat err %v", statErr)
		}
	})

	t.Run("a live session's shared lock still lets an idle session under that account be deleted", func(t *testing.T) {
		deadPath := seed("del-a@example.com", "/home/user/projects/dead", "cafebabe-0000-4000-8000-0000000000ca")
		dead, err := resolveSession(s, c, "cafebabe", "del-a@example.com")
		if err != nil {
			t.Fatal(err)
		}
		// Exactly what store.run holds for the whole life of a live claude
		// session: the shared lock delete used to collide with ("profile or
		// configuration is in use" for every delete under a busy account).
		hold, err := s.accountLock("del-a@example.com", false)
		if err != nil {
			t.Fatal(err)
		}
		defer hold.Close()

		deleted, err := deleteSessions(s, []sessionEntry{dead})
		if err != nil || deleted != 1 {
			t.Fatalf("a shared holder must not block an idle session's delete: deleted %d, err %v", deleted, err)
		}
		if _, statErr := os.Stat(deadPath); !os.IsNotExist(statErr) {
			t.Fatalf("expected the idle session gone, stat err %v", statErr)
		}
	})
}

// TestSessionDeleteCLI covers `cpro session delete` as users and scripts
// actually invoke it (decision 0051): the non-interactive guard, --yes, the
// account filter, and the error paths. The in-app row is covered in
// TestSessionAppDirect.
func TestSessionDeleteCLI(t *testing.T) {
	bin, s := buildCLI(t)
	login := func(email string) {
		t.Helper()
		stage := t.TempDir()
		data := fmt.Sprintf(`{"loggedIn":true,"email":%q,"authMethod":"claude.ai"}`, email)
		if err := os.WriteFile(filepath.Join(stage, ".credentials.json"), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(stage, ".claude.json"), []byte(`{}`), 0600); err != nil {
			t.Fatal(err)
		}
		if err := s.installLogin(email, stage); err != nil {
			t.Fatal(err)
		}
	}
	seed := func(email, cwd, sessionID string) string {
		t.Helper()
		dir := filepath.Join(s.profile(email), "projects", projectDirName(cwd))
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, sessionID+".jsonl")
		if err := os.WriteFile(path, []byte("{}\n"), 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	run := func(t *testing.T, wantCode int, args ...string) (stdout, stderr string) {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Env = os.Environ()
		var out, errBuf strings.Builder
		cmd.Stdout, cmd.Stderr = &out, &errBuf
		err := cmd.Run()
		got := 0
		if err != nil {
			if e, ok := err.(*exec.ExitError); ok {
				got = e.ExitCode()
			} else {
				t.Fatal(err)
			}
		}
		if got != wantCode {
			t.Fatalf("%q: exit %d, want %d; out %q err %q", args, got, wantCode, out.String(), errBuf.String())
		}
		return out.String(), errBuf.String()
	}

	const (
		idA = "26074c99-80ca-4211-9c87-4510d79a27da"
		idB = "a93fe208-0000-4000-8000-000000000000"
	)
	login("cli-a@example.com")
	login("cli-b@example.com")
	pathA := seed("cli-a@example.com", "/home/user/projects/alpha", idA)
	pathB := seed("cli-b@example.com", "/home/user/projects/beta", idB)

	t.Run("no terminal and no --yes: refuses instead of deleting", func(t *testing.T) {
		_, stderr := run(t, 1, "session", "delete", idA)
		if !strings.Contains(stderr, "--yes") {
			t.Fatalf("expected the non-interactive refusal to name --yes, got %q", stderr)
		}
		if _, err := os.Stat(pathA); err != nil {
			t.Fatalf("a refused delete must not touch the transcript: %v", err)
		}
	})

	t.Run("--yes deletes the transcript and reports what it removed", func(t *testing.T) {
		stdout, _ := run(t, 0, "session", "delete", idA, "--yes")
		if !strings.Contains(stdout, "Deleted session") || !strings.Contains(stdout, "/home/user/projects/alpha") {
			t.Fatalf("expected a confirmation naming the project, got %q", stdout)
		}
		if _, err := os.Stat(pathA); !os.IsNotExist(err) {
			t.Fatalf("expected the transcript gone, stat err %v", err)
		}
	})

	t.Run("an unknown selector and a filtering --account both fail clearly", func(t *testing.T) {
		if _, stderr := run(t, 1, "session", "delete", "does-not-exist", "--yes"); !strings.Contains(stderr, "no recorded session matches") {
			t.Fatalf("expected a no-match error, got %q", stderr)
		}
		if _, stderr := run(t, 1, "session", "delete", idB, "--account", "cli-a@example.com", "--yes"); !strings.Contains(stderr, "no recorded session matches") {
			t.Fatalf("expected the account filter to exclude cli-b's session, got %q", stderr)
		}
		if _, stderr := run(t, 1, "session", "delete", idB, "--account", "nobody@example.com", "--yes"); !strings.Contains(stderr, "not registered") {
			t.Fatalf("expected an unregistered --account to fail like every other command, got %q", stderr)
		}
		if _, err := os.Stat(pathB); err != nil {
			t.Fatalf("no failing path may delete anything: %v", err)
		}
	})

	t.Run("--account deletes exactly that account's copy", func(t *testing.T) {
		run(t, 0, "session", "delete", idB, "--account", "cli-b@example.com", "--yes")
		if _, err := os.Stat(pathB); !os.IsNotExist(err) {
			t.Fatalf("expected cli-b's transcript gone, stat err %v", err)
		}
	})

	t.Run("several IDs in one invocation delete together and report the count", func(t *testing.T) {
		pA := seed("cli-a@example.com", "/home/user/projects/alpha", idA)
		pB := seed("cli-b@example.com", "/home/user/projects/beta", idB)
		stdout, _ := run(t, 0, "session", "delete", idA, idB, "--yes")
		if !strings.Contains(stdout, "Deleted 2 of 2 sessions") {
			t.Fatalf("expected a batch summary, got %q", stdout)
		}
		for _, p := range []string{pA, pB} {
			if _, err := os.Stat(p); !os.IsNotExist(err) {
				t.Fatalf("expected %s gone, stat err %v", p, err)
			}
		}
	})

	t.Run("account-qualified selectors delete a mixed-account batch a bare ID could not", func(t *testing.T) {
		pA := seed("cli-a@example.com", "/home/user/projects/alpha", idA)
		pB := seed("cli-b@example.com", "/home/user/projects/beta", idA)
		if _, stderr := run(t, 1, "session", "delete", idA, "--yes"); !strings.Contains(stderr, "matches 2 sessions") {
			t.Fatalf("expected the bare ID to be ambiguous once both accounts hold it, got %q", stderr)
		}
		stdout, _ := run(t, 0, "session", "delete", "cli-a@example.com:"+idA, "cli-b@example.com:"+idA, "--yes")
		if !strings.Contains(stdout, "Deleted 2 of 2 sessions") {
			t.Fatalf("expected both qualified copies deleted, got %q", stdout)
		}
		for _, p := range []string{pA, pB} {
			if _, err := os.Stat(p); !os.IsNotExist(err) {
				t.Fatalf("expected %s gone, stat err %v", p, err)
			}
		}
	})

	t.Run("several IDs without --yes still refuse non-interactively", func(t *testing.T) {
		seed("cli-a@example.com", "/home/user/projects/alpha", idA)
		seed("cli-b@example.com", "/home/user/projects/beta", idB)
		if _, stderr := run(t, 1, "session", "delete", idA, idB); !strings.Contains(stderr, "--yes") {
			t.Fatalf("expected the batch refusal to name --yes, got %q", stderr)
		}
	})

	t.Run("a live session is refused even with --yes, by name", func(t *testing.T) {
		const idLive = "beefbeef-0000-4000-8000-0000000000be"
		pathLive := seed("cli-a@example.com", "/home/user/projects/live", idLive)
		liveSessionProcess(t, s, "cli-a@example.com", idLive)

		stdout, stderr := run(t, 1, "session", "delete", idLive, "--yes")
		if !strings.Contains(stderr, "in use by a running Claude session") || !strings.Contains(stderr, shortSessionID(idLive)) {
			t.Fatalf("expected a refusal naming the live session, got stdout %q stderr %q", stdout, stderr)
		}
		if _, err := os.Stat(pathLive); err != nil {
			t.Fatalf("a refused delete must not touch the transcript: %v", err)
		}
	})
}

// TestDeletePickerHidesActiveSessions is the picker half of decision 0053: the
// DELETE SESSION list must offer only what it can actually delete, so a live
// Claude session is filtered out (and counted, so the list explains itself)
// while every idle one stays. Detection is real — a spawned process with the
// account's CLAUDE_CONFIG_DIR and `--resume <id>` on its command line — not a
// stubbed active set, because the whole fix rests on that scan being right.
func TestDeletePickerHidesActiveSessions(t *testing.T) {
	s := &store{dir: t.TempDir()}
	stage := t.TempDir()
	data := `{"loggedIn":true,"email":"picker@example.com","authMethod":"claude.ai"}`
	if err := os.WriteFile(filepath.Join(stage, ".credentials.json"), []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, ".claude.json"), []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.installLogin("picker@example.com", stage); err != nil {
		t.Fatal(err)
	}
	const (
		idLive = "11111111-1111-4111-8111-111111111111"
		idIdle = "22222222-2222-4222-8222-222222222222"
	)
	for cwd, id := range map[string]string{"/home/user/projects/live": idLive, "/home/user/projects/idle": idIdle} {
		dir := filepath.Join(s.profile("picker@example.com"), "projects", projectDirName(cwd))
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte("{}\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	c, err := s.read()
	if err != nil {
		t.Fatal(err)
	}
	liveSessionProcess(t, s, "picker@example.com", idLive)

	m := &sessionApp{s: s, c: c, stack: newNavStack(screenSessionMenu)}
	m.openPicker("delete")
	if len(m.picker.items) != 1 || m.picker.items[0].sessionID != idIdle {
		t.Fatalf("expected only the idle session listed for deletion, got %+v", m.picker.items)
	}
	if m.hiddenActive != 1 {
		t.Fatalf("expected exactly one hidden active session, got %d", m.hiddenActive)
	}
	if view := m.viewPickerBrowse(); !strings.Contains(view, "in use by a running Claude session") {
		t.Fatalf("expected the picker to explain the session it left out, got %q", view)
	}

	// Every other mode keeps the whole list: only delete hides anything.
	m.openPicker("continue")
	if len(m.picker.items) != 2 || m.hiddenActive != 0 {
		t.Fatalf("expected continue to keep both sessions and hide none, got %d items, hidden %d", len(m.picker.items), m.hiddenActive)
	}
	// ...but marks the live one (decision 0064), and only that one.
	if view := m.viewPickerBrowse(); strings.Count(view, "● running") != 1 {
		t.Fatalf("expected exactly the live session tagged running in CONTINUE SESSION, got %q", view)
	}
}

// TestSessionAppDirect covers sessionApp's (sessionui.go) own logic directly —
// no pty — for the same reason TestRootPickerExitArmed/TestPermissionsPreviewTracksCursor
// already give: a same-screen state change (a search filtering the same
// list, a toggle-then-rerender) is exactly the class of diff bubbletea's
// renderer may not retransmit in full, which a cumulative pty capture can't
// reliably prove either way.
func TestSessionAppDirect(t *testing.T) {
	s := &store{dir: t.TempDir()}
	login := func(email string) {
		t.Helper()
		stage := t.TempDir()
		data := fmt.Sprintf(`{"loggedIn":true,"email":%q,"authMethod":"claude.ai"}`, email)
		if err := os.WriteFile(filepath.Join(stage, ".credentials.json"), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(stage, ".claude.json"), []byte(`{}`), 0600); err != nil {
			t.Fatal(err)
		}
		if err := s.installLogin(email, stage); err != nil {
			t.Fatal(err)
		}
	}
	login("session-a@example.com")
	login("session-b@example.com")

	seed := func(email, cwd, sessionID string) {
		t.Helper()
		dir := filepath.Join(s.profile(email), "projects", projectDirName(cwd))
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, sessionID+".jsonl"), []byte("{}\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	const fullIDA = "26074c99-80ca-4211-9c87-4510d79a27da"
	const fullIDB = "a93fe208-0000-4000-8000-000000000000"
	seed("session-a@example.com", "/home/user/projects/myproject", fullIDA)
	seed("session-b@example.com", "/home/user/projects/api", fullIDB)

	c, err := s.read()
	if err != nil {
		t.Fatal(err)
	}
	newModel := func(hasParent bool) *sessionApp {
		return &sessionApp{s: s, c: c, hasParent: hasParent, stack: newNavStack(screenSessionMenu)}
	}
	enterKey := tea.KeyPressMsg{Code: tea.KeyEnter}
	rightKey := tea.KeyPressMsg{Code: tea.KeyRight}
	escKey := tea.KeyPressMsg{Code: tea.KeyEsc}

	t.Run("SESSIONS menu: Enter opens continue (Right too, it's forward-navigable); Enter opens list (Right does not, it's a leaf row)", func(t *testing.T) {
		m := newModel(true)
		m.updateSessionMenu(enterKey)
		if *m.stack.current() != screenContinuePicker {
			t.Fatalf("expected continue to push screenContinuePicker, got %v", *m.stack.current())
		}
		if len(m.picker.items) != 2 {
			t.Fatalf("expected both seeded sessions to populate the picker, got %d", len(m.picker.items))
		}
		m.stack.pop()

		m.cursor = 0
		m.updateSessionMenu(rightKey)
		if *m.stack.current() != screenContinuePicker {
			t.Fatalf("expected -> on continue to also push screenContinuePicker, got %v", *m.stack.current())
		}
		m.stack.pop()

		// "list" is a leaf row (isForwardSessionItem is false for it, matching
		// configApp's own convention that -> only ever acts on a forward row —
		// e.g. -> does nothing on config's own "mask" toggle either), so only
		// Enter opens it.
		m.cursor = 1
		m.updateSessionMenu(rightKey)
		if *m.stack.current() != screenSessionMenu {
			t.Fatal("expected -> on list (not forward-navigable) to do nothing")
		}
		m.updateSessionMenu(enterKey)
		if *m.stack.current() != screenSessionList {
			t.Fatalf("expected Enter on list to push screenSessionList, got %v", *m.stack.current())
		}
	})

	t.Run("list mode is read-only: Enter/Right on a row does nothing", func(t *testing.T) {
		m := newModel(true)
		m.openPicker("list")
		m.stack.push(screenSessionList)
		before := *m.stack.current()
		m.updatePickerBrowse(enterKey)
		if *m.stack.current() != before {
			t.Fatal("expected list mode's Enter to be a no-op")
		}
		if m.picked != nil {
			t.Fatal("expected list mode never to produce a picked argv")
		}
	})

	t.Run("continue mode: Enter opens the destination account picker, offering every account including the owner", func(t *testing.T) {
		m := newModel(true)
		m.openPicker("continue")
		m.stack.push(screenContinuePicker)
		owner := m.picker.items[0].email
		m.updatePickerBrowse(enterKey)
		if *m.stack.current() != screenContinueAccount {
			t.Fatalf("expected Enter to push screenContinueAccount, got %v", *m.stack.current())
		}
		want := []string{"session-a@example.com", "session-b@example.com"}
		if strings.Join(m.account.items, ",") != strings.Join(want, ",") {
			t.Fatalf("expected every account offered, owner included, got %v", m.account.items)
		}
		if note := m.continuingNote(); !strings.Contains(note, "from "+displayEmail(owner)) {
			t.Fatalf("expected the screen to name the session's owner, got %q", note)
		}
	})

	t.Run("Right Arrow on a session behaves the same as Enter", func(t *testing.T) {
		m := newModel(true)
		m.openPicker("continue")
		m.stack.push(screenContinuePicker)
		m.updatePickerBrowse(rightKey)
		if *m.stack.current() != screenContinueAccount {
			t.Fatal("expected -> to behave like Enter on a session row")
		}
	})

	t.Run("selecting a destination account builds the full, untruncated session ID into picked", func(t *testing.T) {
		m := newModel(true)
		m.openPicker("continue")
		m.stack.push(screenContinuePicker)
		chosen := m.picker.items[0]
		m.updatePickerBrowse(enterKey)
		_, cmd := m.updateAccountBrowse(enterKey)
		if cmd == nil {
			t.Fatal("expected finalizing to quit the program")
		}
		want := []string{"__resume", chosen.sessionID, "--account", m.account.items[0]}
		if len(m.picked) != len(want) {
			t.Fatalf("picked = %v, want %v", m.picked, want)
		}
		for i := range want {
			if m.picked[i] != want[i] {
				t.Fatalf("picked = %v, want %v", m.picked, want)
			}
		}
		if m.picked[1] != chosen.sessionID || shortSessionID(chosen.sessionID) == chosen.sessionID {
			t.Fatalf("expected the full session ID in picked, not a shortened one: %v", m.picked)
		}
	})

	t.Run("Esc at a nested frame pops one level, never exits", func(t *testing.T) {
		m := newModel(true)
		m.openPicker("continue")
		m.stack.push(screenContinuePicker)
		m.updatePickerBrowse(escKey)
		if *m.stack.current() != screenSessionMenu {
			t.Fatalf("expected Esc to pop back to SESSIONS, got %v", *m.stack.current())
		}
		if m.backOut {
			t.Fatal("a nested pop must never set backOut")
		}
	})

	t.Run("Esc at the stack's own root with a parent backs out to the picker", func(t *testing.T) {
		m := newModel(true)
		m.updateSessionMenu(escKey)
		if !m.backOut {
			t.Fatal("expected backOut with hasParent true")
		}
	})

	t.Run("Esc at the stack's own root with no parent arms then exits on the second press", func(t *testing.T) {
		m := newModel(false)
		m.updateSessionMenu(escKey)
		if !m.exitArmed || m.backOut {
			t.Fatal("expected the first Esc to arm exit without backing out")
		}
		_, cmd := m.updateSessionMenu(escKey)
		if cmd == nil {
			t.Fatal("expected the second consecutive Esc to quit")
		}
	})

	t.Run("typing filters sessions by project name, path, session ID, and account", func(t *testing.T) {
		m := newModel(true)
		m.openPicker("continue")
		m.stack.push(screenContinuePicker)
		for _, q := range []string{"myproject", "session-a", fullIDA[:8]} {
			m.picker.query = q
			m.picker.refilter()
			if len(m.picker.filtered) != 1 {
				t.Fatalf("query %q: expected exactly 1 match, got %d", q, len(m.picker.filtered))
			}
		}
		m.picker.query = "no-such-session"
		m.picker.refilter()
		if len(m.picker.filtered) != 0 {
			t.Fatalf("expected no matches for a nonsense query, got %d", len(m.picker.filtered))
		}
	})

	t.Run("mask emails hides the real address from rendering and search", func(t *testing.T) {
		t.Cleanup(func() { maskEmailEnabled, emailMaskTable = false, nil })
		maskEmailEnabled = true
		emailMaskTable = map[string]string{"session-a@example.com": "placeholder-fox"}
		m := newModel(true)
		m.openPicker("continue")
		m.stack.push(screenContinuePicker)
		out := m.viewPickerBrowse()
		if strings.Contains(out, "session-a@example.com") {
			t.Fatalf("expected the real email hidden while masking is on, got %q", out)
		}
		m.picker.query = "session-a@example.com"
		m.picker.refilter()
		if len(m.picker.filtered) != 0 {
			t.Fatal("expected searching by the real email to find nothing while masked")
		}
		m.picker.query = "placeholder-fox"
		m.picker.refilter()
		if len(m.picker.filtered) != 1 {
			t.Fatal("expected searching by the alias to find the session while masked")
		}
	})

	t.Run("delete mode: the row opens the retype guard, which only finalizes on a matching ID", func(t *testing.T) {
		m := newModel(true)
		m.cursor = 2
		m.updateSessionMenu(enterKey)
		if *m.stack.current() != screenSessionList || m.pickerMode != "delete" {
			t.Fatalf("expected the delete row to open the browse in delete mode, got screen %v mode %q", *m.stack.current(), m.pickerMode)
		}
		m.updatePickerBrowse(enterKey)
		if *m.stack.current() != screenDeleteConfirm {
			t.Fatalf("expected a chosen session to open the confirm frame, got %v", *m.stack.current())
		}
		if m.picked != nil {
			t.Fatalf("delete must not finalize before confirmation, got %v", m.picked)
		}
		if len(m.pendingDelete) != 1 {
			t.Fatalf("expected the one chosen session on the confirm frame, got %+v", m.pendingDelete)
		}
		chosen := m.pendingDelete[0]

		// A mismatch stays on screen and finalizes nothing.
		m.confirmInput = "not-the-id"
		m.updateDeleteConfirm(enterKey)
		if *m.stack.current() != screenDeleteConfirm || m.confirmErr == "" {
			t.Fatalf("expected a mismatch to stay on the guard with a message, got screen %v err %q", *m.stack.current(), m.confirmErr)
		}
		if m.picked != nil {
			t.Fatalf("a mismatch must not finalize, got %v", m.picked)
		}

		// Esc backs out without deleting anything.
		m.updateDeleteConfirm(escKey)
		if *m.stack.current() != screenSessionList || m.pendingDelete != nil {
			t.Fatalf("expected Esc to return to the list and drop the pending delete, got screen %v pending %+v", *m.stack.current(), m.pendingDelete)
		}

		// The short form the list displays confirms, and the argv names the
		// exact session through an account-qualified selector (decision 0052).
		m.updatePickerBrowse(enterKey)
		m.confirmInput = shortSessionID(chosen.sessionID)
		m.updateDeleteConfirm(enterKey)
		want := []string{"session", "delete", chosen.email + ":" + chosen.sessionID, "--yes"}
		if !reflect.DeepEqual(m.picked, want) {
			t.Fatalf("got picked=%v, want %v", m.picked, want)
		}
	})

	t.Run("delete mode: Space/Tab check rows and Ctrl+A checks every visible one", func(t *testing.T) {
		m := newModel(true)
		m.cursor = 2
		m.updateSessionMenu(enterKey)
		total := len(m.picker.items)
		if total != 2 {
			t.Fatalf("expected both seeded sessions in the delete picker, got %d", total)
		}
		spaceKey := tea.KeyPressMsg{Code: tea.KeySpace}
		tabKey := tea.KeyPressMsg{Code: tea.KeyTab}
		ctrlA := tea.KeyPressMsg{Code: 'a', Mod: tea.ModCtrl}

		m.updatePickerBrowse(spaceKey)
		if got := len(m.checkedDeleteEntries(false)); got != 1 {
			t.Fatalf("Space should check exactly the highlighted row, got %d checked", got)
		}
		m.updatePickerBrowse(spaceKey)
		if got := len(m.checkedDeleteEntries(false)); got != 0 {
			t.Fatalf("Space on an already-checked row should uncheck it in place, got %d checked", got)
		}

		// Tab walks as it checks, so repeated presses mark a run.
		m.updatePickerBrowse(tabKey)
		if got := len(m.checkedDeleteEntries(false)); got != 1 {
			t.Fatalf("Tab should check the highlighted row, got %d checked", got)
		}
		m.updatePickerBrowse(tabKey)
		if got := len(m.checkedDeleteEntries(false)); got != total {
			t.Fatalf("a second Tab should check the next row too, got %d of %d", got, total)
		}

		// Ctrl+A is a toggle: everything is already checked, so the first press
		// clears the set and the second restores it.
		m.updatePickerBrowse(ctrlA)
		if got := len(m.checkedDeleteEntries(false)); got != 0 {
			t.Fatalf("Ctrl+A on a fully checked list should clear it, got %d checked", got)
		}
		m.updatePickerBrowse(ctrlA)
		if got := len(m.checkedDeleteEntries(false)); got != total {
			t.Fatalf("a second Ctrl+A should check every visible row, got %d of %d", got, total)
		}

		// A checked batch needs the literal word, not one session ID.
		m.updatePickerBrowse(enterKey)
		if *m.stack.current() != screenDeleteConfirm {
			t.Fatalf("expected Enter to open the confirm frame, got %v", *m.stack.current())
		}
		if len(m.pendingDelete) != total {
			t.Fatalf("expected the whole checked batch on the confirm frame, got %+v", m.pendingDelete)
		}
		m.confirmInput = "nope"
		m.updateDeleteConfirm(enterKey)
		if m.confirmErr == "" || m.picked != nil {
			t.Fatalf("a non-%q answer must not finalize: err %q picked %v", bulkDeleteConfirmation, m.confirmErr, m.picked)
		}
		m.confirmInput = bulkDeleteConfirmation
		m.updateDeleteConfirm(enterKey)
		if len(m.picked) != total+3 || m.picked[0] != "session" || m.picked[1] != "delete" || m.picked[len(m.picked)-1] != "--yes" {
			t.Fatalf("expected session delete + %d qualified IDs + --yes, got %v", total, m.picked)
		}
		for _, e := range m.pendingDelete {
			found := false
			for _, a := range m.picked {
				if a == e.email+":"+e.sessionID {
					found = true
				}
			}
			if !found {
				t.Fatalf("expected the account-qualified selector %s:%s in %v", e.email, e.sessionID, m.picked)
			}
		}
	})
}

// TestSessionDeleteMultiSelectUI drives the real DELETE SESSION screen through
// a pty and checks the outcome on disk, which is the one thing a model-level
// test cannot see (decision 0052). It pins two things at once: Space must CHECK
// the highlighted row rather than open the search box, and the account-qualified
// argv the screen builds must actually delete the checked batch when the CLI
// re-runs it.
//
// The search box is the specific trap: every other browse list in cpro treats a
// printable character as "start typing to search" — Space included — so a
// multi-select whose interception is missing or misplaced silently becomes a
// search that filters every row out. Its header ("… DELETE SESSION: <query>_")
// is therefore asserted absent, not the raw diff stream: bubbletea's renderer
// emits only changed cells for a same-screen update, so grepping the capture for
// "[x]" would be checking a redraw artifact rather than the screen.
func TestSessionDeleteMultiSelectUI(t *testing.T) {
	bin, s := buildCLI(t)
	login := func(email string) {
		t.Helper()
		stage := t.TempDir()
		data := fmt.Sprintf(`{"loggedIn":true,"email":%q,"authMethod":"claude.ai"}`, email)
		if err := os.WriteFile(filepath.Join(stage, ".credentials.json"), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(stage, ".claude.json"), []byte(`{}`), 0600); err != nil {
			t.Fatal(err)
		}
		if err := s.installLogin(email, stage); err != nil {
			t.Fatal(err)
		}
	}
	seed := func(email, cwd, sessionID string) string {
		t.Helper()
		dir := filepath.Join(s.profile(email), "projects", projectDirName(cwd))
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, sessionID+".jsonl")
		if err := os.WriteFile(path, []byte("{}\n"), 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	login("multi@example.com")
	first := seed("multi@example.com", "/home/user/projects/alpha", "aaaa1111-1111-4111-8111-111111111111")
	second := seed("multi@example.com", "/home/user/projects/beta", "bbbb2222-2222-4222-8222-222222222222")

	master, slave := openPTY(t)
	capture := drainPTY(master)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	app := exec.CommandContext(ctx, bin, "session")
	app.Stdin, app.Stdout, app.Stderr = slave, slave, slave
	if err := app.Start(); err != nil {
		t.Fatal(err)
	}
	send := func(keys string, wait time.Duration) {
		t.Helper()
		if _, err := master.WriteString(keys); err != nil {
			t.Fatal(err)
		}
		time.Sleep(wait)
	}

	send("", 300*time.Millisecond)
	// SESSIONS menu: continue -> list -> delete (decision 0052's third row).
	send("\x1b[B", 100*time.Millisecond)
	send("\x1b[B", 100*time.Millisecond)
	send("\r", 300*time.Millisecond) // open DELETE SESSION
	if frame := stripANSI(capture()); !strings.Contains(frame, "[ ]") {
		t.Fatalf("expected unchecked checkboxes in DELETE SESSION, got:\n%s", frame)
	}

	// Space is the stay-put toggle, so it must check and uncheck the highlighted
	// row — never open the search box. Tabbing twice then marks a two-row batch
	// (Tab checks the highlighted row and advances).
	send(" ", 200*time.Millisecond)  // check row 0
	send(" ", 200*time.Millisecond)  // uncheck it again, back to a clean slate
	send("\t", 200*time.Millisecond) // check row 0, advance
	send("\t", 200*time.Millisecond) // check row 1, advance
	if frame := stripANSI(capture()); strings.Contains(frame, "DELETE SESSION:") {
		t.Fatalf("Space/Tab opened the search box instead of checking rows:\n%s", frame)
	}
	if frame := stripANSI(capture()); !strings.Contains(frame, "Space Check") {
		t.Fatalf("expected the multi-select footer, got:\n%s", frame)
	}

	send("\r", 300*time.Millisecond) // Enter: confirm the checked batch
	if frame := stripANSI(capture()); !strings.Contains(frame, "This permanently deletes") {
		t.Fatalf("expected the batch confirmation screen, got:\n%s", frame)
	}
	send(bulkDeleteConfirmation+"\r", 500*time.Millisecond) // type the word, then confirm

	if err := app.Wait(); err != nil {
		t.Fatalf("the in-app batch delete should hand the CLI a clean run: %v", err)
	}
	for _, path := range []string{first, second} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("expected %s deleted by the argv the screen built, stat err %v", path, err)
		}
	}
}

// TestSessionUI covers cpro session/cpro session continue end to end through

// TestSessionUI covers cpro session/cpro session continue end to end through
// a real pty — decision 0025's interactive session management. Navigation
// assertions use liveness/ground-truth checks (which screen actually opened,
// whether the process is still alive after a back-navigation) rather than
// cumulative-capture text greps for a same-screen change, matching
// TestNavigationHierarchy's/TestRootPicker's own documented discipline.
func TestSessionUI(t *testing.T) {
	bin, s := buildCLI(t)

	login := func(email string) {
		t.Helper()
		stage := t.TempDir()
		data := fmt.Sprintf(`{"loggedIn":true,"email":%q,"authMethod":"claude.ai"}`, email)
		if err := os.WriteFile(filepath.Join(stage, ".credentials.json"), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(stage, ".claude.json"), []byte(`{}`), 0600); err != nil {
			t.Fatal(err)
		}
		if err := s.installLogin(email, stage); err != nil {
			t.Fatal(err)
		}
	}
	login("ui-from@example.com")
	login("ui-to@example.com")

	projectDir := t.TempDir()
	dirName := projectDirName(projectDir)
	srcSessions := filepath.Join(s.profile("ui-from@example.com"), "projects", dirName)
	if err := os.MkdirAll(srcSessions, 0700); err != nil {
		t.Fatal(err)
	}
	const sessionID = "26074c99-80ca-4211-9c87-4510d79a27da"
	if err := os.WriteFile(filepath.Join(srcSessions, sessionID+".jsonl"), []byte(`{"type":"user"}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}

	send := func(master *os.File, str string) {
		if _, err := master.WriteString(str); err != nil {
			t.Fatal(err)
		}
		time.Sleep(150 * time.Millisecond)
	}
	down := func(master *os.File) { send(master, "\x1b[B") }
	right := func(master *os.File) { send(master, "\x1b[C") }
	esc := func(master *os.File) { send(master, "\x1b") }

	t.Run("bare cpro session opens the SESSIONS screen", func(t *testing.T) {
		master, slave := openPTY(t)
		capture := drainPTY(master)
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, "session")
		cmd.Env = append(os.Environ(), "TERM=xterm-256color")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(500 * time.Millisecond)
		out := stripANSI(capture())
		for _, want := range []string{"claude cpro - SESSIONS", "continue", "list", "Manage Claude sessions"} {
			if !strings.Contains(out, want) {
				t.Fatalf("expected %q, got %q", want, out)
			}
		}
		esc(master)
		esc(master)
		if err := cmd.Wait(); err != nil {
			if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 1 {
				t.Fatalf("expected a clean exit 1 from the double-Esc, got %v", err)
			}
		}
	})

	t.Run("Right Arrow on session from MENU opens SESSIONS; Esc backs out to a live MENU", func(t *testing.T) {
		master, slave := openPTY(t)
		capture := drainPTY(master)
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, "menu")
		cmd.Env = append(os.Environ(), "TERM=xterm-256color")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(500 * time.Millisecond)
		for range 5 { // config -> default -> login -> logout -> remove -> session
			down(master)
		}
		right(master)
		time.Sleep(300 * time.Millisecond)
		out := stripANSI(capture())
		if !strings.Contains(out, "claude cpro - SESSIONS") {
			t.Fatalf("expected Right Arrow on session to open SESSIONS, got %q", out)
		}
		esc(master)
		time.Sleep(300 * time.Millisecond)
		if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
			t.Fatalf("expected the process to still be alive after backing out of SESSIONS to MENU, got %v", err)
		}
		// Re-entering session from the restored MENU frame proves the stack was
		// genuinely popped back to the real MENU frame, not just a stale-looking
		// render — the usual presence-only-can't-prove-a-pop workaround.
		for range 5 {
			down(master)
		}
		right(master)
		time.Sleep(300 * time.Millisecond)
		out = stripANSI(capture())
		if !strings.Contains(out, "claude cpro - SESSIONS") {
			t.Fatalf("expected to re-enter SESSIONS from the restored MENU, got %q", out)
		}
		esc(master)
		esc(master)
		esc(master)
		_ = cmd.Wait()
	})

	t.Run("cpro session continue with no arguments opens CONTINUE SESSION directly", func(t *testing.T) {
		master, slave := openPTY(t)
		capture := drainPTY(master)
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, "session", "continue")
		cmd.Env = append(os.Environ(), "TERM=xterm-256color")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(500 * time.Millisecond)
		out := stripANSI(capture())
		if !strings.Contains(out, "claude cpro - CONTINUE SESSION") {
			t.Fatalf("expected the CONTINUE SESSION screen to open directly, got %q", out)
		}
		esc(master)
		esc(master)
		if err := cmd.Wait(); err != nil {
			if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 1 {
				t.Fatalf("expected a clean exit 1 from the double-Esc, got %v", err)
			}
		}
	})

	t.Run("selecting a session then a destination account resumes it there, even while that account is busy", func(t *testing.T) {
		// A shared lock on the destination stands in for a running Claude
		// session or its background helpers, which inherit s.run's shared
		// lock and outlive the session: the copy must not need exclusivity.
		busy, err := s.accountLock("ui-to@example.com", false)
		if err != nil {
			t.Fatal(err)
		}
		defer busy.Close()
		master, slave := openPTY(t)
		capture := drainPTY(master)
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, "session", "continue")
		// Deliberately an unrelated directory with no recorded sessions of its
		// own — proves the copy is scoped by the picked session's own ID
		// (findSessionDir), not the process's current working directory, which
		// is where continueSession would otherwise have looked.
		cmd.Dir = t.TempDir()
		var stdout bytes.Buffer
		cmd.Env = os.Environ()
		cmd.Stdout = &stdout
		cmd.Stdin, cmd.Stderr = slave, slave
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(500 * time.Millisecond)
		right(master) // select the only seeded session
		time.Sleep(300 * time.Millisecond)
		down(master)       // past the owner (ui-from, listed first) to ui-to
		send(master, "\r") // choose it
		if err := cmd.Wait(); err != nil {
			t.Fatalf("%v; output %q", err, stripANSI(capture()))
		}
		out := stdout.String()
		var invoked struct{ Directory string }
		if err := json.Unmarshal([]byte(out), &invoked); err != nil {
			t.Fatalf("parsing fake claude output: %q: %v", out, err)
		}
		if dir := invoked.Directory; dir != s.profile("ui-to@example.com") {
			t.Fatalf("expected claude resumed under the chosen account, got dir=%q (%q)", dir, out)
		}
		if !strings.Contains(out, sessionID) {
			t.Fatalf("expected the full session ID forwarded to claude via --resume, got %q", out)
		}
		dst := filepath.Join(s.profile("ui-to@example.com"), "projects", dirName, sessionID+".jsonl")
		if _, err := os.Stat(dst); err != nil {
			t.Fatalf("expected the transcript copied into the target account's profile: %v", err)
		}
	})
}

// TestInfoCommand covers "cpro info" (maintenance.go): a concise,
// non-sensitive snapshot of the installation itself — distinct from "cpro
// status", which is about account usage/sessions, not cpro itself.
func TestInfoCommand(t *testing.T) {
	bin, s := buildCLI(t)
	run := func(env []string, args ...string) string {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Env = append(os.Environ(), env...)
		var out bytes.Buffer
		cmd.Stdout = &out
		if err := cmd.Run(); err != nil {
			t.Fatalf("%v: %v; output %s", args, err, &out)
		}
		return out.String()
	}

	t.Run("shows version, binary, config, account count, and Claude Code state", func(t *testing.T) {
		out := run(nil, "info")
		for _, want := range []string{"╭─ cpro", "Version", version, "Binary", "Config", s.dir, "Accounts", "0", "Claude Code", "╰─"} {
			if !strings.Contains(out, want) {
				t.Fatalf("info missing %q, got %q", want, out)
			}
		}
	})

	t.Run("account count reflects registered accounts", func(t *testing.T) {
		stage := t.TempDir()
		data := `{"loggedIn":true,"email":"info-count@example.com","authMethod":"claude.ai"}`
		if err := os.WriteFile(filepath.Join(stage, ".credentials.json"), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(stage, ".claude.json"), []byte(`{}`), 0600); err != nil {
			t.Fatal(err)
		}
		if err := s.installLogin("info-count@example.com", stage); err != nil {
			t.Fatal(err)
		}
		out := run(nil, "info")
		if !strings.Contains(out, "Accounts") || !strings.Contains(out, "1") {
			t.Fatalf("expected the account count to reflect 1 registered account, got %q", out)
		}
	})

	t.Run("never shows credential values, tokens, or secrets", func(t *testing.T) {
		const secret = "super-secret-token-in-info-test"
		stage := t.TempDir()
		data := fmt.Sprintf(`{"loggedIn":true,"email":"info-secret@example.com","authMethod":"claude.ai","claudeAiOauth":{"accessToken":%q}}`, secret)
		if err := os.WriteFile(filepath.Join(stage, ".credentials.json"), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(stage, ".claude.json"), []byte(`{}`), 0600); err != nil {
			t.Fatal(err)
		}
		if err := s.installLogin("info-secret@example.com", stage); err != nil {
			t.Fatal(err)
		}
		out := run(nil, "info")
		if strings.Contains(out, secret) {
			t.Fatalf("info leaked a credential value: %q", out)
		}
	})

	t.Run("NO_COLOR suppresses escape codes", func(t *testing.T) {
		out := run([]string{"NO_COLOR=1"}, "info")
		if strings.Contains(out, "\x1b") {
			t.Fatalf("NO_COLOR should suppress every escape code, got %q", out)
		}
		if !strings.Contains(out, "╭─ cpro") {
			t.Fatalf("expected the panel to still render under NO_COLOR, got %q", out)
		}
	})

	t.Run("distinct from cpro status", func(t *testing.T) {
		infoOut := run(nil, "info")
		statusOut := run(nil, "status")
		for _, usageWord := range []string{"Session", "Week", "Active sessions"} {
			if strings.Contains(infoOut, usageWord) {
				t.Fatalf("info should never show usage/session data (that's status's job), got %q", infoOut)
			}
		}
		for _, infoWord := range []string{"Binary", "Claude Code"} {
			if strings.Contains(statusOut, infoWord) {
				t.Fatalf("status should never show installation info (that's info's job), got %q", statusOut)
			}
		}
	})
}

// TestApplyPreferencesDefaults covers applyPreferences' own fallback: a
// config predating warningColor/dangerColor/theme (every one of those
// fields at its Go zero value, the same shape a config.json saved by an
// older cpro version has) must resolve to the documented defaults — Orange
// warning, Red danger, Rounded theme — with no migration step needed, the
// same way accentColor/barColor/the bar thresholds already fell back before
// this task. Pure/no-process: applyPreferences only ever mutates package
// globals.
func TestApplyPreferencesDefaults(t *testing.T) {
	origAccent, origBar, origWarn, origDanger := accentMode, barColorSafe, warningColor, dangerColor
	origWarnPct, origDangerPct := barWarnThreshold, barDangerThreshold
	origTheme := currentTheme
	t.Cleanup(func() {
		accentMode, barColorSafe, warningColor, dangerColor = origAccent, origBar, origWarn, origDanger
		barWarnThreshold, barDangerThreshold = origWarnPct, origDangerPct
		currentTheme = origTheme
	})

	// applyPreferences doesn't itself "reset to defaults" — the vars' own Go
	// initializers already hold the documented defaults (see ui.go/theme.go);
	// applyPreferences only ever conditionally overrides from a non-blank
	// config field, leaving whatever value is already there untouched
	// otherwise. So the real, meaningful contract to test is: (1) those
	// compiled-in defaults actually match what's documented, and (2)
	// applyPreferences(blank-config) — the shape an existing config.json
	// predating these fields unmarshals to — is a true no-op, changing
	// nothing, which is exactly what makes a predating config fall back
	// correctly with no migration step.
	if accentMode != "#A78BFA" {
		t.Fatalf("accentMode compiled-in default: got %q, want #A78BFA (Purple)", accentMode)
	}
	if barColorSafe != "#34D399" {
		t.Fatalf("barColorSafe compiled-in default: got %q, want #34D399 (Green)", barColorSafe)
	}
	if warningColor != "#FB923C" {
		t.Fatalf("warningColor compiled-in default: got %q, want #FB923C (Orange)", warningColor)
	}
	if dangerColor != "#F87171" {
		t.Fatalf("dangerColor compiled-in default: got %q, want #F87171 (Red)", dangerColor)
	}
	if barWarnThreshold != 80 || barDangerThreshold != 90 {
		t.Fatalf("threshold compiled-in defaults: warn=%v danger=%v, want 80/90", barWarnThreshold, barDangerThreshold)
	}
	if currentTheme.Name != "Rounded" {
		t.Fatalf("theme compiled-in default: got %q, want Rounded", currentTheme.Name)
	}

	before := struct {
		accent, bar, warn, danger string
		warnPct, dangerPct        float64
		theme                     string
	}{accentMode, barColorSafe, warningColor, dangerColor, barWarnThreshold, barDangerThreshold, currentTheme.Name}
	applyPreferences(config{}) // the exact shape an old config.json unmarshals to
	if accentMode != before.accent || barColorSafe != before.bar || warningColor != before.warn ||
		dangerColor != before.danger || barWarnThreshold != before.warnPct || barDangerThreshold != before.dangerPct ||
		currentTheme.Name != before.theme {
		t.Fatalf("applyPreferences(config{}) must be a no-op (existing config predating these fields falls back to whatever was already there): before %+v, accentMode=%q barColorSafe=%q warningColor=%q dangerColor=%q warnPct=%v dangerPct=%v theme=%q",
			before, accentMode, barColorSafe, warningColor, dangerColor, barWarnThreshold, barDangerThreshold, currentTheme.Name)
	}

	// A config with SOME fields set and others still blank (an existing user
	// upgrading cpro and touching only one new preference) must only
	// override what's actually set, leaving everything else exactly as it
	// already was — not silently reset to defaults alongside it.
	accentMode, warningColor = "#000000", "#000000"
	applyPreferences(config{DangerColor: "#8B5CF6"})
	if accentMode != "#000000" {
		t.Fatalf("applyPreferences must not touch accentMode when AccentColor is blank, got %q", accentMode)
	}
	if warningColor != "#000000" {
		t.Fatalf("applyPreferences must not touch warningColor when WarningColor is blank, got %q", warningColor)
	}
	if dangerColor != "#8B5CF6" {
		t.Fatalf("applyPreferences should have applied the one field that was actually set, got %q", dangerColor)
	}
}

// TestThemeGlobalApplication covers the requesting spec's own explicit
// "apply Theme globally... do not hardcode border characters independently
// in each screen" requirement, across every kind of panel cpro draws with:
// the plain accent()-printed panels (cpro info, main.go/maintenance.go),
// the shared bubbletea chrome (renderPanel, tui.go — cpro config), and the
// root picker's own per-line-colored chrome (renderRootPanel, rootui.go —
// bare cpro). Each surface is checked against all four themes, so a screen
// that only happens to work for the default Rounded theme (e.g. by
// accidentally keeping a hardcoded "╭─"/"╰─" fallback somewhere) would still
// be caught here. cpro list/status's own panels (main.go) go through the
// exact same currentTheme.Top()/.Rail()/.Bottom() calls as cpro info's,
// verified directly in the source rather than re-driven through a second
// pty flow here (they need a registered account before their own panel
// renders at all, unlike info's, which never does).
func TestThemeGlobalApplication(t *testing.T) {
	bin, _ := buildCLI(t)
	run := func(env []string, args ...string) string {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Env = append(os.Environ(), env...)
		var out bytes.Buffer
		cmd.Stdout = &out
		if err := cmd.Run(); err != nil {
			t.Fatalf("%v: %v; output %s", args, err, &out)
		}
		return out.String()
	}

	for _, theme := range borderThemes {
		t.Run(theme.Name, func(t *testing.T) {
			set := exec.Command(bin, "config", "theme", strings.ToLower(theme.Name))
			if out, err := set.CombinedOutput(); err != nil {
				t.Fatalf("cpro config theme %s: %v: %s", theme.Name, err, out)
			}

			t.Run("cpro info", func(t *testing.T) {
				out := run(nil, "info")
				if !strings.Contains(out, theme.Top()) || !strings.Contains(out, theme.Rail()) || !strings.Contains(out, theme.Bottom()) {
					t.Fatalf("cpro info did not use theme %s's own border runes (%s/%s/%s), got %q", theme.Name, theme.Top(), theme.Rail(), theme.Bottom(), out)
				}
			})

			t.Run("cpro config", func(t *testing.T) {
				master, slave := openPTY(t)
				capture := drainPTY(master)
				ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, bin, "config")
				cmd.Env = append(os.Environ(), "TERM=xterm-256color")
				cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				time.Sleep(500 * time.Millisecond)
				// A direct `cpro config` invocation now arms a double-Esc-to-exit
				// (decision 0023) rather than quitting on the first press.
				for range 2 {
					if _, err := master.WriteString("\x1b"); err != nil {
						t.Fatal(err)
					}
					time.Sleep(200 * time.Millisecond)
				}
				_ = cmd.Wait()
				out := stripANSI(capture())
				if !strings.Contains(out, theme.Top()) || !strings.Contains(out, theme.Bottom()) {
					t.Fatalf("cpro config did not use theme %s's own border runes, got %q", theme.Name, out)
				}
			})

			t.Run("bare cpro (root picker)", func(t *testing.T) {
				master, slave := openPTY(t)
				capture := drainPTY(master)
				ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, bin)
				cmd.Env = append(os.Environ(), "TERM=xterm-256color")
				cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				time.Sleep(500 * time.Millisecond)
				if _, err := master.WriteString("\x1b"); err != nil {
					t.Fatal(err)
				}
				time.Sleep(150 * time.Millisecond)
				if _, err := master.WriteString("\x1b"); err != nil {
					t.Fatal(err)
				}
				_ = cmd.Wait()
				out := stripANSI(capture())
				if !strings.Contains(out, theme.Top()) || !strings.Contains(out, theme.Bottom()) {
					t.Fatalf("bare cpro's root picker did not use theme %s's own border runes, got %q", theme.Name, out)
				}
			})
		})
	}

	// Restore the default so later tests in this process (and its shared
	// config.json) aren't left on a non-default theme.
	set := exec.Command(bin, "config", "theme", "rounded")
	if out, err := set.CombinedOutput(); err != nil {
		t.Fatalf("restoring theme to rounded: %v: %s", err, out)
	}
}

// TestSessionContinueYOLOOutsideDirectoryReadIntegration is decision 0026's
// permission fix carried through the *actual* `cpro session continue`
// execution path, not `cpro run` a second time — proving the two commands
// really do share one permission resolver rather than merely being expected
// to (session.go's own `continue` RunE ends in the identical `s.run` call
// `cpro run`'s own RunE does; this is the real, end-to-end proof that fact
// alone doesn't regress, e.g. via `session continue`'s own `--resume`
// argument ending up positioned so as to confuse a real claude's flag
// parsing, which a fake-claude-only test can't catch).
//
// Real accounts, real claude, gated behind CPRO_TEST_REAL_CLAUDE_INTEGRATION=1
// (unset by default — costs real, small API usage and needs network) exactly
// like TestYOLOOutsideDirectoryReadIntegration. FROM_EMAIL is a synthetic cpro
// account (continueSession never authenticates it — only reads its recorded
// transcripts, so it needs no real credentials of its own); TO_EMAIL must be
// the real, currently-authenticated account (`s.run`'s own validAuth check
// requires the registered email to match the real one in its credentials
// exactly), discovered via `claude auth status --json` rather than assumed,
// so this runs correctly wherever it's actually invoked. A real session is
// first seeded under FROM_EMAIL's own profile (a real, cheap `claude -p` call
// from the test workspace, with real credentials copied in just so it can
// authenticate) so the actual `--resume SESSION_ID` this test drives is a
// genuine, resumable conversation — not a fabricated transcript a real claude
// wouldn't accept.
func TestSessionContinueYOLOOutsideDirectoryReadIntegration(t *testing.T) {
	if os.Getenv("CPRO_TEST_REAL_CLAUDE_INTEGRATION") != "1" {
		t.Skip("set CPRO_TEST_REAL_CLAUDE_INTEGRATION=1 to run this real, API-calling integration test")
	}
	realClaude, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude not found in PATH")
	}

	statusOut, err := exec.Command(realClaude, "auth", "status", "--json").Output()
	if err != nil {
		t.Skipf("claude auth status failed, skipping (no authenticated account in this environment): %v", err)
	}
	var status struct {
		LoggedIn bool   `json:"loggedIn"`
		Email    string `json:"email"`
	}
	if err := json.Unmarshal(statusOut, &status); err != nil || !status.LoggedIn || status.Email == "" {
		t.Skip("no authenticated claude account in this environment, skipping")
	}
	toEmail := status.Email
	const fromEmail = "session-perm-integration-from@example.com"

	dir := t.TempDir()
	bin := filepath.Join(dir, "cpro")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %s %v", out, err)
	}
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	s, err := openStore()
	if err != nil {
		t.Fatal(err)
	}

	// Copy the real, currently-authenticated credentials into both test
	// profiles — FROM's own copy exists solely so the seeding call below can
	// authenticate; continueSession itself never checks FROM's identity.
	realConfigDir := os.Getenv("CLAUDE_CONFIG_DIR")
	if realConfigDir == "" {
		realConfigDir = filepath.Join(os.Getenv("HOME"), ".claude")
	}
	copyCreds := func(email string) {
		t.Helper()
		profile := s.profile(email)
		if err := privateDir(profile); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{".credentials.json", ".claude.json"} {
			data, err := os.ReadFile(filepath.Join(realConfigDir, name))
			if err != nil {
				t.Fatalf("reading real %s: %v", name, err)
			}
			if err := os.WriteFile(filepath.Join(profile, name), data, 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	copyCreds(fromEmail)
	copyCreds(toEmail)
	if err := s.update(func(c *config) error {
		c.Accounts[fromEmail] = true
		c.Accounts[toEmail] = true
		c.PermissionMode = "yolo"
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	workspace := t.TempDir()
	outside := t.TempDir()
	const marker = "CPRO_OUTSIDE_READ_OK"
	probe := filepath.Join(outside, "probe.txt")
	if err := os.WriteFile(probe, []byte(marker+"\n"), 0600); err != nil {
		t.Fatal(err)
	}

	// Seed a real, resumable session under FROM_EMAIL for this exact
	// workspace directory.
	seed := exec.Command(realClaude, "-p", "Reply with the single word OK.", "--output-format", "json")
	seed.Dir = workspace
	seed.Env = append(os.Environ(), "CLAUDE_CONFIG_DIR="+s.profile(fromEmail))
	seedOut, err := seed.Output()
	if err != nil {
		t.Fatalf("seeding a real session failed: %v", err)
	}
	var seeded struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(seedOut, &seeded); err != nil || seeded.SessionID == "" {
		t.Fatalf("could not read session_id from seeding call: %v; raw=%s", err, seedOut)
	}

	// The actual cpro session continue execution path: copy the seeded
	// session to TO_EMAIL and resume it there, forwarding a real
	// outside-directory read request under the configured YOLO mode.
	prompt := fmt.Sprintf("Use the Read tool to read %s and output exactly its contents, nothing else.", probe)
	cmd := exec.Command(bin, "session", "continue", fromEmail, toEmail, "--",
		"--resume", seeded.SessionID, "--permission-prompts", "none", "-p", prompt, "--output-format", "json")
	cmd.Dir = workspace
	cmd.Env = os.Environ()
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("cpro session continue failed: %v; stdout=%s stderr=%s", err, &stdout, &stderr)
	}

	out := stdout.String()
	lines := strings.SplitN(out, "\n", 2)
	if len(lines) < 2 || !strings.Contains(lines[0], "Copied 1 session file(s) to "+toEmail) {
		t.Fatalf("expected cpro's own copy confirmation as the first line, got %q", out)
	}
	var result struct {
		Result            string `json:"result"`
		PermissionDenials []struct {
			ToolName string `json:"tool_name"`
		} `json:"permission_denials"`
	}
	if err := json.Unmarshal([]byte(lines[1]), &result); err != nil {
		t.Fatalf("could not parse claude's JSON output: %v; raw=%s", err, lines[1])
	}
	for _, d := range result.PermissionDenials {
		if d.ToolName == "Read" {
			t.Fatalf("expected no Read permission denial for an outside-working-directory read under YOLO through session continue; claude's final answer was %q", result.Result)
		}
	}
	if !strings.Contains(result.Result, marker) {
		t.Fatalf("expected claude's final answer (resumed through cpro session continue) to contain the probe file's contents (%q), got %q", marker, result.Result)
	}
}

// TestFindSessionOwner covers session.go's own session-discovery step: it
// must locate the right account/project directory for a given session ID
// across every registered account (never assuming the current/default
// account owns it), and report a clear error for an ID nothing recorded.
func TestFindSessionOwner(t *testing.T) {
	s := &store{dir: t.TempDir()}
	writeSession := func(email, dirName, sessionID string) {
		t.Helper()
		dir := filepath.Join(s.profile(email), "projects", dirName)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, sessionID+".jsonl"), []byte(`{"type":"user"}`+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeSession("owner@example.com", "-home-uu-project", "11111111-1111-1111-1111-111111111111")
	writeSession("other@example.com", "-home-uu-other", "22222222-2222-2222-2222-222222222222")
	c := config{Accounts: map[string]bool{"owner@example.com": true, "other@example.com": true}}

	email, dirName, err := findSessionOwner(s, c, "11111111-1111-1111-1111-111111111111")
	if err != nil || email != "owner@example.com" || dirName != "-home-uu-project" {
		t.Fatalf("got email=%q dirName=%q err=%v", email, dirName, err)
	}

	if _, _, err := findSessionOwner(s, c, "does-not-exist"); err == nil || !strings.Contains(err.Error(), "no cpro-managed session found") {
		t.Fatalf("expected a clear missing-session error, got %v", err)
	}
}

// TestMigrateSession covers session.go's own copy-and-verify session
// migration: it must copy only the one requested session transcript (never
// the rest of that project directory, unlike continueSession's own
// whole-directory copy), never touch or remove the source, skip rather than
// overwrite an already-present destination file, and surface a clear error
// when the source transcript can't be read.
func TestMigrateSession(t *testing.T) {
	s := &store{dir: t.TempDir()}
	const dirName, sessionID = "-home-uu-project", "11111111-1111-1111-1111-111111111111"
	srcDir := filepath.Join(s.profile("from@example.com"), "projects", dirName)
	if err := os.MkdirAll(srcDir, 0700); err != nil {
		t.Fatal(err)
	}
	content := []byte(`{"type":"user","cwd":"/home/uu/project"}` + "\n")
	if err := os.WriteFile(filepath.Join(srcDir, sessionID+".jsonl"), content, 0600); err != nil {
		t.Fatal(err)
	}
	// A second, unrelated session recorded for the same account/directory —
	// must never be swept along with the one actually requested.
	if err := os.WriteFile(filepath.Join(srcDir, "unrelated-session.jsonl"), []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}

	if err := migrateSession(s, "from@example.com", "to@example.com", dirName, sessionID); err != nil {
		t.Fatalf("migrateSession: %v", err)
	}
	dstPath := filepath.Join(s.profile("to@example.com"), "projects", dirName, sessionID+".jsonl")
	got, err := os.ReadFile(dstPath)
	if err != nil {
		t.Fatalf("expected the session copied to the target profile: %v", err)
	}
	if string(got) != string(content) {
		t.Fatalf("copied transcript does not match the source: got %q want %q", got, content)
	}
	if _, err := os.Stat(filepath.Join(s.profile("to@example.com"), "projects", dirName, "unrelated-session.jsonl")); err == nil {
		t.Fatalf("expected only the requested session to be copied, not the whole directory")
	}
	if _, err := os.Stat(filepath.Join(srcDir, sessionID+".jsonl")); err != nil {
		t.Fatalf("expected the source transcript to remain untouched: %v", err)
	}

	// Re-running must not overwrite an already-present destination file.
	if err := os.WriteFile(dstPath, []byte("already there"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := migrateSession(s, "from@example.com", "to@example.com", dirName, sessionID); err != nil {
		t.Fatalf("migrateSession (re-run): %v", err)
	}
	got, err = os.ReadFile(dstPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "already there" {
		t.Fatalf("expected an existing destination file to be left alone, got %q", got)
	}

	if err := migrateSession(s, "missing@example.com", "to@example.com", dirName, sessionID); err == nil {
		t.Fatal("expected an error when the source transcript cannot be read")
	}
}

// TestSessionWorkingDirectory covers reading a transcript's own recorded
// "cwd" field — the mechanism restoreSessionDirectory uses to resume from
// the session's original directory rather than projectDirName's own lossy
// reverse-encoding.
func TestSessionWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	lines := "{\"type\":\"queue-operation\"}\n{\"type\":\"user\",\"cwd\":\"/home/uu/project\"}\n"
	if err := os.WriteFile(path, []byte(lines), 0600); err != nil {
		t.Fatal(err)
	}
	got, ok := sessionWorkingDirectory(path)
	if !ok || got != "/home/uu/project" {
		t.Fatalf("got %q, %v", got, ok)
	}

	empty := filepath.Join(dir, "empty.jsonl")
	if err := os.WriteFile(empty, []byte(`{"type":"queue-operation"}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, ok := sessionWorkingDirectory(empty); ok {
		t.Fatal("expected no cwd found in a transcript that never records one")
	}

	if _, ok := sessionWorkingDirectory(filepath.Join(dir, "missing.jsonl")); ok {
		t.Fatal("expected no cwd found for a file that doesn't exist")
	}
}

// TestRestoreSessionDirectory covers the chdir-on-resume behavior directly:
// a valid recorded cwd is switched to, and an invalid/missing one leaves the
// process in its current directory (with a diagnostic on stderr) rather than
// failing outright.
func TestRestoreSessionDirectory(t *testing.T) {
	original, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(original) })

	s := &store{dir: t.TempDir()}
	const dirName, sessionID = "-tmp-target", "11111111-1111-1111-1111-111111111111"

	t.Run("valid recorded directory", func(t *testing.T) {
		target := t.TempDir()
		srcDir := filepath.Join(s.profile("owner@example.com"), "projects", dirName)
		if err := os.MkdirAll(srcDir, 0700); err != nil {
			t.Fatal(err)
		}
		data := fmt.Sprintf(`{"type":"user","cwd":%q}`+"\n", target)
		if err := os.WriteFile(filepath.Join(srcDir, sessionID+".jsonl"), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		restoreSessionDirectory(s, "owner@example.com", dirName, sessionID)
		got, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		// Resolve symlinks (e.g. /tmp -> /private/tmp) so this doesn't flake
		// on a system where t.TempDir()'s path isn't already canonical.
		wantReal, _ := filepath.EvalSymlinks(target)
		gotReal, _ := filepath.EvalSymlinks(got)
		if gotReal != wantReal {
			t.Fatalf("got cwd %q, want %q", got, target)
		}
	})

	t.Run("missing recorded directory falls back without failing", func(t *testing.T) {
		if err := os.Chdir(original); err != nil {
			t.Fatal(err)
		}
		srcDir := filepath.Join(s.profile("nodir@example.com"), "projects", dirName)
		if err := os.MkdirAll(srcDir, 0700); err != nil {
			t.Fatal(err)
		}
		data := `{"type":"user","cwd":"/this/directory/does/not/exist-cpro-test"}` + "\n"
		if err := os.WriteFile(filepath.Join(srcDir, sessionID+".jsonl"), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		restoreSessionDirectory(s, "nodir@example.com", dirName, sessionID)
		got, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		wantReal, _ := filepath.EvalSymlinks(original)
		gotReal, _ := filepath.EvalSymlinks(got)
		if gotReal != wantReal {
			t.Fatalf("expected to remain in %q when the recorded directory is missing, got %q", original, got)
		}
	})
}

// TestResumeSession is the end-to-end subprocess test for `cpro --resume`/
// `cpro -r` (decision 0032): built exactly like TestSessionContinue, sharing
// its own fake-claude/buildCLI harness, since --resume is meant to share the
// same underlying machinery (s.run, applyPermissionDefaults) rather than a
// second implementation.
func TestResumeSession(t *testing.T) {
	bin, s := buildCLI(t)

	login := func(email string) {
		t.Helper()
		stage := t.TempDir()
		data := fmt.Sprintf(`{"loggedIn":true,"email":%q,"authMethod":"claude.ai"}`, email)
		if err := os.WriteFile(filepath.Join(stage, ".credentials.json"), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(stage, ".claude.json"), []byte(`{}`), 0600); err != nil {
			t.Fatal(err)
		}
		if err := s.installLogin(email, stage); err != nil {
			t.Fatal(err)
		}
	}
	login("resume-owner@example.com")
	login("resume-other@example.com")

	projectDir := t.TempDir()
	dirName := projectDirName(projectDir)
	ownerSessions := filepath.Join(s.profile("resume-owner@example.com"), "projects", dirName)
	if err := os.MkdirAll(ownerSessions, 0700); err != nil {
		t.Fatal(err)
	}
	const sessionID = "33333333-3333-3333-3333-333333333333"
	transcript := fmt.Sprintf(`{"type":"user","cwd":%q}`+"\n", projectDir)
	if err := os.WriteFile(filepath.Join(ownerSessions, sessionID+".jsonl"), []byte(transcript), 0600); err != nil {
		t.Fatal(err)
	}

	run := func(t *testing.T, wantCode int, dir string, args ...string) (string, string) {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Dir = dir
		cmd.Env = os.Environ()
		var out, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &stderr
		err := cmd.Run()
		got := 0
		if err != nil {
			if e, ok := err.(*exec.ExitError); ok {
				got = e.ExitCode()
			} else {
				t.Fatal(err)
			}
		}
		if got != wantCode {
			t.Fatalf("%q: exit %d, want %d; stdout %s stderr %s", args, got, wantCode, &out, &stderr)
		}
		return out.String(), stderr.String()
	}
	defaultAccount := func(t *testing.T) string {
		t.Helper()
		c, err := s.read()
		if err != nil {
			t.Fatal(err)
		}
		return c.Default
	}
	parseClaudeInvocation := func(t *testing.T, out string) (args []string, dir string) {
		t.Helper()
		var result struct {
			Args      []string
			Directory string
		}
		if err := json.Unmarshal([]byte(out), &result); err != nil {
			t.Fatalf("could not parse claude's JSON output: %v; raw=%s", err, out)
		}
		return result.Args, result.Directory
	}

	t.Run("missing session ID errors clearly", func(t *testing.T) {
		_, stderr := run(t, 1, projectDir, "--resume")
		if !strings.Contains(stderr, "missing session ID") {
			t.Fatalf("expected a missing-session-ID error, got %q", stderr)
		}
	})

	t.Run("unknown session ID errors clearly", func(t *testing.T) {
		_, stderr := run(t, 1, projectDir, "--resume", "does-not-exist")
		if !strings.Contains(stderr, "no cpro-managed session found") {
			t.Fatalf("expected a clear missing-session error, got %q", stderr)
		}
	})

	t.Run("cpro --resume ID discovers the owning account automatically", func(t *testing.T) {
		before := defaultAccount(t)
		out, _ := run(t, 0, projectDir, "--resume", sessionID)
		args, dir := parseClaudeInvocation(t, out)
		if !strings.Contains(dir, s.profile("resume-owner@example.com")) {
			t.Fatalf("expected claude invoked under the owning account's profile, got dir=%q", dir)
		}
		if !hasArg(args, "--resume") {
			t.Fatalf("expected --resume in claude's argv, got %q", args)
		}
		if defaultAccount(t) != before {
			t.Fatalf("expected the configured default account to be unchanged by resume: before=%q after=%q", before, defaultAccount(t))
		}
	})

	t.Run("-r ID is a recognized alias", func(t *testing.T) {
		out, _ := run(t, 0, projectDir, "-r", sessionID)
		_, dir := parseClaudeInvocation(t, out)
		if !strings.Contains(dir, s.profile("resume-owner@example.com")) {
			t.Fatalf("expected claude invoked under the owning account's profile via -r, got dir=%q", dir)
		}
	})

	t.Run("same-account resume requires no migration", func(t *testing.T) {
		run(t, 0, projectDir, "--resume", sessionID)
		otherCopy := filepath.Join(s.profile("resume-other@example.com"), "projects", dirName, sessionID+".jsonl")
		if _, err := os.Stat(otherCopy); err == nil {
			t.Fatalf("expected no migration when resuming under the owning account")
		}
	})

	t.Run("explicit --account override migrates into the target profile", func(t *testing.T) {
		before := defaultAccount(t)
		out, _ := run(t, 0, projectDir, "--resume", sessionID, "--account", "resume-other@example.com")
		_, dir := parseClaudeInvocation(t, out)
		if !strings.Contains(dir, s.profile("resume-other@example.com")) {
			t.Fatalf("expected claude invoked under the overridden target account's profile, got dir=%q", dir)
		}
		dst := filepath.Join(s.profile("resume-other@example.com"), "projects", dirName, sessionID+".jsonl")
		if _, err := os.Stat(dst); err != nil {
			t.Fatalf("expected the session migrated into the target account's profile: %v", err)
		}
		src, err := os.ReadFile(filepath.Join(ownerSessions, sessionID+".jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(dst)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(src) {
			t.Fatalf("migrated transcript does not match the original: got %q want %q", got, src)
		}
		if _, err := os.Stat(filepath.Join(ownerSessions, sessionID+".jsonl")); err != nil {
			t.Fatalf("expected the original session to remain in place after migration: %v", err)
		}
		if defaultAccount(t) != before {
			t.Fatalf("expected --account to leave the configured default account unchanged: before=%q after=%q", before, defaultAccount(t))
		}
	})

	t.Run("--account rejects an unregistered account", func(t *testing.T) {
		_, stderr := run(t, 1, projectDir, "--resume", sessionID, "--account", "nobody@example.com")
		if !strings.Contains(stderr, "not registered") {
			t.Fatalf("expected a missing-account error, got %q", stderr)
		}
	})

	t.Run("signed-out owning account uses the intended non-interactive fallback", func(t *testing.T) {
		// A dedicated session/directory, never touched by the migration
		// subtests above: findSessionOwner (session.go) picks the most
		// recently modified copy when a session exists under more than one
		// account (an earlier subtest's own --account override deliberately
		// creates exactly that for the shared sessionID), so reusing it here
		// would resolve to resume-other — already authenticated — instead of
		// exercising the fallback this subtest is actually about.
		otherDir := t.TempDir()
		otherDirName := projectDirName(otherDir)
		otherSessions := filepath.Join(s.profile("resume-owner@example.com"), "projects", otherDirName)
		if err := os.MkdirAll(otherSessions, 0700); err != nil {
			t.Fatal(err)
		}
		const soloSessionID = "44444444-4444-4444-4444-444444444444"
		if err := os.WriteFile(filepath.Join(otherSessions, soloSessionID+".jsonl"), []byte(fmt.Sprintf(`{"type":"user","cwd":%q}`+"\n", otherDir)), 0600); err != nil {
			t.Fatal(err)
		}

		// Sign the owner out (removes its credentials without removing the
		// registered account/profile), then resume with no --account
		// override and no terminal attached (run's own exec.Command has no
		// pty) — this must surface a clear error naming the fallback rather
		// than silently proceeding under an invalid session.
		credPath := filepath.Join(s.profile("resume-owner@example.com"), ".credentials.json")
		orig, err := os.ReadFile(credPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(credPath); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := os.WriteFile(credPath, orig, 0600); err != nil {
				t.Fatal(err)
			}
		}()
		_, stderr := run(t, 1, projectDir, "--resume", soloSessionID)
		if !strings.Contains(stderr, "not authenticated") || !strings.Contains(stderr, "--account") {
			t.Fatalf("expected a clear fallback error naming --account, got %q", stderr)
		}
	})

	t.Run("extra Claude arguments are forwarded", func(t *testing.T) {
		out, _ := run(t, 0, projectDir, "--resume", sessionID, "--", "continue fixing tests")
		args, _ := parseClaudeInvocation(t, out)
		found := false
		for i, a := range args {
			if a == "--resume" && i+1 < len(args) && args[i+1] == sessionID {
				found = i+2 < len(args) && args[i+2] == "continue fixing tests"
			}
		}
		if !found {
			t.Fatalf("expected --resume %s followed by the forwarded argument, got %q", sessionID, args)
		}
	})

	t.Run("permission mode matches cpro run's own resolver", func(t *testing.T) {
		if err := s.update(func(c *config) error { c.PermissionMode = "readonly"; return nil }); err != nil {
			t.Fatal(err)
		}
		out, _ := run(t, 0, projectDir, "--resume", sessionID)
		args, _ := parseClaudeInvocation(t, out)
		if !hasArg(args, "--permission-mode") {
			t.Fatalf("expected the configured Read-only permission mode applied to a resumed session, got %q", args)
		}
		if err := s.update(func(c *config) error { c.PermissionMode = ""; return nil }); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("working directory is restored when the recorded directory is valid", func(t *testing.T) {
		// Run from a directory that has no recorded session of its own — the
		// same "prove the copy/lookup is scoped by the picked session, not
		// the process's own cwd" discipline TestSessionUI already applies —
		// so a successful parse here proves resumeSession's own os.Chdir
		// into the transcript's recorded cwd (projectDir) succeeded rather
		// than accidentally resolving something under elsewhere.
		elsewhere := t.TempDir()
		out, _ := run(t, 0, elsewhere, "--resume", sessionID)
		parseClaudeInvocation(t, out)
	})
}

// TestVersionBumpedForSourceChanges guards IMPROVEMENTS.md item 9 / decision
// 0046: `const version` is bumped by hand, and it was forgotten in practice
// (the installed binary sat two versions behind the built one until noticed).
// A semver cannot be derived from this repository's history alone — it has no
// release tags, so there is no source of truth to compute one from — so this
// is the "check that fails" alternative instead: any commit since the one
// that last changed the version constant that touches src/ and is not one of
// the deliberately non-releasable types (docs/chore/test/ci/build/style/merge)
// means a feature landed without a bump. Skips cleanly when git or the history
// isn't available (source tarball, shallow clone).
func TestVersionBumpedForSourceChanges(t *testing.T) {
	runGit := func(args ...string) (string, bool) {
		out, err := exec.Command("git", args...).Output()
		if err != nil {
			return "", false
		}
		return strings.TrimSpace(string(out)), true
	}
	root, ok := runGit("rev-parse", "--show-toplevel")
	if !ok || root == "" {
		t.Skip("not a git work tree")
	}
	gitAt := func(args ...string) (string, bool) {
		return runGit(append([]string{"-C", root}, args...)...)
	}

	bump, ok := gitAt("log", "-1", "--format=%H", "-G", "^const version", "--", "src/main.go")
	if !ok || bump == "" {
		t.Skip("no commit changing src/main.go's version constant found (shallow history?)")
	}
	subjects, ok := gitAt("log", "--format=%s", bump+"..HEAD", "--", "src/")
	if !ok {
		t.Skip("could not list commits since the version bump")
	}

	nonReleasable := []string{"docs", "chore", "test", "ci", "build", "style", "merge"}
	var offenders []string
	for _, subject := range strings.Split(subjects, "\n") {
		subject = strings.TrimSpace(subject)
		if subject == "" {
			continue
		}
		lower := strings.ToLower(subject)
		skip := false
		for _, prefix := range nonReleasable {
			if strings.HasPrefix(lower, prefix) {
				skip = true
				break
			}
		}
		if !skip {
			offenders = append(offenders, subject)
		}
	}
	if len(offenders) == 0 {
		return
	}
	short := bump
	if len(short) > 8 {
		short = short[:8]
	}
	t.Fatalf("src/ has %d changing commit(s) since the last `const version` bump (%s) with no bump:\n  - %s\nBump the version constant in src/main.go, or make the change a docs:/chore: commit when no release is intended.",
		len(offenders), short, strings.Join(offenders, "\n  - "))
}
