//go:build linux

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestGenerateScreenCaptures writes one ANSI file per TUI screen into
// $CPRO_CAPTURE_DIR, using each screen's own View method with a realistic,
// seeded state — so a capture is the exact frame the running app would draw,
// without needing a live pty or a terminal emulator to reconstruct it. It is
// skipped unless CPRO_CAPTURE_DIR is set, so it never runs as part of the
// normal suite. Pass an absolute path: go test runs this with the package
// directory as its working directory, so a relative one would resolve under
// src/. (../cpro-captures/capture-screens.sh passes an absolute path.)
//
// `../cpro-captures/capture-screens.sh` sets that variable, runs this test, and
// then renders each file to an SVG with charmbracelet/freeze into
// ../cpro-captures/screens/. The
// intermediate .ansi files are the pristine source and are kept so a capture
// can be re-rendered without re-running the test.
func TestGenerateScreenCaptures(t *testing.T) {
	dir := os.Getenv("CPRO_CAPTURE_DIR")
	if dir == "" {
		t.Skip("set CPRO_CAPTURE_DIR to write screen captures")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	const (
		emailA = "you@example.com"
		emailB = "work@example.com"
		width  = 120
		height = 40
	)
	_, s := buildCLI(t)
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
	login(emailA)
	login(emailB)
	if err := s.update(func(c *config) error {
		c.Default = emailA
		// One account pinned to its own mode, so the Permissions screen's own
		// Accounts section (decision 0054) captures both states — "(override)"
		// and "(global)" — instead of two identical rows.
		c.PermissionModeByAccount = map[string]string{emailB: "yolo"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	seedSession := func(email, cwd, sessionID, title string) {
		t.Helper()
		dirName := projectDirName(cwd)
		projectDir := filepath.Join(s.profile(email), "projects", dirName)
		if err := os.MkdirAll(projectDir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(projectDir, sessionID+".jsonl"), []byte(`{"type":"ai-title","aiTitle":"`+title+`"}`+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	seedSession(emailA, "/home/you/projects/cpro", "26074c99-80ca-4211-9c87-4510d79a27da", "Session picker titles")
	seedSession(emailB, "/home/you/work/api", "a93fe208-1111-4000-8000-000000000000", "Rate limiter for the billing API")

	c, err := s.read()
	if err != nil {
		t.Fatal(err)
	}
	// Representative Session/Week usage, supplied directly: a capture should
	// show real-looking bars without reaching Anthropic's usage endpoint.
	usage := map[string]runAccountUsage{
		emailA: {loaded: true, session: 42, week: 65},
		emailB: {loaded: true, session: 79, week: 88},
	}

	write := func(name, content string) {
		t.Helper()
		if !strings.HasSuffix(content, "\n") {
			content += "\n"
		}
		if err := os.WriteFile(filepath.Join(dir, name+".ansi"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	root := rootCommand()
	root.InitDefaultHelpCmd()
	root.InitDefaultCompletionCmd()

	newPicker := func() *rootPickerApp {
		m := &rootPickerApp{
			root: root, s: s, color: true, width: width, height: height,
			shades:       deriveAccentShades(accentMode, len(rootGroups)),
			accountUsage: usage,
		}
		m.stack = newNavStack(rootFrame{kind: frameList, list: newCommandBrowseList(rootPickerFilteredEntries(root, rootLauncherNames))})
		applyLiveMeta(m.stack.current().list.items, s)
		return m
	}

	// The two pickers made of a fixed command list — the reduced ROOT launcher
	// and the full MENU palette — are captured once per entry: every row is
	// selectable and each selection draws its own description below the panel,
	// so one capture per entry is what actually documents the screen. The
	// cursor is moved directly on the browse list — no search is in play, so
	// `filtered`/`fcursor` stay untouched.
	m := newPicker()
	launcher := m.stack.current()
	// The launcher's own frame, then one capture per row. Every file is named
	// `cpro-<parent>_<own name>`, where each underscore marks one navigation
	// step: the launcher is `cpro-root`, and its four rows are
	// `cpro-root_<key>` — run, status, watch and menu.
	write("cpro-root", m.viewNormal())
	for i, e := range launcher.list.items {
		launcher.list.cursor = i
		write("cpro-root-"+e.name, m.viewNormal())
	}
	m.pushMenu()
	menu := m.stack.current()
	// The MENU palette — the screen the launcher's `menu` row opens, so it is
	// `cpro-root-menu` and its rows are `cpro-root-menu_<row>`. CONFIG is the
	// screen the palette's `config` row opens, so its rows are
	// `cpro-root-menu_config_<row>`.
	for i, e := range menu.list.items {
		menu.list.cursor = i
		write("cpro-root-menu-"+e.name, m.viewNormal())
	}

	// The interactive run flow.
	m = newPicker()
	if _, err := m.pushRunAccount(); err != nil {
		t.Fatal(err)
	}
	write("cpro-root-run-account", m.viewRunAccount())
	m.pushRunMode(emailA)
	write("cpro-root-run-account-mode", m.viewRunMode())

	// WATCH MODE, with the Interval row (decision 0045) selected.
	m = newPicker()
	m.pushWatchMode()
	m.stack.current().cursor = len(watchModeItems)
	write("cpro-root-watch-mode", m.viewWatchMode())

	// System credentials submenu.
	m = newPicker()
	m.pushSystem()
	write("cpro-root-menu-system-submenu", m.viewSubmenu())

	// login/logout/remove, including remove's in-app confirm (decision 0042).
	m = newPicker()
	m.pushAccountArg("login")
	m.emailInput = "new@example.com"
	write("cpro-root-menu-login-email", m.viewEmailInput())

	m = newPicker()
	if _, err := m.pushAccountArg("logout"); err != nil {
		t.Fatal(err)
	}
	write("cpro-root-menu-logout-screen", m.viewRunAccount())

	m = newPicker()
	if _, err := m.pushAccountArg("remove"); err != nil {
		t.Fatal(err)
	}
	write("cpro-root-menu-remove-screen", m.viewRunAccount())
	m.chooseAccount(m.stack.current(), emailB)
	write("cpro-root-menu-remove-screen-confirm", m.viewRemoveConfirm())

	// The standalone account pickers.
	exportApp := &accountPickerApp{s: s, color: true, width: width, height: height, accountUsage: usage,
		browseList: newAccountBrowseList([]string{emailA, emailB}), cfg: exportPickerConfig}
	write("cpro-root-menu-system-submenu-select-account", exportApp.viewNormal())

	resumeApp := &accountPickerApp{s: s, color: true, width: width, height: height, accountUsage: usage,
		browseList: newAccountBrowseList([]string{emailA, emailB}), cfg: resumePickerConfig}
	write("cpro-root-resume-account", resumeApp.viewNormal())

	// CONFIG and its sub-screens. The menu itself is captured once per
	// selectable row: the group headings ("Run defaults", "Appearance", "Usage
	// thresholds") aren't selectable and carry no cursor, so one capture per
	// configMenuItem is the whole screen.
	newConfig := func(screen configScreen) *configApp {
		return &configApp{cmd: root, s: s, c: c, color: true, width: width, height: height, stack: newNavStack(screen)}
	}
	cfg := newConfig(screenMenu)
	for i, item := range cfg.menuItems() {
		cfg.cursor = i
		write("cpro-root-menu-config-"+item.key, cfg.viewMenu())
	}

	def := newConfig(screenDefault)
	def.defCursor = 1 + permissionModeIndex("readonly")
	def.defPreview = "readonly"
	write("cpro-root-menu-default-screen", def.viewDefault())

	color := newConfig(screenMenu)
	color.enterColor("accent", "Accent color", orDefault(c.AccentColor, accentMode))
	write("cpro-root-menu-config-color", color.viewColor())

	theme := newConfig(screenMenu)
	theme.enterTheme()
	write("cpro-root-menu-config-theme-picker", theme.viewTheme())

	thresh := newConfig(screenMenu)
	thresh.enterThreshold("warn", "Warning threshold", orDefaultFloat(c.BarWarnThreshold, barWarnThreshold), 1, 99)
	write("cpro-root-menu-config-threshold", thresh.viewThreshold())

	// SESSIONS and its two pickers.
	newSession := func(screen sessionScreen) *sessionApp {
		return &sessionApp{cmd: root, s: s, c: c, color: true, width: width, height: height, stack: newNavStack(screen), accountUsage: usage}
	}
	write("cpro-root-menu-session-screen", newSession(screenSessionMenu).viewSessionMenu())

	// Titles load asynchronously in the real program (loadSessionTitles);
	// run that load synchronously so the capture shows the settled screen.
	loadTitles := func(m *sessionApp) {
		if cmd := m.takePickerLoad(); cmd != nil {
			m.Update(cmd())
		}
	}
	cont := newSession(screenContinuePicker)
	cont.openPicker("continue")
	loadTitles(cont)
	write("cpro-root-menu-session-continue", cont.viewPickerBrowse())

	dest := newSession(screenContinuePicker)
	dest.openPicker("continue")
	loadTitles(dest)
	if entries := dest.picker.items; len(entries) > 0 {
		dest.selectSession(&entries[0])
	}
	write("cpro-root-menu-session-continue-account", dest.viewAccountBrowse())

	// DELETE SESSION (decisions 0051–0053): the browse in delete mode with a
	// checked row and the note it shows when it hid a session a live Claude
	// process is using, then the retype guard Enter opens. The note is seeded
	// directly — the picker's own real detection scans /proc, which a capture
	// generator has no live session to satisfy.
	del := newSession(screenSessionList)
	del.openPicker("delete")
	if len(del.picker.items) > 0 {
		del.deleteSel[sessionEntryKey(del.picker.items[0])] = true
	}
	del.hiddenActive = 1
	write("cpro-root-menu-session-delete", del.viewPickerBrowse())

	delConfirm := newSession(screenSessionList)
	delConfirm.openPicker("delete")
	if entries := delConfirm.picker.items; len(entries) > 0 {
		delConfirm.beginDelete([]sessionEntry{entries[0]})
	}
	write("cpro-root-menu-session-delete-confirm", delConfirm.viewDeleteConfirm())
}
