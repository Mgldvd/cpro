# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`cpro` is a Linux CLI (Go, Cobra-based) that manages multiple Claude Code accounts by
email, each with isolated authentication, settings, and history. It wraps the `claude`
executable, setting `CLAUDE_CONFIG_DIR` per invocation so each account gets its own
`.claude.json` / `.credentials.json`. See README.md for full user-facing behavior
(commands, JSON schemas, environment variables) — that detail is not duplicated here.

## Project knowledge

Project knowledge lives in `.memory/`. Before meaningful work, read `.memory/index.md`
and follow only the documentation relevant to the task; read `.memory/STATUS.md` when
current/in-flight state matters. Treat `.memory/decisions/` as durable rationale for
significant choices. If a code change makes project knowledge false or materially
incomplete, update the affected `.memory/` document in the same change.

**Before changing any screen**, read
[`.memory/ux/cpro-screen-map.md`](.memory/ux/cpro-screen-map.md) — the
user-maintained Obsidian canvas mapping every screen, the navigation between them,
and the open UX items annotated in place on it (a scrolling bug, three commands that
still need in-app account pickers). It is the source of truth for what the UI looks
like and what is pending on it, where
[`.memory/workflows/command-and-permission-flows.md`](.memory/workflows/command-and-permission-flows.md)
is the source of truth for the code underneath. When a change alters a screen the
canvas captures, say so, so its screenshots can be re-taken.

## Commands

Source lives in `src/`; `go.mod`/`go.sum` stay at the repo root (the module root),
so build/install target `./src` explicitly while `go vet`/`go test`'s `./...` still
find it on their own:

```bash
go build -o bin/cpro ./src    # build
go install ./src              # install to $(go env GOPATH)/bin
go vet ./...
go test ./...                 # full suite (builds bin/cpro and a fake `claude`)
go test -run TestCLI ./...    # single test
go test -run TestCLI/subtest ./...  # single subtest (TestCLI uses t.Run)
```

All source files are tagged `//go:build linux`; this is a Linux-only tool (uses
`syscall.Exec`, `flock`, `TCGETS`, etc.) and won't build elsewhere.

## Architecture

Eighteen files (all under `src/`), one package (`main`), each with a distinct responsibility:

