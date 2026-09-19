package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/fang"
	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"
)

const version = "0.1.0"

func main() {
	if s, err := openStore(); err == nil {
		if c, err := s.read(); err == nil {
			applyPreferences(c)
		}
	}
	root := rootCommand()
	// Rewritten once, here, before cobra ever parses anything — see
	// extractRootResumeInvocation's own doc comment for why this happens in
	// main() rather than as a cobra flag.
	if rewritten, ok := extractRootResumeInvocation(os.Args[1:]); ok {
		root.SetArgs(rewritten)
	}
	if err := fang.Execute(context.Background(), root, fang.WithVersion(version), fang.WithoutManpage(), fang.WithErrorHandler(cliError)); err != nil {
		var child *exec.ExitError
		if errors.As(err, &child) && child.ExitCode() > 0 {
			os.Exit(child.ExitCode())
		}
		os.Exit(1)
	}
}

// extractRootResumeInvocation recognizes cpro's own root-level
// --resume/-r/--resume=ID exactly the way runArgs recognizes a leading
// --account: only ever as args[0], so nothing after it is ever mistaken for
// cpro's own flag rather than a value Claude itself would receive. Cobra's
// own flag parsing cannot be taught to forward an *unknown* flag's value
// through to RunE (pflag's ParseErrorsWhitelist.UnknownFlags silently drops
// both the flag and the very next token — confirmed against the vendored
// pflag source, not assumed), so this rewrite happens once, here, in main(),
// entirely before cobra's root command parses anything at all: it turns
// `cpro --resume ID ...`/`cpro -r ID ...` into the hidden "__resume"
// command's own argv (`["__resume", ID, ...]`, parsed the rest of the way by
// parseResumeArgs below), and leaves every other invocation — including
// `cpro --help`/`--version` and every real subcommand — completely
// untouched, since only this one leading-flag shape is ever rewritten.
// false means args should reach cobra exactly as typed.
func extractRootResumeInvocation(args []string) ([]string, bool) {
	if len(args) == 0 {
		return nil, false
	}
	switch {
	case args[0] == "--resume" || args[0] == "-r":
		return append([]string{"__resume"}, args[1:]...), true
	case strings.HasPrefix(args[0], "--resume="):
		return append([]string{"__resume", strings.TrimPrefix(args[0], "--resume=")}, args[1:]...), true
	default:
		return nil, false
	}
}

// parseResumeArgs parses the hidden "__resume" command's own argv (session
// ID first, forwarded by extractRootResumeInvocation above), following
// runArgs' same "only ever consume a leading/recognized cpro flag" style so
// --account can appear before the arguments actually meant for Claude, and
// -- explicitly ends cpro's own parsing the same way it does for `cpro run`/
// `cpro session continue`.
func parseResumeArgs(args []string) (sessionID, account string, forwarded []string, help bool, err error) {
	if len(args) == 0 {
		return "", "", nil, false, nil
	}
	if args[0] == "--help" || args[0] == "-h" {
		return "", "", nil, true, nil
	}
	sessionID = args[0]
	args = args[1:]
	for len(args) > 0 {
		switch {
		case args[0] == "--":
			return sessionID, account, args[1:], false, nil
		case args[0] == "--help" || args[0] == "-h":
			return sessionID, account, nil, true, nil
		case args[0] == "--account" || strings.HasPrefix(args[0], "--account="):
			if account != "" {
				return "", "", nil, false, fmt.Errorf("--account may only be specified once")
			}
			value := strings.TrimPrefix(args[0], "--account=")
			if args[0] == "--account" {
				if len(args) < 2 {
					return "", "", nil, false, fmt.Errorf("missing email after --account")
				}
				args = args[1:]
				value = args[0]
			}
			account, err = normalizeEmail(value)
			if err != nil {
				return "", "", nil, false, err
			}
			args = args[1:]
		default:
			return sessionID, account, args, false, nil
		}
	}
	return sessionID, account, nil, false, nil
}

