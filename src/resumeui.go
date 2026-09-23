//go:build linux

package main

import (
	"fmt"
	"os"
	"sort"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"github.com/spf13/cobra"
)

// resumePickerConfig is RESUME ACCOUNT's own accountPickerApp (browseui.go)
// customization: cpro --resume's interactive fallback for when the session's
// owning account can't be used to resume it (signed out, invalid session) and
// no explicit --account override was given — the same searchable account
// picker as SELECT ACCOUNT (system.go), rootui.go's own RUN ACCOUNT frame,
// and sessionui.go's DESTINATION ACCOUNT, all sharing one browseList state
// machine (browseui.go), one account-row renderer, and one Session-usage
// fetch, rather than each reimplementing them. Unlike SELECT ACCOUNT, → also
// selects here (forward), matching RUN ACCOUNT's own convention.
var resumePickerConfig = accountPickerConfig{title: "RESUME ACCOUNT", actionVerb: "Continue", escVerb: "Back", forward: true}

// pickResumeAccount opens RESUME ACCOUNT, offering every registered account
// except excluded (the session's own unusable owner) — deliberately excluded
// from the list since it's already been ruled out. (sessionui.go's own
// DESTINATION ACCOUNT, by contrast, offers every account, owner included —
// decision 0063 — since nothing has ruled the owner out there.) Returns the chosen email, or a clear error naming excluded
// and suggesting --account when no terminal is available to show the picker
// at all, or when excluded is the only registered account.
func pickResumeAccount(cmd *cobra.Command, s *store, c config, excluded string) (string, error) {
	emails := make([]string, 0, len(c.Accounts))
	for email := range c.Accounts {
		if email == excluded {
			continue
		}
		emails = append(emails, email)
	}
	sort.Strings(emails)
	if len(emails) == 0 {
		return "", fmt.Errorf("%s cannot resume this session (not authenticated), and no other account is registered; run cpro login EMAIL or pass --account", excluded)
	}
	if !terminalInput() || !terminalOutput(cmd.ErrOrStderr()) {
		return "", fmt.Errorf("%s cannot resume this session (not authenticated); pass --account EMAIL to resume under a different account", excluded)
	}

	m := &accountPickerApp{s: s, browseList: newAccountBrowseList(emails), color: tuiColorEnabled(cmd.ErrOrStderr()), cfg: resumePickerConfig}
	// os.Stderr, not cmd.ErrOrStderr(): under NO_COLOR/TERM=dumb that's a
	// *colorprofile.Writer (see uiOutput, ui.go), not a real *os.File, and
	// bubbletea can't drive raw-mode/cursor control through one — the same
	// fix SELECT ACCOUNT/configApp/rootPickerApp already apply.
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
