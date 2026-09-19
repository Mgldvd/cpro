//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

func terminalInput() bool {
	var term syscall.Termios
	_, _, err := syscall.Syscall(syscall.SYS_IOCTL, os.Stdin.Fd(), syscall.TCGETS, uintptr(unsafe.Pointer(&term)))
	return err == 0
}

var authenticationEnvironment = []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR", "CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY"}

func authenticationOverride() string {
	for _, key := range authenticationEnvironment {
		if os.Getenv(key) != "" {
			return key
		}
	}
	return ""
}

func claudeCommand(dir string, args ...string) (*exec.Cmd, error) {
	// Environment authentication overrides would defeat account selection.
	if key := authenticationOverride(); key != "" {
		return nil, fmt.Errorf("%s may override authentication; remove it from the environment to use cpro", key)
	}
	path, err := exec.LookPath("claude")
	if err != nil {
		return nil, fmt.Errorf("claude was not found in PATH; install Claude Code first")
	}
	cmd := exec.Command(path, args...)
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "CLAUDE_CONFIG_DIR=") {
			cmd.Env = append(cmd.Env, value)
		}
	}
	cmd.Env = append(cmd.Env, "CLAUDE_CONFIG_DIR="+dir)
	return cmd, nil
}

type authentication struct {
	LoggedIn   bool   `json:"loggedIn"`
	Email      string `json:"email"`
	AuthMethod string `json:"authMethod"`
}

func authStatus(dir string) (authentication, error) {
	var auth authentication
	cmd, err := claudeCommand(dir, "auth", "status", "--json")
	if err != nil {
		return auth, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	bounded := exec.CommandContext(ctx, cmd.Path, cmd.Args[1:]...)
	bounded.Env = cmd.Env
	cmd = bounded
	b, runErr := cmd.Output()
	var exit *exec.ExitError
	if runErr != nil && (!errors.As(runErr, &exit) || exit.ExitCode() != 1) {
		return auth, fmt.Errorf("could not query claude auth status")
	}
	if err := json.Unmarshal(b, &auth); err != nil {
		return auth, fmt.Errorf("claude auth status returned unexpected JSON; update Claude Code")
	}
	if runErr != nil && auth.LoggedIn {
		return auth, fmt.Errorf("claude auth status returned a contradictory state")
	}
	return auth, nil
}

// validAuth reports whether auth reflects a live session for email, as opposed to a
// stale, logged-out, or mismatched one.
func validAuth(email string, auth authentication) bool {
	return auth.LoggedIn && strings.EqualFold(email, auth.Email) && auth.AuthMethod == "claude.ai"
}

func interactive(cmd *exec.Cmd) error {
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(signals)
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case sig := <-signals:
				_ = cmd.Process.Signal(sig)
			case <-done:
				return
			}
		}
	}()
	return cmd.Wait()
}

