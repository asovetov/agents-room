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

// Session is a connected agent process.
type Session struct {
	ID       string   `json:"id"`
	Room     string   `json:"room"`
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
type JoinOptions struct {
	Room       string
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
	if opt.PID == 0 {
		opt.PID = os.Getpid()
	}
	if opt.Autonomy < AutonomyNotify {
		return nil, fmt.Errorf("autonomy must be 0, 1 or 2, got %d", opt.Autonomy)
	}

	now := h.now()
	roomID, err := h.ensureRoom(ctx, opt.Room, now)
	if err != nil {
		return nil, err
	}
	cap, err := h.roomAutonomyCap(ctx, roomID)
	if err != nil {
		return nil, err
	}
	if opt.Autonomy > cap {
		opt.Autonomy = cap
	}

	if opt.Tags == nil {
		opt.Tags = []string{}
	}
	tags, err := json.Marshal(opt.Tags)
	if err != nil {
		return nil, fmt.Errorf("encode tags: %w", err)
	}

	if opt.HarnessKey != "" {
		if existing, err := h.sessionByHarnessKey(ctx, roomID, opt.Harness, opt.HarnessKey); err != nil {
			return nil, err
		} else if existing != nil {
			_, err := h.db.SQL().ExecContext(ctx, `
				UPDATE sessions
				SET cwd = ?, pid = ?, label = ?, tags = ?, autonomy = ?, last_seen = ?
				WHERE id = ?`,
				opt.Cwd, opt.PID, opt.Label, string(tags), opt.Autonomy, now, existing.ID)
			if err != nil {
				return nil, fmt.Errorf("update session: %w", err)
			}
			if err := h.appendEvent(ctx, roomID, "session.rejoin", map[string]any{
				"session": existing.ID, "harness": opt.Harness,
			}); err != nil {
				return nil, err
			}
			existing.Cwd, existing.PID, existing.Label = opt.Cwd, opt.PID, opt.Label
			existing.Tags, existing.Autonomy, existing.LastSeen = opt.Tags, opt.Autonomy, now
			return existing, nil
		}
	}

	id, err := newID()
	if err != nil {
		return nil, err
	}

	_, err = h.db.SQL().ExecContext(ctx, `
		INSERT INTO sessions
			(id, room_id, harness, cwd, pid, label, tags, autonomy, joined_at, last_seen, harness_key)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, roomID, opt.Harness, opt.Cwd, opt.PID, opt.Label, string(tags),
		opt.Autonomy, now, now, opt.HarnessKey)
	if err != nil {
		return nil, fmt.Errorf("insert session: %w", err)
	}

	if err := h.appendEvent(ctx, roomID, "session.join", map[string]any{
		"session": id, "harness": opt.Harness, "label": opt.Label,
		"autonomy": opt.Autonomy,
	}); err != nil {
		return nil, err
	}

	return &Session{
		ID: id, Room: opt.Room, Harness: opt.Harness, Cwd: opt.Cwd,
		PID: opt.PID, Label: opt.Label, Tags: opt.Tags, Autonomy: opt.Autonomy,
		JoinedAt: now, LastSeen: now,
	}, nil
}

// Leave removes a session and releases every lease it holds.
func (h *Hub) Leave(ctx context.Context, sessionID string) error {
	roomID, err := h.sessionRoom(ctx, sessionID)
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
	return h.appendEvent(ctx, roomID, "session.leave", map[string]any{"session": sessionID})
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
		SELECT id, harness, cwd, pid, label, tags, autonomy, joined_at, last_seen
		FROM sessions WHERE room_id = ? ORDER BY last_seen DESC`, roomID)
	if err != nil {
		return nil, fmt.Errorf("query sessions: %w", err)
	}
	defer rows.Close()

	out := []Session{}
	for rows.Next() {
		s := Session{Room: room}
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
	return out, rows.Err()
}

// RoomOf returns the room name a session belongs to, or an empty string when
// the session is unknown.
func (h *Hub) RoomOf(ctx context.Context, sessionID string) string {
	roomID, err := h.sessionRoom(ctx, sessionID)
	if err != nil {
		return ""
	}
	return h.roomName(ctx, roomID)
}

// PruneStale removes sessions that have not been seen for longer than maxAge
// and releases their leases.
//
// A harness that dies without firing its end event leaves a row behind. Its
// leases still expire on their own, but the row would keep showing in
// room_who forever, so an operator can sweep it. PruneStale never touches a
// session seen within maxAge, which is what keeps a working agent safe.
func (h *Hub) PruneStale(ctx context.Context, room string, maxAge time.Duration) (int, error) {
	roomID, err := h.roomID(ctx, room)
	if errors.Is(err, store.ErrNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	cutoff := h.now() - int64(maxAge.Seconds())

	rows, err := h.db.SQL().QueryContext(ctx, `
		SELECT id FROM sessions WHERE room_id = ? AND last_seen < ?`, roomID, cutoff)
	if err != nil {
		return 0, fmt.Errorf("prune: list stale: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("prune: scan: %w", err)
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
func (h *Hub) sessionByHarnessKey(ctx context.Context, roomID, harness, key string) (*Session, error) {
	s := Session{Harness: harness}
	var tags string
	err := h.db.SQL().QueryRowContext(ctx, `
		SELECT id, cwd, pid, label, tags, autonomy, joined_at, last_seen
		FROM sessions
		WHERE room_id = ? AND harness = ? AND harness_key = ?`,
		roomID, harness, key).Scan(&s.ID, &s.Cwd, &s.PID, &s.Label, &tags,
		&s.Autonomy, &s.JoinedAt, &s.LastSeen)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find session by harness key: %w", err)
	}
	s.Tags = []string{}
	_ = json.Unmarshal([]byte(tags), &s.Tags)
	s.Room = h.roomName(ctx, roomID)
	return &s, nil
}

// FindSession resolves a harness session key to a room session. Hooks use it
// to map a harness session id to the room session id without knowing the
// latter.
func (h *Hub) FindSession(ctx context.Context, room, harness, key string) (*Session, error) {
	roomID, err := h.roomID(ctx, room)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return h.sessionByHarnessKey(ctx, roomID, harness, key)
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
	root := dir
	if out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output(); err == nil {
		if s := strings.TrimSpace(string(out)); s != "" {
			root = s
		}
	}
	return "repo:" + filepath.Base(root)
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
