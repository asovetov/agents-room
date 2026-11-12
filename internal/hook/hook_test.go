package hook

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/asovetov/agents-room/internal/hub"
)

func TestNormalizeEvent(t *testing.T) {
	cases := []struct {
		harness, event, want string
	}{
		{"claude-code", "SessionStart", eventJoin},
		{"claude-code", "SessionEnd", eventLeave},
		{"claude-code", "Stop", eventNudge},
		{"claude-code", "PostToolBatch", eventNudge},
		{"claude-code", "Notification", ""},
		{"hermes", "on_session_start", eventJoin},
		{"hermes", "on_session_finalize", eventLeave},
		{"hermes", "pre_llm_call", eventNudge},
		{"hermes", "pre_verify", eventNudge},
		{"hermes", "post_tool_call", ""},
		{"codex", "SessionStart", eventJoin},
		{"codex", "SessionEnd", eventLeave},
		{"codex", "Stop", eventNudge},
		{"codex", "UserPromptSubmit", eventNudge},
		{"codex", "PostToolUse", eventNudge},
	}
	for _, c := range cases {
		if got := normalizeEvent(c.harness, c.event); got != c.want {
			t.Errorf("normalizeEvent(%q, %q) = %q, want %q", c.harness, c.event, got, c.want)
		}
	}
}

func TestClaudeCodeStopBlocksOnlyAtHoldOrAbove(t *testing.T) {
	line := "[agents-room] 1 unread; call msg_recv"

	notify := outputFor("claude-code", "Stop", hub.AutonomyNotify, line)
	if notify != "" {
		t.Fatalf("notify level produced output %q, want silence", notify)
	}

	held := outputFor("claude-code", "Stop", hub.AutonomyHold, line)
	var out map[string]any
	if err := json.Unmarshal([]byte(held), &out); err != nil {
		t.Fatalf("stop output is not JSON: %v", err)
	}
	if out["decision"] != "block" {
		t.Fatalf("decision = %v, want block", out["decision"])
	}
	if out["reason"] != line {
		t.Fatalf("reason = %v, want the nudge line", out["reason"])
	}
}

