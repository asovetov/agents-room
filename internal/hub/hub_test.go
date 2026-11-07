package hub

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/asovetov/agents-room/internal/store"
)

// nowFn aliases the store clock so tests can move time forward without
// sleeping. It is restored by each test that replaces it.
func newHub(t *testing.T) *Hub {
	t.Helper()
	h, err := OpenPath(filepath.Join(t.TempDir(), "room.db"))
	if err != nil {
		t.Fatalf("open hub: %v", err)
	}
	t.Cleanup(func() { h.Close() })
	return h
}

// join registers a session in a room and fails the test on error.
func join(t *testing.T, h *Hub, room, label string, tags ...string) *Session {
	t.Helper()
	s, err := h.Join(context.Background(), JoinOptions{
		Room: room, Harness: "test", Label: label, Tags: tags, Autonomy: AutonomyWake,
	})
	if err != nil {
		t.Fatalf("join %s: %v", label, err)
	}
	return s
}

func TestSendDirectAndBroadcast(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)
	a := join(t, h, "repo:x", "a")
	b := join(t, h, "repo:x", "b")
	c := join(t, h, "repo:x", "c")

	if _, err := h.Send(ctx, SendOptions{From: a.ID, To: b.ID, Subject: "hi", Body: "body"}); err != nil {
		t.Fatalf("send direct: %v", err)
	}
	got, err := h.Recv(ctx, b.ID, 0)
	if err != nil {
		t.Fatalf("recv b: %v", err)
	}
	if len(got) != 1 || got[0].Subject != "hi" {
		t.Fatalf("b got %d messages, want 1", len(got))
	}
	if got[0].State != StateRead {
		t.Fatalf("state after recv = %q, want %q", got[0].State, StateRead)
	}

	// A broadcast reaches b and c but never the sender.
	if _, err := h.Send(ctx, SendOptions{From: a.ID, To: "*", Subject: "all", Body: "x"}); err != nil {
		t.Fatalf("send broadcast: %v", err)
	}
	for _, s := range []*Session{a, b, c} {
		n, err := h.Unread(ctx, s.ID)
		if err != nil {
			t.Fatalf("unread %s: %v", s.Label, err)
		}
		want := 1
		if s.ID == a.ID {
			want = 0
		}
		if n != want {
			t.Errorf("%s unread = %d, want %d", s.Label, n, want)
		}
	}
}

func TestSendToFileSelectorReachesLeaseHolder(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)
	a := join(t, h, "repo:x", "a")
	b := join(t, h, "repo:x", "b")
	_ = join(t, h, "repo:x", "c")

	if _, err := h.Claim(ctx, b.ID, "src/api/users.go", 60, ModeExclusive); err != nil {
		t.Fatalf("claim: %v", err)
	}

	if _, err := h.Send(ctx, SendOptions{
		From: a.ID, To: "file:src/api/**", Subject: "protocol", Body: "see note",
	}); err != nil {
		t.Fatalf("send: %v", err)
	}

	n, err := h.Unread(ctx, b.ID)
	if err != nil {
		t.Fatalf("unread b: %v", err)
	}
	if n != 1 {
		t.Fatalf("lease holder unread = %d, want 1", n)
	}
}

func TestSendToTagSelector(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)
	a := join(t, h, "repo:x", "a")
	b := join(t, h, "repo:x", "b", "protocol")
	c := join(t, h, "repo:x", "c", "ui")

	if _, err := h.Send(ctx, SendOptions{
		From: a.ID, To: "tag:protocol", Subject: "s", Body: "b",
	}); err != nil {
		t.Fatalf("send: %v", err)
	}
	for _, tc := range []struct {
		s    *Session
		want int
	}{{b, 1}, {c, 0}} {
		n, err := h.Unread(ctx, tc.s.ID)
		if err != nil {
			t.Fatalf("unread: %v", err)
		}
		if n != tc.want {
			t.Errorf("%s unread = %d, want %d", tc.s.Label, n, tc.want)
		}
	}
}

