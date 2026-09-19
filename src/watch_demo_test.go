//go:build linux

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestGenerateWatchDemoFrames writes five real ANSI frames of `cpro status`
// — the exact rendering `cpro watch` redraws on a loop — at different seeded
// Session/Week usage levels, for ../cpro-captures/watch/generate.sh to
// compose into docs/README.md's "Live usage" animation: the bar filling and
// crossing the safe → warn → danger color thresholds (usageColorFor, ui.go),
// then one more frame with the accent color changed via the real `cpro
// config accent` command, to show that's a separate, independently
// configurable axis from the usage-bar color (see ui.go's own comment on
// accentMode/barColorSafe/warningColor/dangerColor being kept semantically
// distinct). Each frame is a real subprocess through a real pty — same
// approach TestStatusViews already uses to get real, colored output
// (announceCommand/accent() only colors through a real terminal fd) — not a
// direct View call, since renderFullView isn't a bubbletea view with its own
// color flag to force; it reads terminalOutput(w) like every plain-printed
// panel in this codebase. Skipped unless CPRO_CAPTURE_DIR is set, same
// convention as TestGenerateScreenCaptures/TestGenerateHeaderDemoFrames.
func TestGenerateWatchDemoFrames(t *testing.T) {
	dir := os.Getenv("CPRO_CAPTURE_DIR")
	if dir == "" {
		t.Skip("set CPRO_CAPTURE_DIR to write watch demo frames")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	bin, s := buildCLI(t)
	const email = "you@example.com"
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

	seed := func(fiveHour, sevenDay float64) {
		t.Helper()
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

	run := func(args ...string) string {
		t.Helper()
		master, slave := openPTY(t)
		resizePTY(t, slave, 40, 120)
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
		slave.Close()
		return capture()
	}

	write := func(name, content string) {
		t.Helper()
		content = strings.ReplaceAll(content, "\r\n", "\n")
		content = strings.TrimRight(content, "\n") + "\n"
		if err := os.WriteFile(filepath.Join(dir, name+".ansi"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	seed(15, 22)
	write("cpro-watch-safe", run("status"))
	seed(58, 60)
	write("cpro-watch-rising", run("status"))
	seed(83, 79) // Session crosses barWarnThreshold(80) first; Week still just under it.
	write("cpro-watch-warn", run("status"))
	seed(96, 91) // Both past barDangerThreshold(90).
	write("cpro-watch-danger", run("status"))

	if out, err := exec.Command(bin, "config", "accent", "blue").CombinedOutput(); err != nil {
		t.Fatalf("config accent blue: %s: %v", out, err)
	}
	write("cpro-watch-accent", run("status"))
}
