//go:build !windows

package wake

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/asovetov/agents-room/internal/hub"
)

// shortSocketDir returns a directory short enough for a Unix socket path.
// macOS limits a socket path to about 104 bytes and t.TempDir() there is
// already longer than that.
func shortSocketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "ar")
	if err != nil {
		t.Skipf("short temp dir unavailable: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func TestNudgeLineCarriesNoBody(t *testing.T) {
	line := Nudge([]string{"repo:gcs"}, 3)
	if !strings.Contains(line, "3 unread") || !strings.Contains(line, "repo:gcs") {
		t.Fatalf("nudge = %q", line)
	}
	if !strings.Contains(line, "msg_recv") {
		t.Fatalf("nudge %q does not tell the agent how to fetch", line)
	}
}

// The nudge names the room the message came through. Naming the session's
// first room instead sent a link agent to its own repository room while the
// message sat in a shared one.
func TestNudgeNamesEveryRoom(t *testing.T) {
	line := Nudge([]string{"shared:storefront", "repo:storefront-web"}, 2)
	if !strings.Contains(line, "shared:storefront") {
		t.Fatalf("nudge %q does not name the shared room the message came through", line)
	}
	if !strings.Contains(line, "repo:storefront-web") {
		t.Fatalf("nudge %q does not name the repository room", line)
	}
	if !strings.Contains(line, "rooms ") {
		t.Fatalf("nudge %q names two rooms but does not say so", line)
	}

	// The line stays one line: past two rooms it counts the rest.
	many := Nudge([]string{"a", "b", "c", "d"}, 4)
	if strings.Count(many, "\n") != 0 {
		t.Fatalf("nudge %q is not one line", many)
	}
	if !strings.Contains(many, "2 more") {
		t.Fatalf("nudge %q does not say how many rooms it left out", many)
	}

	// A delivery whose rooms are all gone still has to produce a usable line.
	if bare := Nudge(nil, 1); !strings.Contains(bare, "1 unread") ||
		!strings.Contains(bare, "msg_recv") {
		t.Fatalf("nudge without a room = %q", bare)
	}
}

func TestCheckCountsAndTouches(t *testing.T) {
	ctx := context.Background()
	h, err := hub.OpenPath(filepath.Join(t.TempDir(), "room.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	a, err := h.Join(ctx, hub.JoinOptions{Room: "repo:x", Harness: "test", HarnessKey: "a"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := h.Join(ctx, hub.JoinOptions{Room: "repo:x", Harness: "test", HarnessKey: "b"})
	if err != nil {
		t.Fatal(err)
	}

	empty, err := Check(ctx, h, b.ID)
	if err != nil {
		t.Fatalf("check with nothing: %v", err)
	}
	if empty.Unread != 0 || empty.Line != "" {
		t.Fatalf("empty result = %+v", empty)
	}

	if _, err := h.Send(ctx, hub.SendOptions{From: a.ID, To: b.ID, Subject: "s", Body: "b"}); err != nil {
		t.Fatal(err)
	}
	res, err := Check(ctx, h, b.ID)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if res.Unread != 1 {
		t.Fatalf("unread = %d, want 1", res.Unread)
	}
	if !strings.Contains(res.Line, "repo:x") {
		t.Fatalf("line = %q, want the room name", res.Line)
	}

	// The hook marks what it signalled. A second Stop must not signal the
	// same message again: at wake level each signal is a fresh turn, and a
	// turn whose Stop signals again never ends.
	if _, err := h.MarkOffered(ctx, b.ID, hub.StateAccepted); err != nil {
		t.Fatalf("mark offered: %v", err)
	}
	again, err := Check(ctx, h, b.ID)
	if err != nil {
		t.Fatalf("check after offering: %v", err)
	}
	if again.Unread != 0 || again.Line != "" {
		t.Fatalf("a message already signalled was signalled again: %+v", again)
	}

	// It is still unread, so the agent still has something to fetch.
	if n, err := h.Unread(ctx, b.ID); err != nil || n != 1 {
		t.Fatalf("unread after offering = %d (%v), want 1", n, err)
	}

	// A new message reopens the lever, and the line counts both.
	if _, err := h.Send(ctx, hub.SendOptions{From: a.ID, To: b.ID, Subject: "s2", Body: "b2"}); err != nil {
		t.Fatal(err)
	}
	fresh, err := Check(ctx, h, b.ID)
	if err != nil {
		t.Fatalf("check with a new message: %v", err)
	}
	if fresh.Unread != 2 {
		t.Fatalf("unread = %d, want both the offered and the new one", fresh.Unread)
	}
}

// TestPostToSocketDelivers builds a real Unix socket that speaks the Claude
// Code inbox protocol and checks the exact bytes the hook sends.
func TestPostToSocketDelivers(t *testing.T) {
	path := filepath.Join(shortSocketDir(t), "inbox.sock")

	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	type received struct {
		auth    map[string]any
		message map[string]any
	}
	got := make(chan received, 1)

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		scanner := bufio.NewScanner(conn)
		var r received
		line := 0
		for scanner.Scan() {
			var obj map[string]any
			if err := json.Unmarshal(scanner.Bytes(), &obj); err != nil {
				continue
			}
			if line == 0 {
				r.auth = obj
			} else {
				r.message = obj
			}
			line++
		}
		got <- r
	}()

	if err := PostToSocket(path, "secret-token", "[agents-room] 1 unread"); err != nil {
		t.Fatalf("post: %v", err)
	}

	r := <-got
	if r.auth["type"] != "auth" || r.auth["token"] != "secret-token" {
		t.Fatalf("auth line = %v", r.auth)
	}
	if r.message["type"] != "user" {
		t.Fatalf("message type = %v, want user", r.message["type"])
	}
	msg, ok := r.message["message"].(map[string]any)
	if !ok {
		t.Fatalf("message payload = %v", r.message["message"])
	}
	if msg["role"] != "user" || msg["content"] != "[agents-room] 1 unread" {
		t.Fatalf("message = %v", msg)
	}
}

func TestPostToSocketOmitsAuthWhenNoToken(t *testing.T) {
	path := filepath.Join(shortSocketDir(t), "inbox.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	lines := make(chan int, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		scanner := bufio.NewScanner(conn)
		n := 0
		for scanner.Scan() {
			n++
		}
		lines <- n
	}()

	if err := PostToSocket(path, "", "hello"); err != nil {
		t.Fatalf("post: %v", err)
	}
	if n := <-lines; n != 1 {
		t.Fatalf("sent %d lines without a token, want 1", n)
	}
}

func TestPostToSocketReportsMissingSocket(t *testing.T) {
	err := PostToSocket(filepath.Join(t.TempDir(), "nope.sock"), "", "x")
	if err == nil {
		t.Fatal("posting to a missing socket succeeded")
	}
}

func TestSocketPathReadsEnv(t *testing.T) {
	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", "/tmp/x.sock")
	t.Setenv("CLAUDE_CODE_MESSAGING_TOKEN", "tok")
	if SocketPath() != "/tmp/x.sock" {
		t.Fatalf("SocketPath = %q", SocketPath())
	}
	if SocketToken() != "tok" {
		t.Fatalf("SocketToken = %q", SocketToken())
	}
	os.Unsetenv("CLAUDE_CODE_MESSAGING_SOCKET")
}

// A delivery offered once must still be announced again by an event that
// cannot hold a turn.
//
// There is no offered -> stored transition, so a single offer used to switch
// the wake signal off for that message for good. On the live store one session
// had eleven offered deliveries and not one read: the hook ran, built the
// line, and never spoke again about that message.
func TestCheckRetryReannouncesAnOfferedDelivery(t *testing.T) {
	ctx := context.Background()
	h, err := hub.OpenPath(filepath.Join(t.TempDir(), "room.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	a, err := h.Join(ctx, hub.JoinOptions{Room: "repo:x", Harness: "test", HarnessKey: "a"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := h.Join(ctx, hub.JoinOptions{Room: "repo:x", Harness: "test", HarnessKey: "b"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Send(ctx, hub.SendOptions{From: a.ID, To: b.ID, Subject: "s", Body: "b"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.MarkOffered(ctx, b.ID, hub.StateOffered); err != nil {
		t.Fatal(err)
	}

	// The strict form stays silent: that is what keeps Stop from looping.
	strict, err := Check(ctx, h, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if strict.Line != "" {
		t.Fatalf("Check re-signalled an offered delivery: %+v", strict)
	}

	// A tool event may retry at once, because it cannot extend a turn.
	retry, err := CheckRetry(ctx, h, b.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if retry.Line == "" {
		t.Fatal("CheckRetry stayed silent about a message nobody had fetched")
	}
	if retry.Unread != 1 {
		t.Fatalf("unread = %d, want 1", retry.Unread)
	}
}

// A Stop may retry only across turns, never inside one.
func TestCheckRetryHonoursTheWindow(t *testing.T) {
	ctx := context.Background()
	h, err := hub.OpenPath(filepath.Join(t.TempDir(), "room.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	a, err := h.Join(ctx, hub.JoinOptions{Room: "repo:x", Harness: "test", HarnessKey: "a"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := h.Join(ctx, hub.JoinOptions{Room: "repo:x", Harness: "test", HarnessKey: "b"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Send(ctx, hub.SendOptions{From: a.ID, To: b.ID, Subject: "s", Body: "b"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.MarkOffered(ctx, b.ID, hub.StateOffered); err != nil {
		t.Fatal(err)
	}

	// Inside the window: silent.
	res, err := CheckRetry(ctx, h, b.ID, 1800)
	if err != nil {
		t.Fatal(err)
	}
	if res.Line != "" {
		t.Fatalf("retried inside the window: %+v", res)
	}
}

// A signal the harness accepted must not be sent again.
//
// `accepted` means the socket write returned OK, so at wake level a turn is
// already on its way. Re-announcing it would start another turn, and a turn
// whose event announces again never ends -- the loop the strict rule exists
// to prevent. Only `offered`, where the signal may not have landed, retries.
func TestCheckRetryNeverRepeatsAnAcceptedDelivery(t *testing.T) {
	ctx := context.Background()
	h, err := hub.OpenPath(filepath.Join(t.TempDir(), "room.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	a, err := h.Join(ctx, hub.JoinOptions{Room: "repo:x", Harness: "test", HarnessKey: "a"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := h.Join(ctx, hub.JoinOptions{Room: "repo:x", Harness: "test", HarnessKey: "b"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Send(ctx, hub.SendOptions{From: a.ID, To: b.ID, Subject: "s", Body: "b"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.MarkOffered(ctx, b.ID, hub.StateAccepted); err != nil {
		t.Fatal(err)
	}

	res, err := CheckRetry(ctx, h, b.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Line != "" {
		t.Fatalf("re-signalled an accepted delivery: %+v", res)
	}
}

// The nudge names the room the message came through, not the room the
// session happened to join first.
//
// A link in repo:storefront-web met the firmware in shared:storefront. The
// message was filed under both, but the nudge said "1 unread in room
// repo:storefront-web" because that was the session's oldest membership, and
// the agent went looking in a room that had nothing new.
func TestCheckNamesTheRoomTheMessageCameThrough(t *testing.T) {
	ctx := context.Background()
	h, err := hub.OpenPath(filepath.Join(t.TempDir(), "room.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	link, err := h.Join(ctx, hub.JoinOptions{
		Room: "repo:storefront-web", ExtraRooms: []string{"shared:storefront"},
		Harness: "test", HarnessKey: "link",
	})
	if err != nil {
		t.Fatal(err)
	}
	fw, err := h.Join(ctx, hub.JoinOptions{
		Room: "repo:storefront-mobile", ExtraRooms: []string{"shared:storefront"},
		Harness: "test", HarnessKey: "fw",
	})
	if err != nil {
		t.Fatal(err)
	}

	// The firmware writes from its own repository room; the message belongs to
	// every room it is in, which is how the link can see it at all.
	if _, err := h.Send(ctx, hub.SendOptions{
		From: fw.ID, To: link.ID, Subject: "s", Body: "b",
	}); err != nil {
		t.Fatal(err)
	}

	res, err := Check(ctx, h, link.ID)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !strings.Contains(res.Line, "shared:storefront") {
		t.Fatalf("nudge %q does not name the shared room the message came through", res.Line)
	}

	// A room the reader is not a member of is never named: it is not where the
	// reader can go and read anything.
	if strings.Contains(res.Line, "repo:storefront-mobile") {
		t.Fatalf("nudge %q names a room the reader is not in", res.Line)
	}
}
