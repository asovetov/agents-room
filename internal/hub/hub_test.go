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
	if afterB["src/c.go"] != beforeB["src/c.go"] {
		t.Fatal("RenewAll extended another session's lease")
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
