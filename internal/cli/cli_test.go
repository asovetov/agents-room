package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/asovetov/agents-room/internal/hub"
	"github.com/asovetov/agents-room/internal/store"
)

// setup points the store at a temporary file and returns a runner that
// captures stdout and stderr.
func setup(t *testing.T) func(args ...string) (int, string, string) {
	t.Helper()
	t.Setenv("AGENTS_ROOM_DB", filepath.Join(t.TempDir(), "room.db"))
	return func(args ...string) (int, string, string) {
		var out, errOut bytes.Buffer
		code := Run(args, &out, &errOut)
		return code, out.String(), errOut.String()
	}
}

func TestJoinPrintsSessionID(t *testing.T) {
	run := setup(t)
	code, out, errOut := run("join", "--room", "repo:x", "--label", "a", "--key", "k1")
	if code != 0 {
		t.Fatalf("join exit %d, stderr %s", code, errOut)
	}
	id := strings.TrimSpace(out)
	if len(id) != 16 {
		t.Fatalf("session id = %q, want 16 hex characters", id)
	}
}

func TestJoinWithSameKeyIsIdempotent(t *testing.T) {
	run := setup(t)
	_, first, _ := run("join", "--room", "repo:x", "--key", "k1")
	_, second, _ := run("join", "--room", "repo:x", "--key", "k1")
	if strings.TrimSpace(first) != strings.TrimSpace(second) {
		t.Fatalf("second join returned a different id: %q != %q", first, second)
	}

	code, out, _ := run("who", "--room", "repo:x", "--json")
	if code != 0 {
		t.Fatalf("who exit %d", code)
	}
	var sessions []map[string]any
	if err := json.Unmarshal([]byte(out), &sessions); err != nil {
		t.Fatalf("who output is not JSON: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("who lists %d sessions, want 1", len(sessions))
	}
}

func TestSendAndRecv(t *testing.T) {
	run := setup(t)
	_, aOut, _ := run("join", "--room", "repo:x", "--key", "a")
	_, bOut, _ := run("join", "--room", "repo:x", "--key", "b")
	a, b := strings.TrimSpace(aOut), strings.TrimSpace(bOut)

	code, _, errOut := run("send", "--session", a, "--to", b, "--subject", "s", "--body", "hello")
	if code != 0 {
		t.Fatalf("send exit %d: %s", code, errOut)
	}

	code, out, errOut := run("recv", "--session", b)
	if code != 0 {
		t.Fatalf("recv exit %d: %s", code, errOut)
	}
	if !strings.Contains(out, "hello") {
		t.Fatalf("recv output %q does not contain the body", out)
	}

	// A second recv has nothing to return.
	_, out, _ = run("recv", "--session", b)
	if !strings.Contains(out, "no unread") {
		t.Fatalf("second recv = %q, want an empty result", out)
	}
}

func TestLeaseClaimDeniedExitsNonZero(t *testing.T) {
	run := setup(t)
	_, aOut, _ := run("join", "--room", "repo:x", "--key", "a")
	_, bOut, _ := run("join", "--room", "repo:x", "--key", "b")
	a, b := strings.TrimSpace(aOut), strings.TrimSpace(bOut)

	if code, _, errOut := run("lease", "claim", "--session", a, "--path", "src/x.go"); code != 0 {
		t.Fatalf("first claim exit %d: %s", code, errOut)
	}

	code, out, _ := run("lease", "claim", "--session", b, "--path", "src/x.go")
	if code == 0 {
		t.Fatal("second claim exited 0, want a denial code")
	}
	if !strings.Contains(out, "denied") {
		t.Fatalf("output %q does not report the denial", out)
	}
}

func TestCapCommand(t *testing.T) {
	run := setup(t)

	code, out, _ := run("cap", "--room", "repo:x", "--json")
	if code != 0 {
		t.Fatalf("read cap exit %d", code)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("cap output is not JSON: %v", err)
	}
	if got["autonomy"].(float64) != 2 {
		t.Fatalf("default cap = %v, want 2", got["autonomy"])
	}

	if code, _, errOut := run("cap", "--room", "repo:x", "--set", "1"); code != 0 {
		t.Fatalf("set cap exit %d: %s", code, errOut)
	}
	_, out, _ = run("cap", "--room", "repo:x", "--json")
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if got["autonomy"].(float64) != 1 {
		t.Fatalf("cap after set = %v, want 1", got["autonomy"])
	}

	if code, _, _ := run("cap", "--room", "repo:x", "--set", "9"); code == 0 {
		t.Fatal("invalid cap accepted")
	}
}

func TestDriftNoteAndCheck(t *testing.T) {
	run := setup(t)
	_, aOut, _ := run("join", "--room", "repo:x", "--key", "a")
	a := strings.TrimSpace(aOut)

	path := filepath.Join(t.TempDir(), "f.go")
	if err := writeFile(path, "v1"); err != nil {
		t.Fatal(err)
	}

	if code, _, errOut := run("drift", "note", "--session", a, "--path", path); code != 0 {
		t.Fatalf("note exit %d: %s", code, errOut)
	}
	code, out, _ := run("drift", "check", "--session", a, "--path", path)
	if code != 0 || !strings.Contains(out, "SAFE") {
		t.Fatalf("check = %q (exit %d), want SAFE", out, code)
	}

	if err := writeFile(path, "v2"); err != nil {
		t.Fatal(err)
	}
	_, out, _ = run("drift", "check", "--session", a, "--path", path)
	if !strings.Contains(out, "MINOR") {
		t.Fatalf("check = %q, want MINOR", out)
	}
}

func TestUnknownCommand(t *testing.T) {
	run := setup(t)
	code, _, errOut := run("nonsense")
	if code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	if !strings.Contains(errOut, "unknown command") {
		t.Fatalf("stderr = %q", errOut)
	}
}

func TestPathCommand(t *testing.T) {
	run := setup(t)
	code, out, _ := run("path")
	if code != 0 {
		t.Fatalf("path exit %d", code)
	}
	if !strings.HasSuffix(strings.TrimSpace(out), "room.db") {
		t.Fatalf("path = %q", out)
	}
}

// A send whose selector matches nobody still succeeds, but it must say so on
// stderr rather than look like a delivery.
func TestSendWarnsWhenNobodyMatches(t *testing.T) {
	run := setup(t)
	_, aOut, _ := run("join", "--room", "repo:x", "--key", "a")
	a := strings.TrimSpace(aOut)

	code, _, errOut := run("send", "--session", a, "--to", "tag:nobody", "--body", "x")
	if code != 0 {
		t.Fatalf("send exit %d: %s", code, errOut)
	}
	if !strings.Contains(errOut, "matched no recipient") {
		t.Fatalf("no warning for a message nobody can read; stderr = %q", errOut)
	}
}

// doctor has to name a room that exists both with and without the shared:
// prefix, because a session in the wrong one is unreachable from the right
// one and nothing merges them automatically.
func TestDoctorReportsSplitSharedRoom(t *testing.T) {
	run := setup(t)
	run("join", "--room", "repo:x", "--also", "shared:storefront", "--key", "a")

	// Join normalises the prefix, so the split can only be built by hand the
	// way the old binary left it behind: a bare room beside the shared one,
	// holding a session that never reached the shared room.
	db, err := store.Open(os.Getenv("AGENTS_ROOM_DB"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.SQL().ExecContext(context.Background(), q, args...); err != nil {
			t.Fatalf("exec: %v", err)
		}
	}
	exec(`INSERT INTO rooms (id, name, created_at) VALUES ('bare1', 'storefront', 1)`)
	exec(`INSERT INTO sessions (id, room_id, harness, cwd, pid, label, tags, autonomy,
	                            joined_at, last_seen, harness_key)
	      VALUES ('stranded', 'bare1', 'test', '/tmp', 1, 'stranded', '[]', 1, 1, 1, '')`)
	exec(`INSERT INTO session_rooms (session_id, room_id, joined_at)
	      VALUES ('stranded', 'bare1', 1)`)

	_, out, _ := run("doctor")
	if !strings.Contains(out, "storefront and shared:storefront are two rooms") {
		t.Fatalf("doctor did not report the split room; output:\n%s", out)
	}
	if !strings.Contains(out, "1 session(s)") {
		t.Fatalf("doctor counted the reachable session as stranded; output:\n%s", out)
	}
}

// One process registered twice is one chat counted twice. doctor has to say
// so, because room_who cannot: it has no way to know two ids are one chat.
func TestDoctorReportsDuplicatePID(t *testing.T) {
	run := setup(t)
	// A pid is what makes the two rows one chat, and join records none when
	// no harness is up the process chain (CI has none). Name one the way a
	// harness does, so the test does not depend on where it runs.
	t.Setenv("AGENTS_ROOM_PID", "4242")
	run("join", "--room", "repo:x", "--key", "a")

	// The second join is what the old room_join did without a harness key.
	db, err := store.Open(os.Getenv("AGENTS_ROOM_DB"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	exec := func(q string) {
		t.Helper()
		if _, err := db.SQL().ExecContext(context.Background(), q); err != nil {
			t.Fatalf("exec %s: %v", q, err)
		}
	}
	exec(`
		INSERT INTO sessions (id, room_id, harness, cwd, pid, label, tags, autonomy,
		                      joined_at, last_seen, harness_key)
		SELECT 'dup1', room_id, harness, cwd, pid, label, tags, autonomy,
		       joined_at, last_seen, '' FROM sessions LIMIT 1`)
	// The duplicate has to be in the room as well, or it is not a chat that
	// room_who could have counted twice.
	exec(`
		INSERT INTO session_rooms (session_id, room_id, joined_at)
		SELECT 'dup1', room_id, joined_at FROM session_rooms
		WHERE session_id = (SELECT id FROM sessions LIMIT 1)`)

	_, out, _ := run("doctor")
	if !strings.Contains(out, "has 2 sessions") {
		t.Fatalf("doctor did not report the duplicate pid; output:\n%s", out)
	}
}

// writeFile is a small helper so the drift test does not import os twice.
func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}

// A lease that lapses under a session that is still working is the case the
// TTL is meant to prevent. doctor has to say so, because nothing else can see
// it coming, and the usual cause is that the harness is not calling the hook.
func TestDoctorReportsExpiringLease(t *testing.T) {
	run := setup(t)
	// doctor reads the current repository room, so the session joins that
	// one rather than a made-up name.
	room := hub.RepoRoom("")
	t.Setenv("AGENTS_ROOM_PID", "4242")
	_, out, _ := run("join", "--room", room, "--key", "a")
	id := strings.TrimSpace(out)
	run("lease", "claim", "--session", id, "--path", "src/x.go", "--ttl", "100")

	// With most of the TTL left there is nothing to report yet.
	_, clean, _ := run("doctor")
	if strings.Contains(clean, "lease ends in") {
		t.Fatalf("doctor warned about a lease with most of its life left:\n%s", clean)
	}

	// Move the expiry close to now, the way a long edit would leave it, and
	// doctor must name it.
	db, err := store.Open(os.Getenv("AGENTS_ROOM_DB"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	if _, err := db.SQL().ExecContext(context.Background(),
		`UPDATE leases SET acquired_at = ?, expires_at = ?`,
		time.Now().Add(-10*time.Minute).Unix(), time.Now().Add(60*time.Second).Unix()); err != nil {
		t.Fatalf("age the lease: %v", err)
	}

	_, aged, _ := run("doctor")
	if !strings.Contains(aged, "lease ends in") {
		t.Fatalf("doctor did not report a lease about to lapse:\n%s", aged)
	}
	// The stored path is absolute, so the separator differs by platform and
	// matching the literal "src/x.go" passes on Linux by luck only.
	if !strings.Contains(aged, filepath.Join("src", "x.go")) {
		t.Fatalf("doctor named the warning but not the path:\n%s", aged)
	}
}

// The session id is the one thing a script parses from join, so a warning
// goes to stderr and leaves stdout as the bare id.
func TestJoinWarningGoesToStderr(t *testing.T) {
	run := setup(t)
	t.Setenv("CLAUDE_CODE_SESSION_ID", "mine")

	code, out, errOut := run("join", "--room", "repo:x", "--key", "someone-else")
	if code != 0 {
		t.Fatalf("join exit %d: %s", code, errOut)
	}
	if len(strings.TrimSpace(out)) != 16 {
		t.Fatalf("stdout = %q, want only the session id", out)
	}
	if !strings.Contains(errOut, "someone-else") {
		t.Fatalf("stderr = %q, want the warning", errOut)
	}
}

// A sender had no way to learn what became of a message after send returned.
// `sent` is that read side: what went out, and how far it got.
func TestSentReportsWhatBecameOfAMessage(t *testing.T) {
	run := setup(t)
	t.Setenv("AGENTS_ROOM_PID", "4242")
	_, aOut, _ := run("join", "--room", "repo:x", "--key", "a")
	_, bOut, _ := run("join", "--room", "repo:x", "--key", "b")
	_, cOut, _ := run("join", "--room", "repo:x", "--key", "c")
	a, b, c := strings.TrimSpace(aOut), strings.TrimSpace(bOut), strings.TrimSpace(cOut)

	run("send", "--session", a, "--to", b, "--subject", "one", "--body", "x")
	run("send", "--session", a, "--to", c, "--subject", "two", "--body", "y")

	// b reads its message, c does not.
	run("recv", "--session", b)

	code, out, errOut := run("sent", "--session", a, "--json")
	if code != 0 {
		t.Fatalf("sent exit %d: %s", code, errOut)
	}
	var sent []map[string]any
	if err := json.Unmarshal([]byte(out), &sent); err != nil {
		t.Fatalf("sent output is not JSON: %v\n%s", err, out)
	}
	if len(sent) != 2 {
		t.Fatalf("sent lists %d messages, want 2", len(sent))
	}

	bySubject := map[string]map[string]any{}
	for _, s := range sent {
		bySubject[s["subject"].(string)] = s
	}
	// Newest first.
	if sent[0]["subject"] != "two" {
		t.Fatalf("sent order = %v, want newest first", sent[0]["subject"])
	}
	if states := bySubject["one"]["states"].(map[string]any); states["read"] != float64(1) {
		t.Fatalf("message one states = %v, want one read", states)
	}
	if p := bySubject["one"]["pending"]; p != float64(0) {
		t.Fatalf("message one pending = %v, want 0", p)
	}
	if states := bySubject["two"]["states"].(map[string]any); states["stored"] != float64(1) {
		t.Fatalf("message two states = %v, want one stored", states)
	}
	if p := bySubject["two"]["pending"]; p != float64(1) {
		t.Fatalf("message two pending = %v, want 1", p)
	}
}

// A message nobody was matched to is still visible to its sender, and says so
// plainly rather than reporting a recipient.
func TestSentShowsAMessageWithNoRecipients(t *testing.T) {
	run := setup(t)
	t.Setenv("AGENTS_ROOM_PID", "4242")
	_, aOut, _ := run("join", "--room", "repo:x", "--key", "a")
	a := strings.TrimSpace(aOut)

	run("send", "--session", a, "--to", "tag:nobody", "--subject", "lost", "--body", "x")

	code, out, errOut := run("sent", "--session", a)
	if code != 0 {
		t.Fatalf("sent exit %d: %s", code, errOut)
	}
	if !strings.Contains(out, "no recipients") {
		t.Fatalf("sent did not say the message reached nobody:\n%s", out)
	}
}

// doctor --fix removes the sessions that registered one chat more than once.
// It is the one repair that deletes rows, so it does nothing without --fix,
// and --dry-run shows the plan without touching the store.
func TestDoctorFixRemovesDuplicateSessions(t *testing.T) {
	run := setup(t)
	t.Setenv("AGENTS_ROOM_PID", "4242")
	_, out, _ := run("join", "--room", "repo:x", "--key", "a")
	keep := strings.TrimSpace(out)

	// A second row on the same pid, the way the old room_join left one.
	db, err := store.Open(os.Getenv("AGENTS_ROOM_DB"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	exec := func(q string) {
		t.Helper()
		if _, err := db.SQL().ExecContext(context.Background(), q); err != nil {
			t.Fatalf("exec: %v", err)
		}
	}
	exec(`INSERT INTO sessions (id, room_id, harness, cwd, pid, label, tags, autonomy,
	                            joined_at, last_seen, harness_key)
	      SELECT 'dup1', room_id, harness, cwd, pid, label, tags, autonomy,
	             joined_at, last_seen, '' FROM sessions LIMIT 1`)
	exec(`INSERT INTO session_rooms (session_id, room_id, joined_at)
	      SELECT 'dup1', room_id, joined_at FROM session_rooms WHERE session_id = '` + keep + `'`)

	// Without --fix nothing changes.
	_, plain, _ := run("doctor")
	if strings.Contains(plain, "removed ") {
		t.Fatalf("doctor removed a session without --fix:\n%s", plain)
	}
	if !strings.Contains(plain, "--fix") {
		t.Fatalf("doctor did not offer --fix:\n%s", plain)
	}

	// --dry-run shows the plan and keeps both rows.
	_, dry, _ := run("doctor", "--fix", "--dry-run")
	if !strings.Contains(dry, "keep "+keep) {
		t.Fatalf("dry run did not name the session it keeps:\n%s", dry)
	}
	if !strings.Contains(dry, "dup1") {
		t.Fatalf("dry run did not name the session it removes:\n%s", dry)
	}
	if strings.Contains(dry, "removed ") {
		t.Fatalf("dry run changed the store:\n%s", dry)
	}

	// --fix applies it, and the surviving session is the one with the key.
	_, fixed, _ := run("doctor", "--fix")
	if !strings.Contains(fixed, "removed 1 duplicate") {
		t.Fatalf("--fix did not report the removal:\n%s", fixed)
	}
	_, who, _ := run("who", "--room", "repo:x")
	if strings.Contains(who, "dup1") {
		t.Fatalf("dup1 survived --fix:\n%s", who)
	}
	if !strings.Contains(who, keep) {
		t.Fatalf("--fix removed the session it should keep:\n%s", who)
	}
}

// who has to say whether the chat behind a session is still running.
//
// last_seen cannot answer that: a chat thinking through a long edit and a
// chat that exited look identical in "seen 40m ago". The difference decides
// whether a file it holds is really held.
func TestWhoShowsLiveness(t *testing.T) {
	run := setup(t)
	// A pid that is this test process: alive, and its start time is recorded.
	t.Setenv("AGENTS_ROOM_PID", strconv.Itoa(os.Getpid()))
	_, out, _ := run("join", "--room", "repo:x", "--key", "alive")
	id := strings.TrimSpace(out)

	code, who, errOut := run("who", "--room", "repo:x")
	if code != 0 {
		t.Fatalf("who exit %d: %s", code, errOut)
	}
	if !strings.Contains(who, "STATE") {
		t.Fatalf("who has no liveness column:\n%s", who)
	}
	line := ""
	for _, l := range strings.Split(who, "\n") {
		if strings.HasPrefix(l, id) {
			line = l
		}
	}
	if line == "" {
		t.Fatalf("who did not list %s:\n%s", id, who)
	}
	if !strings.Contains(line, hub.LivenessAlive) {
		t.Fatalf("a running session is not shown as alive:\n%s", line)
	}
}

// A session whose chat exited is gone even though the row is recent. The old
// table said nothing at all, which read as "present".
func TestWhoShowsGoneSession(t *testing.T) {
	run := setup(t)
	// A pid the OS has just retired. A made-up number is not safe: 4242 was
	// running on the macOS runner, and the test then asserted the opposite of
	// what it had built.
	t.Setenv("AGENTS_ROOM_PID", strconv.Itoa(retiredPID(t)))
	_, out, _ := run("join", "--room", "repo:x", "--key", "gone")
	id := strings.TrimSpace(out)

	// The row records no start time, so the pid alone has to decide: gone.
	db, err := store.Open(os.Getenv("AGENTS_ROOM_DB"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	if _, err := db.SQL().ExecContext(context.Background(),
		`UPDATE sessions SET pid_started_at = 0 WHERE id = ?`, id); err != nil {
		t.Fatalf("exec: %v", err)
	}

	_, who, _ := run("who", "--room", "repo:x")
	for _, l := range strings.Split(who, "\n") {
		if strings.HasPrefix(l, id) && !strings.Contains(l, hub.LivenessGone) {
			t.Fatalf("a session whose process is gone is not shown as gone:\n%s", l)
		}
	}
}

// doctor has to name the rows nobody will ever come back to, and name the
// command that sweeps them. Nothing else in doctor can see them: every other
// check reads the store, and the store cannot tell dead from thinking.
func TestDoctorReportsGoneSessions(t *testing.T) {
	run := setup(t)
	t.Setenv("AGENTS_ROOM_PID", strconv.Itoa(retiredPID(t)))
	run("join", "--room", hub.RepoRoom(""), "--key", "gone")

	_, out, _ := run("doctor")
	if !strings.Contains(out, "whose chat is no longer running") {
		t.Fatalf("doctor did not report the gone session:\n%s", out)
	}
	if !strings.Contains(out, "prune") {
		t.Fatalf("doctor named the problem without naming the repair:\n%s", out)
	}
}

// retiredPID returns a pid that belongs to no running process.
//
// A hard-coded number is not safe: 4242 happened to be running on the macOS
// runner, and a test that asserts "gone" then asserts the opposite of what it
// built. Starting a process and letting it exit gives a number the OS has
// just retired.
func retiredPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Skipf("cannot start a helper process: %v", err)
	}
	return cmd.Process.Pid
}

// A live session that has reported no tool call for hours is running without
// the hook: its leases lapse and wake signals reach nobody. Every symptom
// looks like a room problem, so doctor has to name the real cause.
func TestDoctorReportsALiveSessionWithNoHook(t *testing.T) {
	run := setup(t)
	t.Setenv("AGENTS_ROOM_PID", strconv.Itoa(os.Getpid()))
	room := hub.RepoRoom("")
	_, out, _ := run("join", "--room", room, "--key", "quiet")
	id := strings.TrimSpace(out)

	db, err := store.Open(os.Getenv("AGENTS_ROOM_DB"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	// Alive, but last seen three hours ago.
	if _, err := db.SQL().ExecContext(context.Background(),
		`UPDATE sessions SET last_seen = ? WHERE id = ?`, time.Now().Unix()-3*3600, id); err != nil {
		t.Fatalf("exec: %v", err)
	}

	_, report, _ := run("doctor")
	if !strings.Contains(report, "reported no tool call") {
		t.Fatalf("doctor did not notice the missing hook:\n%s", report)
	}
}

// A quiet session that is also gone is a different finding; the hook warning
// must not fire for it, or every dead row looks like a broken install.
func TestDoctorDoesNotBlameTheHookForAGoneSession(t *testing.T) {
	run := setup(t)
	t.Setenv("AGENTS_ROOM_PID", strconv.Itoa(retiredPID(t)))
	room := hub.RepoRoom("")
	_, out, _ := run("join", "--room", room, "--key", "gone")
	id := strings.TrimSpace(out)

	db, err := store.Open(os.Getenv("AGENTS_ROOM_DB"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	if _, err := db.SQL().ExecContext(context.Background(),
		`UPDATE sessions SET last_seen = ?, pid_started_at = 0 WHERE id = ?`,
		time.Now().Unix()-3*3600, id); err != nil {
		t.Fatalf("exec: %v", err)
	}

	_, report, _ := run("doctor")
	if strings.Contains(report, "reported no tool call") {
		t.Fatalf("doctor blamed the hook for a session whose chat is gone:\n%s", report)
	}
}

// history is the command a newcomer runs to learn what the room decided.
func TestHistoryCommandShowsEarlierMessages(t *testing.T) {
	run := setup(t)
	t.Setenv("AGENTS_ROOM_PID", "4242")
	_, out, _ := run("join", "--room", "repo:x", "--key", "a")
	id := strings.TrimSpace(out)
	// A direct send to the sender's own session is not the point; what the
	// test needs is a message filed in the room. * reaches everyone else in
	// it, and with one session that is nobody, so the message is stored with
	// zero recipients -- which is exactly the case history has to show.
	run("send", "--session", id, "--to", "*", "--subject", "the api needs a phone field")

	code, hist, errOut := run("history", "--room", "repo:x")
	if code != 0 {
		t.Fatalf("history exit %d: %s", code, errOut)
	}
	if !strings.Contains(hist, "the api needs a phone field") {
		t.Fatalf("history did not show the message:\n%s", hist)
	}
}

func TestHistoryCommandOnAnEmptyRoom(t *testing.T) {
	run := setup(t)
	code, out, _ := run("history", "--room", "repo:nothing")
	if code != 0 {
		t.Fatalf("history exit %d", code)
	}
	if !strings.Contains(out, "no messages") {
		t.Fatalf("empty room did not say so:\n%s", out)
	}
}
