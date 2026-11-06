package hub

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func TestReleaseAndList(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)
	a := join(t, h, "repo:x", "a")
	b := join(t, h, "repo:x", "b")

	if r, _ := h.Claim(ctx, a.ID, "src/a.go", 60, ModeExclusive); !r.OK {
		t.Fatal("claim a failed")
	}
	if r, _ := h.Claim(ctx, b.ID, "src/b.go", 60, ModeExclusive); !r.OK {
		t.Fatal("claim b failed")
	}

	leases, err := h.List(ctx, a.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(leases) != 2 {
		t.Fatalf("list returned %d leases, want 2", len(leases))
	}

	if err := h.Release(ctx, a.ID, "src/a.go"); err != nil {
		t.Fatalf("release: %v", err)
	}
	leases, _ = h.List(ctx, a.ID)
	if len(leases) != 1 || filepath.Base(leases[0].Path) != "b.go" {
		t.Fatalf("after release: %+v", leases)
	}

	// Releasing something you do not hold is an error, not silence.
	if err := h.Release(ctx, a.ID, "src/a.go"); err == nil {
		t.Fatal("releasing a lease not held succeeded")
	}
}

func TestListIgnoresExpiredLeases(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)
	a := join(t, h, "repo:x", "a")

	if r, _ := h.Claim(ctx, a.ID, "src/a.go", 1, ModeExclusive); !r.OK {
		t.Fatal("claim failed")
	}
	old := h.now
	t.Cleanup(func() { h.setClock(old) })
	h.setClock(func() int64 { return old() + 10 })

	leases, err := h.List(ctx, a.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(leases) != 0 {
		t.Fatalf("list returned %d expired leases, want 0", len(leases))
	}
}

func TestEventTrailRecordsOperations(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)
	a := join(t, h, "repo:x", "a")
	b := join(t, h, "repo:x", "b")

	if _, err := h.Send(ctx, SendOptions{From: a.ID, To: b.ID, Subject: "s", Body: "b"}); err != nil {
		t.Fatal(err)
	}
	if r, _ := h.Claim(ctx, a.ID, "src/a.go", 60, ModeExclusive); !r.OK {
		t.Fatal("claim failed")
	}
	if err := h.Release(ctx, a.ID, "src/a.go"); err != nil {
		t.Fatal(err)
	}

	rows, err := h.db.SQL().QueryContext(ctx,
		`SELECT kind, payload FROM events ORDER BY id`)
	if err != nil {
		t.Fatalf("query events: %v", err)
	}
	defer rows.Close()

	seen := map[string]int{}
	for rows.Next() {
		var kind string
		var payload sql.NullString
		if err := rows.Scan(&kind, &payload); err != nil {
			t.Fatal(err)
		}
		seen[kind]++
		if payload.Valid {
			var v map[string]any
			if err := json.Unmarshal([]byte(payload.String), &v); err != nil {
				t.Fatalf("event %q payload is not JSON: %v", kind, err)
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	for _, kind := range []string{
		"session.join", "message.send", "lease.claim", "lease.release",
	} {
		if seen[kind] == 0 {
			t.Errorf("no %q event recorded", kind)
		}
	}
	if seen["session.join"] != 2 {
		t.Errorf("session.join count = %d, want 2", seen["session.join"])
	}
}

func TestMarkOfferedMovesOnlyStoredDeliveries(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)
	a := join(t, h, "repo:x", "a")
	b := join(t, h, "repo:x", "b")

	if _, err := h.Send(ctx, SendOptions{From: a.ID, To: b.ID, Subject: "one", Body: "1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Send(ctx, SendOptions{From: a.ID, To: b.ID, Subject: "two", Body: "2"}); err != nil {
		t.Fatal(err)
	}

	n, err := h.MarkOffered(ctx, b.ID, StateAccepted)
	if err != nil {
		t.Fatalf("mark offered: %v", err)
	}
	if n != 2 {
		t.Fatalf("moved %d deliveries, want 2", n)
	}

	// A second pass has nothing left in stored, so it moves nothing.
	n, err = h.MarkOffered(ctx, b.ID, StateAccepted)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("second pass moved %d deliveries, want 0", n)
	}

	// Accepted messages are still delivered by recv.
	msgs, err := h.Recv(ctx, b.ID, 0)
	if err != nil {
		t.Fatalf("recv: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("recv returned %d messages after being marked accepted, want 2", len(msgs))
	}
}

func TestLeaveUnknownSessionFails(t *testing.T) {
	h := newHub(t)
	if err := h.Leave(context.Background(), "nope"); err == nil {
		t.Fatal("leaving an unknown session succeeded")
	}
}

func TestHarnessNameDefaults(t *testing.T) {
	for _, v := range []string{"CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT", "CODEX_HOME", "HERMES_HOME"} {
		t.Setenv(v, "")
	}
	if got := HarnessName(); got != "unknown" {
		t.Fatalf("HarnessName = %q, want unknown", got)
	}
	t.Setenv("HERMES_HOME", "/tmp/hermes")
	if got := HarnessName(); got != "hermes" {
		t.Fatalf("HarnessName = %q, want hermes", got)
	}
}

func TestRoomOf(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)
	a := join(t, h, "repo:thing", "a")
	if got := h.RoomOf(ctx, a.ID); got != "repo:thing" {
		t.Fatalf("RoomOf = %q", got)
	}
	if got := h.RoomOf(ctx, "missing"); got != "" {
		t.Fatalf("RoomOf(unknown) = %q, want empty", got)
	}
}

func TestTouchUpdatesLastSeen(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)
	a := join(t, h, "repo:x", "a")

	old := h.now
	t.Cleanup(func() { h.setClock(old) })
	h.setClock(func() int64 { return old() + 100 })

	if err := h.Touch(ctx, a.ID); err != nil {
		t.Fatalf("touch: %v", err)
	}
	sessions, _ := h.Who(ctx, "repo:x")
	if sessions[0].LastSeen != old()+100 {
		t.Fatalf("last_seen = %d, want %d", sessions[0].LastSeen, old()+100)
	}
}