func (s *store) login(email string) error {
	if !terminalInput() {
		return fmt.Errorf("login requires an interactive terminal; run cpro login %s in your terminal", email)
	}
	lock, err := s.accountLock(email, true)
	if err != nil {
		return err
	}
	defer lock.Close()
	if _, err := s.read(); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(s.dir, ".login-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	// Seed only preferences/account metadata. Never copy a live refresh token.
	if err := s.seedExistingSettings(email, stage); err != nil {
		return err
	}
	cmd, err := claudeCommand(stage, "auth", "login", "--claudeai", "--email", email)
	if err != nil {
		return err
	}
	if err := interactive(cmd); err != nil {
		return err
	}
	auth, err := authStatus(stage)
	if err != nil {
		return err
	}
	if !validAuth(email, auth) {
		return fmt.Errorf("the session does not match the requested Claude account; run cpro login %s again and select that email in the browser", email)
	}
	return s.installLogin(email, stage)
}

func (s *store) installLogin(email, stage string) error {
	if err := privateDir(s.dir); err != nil {
		return err
	}
	lock, err := fileLock(filepath.Join(s.dir, "config.lock"), true)
	if err != nil {
		return err
	}
	defer lock.Close()
	c, err := s.read()
	if err != nil {
		return err
	}
	return func() (result error) {
		if err := privateDir(filepath.Join(s.dir, "accounts")); err != nil {
			return err
		}
		profile := s.profile(email)
		if err := privateDir(profile); err != nil {
			return err
		}
		// Claude Code on Linux stores auth in these two files. Keep project history
		// and other profile files in place when reauthenticating.
		files := []string{".claude.json", ".credentials.json"}
		old := make(map[string][]byte)
		fresh := make(map[string][]byte)
		for _, name := range files {
			b, err := os.ReadFile(filepath.Join(stage, name))
			if err != nil {
				return fmt.Errorf("login did not create %s; check your Claude Code version", name)
			}
			if !json.Valid(b) {
				return fmt.Errorf("login created invalid JSON in %s", name)
			}
			fresh[name] = b
			b, err = os.ReadFile(filepath.Join(profile, name))
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if err == nil {
				old[name] = b
			}
		}
		written := []string{}
		defer func() {
			if result == nil {
				return
			}
			for _, name := range written {
				path := filepath.Join(profile, name)
				var err error
				if b, exists := old[name]; exists {
					err = atomicWrite(path, b)
				} else {
					err = os.Remove(path)
				}
				result = errors.Join(result, err)
			}
		}()
		for _, name := range files {
			if err := atomicWrite(filepath.Join(profile, name), fresh[name]); err != nil {
				return err
			}
			written = append(written, name)
		}
		c.Accounts[email] = true
		if c.Default == "" {
			c.Default = email
		}
		ensureAccountMask(&c, email)
		return s.save(c)
	}()
}

// seedExistingSettings copies email's existing .claude.json (its
// project-trust/settings history, if this account was ever registered
// before) into stage, so a fresh login or credential import doesn't discard
// it — only ever reading, never a live refresh token, which lives solely in
// .credentials.json. A brand-new account (no prior profile) simply has
// nothing to seed, which is fine: installLogin/installCredentials both
// handle a stage missing this file, in whichever way fits their own caller.
func (s *store) seedExistingSettings(email, stage string) error {
	old, err := os.ReadFile(filepath.Join(s.profile(email), ".claude.json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	return atomicWrite(filepath.Join(stage, ".claude.json"), old)
}

// systemConfigPaths locates the real, unmanaged Claude Code credential store
// — the one a bare `claude` invocation (outside cpro, with CLAUDE_CONFIG_DIR
// unset) reads and writes — which cpro system export/import treat as "the
// system store" to transfer credentials to/from. Honors CLAUDE_CONFIG_DIR
// when the caller's own environment happens to have it set, for the same
// reason claudeCommand does: that's genuinely where a bare `claude` would
// look. Confirmed live in this environment: with CLAUDE_CONFIG_DIR unset,
// real Claude Code keeps .claude.json directly at $HOME/.claude.json (a
// top-level dotfile, predating the .claude/ directory) but .credentials.json
// nested under $HOME/.claude/.credentials.json — an asymmetric pair of
// defaults, not a cpro convention; when CLAUDE_CONFIG_DIR IS set, both files
// sit directly inside it instead, exactly like a cpro profile already does
// (see installLogin's own two-file handling).
func systemConfigPaths() (claudeJSON, credentials string, err error) {
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return filepath.Join(dir, ".claude.json"), filepath.Join(dir, ".credentials.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", err
	}
	return filepath.Join(home, ".claude.json"), filepath.Join(home, ".claude", ".credentials.json"), nil
}

// installCredentialsFile atomically writes credentials — the raw bytes of an
// already-validated .credentials.json — into email's profile, registering
// the account in config.json if it wasn't already there, and rolling the
// credentials file back (restoring the previous one, or removing it for a
// brand-new account) if registering fails partway through. Unlike
// installLogin, it never touches .claude.json at all: an existing account's
// settings/trust history is left exactly as it is, and a brand-new account
// simply has none yet, the same as any freshly created profile before its
// first real login. It never sets or changes config.Default either — cpro
// system import must never introduce default-account behavior (see
// newSystemCommand, system.go).
func (s *store) installCredentialsFile(email string, credentials []byte) (created bool, err error) {
	if err := privateDir(s.dir); err != nil {
		return false, err
	}
	lock, err := fileLock(filepath.Join(s.dir, "config.lock"), true)
	if err != nil {
		return false, err
	}
	defer lock.Close()
	c, err := s.read()
	if err != nil {
		return false, err
	}
	created = !c.Accounts[email]

	if err := privateDir(filepath.Join(s.dir, "accounts")); err != nil {
		return false, err
	}
	profile := s.profile(email)
	if err := privateDir(profile); err != nil {
		return false, err
	}
	path := filepath.Join(profile, ".credentials.json")
	old, oldErr := os.ReadFile(path)
	hadOld := oldErr == nil
	if oldErr != nil && !errors.Is(oldErr, os.ErrNotExist) {
		return false, oldErr
	}
	if err := atomicWrite(path, credentials); err != nil {
		return false, err
	}
	c.Accounts[email] = true
	ensureAccountMask(&c, email)
	if err := s.save(c); err != nil {
		if hadOld {
			err = errors.Join(err, atomicWrite(path, old))
		} else {
			err = errors.Join(err, os.Remove(path))
		}
		return false, err
	}
	return created, nil
}

func (s *store) run(email string, args []string) error {
	// Resolve before locking: an empty email (no --account, no interactive
	// picker anymore — see decision 0019) falls back to the configured
	// default account, or errors clearly if none is set. Every subsequent
	// use of email in this function is the resolved value, never the raw
	// (possibly empty) parameter.
	email, err := s.resolve(email)
	if err != nil {
		return err
	}
	lock, err := s.accountLock(email, false)
	if err != nil {
		return err
	}
	defer lock.Close()
	auth, err := authStatus(s.profile(email))
	if err != nil {
		return err
	}
	if !validAuth(email, auth) {
		return fmt.Errorf("invalid session; run cpro login %s", email)
	}
	c, err := s.read()
	if err != nil {
		return err
	}
	// YOLO explicitly promises "no prompts" — the separate folder-trust
	// dialog would be exactly that kind of unexpected prompt if AutoTrust
	// itself happened to be off, so YOLO implies trust here too. The two
	// settings stay independent config fields (AutoTrust is never mutated or
	// merged into PermissionMode); this only ORs their effect for the one
	// side effect (markTrusted) that actually suppresses a prompt. The mode
	// is this account's own resolved default, override included (decision
	// 0050) — not the raw global, which a per-account override may supersede.
	if c.AutoTrust || permissionModeForAccount(c, email) == "yolo" {
		if err := markTrusted(s.profile(email)); err != nil {
			return err
		}
	}
	// The configured permission mode and blocked commands (see permissions.go)
	// are a baseline applied to every run, not just an interactive default — an
	// explicit choice for this invocation (a literal CLI flag, or cpro run's own
	// interactive mode picker, already reflected in args by the time run is
	// called) still wins; see applyPermissionDefaults.
	if permissionModeForAccount(c, email) == "yolo" && !hasArg(args, "--permission-mode") && !hasArg(args, "--dangerously-skip-permissions") {
		// The configured YOLO default (not an explicit per-invocation choice,
		// which already skips this baseline entirely — see
		// applyPermissionDefaults) is about to be applied. Fail clearly here,
		// before Claude ever starts, if this installed version can't actually
		// deliver zero permission prompts, rather than launching it anyway.
		if err := requireYOLOSupport(); err != nil {
			return err
		}
		// A higher-precedence enterprise/organization policy is a source
		// cpro cannot read the content of, let alone override the way
		// ensureBlockReadsOutsideWorkingDirectories overrides the account's
		// own ordinary user settings.json below — fail clearly rather than
		// silently launch a session labeled "YOLO (no prompts)" that this
		// account's own policy could still interrupt (decision 0034).
		if err := requireNoManagedPermissionsPolicy(s.profile(email)); err != nil {
			return err
		}
		// Claude Code itself refuses bypassPermissions as root/sudo — fail
		// clearly here too, before Claude starts, rather than let that
		// refusal surface as Claude's own raw stderr (decision 0035).
		if err := requireNotRoot(); err != nil {
			return err
		}
	}
	args = applyPermissionDefaults(c, email, args)
	// permissions.blockReadsOutsideWorkingDirectories governs more than the
	// direct file-tool read decision 0026 originally widened via --add-dir
	// (superseded by decision 0038 — see yoloArgs, permissions.go): when
	// Claude's own shell parser cannot statically analyze a Bash command's
	// eventual target path — a plain variable expansion, an inline
	// interpreter (python3 -c ...), a runtime-determined find argument, any
	// other computed path — it falls back to consulting this setting's raw
	// persisted value directly, and denies (or, interactively, prompts "Do
	// you want to proceed?") whenever it's still true, regardless of
	// --dangerously-skip-permissions/--permission-mode bypassPermissions or a
	// CLI --settings override (all verified powerless against this exact key
	// once an account's own settings.json already carries it — see decision
	// 0034). The final args (not just the configured default) decide the
	// intended mode here, since an explicit forwarded --permission-mode
	// bypassPermissions (a literal flag, or the interactive RUN MODE step's
	// own synthesized argv) means YOLO just as much as the saved default
	// does. Every real invocation restores the block for anything that is
	// not this exact value, so leaving YOLO for a later
	// Ask/Edit/Read-only/Live a little run re-enables the restriction rather
	// than leaving it disabled forever from one earlier YOLO run.
	//
	// hasArgValue, not hasArg, is required here (decision 0038): edit and
	// readonly also pass --permission-mode, with a different value
	// (acceptEdits/plan) — a plain hasArg(args, "--permission-mode") would
	// misclassify either of those as YOLO too.
	yoloEffective := hasArgValue(args, "--permission-mode", "bypassPermissions")
	if err := ensureBlockReadsOutsideWorkingDirectories(s.profile(email), !yoloEffective); err != nil {
		return err
	}
	// Declares the same "zero prompts, every directory readable" intent
	// permissions.blockReadsOutsideWorkingDirectories=false already
	// participates in, but through the two settings.json keys Claude Code
	// documents as the config-only equivalent of the CLI flags above
	// (decision 0035) — belt-and-suspenders with the --dangerously-skip-
	// permissions/--add-dir argv yoloArgs() still sends, not a replacement
	// for it: keeping both argv and settings.json aligned is what let this
	// account run pure config-driven YOLO (zero CLI permission flags at
	// all) in the real invocation that validated this change, and what
	// keeps yoloEffective's own argv-based mode detection (permissionModeForArgs,
	// the Command preview) unchanged.
	if err := ensureYOLOSettings(s.profile(email), yoloEffective); err != nil {
		return err
	}
	cmd, err := claudeCommand(s.profile(email), args...)
	if err != nil {
		return err
	}
	// Replace cpro so terminal control, signals and exit status belong to Claude.
	// Inherit the shared lock to prevent login/logout/remove during this session.
	_, _, errno := syscall.Syscall(syscall.SYS_FCNTL, lock.Fd(), syscall.F_SETFD, 0)
	if errno != 0 {
		return errno
	}
	return syscall.Exec(cmd.Path, cmd.Args, cmd.Env)
}

// markTrusted marks the current directory as trusted in the account's .claude.json,
// matching the field Claude Code itself writes after "Do you trust this folder?" is
// accepted, so that dialog does not appear. Unknown fields and other projects in the
// file are preserved.
//
// It fabricates a minimal {"hasTrustDialogAccepted": true} entry when the directory
// has no project entry yet, rather than only ever updating one Claude Code already
// created. An earlier version of this function refused to do that, on the belief
// that a minimal fabricated entry (missing the many fields — allowedTools,
// mcpServers, ... — a real Claude Code session eventually adds) broke Claude Code's
// own startup for that directory. Re-verified live against the currently installed
// Claude Code (2.1.277/278): a minimal entry, added to an account's existing,
// already-onboarded .claude.json for a directory it had never seen before, produces
// a completely normal session — no trust dialog, no broken login detection — both
// under a plain invocation and under yoloArgs()'s own
// --permission-mode bypassPermissions. This matters specifically for YOLO: with the
// old refusal, the very first run of an account in any given directory still showed
// the real trust prompt once even under YOLO, contradicting its own "zero prompts"
// guarantee — reported live. The one directory this still can't help with is $HOME
// itself: Claude Code deliberately never persists trust for it (confirmed against
// current behavior), so a YOLO run from $HOME still prompts once, same as before.
//
// dir is resolved through filepath.EvalSymlinks (falling back to the raw path if
// that fails, e.g. a not-yet-existing directory) before use as the lookup/write
// key: Claude Code's own project keys are the resolved real path — confirmed by
// inspecting a real, populated .claude.json, which held two near-duplicate entries
// for the same logical directory reached through different symlinked prefixes — so
// writing the raw, unresolved path here could silently create a second, ineffective
// entry alongside the one Claude Code itself actually looks up.
func markTrusted(profile string) error {
	dir, err := os.Getwd()
	if err != nil {
		return err
	}
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	path := filepath.Join(profile, ".claude.json")
	data := map[string]any{}
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &data); err != nil {
			return fmt.Errorf("invalid .claude.json: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	projects, _ := data["projects"].(map[string]any)
	if projects == nil {
		projects = map[string]any{}
	}
	project, ok := projects[dir].(map[string]any)
	if !ok {
		project = map[string]any{}
	}
	if trusted, _ := project["hasTrustDialogAccepted"].(bool); trusted {
		return nil
	}
	project["hasTrustDialogAccepted"] = true
	projects[dir] = project
	data["projects"] = projects
	b, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(path, append(b, '\n'))
}

// ensureBlockReadsOutsideWorkingDirectories sets the account's own persisted
// user-level settings.json — CLAUDE_CONFIG_DIR/settings.json, the file
// Claude Code itself reads and writes for e.g. "remember my choice" after an
// interactive permission prompt — so its
// permissions.blockReadsOutsideWorkingDirectories key equals blocked,
// preserving every other existing key (theme,
// skipDangerousModePermissionPrompt, any other setting already there) via
// the same generic map[string]any round-trip markTrusted uses for
// .claude.json. Creates the file if it does not exist yet (unlike
// markTrusted's deliberate refusal to fabricate a new .claude.json project
// entry: settings.json has no required-field shape to get wrong — a fresh
// file containing only this one key was verified live to start Claude Code
// normally, auth included). A no-op (no write at all) when the persisted
// value already equals blocked, so a run doesn't needlessly rewrite the file
// every single time.
//
// This is the fix decision 0034 investigated and added: --add-dir (decision
// 0026) only widens the allowlist a *statically analyzable* file-tool read
// is checked against; it does nothing for the shell-parser-fallback case
// (simple variable expansion, inline interpreters, a runtime-determined find
// argument, any other computed path) Claude cannot analyze in the first
// place, which instead consults this setting's own raw persisted value
// directly. A CLI --settings JSON/file override and a project or local
// settings.json were both verified, end to end against a real invocation, to
// be silently outranked by whatever this exact file already has — so this is
// the only source that reliably reaches the shell-parser-fallback path.
// Called unconditionally from store.run for every real invocation (see the
// call site's own comment), never only when applying YOLO, which is what
// keeps this from becoming a permanent, cross-mode side effect: a later
// non-YOLO run for the same account restores blocked=true, so this doesn't
// silently reopen the restriction forever the first time an account ever
// tries YOLO.
func ensureBlockReadsOutsideWorkingDirectories(profile string, blocked bool) error {
	path := filepath.Join(profile, "settings.json")
	data := map[string]any{}
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &data); err != nil {
			return fmt.Errorf("invalid settings.json: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	permissions, _ := data["permissions"].(map[string]any)
	if permissions == nil {
		permissions = map[string]any{}
	}
	if current, ok := permissions["blockReadsOutsideWorkingDirectories"].(bool); ok && current == blocked {
		return nil
	}
	permissions["blockReadsOutsideWorkingDirectories"] = blocked
	data["permissions"] = permissions
	b, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(path, append(b, '\n'))
}

// skipDangerousModePermissionPromptKey is a top-level settings.json key —
// a sibling of "permissions", never nested under it — that suppresses
// Claude Code's own one-time-per-launch "WARNING: Claude Code running in
// Bypass Permissions mode / Yes, I accept" confirmation screen. Undocumented
// on the official settings-reference page as of this writing; confirmed by
// its own community documentation and by live testing on this machine: with
// permissions.defaultMode already "bypassPermissions" (this same function's
// other effect) but this key absent, a real *interactive* `claude` launch —
// with no CLI flag at all, i.e. exactly what `cpro run`'s exec into a normal
// session looks like — still stopped at that confirmation screen every time,
// a real prompt YOLO's own "zero prompts" promise cannot claim to keep
// otherwise. Every cpro test that had verified "zero prompts" before this
// fix did so through `-p`/headless real-`claude` calls or the fake test
// helper, neither of which this screen appears in at all — a gap in what was
// actually being checked, not evidence it was already fine.
const skipDangerousModePermissionPromptKey = "skipDangerousModePermissionPrompt"

// ensureYOLOSettings sets or clears permissions.defaultMode,
// permissions.additionalDirectories, and the top-level
// skipDangerousModePermissionPrompt in the account's own persisted
// CLAUDE_CONFIG_DIR/settings.json — the same file and the same generic
// map[string]any round-trip ensureBlockReadsOutsideWorkingDirectories already
// uses. The first two were added by decision 0035 after cross-checking the
// official docs (code.claude.com/docs/es/permission-modes,
// code.claude.com/docs/es/settings): permissions.defaultMode:
// "bypassPermissions" is documented as settable from "configuración de
// usuario, --settings o administrada", and this account's own settings.json
// is exactly the "user" tier for every invocation cpro makes, since
// claudeCommand always points CLAUDE_CONFIG_DIR at it — never the real,
// system-wide ~/.claude. skipDangerousModePermissionPromptKey was added by
// decision 0058, for the reason its own doc comment above gives. Verified
// live against Claude Code 2.1.278: with all three keys set and
// permissions.blockReadsOutsideWorkingDirectories already false, a real
// *interactive* session reaches zero prompts of any kind — no bypass-mode
// confirmation, no CLI permission flags needed at all — including every
// shell-parser-fallback case decision 0034 fixed (a runtime-resolved path
// via env var expansion or python3 -c) and, newly verified here, a command
// with more than one `cd` and a subshell, the two concrete examples Claude
// Code's own docs give for that same fallback.
//
// enabled false removes all three keys entirely rather than writing an "off"
// value — there is no meaningful non-bypass value for defaultMode this
// function should assert, only that this account stops forcing
// bypassPermissions (and skipping its own warning about doing so) once some
// other mode is what actually applies, so that mode's own
// --permission-mode/configured default governs undisturbed on the very next
// run. Only ever touches a key whose current value is exactly what this
// function itself would have set (yoloOwned below), so a value a person
// configured by hand — a custom additionalDirectories list, a different
// defaultMode, or their own prior "Yes, I accept" already recorded here —
// is left alone rather than silently overwritten or deleted. A no-op (no
// write at all) when the persisted state already matches the requested one,
// mirroring ensureBlockReadsOutsideWorkingDirectories.
func ensureYOLOSettings(profile string, enabled bool) error {
	path := filepath.Join(profile, "settings.json")
	data := map[string]any{}
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &data); err != nil {
			return fmt.Errorf("invalid settings.json: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	permissions, _ := data["permissions"].(map[string]any)
	if permissions == nil {
		permissions = map[string]any{}
	}
	mode, _ := permissions["defaultMode"].(string)
	dirs, _ := permissions["additionalDirectories"].([]any)
	skip, _ := data[skipDangerousModePermissionPromptKey].(bool)
	yoloOwned := mode == "bypassPermissions" && len(dirs) == 1 && dirs[0] == "/" && skip
	switch {
	case enabled && !yoloOwned:
		permissions["defaultMode"] = "bypassPermissions"
		permissions["additionalDirectories"] = []string{"/"}
		data[skipDangerousModePermissionPromptKey] = true
	case !enabled && yoloOwned:
		delete(permissions, "defaultMode")
		delete(permissions, "additionalDirectories")
		delete(data, skipDangerousModePermissionPromptKey)
	default:
		return nil
	}
	data["permissions"] = permissions
	b, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(path, append(b, '\n'))
}

// runningSession describes one live claude process using a given profile.
type runningSession struct {
	PID       int
	Directory string
	Since     time.Time // zero if the process's start time could not be determined
}

// runningSessions finds real, interactive claude processes currently running with
// CLAUDE_CONFIG_DIR set to profile, by scanning /proc — the only place this
// information exists, since accountLock's flock is shared across concurrent runs of
// the same account and can't distinguish or locate them individually. It only
// counts processes attached to a real terminal (checked via fd 0), which excludes
// claude's own background daemon/bg-pty-host/bg-spare helper processes that also
// carry the same CLAUDE_CONFIG_DIR but have no controlling tty. Best-effort: a
// process that exits mid-scan, or whose /proc entries aren't readable, is skipped
// rather than failing the whole scan — this is informational, not authoritative.
func runningSessions(profile string) []runningSession {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	want := []byte("CLAUDE_CONFIG_DIR=" + profile)
	var sessions []runningSession
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		environ, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
		if err != nil || !hasEnvVar(environ, want) {
			continue
		}
		tty, err := os.Readlink(fmt.Sprintf("/proc/%d/fd/0", pid))
		if err != nil || !(strings.HasPrefix(tty, "/dev/pts/") || strings.HasPrefix(tty, "/dev/tty")) {
			continue
		}
		dir, err := os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid))
		if err != nil {
			continue
		}
		since, _ := processStartTime(pid)
		sessions = append(sessions, runningSession{PID: pid, Directory: dir, Since: since})
	}
	sort.Slice(sessions, func(i, j int) bool {
		if sessions[i].Directory != sessions[j].Directory {
			return sessions[i].Directory < sessions[j].Directory
		}
		return sessions[i].PID < sessions[j].PID
	})
	return sessions
}

// hasEnvVar reports whether environ (a /proc/<pid>/environ NUL-separated dump)
// contains want as one exact "KEY=VALUE" entry.
func hasEnvVar(environ, want []byte) bool {
	for _, kv := range bytes.Split(environ, []byte{0}) {
		if bytes.Equal(kv, want) {
			return true
		}
	}
	return false
}

// processStartTime approximates pid's start time from /proc/<pid>/stat's starttime
// field (in clock ticks since boot) and /proc/uptime, assuming the standard Linux
// USER_HZ of 100 ticks/second.
func processStartTime(pid int) (time.Time, error) {
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return time.Time{}, err
	}
	// comm (2nd field) is user-controlled and parenthesized; split after its closing
	// paren so later fields can't be confused by spaces or parens within it.
	i := bytes.LastIndexByte(stat, ')')
	if i < 0 {
		return time.Time{}, fmt.Errorf("unexpected /proc/%d/stat format", pid)
	}
	fields := strings.Fields(string(stat[i+1:]))
	const starttimeField = 19 // fields[0] is state (3rd overall); starttime is 22nd overall
	if len(fields) <= starttimeField {
		return time.Time{}, fmt.Errorf("short /proc/%d/stat", pid)
	}
	ticks, err := strconv.ParseInt(fields[starttimeField], 10, 64)
	if err != nil {
		return time.Time{}, err
	}
	uptime, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return time.Time{}, err
	}
	uptimeSeconds, err := strconv.ParseFloat(strings.Fields(string(uptime))[0], 64)
	if err != nil {
		return time.Time{}, err
	}
	const userHZ = 100
	bootTime := time.Now().Add(-time.Duration(uptimeSeconds * float64(time.Second)))
	return bootTime.Add(time.Duration(ticks) * time.Second / userHZ), nil
}