func rootCommand() *cobra.Command {
	root := &cobra.Command{
		Use: "cpro", Short: "Manage Claude Code accounts by email (Linux)",
		Long: "Manage Claude Code accounts by email.\nStart with: cpro login you@example.com\n" +
			"Resume any recorded session from any account: cpro --resume SESSION_ID (or -r SESSION_ID).\n" +
			"Data: $XDG_CONFIG_HOME/cpro or ~/.config/cpro. See README.md.",
		Version: version, SilenceUsage: true, SilenceErrors: true,
		Example: "cpro login you@example.com\ncpro config\ncpro run --dangerously-skip-permissions\ncpro --resume SESSION_ID",
	}
	root.SetOut(uiOutput(os.Stdout))
	root.SetErr(uiOutput(os.Stderr))
	root.RunE = func(cmd *cobra.Command, args []string) error {
		if !terminalInput() || !terminalOutput(cmd.ErrOrStderr()) {
			return cmd.Help()
		}
		picked, err := pickCommandArgs(cmd, rootLauncherNames, "")
		if err != nil {
			return err
		}
		cmd.SetArgs(picked)
		return cmd.Execute()
	}
	root.AddCommand(&cobra.Command{
		Use: "menu", Short: "Open the full interactive command palette", Args: cobra.NoArgs,
		Long: "Open the complete command picker (everything the reduced, bare-cpro launcher leaves out), the same way bare cpro itself did before the launcher was trimmed down to the handful of everyday actions.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if !terminalInput() || !terminalOutput(cmd.ErrOrStderr()) {
				return cmd.Help()
			}
			picked, err := pickCommandArgs(root, rootMenuNames(), "MENU")
			if err != nil {
				return err
			}
			root.SetArgs(picked)
			return root.Execute()
		},
	})
	root.AddCommand(newDoctorCommand(), newInstallCommand(), newInfoCommand())
	root.AddCommand(&cobra.Command{Use: "version", Short: "Print the cpro version", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		cmd.Println(cmd.Root().Name() + " version " + cmd.Root().Version)
		return nil
	}})
	accountCommand := func(name, short string, fn func(*store, string) error) *cobra.Command {
		return &cobra.Command{Use: name + " EMAIL", Short: short, Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			email, err := normalizeEmail(args[0])
			if err != nil {
				return err
			}
			s, err := openStore()
			if err != nil {
				return err
			}
			return fn(s, email)
		}}
	}
	root.AddCommand(accountCommand("login", "Sign in to a Claude account", func(s *store, email string) error { return s.login(email) }))
	root.AddCommand(accountCommand("logout", "Sign out without removing the profile", func(s *store, email string) error {
		lock, err := s.accountLock(email, true)
		if err != nil {
			return err
		}
		defer lock.Close()
		if _, err = s.resolve(email); err != nil {
			return err
		}
		cmd, err := claudeCommand(s.profile(email), "auth", "logout")
		if err != nil {
			return err
		}
		return interactive(cmd)
	}))
	var yes bool
	remove := accountCommand("remove", "Remove a local profile and its history", func(s *store, email string) error {
		lock, err := s.accountLock(email, true)
		if err != nil {
			return err
		}
		defer lock.Close()
		if _, err = s.resolve(email); err != nil {
			return err
		}
		if !yes {
			if !terminalInput() {
				return fmt.Errorf("remove requires confirmation; use --yes to remove the local profile")
			}
			fmt.Fprintf(os.Stderr, "Remove credentials, settings, and history for %s. Type the email to confirm: ", email)
			var answer string
			if _, err := fmt.Fscanln(os.Stdin, &answer); err != nil || answer != email {
				return fmt.Errorf("removal cancelled")
			}
		}
		return s.remove(email)
	})
	remove.Flags().BoolVarP(&yes, "yes", "y", false, "Confirm removal of the local profile")
	root.AddCommand(remove)
	var listJSON bool
	list := &cobra.Command{Use: "list", Aliases: []string{"ls"}, Short: "List accounts and authentication status", Args: cobra.NoArgs,
		Long: "List every registered account with its identity and authentication state only — no usage, sessions, or PIDs. See cpro status for the detailed usage/session dashboard.",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openStore()
			if err != nil {
				return err
			}
			c, emails, err := loadAccountEmails(s)
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()

			if listJSON {
				type jsonAccount struct {
					Email         string `json:"email"`
					Default       bool   `json:"default"`
					Authenticated bool   `json:"authenticated"`
				}
				states := fetchAuthStates(s, emails)
				accounts := make([]jsonAccount, 0, len(emails))
				for _, email := range emails {
					accounts = append(accounts, jsonAccount{email, email == c.Default, states[email]})
				}
				return json.NewEncoder(w).Encode(struct {
					Version  int           `json:"version"`
					Default  string        `json:"default"`
					Accounts []jsonAccount `json:"accounts"`
				}{1, c.Default, accounts})
			}
			if len(emails) == 0 {
				cmd.Println("No accounts found. Run: cpro login you@example.com")
				return nil
			}
			states := fetchAuthStates(s, emails)
			if err := ensureEmailMasks(s, c, emails); err != nil {
				return err
			}
			if terminalOutput(w) {
				renderAccountList(cmd, w, emails, states)
				return nil
			}
			for _, email := range emails {
				label := "Signed out"
				if states[email] {
					label = "Authenticated"
				}
				cmd.Printf("%s  %s\n", displayEmail(email), label)
			}
			return nil
		}}
	list.Flags().BoolVar(&listJSON, "json", false, "JSON output")
	root.AddCommand(list)
	var statusJSON, statusCompact bool
	status := &cobra.Command{Use: "status", Short: "Show account usage, sessions, and status", Args: cobra.NoArgs,
		Long: "Show the detailed per-account dashboard: authentication state, Session/Week usage bars, reset times, active sessions, and a total-week summary. See cpro list for a lightweight account listing with no usage/session data.",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openStore()
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			if !statusJSON && terminalOutput(w) {
				return renderAccountSnapshot(cmd, s, statusCompact)
			}
			c, emails, err := loadAccountEmails(s)
			if err != nil {
				return err
			}

			type accountState struct {
				authenticated bool
				sessions      []runningSession
			}
			states := map[string]accountState{}
			if len(emails) > 0 {
				results := make(chan struct {
					email string
					state accountState
				}, len(emails))
				for _, email := range emails {
					email := email
					go func() {
						var state accountState
						auth, authErr := authStatus(s.profile(email))
						state.authenticated = authErr == nil && validAuth(email, auth)
						state.sessions = runningSessions(s.profile(email))
						results <- struct {
							email string
							state accountState
						}{email, state}
					}()
				}
				for range emails {
					r := <-results
					states[r.email] = r.state
				}
			}

			if statusJSON {
				type jsonSession struct {
					PID       int    `json:"pid"`
					Directory string `json:"directory"`
					Since     string `json:"since,omitempty"`
				}
				type jsonAccount struct {
					Email         string        `json:"email"`
					Default       bool          `json:"default"`
					Authenticated bool          `json:"authenticated"`
					Sessions      []jsonSession `json:"sessions"`
				}
				accounts := make([]jsonAccount, 0, len(emails))
				for _, email := range emails {
					state := states[email]
					sessions := make([]jsonSession, 0, len(state.sessions))
					for _, sess := range state.sessions {
						since := ""
						if !sess.Since.IsZero() {
							since = sess.Since.UTC().Format(time.RFC3339)
						}
						sessions = append(sessions, jsonSession{sess.PID, sess.Directory, since})
					}
					accounts = append(accounts, jsonAccount{email, email == c.Default, state.authenticated, sessions})
				}
				return json.NewEncoder(w).Encode(struct {
					Version  int           `json:"version"`
					Default  string        `json:"default"`
					Accounts []jsonAccount `json:"accounts"`
				}{1, c.Default, accounts})
			}
			if len(emails) == 0 {
				cmd.Println("No accounts found. Run: cpro login you@example.com")
				return nil
			}
			if err := ensureEmailMasks(s, c, emails); err != nil {
				return err
			}
			for _, email := range emails {
				state := states[email]
				authState := "Authenticated"
				if !state.authenticated {
					authState = "Expired"
				}
				marker := " "
				if email == c.Default {
					marker = "*"
				}
				running := ""
				if n := len(state.sessions); n == 1 {
					running = "  1 session running"
				} else if n > 1 {
					running = fmt.Sprintf("  %d sessions running", n)
				}
				cmd.Printf("%s %s  %s%s\n", marker, displayEmail(email), authState, running)
			}
			return nil
		}}
	status.Flags().BoolVar(&statusJSON, "json", false, "JSON output")
	status.Flags().BoolVar(&statusCompact, "compact", false, "Compact view: one row per account with side-by-side Session/Week bars, no status/session detail")
	root.AddCommand(status)
	var watchInterval time.Duration
	var watchCompact bool
	watch := &cobra.Command{Use: "watch", Short: "Refresh cpro status on an interval until Esc Esc or Ctrl+C", Args: cobra.NoArgs,
		Long: "Repeats cpro status's interactive view on an interval, clearing the screen between refreshes in a terminal (or, redirected to a file, just appending timestamped snapshots for a log). The default interval, 60s, matches the on-disk usage cache, so every refresh shows freshly fetched data without extra requests to Anthropic's usage endpoint — a shorter --interval only redraws the same cached numbers more often, it does not poll more often. In a terminal, press Esc twice (like the rest of cpro's pickers) or Ctrl+C to stop.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if watchInterval < 5*time.Second {
				return fmt.Errorf("--interval must be at least 5s")
			}
			s, err := openStore()
			if err != nil {
				return err
			}
			return watchLoop(cmd, s, watchInterval, watchCompact)
		},
	}
	watch.Flags().DurationVar(&watchInterval, "interval", 60*time.Second, "Refresh interval (minimum 5s); shorter than the 60s usage cache just redraws the same numbers more often")
	watch.Flags().BoolVar(&watchCompact, "compact", false, "Compact view: one row per account with side-by-side Session/Week bars, no status/session detail")
	root.AddCommand(watch)
	run := &cobra.Command{Use: "run [--account EMAIL] [--] [CLAUDE ARGUMENTS...]", Short: "Run Claude Code and forward its arguments", DisableFlagParsing: true,
		Long: "Run Claude Code while preserving arguments, terminal, and exit status.\nAlways non-interactive — never prompts, even in a terminal.\nAccount: --account EMAIL, or the configured default account (cpro config).\nPermission mode: forwarded Claude flags, or the configured Permissions default.\nPlace --account before Claude arguments.\nExample: cpro run --account you@example.com --dangerously-skip-permissions\nRun cpro run -- --help to view Claude help.\nFor an interactive account/mode picker, run bare `cpro` and select \"run\" instead.",
		RunE: func(cmd *cobra.Command, args []string) error {
			email, forwarded, help, err := runArgs(args)
			if err != nil {
				return err
			}
			if help {
				return cmd.Help()
			}
			s, err := openStore()
			if err != nil {
				return err
			}
			return s.run(email, forwarded)
		},
	}
	root.AddCommand(run)
	root.AddCommand(newConfigCommand())
	root.AddCommand(newDefaultCommand())
	root.AddCommand(newSystemCommand())
	root.AddCommand(newSessionCommand())
	root.AddCommand(newResumeCommand())
	return root
}

