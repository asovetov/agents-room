package hub

import (
	"context"
	"strings"
	"testing"
)

// A broadcast is filed under the session's own repository room, whichever
// other rooms it joined. The room list used to come back in whatever order
// SQLite chose, so a message from a chat in repo:gcs and two shared rooms
// could be attributed to a shared one.
func TestSendFilesTheMessageUnderTheRepositoryRoom(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)

	s, err := h.Join(ctx, JoinOptions{
		Room: "repo:gcs", ExtraRooms: []string{"shared:aerobasis", "aerobasis"},
		Harness: "test", Label: "a",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Send(ctx, SendOptions{From: s.ID, To: "*", Subject: "x", Body: "y"}); err != nil {
		t.Fatal(err)
	}

	var room string
	if err := h.db.SQL().QueryRowContext(ctx, `
		SELECT r.name FROM messages m JOIN rooms r ON r.id = m.room_id LIMIT 1`).Scan(&room); err != nil {
		t.Fatal(err)
	}
	if room != "repo:gcs" {
		t.Fatalf("message filed under %q, want the repository room repo:gcs", room)
	}
}

// A relative path names a file in the caller's own checkout. An agent that
// means another repository's file writes the same relative string and leases
// a different file, so the grant has to say so.
func TestClaimWarnsWhenThePathIsOutsideTheCallersDirectory(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)

	s, err := h.Join(ctx, JoinOptions{
		Room: "repo:bot", Harness: "test", Cwd: "/work/bot", PID: 4242,
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := h.Claim(ctx, s.ID, "/work/shop/src/users.go", 60, ModeExclusive)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if !res.OK {
		t.Fatalf("claim denied: %s", res.Reason)
	}
	if res.Warning == "" {
		t.Fatal("no warning for a path outside the caller's directory")
	}
	if !strings.Contains(res.Warning, "/work/bot") {
		t.Fatalf("warning does not name the caller's directory: %q", res.Warning)
	}
}

// The ordinary case stays quiet: a path inside your own checkout is what you
// meant.
func TestClaimDoesNotWarnAboutItsOwnFiles(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)

	s, err := h.Join(ctx, JoinOptions{
		Room: "repo:bot", Harness: "test", Cwd: "/work/bot", PID: 4242,
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := h.Claim(ctx, s.ID, "/work/bot/src/users.go", 60, ModeExclusive)
	if err != nil {
		t.Fatal(err)
	}
	if res.Warning != "" {
		t.Fatalf("unexpected warning for an own file: %q", res.Warning)
	}
}

// A file: selector that matches nobody is the other half of the same trap:
// the pattern is matched against each holder's own checkout, so a relative
// path never reaches a holder in another repository.
func TestSendWarnsWhenAFileSelectorMatchesNobody(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)

	s, err := h.Join(ctx, JoinOptions{
		Room: "repo:bot", Harness: "test", Cwd: "/work/bot", PID: 4242,
	})
	if err != nil {
		t.Fatal(err)
	}
	msg, err := h.Send(ctx, SendOptions{
		From: s.ID, To: "file:src/users.go", Subject: "need a field", Body: "x",
	})
	if err != nil {
		t.Fatal(err)
	}
	if msg.Recipients != 0 {
		t.Fatalf("recipients = %d, want 0", msg.Recipients)
	}
	if msg.Warning == "" {
		t.Fatal("no warning for a file selector that reached nobody")
	}
	if !strings.Contains(msg.Warning, "absolute") {
		t.Fatalf("warning does not say how to fix it: %q", msg.Warning)
	}
}

// A selector that reaches someone is not a mistake, whatever its shape.
func TestSendIsQuietWhenAFileSelectorReachesSomeone(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)

	holder, err := h.Join(ctx, JoinOptions{
		Room: "repo:shop", Harness: "test", Cwd: "/work/shop", PID: 4242, Label: "shop",
	})
	if err != nil {
		t.Fatal(err)
	}
	sender, err := h.Join(ctx, JoinOptions{
		Room: "repo:shop", ExtraRooms: nil, Harness: "test", Cwd: "/work/shop", PID: 4343, Label: "other",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Claim(ctx, holder.ID, "/work/shop/src/users.go", 60, ModeExclusive); err != nil {
		t.Fatal(err)
	}
	msg, err := h.Send(ctx, SendOptions{
		From: sender.ID, To: "file:src/users.go", Subject: "x", Body: "y",
	})
	if err != nil {
		t.Fatal(err)
	}
	if msg.Recipients != 1 {
		t.Fatalf("recipients = %d, want 1", msg.Recipients)
	}
	if msg.Warning != "" {
		t.Fatalf("unexpected warning: %q", msg.Warning)
	}
}
