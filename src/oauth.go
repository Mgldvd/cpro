//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Claude Code's own OAuth refresh parameters (decision 0066), read from the
// installed Claude Code 2.1.280 rather than guessed: its production config's
// TOKEN_URL and CLIENT_ID, and the request body its refresh function sends.
// Variables, not constants, only so tests can point them at a local server.
var (
	oauthTokenURL = "https://platform.claude.com/v1/oauth/token"
	oauthClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
)

const (
	// oauthRefreshLockName is the proper-lockfile lock Claude Code takes (a
	// directory, inside the account's config dir) around its own refresh.
	// Taking the same one means cpro and a Claude process never refresh the
	// same account at once — with rotating refresh tokens, the loser of that
	// race would be signed out.
	oauthRefreshLockName = ".oauth_refresh.lock"
	// oauthRefreshLockStale matches Claude Code's own stale threshold: a lock
	// older than this was left by a process that died holding it.
	oauthRefreshLockStale = 60 * time.Second
	// oauthRefreshEarly refreshes a token this close to expiring, so a fetch
	// never races the expiry itself.
	oauthRefreshEarly = 5 * time.Minute
)

// errRefreshSignedOut means the refresh token itself was rejected: only a new
// login can fix the account.
var errRefreshSignedOut = errors.New("signed out")

// errRefreshSkipped means refreshing was deliberately not attempted now:
// another process holds the refresh lock or cpro's own account lock, or a
// live Claude session is running under the account (which refreshes its own
// token). The caller treats the token as still expired, and tries again after
// its usual backoff.
var errRefreshSkipped = errors.New("refresh skipped")

// refreshAccountToken renews profile's expired claude.ai access token with
// its stored refresh token, the same request Claude Code makes itself, so an
// account left idle — typically the one whose week is full — keeps updating
// in cpro status/watch with no one having to open Claude under it (decision
// 0066). It writes back only the token fields, preserving every other key in
// .credentials.json, and only after a successful response: a failure leaves
// the file exactly as it was. failedToken is the access token the caller
// found unusable: if the file already holds a different one, another process
// renewed it meanwhile and nothing more is done.
func refreshAccountToken(profile, failedToken string) error {
	// A cpro-launched session refreshes its own token; leave it to it.
	if len(liveSessionPIDs(profile)) > 0 {
		return errRefreshSkipped
	}
	// cpro's own exclusive account lock keeps this off a profile that
	// login/logout/remove/system import are rewriting.
	lockPath := profileLockPath(profile)
	if err := privateDir(filepath.Dir(lockPath)); err != nil {
		return err
	}
	lock, err := fileLock(lockPath, true)
	if err != nil {
		return errRefreshSkipped
	}
	defer lock.Close()
	release, err := takeOAuthRefreshLock(profile)
	if err != nil {
		return errRefreshSkipped
	}
	defer release()

	path := filepath.Join(profile, ".credentials.json")
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var file map[string]any
	if err := json.Unmarshal(b, &file); err != nil {
		return err
	}
	oauth, _ := file["claudeAiOauth"].(map[string]any)
	if oauth == nil {
		return errRefreshSignedOut
	}
	// Re-checked under the lock: another process may have renewed it already.
	if current, _ := oauth["accessToken"].(string); current != "" && current != failedToken {
		return nil
	}
	refreshToken, _ := oauth["refreshToken"].(string)
	if refreshToken == "" {
		return errRefreshSignedOut
	}
	var scopes []string
	if list, ok := oauth["scopes"].([]any); ok {
		for _, s := range list {
			if s, ok := s.(string); ok {
				scopes = append(scopes, s)
			}
		}
	}

	body, _ := json.Marshal(map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
		"client_id":     oauthClientID,
		"scope":         strings.Join(scopes, " "),
	})
	req, err := http.NewRequest(http.MethodPost, oauthTokenURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "cpro/"+version)
	// Well under oauthRefreshLockStale, so the lock is never taken for
	// abandoned while this request is still running.
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Only invalid_grant means the refresh token itself is dead. Checked
		// live: an unknown client_id is a 400 too ("Client ... not found"),
		// and must not be reported as the user being signed out.
		var oerr struct {
			Error string `json:"error"`
		}
		if json.NewDecoder(resp.Body).Decode(&oerr) == nil && oerr.Error == "invalid_grant" {
			return errRefreshSignedOut
		}
		return fmt.Errorf("token refresh returned HTTP %d", resp.StatusCode)
	}
	var tok struct {
		AccessToken           string `json:"access_token"`
		RefreshToken          string `json:"refresh_token"`
		ExpiresIn             int64  `json:"expires_in"`
		RefreshTokenExpiresIn int64  `json:"refresh_token_expires_in"`
		Scope                 string `json:"scope"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil || tok.AccessToken == "" || tok.ExpiresIn <= 0 {
		return fmt.Errorf("token refresh returned an unusable response")
	}

	now := time.Now()
	oauth["accessToken"] = tok.AccessToken
	if tok.RefreshToken != "" { // absent means the old one stays valid
		oauth["refreshToken"] = tok.RefreshToken
	}
	oauth["expiresAt"] = now.Add(time.Duration(tok.ExpiresIn) * time.Second).UnixMilli()
	if tok.RefreshTokenExpiresIn > 0 {
		oauth["refreshTokenExpiresAt"] = now.Add(time.Duration(tok.RefreshTokenExpiresIn) * time.Second).UnixMilli()
	}
	if granted := strings.Fields(tok.Scope); len(granted) > 0 {
		oauth["scopes"] = granted
	}
	out, err := json.Marshal(file)
	if err != nil {
		return err
	}
	return atomicWrite(path, out)
}

// takeOAuthRefreshLock acquires Claude Code's refresh lock the way its
// proper-lockfile library does — creating the lock directory atomically —
// clearing it first only when it's older than Claude's own stale threshold.
// It never waits: a held lock means someone else is refreshing right now.
func takeOAuthRefreshLock(profile string) (release func(), err error) {
	path := filepath.Join(profile, oauthRefreshLockName)
	for attempt := 0; attempt < 2; attempt++ {
		if err = os.Mkdir(path, 0700); err == nil {
			return func() { os.Remove(path) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		info, statErr := os.Stat(path)
		if statErr != nil || time.Since(info.ModTime()) < oauthRefreshLockStale {
			return nil, err
		}
		os.Remove(path) // abandoned by a process that died holding it
	}
	return nil, err
}

// profileLockPath is store.accountLockPath's path for a profile directory
// (<config>/accounts/<hash> -> <config>/locks/<hash>.lock), for callers like
// the usage fetch that know only the profile, not the store and email.
func profileLockPath(profile string) string {
	return filepath.Join(filepath.Dir(filepath.Dir(profile)), "locks", filepath.Base(profile)+".lock")
}
