package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

func newConfigCommand() *cobra.Command {
	var configJSON bool
	cfg := &cobra.Command{Use: "config", Short: "Show or interactively change cpro configuration and preferences", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		s, err := openStore()
		if err != nil {
			return err
		}
		c, err := s.read()
		if err != nil {
			return err
		}
		if configJSON {
			emails := make([]string, 0, len(c.Accounts))
			for email := range c.Accounts {
				emails = append(emails, email)
			}
			sort.Strings(emails)
			return json.NewEncoder(cmd.OutOrStdout()).Encode(struct {
				Version                 int               `json:"version"`
				Directory               string            `json:"directory"`
				Default                 string            `json:"default"`
				Accounts                []string          `json:"accounts"`
				AutoTrust               bool              `json:"autoTrust"`
				AccentColor             string            `json:"accentColor"`
				BarColor                string            `json:"barColor"`
				MaskEmail               bool              `json:"maskEmail"`
				BarWarnThreshold        float64           `json:"barWarnThreshold"`
				BarDangerThreshold      float64           `json:"barDangerThreshold"`
				WarningColor            string            `json:"warningColor"`
				DangerColor             string            `json:"dangerColor"`
				Theme                   string            `json:"theme"`
				PermissionMode          string            `json:"permissionMode"`
				PermissionModeByAccount map[string]string `json:"permissionModeByAccount"`
			}{1, s.dir, c.Default, emails, c.AutoTrust, colorName(orDefault(c.AccentColor, accentMode)), colorName(orDefault(c.BarColor, barColorSafe)), c.MaskEmail, orDefaultFloat(c.BarWarnThreshold, barWarnThreshold), orDefaultFloat(c.BarDangerThreshold, barDangerThreshold), colorName(orDefault(c.WarningColor, warningColor)), colorName(orDefault(c.DangerColor, dangerColor)), orDefault(c.Theme, currentTheme.Name), effectivePermissionMode(c.PermissionMode), c.PermissionModeByAccount})
		}
		if terminalInput() && terminalOutput(cmd.ErrOrStderr()) {
			_, err := runConfigUI(cmd, s, c, false)
			return err
		}
		if terminalOutput(cmd.OutOrStdout()) {
			cmd.Println(accent(cmd.OutOrStdout(), "CPRO CONFIG", accentMode))
		}
		cmd.Printf("Directory:    %s\n", s.dir)
		if c.Default == "" {
			cmd.Println("Default:      none")
		} else {
			cmd.Printf("Default:      %s\n", c.Default)
		}
		cmd.Printf("Accounts:     %d\n", len(c.Accounts))
		state := "off"
		if c.AutoTrust {
			state = "on"
		}
		cmd.Printf("Auto-trust:   %s (skips Claude's \"trust this folder\" prompt; toggle with cpro config trust on|off)\n", state)
		cmd.Printf("Accent color: %s (cpro's overall color; change with cpro config accent COLOR)\n", colorName(orDefault(c.AccentColor, accentMode)))
		cmd.Printf("Bar color:    %s (usage bar color below the warn threshold; change with cpro config bar COLOR)\n", colorName(orDefault(c.BarColor, barColorSafe)))
		cmd.Printf("Bar warn at:  %.0f%% (bar turns Warning color; change with cpro config warn-at PERCENT)\n", orDefaultFloat(c.BarWarnThreshold, barWarnThreshold))
		cmd.Printf("Bar danger at:%.0f%% (bar turns Danger color; change with cpro config danger-at PERCENT)\n", orDefaultFloat(c.BarDangerThreshold, barDangerThreshold))
		cmd.Printf("Warning color:%s (change with cpro config warning-color COLOR)\n", colorName(orDefault(c.WarningColor, warningColor)))
		cmd.Printf("Danger color: %s (change with cpro config danger-color COLOR)\n", colorName(orDefault(c.DangerColor, dangerColor)))
		cmd.Printf("Theme:        %s (border style; change with cpro config theme NAME)\n", orDefault(c.Theme, currentTheme.Name))
		maskState := "off"
		if c.MaskEmail {
			maskState = "on"
		}
		cmd.Printf("Mask emails:  %s (random placeholders in list/watch for screen sharing; toggle with cpro config mask on|off)\n", maskState)
		mode := permissionModeByKey(c.PermissionMode)
		cmd.Printf("Permissions:  %s (change interactively with cpro default)\n", mode.Label)
		return nil
	}}
	cfg.Flags().BoolVar(&configJSON, "json", false, "JSON output")
	// boolCommand is the shared shape behind every "on|off" toggle
	// (trust/mask below) — the same factory pattern as colorCommand/
	// thresholdCommand further down, so a third boolean preference needs
	// only one more boolCommand(...) call, not a new command shape.
	boolCommand := func(name, short, long string, set func(*cobra.Command, *store, bool) error) *cobra.Command {
		return &cobra.Command{
			Use: name + " {on|off}", Short: short,
			Long:      long,
			Args:      cobra.MatchAll(cobra.ExactArgs(1), cobra.OnlyValidArgs),
			ValidArgs: []string{"on", "off"},
			RunE: func(cmd *cobra.Command, args []string) error {
				s, err := openStore()
				if err != nil {
					return err
				}
				return set(cmd, s, args[0] == "on")
			},
		}
	}
	cfg.AddCommand(boolCommand("trust", "Enable or disable skipping Claude's folder-trust prompt",
		"When enabled, cpro run marks the current directory as trusted for the selected account before starting Claude, so the \"Do you trust this folder?\" prompt does not appear.",
		setAutoTrust))
	cfg.AddCommand(&cobra.Command{
		Use: "account EMAIL", Short: "Set the default account used by non-interactive cpro run",
		Long: "cpro run with no --account (and no forwarded Claude arguments — see runArgs) resolves this account, erroring clearly if it's unset or no longer registered. EMAIL must already be registered (cpro login EMAIL first). For a one-off account, pass --account instead; for the interactive flow's own per-run choice, use bare cpro and select \"run\".",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			email, err := normalizeEmail(args[0])
			if err != nil {
				return err
			}
			s, err := openStore()
			if err != nil {
				return err
			}
			return setDefaultAccount(cmd, s, email)
		},
	})
	cfg.AddCommand(boolCommand("mask", "Enable or disable masking emails behind a random placeholder",
		"When enabled, cpro list/watch show a random placeholder (e.g. \"kufi@ponuri\") instead of each account's real email, for screen sharing. A fresh placeholder is generated for every account each time this is toggled, on or off.",
		setMaskEmail))
	permissionModeCmd := &cobra.Command{
		Use:   "permission-mode {" + strings.Join(permissionModeNames(), "|") + "} [--account EMAIL]",
		Short: "Set the default Claude permission mode, globally or for one account",
		Long:  "Sets the permission mode a run falls back to when it carries no explicit flag (see cpro default for the interactive screen). Without --account it replaces the global default. With --account EMAIL — an already-registered account — it sets that account's own default only, so a throwaway account can default to yolo while a main account stays on ask; pass \"default\" as the mode to clear that override and fall back to the global default again.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			mode := args[0]
			email := ""
			if account, _ := cmd.Flags().GetString("account"); account != "" {
				var err error
				email, err = normalizeEmail(account)
				if err != nil {
					return err
				}
				if mode == "default" {
					mode = ""
				}
			}
			if mode != "" && !isPermissionMode(mode) {
				return fmt.Errorf("unknown permission mode %q; use one of %s (or \"default\" with --account, to clear an override)", mode, strings.Join(permissionModeNames(), ", "))
			}
			s, err := openStore()
			if err != nil {
				return err
			}
			return setPermissionMode(cmd, s, email, mode)
		},
	}
	permissionModeCmd.Flags().String("account", "", "set this account's own default instead of the global one")
	cfg.AddCommand(permissionModeCmd)
	colorCommand := func(name, short string, set func(*cobra.Command, *store, string) error) *cobra.Command {
		return &cobra.Command{
			Use: name + " {" + strings.Join(paletteNames(), "|") + "}", Short: short,
			Args: cobra.MatchAll(cobra.ExactArgs(1), cobra.OnlyValidArgs), ValidArgs: paletteNames(),
			RunE: func(cmd *cobra.Command, args []string) error {
				hex, err := colorByName(args[0])
				if err != nil {
					return err
				}
				s, err := openStore()
				if err != nil {
					return err
				}
				return set(cmd, s, hex)
			},
		}
	}
	cfg.AddCommand(colorCommand("accent", "Set cpro's overall accent color", setAccentColor))
	cfg.AddCommand(colorCommand("bar", "Set the usage bar color below the warn threshold", setBarColor))
	cfg.AddCommand(colorCommand("warning-color", "Set the usage color at/above the warn threshold", setWarningColor))
	cfg.AddCommand(colorCommand("danger-color", "Set the usage color at/above the danger threshold", setDangerColor))
	cfg.AddCommand(&cobra.Command{
		Use: "theme {" + strings.Join(themeNames(), "|") + "}", Short: "Set the TUI border style",
		Args: cobra.MatchAll(cobra.ExactArgs(1), cobra.OnlyValidArgs), ValidArgs: themeNames(),
		RunE: func(cmd *cobra.Command, args []string) error {
			t, ok := themeByName(args[0])
			if !ok {
				return fmt.Errorf("unknown theme %q; choose one of %s", args[0], strings.Join(themeNames(), ", "))
			}
			s, err := openStore()
			if err != nil {
				return err
			}
			return setTheme(cmd, s, t)
		},
	})
	thresholdCommand := func(name, short string, set func(*cobra.Command, *store, float64) error) *cobra.Command {
		return &cobra.Command{
			Use: name + " PERCENT", Short: short,
			Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				percent, err := strconv.ParseFloat(strings.TrimSuffix(args[0], "%"), 64)
				if err != nil || percent <= 0 || percent > 100 {
					return fmt.Errorf("PERCENT must be a number between 0 and 100, got %q", args[0])
				}
				s, err := openStore()
				if err != nil {
					return err
				}
				return set(cmd, s, percent)
			},
		}
	}
	cfg.AddCommand(thresholdCommand("warn-at", "Set the usage % at which the bar turns Warning color", setBarWarnThreshold))
	cfg.AddCommand(thresholdCommand("danger-at", "Set the usage % at which the bar turns Danger color", setBarDangerThreshold))
	return cfg
}

