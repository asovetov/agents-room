// Package hub implements rooms, sessions, messages, leases and drift on top
// of the shared store. It is the whole protocol; the CLI, the MCP server and
// the harness hooks are thin front ends over it.
package hub

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/asovetov/agents-room/internal/store"
)

// Hub is an open handle on the shared store.
type Hub struct {
	db  *store.DB
	now func() int64
}

// Open opens the store at the default location.
func Open() (*Hub, error) {
	db, err := store.OpenDefault()
	if err != nil {
		return nil, err
	}
	return &Hub{db: db, now: store.Now}, nil
}

// OpenPath opens the store at an explicit path.
func OpenPath(path string) (*Hub, error) {
	db, err := store.Open(path)
	if err != nil {
		return nil, err
	}
	return &Hub{db: db, now: store.Now}, nil
}

// setClock replaces the clock. Tests use it to move time without sleeping.
func (h *Hub) setClock(f func() int64) { h.now = f }

// Close releases the store.
func (h *Hub) Close() error { return h.db.Close() }

// Path returns the database file path.
func (h *Hub) Path() string { return h.db.Path() }

// SchemaVersion reports the applied migration number.
func (h *Hub) SchemaVersion(ctx context.Context) (int, error) {
	return h.db.SchemaVersion(ctx)
}

// Session is a connected agent process.
//
// A session can be in several rooms at once: its repository room and any
// number of shared rooms. Rooms lists all of them.
type Session struct {
	ID       string   `json:"id"`
	Rooms    []string `json:"rooms"`
	Harness  string   `json:"harness"`
	Cwd      string   `json:"cwd"`
	PID      int      `json:"pid"`
	Label    string   `json:"label"`
	Tags     []string `json:"tags"`
	Autonomy int      `json:"autonomy"`
	JoinedAt int64    `json:"joined_at"`
	LastSeen int64    `json:"last_seen"`
	// Liveness reports whether the chat behind this session is still running:
	// LivenessAlive, LivenessGone, or LivenessUnknown when the row records no
	// pid at all.
	//
	// It is an answer about the process, not about activity. A chat that has
	// been quiet for an hour is alive; a row whose chat exited is gone,
	// however recently it was seen. That distinction is the whole point:
	// last_seen alone cannot tell a thinking agent from a dead one.
	Liveness string `json:"liveness"`
	// PIDStartedAt is the start time of PID, recorded when the pid was
	// stored. Together they name one process, so a pid the OS later reuses
	// is not mistaken for the chat that used to hold it. Zero means the
	// platform could not report it.
	PIDStartedAt int64 `json:"pid_started_at,omitempty"`
	// Warning is set when the join succeeded but looks like a mistake the
	// caller can still correct. It is not an error: a join that is right for
	// an unusual setup must still work.
	Warning string `json:"warning,omitempty"`
}

// Liveness is what a session row can honestly say about its chat.
const (
	// LivenessAlive means the recorded process is still running.
	LivenessAlive = "alive"
	// LivenessGone means it exited. The row is a leftover.
	LivenessGone = "gone"
	// LivenessUnknown means no pid was recorded, so nobody can tell. It is
	// not the same as gone and is never treated as one.
	LivenessUnknown = "unknown"
)

// livenessOf decides a session's liveness from its recorded pid.
//
// A pid that is not running is gone. A pid that is running is only the same
// process if its start time matches the one recorded with it; when the start
// time was never recorded (a row from an older binary, or a platform that
// cannot report it) the pid alone has to do, which is the pre-existing
// behaviour. A zero pid means the join could not find a harness process and
// recorded nothing, and guessing from that would delete live sessions.
func livenessOf(pid int, startedAt int64) string {
	if pid <= 0 {
		return LivenessUnknown
	}
	if !processAlive(pid) {
		return LivenessGone
	}
	if startedAt > 0 {
		if current := processStartTime(pid); current > 0 && current != startedAt {
			// Something holds the number, but it is not the process the row
			// was written for.
			return LivenessGone
		}
	}
	return LivenessAlive
}

// sessionLiveness decides liveness from the harness key when the platform can
// report which keys are running, and from the pid otherwise.
//
// The key is the better answer. A chat keeps its key across MCP restarts,
// while the pid the row recorded changes with each one; a row whose pid is
// stale is therefore still a live chat, and calling it gone would send an
// operator to prune a session that is working.
func sessionLiveness(harnessKey string, pid int, startedAt int64,
	liveKeys map[string]bool, keysKnown bool) string {
	if keysKnown && harnessKey != "" {
		if liveKeys[harnessKey] {
			return LivenessAlive
		}
		// The key is not in any readable environment. That is evidence the
		// chat is gone, but not proof: a process this user cannot read would
		// be invisible to the scan too. The recorded process agreeing that it
		// is gone is what makes it conclusive; when the pid still looks
		// alive, the two tests disagree and neither is trusted alone.
		if livenessOf(pid, startedAt) == LivenessGone {
			return LivenessGone
		}
		if pid > 0 {
			return LivenessAlive
		}
		return LivenessUnknown
	}
	return livenessOf(pid, startedAt)
}

// Autonomy levels. The effective level is the minimum of what the agent
// proposes and what the operator allows for the room.
const (
	// AutonomyNotify only signals; it never holds the end of a turn.
	AutonomyNotify = 0
	// AutonomyHold holds the end of a turn until unread messages are fetched.
	AutonomyHold = 1
	// AutonomyWake may also start a turn in an idle session.
	AutonomyWake = 2
)

