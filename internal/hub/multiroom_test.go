package hub

import (
	"context"
	"testing"
)

// Two agents in different repositories reach each other through a shared
// room. This is the scenario the project exists for.
func TestSharedRoomConnectsRepositories(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)

	gcs, err := h.Join(ctx, JoinOptions{
		Room: "repo:gcs", ExtraRooms: []string{"shared:storefront"},
		Harness: "claude-code", HarnessKey: "gcs-1", Label: "gcs",
	})
	if err != nil {
		t.Fatalf("join gcs: %v", err)
	}
	tb, err := h.Join(ctx, JoinOptions{
		Room: "repo:tracker-bench", ExtraRooms: []string{"shared:storefront"},
		Harness: "claude-code", HarnessKey: "tb-1", Label: "tracker",
	})
	if err != nil {
		t.Fatalf("join tracker: %v", err)
	}

	if len(gcs.Rooms) != 2 {
		t.Fatalf("gcs rooms = %v, want both", gcs.Rooms)
	}

	// A direct message across repositories.
	if _, err := h.Send(ctx, SendOptions{
		From: gcs.ID, To: tb.ID, Subject: "contract", Body: "Order 42100",
	}); err != nil {
		t.Fatalf("send across repositories: %v", err)
	}
	msgs, err := h.Recv(ctx, tb.ID, 0)
	if err != nil {
		t.Fatalf("recv: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Body != "Order 42100" {
		t.Fatalf("tracker received %+v", msgs)
	}

	// The shared room lists both.
	who, err := h.Who(ctx, "shared:storefront")
	if err != nil {
		t.Fatalf("who: %v", err)
	}
	if len(who) != 2 {
		t.Fatalf("shared room has %d sessions, want 2", len(who))
	}

	// A broadcast from the shared room reaches across repositories.
	if _, err := h.Send(ctx, SendOptions{From: gcs.ID, To: "*", Body: "hello all"}); err != nil {
		t.Fatalf("broadcast: %v", err)
	}
	if n, _ := h.Unread(ctx, tb.ID); n != 1 {
		t.Fatalf("tracker unread = %d, want 1", n)
	}
}

// A session in no shared room must not reach across repositories.
func TestSeparateRepositoriesStaySeparate(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)

	a, _ := h.Join(ctx, JoinOptions{Room: "repo:a", Harness: "test", HarnessKey: "a"})
	b, _ := h.Join(ctx, JoinOptions{Room: "repo:b", Harness: "test", HarnessKey: "b"})

	if _, err := h.Send(ctx, SendOptions{From: a.ID, To: b.ID, Body: "x"}); err == nil {
		t.Fatal("message crossed repositories without a shared room")
	}
	if _, err := h.Send(ctx, SendOptions{From: a.ID, To: "*", Body: "x"}); err != nil {
		t.Fatalf("broadcast: %v", err)
	}
	if n, _ := h.Unread(ctx, b.ID); n != 0 {
		t.Fatalf("unrelated repository received %d messages", n)
	}
}

// Rejoining from a second room must update the same session, not create a
// second one. The tester saw one chat become four agents.
func TestRejoinAcrossRoomsKeepsOneSession(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)

	first, err := h.Join(ctx, JoinOptions{
		Room: "repo:one", Harness: "claude-code", HarnessKey: "same-chat",
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := h.Join(ctx, JoinOptions{
		Room: "shared:extra", Harness: "claude-code", HarnessKey: "same-chat",
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Fatalf("rejoin created a new session: %s != %s", first.ID, second.ID)
	}

	rooms, err := h.RoomsOf(ctx, first.ID)
	if err != nil {
		t.Fatalf("rooms: %v", err)
	}
	if len(rooms) != 2 {
		t.Fatalf("rooms = %v, want both memberships", rooms)
	}

	// And it is one row, not two.
	var n int
	if err := h.db.SQL().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sessions WHERE harness_key = 'same-chat'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("%d session rows for one chat, want 1", n)
	}
}

// The strictest room cap wins: a quiet shared room cannot be bypassed by a
// loud repository room.
func TestStrictestRoomCapWins(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)

	if _, err := h.SetRoomAutonomyCap(ctx, "shared:quiet", AutonomyNotify); err != nil {
		t.Fatal(err)
	}
	s, err := h.Join(ctx, JoinOptions{
		Room: "repo:loud", ExtraRooms: []string{"shared:quiet"},
		Harness: "test", Autonomy: AutonomyWake,
	})
	if err != nil {
		t.Fatal(err)
	}
	if s.Autonomy != AutonomyNotify {
		t.Fatalf("autonomy = %d, want the quiet room cap %d", s.Autonomy, AutonomyNotify)
	}
}

