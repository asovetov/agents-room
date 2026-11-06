package hub

import (
	"context"
	"os"
	"path/filepath"
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
	// Absolute on every platform: /tmp is not, on Windows.
	file := filepath.Join(t.TempDir(), "shared-file.go")

	a, _ := h.Join(ctx, JoinOptions{
		Room: "repo:a", ExtraRooms: []string{"shared:s"}, Harness: "test", HarnessKey: "a",
	})
	b, _ := h.Join(ctx, JoinOptions{
		Room: "repo:b", ExtraRooms: []string{"shared:s"}, Harness: "test", HarnessKey: "b",
	})

	if r, _ := h.Claim(ctx, a.ID, file, 60, ModeExclusive); !r.OK {
		t.Fatal("first claim failed")
	}
	res, err := h.Claim(ctx, b.ID, file, 60, ModeExclusive)
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
	dir := t.TempDir()
	file := filepath.Join(dir, "wire.go")

	a, _ := h.Join(ctx, JoinOptions{
		Room: "repo:a", ExtraRooms: []string{"shared:s"}, Harness: "test", HarnessKey: "a",
	})
	b, _ := h.Join(ctx, JoinOptions{
		Room: "repo:b", ExtraRooms: []string{"shared:s"}, Harness: "test", HarnessKey: "b",
	})

	if r, _ := h.Claim(ctx, b.ID, file, 60, ModeExclusive); !r.OK {
		t.Fatal("claim failed")
	}
	// The selector names the same directory, in the form the test can build
	// portably.
	if _, err := h.Send(ctx, SendOptions{
		From: a.ID, To: "file:" + filepath.Join(dir, "*.go"), Subject: "s", Body: "b",
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

func TestSharedRoomsFromFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".agents-room"), 0o755); err != nil {
		t.Fatal(err)
	}
	content := "# rooms for this repository\nstorefront\nshared:protocol  # already prefixed\n\n"
	if err := os.WriteFile(filepath.Join(dir, ".agents-room", "shared-rooms"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	got := SharedRoomsFrom(dir)
	want := []string{"shared:storefront", "shared:protocol"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestSharedRoomsFromEnv(t *testing.T) {
	t.Setenv("AGENTS_ROOM_SHARED", "storefront, shared:protocol")
	got := SharedRoomsFrom(t.TempDir())
	if len(got) != 2 || got[0] != "shared:storefront" || got[1] != "shared:protocol" {
		t.Fatalf("got %v", got)
	}
}

// "--also storefront" and "AGENTS_ROOM_SHARED=storefront" must land in the same
// room. Before this, the unprefixed name created a second room with the same
// look and no shared membership, and messages sent there reached nobody.
func TestJoinNormalizesUnprefixedSharedRoom(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)

	// One session spells it without the prefix, as the CLI flag allows.
	cli, err := h.Join(ctx, JoinOptions{
		Room: "repo:gcs", ExtraRooms: []string{"storefront"},
		Harness: "test", HarnessKey: "cli",
	})
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	// The other spells it the way the hook does, with the prefix.
	hooked, err := h.Join(ctx, JoinOptions{
		Room: "repo:tracker", ExtraRooms: []string{"shared:storefront"},
		Harness: "test", HarnessKey: "hooked",
	})
	if err != nil {
		t.Fatalf("join: %v", err)
	}

	for _, s := range []*Session{cli, hooked} {
		if len(s.Rooms) != 2 {
			t.Fatalf("%s rooms = %v, want two", s.ID, s.Rooms)
		}
		for _, r := range s.Rooms {
			if r == "storefront" {
				t.Fatalf("unprefixed room %q survived; want shared:storefront", r)
			}
		}
	}

	who, err := h.Who(ctx, "shared:storefront")
	if err != nil {
		t.Fatalf("who: %v", err)
	}
	if len(who) != 2 {
		t.Fatalf("shared room has %d sessions, want both", len(who))
	}

	// And they can reach each other.
	if _, err := h.Send(ctx, SendOptions{From: cli.ID, To: hooked.ID, Body: "hi"}); err != nil {
		t.Fatalf("send across the shared room: %v", err)
	}
}

// A send whose selector matches nobody must say so instead of reporting a
// delivery that will never happen.
func TestSendReportsZeroRecipients(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)
	a := join(t, h, "repo:x", "a")

	m, err := h.Send(ctx, SendOptions{From: a.ID, To: "tag:nobody", Body: "x"})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if m.Recipients != 0 {
		t.Fatalf("recipients = %d, want 0", m.Recipients)
	}

	// A broadcast in a room of one is the same situation.
	b, err := h.Send(ctx, SendOptions{From: a.ID, To: "*", Body: "x"})
	if err != nil {
		t.Fatalf("broadcast: %v", err)
	}
	if b.Recipients != 0 {
		t.Fatalf("broadcast recipients = %d, want 0", b.Recipients)
	}

	// With a second session the count is honest.
	second := join(t, h, "repo:x", "b")
	c, err := h.Send(ctx, SendOptions{From: a.ID, To: "*", Body: "x"})
	if err != nil {
		t.Fatalf("broadcast: %v", err)
	}
	if c.Recipients != 1 {
		t.Fatalf("broadcast recipients = %d, want 1", c.Recipients)
	}

	// The recipient must see the same number the sender was told, not the
	// zero a freshly built Message starts with.
	got, err := h.Recv(ctx, second.ID, 0)
	if err != nil {
		t.Fatalf("recv: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("recipient got %d messages, want 1", len(got))
	}
	if got[0].Recipients != 1 {
		t.Fatalf("recipient sees recipients = %d, want 1", got[0].Recipients)
	}
}

func TestSharedRoomsDeduplicate(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".agents-room"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".agents-room", "shared-rooms"),
		[]byte("storefront\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTS_ROOM_SHARED", "shared:storefront")

	got := SharedRoomsFrom(dir)
	if len(got) != 1 {
		t.Fatalf("got %v, want one room after de-duplication", got)
	}
}

func TestSharedRoomsMissingConfigIsEmpty(t *testing.T) {
	t.Setenv("AGENTS_ROOM_SHARED", "")
	if got := SharedRoomsFrom(t.TempDir()); len(got) != 0 {
		t.Fatalf("got %v, want none", got)
	}
}
