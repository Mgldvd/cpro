package main

import (
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
)

// permissionMode is one of the five permission modes cpro config's
// Permissions screen offers (Ask for everything/Edit without asking/
// Read-only/Live a little/YOLO). Args is the exact argv appended to the
// claude invocation for that mode (see applyPermissionDefaults) — this is
// the one and only place a mode's argv is defined; the interactive run
// flow's own RUN MODE step (rootui.go, decision 0019) picks straight from
// this table too, rather than a separate Normal/Danger/Plan system (removed
// entirely — see decision 0004, superseded). Description is the one-line
// summary the Permissions screen shows for the highlighted candidate
// (decision 0038, configui.go's viewPermissions) — it replaced the screen's
// earlier "Command preview" (formatCommandPreview, removed — this was its
// only caller), which rendered the raw argv instead.
type permissionMode struct {
	Key, Label, Color, Description string
	Args                           []string
}

// yoloArgs is the exact argv for "zero permission prompts" — named and
// tested on its own (not inlined into permissionModes' "yolo" entry, its
// only caller) since it's also what tests assert the real invocation
// produces, the same role it played for the old Normal/Danger/Plan picker's
// "danger" case before that picker was removed (decision 0004, superseded by
// decision 0019 — the interactive run flow now picks straight from
// permissionModes too).
//
// `--permission-mode bypassPermissions` (decision 0038) replaced an earlier
// `--dangerously-skip-permissions --add-dir /` — officially documented as
// equivalent (code.claude.com/docs/permission-modes: "La bandera
// --dangerously-skip-permissions es equivalente" to `--permission-mode
// bypassPermissions`), and verified live (Claude Code 2.1.268) to reach zero
// permission_denials on its own, --add-dir included, once
// ensureYOLOSettings/ensureBlockReadsOutsideWorkingDirectories (claude.go,
// decisions 0034/0035) have already written the account's own persisted
// settings.json — which every real invocation does before claude ever
// starts. --add-dir / was doing real, useful work when decision 0026 added
// it (widening the file tools' directory boundary for a statically
// resolvable read, the one thing --dangerously-skip-permissions alone never
// covered), but decision 0035's own `permissions.additionalDirectories: ["/"]`
// in that same settings.json now does the identical job — --add-dir on the
// CLI became redundant with it, not a second, independently-needed source.
// The switch to --permission-mode also has a second benefit: it makes YOLO's
// own argv textually distinct from liveALittleArgs' bare
// --dangerously-skip-permissions again on its own terms (previously --add-dir
// was the only thing telling the two apart at the argv level — see
// yoloEffective, claude.go, and permissionModeForArgs below, both of which
// depend on every mode's Args staying unique), rather than continuing to
// share that flag as an accidental, ambiguity-prone overlap. See decision
// 0026 for the original --add-dir investigation and decision 0038 for this
// one.
func yoloArgs() []string {
	return []string{"--permission-mode", "bypassPermissions"}
}

// liveALittleArgs is "skip normal permission prompts, but keep
// blockReadsOutsideWorkingDirectories intact" — the one setting yoloArgs
// explicitly disables. Distinct from yoloArgs on purpose: this is the
// intermediate mode between Read-only and full YOLO, for someone who wants
// fewer prompts without also opening up reads outside the working directory.
// `--dangerously-skip-permissions` (rather than `--permission-mode
// bypassPermissions`, yoloArgs' own choice since decision 0038) is the only
// flag form that expresses this narrower intent — bypassPermissions itself
// has no "but don't touch the outside-directory read block" variant.
func liveALittleArgs() []string {
	return []string{"--dangerously-skip-permissions"}
}

// permissionModes is the Permissions screen's fixed, ordered catalog — order
// here is display order top to bottom, also its own risk gradient (green ->
// red). Each Description is a short, factual summary of what the mode
// approves without a prompt, sourced from Claude Code's own official docs
// (code.claude.com/docs/permission-modes) for the four modes that map onto a
// native Claude Code permission mode; "live" is cpro's own composition (a
// bare --dangerously-skip-permissions, decision 0017), described in the same
// terms the rest of this file already uses for it.
var permissionModes = []permissionMode{
	{"ask", "Ask for everything", "#34D399", "Prompts for approval before every tool use.", nil},
	{"edit", "Edit without asking", "#FBBF24", "Auto-approves file edits and common filesystem commands in the working directory.", []string{"--permission-mode", "acceptEdits"}},
	{"readonly", "Read-only", "#FB923C", "Explores and reads files without editing them or running write commands.", []string{"--permission-mode", "plan"}},
	{"live", "Live a little", "#FB7185", "Skips normal prompts, but still blocks reads outside the working directory.", liveALittleArgs()},
	{"yolo", "YOLO (no prompts)", "#F87171", "Skips every permission prompt, outside-directory reads included. Isolated environments only.", yoloArgs()},
}