func TestSendUnknownSessionFails(t *testing.T) {
	h := newHub(t)
	a := join(t, h, "repo:x", "a")
	if _, err := h.Send(context.Background(), SendOptions{
		From: a.ID, To: "nope", Subject: "s", Body: "b",
	}); err == nil {
		t.Fatal("send to unknown session succeeded, want error")
	}
}

func TestExclusiveLeaseIsExclusive(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)
	a := join(t, h, "repo:x", "a")
	b := join(t, h, "repo:x", "b")

	first, err := h.Claim(ctx, a.ID, "src/wire.go", 60, ModeExclusive)
	if err != nil {
		t.Fatalf("claim a: %v", err)
	}
	if !first.OK {
		t.Fatal("first claim failed")
	}

	second, err := h.Claim(ctx, b.ID, "src/wire.go", 60, ModeExclusive)
	if err != nil {
		t.Fatalf("claim b: %v", err)
	}
	if second.OK {
		t.Fatal("second exclusive claim succeeded, want denied")
	}
	if second.Holder == nil || second.Holder.Owner != a.ID {
		t.Fatalf("holder = %+v, want session a", second.Holder)
	}
}

func TestSharedLeasesCoexist(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)
	a := join(t, h, "repo:x", "a")
	b := join(t, h, "repo:x", "b")

	if r, _ := h.Claim(ctx, a.ID, "docs/api.md", 60, ModeShared); !r.OK {
		t.Fatal("first shared claim failed")
	}
	if r, _ := h.Claim(ctx, b.ID, "docs/api.md", 60, ModeShared); !r.OK {
		t.Fatal("second shared claim failed")
	}
	// An exclusive claim on the same path must still fail.
	if r, _ := h.Claim(ctx, b.ID, "docs/api.md", 60, ModeExclusive); r.OK {
		t.Fatal("exclusive claim over shared leases succeeded, want denied")
	}
}

func TestExpiredLeaseIsReclaimable(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)
	a := join(t, h, "repo:x", "a")
	b := join(t, h, "repo:x", "b")

	if r, _ := h.Claim(ctx, a.ID, "src/wire.go", 1, ModeExclusive); !r.OK {
		t.Fatal("claim a failed")
	}
	// Move the clock past the lease without sleeping.
	old := store.Now
	t.Cleanup(func() { h.setClock(old) })
	h.setClock(func() int64 { return old() + 10 })

	r, err := h.Claim(ctx, b.ID, "src/wire.go", 60, ModeExclusive)
	if err != nil {
		t.Fatalf("claim b: %v", err)
	}
	if !r.OK {
		t.Fatalf("claim after expiry failed: %+v", r)
	}
}

func TestConcurrentClaimsHaveOneWinner(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)

	const n = 8
	sessions := make([]*Session, n)
	for i := range sessions {
		sessions[i] = join(t, h, "repo:x", "s")
	}

	var wg sync.WaitGroup
	results := make([]bool, n)
	for i := range sessions {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r, err := h.Claim(ctx, sessions[i].ID, "src/hot.go", 60, ModeExclusive)
			if err != nil {
				t.Errorf("claim %d: %v", i, err)
				return
			}
			results[i] = r.OK
		}(i)
	}
	wg.Wait()

	winners := 0
	for _, ok := range results {
		if ok {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("winners = %d, want exactly 1", winners)
	}
}

func TestLeaveReleasesLeases(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)
	a := join(t, h, "repo:x", "a")
	b := join(t, h, "repo:x", "b")

	if r, _ := h.Claim(ctx, a.ID, "src/wire.go", 60, ModeExclusive); !r.OK {
		t.Fatal("claim failed")
	}
	if err := h.Leave(ctx, a.ID); err != nil {
		t.Fatalf("leave: %v", err)
	}
	r, err := h.Claim(ctx, b.ID, "src/wire.go", 60, ModeExclusive)
	if err != nil {
		t.Fatalf("claim b: %v", err)
	}
	if !r.OK {
		t.Fatal("lease survived the holder leaving")
	}
}

