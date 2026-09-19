package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

type config struct {
	Version            int               `json:"version"`
	Default            string            `json:"default"`
	Accounts           map[string]bool   `json:"accounts"`
	AutoTrust          bool              `json:"autoTrust,omitempty"`
	AccentColor        string            `json:"accentColor,omitempty"`
	BarColor           string            `json:"barColor,omitempty"`
	MaskEmail          bool              `json:"maskEmail,omitempty"`
	EmailMasks         map[string]string `json:"emailMasks,omitempty"`
	BarWarnThreshold   float64           `json:"barWarnThreshold,omitempty"`
	BarDangerThreshold float64           `json:"barDangerThreshold,omitempty"`
	WarningColor       string            `json:"warningColor,omitempty"`
	DangerColor        string            `json:"dangerColor,omitempty"`
	Theme              string            `json:"theme,omitempty"`
	PermissionMode     string            `json:"permissionMode,omitempty"`

	// PermissionModeByAccount is a per-account default that overrides
	// PermissionMode for that account only (decision 0050) — a throwaway
	// account can default to YOLO while a main account stays on Ask, without
	// switching Permissions before every run. An account with no entry, or
	// whose entry is no longer a real permissionModes key, falls back to
	// PermissionMode, so an old or hand-edited config.json keeps working; nil
	// until the first `cpro config permission-mode --account ...` sets one.
	// permissionModeForAccount (permissions.go) is the one reader.
	PermissionModeByAccount map[string]string `json:"permissionModeByAccount,omitempty"`

	// BlockedCommands is deprecated (decision 0020: the "Block always"
	// feature was removed from the Permissions UI and command builder
	// entirely) and no longer read anywhere — this field exists solely so an
	// old config.json that already has a "blockedCommands" key still loads
	// without error, round-tripping the value unread rather than dropping it
	// on the next save. Never populated or applied to a claude invocation.
	BlockedCommands []blockedCommand `json:"blockedCommands,omitempty"`
}

// blockedCommand is BlockedCommands' element type — kept only so old
// config.json data of this shape still unmarshals; see BlockedCommands'
// own doc comment.
type blockedCommand struct {
	Command string `json:"command"`
	Enabled bool   `json:"enabled"`
}

type store struct{ dir string }

func normalizeEmail(value string) (string, error) {
	a, err := mail.ParseAddress(value)
	if err != nil || a.Address != value || len(value) > 254 || strings.ContainsAny(value, "\r\n\t ") {
		return "", fmt.Errorf("invalid email; use an address such as you@example.com")
	}
	return strings.ToLower(value), nil
}

func openStore() (*store, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("XDG_CONFIG_HOME must be an absolute path")
	}
	return &store{filepath.Join(dir, "cpro")}, nil
}

func (s *store) profile(email string) string {
	return filepath.Join(s.dir, "accounts", fmt.Sprintf("%x", sha256.Sum256([]byte(email))))
}

func (s *store) read() (config, error) {
	c := config{Version: 1, Accounts: map[string]bool{}}
	b, err := os.ReadFile(filepath.Join(s.dir, "config.json"))
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err = json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("invalid config.json: %w", err)
	}
	if c.Version != 1 || c.Accounts == nil {
		return c, fmt.Errorf("unsupported config.json version or format")
	}
	for email, present := range c.Accounts {
		normalized, err := normalizeEmail(email)
		if err != nil || normalized != email || !present {
			return c, fmt.Errorf("invalid account in config.json")
		}
	}
	if c.Default != "" && !c.Accounts[c.Default] {
		return c, fmt.Errorf("default account is not registered in config.json")
	}
	return c, nil
}

// resolve turns a possibly-empty email into a real, registered account:
// non-empty input is validated against c.Accounts; empty input falls back to
// c.Default (see decision 0019 — this is what makes non-interactive cpro run
// work with no --account at all).
func (s *store) resolve(email string) (string, error) {
	c, err := s.read()
	if err != nil {
		return "", err
	}
	if email == "" {
		email = c.Default
	}
	if email == "" {
		return "", fmt.Errorf("no default account configured; set one with: cpro config")
	}
	if !c.Accounts[email] {
		return "", missingAccount(email)
	}
	return email, nil
}

func missingAccount(email string) error {
	return fmt.Errorf("account is not registered; run cpro login %s", email)
}

func privateDir(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s must be a real directory", path)
	}
	return os.Chmod(path, 0700)
}

func fileLock(path string, exclusive bool) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	mode := syscall.LOCK_SH
	if exclusive {
		mode = syscall.LOCK_EX
	}
	if err = syscall.Flock(int(f.Fd()), mode|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("profile or configuration is in use; close the session or wait for the operation to finish")
	}
	return f, nil
}

// accountLockPath names the flock file that guards one account's profile,
// independent of whether it can be taken. Split out from accountLock so a
// caller that failed to lock can still say *which* file is held — and look up
// its holder (session.go's lockHolder) — instead of only reporting that
// something, somewhere, is in use.
func (s *store) accountLockPath(email string) string {
	return filepath.Join(s.dir, "locks", filepath.Base(s.profile(email))+".lock")
}

func (s *store) accountLock(email string, exclusive bool) (*os.File, error) {
	if err := privateDir(s.dir); err != nil {
		return nil, err
	}
	if err := privateDir(filepath.Join(s.dir, "locks")); err != nil {
		return nil, err
	}
	return fileLock(s.accountLockPath(email), exclusive)
}

func (s *store) update(fn func(*config) error) error {
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
	if err = fn(&c); err != nil {
		return err
	}
	return s.save(c)
}

func (s *store) save(c config) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(s.dir, "config.json"), append(b, '\n'))
}

func (s *store) remove(email string) error {
	trash, err := os.MkdirTemp(s.dir, ".remove-*")
	if err != nil {
		return err
	}
	oldPath, newPath := s.profile(email), filepath.Join(trash, "profile")
	moved := false
	err = s.update(func(c *config) error {
		if err := os.Rename(oldPath, newPath); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return err
			}
		} else {
			moved = true
		}
		delete(c.Accounts, email)
		delete(c.EmailMasks, email)
		if c.Default == email {
			c.Default = ""
		}
		return nil
	})
	if err != nil && moved {
		if restoreErr := os.Rename(newPath, oldPath); restoreErr != nil {
			return fmt.Errorf("%w; profile recovery failed, data kept at %s: %v", err, newPath, restoreErr)
		}
	}
	return errors.Join(err, os.RemoveAll(trash))
}

func atomicWrite(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".cpro-write-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