// claudeSupportsFlag reports whether the installed claude binary's own
// --help output documents flag — the one reliable way to know a capability
// like --permission-mode/--settings, or a specific --permission-mode value
// like "bypassPermissions" (both --help substrings, confirmed live against
// Claude Code 2.1.268: `--permission-mode <mode>` lists its accepted values
// inline, "bypassPermissions" among them), actually exists on whatever
// version happens to be installed, without hand-tracking a minimum version
// number that would drift from reality. Shells out once per call:
// callers on a hot path (the Permissions screen's live "Command preview",
// re-rendered on every keystroke) must never call this — only the real run
// path does, right before it would otherwise silently start Claude without
// full "zero prompts" coverage.
func claudeSupportsFlag(flag string) (bool, error) {
	path, err := claudePath()
	if err != nil {
		return false, fmt.Errorf("claude was not found in PATH; install Claude Code first")
	}
	cmd := exec.Command(path, "--help")
	// Explicitly unset CLAUDE_CONFIG_DIR: this probe is a pure capability
	// check, not scoped to any account, and every other bare "claude --help"
	// cpro ever runs (forwarded from a real cpro run) always has it set
	// (claudeCommand always sets it) — clearing it here is a real, if minor,
	// correctness improvement (no account context leaks into a check that
	// isn't about any one account) and happens to also be exactly what lets
	// this probe be told apart from a user-forwarded "claude -- --help" in
	// tests, with no separate signal needed.
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "CLAUDE_CONFIG_DIR=") {
			cmd.Env = append(cmd.Env, v)
		}
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("could not check claude's supported flags: %w", err)
	}
	return strings.Contains(string(out), flag), nil
}

// requireYOLOSupport errors clearly, before Claude ever starts, if the
// installed claude binary doesn't support every flag/value YOLO mode depends
// on to actually reach zero permission prompts — rather than silently
// starting Claude in a more restrictive mode that would still show them.
// Checks for the literal substrings yoloArgs' own two argv elements need to
// be recognized (decision 0038): the --permission-mode flag itself, and the
// bypassPermissions value it must accept.
func requireYOLOSupport() error {
	for _, want := range []string{"--permission-mode", "bypassPermissions"} {
		ok, err := claudeSupportsFlag(want)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("this installed version of claude does not support %s, which YOLO (no prompts) needs for zero permission prompts; update Claude Code or choose a different Permissions mode", want)
		}
	}
	return nil
}

// requireNotRoot errors clearly, before Claude ever starts, if the current
// process is running as root or under sudo (effective UID 0) — the one YOLO
// precondition cpro previously left entirely to Claude's own refusal.
// Confirmed against the official docs (code.claude.com/docs/es/permission-modes,
// "Omitir todas las comprobaciones con modo bypassPermissions"): on Linux and
// macOS, Claude Code itself refuses to start in bypassPermissions —
// regardless of whether it was requested via --dangerously-skip-permissions
// or permissions.defaultMode in settings.json (see ensureYOLOSettings,
// claude.go) — when running as root, with the literal message
// "--dangerously-skip-permissions cannot be used with root/sudo privileges
// for security reasons"; the check is skipped automatically inside a
// recognized sandbox (e.g. a dev container running as a non-root user).
// Without this, that refusal would surface as Claude's own raw stderr rather
// than cpro's usual "fail clearly before Claude starts" convention
// (requireYOLOSupport/requireNoManagedPermissionsPolicy, both below).
func requireNotRoot() error {
	if os.Geteuid() == 0 {
		return fmt.Errorf("YOLO unavailable\n\nClaude Code refuses to start in bypassPermissions mode as root or under sudo, for security reasons. Run cpro as a normal user, or from inside a recognized sandbox/dev container.")
	}
	return nil
}

