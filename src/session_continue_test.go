//go:build linux

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSessionTitle covers sessionTitle (session.go, decision 0064) against
// hand-written transcripts in Claude Code's own record shapes: a /rename
// custom title beats a generated one, the latest generated title wins, a title
// past the head window is still found in the tail, and the first real prompt
// is the fallback — skipping meta messages, command wrappers and tool results.
func TestSessionTitle(t *testing.T) {
	write := func(t *testing.T, lines ...string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "s.jsonl")
		if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	prompt := func(text string) string {
		return `{"type":"user","message":{"role":"user","content":"` + text + `"}}`
	}

	t.Run("latest ai-title wins", func(t *testing.T) {
		path := write(t, prompt("hello"), `{"type":"ai-title","aiTitle":"First title"}`, `{"type":"ai-title","aiTitle":"Better  title\nhere"}`)
		if got := sessionTitle(path); got != "Better title here" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("custom title beats a later generated one", func(t *testing.T) {
		path := write(t, `{"type":"custom-title","customTitle":"Mine"}`, `{"type":"ai-title","aiTitle":"Generated"}`)
		if got := sessionTitle(path); got != "Mine" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("falls back to the first real prompt", func(t *testing.T) {
		path := write(t,
			`{"type":"user","isMeta":true,"message":{"content":"meta caveat"}}`,
			prompt("<command-name>/clear</command-name>"),
			`{"type":"user","message":{"content":[{"type":"tool_result","content":"x"}]}}`,
			prompt(`fix the  login\nbug`),
			prompt("second prompt"))
		if got := sessionTitle(path); got != "fix the login bug" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("a title beyond the head window is found in the tail", func(t *testing.T) {
		filler := `{"type":"assistant","message":{"content":"` + strings.Repeat("x", 1000) + `"}}`
		lines := []string{prompt("early prompt")}
		for range 3 * sessionTitleWindow / 1000 {
			lines = append(lines, filler)
		}
		lines = append(lines, `{"type":"ai-title","aiTitle":"Late title"}`)
		if got := sessionTitle(write(t, lines...)); got != "Late title" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("nothing recognizable, or no file, is empty", func(t *testing.T) {
		if got := sessionTitle(write(t, `{}`)); got != "" {
			t.Fatalf("got %q", got)
		}
		if got := sessionTitle(filepath.Join(t.TempDir(), "missing.jsonl")); got != "" {
			t.Fatalf("got %q", got)
		}
	})
}

// TestDedupeSessions: one row per session ID, the first (newest) copy kept,
// every other holder recorded against it; distinct sessions untouched.
func TestDedupeSessions(t *testing.T) {
	entries := []sessionEntry{
		{email: "b@example.com", dirName: "-p", sessionID: "s1"},
		{email: "c@example.com", dirName: "-q", sessionID: "s2"},
		{email: "a@example.com", dirName: "-p", sessionID: "s1"},
	}
	unique, alsoIn := dedupeSessions(entries)
	if len(unique) != 2 || unique[0].email != "b@example.com" || unique[1].sessionID != "s2" {
		t.Fatalf("unique = %+v", unique)
	}
	if got := alsoIn[sessionEntryKey(unique[0])]; len(got) != 1 || got[0] != "a@example.com" {
		t.Fatalf("alsoIn = %v", alsoIn)
	}
	if len(alsoIn[sessionEntryKey(unique[1])]) != 0 {
		t.Fatalf("a session with one copy must list no others: %v", alsoIn)
	}
}

// TestSessionPickerAnnotations covers decision 0064's picker additions without
// a pty (the same reasoning TestSessionAppDirect gives): titles arrive
// asynchronously and become searchable, CONTINUE SESSION shows a moved session
// once with "also in", and DESTINATION ACCOUNT tags a signed-out account.
func TestSessionPickerAnnotations(t *testing.T) {
	s := &store{dir: t.TempDir()}
	c := config{Accounts: map[string]bool{"a@example.com": true, "b@example.com": true}}
	const id = "33333333-3333-4333-8333-333333333333"
	for _, email := range []string{"a@example.com", "b@example.com"} {
		dir := filepath.Join(s.profile(email), "projects", projectDirName("/home/user/projects/app"))
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(`{"type":"ai-title","aiTitle":"Refactor billing"}`+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	m := &sessionApp{s: s, c: c, stack: newNavStack(screenContinuePicker)}
	m.openPicker("continue")
	if len(m.picker.items) != 1 {
		t.Fatalf("expected the two copies collapsed into one row, got %+v", m.picker.items)
	}
	load := m.Init()
	if load == nil {
		t.Fatal("expected opening the picker to queue its title load")
	}
	m.Update(load())
	view := m.viewPickerBrowse()
	if !strings.Contains(view, "Refactor billing") || !strings.Contains(view, "also in") {
		t.Fatalf("expected the title and the other copy's account, got %q", view)
	}
	m.picker.typeRune("billing")
	if len(m.picker.filtered) != 1 {
		t.Fatal("expected the title to be searchable")
	}

	m.picker.clearSearch()
	e := m.picker.items[0]
	m.selectSession(&e)
	m.Update(sessionAuthMsg{email: "a@example.com", ok: true})
	m.Update(sessionAuthMsg{email: "b@example.com", ok: false})
	lines := m.destinationLines(m.account.items, 0)
	if strings.Contains(lines[0], signedOutTag) || !strings.Contains(lines[1], signedOutTag) {
		t.Fatalf("expected only b tagged signed out, got %q", lines)
	}
	if note := m.continuingNote(); !strings.Contains(note, "Refactor billing") {
		t.Fatalf("expected the note to carry the session's title, got %q", note)
	}
}
