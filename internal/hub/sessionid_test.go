package hub

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// A read for a session id that names nothing must fail, not answer "nothing
// for you". Both are zero rows, and a caller that passed the wrong id cannot
// tell them apart: one agent spent weeks believing no message had reached it
// while nineteen piled up, because every msg_recv with its harness session id
// came back empty.
func TestUnknownSessionIsAnErrorOnReads(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)
	a := join(t, h, "repo:x", "a")

	reads := map[string]func() error{
		"recv":   func() error { _, err := h.Recv(ctx, "deadbeefdeadbeef", 0); return err },
		"unread": func() error { _, err := h.Unread(ctx, "deadbeefdeadbeef"); return err },
		"sent":   func() error { _, err := h.SentMessages(ctx, "deadbeefdeadbeef", 0); return err },
		"pending": func() error {
			_, err := h.PendingSince(ctx, "deadbeefdeadbeef", 0)
			return err
		},
	}
	for name, read := range reads {
		err := read()
		if !errors.Is(err, ErrNoSuchSession) {
			t.Fatalf("%s with an unknown id = %v, want ErrNoSuchSession", name, err)
		}
	}

	// The same calls for a session that exists still answer normally.
	if msgs, err := h.Recv(ctx, a.ID, 0); err != nil || len(msgs) != 0 {
		t.Fatalf("recv for a real session = %d messages, %v; want 0, nil", len(msgs), err)
	}
}

// The common wrong id is the harness key, because the harness puts it in the
// environment and it looks exactly like an identity. The error has to name the
// session id that key belongs to, or the caller is stuck with two ids and no
// way to choose.
func TestUnknownSessionNamesTheHarnessKeyOwner(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)
	s, err := h.Join(ctx, JoinOptions{
		Room: "repo:x", Harness: "test", HarnessKey: "2b50133e-aaaa-bbbb-cccc-1234567890ab",
	})
	if err != nil {
		t.Fatalf("join: %v", err)
	}

	_, err = h.Recv(ctx, "2b50133e-aaaa-bbbb-cccc-1234567890ab", 0)
	if !errors.Is(err, ErrNoSuchSession) {
		t.Fatalf("recv with the harness key = %v, want ErrNoSuchSession", err)
	}
	if !strings.Contains(err.Error(), s.ID) {
		t.Fatalf("error %q does not name the session id %s the key belongs to", err, s.ID)
	}
	if !strings.Contains(err.Error(), "harness key") {
		t.Fatalf("error %q does not say that the id given is a harness key", err)
	}
}

// A truncated id is what a model produces when it copies the first characters
// of what it saw, so the error offers the full id instead of only refusing.
func TestUnknownSessionSuggestsTheNearestMatch(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)
	s := join(t, h, "repo:x", "a")

	_, err := h.Recv(ctx, s.ID[:6], 0)
	if !errors.Is(err, ErrNoSuchSession) {
		t.Fatalf("recv with a truncated id = %v, want ErrNoSuchSession", err)
	}
	if !strings.Contains(err.Error(), s.ID) {
		t.Fatalf("error %q does not suggest the full id %s", err, s.ID)
	}
}

// An id of the right shape that names nothing is a different case: the session
// left, or the caller is reading another store. The hint says so rather than
// blaming the shape.
func TestUnknownSessionDistinguishesAStaleID(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)

	_, err := h.Recv(ctx, "deadbeefdeadbeef", 0)
	if !errors.Is(err, ErrNoSuchSession) {
		t.Fatalf("recv with a stale id = %v, want ErrNoSuchSession", err)
	}
	if !strings.Contains(err.Error(), "may have left") {
		t.Fatalf("error %q does not suggest the session left", err)
	}
}

