//go:build linux

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// projectDirName mirrors the folder-name encoding Claude Code itself uses
// under CLAUDE_CONFIG_DIR/projects for a given working directory: every "/"
// becomes "-". Confirmed against every real project folder Claude Code has
// actually created on this machine; there's no evidence any other character
// needs translating, so this deliberately doesn't try to replicate more of
// Claude Code's own slugging than what's observable.
func projectDirName(cwd string) string {
	return strings.ReplaceAll(cwd, "/", "-")
}

// continueSession copies every Claude session transcript recorded under one
// project directory from fromEmail's cpro profile into toEmail's, so a
// conversation started under one account (typically one that just hit its
// usage limit) can be picked up under another. It only ever adds files: the
// originals under fromEmail are untouched, and a transcript already present
// under toEmail (same session ID) is left alone rather than overwritten.
// Only toEmail is locked (shared — see sessionCopyLock) since fromEmail is
// only ever read here, the same way exportAccount reads a source account's
// credentials with no lock at all.
//
// sessionID, when non-empty, is an explicit --resume SESSION_ID already
// forwarded by the caller (decision 0025's interactive session picker always
// supplies one, having shown the user exactly which session they picked from
// across every account/directory): the project directory to copy is found by
// locating that ID's own transcript (findSessionDir), not the current
// process's own working directory, since the session picked may not belong
// to whatever directory cpro happens to be run from. An empty sessionID
// preserves the original, unchanged behavior — the current working
// directory's own project folder — for the plain, non-interactive
// `cpro session continue FROM_EMAIL TO_EMAIL` invocation.
func continueSession(s *store, fromEmail, toEmail, sessionID string) (copied int, err error) {
	c, err := s.read()
	if err != nil {
		return 0, err
	}
	if !c.Accounts[fromEmail] {
		return 0, missingAccount(fromEmail)
	}
	if !c.Accounts[toEmail] {
		return 0, missingAccount(toEmail)
	}
	if fromEmail == toEmail {
		return 0, fmt.Errorf("FROM_EMAIL and TO_EMAIL must be different accounts")
	}

	var dirName string
	if sessionID != "" {
		dirName, err = findSessionDir(s, fromEmail, sessionID)
		if err != nil {
			return 0, err
		}
	} else {
		cwd, err := os.Getwd()
		if err != nil {
			return 0, err
		}
		dirName = projectDirName(cwd)
	}
	srcDir := filepath.Join(s.profile(fromEmail), "projects", dirName)
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return 0, fmt.Errorf("no Claude session history found for this directory under %s", fromEmail)
	}

	lock, err := sessionCopyLock(s, toEmail)
	if err != nil {
		return 0, err
	}
	defer lock.Close()

	dstDir := filepath.Join(s.profile(toEmail), "projects", dirName)
	if err := privateDir(dstDir); err != nil {
		return 0, err
	}

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		if sessionID != "" && name != sessionID+".jsonl" {
			continue // an explicit session moves alone (decision 0064)
		}
		dst := filepath.Join(dstDir, name)
		if _, err := os.Stat(dst); err == nil {
			continue // already present under toEmail
		}
		data, err := os.ReadFile(filepath.Join(srcDir, name))
		if err != nil {
			return copied, err
		}
		if err := atomicWrite(dst, data); err != nil {
			return copied, err
		}
		copied++
	}
	return copied, nil
}

// sessionCopyLock takes the lock continueSession/migrateSession hold while
// adding a transcript to toEmail's profile. Shared, not exclusive: adding a
// new <sessionID>.jsonl (atomicWrite, never overwriting) cannot disturb a
// running claude, so a busy destination — the usual case, since that is the
// account work is moving to — must not block it (decision 0063; an exclusive
// lock here once failed whenever claude's background helpers still held the
// lock it used to inherit, see decision 0064). The shared lock still excludes
// login/logout/remove while they run. When even that fails, the holder is
// named, the same diagnostic deleteSessions gives.
func sessionCopyLock(s *store, email string) (*os.File, error) {
	lock, err := s.accountLock(email, false)
	if err == nil {
		return lock, nil
	}
	if holder := lockHolder(s.accountLockPath(email)); holder != "" {
		return nil, fmt.Errorf("%s: %w — held by %s", displayEmail(email), err, holder)
	}
	return nil, fmt.Errorf("%s: %w", displayEmail(email), err)
}

// findSessionDir locates which project directory under fromEmail's profile
// contains sessionID's own transcript file — see continueSession's own doc
// comment for why this is needed once a session can be chosen from any
// directory, not just the current one.
func findSessionDir(s *store, fromEmail, sessionID string) (string, error) {
	projectsDir := filepath.Join(s.profile(fromEmail), "projects")
	dirs, err := os.ReadDir(projectsDir)
	if err != nil {
		return "", fmt.Errorf("no Claude session history found for %s under %s", sessionID, fromEmail)
	}
	for _, dir := range dirs {
		if !dir.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(projectsDir, dir.Name(), sessionID+".jsonl")); err == nil {
			return dir.Name(), nil
		}
	}
	return "", fmt.Errorf("no Claude session history found for %s under %s", sessionID, fromEmail)
}

// resumeSessionIDArg extracts the value following an explicit --resume flag
// already present in extra (the arguments continue forwards to Claude
// alongside its own --resume), if any — used to look up the right project
// directory by session ID instead of the current working directory; see
// continueSession.
func resumeSessionIDArg(extra []string) string {
	for i, a := range extra {
		if a == "--resume" && i+1 < len(extra) {
			return extra[i+1]
		}
	}
	return ""
}

