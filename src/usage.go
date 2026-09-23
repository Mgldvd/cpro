package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

const usageEndpoint = "https://api.anthropic.com/api/oauth/usage?at_wall=1&skip_spend=1"

// usageFreshFor is how long a fetched value is served from cache without
// asking again. Deliberately a little under cpro watch's 60s default
// interval: with the two equal, the next redraw found a cache 59.x seconds
// old — still "fresh" — so real data only changed every other refresh
// (decision 0065).
const usageFreshFor = 55 * time.Second

// Backoff after a failed fetch (decision 0065). The usage endpoint rate-limits
// hard — per IP, and shared with Claude Code itself and every other cpro
// window — so retrying on every redraw only extends the 429. An expired token
// is renewed by cpro itself (decision 0066); a dead login ("signed out", "not
// signed in") only a new login fixes, so it waits longest. Consecutive
// failures double the wait up to usageBackoffMax.
const (
	usageBackoffBase    = time.Minute
	usageBackoffMax     = 10 * time.Minute
	usageBackoffExpired = 5 * time.Minute
)

type usageWindow struct {
	Utilization float64 `json:"utilization"`
	ResetsAt    string  `json:"resets_at"`
}

type accountUsage struct {
	FiveHour usageWindow `json:"five_hour"`
	SevenDay usageWindow `json:"seven_day"`
}

// usageCache is cpro-usage.json. FetchedAt/Usage are the last successful
// fetch (FetchedAt zero: none yet). FailReason/FailCount/RetryAt record the
// latest failure since then, so every cpro process sharing this profile
// honors the same backoff instead of each hammering the endpoint on its own.
type usageCache struct {
	FetchedAt  time.Time    `json:"fetched_at"`
	Usage      accountUsage `json:"usage"`
	FailReason string       `json:"fail_reason,omitempty"`
	FailCount  int          `json:"fail_count,omitempty"`
	RetryAt    time.Time    `json:"retry_at,omitzero"`
}

// usageStatus says how current a loadUsage result is. Stale means the value
// shown is the last successful fetch, from FetchedAt, because the newest
// attempt failed (Reason) or is waiting out a backoff — it must be shown as
// such, never passed off as live (decision 0065).
type usageStatus struct {
	Stale     bool
	FetchedAt time.Time
	Reason    string
}

// usageFetchError is a failed fetch with a short, user-facing reason and how
// long to wait before trying again.
type usageFetchError struct {
	reason string
	wait   time.Duration
}

func (e *usageFetchError) Error() string { return e.reason }

// loadUsage returns an account's usage: from cache while fresh, otherwise
// fetched — unless a previous failure's backoff is still running. When the
// fetch fails (or is skipped for backoff) and an earlier value exists, that
// value is returned with status.Stale set and no error; only "never fetched
// successfully" is an error.
func loadUsage(profile, endpoint string) (accountUsage, usageStatus, error) {
	cachePath := filepath.Join(profile, "cpro-usage.json")
	cached, cacheErr := readUsageCache(cachePath)
	if cacheErr != nil {
		cached = usageCache{}
	}
	hasData := !cached.FetchedAt.IsZero()
	now := time.Now()

	if hasData && now.Sub(cached.FetchedAt) < usageFreshFor && cached.FailReason == "" {
		return cached.Usage, usageStatus{FetchedAt: cached.FetchedAt}, nil
	}
	if cached.FailReason != "" && now.Before(cached.RetryAt) {
		return staleOrError(cached, hasData)
	}

	usage, err := fetchUsage(profile, endpoint)
	if err != nil {
		var ferr *usageFetchError
		if !errors.As(err, &ferr) {
			ferr = &usageFetchError{reason: "offline", wait: usageBackoffBase}
		}
		cached.FailCount++
		cached.FailReason = ferr.reason
		cached.RetryAt = now.Add(backoffFor(ferr.wait, cached.FailCount))
		writeUsageCache(cachePath, cached)
		return staleOrError(cached, hasData)
	}
	cached = usageCache{FetchedAt: now, Usage: usage}
	// A cache that can't be written costs only the next request, never this
	// already-fetched, valid value.
	writeUsageCache(cachePath, cached)
	return usage, usageStatus{FetchedAt: now}, nil
}

func staleOrError(cached usageCache, hasData bool) (accountUsage, usageStatus, error) {
	if !hasData {
		return accountUsage{}, usageStatus{Reason: cached.FailReason}, errors.New(cached.FailReason)
	}
	return cached.Usage, usageStatus{Stale: true, FetchedAt: cached.FetchedAt, Reason: cached.FailReason}, nil
}

// backoffFor doubles wait per consecutive failure beyond the first, capped at
// usageBackoffMax (but never below wait itself, e.g. a longer Retry-After).
func backoffFor(wait time.Duration, failures int) time.Duration {
	d := wait
	for i := 1; i < failures && d < usageBackoffMax; i++ {
		d *= 2
	}
	return max(wait, min(d, usageBackoffMax))
}

