// Package store owns the SQLite database that every participant opens
// directly. There is no daemon: concurrency is handled by SQLite itself
// (WAL mode, busy timeout, short transactions).
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaV1 string

// DB is a handle on the shared store.
type DB struct {
	sql  *sql.DB
	path string
}

// DefaultPath returns the store location for the current user.
//
// AGENTS_ROOM_DB overrides it, which tests use to point at a temporary file.
func DefaultPath() (string, error) {
	if p := os.Getenv("AGENTS_ROOM_DB"); p != "" {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find home directory: %w", err)
	}
	return filepath.Join(home, ".agents-room", "room.db"), nil
}

// Open opens the store at path, creating the directory and schema if needed.
func Open(path string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create store directory: %w", err)
	}

	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}

	// SQLite serializes writers itself. Keeping the pool small avoids
	// lock contention between connections inside one process.
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(time.Hour)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping store: %w", err)
	}
	d := &DB{sql: db, path: path}
	if err := d.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}

	return d, nil
}

// OpenDefault opens the store at DefaultPath.
func OpenDefault() (*DB, error) {
	path, err := DefaultPath()
	if err != nil {
		return nil, err
	}
	return Open(path)
}

// Close releases the handle.
func (d *DB) Close() error { return d.sql.Close() }

// Path returns the database file path.
func (d *DB) Path() string { return d.path }

// SQL exposes the underlying handle for tests and for packages in this module.
func (d *DB) SQL() *sql.DB { return d.sql }

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("not found")

// Immediate runs fn inside a transaction that holds the write lock from the
// first statement.
//
// A plain BEGIN DEFERRED transaction that reads and then writes can deadlock
// against another process doing the same; SQLite reports that as SQLITE_BUSY
// instead of retrying. Anything that checks a condition and then writes on
// the strength of it must use this.
func (d *DB) Immediate(ctx context.Context, fn func(*sql.Conn) error) error {
	conn, err := d.sql.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire connection: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("begin immediate: %w", err)
	}
	if err := fn(conn); err != nil {
		conn.ExecContext(context.Background(), "ROLLBACK")
		return err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		conn.ExecContext(context.Background(), "ROLLBACK")
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// Now is the clock used across the module. Tests replace it.
var Now = func() int64 { return time.Now().Unix() }