// sessionEntry is one recorded Claude conversation found under some
// registered account's own projects directory — the same on-disk unit
// continueSession already operates on (one *.jsonl transcript file), just
// enumerated across every account and every working directory instead of
// only the current one, for the interactive session picker (sessionui.go,
// decision 0025).
type sessionEntry struct {
	email     string    // the cpro account this session's transcript lives under
	dirName   string    // the raw, encoded project directory name (projectDirName's own output)
	sessionID string    // the transcript's file name without ".jsonl" — Claude's own session ID
	modTime   time.Time // the transcript file's own mtime, used as "last activity"
	// title is what the conversation is about (sessionTitle), filled in
	// asynchronously by the interactive pickers only — listSessions never
	// reads transcript content, and the CLI list doesn't show it.
	title string
}

// dedupeSessions keeps one row per session ID — the first, i.e. the newest,
// since listSessions sorts newest first, which is also the copy
// findSessionOwner resumes from — and records which other accounts hold a
// copy, keyed by the kept row's sessionEntryKey. A session moved between
// accounts otherwise shows up once per copy in CONTINUE SESSION.
func dedupeSessions(entries []sessionEntry) (unique []sessionEntry, alsoIn map[string][]string) {
	alsoIn = map[string][]string{}
	kept := map[string]int{}
	for _, e := range entries {
		if i, ok := kept[e.sessionID]; ok {
			key := sessionEntryKey(unique[i])
			alsoIn[key] = append(alsoIn[key], e.email)
			continue
		}
		kept[e.sessionID] = len(unique)
		unique = append(unique, e)
	}
	return unique, alsoIn
}

// sessionTitleWindow bounds how much of a transcript sessionTitle reads from
// each end. Transcripts run to tens of megabytes, so reading them whole for
// every row would stall the picker; measured across real transcripts, Claude
// Code's latest title record always sat within the last ~40KB.
const sessionTitleWindow = 64 << 10

// sessionTitle best-effort names a recorded conversation from its own
// transcript: the latest title Claude Code recorded (a /rename "custom-title"
// wins over its generated "ai-title"; an older "summary" record counts too),
// read from the transcript's tail, falling back to the first real prompt
// typed, from its head. Returns "" when neither is found — the row simply
// shows no title. Never an error: this is display text only.
func sessionTitle(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return ""
	}
	size := info.Size()
	read := func(off, n int64) []byte {
		buf := make([]byte, n)
		got, _ := f.ReadAt(buf, off)
		return buf[:got]
	}
	var head, tail []byte
	if size <= 2*sessionTitleWindow {
		head = read(0, size)
		tail = head
	} else {
		head = read(0, sessionTitleWindow)
		tail = read(size-sessionTitleWindow, sessionTitleWindow)
		if i := bytes.IndexByte(tail, '\n'); i >= 0 {
			tail = tail[i+1:] // drop the partial first line
		}
	}
	if t := latestRecordedTitle(tail); t != "" {
		return t
	}
	if t := latestRecordedTitle(head); t != "" {
		return t
	}
	return firstPrompt(head)
}

// latestRecordedTitle scans transcript lines for Claude Code's own title
// records, preferring a user-set custom title over a generated one, and the
// last of each kind over earlier ones (Claude rewrites them as a
// conversation evolves).
func latestRecordedTitle(data []byte) string {
	var custom, generated string
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		if !bytes.Contains(line, []byte(`title"`)) && !bytes.Contains(line, []byte(`"summary"`)) {
			continue // cheap filter: never unmarshal a large message line
		}
		var rec struct {
			Type        string `json:"type"`
			CustomTitle string `json:"customTitle"`
			AITitle     string `json:"aiTitle"`
			Summary     string `json:"summary"`
		}
		if json.Unmarshal(line, &rec) != nil {
			continue
		}
		switch rec.Type {
		case "custom-title":
			if rec.CustomTitle != "" {
				custom = rec.CustomTitle
			}
		case "ai-title":
			if rec.AITitle != "" {
				generated = rec.AITitle
			}
		case "summary":
			if rec.Summary != "" {
				generated = rec.Summary
			}
		}
	}
	if custom != "" {
		return oneLine(custom)
	}
	return oneLine(generated)
}

// firstPrompt returns the first prompt the user actually typed: a user
// message whose content is plain text, skipping Claude Code's own meta
// messages and injected <command-…>/<local-command-…> wrappers.
func firstPrompt(data []byte) string {
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		if !bytes.Contains(line, []byte(`"type":"user"`)) {
			continue
		}
		var rec struct {
			IsMeta  bool `json:"isMeta"`
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &rec) != nil || rec.IsMeta {
			continue
		}
		var text string
		if json.Unmarshal(rec.Message.Content, &text) != nil {
			continue // a tool result or other structured content, not a prompt
		}
		if text = oneLine(text); text != "" && !strings.HasPrefix(text, "<") {
			return text
		}
	}
	return ""
}

// oneLine collapses all whitespace runs, newlines included, to single spaces.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// listSessions enumerates every recorded session across every registered
// account, newest first by last activity. It never reads a transcript's own
// content — only the file system layout continueSession already relies on
// (one directory per working directory, one *.jsonl per session) — so the
// session picker never needs a second data source or a heavier index to stay
// in sync with what continue actually operates on. A missing/unreadable
// projects directory for an account (never used cpro session yet) is simply
// skipped, not an error.
func listSessions(s *store, c config) []sessionEntry {
	var out []sessionEntry
	for email := range c.Accounts {
		projectsDir := filepath.Join(s.profile(email), "projects")
		dirs, err := os.ReadDir(projectsDir)
		if err != nil {
			continue
		}
		for _, dir := range dirs {
			if !dir.IsDir() {
				continue
			}
			sessDir := filepath.Join(projectsDir, dir.Name())
			files, err := os.ReadDir(sessDir)
			if err != nil {
				continue
			}
			for _, f := range files {
				name := f.Name()
				if f.IsDir() || !strings.HasSuffix(name, ".jsonl") {
					continue
				}
				info, err := f.Info()
				if err != nil {
					continue
				}
				out = append(out, sessionEntry{
					email:     email,
					dirName:   dir.Name(),
					sessionID: strings.TrimSuffix(name, ".jsonl"),
					modTime:   info.ModTime(),
				})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].modTime.After(out[j].modTime) })
	return out
}