// deliveries.session_id has no foreign key, so a session's deliveries are
// not removed with it. Left behind they are counted as unread by every hook
// event forever, and the session that would have fetched them is gone.
func TestLeaveDropsTheSessionsDeliveries(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)
	a := join(t, h, "repo:x", "a")
	b := join(t, h, "repo:x", "b")

	if _, err := h.Send(ctx, SendOptions{From: a.ID, To: b.ID, Subject: "s", Body: "b"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if n, err := h.Unread(ctx, b.ID); err != nil || n != 1 {
		t.Fatalf("unread before leave = %d (%v), want 1", n, err)
	}

	if err := h.Leave(ctx, b.ID); err != nil {
		t.Fatalf("leave: %v", err)
	}

	var left int
	if err := h.db.SQL().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM deliveries WHERE session_id = ?`, b.ID).Scan(&left); err != nil {
		t.Fatalf("count deliveries: %v", err)
	}
	if left != 0 {
		t.Fatalf("%d deliveries survived the session, want 0", left)
	}

	// The message itself stays: a is still a participant and may want to
	// know what it sent.
	var messages int
	if err := h.db.SQL().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM messages WHERE sender = ?`, a.ID).Scan(&messages); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if messages != 1 {
		t.Fatalf("%d messages survived, want the sent one to stay", messages)
	}
}

func TestExpiredMessageIsNotDelivered(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)
	a := join(t, h, "repo:x", "a")
	b := join(t, h, "repo:x", "b")

	if _, err := h.Send(ctx, SendOptions{
		From: a.ID, To: b.ID, Subject: "s", Body: "b", TTL: 1,
	}); err != nil {
		t.Fatalf("send: %v", err)
	}

	old := store.Now
	t.Cleanup(func() { h.setClock(old) })
	h.setClock(func() int64 { return old() + 10 })

	got, err := h.Recv(ctx, b.ID, 0)
	if err != nil {
		t.Fatalf("recv: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("received %d expired messages, want 0", len(got))
	}
}

func TestAckMovesStateForward(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)
	a := join(t, h, "repo:x", "a")
	b := join(t, h, "repo:x", "b")

	m, err := h.Send(ctx, SendOptions{From: a.ID, To: b.ID, Subject: "s", Body: "b"})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if _, err := h.Recv(ctx, b.ID, 0); err != nil {
		t.Fatalf("recv: %v", err)
	}
	if err := h.Ack(ctx, b.ID, m.ID); err != nil {
		t.Fatalf("ack: %v", err)
	}
	// Acking twice is harmless, but acking from the wrong session fails.
	if err := h.Ack(ctx, a.ID, m.ID); err == nil {
		t.Fatal("ack from the sender succeeded, want error")
	}
}