// newResumeCommand is the hidden dispatch target extractRootResumeInvocation
// (above) rewrites `cpro --resume ID`/`cpro -r ID` into — never invoked by
// its own literal name (Hidden excludes it from cobra's help output and
// TestCommandTreeAudit's IsAvailableCommand check, so it needs no picker
// entry or exception either), and DisableFlagParsing so parseResumeArgs sees
// the raw argv, exactly like `cpro run`/`cpro session continue` already do
// for their own hand-rolled parsers.
func newResumeCommand() *cobra.Command {
	return &cobra.Command{
		Use:                "__resume",
		Hidden:             true,
		DisableFlagParsing: true,
		Short:              "internal dispatch target for cpro --resume/-r; do not invoke directly",
		RunE: func(cmd *cobra.Command, args []string) error {
			sessionID, account, forwarded, help, err := parseResumeArgs(args)
			if err != nil {
				return err
			}
			if help {
				cmd.Println("Usage: cpro --resume SESSION_ID [--account EMAIL] [--] [CLAUDE ARGUMENTS...]")
				cmd.Println("       cpro -r SESSION_ID [--account EMAIL] [--] [CLAUDE ARGUMENTS...]")
				return nil
			}
			if sessionID == "" {
				return fmt.Errorf("missing session ID; usage: cpro --resume SESSION_ID")
			}
			s, err := openStore()
			if err != nil {
				return err
			}
			return resumeSession(cmd, s, sessionID, account, forwarded)
		},
	}
}

// accountSnapshotState is one account's fetched state for renderAccountSnapshot
// and its full/compact view renderers.
// loadAccountEmails reads store's config and returns every registered
// account's email, sorted — the one place both cpro list and cpro status get
// the accounts they're about, so neither can drift from the other on
// ordering or what counts as "registered" (see the account-data-reuse note
// in main.go's cpro list/status commands).
func loadAccountEmails(s *store) (config, []string, error) {
	c, err := s.read()
	if err != nil {
		return c, nil, err
	}
	emails := make([]string, 0, len(c.Accounts))
	for email := range c.Accounts {
		emails = append(emails, email)
	}
	sort.Strings(emails)
	return c, emails, nil
}

// fetchAuthStates fetches each email's authentication state concurrently —
// the lightweight half of what renderAccountSnapshot fetches for cpro
// status: no usage (loadUsage, the actual network/cache-bound call), no
// session scan (runningSessions). cpro list's interactive and
// non-interactive paths both call this, so there's exactly one place that
// decides what "authenticated" means for it.
func fetchAuthStates(s *store, emails []string) map[string]bool {
	states := make(map[string]bool, len(emails))
	if len(emails) == 0 {
		return states
	}
	results := make(chan struct {
		email string
		ok    bool
	}, len(emails))
	for _, email := range emails {
		email := email
		go func() {
			auth, err := authStatus(s.profile(email))
			results <- struct {
				email string
				ok    bool
			}{email, err == nil && validAuth(email, auth)}
		}()
	}
	for range emails {
		r := <-results
		states[r.email] = r.ok
	}
	return states
}