func TestClaudeCodeUserPromptSubmitNestsContext(t *testing.T) {
	line := "[agents-room] 2 unread; call msg_recv"
	raw := outputFor("claude-code", "UserPromptSubmit", hub.AutonomyNotify, line)

	var out struct {
		HookSpecific struct {
			EventName string `json:"hookEventName"`
			Context   string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}
	// A top-level additionalContext is silently ignored by Claude Code, so
	// the nesting is the contract, not a style choice.
	if out.HookSpecific.EventName != "UserPromptSubmit" {
		t.Fatalf("hookEventName = %q", out.HookSpecific.EventName)
	}
	if out.HookSpecific.Context != line {
		t.Fatalf("additionalContext = %q, want the nudge line", out.HookSpecific.Context)
	}
}

func TestHermesOutputs(t *testing.T) {
	line := "[agents-room] 1 unread; call msg_recv"

	var ctxOut map[string]any
	if err := json.Unmarshal([]byte(outputFor("hermes", "pre_llm_call", hub.AutonomyNotify, line)), &ctxOut); err != nil {
		t.Fatalf("hermes pre_llm_call output: %v", err)
	}
	if ctxOut["context"] != line {
		t.Fatalf("context = %v, want the nudge line", ctxOut["context"])
	}

	// Hermes pre_verify holds only when autonomy allows it.
	if got := outputFor("hermes", "pre_verify", hub.AutonomyNotify, line); got != "" {
		t.Fatalf("notify produced %q on pre_verify, want silence", got)
	}
	var verify map[string]any
	if err := json.Unmarshal([]byte(outputFor("hermes", "pre_verify", hub.AutonomyHold, line)), &verify); err != nil {
		t.Fatalf("hermes pre_verify output: %v", err)
	}
	if verify["action"] != "continue" || verify["message"] != line {
		t.Fatalf("pre_verify = %v, want action continue with the line", verify)
	}
}

func TestCodexOutputContract(t *testing.T) {
	line := "[agents-room] 1 unread; call msg_recv"

	// Codex ignores plain stdout on Stop, so a nudge at notify level must be
	// silent rather than a line nobody reads.
	if got := outputFor("codex", "Stop", hub.AutonomyNotify, line); got != "" {
		t.Fatalf("codex Stop at notify produced %q, want silence", got)
	}

	var stop map[string]any
	if err := json.Unmarshal([]byte(outputFor("codex", "Stop", hub.AutonomyHold, line)), &stop); err != nil {
		t.Fatalf("codex Stop output is not JSON: %v", err)
	}
	if stop["decision"] != "block" || stop["reason"] != line {
		t.Fatalf("codex Stop = %v, want decision block with the line", stop)
	}

	// UserPromptSubmit and the tool events use the JSON envelope, not plain
	// text: Codex ignores plain stdout there.
	for _, event := range []string{"UserPromptSubmit", "PostToolUse", "PreToolUse"} {
		var out struct {
			HookSpecific struct {
				EventName string `json:"hookEventName"`
				Context   string `json:"additionalContext"`
			} `json:"hookSpecificOutput"`
		}
		raw := outputFor("codex", event, hub.AutonomyNotify, line)
		if err := json.Unmarshal([]byte(raw), &out); err != nil {
			t.Fatalf("codex %s output is not JSON: %q", event, raw)
		}
		if out.HookSpecific.EventName != event || out.HookSpecific.Context != line {
			t.Fatalf("codex %s = %+v", event, out.HookSpecific)
		}
	}
}

// A Stop hook that blocks a Stop which is itself the continuation of a block
// never lets the turn end. Claude Code marks that case with stop_hook_active,
// and the hook must go quiet on it whatever the autonomy level says.
func TestStopHookDoesNotBlockItsOwnContinuation(t *testing.T) {
	ctx := context.Background()
	h, err := hub.OpenPath(filepath.Join(t.TempDir(), "room.db"))
	if err != nil {
		t.Fatalf("open hub: %v", err)
	}
	defer h.Close()

	self, err := h.Join(ctx, hub.JoinOptions{
		Room: "repo:x", Harness: "claude-code", HarnessKey: "k",
		Autonomy: hub.AutonomyHold,
	})
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	other, err := h.Join(ctx, hub.JoinOptions{
		Room: "repo:x", Harness: "test", HarnessKey: "other", Autonomy: hub.AutonomyNotify,
	})
	if err != nil {
		t.Fatalf("join other: %v", err)
	}
	if _, err := h.Send(ctx, hub.SendOptions{From: other.ID, To: self.ID, Body: "hi"}); err != nil {
		t.Fatalf("send: %v", err)
	}

	// The first Stop is allowed to hold: the guard must not silence it.
	if got := captureStdout(t, func() int {
		return nudge(ctx, h, "claude-code", "repo:x", "k", "Stop", false)
	}); got == "" {
		t.Fatal("a Stop that is not a continuation produced no hold, so the guard proves nothing")
	}

	// The continuation must be silent, or the loop never ends.
	if got := captureStdout(t, func() int {
		return nudge(ctx, h, "claude-code", "repo:x", "k", "Stop", true)
	}); got != "" {
		t.Fatalf("a Stop continuing a block produced %q, which is the loop", got)
	}
}

// captureStdout runs fn and returns what it wrote to stdout.
func captureStdout(t *testing.T, fn func() int) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	code := fn()
	w.Close()
	os.Stdout = old

	out, err := io.ReadAll(r)
	r.Close()
	if err != nil {
		t.Fatalf("read captured stdout: %v", err)
	}
	if code != 0 {
		t.Fatalf("hook returned %d", code)
	}
	return strings.TrimSpace(string(out))
}

