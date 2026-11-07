package hub

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A session that joins a room where work is under way has no deliveries, so
// Recv correctly reports nothing. That is the wrong answer to "what has been
// decided here", which is the question a joining agent actually has.
func TestHistoryShowsWhatANewcomerMissed(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)

	api := join(t, h, "repo:api", "api")
	bot := join(t, h, "repo:bot", "bot")
	if _, err := h.Send(ctx, SendOptions{
		From: api.ID, To: "*", Subject: "users endpoint changed", Body: "phone is required",
	}); err != nil {
		t.Fatal(err)
	}

	// A session that arrives afterwards sees nothing in Recv.
	late, err := h.Join(ctx, JoinOptions{Room: "repo:api", Harness: "test", Label: "late"})
	if err != nil {
		t.Fatal(err)
	}
	if unread, err := h.Unread(ctx, late.ID); err != nil || unread != 0 {
		t.Fatalf("late session has %d unread (err %v), want 0", unread, err)
	}
	_ = bot

	entries, err := h.History(ctx, "repo:api", 10)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("history has %d entries, want 1", len(entries))
	}
	if entries[0].Subject != "users endpoint changed" {
		t.Fatalf("subject = %q", entries[0].Subject)
	}
	if entries[0].SenderTag != "api" {
		t.Fatalf("sender label = %q, want the joining session's label", entries[0].SenderTag)
	}
}

// History is oldest first, so a reader follows the conversation forwards.
func TestHistoryIsOldestFirst(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)
	a := join(t, h, "repo:x", "a")

	old := h.now
	t.Cleanup(func() { h.setClock(old) })
	base := old()
	for i, subject := range []string{"first", "second", "third"} {
		h.setClock(func() int64 { return base + int64(i) })
		if _, err := h.Send(ctx, SendOptions{From: a.ID, To: "*", Subject: subject, Body: "x"}); err != nil {
			t.Fatal(err)
		}
	}

	entries, err := h.History(ctx, "repo:x", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("got %d entries, want 3", len(entries))
	}
	for i, want := range []string{"first", "second", "third"} {
		if entries[i].Subject != want {
			t.Fatalf("entry %d = %q, want %q", i, entries[i].Subject, want)
		}
	}
}

// A limit keeps the newest, then presents them forwards.
func TestHistoryLimitKeepsTheNewest(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)
	a := join(t, h, "repo:x", "a")

	old := h.now
	t.Cleanup(func() { h.setClock(old) })
	base := old()
	for i, subject := range []string{"first", "second", "third"} {
		h.setClock(func() int64 { return base + int64(i) })
		if _, err := h.Send(ctx, SendOptions{From: a.ID, To: "*", Subject: subject, Body: "x"}); err != nil {
			t.Fatal(err)
		}
	}

	entries, err := h.History(ctx, "repo:x", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	if entries[0].Subject != "second" || entries[1].Subject != "third" {
		t.Fatalf("got %q, %q; want the two newest in order", entries[0].Subject, entries[1].Subject)
	}
}

// An expired message is not history a newcomer should act on.
func TestHistorySkipsExpiredMessages(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)
	a := join(t, h, "repo:x", "a")

	if _, err := h.Send(ctx, SendOptions{
		From: a.ID, To: "*", Subject: "gone", Body: "x", TTL: 1,
	}); err != nil {
		t.Fatal(err)
	}

	old := h.now
	t.Cleanup(func() { h.setClock(old) })
	h.setClock(func() int64 { return old() + 60 })

	entries, err := h.History(ctx, "repo:x", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("history shows %d expired message(s), want 0", len(entries))
	}
}