// ensureEmailMasks is a defense-in-depth repair pass: it makes sure every
// email in emails has a mask placeholder when c.MaskEmail is on, backfilling
// and persisting any missing ones, then refreshes the live
// maskEmailEnabled/emailMaskTable globals (ui.go) so the shared displayEmail
// formatter reflects them immediately. Accounts registered through
// installLogin/installCredentialsFile (claude.go) already get their own
// alias the moment they're added — ensureAccountMask, config.go — so this
// should rarely find anything actually missing; it exists mainly to repair a
// config saved by an older cpro version that predates that eager backfill,
// or a mask map edited by hand. Called before any command renders a list of
// accounts (cpro list/status/watch — see loadAccountEmails's own callers).
func ensureEmailMasks(s *store, c config, emails []string) error {
	if !c.MaskEmail {
		return nil
	}
	missing := false
	for _, email := range emails {
		if _, ok := c.EmailMasks[email]; !ok {
			missing = true
			break
		}
	}
	if missing {
		if err := s.update(func(cfg *config) error {
			if cfg.EmailMasks == nil {
				cfg.EmailMasks = map[string]string{}
			}
			for _, email := range emails {
				if _, ok := cfg.EmailMasks[email]; !ok {
					cfg.EmailMasks[email] = newEmailMask()
				}
			}
			return nil
		}); err != nil {
			return err
		}
		var err error
		if c, err = s.read(); err != nil {
			return err
		}
	}
	maskEmailEnabled = c.MaskEmail
	emailMaskTable = c.EmailMasks
	return nil
}

// renderAccountList prints cpro list's own lightweight panel: one line per
// account, its identity and authentication state only (see fetchAuthStates)
// — no usage, sessions, or PIDs, and no selection cursor, since list is
// purely informational and every account is shown on equal footing. Colors
// degrade to plain text automatically when w isn't a terminal, same as every
// other panel in this file. See renderAccountSnapshot for the detailed
// dashboard cpro status shows instead.
func renderAccountList(cmd *cobra.Command, w io.Writer, emails []string, authenticated map[string]bool) {
	nameWidth := 0
	for _, email := range emails {
		nameWidth = max(nameWidth, visibleWidth(displayEmail(email)))
	}
	cmd.Println(accent(w, currentTheme.Top()+" Accounts", accentMode))
	for _, email := range emails {
		dot, label, color := "○", "Signed out", ""
		if authenticated[email] {
			dot, label, color = "●", "Authenticated", "#34D399"
		}
		marker := dot + " " + label
		if color != "" {
			marker = accent(w, dot, color) + " " + accent(w, label, color)
		}
		cmd.Println(accent(w, currentTheme.Rail(), accentMode) + "  " + padEnd(displayEmail(email), nameWidth) + "    " + marker)
	}
	cmd.Println(accent(w, currentTheme.Bottom(), accentMode))
}

type accountSnapshotState struct {
	usage         accountUsage
	stale         bool
	usageErr      error
	authenticated bool
	sessions      []runningSession
}

// renderAccountSnapshot prints one interactive-style snapshot of every registered
// account — status, Session/Week usage bars, active sessions, and a Total Week
// summary in a "╭─"/"│"/"╰─" panel (renderFullView), or, compact, a denser
// one-row-per-account view (renderCompactView) — the shared rendering behind both
// "cpro status" in a terminal and each "cpro watch" refresh. Colors degrade to
// plain text automatically when cmd's output isn't a terminal (see accent/
// terminalOutput), which is what makes watch's redirected-to-a-file case a plain
// timestamped log instead of raw escape codes. See renderAccountList for cpro
// list's own lightweight, usage-free panel.
func renderAccountSnapshot(cmd *cobra.Command, s *store, compact bool) error {
	c, emails, err := loadAccountEmails(s)
	if err != nil {
		return err
	}
	if len(emails) == 0 {
		cmd.Println("No accounts found. Run: cpro login you@example.com")
		return nil
	}
	if err := ensureEmailMasks(s, c, emails); err != nil {
		return err
	}

	w := cmd.OutOrStdout()
	states := map[string]accountSnapshotState{}
	results := make(chan struct {
		email string
		state accountSnapshotState
	}, len(emails))
	for _, email := range emails {
		email := email
		go func() {
			var state accountSnapshotState
			state.usage, state.stale, state.usageErr = loadUsage(s.profile(email), usageEndpoint)
			auth, authErr := authStatus(s.profile(email))
			state.authenticated = authErr == nil && validAuth(email, auth)
			state.sessions = runningSessions(s.profile(email))
			results <- struct {
				email string
				state accountSnapshotState
			}{email, state}
		}()
	}
	for range emails {
		r := <-results
		states[r.email] = r.state
	}

	width := outputWidth(w)
	if compact {
		return renderCompactView(cmd, w, width, emails, states)
	}
	return renderFullView(cmd, w, width, emails, states)
}

// fullRowDefault/fullRowMin bound the total OUTER width of every account card
// renderFullView draws (both rails/corners included — see statusCardLayout):
// fullRowDefault on a terminal wide enough to afford it (or one whose width can't
// be determined, e.g. cpro watch redirected to a file), shrinking down to
// fullRowMin as the real terminal narrows. Below fullRowMin the card simply stops
// shrinking further (decision 0033 kept this same floor-below-the-real-width
// tradeoff the pre-boxed layout already made, rather than ever rendering an
// unusably tiny card) — a genuinely narrower terminal wraps rather than the
// layout degrading past readability.
const (
	fullRowDefault = 68
	fullRowMin     = 48
)

// fullRowWidth resolves termWidth (see outputWidth; 0 means unknown/unconstrained)
// to the one card width every account in renderFullView's output shares.
func fullRowWidth(termWidth int) int {
	if termWidth <= 0 {
		return fullRowDefault
	}
	if termWidth < fullRowDefault {
		return max(fullRowMin, termWidth)
	}
	return fullRowDefault
}