func TestLeaveRoomKeepsOtherMemberships(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)

	s, err := h.Join(ctx, JoinOptions{
		Room: "repo:x", ExtraRooms: []string{"shared:s"}, Harness: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.LeaveRoom(ctx, s.ID, "shared:s"); err != nil {
		t.Fatalf("leave room: %v", err)
	}

	rooms, _ := h.RoomsOf(ctx, s.ID)
	if len(rooms) != 1 || rooms[0] != "repo:x" {
		t.Fatalf("rooms after leaving one = %v, want only repo:x", rooms)
	}
	if err := h.LeaveRoom(ctx, s.ID, "shared:s"); err == nil {
		t.Fatal("leaving a room twice succeeded")
	}
}

// A lease claimed in one repository blocks a claim on the same file from a
// session that shares only a shared room with the holder.
func TestLeaseContentionAcrossSharedRoom(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)

	a, _ := h.Join(ctx, JoinOptions{
		Room: "repo:a", ExtraRooms: []string{"shared:s"}, Harness: "test", HarnessKey: "a",
	})
	b, _ := h.Join(ctx, JoinOptions{
		Room: "repo:b", ExtraRooms: []string{"shared:s"}, Harness: "test", HarnessKey: "b",
	})

	if r, _ := h.Claim(ctx, a.ID, "/tmp/shared-file.go", 60, ModeExclusive); !r.OK {
		t.Fatal("first claim failed")
	}
	res, err := h.Claim(ctx, b.ID, "/tmp/shared-file.go", 60, ModeExclusive)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if res.OK {
		t.Fatal("two sessions sharing a room both held one file")
	}
}

// A file selector reaches a lease holder in a room the sender is not in,
// as long as the two share some room.
func TestFileSelectorAcrossSharedRoom(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)

	a, _ := h.Join(ctx, JoinOptions{
		Room: "repo:a", ExtraRooms: []string{"shared:s"}, Harness: "test", HarnessKey: "a",
	})
	b, _ := h.Join(ctx, JoinOptions{
		Room: "repo:b", ExtraRooms: []string{"shared:s"}, Harness: "test", HarnessKey: "b",
	})

	if r, _ := h.Claim(ctx, b.ID, "/tmp/wire.go", 60, ModeExclusive); !r.OK {
		t.Fatal("claim failed")
	}
	if _, err := h.Send(ctx, SendOptions{
		From: a.ID, To: "file:/tmp/*.go", Subject: "s", Body: "b",
	}); err != nil {
		t.Fatalf("send to file: %v", err)
	}
	if n, _ := h.Unread(ctx, b.ID); n != 1 {
		t.Fatalf("lease holder unread = %d, want 1", n)
	}
}

// A relative file selector must resolve against the holder's repository, not
// against the shared room. Two repositories both have src/api/users.go,
// and each means a different file.
func TestFileSelectorUsesHolderDirectory(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)

	gcsDir := t.TempDir()
	trackerDir := t.TempDir()

	gcs, _ := h.Join(ctx, JoinOptions{
		Room: "repo:gcs", ExtraRooms: []string{"shared:storefront"},
		Cwd: gcsDir, Harness: "test", HarnessKey: "gcs",
	})
	tracker, _ := h.Join(ctx, JoinOptions{
		Room: "repo:tracker", ExtraRooms: []string{"shared:storefront"},
		Cwd: trackerDir, Harness: "test", HarnessKey: "tracker",
	})

	if r, _ := h.Claim(ctx, tracker.ID, trackerDir+"/src/api/users.go", 60, ModeExclusive); !r.OK {
		t.Fatal("claim failed")
	}

	if _, err := h.Send(ctx, SendOptions{
		From: gcs.ID, To: "file:src/api/**", Subject: "s", Body: "who",
	}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if n, _ := h.Unread(ctx, tracker.ID); n != 1 {
		t.Fatalf("tracker unread = %d, want 1", n)
	}

	// The same relative selector must not reach the other repository, whose
	// path is different.
	if _, err := h.Send(ctx, SendOptions{
		From: tracker.ID, To: "file:src/api/**", Subject: "s", Body: "x",
	}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if n, _ := h.Unread(ctx, gcs.ID); n != 0 {
		t.Fatalf("gcs received %d messages for a file it does not hold", n)
	}
}
