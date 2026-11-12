package hub

import (
	"context"
	"database/sql"
	"encoding/json"
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
	// Warning is set when the send succeeded but looks like it reached the
	// wrong place, such as a file: selector that matched nobody.
	Warning string `json:"warning,omitempty"`
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
//     file:<repo>:<path> sessions in repo:<repo> holding a lease matching
//     path, read relative to the holder's checkout
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
	// roomID is where the message is filed, and it is the sender's own room.
	// The message also belongs to every other room the sender is in, because
	// that is how it can reach a recipient in another repository: a bot in
	// repo:bot writing to an api agent it meets in shared:storefront must
	// leave the message findable in shared:storefront, or the history of that
	// room says nothing happened while the work is plainly going on.
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
	for _, room := range roomIDs {
		if _, err := tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO message_rooms (message_id, room_id)
			VALUES (?, ?)`, id, room); err != nil {
			return nil, fmt.Errorf("send: file message in room: %w", err)
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
		Warning: h.fileSelectorWarning(ctx, opt.From, opt.To, len(recipients)),
	}, nil
}

// fileSelectorWarning explains a file: selector that reached nobody.
//
// A file: pattern is matched against each holder's leased path in three
// forms, one of them relative to the holder's own directory. The usual miss
// is a path that no holder has: a file the sender has not leased, or a
// relative path that exists in no checkout it can see.
//
// The warning names the fix, not only the mistake. A repository-qualified
// selector, file:<repo>:<path>, says which checkout the path belongs to, so
// the path is read where the sender means it and is not matched against every
// other checkout that happens to have the same relative path.
func (h *Hub) fileSelectorWarning(ctx context.Context, sessionID, selector string, recipients int) string {
	if recipients > 0 || !strings.HasPrefix(selector, "file:") {
		return ""
	}
	pattern := strings.TrimPrefix(selector, "file:")
	// A qualified selector that reached nobody gets its own sentence: telling
	// the sender to qualify the selector it already qualified would be noise.
	if repoRoomID, rest, ok := h.splitRepoQualifier(ctx, pattern); ok {
		return fmt.Sprintf("file:%s matched no recipient: no session in %s that shares "+
			"one of your rooms holds a live lease matching %s",
			pattern, h.roomName(ctx, repoRoomID), rest)
	}
	var cwd string
	if err := h.db.SQL().QueryRowContext(ctx,
		`SELECT cwd FROM sessions WHERE id = ?`, sessionID).Scan(&cwd); err != nil {
		cwd = ""
	}
	msg := fmt.Sprintf("file:%s matched no recipient", pattern)
	if cwd != "" {
		msg += fmt.Sprintf(": a relative path is matched against every holder's own "+
			"checkout, and nobody holds %s. To name one repository's file, write "+
			"file:<repo>:<path> with its repository room name, for example "+
			"file:api:src/users.go; an absolute path works too",
			filepath.Join(cwd, pattern))
	}
	return msg
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
		// file:<repo>:<path> names the checkout the path belongs to, so a
		// sender in one repository can address a holder in another with the
		// path it sees in that repository's tree. The qualifier is a
		// repository name only when a room repo:<name> exists; otherwise the
		// colon is part of the path (a Windows drive letter) and the pattern
		// is matched exactly as before.
		repoRoomID, repoPath, qualified := h.splitRepoQualifier(ctx, pattern)

		// A lease lives in the holder's repository room, so the search joins
		// through session_rooms: a holder reachable only through a shared
		// room must still be found.
		// The holder's own working directory is what a relative selector is
		// relative to. A shared room can hold sessions from different
		// repositories, so the room's directory is the wrong anchor.
		//
		// A qualified selector narrows the holders to sessions in the named
		// repository room. That has to be a membership test on the owner,
		// not on the row: the row that makes a cross-repository holder
		// reachable is its membership in the shared room, and the same
		// session is also a member of repo:<name>, which the sender is not.
		query := `
			SELECT l.path, l.owner, COALESCE(s.cwd, '') FROM leases l
			JOIN session_rooms sr ON sr.session_id = l.owner
			LEFT JOIN sessions s ON s.id = l.owner
			WHERE sr.room_id IN (` + placeholders(len(roomIDs)) + `) AND l.expires_at > ?`
		args := append(anySlice(roomIDs), now)
		if qualified {
			query += ` AND l.owner IN (SELECT session_id FROM session_rooms WHERE room_id = ?)`
			args = append(args, repoRoomID)
		}
		rows, err := h.db.SQL().QueryContext(ctx, query, args...)
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
			matched := matchLeasedPath(pattern, path, holderCwd)
			if qualified {
				matched = matchQualifiedPath(repoPath, path, holderCwd)
			}
			if matched {
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

// matchQualifiedPath matches the path half of file:<repo>:<path> against a
// lease held in the named repository.
//
// The path is what a person in that repository would type, so it is anchored
// at the holder's checkout. The bare base name is still accepted, for the
// same reason matchLeasedPath accepts it: naming users.go is how a sender who
// does not remember the tree addresses a file.
func matchQualifiedPath(pattern, leased, holderCwd string) bool {
	if holderCwd != "" {
		if rel, err := filepath.Rel(holderCwd, leased); err == nil {
			if globMatch(pattern, rel) {
				return true
			}
		}
	}
	if globMatch(pattern, leased) {
		return true
	}
	return globMatch(pattern, filepath.Base(leased))
}

// splitRepoQualifier splits a file selector into a repository room id and the
// path inside it.
//
// The first segment is a repository name only when a room repo:<segment>
// exists. That rule is what keeps a Windows drive letter (file:C:\src\wire.go)
// and any other path with a colon from being read as a repository that does
// not exist: an unknown name is not a repository, and the selector falls
// through to the plain path matching it had before.
//
// It is deliberately not "a room the sender is in". The sender in mobile is
// in repo:mobile and shared:storefront, never in repo:api, and reaching
// api's holder through the shared room is exactly the case the qualifier
// exists for. Reachability is still enforced: the holder must be in the
// sender's rooms like any other recipient, and the qualifier then narrows
// that set to the named repository.
func (h *Hub) splitRepoQualifier(ctx context.Context, pattern string) (roomID, path string, ok bool) {
	name, rest, ok := splitRepoName(pattern)
	if !ok {
		return "", "", false
	}
	id, err := h.roomID(ctx, "repo:"+name)
	if err != nil {
		return "", "", false
	}
	return id, rest, true
}

// splitRepoName splits the first colon-separated segment off a file pattern.
//
// It is deliberately ignorant of rooms: callers decide whether the segment
// names one. The path must be non-empty, because "file:api:" names no file.
func splitRepoName(pattern string) (name, rest string, ok bool) {
	i := strings.IndexByte(pattern, ':')
	if i <= 0 || i == len(pattern)-1 {
		return "", "", false
	}
	return pattern[:i], pattern[i+1:], true
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
//
// An unknown session id is an error, not an empty inbox. Both are zero rows,
// so without the check a caller that passes the wrong id is told "nothing for
// you" forever and never learns it asked about the wrong thing.
func (h *Hub) Recv(ctx context.Context, sessionID string, limit int) ([]Message, error) {
	if limit <= 0 {
		limit = 50
	}
	if err := h.requireSession(ctx, sessionID); err != nil {
		return nil, fmt.Errorf("recv: %w", err)
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
	// Left is recipients that went away with the message unread. Their
	// delivery is gone, so Pending alone would have said "still waiting",
	// which is untrue: there is nobody left to wait for.
	Left int `json:"left"`
	// Sent is how many recipients the send resolved, read from the send
	// event. Recipients counts the deliveries still on disk, and a session
	// that leaves takes its delivery with it, so Sent == Recipients + Left.
	Sent int `json:"sent"`
	// Readers names the recipients that fetched the body, with what is known
	// about each.
	//
	// A count alone cannot answer "did the agent I meant read it". In a
	// shared room a broadcast reaches every member, including the ones the
	// message had nothing to do with, and their read is indistinguishable
	// from the intended recipient's in a tally of three. A sender chasing a
	// decision needs the names, and needs to know which of them are the
	// people the message was about.
	Readers []Reader `json:"readers,omitempty"`
}

// Reader is one session that fetched a message, and the little that can be
// said about whether it was the right one.
type Reader struct {
	SessionID string `json:"session_id"`
	Label     string `json:"label,omitempty"`
	Harness   string `json:"harness,omitempty"`
	// Rooms are the rooms the reader shares with the sender, which is how
	// the message reached it at all.
	Rooms []string `json:"rooms,omitempty"`
	// SharedOnly is true when the only room the two share is a shared room.
	//
	// That is a hint, not a verdict: in a cross-repository room it is the
	// normal case, and two agents that work on the same interface still meet
	// only there. It is reported because a reader with no repository room in
	// common and no matching tag is the shape of a bystander, and the sender
	// is the only one who can say whether it was.
	SharedOnly bool  `json:"shared_only,omitempty"`
	ReadAt     int64 `json:"read_at,omitempty"`
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
	// "Nothing sent from this session" is true of a session that sent nothing
	// and of an id that names nothing. The sender asking this question is
	// usually chasing a reply that never came, so a wrong id here would send
	// it looking in the wrong place for the wrong thing.
	if err := h.requireSession(ctx, sessionID); err != nil {
		return nil, fmt.Errorf("sent: %w", err)
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
		// The send event recorded how many recipients resolved at send time.
		// Comparing it with the rows still on disk is what turns "3 have not
		// read it" into the truth when one of the three walked out.
		if n, err := h.recipientsAtSend(ctx, out[i].ID); err == nil && n > 0 {
			out[i].Sent = n
			if gone := n - out[i].Recipients; gone > 0 {
				out[i].Left = gone
			}
		} else {
			out[i].Sent = out[i].Recipients
		}
		readers, err := h.readersOf(ctx, sessionID, out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].Readers = readers
	}
	return out, nil
}

// readersOf names the sessions that fetched a message, relative to its
// sender.
//
// The rooms listed are the ones the reader shares with the sender, because
// that is how the message could reach it. SharedOnly says the two meet only
// in a shared room: in a cross-repository room that is normal and often
// exactly right, so it is a hint for the sender to judge, not a label the
// store is entitled to apply.
func (h *Hub) readersOf(ctx context.Context, senderID, messageID string) ([]Reader, error) {
	// Names, not ids: SharedOnly is a judgement about what kind of room the
	// two meet in, and only the name carries that. Membership ids are opaque
	// hex, so a prefix check on them would never match.
	senderRooms, err := h.RoomsOf(ctx, senderID)
	if err != nil {
		return nil, err
	}
	rows, err := h.db.SQL().QueryContext(ctx, `
		SELECT d.session_id, COALESCE(s.label, ''), COALESCE(s.harness, ''), d.updated_at
		FROM deliveries d
		LEFT JOIN sessions s ON s.id = d.session_id
		WHERE d.message_id = ? AND d.state IN (?, ?)
		ORDER BY d.updated_at ASC, d.session_id ASC`,
		messageID, StateRead, StateAcked)
	if err != nil {
		return nil, fmt.Errorf("readers: %w", err)
	}
	defer rows.Close()

	type row struct {
		id, label, harness string
		at                 int64
	}
	var found []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.label, &r.harness, &r.at); err != nil {
			return nil, fmt.Errorf("readers: scan: %w", err)
		}
		found = append(found, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("readers: rows: %w", err)
	}

	out := []Reader{}
	for _, r := range found {
		readerRooms, err := h.RoomsOf(ctx, r.id)
		if err != nil {
			// A reader that left after reading has no memberships left; its
			// delivery survives, so name it without rooms rather than failing
			// the whole listing.
			if !errors.Is(err, ErrNoSuchSession) {
				return nil, err
			}
			readerRooms = nil
		}
		shared := sharedRooms(senderRooms, readerRooms)
		out = append(out, Reader{
			SessionID: r.id, Label: r.label, Harness: r.harness,
			Rooms:      shared,
			SharedOnly: len(shared) > 0 && !anyRepoRoom(shared),
			ReadAt:     r.at,
		})
	}
	return out, nil
}

// sharedRooms returns the rooms two sessions have in common, as names.
func sharedRooms(a, b []string) []string {
	inB := make(map[string]bool, len(b))
	for _, name := range b {
		inB[name] = true
	}
	out := []string{}
	for _, name := range a {
		if inB[name] {
			out = append(out, name)
		}
	}
	return out
}

// anyRepoRoom reports whether any of the names is a repository room rather
// than a shared one.
func anyRepoRoom(rooms []string) bool {
	for _, name := range rooms {
		if !strings.HasPrefix(name, "shared:") {
			return true
		}
	}
	return false
}

// recipientsAtSend reads the recipient count recorded when a message was
// sent. It is the only place the original number survives, because a
// delivery is deleted with the session that held it.
func (h *Hub) recipientsAtSend(ctx context.Context, messageID string) (int, error) {
	var payload string
	err := h.db.SQL().QueryRowContext(ctx, `
		SELECT payload FROM events
		WHERE kind = 'message.send' AND payload LIKE ? LIMIT 1`,
		"%"+messageID+"%").Scan(&payload)
	if err != nil {
		return 0, err
	}
	var p struct {
		Recipients int `json:"recipients"`
	}
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		return 0, err
	}
	return p.Recipients, nil
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
	if err := h.requireSession(ctx, sessionID); err != nil {
		return 0, fmt.Errorf("unread: %w", err)
	}
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

// UnreadRooms names the rooms a session's unread messages are in, the busiest
// first.
//
// A nudge has to name the room the message actually came through. The room a
// session joined first is its repository room, and it is almost never the one
// a cross-repository message arrives in: a link in `repo:storefront-web` that
// meets the firmware in `shared:storefront` was told "1 unread in room
// repo:storefront-web" and went looking in the wrong place.
//
// The room list is intersected with the recipient's own membership, because a
// message is filed under every room its *sender* was in, and naming a room the
// reader is not a member of would be a different wrong answer.
func (h *Hub) UnreadRooms(ctx context.Context, sessionID string) ([]string, error) {
	if err := h.requireSession(ctx, sessionID); err != nil {
		return nil, fmt.Errorf("unread rooms: %w", err)
	}
	rows, err := h.db.SQL().QueryContext(ctx, `
		SELECT r.name, COUNT(*) AS n
		FROM deliveries d
		JOIN message_rooms mr ON mr.message_id = d.message_id
		JOIN rooms r ON r.id = mr.room_id
		JOIN session_rooms sr ON sr.room_id = mr.room_id AND sr.session_id = d.session_id
		WHERE d.session_id = ? AND d.state IN (?, ?, ?)
		GROUP BY r.name
		ORDER BY n DESC, r.name ASC`,
		sessionID, StateStored, StateOffered, StateAccepted)
	if err != nil {
		return nil, fmt.Errorf("unread rooms: %w", err)
	}
	defer rows.Close()

	out := []string{}
	for rows.Next() {
		var name string
		var n int
		if err := rows.Scan(&name, &n); err != nil {
			return nil, fmt.Errorf("unread rooms: scan: %w", err)
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// InboxSummary answers "is this inbox empty, or am I asking in the wrong
// place" for a session that just fetched nothing.
//
// An empty Recv is correct and uninformative: it says no message is waiting,
// which is also what a session in the wrong room would see. An agent that
// suspected it had written to the wrong room had to read the database by hand
// to check, because nothing in the tool's answer said which rooms it was in or
// whether anything had ever been delivered to it.
type InboxSummary struct {
	// Rooms is every room the session is in, oldest membership first.
	Rooms []string `json:"rooms"`
	// Delivered is how many deliveries this session has ever had, in any
	// state. Zero means nothing was ever addressed to it, which is the
	// answer "you may be in the wrong room" needs.
	Delivered int `json:"delivered"`
	// LastDeliveredAt is when the most recent delivery was created, in unix
	// seconds. Zero when Delivered is zero.
	LastDeliveredAt int64 `json:"last_delivered_at,omitempty"`
	// Read is how many of them this session has read, so "I have received
	// nothing" and "I have received things and read them" stay distinct.
	Read int `json:"read"`
}

// Inbox summarizes what a session has been sent and where it can be reached.
func (h *Hub) Inbox(ctx context.Context, sessionID string) (*InboxSummary, error) {
	if err := h.requireSession(ctx, sessionID); err != nil {
		return nil, fmt.Errorf("inbox: %w", err)
	}
	rooms, err := h.RoomsOf(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("inbox: %w", err)
	}
	out := &InboxSummary{Rooms: rooms}
	if err := h.db.SQL().QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(MAX(m.created_at), 0),
		       COALESCE(SUM(CASE WHEN d.state IN (?, ?) THEN 1 ELSE 0 END), 0)
		FROM deliveries d
		JOIN messages m ON m.id = d.message_id
		WHERE d.session_id = ?`,
		StateRead, StateAcked, sessionID).Scan(
		&out.Delivered, &out.LastDeliveredAt, &out.Read); err != nil {
		return nil, fmt.Errorf("inbox: %w", err)
	}
	return out, nil
}