// JoinOptions describes a session joining a room.
//
// ExtraRooms are additional shared rooms the session joins alongside Room.
// A session stays in its repository room and can be addressed from any
// shared room it joined, which is how agents in different repositories
// reach each other.
type JoinOptions struct {
	Room       string
	ExtraRooms []string
	Harness    string
	HarnessKey string // stable id from the harness; makes a rejoin idempotent
	Label      string
	Tags       []string
	Autonomy   int
	PID        int
	Cwd        string
}

// Join registers a session in a room and returns it.
//
// The room is created on first use. Autonomy is clamped to the room cap,
// which defaults to AutonomyWake and can be lowered by the operator.
//
// When HarnessKey is set, joining twice with the same key in the same room
// updates the existing row instead of creating a second session. Harnesses
// fire SessionStart on startup, resume, clear, compact and fork; without
// this, one chat would appear as five sessions.
func (h *Hub) Join(ctx context.Context, opt JoinOptions) (*Session, error) {
	if opt.Room == "" {
		opt.Room = RepoRoom("")
	}
	if opt.Cwd == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("get working directory: %w", err)
		}
		opt.Cwd = cwd
	}
	if opt.Harness == "" {
		opt.Harness = HarnessName()
	}
	// A session belongs to the harness chat, not to the process that happens
	// to be joining. `agents-room join` and `agents-room mcp` are both
	// short-lived, so os.Getpid() here names a process that exits within
	// milliseconds; prune would then see the row as dead and delete a live
	// chat. Callers that know the harness process pass it explicitly.
	if opt.PID == 0 {
		opt.PID = HarnessPIDFromEnv()
	}
	// Record the pid together with the moment the process started. A pid on
	// its own stops naming a chat as soon as the chat exits and the number is
	// reused, and both room_who and prune would then answer about whatever
	// process inherited it.
	pidStartedAt := processStartTime(opt.PID)
	if opt.Autonomy < AutonomyNotify {
		return nil, fmt.Errorf("autonomy must be 0, 1 or 2, got %d", opt.Autonomy)
	}

	now := h.now()
	rooms := append([]string{opt.Room}, opt.ExtraRooms...)
	roomIDs := make([]string, 0, len(rooms))
	for i, name := range rooms {
		// A shared room is spelled "shared:<name>", and callers forget the
		// prefix. Without this, `--also storefront` and
		// AGENTS_ROOM_SHARED=storefront would be two rooms that look like one
		// and never meet.
		if i > 0 {
			name = NormalizeSharedRoom(name)
			rooms[i] = name
		}
		id, err := h.ensureRoom(ctx, name, now)
		if err != nil {
			return nil, err
		}
		roomIDs = append(roomIDs, id)
	}

	// The cap that applies is the strictest of the rooms the session is in:
	// a shared room set to notify must not be bypassed by joining a
	// repository room set to wake.
	effective := opt.Autonomy
	for _, id := range roomIDs {
		c, err := h.roomAutonomyCap(ctx, id)
		if err != nil {
			return nil, err
		}
		if c < effective {
			effective = c
		}
	}

	if opt.Tags == nil {
		opt.Tags = []string{}
	}
	tags, err := json.Marshal(opt.Tags)
	if err != nil {
		return nil, fmt.Errorf("encode tags: %w", err)
	}

	var sessionID string
	if opt.HarnessKey != "" {
		existing, err := h.sessionByHarnessKey(ctx, opt.Harness, opt.HarnessKey)
		if err != nil {
			return nil, err
		}
		if existing != nil {
			sessionID = existing.ID
			if _, err := h.db.SQL().ExecContext(ctx, `
				UPDATE sessions
				SET cwd = ?, pid = ?, pid_started_at = ?, label = ?, tags = ?, autonomy = ?, last_seen = ?
				WHERE id = ?`,
				opt.Cwd, opt.PID, pidStartedAt, opt.Label, string(tags), effective, now, sessionID); err != nil {
				return nil, fmt.Errorf("update session: %w", err)
			}
		}
	}

	if sessionID == "" {
		sessionID, err = newID()
		if err != nil {
			return nil, err
		}
		// room_id is the legacy single-room column. It keeps the primary
		// room for a downgrade; memberships live in session_rooms.
		_, err = h.db.SQL().ExecContext(ctx, `
			INSERT INTO sessions
				(id, room_id, harness, cwd, pid, pid_started_at, label, tags,
				 autonomy, joined_at, last_seen, harness_key)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			sessionID, roomIDs[0], opt.Harness, opt.Cwd, opt.PID, pidStartedAt,
			opt.Label, string(tags), effective, now, now, opt.HarnessKey)
		if err != nil {
			return nil, fmt.Errorf("insert session: %w", err)
		}
	}

	for i, id := range roomIDs {
		// joined_at is incremented per room so the join order is recorded,
		// not just the moment. All the rooms of one join share a second, so
		// without this the repository room and a shared room are
		// indistinguishable and "the session's own room" becomes whichever
		// row SQLite returns first.
		if _, err := h.db.SQL().ExecContext(ctx, `
			INSERT INTO session_rooms (session_id, room_id, joined_at)
			VALUES (?, ?, ?)
			ON CONFLICT(session_id, room_id) DO NOTHING`,
			sessionID, id, now+int64(i)); err != nil {
			return nil, fmt.Errorf("join room %q: %w", rooms[i], err)
		}
	}

	kind := "session.join"
	if opt.HarnessKey != "" {
		kind = "session.rejoin"
	}
	for _, id := range roomIDs {
		if err := h.appendEvent(ctx, id, kind, map[string]any{
			"session": sessionID, "harness": opt.Harness, "label": opt.Label,
			"autonomy": effective,
		}); err != nil {
			return nil, err
		}
	}

	return &Session{
		ID: sessionID, Rooms: rooms, Harness: opt.Harness, Cwd: opt.Cwd,
		PID: opt.PID, PIDStartedAt: pidStartedAt, Label: opt.Label,
		Tags: opt.Tags, Autonomy: effective,
		JoinedAt: now, LastSeen: now,
		Liveness: livenessOf(opt.PID, pidStartedAt),
		Warning:  harnessKeyWarning(opt.HarnessKey),
	}, nil
}

// harnessKeyWarning reports a harness key that does not belong to the process
// asking, which is almost always a copy-paste from another chat.
//
// It is a warning, not a refusal: a caller may legitimately run under a key
// it set itself, and the room must not break for that. But passing the wrong
// key silently adopts another chat's session, which is how one chat came to
// answer messages addressed to a different one.
func harnessKeyWarning(key string) string {
	env := strings.TrimSpace(os.Getenv("CLAUDE_CODE_SESSION_ID"))
	if key == "" || env == "" || key == env {
		return ""
	}
	return fmt.Sprintf("harness_key %q is not this process's CLAUDE_CODE_SESSION_ID (%q); "+
		"the session of another chat may have been adopted", key, env)
}

// Leave removes a session from every room and releases every lease it holds.
func (h *Hub) Leave(ctx context.Context, sessionID string) error {
	roomIDs, err := h.sessionRoomIDs(ctx, sessionID)
	if err != nil {
		return err
	}
	if _, err := h.db.SQL().ExecContext(ctx,
		`DELETE FROM sessions WHERE id = ?`, sessionID); err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	if _, err := h.db.SQL().ExecContext(ctx,
		`DELETE FROM leases WHERE owner = ?`, sessionID); err != nil {
		return fmt.Errorf("release leases: %w", err)
	}
	// deliveries.session_id has no foreign key, so the session's rows do not
	// cascade. Left behind they are counted by Unread and Pending forever,
	// and no one can read them: the session that would fetch them is gone.
	//
	// What the deletion must not do is make the sender's history lie. The
	// count recorded in the send event survives, so `sent` can tell "nobody
	// read it" from "the recipient left before reading it" without keeping a
	// row for a session that no longer exists.
	if _, err := h.db.SQL().ExecContext(ctx,
		`DELETE FROM deliveries WHERE session_id = ?`, sessionID); err != nil {
		return fmt.Errorf("drop deliveries: %w", err)
	}
	for _, id := range roomIDs {
		if err := h.appendEvent(ctx, id, "session.leave", map[string]any{"session": sessionID}); err != nil {
			return err
		}
	}
	return nil
}

// LeaveRoom removes a session from one room and keeps it in the others.
//
// Leases the session holds in that room are released with it. This is how an
// agent leaves a shared room when its part of the contract work is done
// without dropping out of its own repository room.
func (h *Hub) LeaveRoom(ctx context.Context, sessionID, room string) error {
	roomID, err := h.roomID(ctx, room)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("leave: no room %q", room)
	}
	if err != nil {
		return fmt.Errorf("leave: %w", err)
	}
	res, err := h.db.SQL().ExecContext(ctx,
		`DELETE FROM session_rooms WHERE session_id = ? AND room_id = ?`, sessionID, roomID)
	if err != nil {
		return fmt.Errorf("leave: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("leave: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("leave: session is not in room %q", room)
	}
	if _, err := h.db.SQL().ExecContext(ctx,
		`DELETE FROM leases WHERE owner = ? AND room_id = ?`, sessionID, roomID); err != nil {
		return fmt.Errorf("leave: release leases: %w", err)
	}
	return h.appendEvent(ctx, roomID, "session.leave_room", map[string]any{"session": sessionID})
}

// Who lists the sessions in a room, most recently seen first.
func (h *Hub) Who(ctx context.Context, room string) ([]Session, error) {
	roomID, err := h.roomID(ctx, room)
	if errors.Is(err, store.ErrNotFound) {
		// Return an empty slice, not nil: a JSON encoder renders nil as
		// null, and callers expect a list.
		return []Session{}, nil
	}
	if err != nil {
		return nil, err
	}

	rows, err := h.db.SQL().QueryContext(ctx, `
		SELECT s.id, s.harness, s.cwd, s.pid, s.pid_started_at, s.harness_key,
		       s.label, s.tags, s.autonomy, s.joined_at, s.last_seen
		FROM sessions s
		JOIN session_rooms sr ON sr.session_id = s.id
		WHERE sr.room_id = ?
		ORDER BY s.last_seen DESC`, roomID)
	if err != nil {
		return nil, fmt.Errorf("query sessions: %w", err)
	}
	defer rows.Close()

	// One /proc scan answers for every session below. The harness key is the
	// better test than the pid: the MCP server is restarted under a live chat
	// and every restart changes the recorded pid, while the key does not.
	liveKeys, keysKnown := liveHarnessKeys()

	out := []Session{}
	for rows.Next() {
		s := Session{Rooms: []string{room}}
		var tags, harnessKey string
		if err := rows.Scan(&s.ID, &s.Harness, &s.Cwd, &s.PID, &s.PIDStartedAt,
			&harnessKey, &s.Label, &tags, &s.Autonomy, &s.JoinedAt, &s.LastSeen); err != nil {
			return nil, fmt.Errorf("scan session: %w", err)
		}
		s.Tags = []string{}
		if err := json.Unmarshal([]byte(tags), &s.Tags); err != nil {
			s.Tags = []string{}
		}
		s.Liveness = sessionLiveness(harnessKey, s.PID, s.PIDStartedAt, liveKeys, keysKnown)
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Fill in every room each session is in, so a caller sees the full
	// picture rather than only the room it asked about.
	for i := range out {
		if rooms, err := h.RoomsOf(ctx, out[i].ID); err == nil {
			out[i].Rooms = rooms
		}
	}
	return out, nil
}

// RoomOf returns the session's repository room: the first room it joined.
// Use RoomsOf for the full list.
func (h *Hub) RoomOf(ctx context.Context, sessionID string) string {
	rooms, err := h.RoomsOf(ctx, sessionID)
	if err != nil || len(rooms) == 0 {
		return ""
	}
	return rooms[0]
}

// RoomsOf returns every room a session belongs to, oldest membership first.
func (h *Hub) RoomsOf(ctx context.Context, sessionID string) ([]string, error) {
	rows, err := h.db.SQL().QueryContext(ctx, `
		SELECT r.name
		FROM session_rooms sr
		JOIN rooms r ON r.id = sr.room_id
		WHERE sr.session_id = ?
		ORDER BY sr.joined_at ASC, r.name ASC`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("list session rooms: %w", err)
	}
	defer rows.Close()

	out := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan room: %w", err)
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// sessionRoomIDs returns the room ids a session belongs to, oldest membership
// first.
//
// The order matters: callers use the first id as "the session's room", for the
// room a lease or a message is filed under. Without it SQLite may return the
// rows in any order, so a broadcast from a chat in three rooms was filed under
// whichever one came back first -- on the live store a `repo:gcs` message was
// attributed to `storefront`.
func (h *Hub) sessionRoomIDs(ctx context.Context, sessionID string) ([]string, error) {
	rows, err := h.db.SQL().QueryContext(ctx,
		`SELECT room_id FROM session_rooms WHERE session_id = ?
		 ORDER BY joined_at ASC, room_id ASC`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("list session rooms: %w", err)
	}
	defer rows.Close()

	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan room id: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, store.ErrNotFound
	}
	return out, nil
}

// PruneStale removes sessions that have not been seen for longer than maxAge
// and releases their leases.
//
// A harness that dies without firing its end event leaves a row behind. Its
// leases still expire on their own, but the row would keep showing in
// room_who forever, so an operator can sweep it.
//
// A session is only removed when both tests agree: it has been quiet longer
// than maxAge and its process is gone. last_seen alone is not enough. It
// moves on tool calls and on a wake nudge, so an agent that is thinking, or
// sitting in a long edit, is quiet while very much alive; pruning it would
// delete the row and release the leases on files it is still writing.
//
// "Gone" is what room_who would call gone, decided by the same function. The
// two must not disagree: pruning a session room_who still shows as alive, or
// keeping one it shows as gone, leaves the operator with two commands that
// contradict each other. So the pid is not the test here either -- a chat
// whose MCP server was restarted keeps working under a pid the row no longer
// records -- and the harness key is.
func (h *Hub) PruneStale(ctx context.Context, room string, maxAge time.Duration) (int, error) {
	roomID, err := h.roomID(ctx, room)
	if errors.Is(err, store.ErrNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	cutoff := h.now() - int64(maxAge.Seconds())

	// Membership is session_rooms, not the legacy sessions.room_id column.
	// Filtering on room_id would make `prune --room shared:storefront` see
	// nobody: a session's primary room is the one it joined first.
	rows, err := h.db.SQL().QueryContext(ctx, `
		SELECT s.id, s.pid, s.pid_started_at, s.harness_key FROM sessions s
		JOIN session_rooms sr ON sr.session_id = s.id
		WHERE sr.room_id = ? AND s.last_seen < ?`, roomID, cutoff)
	if err != nil {
		return 0, fmt.Errorf("prune: list stale: %w", err)
	}
	// The same scan room_who uses, so both give the same answer about the
	// same row.
	liveKeys, keysKnown := liveHarnessKeys()
	var ids []string
	for rows.Next() {
		var id string
		var pid int
		var startedAt int64
		var harnessKey string
		if err := rows.Scan(&id, &pid, &startedAt, &harnessKey); err != nil {
			rows.Close()
			return 0, fmt.Errorf("prune: scan: %w", err)
		}
		// Quiet but running is not stale: the agent is thinking or editing.
		// A row that is unknown is never pruned either -- deleting on a guess
		// would take a live chat's leases.
		if sessionLiveness(harnessKey, pid, startedAt, liveKeys, keysKnown) != LivenessGone {
			continue
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("prune: rows: %w", err)
	}

	for _, id := range ids {
		if _, err := h.db.SQL().ExecContext(ctx,
			`DELETE FROM sessions WHERE id = ?`, id); err != nil {
			return 0, fmt.Errorf("prune: delete session: %w", err)
		}
		if _, err := h.db.SQL().ExecContext(ctx,
			`DELETE FROM leases WHERE owner = ?`, id); err != nil {
			return 0, fmt.Errorf("prune: release leases: %w", err)
		}
		// Same reason as in Leave: the column has no foreign key, so the
		// rows would outlive the session and be counted as unread forever.
		if _, err := h.db.SQL().ExecContext(ctx,
			`DELETE FROM deliveries WHERE session_id = ?`, id); err != nil {
			return 0, fmt.Errorf("prune: drop deliveries: %w", err)
		}
	}
	if len(ids) > 0 {
		if err := h.appendEvent(ctx, roomID, "session.prune", map[string]any{"count": len(ids)}); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}

// Touch updates a session's last_seen so the room shows it as alive.
func (h *Hub) Touch(ctx context.Context, sessionID string) error {
	now := h.now()
	// A tool call is also proof of which process the chat is running as now,
	// so the pid is refreshed along with last_seen -- but only on the row
	// this process can prove it owns. A caller may touch a session that is
	// not its own (an operator naming another session), and writing this
	// process's pid there would attach a stranger to it. The condition is in
	// the SQL rather than in Go so that last_seen is updated in every case:
	// the CASE falls through to the recorded pid when the keys do not match.
	//
	// Without the refresh the pid goes stale the first time the MCP server is
	// restarted under a live chat. last_seen keeps moving while the recorded
	// number points at a process that exited, and every liveness check then
	// calls a working chat gone.
	key := HarnessKeyFromEnv()
	pid := 0
	startedAt := int64(0)
	if key != "" {
		if pid = HarnessPIDFromEnv(); pid > 0 {
			startedAt = processStartTime(pid)
		}
	}
	if _, err := h.db.SQL().ExecContext(ctx, `
		UPDATE sessions SET
			last_seen = ?,
			pid_started_at = CASE WHEN ? > 0 AND harness_key = ? AND harness_key <> ''
			                      THEN ? ELSE pid_started_at END,
			pid = CASE WHEN ? > 0 AND harness_key = ? AND harness_key <> ''
			           THEN ? ELSE pid END
		WHERE id = ?`,
		now, pid, key, startedAt, pid, key, pid, sessionID); err != nil {
		return fmt.Errorf("touch session: %w", err)
	}
	return nil
}

func (h *Hub) ensureRoom(ctx context.Context, name string, now int64) (string, error) {
	var id string
	err := h.db.SQL().QueryRowContext(ctx,
		`SELECT id FROM rooms WHERE name = ?`, name).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("look up room: %w", err)
	}

	id, err = newID()
	if err != nil {
		return "", err
	}
	if _, err := h.db.SQL().ExecContext(ctx,
		`INSERT INTO rooms (id, name, created_at) VALUES (?, ?, ?)`,
		id, name, now); err != nil {
		return "", fmt.Errorf("create room: %w", err)
	}
	return id, nil
}

func (h *Hub) roomID(ctx context.Context, name string) (string, error) {
	var id string
	err := h.db.SQL().QueryRowContext(ctx,
		`SELECT id FROM rooms WHERE name = ?`, name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", store.ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("look up room: %w", err)
	}
	return id, nil
}

func (h *Hub) sessionRoom(ctx context.Context, sessionID string) (string, error) {
	var roomID string
	err := h.db.SQL().QueryRowContext(ctx,
		`SELECT room_id FROM sessions WHERE id = ?`, sessionID).Scan(&roomID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", store.ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("look up session: %w", err)
	}
	return roomID, nil
}

// roomAutonomyCap returns the highest autonomy the operator allows in a room.
//
// The cap is stored per room and defaults to AutonomyWake, so an operator
// can quiet a whole room without touching each session.
func (h *Hub) roomAutonomyCap(ctx context.Context, roomID string) (int, error) {
	var cap int
	err := h.db.SQL().QueryRowContext(ctx,
		`SELECT autonomy_cap FROM rooms_meta WHERE room_id = ?`, roomID).Scan(&cap)
	if errors.Is(err, sql.ErrNoRows) {
		return AutonomyWake, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read room autonomy cap: %w", err)
	}
	if cap < AutonomyNotify || cap > AutonomyWake {
		return AutonomyWake, nil
	}
	return cap, nil
}

// SetRoomAutonomyCap lowers or raises the ceiling for a room.
//
// It returns the cap actually in force. A session that asked for more keeps
// its lower effective level until it rejoins, which is deliberate: raising a
// cap must not silently grant interrupt rights to sessions already running.
func (h *Hub) SetRoomAutonomyCap(ctx context.Context, room string, cap int) (int, error) {
	if cap < AutonomyNotify || cap > AutonomyWake {
		return 0, fmt.Errorf("autonomy cap must be 0, 1 or 2, got %d", cap)
	}
	now := h.now()
	roomID, err := h.ensureRoom(ctx, room, now)
	if err != nil {
		return 0, err
	}
	if _, err := h.db.SQL().ExecContext(ctx, `
		INSERT INTO rooms_meta (room_id, autonomy_cap) VALUES (?, ?)
		ON CONFLICT(room_id) DO UPDATE SET autonomy_cap = excluded.autonomy_cap`,
		roomID, cap); err != nil {
		return 0, fmt.Errorf("set room autonomy cap: %w", err)
	}
	if err := h.appendEvent(ctx, roomID, "room.autonomy_cap", map[string]any{"cap": cap}); err != nil {
		return 0, err
	}
	return cap, nil
}

// RoomAutonomyCap reports the cap in force for a room.
func (h *Hub) RoomAutonomyCap(ctx context.Context, room string) (int, error) {
	roomID, err := h.roomID(ctx, room)
	if errors.Is(err, store.ErrNotFound) {
		return AutonomyWake, nil
	}
	if err != nil {
		return 0, err
	}
	return h.roomAutonomyCap(ctx, roomID)
}

// sessionByHarnessKey finds a session previously joined under the same
// harness key, or nil when there is none.
func (h *Hub) sessionByHarnessKey(ctx context.Context, harness, key string) (*Session, error) {
	s := Session{Harness: harness}
	var tags string
	// A harness session is one chat regardless of how many rooms it joined,
	// so the lookup is not scoped to a room. Rejoining from a second room
	// must find the same session, not create a second one.
	err := h.db.SQL().QueryRowContext(ctx, `
		SELECT id, cwd, pid, label, tags, autonomy, joined_at, last_seen
		FROM sessions
		WHERE harness = ? AND harness_key = ?
		ORDER BY joined_at ASC LIMIT 1`,
		harness, key).Scan(&s.ID, &s.Cwd, &s.PID, &s.Label, &tags,
		&s.Autonomy, &s.JoinedAt, &s.LastSeen)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find session by harness key: %w", err)
	}
	s.Tags = []string{}
	_ = json.Unmarshal([]byte(tags), &s.Tags)
	rooms, err := h.RoomsOf(ctx, s.ID)
	if err != nil {
		return nil, err
	}
	s.Rooms = rooms
	return &s, nil
}

// FindSession resolves a harness session key to a room session. Hooks use it
// to map a harness session id to the room session id without knowing the
// latter.
//
// When room is empty the session is looked up across every room, which is
// what a hook needs: it may fire before the session has joined anything.
func (h *Hub) FindSession(ctx context.Context, room, harness, key string) (*Session, error) {
	if room == "" {
		return h.sessionByHarnessKey(ctx, harness, key)
	}
	roomID, err := h.roomID(ctx, room)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	s, err := h.sessionByHarnessKey(ctx, harness, key)
	if err != nil || s == nil {
		return s, err
	}
	for _, id := range mustRoomIDs(ctx, h, s.ID) {
		if id == roomID {
			return s, nil
		}
	}
	return nil, nil
}

// mustRoomIDs is a small helper: membership ids for a session, ignoring the
// error because the caller already has the session row.
func mustRoomIDs(ctx context.Context, h *Hub, sessionID string) []string {
	rows, err := h.db.SQL().QueryContext(ctx,
		`SELECT room_id FROM session_rooms WHERE session_id = ?`, sessionID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			out = append(out, id)
		}
	}
	return out
}

// SharedRoomsFrom discovers shared rooms a session should join without being
// told, so that a "shared by default" setup needs no per-agent flag.
//
// Two sources, in order:
//
//  1. AGENTS_ROOM_SHARED: comma-separated room names, from the environment
//     the harness passes to its hooks. Set it once in your shell profile.
//  2. .agents-room/shared-rooms in the repository root: one room name per
//     line, "#" starts a comment. This is per-repository and travels with
//     the checkout, so a team gets the same rooms without extra setup.
//
// Names without a "shared:" prefix get one. Duplicates are removed. An
// unreadable file is not an error: a missing config means no shared rooms.
func SharedRoomsFrom(dir string) []string {
	seen := map[string]bool{}
	out := []string{}
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" || strings.HasPrefix(name, "#") {
			return
		}
		name = NormalizeSharedRoom(name)
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}

	for _, name := range strings.Split(os.Getenv("AGENTS_ROOM_SHARED"), ",") {
		add(name)
	}

	root := repoRoot(dir)
	data, err := os.ReadFile(filepath.Join(root, ".agents-room", "shared-rooms"))
	if err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if i := strings.Index(line, "#"); i >= 0 {
				line = line[:i]
			}
			add(line)
		}
	}
	return out
}

// RoomNames lists every room name in the store, oldest first.
func (h *Hub) RoomNames(ctx context.Context) []string {
	rows, err := h.db.SQL().QueryContext(ctx,
		`SELECT name FROM rooms ORDER BY created_at ASC, name ASC`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if rows.Scan(&name) == nil {
			out = append(out, name)
		}
	}
	return out
}

// DuplicatePID is one chat registered as more than one session.
type DuplicatePID struct {
	// Identity is what ties the rows together: the harness key they share,
	// or the pid they share when neither carries a key.
	Identity string
	// Kind is "harness_key" or "pid".
	Kind string
	// PID is the process the rows name, when they name one.
	PID int
	IDs []string
}

// DuplicatePIDs finds one chat holding several session rows.
//
// A harness runs one chat per process, so two rows for one pid are the same
// chat counted twice; that is how room_join minted a session without the
// harness key, beside the one the SessionStart hook had already made.
//
// Grouping by pid alone misses the other half of the same bug. A row created
// by room_join records the pid of the MCP server that answered it, and that
// process exits when the chat does; the row keeps the dead number while the
// hook's row, which does carry the key, records the chat's own pid. The two
// rows are then one chat with two pids, and a pid grouping never sees them
// together. A harness key is therefore tried first: two rows with the same
// key are the same chat whatever their pids say.
func (h *Hub) DuplicatePIDs(ctx context.Context) []DuplicatePID {
	var out []DuplicatePID

	// Identity is the harness key a row carries, or the key of the process it
	// names when it carries none. The second half is what ties a ghost row --
	// minted by room_join, with no key and a pid that died with the MCP server
	// -- to the keyed row of the same chat: the dead pid's process, while it
	// lived, had the chat's own session id in its environment.
	type identified struct {
		id       string
		identity string
	}
	var rows_ []identified
	rows, err := h.db.SQL().QueryContext(ctx,
		`SELECT id, harness_key, pid FROM sessions ORDER BY joined_at`)
	if err != nil {
		return nil
	}
	for rows.Next() {
		var r identified
		var pid int
		if rows.Scan(&r.id, &r.identity, &pid) != nil {
			continue
		}
		if r.identity == "" {
			r.identity = harnessKeyOfProcess(pid)
		}
		rows_ = append(rows_, r)
	}
	rows.Close()

	byIdentity := map[string][]string{}
	order := []string{}
	for _, r := range rows_ {
		if r.identity == "" {
			continue
		}
		if _, ok := byIdentity[r.identity]; !ok {
			order = append(order, r.identity)
		}
		byIdentity[r.identity] = append(byIdentity[r.identity], r.id)
	}
	for _, identity := range order {
		if ids := byIdentity[identity]; len(ids) > 1 {
			out = append(out, DuplicatePID{
				Identity: identity, Kind: "harness_key", IDs: ids,
			})
		}
	}

	// Which rows a key group already covers, and by which identity.
	// A pid group is a *different* defect even when its rows are also in a
	// key group: two rows on one pid are one chat registered twice, and the
	// key grouping does not say so. Filtering those rows out entirely is what
	// let a live store hold three rows for one chat while doctor reported
	// only the key collision and named none of the pid duplicates.
	byKey := map[string]string{}
	for _, d := range out {
		for _, id := range d.IDs {
			byKey[id] = d.Identity
		}
	}

	rows, err = h.db.SQL().QueryContext(ctx, `
		SELECT pid, GROUP_CONCAT(id) FROM sessions
		WHERE pid > 0
		GROUP BY pid HAVING COUNT(*) > 1
		ORDER BY pid`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var d DuplicatePID
		var ids string
		if rows.Scan(&d.PID, &ids) != nil {
			continue
		}
		// A pid group is skipped only when every one of its rows is already
		// accounted for by the same key: that is one finding, not two.
		identity := ""
		sameGroup := true
		var fresh []string
		for _, id := range strings.Split(ids, ",") {
			key, ok := byKey[id]
			if !ok {
				fresh = append(fresh, id)
				sameGroup = false
				continue
			}
			if identity == "" {
				identity = key
			} else if identity != key {
				sameGroup = false
			}
			fresh = append(fresh, id)
		}
		if sameGroup {
			continue
		}
		d.Kind = "pid"
		d.Identity = strconv.Itoa(d.PID)
		d.IDs = fresh
		out = append(out, d)
	}
	return out
}

// DuplicateFix is what doctor would do to collapse a chat that registered
// more than once: the id it keeps, the ids it removes, and what it does with
// the messages the removed rows were still holding.
type DuplicateFix struct {
	Identity string   `json:"identity"`
	Kind     string   `json:"kind"`
	PID      int      `json:"pid,omitempty"`
	Keep     string   `json:"keep"`
	Remove   []string `json:"remove"`
	// Moved counts deliveries carried over to the kept session. A row that
	// still had unread messages has them for a reason: it was a real member
	// of the room, and dropping them would discard a message nobody read.
	Moved int `json:"moved,omitempty"`
	// Because explains the choice in one line, for an operator reading it.
	Because string `json:"because"`
}

// PlanDuplicateFix decides, for every chat registered more than once, which
// row to keep and what to do with the rest.
//
// The rule is the one a person would apply by hand, in order:
//
//  1. a row with a harness key survives. The key is the durable identity:
//     the harness rejoins through it, and a rejoin updates that row's pid and
//     start time. The other row can never heal itself -- nothing will ever
//     write to it again -- so keeping it means the next SessionStart mints a
//     fresh keyed row and the duplicate is back;
//  2. between two rows with no key, the one that is alive survives, because
//     it is the row whose process still exists;
//  3. the oldest survives, because other agents already have its id.
//
// Liveness deliberately ranks below the key. It is tempting to keep the row
// that looks alive, and it is wrong: the pid on a keyless row is the MCP
// server that answered the join, which is alive for as long as the chat is
// and says nothing about which row the chat will come back as.
//
// Nothing is changed here: the caller decides whether to apply it.
func (h *Hub) PlanDuplicateFix(ctx context.Context) ([]DuplicateFix, error) {
	dups := h.DuplicatePIDs(ctx)
	out := make([]DuplicateFix, 0, len(dups))
	for _, d := range dups {
		type candidate struct {
			id         string
			harnessKey string
			joinedAt   int64
			liveness   string
			deliveries int
		}
		var cands []candidate
		for _, id := range d.IDs {
			var c candidate
			var pid int
			var startedAt int64
			if err := h.db.SQL().QueryRowContext(ctx,
				`SELECT id, harness_key, joined_at, pid, pid_started_at FROM sessions WHERE id = ?`, id).
				Scan(&c.id, &c.harnessKey, &c.joinedAt, &pid, &startedAt); err != nil {
				continue
			}
			c.liveness = livenessOf(pid, startedAt)
			_ = h.db.SQL().QueryRowContext(ctx,
				`SELECT COUNT(*) FROM deliveries WHERE session_id = ?`, id).Scan(&c.deliveries)
			cands = append(cands, c)
		}
		if len(cands) < 2 {
			continue
		}

		rank := func(c candidate) int {
			score := 0
			if c.harnessKey != "" {
				score += 2
			}
			if c.liveness == LivenessAlive {
				score++
			}
			return score
		}
		keep := cands[0]
		for _, c := range cands {
			// A higher score wins; at equal score the older row wins, so the
			// comparison never depends on the order the ids came back in.
			ks, cs := rank(keep), rank(c)
			if cs > ks || (cs == ks && c.joinedAt < keep.joinedAt) {
				keep = c
			}
		}

		because := "it joined first"
		switch {
		case keep.harnessKey != "" && keep.liveness == LivenessAlive:
			because = "it carries the harness key the chat rejoins with, and it is running"
		case keep.harnessKey != "":
			because = "it carries the harness key the chat rejoins with; the other row would never be updated again"
		case keep.liveness == LivenessAlive:
			because = "it is the row whose chat is still running"
		}

		fix := DuplicateFix{
			Identity: d.Identity, Kind: d.Kind, PID: d.PID,
			Keep: keep.id, Because: because,
		}
		for _, c := range cands {
			if c.id == keep.id {
				continue
			}
			fix.Remove = append(fix.Remove, c.id)
			fix.Moved += c.deliveries
		}
		out = append(out, fix)
	}
	return out, nil
}

// ApplyDuplicateFix removes the extra sessions a PlanDuplicateFix named,
// carrying their unread deliveries over to the session that survives.
//
// The deliveries have to move rather than disappear. The removed row was a
// real member of the room: it received the messages, and a message whose only
// delivery is deleted was never read by anyone. Leaving the row in place is
// not the alternative either -- it is the row that makes room_who count the
// chat twice. Moving is the only outcome that loses nothing.
func (h *Hub) ApplyDuplicateFix(ctx context.Context, fixes []DuplicateFix) (int, error) {
	n := 0
	for _, f := range fixes {
		for _, id := range f.Remove {
			if err := h.mergeDeliveries(ctx, id, f.Keep); err != nil {
				return n, fmt.Errorf("move deliveries from %s: %w", id, err)
			}
			if err := h.Leave(ctx, id); err != nil {
				return n, fmt.Errorf("remove duplicate %s: %w", id, err)
			}
			n++
		}
	}
	return n, nil
}

// mergeDeliveries re-points one session's deliveries at another.
//
// A delivery is keyed by (message_id, session_id), so a message both rows
// already hold would collide. The kept row's state wins in that case: it is
// the row whose reader exists, and a delivery can only move forward, so an
// older state must not overwrite a newer one.
func (h *Hub) mergeDeliveries(ctx context.Context, from, to string) error {
	if from == to {
		return nil
	}
	tx, err := h.db.SQL().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `
		UPDATE OR IGNORE deliveries SET session_id = ?, updated_at = ?
		WHERE session_id = ?`, to, h.now(), from); err != nil {
		return err
	}
	// Whatever the UPDATE skipped was a duplicate of a delivery the kept row
	// already had, so it is now redundant rather than lost.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM deliveries WHERE session_id = ?`, from); err != nil {
		return err
	}
	return tx.Commit()
}