// newDefaultCommand is `cpro default`: the SET DEFAULT ACCOUNT screen
// (configui.go) as its own top-level entry point, rather than only reachable
// by navigating into cpro config first — mirroring how cpro config is itself
// a separate command from the root/menu picker's own "config" entry. It
// replaces what used to be two separate top-level commands, `cpro
// permissions` and `cpro account`: both edited one of the two values
// non-interactive `cpro run` resolves from when given no flags
// (config.PermissionMode and config.Default), so they're now one screen and
// one command, worth reaching in one press rather than through CONFIG.
//
// It's interactive-only — there's no meaningful non-interactive rendering of
// a live mode/account picker — since cpro config's own plain-text summary
// (above) already prints the same underlying state (Permissions mode,
// Default account, Auto-trust) non-interactively, and the scriptable `cpro
// config permission-mode`/`cpro config account EMAIL` (above) set either
// value directly.
func newDefaultCommand() *cobra.Command {
	return &cobra.Command{
		Use: "default", Short: "Interactively set cpro run's defaults: permission mode, workspace trust, and account", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !terminalInput() || !terminalOutput(cmd.ErrOrStderr()) {
				return fmt.Errorf("the SET DEFAULT ACCOUNT screen requires an interactive terminal; run cpro config for a non-interactive summary")
			}
			s, err := openStore()
			if err != nil {
				return err
			}
			c, err := s.read()
			if err != nil {
				return err
			}
			_, err = runDefaultUI(cmd, s, c, false)
			return err
		},
	}
}