// Waker is one recipient that can be told about a waiting message.
type Waker struct {
	SessionID string `json:"session_id"`
	Label     string `json:"label,omitempty"`
	// Socket is the inbox socket this chat's harness reads. Empty means the
	// chat cannot be woken while idle and its message waits for a pull.
	Socket string `json:"socket,omitempty"`
	// Pending is how many messages are waiting for this recipient with no
	// signal yet.
	Pending int `json:"pending"`
	// Rooms are the rooms the unread messages came through, for the line.
	Rooms []string `json:"rooms,omitempty"`
}

// Wakers lists the sessions that should be told about messages they have not
// seen, with the socket to reach each of them.
//
// A wake signal is emitted by the sender's hook, which runs on the sender's
// machine and under the sender's environment. Reading CLAUDE_CODE_MESSAGING_SOCKET
// there yields the *sender's* inbox, so the signal used to be posted back to
// the process that sent it while the recipient sat idle and was never told
// anything. The socket has to come from the recipient's row.
//
// Only `stored` deliveries are listed, and only for sessions at wake level:
// a message that has been offered once is the retry path's business, and a
// session below wake cannot be woken while idle at all.
func (h *Hub) Wakers(ctx context.Context, minAge int64) ([]Waker, error) {
	cutoff := h.now() - minAge
	rows, err := h.db.SQL().QueryContext(ctx, `
		SELECT d.session_id, COALESCE(s.label, ''), COALESCE(s.wake_socket, ''),
		       COUNT(*)
		FROM deliveries d
		JOIN sessions s ON s.id = d.session_id
		WHERE s.autonomy >= ?
		  AND (d.state = ? OR (d.state = ? AND d.updated_at <= ?))
		GROUP BY d.session_id
		ORDER BY d.session_id`,
		AutonomyWake, StateStored, StateOffered, cutoff)
	if err != nil {
		return nil, fmt.Errorf("wakers: %w", err)
	}
	defer rows.Close()

	out := []Waker{}
	for rows.Next() {
		var w Waker
		if err := rows.Scan(&w.SessionID, &w.Label, &w.Socket, &w.Pending); err != nil {
			return nil, fmt.Errorf("wakers: scan: %w", err)
		}
		rooms, err := h.UnreadRooms(ctx, w.SessionID)
		if err != nil {
			return nil, err
		}
		w.Rooms = rooms
		out = append(out, w)
	}
	return out, rows.Err()
}