func readUsageCache(path string) (usageCache, error) {
	var cached usageCache
	b, err := os.ReadFile(path)
	if err != nil {
		return cached, err
	}
	err = json.Unmarshal(b, &cached)
	return cached, err
}

func writeUsageCache(path string, cached usageCache) {
	if b, err := json.Marshal(cached); err == nil {
		_ = atomicWrite(path, append(b, '\n'))
	}
}

// oauthCredentials is the part of .credentials.json fetchUsage needs.
type oauthCredentials struct {
	ClaudeAI struct {
		AccessToken string `json:"accessToken"`
		ExpiresAt   int64  `json:"expiresAt"` // Unix milliseconds; 0 when absent
	} `json:"claudeAiOauth"`
}

func readOAuthCredentials(profile string) (oauthCredentials, error) {
	var creds oauthCredentials
	b, err := os.ReadFile(filepath.Join(profile, ".credentials.json"))
	if err != nil {
		return creds, err
	}
	if err := json.Unmarshal(b, &creds); err != nil || creds.ClaudeAI.AccessToken == "" {
		return creds, fmt.Errorf("OAuth credentials unavailable")
	}
	return creds, nil
}

// renewToken refreshes profile's token (refreshAccountToken, oauth.go) and
// maps the outcome to the reason usage reports when it could not: "signed
// out" needs a new login, "token expired" is retried after the backoff
// (decision 0066).
func renewToken(profile, failedToken string) error {
	switch err := refreshAccountToken(profile, failedToken); {
	case err == nil:
		return nil
	case errors.Is(err, errRefreshSignedOut):
		return &usageFetchError{reason: "signed out", wait: usageBackoffExpired}
	default:
		return &usageFetchError{reason: "token expired", wait: usageBackoffBase}
	}
}

func fetchUsage(profile, endpoint string) (accountUsage, error) {
	var usage accountUsage
	creds, err := readOAuthCredentials(profile)
	if err != nil {
		return usage, &usageFetchError{reason: "not signed in", wait: usageBackoffExpired}
	}
	// cpro renews an expired (or about-to-expire) token itself, the way Claude
	// Code would if it were running, so an idle account keeps updating with no
	// one opening Claude under it (decision 0066).
	if exp := creds.ClaudeAI.ExpiresAt; exp > 0 && time.Now().Add(oauthRefreshEarly).After(time.UnixMilli(exp)) {
		if err := renewToken(profile, creds.ClaudeAI.AccessToken); err != nil {
			return usage, err
		}
		if creds, err = readOAuthCredentials(profile); err != nil {
			return usage, &usageFetchError{reason: "not signed in", wait: usageBackoffExpired}
		}
	}

	usage, status, err := requestUsage(endpoint, creds.ClaudeAI.AccessToken)
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		// Revoked or expired early, which expiresAt couldn't show: renew once
		// and retry, rather than waiting out a backoff for nothing.
		if err := renewToken(profile, creds.ClaudeAI.AccessToken); err != nil {
			return usage, err
		}
		if creds, err = readOAuthCredentials(profile); err != nil {
			return usage, &usageFetchError{reason: "not signed in", wait: usageBackoffExpired}
		}
		usage, _, err = requestUsage(endpoint, creds.ClaudeAI.AccessToken)
	}
	return usage, err
}

// requestUsage makes the usage request itself, returning the HTTP status
// alongside the classified error so fetchUsage can react to a 401.
func requestUsage(endpoint, accessToken string) (accountUsage, int, error) {
	var usage accountUsage
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return usage, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	req.Header.Set("User-Agent", "cpro/"+version)
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return usage, 0, &usageFetchError{reason: "offline", wait: usageBackoffBase}
	}
	defer response.Body.Close()
	switch {
	case response.StatusCode == http.StatusTooManyRequests:
		wait := usageBackoffBase
		if secs, err := strconv.Atoi(response.Header.Get("Retry-After")); err == nil && time.Duration(secs)*time.Second > wait {
			wait = time.Duration(secs) * time.Second
		}
		return usage, response.StatusCode, &usageFetchError{reason: "rate limited", wait: wait}
	case response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden:
		return usage, response.StatusCode, &usageFetchError{reason: "token expired", wait: usageBackoffExpired}
	case response.StatusCode != http.StatusOK:
		return usage, response.StatusCode, &usageFetchError{reason: fmt.Sprintf("HTTP %d", response.StatusCode), wait: usageBackoffBase}
	}
	if err := json.NewDecoder(response.Body).Decode(&usage); err != nil {
		return usage, response.StatusCode, &usageFetchError{reason: "bad response", wait: usageBackoffBase}
	}
	return usage, response.StatusCode, nil
}