// resolveSession turns a user-supplied selector into exactly one recorded
// session. The selector is a full session ID or an unambiguous prefix of one
// (the full ID is the first column `cpro session list` prints, so a line can
// be pasted straight in); account, when non-empty, restricts the search to
// that one account. Ambiguity is an error listing the candidates rather than a
// guess: the same session ID can legitimately live under two accounts at once
// after a `cpro session continue` copy, so "which one" is a real question the
// caller has to answer, not a detail to choose for them.
func resolveSession(s *store, c config, selector, account string) (sessionEntry, error) {
	if selector == "" {
		return sessionEntry{}, fmt.Errorf("a session ID, or an unambiguous prefix of one, is required")
	}
	var matches []sessionEntry
	for _, e := range listSessions(s, c) {
		if account != "" && e.email != account {
			continue
		}
		if e.sessionID == selector || strings.HasPrefix(e.sessionID, selector) {
			matches = append(matches, e)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		if account != "" {
			return sessionEntry{}, fmt.Errorf("no recorded session matches %q under %s", selector, account)
		}
		return sessionEntry{}, fmt.Errorf("no recorded session matches %q", selector)
	default:
		var b strings.Builder
		fmt.Fprintf(&b, "%q matches %d sessions; pass --account, or a longer prefix, to pick one:", selector, len(matches))
		for _, e := range matches {
			fmt.Fprintf(&b, "\n  %s  %s  %s", e.sessionID, projectDisplayPath(e.dirName), displayEmail(e.email))
		}
		return sessionEntry{}, errors.New(b.String())
	}
}

// sessionEntryKey identifies one recorded session uniquely: the same session ID
// can exist under two accounts (and even two project directories), so the ID
// alone is not a key. Used to collapse duplicate selectors and to track the
// in-app multi-select set.
func sessionEntryKey(e sessionEntry) string {
	return e.email + "|" + e.dirName + "|" + e.sessionID
}

// parseSessionSelector splits the optional account-qualified form
// "EMAIL:SESSION_ID" a variadic `session delete` accepts. It exists because the
// same session ID can be recorded under two accounts after a `continue` copy:
// with several IDs in one invocation, a single --account could not say which
// account each one belongs to. Anything without an "@" before the first colon
// is left alone, so a bare ID (and a bare prefix) keeps its old meaning. Emails
// cannot contain ":" and session IDs are UUIDs, so the split is unambiguous.
func parseSessionSelector(arg string) (account, id string) {
	if i := strings.Index(arg, ":"); i > 0 && strings.Contains(arg[:i], "@") {
		return strings.ToLower(arg[:i]), arg[i+1:]
	}
	return "", arg
}

// resolveSessions resolves every selector to a recorded session, in order, with
// duplicates (the same entry named twice, or a prefix colliding with an exact
// ID) collapsed. defaultAccount applies to selectors that don't name their own
// account; a qualified selector always wins over it. The first problem — an
// unknown selector, or an ambiguous one — aborts the whole call, so a bulk
// delete never removes a subset after a typo.
func resolveSessions(s *store, c config, args []string, defaultAccount string) ([]sessionEntry, error) {
	var out []sessionEntry
	seen := map[string]bool{}
	for _, arg := range args {
		account, id := parseSessionSelector(arg)
		if account == "" {
			account = defaultAccount
		}
		if account != "" && !c.Accounts[account] {
			return nil, missingAccount(account)
		}
		e, err := resolveSession(s, c, id, account)
		if err != nil {
			return nil, err
		}
		if key := sessionEntryKey(e); !seen[key] {
			seen[key] = true
			out = append(out, e)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no session selected")
	}
	return out, nil
}

// deleteSessions permanently removes every listed session's transcript — each
// one's own <sessionID>.jsonl, and nothing else. Removing that file is what
// makes a session disappear from `session list`, from both pickers and from
// `--resume`'s own discovery alike, because all four read this same on-disk
// layout (continueSession copies exactly this file too); cpro owns no other
// representation of a session that could go stale. Irreversible, so the caller
// owns the confirmation — the CLI's guards or the in-app DELETE SESSION frame —
// the same division `remove` keeps.
//
// A session a live Claude process is writing right now (deletableSessions) is
// refused and named rather than removed: deleting the conversation someone is in
// the middle of is the one outcome worse than a failed command. The DELETE
// SESSION picker never lists those at all, so this guard exists for the other
// door — an ID the user typed on the command line, which cannot be hidden from
// them.
//
// Entries are grouped by owning account and each account's SHARED lock is taken
// once around all of its removals, rather than once per file: a bulk delete of
// twenty sessions under one account should not open and close the same flock
// twenty times. Shared, not exclusive, is the whole point (decision 0053): a
// live Claude session used to inherit its account's shared lock for its entire
// life, so an exclusive lock made "one session is open under this account" fail
// every delete of that account — including the idle sessions that have nothing
// to do with it. (Decision 0064 stopped that inheritance; shared stays right,
// since deleting one transcript never needs the whole profile to itself.) Deleting one transcript only ever touches cpro's own
// per-session file, so it does not collide with the profile-wide operations
// (login/logout/remove) exclusive locks actually serialize; those still block
// it, because a shared lock waits on an exclusive one. An account whose
// exclusive lock is held — another cpro operation is in progress under it — is
// skipped whole and reported by name with the number of sessions it cost, while
// every other account still proceeds; that lock is non-blocking, so waiting
// would hang a cleanup behind an unrelated command. Returns how many were
// actually removed, plus a joined error naming every failure — a file that
// vanished between listing and deleting does not abort the rest either, so one
// stale entry cannot block a cleanup.
func deleteSessions(s *store, entries []sessionEntry) (deleted int, err error) {
	if len(entries) == 0 {
		return 0, nil
	}
	c, readErr := s.read()
	if readErr != nil {
		return 0, readErr
	}
	deletable, active := deletableSessions(s, c, entries)

	byAccount := map[string][]sessionEntry{}
	accounts := make([]string, 0, len(deletable))
	for _, e := range deletable {
		if _, ok := byAccount[e.email]; !ok {
			accounts = append(accounts, e.email)
		}
		byAccount[e.email] = append(byAccount[e.email], e)
	}
	sort.Strings(accounts)

	var failures []error
	if activeErr := activeSessionsError(active); activeErr != nil {
		failures = append(failures, activeErr)
	}
	for _, email := range accounts {
		lock, lockErr := s.accountLock(email, false)
		if lockErr != nil {
			// Named per account, with the count it cost: the raw lock error says
			// "profile or configuration is in use" without saying whose, which
			// reads as a generic failure when a batch spans accounts and only
			// one of them is busy (reported live while deleting seven sessions).
			// The holder is appended when /proc can say — "close the session or
			// wait" is unactionable advice when the blocking operation might be
			// your own forgotten login sitting in another terminal.
			note := ""
			if holder := lockHolder(s.accountLockPath(email)); holder != "" {
				note = " — held by " + holder
			}
			failures = append(failures, fmt.Errorf("%s (%d session(s) skipped): %w%s",
				displayEmail(email), len(byAccount[email]), lockErr, note))
			continue
		}
		for _, e := range byAccount[email] {
			path := filepath.Join(s.profile(email), "projects", e.dirName, e.sessionID+".jsonl")
			if removeErr := os.Remove(path); removeErr != nil {
				if os.IsNotExist(removeErr) {
					failures = append(failures, fmt.Errorf("session %s under %s is already gone", shortSessionID(e.sessionID), displayEmail(email)))
					continue
				}
				failures = append(failures, fmt.Errorf("session %s: %w", shortSessionID(e.sessionID), removeErr))
				continue
			}
			deleted++
		}
		lock.Close()
	}
	return deleted, errors.Join(failures...)
}

// activeSessionsError names every session a live Claude process is using, one
// line each, so a refused delete says which conversation is in the way instead
// of only that something is. Nil when nothing is active, so a caller can append
// it to failures unconditionally.
func activeSessionsError(active []sessionEntry) error {
	var errs []error
	for _, e := range active {
		errs = append(errs, fmt.Errorf("session %s (%s, %s) is in use by a running Claude session and was not deleted",
			shortSessionID(e.sessionID), projectDisplayPath(e.dirName), displayEmail(e.email)))
	}
	return errors.Join(errs...)
}

// lockHolder best-effort names the process currently holding a flock file, by
// scanning /proc for an fd that resolves to it — the same "the /proc scan is the
// only source" approach runningSessions (claude.go) already takes for the
// account-to-process mapping, and the same answer lsof would give, without
// requiring lsof to be installed. Returns "" whenever it cannot tell (no /proc,
// another user's fd table, a holder outside this namespace): this is diagnostic
// text appended to an error message, never a reason to change what the operation
// actually does.
func lockHolder(path string) string {
	procs, err := os.ReadDir("/proc")
	if err != nil {
		return ""
	}
	for _, proc := range procs {
		pid := proc.Name()
		if pid == "" || pid[0] < '0' || pid[0] > '9' {
			continue
		}
		fds, err := os.ReadDir(filepath.Join("/proc", pid, "fd"))
		if err != nil {
			continue // gone, or not ours to inspect
		}
		for _, fd := range fds {
			target, err := os.Readlink(filepath.Join("/proc", pid, "fd", fd.Name()))
			if err != nil || target != path {
				continue
			}
			command := ""
			if raw, err := os.ReadFile(filepath.Join("/proc", pid, "comm")); err == nil {
				command = strings.TrimSpace(string(raw))
			}
			if command == "" {
				return "pid " + pid
			}
			return "pid " + pid + " (" + command + ")"
		}
	}
	return ""
}

// sessionIDFromCmdline pulls the conversation id a live Claude process names on
// its own command line, if it names one at all: cpro resumes, and Claude Code's
// own --session-id, put it right there — which makes "which transcript is this
// process writing?" exact instead of a guess. Accepts both `--flag value` and
// `--flag=value`.
func sessionIDFromCmdline(argv []string, flags ...string) string {
	for i, a := range argv {
		for _, flag := range flags {
			if a == flag && i+1 < len(argv) {
				return strings.TrimSpace(argv[i+1])
			}
			if strings.HasPrefix(a, flag+"=") {
				return strings.TrimSpace(strings.TrimPrefix(a, flag+"="))
			}
		}
	}
	return ""
}

// liveProcess is the little /proc tells us about a running Claude process:
// enough to decide which transcript it is writing (cmdline) and where (cwd).
type liveProcess struct {
	pid     int
	cmdline []string
	cwd     string
}

// liveProcessesForProfile finds every running process whose environment carries
// CLAUDE_CONFIG_DIR=<profile> — the only account-to-process mapping that exists,
// the same one runningSessions (claude.go) relies on. Unlike runningSessions it
// does not require a terminal: a headless `cpro run` writes a transcript just as
// much as an interactive one, and missing it is the dangerous direction here
// (it would offer a live conversation for deletion).
func liveProcessesForProfile(profile string) []liveProcess {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	want := []byte("CLAUDE_CONFIG_DIR=" + profile)
	var out []liveProcess
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		environ, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
		if err != nil || !hasEnvVar(environ, want) {
			continue
		}
		raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if err != nil {
			continue
		}
		var argv []string
		for _, part := range bytes.Split(raw, []byte{0}) {
			if len(part) > 0 {
				argv = append(argv, string(part))
			}
		}
		cwd, _ := os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid))
		out = append(out, liveProcess{pid: pid, cmdline: argv, cwd: cwd})
	}
	return out
}