func TestStopHookActiveParsesFromPayload(t *testing.T) {
	in := readInputFrom(strings.NewReader(`{"session_id":"s","stop_hook_active":true}`))
	if !in.StopHookActive {
		t.Fatal("stop_hook_active was not read from the payload")
	}
	if in.SessionID != "s" {
		t.Fatalf("session id = %q", in.SessionID)
	}

	// Absent means false: most events carry no such field.
	if readInputFrom(strings.NewReader(`{"session_id":"s"}`)).StopHookActive {
		t.Fatal("stop_hook_active defaulted to true")
	}
}

func TestShortID(t *testing.T) {
	if got := shortID("  0123456789abcdef  "); got != "01234567" {
		t.Fatalf("shortID = %q", got)
	}
	if got := shortID("abc"); got != "abc" {
		t.Fatalf("shortID = %q", got)
	}
}

func TestClaudeCodeNotifyNeverBlocksOrWakes(t *testing.T) {
	line := "[agents-room] 1 unread; call msg_recv"

	// At notify level the signal must ride the current turn and never
	// produce a Stop block, because a Stop block is a hold.
	for _, event := range []string{"Stop", "PostToolBatch", "UserPromptSubmit"} {
		got := outputFor("claude-code", event, hub.AutonomyNotify, line)
		if event == "Stop" && got != "" {
			t.Fatalf("notify level produced a Stop block: %q", got)
		}
		if event != "Stop" && got == "" {
			t.Fatalf("notify level produced nothing for %s", event)
		}
	}
}

// A session that is doing anything at all keeps its leases: the hook renews
// them on every tool event, so an edit cannot outlive the TTL that guards it.
func TestToolEventRenewsTheSessionsLeases(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "room.db")
	h, err := hub.OpenPath(dbPath)
	if err != nil {
		t.Fatalf("open hub: %v", err)
	}
	self, err := h.Join(ctx, hub.JoinOptions{
		Room: "repo:x", Harness: "claude-code", HarnessKey: "k",
		Autonomy: hub.AutonomyNotify,
	})
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	if r, _ := h.Claim(ctx, self.ID, "src/x.go", 60, hub.ModeExclusive); !r.OK {
		t.Fatal("claim failed")
	}
	before, err := h.Check(ctx, self.ID, "src/x.go")
	if err != nil || before == nil {
		t.Fatalf("check: %v", err)
	}
	h.Close()

	// The hook opens the store itself, so point it at the same file.
	t.Setenv("AGENTS_ROOM_DB", dbPath)
	t.Setenv("AGENTS_ROOM_PID", "4242")
	if code := runWithInput("claude-code", "PostToolBatch", "repo:x", hub.AutonomyNotify,
		hookInput{SessionID: "k", Cwd: t.TempDir()}); code != 0 {
		t.Fatalf("hook exit %d", code)
	}

	h2, err := hub.OpenPath(dbPath)
	if err != nil {
		t.Fatalf("reopen hub: %v", err)
	}
	defer h2.Close()
	after, err := h2.Check(ctx, self.ID, "src/x.go")
	if err != nil || after == nil {
		t.Fatalf("check after: %v", err)
	}
	if after.ExpiresAt <= before.ExpiresAt {
		t.Fatalf("lease expiry did not move (%d -> %d); the tool event did not renew it",
			before.ExpiresAt, after.ExpiresAt)
	}
}