func TestDriftSeverity(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)
	a := join(t, h, "repo:x", "a")
	b := join(t, h, "repo:x", "b")

	path := filepath.Join(t.TempDir(), "wire.go")
	if err := os.WriteFile(path, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := h.NoteRead(ctx, a.ID, path); err != nil {
		t.Fatalf("note read: %v", err)
	}
	r, err := h.DriftCheck(ctx, a.ID, path)
	if err != nil {
		t.Fatalf("drift: %v", err)
	}
	if r.Severity != DriftSafe {
		t.Fatalf("unchanged file severity = %q, want SAFE", r.Severity)
	}

	// Someone edits the file, nobody holds a lease: MINOR.
	if err := os.WriteFile(path, []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err = h.DriftCheck(ctx, a.ID, path)
	if err != nil {
		t.Fatalf("drift: %v", err)
	}
	if r.Severity != DriftMinor {
		t.Fatalf("severity = %q, want MINOR", r.Severity)
	}

	// b holds a lease and edits again: CRITICAL for a.
	abs, _ := filepath.Abs(path)
	if res, _ := h.Claim(ctx, b.ID, abs, 60, ModeExclusive); !res.OK {
		t.Fatal("claim b failed")
	}
	if err := os.WriteFile(path, []byte("v3"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err = h.DriftCheck(ctx, a.ID, path)
	if err != nil {
		t.Fatalf("drift: %v", err)
	}
	if r.Severity != DriftCritical || r.Holder != b.ID {
		t.Fatalf("severity = %q holder = %q, want CRITICAL and b", r.Severity, r.Holder)
	}
}

func TestDriftWithoutBaselineIsSafe(t *testing.T) {
	h := newHub(t)
	a := join(t, h, "repo:x", "a")
	path := filepath.Join(t.TempDir(), "x.go")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := h.DriftCheck(context.Background(), a.ID, path)
	if err != nil {
		t.Fatalf("drift: %v", err)
	}
	if r.Severity != DriftSafe || r.Changed {
		t.Fatalf("got %+v, want SAFE without baseline", r)
	}
}

func TestJoinClampsAutonomy(t *testing.T) {
	h := newHub(t)
	s, err := h.Join(context.Background(), JoinOptions{
		Room: "repo:x", Harness: "test", Autonomy: 99,
	})
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	if s.Autonomy > AutonomyWake {
		t.Fatalf("autonomy = %d, want <= %d", s.Autonomy, AutonomyWake)
	}
	if _, err := h.Join(context.Background(), JoinOptions{
		Room: "repo:x", Harness: "test", Autonomy: -1,
	}); err == nil {
		t.Fatal("negative autonomy accepted, want error")
	}
}

func TestWhoListsRoomSessions(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)
	join(t, h, "repo:x", "a")
	join(t, h, "repo:x", "b")
	join(t, h, "repo:y", "c")

	got, err := h.Who(ctx, "repo:x")
	if err != nil {
		t.Fatalf("who: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("who returned %d sessions, want 2", len(got))
	}
	for _, s := range got {
		if len(s.Rooms) > 0 && strings.HasPrefix(s.Rooms[0], "repo:y") {
			t.Fatalf("session from another room leaked: %+v", s)
		}
	}
}

func TestRepoRoomName(t *testing.T) {
	if got := RepoRoom("/home/user/proj/thing"); got != "repo:thing" {
		t.Fatalf("RepoRoom = %q, want repo:thing", got)
	}
}

func TestGlobMatch(t *testing.T) {
	cases := []struct {
		pattern, path string
		want          bool
	}{
		{"src/wire.go", "src/wire.go", true},
		{"src/*.go", "src/wire.go", true},
		{"src/*.go", "src/sub/wire.go", false},
		{"src/**", "src/sub/wire.go", true},
		{"src/**", "src", true},
		{"src/**", "other/wire.go", false},
		// A selector must never match across a directory separator on any
		// platform. filepath.Match gets this wrong on Windows, where a
		// backslash is the separator and "*" spans "/".
		{"src/*.go", "src/sub/wire.go", false},
		{`src\*.go`, "src/sub/wire.go", false},
		{`src\*.go`, "src/wire.go", true},
	}
	for _, c := range cases {
		if got := globMatch(c.pattern, c.path); got != c.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", c.pattern, c.path, got, c.want)
		}
	}
}

func TestJoinWithHarnessKeyIsIdempotent(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)

	first, err := h.Join(ctx, JoinOptions{
		Room: "repo:x", Harness: "claude-code", HarnessKey: "abc-123", Label: "a",
	})
	if err != nil {
		t.Fatalf("first join: %v", err)
	}
	second, err := h.Join(ctx, JoinOptions{
		Room: "repo:x", Harness: "claude-code", HarnessKey: "abc-123", Label: "a",
	})
	if err != nil {
		t.Fatalf("second join: %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("rejoin created a new session: %s != %s", first.ID, second.ID)
	}
	sessions, err := h.Who(ctx, "repo:x")
	if err != nil {
		t.Fatalf("who: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("room has %d sessions after two joins, want 1", len(sessions))
	}
}

func TestJoinDifferentHarnessKeysAreDifferentSessions(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)
	if _, err := h.Join(ctx, JoinOptions{Room: "repo:x", Harness: "claude-code", HarnessKey: "one"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Join(ctx, JoinOptions{Room: "repo:x", Harness: "claude-code", HarnessKey: "two"}); err != nil {
		t.Fatal(err)
	}
	sessions, _ := h.Who(ctx, "repo:x")
	if len(sessions) != 2 {
		t.Fatalf("got %d sessions, want 2", len(sessions))
	}
}

func TestJoinWithoutHarnessKeyAlwaysCreates(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)
	a, _ := h.Join(ctx, JoinOptions{Room: "repo:x", Harness: "manual"})
	b, _ := h.Join(ctx, JoinOptions{Room: "repo:x", Harness: "manual"})
	if a.ID == b.ID {
		t.Fatal("joins without a harness key were deduplicated")
	}
}

func TestRoomAutonomyCapClampsJoin(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)

	if _, err := h.SetRoomAutonomyCap(ctx, "repo:x", AutonomyHold); err != nil {
		t.Fatalf("set cap: %v", err)
	}
	s, err := h.Join(ctx, JoinOptions{Room: "repo:x", Harness: "test", Autonomy: AutonomyWake})
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	if s.Autonomy != AutonomyHold {
		t.Fatalf("autonomy = %d, want %d clamped by the room cap", s.Autonomy, AutonomyHold)
	}
}

func TestRoomAutonomyCapRejectsBadValue(t *testing.T) {
	h := newHub(t)
	if _, err := h.SetRoomAutonomyCap(context.Background(), "repo:x", 7); err == nil {
		t.Fatal("cap 7 accepted, want error")
	}
}

func TestRoomAutonomyCapDefaultsToWake(t *testing.T) {
	h := newHub(t)
	cap, err := h.RoomAutonomyCap(context.Background(), "repo:missing")
	if err != nil {
		t.Fatalf("cap: %v", err)
	}
	if cap != AutonomyWake {
		t.Fatalf("default cap = %d, want %d", cap, AutonomyWake)
	}
}

func TestFindSession(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)
	joined, err := h.Join(ctx, JoinOptions{
		Room: "repo:x", Harness: "claude-code", HarnessKey: "key-1", Label: "labelled",
	})
	if err != nil {
		t.Fatal(err)
	}
	found, err := h.FindSession(ctx, "repo:x", "claude-code", "key-1")
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if found == nil || found.ID != joined.ID {
		t.Fatalf("found %+v, want %s", found, joined.ID)
	}
	missing, err := h.FindSession(ctx, "repo:x", "claude-code", "nope")
	if err != nil {
		t.Fatalf("find missing: %v", err)
	}
	if missing != nil {
		t.Fatalf("found %+v for an unknown key, want nil", missing)
	}
}

// A lease is meant to cover the edit it guards, and an edit can outlast the
// TTL. RenewAll is what the hook calls on every tool event, so a session that
// is doing anything keeps what it holds without naming each path.
func TestRenewAllExtendsEveryLiveLease(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)
	a := join(t, h, "repo:x", "a")
	b := join(t, h, "repo:x", "b")

	for _, p := range []string{"src/a.go", "src/b.go"} {
		if r, _ := h.Claim(ctx, a.ID, p, 60, ModeExclusive); !r.OK {
			t.Fatalf("claim %s failed", p)
		}
	}
	// Another session's lease must not be touched.
	if r, _ := h.Claim(ctx, b.ID, "src/c.go", 60, ModeExclusive); !r.OK {
		t.Fatal("claim c failed")
	}

	// List returns the whole room, so pick this session's leases by owner.
	owned := func(sessionID string) map[string]int64 {
		t.Helper()
		all, err := h.List(ctx, sessionID)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		out := map[string]int64{}
		for _, l := range all {
			if l.Owner == sessionID {
				out[l.Path] = l.ExpiresAt
			}
		}
		return out
	}

	beforeA := owned(a.ID)
	beforeB := owned(b.ID)
	if len(beforeA) != 2 || len(beforeB) != 1 {
		t.Fatalf("before: a holds %d, b holds %d", len(beforeA), len(beforeB))
	}

	n, err := h.RenewAll(ctx, a.ID, 3600)
	if err != nil {
		t.Fatalf("renew all: %v", err)
	}
	if n != 2 {
		t.Fatalf("renewed %d leases, want 2", n)
	}

	afterA := owned(a.ID)
	afterB := owned(b.ID)
	for path, was := range beforeA {
		if afterA[path] <= was {
			t.Fatalf("%s: expiry did not move (%d -> %d)", path, was, afterA[path])
		}
	}
	// The stored path is absolute, so the key is looked up by its base name
	// rather than by the relative path it was claimed with.
	if len(beforeB) != 1 || len(afterB) != 1 {
		t.Fatalf("b: %d leases before, %d after", len(beforeB), len(afterB))
	}
	for path, was := range beforeB {
		if afterB[path] != was {
			t.Fatalf("RenewAll extended another session's lease on %s (%d -> %d)", path, was, afterB[path])
		}
	}
}

