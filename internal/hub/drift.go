package hub

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/asovetov/agents-room/internal/store"
)

// Drift severities.
const (
	// DriftSafe means the file is unchanged since this session read it.
	DriftSafe = "SAFE"
	// DriftMinor means the file changed, but nobody holds a lease on it.
	DriftMinor = "MINOR"
	// DriftCritical means the file changed while another session held a lease.
	DriftCritical = "CRITICAL"
)

// DriftResult answers "is my read of this file still valid".
type DriftResult struct {
	Path     string `json:"path"`
	Severity string `json:"severity"`
	Changed  bool   `json:"changed"`
	Holder   string `json:"holder,omitempty"`
	Since    int64  `json:"since,omitempty"`
	Message  string `json:"message"`
}

// HashFile returns the SHA-256 of a file's contents.
func HashFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// NoteRead records that a session read path at its current content. Call it
// after reading a file so DriftCheck has a baseline.
func (h *Hub) NoteRead(ctx context.Context, sessionID, path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("note read: %w", err)
	}
	sum, err := HashFile(abs)
	if err != nil {
		return "", err
	}
	roomID, err := h.sessionRoom(ctx, sessionID)
	if err != nil {
		return "", fmt.Errorf("note read: %w", err)
	}
	if _, err := h.db.SQL().ExecContext(ctx, `
		INSERT INTO file_reads (room_id, path, session_id, hash, read_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(room_id, path, session_id)
		DO UPDATE SET hash = excluded.hash, read_at = excluded.read_at`,
		roomID, abs, sessionID, sum, h.now()); err != nil {
		return "", fmt.Errorf("note read: %w", err)
	}
	return sum, nil
}

// DriftCheck compares the file on disk with what the session last read.
//
// Severity:
//
//	SAFE      unchanged
//	MINOR     changed, no live lease on the path
//	CRITICAL  changed and another session holds a lease on the path
//
// A session that never read the file gets SAFE: there is no baseline to
// invalidate.
func (h *Hub) DriftCheck(ctx context.Context, sessionID, path string) (*DriftResult, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("drift check: %w", err)
	}
	roomID, err := h.sessionRoom(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("drift check: %w", err)
	}

	var recorded string
	var readAt int64
	err = h.db.SQL().QueryRowContext(ctx, `
		SELECT hash, read_at FROM file_reads
		WHERE room_id = ? AND path = ? AND session_id = ?`,
		roomID, abs, sessionID).Scan(&recorded, &readAt)
	if errors.Is(err, sql.ErrNoRows) {
		return &DriftResult{
			Path: abs, Severity: DriftSafe,
			Message: "no baseline recorded; call note_read before relying on this file",
		}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("drift check: %w", err)
	}

	current, err := HashFile(abs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &DriftResult{
				Path: abs, Severity: DriftCritical, Changed: true,
				Since: readAt, Message: "file was deleted since you read it",
			}, nil
		}
		return nil, err
	}

	if current == recorded {
		return &DriftResult{Path: abs, Severity: DriftSafe}, nil
	}

	holder, err := h.Check(ctx, sessionID, abs)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}

	res := &DriftResult{Path: abs, Changed: true, Since: readAt}
	if holder != nil && holder.Owner != sessionID {
		res.Severity = DriftCritical
		res.Holder = holder.Owner
		res.Message = "file changed while another session held a lease; re-read before editing"
	} else {
		res.Severity = DriftMinor
		res.Message = "file changed since you read it; re-read before editing"
	}
	return res, nil
}
