//go:build linux

package main

import (
	"bytes"
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
// locally without a request, and a failure with nothing cached is an error.
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

	t.Run("expired token is detected locally, without a request", func(t *testing.T) {
		srv, profile := newServer(t), newProfile(t, time.Now().Add(-time.Minute))
		_, st, err := loadUsage(profile, srv.url)
		if err == nil || st.Reason != "token expired" || *srv.calls != 0 {
			t.Fatalf("expected a local token-expired failure and no request: %+v %v calls=%d", st, err, *srv.calls)
		}
	})

	t.Run("401 is reported as an expired token", func(t *testing.T) {
		srv, profile := newServer(t), newProfile(t, time.Time{})
		*srv.status = http.StatusUnauthorized
		if _, st, err := loadUsage(profile, srv.url); err == nil || st.Reason != "token expired" {
			t.Fatalf("got %+v %v", st, err)
		}
		if note := usageStaleNote(usageStatus{Stale: true, FetchedAt: time.Now(), Reason: "token expired"}); !strings.Contains(note, "run Claude on this account") {
			t.Fatalf("expected the fix in the note, got %q", note)
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
