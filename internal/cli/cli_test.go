package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	run("join", "--room", "repo:x", "--key", "a")

	// The second join is what the old room_join did without a harness key.
	db, err := store.Open(os.Getenv("AGENTS_ROOM_DB"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	if _, err := db.SQL().ExecContext(context.Background(), `
		INSERT INTO sessions (id, room_id, harness, cwd, pid, label, tags, autonomy,
		                      joined_at, last_seen, harness_key)
		SELECT 'dup1', room_id, harness, cwd, pid, label, tags, autonomy,
		       joined_at, last_seen, '' FROM sessions LIMIT 1`); err != nil {
		t.Fatalf("insert duplicate session: %v", err)
	}

	_, out, _ := run("doctor")
	if !strings.Contains(out, "has 2 sessions") {
		t.Fatalf("doctor did not report the duplicate pid; output:\n%s", out)
	}
}

// writeFile is a small helper so the drift test does not import os twice.
func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}