// A tool event proves the session is alive, so it has to move last_seen as
// well as renew leases.
//
// Only the MCP middleware used to touch last_seen, and an agent editing files
// for an hour without calling an MCP tool never reaches it. `who` then calls a
// working chat quiet, and doctor blames the hook that is in fact running.
func TestNudgeTouchesLastSeen(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "room.db")
	t.Setenv("AGENTS_ROOM_DB", dbPath)

	h, err := hub.OpenPath(dbPath)
	if err != nil {
		t.Fatalf("open hub: %v", err)
	}
	defer h.Close()

	if _, err := h.Join(ctx, hub.JoinOptions{
		Room: "repo:x", Harness: "claude-code", HarnessKey: "k",
	}); err != nil {
		t.Fatalf("join: %v", err)
	}
	before, err := h.FindSession(ctx, "repo:x", "claude-code", "k")
	if err != nil || before == nil {
		t.Fatalf("find before: %v", err)
	}
	// last_seen has second resolution, so the touch has to land in a later
	// second or the comparison proves nothing. Waiting is honest here: the
	// alternative is a clock hook the hook package does not expose.
	time.Sleep(1100 * time.Millisecond)

	captureStdout(t, func() int {
		return runWithInput("claude-code", "PostToolBatch", "repo:x", 0, hookInput{
			SessionID: "k", Cwd: "/tmp",
		})
	})

	after, err := h.FindSession(ctx, "repo:x", "claude-code", "k")
	if err != nil || after == nil {
		t.Fatalf("find after: %v", err)
	}
	if after.LastSeen <= before.LastSeen {
		t.Fatalf("last_seen did not move: %d -> %d", before.LastSeen, after.LastSeen)
	}
}

// retryWindow is what keeps the fix safe: a tool event may re-announce an
// offered message, a Stop may not, because only Stop can hold a turn.
func TestRetryWindowIsZeroForToolEventsAndNotForStop(t *testing.T) {
	if got := retryWindow("PostToolBatch"); got != 0 {
		t.Fatalf("PostToolBatch window = %d, want 0: it cannot extend a turn", got)
	}
	if got := retryWindow("UserPromptSubmit"); got != 0 {
		t.Fatalf("UserPromptSubmit window = %d, want 0", got)
	}
	if got := retryWindow("Stop"); got == 0 {
		t.Fatal("Stop window is 0: a Stop would re-signal inside its own turn and never let the chat end")
	}
}

