//go:build linux

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestGenerateHeaderDemoFrames writes two extra ANSI frames used only by
// cpro-app/docs' animated header (../cpro-captures/header/generate.sh), not
// by the canonical 48-screen inventory capture_test.go writes (that set stays
// exactly one file per real screen/route; these two are additional cursor
// states of screens that inventory already covers once, chosen specifically
// to narrate "run → pick an account → YOLO → hand off to claude" for the
// header): the RUN MODE screen with YOLO preselected (capture_test.go's own
// cpro-root-run-account-mode instead preselects the account's saved default,
// which is "ask" for its seeded accounts), and the "Running:" announcement
// panel cpro prints just before it execs into claude. Skipped unless
// CPRO_CAPTURE_DIR is set, same convention as TestGenerateScreenCaptures.
func TestGenerateHeaderDemoFrames(t *testing.T) {
	dir := os.Getenv("CPRO_CAPTURE_DIR")
	if dir == "" {
		t.Skip("set CPRO_CAPTURE_DIR to write header demo frames")
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
		c.PermissionModeByAccount = map[string]string{emailB: "yolo"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	root := rootCommand()
	root.InitDefaultHelpCmd()
	root.InitDefaultCompletionCmd()

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

	m := &rootPickerApp{
		root: root, s: s, color: true, width: width, height: height,
		shades:       deriveAccentShades(accentMode, len(rootGroups)),
		accountUsage: usage,
	}
	m.stack = newNavStack(rootFrame{kind: frameList, list: newCommandBrowseList(rootPickerFilteredEntries(root, rootLauncherNames))})
	applyLiveMeta(m.stack.current().list.items, s)

	if _, err := m.pushRunAccount(); err != nil {
		t.Fatal(err)
	}
	// emailB carries a "yolo" per-account override (seeded above), so this
	// preselects YOLO exactly the way a real account with that saved default
	// would — no manual cursor poking needed.
	m.pushRunMode(emailB)
	write("cpro-header-run-mode-yolo", m.viewRunMode())

	// The "Running:" announcement cpro prints just before it execs into
	// claude. announceCommand only colors through a real terminal fd
	// (terminalOutput checks os.File+isatty), so this needs a real pty rather
	// than a bytes.Buffer.
	master, slave := openPTY(t)
	root.SetOut(slave)
	announceCommand(root, emailB, yoloArgs())
	slave.Close()
	got := drainPTY(master)
	time.Sleep(100 * time.Millisecond)
	write("cpro-header-announce-yolo", got())
}
