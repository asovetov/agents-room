package mcpserver

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/asovetov/agents-room/internal/hub"
	"github.com/asovetov/agents-room/internal/store"
)

// connect wires a client to a server over the in-memory transport, the same
// way a harness would connect over stdio.
func connect(t *testing.T) (*mcp.ClientSession, *hub.Hub) {
	t.Helper()
	h, err := hub.OpenPath(filepath.Join(t.TempDir(), "room.db"))
	if err != nil {
		t.Fatalf("open hub: %v", err)
	}
	t.Cleanup(func() { h.Close() })

	srv := New(h)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()

	ctx := context.Background()
	ss, err := srv.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("connect server: %v", err)
	}
	t.Cleanup(func() { ss.Close() })

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	cs, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("connect client: %v", err)
	}
	t.Cleanup(func() { cs.Close() })

	return cs, h
}

// call invokes a tool and fails the test on a protocol error.
func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) map[string]any {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      name,
		Arguments: args,
	})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if res.IsError {
		t.Fatalf("%s returned a tool error: %v", name, res.Content)
	}
	out, _ := res.StructuredContent.(map[string]any)
	return out
}

func TestToolsAreListed(t *testing.T) {
	cs, _ := connect(t)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	want := map[string]bool{
		"room_join": false, "room_leave": false, "room_leave_room": false, "room_who": false,
		"room_set_autonomy_cap": false,
		"msg_send":              false, "msg_recv": false, "msg_ack": false, "msg_unread": false,
		"msg_sent":    false,
		"lease_claim": false, "lease_renew": false, "lease_release": false,
		"lease_check": false, "lease_list": false,
		"note_read": false, "drift_check": false,
		"room_history": false,
	}
	for _, tool := range res.Tools {
		if _, ok := want[tool.Name]; ok {
			want[tool.Name] = true
		}
		if tool.Description == "" {
			t.Errorf("tool %q has no description", tool.Name)
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("tool %q is not registered", name)
		}
	}
}

func TestJoinSendRecvOverMCP(t *testing.T) {
	cs, _ := connect(t)
	ctx := context.Background()

	a := call(t, cs, "room_join", map[string]any{
		"room": "repo:x", "harness": "claude-code", "harness_key": "aaa", "label": "a",
	})
	b := call(t, cs, "room_join", map[string]any{
		"room": "repo:x", "harness": "codex", "harness_key": "bbb", "label": "b",
	})
	aID, _ := a["session_id"].(string)
	bID, _ := b["session_id"].(string)
	if aID == "" || bID == "" {
		t.Fatalf("join returned empty ids: a=%v b=%v", a, b)
	}

	call(t, cs, "msg_send", map[string]any{
		"session_id": aID, "to": bID, "subject": "protocol", "body": "dialect 42100",
	})

	unread := call(t, cs, "msg_unread", map[string]any{"session_id": bID})
	if n, _ := unread["count"].(float64); n != 1 {
		t.Fatalf("unread = %v, want 1", unread["count"])
	}

	recv := call(t, cs, "msg_recv", map[string]any{"session_id": bID})
	msgs, _ := recv["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("recv returned %d messages, want 1", len(msgs))
	}
	first, _ := msgs[0].(map[string]any)
	if first["body"] != "dialect 42100" {
		t.Fatalf("body = %v", first["body"])
	}

	// The sender must not receive its own broadcast either.
	call(t, cs, "msg_send", map[string]any{
		"session_id": aID, "to": "*", "subject": "all", "body": "hello",
	})
	self := call(t, cs, "msg_unread", map[string]any{"session_id": aID})
	if n, _ := self["count"].(float64); n != 0 {
		t.Fatalf("sender unread = %v, want 0", self["count"])
	}

	_ = ctx
}