// activeSessionKeys is the set of sessionEntryKey values a live Claude process
// appears to be using right now. It is derived entirely from the running system
// — /proc — and never stored anywhere, so nothing can go stale: close the
// terminal, crash, or pull the plug and the process is gone, which is exactly
// what makes the session inactive again. There is no "active" flag on disk to
// clean up, by construction.
//
// Exact when Claude Code itself says so: a process whose command line names
// --resume/--session-id ID is writing that transcript. A session started fresh
// names nothing yet, so the newest transcript in that process's own working
// directory is treated as active instead — deliberately conservative, because
// hiding one deletable session costs a click, while deleting a live
// conversation cannot be undone.
func activeSessionKeys(s *store, c config) map[string]bool {
	active := map[string]bool{}
	for email := range c.Accounts {
		for _, proc := range liveProcessesForProfile(s.profile(email)) {
			if id := sessionIDFromCmdline(proc.cmdline, "--resume", "-r", "--session-id"); id != "" {
				if dirName, err := findSessionDir(s, email, id); err == nil {
					active[sessionEntryKey(sessionEntry{email: email, dirName: dirName, sessionID: id})] = true
				}
				continue
			}
			if proc.cwd == "" {
				continue
			}
			dirName := projectDirName(proc.cwd)
			if id := newestSessionID(s, email, dirName); id != "" {
				active[sessionEntryKey(sessionEntry{email: email, dirName: dirName, sessionID: id})] = true
			}
		}
	}
	return active
}