// A lease that already lapsed is not taken back: someone else may hold the
// path by now, and renewing a dead claim would steal it.
func TestRenewAllDoesNotReviveAnExpiredLease(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)
	a := join(t, h, "repo:x", "a")
	b := join(t, h, "repo:x", "b")

	if r, _ := h.Claim(ctx, a.ID, "src/x.go", 1, ModeExclusive); !r.OK {
		t.Fatal("claim failed")
	}
	old := h.now
	t.Cleanup(func() { h.setClock(old) })
	h.setClock(func() int64 { return old() + 3600 })

	if r, _ := h.Claim(ctx, b.ID, "src/x.go", 600, ModeExclusive); !r.OK {
		t.Fatal("b could not take the lapsed path")
	}
	n, err := h.RenewAll(ctx, a.ID, 600)
	if err != nil {
		t.Fatalf("renew all: %v", err)
	}
	if n != 0 {
		t.Fatalf("renewed %d lapsed leases, want 0", n)
	}
	holder, err := h.Check(ctx, b.ID, "src/x.go")
	if err != nil || holder == nil || holder.Owner != b.ID {
		t.Fatalf("holder = %+v (%v), want b", holder, err)
	}
}

// Passing another chat's harness key silently adopts its session: the join
// succeeds, and the caller answers messages addressed to someone else. That
// is a mistake worth naming, but not one worth refusing a join over.
func TestJoinWarnsAboutAForeignHarnessKey(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)
	t.Setenv("CLAUDE_CODE_SESSION_ID", "mine")

	s, err := h.Join(ctx, JoinOptions{
		Room: "repo:x", Harness: "claude-code", HarnessKey: "someone-else",
	})
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	if s.Warning == "" {
		t.Fatal("a foreign harness key joined without a warning")
	}
	if !strings.Contains(s.Warning, "someone-else") {
		t.Fatalf("warning = %q, want the key it names", s.Warning)
	}

	// The right key, and a caller with no harness in its environment, are
	// both silent: the warning must not cry wolf on the normal path.
	ok, err := h.Join(ctx, JoinOptions{Room: "repo:x", Harness: "claude-code", HarnessKey: "mine"})
	if err != nil {
		t.Fatalf("join with the right key: %v", err)
	}
	if ok.Warning != "" {
		t.Fatalf("the right key warned: %q", ok.Warning)
	}
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	none, err := h.Join(ctx, JoinOptions{Room: "repo:x", Harness: "test", HarnessKey: "x"})
	if err != nil {
		t.Fatalf("join without an env key: %v", err)
	}
	if none.Warning != "" {
		t.Fatalf("a harness with no session id in the environment warned: %q", none.Warning)
	}
}

