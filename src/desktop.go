//go:build linux

package main

import (
	"os"
	"os/exec"
	"path/filepath"
)

// credentialTargetStatus is one local Claude client's own sanitized outcome
// from a single `cpro system export` call — never a raw success/failure
// bool, since "not installed" and "installed but cannot safely receive this
// credential" are both legitimate, distinct, non-error outcomes (decision
// 0028), not failures of the export itself.
type credentialTargetStatus int

const (
	targetUpdated credentialTargetStatus = iota
	targetNotInstalled
	targetSignInRequired
)

// dot is the same filled/hollow marker onOffMark (configui.go) uses for a
// boolean setting, reused here for a tri-state result: filled only for the
// one outcome that actually changed something.
func (s credentialTargetStatus) dot() string {
	if s == targetUpdated {
		return "●"
	}
	return "○"
}

func (s credentialTargetStatus) label() string {
	switch s {
	case targetUpdated:
		return "Updated"
	case targetNotInstalled:
		return "Not installed"
	case targetSignInRequired:
		return "Sign-in required"
	default:
		return ""
	}
}

// credentialTargetResult is one target's own line in `cpro system export`'s
// per-client report.
type credentialTargetResult struct {
	name   string
	status credentialTargetStatus
	// detail is extra, sanitized context shown only for targetSignInRequired
	// — always a fixed, human-written explanation, never a secret or a raw
	// error message that might embed one.
	detail string
}

// claudeDesktopConfigDir resolves Claude Desktop's own Electron config
// directory. This is deliberately independent of CLAUDE_CONFIG_DIR: that
// variable scopes Claude Code's own per-account profile (claude.go's
// systemConfigPaths, claudeCommand), a cpro/Claude-Code-specific concept
// Desktop's own top-level account session predates and has nothing to do
// with — Desktop has exactly one, single, per-machine config location,
// resolved the same way Electron itself resolves it for any app: under
// XDG_CONFIG_HOME (falling back to ~/.config) by the app's own product name.
func claudeDesktopConfigDir() (string, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "Claude"), nil
}

// claudeDesktopInstalled reports whether Claude Desktop appears to be
// installed on this machine: its own launcher on PATH (the same style of
// check claudePath, maintenance.go, already uses for claude itself), or —
// covering an install that doesn't put a binary on PATH — its own Electron
// config file already existing from at least one real launch.
//
// CPRO_TEST_NO_CLAUDE_DESKTOP forces "not installed" regardless of the real
// machine — the same test-only-override idiom CPRO_TEST_NO_YOLO_SUPPORT
// (permissions.go) already uses — since a test's own PATH (buildCLI,
// main_test.go) prepends rather than replaces the real one, so a dev
// machine that genuinely has Claude Desktop installed would otherwise make
// the "not installed" case untestable.
func claudeDesktopInstalled() bool {
	if os.Getenv("CPRO_TEST_NO_CLAUDE_DESKTOP") == "1" {
		return false
	}
	if _, err := exec.LookPath("claude-desktop"); err == nil {
		return true
	}
	dir, err := claudeDesktopConfigDir()
	if err != nil {
		return false
	}
	info, err := os.Stat(filepath.Join(dir, "config.json"))
	return err == nil && !info.IsDir()
}

// exportToClaudeDesktop reports Claude Desktop's own outcome for a `cpro
// system export` call — it never writes a credential of its own. Verified
// end to end (decision 0028), not assumed: Desktop's top-level account
// session lives in its own Electron config.json under the keys
// "oauth:tokenCache"/"oauth:tokenCacheV2", each a base64 blob whose decoded
// bytes begin with Electron's own safeStorage version marker ("v10" when a
// real OS keyring backs it, "v11" for its own documented, keyring-less
// fallback on this machine) — a real OAuth session issued to Desktop's own,
// separate OAuth client, not the bearer token Claude Code's CLI (and cpro's
// own login flow) obtains. There is no legitimate way to derive or transfer
// one from the other: writing something into that field would mean
// fabricating an auth record for a client cpro never actually authenticated
// against, which the task's own explicit security constraints rule out.
//
// Desktop does separately bundle its own embedded Claude Code engine (used
// for its "cowork"/local-agent features), which reads the exact same
// CLAUDE_CONFIG_DIR-scoped .credentials.json file the standalone `claude`
// CLI does — confirmed live by inspecting Desktop's own app bundle, which
// contains the identical CLAUDE_CONFIG_DIR/.claude.json/.credentials.json
// resolution logic as the CLI. That piece already works with no code change
// needed here, the same way decision 0027 found `cpro session continue`
// already sharing `cpro run`'s own fix — cpro's existing Claude Code export
// (exportAccount, system.go) already satisfies it. Desktop's own top-level
// sign-in dialog, the specific symptom this task reported, is driven by the
// separate, non-transferable session above instead.
func exportToClaudeDesktop() credentialTargetResult {
	const name = "Claude Desktop"
	if !claudeDesktopInstalled() {
		return credentialTargetResult{name: name, status: targetNotInstalled}
	}
	return credentialTargetResult{
		name:   name,
		status: targetSignInRequired,
		detail: "Desktop's own account session is separate from Claude Code's and can't be set from a CLI credential — sign in once in Desktop",
	}
}
