package hub

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/asovetov/agents-room/internal/store"
)

// normalizePath makes a path absolute and clean so that two sessions naming
// the same file differently still contend for one lease. "src/wire.go" and
// "/abs/src/wire.go" from the same directory must not become two leases.
func normalizePath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve path: %w", err)
	}
	return abs, nil
}

// Lease modes. A shared lease allows several holders; an exclusive lease
// allows exactly one.
const (
	ModeExclusive = "exclusive"
	ModeShared    = "shared"
)

// Lease is a claim on a path by a session.
type Lease struct {
	Room       string `json:"room"`
	Path       string `json:"path"`
	Owner      string `json:"owner"`
	OwnerLabel string `json:"owner_label,omitempty"`
	Mode       string `json:"mode"`
	AcquiredAt int64  `json:"acquired_at"`
	ExpiresAt  int64  `json:"expires_at"`
}

// LeaseResult reports the outcome of a claim.
type LeaseResult struct {
	OK     bool   `json:"ok"`
	Holder *Lease `json:"holder,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// Claim tries to take a lease on path.
//
// The check and the insert run in one immediate transaction, so two processes
// claiming the same path cannot both succeed. Expired leases are removed
// first, which is what makes a crashed holder harmless.
func (h *Hub) Claim(ctx context.Context, sessionID, path string, ttlSeconds int64, mode string) (*LeaseResult, error) {
	if path == "" {
		return nil, errors.New("claim: path is required")
	}
	path, err := normalizePath(path)
	if err != nil {
		return nil, fmt.Errorf("claim: %w", err)
	}
	if ttlSeconds <= 0 {
		ttlSeconds = 1800
	}
	if mode == "" {
		mode = ModeExclusive
	}
	if mode != ModeExclusive && mode != ModeShared {
		return nil, fmt.Errorf("claim: mode must be %q or %q", ModeExclusive, ModeShared)
	}

	roomID, err := h.sessionRoom(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("claim: %w", err)
	}
	now := h.now()

	var denied *Lease
	err = h.db.Immediate(ctx, func(conn *sql.Conn) error {
		if _, err := conn.ExecContext(ctx,
			`DELETE FROM leases WHERE room_id = ? AND expires_at <= ?`, roomID, now); err != nil {
			return fmt.Errorf("claim: sweep expired: %w", err)
		}

		rows, err := conn.QueryContext(ctx, `
			SELECT owner, mode FROM leases WHERE room_id = ? AND path = ?`,
			roomID, path)
		if err != nil {
			return fmt.Errorf("claim: query holders: %w", err)
		}
		var holders []struct{ owner, mode string }
		for rows.Next() {
			var o, m string
			if err := rows.Scan(&o, &m); err != nil {
				rows.Close()
				return fmt.Errorf("claim: scan holder: %w", err)
			}
			holders = append(holders, struct{ owner, mode string }{o, m})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("claim: rows: %w", err)
		}

		for _, held := range holders {
			if held.owner == sessionID {
				// Re-claiming your own lease extends it.
				continue
			}
			if mode == ModeShared && held.mode == ModeShared {
				continue
			}
			// Do not open another connection here: this callback already
			// holds one, and a nested query would deadlock the pool under
			// contention. The room name is filled in after the commit.
			denied = &Lease{Path: path, Owner: held.owner, Mode: held.mode}
			return nil
		}

		expires := now + ttlSeconds
		if _, err := conn.ExecContext(ctx, `
			INSERT INTO leases (room_id, path, owner, mode, acquired_at, expires_at)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT(room_id, path, owner)
			DO UPDATE SET mode = excluded.mode,
			              acquired_at = excluded.acquired_at,
			              expires_at = excluded.expires_at`,
			roomID, path, sessionID, mode, now, expires); err != nil {
			return fmt.Errorf("claim: insert lease: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if denied != nil {
		denied.Room = h.roomName(ctx, roomID)
		if holder, err := h.leaseFor(ctx, roomID, path, denied.Owner); err == nil {
			denied = holder
		}
		return &LeaseResult{OK: false, Holder: denied, Reason: "held by another session"}, nil
	}

	if err := h.appendEvent(ctx, roomID, "lease.claim", map[string]any{
		"path": path, "owner": sessionID, "mode": mode, "ttl": ttlSeconds,
	}); err != nil {
		return nil, err
	}

	lease, err := h.leaseFor(ctx, roomID, path, sessionID)
	if err != nil {
		return nil, err
	}
	return &LeaseResult{OK: true, Holder: lease}, nil
}

// Renew extends a lease the session already holds.
//
// A long edit can outlive the TTL. Renewing is not the same as claiming: it
// never takes a path the session does not hold, so a stale agent cannot
// re-acquire a file someone else has since taken.
func (h *Hub) Renew(ctx context.Context, sessionID, path string, ttlSeconds int64) (*Lease, error) {
	path, err := normalizePath(path)
	if err != nil {
		return nil, fmt.Errorf("renew: %w", err)
	}
	if ttlSeconds <= 0 {
		ttlSeconds = 1800
	}
	roomID, err := h.sessionRoom(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("renew: %w", err)
	}
	now := h.now()
	res, err := h.db.SQL().ExecContext(ctx, `
		UPDATE leases SET expires_at = ?
		WHERE room_id = ? AND path = ? AND owner = ? AND expires_at > ?`,
		now+ttlSeconds, roomID, path, sessionID, now)
	if err != nil {
		return nil, fmt.Errorf("renew: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("renew: %w", err)
	}
	if n == 0 {
		return nil, fmt.Errorf("renew: no live lease on %q held by this session", path)
	}
	return h.leaseFor(ctx, roomID, path, sessionID)
}

// Release drops every lease a session holds on path.
func (h *Hub) Release(ctx context.Context, sessionID, path string) error {
	path, err := normalizePath(path)
	if err != nil {
		return fmt.Errorf("release: %w", err)
	}
	roomID, err := h.sessionRoom(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("release: %w", err)
	}
	res, err := h.db.SQL().ExecContext(ctx,
		`DELETE FROM leases WHERE room_id = ? AND path = ? AND owner = ?`,
		roomID, path, sessionID)
	if err != nil {
		return fmt.Errorf("release: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("release: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("release: no lease on %q held by this session", path)
	}
	return h.appendEvent(ctx, roomID, "lease.release", map[string]any{
		"path": path, "owner": sessionID,
	})
}

// Check reports the live holder of an exact path, if any.
func (h *Hub) Check(ctx context.Context, sessionID, path string) (*Lease, error) {
	path, err := normalizePath(path)
	if err != nil {
		return nil, fmt.Errorf("check: %w", err)
	}
	roomID, err := h.sessionRoom(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("check: %w", err)
	}
	var owner string
	err = h.db.SQL().QueryRowContext(ctx, `
		SELECT owner FROM leases
		WHERE room_id = ? AND path = ? AND expires_at > ?
		ORDER BY acquired_at ASC LIMIT 1`, roomID, path, h.now()).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("check: %w", err)
	}
	return h.leaseFor(ctx, roomID, path, owner)
}

// List returns every live lease in a room.
func (h *Hub) List(ctx context.Context, sessionID string) ([]Lease, error) {
	roomID, err := h.sessionRoom(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("list: %w", err)
	}
	rows, err := h.db.SQL().QueryContext(ctx, `
		SELECT l.path, l.owner, l.mode, l.acquired_at, l.expires_at,
		       COALESCE(s.label, ''), COALESCE(r.name, '')
		FROM leases l
		LEFT JOIN sessions s ON s.id = l.owner
		LEFT JOIN rooms r ON r.id = l.room_id
		WHERE l.room_id = ? AND l.expires_at > ?
		ORDER BY l.path`, roomID, h.now())
	if err != nil {
		return nil, fmt.Errorf("list: query: %w", err)
	}
	defer rows.Close()

	out := []Lease{}
	for rows.Next() {
		var l Lease
		if err := rows.Scan(&l.Path, &l.Owner, &l.Mode, &l.AcquiredAt,
			&l.ExpiresAt, &l.OwnerLabel, &l.Room); err != nil {
			return nil, fmt.Errorf("list: scan: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (h *Hub) leaseFor(ctx context.Context, roomID, path, owner string) (*Lease, error) {
	var l Lease
	err := h.db.SQL().QueryRowContext(ctx, `
		SELECT l.path, l.owner, l.mode, l.acquired_at, l.expires_at,
		       COALESCE(s.label, ''), COALESCE(r.name, '')
		FROM leases l
		LEFT JOIN sessions s ON s.id = l.owner
		LEFT JOIN rooms r ON r.id = l.room_id
		WHERE l.room_id = ? AND l.path = ? AND l.owner = ?`,
		roomID, path, owner).Scan(&l.Path, &l.Owner, &l.Mode, &l.AcquiredAt,
		&l.ExpiresAt, &l.OwnerLabel, &l.Room)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load lease: %w", err)
	}
	return &l, nil
}
