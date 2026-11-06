package store

import (
	"context"
	"database/sql"
	"fmt"
)

// migration is one forward-only schema step. Steps are applied in order and
// recorded in schema_migrations, so an existing store is upgraded in place
// rather than recreated.
type migration struct {
	version int
	apply   func(context.Context, *sql.Conn) error
}

// migrations is append-only. Never edit a shipped step; add a new one.
var migrations = []migration{
	{
		version: 1,
		apply: func(ctx context.Context, conn *sql.Conn) error {
			_, err := conn.ExecContext(ctx, schemaV1)
			return err
		},
	},
	{
		version: 2,
		apply: func(ctx context.Context, conn *sql.Conn) error {
			// A harness may report the same session more than once (Claude
			// Code fires SessionStart on startup, resume, clear, compact and
			// fork). harness_key lets a rejoin update the existing row
			// instead of creating a second one.
			//
			// autonomy_cap is the operator ceiling for a room, which an
			// operator can lower without touching any session.
			stmts := []string{
				`ALTER TABLE sessions ADD COLUMN harness_key TEXT NOT NULL DEFAULT ''`,
				`CREATE UNIQUE INDEX IF NOT EXISTS idx_sessions_harness_key
				   ON sessions(room_id, harness, harness_key)
				   WHERE harness_key <> ''`,
				`CREATE TABLE IF NOT EXISTS rooms_meta (
				   room_id      TEXT PRIMARY KEY REFERENCES rooms(id) ON DELETE CASCADE,
				   autonomy_cap INTEGER NOT NULL DEFAULT 2
				 )`,
			}
			for _, s := range stmts {
				if _, err := conn.ExecContext(ctx, s); err != nil {
					return fmt.Errorf("migration 2: %w", err)
				}
			}
			return nil
		},
	},
	{
		version: 3,
		apply: func(ctx context.Context, conn *sql.Conn) error {
			// A session can be in several rooms: its repository room plus any
			// number of shared rooms. That is what makes an agent in one
			// repository able to reach an agent in another, which is the whole
			// point of a shared room.
			//
			// The old sessions.room_id column stays for one release so a
			// downgrade still reads, but nothing writes it any more. SQLite
			// cannot drop a column with a foreign key in place on every
			// supported version, and an unused column costs nothing.
			stmts := []string{
				`CREATE TABLE IF NOT EXISTS session_rooms (
				   session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
				   room_id    TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
				   joined_at  INTEGER NOT NULL,
				   PRIMARY KEY (session_id, room_id)
				 )`,
				`CREATE INDEX IF NOT EXISTS idx_session_rooms_room
				   ON session_rooms(room_id)`,
				// Carry every existing membership across, so a store written
				// before this migration keeps its rooms.
				`INSERT OR IGNORE INTO session_rooms (session_id, room_id, joined_at)
				   SELECT id, room_id, joined_at FROM sessions WHERE room_id IS NOT NULL`,
				// The idempotency key is per harness session, not per room:
				// rejoining must find the session wherever it already is.
				`CREATE INDEX IF NOT EXISTS idx_sessions_harness_lookup
				   ON sessions(harness, harness_key) WHERE harness_key <> ''`,
			}
			for _, s := range stmts {
				if _, err := conn.ExecContext(ctx, s); err != nil {
					return fmt.Errorf("migration 3: %w", err)
				}
			}
			return nil
		},
	},
	{
		version: 4,
		apply: func(ctx context.Context, conn *sql.Conn) error {
			// deliveries.session_id was the one column with no REFERENCES
			// clause, so deleting a session left its deliveries behind. They
			// are invisible in room_who and never read, but Unread and Pending
			// count them, so every hook event paid for rows that belonged to
			// sessions that no longer exist. A store that ran for a day had
			// more of them than live ones.
			//
			// Deleting the orphans is safe: a delivery is only meaningful for
			// a session that can still fetch it.
			stmts := []string{
				`DELETE FROM deliveries
				   WHERE session_id NOT IN (SELECT id FROM sessions)`,
			}
			for _, s := range stmts {
				if _, err := conn.ExecContext(ctx, s); err != nil {
					return fmt.Errorf("migration 4: %w", err)
				}
			}
			return nil
		},
	},
}

// migrate applies every step the database has not seen yet.
//
// Each step runs inside its own immediate transaction so two processes
// starting at once cannot apply the same step twice.
func (d *DB) migrate(ctx context.Context) error {
	if _, err := d.sql.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    INTEGER PRIMARY KEY,
			applied_at INTEGER NOT NULL
		)`); err != nil {
		return fmt.Errorf("create migration table: %w", err)
	}

	for _, m := range migrations {
		var applied int
		err := d.sql.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, m.version).Scan(&applied)
		if err != nil {
			return fmt.Errorf("check migration %d: %w", m.version, err)
		}
		if applied > 0 {
			continue
		}

		err = d.Immediate(ctx, func(conn *sql.Conn) error {
			// Re-check inside the write lock: another process may have
			// applied this step between our read and our begin.
			var n int
			if err := conn.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, m.version).Scan(&n); err != nil {
				return fmt.Errorf("recheck migration %d: %w", m.version, err)
			}
			if n > 0 {
				return nil
			}
			if err := m.apply(ctx, conn); err != nil {
				return err
			}
			_, err := conn.ExecContext(ctx,
				`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
				m.version, Now())
			return err
		})
		if err != nil {
			return fmt.Errorf("apply migration %d: %w", m.version, err)
		}
	}
	return nil
}

// SchemaVersion reports the highest applied migration.
func (d *DB) SchemaVersion(ctx context.Context) (int, error) {
	var v sql.NullInt64
	err := d.sql.QueryRowContext(ctx,
		`SELECT MAX(version) FROM schema_migrations`).Scan(&v)
	if err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	if !v.Valid {
		return 0, nil
	}
	return int(v.Int64), nil
}
