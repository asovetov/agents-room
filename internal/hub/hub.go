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
	Room     string
	Harness  string
	Label    string
	Tags     []string
	Autonomy int
	PID      int
	Cwd      string
}

// Join registers a session in a room and returns it with its new id.
//
// The room is created on first use. Autonomy is clamped to the room cap,
// which defaults to AutonomyWake and can be lowered by the operator.
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
	cap := h.roomAutonomyCap(ctx, roomID)
	if opt.Autonomy > cap {
		opt.Autonomy = cap
	}

	id, err := newID()
	if err != nil {
		return nil, err
	}
	tags, err := json.Marshal(opt.Tags)
	if err != nil {
		return nil, fmt.Errorf("encode tags: %w", err)
	}

	_, err = h.db.SQL().ExecContext(ctx, `
		INSERT INTO sessions
			(id, room_id, harness, cwd, pid, label, tags, autonomy, joined_at, last_seen)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, roomID, opt.Harness, opt.Cwd, opt.PID, opt.Label, string(tags),
		opt.Autonomy, now, now)
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
		return nil, nil
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

	var out []Session
	for rows.Next() {
		s := Session{Room: room}
		var tags string
		if err := rows.Scan(&s.ID, &s.Harness, &s.Cwd, &s.PID, &s.Label,
			&tags, &s.Autonomy, &s.JoinedAt, &s.LastSeen); err != nil {
			return nil, fmt.Errorf("scan session: %w", err)
		}
		if err := json.Unmarshal([]byte(tags), &s.Tags); err != nil {
			s.Tags = nil
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
// There is no separate cap table yet: the cap is the maximum level, and a
// later operator setting can lower it without changing the schema.
func (h *Hub) roomAutonomyCap(context.Context, string) int {
	return AutonomyWake
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