// managedSettingsNotFetchedMarker is the exact substring `claude doctor`
// prints on an account with no enterprise/organization policy in effect
// (confirmed live against a real Pro account: "Managed settings (remote):
// not fetched — requires an Enterprise or Team subscription"). A managed
// policy IS in effect whenever a "Managed settings (remote):" line is
// present WITHOUT this "not fetched" qualifier — `claude doctor`'s own
// documented purpose ("Reads settings files...") plus its explicit mention
// of "Admin-managed (policy) settings" (--safe-mode's own --help text) is
// what makes this the one real, already-shipped signal cpro can check
// without reading an opaque, potentially-remote-fetched policy file whose
// location and format cpro has no reliable way to know in every deployment.
const managedSettingsNotFetchedMarker = "not fetched"

// requireNoManagedPermissionsPolicy errors clearly, before YOLO is ever
// applied, if the given profile's account has an enterprise/organization
// policy in effect — a higher-precedence settings source cpro has no
// documented, supported way to read the content of (let alone override),
// unlike the account's own ordinary user settings.json (see
// ensureBlockReadsOutsideWorkingDirectories, claude.go). Detection is
// necessarily best-effort: it parses `claude doctor`'s own human-readable
// "Managed settings (remote): ..." line rather than any documented,
// stable API, since Claude Code does not expose one — verified live only
// against a Pro account with no managed policy (reports "not fetched");
// never verified against a real Enterprise/Team deployment, since none was
// available to test against (see decision 0034's own note on this
// limitation). A parse failure or a `claude doctor` that predates this line
// entirely degrades to "no known policy" (nil error) rather than blocking
// YOLO on every installed version — the goal is to catch a policy cpro can
// positively detect, not to assume one exists by default.
func requireNoManagedPermissionsPolicy(profile string) error {
	path, err := claudePath()
	if err != nil {
		return fmt.Errorf("claude was not found in PATH; install Claude Code first")
	}
	cmd := exec.Command(path, "doctor")
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "CLAUDE_CONFIG_DIR=") {
			cmd.Env = append(cmd.Env, v)
		}
	}
	cmd.Env = append(cmd.Env, "CLAUDE_CONFIG_DIR="+profile)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// claude doctor itself failing is not this check's problem to solve —
		// degrade to "no known policy" rather than blocking a legitimate run
		// over an unrelated doctor failure.
		return nil
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "Managed settings (remote):") && !strings.Contains(line, managedSettingsNotFetchedMarker) {
			return fmt.Errorf("YOLO unavailable\n\nA higher-priority Claude policy keeps outside-workspace read protection enabled.\n\ncpro cannot guarantee zero permission prompts.")
		}
	}
	return nil
}

// defaultPermissionMode is what an empty/unset config.PermissionMode means —
// "ask for everything" is Claude's own actual default behavior with no flags
// at all, so this is also what a config predating this feature (omitempty,
// see store.go) transparently falls back to.
const defaultPermissionMode = "ask"

// isPermissionMode reports whether mode is one of permissionModes' own keys.
func isPermissionMode(mode string) bool {
	for _, m := range permissionModes {
		if m.Key == mode {
			return true
		}
	}
	return false
}

// permissionModeNames returns every permissionModes key in display order — the
// usage/error list for the scriptable `cpro config permission-mode`.
func permissionModeNames() []string {
	names := make([]string, len(permissionModes))
	for i, m := range permissionModes {
		names[i] = m.Key
	}
	return names
}

// effectivePermissionMode resolves a possibly-empty or malformed stored mode
// (a config from before this feature existed, or a hand-edited config.json
// with a typo) down to a real, always-valid permissionModes key.
func effectivePermissionMode(mode string) string {
	if isPermissionMode(mode) {
		return mode
	}
	return defaultPermissionMode
}

// permissionModeForAccount resolves the mode a specific account defaults to:
// its own override when one is stored, otherwise the global
// config.PermissionMode (decision 0050). A malformed override is treated as no
// override at all, the same lenient fallback effectivePermissionMode applies to
// a malformed global — a hand-edited config.json can never make a run resolve
// to a mode that doesn't exist. Every run-path reader of the default must go
// through this one function: if the resolver and its callers disagreed, `cpro
// config permission-mode --account` would promise a default that
// applyPermissionDefaults never actually applies.
func permissionModeForAccount(c config, email string) string {
	if override, ok := c.PermissionModeByAccount[email]; ok && isPermissionMode(override) {
		return override
	}
	return effectivePermissionMode(c.PermissionMode)
}

