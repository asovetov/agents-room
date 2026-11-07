package hub

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

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
	// Warning is set when the claim succeeded but on a path that is probably
	// not the one the caller meant. It is not an error: a relative path is
	// legitimately relative to the caller's own directory, and refusing it
	// would break the ordinary case.
	Warning string `json:"warning,omitempty"`
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

	// A lease lives in the session's repository room. Contention is checked
	// across every room the session is in, so two agents that share only a
	// shared room still cannot hold one file at the same time.
	roomIDs, err := h.sessionRoomIDs(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("claim: %w", err)
	}
	roomID := roomIDs[0]
	now := h.now()

	var denied *Lease
	err = h.db.Immediate(ctx, func(conn *sql.Conn) error {
		if _, err := conn.ExecContext(ctx,
			`DELETE FROM leases WHERE expires_at <= ?`, now); err != nil {
			return fmt.Errorf("claim: sweep expired: %w", err)
		}

		// Contention is between sessions, not between room rows: a lease
		// lives in the holder's repository room, so matching on the
		// claimant's rooms would miss a holder that shares only a shared
		// room. Join through session_rooms instead.
		rows, err := conn.QueryContext(ctx, `
			SELECT l.owner, l.mode FROM leases l
			JOIN session_rooms sr ON sr.session_id = l.owner
			WHERE sr.room_id IN (`+placeholders(len(roomIDs))+`) AND l.path = ?`,
			append(anySlice(roomIDs), path)...)
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
	return &LeaseResult{
		OK: true, Holder: lease,
		Warning: h.foreignPathWarning(ctx, sessionID, path),
	}, nil
}

// foreignPathWarning reports a claim on a file that lives in another
// repository's checkout.
//
// A relative path is resolved against the caller's own working directory,
// which is the only sensible rule. The trap is that an agent meaning "the api
// repository's src/users.go" writes a relative path and silently leases its
// own `src/users.go`, which is a different file; the grant looks like success
// and the two agents believe they agreed. A path outside the caller's own
// directory is therefore reported, not refused: cross-repository leases are
// exactly what the tool is for, and the absolute path is how you mean them.
func (h *Hub) foreignPathWarning(ctx context.Context, sessionID, path string) string {
	var cwd string
	if err := h.db.SQL().QueryRowContext(ctx,
		`SELECT cwd FROM sessions WHERE id = ?`, sessionID).Scan(&cwd); err != nil || cwd == "" {
		return ""
	}
	if withinDir(path, cwd) {
		return ""
	}
	return fmt.Sprintf("leased %s, which is outside your working directory %s; "+
		"a relative path is resolved against your own repository, so if you "+
		"meant another repository's file, pass its absolute path", path, cwd)
}

// withinDir reports whether path is dir or below it.
func withinDir(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
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

// RenewAll extends every live lease a session holds and reports how many it
// touched.
//
// A lease is meant to cover the edit it guards, and an edit can outlast the
// TTL. Without this the only ways to keep a lease were to renew each path by
// name, which needs the agent to remember them, or to let it expire under a
// session that is still working. The hook calls this on every tool event, so
// a session that is doing anything at all keeps what it holds.
//
// It renews rather than re-acquires: a lease this session lost to expiry is
// not taken back, because someone else may hold it by now.
func (h *Hub) RenewAll(ctx context.Context, sessionID string, ttlSeconds int64) (int, error) {
	if ttlSeconds <= 0 {
		ttlSeconds = 1800
	}
	now := h.now()
	res, err := h.db.SQL().ExecContext(ctx, `
		UPDATE leases SET expires_at = ?
		WHERE owner = ? AND expires_at > ?`,
		now+ttlSeconds, sessionID, now)
	if err != nil {
		return 0, fmt.Errorf("renew all: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("renew all: %w", err)
	}
	return int(n), nil
}

// ExpiringLeases reports the live leases of a session that have less than
// the given fraction of their original lifetime left.
//
// It exists for doctor: a lease that is about to lapse under a session that
// is still editing is the case the TTL was supposed to prevent, and nothing
// else can see it coming.
func (h *Hub) ExpiringLeases(ctx context.Context, sessionID string, fraction float64) ([]Lease, error) {
	roomID, err := h.sessionRoom(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("expiring leases: %w", err)
	}
	now := h.now()
	rows, err := h.db.SQL().QueryContext(ctx, `
		SELECT l.path, l.owner, l.mode, l.acquired_at, l.expires_at,
		       COALESCE(s.label, ''), COALESCE(r.name, '')
		FROM leases l
		LEFT JOIN sessions s ON s.id = l.owner
		LEFT JOIN rooms r ON r.id = l.room_id
		WHERE l.room_id = ? AND l.owner = ? AND l.expires_at > ?
		  AND (l.expires_at - ?) < (? - l.acquired_at) * ?
		ORDER BY l.expires_at`, roomID, sessionID, now, now, now, fraction)
	if err != nil {
		return nil, fmt.Errorf("expiring leases: query: %w", err)
	}
	defer rows.Close()

	out := []Lease{}
	for rows.Next() {
		var l Lease
		if err := rows.Scan(&l.Path, &l.Owner, &l.Mode, &l.AcquiredAt,
			&l.ExpiresAt, &l.OwnerLabel, &l.Room); err != nil {
			return nil, fmt.Errorf("expiring leases: scan: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
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
	roomIDs, err := h.sessionRoomIDs(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("check: %w", err)
	}
	var owner, leaseRoom string
	err = h.db.SQL().QueryRowContext(ctx, `
		SELECT l.owner, l.room_id FROM leases l
		JOIN session_rooms sr ON sr.session_id = l.owner
		WHERE sr.room_id IN (`+placeholders(len(roomIDs))+`) AND l.path = ? AND l.expires_at > ?
		ORDER BY l.acquired_at ASC LIMIT 1`,
		append(anySlice(roomIDs), path, h.now())...).Scan(&owner, &leaseRoom)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("check: %w", err)
	}
	return h.leaseFor(ctx, leaseRoom, path, owner)
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
