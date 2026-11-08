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
