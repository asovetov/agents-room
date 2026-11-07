package hook

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
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
