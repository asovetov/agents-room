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
				SET cwd = ?, pid = ?, label = ?, tags = ?, autonomy = ?, last_seen = ?
				WHERE id = ?`,
				opt.Cwd, opt.PID, opt.Label, string(tags), effective, now, sessionID); err != nil {
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
				(id, room_id, harness, cwd, pid, label, tags, autonomy, joined_at, last_seen, harness_key)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			sessionID, roomIDs[0], opt.Harness, opt.Cwd, opt.PID, opt.Label,
			string(tags), effective, now, now, opt.HarnessKey)
		if err != nil {
			return nil, fmt.Errorf("insert session: %w", err)
		}
	}

	for i, id := range roomIDs {
		if _, err := h.db.SQL().ExecContext(ctx, `
			INSERT INTO session_rooms (session_id, room_id, joined_at)
			VALUES (?, ?, ?)
			ON CONFLICT(session_id, room_id) DO NOTHING`,
			sessionID, id, now); err != nil {
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
		PID: opt.PID, Label: opt.Label, Tags: opt.Tags, Autonomy: effective,
		JoinedAt: now, LastSeen: now,
	}, nil
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
		SELECT s.id, s.harness, s.cwd, s.pid, s.label, s.tags, s.autonomy,
		       s.joined_at, s.last_seen
		FROM sessions s
		JOIN session_rooms sr ON sr.session_id = s.id
		WHERE sr.room_id = ?
		ORDER BY s.last_seen DESC`, roomID)
	if err != nil {
		return nil, fmt.Errorf("query sessions: %w", err)
	}
	defer rows.Close()

	out := []Session{}
	for rows.Next() {
		s := Session{Rooms: []string{room}}
		var tags string
		if err := rows.Scan(&s.ID, &s.Harness, &s.Cwd, &s.PID, &s.Label,
			&tags, &s.Autonomy, &s.JoinedAt, &s.LastSeen); err != nil {
			return nil, fmt.Errorf("scan session: %w", err)
		}
		s.Tags = []string{}
		if err := json.Unmarshal([]byte(tags), &s.Tags); err != nil {
			s.Tags = []string{}
		}
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

// sessionRoomIDs returns the room ids a session belongs to.
func (h *Hub) sessionRoomIDs(ctx context.Context, sessionID string) ([]string, error) {
	rows, err := h.db.SQL().QueryContext(ctx,
		`SELECT room_id FROM session_rooms WHERE session_id = ?`, sessionID)
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
		SELECT s.id, s.pid FROM sessions s
		JOIN session_rooms sr ON sr.session_id = s.id
		WHERE sr.room_id = ? AND s.last_seen < ?`, roomID, cutoff)
	if err != nil {
		return 0, fmt.Errorf("prune: list stale: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		var pid int
		if err := rows.Scan(&id, &pid); err != nil {
			rows.Close()
			return 0, fmt.Errorf("prune: scan: %w", err)
		}
		// Quiet but running is not stale: the agent is thinking or editing.
		if processAlive(pid) {
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
	_, err := h.db.SQL().ExecContext(ctx,
		`UPDATE sessions SET last_seen = ? WHERE id = ?`, h.now(), sessionID)
	if err != nil {
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

// DuplicatePID is one process registered as more than one session.
type DuplicatePID struct {
	PID  int
	IDs  []string
	Room string // the repository room the first of them is in
}

// DuplicatePIDs finds one process holding several session rows.
//
// A harness runs one chat per process, so two rows for one pid are the same
// chat counted twice. It happened when room_join minted a session without
// the harness key, beside the one the SessionStart hook had already made;
// room_who then reported more agents than exist.
func (h *Hub) DuplicatePIDs(ctx context.Context) []DuplicatePID {
	rows, err := h.db.SQL().QueryContext(ctx, `
		SELECT pid, GROUP_CONCAT(id) FROM sessions
		WHERE pid > 0
		GROUP BY pid HAVING COUNT(*) > 1
		ORDER BY pid`)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var out []DuplicatePID
	for rows.Next() {
		var d DuplicatePID
		var ids string
		if rows.Scan(&d.PID, &ids) != nil {
			continue
		}
		d.IDs = strings.Split(ids, ",")
		out = append(out, d)
	}
	return out
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
