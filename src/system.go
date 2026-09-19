package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"github.com/spf13/cobra"
)

// systemExport writes email's cpro-managed credentials to every supported
// local Claude client (decision 0028), reporting each one's own sanitized
// outcome — Claude Code (exportAccount, unchanged: still the one real
// credential write, and still the one place a genuine validation failure
// aborts the whole command, exactly as before this decision) and Claude
// Desktop (exportToClaudeDesktop, desktop.go — detection and reporting
// only, since Desktop's own account session cannot safely be produced from
// a Claude Code credential; see its own doc comment for the full
// investigation). credentialTarget is deliberately interface-shaped even
// though there are only two concrete outcomes today: a future target would
// report through the exact same Updated/Not installed/Sign-in required
// vocabulary.
func systemExport(s *store, email string) ([]credentialTargetResult, error) {
	if err := exportAccount(s, email); err != nil {
		return nil, err
	}
	return []credentialTargetResult{
		{name: "Claude Code", status: targetUpdated},
		exportToClaudeDesktop(),
	}, nil
}

// exportAccount reads email's cpro-managed credentials — after verifying
// they're a live, matching, claude.ai session via the same authStatus/
// validAuth check every other command that touches an account's session
// already uses, not a re-derived one — and atomically writes them to the
// real system Claude credential store (systemConfigPaths, claude.go). It
// never touches config.json: no default account, no "preferred for run"
// state, nothing about cpro's own registry changes just because credentials
// were copied out — export is a one-way read of cpro's own data, not an
// account operation.
func exportAccount(s *store, email string) error {
	c, err := s.read()
	if err != nil {
		return err
	}
	if !c.Accounts[email] {
		return missingAccount(email)
	}
	auth, err := authStatus(s.profile(email))
	if err != nil {
		return err
	}
	if !validAuth(email, auth) {
		return fmt.Errorf("%s is not authenticated; run cpro login %s", email, email)
	}
	credentials, err := os.ReadFile(filepath.Join(s.profile(email), ".credentials.json"))
	if err != nil {
		return fmt.Errorf("could not read %s's credentials", email)
	}
	_, credsPath, err := systemConfigPaths()
	if err != nil {
		return err
	}
	if err := ensureExternalDir(filepath.Dir(credsPath)); err != nil {
		return err
	}
	if err := atomicWrite(credsPath, credentials); err != nil {
		return fmt.Errorf("could not write system credentials")
	}
	return nil
}

// printCredentialTargetResults renders `cpro system export`'s per-client
// report (decision 0028): each target's name padded to a shared column,
// then a filled/hollow dot (accentMode when actually updated, dim
// otherwise — the same "only the true/active state gets the accent color"
// convention onOffMark, configui.go, uses) and its label, with a target's
// own sanitized detail (never a secret) on the line right below when it has
// one.
func printCredentialTargetResults(cmd *cobra.Command, results []credentialTargetResult) {
	nameWidth := 0
	for _, r := range results {
		nameWidth = max(nameWidth, len(r.name))
	}
	color := tuiColorEnabled(cmd.OutOrStdout())
	for _, r := range results {
		dot := r.status.dot()
		if r.status == targetUpdated {
			dot = styleText(color, dot, accentMode)
		} else {
			dot = dimStyle(color, dot)
		}
		cmd.Printf("%s   %s %s\n", padEnd(r.name, nameWidth), dot, r.status.label())
		if r.detail != "" {
			cmd.Printf("  %s\n", dimStyle(color, r.detail))
		}
	}
}