// hasAccountPermissionOverride reports whether email carries a real override of
// its own, as opposed to inheriting the global default — the same validity test
// permissionModeForAccount applies, so a stale or hand-edited value counts as
// no override in the UI exactly as it does when a run resolves the mode. The
// Permissions screen's per-account rows and its per-account editor use it to
// tell "this account is pinned to X" from "this account follows the global
// default" (decision 0054).
func hasAccountPermissionOverride(c config, email string) bool {
	override, ok := c.PermissionModeByAccount[email]
	return ok && isPermissionMode(override)
}

// permissionModeByKey returns the permissionMode for key, resolved through
// effectivePermissionMode first, so it always returns a real entry.
func permissionModeByKey(key string) permissionMode {
	key = effectivePermissionMode(key)
	for _, m := range permissionModes {
		if m.Key == key {
			return m
		}
	}
	panic("unreachable: effectivePermissionMode always returns a valid key")
}

// permissionModeIndex returns key's position in permissionModes — used by the
// Permissions screen to start its cursor on the currently active mode's row
// rather than always the first one, since (unlike the Accent/Usage bar color
// pickers) every mode stays in the list, active one included, so there's
// always a real row to land on.
func permissionModeIndex(key string) int {
	for i, m := range permissionModes {
		if m.Key == key {
			return i
		}
	}
	return 0
}

// permissionModeArgs is the exact argv for mode (see permissionMode.Args).
func permissionModeArgs(mode string) []string {
	return permissionModeByKey(mode).Args
}

// permissionModeForArgs is permissionModeArgs in reverse: the mode whose own
// argv is exactly args, for rendering a human mode label from an argv that was
// already built (announceCommand, ui.go). Reports false when args match no
// mode — arbitrary forwarded Claude flags — so a caller falls back rather than
// labelling them as a mode they aren't. Empty args legitimately match "ask",
// whose own Args are nil.
func permissionModeForArgs(args []string) (permissionMode, bool) {
	for _, m := range permissionModes {
		if slices.Equal(m.Args, args) {
			return m, true
		}
	}
	return permissionMode{}, false
}

// hasArg reports whether flag appears literally among args.
func hasArg(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

// hasArgValue reports whether flag appears among args immediately followed
// by value — the value-aware sibling hasArg's plain presence check can't
// answer (a flag's own name always matches hasArg regardless of what value
// follows it). yoloEffective (claude.go, decision 0038) is the one caller:
// since yoloArgs() moved from two mode-agnostic flags
// (--dangerously-skip-permissions plus --add-dir) to --permission-mode
// bypassPermissions, telling YOLO's own choice of --permission-mode apart
// from edit's (acceptEdits) or readonly's (plan) requires checking the value,
// not just that --permission-mode is present somewhere in args.
func hasArgValue(args []string, flag, value string) bool {
	for i, a := range args {
		if a == flag && i+1 < len(args) && args[i+1] == value {
			return true
		}
	}
	return false
}

// applyPermissionDefaults prepends the configured baseline permission-mode
// arguments to forwarded (the arguments cpro run is about to pass to
// claude), unless forwarded already carries its own permission-mode flag —
// an explicit choice for THIS invocation (a literal CLI flag, or the
// interactive run flow's own RUN MODE step, rootui.go, which synthesizes the
// equivalent flags into the argv it hands back to `cpro run` before this is
// ever called — see decision 0019) always wins over the persisted config
// default; the two are deliberately independent layers, not merged into one,
// so neither has to know about the other.
//
// email is the account this run resolved to, so the baseline can come from
// that account's own override when it has one (decision 0050) — see
// permissionModeForAccount. Callers must pass the resolved account, never "".
func applyPermissionDefaults(c config, email string, forwarded []string) []string {
	if hasArg(forwarded, "--permission-mode") || hasArg(forwarded, "--dangerously-skip-permissions") {
		return forwarded
	}
	baseline := permissionModeArgs(permissionModeForAccount(c, email))
	if len(baseline) == 0 {
		return forwarded
	}
	return append(baseline, forwarded...)
}