// newestSessionID is the most recently modified transcript in one project
// directory, or "" when there is none — the conservative fallback for a live
// process that does not name the conversation it is writing.
func newestSessionID(s *store, email, dirName string) string {
	entries, err := os.ReadDir(filepath.Join(s.profile(email), "projects", dirName))
	if err != nil {
		return ""
	}
	newest, newestTime := "", time.Time{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(newestTime) {
			newest, newestTime = strings.TrimSuffix(name, ".jsonl"), info.ModTime()
		}
	}
	return newest
}

// deletableSessions splits recorded sessions into the ones a delete may touch
// and the ones a live Claude process is using. The delete picker lists only the
// first, so choosing "delete" never offers something that would fail; the CLI
// keeps the second as an explicit refusal, since a selector you typed cannot be
// hidden from you.
func deletableSessions(s *store, c config, entries []sessionEntry) (deletable, active []sessionEntry) {
	inUse := activeSessionKeys(s, c)
	for _, e := range entries {
		if inUse[sessionEntryKey(e)] {
			active = append(active, e)
			continue
		}
		deletable = append(deletable, e)
	}
	return deletable, active
}

// deleteSession is deleteSessions for the single-session case, kept because one
// transcript is the feature's natural unit (and the tests read better for it).
func deleteSession(s *store, e sessionEntry) error {
	_, err := deleteSessions(s, []sessionEntry{e})
	return err
}

// deleteConfirmMatches is the single-session guard's comparison, deliberately
// the same shape as removeConfirmMatches (decision 0042): the typed answer must
// be the session's full ID, or the short head…tail form the list and the picker
// actually put on screen. An exact-match rule on the displayed form alone would
// reject a paste of the full ID from `session list`; requiring the full 36
// characters alone would demand retyping something the UI never showed in full.
func deleteConfirmMatches(answer, sessionID string) bool {
	return answer == sessionID || answer == shortSessionID(sessionID)
}

// bulkDeleteConfirmation is what a multi-session deletion asks you to type
// (decision 0052). Retyping one ID does not scale to twenty, and a bare Enter
// would be too easy to hit by reflex, so the deliberate act becomes typing this
// literal word — the same "restate intent, don't just confirm" spirit as the
// single-session guard and `remove`'s retype, sized to a bulk action.
const bulkDeleteConfirmation = "delete"

// bulkDeleteConfirmMatches reports whether answer restates the bulk intent.
func bulkDeleteConfirmMatches(answer string) bool {
	return strings.EqualFold(strings.TrimSpace(answer), bulkDeleteConfirmation)
}

// projectDisplayPath best-effort reverses projectDirName for display:
// cpro's own encoding (every "/" becomes "-") is lossy whenever a real path
// segment contains a literal "-" of its own, so this is a readable
// approximation, not a guaranteed-exact original path — good enough to help
// identify a session by, never used to actually locate one on disk (dirName
// itself, unchanged, is what every real file lookup still uses).
func projectDisplayPath(dirName string) string {
	return strings.ReplaceAll(dirName, "-", "/")
}

// projectDisplayName is the last path segment of projectDisplayPath — a
// short label for a session row, falling back to the full decoded path if it
// has no meaningful last segment (e.g. "/").
func projectDisplayName(dirName string) string {
	path := projectDisplayPath(dirName)
	name := filepath.Base(path)
	if name == "" || name == "." || name == "/" {
		return path
	}
	return name
}

// shortSessionID shortens a full session ID (a UUID) to a head…tail form for
// display ("26074c99…4a27da" style). Every actual lookup/execution path
// (continueSession's own --resume argument) keeps using the full,
// untruncated sessionEntry.sessionID — only the displayed representation is
// ever shortened.
func shortSessionID(id string) string {
	const head, tail = 8, 6
	r := []rune(id)
	if len(r) <= head+tail+1 {
		return id
	}
	return string(r[:head]) + "…" + string(r[len(r)-tail:])
}