// statusUsageBarDefault/statusUsageBarMin, statusPathWidthMin, statusPctWidth,
// statusSessionMidGap, and statusDurationWidthMin are the fixed constants
// newStatusCardLayout builds a statusCardLayout from. statusPctWidth is 4, not 3,
// for the same reason compactPctWidth (above) is: "100%" is itself 4 characters,
// and a narrower field would throw off alignment the one time an account is
// actually at 100%. statusSessionMidGap is the fixed 2-space gap a session row
// keeps between its right-aligned pid field and the "·" runtime separator — fixed
// because pathWidth (the flexible column, see newStatusCardLayout) is itself
// computed backwards from this gap staying constant, not the other way around.
const (
	statusUsageBarDefault  = 20
	statusUsageBarMin      = 6
	statusPathWidthMin     = 8
	statusPctWidth         = 4
	statusSessionMidGap    = 2
	statusDurationWidthMin = 2 // formatDuration/formatCountdown's shortest possible output, "0m"
	// statusPathPidGap guarantees a session row's path and its right-aligned pid
	// field never visually merge — without it, a path truncated/padded to fill
	// its entire pathWidth column would butt directly against the pid's first
	// digit with no separating space at all (confirmed live).
	statusPathPidGap = 1
	// statusSepGap guarantees a row's "-"/"·" separator never visually merges
	// into its right-aligned duration field — without it, whichever account
	// happens to have the longest reset/runtime string this render would fill
	// its field with zero padding, butting the digits directly against the
	// separator with no space at all (confirmed live).
	statusSepGap = 1
)

// statusCardLayout is the one set of column widths every account card in cpro
// status's full view renders with — computed once per render (see
// newStatusCardLayout) from the real terminal width and every account's own data
// (the longest pid, the longest reset/runtime duration actually seen this
// render), never recomputed per account. This is what keeps every card's bar,
// percentage, reset, path, pid, and runtime columns landing in the same visible
// column regardless of which particular account happens to have the longest
// value — see the task's own "never calculated separately per account" rule.
type statusCardLayout struct {
	cardWidth          int // total outer width, both rails/corners included
	contentWidth       int // cardWidth minus the "│ "/" │" frame on both sides
	usageLabelWidth    int // "S"/"W" itself — always 1
	barWidth           int // the usage bar's glyph count (the second narrow-terminal lever, after pathWidth)
	pctWidth           int // the usage row's right-aligned percentage field (statusPctWidth)
	resetWidth         int // the usage row's right-aligned "-  <duration>" field
	sessionMarkerWidth int // the session row's leading "·" — always 1
	pathWidth          int // the session row's own flexible column (the first narrow-terminal lever)
	pidWidth           int // the session row's right-aligned pid field
	sepWidth           int // both separators ("-" and the session row's own "·") — always 1
	runtimeWidth       int // the session row's right-aligned "·  <duration>" field
}

// newStatusCardLayout computes one statusCardLayout for the whole render: cardWidth
// from the real terminal width (fullRowWidth), and resetWidth/pidWidth/runtimeWidth
// from the actual longest value seen across every account and session passed in —
// so, e.g., one account with a 7-digit pid widens the pid column for every other
// account's cards too, keeping every card's columns aligned rather than each
// choosing its own width. barWidth only ever shrinks below statusUsageBarDefault
// when contentWidth is so narrow the usage row wouldn't otherwise fit even with no
// gap at all — pathWidth is always the first, and usually the only, column that
// actually shrinks as the terminal narrows (see fullRowMin's own floor above).
func newStatusCardLayout(termWidth int, emails []string, states map[string]accountSnapshotState) statusCardLayout {
	cardWidth := fullRowWidth(termWidth)
	contentWidth := cardWidth - 4

	resetWidth := statusDurationWidthMin
	runtimeWidth := statusDurationWidthMin
	pidWidth := 1
	for _, email := range emails {
		state := states[email]
		if state.usageErr == nil {
			for _, win := range [...]usageWindow{state.usage.FiveHour, state.usage.SevenDay} {
				if reset, ok := usageReset(win.ResetsAt); ok {
					resetWidth = max(resetWidth, visibleWidth(formatCountdown(reset)))
				}
			}
		}
		for _, sess := range state.sessions {
			pidWidth = max(pidWidth, visibleWidth(fmt.Sprintf("%d", sess.PID)))
			if text, ok := sessionMeta(sess); ok {
				runtimeWidth = max(runtimeWidth, visibleWidth(text))
			}
		}
	}

	barWidth := statusUsageBarDefault
	usageFixed := 1 + 2 + 1 + statusPctWidth + 1 + statusSepGap + resetWidth // label + 2 spaces + space-after-bar + pct + sep + sepGap + reset
	if avail := contentWidth - usageFixed; avail < barWidth {
		barWidth = max(statusUsageBarMin, avail)
	}

	// marker + space + pathPidGap + midGap + sep + sepGap
	pathWidth := contentWidth - runtimeWidth - pidWidth - (1 + 1 + statusPathPidGap + statusSessionMidGap + 1 + statusSepGap)
	if pathWidth < statusPathWidthMin {
		pathWidth = statusPathWidthMin
	}

	return statusCardLayout{
		cardWidth: cardWidth, contentWidth: contentWidth,
		usageLabelWidth: 1, barWidth: barWidth, pctWidth: statusPctWidth, resetWidth: resetWidth,
		sessionMarkerWidth: 1, pathWidth: pathWidth, pidWidth: pidWidth, sepWidth: 1, runtimeWidth: runtimeWidth,
	}
}

// statusAuthLabel returns cpro status's fixed Authenticated/Expired label and
// color for an account — a deliberately fixed green/red pair, independent of the
// user-configurable barColorSafe/warningColor/dangerColor usage-threshold colors
// (see usageColorFor, ui.go), exactly as it was before this card layout existed.
func statusAuthLabel(authenticated bool) (label, color string) {
	if authenticated {
		return "Authenticated", "#34D399"
	}
	return "Expired", "#F87171"
}

// sessionCountText renders a header's own session count — "1 session", plural
// otherwise (including zero: "0 sessions") — the one place cpro status decides
// between singular and plural.
func sessionCountText(n int) string {
	if n == 1 {
		return "1 session"
	}
	return fmt.Sprintf("%d sessions", n)
}