// The rule a person applies by hand: keep the session the harness keeps
// rejoining, because that is the one whose id survives.
func TestPlanDuplicateFixKeepsTheHarnessKey(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)

	keyed, err := h.Join(ctx, JoinOptions{
		Room: "repo:x", Harness: "claude-code", HarnessKey: "k", PID: 4242,
	})
	if err != nil {
		t.Fatal(err)
	}
	// An older row on the same pid, the way the old room_join left one.
	if _, err := h.db.SQL().ExecContext(ctx, `
		INSERT INTO sessions (id, room_id, harness, cwd, pid, label, tags, autonomy,
		                      joined_at, last_seen, harness_key)
		SELECT 'older', room_id, harness, cwd, pid, label, tags, autonomy,
		       joined_at - 100, last_seen, '' FROM sessions WHERE id = ?`, keyed.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.SQL().ExecContext(ctx, `
		INSERT INTO session_rooms (session_id, room_id, joined_at)
		SELECT 'older', room_id, joined_at FROM session_rooms WHERE session_id = ?`, keyed.ID); err != nil {
		t.Fatal(err)
	}

	plan, err := h.PlanDuplicateFix(ctx)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(plan) != 1 {
		t.Fatalf("plan has %d fixes, want 1", len(plan))
	}
	if plan[0].Keep != keyed.ID {
		t.Fatalf("keep = %q, want the keyed session %q", plan[0].Keep, keyed.ID)
	}
	if len(plan[0].Remove) != 1 || plan[0].Remove[0] != "older" {
		t.Fatalf("remove = %v, want [older]", plan[0].Remove)
	}

	// Planning must not change anything.
	if _, err := h.sessionByHarnessKey(ctx, "claude-code", "k"); err != nil {
		t.Fatalf("planning removed the keyed session: %v", err)
	}
	var n int
	if err := h.db.SQL().QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions WHERE id = 'older'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatal("planning removed a session")
	}
}