// History does not touch delivery states: reading what was said is not
// acknowledging what was sent to you.
func TestHistoryDoesNotMarkAnythingRead(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)
	a := join(t, h, "repo:x", "a")
	b := join(t, h, "repo:x", "b")

	if _, err := h.Send(ctx, SendOptions{From: a.ID, To: "*", Subject: "hi", Body: "x"}); err != nil {
		t.Fatal(err)
	}
	before, _ := h.Unread(ctx, b.ID)
	if _, err := h.History(ctx, "repo:x", 10); err != nil {
		t.Fatal(err)
	}
	after, _ := h.Unread(ctx, b.ID)
	if before != after {
		t.Fatalf("unread changed from %d to %d by reading history", before, after)
	}
}

// The join summary is what tells a joining agent that there is history to
// read; without it, Recv's silence reads as an empty room.
func TestSummarizeReportsRecentTraffic(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)
	a := join(t, h, "repo:x", "api")

	if _, err := h.Send(ctx, SendOptions{From: a.ID, To: "*", Subject: "phone field", Body: "x"}); err != nil {
		t.Fatal(err)
	}

	s, err := h.Summarize(ctx, "repo:x", 24*time.Hour)
	if err != nil {
		t.Fatalf("summarize: %v", err)
	}
	if s.Messages != 1 {
		t.Fatalf("messages = %d, want 1", s.Messages)
	}
	if !strings.Contains(s.Last, "phone") {
		t.Fatalf("last subject = %q", s.Last)
	}
	if s.LastFrom != "api" {
		t.Fatalf("last from = %q, want the sender's label", s.LastFrom)
	}
}

// An empty room says so rather than reporting stale traffic.
func TestSummarizeOfAnEmptyRoom(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)
	join(t, h, "repo:x", "a")

	s, err := h.Summarize(ctx, "repo:x", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if s.Messages != 0 {
		t.Fatalf("messages = %d, want 0", s.Messages)
	}
}

// A message sent from one repository to an agent it meets in a shared room
// belongs to that shared room too. Filing it only under the sender's own
// room made the shared room's history empty while work was going on there.
func TestHistoryOfASharedRoomShowsMessagesSentFromARepository(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)

	// Cwd does not matter to this test, but it has to be a path the platform
	// calls absolute, so it is built rather than written as a POSIX literal.
	dir := t.TempDir()
	shop, err := h.Join(ctx, JoinOptions{
		Room: "repo:shop", ExtraRooms: []string{"shared:storefront"},
		Harness: "test", Label: "shop", Cwd: filepath.Join(dir, "shop"), PID: 4242,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Join(ctx, JoinOptions{
		Room: "repo:bot", ExtraRooms: []string{"shared:storefront"},
		Harness: "test", Label: "bot", Cwd: filepath.Join(dir, "bot"), PID: 4343,
	}); err != nil {
		t.Fatal(err)
	}

	// The shop agent writes to whoever is in the shared room.
	if _, err := h.Send(ctx, SendOptions{
		From: shop.ID, To: "*", Subject: "users now returns phone", Body: "bump your client",
	}); err != nil {
		t.Fatal(err)
	}

	for _, room := range []string{"repo:shop", "shared:storefront"} {
		entries, err := h.History(ctx, room, 10)
		if err != nil {
			t.Fatalf("history %s: %v", room, err)
		}
		if len(entries) != 1 {
			t.Fatalf("history of %s has %d entries, want 1", room, len(entries))
		}
	}
}

// A message addressed to one session in the sender's own room is still
// history for that room, and must not appear in a room it never reached.
func TestHistoryDoesNotLeakIntoUnrelatedRooms(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)

	a, err := h.Join(ctx, JoinOptions{Room: "repo:x", Harness: "test", Label: "a", PID: 4242})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Join(ctx, JoinOptions{
		Room: "repo:y", ExtraRooms: []string{"shared:other"},
		Harness: "test", Label: "b", PID: 4343,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Send(ctx, SendOptions{From: a.ID, To: "*", Subject: "x only", Body: "y"}); err != nil {
		t.Fatal(err)
	}

	entries, err := h.History(ctx, "shared:other", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("history of an unrelated room shows %d entries", len(entries))
	}
}
