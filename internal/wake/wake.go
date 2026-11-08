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
	"strconv"
	"strings"

	"github.com/asovetov/agents-room/internal/hub"
)

// Nudge is the one line a harness sees.
//
// It names every room the unread messages came through, not just the first
// one. A session joined its repository room first and its shared rooms after,
// so naming only the first told an agent in `repo:storefront-web` that its
// message was in `repo:storefront-web` when it had arrived in
// `shared:storefront` -- the agent then looked in a room that had nothing new.
// The line stays one line: past two rooms it says how many more there are
// rather than growing without bound.
func Nudge(rooms []string, n int) string {
	if len(rooms) == 0 {
		return fmt.Sprintf("[agents-room] %d unread; call msg_recv", n)
	}
	named := rooms
	more := 0
	if len(named) > 2 {
		more = len(named) - 2
		named = named[:2]
	}
	quoted := make([]string, len(named))
	for i, r := range named {
		quoted[i] = strconv.Quote(r)
	}
	where := "room " + strings.Join(quoted, ", ")
	if len(named) > 1 {
		where = "rooms " + strings.Join(quoted, ", ")
	}
	if more > 0 {
		where += fmt.Sprintf(" and %d more", more)
	}
	return fmt.Sprintf("[agents-room] %d unread in %s; call msg_recv", n, where)
}

// Result describes what happened, so a hook can decide its exit code.
type Result struct {
	Unread int
	Line   string
}

// Check builds a nudge line for a session, but only while a message is still
// waiting for its first signal.
//
// Unread alone is the wrong trigger: a row that was already offered stays
// unread until the agent fetches it, so every Stop would signal again. At
// wake level each signal is a new turn, and a turn whose Stop signals again
// never ends. The count in the line is the full unread total, because that is
// what the agent will find when it calls msg_recv.
func Check(ctx context.Context, h *hub.Hub, sessionID string) (Result, error) {
	// The zero-window form is the strict one: a delivery that has been offered
	// is not pending, so this never signals the same message twice. Callers
	// that cannot hold a turn and want a retry use CheckRetry.
	return check(ctx, h, sessionID, strictWindow)
}

// strictWindow is "never retry an offered delivery". PendingSince compares
// updated_at <= now-minAge, so a window larger than any plausible age means
// nothing qualifies.
const strictWindow = int64(1) << 40

// CheckRetry is Check with a retry window for deliveries whose earlier signal
// went out but was never fetched.
//
// minAge is how long an offered delivery must have been waiting before it is
// worth signalling again. A caller that can hold a turn must pass a window
// longer than one turn: re-signalling inside the same turn is the loop this
// package exists to avoid. A caller that only adds context to a tool result
// passes 0, because it cannot extend a turn, and without the retry a message
// offered once would never be announced again.
func CheckRetry(ctx context.Context, h *hub.Hub, sessionID string, minAge int64) (Result, error) {
	return check(ctx, h, sessionID, minAge)
}

func check(ctx context.Context, h *hub.Hub, sessionID string, minAge int64) (Result, error) {
	pending, err := h.PendingSince(ctx, sessionID, minAge)
	if err != nil {
		return Result{}, err
	}
	if pending == 0 {
		return Result{}, nil
	}
	n, err := h.Unread(ctx, sessionID)
	if err != nil {
		return Result{}, err
	}
	rooms, err := h.UnreadRooms(ctx, sessionID)
	if err != nil {
		return Result{}, err
	}
	if err := h.Touch(ctx, sessionID); err != nil {
		return Result{}, err
	}
	return Result{Unread: n, Line: Nudge(rooms, n)}, nil
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