func TestOpenUsesDefaultPath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENTS_ROOM_DB", dir+"/room.db")

	h, err := Open()
	if err != nil {
		t.Fatalf("open default: %v", err)
	}
	defer h.Close()

	if got := h.Path(); got != dir+"/room.db" {
		t.Fatalf("Path = %q, want the environment override", got)
	}
	if _, err := h.Join(context.Background(), JoinOptions{Room: "repo:x"}); err != nil {
		t.Fatalf("join on a default-path hub: %v", err)
	}
}

func TestLeasePathIsNormalised(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)
	a := join(t, h, "repo:x", "a")
	b := join(t, h, "repo:x", "b")

	// One session names the file relatively, the other absolutely. They must
	// contend for the same lease.
	if r, _ := h.Claim(ctx, a.ID, "src/wire.go", 60, ModeExclusive); !r.OK {
		t.Fatal("relative claim failed")
	}
	abs, err := filepath.Abs("src/wire.go")
	if err != nil {
		t.Fatal(err)
	}
	res, err := h.Claim(ctx, b.ID, abs, 60, ModeExclusive)
	if err != nil {
		t.Fatalf("absolute claim: %v", err)
	}
	if res.OK {
		t.Fatal("absolute claim on the same file succeeded; paths are not normalised")
	}
	if res.Holder == nil || res.Holder.Owner != a.ID {
		t.Fatalf("holder = %+v, want session a", res.Holder)
	}

	lease, err := h.Check(ctx, b.ID, abs)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if lease == nil || lease.Owner != a.ID {
		t.Fatalf("check by absolute path = %+v", lease)
	}
}