// Pending counts deliveries no wake signal has been emitted for yet.
//
// It is the number a hook may act on. Unread includes rows already offered,
// and offering one twice at wake level starts a second turn for the same
// message: the agent is never allowed to end, which is a loop, not a
// reminder. Once a signal has gone out, the message waits for msg_recv.
func (h *Hub) Pending(ctx context.Context, sessionID string) (int, error) {
	return h.PendingSince(ctx, sessionID, 0)
}

// PendingSince counts deliveries still waiting for a signal, including the
// ones whose last signal is older than minAge seconds.
//
// A delivery that has been offered once used to be invisible to Pending for
// good: there is no offered -> stored transition, so a single offer switched
// the wake signal off for that message forever. At wake level the socket
// confirms the offer as accepted, so it never showed; at hold level the
// delivery stayed in `offered`, and a chat that never called msg_recv simply
// stopped being told. On the live store one session had eleven messages in
// `offered` and not one `read`.
//
// minAge is what keeps this from becoming the loop it was written to avoid.
// A caller that can block a turn (the Stop hook) passes a window long enough
// that it never re-signals within one turn; a caller that only adds context
// (a tool event) can pass 0, because it cannot extend a turn.
func (h *Hub) PendingSince(ctx context.Context, sessionID string, minAge int64) (int, error) {
	// An unknown id has no pending deliveries, which reads as "nothing to
	// announce" and silences the hook for a session that does exist under
	// another id. The check is one indexed lookup, and the hook only reaches
	// here after resolving the session, so it costs nothing in practice.
	if err := h.requireSession(ctx, sessionID); err != nil {
		return 0, fmt.Errorf("pending: %w", err)
	}
	cutoff := h.now() - minAge
	var n int
	// Only `offered` is retried. `accepted` means the harness took the signal
	// -- at wake level that is the socket write returning OK -- so the turn
	// is already coming and re-signalling it would start another one. That is
	// exactly the loop the strict rule existed to prevent, and including
	// `accepted` here would bring it back at wake level.
	err := h.db.SQL().QueryRowContext(ctx, `
		SELECT COUNT(*) FROM deliveries
		WHERE session_id = ?
		  AND (state = ? OR (state = ? AND updated_at <= ?))`,
		sessionID, StateStored, StateOffered, cutoff).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count pending: %w", err)
	}
	return n, nil
}

// Ack marks a message as confirmed by its recipient.
func (h *Hub) Ack(ctx context.Context, sessionID, messageID string) error {
	// Without this the error below says "no delivery of <id> for this
	// session", which is equally true of a message the caller never received
	// and of a session id that names nothing. The caller needs to know which.
	if err := h.requireSession(ctx, sessionID); err != nil {
		return fmt.Errorf("ack: %w", err)
	}
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
