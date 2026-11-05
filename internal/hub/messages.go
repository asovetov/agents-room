package hub

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// Delivery states. A message has one state per recipient, and the state only
// moves forward. We never claim a delivery we cannot prove.
const (
	StateStored        = "stored"        // written, no wake signal attempted yet
	StateOffered       = "offered"       // a wake signal was emitted
	StateAccepted      = "accepted"      // the harness accepted the signal
	StateRead          = "read"          // the recipient fetched the body
	StateAcked         = "acked"         // the recipient confirmed
	StateExpired       = "expired"       // TTL passed before it was read
	StateUndeliverable = "undeliverable" // no wake lever exists for this harness
)

// Message is one send.
type Message struct {
	ID        string `json:"id"`
	Room      string `json:"room"`
	Sender    string `json:"sender"`
	Selector  string `json:"to"`
	Subject   string `json:"subject"`
	Body      string `json:"body"`
	ReplyTo   string `json:"reply_to,omitempty"`
	CreatedAt int64  `json:"created_at"`
	ExpiresAt *int64 `json:"expires_at,omitempty"`
	State     string `json:"state"`
}

// SendOptions describes a message to send.
type SendOptions struct {
	From    string
	To      string
	Subject string
	Body    string
	ReplyTo string
	TTL     int64 // seconds; zero means no expiry
}