func TestRenewExtendsAndRefusesForeignLease(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)
	a := join(t, h, "repo:x", "a")
	b := join(t, h, "repo:x", "b")

	if r, _ := h.Claim(ctx, a.ID, "src/a.go", 60, ModeExclusive); !r.OK {
		t.Fatal("claim failed")
	}
	old := h.now
	t.Cleanup(func() { h.setClock(old) })
	h.setClock(func() int64 { return old() + 30 })

	lease, err := h.Renew(ctx, a.ID, "src/a.go", 600)
	if err != nil {
		t.Fatalf("renew: %v", err)
	}
	if lease.ExpiresAt != old()+30+600 {
		t.Fatalf("expires_at = %d, want %d", lease.ExpiresAt, old()+30+600)
	}

	// A session that does not hold the lease cannot renew it.
	if _, err := h.Renew(ctx, b.ID, "src/a.go", 600); err == nil {
		t.Fatal("renewing a lease held by another session succeeded")
	}

	// Renewing an expired lease fails: the holder must claim again.
	h.setClock(func() int64 { return old() + 10_000 })
	if _, err := h.Renew(ctx, a.ID, "src/a.go", 600); err == nil {
		t.Fatal("renewing an expired lease succeeded")
	}
}

func TestPruneStaleRemovesOnlyOldSessions(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)

	stale, err := h.Join(ctx, JoinOptions{Room: "repo:x", Harness: "test", HarnessKey: "stale"})
	if err != nil {
		t.Fatal(err)
	}
	old := h.now
	t.Cleanup(func() { h.setClock(old) })

	// Move the clock forward, then join a fresh session so it has a recent
	// last_seen while the first one is an hour old.
	h.setClock(func() int64 { return old() + 3600 })
	fresh, err := h.Join(ctx, JoinOptions{Room: "repo:x", Harness: "test", HarnessKey: "fresh"})
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := h.Claim(ctx, stale.ID, "src/old.go", 7200, ModeExclusive); !r.OK {
		t.Fatal("stale claim failed")
	}

	n, err := h.PruneStale(ctx, "repo:x", 30*time.Minute)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n != 1 {
		t.Fatalf("pruned %d sessions, want 1", n)
	}

	sessions, _ := h.Who(ctx, "repo:x")
	if len(sessions) != 1 || sessions[0].ID != fresh.ID {
		t.Fatalf("remaining sessions = %+v, want only the fresh one", sessions)
	}

	// The stale session's leases went with it.
	leases, _ := h.List(ctx, fresh.ID)
	if len(leases) != 0 {
		t.Fatalf("%d leases survived the prune, want 0", len(leases))
	}
}

// A session reachable only through a shared room is still prunable from
// that room. The first version filtered on the legacy sessions.room_id, so
// `prune --room shared:x` silently matched nobody.
func TestPruneStaleFindsSessionsBySharedMembership(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)

	stale, err := h.Join(ctx, JoinOptions{
		Room: "repo:gcs", ExtraRooms: []string{"shared:storefront"},
		Harness: "test", HarnessKey: "stale",
	})
	if err != nil {
		t.Fatal(err)
	}
	old := h.now
	t.Cleanup(func() { h.setClock(old) })

	h.setClock(func() int64 { return old() + 3600 })
	if _, err := h.Join(ctx, JoinOptions{
		Room: "repo:tracker", ExtraRooms: []string{"shared:storefront"},
		Harness: "test", HarnessKey: "fresh",
	}); err != nil {
		t.Fatal(err)
	}

	n, err := h.PruneStale(ctx, "shared:storefront", 30*time.Minute)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n != 1 {
		t.Fatalf("pruned %d sessions from the shared room, want 1", n)
	}
	who, _ := h.Who(ctx, "shared:storefront")
	if len(who) != 1 {
		t.Fatalf("shared room still has %d sessions, want 1", len(who))
	}
	if who[0].ID == stale.ID {
		t.Fatal("the stale session survived")
	}
}

func TestPruneStaleOnUnknownRoomIsHarmless(t *testing.T) {
	h := newHub(t)
	n, err := h.PruneStale(context.Background(), "repo:nope", time.Hour)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n != 0 {
		t.Fatalf("pruned %d sessions in a room that does not exist", n)
	}
}