// statusHeaderTrailingDashesTarget is how many "─" statusCardHeader tries to
// leave between the status text and the card's closing top-right corner — the
// rest of the header's own dash budget goes to the (usually much longer) leading
// run between the account label and the status text, which is what actually
// grows/shrinks as the label or status text's own length changes; this trailing
// run stays a small, mostly-constant flourish instead.
const statusHeaderTrailingDashesTarget = 5

// statusCardHeader renders one account card's entire top border — "╭─ EMAIL
// ──── ● Authenticated · N sessions ────╮" — carrying all of that account's
// identifying info directly in the border (decision 0033: no separate header line
// inside the card). The two dash runs are computed to make the whole line exactly
// cardWidth wide regardless of how long label/status happen to be for this
// particular account; only cardWidth itself is shared across every card in a
// render (see statusCardLayout) — the dash split is otherwise purely per-card
// cosmetic filler, not an alignment concern (nothing below the header aligns
// against it).
func statusCardHeader(w io.Writer, cardWidth int, label string, authenticated bool, sessionCount int) string {
	status, color := statusAuthLabel(authenticated)
	statusPlain := "● " + status + " · " + sessionCountText(sessionCount)
	statusStyled := accent(w, "●", color) + " " + accent(w, status, color) + " · " + sessionCountText(sessionCount)

	total := cardWidth - 7 - visibleWidth(label) - visibleWidth(statusPlain) // 7 fixed single-char pieces: corner+horizontal+4 spaces+corner
	if total < 2 {
		total = 2
	}
	trailing := statusHeaderTrailingDashesTarget
	if trailing > total-1 {
		trailing = total - 1
	}
	if trailing < 1 {
		trailing = 1
	}
	leading := total - trailing

	corner := func(s string) string { return accent(w, s, accentMode) }
	dashes := func(n int) string { return accent(w, strings.Repeat(currentTheme.Horizontal, n), accentMode) }

	return corner(currentTheme.TopLeft) + corner(currentTheme.Horizontal) + " " + label + " " +
		dashes(leading) + " " + statusStyled + " " + dashes(trailing) + corner(currentTheme.TopRight)
}

// statusCardBottom renders one account card's closing "╰────...────╯" line —
// cardWidth wide, matching statusCardHeader.
func statusCardBottom(w io.Writer, cardWidth int) string {
	return accent(w, currentTheme.BottomLeft+strings.Repeat(currentTheme.Horizontal, cardWidth-2)+currentTheme.BottomRight, accentMode)
}

// statusCardLine frames one body line inside "│ "/" │", padding content out to
// contentWidth so every card's right border lands in the same column regardless
// of how much visible text content actually carries (see padEnd; content may
// already carry ANSI styling, which padEnd measures around via visibleWidth).
func statusCardLine(w io.Writer, contentWidth int, content string) string {
	rail := accent(w, currentTheme.Rail(), accentMode)
	return rail + " " + padEnd(content, contentWidth) + " " + rail
}

// statusUsageLine renders one "S"/"W" row: the label, a proportional bar,
// a right-aligned percentage, a flexible gap, then "-", a fixed statusSepGap,
// and the right-aligned countdown to reset — or, when this window hasn't
// started yet (usageReset reports false), a blank filler in place of the
// separator and duration rather than a misleading dash. The flexible gap
// absorbs whatever contentWidth leaves over, which is what makes the reset
// column land in the same place across every account's cards regardless of
// bar width (see statusCardLayout.barWidth); statusSepGap is a small
// additional fixed gap so the reset text never butts directly against the
// "-" even when this render's longest reset string fills resetWidth exactly.
func statusUsageLine(w io.Writer, layout statusCardLayout, letter string, win usageWindow) string {
	value := max(0, win.Utilization)
	pct := padStart(accent(w, pctText(value), usageColorFor(value)), layout.pctWidth)
	left := letter + "  " + usageBarWidth(w, value, layout.barWidth) + " " + pct

	var right string
	if reset, ok := usageReset(win.ResetsAt); ok {
		right = "-" + strings.Repeat(" ", statusSepGap) + padStart(formatCountdown(reset), layout.resetWidth)
	} else {
		right = strings.Repeat(" ", layout.sepWidth+statusSepGap+layout.resetWidth)
	}

	gap := layout.contentWidth - visibleWidth(left) - visibleWidth(right)
	if gap < 1 {
		gap = 1
	}
	return left + strings.Repeat(" ", gap) + right
}

// statusSessionLine renders one "· path  pid  ·  runtime" row: the leading "·"
// marker, a path truncated/padded to layout.pathWidth (see truncatePath), a
// fixed statusPathPidGap, a right-aligned pid, a fixed statusSessionMidGap,
// then "·", a fixed statusSepGap, and the right-aligned runtime — or a blank
// filler when the session's start time couldn't be determined (see
// sessionMeta). Both small fixed gaps exist so a path that fills its entire
// pathWidth, or a runtime that fills its entire runtimeWidth, never visually
// merges into the field that follows it (confirmed live, before they were
// added). pathWidth was already computed (see newStatusCardLayout) so this
// always sums to exactly layout.contentWidth, the same way every other
// account's session rows do, regardless of this particular session's own
// path/pid length.
func statusSessionLine(w io.Writer, layout statusCardLayout, sess runningSession) string {
	path := padEnd(truncatePath(sess.Directory, layout.pathWidth), layout.pathWidth)
	pid := padStart(fmt.Sprintf("%d", sess.PID), layout.pidWidth)
	left := "· " + path + strings.Repeat(" ", statusPathPidGap) + pid

	var right string
	if text, ok := sessionMeta(sess); ok {
		right = "·" + strings.Repeat(" ", statusSepGap) + padStart(text, layout.runtimeWidth)
	} else {
		right = strings.Repeat(" ", layout.sepWidth+statusSepGap+layout.runtimeWidth)
	}
	return left + strings.Repeat(" ", statusSessionMidGap) + right
}