- **main.go** — builds the Cobra command tree (`rootCommand`). Owns argument parsing,
  including the hand-rolled `runArgs` parser for `cpro run`: Cobra flag parsing is
  disabled (`DisableFlagParsing: true`) so Claude's own arguments pass through
  untouched, and only a leading `--account`/`--account=`/`--`/`--help` is consumed by
  `cpro` itself. `main()` also runs `extractRootResumeInvocation` (decision 0032)
  once, before `fang.Execute`/cobra ever parses `os.Args`: a literal leading
  `--resume`/`-r`/`--resume=ID` is rewritten into the hidden `__resume` command's
  own argv (`newResumeCommand`, `Hidden: true`, `DisableFlagParsing: true` — excluded
  from cobra's help and from `TestCommandTreeAudit`'s picker-reachability check,
  since it's never invoked by its literal name), parsed the rest of the way by
  `parseResumeArgs`; this happens outside cobra's own flag machinery entirely
  because pflag cannot forward an *unknown* flag's value through to `RunE` (verified
  against the vendored pflag source — `ParseErrorsAllowlist.UnknownFlags` silently
  drops both the flag and the next token). Every other invocation, including a real
  `cpro run --account EMAIL -- --resume` forwarding `--resume` as a literal Claude
  argument, is left untouched — see decision 0032, and `resumeSession`/
  `findSessionOwner`/`migrateSession` (session.go) for the actual implementation.
  There is no `use` command (decision 0001). `run` itself is
  unconditionally non-interactive (decision 0019) — it never prompts, regardless of
  TTY state: its RunE is just `runArgs` → `s.run(email, forwarded)`, full stop.
  `s.run` (claude.go) resolves an empty `email` to the configured default account
  (`config.Default`, set via Settings' "Run defaults" group or `cpro config account
  EMAIL` — `s.resolve`, store.go), failing clearly
  (`"no default account configured; set one with: cpro config"`) if neither
  `--account` nor a default is available — this account (or an explicit `--account`)
  applies only to that one invocation and is never written back as a new default.
  Its permission mode comes from forwarded Claude flags or the saved Permissions
  default (`applyPermissionDefaults`, permissions.go), unchanged. The interactive
  account/mode-picking flow this command used to run inline (`pickAccount`/
  `pickRunMode`, now removed) moved entirely into bare `cpro`'s own root picker as a
  4-step flow — see rootui.go, below. `list` (aliased `ls`) and
  `status` split what used to be one command doing both jobs: `list` is a
  lightweight account listing — identity and authentication state only, via
  `loadAccountEmails` and `fetchAuthStates` (both concurrent-`authStatus`-only,
  no usage fetch, no `/proc` session scan) — rendered as a compact `renderAccountList`
  panel in a terminal or a one-line-per-account/`--json` summary otherwise; it
  never shows usage, sessions, PIDs, or a selection cursor, and is purely
  informational the same way the old combined command was: it never fails
  because an account is unauthenticated, only because the config itself can't
  be read. `status` is the detailed dashboard (moved here unchanged from the
  old `list`): it fetches usage, `authStatus`, and `runningSessions` (claude.go)
  for every account in parallel goroutines (usage only when rendering the
  interactive view; auth status and sessions always, since they drive the JSON
  `authenticated`/`sessions` fields and the plain-text session-count output),
  and in a terminal `renderAccountSnapshot` renders the fetched state as one of
  two views sharing the visual language also used by `cpro config`/the root
  picker (configui.go/rootui.go) — drawn in `currentTheme`'s own runes
  (theme.go — `Top()`/`Rail()`/`Divider()`/`Bottom()`, plus `TopRight`/
  `BottomRight` since decision 0033), never a hardcoded "╭─"/"│"/"├─"/"╰─"/
  "╮"/"╯", so `cpro config`'s Theme setting applies here too —
  `renderFullView` (decision 0033: one independent, fully boxed card per
  account — no outer "Accounts" panel, no "├─" divider, exactly one blank
  terminal line between cards; each card's own top border carries email/
  auth-status/session-count entirely, `"╭─ EMAIL ──── ● Authenticated · N
  sessions ────╮"`, no separate header line inside; minimal `S`/`W` usage
  rows, one blank row then flat `"· path  pid  ·  runtime"` session rows, no
  "Active sessions"/"PID"/"running" labels or tree characters; a plain,
  unboxed "Total week" line outside every card) or, `--compact`,
  `renderCompactView` (one row per account, unchanged by decision 0033, which
  also owns the further compact-narrow fallback to a single glyph per bar on a
  terminal too narrow for the 16-glyph proportional one — see `outputWidth`/
  ui.go). `renderFullView`'s own `statusCardLayout` (`newStatusCardLayout`) is
  computed exactly once per render, from every account's own data (the
  longest pid, the longest reset/runtime duration actually seen), never
  per-card — this is what keeps every card's bar/percentage/reset/path/pid/
  runtime column landing in the same visible column across every account
  regardless of which one happens to have the longest value; `statusUsageLine`/
  `statusSessionLine` render one row against that shared layout, and
  `truncatePath` (ui.go) is what absorbs a narrow terminal into the one
  flexible column (path) rather than ever wrapping a row. Neither view marks a
  "current"/default account (there is no such concept
  to display anymore); every account renders on equal footing. Both take the
  real terminal width into account and are the shared rendering behind every
  `cpro watch` redraw too. `list` and `status` both start from the same
  `loadAccountEmails` (main.go) — the one place either reads the account
  registry — so neither can drift from the other on ordering or what counts
  as "registered"; adding a data source either command needs belongs there,
  not duplicated into both RunE closures. `ensureEmailMasks` (main.go,
  decision 0024) is a defense-in-depth repair pass, not the primary masking
  mechanism any more — see ui.go's `displayEmail` below for that; both
  commands' terminal and non-terminal/piped output paths call it before
  rendering, then read every account's email through the shared, live
  `displayEmail` formatter, never a raw string. `root.RunE` (bare `cpro`) and the `menu` command it also registers
  (`cpro menu`) both call `pickCommandArgs` (rootui.go), differing only in
  which name list they pass — `rootLauncherNames` (run/status/watch/menu) for
  the former, `rootMenuNames()` (every other registered command, "menu"
  itself excluded so it can't nest) for the latter — see rootui.go for why
  that's one shared picker component rather than two.
- **claude.go** — everything that touches the real `claude` process: building the
  child command with the right `CLAUDE_CONFIG_DIR` and environment, `login` (stages a
  fresh login in a temp dir, verifies the resulting email/authMethod, then atomically
  swaps it into the profile — `installLogin`, its own account-registration step, calls
  `ensureAccountMask` (config.go, decision 0024) right before persisting, so a
  freshly logged-in account already has its own alias if masking happens to be
  on, rather than showing its real email until the next screen that happens to
  repair a missing one; `installCredentialsFile`, system.go's sibling for
  `cpro system import`, does the same), `run` (applies `markTrusted` when auto-trust is on OR
  the effective permission mode is YOLO — decision 0018, so YOLO's own "zero prompts"
  promise isn't undercut by a separate folder-trust dialog when auto-trust itself is
  off; the two settings stay independent fields, only this one decision ORs their
  effect — then execs `claude` via `syscall.Exec`, replacing the `cpro` process so
  signals/terminal/
  exit status pass through natively), `authStatus` (wraps `claude auth status --json`),
  `markTrusted` (sets `hasTrustDialogAccepted: true` for the current directory in the
  profile's `.claude.json` — the same field Claude Code itself sets when its "trust
  this folder" prompt is accepted — via a generic `map[string]any` round-trip that
  preserves every other field and project entry; **it fabricates a minimal
  `{"hasTrustDialogAccepted": true}` entry when the directory has none yet** —
  decision 0005 originally refused to do this, on the belief that a minimal
  fabricated entry broke Claude Code's own startup for that directory; re-verified
  live against the currently installed Claude Code (2.1.277/278) that a minimal
  entry added to an account's existing, already-onboarded `.claude.json` produces a
  completely normal session, under both a plain invocation and YOLO's own
  `--permission-mode bypassPermissions` — see decision 0056, which supersedes 0005.
  This matters specifically for YOLO: under the old refusal, the very first run of an
  account in any given directory still showed the real trust prompt once even under
  YOLO, contradicting its own "zero prompts" guarantee. The lookup/write key is
  resolved through `filepath.EvalSymlinks` first (falling back to the raw path if
  that fails) — Claude Code's own project keys are the resolved real path, confirmed
  by a real, populated `.claude.json` holding two near-duplicate entries for the same
  logical directory reached through different symlinked prefixes. `$HOME` itself is
  the one directory this still can't help with: Claude Code deliberately never
  persists trust for it), `validAuth` (the shared
  "is this a live, matching, claude.ai session" predicate used by `run`, `login`,
  `doctor`, and `list`/`status` — keep it the single source of truth rather than
  re-deriving the check), and `runningSessions` (finds real, currently-running
  interactive `claude` processes for a profile by scanning `/proc`: matches
  `CLAUDE_CONFIG_DIR` in each pid's `environ` — the only place this account↔process
  mapping exists, since `accountLock`'s flock can't distinguish concurrent holders —
  and requires fd 0 to resolve to a real `/dev/pts`/`/dev/tty`, which is what
  excludes Claude Code's own background daemon/bg-pty-host/bg-spare helpers and a
  headless/scripted `cpro run`, all of which carry the same env var but no attached
  terminal; `processStartTime` reads `/proc/<pid>/stat`'s `starttime` field plus
  `/proc/uptime`, assuming the standard Linux `USER_HZ` of 100, to get each
  session's start time. Best-effort throughout: any pid that vanishes or can't be
  read mid-scan is skipped, never fails the scan).
- **store.go** — the on-disk model: `config.json` under `$XDG_CONFIG_HOME/cpro` (or
  `~/.config/cpro`) lists registered emails, the default (`config.Default` — set by
  `login`'s first account, cleared by `remove` if that account goes; since decision
  0019 this is also what non-interactive `cpro run` resolves an empty `--account`
  to, not just a `list`/`status` display field), and `AutoTrust`; each account's
  profile lives in `accounts/<sha256(email)>/`. `resolve(email)` (called from
  `s.run`, claude.go) is the one place "empty email → fall back to `config.Default`,
  error clearly if still empty" is decided — a new command needing the same
  behavior should call it, not re-derive the fallback. `config.LastMode` (per-email
  last-chosen `run` mode) is gone — decision 0004, superseded by 0019: the
  interactive run flow's own RUN MODE step preselects from the single saved
  `PermissionMode` default instead. `config.BlockedCommands`/`blockedCommand`
  are deprecated (decision 0020 removed the "Block always" feature entirely)
  and kept only so an old config.json with that key already in it still
  loads — never read, rendered, or applied anywhere; do not add a new reader
  for this field. Provides `privateDir`/`atomicWrite` (mode
  0700/0600, atomic rename-based writes) and `fileLock`/`accountLock` (flock-based,
  shared for `run` until it execs claude, exclusive for `login`/`logout`/`remove`/
  `system import` — this is what makes concurrent `cpro` invocations safe). The
  lock is **not** inherited by claude (decision 0064): claude's background
  helpers, MCP servers and leftover jobs would inherit it too and hold the
  account long after the session ended. Instead `s.run` tags the exec'd process
  with `CPRO_SESSION_PID=<its own pid>` (exec keeps the pid, so only that exact
  process matches — children inherit the variable with a different pid), and
  the exclusive-lock commands call `requireNoLiveSession` (claude.go), which
  refuses while such a process is alive, headless runs included. `list`/`ls`, `status`, and `doctor` read
  profiles without taking a lock, since they're purely informational.
- **usage.go** — fetches session/week usage from the undocumented Anthropic OAuth
  usage endpoint using the account's stored access token, with an on-disk cache
  (`cpro-usage.json` per profile) fresh for `usageFreshFor` (55s — just under
  `cpro watch`'s 60s default interval, so each refresh fetches). Decision 0065:
  `loadUsage` returns a `usageStatus` (`Stale`/`FetchedAt`/`Reason`) that every
  view must surface rather than passing an old value off as live (the status
  card's `usageStaleNote` line, a compact row's age); a failed fetch is
  recorded in the same cache file (`fail_reason`/`fail_count`/`retry_at`) and
  backed off — 1m for a 429 (the endpoint rate-limits per IP, shared with
  Claude Code), 5m for an expired token, doubling to a 10m cap — so no cpro
  process retries on every redraw; an expired token is detected locally from
  `expiresAt` (cpro never refreshes tokens — only Claude Code does, when it
  runs). `windowElapsed` (ui.go) marks a window whose reset has passed: it
  renders `--`/`reset` and never counts toward Total week.
- **ui.go** — terminal presentation only: color/TTY detection (`NO_COLOR`,
  `TERM=dumb`, non-terminal output), the shared Huh form runner (`runSelect`, still
  used by `pickRequiredArgs`/`config`'s scriptable-arg prompts and taking an `accent`
  hex color — `accentTheme` recolors Huh's own `┃` focused-group border/title and
  changes the select cursor from Huh's default `"> "` to `"❯ "`), and
  `announceCommand` — called from `pickCommandArgs` (rootui.go) once the interactive
  run flow's STEP 3 (RUN MODE) finalizes an account/mode, printing a "Running:"
  header plus `announceTarget`'s own line before the caller's `cmd.Execute()`
  actually runs it — purely informational (cpro is already about to execute it), not
  an alternative to running (decision 0007; the printing call site moved from
  `main.go`'s old `run` RunE to rootui.go under decision 0019, but the behavior
  itself didn't change). `announceTarget` is what that second line says, and it is
  the one piece masking changes the *shape* of rather than just a value — the
  literal `cpro run --account EMAIL [mode flags]` with masking off, `<alias> ·
  <mode label>` with masking on (decision 0030; see `maskEmailEnabled` below).
  `permissionModeForArgs` (permissions.go) is the reverse of `permissionModeArgs`
  that gives it that label, so the mode name comes from the one
  `permissionModes` table rather than a second mapping.
  `announceCommand` itself draws a small themed panel (decision 0037) —
  `accent(w, currentTheme.Top()+" Running:", accentRunning)` then
  `accent(w, currentTheme.Rail(), accentRunning) + "  " + announceTarget(...)`
  then `accent(w, currentTheme.Bottom(), accentRunning)` — the identical
  non-bubbletea printed-panel idiom `cpro info`/`doctor`
  (maintenance.go)/`list`/`status` (main.go) already use, so `cpro config`'s
  Theme setting applies to this announcement too; `accentRunning`, not
  `accentMode`, stays the fixed color regardless of theme (Theme only ever
  changes border runes, never color — same separation `renderPanel` keeps).
  The earlier `announceLine`, which prefixed each printed line with a fixed
  colored `┃` predating Theme becoming configurable, is gone — it had no
  other caller. It does not touch the clipboard: an earlier version also
  copied the printed command and reported the result (`writeClipboard`, wrapping
  `clipboard.WriteAll` with a 2-second timeout in its own goroutine — observed
  hanging indefinitely in a sandboxed environment where the `xclip` subprocess it
  shelled out to failed to detach); removed as unnecessary once `run` already
  executes the command immediately after — see decision 0016. `pickAccount`/
  `pickRunMode`/`isRunMode`/`runModeArgs` (the huh-based account/Normal-Danger-Plan
  picker `cpro run`'s own RunE used to drive inline) and the `accentAccount` constant
  that colored one of their steps are gone entirely — decision 0019 replaced them
  with rootui.go's own RUN ACCOUNT/RUN MODE frames, which pick straight from
  `permissionModes` (permissions.go) instead of a separate mode system.
  Also the usage/layout primitives shared by `cpro status`/`cpro watch`'s
  `renderFullView`/`renderCompactView` (main.go) — `usageColorFor` (the single
  source of truth for the safe/warn/danger threshold color, so a bar, its
  compact-narrow single-glyph fallback (`usageGlyph`), and its percentage text
  always agree — below `barWarnThreshold` it's `barColorSafe`, from there
  `warningColor`, from `barDangerThreshold` `dangerColor`: never a hardcoded
  orange/red, always whatever's actually configured), `usageBarWidth`/`usageBar`
  (a proportional bar at an arbitrary glyph width; `usageBar` is the full view's
  fixed 20-wide case), `pctText` (a clamped whole-percent string), `outputWidth`
  (the real terminal column count behind a writer, via `term.GetSize`, or 0 when
  it isn't a real terminal — main.go's `fullRowWidth`/`renderCompactView` treat 0
  as unconstrained), and `alignRight`/`padEnd`/`padStart`/`truncatePath` (ANSI-aware
  right-alignment, end-padding, start-padding for a fixed-width right-aligned field
  (decision 0031), and path truncation that keeps a path's
  identifying tail — all measuring via tui.go's `visibleWidth` rather than raw
  byte length, so a colored string aligns the same as its plain equivalent);
  plus Lip Gloss/Fang styling for doctor output and error rendering.

  `accentMode`/`barColorSafe`/`warningColor`/`dangerColor` are cpro's four
  user-configurable colors, deliberately kept semantically distinct (Accent =
  focus/navigation/normal borders, Usage bar = usage below the warn threshold,
  Warning color = usage at/above warn, Danger color = usage at/above danger —
  Theme, theme.go, is a fifth, orthogonal axis: border characters only, never
  color) — all four default to their original fixed values (purple, green,
  orange, red) and are overridden once at startup by `applyPreferences` from
  `cpro config accent/bar/warning-color/danger-color`. `dangerColor` doubles as
  the "press Esc again to exit" two-step warning color everywhere that
  convention appears — `escGuardField` below, `watchLoop` (main.go),
  `rootPickerApp`'s armed-exit footer (rootui.go) — deliberately reusing Danger
  rather than introducing a sixth color, since exiting is the more severe of
  the two thresholds' own colors. `colorPalette` is the fixed palette shared by
  every one of these preferences (and their pickers, configui.go) — Purple,
  Violet, Blue, Cyan, Green, Amber, Orange, Red, Rose; Orange/Red were added
  specifically so Warning/Danger color could offer them (their hex values match
  `usageColorFor`'s own original hardcoded ones exactly), which also makes them
  valid, unreserved choices for Accent/Usage bar now — there is no longer any
  reserved-color subset for any one preference.

  `maskEmailEnabled`/`emailMaskTable` (decision 0024) are `config.MaskEmail`/
  `EmailMasks`' own live, in-process mirror, following the exact same pattern
  as `accentMode` above: hydrated once at startup by `applyPreferences`, kept
  current for the rest of the run by `saveMaskEmail` (config.go) the instant
  it persists a toggle. `displayEmail(email string) string` is the one
  formatter reading them that every UI/output path showing an account's
  email must call instead of interpolating the value directly — real email
  when masking is off, `emailMaskTable[email]` when on, falling back to the
  real email if a mask is somehow still missing rather than showing nothing.
  Being a plain package-level function (not a closure threaded through a
  chain of callers, which is what `cpro list`/`status` alone did before this
  decision) is what makes it reachable from every file that needs it
  (rootui.go's RUN ACCOUNT, configui.go's Settings/DEFAULT ACCOUNT,
  system.go's export picker, maintenance.go's `doctor`) without a signature
  change rippling through every caller in between, and what makes a toggle
  update whatever screen is currently open on its very next render: nothing
  needs to be re-passed a fresh value, the formatter's own global state is
  already current the instant `saveMaskEmail` returns. `announceTarget`
  (the body of `announceCommand`'s own output, decision 0030) reads it too,
  but changes shape rather than just substituting a value: masking off it
  prints the literal, copy-pasteable `cpro run --account EMAIL <flags>`;
  masking on it prints `<alias> · <mode label>` with no `cpro run`/
  `--account` fragment at all, since an alias is never an account
  identifier `s.resolve` accepts and a command-shaped line invited a
  copy-paste that would fail. The real email is what `cpro run` execs
  either way. `--json` output (`cpro list`/`status`) and
  error hints that embed an email as part of an actionable command
  (`missingAccount`, "run cpro login EMAIL" suggestions) are deliberately
  left unmasked — see decision 0024 for why both are out of scope.
- **maintenance.go** — `doctor` (read-only diagnostic checks: cpro version, env
  overrides, `claude` presence/version, network reachability, config, per-account auth),
  `install` (atomically copies the current executable to `~/.local/bin/cpro`), and
  `info` (`cpro info`): a concise, read-only snapshot of the installation itself —
  version, binary path (`os.Executable()`), config directory, registered account
  count, and whether `claude` is installed — rendered in the same panel language as
  `doctor`, drawn in `currentTheme`'s own runes (theme.go), never hardcoded.
  Deliberately never touches an account's own credentials or
  usage/session state (that's `status`'s job, main.go) and never fails: there's
  nothing here to pass/fail a check on, unlike `doctor`. `claudePath` (extracted from
  `runDoctorChecks`'s inline `exec.LookPath("claude")`) is the one shared "is Claude
  Code installed" check both commands call, so they can't drift on what counts as
  installed. `doctor`'s own per-account check name and its "Default account"
  detail line both render through `displayEmail` (decision 0024, ui.go) — the
  embedded "run cpro login EMAIL" hint on an invalid-session check stays
  unmasked, since masking it would suggest a command that doesn't actually
  work (see decision 0024's own reasoning for every error-hint exception).
- **theme.go** — the shared border-rune abstraction cpro config's Theme setting
  picks between: `borderTheme` (`TopLeft`/`Horizontal`/`Vertical`/`BottomLeft`/
  `Tee` runes, plus `Top()`/`Bottom()`/`Rail()`/`Divider()` convenience methods
  that concatenate them into the actual two-or-one-rune sequences a screen draws
  with — e.g. `"╭"+"─"` → `"╭─"`) and `borderThemes` (Minimal/Rounded/Heavy/
  Double, in that display order — Rounded is index 1 and the default). `currentTheme`
  is the one live, in-effect theme every panel in cpro renders with — `renderPanel`
  (tui.go), `renderRootPanel` (rootui.go), and the plain `accent()`-printed panels
  in main.go/maintenance.go all read `currentTheme.Top()/.Rail()/.Bottom()/
  .Divider()` instead of ever hardcoding "╭─"/"│"/"╰─"/"├─" themselves — overridden
  once at startup by `applyPreferences` from `cpro config theme` (ui.go), exactly
  like `accentMode`/`barColorSafe`. Theme only ever changes which runes are drawn;
  it never carries or implies a color — every caller still applies its own
  accent/status color exactly the same way regardless of which theme is active.
  `themeByName`/`themeNames` mirror `colorByName`/`paletteNames` (ui.go) for the
  scriptable `cpro config theme NAME`.
- **browseui.go** — the one browse/search/cursor state machine behind every list
  screen (decision 0041): a generic `browseList[T]` owning items, browse `cursor`,
  search `query`/`filtered`/`fcursor`, a `match` haystack function and an optional
  `rank` function, with `setItems`/`searching`/`move`/`searchMove`/`selected`/
  `searchSelected`/`typeRune`/`backspace`/`clearSearch`/`refilter` and the shared
  `browseKey`/`searchKey` dispatchers (`browseKeyOutcome` + `browseKeyOpts{forward,
  leftBack}`, so a screen supplies only what Enter and Esc/← mean for it). Four
  constructors pin each list's matching policy: `newAccountBrowseList` (plain,
  `displayEmail`), `newAccountEntryBrowseList` (the root picker's email-shaped
  `rootPickerEntry` frames, same plain policy), `newSessionBrowseList`, and
  `newCommandBrowseList` (`rootPickerScore`'s ranked tiers). Also the shared list
  chrome/scroll primitives — `listChromeLines`, `scrollRange`/`scrollLines` (the
  keep-the-cursor-row-visible window every long list now uses) and
  `accountListLines` (a whole scrolled account list rendered through
  `accountPickerRow`) — plus `forwardAll[T]` and `entryEmails`. `browseList[T]` is
  embedded by `exportPickerApp` (system.go) and `resumeAccountPickerApp`
  (resumeui.go), wrapped by `defaultAccountState`/`sessionListState`/
  `sessionAccountState` (configui.go/sessionui.go), and held as
  `rootFrame.list browseList[rootPickerEntry]` for `rootPickerApp`'s two list frames —
  the seven per-screen `refilter*` methods and five copies of the same up/down/
  type/Backspace handling it replaced are gone.
- **config.go** — the `cpro config` command: with a terminal and no `--json`, it calls
  `runConfigUI` (configui.go); otherwise (piped, non-interactive, or `--json`) it
  prints the config directory, default account, and preferences instead. Every
  preference also has a scriptable equivalent (`cpro config trust|mask|accent|bar|
  warning-color|danger-color on|off|COLOR`, `cpro config warn-at|danger-at PERCENT`,
  `cpro config theme NAME`, `cpro config account EMAIL`, `cpro config
  permission-mode MODE [--account EMAIL]` — decision 0019/0050, validated
  against registered accounts via `missingAccount`, store.go). Both paths funnel
  through one `saveXxx`/`setXxx` pair
  per preference — `saveXxx` is pure persistence via `store.update` (and, for every
  color/theme/threshold preference, also applies the change to the matching
  in-process global immediately — `accentMode`/`barColorSafe`/`warningColor`/
  `dangerColor`/`currentTheme`/`barWarnThreshold`/`barDangerThreshold` — so the
  rest of that same run, not just future ones, reflects it too), `setXxx` wraps
  it with the scriptable command's printed confirmation — so `runConfigUI` (a live
  bubbletea screen, which must never have raw `cmd.Println` output interleaved with
  its own rendering) calls `saveXxx` directly while the `config accent`/`config
  warn-at`/etc. subcommands call `setXxx`. `colorCommand` (the shared factory
  behind `accent`/`bar`/`warning-color`/`danger-color`) is the one place a color
  preference's CLI shape is defined — a fifth color preference needs only a new
  `saveXxx`/`setXxx` pair and one more `colorCommand(...)` call, not a new command
  shape. Keep new preferences following that same save/set split rather than
  adding flags only. `saveMaskEmail` follows the same immediate-apply
  pattern for `maskEmailEnabled`/`emailMaskTable` (ui.go, decision 0024)
  instead of a single color/threshold global. `ensureAccountMask(c *config,
  email string)` is the small helper a new account's own registration path
  calls (claude.go) to give it an alias right away if masking is already
  on, rather than leaving a render-time gap until the next lazy repair pass
  (`ensureEmailMasks`, main.go). `newPermissionsCommand` (decision 0021) is a separate,
  sibling cobra command in this same file, not a `cfg.AddCommand` subcommand:
  `cpro permissions` opens the Permissions screen directly via
  `runPermissionsUI` (configui.go), interactive-only — outside a terminal it
  errors naming `cpro config`'s own plain-text summary (which still prints
  `Permissions: MODE`) as the fallback, since there's no meaningful
  non-interactive rendering of a live mode picker.
- **tui.go** — rendering/color primitives shared by every custom bubbletea screen
  in cpro (currently configui.go and rootui.go): `tuiColorEnabled` (the NO_COLOR/
  TERM=dumb/terminal-output gate, computed once per screen at startup — same rule
  `accent()` applies per call), `styleText` (a lipgloss foreground wrapper gated by
  that decision), `dimStyle` (its SGR-faint equivalent, for visually secondary text
  like a root-picker row's metadata), `renderPanel`/`renderFooter` (the shared
  left-rule frame — drawn in `currentTheme`'s own runes, theme.go, never hardcoded
  here — and the `key action ∙ key action` footer line every screen uses —
  `renderPanel` colors the whole frame one color; rootui.go's own `renderRootPanel`
  is a separate, per-line-colored sibling for the one screen that needs a multi-color
  rail, rather than reworking this signature for every existing single-color caller),
  `deriveAccentShades`/`colorToHex` (an n-shade gradient in the same hue family as a
  given accent color, via lipgloss's own `Lighten`/`Darken` rather than hand-rolled
  RGB/HSL — the root picker's five-shade semantic-group rail is its only caller so
  far, but it's written generically), `visibleWidth`/`truncateToWidth`
  (ANSI-aware width measurement and plain-text truncation), `screenTitle` (every
  cpro TUI screen's own `"claude cpro[ - NAME]"` border title — the border is
  the location indicator, so nothing ever prints a second title line above the
  panel; `""` is reserved for bare `cpro`'s reduced launcher, the one true
  outermost screen — every other screen, including one launched directly with
  nothing above it, e.g. `cpro config` on its own, still carries its own name),
  and `navStack[T]` (`push`/`pop`/`current`/`atRoot` — the one reusable
  navigation history every hierarchical screen shares, generic over whatever a
  screen's own "frame" type is; `pop` is a no-op at the last remaining frame,
  which is that stack's own root — see rootui.go's `rootFrame` and configui.go's
  own use of `configScreen` directly as the frame type). A new custom screen
  should reuse all of these rather than reimplementing its own chrome or its
  own one-off "go back a level" field.
- **configui.go** — `cpro config`'s interactive screen: a small custom
  `tea.Model` (`configApp`), not huh Fields — the grouped menu with a right-aligned
  `●`/`○`/percent value column, a color picker showing the saved value separately
  from the cursor-navigable remaining palette, and a live left/right-adjustable
  threshold stepper don't fit huh's built-in field types, and bubbletea itself isn't
  a new dependency (huh is built on it; `escGuardField`/`watchLoop` already drive it
  directly). The main menu (`screenMenu`) now shows one of two different item
  sets, chosen by `menuItems()` off the same `hasParent` flag that already
  distinguished the screen's own Esc/← meaning (decision 0023):
  `configMenuItemsSettings` — Mask emails (ungrouped, first), Run defaults
  (Account: what non-interactive `cpro run` uses with no flags — decision
  0019), Appearance (Accent/Usage bar/Theme), Usage thresholds (Warning/
  Danger, each row showing both its percentage AND its color, e.g.
  "80%   ● Orange") — for the picker's own "config →" round trip
  (`hasParent == true`); or `configMenuItemsConfig` — the same items plus
  Permissions restored as a leading, ungrouped row alongside Mask emails —
  for a direct `cpro config` invocation (`hasParent == false`).
  `menuTitle()` returns "CONFIG" from either entry point (decision 0040 retired the separate "SETTINGS" title; only the item sets still differ, each omitting what its own entry point already offers one press away).
  Permissions moved a lot: decision 0018 first moved Auto-trust out of
  Security into the Permissions screen itself, 0019 relocated Permissions to
  Run defaults, 0021 promoted the whole screen to its own top-level entry
  point (`cpro permissions`, config.go; a "Permissions →" row directly on the
  root/menu picker — see rootui.go) removing it from Settings entirely, and
  0023 restored it as a CONFIG-only row (never SETTINGS) once direct
  `cpro config` turned out to have no way back to it otherwise. Auto-trust
  (`config.AutoTrust`) is not a main-menu row either:
  it lives inside the Permissions screen itself as a "Workspace" section's one
  row, "Trust working directory" (decision 0018) — a run-time permission concern
  grouped with the permission mode it's coherently combined with for YOLO. The
  screen's own "Block always" blocked-command section (rows below Workspace) is
  gone entirely — decision 0020 removed the whole feature; the screen now covers
  only permission mode, workspace trust, and the live command preview.
  `screenDefaultAccount` (decision 0019) is "Account →"'s own screen
  — a searchable account list, same browse/search shape as `exportPickerApp`
  (system.go) and rootui.go's own RUN ACCOUNT frame, saving `config.Default` via
  `saveDefaultAccount` (config.go) on Enter and popping back to the menu regardless
  of outcome, same convention as the color picker. Its own rows and search
  filter (`refilterDefaultAccount`) render/match through `displayEmail`
  (decision 0024, ui.go), same as RUN ACCOUNT; the main menu's own "Account"
  row value (`viewMenu`'s `"account"` case) does too. Since decision 0036 its
  rows also carry that account's live Session usage (`S █ 53%`), rendered by
  the *same* `accountPickerRow`/`accountPickerNameWidth` and fetched by the
  *same* `fetchAccountPickerUsage` RUN ACCOUNT uses (rootui.go) — not a
  parallel implementation: `enterDefaultAccount` therefore returns a
  `tea.Cmd` (so `activateMenuItem`/`updateMenu` return one too, nil for every
  other row), the screen renders and saves correctly before any usage
  arrives, `configApp.accountUsage` mirrors `rootPickerApp.accountUsage`
  (keyed by the **real** email; a re-entry with everything cached returns a
  `nil` cmd rather than refetching), and `configApp.Update`'s own
  `runAccountUsageMsg` case records and re-renders without ever touching
  `m.defaultAccount.cursor`/`.fcursor`. A failed usage fetch keeps the dim
  `S ·   --` placeholder and never blocks saving that account as the
  default; the alias remains display-only, the real account is what's
  persisted. Eight screens in one
  `tea.Program` (`screenMenu`/`screenColor`/`screenTheme`/`screenThreshold`/
  `screenThresholdMenu`/`screenPermissions`/`screenDefaultAccount`/
  `screenAccountMode`) so there's no
  flicker between them. A boolean setting (`mask` on the main menu; `trust`
  inside `screenPermissions`) toggles immediately on Enter — no secondary
  confirm screen. `screenAccountMode` (decision 0054) is `screenPermissions`'
  own per-account mode editor: `viewPermissions` appends one row per registered
  account (its effective mode, `● (override)` vs `○ (global)`), Enter/→ on one
  opens this screen, and Enter there saves that account's override — or clears
  it with the trailing "Use the global default" row — through the same
  `saveAccountPermissionMode` `cpro config permission-mode MODE --account EMAIL`
  calls, closing decision 0050's parity gap. `screenColor` (the shared
  color picker) now
  serves four targets, not two — `enterColor`'s `target` is `"accent"`/`"bar"`/
  `"warning-color"`/`"danger-color"`, dispatched in `updateColor`'s save switch —
  reused verbatim rather than building separate pickers per preference (the Warning/
  Danger color mockups are pixel-identical to Accent/Usage bar's own). `screenTheme`
  is the Theme picker: all four `borderThemes` (theme.go) shown together, each with
  a saved/cursor-independent ●/○ marker (like `colorPickerState`, but the saved
  entry stays in the list rather than being pulled out and shown separately — the
  spec's own mockup shows all four together) and a 3-line preview of its own actual
  border runes (`Top()`/`Rail()`/`Bottom()`), deliberately never colored — Theme only
  ever changes border characters, never color. Selecting "Warning" or "Danger" from
  the main menu doesn't jump straight to the percentage stepper anymore: it opens
  `screenThresholdMenu`, a small two-row "Threshold / Color" sub-menu
  (`thresholdMenuState`) so both are independently, visibly reachable from one main-
  menu row — Enter (or `→`, `isForwardConfigItem`) on "Threshold →" opens the existing
  `screenThreshold` stepper unchanged, on "Color →" opens `screenColor` with target
  `"warning-color"`/`"danger-color"`; both rows carry the same trailing `" →"` cue
  `configItemLabel` gives every forward row on the main menu (below), since neither
  edits in place. Every screen deeper than the main menu lives on
  `m.stack` (`navStack[configScreen]`, tui.go) rather than a single hardcoded
  "return to X" field: entering one is `m.stack.push(screenX)`, Esc/← is
  `m.stack.pop()` — one generic mechanism that replaces what used to be a
  `returnScreen` field only ever capable of remembering one predetermined parent.
  `screenMenu` (via `runConfigUI`, `cpro config`) is always its own stack's
  own root frame. `screenPermissions` (via `runPermissionsUI`, `cpro
  permissions` — decision 0021) usually is too, but — decision 0023 — not
  always any more: reached through CONFIG's own restored `"permissions"`
  case in `activateMenuItem` (`m.stack.push(screenPermissions)`), it's an
  ordinary *nested* frame instead, popped like any other one via
  `m.stack.pop()`. `updatePermissions`/`viewPermissions` check
  `m.stack.atRoot()` to tell the two cases apart — nested means a plain pop;
  root means the shared Esc/← handler, `exitRootScreen` (renamed from the
  earlier `menuEsc` once Permissions first needed the identical root-frame
  logic in 0021), applies: `m.hasParent` (set by the caller: `false` for
  either command's own direct `cfg.RunE`, `true` when opened from the
  root/menu picker, rootui.go) decides whether it sets `m.backOut` and quits
  (so the caller can relaunch the picker) or arms a double-Esc-to-exit
  instead of quitting immediately (decision 0023 — reusing rootPickerApp's
  own `exitArmed`/`arm`/`exitArmTimeout`/`exitArmExpiredMsg`, rootui.go,
  directly; only `configApp` gets its own `exitArmed`/`exitArmedGen` fields
  and `arm()` method). A direct, no-parent root screen is exactly the "this
  Esc would close the whole program" case that convention exists for — single-
  Esc remained correct only for a screen that still has a parent to go back
  to. `viewMenu`/`viewPermissions` recolor their border to `dangerColor`
  while armed (`frameColor()`) and swap the footer hint to a pre-styled
  `"Esc¹ again"`, matching the picker's own visual convention; any key other
  than Esc cancels the arm. Titles ("claude cpro - CONFIG" on the menu from
  either entry point since decision 0040, `"claude cpro - " +
  screen-specific name` on every other screen — see `screenTitle`, tui.go)
  sit on the border, doubling as the location indicator; no screen prints a
  separate title line. The color picker, Theme picker, threshold editor, and
  threshold sub-menu are always-nested sub-screens, all discarding on Esc/←
  (color/theme/threshold) or backing out one level via `m.stack.pop()`.
  Permissions, unlike those (which persist on Enter and navigate back), stays
  open after Enter — selecting a mode or toggling workspace trust both save
  in place and leave the cursor exactly where it was, since a user picking a
  permission mode plausibly wants to glance at its description next,
  in the same visit; only Esc/← leaves (`exitRootScreen` when root,
  `m.stack.pop()` when nested inside CONFIG), and its own footer covers all
  three reachability cases: hasParent or nested both render "← Back"/"Esc
  Back" (identical, one switch case); root-with-no-parent is the armed/
  unarmed pair described above.
  Every nested screen (color/theme/threshold/threshold-menu, and Permissions
  when reached from CONFIG) still uses plain single-Esc navigation — Esc
  always backs out one level immediately, no arming, since a nested frame by
  definition still has a parent frame within the same program to fall back
  to. Only a screen that IS the stack's own root with no parent at all now
  diverges from that (decision 0023) into `escGuardField`'s app-wide double-
  Esc-to-exit convention, since that's precisely the case where a shallow
  settings tree's own accidental-exit risk becomes as real as a deep command
  tree's. The one further exception is the threshold editor, whose `←`/`→`
  keys already mean decrease/increase the value (its own `"←→", "Adjust"`
  footer hint), so it deliberately has no separate `←`-backs-out binding, only Esc.
  `colorPalette` (ui.go) is the single palette shared by all four
  color pickers (accent/bar/warning-color/danger-color) and their scriptable
  equivalents — there is no per-preference reserved-color subset; Orange and Red
  were added to it specifically so Warning/Danger color could offer them (matching
  `usageColorFor`'s own original hardcoded hex values exactly), which also makes
  them valid, unreserved choices for Accent/Usage bar now. `tea.WithOutput` is
  given `os.Stderr` directly, not `cmd.ErrOrStderr()` — under `NO_COLOR`/`TERM=dumb`
  that's a `*colorprofile.Writer` (see `uiOutput`, ui.go), not a real `*os.File`,
  and bubbletea can't drive raw-mode/cursor control through one (confirmed live: no
  frame ever renders); the screen's own `m.color` flag, not bubbletea's writer,
  already decides whether any color escape is emitted at all, so nothing is lost by
  bypassing it here.
- **permissions.go** — the engine behind the Permissions screen, `cpro run`'s
  actual permission behavior, AND the interactive run flow's own RUN MODE step
  (rootui.go, decision 0019), so none of the three can drift apart: `permissionModes`
  (the five modes — Ask for everything/Edit without asking/Read-only/Live a
  little/YOLO — each with its own risk color and Claude argv; "readonly" uses
  `--permission-mode plan`, "yolo" is `yoloArgs()` (below); "Live a little" is
  `--dangerously-skip-permissions` alone — skips normal prompts but, unlike YOLO,
  leaves `permissions.blockReadsOutsideWorkingDirectories` untouched). This is the
  one and only table any of the three ever reads a mode's argv from — there is no
  separate Normal/Danger/Plan system anymore (decision 0004, superseded).
  `permissionModeArgs(mode)` is the one argv builder `applyPermissionDefaults`
  (below) calls, so every entry point resolves a mode to flags identically.
  Each mode also carries a `Description` (decision 0038) — the one-line
  summary the Permissions screen renders for the highlighted candidate, read
  off the same struct `Args` lives on, so the text can't drift from the mode
  it describes; it replaced that screen's earlier "Command preview" block
  (and `formatCommandPreview`, deleted) — see configui.go's `permPreview`,
  decisions 0017 and 0038.
  `applyPermissionDefaults` — called from `store.run` (claude.go), right before
  every real `claudeCommand` invocation: prepends
  `permissionModeArgs(permissionModeForAccount(c, email))`
  to the forwarded args, unless an explicit `--permission-mode`/
  `--dangerously-skip-permissions` is already among them (a literal CLI flag,
  or the interactive run flow's own RUN MODE step, rootui.go, which
  synthesizes the equivalent flags into the argv it hands back to `cpro run`)
  — an explicit per-invocation choice always wins, the two layers stay
  independent rather than merged into one. `permissionModeForAccount`
  (permissions.go) is the one resolver of that default: an account's own
  `config.PermissionModeByAccount` override first, the global
  `config.PermissionMode` otherwise, then `defaultPermissionMode` — a stale or
  hand-edited override is ignored rather than trusted (decision 0050).
  `store.run`'s own AutoTrust/YOLO checks and the RUN MODE preselect read it
  too, so a saved default can never disagree with what actually runs; `cpro
  config permission-mode MODE [--account EMAIL]` is the scriptable equivalent
  (reserved mode `default` clears an account's override), and since decision
  0054 the interactive Permissions screen reaches the same per-account value
  through its own `screenAccountMode` editor — see configui.go below. The
  "Block always" blocked-command
  feature (`blockedCommandRule`/`validBlockedCommands`/`disallowedToolsArgs`/
  `permissionArgsForMode`/`permissionArgs`, plus its own UI section and
  `saveBlockedCommandEnabled`, config.go) is gone entirely — decision 0020
  removed it, config.go/config.go's `--json` output no longer exposes
  `blockedCommands` either. `config.BlockedCommands`/the `blockedCommand`
  type (store.go) still exist, but only so an old config.json with that key
  already in it still loads — never read, rendered, or applied to a claude
  invocation; do not resurrect a reader for this field without a fresh
  decision.

  The Permissions screen also carries a "Workspace" section with one row,
  "Trust working directory" — `config.AutoTrust`, moved here from the main
  Settings menu (decision 0018) since it's a run-time permission concern, not
  a top-level preference; same field, same `saveAutoTrust`, no config
  migration, just a new location and label in the UI. It never enters
  `permissionModeArgs`'s argv (it isn't a claude flag), so the mode
  description never reflects it — `store.run` (claude.go) is the one place its
  effect (`markTrusted`) is decided, now also triggered whenever the effective
  permission mode is YOLO, independent of the `AutoTrust` field's own value.

  YOLO's real meaning is "zero permission prompts, period" — `yoloArgs()` is the
  one builder for that, named and tested on its own even though `permissionModes`'s
  "yolo" entry is its only caller (it's also what tests assert the real
  invocation produces): `--permission-mode bypassPermissions` (decision 0038).
  Two earlier shapes led here. First, `--settings` carrying inline JSON
  disabling `permissions.blockReadsOutsideWorkingDirectories`, added after a
  live report that this specific restriction still prompted ("Do you want to
  proceed?") even under the blanket bypass flag — verified end-to-end against
  a real `claude` invocation to have been silently defeated whenever an
  account's own persisted user `settings.json` already carried that key as
  `true` (most likely Claude Code's own "remember this choice" behavior from
  an earlier interactive "No" answer). Then `--dangerously-skip-permissions
  --add-dir /` (decision 0026), where `--add-dir` was the documented
  mechanism for widening the file tools' own directory boundary and did
  bypass the block reliably regardless of that conflicting source. Decision
  0038 retired `--add-dir` once decision 0035 began writing
  `permissions.additionalDirectories: ["/"]` into that same per-account
  `settings.json` before every invocation — the flag had become redundant
  with it — switching to the officially documented equivalent
  (`--permission-mode bypassPermissions`; the docs state
  "--dangerously-skip-permissions es equivalente"), verified live to reach
  zero `permission_denials` on its own. That switch also matters for a
  second, non-obvious reason: `--add-dir` had become the *only* thing
  distinguishing YOLO's argv from `liveALittleArgs`' bare
  `--dangerously-skip-permissions`, and both `yoloEffective` (claude.go) and
  `permissionModeForArgs` (below) depend on every mode's `Args` being unique
  — dropping it without replacement would have silently collapsed the two
  modes into one at the argv level. `claudeSupportsFlag`/`requireYOLOSupport`
  grep the installed `claude --help` output for `--permission-mode` and the
  `bypassPermissions` value (both plain substrings of the real `--help`,
  which lists each flag's accepted values inline) before any entry point ever
  applies them: `store.run` (claude.go, for the saved-default case — an explicit
  forwarded flag skips this check, since it's already the user's own explicit
  choice) and the interactive run flow's own RUN MODE step (rootui.go's
  `updateRunMode`, checked before it ever builds a YOLO argv, the same
  pre-flight the old `pickRunMode` used to do) — an installed version missing
  one fails with a clear error before Claude starts, rather than silently
  launching in a mode that would still show prompts. Never called from the
  Permissions screen itself (re-rendered on every keystroke) — that screen
  only reads each mode's static `Description`, so it has no version-dependent
  behavior to keep in sync in the first place.

  The CLI flags alone only ever covered the boundary a *statically
  resolvable* file-tool read is checked against. They do nothing for a
  different case decision 0034 investigated and fixed: whenever Claude's own
  shell parser cannot statically analyze a Bash command's eventual target
  path — a simple variable expansion of a runtime-only-known value (an
  inherited env var, not a same-command literal assignment, which is still
  statically resolvable and unaffected), an inline interpreter (`python3 -c
  ...`), a runtime-determined `find` argument, or any other runtime-computed
  path (command substitution, `dirname`, etc.) — it falls back to consulting
  `permissions.blockReadsOutsideWorkingDirectories`'s own raw persisted value
  directly, bypassing any CLI allowlist entirely and ignoring
  `--dangerously-skip-permissions`/`--permission-mode bypassPermissions` and
  a CLI `--settings` override alike (verified live against a real
  invocation: all proven powerless against this exact fallback path, the same
  account-level `settings.json`
  precedence decision 0026 already found for the direct-read case). The only
  source that reliably reaches this fallback path is the account's own real,
  persisted user `settings.json` — the exact file Claude Code itself
  reads/writes back after an interactive "remember this choice" answer.
  `ensureBlockReadsOutsideWorkingDirectories(profile, blocked bool)`
  (claude.go) is the fix: a generic `map[string]any` round-trip (same shape
  as `markTrusted`) that sets that file's own
  `permissions.blockReadsOutsideWorkingDirectories` key to `blocked`,
  preserving every other existing key, creating the file if it doesn't exist
  yet (unlike `markTrusted`'s deliberate refusal to fabricate a `.claude.json`
  project entry — a fresh settings.json containing only this one key was
  verified live to start Claude Code normally), and a no-op write when the
  persisted value already matches, so a run doesn't needlessly rewrite the
  file every time. Called unconditionally from `store.run` (claude.go), right
  after `applyPermissionDefaults`, for **every** real invocation, not only a
  YOLO one — the effective value comes from the **final args**
  (`!hasArgValue(args, "--permission-mode", "bypassPermissions")` since
  decision 0038; `hasArgValue`, not `hasArg`, because edit/readonly pass that
  same flag with different values), the
  same "final args decide, not just the saved default" principle
  `applyPermissionDefaults` already follows, so an explicit forwarded YOLO
  combination unblocks it exactly like a configured YOLO default does, and
  any other mode restores the block on its very next run — leaving YOLO
  later doesn't leave this restriction silently disabled forever. This is
  the one and only place this fix lives: there is no second, parallel argv
  or settings builder for `cpro run`/`cpro session continue`/the interactive
  RUN MODE step, since all three already end in this same `store.run` call
  (confirmed by inspection for this decision, not assumed).

  `requireNoManagedPermissionsPolicy(profile)` (permissions.go) is a second,
  narrower fix for the one layer cpro categorically cannot read or override:
  a higher-precedence enterprise/organization managed policy. Rather than
  silently launch a session labeled "YOLO (no prompts)" that such a policy
  could still interrupt, it parses `claude doctor`'s own human-readable
  `"Managed settings (remote): ..."` line (the one already-shipped signal
  available — Claude Code exposes no stable API for this) and fails clearly,
  before Claude ever starts, whenever a policy is positively detected (the
  line present without a `"not fetched"` qualifier); a parse failure, a
  `claude doctor` predating this line, or `claude doctor` itself failing all
  degrade to "no known policy" (nil error) rather than blocking a legitimate
  run over an unrelated failure. Called from the same two places
  `requireYOLOSupport` already is: `store.run` (the configured-default YOLO
  case) and the interactive run flow's own RUN MODE step (rootui.go's
  `updateRunMode`) — never verified against a real Enterprise/Team
  deployment, since none was available to test against; a real, acknowledged
  gap, not a fully proven implementation of Claude Code's actual precedence.
  See decision 0034 for the full investigation, including a sanitized
  settings-precedence table.

  `ensureYOLOSettings(profile string, enabled bool)` (claude.go, decision
  0035) is a third settings-file effect, added after cross-checking Claude
  Code's own current documentation rather than relying only on 0026/0034's
  earlier findings: `permissions.defaultMode: "bypassPermissions"` is
  documented as settable from the **user** settings tier, which for every
  cpro invocation is the account's own `CLAUDE_CONFIG_DIR/settings.json`
  (never the real, system-wide `~/.claude` — `claudeCommand` always redirects
  it). Verified live against 2.1.268: that key plus
  `permissions.additionalDirectories: ["/"]`, with
  `blockReadsOutsideWorkingDirectories` already false, reaches full YOLO with
  **zero CLI permission flags at all**, the shell-parser-fallback cases
  included. It also sets a third, top-level key —
  `skipDangerousModePermissionPrompt: true`, a sibling of `"permissions"`,
  never nested under it — added by decision 0058 after a real *interactive*
  session (unlike every prior YOLO test, which only ever drove real `claude`
  headlessly via `-p`) was found to still stop at Claude Code's own one-time
  "WARNING: Bypass Permissions mode / Yes, I accept" confirmation screen
  without it; undocumented on the official settings-reference page as of this
  writing, confirmed by community documentation and live testing. All three
  keys share one `map[string]any` round-trip and one no-op-when-unchanged
  discipline, same as `ensureBlockReadsOutsideWorkingDirectories`, called from
  the same `store.run` site with the same `yoloEffective` value, so all stay
  in lockstep. Disabling **removes** all three keys rather than writing an
  "off" value (there is no meaningful non-bypass `defaultMode` for this
  function to assert — removing lets the actually-configured mode govern
  undisturbed), and it only ever touches a key whose current value is exactly
  what it itself would have set, so a hand-configured `defaultMode`/
  `additionalDirectories`/prior "Yes, I accept" is left alone rather than
  clobbered. It works **alongside** `yoloArgs()`'s own
  `--permission-mode bypassPermissions` argv rather than replacing it — and
  its `additionalDirectories: ["/"]` is specifically what made decision
  0026's `--add-dir /` redundant enough for decision 0038 to drop it from
  that argv.

  `requireNotRoot()` (permissions.go, decision 0035) is the third preflight
  alongside `requireYOLOSupport`/`requireNoManagedPermissionsPolicy`, called
  from the same two places both of those are (`store.run`'s
  configured-default YOLO branch, and rootui.go's `updateRunMode`) so the
  three can't drift apart. A single `os.Geteuid() == 0` check: Claude Code
  itself refuses to start in `bypassPermissions` as root or under sudo on
  Linux/macOS (documented, and independent of whether bypass was requested
  via CLI flag or `defaultMode`), skipping the check only inside a recognized
  sandbox. Without this, that refusal reached the user as Claude's own raw
  stderr after cpro had already promised "no prompts"; now it fails clearly,
  before Claude starts, in cpro's own voice.
- **rootui.go** — one shared command-palette component serving two entry points:
  bare `cpro` (the reduced, everyday-actions launcher) and `cpro menu` (the full
  command list, formerly what bare `cpro` itself showed) — both call
  `pickCommandArgs(root, names, title)` (main.go), differing only in the `names
  []string`/`title string` they pass (`rootLauncherNames`/`""` for bare `cpro`;
  `rootMenuNames()`/`"MENU"` for `cpro menu`). `rootLauncherNames` is the fixed,
  short list bare `cpro` shows (`run`/`status`/`watch`/`menu`); `rootMenuNames()`
  derives the full list from `rootPickerMeta` at call time, filtering out
  `rootMenuExcluded` — `menu` itself (which would let the full palette nest a link
  back to the reduced one) and `run`/`status`/`watch` (already one press away in
  the launcher every path to Menu passes through first, so repeating them would
  just be noise) — so the full palette is exactly config/permissions/login/
  logout/remove/session/system/doctor/install/completion/info/help/version, in
  that order (decision 0025 added `session`, positioned with the `system`/
  `doctor` "Settings" group — `TestCommandTreeAudit`, main_test.go, now fails
  if a future registered command is left out of `rootPickerMeta` entirely,
  which is exactly the gap that let `session` go unreachable from the picker
  until this decision). `list`/`ls`
  is deliberately absent from both — it stays a fully working direct CLI command,
  just not picker-surfaced, since `status` already covers the interactive/
  at-a-glance use case a picker serves. A custom `tea.Model` (`rootPickerApp`)
  renders a compact command palette — one row per command, no group headings, no
  `├─` separators, just one blank rail row between groups while browsing (dropped
  entirely once search narrows the list to one flat result set) — see
  `viewNormal`. `cpro` itself prints nothing above the panel; the shell's own echo
  of the typed command is the only thing there. `rootPickerMeta` lists every
  picker-eligible command in its fixed display order with three pieces of
  picker-only presentation data: `group` (Core/Account/Settings/System/About —
  internal only, never rendered as text, it only picks a rail shade, see below;
  `config`'s own group is "Core", reused rather than a sixth group — "Core" only
  ever renders in the Root frame, run/status/watch, or the Menu frame, config
  and permissions, never both at once, and listing `config` first with a Core
  group is what puts it alone in its own leading section at the top of the Menu
  frame, ahead of Account, matching the task's own mockup) and `shortLabel` (the
  short right-side metadata, e.g. "Claude Code" for `run`, "Preferences" for
  `config`). `permissions` (decision 0021) sits directly below `config`, same
  Core group — no blank rail separator between them — and is the one entry
  whose row shows *live*, not static, metadata: `rootPickerEntry.permMode`
  (set by `applyLiveMeta`, called wherever fresh Menu-frame entries are
  built) carries the currently saved permission mode, and `rootRow` renders
  it specially for this one entry as a colored "● <mode label>" (the "●" in
  the mode's own risk color, `permissionModeByKey(mode).Color`) instead of
  the generic dimmed `shortLabel` every other row uses — truncating the plain
  text first via the same `metaBudget`/`truncateToWidth` every row's metadata
  degrades through, then splitting off the "●" prefix to color it, since
  `truncateToWidth` isn't ANSI-aware and would corrupt a pre-colored string.
  `rootPickerFilteredEntries(root, names)` builds entries from the given `names`,
  in that exact order, filtered down to whichever of them `root` actually has
  registered (a removed command silently disappears rather than crashing the
  picker), attaching each entry's `description` — the longer, below-panel text
  shown only for the currently highlighted row — from that command's own real
  `cmd.Short`, rather than a second, hand-maintained string (`rootPickerEntries`
  is the same table built from every `rootPickerMeta` entry with no filtering and
  no real description, for the pure unit tests that don't need one). `rootRow`
  renders one row (reserved cursor slot, name padded to the longest visible name,
  then metadata) and is what responsively degrades metadata — via `metaBudget`,
  from the real terminal width — truncating with `truncateToWidth` (ui.go) and
  then dropping metadata entirely on a narrow terminal, while the rail, cursor,
  and command name are never touched; the command list itself never wraps. On a
  terminal *shorter* than the full list, `scrollLines` scrolls the list body
  (only the body — the panel edges, description, and footer, `rootChromeLines`
  worth of lines, always render) to keep whichever row the cursor/fcursor is on
  inside the visible window, centering it when there's room on both sides and
  clamping at either end of the list otherwise — without this, a command past the
  bottom edge of a short terminal was reachable by Down (the selection moved) but
  never came back on screen, so Enter had no visible target to confirm — reported
  live and reproduced with a 15-row pty (see `TestRootPickerResponsive`'s
  "scrolling reaches the last command" subtest).

  Every screen this program can show past its own outermost frame — the full
  "MENU" palette, the "SYSTEM" submenu — is a frame on `m.stack`
  (`navStack[rootFrame]`, tui.go), replacing what used to be two different one-off
  fields (a `submenu` bool, a `launcherEntries` slice-swap): `rootFrame` holds
  `kind` (`frameList`, a searchable entries list, or `frameSubmenu`, the fixed
  "System credentials" choice), `title` (`""` only for the picker's own outermost
  frame — see `screenTitle`, tui.go — `"MENU"`/`"SYSTEM"` otherwise), a
  `list browseList[rootPickerEntry]` for the two searchable frames (`frameList`,
  and `frameRunAccount`'s account rows — see browseui.go) and a plain `cursor`
  for the fixed-choice ones (`frameSubmenu`). `pushMenu`/`pushSystem` push a
  frame; Esc/← (`updateNormal`/`updateSubmenu`) pops one via `m.popFrame()`
  (never a bare `m.stack.pop()`, so the revealed frame's search is cleared) or
  `m.stack.pop()` for the fixed frames, and
  `m.stack.atRoot()` is what decides whether Esc there means "back" or arms
  `exitArmed`'s double-Esc-to-exit (below) — one generic mechanism instead of
  hand-checking two different fields. `isForwardEntry` (`"menu"`/`"config"`/
  `"permissions"`/`"system"`/`"run"`/`"session"`/`"watch"`, plus
  `"login"`/`"logout"`/`"remove"` since decision 0039) is what lets `→` act
  as an alias for Enter on exactly the rows that open a further screen — a
  no-op on every leaf row (`status`/`doctor`/…/`version`), so `→` never
  accidentally executes a command; the same screens' footers add a `"→",
  "Open"` hint, omitted from the System submenu (nothing to open further
  from there). `watch` joined this set last (decision 0029, below), the
  same way `run`/`session` did before it.
  `rootEntryLabel` renders the same distinction visibly, not just behaviorally:
  a trailing `" →"` appended to a forward entry's name in the command-name
  column itself (`"menu →"`, matching `configItemLabel`'s identical convention
  on the Settings menu — see below) —
  purely a rendering-time decoration computed in `rootRow`/`viewNormal`/
  `viewSearch`, never written back into `rootPickerEntry.name` itself, so search
  ranking and `m.picked` keep matching on the real, undecorated name. `nameWidth`
  (the column every row's metadata aligns against) is measured against this
  decorated label, not the bare name, so a row with an arrow still lines up with
  the rest.

  The five-shade rail comes from `deriveAccentShades` (tui.go): n perceptually
  darkened variants of the current `accentMode`, computed once per picker launch via
  lipgloss's own `Darken` (not hand-rolled RGB/HSL, and not a fixed per-group palette
  — the whole gradient follows whatever Accent is configured, see `cpro config
  accent`), from the most subdued (index 0, `rootGroups`' first entry, "Core") to
  `accentMode` itself unchanged (the last index, "About"). `rootGroupShadeIndex` maps
  a row's group to that shade; `renderRootPanel` (this file, not tui.go's
  single-color `renderPanel` — a second, per-line-colored sibling built specifically
  for this one screen's multi-color rail, rather than reworking `renderPanel`'s
  signature for every existing caller) applies it per "│", with the panel's opening
  "╭─" always shade 0 and closing "╰─" always the last shade. The cursor ("❯") is
  always styled in the plain `accentMode`, never the row's own group shade — a
  deliberate, spec'd distinction between "rail shade = grouping" and "Accent = active
  focus." `dimStyle` (tui.go, SGR faint) gives a row's metadata secondary visual
  weight without its own color.

  Typing any printable character while browsing switches straight into a flat,
  ranked search (`rootPickerScore`: exact name > name prefix > name substring > name
  fuzzy/subsequence match (`subsequenceMatch` — a small dependency-free stand-in;
  cpro has no fuzzy-matching library to reuse) > short-metadata substring > description
  substring, the last three gated behind `fuzzyMatchMinLength` so a one-/two-character
  query doesn't flood the list with incidental hits — see its comment for why that
  exact cutoff was picked) with no explicit shortcut, and back out on Esc or emptying
  the query with Backspace; both states live in one `tea.Program`, avoiding flicker
  between them. `searchLine` is a body row (`"  Search:"`, growing to `"  Search:
  co_"` once typing starts — the trailing "_" a visual cursor) right below the
  panel's own title border — unlike the border-merged search field this screen used
  before titles existed, the border now carries only `screenTitle(frame.title)`, so
  the two don't compete for the same line (matching the task's own Root/Menu
  mockups, which both show "Search:" as its own row under the title); search mode
  also collapses the rail to one flat `accentMode` color instead of the five-group
  gradient, since a filtered result set isn't several groups anymore. Like
  configApp, this screen uses single-Esc (no "press again" warning) — after Enter
  picks a command, `pickCommandArgs` still calls the existing `pickRequiredArgs`/
  `requiredArgTokens` (ui.go) unchanged for any command needing positional
  arguments. Since decision 0039 no picker-reachable command actually does:
  `login`/`logout`/`remove` supply their own EMAIL in-app (an account list for
  the latter two, a `frameEmailInput` for `login` — see `pushAccountArg`), and
  `pickCommandArgs` returns such a pick immediately rather than prompting for an
  argument it already has. That path stays as the correct fallback for a future
  command that adds a required argument, and is still used outside the picker.
  Cancelling (Esc/Ctrl+C) returns
  `huh.ErrUserAborted`, not a bespoke error — `cliError` (ui.go) special-cases that
  exact sentinel to exit quietly instead of showing an ERROR box, and only recognizes
  that one.

  `pickCommandArgs`'s `tea.NewProgram` passes `os.Stderr` directly to
  `tea.WithInput`/`tea.WithOutput`, not `root.ErrOrStderr()`: under `NO_COLOR`/
  `TERM=dumb` that's a `*colorprofile.Writer` (see `uiOutput`, ui.go), not a real
  `*os.File`, and bubbletea's render loop never produces a first frame at all without
  one — confirmed live, it spins indefinitely instead of erroring. No color-profile
  option is needed either way: `m.color` already decides, in every string this screen
  builds (`styleText`/`dimStyle`), whether a color escape is emitted in the first
  place, so bubbletea itself has nothing color-related left to convert.

  `View()` also sets `tea.View.AltScreen = true` — the full command list (12 rows
  plus 4 blank group separators, the panel edges, description, and footer, ~22
  lines) is taller than plenty of real terminal windows, and without a dedicated
  alt-screen buffer, bubbletea's default renderer repositions the cursor with
  *relative* up-moves that assume the previous frame is still fully on screen; once a
  frame taller than the window has forced the terminal to scroll, that assumption
  breaks and every redraw overlaps stale content instead of replacing it (confirmed
  live: reported as a duplicated panel header and a truncated command list,
  reproduced with a 15-row pty — see `TestRootPickerResponsive`'s "short terminal"
  subtest). The alt screen sidesteps
  this entirely, the standard fix for exactly this problem in any full-screen
  bubbletea program; `cpro config`'s smaller settings menu doesn't need it and isn't
  changed. The alt screen alone only stops the corruption, though — it doesn't make a
  taller-than-the-terminal list scrollable on its own; see `scrollLines`, above,
  for the piece that does.

  `exitArmed` (normal-mode-only; Esc while actively searching keeps its own
  existing meaning, clearing the query, and never touches this) restores cpro's
  usual double-Esc-to-exit convention for this screen's own outermost frame
  (`m.stack.atRoot()`): a first Esc arms it, a second consecutive one quits, and
  any other key cancels the arm while still doing its own normal thing
  (`updateNormal`) — the same "any other key clears it" rule `escGuardField`
  (ui.go) uses one level down. Two signals fire together while armed:
  `railShade` replaces the whole five-shade gradient (rail rows and both panel
  edges alike, plus the search line) with one solid `dangerColor` (ui.go — the
  same user-configured Danger color `cpro config` uses, not a fixed hex) —
  deliberately not tinting the existing gradient, which would read as a sixth
  shade rather than a warning — and the footer's own key hint changes from
  `"Esc²", "to exit"` to a single pre-styled `"Esc¹ again"` segment, also in
  `dangerColor`. Both revert the instant the arm is cancelled. The arm also
  expires on its own if nothing happens: `arm` (rootui.go) starts a
  `tea.Tick(exitArmTimeout, …)` (2s) tagged with the current `exitArmedGen`,
  delivered back as `exitArmExpiredMsg`; `Update` only actually clears
  `exitArmed` if that message's `gen` still matches `m.exitArmedGen` — bumped
  on every new arm, never on a cancel — so a stale timer left over from an arm
  that was already cancelled (and possibly re-armed since, with its own fresh
  timer) can never clear a newer one. `TestRootPickerExitArmed`'s own
  generation-race subtest delivers `exitArmExpiredMsg` values directly rather
  than actually invoking the real `tea.Tick` command (which blocks for the
  full `exitArmTimeout` internally) or sleeping in the test.

  `rootPickerApp` also owns two deliberate exceptions to being otherwise flat.
  Selecting "system" doesn't pick a command directly, it pushes the small
  `frameSubmenu` frame (`updateSubmenu`/`viewSubmenu`, listing
  `systemSubmenuItems` — system.go) for `system export`/`system import`, so the
  picker gets one root-level row instead of two. Esc/← on the submenu pops back
  to whatever frame pushed it (the same convention `configApp`'s own nested
  sub-screens use — configui.go), not `exitArmed`'s double-Esc, which stays
  reserved for the stack's own root frame.

  Selecting "menu" pushes a `frameList` frame the same way (`pushMenu`), one
  level up — it does not start a second `tea.Program`, and "menu" is never
  itself returned as a picked command, same as "system" above. "config" is the
  one entry `pickCommandArgs`'s own loop (not `rootPickerApp` itself) special-
  cases: `cpro config` is a separate cobra command with its own interactive
  program (`configApp`), not a frame this screen can render in place, so rather
  than returning `["config"]` straight up (which would run it and end the whole
  picker, with no way back), the loop calls `runConfigUI(root, s, c, hasParent:
  true)` right there and, on a `true` `backOut`, relaunches `rootPickerApp` with
  its `navStack` resumed exactly as it stood the moment "config" was picked
  (`resume` in `pickCommandArgs`) — so the round trip through that second
  program looks, from the outside, like popping back to Menu the same way any
  in-process frame does. `updateSearch`'s own Enter/`→` handler treats "menu" the
  same way `updateNormal` does, so typing to search for it and confirming from
  search mode drills in identically. "permissions" (decision 0021) gets the
  identical round trip, against `runPermissionsUI` instead of `runConfigUI` —
  the one difference is that its `backOut` also refreshes the resumed Menu
  frame's own "permissions" entry (`applyLiveMeta(m.stack.current().entries,
  s)`, re-reading config fresh) before resuming, since a change made inside
  that screen (the permission mode) is exactly the one thing this row's live
  metadata needs to reflect — `"config"`'s own round trip needs no such
  refresh, since it can no longer change anything this picker displays once
  Permissions moved out of it.

  `run` (decision 0019) joined `isForwardEntry` too — selecting it no longer runs
  `cpro run` directly, it pushes `frameRunAccount` (`pushRunAccount`), STEP 2 of the
  interactive run flow: a searchable list of registered accounts, built on the same
  browse/search pattern `exportPickerApp` (system.go) already proved out
  (`updateRunAccount`/`updateRunAccountSearch`/`refilterAccounts`, a plain
  case-insensitive substring match, not `rootPickerScore`'s command-ranking tiers).
  Every row here renders through `displayEmail` (decision 0024, ui.go), and
  `refilterAccounts` matches the query against `displayEmail(e.name)` too,
  not the real address — while masking is on, the row shown and the row
  matched are always the same string.
  Each row also carries that account's live Session usage (decision 0031):
  `accountPickerRow`/`runAccountSessionCell` render `S █ 79%`, the `█`
  colored by the shared `usageColorFor` (ui.go), so a configured
  Warning/Danger threshold or color applies here identically — never a
  second threshold rule. `pushRunAccount` returns `(tea.Cmd, error)`, not
  just an error: the
  frame is pushed and drawn immediately and that cmd (a `tea.Batch`, one
  closure per account — bubbletea's own goroutine per command is the
  concurrency, no hand-rolled fan-in like `renderAccountSnapshot`'s in
  main.go) fetches through the *same* `loadUsage`/`usageEndpoint` and 60s
  `cpro-usage.json` cache `cpro status`/`watch` use; there is no second
  usage cache or freshness policy. Each result arrives as its own
  `runAccountUsageMsg` and updates only that row — `Update`'s handler
  deliberately touches neither `frame.cursor` nor `m.fcursor`, so usage
  landing mid-navigation can never move the selection. `rootPickerApp.
  accountUsage` holds the results, keyed by the **real** email (an alias is
  display-only), and lives on the app rather than the frame so search
  re-renders from cache and a fully-cached revisit returns a `nil` cmd
  instead of refetching. Unknown or failed usage renders a dim `S ·   --`
  placeholder — never `0%`, which is a real value — and never blocks
  selecting that account or prints an inline error.
  `accountPickerNameWidth`
  measures the *displayed* name (ANSI-aware `visibleWidth`) for alignment,
  and `accountPickerRow` degrades across three tiers as the terminal narrows
  (full cell → percentage only → identity only), never wrapping; `padStart`
  (ui.go, `padEnd`'s sibling) keeps the percentage column straight from
  `  0%` to `100%`.

  **Three of those pieces are shared with DEFAULT ACCOUNT** (configui.go's
  `screenDefaultAccount`) since decision 0036, and are therefore plain free
  functions rather than `*rootPickerApp` methods:
  `fetchAccountPickerUsage(s, emails)` (the one usage-fetch implementation
  behind both screens), `accountPickerRow(color, width, email, usage,
  selected, nameWidth)` (the one row renderer — it owns masking, the label
  column, the Session marker and percentage, threshold color, the loading
  placeholder, ANSI-aware alignment, and narrow-terminal degradation), and
  `accountPickerNameWidth(emails)`. `(*rootPickerApp).runAccountRow` and
  `runAccountNameWidth` survive only as thin adapters unpacking
  `rootPickerEntry`/`m.color`/`m.width`/`m.accountUsage`, so RUN ACCOUNT's
  own call sites read exactly as they did before. The rule this encodes:
  **any account picker where the user must choose an account shows the same
  Session usage, through this same component** — adding a third such screen
  means calling these, never writing a parallel renderer or fetcher.
  `pushRunAccount` returns an error instead of pushing when there are no registered
  accounts, stored in `rootPickerApp.runFlowErr` and surfaced by `pickCommandArgs`
  after the program exits, the same way `configApp.err` surfaces a save failure —
  a TUI frame has no error box of its own to show one inline. Enter there calls
  `pushRunMode(account)`: STEP 3, `frameRunMode`, the same 5 `permissionModes` rows
  (permissions.go) the Permissions screen itself renders, cursor preselected from
  `effectivePermissionMode(c.PermissionMode)` — the saved Settings default — read
  once when the frame is pushed. Esc/← on either frame pops back one step
  (RUN MODE → RUN ACCOUNT → ROOT), never `exitArmed`'s double-Esc, which stays
  reserved for the stack's own root frame. → is a consistent forward-select
  alias for Enter on both frames (decision 0022): `updateRunAccount`/
  `updateRunAccountSearch` treat `"right"` the same as `"enter"` (select the
  highlighted account, push RUN MODE), and `updateRunMode`'s own Enter *or*
  → is STEP 4: it builds `["run", "--account", account,
  permissionModeArgs(mode)...]` — exactly what typing that command by hand
  would produce — and sets it as `m.picked`. Neither an account row nor a
  mode row gains a trailing `" →"` of its own even though → does something on
  it: both are selectable values, not child screens, so `isForwardEntry`'s
  labeling convention deliberately doesn't apply here — only each screen's
  own footer hint reflects the new key. RUN ACCOUNT's actively-searching
  footer gained the matching hint too, but kept its existing `"Esc",
  "Clear"` (Esc still only clears the query mid-search, same as every other
  search screen in cpro — confirmed deliberately, not a mockup's `"Esc
  Back"` wording taken literally).
  `pickCommandArgs` special-cases `m.picked[0] == "run"` the same way it already
  special-cases `"config"`: it parses the argv back apart with `runArgs` (main.go —
  the same parser `cpro run`'s own RunE uses) purely to print the decision-0007
  "Running: ..." announcement (`announceCommand`, ui.go), then returns the argv
  unchanged for the caller's existing `cmd.SetArgs(picked); cmd.Execute()` — so this
  flow runs through the exact same non-interactive `run` RunE and
  `applyPermissionDefaults` (permissions.go) as direct `cpro run`, never a second
  argument-building path.

  `watch` (decision 0029) got the same "opens a screen, not an immediate command"
  treatment `run` already had, joining `isForwardEntry` and gaining its own `" →"`
  label. Selecting it pushes `frameWatchMode` (`pushWatchMode`) — a small,
  fixed-choice list (`watchModeItems`: "Full"/"Compact", each with an inline
  description, plus the separate "Interval" row added by decision 0045), the same
  small-fixed-list shape `frameSubmenu` (System credentials) already uses rather
  than a searchable one, since there's nothing to type-to-search over three rows.
  It always starts on "Full" (cursor 0) and the 60s default interval — there's no
  persisted watch-mode preference to preselect from, matching plain `cpro watch`'s
  own no-flag default. `updateWatchMode`: ↑/↓ move the cursor over all three rows,
  Esc pops back to ROOT (never `exitArmed`'s double-Esc, reserved for the stack's
  own root frame, same as RUN ACCOUNT/RUN MODE/the System submenu), Enter or →
  (decision 0022's forward-select convention) on a mode row finalizes into the
  exact `["watch"]`/`["watch","--compact"]` argv typing either command by hand
  would produce — plus `--interval D` when the chosen interval is not the 60s
  default (decision 0045) — this frame only ever decides which already-supported
  invocation to build, reusing `watchLoop`/`renderFullView`/`renderCompactView`
  entirely; there is no new watch implementation. On the Interval row ←/→ step
  through `watchIntervalChoices` (clamped at 5s/15m) and Enter does nothing, so
  reaching the interval can't accidentally start a watch; nothing found there is
  ever persisted. Direct `cpro watch`/`cpro watch --compact`/`cpro watch
  --interval D` are completely unchanged — no picker, no WATCH MODE frame, only
  the interactive picker's own "watch" entry gained this extra step.
- **system.go** — `cpro system export [EMAIL]`/`cpro system import`: explicit,
  one-way credential transfer between a cpro-managed account and the real,
  unmanaged Claude Code credential store (`systemConfigPaths`, claude.go) —
  deliberately not a second "default account" concept, and kept separate from
  `cpro run` (main.go/claude.go), which still always needs an explicit account.
  `exportAccount` reuses the exact same `authStatus`/`validAuth` check every
  other command that touches a session already uses before copying
  `.credentials.json` out; it never touches `config.json` at all. `importSystemAccount`
  never hand-parses the system file's shape to guess whose account it is —
  it stages the raw credentials into a temp `CLAUDE_CONFIG_DIR` and asks the
  real `claude auth status --json` (via `authStatus`, the same helper `login`
  uses) who it belongs to, then normalizes that email the same way
  (`normalizeEmail`, store.go) every other account identity in cpro is
  validated. The actual persistence goes through `installCredentialsFile`
  (claude.go) — a sibling of `installLogin` that touches only
  `.credentials.json` (never `.claude.json`, so an existing account's
  settings/trust history and any other profile data are left completely
  alone) and never sets `config.Default`, unlike `installLogin`, which still
  does for a real `login` (first account logged in stays the default — see
  main.go). `exportPickerApp` is export's own account picker when no EMAIL is
  given — a small custom `tea.Model` sharing `tui.go`'s chrome, not the older
  huh-based `pickAccount` (ui.go, removed — decision 0019), because this screen's
  own spec calls for immediate type-to-search, which a plain `huh.Select` never
  had; single-Esc cancels it, matching every other screen built this way. Its
  browse/search state machine is the template decision 0019's own RUN ACCOUNT
  frame (rootui.go) and the Settings "Default account" screen (configui.go) both
  follow — including, since decision 0024, rendering/matching every row
  through `displayEmail` (ui.go) rather than the real email directly.
  `systemExport` (decision 0028) wraps `exportAccount` with a second,
  report-only step for Claude Desktop (`exportToClaudeDesktop`, desktop.go):
  `export`'s RunE now prints a per-target report
  (`printCredentialTargetResults`) instead of one flat confirmation — see
  desktop.go for why Desktop is detection/reporting-only, never a second
  credential write.
- **desktop.go** (decision 0028) — Claude Desktop as a second, read-only
  `cpro system export` target. `claudeDesktopConfigDir`/`claudeDesktopInstalled`
  detect Desktop independently of `CLAUDE_CONFIG_DIR` (a Claude-Code/cpro-only
  concept Desktop's own single, per-machine Electron config directory has
  nothing to do with) via its `claude-desktop` binary on PATH or its own
  config file's presence — `CPRO_TEST_NO_CLAUDE_DESKTOP=1` forces "not
  installed" for tests, the same override idiom `CPRO_TEST_NO_YOLO_SUPPORT`
  (permissions.go) already uses. `exportToClaudeDesktop` never writes
  anything: verified live that Desktop's own top-level account session is a
  separate, Electron-`safeStorage`-encrypted OAuth token
  (`oauth:tokenCacheV2` in its own `config.json`) issued to a different OAuth
  client than Claude Code's CLI, with no safe way to produce or transfer one
  from a cpro credential — it only ever reports `targetNotInstalled` or
  `targetSignInRequired` (with a fixed, sanitized detail string), never
  `targetUpdated`, so a false "Desktop was updated" claim is impossible by
  construction, not just by care. Desktop's own *embedded* Claude Code
  engine (used for its cowork/local-agent features) already reads the exact
  same `CLAUDE_CONFIG_DIR`-scoped `.credentials.json` the standalone CLI
  does — confirmed by inspecting Desktop's own app bundle — so that piece
  needed no code change at all, mirroring decision 0027's "already shared"
  finding for `cpro session continue`.
- **session.go** — `cpro session continue FROM_EMAIL TO_EMAIL`: hands off an
  in-progress Claude conversation from one cpro account to another, for the
  case that prompted it — `FROM_EMAIL` hit its usage limit mid-conversation
  and the user wants to keep going under `TO_EMAIL` without hand-copying
  files or guessing a session ID (see decision 0012). `projectDirName`
  reproduces the one piece of Claude Code's own on-disk layout this needs —
  the `CLAUDE_CONFIG_DIR/projects/<name>` folder Claude Code picks per working
  directory — by replacing every `/` in the absolute cwd with `-`; this is
  deliberately only as much of Claude Code's own encoding as was actually
  observed across every real project folder on the machine this was built on,
  not a guessed-at full reimplementation. `continueSession` copies every
  `*.jsonl` transcript for the current directory's project folder from
  `FROM_EMAIL`'s profile into `TO_EMAIL`'s — skipping (never overwriting) a
  same-named file already present at the destination, and never touching or
  deleting anything under `FROM_EMAIL` — then the command calls the same
  `s.run` (claude.go) `cpro run` itself uses, with `--resume` and no session
  ID, so Claude's own resume list (previews included) is what picks the exact
  conversation rather than cpro guessing one from file-modification times
  (the wrong guess, twice, in the live session that prompted this feature).
  Only `TO_EMAIL` is locked (shared, `sessionCopyLock` — decision 0063) for
  the copy; `FROM_EMAIL` is only ever read here, unlocked, the same way
  `exportAccount`'s own read of a source account takes no lock either. Both
  accounts must already be registered, and `FROM_EMAIL` must have at least
  one recorded session for the current directory, or the command errors
  before copying anything. `continueSession` now also takes an explicit
  `sessionID` (decision 0025): when non-empty, the project directory to copy
  is found by `findSessionDir` (scanning `FROM_EMAIL`'s own `projects/*` for
  that session's transcript) instead of the current process's own working
  directory — the interactive session picker (sessionui.go) always supplies
  one, since the session it lets you pick may not belong to whatever
  directory `cpro` happens to be run from; an empty `sessionID` preserves the
  exact original cwd-based behavior for the plain, argument-only invocation.
  `resumeSessionIDArg` pulls that ID back out of the forwarded
  `--resume SESSION_ID` arguments the command already supported (decision
  0012), so both the manual and the picker-built paths share one lookup.
  `sessionEntry`/`listSessions` enumerate every recorded session (one row per
  `*.jsonl`) across every registered account's own `projects/*`, newest first
  by file mtime — the data source behind the interactive picker below, never
  a second index to keep in sync with what `continueSession` itself reads.
  `projectDisplayPath`/`projectDisplayName` best-effort reverse
  `projectDirName`'s own lossy `"/"` → `"-"` encoding for a readable label
  (acknowledged-lossy when a real path segment has its own `"-"`, the same
  known limitation `projectDirName` itself already documents);
  `shortSessionID` only ever shortens the *displayed* ID, never the one
  actually used to resume.

  `resolveSession`/`resolveSessions`/`deleteSessions` (decisions 0051/0052) add
  the missing destructive half of this file. `resolveSession` turns a full
  session ID, or an unambiguous prefix of one, into exactly one `sessionEntry` —
  erroring on both "nothing matches" and "this ID exists under two accounts" (a
  real case after a `continue` copy) instead of guessing, with `--account EMAIL`
  both filtering the search and disambiguating that second case.
  `resolveSessions` resolves every selector of a variadic `session delete` in
  order, collapsing duplicates and aborting the whole call on the first bad
  selector so a typo can never delete a subset; each selector may also name its
  own account as `EMAIL:SESSION_ID` (`parseSessionSelector`), which is how one
  invocation deletes a batch spanning accounts — a single `--account` cannot say
  which account each ID belongs to once the same ID exists under two.
  `deleteSessions` removes each listed `<sessionID>.jsonl` and nothing else:
  cpro owns no other representation of a session, so removing that file is what
  makes it disappear from `session list`, both session pickers and `--resume`'s
  own discovery alike. It groups entries by owning account and takes each
  account's exclusive lock once around all of its removals rather than once per
  file, and joins every failure rather than stopping — one already-gone entry
  cannot block the rest of a cleanup. An account whose lock is already held
  (another cpro operation in flight under it) is skipped whole and reported by
  name with the number of sessions it cost; that lock is non-blocking by design,
  so a cleanup would otherwise hang behind an unrelated command. `deleteSession` (singular) is the same
  function for one entry.

  Two guards, both `removeConfirmMatches`-shaped (decision 0042): a single
  session must be restated by its full ID or the short head…tail form the UI
  displays (`deleteConfirmMatches`), while several must be restated by typing the
  literal word `delete` (`bulkDeleteConfirmMatches`) — retyping twenty IDs is not
  a guard anyone would pass, and a bare Enter would be too reflexive. The
  `delete` subcommand is otherwise guarded the way `remove` is: a terminal
  prompts, non-interactively it refuses unless `--yes`. The interactive SESSIONS
  screen's own `delete` row (sessionui.go) runs this exact command rather than
  deleting anything itself.

  `resumeSession` (decision 0032) is the implementation behind root-level `cpro
  --resume SESSION_ID`/`cpro -r SESSION_ID` (main.go's hidden `__resume` command
  dispatches here): `findSessionOwner` locates the session across every registered
  account by reusing `listSessions` (never a second scan), account resolution
  defaults to the discovered owner if it has a live session, falls back to an
  explicit `--account` or (with no override) `pickResumeAccount`'s interactive
  RESUME ACCOUNT picker (resumeui.go) otherwise, and never writes `config.Default`
  either way. `migrateSession` copies only the one requested session transcript
  (narrower than `continueSession`'s own whole-directory copy) into the target
  account's profile when it differs from the owner — copy-and-verify, skipping an
  already-present destination file, never touching or removing the source.
  `restoreSessionDirectory`/`sessionWorkingDirectory` read the transcript's own
  recorded `"cwd"` field directly (more precise than reversing `projectDirName`'s
  lossy encoding) and best-effort `os.Chdir` into it, falling back to the current
  directory with a stderr diagnostic if it's missing or unreadable rather than
  failing the whole resume. Ends in the identical `s.run` (claude.go) `cpro run`/
  `cpro session continue` already call, so all three share one permission resolver
  — see decision 0032 for the full rationale, including why the rewrite happens in
  main.go rather than as a cobra flag.
- **resumeui.go** (decision 0032) — `resumeAccountPickerApp`, the RESUME ACCOUNT
  fallback picker `resumeSession` (session.go) opens when a session's automatically
  discovered owning account can't actually be used (signed out, invalid session) and
  no explicit `--account` was given: the same searchable, single-Esc-cancels
  `tea.Model` shape as `exportPickerApp` (system.go)/rootui.go's own RUN ACCOUNT
  frame, reused as its own small standalone `tea.Program` rather than a fourth
  reimplementation of the same browse/search state machine. Deliberately excludes
  the already-ruled-out owner account from the list (unlike sessionui.go's own
  DESTINATION ACCOUNT, which offers every account, owner included — decision 0063).
- **sessionui.go** (decision 0025) — `cpro session`'s interactive screen:
  `sessionApp`, the same shape `configApp` established — a small custom
  `tea.Model`, `m.stack` (`navStack[sessionScreen]`), `hasParent`/`backOut`
  deciding whether Esc/← at the stack's own root backs out to the picker
  (opened via the root/menu picker's own `"session →"` round trip,
  `pickCommandArgs`, rootui.go) or arms the double-Esc-to-exit convention
  (`exitArmed`/`arm`, reusing rootPickerApp's `exitArmTimeout`/
  `exitArmExpiredMsg` directly, exactly like `configApp` does) for a direct
  invocation. `runSessionUI` seeds `screenSessionMenu` (three rows: `continue
  →`, `list`, `delete →`) as the root — bare `cpro session`, and the picker's
  own entry.
  `runSessionContinueUI` instead seeds `screenContinuePicker` itself as the
  root, skipping the menu — `cpro session continue` called with **no**
  arguments (the only new non-interactive-vs-interactive branch point;
  `FROM_EMAIL TO_EMAIL [...]` stays required and unchanged for the explicit
  path, confirmed with the user rather than inventing a bare-session-ID
  calling form). `screenSessionList`/`screenContinuePicker` share one
  `sessionListState` (browse/search over `listSessions`' own entries) via
  `m.pickerMode`, with one `pickSession` deciding what Enter/→ does on the
  highlighted row so the browse and search paths cannot diverge: "list" is
  read-only (nothing), "continue" pushes `screenContinueAccount` (the
  destination-account picker), and "delete" pushes the retype guard below. `refilterPicker` matches project name, decoded path, the full
  session ID, and the owning account through `displayEmail` (decision 0024)
  — never a real email while masking is on. DESTINATION ACCOUNT offers every
  registered account, the session's owner included (decision 0063: the user
  picks, from each row's live Session usage — cpro never guesses), with a
  `continuingNote` naming the session and its source account below the panel.
  Selecting one (`finalizeContinue`) builds `["__resume", ID, "--account", TO]`
  — `cpro --resume`'s own path (`resumeSession`, decision 0032: one-transcript
  copy when TO isn't the owner, cwd restore, then `s.run`) — and sets it as
  `m.picked`; `sessionApp` never migrates or runs anything itself. Transcript
  copies (`continueSession`/`migrateSession`) take the destination's lock
  *shared* (`sessionCopyLock`), since claude's background helpers inherit
  `s.run`'s shared lock and outlive the session — an exclusive lock there
  blocked every move to a busy account (decision 0063, same cause as 0053).
  Decision 0064 annotates the pickers: rows carry the session's title
  (`sessionTitle`, session.go — the latest `custom-title`/`ai-title` record
  read from a bounded tail window, else the first real prompt from the head;
  loaded asynchronously via `loadSessionTitles`/`sessionTitlesMsg` and part of
  the search haystack), a `● running` tag on sessions a live Claude process is
  using (`activeSessionKeys`), and CONTINUE SESSION shows one row per session
  ID (`dedupeSessions`, newest copy, `also in …` for the rest); DESTINATION
  ACCOUNT tags signed-out accounts (`fetchSessionAuth`, `destinationLines`)
  without blocking them. The `delete` row keeps that same discipline (decisions 0051/0052):
  `beginDelete`/`updateDeleteConfirm`/`viewDeleteConfirm` (a new
  `screenDeleteConfirm` frame carrying `pendingDelete []sessionEntry` plus the
  `confirmInput`/`confirmErr` fields `rootPickerApp`'s remove-confirm has) render
  the guard in this program's own chrome — the shape decision 0042 gave `remove`,
  and for the same reason, since a polished picker ending in the command's bare
  terminal prompt is precisely the half-migrated state that decision removed. It
  is also this screen's multi-select: `deletePickerKey` intercepts Tab (check the
  highlighted row and advance, so a run can be marked with repeated presses),
  Space (the stay-put toggle; browse-only, since in search mode it types) and
  Ctrl+A (check every *visible* row, or clear them when all are already checked)
  before the shared browse/search machine sees them; checked rows render a
  leading `[x]`/`[ ]` marker through `pickerRowLine`, and `deleteSel` is keyed by
  `sessionEntryKey` so a check survives a search filtering the row out and back
  in. Enter deletes the checked visible set, or the highlighted row when nothing
  is checked, so single deletion stays one Enter. One session asks for its ID
  back; several ask for the literal word `delete`. On a match it finalizes
  `["session","delete",EMAIL:ID,…, "--yes"]` — account-qualified, because that
  exact ID can exist under two accounts after a continue copy. Nothing is deleted
  by this screen; the argv runs `cpro session delete`, which owns the removal.
  Rows render single-line (cursor, project name, dim truncated
  metadata), not the three-line mockup sketch this task's own spec included,
  to stay consistent with every other list in cpro (`rootRow`,
  `exportPickerApp`, RUN ACCOUNT) — all one-row-per-entry, responsively
  degrading rather than wrapping.

### Key invariants to preserve when changing this code

- **Never let environment-based auth override account selection.** `claudeCommand`
  rejects `ANTHROPIC_API_KEY`, `CLAUDE_CODE_OAUTH_TOKEN`, `ANTHROPIC_BASE_URL`, etc. —
  any change to child-process environment setup must keep this check.
- **Never copy a live refresh token during reauthentication.** `login` only seeds
  `.claude.json` (settings) into the staging dir, not `.credentials.json`; the new
  credentials always come from a fresh `claude auth login`.
- **Login is verify-then-install.** A login only overwrites the real profile after
  confirming the authenticated email and `authMethod == "claude.ai"` match the
  requested account, and `installLogin` restores the previous files on failure.
- **All config/profile mutations go through `store.update`/`installLogin`'s
  lock-then-read-then-write pattern**, and all file writes use `atomicWrite`. Don't add
  a direct `os.WriteFile` for anything under the config directory.
- **Manager commands print data to stdout and diagnostics to stderr**, return exit 0/1;
  `run` instead preserves Claude's own exit status/signal via `syscall.Exec`. Keep this
  split when adding commands.

## Testing patterns

- `TestClaudeProcess` (main_test.go) is not a real test: it's a fake `claude`
  executable implemented inside the test binary, gated by `CPRO_TEST_HELPER=1`. Other
  tests build `bin/cpro` and point `PATH` at the test binary re-invoked with that env
  var, so `claude auth login/status/logout` behavior can be scripted per test via
  further env vars (`CPRO_TEST_WRONG_ACCOUNT`, `CPRO_TEST_LOGIN_FAIL`,
  `CPRO_TEST_EXIT`, `CPRO_TEST_WAIT`). When adding CLI behavior that shells out to
  `claude`, extend this fake rather than mocking at a different layer.
- Interactive flows (login, the `run` account/mode picker) are
  tested through a pseudoterminal — keep terminal-detection logic
  (`terminalInput`/`terminalOutput`) mockable by checking real fds, not by adding
  parallel test-only code paths. When a single `cpro` invocation shows two sequential
  prompts, `TestCLI` avoids exercising them in one process: huh's accessible mode
  builds a fresh `bufio.Scanner` per prompt, so two answers written back-to-back onto
  the same pty risk the first prompt's scanner reading ahead into the second answer.
  Prefer skipping the earlier prompt (e.g. pass `--account` to isolate the mode picker)
  over adding synchronization to make two prompts safe in one process.
- `usage.go` is tested against `httptest.NewServer`; `loadUsage` takes the endpoint as
  a parameter for exactly this reason — preserve that when touching it.
- `TestInteractivePicker`, `TestEscGuard`, `TestRootPicker`, `TestRootPickerExitArmed`
  (plus the pure unit tests `TestRootLauncherEntries`/`TestRootPickerEntries`/
  `TestRootPickerScore`/`TestRootPickerRefilter`/`TestRootPickerNoResultView`),
  `TestWatch`, `TestConfigPreferences`, `TestConfigUI`, and `TestInfoCommand` cover
  the shared root command picker (rootui.go — both bare `cpro` and `cpro menu`,
  see rootui.go above) and `cpro info` (maintenance.go). Since bare `cpro` now
  shows only the reduced launcher, most subtests that exercise secondary/admin
  commands (`login`, `config`, `version`, ...) launch `cpro menu` instead of bare
  `cpro` — when adding a picker subtest, launch whichever of the two actually
  lists the command under test, and recompute any `for range N { down(master) }`
  step count against that list's actual order (`rootLauncherNames`/
  `rootMenuNames()`), not the other one's. `TestRootLauncherEntries` asserts the
  reduced launcher's exact order and that no secondary command leaks into it;
  `TestRootPickerEntries` does the same for `cpro menu`'s full list and asserts
  neither `"menu"` (which would let it nest) nor `"list"` (deliberately picker-
  absent, see rootui.go) appear in it. `TestInfoCommand` checks `cpro info`'s
  output format, that its account count reflects real registered accounts, and —
  mirroring the no-secrets checks `cpro system`'s own tests already use — that no
  credential value ever appears in its output, even though the command never reads
  credential files to begin with. `TestRootPicker`'s "Esc from the full palette
  (opened via menu) goes back to the launcher, not exit" and "double-Esc still
  exits the launcher after backing out of the full palette" subtests cover
  `pushMenu`'s in-place transition onto `m.stack` (rootui.go): a live
  process-alive check (`cmd.Process.Signal(syscall.Signal(0))`) after a single
  Esc proves it backed out (`m.stack.pop()`) rather than quit, and re-entering
  "menu" from the restored launcher (three Downs reaching it again, not
  "login") proves the stack was genuinely popped back to the launcher frame
  rather than the frame just looking stale — the usual
  presence-only discipline this test already documents, since a raw pty capture
  can't prove absence. A full-list-sized transition like this one needs a
  longer settle (600ms, not the usual 300ms) before the next capture — confirmed
  live as a flaky read of a still-mid-redraw frame otherwise, not a real bug (a
  `tmux capture-pane` check of the same sequence rendered correctly at the
  normal timing). The double-Esc-to-exit convention (at the root
  picker's own
  Esc — `exitArmed`, which `TestRootPickerExitArmed` drives directly via
  `updateNormal`/`viewNormal`/`Update` with no pty, covering arming, confirming,
  every cancellation path, the rail actually turning red and the footer
  switching to "Esc¹ again" in Danger color together and back, automatic
  expiry via `exitArmExpiredMsg`, and the generation check that keeps a stale
  timer from clearing a newer arm; its second subtest covered the huh EMAIL
  field `pickRequiredArgs` used to open for `login` and, since decision 0039
  replaced that with an in-app frame, now asserts the rule that actually
  governs there — a nested frame pops on a single Esc, proven by process
  liveness rather than text, since a cumulative pty capture already holds the
  popped-to screen), `cpro watch`, the scriptable `cpro config accent|bar|mask|warn-at|
  danger-at`, and the interactive `cpro config` screen (configui.go) respectively —
  the pty-driven ones via `buildCLI`/`openPTY`/`drainPTY`/`stripANSI` (factored out
  of `TestCLI`'s own, untouched, inline setup). `TestWatch` deliberately registers
  no accounts, so every redraw takes the fast "No accounts found" path — real usage
  fetching is exercised elsewhere, and pulling it into a timing-sensitive test would
  make the timing depend on network reachability too. `TestConfigUI` drives the live
  bubbletea screen by key sequence (arrow keys, Enter, Esc) and asserts against
  `s.read()` rather than printed confirmation text, since `configApp` persists
  silently (via `saveXxx`, not `setXxx` — see config.go) while the screen stays up;
  it also runs a plain command through the same pty right after `cpro config` exits,
  as the simplest reliable check that the program restored the terminal correctly.
  `TestRootPicker` follows the same pattern, plus one specific to a list-shaped
  screen: bubbletea's diffing renderer only rewrites what actually changed between
  frames (e.g. a same-screen cursor move may touch only the description line, not
  retransmit whole rows), and this test setup captures every frame cumulatively — so
  a check for whether some text is ABSENT from the final state is unreliable (it may
  simply be sitting in an earlier, by-then-superseded frame, still physically present
  in the captured bytes even though a real terminal would show it overwritten).
  `TestRootPicker`'s assertions are therefore presence-only, verifying "which command
  actually ran" as the ground truth for navigation, and one-shot content rendered by
  a fresh, single call (`TestRootPickerNoResultView`, calling `viewSearch()` directly)
  instead of a pty capture wherever presence-only checks in a live run can't
  distinguish two states — see the comment at the top of `TestRootPicker` for the
  full rule. `TestInteractivePicker`'s own "group boundaries are invisible"
  subtest hit this same rule from a new angle: a cursor move onto an *adjacent*
  row (one Down, not several) can paint as a genuinely fragmented diff — cursor-
  repositioning writes that split a line like "Sign in to a Claude account"
  across raw bytes a plain substring search can't reassemble, even though the
  cursor landed correctly — so it asserts "which command Enter actually runs"
  (login's own EMAIL prompt) instead of grepping the cumulative capture for the
  description text; reach for the same fix (a ground-truth action, not a text
  search) if a new single-step-adjacent-row test flakes the same way.
  `screenTitle` (tui.go) and the `→`/`←` conventions this picker and `cpro
  config` both now use are covered directly rather than only through existing
  pty subtests: every screen's own `"claude cpro[ - NAME]"` title, `→` opening
  `menu`/`config`/`system` (rootui.go) or `theme`/`permissions`/a color picker/
  the Warning-Danger threshold sub-menu (configui.go) while doing nothing on a
  leaf row, `←` popping one level the same as Esc (except the threshold editor,
  whose `←`/`→` already mean decrease/increase), and a screen's footer showing
  `"→ Open"` only when it actually has a forward-navigable row. `TestScreenTitles`
  asserts the literal title string of every screen (Root/Menu/System submenu,
  rootui.go; Settings/Theme/Permissions/a color picker/a threshold sub-menu,
  configui.go; the export account picker, system.go) via one fresh render call
  each — not a live pty, for the same diffing-fragility reason.
  `TestNavigationHierarchy` walks the full Root -> Menu -> Settings ->
  Permissions tree end to end through a real pty and back out again,
  specifically covering the one cross-program seam the redesign added
  (`pickCommandArgs`'s own loop relaunching the picker, resumed exactly where
  "config" was picked, once `runConfigUI`'s `backOut` reports the user backed
  out of Settings) — the riskiest new code path, since everywhere else "back"
  is an in-process `navStack.pop()`. It needs a longer `context.WithTimeout`
  (20s) than most picker tests: it spawns two real processes end to end (the
  picker, then a nested `cpro config`), each doing its own `openStore`/terminal
  setup, which is measurably slower than an in-process frame update.
- `TestListRenderPrimitives` (pure, no process/pty) and `TestStatusViews` (pty-driven,
  via `buildCLI`/`openPTY`/`drainPTY`/`stripANSI`) cover `cpro status`'s full/
  compact/compact-narrow rendering — the detailed dashboard that used to live
  under `cpro list`, moved (unchanged) when `list` became a lightweight account
  listing; see `TestListLightweight` for that. `TestStatusViews` pins exact
  Session/Week percentages across the safe/warn/danger thresholds by writing a
  fresh `cpro-usage.json` cache straight into each seeded profile (see usage.go's
  `usageCache`) instead of fetching — `loadUsage` treats a cache under a minute
  old as authoritative and skips the network call entirely, which is what makes
  this deterministic and network-free. `resizePTY` (factored out of `openPTY`,
  which stays fixed at 40x120) re-applies a pty's `TIOCSWINSZ` after opening, for
  the specific narrow widths compact-narrow and full's own narrow-terminal
  behavior need to trigger. Decision 0033's card redesign added
  `TestStatusCardLayout` (pure, driving `renderFullView`/`newStatusCardLayout`/
  `statusUsageLine`/`statusSessionLine` directly against a `bytes.Buffer`, same
  reasoning as `TestListRenderPrimitives`): three accounts with deliberately
  different data lengths (percentages across 0/84/88/100, runtimes from 12m to
  17h 44m, a short vs. very-long — truncated — path, a 6- vs. 7-digit pid, an
  Expired account, an account with no usage data, and 3/1/0 sessions) assert
  every card's header/bottom is exactly `cardWidth`, singular/plural session
  count, every usage row's `%`/`-` and every session row's leading/trailing "·"
  land in the same rune column across every account, no session row ever
  exceeds `contentWidth` (never wraps), a too-long path truncates with "…", the
  exact blank-row rhythm, `Total week` staying outside every card, and no
  "Accounts"/divider text anywhere. `TestStatusViews`'s "full view" subtest was
  rewritten for the new per-card strings (`"╭─ EMAIL"` instead of one
  `"╭─ Accounts"`/`"├─"`/`"Active sessions"`, plus a negated list so the old
  labels can never silently reappear) and gained a live-pty alignment check
  (real ANSI styling doesn't break the `%`/`-` column match) and a
  `NO_COLOR`-vs-colored geometry-identical check; a new subtest spawns real
  `/proc`-visible sessions (not seeded cache data) to prove the header's
  singular/plural wording and session rows' real path/pid/no-wrap behavior
  end to end through an actual pty.
- `TestDeriveAccentShades`, `TestRootRowResponsive`, and `TestSubsequenceMatch`
  (all pure, no pty) cover the root picker's gradient math, its per-row metadata
  responsiveness, and its dependency-free fuzzy matcher directly.
  `TestRootPickerGradient` and `TestRootPickerResponsive` (pty-driven, via
  `resizePTY`) cover the rest live: which rail shade each command's row actually
  renders in, that the cursor stays in the plain Accent color regardless of its
  row's shade, that `cpro config accent` changing the preference is reflected in
  the very next picker launch, that search mode collapses to one Accent color,
  and metadata truncating/disappearing (never the command list wrapping) as the
  terminal narrows. The "cursor on a different group's row" case is rendered
  directly (`m.viewNormal()` with `cursor` set to the target index) rather than
  driven live through a pty, for the same reason `TestRootPicker` gives for its
  own presence-only assertions: bubbletea's diffing renderer may only rewrite a
  couple of cells on a same-screen cursor move, not the whole destination row, so
  a cumulative pty capture can't reliably prove what color a non-default row's
  cursor ends up in — a fresh, single render sidesteps that class of flakiness
  entirely. `TestRootPickerGradient`'s color assertions match against the
  ANSI-stripped line to locate the right row (a raw line carries a color-reset
  escape between "❯" and the command name, which breaks a plain substring search
  for "❯ run") but return the original, still-styled line, and only consider
  actual panel rows (`│`/`╭`/`╰`-prefixed) — excluding the below-panel description
  line, whose text can otherwise collide with a command name substring (e.g.
  "config" inside "...cpro configuration..."). Sending a multi-byte key sequence
  (an arrow key) to the pty must happen as one atomic write, never one rune from
  the Go string at a time with a delay in between — a lone leading `\x1b` arriving
  on its own is read back as a real, standalone Esc and exits the picker before
  the rest of the sequence even arrives (confirmed live). `TestVisibleWindow` (pure)
  and `TestRootPickerResponsive`'s "scrolling reaches the last command" subtest
  (pty-driven, 15 rows, 13 Downs) cover `scrollLines`'s scrolling directly and
  end to end — the latter down to actually running the scrolled-to "version"
  command, not just seeing its description.
- `TestSystemCredentials` (non-interactive: a direct `EMAIL` argument for export,
  always-non-interactive for import) and `TestSystemUI` (pty-driven: the root
  picker's "System credentials" submenu, and export's own account picker when
  no `EMAIL` is given) cover `cpro system export`/`cpro system import`
  (system.go). Both point `systemConfigPaths` at a hermetic temp directory via
  `CLAUDE_CONFIG_DIR` — the one env var that function deliberately honors — so
  neither test ever touches the real machine's actual `~/.claude`.
  `TestSystemCredentials` includes an explicit no-leaked-secrets check: a
  distinctive fake token value is planted in both a cpro account's and the
  system store's credentials, then asserted absent from stdout/stderr across
  every export/import invocation, success and failure paths alike.
  `TestSystemUI`'s submenu-and-back-navigation subtest can't use the other
  subtests' shared `drive()` helper (which fatals on any non-zero exit): it
  deliberately ends in a cancelling Esc sequence, which exits 1 by cpro's own
  convention (see `TestEscGuard`) — a successful run of that check, not a
  failure.
- `TestSessionContinue` covers `cpro session continue` (session.go):
  non-interactive throughout, it seeds two logged-in accounts via
  `installLogin` and a fake `.jsonl` transcript directly under one account's
  `projects/<projectDirName(dir)>`, runs the command with `cmd.Dir` set to
  that same directory, and asserts against the fake `claude` helper's own
  printed argv (`TestClaudeProcess`) that the follow-up run really does carry
  `--resume` — not just that the file landed on disk. It also covers both
  error paths (an unregistered email; a directory with no recorded session
  under `FROM_EMAIL`) and that re-running the command a second time copies
  nothing further (`Copied 0 session file(s)`) rather than duplicating or
  overwriting the transcript already in place.
- `TestYOLOSupport` and `TestYOLONoPromptsEndToEnd` (main_test.go) cover
  YOLO's "zero permission prompts" guarantee (permissions.go): the former
  calls `claudeSupportsFlag`/`requireYOLOSupport` directly, in-process,
  against the fake `claude --help` (`TestClaudeProcess`, extended with a
  `CPRO_TEST_NO_YOLO_SUPPORT=1` branch that omits
  `--dangerously-skip-permissions`/`--settings` from its own output, to
  simulate an older installed version) — `buildCLI`'s PATH setup is what
  makes `claudePath()` resolve the fake helper here too, so no subprocess
  spawn is needed just to check this. `claudeSupportsFlag` itself explicitly
  clears `CLAUDE_CONFIG_DIR` before running `claude --help` (it's a pure
  capability check, not scoped to any account) — this doubles as the exact
  signal `TestClaudeProcess` uses to tell that probe apart from a literal
  `--help` forwarded as a real Claude argument (e.g. `cpro run --account
  EMAIL -- --help`, which always goes through `claudeCommand` and so always
  has `CLAUDE_CONFIG_DIR` set): `dir == ""` means the capability probe, no
  separate test-only env var needed for it. The latter drives real `cpro run
  --account EMAIL` subprocesses end to end: the supported case asserts the
  fake claude actually received `yoloArgs()`'s exact argv (proving the
  version check didn't block a legitimate run); the unsupported case asserts
  a non-zero exit, a clear stderr message naming the missing flag, and —
  critically — that the fake claude's own `{"Args":...}` stdout echo is
  completely absent, which is what proves `cpro` never started Claude at all
  rather than silently falling back to a mode that would still prompt.
  `TestPermissionModeArgs`/`TestApplyPermissionDefaults`/
  `TestPermissionsRunIntegration` (pre-existing) were updated for `yoloArgs()`
  now returning three argv elements instead of one bare
  `--dangerously-skip-permissions`.
- `TestInteractiveRunFlow` and `TestInteractiveRunFlowEndToEnd` cover
  decision 0019's interactive run flow (rootui.go's `frameRunAccount`/
  `frameRunMode`). The former is pure, no pty — the same reasoning
  `TestPermissionsPreviewTracksCursor` and `TestRootPickerExitArmed` already
  give for driving `pushRunAccount`/`updateRunAccount`/
  `updateRunAccountSearch`/`updateRunMode` directly: bubbletea's diffing
  renderer makes a same-screen cursor move unreliable to prove via a
  cumulative pty capture. It covers account population/sorting, the
  no-registered-accounts error (`m.runFlowErr`, surfaced by `pickCommandArgs`
  after the program exits — the same pattern `configApp.err` uses), RUN
  MODE's preselection from the saved `PermissionMode` default, search
  filtering, Esc popping one step at a time, the built argv matching
  `permissionModeArgs` exactly, and that selecting YOLO calls
  `requireYOLOSupport()` before building the argv (restoring parity with the
  old, removed `pickRunMode`'s own check — `applyPermissionDefaults`'s
  version in `s.run` skips this for an *explicit* forwarded
  `--dangerously-skip-permissions`, which RUN MODE's own argv always is, so
  without this the interactive flow would silently skip the check
  entirely). `TestInteractiveRunFlowEndToEnd` drives the whole STEP1-4 flow
  through a real pty end to end, reaching the fake claude helper: only
  stdin/stderr go through the pty (the picker itself renders to stderr, see
  rootui.go's `pickCommandArgs`), stdout is captured in a separate buffer so
  the announce line and the fake claude's JSON output — both real stdout —
  aren't interleaved with the TUI's own stderr rendering, mirroring how
  `TestCLI`'s own non-interactive run checks already keep the two streams
  separate. `TestConfigUI` gained its own "Default account picker" subtest
  (search/select persists `config.Default`, Esc cancels) and every
  index-based navigation sequence in `TestConfigUI`/`TestPermissionsUI`/
  `TestNavigationHierarchy` was recomputed for `configMenuItems`' new order
  (account, permissions, then accent/bar/theme/warn/danger/mask) — in
  particular, `TestPermissionsUI`'s `openPermissions` helper could no longer
  rely on "up wraps from the first item to the last" to reach Permissions,
  since Permissions is no longer the last item (mask is).
- Decision 0021 moved Permissions out of `configMenuItems` entirely, so
  `TestPermissionsUI` was rebuilt to launch `cpro permissions` directly
  (dropping `openPermissions` and its down+enter navigation, and the extra
  trailing Esc every subtest used to need just to also leave Settings — one
  Esc now exits cleanly, since the screen has no parent). `TestConfigUI`'s
  index arithmetic was recomputed again for the new 7-item order (mask,
  account, accent, bar, theme, warn, danger) — coincidentally the same
  down-counts as before for accent/bar/theme/warn/danger, since removing
  "permissions" from position 1 and adding "mask" at position 0 cancel out;
  only the very-first-item and very-last-item cases (the boolean-toggle and
  wrap-around subtests, and the Default account picker's own first `enter`)
  actually needed new counts. `TestNavigationHierarchy` now covers Menu ->
  Permissions as a second, sibling instance of the cross-program relaunch
  seam (alongside its existing Menu -> Settings coverage), proving the
  mechanism generalizes rather than only ever being exercised for "config".
  `TestRootPickerEntries`/`TestForwardEntryArrows`/`TestRootPickerResponsive`
  gained the new "permissions" entry (order, group, shortLabel, forward-arrow
  status, and the shifted `nameWidth` — "permissions →" is now the longest
  visible label, so every hardcoded column-alignment string elsewhere in
  these tests needed recomputing too). New pure tests
  `TestApplyPermissionsMeta` (sets `permMode` only on the matching entry,
  silent no-op on a nil store) and `TestRootRowPermissionsColoredDot` (the
  colored-"●"-prefix rendering, the static-shortLabel fallback with no live
  `permMode`, and that truncation at a narrow width doesn't corrupt the color
  escape) — both direct calls, no pty, for the same reason
  `TestRootPickerGradient`'s own subtests avoid a live pty for a per-row
  color check: a same-cell, style-only diff (a row's rail color changing
  while its glyph doesn't) is exactly the kind of partial rewrite bubbletea's
  renderer may leave to a bare cursor-positioning write with no fresh color
  escape at all — confirmed live when the search-mode gradient subtest above
  was first extended to account for the new entry, and fixed by switching
  that subtest to a direct `viewSearch()` call instead of a live pty capture.
- Decision 0022 added `TestInteractiveRunFlow` subtests for → as a
  forward-select alias on RUN ACCOUNT/RUN MODE: → selecting an account
  (browsing and from a filtered search) and pushing RUN MODE, → selecting a
  mode (including YOLO, which still runs `requireYOLOSupport`) and
  finalizing `m.picked`, ← popping back one step at a time from either frame
  (mirroring the existing Esc coverage), and that neither an interactive
  account nor mode choice ever persists `config.Default`/
  `config.PermissionMode`. `TestInteractiveRunFlowEndToEnd` was reworked to
  finalize STEP 4 via → instead of Enter, so the end-to-end path (through
  the fake claude helper) covers the new key too, not just the pure
  in-process one.
- Decision 0023 split `configMenuItems` into `configMenuItemsSettings`/
  `configMenuItemsConfig`, so `TestConfigUI` now specifically covers CONFIG
  (direct `cpro config`, Permissions included as the new leading item) —
  every subtest's down-count was recomputed for the 8-item order (permissions
  leads; every other item's index shifts by exactly +1 from the old 7-item
  SETTINGS-only order). `TestNavigationHierarchy` gained one more assertion
  on its existing Menu -> Settings capture: the literal string "Permissions"
  (that capitalization only ever appears as this row's own label, never in
  the lowercase "permissions →" Menu row's live metadata) must be absent,
  confirming SETTINGS still excludes it. A new `TestConfigUI` subtest drives
  CONFIG's own nested Permissions round trip live: opening it (cursor 0),
  selecting a new mode, a single Esc popping back to CONFIG (checked via
  `cmd.Process.Signal(syscall.Signal(0))` staying alive — the same
  presence/liveness proof `TestNavigationHierarchy`'s own cross-program
  checks use, since a pty capture can't prove a frame *popped* rather than
  the program having exited) with the row's own value already reflecting the
  new mode in the very next render, and the mode persisting to config.json
  once the program is actually exited afterward. Every subtest across
  `TestConfigUI`/`TestPermissionsUI`/`TestCLI`/`TestThemeGlobalApplication`
  that drives a direct `cpro config`/`cpro permissions` invocation to actual
  completion had its final single Esc doubled (arm, then confirm) for the
  new double-Esc-to-exit convention — in `TestPermissionsUI` specifically,
  every `esc()` call in the whole file means "exit" (Permissions has no
  nested screens of its own to merely back out of), so the shared closure
  itself was changed to send two presses rather than touching each call site.
- Decision 0024 added `TestEmailMaskingGlobal`, a single real subprocess/pty
  test sharing one pair of logged-in accounts and one mask-on toggle across
  every subtest, so the two aliases (read once from config.json) can be
  asserted identical everywhere they appear — the actual proof that masking
  is centralized rather than independently regenerated per screen. Covers
  `cpro list`/`status` (plain and interactive, `--json` staying unmasked on
  purpose), RUN ACCOUNT (masked rows, and that searching the real local-part
  finds nothing while searching the alias does, then that the *real* account
  still actually runs despite being found by alias), Settings/CONFIG's
  Account row and the DEFAULT ACCOUNT picker, `cpro system export`'s own
  picker, and that the underlying persisted account identities/keys are
  untouched by masking. Its "toggling Mask emails updates the currently
  running screen immediately" subtest is deliberately direct-call/no-pty
  (building a `configApp` against an isolated, throwaway store and calling
  `updateMenu`/`viewMenu` directly) rather than live pty, for the same
  reason `TestPermissionsPreviewTracksCursor`/`TestRootPickerExitArmed`
  already document: a same-screen toggle-then-rerender is exactly the class
  of diff bubbletea's renderer may not retransmit in full, which a
  cumulative pty capture can't reliably prove either way — confirmed live
  when an earlier pty-based version of this subtest produced a capture with
  two frames' text interleaved. It also asserts *some* fresh alias appears
  after re-enabling, not the original one again — `saveMaskEmail` regenerates
  every alias on every toggle by design (a placeholder shouldn't survive past
  the toggle that showed it), so stability only holds while masking stays
  continuously on, which is what every other subtest's shared single toggle
  already proves. Uses its own isolated `store{dir: t.TempDir()}` rather than
  the outer test's shared one specifically so a mid-test toggle can't leave
  the shared config's `MaskEmail` in the wrong state for whichever subtest
  runs next. `TestDisplayEmail` is the small, pure counterpart covering the
  formatter directly: real email off, alias when registered, real email as a
  safe fallback when masking is on but no alias exists yet — saving and
  restoring the package-level `maskEmailEnabled`/`emailMaskTable` via
  `t.Cleanup` so this in-process test can't leak state into any other test
  sharing the same binary.
- Decision 0025 added `TestCommandTreeAudit` (pure): every real, available
  top-level command must appear in `rootPickerMeta` or a small, named
  exception map (`list`, decision 0010) — this is exactly the check that
  would have caught `session` going unreachable from the picker before this
  task. `TestSessionAppDirect` covers `sessionApp` (sessionui.go) directly, no
  pty, for the same reason `TestRootPickerExitArmed`/
  `TestPermissionsPreviewTracksCursor` already give: menu navigation
  (Enter/→, including that `list` — a leaf row — doesn't respond to → the way
  `config`'s own non-forward rows like "mask" don't either), the destination
  picker offering every account, the session's owner included (decision 0063), that the *full*
  (never shortened) session ID lands in the built argv, Esc/← popping a
  nested frame vs. arming/exiting at the stack's own root (both hasParent
  states), search filtering by project name/path/session ID/account, and
  mask-emails hiding the real address from both rendering and search.
  `TestSessionUI` (pty-driven) covers the same tree end to end: bare
  `cpro session` opening SESSIONS, → from MENU opening it (with a liveness
  check proving Esc backs out rather than exits, and a re-entry proving the
  restored frame is real, not stale — `TestRootPicker`'s own established
  proof pattern), `cpro session continue` with no arguments opening CONTINUE
  SESSION directly, and a full session-pick → account-pick run that asserts
  against the real `continueSession` output (`"Copied 1 session file(s)..."`,
  the full session ID, the transcript actually landing in the destination
  profile) — run from a directory that has *no* recorded sessions of its own,
  specifically to prove the copy is scoped by the picked session's own ID
  (`findSessionDir`) and not the process's own working directory, the exact
  latent bug this decision's own `continueSession` change fixes.
- Decision 0026 updated every existing YOLO-argv test
  (`TestPermissionModeArgs`/`TestApplyPermissionDefaults`/
  `TestPermissionsRunIntegration`/`TestYOLOSupport`/`TestYOLONoPromptsEndToEnd`/
  `TestInteractiveRunFlow`/`TestPermissionsUI`, plus the fake claude helper's
  own advertised `--help` flags) for `yoloArgs()`'s new
  `{"--dangerously-skip-permissions", "--add-dir", "/"}` shape. New
  `TestYOLOOutsideDirectoryReadIntegration` is the real, end-to-end proof this
  decision's own investigation demanded — not a string-contains check on the
  argv, but an actual installed `claude` asked to read a file in a directory
  outside its own working directory, asserting no `Read` entry appears in the
  real response's `permission_denials` and that the response text actually
  contains the probe file's contents. Deliberately gated behind
  `CPRO_TEST_REAL_CLAUDE_INTEGRATION=1` (unset by default) since it spends
  real API usage against a live, authenticated `claude` and needs network —
  not part of the default `go test ./...` run, confirmed passing when run
  explicitly.
- Decision 0027 verified (no production code change needed) that `cpro
  session continue`'s own RunE, ending in the same `s.run` call `cpro run`
  uses, already carries decision 0026's fix. `TestSessionContinuePermissionResolution`
  (fake-claude, no real API cost) proves the argv a resumed session actually
  receives matches `permissionModeArgs`/`yoloArgs` for both a configured YOLO
  default and a non-YOLO mode (Read-only, proving continue never forces
  YOLO). `TestSessionContinueYOLOOutsideDirectoryReadIntegration` mirrors
  decision 0026's own real-claude test but through the actual `cpro session
  continue FROM TO -- --resume ID ...` execution path: a synthetic
  `FROM_EMAIL` (continueSession never authenticates it, only reads its
  transcripts) with a genuinely seeded, resumable real session, continued
  into the real, currently-authenticated account (discovered via `claude
  auth status --json`, not hardcoded) under a configured YOLO default —
  confirmed passing. Same `CPRO_TEST_REAL_CLAUDE_INTEGRATION=1` gate, same
  reasoning for staying out of the default suite.
- Decision 0028 added `TestClaudeDesktopCredentialTarget`, which drives the
  real built `cpro system export` binary (not a direct function call, since
  the report is assembled by the RunE) with `CPRO_TEST_NO_CLAUDE_DESKTOP=1`
  to prove the "not installed" report and its own `XDG_CONFIG_HOME/Claude/
  config.json` fixture to prove the "installed → sign-in required" report —
  both asserting the exact per-target text, that Claude Desktop's row never
  renders the filled/Updated dot, that a planted fake Desktop config value
  never leaks into cpro's own output (the same no-secrets discipline
  `TestSystemCredentials` already applies to Claude Code's own credentials),
  and that Desktop's own config file is byte-for-byte untouched afterward —
  the direct proof that export only ever reads Desktop's presence, never
  writes to it. Existing `TestSystemCredentials`/`TestSystemUI` assertions on
  the old flat `"System credentials updated"` string were updated to the new
  `"Credentials exported"` header.
- Decision 0029 added `TestWatchModeFlow` (pure, no pty — the same reasoning
  `TestInteractiveRunFlow`/`TestRootPickerExitArmed` already give for a
  same-screen cursor move being unreliable to prove via a cumulative diffed
  pty capture) covering Enter/→ opening `frameWatchMode` from the launcher,
  both selections' exact argv (`["watch"]`/`["watch", "--compact"]`), the
  cursor's default/wrap, and Esc/← popping back to ROOT.
  `TestWatchModePickerEndToEnd` (pty-driven) covers the same tree through a
  real subprocess end to end, distinguishing Full from Compact by their own
  real, structurally different output (Full's own boxed per-account card,
  `"╭─ EMAIL ..."` — decision 0033 — vs. `renderCompactView`'s bare "╭─" that
  never carries an account's email in its border) since `watch`, unlike
  `run`, prints no announcement line to check instead; it also re-confirms
  direct `cpro watch`/`cpro watch --compact` stay picker-free. This decision
  also updated `TestRootPickerResponsive`'s pre-existing "short terminal: no
  corrupted/duplicated redraws" subtest to send a second Enter before
  checking for watch's fast-path output, since the first Enter now only
  opens WATCH MODE rather than running `watch` immediately.
- Decision 0030 added `TestAnnounceTarget` (pure, in-process, restoring the
  package-level mask globals via `t.Cleanup` exactly like `TestDisplayEmail`):
  the unchanged literal command with masking off, and with masking on the
  alias-plus-mode-label form, the real email's absence, the absence of any
  `cpro run`/`--account` fragment, `"ask"`'s empty argv still resolving to its
  own label, and the fallback for forwarded flags that are no mode's argv.
  `TestEmailMaskingIdentityIntegrity` is the same task's regression coverage
  for the rest of the masking audit — the side-effecting operations
  `TestEmailMaskingGlobal` (rendering/search only) doesn't reach: `cpro run`
  resolving to the real account's own profile directory while masking is on,
  an alias being *rejected* as an `--account` value (`missingAccount`, proving
  an alias can't stand in as an identifier), `cpro session continue` copying
  into the real destination profile, `cpro system export` writing the real
  account's own credentials byte-for-byte, and a mask toggle leaving
  everything in config.json but `MaskEmail`/`EmailMasks` deep-equal (the
  task's own explicit before/after comparison requirement), credentials
  included. Note it clears `MaskEmail`/`EmailMasks` on both sides before
  comparing, since `saveMaskEmail` regenerates every alias on every toggle by
  design — the alias table is the preference's own data, not account identity.
- Decision 0031 added `TestRunAccountUsage` (pure, direct calls, no pty — the
  same reasoning `TestRootPickerExitArmed`/`TestEmailMaskingGlobal` already
  document, since an asynchronous in-place row update is exactly the
  partial-rewrite case a cumulative diffed pty capture can't prove either
  way): rows rendering before any usage arrives, a delivered
  `runAccountUsageMsg` updating only its own row, the cursor never moving on
  arrival, a failed fetch keeping the `--` placeholder *and* that account
  still reaching RUN MODE, the `█` following reconfigured thresholds/colors
  (via the existing `ansiTrueColor` helper — lipgloss emits decimal RGB SGR
  sequences, never literal hex, so assert against that fragment), search
  retaining each row's usage while a fully-cached revisit returns a `nil`
  cmd (the "searching must not refetch"/"no second cache" requirements), and
  usage resolving by the real email while the alias is what renders.
  Deliberately network-free: every percentage comes from a `cpro-usage.json`
  written straight into the profile, the same approach `TestStatusViews`
  uses, since `loadUsage` treats a cache under a minute old as
  authoritative. New helper `panelRows` extracts a rendered panel's own
  `│ `-prefixed body rows (excluding edges, description, and footer) so an
  alignment assertion measures rows rather than chrome. Every existing
  `TestInteractiveRunFlow` call site became `if _, err := m.pushRunAccount()`
  for the new two-value signature.
- Decision 0032 added `TestExtractRootResumeInvocation`/`TestParseResumeArgs`
  (pure) covering the root-level `--resume`/`-r` rewrite and its own argument
  parser directly — only a literal leading `--resume`/`-r`/`--resume=ID` is
  ever recognized, matching `runArgs`' own "only a leading, recognized flag"
  discipline, so a real `cpro run --resume`/`cpro session continue ... --
  --resume ID` (forwarding `--resume` as a literal Claude argument) is
  asserted to pass through untouched. `TestFindSessionOwner`/
  `TestMigrateSession`/`TestSessionWorkingDirectory`/
  `TestRestoreSessionDirectory` (all pure) cover session.go's own discovery/
  migration/working-directory building blocks in isolation — `TestMigrateSession`
  specifically proves only the one requested session transcript is copied (not an
  unrelated sibling session recorded for the same account/directory), that
  re-running never overwrites an already-present destination file, and that a
  missing source transcript surfaces a clear error rather than silently
  succeeding. `TestResumeSession` (fake-claude subprocess harness, mirroring
  `TestSessionContinue`'s own setup) covers the CLI end to end: `--resume`/
  `-r` recognized, an unknown session ID's clear error, automatic
  same-account resume with no migration, that the configured default account
  is never changed by either automatic resolution or an explicit
  `--account` override, an explicit `--account` override safely migrating
  (byte-identical copy, original left in place), an unregistered `--account`
  rejected, extra Claude arguments forwarded after `--resume SESSION_ID`,
  the configured permission mode (including a non-YOLO mode, proving
  `--resume` never forces YOLO) applied identically to `cpro run`'s own, and
  the recorded working directory restored when run from an unrelated
  directory. Its "signed-out owning account" subtest seeds its own
  dedicated session/directory rather than reusing the shared `sessionID` —
  `findSessionOwner`'s own newest-first resolution means an earlier
  subtest's `--account` migration would otherwise make the shared session
  resolve to the already-authenticated target account instead of the owner
  actually being signed out for that check (confirmed live: reusing the
  shared session made this subtest flake exactly this way).
- Decision 0034 added `TestEnsureBlockReadsOutsideWorkingDirectories` (pure:
  creating settings.json from nothing, preserving unrelated existing keys,
  toggling both ways, the documented no-op when the persisted value already
  matches), `TestYOLOSettingsToggle` (fake-claude subprocess: a configured
  YOLO default disables the block, a later non-YOLO default restores it, an
  explicit forwarded YOLO combination disables it independent of the
  configured default, a plain non-YOLO run afterward restores it again — no
  lingering state from a prior invocation's explicit override), and
  `TestRequireNoManagedPermissionsPolicy` (no managed policy → nil error,
  `CPRO_TEST_MANAGED_POLICY=1`'s simulated `claude doctor` output → the exact
  "YOLO unavailable... cpro cannot guarantee zero permission prompts" error,
  a `claude doctor` failure degrading to "no known policy" rather than
  blocking an unrelated run) — the fake `claude` helper
  (`TestClaudeProcess`) gained a `doctor` branch for this, printing the
  "Managed settings (remote): ..." line real Claude Code prints, toggled by
  that same env var. Two new `TestSessionContinuePermissionResolution`
  subtests prove `cpro session continue` shares this exact settings.json
  toggle for its own destination account (`TO_EMAIL`), not just argv
  resolution, mirroring decision 0027's own "no second path" verification
  for the settings-file side of the fix.
  **`TestYOLOShellParserFallbackIntegration`** is this decision's own real,
  end-to-end proof, gated behind `CPRO_TEST_REAL_CLAUDE_INTEGRATION=1` (same
  convention as `TestYOLOOutsideDirectoryReadIntegration`/
  `TestSessionContinueYOLOOutsideDirectoryReadIntegration`): unlike those two,
  which invoke the real `claude` directly with hand-built YOLO argv, this one
  drives the actual **built `cpro run` binary** against the real,
  currently-authenticated account — the fix under test
  (`ensureBlockReadsOutsideWorkingDirectories`) lives in `store.run` itself,
  not in `yoloArgs()`, so exercising it means going through `cpro run`, not
  `claude` directly. One real call covers all 4 shell-parser-fallback
  classes together (a genuinely inherited env var expansion — not a
  same-command literal assignment, which was never affected — `python3 -c`,
  `find -exec`, and a `$(dirname ...)` computed path): asserts
  `permission_denials` is empty, the final answer contains every probe's
  marker, none of the reported prompt/denial phrasings appear anywhere in
  the raw output (not just exit code 0), and the account's own real
  `settings.json` actually ends up with `blockReadsOutsideWorkingDirectories:
  false` afterward — confirming the fix actually ran, not just that the
  probes happened to succeed some other way. Confirmed passing (~21s)
  alongside the pre-existing two real-integration tests above, all three run
  together as part of this decision's own verification. `cpro --resume`
  (added on the separate `agent/mgldvd/TASK-15` branch, not present in this
  worktree) could not be directly tested here; since the fix lives in the one
  shared `store.run` every launch path funnels through, it is expected to
  apply automatically once that branch merges, but this is an architectural
  inference, not something directly exercised — re-verify once merged.
- Decision 0035 added `TestEnsureYOLOSettings` (pure, mirroring
  `TestEnsureBlockReadsOutsideWorkingDirectories`'s own shape: creating
  settings.json from nothing, preserving unrelated keys, toggling both ways
  — enabling writes both `permissions.defaultMode` and
  `permissions.additionalDirectories`, disabling **removes** both rather than
  writing an "off" value — the no-op when already matching, the no-op when
  disabling a profile that was never enabled, and the one case that function's
  sibling has no equivalent of: a hand-configured `defaultMode`/
  `additionalDirectories` this code did not itself set is left untouched,
  since unlike `blockReadsOutsideWorkingDirectories` these two keys are ones
  a person may legitimately set for their own reasons) and `TestRequireNotRoot`
  (pure: asserts nil against this test process's own real, current, non-root
  UID — the same "check real state, don't add a parallel test-only path"
  convention `terminalInput` already follows; root itself is deliberately not
  simulated, since there is no safe way to become root inside this suite and
  the function is a single `os.Geteuid()` comparison — it `t.Skip`s rather
  than fails if the test process somehow is root). Every existing
  `TestYOLOSettingsToggle` subtest gained a `yoloSettingsEnabled` assertion
  beside its existing `blocked` one, proving both settings-file effects follow
  the identical on/off rhythm across a configured YOLO default, a later
  non-YOLO default, an explicit forwarded YOLO combination, and a plain Ask
  run afterward — they are driven by the same `yoloEffective` value in
  `store.run`, so a future change that desynchronized them would fail here.
- Decision 0036 added `TestDefaultAccountUsage` (pure, direct calls, no pty —
  the same reasoning `TestRunAccountUsage`/`TestRootPickerExitArmed` already
  document, since an asynchronous in-place row update is exactly the
  partial-rewrite case a cumulative diffed pty capture can't prove either
  way), mirroring `TestRunAccountUsage`'s own subtests one for one wherever
  the behavior applies to DEFAULT ACCOUNT: rows rendering before any usage
  arrives with a `--` placeholder (and no misleading `0%`), a delivered
  `runAccountUsageMsg` updating only its own row, the cursor never moving on
  arrival, a failed fetch keeping the placeholder *and* that account still
  being **savable as the default** (asserted against the persisted config,
  not just in-memory state — this screen's equivalent of RUN ACCOUNT's "still
  reaches RUN MODE"), search retaining usage while a fully-cached revisit
  returns a `nil` cmd, usage resolving by the real email while the alias
  renders *and* the real identity being what gets persisted, percentages
  right-aligning across 0/9/53/100%, the `█` following reconfigured
  thresholds/colors, and rows staying equal-width and never wrapping at width
  120 and 22. One further subtest asserts the cross-screen invariant
  directly: DEFAULT ACCOUNT and RUN ACCOUNT are each driven through their own
  real fetch batch against the same seeded cache, and their rendered Session
  cells for the same account must be byte-identical — the concrete proof one
  implementation backs both rather than two that merely look alike today.
  `TestConfigUI`'s existing pty-driven "Default account picker" subtest was
  extended to seed a usage cache and assert a real `53%` reaches the screen,
  covering the one thing the pure test cannot: that the `tea.Cmd`
  `enterDefaultAccount` returns is actually executed by the real bubbletea
  runtime through the real `activateMenuItem`/`updateMenu` plumbing in a real
  subprocess. Two new shared helpers keep the two tests from duplicating
  setup: `seedUsageCache` (extracted from `TestRunAccountUsage`'s own inline
  closure — writes a fresh `cpro-usage.json`, which `loadUsage` treats as
  authoritative under a minute old, keeping both tests deterministic and
  network-free) and `deliverUsageBatch` (unwraps a `tea.BatchMsg` into
  `Update` calls).
- Decision 0037 added `TestAnnounceCommand` (pure: a bare `*cobra.Command`
  with a captured `*bytes.Buffer`, the same non-interactive-printed-panel
  pattern `TestListRenderPrimitives`/`TestStatusCardLayout` already use, no
  subprocess needed): one subtest drives every entry in `borderThemes` and
  asserts the exact three-line `Top()+" Running:"` / `Rail()+"  "+<command>` /
  `Bottom()` output for each, proving Theme actually reaches this
  announcement now; a second confirms non-terminal output (a plain
  `*bytes.Buffer`) stays plain text with no ANSI escapes, the same degrade
  every other printed panel already gets from `accent()`/`terminalOutput`.
  Pre-existing `TestInteractiveRunFlowEndToEnd`/`TestAnnounceTarget` both
  re-run unchanged, confirming the format change didn't alter what's actually
  announced (the command text, masking's own behavior), only how it's framed.
- Decision 0038 updated every argv-asserting test for `yoloArgs()`'s new
  `{"--permission-mode", "bypassPermissions"}` shape
  (`TestPermissionModeArgs`/`TestApplyPermissionDefaults`/
  `TestLegacyBlockedCommandsIgnored`/`TestPermissionsRunIntegration`/
  `TestYOLONoPromptsEndToEnd`/`TestYOLOSettingsToggle`/
  `TestSessionContinuePermissionResolution`/`TestResumeSession`/
  `TestInteractiveRunFlowEndToEnd`/`TestAnnounceCommand`), and rewrote
  `TestYOLOSupport` plus the fake claude helper's own advertised `--help`
  output for the new pre-flight: the helper now prints `--permission-mode`
  with its accepted values inline, exactly as the real 2.1.268 does, and
  `CPRO_TEST_NO_YOLO_SUPPORT` simulates the realistic version gap (the flag
  present, `bypassPermissions` missing from its own choices list) rather
  than an entirely absent flag. New `TestPermissionModeDescriptions` (pure)
  asserts every mode carries a non-empty, single-line `Description` — a
  blank one renders an empty gap on the Permissions screen, a multi-line one
  breaks the one-line-per-mode layout that screen and RUN MODE both rely on
  — plus `permissionModeByKey("")` falling back to "ask"'s own, which
  `viewPermissions` leans on so an unset `permPreview` never renders blank.
  `TestFormatCommandPreview` was deleted with the function it covered, and
  `TestPermissionsPreviewTracksCursor`/`TestPermissionsUI` kept their full
  coverage retargeted from preview lines to the description; their two
  "toggling Trust changes nothing" assertions now extract the panel's own
  last `│ `-prefixed content line instead of searching for the removed
  "Command preview" heading, which is what makes them robust to this kind of
  rendering change in the first place.
- Decision 0039 added `TestAccountArgCommands` (pure, no pty — same reasoning
  as `TestInteractiveRunFlow`): `logout`/`remove` list every registered
  account and finalize `{command, email}` on Enter, `→` does the same, search
  filters and still finalizes the real account, those lists carry the same
  live Session usage RUN ACCOUNT's does (placeholder first, percentage after a
  delivered `runAccountUsageMsg`), the run flow's own untagged frame still
  continues to RUN MODE without finalizing, `login` validates through
  `normalizeEmail` and keeps the typed value on rejection so a typo is
  correctable in place, Esc pops each frame without picking, and
  `logout`/`remove` fail clearly with no accounts registered while `login`
  still opens (registering the first account being what it is for). Three
  existing tests were repointed rather than deleted, each keeping its original
  job: `TestForwardEntryArrows`' `forward` set gained the three;
  `TestInteractivePicker`'s "group boundaries" subtest proves the cursor
  landed on `login` via the new `LOGIN` frame's title instead of the old huh
  prompt's `you@example.com` placeholder; and `TestEscGuard`'s second subtest
  is described above.
- **`buildCLI` isolates `CLAUDE_CONFIG_DIR`** (decision 0040), and
  `TestBuildCLIIsolatesCredentialStores` guards that it stays isolated. This
  is not a nicety: cpro is developed *under* cpro, so `go test` inherits that
  variable already pointing at a real, logged-in account profile;
  `systemConfigPaths` honors it, and `cpro system export` **writes
  credentials** to whatever it resolves — so every export-exercising test
  silently overwrote the developer's live session and signed them out on each
  full run. Note it is *pointed at a temp directory*, never merely cleared:
  unset, `systemConfigPaths` falls back to the machine's real
  `~/.claude.json` + `~/.claude/.credentials.json`, an equally destructive
  target. Any new test that writes credentials must keep that isolation (the
  three tests needing a specific system directory override it after
  `buildCLI`).
- Decision 0040 renamed `TestApplyPermissionsMeta` to `TestApplyLiveMeta` and
  extended it for the second live-metadata row (`account`'s `defaultAcct`):
  only that row carries it, the two live fields never bleed into each other
  or a static row, masking yields the alias rather than the real address, an
  unset default leaves it empty, and a nil store is a silent no-op for both.
  `TestRootRowPermissionsColoredDot` gained the account row's own rendering,
  and `TestScreenTitles` now asserts both `hasParent` values title themselves
  CONFIG *and* that "SETTINGS" appears nowhere, so the retired title cannot
  quietly return. Fourteen existing expectations were updated for the new row
  rather than loosened — entry order/group/`shortLabel` tables, the `forward`
  arrow set, and five hard-coded navigation down-counts. `TestRootPickerScore`
  and `TestRootPickerRefilter` deserve special mention: `account` genuinely
  matches the query `"co"` as a name substring, a tier that is deliberately
  *not* length-gated, and ranks below the prefix matches `config`/`completion`
  — so those tests now assert that **ordering** (in `TestRootPickerRefilter`,
  the one that actually sorts) rather than a bare count.