// With no key anywhere, the oldest row survives: it has been in the room
// longest, and other agents already have its id.
func TestPlanDuplicateFixKeepsTheOldestWithoutKeys(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)

	first, err := h.Join(ctx, JoinOptions{Room: "repo:x", Harness: "test", PID: 4242})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.SQL().ExecContext(ctx, `
		INSERT INTO sessions (id, room_id, harness, cwd, pid, label, tags, autonomy,
		                      joined_at, last_seen, harness_key)
		SELECT 'newer', room_id, harness, cwd, pid, label, tags, autonomy,
		       joined_at + 100, last_seen, '' FROM sessions WHERE id = ?`, first.ID); err != nil {
		t.Fatal(err)
	}

	plan, err := h.PlanDuplicateFix(ctx)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(plan) != 1 || plan[0].Keep != first.ID {
		t.Fatalf("plan = %+v, want to keep the older %q", plan, first.ID)
	}
}

// Two rows for one chat do not always share a pid.
//
// The row minted by room_join records the pid of the MCP server that answered
// it, and that process exits with the chat. The row the hook made carries the
// harness key and records the chat's own pid. Grouping by pid never sees the
// pair, so the chat is counted twice in every room it is in and doctor says
// nothing.
//
// The ghost has no key of its own, so what ties it back is the key of the
// process it names: while that process lived it carried the chat's session id
// in its environment. The test process stands in for it.
func TestDuplicatePIDsFindsRowsThatShareAHarnessKey(t *testing.T) {
	if !hasProcFS {
		t.Skip("process environments are read from /proc on this path")
	}
	ctx := context.Background()
	h := newHub(t)

	live, err := h.Join(ctx, JoinOptions{
		Room: "repo:x", Harness: "claude-code", HarnessKey: "chat", PID: 4242,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The ghost: same chat, no key, and the pid of the process that answered
	// the join.
	if _, err := h.db.SQL().ExecContext(ctx, `
		INSERT INTO sessions (id, room_id, harness, cwd, pid, pid_started_at, label,
		                      tags, autonomy, joined_at, last_seen, harness_key)
		SELECT 'ghost', room_id, harness, cwd, ?, 0, label, tags, autonomy,
		       joined_at + 60, joined_at + 60, '' FROM sessions WHERE id = ?`,
		standInChat(t, "chat"), live.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.SQL().ExecContext(ctx, `
		INSERT INTO session_rooms (session_id, room_id, joined_at)
		SELECT 'ghost', room_id, joined_at FROM session_rooms WHERE session_id = ?`,
		live.ID); err != nil {
		t.Fatal(err)
	}

	dups := h.DuplicatePIDs(ctx)
	if len(dups) != 1 {
		t.Fatalf("found %d duplicates, want 1: the two rows are one chat", len(dups))
	}
	if dups[0].Kind != "harness_key" {
		t.Fatalf("grouped by %q, want harness_key", dups[0].Kind)
	}
	if len(dups[0].IDs) != 2 {
		t.Fatalf("group has %d ids, want 2: %v", len(dups[0].IDs), dups[0].IDs)
	}
}

// When the rows are collapsed, the messages the doomed row still holds have to
// reach the session that survives. The row was a real member of the room: a
// delivery deleted with it is a message nobody ever read.
func TestApplyDuplicateFixMovesDeliveriesToTheKeptSession(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)

	if !hasProcFS {
		t.Skip("process environments are read from /proc on this path")
	}
	sender := join(t, h, "repo:x", "sender")
	keep, err := h.Join(ctx, JoinOptions{
		Room: "repo:x", Harness: "claude-code", HarnessKey: "chat", PID: 4242,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.SQL().ExecContext(ctx, `
		INSERT INTO sessions (id, room_id, harness, cwd, pid, pid_started_at, label,
		                      tags, autonomy, joined_at, last_seen, harness_key)
		SELECT 'ghost', room_id, harness, cwd, ?, 0, label, tags, autonomy,
		       joined_at, joined_at, '' FROM sessions WHERE id = ?`,
		standInChat(t, "chat"), keep.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.SQL().ExecContext(ctx, `
		INSERT INTO session_rooms (session_id, room_id, joined_at)
		SELECT 'ghost', room_id, joined_at FROM session_rooms WHERE session_id = ?`, keep.ID); err != nil {
		t.Fatal(err)
	}

	// One message that reaches both rows, one that only the ghost got. The
	// second is the delivery a plain delete would throw away.
	if _, err := h.Send(ctx, SendOptions{From: sender.ID, To: "*", Subject: "both", Body: "x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Send(ctx, SendOptions{From: sender.ID, To: "ghost", Subject: "ghost only", Body: "y"}); err != nil {
		t.Fatal(err)
	}
	// A direct send to the ghost does not reach the kept row, which is the
	// delivery that would be lost by a plain delete.
	if n, _ := h.Unread(ctx, keep.ID); n != 1 {
		t.Fatalf("kept row unread = %d before the fix, want 1", n)
	}

	plan, err := h.PlanDuplicateFix(ctx)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(plan) != 1 {
		t.Fatalf("plan has %d fixes, want 1", len(plan))
	}
	if _, err := h.ApplyDuplicateFix(ctx, plan); err != nil {
		t.Fatalf("apply: %v", err)
	}

	got, err := h.Recv(ctx, keep.ID, 0)
	if err != nil {
		t.Fatalf("recv: %v", err)
	}
	subjects := map[string]bool{}
	for _, m := range got {
		subjects[m.Subject] = true
	}
	if !subjects["both"] || !subjects["ghost only"] {
		t.Fatalf("kept session received %v, want both messages", subjects)
	}
	var orphans int
	if err := h.db.SQL().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM deliveries WHERE session_id = 'ghost'`).Scan(&orphans); err != nil {
		t.Fatal(err)
	}
	if orphans != 0 {
		t.Fatalf("ghost row still holds %d deliveries", orphans)
	}
}

// A recipient that leaves with the message unread is not a recipient that is
// still waiting. Its delivery is gone, so counting only the rows on disk says
// "nobody read it yet" about a session that no longer exists.
func TestSentReportsRecipientsThatLeft(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)

	sender := join(t, h, "repo:x", "sender")
	stay := join(t, h, "repo:x", "stay")
	leave := join(t, h, "repo:x", "leave")

	msg, err := h.Send(ctx, SendOptions{From: sender.ID, To: "*", Subject: "hi", Body: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if msg.Recipients != 2 {
		t.Fatalf("recipients = %d, want 2", msg.Recipients)
	}
	if _, err := h.Recv(ctx, stay.ID, 0); err != nil {
		t.Fatal(err)
	}
	if err := h.Leave(ctx, leave.ID); err != nil {
		t.Fatal(err)
	}

	sent, err := h.SentMessages(ctx, sender.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(sent) != 1 {
		t.Fatalf("sent has %d messages, want 1", len(sent))
	}
	got := sent[0]
	if got.Sent != 2 {
		t.Fatalf("sent to %d recipients, want the 2 recorded at send time", got.Sent)
	}
	if got.Left != 1 {
		t.Fatalf("left = %d, want 1: one recipient walked out without reading", got.Left)
	}
	if got.Pending != 0 {
		t.Fatalf("pending = %d, want 0: nobody is still being waited on", got.Pending)
	}
	if got.Recipients != 1 {
		t.Fatalf("recipients = %d, want 1 delivery left on disk", got.Recipients)
	}
}