// renderStatusCard prints one account's entire card: its own boxed header (email/
// alias, authenticated state, session count — statusCardHeader), Session/Week
// usage rows, exactly one blank row then its active sessions (only when there are
// any — see the task's own "no blank rows... before the bottom border" rule), and
// its own closing border. Never wraps a row: statusSessionLine's path column is
// what absorbs a narrow terminal instead.
func renderStatusCard(cmd *cobra.Command, w io.Writer, layout statusCardLayout, email string, state accountSnapshotState) {
	cmd.Println(statusCardHeader(w, layout.cardWidth, displayEmail(email), state.authenticated, len(state.sessions)))
	line := func(content string) { cmd.Println(statusCardLine(w, layout.contentWidth, content)) }

	if state.usageErr != nil {
		line("Usage unavailable")
	} else {
		for _, block := range [...]struct {
			letter string
			win    usageWindow
		}{{"S", state.usage.FiveHour}, {"W", state.usage.SevenDay}} {
			line(statusUsageLine(w, layout, block.letter, block.win))
		}
	}

	if len(state.sessions) > 0 {
		line("")
		for _, sess := range state.sessions {
			line(statusSessionLine(w, layout, sess))
		}
	}
	cmd.Println(statusCardBottom(w, layout.cardWidth))
}

// renderFullView prints cpro status's full layout: one independent, fully boxed
// card per account (decision 0033 — no outer "Accounts" panel wrapping them, and
// no "├─" divider between them; exactly one blank terminal line separates two
// cards, matching the blank line already used between a card's own usage rows and
// its active sessions), then a plain, unboxed "Total week" line averaging Week
// usage across every account with valid data — deliberately outside every card,
// never itself boxed. There is no "current"/default account to mark: every
// account renders on equal footing, from the one statusCardLayout computed for
// this whole render (see newStatusCardLayout).
func renderFullView(cmd *cobra.Command, w io.Writer, termWidth int, emails []string, states map[string]accountSnapshotState) error {
	layout := newStatusCardLayout(termWidth, emails, states)
	var weekSum float64
	var weekCount int
	for i, email := range emails {
		if i > 0 {
			cmd.Println("")
		}
		state := states[email]
		if state.usageErr == nil {
			weekSum += max(0, state.usage.SevenDay.Utilization)
			weekCount++
		}
		renderStatusCard(cmd, w, layout, email, state)
	}
	if weekCount > 0 {
		avg := weekSum / float64(weekCount)
		cmd.Println("")
		cmd.Println("Total week  " + usageBar(w, avg) + " " + accent(w, pctText(avg), usageColorFor(avg)))
	}
	return nil
}

// sessionMeta formats a running session's elapsed runtime for its card row's
// right-aligned trailing field (see statusSessionLine) — "", false when the start
// time couldn't be determined (runningSessions), telling the caller to render a
// blank filler instead of a misleading duration.
func sessionMeta(s runningSession) (string, bool) {
	if s.Since.IsZero() {
		return "", false
	}
	return formatDuration(time.Since(s.Since)), true
}

// compactBarWidth/compactNameGap/compactBlockGap are the compact view's fixed
// layout constants: a 16-glyph proportional bar (vs. the full view's 20), 6 spaces
// between the name column and the Session field, 4 between the Session and Week
// fields.
const (
	compactBarWidth = 16
	compactNameGap  = 6
	compactBlockGap = 4
	// compactPctWidth is the percentage field's fixed width: 4, not 3, because
	// 100% (unlike every other whole percentage) is itself 4 characters — using
	// 3 here left the "W" column one short of the "S" column's width whenever an
	// account happened to be at exactly 100%, throwing off every row's alignment
	// (confirmed live: the Week column shifted by one space vs. sibling rows).
	compactPctWidth = 4
)

// compactFieldWidth is the visible width of one "S "/"W " field: the letter, a
// space, the bar (16 wide, or 1 in the narrow single-glyph fallback), a space,
// and the right-aligned percentage (see compactPctWidth).
func compactFieldWidth(narrow bool) int {
	bar := compactBarWidth
	if narrow {
		bar = 1
	}
	return 1 + 1 + bar + 1 + compactPctWidth
}

// compactRowWidth is one account row's total visible width (including its "│  "
// rail) at the given name column width — used both to lay out each row and to
// decide, in renderCompactView, whether the terminal is wide enough for it.
func compactRowWidth(nameWidth int, narrow bool) int {
	return 3 + 2 + nameWidth + compactNameGap + compactFieldWidth(narrow) + compactBlockGap + compactFieldWidth(narrow)
}

// compactField renders one "S"/"W" field: the letter, a space, the bar (or, when
// narrow, usageGlyph's single colored block), a space, and the percentage
// right-aligned in a fixed compactPctWidth-column field, colored to match the bar.
func compactField(w io.Writer, letter string, value float64, narrow bool) string {
	value = max(0, min(100, value))
	bar := usageBarWidth(w, value, compactBarWidth)
	if narrow {
		bar = usageGlyph(w, value)
	}
	pct := fmt.Sprintf("%*s", compactPctWidth, pctText(value))
	return letter + " " + bar + " " + accent(w, pct, usageColorFor(value))
}

// compactBarCol is the visible column (measured from the start of an account row's
// content, right after its "│  " rail) at which that row's Week bar/glyph itself
// begins — renderCompactView's Total week row aligns its own bar to this same
// column regardless of narrow, which is what keeps every bar in the panel on one
// vertical line.
func compactBarCol(nameWidth int, narrow bool) int {
	return 2 + nameWidth + compactNameGap + compactFieldWidth(narrow) + compactBlockGap + 2 // 2 = len("W ")
}

