package hub

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	pathpkg "path"
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
	// Recipients is how many sessions the selector matched. Zero means the
	// message is stored but nobody will ever read it, which a sender needs
	// to know before it stops paying attention.
	Recipients int `json:"recipients"`
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

	roomIDs, err := h.sessionRoomIDs(ctx, opt.From)
	if err != nil {
		return nil, fmt.Errorf("send: %w", err)
	}
	roomID := roomIDs[0]

	recipients, err := h.resolve(ctx, roomIDs, opt.From, opt.To)
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
		ExpiresAt: expires, State: StateStored, Recipients: len(recipients),
	}, nil
}

// resolve turns a selector into a list of session ids.
//
// A sender reaches every room it is in, which is what lets an agent in one
// repository address an agent in another through a shared room. Results are
// de-duplicated: a session in two of the sender's rooms is one recipient.
func (h *Hub) resolve(ctx context.Context, roomIDs []string, sender, selector string) ([]string, error) {
	now := h.now()

	switch {
	case selector == "*":
		return h.sessionsInRooms(ctx, roomIDs, sender)

	case strings.HasPrefix(selector, "file:"):
		pattern := strings.TrimPrefix(selector, "file:")
		if pattern == "" {
			return nil, errors.New("send: empty file selector")
		}
		// A lease lives in the holder's repository room, so the search joins
		// through session_rooms: a holder reachable only through a shared
		// room must still be found.
		// The holder's own working directory is what a relative selector is
		// relative to. A shared room can hold sessions from different
		// repositories, so the room's directory is the wrong anchor.
		rows, err := h.db.SQL().QueryContext(ctx, `
			SELECT l.path, l.owner, COALESCE(s.cwd, '') FROM leases l
			JOIN session_rooms sr ON sr.session_id = l.owner
			LEFT JOIN sessions s ON s.id = l.owner
			WHERE sr.room_id IN (`+placeholders(len(roomIDs))+`) AND l.expires_at > ?`,
			append(anySlice(roomIDs), now)...)
		if err != nil {
			return nil, fmt.Errorf("resolve file selector: %w", err)
		}
		defer rows.Close()

		seen := map[string]bool{}
		out := []string{}
		for rows.Next() {
			var path, owner, holderCwd string
			if err := rows.Scan(&path, &owner, &holderCwd); err != nil {
				return nil, fmt.Errorf("resolve file selector: %w", err)
			}
			if owner == sender || seen[owner] {
				continue
			}
			if matchLeasedPath(pattern, path, holderCwd) {
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
		seen := map[string]bool{}
		out := []string{}
		for _, roomID := range roomIDs {
			sessions, err := h.Who(ctx, h.roomName(ctx, roomID))
			if err != nil {
				return nil, err
			}
			for _, s := range sessions {
				if s.ID == sender || seen[s.ID] {
					continue
				}
				for _, t := range s.Tags {
					if t == tag {
						seen[s.ID] = true
						out = append(out, s.ID)
						break
					}
				}
			}
		}
		return out, nil

	default:
		// A direct session id. It must share at least one room with the
		// sender, so a typo or an unrelated session is an error rather than
		// a silent no-op.
		if selector == sender {
			return nil, nil
		}
		rows, err := h.db.SQL().QueryContext(ctx, `
			SELECT s.id FROM sessions s
			JOIN session_rooms sr ON sr.session_id = s.id
			WHERE s.id = ? AND sr.room_id IN (`+placeholders(len(roomIDs))+`)`,
			append([]any{selector}, anySlice(roomIDs)...)...)
		if err != nil {
			return nil, fmt.Errorf("resolve session selector: %w", err)
		}
		defer rows.Close()
		var id string
		if rows.Next() {
			if err := rows.Scan(&id); err != nil {
				return nil, fmt.Errorf("resolve session selector: %w", err)
			}
		}
		rows.Close()
		if id == "" {
			return nil, fmt.Errorf("send: session %q is not in any room shared with you", selector)
		}
		return []string{id}, nil
	}
}

// sessionsInRooms returns every session in the given rooms except one.
func (h *Hub) sessionsInRooms(ctx context.Context, roomIDs []string, except string) ([]string, error) {
	rows, err := h.db.SQL().QueryContext(ctx, `
		SELECT DISTINCT s.id FROM sessions s
		JOIN session_rooms sr ON sr.session_id = s.id
		WHERE sr.room_id IN (`+placeholders(len(roomIDs))+`) AND s.id <> ?`,
		append(anySlice(roomIDs), except)...)
	if err != nil {
		return nil, fmt.Errorf("query sessions: %w", err)
	}
	defer rows.Close()

	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan session: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// placeholders returns n comma-separated SQL placeholders.
func placeholders(n int) string {
	if n == 0 {
		return "NULL"
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// anySlice converts a string slice for variadic SQL arguments.
//
// The result has no spare capacity on purpose. A caller writing
// append(anySlice(rooms), path) would otherwise write into that spare slot
// and silently replace the first room, because append reuses the backing
// array when capacity allows.
func anySlice(v []string) []any {
	out := make([]any, len(v), len(v))
	for i := range v {
		out[i] = v[i]
	}
	return out
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

// matchLeasedPath matches a file selector against a leased path.
//
// Leases store absolute paths so two sessions cannot hold one file under
// different names. A selector is written relative to the project, so the
// pattern is tried against the full path, the path relative to the holder's
// working directory, and the bare base name.
//
// The holder's directory is the anchor, not the room's: a shared room can
// contain sessions from several repositories, and "src/wire.go" means
// different files to each of them.
func matchLeasedPath(pattern, leased, holderCwd string) bool {
	if globMatch(pattern, leased) {
		return true
	}
	if holderCwd != "" {
		if rel, err := filepath.Rel(holderCwd, leased); err == nil {
			if globMatch(pattern, rel) {
				return true
			}
		}
	}
	return globMatch(pattern, filepath.Base(leased))
}

func (h *Hub) roomName(ctx context.Context, roomID string) string {
	var name string
	if err := h.db.SQL().QueryRowContext(ctx,
		`SELECT name FROM rooms WHERE id = ?`, roomID).Scan(&name); err != nil {
		return ""
	}
	return name
}

// roomNames resolves ids to names for an error message, skipping ids that
// no longer exist rather than failing the send twice.
func (h *Hub) roomNames(ctx context.Context, roomIDs []string) []string {
	out := make([]string, 0, len(roomIDs))
	for _, id := range roomIDs {
		if name := h.roomName(ctx, id); name != "" {
			out = append(out, name)
		}
	}
	return out
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

	// Recipients is the message's own delivery count, not this recipient's.
	// A reader asking "did this reach anyone else" needs the real number;
	// leaving it at zero would say the message reached nobody, which is
	// false for every message a reader can see.
	rows, err := tx.QueryContext(ctx, `
		SELECT m.id, m.sender, m.selector, m.subject, m.body,
		       COALESCE(m.reply_to, ''), m.created_at, m.expires_at, d.state,
		       (SELECT COUNT(*) FROM deliveries d2 WHERE d2.message_id = m.id)
		FROM deliveries d
		JOIN messages m ON m.id = d.message_id
		WHERE d.session_id = ? AND d.state IN (?, ?, ?)
		ORDER BY m.created_at ASC
		LIMIT ?`, sessionID, StateStored, StateOffered, StateAccepted, limit)
	if err != nil {
		return nil, fmt.Errorf("recv: query: %w", err)
	}

	out := []Message{}
	var ids []string
	for rows.Next() {
		var m Message
		var expires sql.NullInt64
		if err := rows.Scan(&m.ID, &m.Sender, &m.Selector, &m.Subject, &m.Body,
			&m.ReplyTo, &m.CreatedAt, &expires, &m.State, &m.Recipients); err != nil {
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

// Sent is one message a session sent, with what became of it per recipient.
type Sent struct {
	ID         string `json:"id"`
	Subject    string `json:"subject"`
	To         string `json:"to"`
	CreatedAt  int64  `json:"created_at"`
	Recipients int    `json:"recipients"`
	// States counts deliveries by state, so a sender can see at a glance
	// whether anyone read what it sent.
	States map[string]int `json:"states"`
	// Pending is recipients whose delivery has not reached a terminal state:
	// the message is still waiting on them.
	Pending int `json:"pending"`
}

// SentMessages lists what a session sent, newest first, with delivery states.
//
// A sender had no way to find out what happened after msg_send returned a
// recipient count. A message could sit unread forever, or go to a session
// that died, and the sender never learned; this is the read side of that.
func (h *Hub) SentMessages(ctx context.Context, sessionID string, limit int) ([]Sent, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := h.db.SQL().QueryContext(ctx, `
		SELECT id, subject, selector, created_at FROM messages
		WHERE sender = ? ORDER BY created_at DESC, rowid DESC LIMIT ?`, sessionID, limit)
	if err != nil {
		return nil, fmt.Errorf("sent: query: %w", err)
	}
	defer rows.Close()

	out := []Sent{}
	for rows.Next() {
		var s Sent
		if err := rows.Scan(&s.ID, &s.Subject, &s.To, &s.CreatedAt); err != nil {
			return nil, fmt.Errorf("sent: scan: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sent: rows: %w", err)
	}

	for i := range out {
		states, err := h.deliveryStates(ctx, out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].States = states
		for _, n := range states {
			out[i].Recipients += n
		}
		for _, state := range []string{StateStored, StateOffered, StateAccepted} {
			out[i].Pending += states[state]
		}
	}
	return out, nil
}

// deliveryStates counts one message's deliveries by state.
func (h *Hub) deliveryStates(ctx context.Context, messageID string) (map[string]int, error) {
	rows, err := h.db.SQL().QueryContext(ctx, `
		SELECT state, COUNT(*) FROM deliveries
		WHERE message_id = ? GROUP BY state`, messageID)
	if err != nil {
		return nil, fmt.Errorf("delivery states: %w", err)
	}
	defer rows.Close()

	out := map[string]int{}
	for rows.Next() {
		var state string
		var n int
		if err := rows.Scan(&state, &n); err != nil {
			return nil, fmt.Errorf("delivery states: scan: %w", err)
		}
		out[state] = n
	}
	return out, rows.Err()
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

// Pending counts deliveries no wake signal has been emitted for yet.
//
// It is the number a hook may act on. Unread includes rows already offered,
// and offering one twice at wake level starts a second turn for the same
// message: the agent is never allowed to end, which is a loop, not a
// reminder. Once a signal has gone out, the message waits for msg_recv.
func (h *Hub) Pending(ctx context.Context, sessionID string) (int, error) {
	var n int
	err := h.db.SQL().QueryRowContext(ctx, `
		SELECT COUNT(*) FROM deliveries
		WHERE session_id = ? AND state = ?`,
		sessionID, StateStored).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count pending: %w", err)
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

// globMatch matches a path against a pattern, with a trailing /** also
// matching everything below.
//
// It uses path.Match, not filepath.Match: on Windows the latter treats
// backslash as the separator, so "*" would match across "/" and a selector
// meant for one directory would reach the whole tree. Selectors are always
// written with forward slashes, on every platform.
func globMatch(pattern, path string) bool {
	pattern = strings.ReplaceAll(pattern, `\`, "/")
	path = strings.ReplaceAll(path, `\`, "/")
	if ok, _ := pathpkg.Match(pattern, path); ok {
		return true
	}
	if strings.HasSuffix(pattern, "/**") {
		prefix := strings.TrimSuffix(pattern, "/**")
		return path == prefix || strings.HasPrefix(path, prefix+"/")
	}
	return false
}