func TestLeaseContentionOverMCP(t *testing.T) {
	cs, _ := connect(t)
	a := call(t, cs, "room_join", map[string]any{"room": "repo:x", "harness_key": "a"})
	b := call(t, cs, "room_join", map[string]any{"room": "repo:x", "harness_key": "b"})
	aID, _ := a["session_id"].(string)
	bID, _ := b["session_id"].(string)

	first := call(t, cs, "lease_claim", map[string]any{
		"session_id": aID, "path": "src/wire.go", "ttl_s": 60,
	})
	if ok, _ := first["ok"].(bool); !ok {
		t.Fatalf("first claim denied: %v", first)
	}

	second := call(t, cs, "lease_claim", map[string]any{
		"session_id": bID, "path": "src/wire.go", "ttl_s": 60,
	})
	if ok, _ := second["ok"].(bool); ok {
		t.Fatal("second claim on an exclusive lease succeeded")
	}
	holder, _ := second["holder"].(map[string]any)
	if holder["owner"] != aID {
		t.Fatalf("holder = %v, want %s", holder["owner"], aID)
	}

	check := call(t, cs, "lease_check", map[string]any{"session_id": bID, "path": "src/wire.go"})
	if free, _ := check["free"].(bool); free {
		t.Fatal("lease_check reports free while a lease is held")
	}

	call(t, cs, "lease_release", map[string]any{"session_id": aID, "path": "src/wire.go"})
	after := call(t, cs, "lease_check", map[string]any{"session_id": bID, "path": "src/wire.go"})
	if free, _ := after["free"].(bool); !free {
		t.Fatal("lease still held after release")
	}
}

func TestAutonomyCapOverMCP(t *testing.T) {
	cs, _ := connect(t)

	capRes := call(t, cs, "room_set_autonomy_cap", map[string]any{
		"room": "repo:x", "autonomy": hub.AutonomyHold,
	})
	if v, _ := capRes["autonomy"].(float64); int(v) != hub.AutonomyHold {
		t.Fatalf("cap = %v, want %d", capRes["autonomy"], hub.AutonomyHold)
	}

	joined := call(t, cs, "room_join", map[string]any{
		"room": "repo:x", "harness_key": "k", "autonomy": hub.AutonomyWake,
	})
	if v, _ := joined["autonomy"].(float64); int(v) != hub.AutonomyHold {
		t.Fatalf("session autonomy = %v, want it clamped to %d", joined["autonomy"], hub.AutonomyHold)
	}
}

// room_who promises to show who is alive, and last_seen is how it knows.
// Every tool call carrying a session_id has to move it, not only the ones
// that happen to pass through the wake path.
func TestToolCallsUpdateLastSeen(t *testing.T) {
	cs, h := connect(t)
	ctx := context.Background()

	joined := call(t, cs, "room_join", map[string]any{
		"room": "repo:x", "harness": "test", "harness_key": "k",
	})
	id, _ := joined["session_id"].(string)
	if id == "" {
		t.Fatalf("join returned no session id: %v", joined)
	}

	// Backdate last_seen far enough that a real update cannot be confused
	// with the same-second granularity of a fast test.
	const stale = 1
	db, err := store.Open(h.Path())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	if _, err := db.SQL().ExecContext(ctx,
		`UPDATE sessions SET last_seen = ? WHERE id = ?`, stale, id); err != nil {
		t.Fatalf("backdate last_seen: %v", err)
	}

	// A tool that does nothing to the session itself.
	call(t, cs, "msg_unread", map[string]any{"session_id": id})

	after, err := h.Who(ctx, "repo:x")
	if err != nil {
		t.Fatalf("who: %v", err)
	}
	if len(after) != 1 {
		t.Fatalf("who returned %d sessions, want 1", len(after))
	}
	if after[0].LastSeen == stale {
		t.Fatal("a tool call did not update last_seen; room_who would show a live session as stale")
	}
}

// A harness-spawned MCP server inherits the harness session id. room_join
// must use it when the model omits harness_key, or a chat that joins twice
// appears as two agents with one pid.
func TestJoinWithoutKeyReusesHarnessSession(t *testing.T) {
	t.Setenv("CLAUDE_CODE_SESSION_ID", "harness-chat-1")
	cs, h := connect(t)

	first := call(t, cs, "room_join", map[string]any{"room": "repo:x"})
	second := call(t, cs, "room_join", map[string]any{"room": "repo:x"})
	if first["session_id"] != second["session_id"] {
		t.Fatalf("two joins made two sessions: %v then %v", first["session_id"], second["session_id"])
	}

	who, err := h.Who(context.Background(), "repo:x")
	if err != nil {
		t.Fatalf("who: %v", err)
	}
	if len(who) != 1 {
		t.Fatalf("who returned %d sessions, want 1", len(who))
	}
}

