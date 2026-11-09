// Package hook is the bridge between a harness event and the room. It runs
// as `agents-room hook` and decides, per event, whether to join, nudge, or
// stay quiet.
//
// It prints to stdout only when the calling harness is meant to consume the
// output. A SessionStart hook that prints would inject that text into the
// model context, which is exactly what the design avoids.
package hook

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/asovetov/agents-room/internal/hub"
	"github.com/asovetov/agents-room/internal/wake"
)

// hookInput is the subset of the harness payload we use. The field names are
// shared by Claude Code and Hermes.
type hookInput struct {
	SessionID string         `json:"session_id"`
	Cwd       string         `json:"cwd"`
	Source    string         `json:"source"`
	Extra     map[string]any `json:"extra"`
	// StopHookActive is set by Claude Code on a Stop that is already
	// continuing a turn because a previous Stop blocked it. Blocking again
	// from here is how a hook becomes an endless loop: the agent never
	// finishes, and each block starts another turn.
	StopHookActive bool `json:"stop_hook_active"`
}

// Run executes one hook invocation and returns the process exit code.
//
// It is called as `agents-room hook <flags>` from a harness configuration.
func Run(args []string) int {
	fs := flag.NewFlagSet("hook", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	harness := fs.String("harness", "claude-code", "which harness is calling: claude-code, codex or hermes")
	event := fs.String("event", "", "harness event name, e.g. SessionStart or Stop")
	autonomy := fs.Int("autonomy", hub.AutonomyNotify, "0 notify, 1 hold, 2 wake")
	room := fs.String("room", "", "override the room name")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	return run(*harness, *event, *room, *autonomy)
}

func run(harness, event, room string, autonomy int) int {
	return runWithInput(harness, event, room, autonomy, readInput())
}

// runWithInput is run over an already-decoded payload, so a test can drive a
// whole event without replacing the process's stdin.
func runWithInput(harness, event, room string, autonomy int, in hookInput) int {
	flagRoom := room
	// Hooks have small time budgets: 30 seconds on UserPromptSubmit, 1.5
	// seconds shared on SessionEnd. Stay well inside the smallest one.
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	if in.Cwd == "" {
		in.Cwd, _ = os.Getwd()
	}
	if in.SessionID == "" {
		// Without a harness session id there is nothing to key a session
		// on; stay silent rather than create an orphan.
		return 0
	}

	h, err := hub.Open()
	if err != nil {
		// The room being unavailable must never break the harness.
		return 0
	}
	defer h.Close()

	roomName := room
	if roomName == "" {
		roomName = hub.RepoRoom(in.Cwd)
	}

	switch normalizeEvent(harness, event) {
	case eventJoin:
		// Shared rooms are discovered, not asked for: a session joins the
		// repository room and every shared room configured for that
		// repository or for the machine. Without this every agent would
		// have to remember --also, and in practice none would.
		extra := hub.SharedRoomsFrom(in.Cwd)
		if flagRoom != "" {
			// An explicit room on the command line replaces discovery, so an
			// operator can run one session in isolation.
			extra = nil
		}
		// Rejoining is idempotent: SessionStart fires on startup, resume,
		// clear, compact and fork, and all of them are the same chat.
		//
		// The hook is the one caller that always states a level: its
		// --autonomy comes from the harness configuration, which is the
		// operator's decision. A model calling room_join usually omits the
		// field, and that must not undo this.
		// The inbox socket is recorded with the session, because the process
		// that will need it is not this one: another chat's hook posts here to
		// wake this chat while it is idle, and only this process can see its
		// own socket in the environment.
		if _, err := h.Join(ctx, hub.JoinOptions{
			Room: roomName, ExtraRooms: extra, Harness: harness,
			HarnessKey: in.SessionID, Cwd: in.Cwd,
			Autonomy: autonomy, AutonomySet: true,
			WakeSocket: wake.SocketPath(),
			Label:      shortID(in.SessionID),
		}); err != nil {
			return 0
		}
		return 0

	case eventLeave:
		if s, err := h.FindSession(ctx, roomName, harness, in.SessionID); err == nil && s != nil {
			h.Leave(ctx, s.ID)
		}
		return 0

	case eventNudge:
		// Every tool event is proof that the session is still working, which
		// is exactly when its leases must not lapse. A session that does
		// nothing at all is the one that should lose them.
		//
		// The same event is proof the session is alive, so last_seen moves
		// here too. It used to move only through the MCP middleware, which
		// misses the case where the agent works without calling an MCP tool:
		// a session editing files for an hour never touches the room, `who`
		// calls it quiet, and doctor blames a hook that is in fact running.
		if s, err := h.FindSession(ctx, roomName, harness, in.SessionID); err == nil && s != nil {
			_, _ = h.RenewAll(ctx, s.ID, leaseTTL)
			_ = h.Touch(ctx, s.ID)
		}
		return nudge(ctx, h, harness, roomName, in.SessionID, event, in.StopHookActive)
	}
	return 0
}

// leaseTTL is the lifetime given to a lease on renewal.
//
// It matches the hub default, so a session that keeps working keeps its
// lease for as long as it keeps working, and a session that stops loses it
// after the same half hour as before.
const leaseTTL = 1800

// Event kinds the hook reacts to.
const (
	eventJoin  = "join"
	eventLeave = "leave"
	eventNudge = "nudge"
)

// normalizeEvent maps harness-specific event names onto the three things the
// hook does.
func normalizeEvent(harness, event string) string {
	switch harness {
	case "hermes":
		switch event {
		case "on_session_start", "SessionStart":
			return eventJoin
		case "on_session_finalize", "on_session_end", "SessionEnd":
			return eventLeave
		case "pre_llm_call", "pre_tool_call", "pre_verify", "Stop", "PostToolBatch":
			return eventNudge
		}
		return ""
	default:
		switch event {
		case "SessionStart":
			return eventJoin
		case "SessionEnd":
			return eventLeave
		case "Stop", "PostToolBatch", "UserPromptSubmit", "PostToolUse", "pre_verify":
			return eventNudge
		}
		return ""
	}
}

// nudge emits a wake signal when the session has unread messages.
//
// The body stays in the store. The signal is one line.
//
// stopActive reports that the harness is already running a turn this hook
// asked for. It is not re-blocked: the turn has to be allowed to end, or the
// agent never reaches the recv the nudge was asking for and the Stop hook
// loops forever.
func nudge(ctx context.Context, h *hub.Hub, harness, roomName, harnessKey, event string, stopActive bool) int {
	// Every tool event is the moment to look for chats that cannot announce
	// themselves. This runs before anything about the caller is resolved,
	// because the hook that happens to fire belongs to whichever chat acted
	// and the messages waiting are usually somebody else's: waking only when
	// the caller has mail, or only when the caller's room could be derived,
	// is how an idle recipient was never woken at all.
	wakeIdleRecipients(ctx, h, retryWindow(event))

	self, err := h.FindSession(ctx, roomName, harness, harnessKey)
	if err != nil || self == nil {
		return 0
	}

	res, err := wake.CheckRetry(ctx, h, self.ID, retryWindow(event))
	if err != nil || res.Unread == 0 {
		return 0
	}

	if stopActive {
		// The harness is already continuing a turn because a Stop hook asked
		// it to. Asking again is the loop: the agent is never allowed to end,
		// and every block starts another turn. Stay silent, and let the
		// message be offered again on the next Stop that is not a
		// continuation.
		return 0
	}

	out := outputFor(harness, event, self.Autonomy, res.Line)
	if out == "" {
		// Nothing went out for this event at this level, and the delivery
		// stays signallable. Marking it offered here would record a signal
		// that was never emitted, and would hide the message from the next
		// Stop that can actually hold.
		return 0
	}
	h.MarkOffered(ctx, self.ID, hub.StateOffered)
	fmt.Fprintln(os.Stdout, out)
	return 0
}

// wakeIdleRecipients posts a nudge to every session that has a message waiting
// and a socket to receive it, and returns how many posts were delivered.
//
// This is the only lever that starts a turn in an idle chat, so it is the only
// way a message reaches an agent that is not already doing something -- which
// is most of the time. It runs on the *sender's* hook, so every socket comes
// from the recipient's row: this process has no idea where the other chats
// live, and the one socket it can see is its own.
//
// A session that recorded no socket is skipped rather than guessed at. Its
// messages stay stored and are announced the next time it runs anything, which
// is the honest behaviour the protocol promises for a harness with no wake
// lever.
func wakeIdleRecipients(ctx context.Context, h *hub.Hub, minAge int64) int {
	wakers, err := h.Wakers(ctx, minAge)
	if err != nil {
		return 0
	}
	posted := 0
	for _, w := range wakers {
		if w.Socket == "" {
			continue
		}
		line := wake.Nudge(w.Rooms, w.Pending)
		if err := wake.PostToSocket(w.Socket, "", line); err != nil {
			// The chat may have exited without leaving; its row stays and its
			// messages wait, which is the safe direction.
			continue
		}
		h.MarkOffered(ctx, w.SessionID, hub.StateAccepted)
		posted++
	}
	return posted
}

// outputFor renders the response the given harness expects.
//
// Claude Code: Stop takes a top-level decision, other events take
// hookSpecificOutput.additionalContext. Hermes: pre_llm_call takes a context
// string, pre_verify takes an action. Codex: plain text.
func outputFor(harness, event string, autonomy int, line string) string {
	switch harness {
	case "hermes":
		switch event {
		case "pre_verify":
			if autonomy >= hub.AutonomyHold {
				return jsonString(map[string]any{"action": "continue", "message": line})
			}
		case "pre_llm_call":
			return jsonString(map[string]any{"context": line})
		}
		return ""

	case "codex":
		// Codex only reads plain stdout as additionalContext on
		// SessionStart, SubagentStart and UserPromptSubmit. Stop requires the
		// JSON envelope; plain text there is ignored.
		switch event {
		case "Stop":
			if autonomy >= hub.AutonomyHold {
				return jsonString(map[string]any{"decision": "block", "reason": line})
			}
			return ""
		case "UserPromptSubmit":
			return jsonString(map[string]any{
				"hookSpecificOutput": map[string]any{
					"hookEventName":     "UserPromptSubmit",
					"additionalContext": line,
				},
			})
		case "PostToolUse", "PreToolUse":
			return jsonString(map[string]any{
				"hookSpecificOutput": map[string]any{
					"hookEventName":     event,
					"additionalContext": line,
				},
			})
		}
		return ""

	default: // claude-code
		switch event {
		case "Stop":
			if autonomy >= hub.AutonomyHold {
				return jsonString(map[string]any{"decision": "block", "reason": line})
			}
		case "UserPromptSubmit":
			return jsonString(map[string]any{
				"hookSpecificOutput": map[string]any{
					"hookEventName":     "UserPromptSubmit",
					"additionalContext": line,
				},
			})
		case "PostToolBatch", "PostToolUse":
			return jsonString(map[string]any{
				"hookSpecificOutput": map[string]any{
					"hookEventName":     event,
					"additionalContext": line,
				},
			})
		}
		return ""
	}
}

func jsonString(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(raw)
}

func readInput() hookInput { return readInputFrom(os.Stdin) }

// readInputFrom is readInput over an arbitrary reader, so a test can feed a
// harness payload without touching the process's stdin.
func readInputFrom(r io.Reader) hookInput {
	var in hookInput
	raw, err := io.ReadAll(io.LimitReader(r, 1<<20))
	if err != nil || len(raw) == 0 {
		return in
	}
	_ = json.Unmarshal(raw, &in)
	return in
}

// shortID keeps the first eight characters of a harness session id. It is a
// label, not a key; the harness key is what identifies the session.
func shortID(id string) string {
	id = strings.TrimSpace(id)
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// retryWindow is how long an already-offered delivery must wait before this
// event may announce it again.
//
// Stop is the event that can hold a turn, so it must never re-signal inside
// one: a Stop that blocks again starts a turn whose Stop blocks again, and the
// chat never ends. Its window is longer than any turn the harness allows, so
// in practice it only retries across turns.
//
// Every other event adds context to a tool result and cannot extend a turn,
// so it may retry at once. Without that, a message offered once -- by a
// PostToolBatch whose context the model did not act on -- was never announced
// again, which is the bug that left a session with eleven offered and zero
// read deliveries.
func retryWindow(event string) int64 {
	if event == "Stop" {
		return int64((30 * time.Minute).Seconds())
	}
	return 0
}