// orDefault returns value, or fallback if value is empty — used to show the
// in-effect color (a stored preference, or else the built-in default) consistently
// across the interactive, plain-text, and JSON cpro config output.
func orDefault(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

// orDefaultFloat is orDefault for the bar warn/danger thresholds, where 0 means
// "never set".
func orDefaultFloat(value, fallback float64) float64 {
	if value > 0 {
		return value
	}
	return fallback
}

// saveAutoTrust persists the auto-trust preference — the pure-persistence half of
// setAutoTrust, split out so the interactive config UI (configui.go) can save a
// toggle immediately without also printing a confirmation line, which would
// corrupt its own live-redrawn screen.
func saveAutoTrust(s *store, enabled bool) error {
	return s.update(func(c *config) error {
		c.AutoTrust = enabled
		return nil
	})
}

// setAutoTrust saves the auto-trust preference and confirms it, for the scriptable
// cpro config trust on|off.
func setAutoTrust(cmd *cobra.Command, s *store, enabled bool) error {
	if err := saveAutoTrust(s, enabled); err != nil {
		return err
	}
	state := "disabled"
	if enabled {
		state = "enabled"
	}
	if terminalOutput(cmd.OutOrStdout()) {
		cmd.Println(accent(cmd.OutOrStdout(), "✓ Auto-trust "+state, accentMode))
	} else {
		cmd.Println("Auto-trust " + state)
	}
	return nil
}

// saveDefaultAccount persists the default account non-interactive cpro run
// resolves to when no --account is given (decision 0019, store.go's
// s.resolve) — the pure-persistence half of setDefaultAccount, split out so
// the interactive config UI (configui.go) can save a choice immediately
// without also printing a confirmation line. email must already be a
// registered account; this re-validates against the freshly read config
// (store.update always reads before applying its func), not whatever the
// caller's own possibly-stale copy looked like.
func saveDefaultAccount(s *store, email string) error {
	return s.update(func(c *config) error {
		if !c.Accounts[email] {
			return missingAccount(email)
		}
		c.Default = email
		return nil
	})
}

// setDefaultAccount saves the default account and confirms it, for the
// scriptable cpro config account EMAIL.
func setDefaultAccount(cmd *cobra.Command, s *store, email string) error {
	if err := saveDefaultAccount(s, email); err != nil {
		return err
	}
	if terminalOutput(cmd.OutOrStdout()) {
		cmd.Println(accent(cmd.OutOrStdout(), "✓ Default account: "+email, accentMode))
	} else {
		cmd.Println("Default account: " + email)
	}
	return nil
}

// saveAccentColor persists cpro's accent color preference and applies it
// immediately (so the rest of this run reflects it too, not just future ones) —
// see saveAutoTrust for why this is split from setAccentColor.
func saveAccentColor(s *store, hex string) error {
	if err := s.update(func(c *config) error {
		c.AccentColor = hex
		return nil
	}); err != nil {
		return err
	}
	accentMode = hex
	return nil
}

// setAccentColor saves cpro's accent color preference and confirms it, for the
// scriptable cpro config accent COLOR.
func setAccentColor(cmd *cobra.Command, s *store, hex string) error {
	if err := saveAccentColor(s, hex); err != nil {
		return err
	}
	if terminalOutput(cmd.OutOrStdout()) {
		cmd.Println(accent(cmd.OutOrStdout(), "✓ Accent color: "+colorName(hex), accentMode))
	} else {
		cmd.Println("Accent color: " + colorName(hex))
	}
	return nil
}

// saveBarColor persists the usage bar color preference (see barColorSafe) and
// applies it immediately — see saveAutoTrust for why this is split from
// setBarColor.
func saveBarColor(s *store, hex string) error {
	if err := s.update(func(c *config) error {
		c.BarColor = hex
		return nil
	}); err != nil {
		return err
	}
	barColorSafe = hex
	return nil
}

// setBarColor saves the usage bar color preference and confirms it, for the
// scriptable cpro config bar COLOR.
func setBarColor(cmd *cobra.Command, s *store, hex string) error {
	if err := saveBarColor(s, hex); err != nil {
		return err
	}
	if terminalOutput(cmd.OutOrStdout()) {
		cmd.Println(accent(cmd.OutOrStdout(), "✓ Bar color: "+colorName(hex), accentMode))
	} else {
		cmd.Println("Bar color: " + colorName(hex))
	}
	return nil
}

// saveWarningColor persists the usage color at/above barWarnThreshold (see
// usageColorFor) and applies it immediately — see saveAutoTrust for why this
// is split from setWarningColor.
func saveWarningColor(s *store, hex string) error {
	if err := s.update(func(c *config) error {
		c.WarningColor = hex
		return nil
	}); err != nil {
		return err
	}
	warningColor = hex
	return nil
}

// setWarningColor saves the warning color preference and confirms it, for the
// scriptable cpro config warning-color COLOR.
func setWarningColor(cmd *cobra.Command, s *store, hex string) error {
	if err := saveWarningColor(s, hex); err != nil {
		return err
	}
	if terminalOutput(cmd.OutOrStdout()) {
		cmd.Println(accent(cmd.OutOrStdout(), "✓ Warning color: "+colorName(hex), accentMode))
	} else {
		cmd.Println("Warning color: " + colorName(hex))
	}
	return nil
}

// saveDangerColor persists the usage color at/above barDangerThreshold (see
// usageColorFor), which also doubles as cpro's two-step Esc-to-exit warning
// color (dangerColor, ui.go), and applies it immediately — see saveAutoTrust
// for why this is split from setDangerColor.
func saveDangerColor(s *store, hex string) error {
	if err := s.update(func(c *config) error {
		c.DangerColor = hex
		return nil
	}); err != nil {
		return err
	}
	dangerColor = hex
	return nil
}

// setDangerColor saves the danger color preference and confirms it, for the
// scriptable cpro config danger-color COLOR.
func setDangerColor(cmd *cobra.Command, s *store, hex string) error {
	if err := saveDangerColor(s, hex); err != nil {
		return err
	}
	if terminalOutput(cmd.OutOrStdout()) {
		cmd.Println(accent(cmd.OutOrStdout(), "✓ Danger color: "+colorName(hex), accentMode))
	} else {
		cmd.Println("Danger color: " + colorName(hex))
	}
	return nil
}

// saveTheme persists the border theme preference (see currentTheme, theme.go)
// and applies it immediately — see saveAutoTrust for why this is split from
// setTheme.
func saveTheme(s *store, t borderTheme) error {
	if err := s.update(func(c *config) error {
		c.Theme = t.Name
		return nil
	}); err != nil {
		return err
	}
	currentTheme = t
	return nil
}

// setTheme saves the theme preference and confirms it, for the scriptable
// cpro config theme NAME.
func setTheme(cmd *cobra.Command, s *store, t borderTheme) error {
	if err := saveTheme(s, t); err != nil {
		return err
	}
	if terminalOutput(cmd.OutOrStdout()) {
		cmd.Println(accent(cmd.OutOrStdout(), "✓ Theme: "+t.Name, accentMode))
	} else {
		cmd.Println("Theme: " + t.Name)
	}
	return nil
}

// saveBarWarnThreshold persists the usage % at which usageBar turns the bar
// warningColor (see barWarnThreshold) and applies it immediately. It must
// stay below the danger threshold, or the bar would jump straight from
// barColorSafe to dangerColor with no warningColor step in between — see
// saveAutoTrust for why this is split from setBarWarnThreshold.
func saveBarWarnThreshold(s *store, percent float64) error {
	if err := s.update(func(c *config) error {
		danger := orDefaultFloat(c.BarDangerThreshold, barDangerThreshold)
		if percent >= danger {
			return fmt.Errorf("warn threshold (%.0f%%) must be lower than the danger threshold (%.0f%%)", percent, danger)
		}
		c.BarWarnThreshold = percent
		return nil
	}); err != nil {
		return err
	}
	barWarnThreshold = percent
	return nil
}

// setBarWarnThreshold saves the warn threshold and confirms it, for the scriptable
// cpro config warn-at PERCENT.
func setBarWarnThreshold(cmd *cobra.Command, s *store, percent float64) error {
	if err := saveBarWarnThreshold(s, percent); err != nil {
		return err
	}
	if terminalOutput(cmd.OutOrStdout()) {
		cmd.Println(accent(cmd.OutOrStdout(), fmt.Sprintf("✓ Bar warn at: %.0f%%", percent), accentMode))
	} else {
		cmd.Printf("Bar warn at: %.0f%%\n", percent)
	}
	return nil
}

// saveBarDangerThreshold persists the usage % at which usageBar turns the bar
// dangerColor (see barDangerThreshold) and applies it immediately. It must
// stay above the warn threshold, for the same reason as above.
func saveBarDangerThreshold(s *store, percent float64) error {
	if err := s.update(func(c *config) error {
		warn := orDefaultFloat(c.BarWarnThreshold, barWarnThreshold)
		if percent <= warn {
			return fmt.Errorf("danger threshold (%.0f%%) must be higher than the warn threshold (%.0f%%)", percent, warn)
		}
		c.BarDangerThreshold = percent
		return nil
	}); err != nil {
		return err
	}
	barDangerThreshold = percent
	return nil
}

// setBarDangerThreshold saves the danger threshold and confirms it, for the
// scriptable cpro config danger-at PERCENT.
func setBarDangerThreshold(cmd *cobra.Command, s *store, percent float64) error {
	if err := saveBarDangerThreshold(s, percent); err != nil {
		return err
	}
	if terminalOutput(cmd.OutOrStdout()) {
		cmd.Println(accent(cmd.OutOrStdout(), fmt.Sprintf("✓ Bar danger at: %.0f%%", percent), accentMode))
	} else {
		cmd.Printf("Bar danger at: %.0f%%\n", percent)
	}
	return nil
}

// saveMaskEmail persists the email-masking preference and applies it
// immediately (decision 0024) — so the rest of that same run, not just
// future ones, reflects it too, the same as every other saveXxx here. A
// fresh placeholder (see newEmailMask) is generated for every currently
// registered account on every toggle — on or off — so a placeholder never
// survives past the toggle that showed it; an account added afterward gets
// its own the moment it's registered (installLogin/installCredentialsFile,
// claude.go), not lazily on first render. See saveAutoTrust for why this is
// split from setMaskEmail.
func saveMaskEmail(s *store, enabled bool) error {
	var masks map[string]string
	if err := s.update(func(c *config) error {
		c.MaskEmail = enabled
		c.EmailMasks = make(map[string]string, len(c.Accounts))
		for email := range c.Accounts {
			c.EmailMasks[email] = newEmailMask()
		}
		masks = c.EmailMasks
		return nil
	}); err != nil {
		return err
	}
	maskEmailEnabled = enabled
	emailMaskTable = masks
	return nil
}

// ensureAccountMask assigns email a fresh alias in c.EmailMasks if masking
// is currently on and it doesn't have one yet — called wherever a new
// account is registered (installLogin/installCredentialsFile, claude.go),
// right before that same c is persisted, so a freshly added account is never
// displayed by its real email even for the one render between registration
// and the next explicit mask-refresh. A no-op when masking is off; c itself
// is mutated in place, matching every other in-place config field a caller
// sets right before its own s.save(c).
func ensureAccountMask(c *config, email string) {
	if !c.MaskEmail {
		return
	}
	if c.EmailMasks == nil {
		c.EmailMasks = map[string]string{}
	}
	if _, ok := c.EmailMasks[email]; !ok {
		c.EmailMasks[email] = newEmailMask()
	}
}

// setMaskEmail saves the email-masking preference and confirms it, for the
// scriptable cpro config mask on|off.
func setMaskEmail(cmd *cobra.Command, s *store, enabled bool) error {
	if err := saveMaskEmail(s, enabled); err != nil {
		return err
	}
	state := "disabled"
	if enabled {
		state = "enabled"
	}
	if terminalOutput(cmd.OutOrStdout()) {
		cmd.Println(accent(cmd.OutOrStdout(), "✓ Mask emails "+state, accentMode))
	} else {
		cmd.Println("Mask emails " + state)
	}
	return nil
}

// savePermissionMode persists the selected Claude permission mode (see
// permissions.go), validating it against permissionModes rather than trusting
// the caller — the Permissions screen only ever passes one of its own keys,
// but this is the same defense-in-depth every other saveXxx here applies at
// the actual persistence boundary. Its scriptable equivalent is `cpro config
// permission-mode MODE` (setPermissionMode below); a per-account default goes
// through saveAccountPermissionMode instead (decision 0050).
func savePermissionMode(s *store, mode string) error {
	if !isPermissionMode(mode) {
		return fmt.Errorf("unknown permission mode %q", mode)
	}
	return s.update(func(c *config) error {
		c.PermissionMode = mode
		return nil
	})
}

// saveAccountPermissionMode persists a per-account permission-mode default
// (decision 0050). mode == "" clears that account's override so it falls back
// to the global PermissionMode; any other value must be a real permissionModes
// key. email must already be registered — re-validated here against the
// freshly read config (store.update always reads before applying its func),
// the same defense saveDefaultAccount applies.
func saveAccountPermissionMode(s *store, email, mode string) error {
	if mode != "" && !isPermissionMode(mode) {
		return fmt.Errorf("unknown permission mode %q", mode)
	}
	return s.update(func(c *config) error {
		if !c.Accounts[email] {
			return missingAccount(email)
		}
		if mode == "" {
			delete(c.PermissionModeByAccount, email)
			return nil
		}
		if c.PermissionModeByAccount == nil {
			c.PermissionModeByAccount = map[string]string{}
		}
		c.PermissionModeByAccount[email] = mode
		return nil
	})
}

// setPermissionMode saves and confirms a default for the scriptable `cpro
// config permission-mode`. email == "" saves the global default; otherwise it
// saves that one account's override, or clears it when mode == "". Every other
// preference here splits persistence (saveXxx) from the printed confirmation
// (setXxx) so the interactive config UI can call the saveXxx directly without
// interleaving cmd output — this follows the same split.
func setPermissionMode(cmd *cobra.Command, s *store, email, mode string) error {
	if email == "" {
		if err := savePermissionMode(s, mode); err != nil {
			return err
		}
		if terminalOutput(cmd.OutOrStdout()) {
			cmd.Println(accent(cmd.OutOrStdout(), "✓ Permission mode: "+mode, accentMode))
		} else {
			cmd.Println("Permission mode: " + mode)
		}
		return nil
	}
	if err := saveAccountPermissionMode(s, email, mode); err != nil {
		return err
	}
	label := mode
	if label == "" {
		label = "global default"
	}
	if terminalOutput(cmd.OutOrStdout()) {
		cmd.Println(accent(cmd.OutOrStdout(), "✓ Permission mode for "+email+": "+label, accentMode))
	} else {
		cmd.Println("Permission mode for " + email + ": " + label)
	}
	return nil
}