// Send stores a message and resolves its recipients.
//
// Selectors:
//
//   - every session in the room except the sender
//     <session-id> one session
//     file:<glob>  sessions holding a lease matching the glob
//     tag:<tag>    sessions whose join tags include the tag
//
// The body is never pushed into a recipient's context. Recipients learn that
// something is waiting and fetch it with Recv.
func (h *Hub) Send(ctx context.Context, opt SendOptions) (*Message, error) {
	if opt.From == "" {
		return nil, errors.New("send: sender is required")
	}
	if opt.To == "" {
		opt.To = "*"
	}
	if opt.Subject == "" && opt.Body == "" {
		return nil, errors.New("send: subject and body are both empty")
	}

	roomID, err := h.sessionRoom(ctx, opt.From)
	if err != nil {
		return nil, fmt.Errorf("send: %w", err)
	}

	recipients, err := h.resolve(ctx, roomID, opt.From, opt.To)
	if err != nil {
		return nil, err
	}

	id, err := newID()
	if err != nil {
		return nil, err
	}
	now := h.now()
	var expires *int64
	if opt.TTL > 0 {
		v := now + opt.TTL
		expires = &v
	}

	tx, err := h.db.SQL().BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("send: begin: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO messages
			(id, room_id, sender, selector, subject, body, reply_to, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, roomID, opt.From, opt.To, opt.Subject, opt.Body,
		nullString(opt.ReplyTo), now, expires); err != nil {
		return nil, fmt.Errorf("send: insert message: %w", err)
	}

	for _, r := range recipients {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO deliveries (message_id, session_id, state, updated_at)
			VALUES (?, ?, ?, ?)`, id, r, StateStored, now); err != nil {
			return nil, fmt.Errorf("send: insert delivery: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("send: commit: %w", err)
	}

	if err := h.appendEvent(ctx, roomID, "message.send", map[string]any{
		"message": id, "from": opt.From, "to": opt.To, "recipients": len(recipients),
	}); err != nil {
		return nil, err
	}

	return &Message{
		ID: id, Sender: opt.From, Selector: opt.To, Subject: opt.Subject,
		Body: opt.Body, ReplyTo: opt.ReplyTo, CreatedAt: now,
		ExpiresAt: expires, State: StateStored,
	}, nil
}

// resolve turns a selector into a list of session ids in the room.
func (h *Hub) resolve(ctx context.Context, roomID, sender, selector string) ([]string, error) {
	now := h.now()

	switch {
	case selector == "*":
		return h.querySessions(ctx, `
			SELECT id FROM sessions WHERE room_id = ? AND id <> ?`, roomID, sender)

	case strings.HasPrefix(selector, "file:"):
		pattern := strings.TrimPrefix(selector, "file:")
		if pattern == "" {
			return nil, errors.New("send: empty file selector")
		}
		rows, err := h.db.SQL().QueryContext(ctx,
			`SELECT path, owner FROM leases WHERE room_id = ? AND expires_at > ?`,
			roomID, now)
		if err != nil {
			return nil, fmt.Errorf("resolve file selector: %w", err)
		}
		defer rows.Close()

		seen := map[string]bool{}
		var out []string
		for rows.Next() {
			var path, owner string
			if err := rows.Scan(&path, &owner); err != nil {
				return nil, fmt.Errorf("resolve file selector: %w", err)
			}
			if owner == sender || seen[owner] {
				continue
			}
			if globMatch(pattern, path) {
				seen[owner] = true
				out = append(out, owner)
			}
		}
		return out, rows.Err()

	case strings.HasPrefix(selector, "tag:"):
		tag := strings.TrimPrefix(selector, "tag:")
		if tag == "" {
			return nil, errors.New("send: empty tag selector")
		}
		sessions, err := h.Who(ctx, h.roomName(ctx, roomID))
		if err != nil {
			return nil, err
		}
		var out []string
		for _, s := range sessions {
			if s.ID == sender {
				continue
			}
			for _, t := range s.Tags {
				if t == tag {
					out = append(out, s.ID)
					break
				}
			}
		}
		return out, nil

	default:
		// A direct session id. Verify it belongs to the room so a typo does
		// not silently deliver nothing.
		var id string
		err := h.db.SQL().QueryRowContext(ctx,
			`SELECT id FROM sessions WHERE id = ? AND room_id = ?`,
			selector, roomID).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("send: no session %q in this room", selector)
		}
		if err != nil {
			return nil, fmt.Errorf("resolve session selector: %w", err)
		}
		if id == sender {
			return nil, nil
		}
		return []string{id}, nil
	}
}

func (h *Hub) querySessions(ctx context.Context, query string, args ...any) ([]string, error) {
	rows, err := h.db.SQL().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query sessions: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan session: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (h *Hub) roomName(ctx context.Context, roomID string) string {
	var name string
	if err := h.db.SQL().QueryRowContext(ctx,
		`SELECT name FROM rooms WHERE id = ?`, roomID).Scan(&name); err != nil {
		return ""
	}
	return name
}

// Recv returns the unread messages for a session and marks them read.
//
// Expired messages are moved to StateExpired instead of being returned.
func (h *Hub) Recv(ctx context.Context, sessionID string, limit int) ([]Message, error) {
	if limit <= 0 {
		limit = 50
	}
	now := h.now()

	tx, err := h.db.SQL().BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("recv: begin: %w", err)
	}
	defer tx.Rollback()

	// Expire first so a caller never sees a message past its TTL.
	if _, err := tx.ExecContext(ctx, `
		UPDATE deliveries SET state = ?, updated_at = ?
		WHERE session_id = ? AND state IN (?, ?, ?)
		  AND message_id IN (SELECT id FROM messages WHERE expires_at IS NOT NULL AND expires_at <= ?)`,
		StateExpired, now, sessionID,
		StateStored, StateOffered, StateAccepted, now); err != nil {
		return nil, fmt.Errorf("recv: expire: %w", err)
	}

	rows, err := tx.QueryContext(ctx, `
		SELECT m.id, m.sender, m.selector, m.subject, m.body,
		       COALESCE(m.reply_to, ''), m.created_at, m.expires_at, d.state
		FROM deliveries d
		JOIN messages m ON m.id = d.message_id
		WHERE d.session_id = ? AND d.state IN (?, ?, ?)
		ORDER BY m.created_at ASC
		LIMIT ?`, sessionID, StateStored, StateOffered, StateAccepted, limit)
	if err != nil {
		return nil, fmt.Errorf("recv: query: %w", err)
	}

	var out []Message
	var ids []string
	for rows.Next() {
		var m Message
		var expires sql.NullInt64
		if err := rows.Scan(&m.ID, &m.Sender, &m.Selector, &m.Subject, &m.Body,
			&m.ReplyTo, &m.CreatedAt, &expires, &m.State); err != nil {
			rows.Close()
			return nil, fmt.Errorf("recv: scan: %w", err)
		}
		if expires.Valid {
			v := expires.Int64
			m.ExpiresAt = &v
		}
		m.State = StateRead
		out = append(out, m)
		ids = append(ids, m.ID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("recv: rows: %w", err)
	}

	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `
			UPDATE deliveries SET state = ?, updated_at = ?
			WHERE message_id = ? AND session_id = ?`,
			StateRead, now, id, sessionID); err != nil {
			return nil, fmt.Errorf("recv: mark read: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("recv: commit: %w", err)
	}
	return out, nil
}

// Unread counts messages waiting for a session, including expired ones not
// yet swept. Used by the wake hooks, which must stay cheap.
func (h *Hub) Unread(ctx context.Context, sessionID string) (int, error) {
	var n int
	err := h.db.SQL().QueryRowContext(ctx, `
		SELECT COUNT(*) FROM deliveries
		WHERE session_id = ? AND state IN (?, ?, ?)`,
		sessionID, StateStored, StateOffered, StateAccepted).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count unread: %w", err)
	}
	return n, nil
}

// Ack marks a message as confirmed by its recipient.
func (h *Hub) Ack(ctx context.Context, sessionID, messageID string) error {
	res, err := h.db.SQL().ExecContext(ctx, `
		UPDATE deliveries SET state = ?, updated_at = ?
		WHERE message_id = ? AND session_id = ? AND state <> ?`,
		StateAcked, h.now(), messageID, sessionID, StateExpired)
	if err != nil {
		return fmt.Errorf("ack: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("ack: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("ack: no delivery of %q for this session", messageID)
	}
	return nil
}

// MarkOffered records that a wake signal was attempted for a session's
// pending messages and reports how many moved.
func (h *Hub) MarkOffered(ctx context.Context, sessionID, state string) (int, error) {
	res, err := h.db.SQL().ExecContext(ctx, `
		UPDATE deliveries SET state = ?, updated_at = ?
		WHERE session_id = ? AND state = ?`,
		state, h.now(), sessionID, StateStored)
	if err != nil {
		return 0, fmt.Errorf("mark offered: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("mark offered: %w", err)
	}
	return int(n), nil
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// globMatch matches a path against a pattern with the same semantics as
// filepath.Match, but a trailing /** also matches everything below.
func globMatch(pattern, path string) bool {
	pattern = filepath.ToSlash(pattern)
	path = filepath.ToSlash(path)
	if ok, _ := filepath.Match(pattern, path); ok {
		return true
	}
	if strings.HasSuffix(pattern, "/**") {
		prefix := strings.TrimSuffix(pattern, "/**")
		return path == prefix || strings.HasPrefix(path, prefix+"/")
	}
	return false
}