// renderCompactView prints cpro list's compact layout: a bare "╭─" panel, one row
// per account (name, then Session and Week side by side — no "current"/default
// marker, every account on equal footing), and a closing "╰─ Total week" row whose
// bar lines up under every account's Week bar. It switches to the narrow
// single-glyph fallback (see usageGlyph) on its own, whenever termWidth is known
// and too small to fit the proportional-bar row.
func renderCompactView(cmd *cobra.Command, w io.Writer, termWidth int, emails []string, states map[string]accountSnapshotState) error {
	nameWidth := 0
	names := make(map[string]string, len(emails))
	for _, email := range emails {
		name := displayEmail(email)
		names[email] = name
		nameWidth = max(nameWidth, visibleWidth(name))
	}
	narrow := termWidth > 0 && termWidth < compactRowWidth(nameWidth, false)

	cmd.Println(accent(w, currentTheme.Top(), accentMode))
	var weekSum float64
	var weekCount int
	for _, email := range emails {
		state := states[email]
		if state.usageErr == nil {
			weekSum += max(0, state.usage.SevenDay.Utilization)
			weekCount++
		}
		name := names[email]
		if !state.authenticated {
			name = accent(w, name, "#F87171")
		}
		row := "  " + padEnd(name, nameWidth) + strings.Repeat(" ", compactNameGap)
		row += compactField(w, "S", state.usage.FiveHour.Utilization, narrow)
		row += strings.Repeat(" ", compactBlockGap)
		row += compactField(w, "W", state.usage.SevenDay.Utilization, narrow)
		cmd.Println(accent(w, currentTheme.Rail(), accentMode) + "  " + row)
	}
	cmd.Println(accent(w, currentTheme.Rail(), accentMode))
	if weekCount == 0 {
		cmd.Println(accent(w, currentTheme.Bottom(), accentMode))
		return nil
	}
	avg := weekSum / float64(weekCount)
	barCol := compactBarCol(nameWidth, narrow)
	label := "Total week"
	lead := ""
	bar := usageBarWidth(w, avg, compactBarWidth)
	if narrow {
		lead = "W "
		bar = usageGlyph(w, avg)
	}
	pad := max(1, barCol-len(lead)-len(label))
	pct := fmt.Sprintf("%*s", compactPctWidth, pctText(avg))
	total := label + strings.Repeat(" ", pad) + lead + bar + " " + accent(w, pct, usageColorFor(avg))
	cmd.Println(accent(w, currentTheme.Bottom(), accentMode) + " " + total)
	return nil
}

// crlfWriter rewrites every bare "\n" to "\r\n" before writing — see watchLoop,
// which needs it once the terminal is in raw mode.
type crlfWriter struct{ w io.Writer }

func (c crlfWriter) Write(p []byte) (int, error) {
	if _, err := c.w.Write(bytes.ReplaceAll(p, []byte("\n"), []byte("\r\n"))); err != nil {
		return 0, err
	}
	return len(p), nil
}

// watchLoop drives "cpro watch": redraws renderAccountSnapshot every interval until
// stopped. In a terminal, it puts stdin into raw mode to read keys directly — the
// same double-Esc-to-exit convention as every huh-based picker (see escGuardField):
// a first Esc shows a warning that clears on its own after escWarnWindow or on any
// other key, a second Esc within that window exits (as does Ctrl+C, immediately,
// same as it would with the terminal in its normal/cooked mode). Piped or otherwise
// non-interactive output (e.g. redirected to a log file) falls back to redrawing on
// a plain ticker with no way to stop it from the keyboard, same as before this was
// added — the caller (an actual terminal, or Ctrl+C via the OS's normal SIGINT
// handling for a backgrounded/redirected process) is what ends it then.
func watchLoop(cmd *cobra.Command, s *store, interval time.Duration, compact bool) error {
	clearScreen := terminalOutput(cmd.OutOrStdout())

	var keys chan byte
	if clearScreen && terminalInput() {
		if state, err := term.MakeRaw(os.Stdin.Fd()); err == nil {
			defer term.Restore(os.Stdin.Fd(), state)
			// Stdin and stdout share one tty device, so making stdin raw also drops
			// the terminal's own "\n" -> "\r\n" translation (POSIX OPOST/ONLCR) on
			// output — confirmed live: every line lands one column further right
			// than the last, a staircase, without this rewrite in front of it.
			cmd.SetOut(crlfWriter{cmd.OutOrStdout()})
			keys = make(chan byte)
			go func() {
				defer close(keys)
				buf := make([]byte, 1)
				for {
					n, err := os.Stdin.Read(buf)
					if n > 0 {
						keys <- buf[0]
					}
					if err != nil {
						return
					}
				}
			}()
		}
	}

	w := cmd.OutOrStdout()
	redraw := func() error {
		if clearScreen {
			fmt.Fprint(w, "\x1b[H\x1b[2J")
		}
		return renderAccountSnapshot(cmd, s, compact)
	}
	if err := redraw(); err != nil {
		return err
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	if keys == nil {
		for range ticker.C {
			if err := redraw(); err != nil {
				return err
			}
		}
		return nil
	}

	armed := false
	var warnTimer *time.Timer
	var warnC <-chan time.Time
	disarm := func() {
		armed = false
		warnC = nil
		if warnTimer != nil {
			warnTimer.Stop()
		}
	}
	for {
		select {
		case <-ticker.C:
			disarm()
			if err := redraw(); err != nil {
				return err
			}
		case b, ok := <-keys:
			if !ok {
				return nil
			}
			switch b {
			case 0x03: // Ctrl+C — raw mode disables the kernel's own SIGINT-on-Ctrl+C.
				return nil
			case 0x1b: // Esc
				if armed {
					return nil
				}
				armed = true
				warnTimer = time.NewTimer(escWarnWindow)
				warnC = warnTimer.C
				cmd.Println(accent(w, "Press Esc again to exit cpro watch", dangerColor))
			default:
				if armed {
					disarm()
					if err := redraw(); err != nil {
						return err
					}
				}
			}
		case <-warnC:
			disarm()
			if err := redraw(); err != nil {
				return err
			}
		}
	}
}

// Only consume leading cpro options; Claude option values must stay untouched.
func runArgs(args []string) (email string, forwarded []string, help bool, err error) {
	for len(args) > 0 {
		switch {
		case args[0] == "--":
			return email, args[1:], false, nil
		case args[0] == "--help" || args[0] == "-h":
			return email, nil, true, nil
		case args[0] == "--account" || strings.HasPrefix(args[0], "--account="):
			if email != "" {
				return "", nil, false, fmt.Errorf("--account may only be specified once")
			}
			value := strings.TrimPrefix(args[0], "--account=")
			if args[0] == "--account" {
				if len(args) < 2 {
					return "", nil, false, fmt.Errorf("missing email after --account")
				}
				args = args[1:]
				value = args[0]
			}
			email, err = normalizeEmail(value)
			if err != nil {
				return "", nil, false, err
			}
			args = args[1:]
		default:
			return email, args, false, nil
		}
	}
	return email, args, false, nil
}