// resumeSession implements cpro's own root-level `--resume SESSION_ID`/
// `-r SESSION_ID` (main.go's "__resume" hidden dispatch command): the
// native-style equivalent of `claude --resume SESSION_ID`, refactored to end
// in the exact same s.run (claude.go) cpro run/cpro session continue already
// call — so all three share one permission resolver
// (applyPermissionDefaults) and one Claude command builder, never a second,
// resume-specific implementation (decision 0032). It never fakes a resume:
// discovery only locates whose profile already holds sessionID's transcript
// (findSessionOwner, reusing listSessions — the same data source the
// interactive session picker, sessionui.go, already reads), migration only
// ever copies — never moves — that one transcript into a different profile
// when required (migrateSession), and the actual resume is always s.run.
//
// account, when non-empty, is an explicit --account override: resume/
// migrate unconditionally under that account. Empty means automatic
// resolution — use whichever account already owns the session, falling back
// to the interactive RESUME ACCOUNT picker (resumeui.go) only if that owning
// account turns out to be unusable (signed out, invalid session). Neither
// path ever writes config.Default — this is execution-time-only account
// resolution, exactly like cpro run's own --account, never a change to the
// user's configured default.
//
// extra is forwarded to Claude alongside --resume SESSION_ID (e.g. a
// trailing prompt), the same convention cpro session continue's own
// forwarded arguments already follow.
//
// Full unification with continueSession's own FROM_EMAIL/TO_EMAIL-shaped
// entry point is deliberately out of scope here: the two already share their
// real building blocks (findSessionDir/listSessions, the copy-and-verify
// discipline, and s.run itself) — see decision 0032 for why forcing one
// literal call signature onto both isn't warranted by this task alone.
func resumeSession(cmd *cobra.Command, s *store, sessionID, account string, extra []string) error {
	c, err := s.read()
	if err != nil {
		return err
	}
	ownerEmail, dirName, err := findSessionOwner(s, c, sessionID)
	if err != nil {
		return err
	}

	target := account
	if target == "" {
		target = ownerEmail
		if auth, authErr := authStatus(s.profile(ownerEmail)); authErr != nil || !validAuth(ownerEmail, auth) {
			picked, err := pickResumeAccount(cmd, s, c, ownerEmail)
			if err != nil {
				return err
			}
			target = picked
		}
	} else if !c.Accounts[target] {
		return missingAccount(target)
	}

	if target != ownerEmail {
		if err := migrateSession(s, ownerEmail, target, dirName, sessionID); err != nil {
			return err
		}
	}

	restoreSessionDirectory(s, ownerEmail, dirName, sessionID)

	forwarded := append([]string{"--resume", sessionID}, extra...)
	return s.run(target, forwarded)
}

// findSessionOwner locates which registered account's own profile holds
// sessionID's transcript, reusing listSessions rather than a second
// directory-scanning implementation. listSessions sorts newest-first, so if
// a session was already migrated to more than one account by an earlier
// --resume/session continue, the most recently modified copy wins rather
// than this erroring on the (harmless) duplicate.
func findSessionOwner(s *store, c config, sessionID string) (email, dirName string, err error) {
	for _, entry := range listSessions(s, c) {
		if entry.sessionID == sessionID {
			return entry.email, entry.dirName, nil
		}
	}
	return "", "", fmt.Errorf("no cpro-managed session found with ID %s", sessionID)
}

