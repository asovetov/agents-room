package harnesscwd

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// makeHermesStore writes a minimal stand-in for Hermes' session store: one
// table with the two columns the lookup reads.
func makeHermesStore(t *testing.T, rows map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE sessions (id TEXT PRIMARY KEY, cwd TEXT)`); err != nil {
		db.Close()
		t.Fatalf("create: %v", err)
	}
	for id, cwd := range rows {
		if _, err := db.Exec(`INSERT INTO sessions (id, cwd) VALUES (?, ?)`, id, cwd); err != nil {
			db.Close()
			t.Fatalf("insert: %v", err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	t.Setenv("HERMES_HOME", dir)
	return path
}

// The whole reason this package exists: the hook payload carries the Hermes
// install directory, and the session's own directory is in its store.
func TestHermesSessionCwdReturnsTheSessionDirectory(t *testing.T) {
	makeHermesStore(t, map[string]string{
		"local-abc": "/home/someone/proj/shop",
	})
	got, err := HermesSessionCwd(context.Background(), "local-abc")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if got != "/home/someone/proj/shop" {
		t.Fatalf("cwd = %q, want the session's own directory", got)
	}
}

// An unknown key is not an error: the caller falls back to the payload, and
// a hook must never fail because the store is a moment behind.
func TestHermesSessionCwdUnknownKeyIsEmpty(t *testing.T) {
	makeHermesStore(t, map[string]string{"local-abc": "/somewhere"})
	got, err := HermesSessionCwd(context.Background(), "local-nope")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Fatalf("cwd = %q, want empty", got)
	}
}

// A machine without Hermes has no store, and that is not an error either.
func TestHermesSessionCwdMissingStoreIsEmpty(t *testing.T) {
	t.Setenv("HERMES_HOME", filepath.Join(t.TempDir(), "absent"))
	got, err := HermesSessionCwd(context.Background(), "local-abc")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Fatalf("cwd = %q, want empty", got)
	}
}

// The lookup must never write: Hermes owns that database, and a hook that
// created a WAL or took a write lock on it would be a different bug.
func TestHermesSessionCwdDoesNotWrite(t *testing.T) {
	path := makeHermesStore(t, map[string]string{"local-abc": "/somewhere"})
	before, err := filepath.Glob(filepath.Join(filepath.Dir(path), "state.db-*"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := HermesSessionCwd(context.Background(), "local-abc"); err != nil {
		t.Fatalf("lookup: %v", err)
	}
	after, err := filepath.Glob(filepath.Join(filepath.Dir(path), "state.db-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("lookup created files beside the store: %v -> %v", before, after)
	}
}

// The retry exists because Hermes may not have committed the row when the
// session-start hook runs. A row that appears during the wait is found.
func TestHermesSessionCwdRetryWaitsForALateRow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	t.Setenv("HERMES_HOME", dir)

	// busy_timeout: the retry opens the same file read-only while this
	// connection holds it, and on Windows the write blocks rather than
	// failing fast.
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?_pragma=busy_timeout(2000)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE sessions (id TEXT PRIMARY KEY, cwd TEXT)`); err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// The insert must not fail silently, or the test would blame the lookup
	// for a write that never happened.
	inserted := make(chan error, 1)
	go func() {
		time.Sleep(120 * time.Millisecond)
		_, err := db.Exec(`INSERT INTO sessions (id, cwd) VALUES ('local-late', '/late/dir')`)
		inserted <- err
	}()

	got, err := HermesSessionCwdRetry(context.Background(), "local-late", 2*time.Second)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if got != "/late/dir" {
		t.Fatalf("cwd = %q, want the row that appeared during the wait", got)
	}
	if err := <-inserted; err != nil {
		t.Fatalf("the row the test waited for was never written: %v", err)
	}
}

// And it gives up rather than hanging a hook that has a time budget.
func TestHermesSessionCwdRetryGivesUp(t *testing.T) {
	makeHermesStore(t, nil)
	start := time.Now()
	got, err := HermesSessionCwdRetry(context.Background(), "local-never", 150*time.Millisecond)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Fatalf("cwd = %q, want empty", got)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("retry took %v, want it bounded by the wait", elapsed)
	}
}

// A Windows path is a drive letter and backslashes, and "file:C:\..." is a
// URI the driver cannot open. The lookup writes the path with forward slashes
// for that reason; this pins the spelling so a later "simplification" back to
// string concatenation fails here rather than only on the Windows runner.
func TestHermesStoreDsnSpelling(t *testing.T) {
	// filepath.ToSlash is a no-op on POSIX, so the code uses ReplaceAll; this
	// assertion pins that, and would fail if it went back to ToSlash.
	win := `C:\Users\someone\.hermes\state.db`
	got := "file:" + strings.ReplaceAll(win, `\`, "/") + "?mode=ro"
	if strings.Contains(got, `\`) {
		t.Fatalf("dsn keeps backslashes, which the driver parses as an opaque URI: %q", got)
	}
	if !strings.HasPrefix(got, "file:C:/Users/") {
		t.Fatalf("dsn = %q, want the drive letter followed by forward slashes", got)
	}
}