// An ack with an unknown session id used to fail with "no delivery of <id> for
// this session", which is also what a real session gets for a message it never
// received. The caller could not tell which of its two arguments was wrong.
func TestAckWithUnknownSessionSaysSo(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)
	a := join(t, h, "repo:x", "a")
	b := join(t, h, "repo:x", "b")
	m, err := h.Send(ctx, SendOptions{From: a.ID, To: b.ID, Subject: "s", Body: "b"})
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	if err := h.Ack(ctx, "deadbeefdeadbeef", m.ID); !errors.Is(err, ErrNoSuchSession) {
		t.Fatalf("ack from an unknown session = %v, want ErrNoSuchSession", err)
	}
	// A real session acking a message it never received is a different error,
	// and must not be mistaken for the first one.
	err = h.Ack(ctx, a.ID, m.ID)
	if err == nil || errors.Is(err, ErrNoSuchSession) {
		t.Fatalf("ack of a message the session never received = %v, want a delivery error", err)
	}
}

// RoomFor is the read that must not guess. RoomOf falls back to "", which
// callers turn into the current directory's room; a mistyped id then makes
// room_set_autonomy_cap write into the wrong room and report success.
func TestRoomForRefusesAnUnknownSession(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)
	a := join(t, h, "repo:x", "a")

	room, err := h.RoomFor(ctx, a.ID)
	if err != nil || room != "repo:x" {
		t.Fatalf("RoomFor a real session = %q, %v; want repo:x", room, err)
	}
	if _, err := h.RoomFor(ctx, "deadbeefdeadbeef"); !errors.Is(err, ErrNoSuchSession) {
		t.Fatalf("RoomFor an unknown session = %v, want ErrNoSuchSession", err)
	}
}

// An empty inbox says which rooms the session is in and whether anything was
// ever delivered to it, so "nothing waiting" can be told from "you are asking
// in the wrong place". An agent that suspected the second had to read the
// database by hand to find out.
func TestInboxExplainsAnEmptyAnswer(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)
	a := join(t, h, "repo:x", "a")
	b := join(t, h, "repo:x", "b")

	inbox, err := h.Inbox(ctx, a.ID)
	if err != nil {
		t.Fatalf("inbox: %v", err)
	}
	if len(inbox.Rooms) != 1 || inbox.Rooms[0] != "repo:x" {
		t.Fatalf("rooms = %v, want [repo:x]", inbox.Rooms)
	}
	if inbox.Delivered != 0 {
		t.Fatalf("delivered = %d for a session nobody wrote to, want 0", inbox.Delivered)
	}

	m, err := h.Send(ctx, SendOptions{From: b.ID, To: a.ID, Subject: "s", Body: "b"})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if _, err := h.Recv(ctx, a.ID, 0); err != nil {
		t.Fatalf("recv: %v", err)
	}

	inbox, err = h.Inbox(ctx, a.ID)
	if err != nil {
		t.Fatalf("inbox after reading: %v", err)
	}
	if inbox.Delivered != 1 || inbox.Read != 1 {
		t.Fatalf("inbox = %d delivered, %d read; want 1 and 1", inbox.Delivered, inbox.Read)
	}
	if inbox.LastDeliveredAt == 0 {
		t.Fatal("last delivery time was not recorded")
	}
	_ = m
}

// The rooms listed are the reader's own, oldest membership first.
func TestInboxListsEveryRoomOfTheSession(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)
	s, err := h.Join(ctx, JoinOptions{
		Room: "repo:a", ExtraRooms: []string{"shared:s"}, Harness: "test", HarnessKey: "a",
	})
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	inbox, err := h.Inbox(ctx, s.ID)
	if err != nil {
		t.Fatalf("inbox: %v", err)
	}
	want := []string{"repo:a", "shared:s"}
	if len(inbox.Rooms) != len(want) {
		t.Fatalf("rooms = %v, want %v", inbox.Rooms, want)
	}
	for i := range want {
		if inbox.Rooms[i] != want[i] {
			t.Fatalf("rooms = %v, want %v", inbox.Rooms, want)
		}
	}
}