// migrateSession copies exactly the one session transcript sessionID needs —
// never the rest of that project directory's other sessions, unlike
// continueSession, which intentionally copies every session recorded for a
// whole directory at once for its own "hand off an entire conversation
// history" use case — from fromEmail's profile into toEmail's, preserving
// the same projects/<dirName>/<sessionID>.jsonl layout Claude Code itself
// expects, then verifies the copy actually landed. Skips (never overwrites)
// an already-present destination file, and never touches or removes the
// source: copy-and-verify, not move, so a failed or aborted resume can never
// destroy the original session.
func migrateSession(s *store, fromEmail, toEmail, dirName, sessionID string) error {
	srcPath := filepath.Join(s.profile(fromEmail), "projects", dirName, sessionID+".jsonl")
	data, err := os.ReadFile(srcPath)
	if err != nil {
		return fmt.Errorf("could not read session %s under %s: %w", sessionID, fromEmail, err)
	}

	lock, err := sessionCopyLock(s, toEmail)
	if err != nil {
		return err
	}
	defer lock.Close()

	dstDir := filepath.Join(s.profile(toEmail), "projects", dirName)
	if err := privateDir(dstDir); err != nil {
		return err
	}
	dstPath := filepath.Join(dstDir, sessionID+".jsonl")
	if _, err := os.Stat(dstPath); errors.Is(err, os.ErrNotExist) {
		if err := atomicWrite(dstPath, data); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if _, err := os.Stat(dstPath); err != nil {
		return fmt.Errorf("session migration did not produce a discoverable copy under %s: %w", toEmail, err)
	}
	return nil
}

// restoreSessionDirectory best-effort chdirs to the working directory
// sessionID's own transcript recorded (its "cwd" field, read directly via
// sessionWorkingDirectory rather than reversing projectDirName's lossy
// "/" -> "-" encoding — see projectDisplayPath's own doc comment for why
// that reversal can't be trusted as exact), matching the task's own
// requirement that Claude's project/session resolution isn't silently
// changed by resuming from an unrelated directory. A missing/unreadable
// transcript, a transcript with no recorded cwd, or a recorded directory
// that no longer exists on disk are all left as a diagnostic on stderr,
// never a hard failure: falling back to resuming from wherever cpro itself
// was invoked is the same acceptable behavior cpro session continue already
// relies on (it never chdirs at all).
func restoreSessionDirectory(s *store, ownerEmail, dirName, sessionID string) {
	path := filepath.Join(s.profile(ownerEmail), "projects", dirName, sessionID+".jsonl")
	dir, ok := sessionWorkingDirectory(path)
	if !ok {
		dir = projectDisplayPath(dirName)
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		fmt.Fprintf(os.Stderr, "cpro: original working directory %s is no longer available; resuming from the current directory\n", dir)
		return
	}
	if err := os.Chdir(dir); err != nil {
		fmt.Fprintf(os.Stderr, "cpro: could not switch to original working directory %s: %v\n", dir, err)
	}
}

// sessionWorkingDirectory reads a session transcript's own recorded "cwd"
// field — the exact working directory Claude Code logged for that
// conversation — from the first line that carries one. Precise even when
// projectDirName's own "/" -> "-" encoding would be ambiguous to reverse
// (projectDisplayPath's own documented limitation); ("", false) if no line
// in the transcript carries a cwd at all, a non-fatal case callers fall back
// from rather than treat as an error.
func sessionWorkingDirectory(path string) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		var rec struct {
			Cwd string `json:"cwd"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &rec); err == nil && rec.Cwd != "" {
			return rec.Cwd, true
		}
	}
	return "", false
}

func newSessionCommand() *cobra.Command {
	session := &cobra.Command{
		Use:   "session",
		Short: "Manage Claude sessions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !terminalInput() || !terminalOutput(cmd.ErrOrStderr()) {
				return cmd.Help()
			}
			s, err := openStore()
			if err != nil {
				return err
			}
			c, err := s.read()
			if err != nil {
				return err
			}
			picked, _, err := runSessionUI(cmd, s, c, false)
			if err != nil {
				return err
			}
			if picked == nil {
				return nil
			}
			root := cmd.Root()
			root.SetArgs(picked)
			return root.Execute()
		},
	}
	session.AddCommand(&cobra.Command{
		Use:                "continue [FROM_EMAIL TO_EMAIL] [--] [CLAUDE ARGUMENTS...]",
		Short:              "Move a Claude session to another account and resume it",
		DisableFlagParsing: true,
		Long: "Copy every Claude session transcript recorded for the current directory from " +
			"FROM_EMAIL's cpro profile into TO_EMAIL's (adding only; nothing is deleted or " +
			"overwritten), then immediately run Claude under TO_EMAIL with --resume so you " +
			"pick the exact conversation from Claude's own list, previews included. Meant " +
			"for picking up a conversation that hit FROM_EMAIL's usage limit under a " +
			"different account without guessing a session ID by hand.\n" +
			"Any arguments after FROM_EMAIL and TO_EMAIL (optionally after a --) are forwarded " +
			"to Claude alongside --resume, e.g. --dangerously-skip-permissions. If the forwarded " +
			"arguments already include --resume SESSION_ID, only that one session is copied (from " +
			"whichever directory it was recorded in) and Claude is started in that directory, " +
			"resuming it directly instead of showing its own list.\n" +
			"Called with no arguments at all (from a terminal), this opens an interactive picker " +
			"instead (decision 0025): pick a recorded session, then a destination account, and " +
			"the exact same copy-and-resume flow above runs with --resume SESSION_ID already " +
			"filled in — FROM_EMAIL/TO_EMAIL stay required for this non-interactive form either way.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
				return cmd.Help()
			}
			if len(args) == 0 {
				if !terminalInput() || !terminalOutput(cmd.ErrOrStderr()) {
					return fmt.Errorf("picking a session interactively requires a terminal; run cpro session continue FROM_EMAIL TO_EMAIL instead")
				}
				s, err := openStore()
				if err != nil {
					return err
				}
				c, err := s.read()
				if err != nil {
					return err
				}
				picked, _, err := runSessionContinueUI(cmd, s, c, false)
				if err != nil {
					return err
				}
				if picked == nil {
					return nil
				}
				root := cmd.Root()
				root.SetArgs(picked)
				return root.Execute()
			}
			if len(args) < 2 {
				return fmt.Errorf("accepts 2 args, received %d", len(args))
			}
			from, err := normalizeEmail(args[0])
			if err != nil {
				return err
			}
			to, err := normalizeEmail(args[1])
			if err != nil {
				return err
			}
			extra := args[2:]
			if len(extra) > 0 && extra[0] == "--" {
				extra = extra[1:]
			}
			s, err := openStore()
			if err != nil {
				return err
			}
			copied, err := continueSession(s, from, to, resumeSessionIDArg(extra))
			if err != nil {
				return err
			}
			cmd.Println(accent(cmd.OutOrStdout(), fmt.Sprintf("Copied %d session file(s) to %s", copied, to), accentMode))
			// A named session resumes from the directory it was recorded in,
			// exactly as `cpro --resume` does — Claude looks a session up
			// by the working directory it is started from.
			if id := resumeSessionIDArg(extra); id != "" {
				if dirName, err := findSessionDir(s, from, id); err == nil {
					restoreSessionDirectory(s, from, dirName, id)
				}
			}
			forwarded := extra
			if !hasArg(extra, "--resume") {
				forwarded = append([]string{"--resume"}, extra...)
			}
			return s.run(to, forwarded)
		},
	})
	var sessionListJSON bool
	sessionList := &cobra.Command{
		Use:   "list",
		Short: "List every recorded Claude session across accounts",
		Long:  "List every recorded Claude session transcript across all registered accounts, newest first by last activity. Read-only: it copies nothing and never runs Claude. The same data backs the interactive SESSIONS LIST screen (cpro session, then list) and the continue picker; this is its scriptable, plain-text/--json form (decision 0044).",
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
			entries := listSessions(s, c)
			if sessionListJSON {
				type jsonSession struct {
					SessionID    string    `json:"sessionId"`
					Account      string    `json:"account"`
					Project      string    `json:"project"`
					Directory    string    `json:"directory"`
					LastActivity time.Time `json:"lastActivity"`
				}
				sessions := make([]jsonSession, 0, len(entries))
				for _, e := range entries {
					sessions = append(sessions, jsonSession{
						SessionID:    e.sessionID,
						Account:      e.email,
						Project:      projectDisplayName(e.dirName),
						Directory:    projectDisplayPath(e.dirName),
						LastActivity: e.modTime,
					})
				}
				return json.NewEncoder(cmd.OutOrStdout()).Encode(struct {
					Version  int           `json:"version"`
					Sessions []jsonSession `json:"sessions"`
				}{1, sessions})
			}
			if len(entries) == 0 {
				cmd.Println("No sessions found")
				return nil
			}
			for _, e := range entries {
				cmd.Println(sessionListLine(e))
			}
			return nil
		},
	}
	sessionList.Flags().BoolVar(&sessionListJSON, "json", false, "JSON output")
	session.AddCommand(sessionList)
	var (
		sessionDeleteYes     bool
		sessionDeleteAccount string
	)
	sessionDelete := &cobra.Command{
		Use:   "delete SESSION_ID [SESSION_ID...]",
		Short: "Permanently delete recorded Claude session transcripts",
		Long: "Permanently delete one or more recorded session transcripts, each identified by its full " +
			"session ID or an unambiguous prefix of one (the ID is the first column of `cpro session list`). " +
			"`--account EMAIL` restricts every plain selector to one account, which is also how to " +
			"disambiguate the same session ID recorded under two accounts by an earlier `cpro session " +
			"continue`; a selector may instead name its own account as EMAIL:SESSION_ID, which is what the " +
			"interactive picker's multi-select emits so one invocation can delete a mixed-account batch.\n" +
			"Deletion is irreversible, so it is guarded the way `cpro remove` guards an account: with a " +
			"terminal, one session makes you retype its ID and several make you type \"" + bulkDeleteConfirmation + "\"; " +
			"without one the command refuses unless --yes is passed. The interactive SESSIONS screen's own " +
			"\"delete\" row runs this same command — Space/Tab checks a row, Ctrl+A checks every visible one — " +
			"and renders the same confirmation in-app (decisions 0051/0052).",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openStore()
			if err != nil {
				return err
			}
			c, err := s.read()
			if err != nil {
				return err
			}
			account := ""
			if sessionDeleteAccount != "" {
				account, err = normalizeEmail(sessionDeleteAccount)
				if err != nil {
					return err
				}
				if !c.Accounts[account] {
					return missingAccount(account)
				}
			}
			entries, err := resolveSessions(s, c, args, account)
			if err != nil {
				return err
			}
			// Refused before the confirmation prompt, not after: typing a
			// session ID back to confirm a delete that is going to be turned
			// away anyway is a pointless trap (deleteSessions would refuse it a
			// second time regardless — this is the same guard, moved to where
			// the user can still do something else).
			if _, active := deletableSessions(s, c, entries); len(active) > 0 {
				return activeSessionsError(active)
			}
			if !sessionDeleteYes {
				if !terminalInput() || !terminalOutput(cmd.ErrOrStderr()) {
					return fmt.Errorf("deleting sessions requires confirmation; use --yes to delete them non-interactively")
				}
				var answer string
				if len(entries) == 1 {
					e := entries[0]
					fmt.Fprintf(os.Stderr, "Delete session %s (%s, %s). Type the session ID to confirm: ",
						shortSessionID(e.sessionID), projectDisplayPath(e.dirName), displayEmail(e.email))
					if _, err := fmt.Fscanln(os.Stdin, &answer); err != nil || !deleteConfirmMatches(answer, e.sessionID) {
						return fmt.Errorf("deletion cancelled")
					}
				} else {
					fmt.Fprintf(os.Stderr, "Delete %d sessions. Type %q to confirm: ", len(entries), bulkDeleteConfirmation)
					if _, err := fmt.Fscanln(os.Stdin, &answer); err != nil || !bulkDeleteConfirmMatches(answer) {
						return fmt.Errorf("deletion cancelled")
					}
				}
			}
			deleted, deleteErr := deleteSessions(s, entries)
			out := cmd.OutOrStdout()
			// "Deleted session X" only when that one session was really removed:
			// a skip (an entry that vanished, an account in use) already carries
			// its own error line, and claiming success over it would be a lie.
			if len(entries) == 1 && deleted == 1 {
				e := entries[0]
				cmd.Println(accent(out,
					fmt.Sprintf("Deleted session %s (%s, %s)", shortSessionID(e.sessionID), projectDisplayPath(e.dirName), displayEmail(e.email)), accentMode))
			} else {
				cmd.Println(accent(out, fmt.Sprintf("Deleted %d of %d sessions", deleted, len(entries)), accentMode))
			}
			return deleteErr
		},
	}
	sessionDelete.Flags().BoolVar(&sessionDeleteYes, "yes", false, "delete without the confirmation prompt")
	sessionDelete.Flags().StringVar(&sessionDeleteAccount, "account", "", "restrict plain selectors to sessions recorded under this account")
	session.AddCommand(sessionDelete)
	return session
}

// sessionListLine is `cpro session list`'s plain, one-line-per-session
// rendering. The full session ID comes first so a line can be pasted straight
// into `cpro --resume ID`; the owning account goes through displayEmail
// (decision 0024), so a masked install doesn't print real addresses here
// either. `--json` is the machine-readable form and stays unmasked, like every
// other cpro --json output.
func sessionListLine(e sessionEntry) string {
	return e.sessionID + "  " + projectDisplayPath(e.dirName) + "  " + displayEmail(e.email) + "  " + formatDuration(time.Since(e.modTime)) + " ago"
}
