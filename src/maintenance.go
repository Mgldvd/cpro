package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

type doctorCheck struct {
	Name   string
	Detail string
	Err    error
}

func newDoctorCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check cpro, Claude Code, network access, and accounts",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			checks := runDoctorChecks()
			cmd.Println(accent(cmd.OutOrStdout(), "CPRO DOCTOR", accentMode))
			failures := 0
			for _, check := range checks {
				if check.Err != nil {
					failures++
					cmd.Println(doctorLine(cmd.OutOrStdout(), false, check.Name, check.Err.Error()))
				} else {
					cmd.Println(doctorLine(cmd.OutOrStdout(), true, check.Name, check.Detail))
				}
			}
			if failures > 0 {
				return fmt.Errorf("doctor found %d failed check(s)", failures)
			}
			cmd.Println(accent(cmd.OutOrStdout(), "All checks passed.", "#34D399"))
			return nil
		},
	}
}

func runDoctorChecks() []doctorCheck {
	checks := []doctorCheck{{Name: "cpro", Detail: "version " + version}}
	if key := authenticationOverride(); key != "" {
		checks = append(checks, doctorCheck{Name: "Environment", Err: fmt.Errorf("%s overrides account authentication", key)})
	} else {
		checks = append(checks, doctorCheck{Name: "Environment", Detail: "no authentication overrides"})
	}

	path, err := claudePath()
	if err != nil {
		checks = append(checks, doctorCheck{Name: "Claude Code", Err: fmt.Errorf("not found in PATH")})
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		output, commandErr := exec.CommandContext(ctx, path, "--version").Output()
		cancel()
		if commandErr != nil {
			checks = append(checks, doctorCheck{Name: "Claude Code", Err: commandErr})
		} else {
			checks = append(checks, doctorCheck{Name: "Claude Code", Detail: strings.TrimSpace(string(output))})
		}
	}
	if err := checkNetwork("https://api.anthropic.com/"); err != nil {
		checks = append(checks, doctorCheck{Name: "Anthropic network", Err: err})
	} else {
		checks = append(checks, doctorCheck{Name: "Anthropic network", Detail: "reachable"})
	}

	s, err := openStore()
	if err != nil {
		return append(checks, doctorCheck{Name: "Configuration", Err: err})
	}
	c, err := s.read()
	if err != nil {
		return append(checks, doctorCheck{Name: "Configuration", Err: err})
	}
	checks = append(checks, doctorCheck{Name: "Configuration", Detail: s.dir})
	if len(c.Accounts) == 0 {
		return append(checks, doctorCheck{Name: "Accounts", Err: fmt.Errorf("none registered; run cpro login EMAIL")})
	}
	if c.Default == "" {
		checks = append(checks, doctorCheck{Name: "Default account", Err: fmt.Errorf("none selected; run cpro config account EMAIL")})
	} else {
		checks = append(checks, doctorCheck{Name: "Default account", Detail: displayEmail(c.Default)})
	}
	emails := make([]string, 0, len(c.Accounts))
	for email := range c.Accounts {
		emails = append(emails, email)
	}
	sort.Strings(emails)
	for _, email := range emails {
		auth, authErr := authStatus(s.profile(email))
		if authErr == nil && !validAuth(email, auth) {
			authErr = fmt.Errorf("invalid session; run cpro login %s", email)
		}
		checks = append(checks, doctorCheck{Name: displayEmail(email), Detail: "authenticated", Err: authErr})
	}
	return checks
}

// claudePath resolves the claude executable in PATH — the one lookup both
// doctor's own check and cpro info's "Claude Code" line need, kept in one
// place so they can't disagree about whether it's installed.
func claudePath() (string, error) {
	return exec.LookPath("claude")
}

func checkNetwork(endpoint string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	return response.Body.Close()
}

func newInstallCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "install",
		Short: "Install cpro in ~/.local/bin",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			source, err := os.Executable()
			if err != nil {
				return err
			}
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			target := filepath.Join(home, ".local", "bin", "cpro")
			installed, err := installExecutable(source, target)
			if err != nil {
				return err
			}
			if installed {
				cmd.Printf("Installed cpro %s to %s\n", version, target)
			} else {
				cmd.Printf("cpro is already installed at %s\n", target)
			}
			if !pathContains(filepath.Dir(target)) {
				fmt.Fprintf(cmd.ErrOrStderr(), "Add %s to PATH to run cpro from any directory.\n", filepath.Dir(target))
			}
			return nil
		},
	}
}

// newInfoCommand builds "cpro info": a concise, read-only snapshot of the
// installation itself (version, binary location, config directory, account
// count, whether Claude Code is installed) — deliberately never anything
// about a specific account's usage or session state, which is cpro status's
// own job (main.go), not this one's. Unlike doctor, it never fails (there's
// nothing here to pass/fail a check on) and never touches the network or an
// account's own credentials — everything it shows is either a constant, a
// local path, or a count.
func newInfoCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "info",
		Short: "cpro information",
		Long:  "Show a concise, non-sensitive summary of this cpro installation: version, binary location, config directory, registered account count, and whether Claude Code is installed. For account usage and sessions, see cpro status.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openStore()
			if err != nil {
				return err
			}
			c, err := s.read()
			if err != nil {
				return err
			}
			binary, err := os.Executable()
			if err != nil || binary == "" {
				binary = "unknown"
			}
			claudeState := "not found"
			if _, err := claudePath(); err == nil {
				claudeState = "installed"
			}
			rows := [][2]string{
				{"Version", version},
				{"Binary", binary},
				{"Config", s.dir},
				{"Accounts", strconv.Itoa(len(c.Accounts))},
				{"Claude Code", claudeState},
			}
			labelWidth := 0
			for _, row := range rows {
				labelWidth = max(labelWidth, len(row[0]))
			}
			w := cmd.OutOrStdout()
			cmd.Println(accent(w, currentTheme.Top()+" cpro", accentMode))
			for _, row := range rows {
				cmd.Println(accent(w, currentTheme.Rail(), accentMode) + "  " + padEnd(row[0], labelWidth) + "   " + row[1])
			}
			cmd.Println(accent(w, currentTheme.Bottom(), accentMode))
			return nil
		},
	}
}

func installExecutable(source, target string) (bool, error) {
	sourceInfo, err := os.Stat(source)
	if err != nil {
		return false, err
	}
	if !sourceInfo.Mode().IsRegular() {
		return false, fmt.Errorf("current executable is not a regular file")
	}
	if targetInfo, err := os.Stat(target); err == nil && os.SameFile(sourceInfo, targetInfo) {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		return false, err
	}
	in, err := os.Open(source)
	if err != nil {
		return false, err
	}
	defer in.Close()
	out, err := os.CreateTemp(filepath.Dir(target), ".cpro-install-*")
	if err != nil {
		return false, err
	}
	defer os.Remove(out.Name())
	if err := out.Chmod(0755); err != nil {
		out.Close()
		return false, err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return false, err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return false, err
	}
	if err := out.Close(); err != nil {
		return false, err
	}
	return true, os.Rename(out.Name(), target)
}

func pathContains(dir string) bool {
	for _, entry := range filepath.SplitList(os.Getenv("PATH")) {
		if filepath.Clean(entry) == filepath.Clean(dir) {
			return true
		}
	}
	return false
}
