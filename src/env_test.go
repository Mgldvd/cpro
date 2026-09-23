//go:build linux

package main

import (
	"os"
	"testing"
)

// TestMain pins the color environment the suite's assertions are written
// against. The pty and rendering tests assert real truecolor output, so a
// harness or shell exporting NO_COLOR=1 / TERM=dumb (common in CI, editors,
// and agent sandboxes) used to fail them for reasons unrelated to the code —
// the most common false red this suite had, previously only documented as a
// manual "unset NO_COLOR" step. Tests that need plain output still opt in
// with t.Setenv("NO_COLOR", "1"), and every child process (the built cpro,
// the fake claude helper) inherits the pinned environment from here.
func TestMain(m *testing.M) {
	os.Unsetenv("NO_COLOR")
	os.Setenv("TERM", "xterm-256color")
	os.Setenv("COLORTERM", "truecolor")
	// No test may reach Claude's real OAuth token endpoint (decision 0066):
	// a test that needs it points oauthTokenURL at its own local server.
	oauthTokenURL = "http://127.0.0.1:1/oauth-token-endpoint-disabled-in-tests"
	os.Exit(m.Run())
}
