// Command agents-room-hook is the bridge between a harness event and the
// room. It is installed as a hook command and decides, per event, whether to
// join, nudge, or stay quiet.
//
// It never prints to stdout unless the harness is meant to consume the
// output: a SessionStart hook that prints would inject that text into the
// model context, which is exactly what we are trying to avoid.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/asovetov/agents-room/internal/hub"
	"github.com/asovetov/agents-room/internal/wake"
)

// hookInput is the subset of the harness payload we use.
type hookInput struct {
	SessionID string `json:"session_id"`
	Cwd       string `json:"cwd"`
	Source    string `json:"source"`
}

func main() {
	harness := flag.String("harness", "claude-code", "which harness is calling")
	event := flag.String("event", "", "harness event name, e.g. SessionStart or Stop")
	autonomy := flag.Int("autonomy", hub.AutonomyNotify, "0 notify, 1 hold, 2 wake")
	room := flag.String("room", "", "override the room name")
	quiet := flag.Bool("quiet", false, "suppress output")
	flag.Parse()

	os.Exit(run(*harness, *event, *room, *autonomy, *quiet))
}

func run(harness, event, room string, autonomy int, quiet bool) int {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	in := readInput()
	if in.Cwd == "" {
		in.Cwd, _ = os.Getwd()
	}
	if in.SessionID == "" {
		// Without a harness session id there is nothing to key a room
		// session on; stay silent rather than create an orphan.
		return 0
	}

	h, err := hub.Open()
	if err != nil {
		// The room being unavailable must never break the harness.
		return 0
	}
	defer h.Close()

	switch event {
	case "SessionStart":
		if _, err := h.Join(ctx, hub.JoinOptions{
			Room: room, Harness: harness, Cwd: in.Cwd, Autonomy: autonomy,
			Label: shortID(in.SessionID),
		}); err != nil {
			return 0
		}
		return 0

	case "SessionEnd":
		// Best effort: the harness gives this event very little time.
		sessions, err := h.Who(ctx, hub.RepoRoom(in.Cwd))
		if err != nil {
			return 0
		}
		for _, s := range sessions {
			if s.Label == shortID(in.SessionID) {
				h.Leave(ctx, s.ID)
				break
			}
		}
		return 0

	case "Stop", "PostToolBatch", "idle_prompt":
		return nudge(ctx, h, in, quiet, event)
	}
	return 0
}

// nudge emits a wake signal when the session has unread messages.
//
// The body stays in the store. The signal is one line.
func nudge(ctx context.Context, h *hub.Hub, in hookInput, quiet bool, event string) int {
	sessions, err := h.Who(ctx, hub.RepoRoom(in.Cwd))
	if err != nil {
		return 0
	}
	var self *hub.Session
	for i := range sessions {
		if sessions[i].Label == shortID(in.SessionID) {
			self = &sessions[i]
			break
		}
	}
	if self == nil {
		return 0
	}

	res, err := wake.Check(ctx, h, self.ID)
	if err != nil || res.Unread == 0 {
		return 0
	}

	// A hook posting to its own session's socket counts as an own-child
	// message, which Claude Code auto-delivers without approval.
	if path := wake.SocketPath(); path != "" {
		if err := wake.PostToSocket(path, wake.SocketToken(), res.Line); err == nil {
			h.MarkOffered(ctx, self.ID, hub.StateAccepted)
			return 0
		}
	}
	h.MarkOffered(ctx, self.ID, hub.StateOffered)

	// No socket: fall back to the harness output contract. For Stop, that
	// means holding the end of the turn when autonomy allows it.
	if quiet {
		return 0
	}
	switch event {
	case "Stop":
		if self.Autonomy >= hub.AutonomyHold {
			fmt.Fprintln(os.Stdout, stopBlockJSON(res.Line))
		}
	default:
		fmt.Fprintln(os.Stdout, contextJSON(res.Line))
	}
	return 0
}

func stopBlockJSON(reason string) string {
	raw, _ := json.Marshal(map[string]any{
		"decision": "block",
		"reason":   reason,
	})
	return string(raw)
}

func contextJSON(line string) string {
	raw, _ := json.Marshal(map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":     "PostToolBatch",
			"additionalContext": line,
		},
	})
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
// label, not a key; the room session id is the key.
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
