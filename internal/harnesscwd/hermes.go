// Package harnesscwd recovers the working directory a harness session was
// started in, for harnesses that do not report it to their hooks.
//
// Hermes builds the cwd field of its hook payload from the hook process's own
// directory, and that process lives in the Hermes install tree (~/.hermes),
// not in the directory the agent was launched from. Every session therefore
// arrived as repo:.hermes: the room was wrong, the lease anchor was wrong,
// and a message addressed to the session could never find it.
//
// The real directory is in Hermes' own session store, which is the same
// source `hermes sessions list` prints as Workspace. Reading it is a local,
// read-only lookup; nothing here writes to that database.
package harnesscwd

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// HermesStatePath returns the path of Hermes' session store.
//
// HERMES_HOME names the install root when set; otherwise the default is
// ~/.hermes. The file may be absent on a machine that has never run Hermes,
// which callers treat as "no answer" rather than an error.
func HermesStatePath() string {
	if home := strings.TrimSpace(os.Getenv("HERMES_HOME")); home != "" {
		return filepath.Join(home, "state.db")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".hermes", "state.db")
}

// HermesSessionCwd returns the directory a Hermes session was started in.
//
// The key is the value Hermes puts in the hook payload as session_id, which is
// the same string as sessions.id in its store: the long "agent:main:..." form
// is what Hermes exports to its own children, not what the hook receives.
// Both spellings are accepted so a caller that read the environment instead
// of the payload still lands on the right row.
//
// An empty answer with no error means the store exists but has no such
// session, which is normal during the first moments of a run; callers retry
// briefly and then fall back to the payload's own cwd.
func HermesSessionCwd(ctx context.Context, key string) (string, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return "", nil
	}
	path := HermesStatePath()
	if path == "" {
		return "", nil
	}
	if _, err := os.Stat(path); err != nil {
		return "", nil
	}

	// Read-only: the store belongs to Hermes, and a hook has no business
	// creating a WAL or taking a write lock on it. mode=ro also makes the
	// intent obvious to whoever reads this later.
	//
	// The path is written with forward slashes. On Windows it starts with a
	// drive letter and a backslash, and "file:C:\..." is parsed as a URI
	// whose body the driver cannot open; "file:C:/..." is the same path in
	// the one spelling SQLite reads on every platform.
	//
	// ReplaceAll, not filepath.ToSlash: on POSIX the latter is a no-op, so a
	// test that pins this spelling on Linux would pass while Windows broke.
	dsn := "file:" + strings.ReplaceAll(path, `\`, "/") + "?mode=ro&_pragma=busy_timeout(500)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return "", fmt.Errorf("open hermes store: %w", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	var cwd string
	err = db.QueryRowContext(ctx,
		`SELECT COALESCE(cwd, '') FROM sessions WHERE id = ?`, key).Scan(&cwd)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read hermes session: %w", err)
	}
	return strings.TrimSpace(cwd), nil
}

// HermesSessionCwdRetry is HermesSessionCwd with a short wait for a row that
// Hermes has not committed yet.
//
// The row is normally written a fraction of a second before the session-start
// hook runs, but "normally" is not a contract, and a missed first read would
// put the session back in repo:.hermes for its whole life. The wait is
// bounded well inside the hook's own time budget.
func HermesSessionCwdRetry(ctx context.Context, key string, wait time.Duration) (string, error) {
	deadline := time.Now().Add(wait)
	for {
		cwd, err := HermesSessionCwd(ctx, key)
		// A transient error is not an answer. Hermes is writing to its own
		// store at the same moment, and on a platform where the file is
		// briefly locked the read fails; retrying is the whole point of this
		// function, so only the last attempt's error is returned.
		if err == nil && cwd != "" {
			return cwd, nil
		}
		if time.Now().After(deadline) {
			return cwd, err
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}
