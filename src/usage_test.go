//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// TestUsageFreshness covers decision 0065's loadUsage behavior against a
// local server: a failure keeps the last value but marks it stale with a
// reason and age, a rate limit is backed off (shared through the cache file,
// so no request is sent until it expires), an expired token is detected
// before any usage request, and a failure with nothing cached is an error.
func TestUsageFreshness(t *testing.T) {
	const body = `{"five_hour":{"utilization":20,"resets_at":"2099-01-01T00:00:00Z"},"seven_day":{"utilization":100,"resets_at":"2099-01-02T00:00:00Z"}}`
	type server struct {
		url    string
		calls  *int
		status *int
	}
	newServer := func(t *testing.T) server {
		calls, status := new(int), new(int)
		*status = http.StatusOK
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			*calls++
			if *status != http.StatusOK {
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(*status)
				return
			}
			fmt.Fprint(w, body)
		}))
		t.Cleanup(srv.Close)
		return server{srv.URL, calls, status}
	}
	newProfile := func(t *testing.T, expiresAt time.Time) string {
		profile := t.TempDir()
		creds := `{"claudeAiOauth":{"accessToken":"t"}}`
		if !expiresAt.IsZero() {
			creds = fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"t","expiresAt":%d}}`, expiresAt.UnixMilli())
		}
		if err := os.WriteFile(filepath.Join(profile, ".credentials.json"), []byte(creds), 0600); err != nil {
			t.Fatal(err)
		}
		return profile
	}
	age := func(t *testing.T, profile string, d time.Duration) {
		t.Helper()
		path := filepath.Join(profile, "cpro-usage.json")
		c, err := readUsageCache(path)
		if err != nil {
			t.Fatal(err)
		}
		c.FetchedAt = c.FetchedAt.Add(-d)
		writeUsageCache(path, c)
	}

	t.Run("cache is fresh for less than the default watch interval", func(t *testing.T) {
		if usageFreshFor >= 60*time.Second {
			t.Fatalf("usageFreshFor %v must be under cpro watch's 60s default, or every other refresh reuses the cache", usageFreshFor)
		}
	})

	t.Run("rate limited: stale with reason, then backed off without new requests", func(t *testing.T) {
		srv, profile := newServer(t), newProfile(t, time.Now().Add(time.Hour))
		if _, st, err := loadUsage(profile, srv.url); err != nil || st.Stale {
			t.Fatalf("first fetch: %+v %v", st, err)
		}
		age(t, profile, 2*time.Minute)
		*srv.status = http.StatusTooManyRequests
		usage, st, err := loadUsage(profile, srv.url)
		if err != nil || !st.Stale || st.Reason != "rate limited" || usage.SevenDay.Utilization != 100 {
			t.Fatalf("expected the old value, stale, rate limited: %+v %+v %v", usage, st, err)
		}
		if note := usageStaleNote(st); !strings.Contains(note, "updated 2m ago") || !strings.Contains(note, "rate limited") {
			t.Fatalf("stale note = %q", note)
		}
		calls := *srv.calls
		for range 3 {
			if _, st, _ := loadUsage(profile, srv.url); !st.Stale {
				t.Fatal("expected still stale during backoff")
			}
		}
		if *srv.calls != calls {
			t.Fatalf("backoff must not send requests: %d -> %d", calls, *srv.calls)
		}
		// Once the backoff expires, a successful fetch clears the failure.
		path := filepath.Join(profile, "cpro-usage.json")
		c, _ := readUsageCache(path)
		c.RetryAt = time.Now().Add(-time.Second)
		writeUsageCache(path, c)
		*srv.status = http.StatusOK
		if _, st, err := loadUsage(profile, srv.url); err != nil || st.Stale {
			t.Fatalf("expected a live value after the backoff: %+v %v", st, err)
		}
		if c, _ := readUsageCache(path); c.FailReason != "" || c.FailCount != 0 {
			t.Fatalf("a success must clear the failure record: %+v", c)
		}
	})

	t.Run("backoff grows and is capped", func(t *testing.T) {
		if got := backoffFor(time.Minute, 1); got != time.Minute {
			t.Fatalf("first failure: %v", got)
		}
		if got := backoffFor(time.Minute, 3); got != 4*time.Minute {
			t.Fatalf("third failure: %v", got)
		}
		if got := backoffFor(time.Minute, 20); got != usageBackoffMax {
			t.Fatalf("capped: %v", got)
		}
	})
}

// TestUsageElapsedWindowRendering: a stale value whose window already reset
// renders as "--"/"reset" in the full card and "--" in the compact row, is
// left out of Total week, and the compact view no longer shows an unavailable
// account as 0%.
func TestUsageElapsedWindowRendering(t *testing.T) {
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	future := time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339)
	const a, b = "a@example.com", "b@example.com"
	states := map[string]accountSnapshotState{
		a: {authenticated: true,
			usage:       accountUsage{FiveHour: usageWindow{Utilization: 40, ResetsAt: future}, SevenDay: usageWindow{Utilization: 100, ResetsAt: past}},
			usageStatus: usageStatus{Stale: true, FetchedAt: time.Now().Add(-3 * time.Hour), Reason: "token expired"}},
		b: {authenticated: true, usageErr: fmt.Errorf("offline")},
	}
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)

	if err := renderFullView(cmd, &out, 120, []string{a, b}, states); err != nil {
		t.Fatal(err)
	}
	full := out.String()
	for _, want := range []string{"reset", "--", "updated 3h 0m ago · token expired"} {
		if !strings.Contains(full, want) {
			t.Fatalf("full view missing %q:\n%s", want, full)
		}
	}
	if strings.Contains(full, "100%") || strings.Contains(full, "Total week") {
		t.Fatalf("an elapsed week must not show 100%% nor count toward Total week:\n%s", full)
	}

	out.Reset()
	if err := renderCompactView(cmd, &out, 120, []string{a, b}, states); err != nil {
		t.Fatal(err)
	}
	compact := out.String()
	if strings.Contains(compact, " 0%") || strings.Count(compact, "--") != 3 {
		t.Fatalf("expected --, never 0%%, for the elapsed week and both of b's windows:\n%s", compact)
	}
	if !strings.Contains(compact, "3h 0m ago") {
		t.Fatalf("expected the compact row to show its age:\n%s", compact)
	}
}

// TestOAuthRefresh covers decision 0066: cpro renews an idle account's expired
// token itself — the same refresh request Claude Code makes — against a local
// token server, so usage keeps updating with no one opening Claude.
func TestOAuthRefresh(t *testing.T) {
	type env struct {
		profile     string
		usageCalls  *int
		tokenCalls  *int
		tokenStatus *int
		usageURL    string
		lastRefresh *map[string]string
		tokenBody   *string
	}
	setup := func(t *testing.T, creds string) env {
		t.Helper()
		// A profile shaped like cpro's own (<config>/accounts/<hash>), so the
		// account lock path resolves inside this test's directory.
		profile := filepath.Join(t.TempDir(), "accounts", "hash")
		if err := os.MkdirAll(profile, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(profile, ".credentials.json"), []byte(creds), 0600); err != nil {
			t.Fatal(err)
		}
		e := env{profile: profile, usageCalls: new(int), tokenCalls: new(int), tokenStatus: new(int), lastRefresh: new(map[string]string), tokenBody: new(string)}
		*e.tokenBody = `{"error":"invalid_grant","error_description":"Refresh token not found or invalid"}`
		*e.tokenStatus = http.StatusOK
		usage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			*e.usageCalls++
			if r.Header.Get("Authorization") != "Bearer new-access" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			fmt.Fprint(w, `{"five_hour":{"utilization":7},"seven_day":{"utilization":100}}`)
		}))
		t.Cleanup(usage.Close)
		token := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			*e.tokenCalls++
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			*e.lastRefresh = body
			if *e.tokenStatus != http.StatusOK {
				w.WriteHeader(*e.tokenStatus)
				fmt.Fprint(w, *e.tokenBody)
				return
			}
			fmt.Fprint(w, `{"access_token":"new-access","refresh_token":"new-refresh","expires_in":28800,"scope":"user:inference user:profile"}`)
		}))
		t.Cleanup(token.Close)
		old := oauthTokenURL
		oauthTokenURL = token.URL
		t.Cleanup(func() { oauthTokenURL = old })
		e.usageURL = usage.URL
		return e
	}
	expired := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"old-access","refreshToken":"old-refresh","expiresAt":%d,"scopes":["user:inference","user:profile"],"subscriptionType":"pro"},"mcpOAuth":{"keep":"me"}}`,
		time.Now().Add(-time.Hour).UnixMilli())
	readCreds := func(t *testing.T, profile string) map[string]any {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(profile, ".credentials.json"))
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}

	t.Run("an expired token is renewed and usage fetched with the new one", func(t *testing.T) {
		e := setup(t, expired)
		usage, st, err := loadUsage(e.profile, e.usageURL)
		if err != nil || st.Stale || usage.SevenDay.Utilization != 100 {
			t.Fatalf("expected live usage after renewal: %+v %+v %v", usage, st, err)
		}
		if *e.tokenCalls != 1 || *e.usageCalls != 1 {
			t.Fatalf("expected one refresh then one usage request, got %d/%d", *e.tokenCalls, *e.usageCalls)
		}
		req := *e.lastRefresh
		if req["grant_type"] != "refresh_token" || req["refresh_token"] != "old-refresh" || req["client_id"] != oauthClientID || req["scope"] != "user:inference user:profile" {
			t.Fatalf("refresh request = %v", req)
		}
		creds := readCreds(t, e.profile)
		oauth := creds["claudeAiOauth"].(map[string]any)
		if oauth["accessToken"] != "new-access" || oauth["refreshToken"] != "new-refresh" || oauth["subscriptionType"] != "pro" {
			t.Fatalf("credentials not updated in place: %v", oauth)
		}
		if exp := int64(oauth["expiresAt"].(float64)); time.UnixMilli(exp).Before(time.Now().Add(7 * time.Hour)) {
			t.Fatalf("expiresAt not moved forward: %v", time.UnixMilli(exp))
		}
		if mcp, _ := creds["mcpOAuth"].(map[string]any); mcp["keep"] != "me" {
			t.Fatalf("other credential keys must survive: %v", creds)
		}
		if info, err := os.Stat(filepath.Join(e.profile, ".credentials.json")); err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("credentials must stay 0600: %v %v", info.Mode(), err)
		}
		if _, err := os.Stat(filepath.Join(e.profile, oauthRefreshLockName)); !os.IsNotExist(err) {
			t.Fatal("the refresh lock must be released")
		}
	})

	t.Run("a 401 with an unexpired token renews once and retries", func(t *testing.T) {
		e := setup(t, fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"old-access","refreshToken":"old-refresh","expiresAt":%d}}`, time.Now().Add(time.Hour).UnixMilli()))
		if _, st, err := loadUsage(e.profile, e.usageURL); err != nil || st.Stale {
			t.Fatalf("expected the retry to succeed: %+v %v", st, err)
		}
		if *e.usageCalls != 2 || *e.tokenCalls != 1 {
			t.Fatalf("expected 401, refresh, retry: usage=%d token=%d", *e.usageCalls, *e.tokenCalls)
		}
	})

	t.Run("a rejected refresh token reports signed out and leaves credentials untouched", func(t *testing.T) {
		e := setup(t, expired)
		*e.tokenStatus = http.StatusBadRequest
		if _, st, err := loadUsage(e.profile, e.usageURL); err == nil || st.Reason != "signed out" || *e.usageCalls != 0 {
			t.Fatalf("got %+v %v usage=%d", st, err, *e.usageCalls)
		}
		if oauth := readCreds(t, e.profile)["claudeAiOauth"].(map[string]any); oauth["accessToken"] != "old-access" {
			t.Fatalf("a failed refresh must not write: %v", oauth)
		}
		if note := usageStaleNote(usageStatus{Stale: true, FetchedAt: time.Now(), Reason: "signed out"}); !strings.Contains(note, "run cpro login") {
			t.Fatalf("expected the fix in the note, got %q", note)
		}
	})

	t.Run("Claude holding its refresh lock means skip, not race", func(t *testing.T) {
		e := setup(t, expired)
		if err := os.Mkdir(filepath.Join(e.profile, oauthRefreshLockName), 0700); err != nil {
			t.Fatal(err)
		}
		if _, st, err := loadUsage(e.profile, e.usageURL); err == nil || st.Reason != "token expired" || *e.tokenCalls != 0 {
			t.Fatalf("expected a skipped refresh: %+v %v token=%d", st, err, *e.tokenCalls)
		}
	})

	t.Run("an abandoned refresh lock is taken over", func(t *testing.T) {
		e := setup(t, expired)
		lock := filepath.Join(e.profile, oauthRefreshLockName)
		if err := os.Mkdir(lock, 0700); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-2 * oauthRefreshLockStale)
		if err := os.Chtimes(lock, old, old); err != nil {
			t.Fatal(err)
		}
		if _, _, err := loadUsage(e.profile, e.usageURL); err != nil || *e.tokenCalls != 1 {
			t.Fatalf("expected the stale lock taken and the refresh done: %v token=%d", err, *e.tokenCalls)
		}
	})

	t.Run("a 400 that isn't invalid_grant is not reported as signed out", func(t *testing.T) {
		e := setup(t, expired)
		*e.tokenStatus = http.StatusBadRequest
		*e.tokenBody = `{"type":"error","error":{"type":"invalid_request_error","message":"Client not found"}}`
		if _, st, err := loadUsage(e.profile, e.usageURL); err == nil || st.Reason != "token expired" {
			t.Fatalf("got %+v %v", st, err)
		}
	})

	t.Run("no refresh token means signed out, without a request", func(t *testing.T) {
		e := setup(t, fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"old-access","expiresAt":%d}}`, time.Now().Add(-time.Hour).UnixMilli()))
		if _, st, err := loadUsage(e.profile, e.usageURL); err == nil || st.Reason != "signed out" || *e.tokenCalls != 0 {
			t.Fatalf("got %+v %v token=%d", st, err, *e.tokenCalls)
		}
	})

	t.Run("profileLockPath matches the store's account lock", func(t *testing.T) {
		s := &store{dir: t.TempDir()}
		if got, want := profileLockPath(s.profile("a@example.com")), s.accountLockPath("a@example.com"); got != want {
			t.Fatalf("%s != %s", got, want)
		}
	})
}

