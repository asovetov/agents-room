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
	line := Nudge("repo:gcs", 3)
	if !strings.Contains(line, "3 unread") || !strings.Contains(line, "repo:gcs") {
		t.Fatalf("nudge = %q", line)
	}
	if !strings.Contains(line, "msg_recv") {
		t.Fatalf("nudge %q does not tell the agent how to fetch", line)
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