// ensureExternalDir makes sure dir exists, creating it (mode 0700, matching
// cpro's own directory convention) only when it's missing entirely. Unlike
// privateDir (store.go), it never chmods an already-existing directory —
// systemConfigPaths points at Claude Code's own ~/.claude, a directory cpro
// doesn't own and has no business tightening the permissions of; this is
// only ever reached for the (uncommon) case of a system Claude Code that has
// literally never run, and so has no config directory yet at all.
func ensureExternalDir(dir string) error {
	info, err := os.Stat(dir)
	if err == nil {
		if !info.IsDir() {
			return fmt.Errorf("%s is not a directory", dir)
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.MkdirAll(dir, 0700)
}

// importSystemAccount reads the real system Claude credential store
// (systemConfigPaths) and imports it into cpro: created reports whether this
// added a new account (true) or updated one already registered (false). The
// account's identity is never guessed from the file's shape by hand — the
// credentials are staged into a temp CLAUDE_CONFIG_DIR and checked with the
// exact same authStatus (a real `claude auth status --json`) cpro already
// uses everywhere else, so "is this really a live claude.ai session, and
// whose" is answered by Claude Code itself, not reimplemented. It never
// touches config.Default: see installCredentialsFile (claude.go), which this
// calls for the actual write.
func importSystemAccount(s *store) (email string, created bool, err error) {
	_, credsPath, err := systemConfigPaths()
	if err != nil {
		return "", false, err
	}
	credentials, err := os.ReadFile(credsPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, fmt.Errorf("no system Claude credentials found; run claude auth login first")
		}
		return "", false, fmt.Errorf("could not read system credentials")
	}
	if !json.Valid(credentials) {
		return "", false, fmt.Errorf("system credentials are not valid JSON")
	}

	stage, err := os.MkdirTemp(s.dir, ".import-*")
	if err != nil {
		return "", false, err
	}
	defer os.RemoveAll(stage)
	if err := atomicWrite(filepath.Join(stage, ".credentials.json"), credentials); err != nil {
		return "", false, err
	}
	auth, err := authStatus(stage)
	if err != nil {
		return "", false, err
	}
	// validAuth normally compares against an *expected* email; there isn't
	// one yet here — the point of import is to discover it — so it's called
	// with the account's own reported email, which reduces exactly to the
	// LoggedIn/AuthMethod checks that matter without re-deriving them.
	if auth.Email == "" || !validAuth(auth.Email, auth) {
		return "", false, fmt.Errorf("system credentials are not a valid, logged-in claude.ai session")
	}
	email, err = normalizeEmail(strings.ToLower(auth.Email))
	if err != nil {
		return "", false, fmt.Errorf("system credentials contain an invalid email")
	}

	lock, err := s.accountLock(email, true)
	if err != nil {
		return "", false, err
	}
	defer lock.Close()

	created, err = s.installCredentialsFile(email, credentials)
	if err != nil {
		return "", false, fmt.Errorf("could not update the cpro account for %s", email)
	}
	return email, created, nil
}

// systemSubmenuItems is the "System credentials" submenu the root picker
// (rootui.go) opens for the single "system" row, rather than adding
// "export"/"import" as their own root-level entries.
var systemSubmenuItems = []struct{ key, label, desc string }{
	{"export", "export", "Send account to system"},
	{"import", "import", "Add system account to cpro"},
}

func newSystemCommand() *cobra.Command {
	system := &cobra.Command{
		Use:   "system",
		Short: "System credentials",
		Long:  "Transfer credentials between a cpro-managed account and the system Claude credential store, so one Claude sign-in covers both: export makes a cpro account the system `claude`'s session, import adopts the system session into cpro. It exists so you never authenticate twice on this machine; it is credential transfer, not a backup, and not a way to move cpro to another machine. Does not create or change a default account.",
	}
	system.AddCommand(&cobra.Command{
		Use:   "export [EMAIL]",
		Short: "Send a cpro account's credentials to the system credential store",
		Long:  "Write a cpro-managed account's credentials into the real Claude Code credential store (the one a bare `claude` command reads), so that account becomes claude's system session — use this when you already use cpro and don't want to sign in to the system Claude Code again. Only `.credentials.json` is copied, only on this machine. Does not change cpro's default account.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openStore()
			if err != nil {
				return err
			}
			var email string
			if len(args) == 1 {
				email, err = normalizeEmail(args[0])
				if err != nil {
					return err
				}
			} else {
				email, err = pickExportAccount(cmd, s)
				if err != nil {
					return err
				}
			}
			results, err := systemExport(s, email)
			if err != nil {
				return err
			}
			cmd.Println(accent(cmd.OutOrStdout(), "Credentials exported", accentMode))
			cmd.Println()
			printCredentialTargetResults(cmd, results)
			cmd.Println()
			cmd.Println(email)
			return nil
		},
	})
	system.AddCommand(&cobra.Command{
		Use:   "import",
		Short: "Add the system Claude credentials to cpro",
		Long:  "Read the real Claude Code credential store (the one a bare `claude` command uses) and import it into cpro: adds a new account, or updates an already-registered one, preserving its existing profile/history — use this when you already signed in to the system Claude Code and don't want to sign in to cpro again. Only `.credentials.json` is copied. Does not change cpro's default account.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openStore()
			if err != nil {
				return err
			}
			email, created, err := importSystemAccount(s)
			if err != nil {
				return err
			}
			label := "Updated"
			if created {
				label = "Imported"
			}
			cmd.Println(accent(cmd.OutOrStdout(), label, accentMode))
			cmd.Println()
			cmd.Println(email)
			return nil
		},
	})
	return system
}