// A send that reaches nobody must say so in its output instead of looking
// like a delivery.
func TestSendReportsRecipientCount(t *testing.T) {
	cs, _ := connect(t)
	joined := call(t, cs, "room_join", map[string]any{
		"room": "repo:x", "harness": "test", "harness_key": "a",
	})
	id, _ := joined["session_id"].(string)

	out := call(t, cs, "msg_send", map[string]any{
		"session_id": id, "to": "tag:nobody", "body": "x",
	})
	if n, _ := out["recipients"].(float64); n != 0 {
		t.Fatalf("recipients = %v, want 0", out["recipients"])
	}
}

func TestToolErrorsAreReported(t *testing.T) {
	cs, _ := connect(t)
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "msg_send",
		Arguments: map[string]any{"session_id": "does-not-exist", "to": "*", "body": "x"},
	})
	if err != nil {
		t.Fatalf("protocol error: %v", err)
	}
	if !res.IsError {
		t.Fatal("sending from an unknown session was not reported as a tool error")
	}
}

// A sender can find out what became of what it sent, which is the question a
// missing reply leaves open.
func TestMsgSentReportsDeliveryStates(t *testing.T) {
	cs, _ := connect(t)
	a := call(t, cs, "room_join", map[string]any{"room": "repo:x", "harness": "test", "harness_key": "a"})
	b := call(t, cs, "room_join", map[string]any{"room": "repo:x", "harness": "test", "harness_key": "b"})
	aID, _ := a["session_id"].(string)
	bID, _ := b["session_id"].(string)

	call(t, cs, "msg_send", map[string]any{
		"session_id": aID, "to": bID, "subject": "ping", "body": "x",
	})

	// Before b reads it, the message is pending on one recipient.
	sent := call(t, cs, "msg_sent", map[string]any{"session_id": aID})
	msgs, _ := sent["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("msg_sent returned %d messages, want 1", len(msgs))
	}
	first, _ := msgs[0].(map[string]any)
	if p, _ := first["pending"].(float64); p != 1 {
		t.Fatalf("pending = %v, want 1", first["pending"])
	}

	call(t, cs, "msg_recv", map[string]any{"session_id": bID})

	sent = call(t, cs, "msg_sent", map[string]any{"session_id": aID})
	msgs, _ = sent["messages"].([]any)
	first, _ = msgs[0].(map[string]any)
	if p, _ := first["pending"].(float64); p != 0 {
		t.Fatalf("pending after the recipient read it = %v, want 0", first["pending"])
	}
	states, _ := first["states"].(map[string]any)
	if states["read"] != float64(1) {
		t.Fatalf("states = %v, want one read", states)
	}
}

// A session joining a room with history gets one line saying so. Without it
// msg_recv's silence reads as an empty room, and the agent starts from zero
// on work that has been under way for hours.
func TestJoinMentionsRecentHistory(t *testing.T) {
	cs, _ := connect(t)
	ctx := context.Background()

	first := call(t, cs, "room_join", map[string]any{"room": "repo:x", "harness_key": "a"})
	sender, _ := first["session_id"].(string)
	call(t, cs, "msg_send", map[string]any{
		"session_id": sender, "to": "*", "subject": "phone field needed",
	})

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "room_join",
		Arguments: map[string]any{"room": "repo:x", "harness_key": "c"},
	})
	if err != nil {
		t.Fatal(err)
	}
	text := contentText(t, res)
	if !strings.Contains(text, "message(s) in the last day") {
		t.Fatalf("join did not mention recent history:\n%s", text)
	}
	if !strings.Contains(text, "room_history") {
		t.Fatalf("join did not point at room_history:\n%s", text)
	}
}

// room_history returns what was said, so a newcomer can catch up.
func TestRoomHistoryTool(t *testing.T) {
	cs, _ := connect(t)
	first := call(t, cs, "room_join", map[string]any{"room": "repo:x", "harness_key": "a"})
	sender, _ := first["session_id"].(string)
	call(t, cs, "msg_send", map[string]any{
		"session_id": sender, "to": "*", "subject": "phone field needed", "body": "for the profile screen",
	})

	out := call(t, cs, "room_history", map[string]any{"room": "repo:x"})
	msgs, _ := out["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("history returned %d messages, want 1: %v", len(msgs), out)
	}
	m, _ := msgs[0].(map[string]any)
	if m["subject"] != "phone field needed" {
		t.Fatalf("subject = %v", m["subject"])
	}
}

// contentText pulls the text out of a tool result.
func contentText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}
