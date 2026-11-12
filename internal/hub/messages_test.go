package hub

import (
	"context"
	"testing"
)

// A Hermes session has no socket, so without the CLI backend it must come
// back with no backend rather than be silently dropped or mistaken for one
// with a socket.
func TestWakersReportsBackends(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)

	// A Hermes session: no socket, a harness key, wake autonomy.
	hermes, err := h.Join(ctx, JoinOptions{
		Room: "repo:x", Harness: "hermes", HarnessKey: "local-key",
		Autonomy: AutonomyWake, AutonomySet: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	// A Claude Code session with a socket.
	sock, err := h.Join(ctx, JoinOptions{
		Room: "repo:x", Harness: "claude-code", HarnessKey: "cc-key",
		Autonomy: AutonomyWake, AutonomySet: true, WakeSocket: "/tmp/x.sock",
	})
	if err != nil {
		t.Fatal(err)
	}
	sender := join(t, h, "repo:x", "sender")

	for _, to := range []string{hermes.ID, sock.ID} {
		if _, err := h.Send(ctx, SendOptions{From: sender.ID, To: to, Subject: "s", Body: "b"}); err != nil {
			t.Fatal(err)
		}
	}

	// Backend off: the Hermes session has no lever, the socket one does.
	off, err := h.WakersWithHermesCLI(ctx, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]Waker{}
	for _, w := range off {
		byID[w.SessionID] = w
	}
	if byID[hermes.ID].Backend != "" {
		t.Fatalf("hermes backend = %q with the CLI backend off", byID[hermes.ID].Backend)
	}
	if byID[sock.ID].Backend != WakeSocket {
		t.Fatalf("socket backend = %q", byID[sock.ID].Backend)
	}

	// Backend on: the Hermes session gets a CLI backend and its key.
	on, err := h.WakersWithHermesCLI(ctx, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	byID = map[string]Waker{}
	for _, w := range on {
		byID[w.SessionID] = w
	}
	if byID[hermes.ID].Backend != WakeHermesCLI {
		t.Fatalf("hermes backend = %q with the CLI backend on", byID[hermes.ID].Backend)
	}
	if byID[hermes.ID].HarnessKey != "local-key" {
		t.Fatalf("hermes harness key = %q", byID[hermes.ID].HarnessKey)
	}
}

// Wakers, the old name, must stay off for Hermes: a caller that has not been
// taught about the opt-in must not start agent turns.
func TestWakersDefaultsToNoHermesBackend(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)
	s, err := h.Join(ctx, JoinOptions{
		Room: "repo:x", Harness: "hermes", HarnessKey: "local-key",
		Autonomy: AutonomyWake, AutonomySet: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	sender := join(t, h, "repo:x", "sender")
	if _, err := h.Send(ctx, SendOptions{From: sender.ID, To: s.ID, Subject: "s", Body: "b"}); err != nil {
		t.Fatal(err)
	}
	wakers, err := h.Wakers(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range wakers {
		if w.SessionID == s.ID && w.Backend != "" {
			t.Fatalf("Wakers gave a hermes session backend %q without opt-in", w.Backend)
		}
	}
}
