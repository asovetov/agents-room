package hub

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/asovetov/agents-room/internal/store"
)

// HistoryEntry is one message as a newcomer sees it: who said what to whom,
// and when. The body is included because the whole point of reading history
// is learning what was agreed, and a subject alone rarely says it.
type HistoryEntry struct {
	ID        string `json:"id"`
	Sender    string `json:"sender"`
	SenderTag string `json:"sender_label,omitempty"`
	Selector  string `json:"to"`
	Subject   string `json:"subject"`
	Body      string `json:"body"`
	CreatedAt int64  `json:"created_at"`
	// ReplyTo links an answer to the question it answers, so a reader can
	// follow a thread without reading everything in between.
	ReplyTo string `json:"reply_to,omitempty"`
}

// History returns a room's recent messages, newest last.
//
// A session that joins a room where work has been going on cannot see what
// was decided before it arrived: deliveries are created at send time, so a
// newcomer has none, and Recv correctly reports nothing unread. That is right
// for "what is waiting for me" and wrong for "what is going on here", which
// is the question a joining agent actually has.
//
// History is read-only and does not mark anything read: a session that
// catches up this way has not acknowledged any message addressed to it, and
// pretending otherwise would make the delivery states lie.
func (h *Hub) History(ctx context.Context, room string, limit int) ([]HistoryEntry, error) {
	if limit <= 0 {
		limit = 20
	}
	roomID, err := h.roomID(ctx, room)
	if errors.Is(err, store.ErrNotFound) {
		return []HistoryEntry{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("history: %w", err)
	}

	now := h.now()
	// Expired messages are left out: a newcomer reading a week of chatter
	// should not act on a request that timed out before they arrived.
	// The join through message_rooms is what makes a shared room's history
	// complete: a message sent from repo:bot to an agent it meets in
	// shared:storefront belongs to both, and only the second one is where the
	// people it concerns will look for it.
	rows, err := h.db.SQL().QueryContext(ctx, `
		SELECT m.id, m.sender, COALESCE(s.label, ''), m.selector, m.subject,
		       m.body, m.created_at, COALESCE(m.reply_to, '')
		FROM messages m
		JOIN message_rooms mr ON mr.message_id = m.id
		LEFT JOIN sessions s ON s.id = m.sender
		WHERE mr.room_id = ? AND (m.expires_at IS NULL OR m.expires_at > ?)
		ORDER BY m.created_at DESC, m.rowid DESC
		LIMIT ?`, roomID, now, limit)
	if err != nil {
		return nil, fmt.Errorf("history: query: %w", err)
	}
	defer rows.Close()

	out := []HistoryEntry{}
	for rows.Next() {
		var e HistoryEntry
		if err := rows.Scan(&e.ID, &e.Sender, &e.SenderTag, &e.Selector,
			&e.Subject, &e.Body, &e.CreatedAt, &e.ReplyTo); err != nil {
			return nil, fmt.Errorf("history: scan: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("history: rows: %w", err)
	}

	// The query reads newest first so LIMIT keeps the most recent, then the
	// slice is reversed: a reader follows a conversation forwards.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// RoomSummary is the one-line answer to "what is going on in this room",
// which room_join returns so a joining agent knows whether to read History.
type RoomSummary struct {
	Room     string `json:"room"`
	Messages int    `json:"messages"`
	Since    int64  `json:"since,omitempty"`
	LastFrom string `json:"last_from,omitempty"`
	Last     string `json:"last_subject,omitempty"`
}

// Summarize reports how much has happened in a room recently.
//
// It exists so a join can say "12 messages in the last day, the last from
// api about a phone field" in one line, instead of either dumping history
// into the context or saying nothing at all.
func (h *Hub) Summarize(ctx context.Context, room string, window time.Duration) (*RoomSummary, error) {
	roomID, err := h.roomID(ctx, room)
	if errors.Is(err, store.ErrNotFound) {
		return &RoomSummary{Room: room}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("summarize: %w", err)
	}
	since := h.now() - int64(window.Seconds())

	out := &RoomSummary{Room: room}
	if err := h.db.SQL().QueryRowContext(ctx, `
		SELECT COUNT(*) FROM messages m
		JOIN message_rooms mr ON mr.message_id = m.id
		WHERE mr.room_id = ? AND m.created_at >= ?`, roomID, since).Scan(&out.Messages); err != nil {
		return nil, fmt.Errorf("summarize: count: %w", err)
	}
	if out.Messages == 0 {
		return out, nil
	}
	out.Since = since
	var label sql.NullString
	if err := h.db.SQL().QueryRowContext(ctx, `
		SELECT m.sender, m.subject, s.label
		FROM messages m
		JOIN message_rooms mr ON mr.message_id = m.id
		LEFT JOIN sessions s ON s.id = m.sender
		WHERE mr.room_id = ? AND m.created_at >= ?
		ORDER BY m.created_at DESC, m.rowid DESC LIMIT 1`,
		roomID, since).Scan(&out.LastFrom, &out.Last, &label); err != nil {
		return nil, fmt.Errorf("summarize: last: %w", err)
	}
	if label.Valid && label.String != "" {
		out.LastFrom = label.String
	}
	return out, nil
}