// HasRoom reports whether a room with this name exists.
func (h *Hub) HasRoom(ctx context.Context, name string) bool {
	_, err := h.roomID(ctx, name)
	return err == nil
}

// NormalizeSharedRoom spells a shared room the one way the store knows it.
//
// "storefront" and "shared:storefront" are the same room to a human and two
// different rows to the store, so one of them silently collects nobody. The
// rule is the one the docs promise: a name without a prefix gets "shared:".
// A name with any other prefix ("repo:x") is left alone.
func NormalizeSharedRoom(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	if strings.Contains(name, ":") {
		return name
	}
	return "shared:" + name
}

// repoRoot returns the git top level of dir, or dir itself.
func repoRoot(dir string) string {
	if dir == "" {
		var err error
		dir, err = os.Getwd()
		if err != nil {
			return ""
		}
	}
	if out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output(); err == nil {
		if s := strings.TrimSpace(string(out)); s != "" {
			return s
		}
	}
	return dir
}

// RepoRoom derives the default room name from a directory: the git top level
// when there is one, otherwise the directory itself.
func RepoRoom(dir string) string {
	if dir == "" {
		var err error
		dir, err = os.Getwd()
		if err != nil {
			return "repo:default"
		}
	}
	return "repo:" + filepath.Base(repoRoot(dir))
}