// exportPickerConfig is SELECT ACCOUNT's own accountPickerApp (browseui.go)
// customization: cpro system export's account picker, reached when export is
// run with no EMAIL. A small, custom bubbletea.Model sharing tui.go's chrome
// (renderPanel/renderFooter/styleText — the same visual system as
// configApp/rootPickerApp) rather than the older huh-based pickAccount
// (ui.go, removed — decision 0019), because this screen's own spec calls for
// immediate type-to-search with no "/" shortcut, which a plain huh.Select
// never had. Single-Esc cancels, matching every other screen built this way
// in cpro (see rootPickerApp's own doc comment for why). Neither → nor ← is
// special here, matching this picker before the shared component existed.
var exportPickerConfig = accountPickerConfig{title: "SELECT ACCOUNT", actionVerb: "Export", escVerb: "Cancel"}

func pickExportAccount(cmd *cobra.Command, s *store) (string, error) {
	if !terminalInput() || !terminalOutput(cmd.ErrOrStderr()) {
		return "", fmt.Errorf("the picker requires an interactive terminal; run cpro system export EMAIL")
	}
	c, err := s.read()
	if err != nil {
		return "", err
	}
	if len(c.Accounts) == 0 {
		return "", fmt.Errorf("no accounts found; run cpro login EMAIL")
	}
	emails := make([]string, 0, len(c.Accounts))
	for email := range c.Accounts {
		emails = append(emails, email)
	}
	sort.Strings(emails)

	m := &accountPickerApp{s: s, browseList: newAccountBrowseList(emails), color: tuiColorEnabled(cmd.ErrOrStderr()), cfg: exportPickerConfig}
	// os.Stderr, not cmd.ErrOrStderr(): under NO_COLOR/TERM=dumb that's a
	// *colorprofile.Writer (see uiOutput, ui.go), not a real *os.File, and
	// bubbletea can't drive raw-mode/cursor control through one — the same
	// issue configApp/rootPickerApp already hit and fixed this same way.
	p := tea.NewProgram(m, tea.WithContext(cmd.Context()), tea.WithInput(os.Stdin), tea.WithOutput(os.Stderr))
	if _, err := p.Run(); err != nil {
		return "", err
	}
	if m.picked == "" {
		// cliError (ui.go) special-cases this exact sentinel to exit quietly
		// rather than showing an ERROR box — cancelling a picker isn't an
		// error, matching every other picker in cpro.
		return "", huh.ErrUserAborted
	}
	return m.picked, nil
}