// The end-to-end shape of the bug: a message is offered once, never fetched,
// and a later tool event must announce it again.
func TestToolEventReannouncesAnOfferedMessage(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "room.db")
	t.Setenv("AGENTS_ROOM_DB", dbPath)

	h, err := hub.OpenPath(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	self, err := h.Join(ctx, hub.JoinOptions{
		Room: "repo:x", Harness: "claude-code", HarnessKey: "k", Autonomy: hub.AutonomyNotify,
	})
	if err != nil {
		t.Fatal(err)
	}
	other, err := h.Join(ctx, hub.JoinOptions{
		Room: "repo:x", Harness: "test", HarnessKey: "other",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Send(ctx, hub.SendOptions{From: other.ID, To: self.ID, Body: "hi"}); err != nil {
		t.Fatal(err)
	}

	// First event announces it and marks it offered.
	first := captureStdout(t, func() int {
		return runWithInput("claude-code", "PostToolBatch", "repo:x", 0, hookInput{
			SessionID: "k", Cwd: "/tmp",
		})
	})
	if !strings.Contains(first, "unread") {
		t.Fatalf("first tool event said nothing: %q", first)
	}
	// The agent did not fetch it, which is the case that used to go silent.
	second := captureStdout(t, func() int {
		return runWithInput("claude-code", "PostToolBatch", "repo:x", 0, hookInput{
			SessionID: "k", Cwd: "/tmp",
		})
	})
	if !strings.Contains(second, "unread") {
		t.Fatalf("a later tool event stayed silent about a message nobody fetched: %q", second)
	}
}

// A wake signal has to reach the recipient's inbox, not the sender's.
//
// The socket is read from the process environment, and a hook runs under the
// environment of the chat it belongs to. So the sender's hook used to post to
// the sender's own socket: the sender was told about its own message and the
// recipient, sitting idle, was never told anything. That is why an idle chat
// had to be woken by hand while the delivery sat in `stored`.
//
// This test gives each side its own socket and checks which one is spoken to.
func TestWakeSignalReachesTheRecipientsSocket(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix sockets")
	}
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "room.db")
	t.Setenv("AGENTS_ROOM_DB", dbPath)

	h, err := hub.OpenPath(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	// Two chats, each with its own inbox socket recorded at join time.
	senderSock, senderGot := listenInbox(t)
	recvSock, recvGot := listenInbox(t)

	sender, err := h.Join(ctx, hub.JoinOptions{
		Room: "repo:x", Harness: "claude-code", HarnessKey: "sender",
		Autonomy: hub.AutonomyWake, AutonomySet: true, WakeSocket: senderSock,
	})
	if err != nil {
		t.Fatal(err)
	}
	recv, err := h.Join(ctx, hub.JoinOptions{
		Room: "repo:x", Harness: "claude-code", HarnessKey: "recv",
		Autonomy: hub.AutonomyWake, AutonomySet: true, WakeSocket: recvSock,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Send(ctx, hub.SendOptions{
		From: sender.ID, To: recv.ID, Subject: "s", Body: "b",
	}); err != nil {
		t.Fatal(err)
	}

	// The sender's hook runs. Its own environment names its own socket, which
	// is exactly the trap: reading the environment here reaches the wrong
	// chat.
	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", senderSock)
	captureStdout(t, func() int {
		return runWithInput("claude-code", "PostToolBatch", "repo:x", hub.AutonomyWake, hookInput{
			SessionID: "sender", Cwd: "/tmp",
		})
	})

	select {
	case line := <-recvGot:
		if !strings.Contains(line, "msg_recv") {
			t.Fatalf("the recipient was told %q, want the nudge line", line)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the recipient's inbox was never written: the signal went somewhere else")
	}

	// And nothing was posted back to the sender about its own message.
	select {
	case line := <-senderGot:
		t.Fatalf("the sender's own inbox was written (%q); the signal went to the wrong chat", line)
	case <-time.After(150 * time.Millisecond):
	}
}

// listenInbox accepts one connection on a fresh unix socket and reports the
// message line it carried.
func listenInbox(t *testing.T) (path string, got <-chan string) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "hookinbox")
	if err != nil {
		t.Skipf("short temp dir unavailable: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path = filepath.Join(dir, "inbox.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Skipf("unix socket unavailable: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	ch := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		scanner := bufio.NewScanner(conn)
		for scanner.Scan() {
			var obj map[string]any
			if json.Unmarshal(scanner.Bytes(), &obj) != nil {
				continue
			}
			if msg, ok := obj["message"].(map[string]any); ok {
				if content, ok := msg["content"].(string); ok {
					ch <- content
					return
				}
			}
		}
	}()
	return path, ch
}

// Waking idle recipients does not depend on the caller's own session being
// resolvable. The hook that fires belongs to whichever chat acted; the
// messages waiting are usually somebody else's, and the caller may not even
// be in the room it is running in.
func TestWakingIsIndependentOfTheCallersSession(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix sockets")
	}
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "room.db")
	t.Setenv("AGENTS_ROOM_DB", dbPath)

	h, err := hub.OpenPath(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	senderSock, _ := listenInbox(t)
	recvSock, recvGot := listenInbox(t)
	sender, err := h.Join(ctx, hub.JoinOptions{
		Room: "repo:x", Harness: "claude-code", HarnessKey: "sender",
		Autonomy: hub.AutonomyWake, AutonomySet: true, WakeSocket: senderSock,
	})
	if err != nil {
		t.Fatal(err)
	}
	recv, err := h.Join(ctx, hub.JoinOptions{
		Room: "repo:x", Harness: "claude-code", HarnessKey: "recv",
		Autonomy: hub.AutonomyWake, AutonomySet: true, WakeSocket: recvSock,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Send(ctx, hub.SendOptions{
		From: sender.ID, To: recv.ID, Subject: "s", Body: "b",
	}); err != nil {
		t.Fatal(err)
	}

	// A hook whose session is in no room at all: an operator running the
	// binary by hand, or a chat whose room could not be derived. It must
	// still wake the recipient.
	captureStdout(t, func() int {
		return runWithInput("claude-code", "PostToolBatch", "repo:nowhere", hub.AutonomyWake,
			hookInput{SessionID: "stranger", Cwd: "/tmp"})
	})

	select {
	case <-recvGot:
	case <-time.After(2 * time.Second):
		t.Fatal("a hook with no session of its own did not wake the recipient")
	}
}

// Hermes reports the hook process's directory as the session's, and that
// process lives in the Hermes install tree. Without the lookup from Hermes'
// own store every session landed in repo:.hermes, so a message addressed to
// it never found it.
func TestHermesJoinUsesTheSessionsOwnDirectory(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "room.db")
	t.Setenv("AGENTS_ROOM_DB", dbPath)
	t.Setenv("AGENTS_ROOM_PID", "4242")

	// A stand-in for Hermes' store: the session was started in a checkout,
	// while the payload will claim the install directory.
	hermesHome := t.TempDir()
	t.Setenv("HERMES_HOME", hermesHome)
	store, err := sql.Open("sqlite", "file:"+filepath.Join(hermesHome, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Exec(`CREATE TABLE sessions (id TEXT PRIMARY KEY, cwd TEXT)`); err != nil {
		t.Fatal(err)
	}
	checkout := filepath.Join(t.TempDir(), "shop")
	if err := os.MkdirAll(checkout, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Exec(`INSERT INTO sessions (id, cwd) VALUES ('local-key', ?)`, checkout); err != nil {
		t.Fatal(err)
	}
	store.Close()

	installDir := filepath.Join(t.TempDir(), ".hermes")
	if code := runWithInput("hermes", "on_session_start", "", hub.AutonomyWake, hookInput{
		SessionID: "local-key", Cwd: installDir,
	}); code != 0 {
		t.Fatalf("hook exit %d", code)
	}

	h, err := hub.OpenPath(dbPath)
	if err != nil {
		t.Fatalf("reopen hub: %v", err)
	}
	defer h.Close()

	want := hub.RepoRoom(checkout)
	sessions, err := h.Who(ctx, want)
	if err != nil {
		t.Fatalf("who %s: %v", want, err)
	}
	if len(sessions) != 1 {
		t.Fatalf("session not in %s; it landed in the payload's directory instead", want)
	}
	if sessions[0].Cwd != checkout {
		t.Fatalf("session cwd = %q, want the checkout %q", sessions[0].Cwd, checkout)
	}
}

// When Hermes' store has no answer the payload is used, so a machine without
// Hermes or a session that is not in the store still joins somewhere.
func TestHermesJoinFallsBackToThePayloadDirectory(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "room.db")
	t.Setenv("AGENTS_ROOM_DB", dbPath)
	t.Setenv("AGENTS_ROOM_PID", "4242")
	t.Setenv("HERMES_HOME", filepath.Join(t.TempDir(), "absent"))

	dir := filepath.Join(t.TempDir(), "plain")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if code := runWithInput("hermes", "on_session_start", "", hub.AutonomyWake, hookInput{
		SessionID: "local-key", Cwd: dir,
	}); code != 0 {
		t.Fatalf("hook exit %d", code)
	}

	h, err := hub.OpenPath(dbPath)
	if err != nil {
		t.Fatalf("reopen hub: %v", err)
	}
	defer h.Close()
	sessions, err := h.Who(ctx, hub.RepoRoom(dir))
	if err != nil || len(sessions) != 1 {
		t.Fatalf("session not in the payload's room: %v (%d sessions)", err, len(sessions))
	}
}
