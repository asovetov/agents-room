// Command agents-room-hook is the bridge between a harness event and the
// room. It is installed as a hook command and decides, per event, whether to
// join, nudge, or stay quiet.
//
// It prints to stdout only when the calling harness is meant to consume the
// output. A SessionStart hook that prints would inject that text into the
// model context, which is exactly what the design avoids.
package main

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
}

func main() {
	harness := flag.String("harness", "claude-code", "which harness is calling: claude-code, codex or hermes")
	event := flag.String("event", "", "harness event name, e.g. SessionStart or Stop")
	autonomy := flag.Int("autonomy", hub.AutonomyNotify, "0 notify, 1 hold, 2 wake")
	room := flag.String("room", "", "override the room name")
	flag.Parse()

	os.Exit(run(*harness, *event, *room, *autonomy))
}

func run(harness, event, room string, autonomy int) int {
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
		// Rejoining is idempotent: SessionStart fires on startup, resume,
		// clear, compact and fork, and all of them are the same chat.
		if _, err := h.Join(ctx, hub.JoinOptions{
			Room: roomName, Harness: harness, HarnessKey: in.SessionID,
			Cwd: in.Cwd, Autonomy: autonomy,
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
		return nudge(ctx, h, harness, roomName, in.SessionID, event)
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
func nudge(ctx context.Context, h *hub.Hub, harness, roomName, harnessKey, event string) int {
	self, err := h.FindSession(ctx, roomName, harness, harnessKey)
	if err != nil || self == nil {
		return 0
	}

	res, err := wake.Check(ctx, h, self.ID)
	if err != nil || res.Unread == 0 {
		return 0
	}

	// A hook posting to its own session's socket counts as an own-child
	// message, which Claude Code auto-delivers without approval, and it
	// wakes an idle session.
	if harness == "claude-code" {
		if path := wake.SocketPath(); path != "" {
			if err := wake.PostToSocket(path, wake.SocketToken(), res.Line); err == nil {
				h.MarkOffered(ctx, self.ID, hub.StateAccepted)
				return 0
			}
		}
	}
	h.MarkOffered(ctx, self.ID, hub.StateOffered)

	fmt.Fprintln(os.Stdout, outputFor(harness, event, self.Autonomy, res.Line))
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

func readInput() hookInput {
	var in hookInput
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
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