// A rejoin that does not mention autonomy keeps the level the chat already
// has. An MCP room_join sends no autonomy field, which used to arrive as 0
// and overwrite the 1 the SessionStart hook had set; the chat was then never
// woken while idle, which is the symptom two live sessions reported.
func TestRejoinWithoutAutonomyKeepsTheStoredLevel(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)

	// The hook joins first, stating the operator's level.
	first, err := h.Join(ctx, JoinOptions{
		Room: "repo:x", Harness: "test", HarnessKey: "chat",
		Autonomy: AutonomyHold, AutonomySet: true,
	})
	if err != nil {
		t.Fatalf("hook join: %v", err)
	}
	if first.Autonomy != AutonomyHold {
		t.Fatalf("first join autonomy = %d, want %d", first.Autonomy, AutonomyHold)
	}

	// A model calls room_join with the field omitted.
	again, err := h.Join(ctx, JoinOptions{
		Room: "repo:x", Harness: "test", HarnessKey: "chat",
	})
	if err != nil {
		t.Fatalf("rejoin: %v", err)
	}
	if again.ID != first.ID {
		t.Fatalf("rejoin created %s, want the same session %s", again.ID, first.ID)
	}
	if again.Autonomy != AutonomyHold {
		t.Fatalf("autonomy after a rejoin that omitted it = %d, want %d kept",
			again.Autonomy, AutonomyHold)
	}
}

// A caller that explicitly asks to go quiet is told it did not. Lowering on a
// rejoin is refused, but refusing in silence would leave a caller that meant
// notify believing it got it.
func TestRejoinThatAsksToLowerIsToldItWasNot(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)
	if _, err := h.Join(ctx, JoinOptions{
		Room: "repo:x", Harness: "test", HarnessKey: "chat",
		Autonomy: AutonomyHold, AutonomySet: true,
	}); err != nil {
		t.Fatalf("join: %v", err)
	}
	got, err := h.Join(ctx, JoinOptions{
		Room: "repo:x", Harness: "test", HarnessKey: "chat",
		Autonomy: AutonomyNotify, AutonomySet: true,
	})
	if err != nil {
		t.Fatalf("rejoin: %v", err)
	}
	if got.Autonomy != AutonomyHold {
		t.Fatalf("autonomy = %d, want the stored %d", got.Autonomy, AutonomyHold)
	}
	if !strings.Contains(got.Warning, "autonomy stays") {
		t.Fatalf("warning = %q, want it to say the level did not change", got.Warning)
	}
	if !strings.Contains(got.Warning, "room_set_autonomy_cap") {
		t.Fatalf("warning = %q, want it to name the way to lower a room", got.Warning)
	}
}

// A rejoin may raise the level, never lower it: lowering is the operator's
// decision and has one route, the room cap.
func TestRejoinRaisesButNeverLowers(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)
	if _, err := h.Join(ctx, JoinOptions{
		Room: "repo:x", Harness: "test", HarnessKey: "chat",
		Autonomy: AutonomyWake, AutonomySet: true,
	}); err != nil {
		t.Fatalf("join: %v", err)
	}

	lowered, err := h.Join(ctx, JoinOptions{
		Room: "repo:x", Harness: "test", HarnessKey: "chat",
		Autonomy: AutonomyNotify, AutonomySet: true,
	})
	if err != nil {
		t.Fatalf("rejoin: %v", err)
	}
	if lowered.Autonomy != AutonomyWake {
		t.Fatalf("a rejoin lowered autonomy to %d; only the room cap may lower it",
			lowered.Autonomy)
	}

	// Raising works, so the rule is "never lower", not "never change".
	raised, err := h.Join(ctx, JoinOptions{
		Room: "repo:x", Harness: "test", HarnessKey: "chat",
		Autonomy: AutonomyWake, AutonomySet: true,
	})
	if err != nil {
		t.Fatalf("rejoin: %v", err)
	}
	if raised.Autonomy != AutonomyWake {
		t.Fatalf("autonomy = %d, want %d", raised.Autonomy, AutonomyWake)
	}
}

// The room cap still lowers the level, so the operator keeps the one lever
// that is meant to.
func TestRoomCapStillLowersOnRejoin(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)
	if _, err := h.SetRoomAutonomyCap(ctx, "repo:x", AutonomyNotify); err != nil {
		t.Fatalf("set cap: %v", err)
	}
	s, err := h.Join(ctx, JoinOptions{
		Room: "repo:x", Harness: "test", HarnessKey: "chat",
		Autonomy: AutonomyWake, AutonomySet: true,
	})
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	if s.Autonomy != AutonomyNotify {
		t.Fatalf("autonomy = %d under a notify cap, want %d", s.Autonomy, AutonomyNotify)
	}
}