// TestCheckOAuthRefreshCompat covers doctor's "Token renewal" check (decision
// 0068): both of cpro's copied Claude Code values are found in the executable
// — through a symlink, and even when one straddles the 4MB read boundary —
// and a missing one is named.
func TestCheckOAuthRefreshCompat(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, data []byte) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, data, 0755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	// The token URL straddles the end of the first read (4MB plus the overlap
	// the scan keeps); the client ID sits after it.
	firstRead := 4<<20 + max(len(oauthTokenURL), len(oauthClientID)) - 1
	data := bytes.Repeat([]byte{'x'}, firstRead-10)
	data = append(data, []byte(oauthTokenURL)...)
	data = append(data, bytes.Repeat([]byte{'y'}, 100)...)
	data = append(data, []byte(oauthClientID)...)
	real := write("claude-real", data)
	link := filepath.Join(dir, "claude")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if err := checkOAuthRefreshCompat(link); err != nil {
		t.Fatalf("expected both values found through the symlink: %v", err)
	}

	onlyURL := write("claude-old", []byte("…"+oauthTokenURL+"…"))
	err := checkOAuthRefreshCompat(onlyURL)
	if err == nil || !strings.Contains(err.Error(), "client ID") || strings.Contains(err.Error(), "token URL") {
		t.Fatalf("expected only the client ID reported missing, got %v", err)
	}
	if err := checkOAuthRefreshCompat(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("expected an error for a missing executable")
	}
}
