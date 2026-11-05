// Package wake turns "this session has unread messages" into a signal the
// harness will act on.
//
// The signal never carries a message body. It carries one line, so the body
// arrives through msg_recv and the delivery state stays honest.
package wake

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/asovetov/agents-room/internal/hub"
)

// Nudge is the one line a harness sees.
func Nudge(room string, n int) string {
	return fmt.Sprintf("[agents-room] %d unread in room %q; call msg_recv", n, room)
}

// Result describes what happened, so a hook can decide its exit code.
type Result struct {
	Unread int
	Line   string
}

// Check looks up a session's unread count and builds the nudge line.
func Check(ctx context.Context, h *hub.Hub, sessionID string) (Result, error) {
	room := h.RoomOf(ctx, sessionID)
	n, err := h.Unread(ctx, sessionID)
	if err != nil {
		return Result{}, err
	}
	if n == 0 {
		return Result{}, nil
	}
	if err := h.Touch(ctx, sessionID); err != nil {
		return Result{}, err
	}
	return Result{Unread: n, Line: Nudge(room, n)}, nil
}

// SocketPath returns the Claude Code inbox socket for this process, if any.
func SocketPath() string { return os.Getenv("CLAUDE_CODE_MESSAGING_SOCKET") }

// SocketToken returns the token that socket requires on native Windows.
func SocketToken() string { return os.Getenv("CLAUDE_CODE_MESSAGING_TOKEN") }

// PostToSocket writes one user message to a Claude Code inbox socket.
//
// The wire format, taken from the Claude Code binary itself:
//
//	{"type":"auth","token":"<token>"}
//	{"type":"user","message":{"role":"user","content":"hello"}}
//
// One JSON object per line. The auth line is optional on macOS and Linux and
// required on native Windows. The connection must be opened only once the
// payload is ready: Claude Code closes a connection that has not sent a
// complete line within 30 seconds.
func PostToSocket(path, token, message string) error {
	conn, err := dial(path)
	if err != nil {
		return err
	}
	defer conn.Close()

	var buf strings.Builder
	if token != "" {
		auth, err := json.Marshal(map[string]string{"type": "auth", "token": token})
		if err != nil {
			return fmt.Errorf("encode auth line: %w", err)
		}
		buf.Write(auth)
		buf.WriteByte('\n')
	}

	msg, err := json.Marshal(map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": message,
		},
	})
	if err != nil {
		return fmt.Errorf("encode message line: %w", err)
	}
	buf.Write(msg)
	buf.WriteByte('\n')

	if _, err := io.WriteString(conn, buf.String()); err != nil {
		return fmt.Errorf("write to inbox socket: %w", err)
	}
	return nil
}
