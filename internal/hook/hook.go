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
	flagRoom := room
	// Hooks have small time budgets: 30 seconds on UserPromptSubmit, 1.5
	// seconds shared on SessionEnd. Stay well inside the smallest one.
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	in := readInput()
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
		if _, err := h.Join(ctx, hub.JoinOptions{
			Room: roomName, ExtraRooms: extra, Harness: harness,
			HarnessKey: in.SessionID, Cwd: in.Cwd, Autonomy: autonomy,
			Label: shortID(in.SessionID),
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
		return nudge(ctx, h, harness, roomName, in.SessionID, event, in.StopHookActive)
	}
	return 0
}

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
	self, err := h.FindSession(ctx, roomName, harness, harnessKey)
	if err != nil || self == nil {
		return 0
	}

	res, err := wake.Check(ctx, h, self.ID)
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

	// Posting to the session's own inbox socket is the only lever that
	// starts a turn in an idle session, so it belongs to the wake level.
	// At notify and hold the signal rides the current turn instead; doing
	// both would wake the agent at level 0, which the operator asked not to
	// happen.
	//
	// The post counts as an own-child message, so Claude Code auto-delivers
	// it without an approval prompt.
	if harness == "claude-code" && self.Autonomy >= hub.AutonomyWake {
		if path := wake.SocketPath(); path != "" {
			if err := wake.PostToSocket(path, wake.SocketToken(), res.Line); err == nil {
				h.MarkOffered(ctx, self.ID, hub.StateAccepted)
				return 0
			}
		}
	}
	h.MarkOffered(ctx, self.ID, hub.StateOffered)

	if out := outputFor(harness, event, self.Autonomy, res.Line); out != "" {
		fmt.Fprintln(os.Stdout, out)
	}
	return 0
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
