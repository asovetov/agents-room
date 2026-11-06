package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func openTemp(t *testing.T) (*DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "room.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db, path
}

func TestSchemaVersionReachesLatest(t *testing.T) {
	db, _ := openTemp(t)
	v, err := db.SchemaVersion(context.Background())
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	want := migrations[len(migrations)-1].version
	if v != want {
		t.Fatalf("schema version = %d, want %d", v, want)
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	db, path := openTemp(t)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		again, err := Open(path)
		if err != nil {
			t.Fatalf("reopen %d: %v", i, err)
		}
		v, err := again.SchemaVersion(context.Background())
		if err != nil {
			t.Fatalf("version on reopen %d: %v", i, err)
		}
		want := migrations[len(migrations)-1].version
		if v != want {
			t.Fatalf("reopen %d: version = %d, want %d", i, v, want)
		}
		again.Close()
	}
}

// TestMigrateUpgradesV1Store builds a database at version 1 by hand, the way
// a user who installed before migration 2 would have it, and checks that
// opening it applies the remaining steps without losing rows.
func TestMigrateUpgradesV1Store(t *testing.T) {
	path := filepath.Join(t.TempDir(), "room.db")
	raw, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := raw.ExecContext(ctx, schemaV1); err != nil {
		t.Fatalf("apply v1 schema: %v", err)
	}
	if _, err := raw.ExecContext(ctx, `
		CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, applied_at) VALUES (1, 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx,
		`INSERT INTO rooms (id, name, created_at) VALUES ('r1', 'repo:old', 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx,
		`INSERT INTO sessions (id, room_id, harness, cwd, pid, label, tags, autonomy, joined_at, last_seen)
		 VALUES ('s1', 'r1', 'claude-code', '/tmp', 1, 'old', '[]', 1, 1, 1)`); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	db, err := Open(path)
	if err != nil {
		t.Fatalf("open upgraded store: %v", err)
	}
	defer db.Close()

	v, err := db.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	if v != migrations[len(migrations)-1].version {
		t.Fatalf("version = %d, want the latest", v)
	}

	var label string
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT label FROM sessions WHERE id = 's1'`).Scan(&label); err != nil {
		t.Fatalf("existing session lost: %v", err)
	}
	if label != "old" {
		t.Fatalf("label = %q, want old", label)
	}

	// The table must exist and be usable. A room with no row falls back to
	// the maximum cap, so the migration does not need to backfill.
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO rooms_meta (room_id, autonomy_cap) VALUES ('r1', 1)`); err != nil {
		t.Fatalf("rooms_meta not created: %v", err)
	}
	var cap int
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT autonomy_cap FROM rooms_meta WHERE room_id = 'r1'`).Scan(&cap); err != nil {
		t.Fatalf("read cap: %v", err)
	}
	if cap != 1 {
		t.Fatalf("cap = %d, want 1", cap)
	}
}

func TestImmediateRollsBackOnError(t *testing.T) {
	db, _ := openTemp(t)
	ctx := context.Background()
	sentinel := context.Canceled

	err := db.Immediate(ctx, func(conn *sql.Conn) error {
		if _, err := conn.ExecContext(ctx,
			`INSERT INTO rooms (id, name, created_at) VALUES ('x', 'repo:x', 1)`); err != nil {
			return err
		}
		return sentinel
	})
	if err != sentinel {
		t.Fatalf("error = %v, want the callback error", err)
	}

	var n int
	if err := db.SQL().QueryRowContext(ctx, `SELECT COUNT(*) FROM rooms`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d rows survived a rolled back transaction, want 0", n)
	}
}

func TestDefaultPathHonoursEnv(t *testing.T) {
	t.Setenv("AGENTS_ROOM_DB", "/tmp/custom.db")
	got, err := DefaultPath()
	if err != nil {
		t.Fatalf("default path: %v", err)
	}
	if got != "/tmp/custom.db" {
		t.Fatalf("path = %q, want the environment override", got)
	}
}

func TestDefaultPathFallsBackToHome(t *testing.T) {
	t.Setenv("AGENTS_ROOM_DB", "")
	got, err := DefaultPath()
	if err != nil {
		t.Fatalf("default path: %v", err)
	}
	if filepath.Base(got) != "room.db" {
		t.Fatalf("path = %q, want a room.db under the home directory", got)
	}
}

// A session deleted before deliveries had a foreign key left its rows
// behind. They are invisible in room_who, never read, and still counted as
// unread by the hook, so the upgrade drops them.
func TestMigrateDropsOrphanDeliveries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "room.db")
	raw, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := raw.ExecContext(ctx, schemaV1); err != nil {
		t.Fatalf("apply v1 schema: %v", err)
	}
	if _, err := raw.ExecContext(ctx, `
		CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	// Stop before migration 4, so the upgrade under test is the one that
	// removes the rows.
	if _, err := raw.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, applied_at) VALUES (1, 1), (2, 1), (3, 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx,
		`INSERT INTO rooms (id, name, created_at) VALUES ('r1', 'repo:old', 1)`); err != nil {
		t.Fatal(err)
	}
	// One live session with a delivery, one orphan delivery whose session is
	// already gone.
	if _, err := raw.ExecContext(ctx,
		`INSERT INTO sessions (id, room_id, harness, cwd, pid, label, tags, autonomy, joined_at, last_seen)
		 VALUES ('s1', 'r1', 'claude-code', '/tmp', 1, 'live', '[]', 1, 1, 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx,
		`INSERT INTO messages (id, room_id, sender, selector, subject, body, created_at)
		 VALUES ('m1', 'r1', 's1', '*', 's', 'b', 1)`); err != nil {
		t.Fatal(err)
	}
	for _, sid := range []string{"s1", "ghost"} {
		if _, err := raw.ExecContext(ctx,
			`INSERT INTO deliveries (message_id, session_id, state, updated_at) VALUES ('m1', ?, 'stored', 1)`,
			sid); err != nil {
			t.Fatal(err)
		}
	}
	raw.Close()

	db, err := Open(path)
	if err != nil {
		t.Fatalf("open upgraded store: %v", err)
	}
	defer db.Close()

	var ghosts, live int
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM deliveries WHERE session_id = 'ghost'`).Scan(&ghosts); err != nil {
		t.Fatalf("count orphans: %v", err)
	}
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM deliveries WHERE session_id = 's1'`).Scan(&live); err != nil {
		t.Fatalf("count live: %v", err)
	}
	if ghosts != 0 {
		t.Fatalf("%d orphan deliveries survived the upgrade, want 0", ghosts)
	}
	if live != 1 {
		t.Fatalf("live delivery count = %d, want 1: the migration dropped a row it should keep", live)
	}
}