// A tally of readers cannot answer "did the agent I meant read it". In a
// shared room a broadcast reaches every member, and a bystander's read is
// indistinguishable from the intended recipient's in a count of three.
func TestSentNamesTheReaders(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)

	// The sender and its intended recipient share a repository room.
	sender, err := h.Join(ctx, JoinOptions{
		Room: "repo:gcs", ExtraRooms: []string{"shared:s"}, Harness: "test", HarnessKey: "sender",
	})
	if err != nil {
		t.Fatalf("join sender: %v", err)
	}
	intended, err := h.Join(ctx, JoinOptions{
		Room: "repo:fw", ExtraRooms: []string{"shared:s"}, Harness: "test", HarnessKey: "fw",
	})
	if err != nil {
		t.Fatalf("join intended: %v", err)
	}
	// A bystander meets the sender only in the shared room.
	bystander, err := h.Join(ctx, JoinOptions{
		Room: "repo:other", ExtraRooms: []string{"shared:s"}, Harness: "test", HarnessKey: "other",
	})
	if err != nil {
		t.Fatalf("join bystander: %v", err)
	}

	m, err := h.Send(ctx, SendOptions{From: sender.ID, To: "*", Subject: "s", Body: "b"})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if _, err := h.Recv(ctx, intended.ID, 0); err != nil {
		t.Fatalf("recv intended: %v", err)
	}
	if _, err := h.Recv(ctx, bystander.ID, 0); err != nil {
		t.Fatalf("recv bystander: %v", err)
	}

	sent, err := h.SentMessages(ctx, sender.ID, 0)
	if err != nil {
		t.Fatalf("sent: %v", err)
	}
	if len(sent) != 1 || sent[0].ID != m.ID {
		t.Fatalf("sent listed %d messages, want the one", len(sent))
	}
	if got := len(sent[0].Readers); got != 2 {
		t.Fatalf("readers = %d, want both who read it", got)
	}
	byID := map[string]Reader{}
	for _, r := range sent[0].Readers {
		byID[r.SessionID] = r
	}
	if _, ok := byID[intended.ID]; !ok {
		t.Fatal("the intended recipient is missing from the readers")
	}
	// The bystander and the intended recipient share the same room set, so
	// both read as shared-only: the store cannot tell them apart, and saying
	// otherwise would be a guess.
	if !byID[bystander.ID].SharedOnly || !byID[intended.ID].SharedOnly {
		t.Fatalf("shared-only = %v/%v, want both true",
			byID[intended.ID].SharedOnly, byID[bystander.ID].SharedOnly)
	}
	if got := byID[intended.ID].Rooms; len(got) != 1 || got[0] != "shared:s" {
		t.Fatalf("rooms = %v, want the shared room they meet in", got)
	}
}

// A sender that shares a repository room with the reader is not told it is a
// stranger: SharedOnly is a hint about the meeting place, not a verdict.
func TestSentReaderInACommonRepositoryRoomIsNotSharedOnly(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)
	a, err := h.Join(ctx, JoinOptions{Room: "repo:x", Harness: "test", HarnessKey: "a"})
	if err != nil {
		t.Fatalf("join a: %v", err)
	}
	b, err := h.Join(ctx, JoinOptions{Room: "repo:x", Harness: "test", HarnessKey: "b"})
	if err != nil {
		t.Fatalf("join b: %v", err)
	}
	if _, err := h.Send(ctx, SendOptions{From: a.ID, To: b.ID, Subject: "s", Body: "b"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if _, err := h.Recv(ctx, b.ID, 0); err != nil {
		t.Fatalf("recv: %v", err)
	}
	sent, err := h.SentMessages(ctx, a.ID, 0)
	if err != nil {
		t.Fatalf("sent: %v", err)
	}
	if len(sent[0].Readers) != 1 {
		t.Fatalf("readers = %d, want 1", len(sent[0].Readers))
	}
	if sent[0].Readers[0].SharedOnly {
		t.Fatal("a reader in a common repository room was reported as shared-only")
	}
}