// harnessProcessNames are the program names of the chats this project knows.
// The harness is recognised by its own name, never by "the nearest parent":
// a hook runs under a shell, and the shell exits before anyone looks.
var harnessProcessNames = map[string]bool{
	"claude": true,
	"codex":  true,
	"hermes": true,
}

// HarnessPIDFromEnv returns the pid of the harness process that owns this
// one, or 0 when it cannot be found.
//
// The join and MCP processes are short-lived children of the chat, and the
// shell between them is short-lived too, so neither their own pid nor their
// parent's names the session. The process chain is walked until a known
// harness appears; AGENTS_ROOM_PID short-circuits that when a harness sets
// it.
func HarnessPIDFromEnv() int {
	if v := strings.TrimSpace(os.Getenv("AGENTS_ROOM_PID")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	pid := os.Getppid()
	// A shell or two between the command and the chat is normal; a longer
	// chain means the harness is not what we think it is.
	for depth := 0; depth < 6 && pid > 1; depth++ {
		if harnessProcessNames[processName(pid)] {
			return pid
		}
		next, err := parentPID(pid)
		if err != nil || next == pid {
			break
		}
		pid = next
	}
	return 0
}

// HarnessKeyFromEnv returns the harness session id this process inherited, or
// an empty string when the harness does not export one.
//
// It exists so that a caller which forgets harness_key still rejoins its own
// session instead of minting a second one. Only variables the harness sets
// itself are read; anything else would key on the wrong thing.
func HarnessKeyFromEnv() string {
	for _, name := range []string{
		"CLAUDE_CODE_SESSION_ID",
		"CODEX_SESSION_ID",
		"HERMES_SESSION_ID",
	} {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			return v
		}
	}
	return ""
}

// HarnessName is a best-effort label for the current harness. It is never
// used for authorisation.
func HarnessName() string {
	switch {
	case os.Getenv("CLAUDECODE") != "" || os.Getenv("CLAUDE_CODE_ENTRYPOINT") != "":
		return "claude-code"
	case os.Getenv("CODEX_HOME") != "":
		return "codex"
	case os.Getenv("HERMES_HOME") != "":
		return "hermes"
	default:
		return "unknown"
	}
}

func newID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

func (h *Hub) appendEvent(ctx context.Context, roomID, kind string, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode event: %w", err)
	}
	if _, err := h.db.SQL().ExecContext(ctx,
		`INSERT INTO events (room_id, ts, kind, payload) VALUES (?, ?, ?, ?)`,
		roomID, h.now(), kind, string(raw)); err != nil {
		return fmt.Errorf("append event: %w", err)
	}
	return nil
}
